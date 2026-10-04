package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Lobby client support. The lobby module (nakama/modules/ikemen_lobby.lua)
// broadcasts its full state on OP_LOBBY_STATE whenever it changes and sends
// chat, notices and errors on OP_LOBBY_EVENT. The client keeps the latest
// state and a short event queue so the Lua lobby screens can poll them once
// per frame; a Lua callback could miss a message while the main-thread task
// queue is full (during loading, for example), and a later state replaces
// everything the missed one carried.

const (
	nakamaLobbyStateOp   uint32 = 0x494B_4C12
	nakamaLobbyCommandOp uint32 = 0x494B_4C13
	nakamaLobbyEventOp   uint32 = 0x494B_4C14

	// nakamaLobbyEventLimit bounds the event queue if nothing polls it.
	nakamaLobbyEventLimit = 128
)

// lobbyClientState is embedded in NakamaClient and guarded by its mutex.
type lobbyClientState struct {
	lobbyState      map[string]any
	lobbyStateMatch string
	lobbyStateVer   int64
	lobbyEvents     []map[string]any
	pingCID         string
	pingSent        time.Time
	rtt             time.Duration
}

// applyLobbyMessage records lobby state and events from a relayed match
// message. It is called for every match_data message with a JSON body.
func (n *NakamaClient) applyLobbyMessage(matchID string, op uint32, payload any) {
	m, ok := payload.(map[string]any)
	if !ok {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	switch op {
	case nakamaLobbyStateOp:
		if kind, _ := m["kind"].(string); kind == "lobby_error" {
			// First lobby protocol: errors were sent on the state op code.
			n.pushLobbyEventLocked(map[string]any{"kind": "error", "error": m["error"]})
			return
		}
		n.lobbyState = m
		n.lobbyStateMatch = matchID
		n.lobbyStateVer++
	case nakamaLobbyEventOp:
		n.pushLobbyEventLocked(m)
	}
}

func (n *NakamaClient) pushLobbyEventLocked(ev map[string]any) {
	if len(n.lobbyEvents) >= nakamaLobbyEventLimit {
		n.lobbyEvents = n.lobbyEvents[1:]
	}
	n.lobbyEvents = append(n.lobbyEvents, ev)
}

// resetLobbyLocked forgets the state and events of the previous match.
func (n *NakamaClient) resetLobbyLocked() {
	n.lobbyState = nil
	n.lobbyStateMatch = ""
	n.lobbyStateVer++
	n.lobbyEvents = nil
}

// LobbyState returns the latest state of the lobby this client is in and a
// version that changes whenever the state does (including when it is
// cleared). ok is false when the current match has sent no lobby state.
func (n *NakamaClient) LobbyState() (state map[string]any, version int64, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lobbyState == nil || n.lobbyStateMatch == "" || n.lobbyStateMatch != n.matchID {
		return nil, n.lobbyStateVer, false
	}
	return n.lobbyState, n.lobbyStateVer, true
}

// LobbyEvents returns and clears the queued lobby events, oldest first.
func (n *NakamaClient) LobbyEvents() []map[string]any {
	n.mu.Lock()
	defer n.mu.Unlock()
	events := n.lobbyEvents
	n.lobbyEvents = nil
	return events
}

// SendLobbyCommand sends a command to the lobby module, for example
// SendLobbyCommand("ready", map[string]any{"ready": true}).
func (n *NakamaClient) SendLobbyCommand(kind string, fields map[string]any) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return errors.New("lobby command kind is empty")
	}
	body := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		body[k] = v
	}
	body["kind"] = kind
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return n.SendMatchData(nakamaLobbyCommandOp, data, true)
}

// ReportLobbyResultSeq reports the winner of the pairing with sequence
// number seq. Reports for an older pairing are ignored by the lobby.
func (n *NakamaClient) ReportLobbyResultSeq(winnerID, loserID string, seq int64) error {
	winnerID = strings.TrimSpace(winnerID)
	loserID = strings.TrimSpace(loserID)
	if winnerID == "" || loserID == "" || winnerID == loserID {
		return errors.New("lobby result requires distinct winner and loser user ids")
	}
	body, err := json.Marshal(map[string]any{
		"kind":   "match_result",
		"winner": winnerID,
		"loser":  loserID,
		"seq":    seq,
	})
	if err != nil {
		return err
	}
	return n.SendMatchData(nakamaLobbyResultOp, body, true)
}

// CreateLobbyWithCode creates a lobby and returns its match id and room ID.
func (n *NakamaClient) CreateLobbyWithCode(ctx context.Context, cfg NakamaLobbyConfig) (matchID, code string, err error) {
	body, err := n.createLobbyRPC(ctx, cfg)
	if err != nil {
		return "", "", err
	}
	var result struct {
		MatchID string `json:"match_id"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", "", err
	}
	if result.MatchID == "" {
		return "", "", errors.New("lobby creation returned an empty match id")
	}
	return result.MatchID, result.Code, nil
}

// FindLobby resolves a room ID to the lobby's match id.
func (n *NakamaClient) FindLobby(ctx context.Context, code string) (string, error) {
	body, err := n.CallRPC(ctx, "ikemen_lobby_find", map[string]string{"code": code})
	if err != nil {
		return "", err
	}
	var result struct {
		MatchID string `json:"match_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	if result.MatchID == "" {
		return "", errors.New("no lobby has that room ID")
	}
	return result.MatchID, nil
}

// Ping measures the round trip to the server with a realtime ping; the
// result is available from Status().RTT once the pong arrives.
func (n *NakamaClient) Ping() error {
	cid := n.nextCID()
	n.mu.Lock()
	n.pingCID = cid
	n.pingSent = time.Now()
	n.mu.Unlock()
	return n.sendEnvelopeCID(cid, "ping", map[string]any{})
}

// applyPong completes a Ping when its answer arrives.
func (n *NakamaClient) applyPong(raw map[string]json.RawMessage) {
	var cid string
	if v, ok := raw["cid"]; ok {
		_ = json.Unmarshal(v, &cid)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if cid != "" && cid == n.pingCID {
		n.rtt = time.Since(n.pingSent)
		n.pingCID = ""
	}
}

// decodeLobbyLabels adds label_data, the decoded label, to each listed
// match whose label is a JSON object.
func decodeLobbyLabels(matches []map[string]any) {
	for _, m := range matches {
		label, ok := m["label"].(string)
		if !ok || label == "" {
			continue
		}
		var data map[string]any
		if json.Unmarshal([]byte(label), &data) == nil {
			m["label_data"] = data
		}
	}
}
