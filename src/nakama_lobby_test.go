package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func TestNormalizeNakamaLobbyFormat(t *testing.T) {
	tests := map[string]string{
		"":                "queue",
		"queue":           "queue",
		"QUEUE":           "queue",
		"winner_stays":    "winner_stays_on",
		"winner_stays_on": "winner_stays_on",
		"round_robin":     "round_robin",
		"bracket":         "bracket",
		"unknown":         "queue",
	}
	for input, want := range tests {
		if got := normalizeNakamaLobbyFormat(input); got != want {
			t.Fatalf("format %q = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeNakamaLobbyMaxGames(t *testing.T) {
	tests := map[int]int{-1: 3, 0: 3, 1: 1, 3: 3, 99: 99, 100: 99}
	for input, want := range tests {
		if got := normalizeNakamaLobbyMaxGames(input); got != want {
			t.Fatalf("max games %d = %d, want %d", input, got, want)
		}
	}
}

func lobbyTestClient() *NakamaClient {
	return NewNakamaClient(NakamaConfig{Host: "127.0.0.1", Port: 7350, ServerKey: "defaultkey", DeviceID: "test-device"})
}

// The client keeps the latest state of the lobby it is in; a state from
// another match is not reported as the current lobby's.
func TestLobbyStateFollowsCurrentMatch(t *testing.T) {
	n := lobbyTestClient()
	n.matchID = "m1"
	if _, _, ok := n.LobbyState(); ok {
		t.Fatal("state before any lobby message")
	}
	n.applyLobbyMessage("m1", nakamaLobbyStateOp, map[string]any{"kind": "lobby_state", "phase": "waiting"})
	st, v1, ok := n.LobbyState()
	if !ok || st["phase"] != "waiting" {
		t.Fatalf("state %v ok=%v", st, ok)
	}
	n.applyLobbyMessage("m1", nakamaLobbyStateOp, map[string]any{"kind": "lobby_state", "phase": "playing"})
	st, v2, _ := n.LobbyState()
	if v2 == v1 || st["phase"] != "playing" {
		t.Fatalf("second state %v, version %d -> %d", st, v1, v2)
	}
	n.applyLobbyMessage("m2", nakamaLobbyStateOp, map[string]any{"kind": "lobby_state", "phase": "results"})
	if _, _, ok := n.LobbyState(); ok {
		t.Fatal("a state from another match was reported for the current one")
	}
	n.mu.Lock()
	n.resetLobbyLocked()
	n.mu.Unlock()
	if _, v3, ok := n.LobbyState(); ok || v3 == v2 {
		t.Fatalf("after reset: ok=%v version %d", ok, v3)
	}
}

// Events queue up in order, are capped, and the first protocol's error on
// the state op code becomes an error event.
func TestLobbyEventsQueue(t *testing.T) {
	n := lobbyTestClient()
	n.applyLobbyMessage("m", nakamaLobbyStateOp, map[string]any{"kind": "lobby_error", "error": "bad result"})
	for i := 0; i < nakamaLobbyEventLimit+5; i++ {
		n.applyLobbyMessage("m", nakamaLobbyEventOp, map[string]any{"kind": "chat", "text": i})
	}
	events := n.LobbyEvents()
	if len(events) != nakamaLobbyEventLimit {
		t.Fatalf("%d events queued", len(events))
	}
	if events[len(events)-1]["text"] != nakamaLobbyEventLimit+4 {
		t.Fatalf("newest event %v", events[len(events)-1])
	}
	if len(n.LobbyEvents()) != 0 {
		t.Fatal("events not cleared")
	}
	n.applyLobbyMessage("m", nakamaLobbyStateOp, map[string]any{"kind": "lobby_error", "error": "bad result"})
	if ev := n.LobbyEvents(); len(ev) != 1 || ev[0]["kind"] != "error" || ev[0]["error"] != "bad result" {
		t.Fatalf("legacy error event %v", ev)
	}
}

func TestDecodeLobbyLabels(t *testing.T) {
	matches := []map[string]any{
		{"match_id": "a", "label": `{"kind":"ikemen-lobby","name":"x","players":2}`},
		{"match_id": "b", "label": "not json"},
		{"match_id": "c"},
	}
	decodeLobbyLabels(matches)
	d, _ := matches[0]["label_data"].(map[string]any)
	if d["name"] != "x" || d["players"] != float64(2) {
		t.Fatalf("label_data %v", matches[0]["label_data"])
	}
	if _, ok := matches[1]["label_data"]; ok {
		t.Fatal("label_data for a label that is not JSON")
	}
}

func TestPongMeasuresRoundTrip(t *testing.T) {
	n := lobbyTestClient()
	n.pingCID = "p1"
	n.pingSent = time.Now().Add(-50 * time.Millisecond)
	n.applyPong(map[string]json.RawMessage{"cid": json.RawMessage(`"other"`), "pong": json.RawMessage(`{}`)})
	if n.Status().RTT != 0 {
		t.Fatal("a pong for another request was used")
	}
	n.applyPong(map[string]json.RawMessage{"cid": json.RawMessage(`"p1"`), "pong": json.RawMessage(`{}`)})
	if rtt := n.Status().RTT; rtt < 50*time.Millisecond || rtt > 5*time.Second {
		t.Fatalf("round trip %v", rtt)
	}
}

func testSessionToken(uid, username string) string {
	enc := func(v string) string { return base64.RawURLEncoding.EncodeToString([]byte(v)) }
	claims := fmt.Sprintf(`{"uid":%q,"usn":%q,"exp":%d}`, uid, username, time.Now().Add(time.Hour).Unix())
	return enc(`{"alg":"HS256"}`) + "." + enc(claims) + ".sig"
}

// The profile's username is sent where Nakama reads it (the query string);
// a taken username falls back to a generated one instead of failing.
func TestAuthenticateSendsUsername(t *testing.T) {
	var queries []url.Values
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		queries = append(queries, r.URL.Query())
		bodies = append(bodies, string(body))
		if r.URL.Query().Get("username") == "Taken" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"Username is already in use.","code":6}`))
			return
		}
		name := r.URL.Query().Get("username")
		if name == "" {
			name = "Generated"
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"token":%q,"refresh_token":"r"}`, testSessionToken("u1", name))))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	for _, tc := range []struct{ username, want string }{{"KaiserWave", "KaiserWave"}, {"Taken", "Generated"}, {"", "Generated"}} {
		queries, bodies = nil, nil
		n := NewNakamaClient(NakamaConfig{Host: u.Hostname(), Port: port, ServerKey: "k", DeviceID: "device-1234567890", Username: tc.username, RequestTTL: time.Second})
		if err := n.Authenticate(context.Background()); err != nil {
			t.Fatalf("%q: %v", tc.username, err)
		}
		if got := n.Session().Username; got != tc.want {
			t.Fatalf("%q: username %q, want %q", tc.username, got, tc.want)
		}
		if queries[0].Get("create") != "true" || queries[0].Get("username") != tc.username {
			t.Fatalf("%q: query %v", tc.username, queries[0])
		}
		if strings.Contains(bodies[0], "username") {
			t.Fatalf("%q: username in the body %s", tc.username, bodies[0])
		}
	}
}

// nakama.on(event, fn, name) keeps a named handler beside the unnamed one, so
// a script that sets the unnamed handler does not replace the lobby screens'
// handlers; on(event, nil, name) removes it.
func TestNakamaLuaHandlersNamedAndUnnamed(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	if err := l.DoString(`calls = {}
		function a() table.insert(calls, "a") end
		function b() table.insert(calls, "b") end
		function c() table.insert(calls, "c") end`); err != nil {
		t.Fatal(err)
	}
	fn := func(name string) *lua.LFunction { return l.GetGlobal(name).(*lua.LFunction) }
	calls := func() string {
		var out []string
		l.GetGlobal("calls").(*lua.LTable).ForEach(func(_, v lua.LValue) { out = append(out, v.String()) })
		l.SetGlobal("calls", l.NewTable())
		return strings.Join(out, ",")
	}
	h := newNakamaLuaHandlers()
	h.set("lobby_list", "", fn("a"))
	h.set("lobby_list", "lobby", fn("b"))
	h.set("lobby_list", "", fn("c"))
	nakamaDispatchLuaEvent(l, h, NakamaEvent{Type: "lobby_list"})
	if got := calls(); got != "c,b" {
		t.Fatalf("handlers called %q, want c,b", got)
	}
	h.set("lobby_list", "lobby", nil)
	nakamaDispatchLuaEvent(l, h, NakamaEvent{Type: "lobby_list"})
	if got := calls(); got != "c" {
		t.Fatalf("after removing the named handler: %q, want c", got)
	}
	nakamaDispatchLuaEvent(l, h, NakamaEvent{Type: "lobby_created"})
	if got := calls(); got != "" {
		t.Fatalf("an event without handlers called %q", got)
	}
}
