package main

// The server's relay of a P2P session (nakama_relay.go,
// nakama/modules/ikemen_relay.lua) as the client asks for it, joins it and
// keeps it to itself, against a stand-in for the server's RPC and realtime
// answers. The module itself is tested in nakama_relay_lua_test.go, and both
// against Nakama in nakama_relay_live_test.go.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testServerRelayID = "3b4e28ba-2fa1-11d2-883f-0016d3cca427.nakama1"

// serverRelayClient is a relayTestClient, signed in as "me", whose RPCs go to
// rpc. It records the bodies of the RPCs it receives.
func serverRelayClient(t *testing.T, rpc func(w http.ResponseWriter, body map[string]string)) (*NakamaClient, chan []byte, func() []map[string]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/rpc/"+nakamaRelayRPC || r.Header.Get("Authorization") != "Bearer token" {
			http.Error(w, `{"code":5,"message":"RPC function not found"}`, http.StatusNotFound)
			return
		}
		var body map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		calls = append(calls, body)
		mu.Unlock()
		rpc(w, body)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	n, q := relayTestClient(t, 64)
	n.mu.Lock()
	n.cfg.Host, n.cfg.Port, n.cfg.RequestTTL = u.Hostname(), port, 2*time.Second
	n.session = &NakamaSession{Token: "token", UserID: "me"}
	n.mu.Unlock()
	return n, q, func() []map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]string(nil), calls...)
	}
}

func answerRelay(id string) func(http.ResponseWriter, map[string]string) {
	return func(w http.ResponseWriter, _ map[string]string) {
		fmt.Fprintf(w, `{"match_id":%q}`, id)
	}
}

type relayJoin struct {
	link p2pRelayLink
	err  error
}

// joinRelayAsync runs JoinRelay and returns where its result arrives, and
// where the datagrams it delivers do.
func joinRelayAsync(n *NakamaClient, req p2pRelayRequest) (chan relayJoin, func() []string) {
	var mu sync.Mutex
	var got []string
	joined := make(chan relayJoin, 1)
	go func() {
		link, err := n.JoinRelay(req, func(b []byte) {
			mu.Lock()
			got = append(got, string(b))
			mu.Unlock()
		})
		joined <- relayJoin{link, err}
	}()
	return joined, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func noEnvelope(t *testing.T, q chan []byte, what string) {
	t.Helper()
	select {
	case body := <-q:
		t.Fatalf("%s: sent %s", what, body)
	case <-time.After(200 * time.Millisecond):
	}
}

var testServerRelayRequest = p2pRelayRequest{MatchID: "session-match", Name: "ikemen-p2p-relay-x", Peer: "peer", Server: true}

// With the peer's game offering it, the client asks the session's match for
// the server's relay and joins that match by its id; everything that comes
// from it is the relay's, before the join's answer and after the relay was
// left, and only the peer's relay data reaches the session.
func TestNakamaServerRelayJoin(t *testing.T) {
	n, q, calls := serverRelayClient(t, answerRelay(testServerRelayID))
	rec := newRecorder()
	n.SetEventHandler(rec.fn)
	data := func(matchID, user string, op uint32, payload string) []byte {
		return []byte(fmt.Sprintf(`{"match_data":{"match_id":"%s","op_code":"%d","data":"%s","presence":{"user_id":"%s"}}}`,
			matchID, op, base64.StdEncoding.EncodeToString([]byte(payload)), user))
	}
	decision := `{"kind":"ikemen-rematch-decision","version":1,"round":999,"action":"exit"}`

	joined, got := joinRelayAsync(n, testServerRelayRequest)
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	var join struct {
		MatchID string `json:"match_id"`
	}
	if !strings.HasPrefix(cid, "ikemen-relay-") || json.Unmarshal(env["match_join"], &join) != nil || join.MatchID != testServerRelayID || env["match_create"] != nil {
		t.Fatalf("join request %s, want a match_join of the server's relay", env)
	}
	if c := calls(); len(c) != 1 || c[0]["match_id"] != "session-match" || c[0]["peer_id"] != "peer" {
		t.Fatalf("relay RPC calls %v", c)
	}
	// The relay's messages are its own before the answer to the join too.
	n.handleJSON([]byte(`{"match_presence_event":{"match_id":"` + testServerRelayID + `","joins":[{"user_id":"peer"}]}}`))
	n.handleJSON(data(testServerRelayID, "peer", nakamaRematchDecisionOp, decision))

	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"` + testServerRelayID + `","authoritative":true,"size":2}}`))
	r := <-joined
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n.CurrentMatchID() != "session-match" {
		t.Fatalf("joining the relay made it the client's match: %q", n.CurrentMatchID())
	}
	n.handleJSON(data(testServerRelayID, "peer", nakamaRelayOp, "from the peer"))
	n.handleJSON(data(testServerRelayID, "someone-else", nakamaRelayOp, "from a stranger"))
	n.handleJSON(data(testServerRelayID, "peer", nakamaRematchDecisionOp, decision))
	n.handleJSON(data(testServerRelayID, "peer", 0x494B_4C12, `{"kind":"state","state":{"phase":"closed"}}`))
	if g := got(); len(g) != 1 || g[0] != "from the peer" {
		t.Fatalf("delivered %q, want only the peer's relay data", g)
	}

	if err := r.link.Send([]byte("to the peer")); err != nil {
		t.Fatal(err)
	}
	env = nextEnvelope(t, q)
	var sent struct {
		MatchID string `json:"match_id"`
		OpCode  uint32 `json:"op_code"`
		Data    []byte `json:"data"`
	}
	if err := json.Unmarshal(env["match_data_send"], &sent); err != nil || sent.MatchID != testServerRelayID ||
		sent.OpCode != nakamaRelayOp || string(sent.Data) != "to the peer" {
		t.Fatalf("relay send %s (%v)", env, err)
	}
	_ = r.link.Close()
	env = nextEnvelope(t, q)
	if !bytes.Contains(env["match_leave"], []byte(`"`+testServerRelayID+`"`)) {
		t.Fatalf("close sent %s, want a match_leave of the relay", env)
	}
	// What the relay sent before the server took the client out of it.
	n.handleJSON(data(testServerRelayID, "peer", nakamaRelayOp, "after the leave"))
	n.handleJSON(data(testServerRelayID, "peer", nakamaRematchDecisionOp, decision))
	n.dropRelays(errors.New("the connection to the server ended"))
	n.handleJSON(data(testServerRelayID, "peer", nakamaRematchDecisionOp, decision))
	if g := got(); len(g) != 1 {
		t.Fatalf("a relay left delivered %q", g)
	}
	rec.mu.Lock()
	evs := append([]NakamaEvent(nil), rec.evs...)
	rec.mu.Unlock()
	if len(evs) != 0 {
		t.Fatalf("the server relay's messages reached the client's handlers: first %q", evs[0].Type)
	}
	if _, _, ok := n.LobbyState(); ok {
		t.Fatal("the server relay set the lobby state")
	}
	// The client's own match still gets through.
	n.handleJSON(data("session-match", "peer", nakamaRematchDecisionOp, decision))
	if _, ok := rec.waitFor(t, time.Second, func(ev NakamaEvent) bool { return ev.Type == "rematch_decision" }); !ok {
		t.Fatal("the client's own match data was dropped")
	}
}

// A server without the relay module has no ikemen_relay RPC: the client joins
// the relayed match by its name instead, as the peer's game does on the same
// server.
func TestNakamaServerRelayFallsBackWithoutTheModule(t *testing.T) {
	n, q, calls := serverRelayClient(t, func(w http.ResponseWriter, _ map[string]string) {
		http.Error(w, `{"code":5,"message":"RPC function not found"}`, http.StatusNotFound)
	})
	joined, _ := joinRelayAsync(n, testServerRelayRequest)
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	if !bytes.Contains(env["match_create"], []byte(`"ikemen-p2p-relay-x"`)) || len(calls()) != 1 {
		t.Fatalf("join request %s after %d relay RPC calls, want the relayed match by name", env, len(calls()))
	}
	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"relay-1.","size":2}}`))
	if r := <-joined; r.err != nil {
		t.Fatal(r.err)
	}
}

// A peer whose game does not offer the server's relay joins the relayed match
// by name only: so does this client, without asking the server.
func TestNakamaRelayedMatchForAnOlderPeer(t *testing.T) {
	n, q, calls := serverRelayClient(t, answerRelay(testServerRelayID))
	req := testServerRelayRequest
	req.Server = false
	joined, _ := joinRelayAsync(n, req)
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	if !bytes.Contains(env["match_create"], []byte(`"ikemen-p2p-relay-x"`)) || len(calls()) != 0 {
		t.Fatalf("join request %s after %d relay RPC calls", env, len(calls()))
	}
	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"relay-1.","size":2}}`))
	if r := <-joined; r.err != nil {
		t.Fatal(r.err)
	}
}

// A server with the relay module that refuses the relay, or names something
// else, does not make the client fall back to a relayed match, which anybody
// could join: the join fails, and the session tries again later.
func TestNakamaServerRelayRefusalIsNoFallback(t *testing.T) {
	for _, tc := range []struct {
		name string
		rpc  func(http.ResponseWriter, map[string]string)
	}{
		{"not paired", func(w http.ResponseWriter, _ map[string]string) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":9,"message":"the session or lobby did not pair you with peer_id"}`))
		}},
		{"the session's own match", answerRelay("session-match")},
		{"a relayed match", answerRelay("4b4e28ba-2fa1-11d2-883f-0016d3cca427.")},
		{"no id", answerRelay("")},
		{"not JSON", func(w http.ResponseWriter, _ map[string]string) { _, _ = w.Write([]byte("relay")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, q, _ := serverRelayClient(t, tc.rpc)
			joined, _ := joinRelayAsync(n, testServerRelayRequest)
			select {
			case r := <-joined:
				if r.err == nil {
					t.Fatal("joined a relay the server did not give")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("JoinRelay did not return")
			}
			noEnvelope(t, q, "a refused relay")
			n.relayMu.Lock()
			held := len(n.relayByCID) + len(n.relayByMatch) + len(n.relayServer)
			n.relayMu.Unlock()
			if held != 0 {
				t.Fatalf("the client holds %d relays", held)
			}
		})
	}
}

// The answer to a join of a server relay that the client gave up on keeps the
// client in the relay: leaving it would end it for the peer, and the
// session's next join, which the server answers with the same relay while it
// runs, finds the client in it.
func TestNakamaServerRelayLateAnswerStays(t *testing.T) {
	n, q, _ := serverRelayClient(t, answerRelay(testServerRelayID))
	n.relayMu.Lock()
	n.relayGivenUp = map[string]string{"ikemen-relay-first": "ikemen-p2p-relay-x"}
	n.relayServer = map[string]struct{}{testServerRelayID: {}}
	n.relayMu.Unlock()
	n.handleJSON([]byte(`{"cid":"ikemen-relay-first","match":{"match_id":"` + testServerRelayID + `","authoritative":true,"size":2}}`))
	noEnvelope(t, q, "the late answer of a server relay join")
	if n.CurrentMatchID() != "session-match" {
		t.Fatalf("the late answer made the relay the client's match: %q", n.CurrentMatchID())
	}
	joined, got := joinRelayAsync(n, testServerRelayRequest)
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"` + testServerRelayID + `","authoritative":true,"size":2}}`))
	if r := <-joined; r.err != nil {
		t.Fatal(r.err)
	}
	n.handleJSON([]byte(fmt.Sprintf(`{"match_data":{"match_id":"%s","op_code":"%d","data":"%s","presence":{"user_id":"peer"}}}`,
		testServerRelayID, nakamaRelayOp, base64.StdEncoding.EncodeToString([]byte("hello")))))
	if g := got(); len(g) != 1 || g[0] != "hello" {
		t.Fatalf("the joined relay delivered %q", g)
	}
}

// Nakama's answer to a call of an RPC nobody registered, and only that, says
// the server has no relay module.
func TestNakamaRPCMissing(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{newNakamaRPCError(nakamaRelayRPC, 404, []byte(`{"code":5,"message":"RPC function not found"}`)), true},
		{newNakamaRPCError(nakamaRelayRPC, 404, []byte("not found")), true},
		{fmt.Errorf("wrapped: %w", newNakamaRPCError(nakamaRelayRPC, 404, nil)), true},
		{newNakamaRPCError(nakamaRelayRPC, 400, []byte(`{"code":9,"message":"not paired"}`)), false},
		{newNakamaRPCError(nakamaRelayRPC, 500, []byte(`{"code":13,"message":"the relay could not be made"}`)), false},
		{errors.New("connection refused"), false},
	} {
		if got := nakamaRPCMissing(tc.err); got != tc.want {
			t.Errorf("nakamaRPCMissing(%v) = %v", tc.err, got)
		}
	}
}

// A game's signal offers the server's relay, and the session asks for it only
// when the peer's signal does too: an older game joins only the relayed match.
func TestP2PRelayRequestFollowsThePeersSignal(t *testing.T) {
	for _, peerOffers := range []bool{true, false} {
		hub := newFakeRelayHub()
		p, err := NewNakamaP2P("session-match", 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		p.SetPeer("me", "opponent")
		body, _ := json.Marshal(p.localSignal())
		if !bytes.Contains(body, []byte(`"server_relay":true`)) {
			t.Fatalf("the signal %s does not offer the server's relay", body)
		}
		peer := P2PSignal{Magic: p2pMagic, MatchID: "session-match", Token: "opponent-token", Key: testP2PKey(t), From: "opponent", To: "me", ServerRelay: peerOffers}
		body, _ = json.Marshal(peer)
		if bytes.Contains(body, []byte("server_relay")) != peerOffers {
			t.Fatalf("signal %s", body)
		}
		var decoded P2PSignal
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		if err := p.HandleSignal(decoded); err != nil {
			t.Fatal(err)
		}
		p.joinRelay(hub.relayer("me"))
		hub.mu.Lock()
		reqs := append([]p2pRelayRequest(nil), hub.requests...)
		hub.mu.Unlock()
		want := p2pRelayRequest{MatchID: "session-match", Name: p.keys.relayName, Peer: "opponent", Server: peerOffers}
		if len(reqs) != 1 || reqs[0] != want {
			t.Fatalf("relay requests %+v, want %+v", reqs, want)
		}
		p.Close()
	}
}
