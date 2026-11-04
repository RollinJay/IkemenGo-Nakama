package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// A P2P session that finds no UDP path to the peer falls back to a relay
// (nakama_p2ppath.go): a Nakama relayed match that the two players join,
// whose server forwards each datagram one sends to the other. Nakama relays
// match data to the match's other presences at once, without an
// authoritative handler's tick, and needs no server module for it.
//
// The relay is not the client's match (n.matchID): the session's match, a
// lobby or a matchmaker session, stays what it was. handleRelay therefore
// takes the server's answers about it before handleJSON looks at them, and
// every message from a relayed match too: the game's own matches are
// authoritative, and anyone can join a relayed match by its id.

// nakamaRelayJoinTimeout bounds the wait for the server's answer to joining.
const nakamaRelayJoinTimeout = 5 * time.Second

// nakamaRelay is one joined relayed match; it implements p2pRelayLink.
type nakamaRelay struct {
	client    *NakamaClient
	cid       string
	name      string
	matchID   string
	peer      string
	deliver   func([]byte)
	joined    chan error
	closeOnce sync.Once
}

// JoinRelay implements p2pRelayer. It creates the relayed match of the given
// name, or joins it when the peer created it first: Nakama gives a relayed
// match created with a name the id derived from that name. Only datagrams
// from peerUserID reach deliver.
func (n *NakamaClient) JoinRelay(name, peerUserID string, deliver func([]byte)) (p2pRelayLink, error) {
	r := &nakamaRelay{
		client:  n,
		cid:     "ikemen-relay-" + strings.TrimPrefix(n.nextCID(), "ikemen-"),
		name:    name,
		peer:    peerUserID,
		deliver: deliver,
		joined:  make(chan error, 1),
	}
	n.relayMu.Lock()
	if n.relayByCID == nil {
		n.relayByCID = map[string]*nakamaRelay{}
		n.relayByMatch = map[string]*nakamaRelay{}
	}
	n.relayByCID[r.cid] = r
	n.relayMu.Unlock()
	if err := n.sendEnvelopeCID(r.cid, "match_create", map[string]any{"name": name}); err != nil {
		n.forgetRelay(r)
		return nil, err
	}
	select {
	case err := <-r.joined:
		if err != nil {
			n.forgetRelay(r)
			return nil, err
		}
		return r, nil
	case <-time.After(nakamaRelayJoinTimeout):
	}
	n.relayMu.Lock()
	_, pending := n.relayByCID[r.cid]
	if pending {
		// Given up on: its answer, should it come, is left alone unless no
		// join of the same relay holds the match by then (handleRelay).
		delete(n.relayByCID, r.cid)
		if n.relayGivenUp == nil {
			n.relayGivenUp = map[string]string{}
		}
		n.relayGivenUp[r.cid] = name
	}
	n.relayMu.Unlock()
	if !pending {
		// The answer came at the same moment, or the connection ended.
		if err := <-r.joined; err != nil {
			n.forgetRelay(r)
			return nil, err
		}
		return r, nil
	}
	return nil, errors.New("the server did not answer the request to join the relay")
}

// nakamaRelayedMatchID reports whether id names a relayed match: Nakama
// writes a match id as "<uuid>.<node>", and a relayed match has no node.
// The game's own matches, lobbies and sessions, are authoritative and always
// have one.
func nakamaRelayedMatchID(id string) bool {
	_, node, ok := strings.Cut(id, ".")
	return ok && node == ""
}

func (n *NakamaClient) forgetRelay(r *nakamaRelay) {
	n.relayMu.Lock()
	if n.relayByCID[r.cid] == r {
		delete(n.relayByCID, r.cid)
	}
	if r.matchID != "" && n.relayByMatch[r.matchID] == r {
		delete(n.relayByMatch, r.matchID)
	}
	n.relayMu.Unlock()
}

// dropRelays forgets every relay: the connection that carried them ended.
// A join still waiting for its answer fails with err.
func (n *NakamaClient) dropRelays(err error) {
	n.relayMu.Lock()
	pending := make([]*nakamaRelay, 0, len(n.relayByCID))
	for _, r := range n.relayByCID {
		pending = append(pending, r)
	}
	n.relayByCID = nil
	n.relayByMatch = nil
	n.relayGivenUp = nil
	n.relayMu.Unlock()
	for _, r := range pending {
		select {
		case r.joined <- err:
		default:
		}
	}
}

// handleRelay takes the server's messages about the relays: the answer to
// joining one, and every message from a relayed match. It reports whether
// the message was one of those.
func (n *NakamaClient) handleRelay(raw map[string]json.RawMessage) bool {
	// Answers to joining a relay come first, whether or not a join still
	// waits for them: one the client gave up on must not reach handleJSON,
	// whose "match" branch would make the relay the client's match.
	if c, ok := raw["cid"]; ok {
		var cid string
		if json.Unmarshal(c, &cid) == nil && strings.HasPrefix(cid, "ikemen-relay-") {
			n.relayMu.Lock()
			r := n.relayByCID[cid]
			delete(n.relayByCID, cid)
			name, givenUp := n.relayGivenUp[cid]
			delete(n.relayGivenUp, cid)
			n.relayMu.Unlock()
			if r != nil {
				r.joined <- n.relayJoined(r, raw)
				return true
			}
			// The answer to a join given up on: leave what it joined, unless
			// a later join of the same relay holds it (the same name gives
			// the same match, and leaving would take the client out).
			var m struct {
				MatchID string `json:"match_id"`
			}
			if v, ok := raw["match"]; ok && givenUp && json.Unmarshal(v, &m) == nil && m.MatchID != "" && !n.relayHeld(m.MatchID, name) {
				_ = n.sendEnvelope("match_leave", map[string]any{"match_id": m.MatchID})
			}
			return true
		}
	}
	if v, ok := raw["match_data"]; ok {
		var p struct {
			MatchID  string      `json:"match_id"`
			OpCode   nakamaInt64 `json:"op_code"`
			Data     string      `json:"data"`
			Presence struct {
				UserID string `json:"user_id"`
			} `json:"presence"`
		}
		if json.Unmarshal(v, &p) != nil || !nakamaRelayedMatchID(p.MatchID) {
			return false
		}
		// Anyone can join a relayed match by its id, and write the id in
		// another way that Nakama accepts and passes on as written. So the
		// data of a relayed match never reaches the client's handlers, and
		// only the peer's data on a relay this client is in, under the id the
		// server gave it, reaches the session: the server names the sender,
		// and nobody can pass for another player. What is passed on still has
		// to open (p2pCrypt).
		n.relayMu.Lock()
		r := n.relayByMatch[p.MatchID]
		n.relayMu.Unlock()
		if r != nil && uint32(p.OpCode) == nakamaRelayOp && (r.peer == "" || p.Presence.UserID == r.peer) {
			if b, err := base64.StdEncoding.DecodeString(p.Data); err == nil && len(b) > 0 {
				r.deliver(b)
			}
		}
		return true
	}
	if v, ok := raw["match_presence_event"]; ok {
		var p struct {
			MatchID string `json:"match_id"`
		}
		return json.Unmarshal(v, &p) == nil && nakamaRelayedMatchID(p.MatchID)
	}
	return false
}

// relayHeld reports whether a joined relay is in matchID, or a join of the
// relay named name waits for its answer.
func (n *NakamaClient) relayHeld(matchID, name string) bool {
	n.relayMu.Lock()
	defer n.relayMu.Unlock()
	if n.relayByMatch[matchID] != nil {
		return true
	}
	for _, r := range n.relayByCID {
		if r.name == name {
			return true
		}
	}
	return false
}

// relayJoined reads the server's answer to joining r: the relayed match, or
// an error.
func (n *NakamaClient) relayJoined(r *nakamaRelay, raw map[string]json.RawMessage) error {
	if v, ok := raw["match"]; ok {
		var p struct {
			MatchID string `json:"match_id"`
		}
		if json.Unmarshal(v, &p) == nil && p.MatchID != "" {
			n.relayMu.Lock()
			r.matchID = p.MatchID
			if n.relayByMatch != nil {
				n.relayByMatch[p.MatchID] = r
			}
			n.relayMu.Unlock()
			return nil
		}
	}
	if v, ok := raw["error"]; ok {
		var p struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(v, &p)
		return fmt.Errorf("nakama error %d: %s", p.Code, p.Message)
	}
	return errors.New("unexpected answer to the request to join the relay")
}

// Send forwards one datagram to the peer. The relay shares the realtime
// socket's output queue with everything else the client sends, so when the
// queue is three quarters full the datagram is dropped instead, as a busy
// UDP socket would drop it: KCP and GGPO resend what they need, and the
// client's own messages keep their room.
func (r *nakamaRelay) Send(b []byte) error {
	n := r.client
	n.mu.Lock()
	queue := n.writeQueue
	n.mu.Unlock()
	if queue != nil && len(queue) > cap(queue)*3/4 {
		return errors.New("nakama relay: the realtime socket is busy")
	}
	return n.sendEnvelope("match_data_send", map[string]any{
		"match_id": r.matchID,
		"op_code":  nakamaRelayOp,
		"data":     b,
		"reliable": false,
	})
}

// Close leaves the relayed match; Nakama ends it when both players left.
func (r *nakamaRelay) Close() error {
	r.closeOnce.Do(func() {
		n := r.client
		n.forgetRelay(r)
		if r.matchID != "" && n.IsConnected() {
			_ = n.sendEnvelope("match_leave", map[string]any{"match_id": r.matchID})
		}
	})
	return nil
}
