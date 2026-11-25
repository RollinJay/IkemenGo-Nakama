package main

// Connection telemetry: the "link" report of the route a P2P session runs
// over (nakama_watch.go reportLink), and what the lobby and session modules
// do with it (nakama/modules/ikemen_telemetry.lua): keep it, show it in the
// lobby's state, and log one line per pairing and change of route.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yuin/gopher-lua/parse"
)

func TestLinkPayload(t *testing.T) {
	got := linkPayload("peer-1", 42*time.Millisecond+400*time.Microsecond, "direct-srflx", 1)
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"peer": "peer-1", "rtt": 42.0, "route": "direct-srflx", "upgrades": 1.0}
	if len(back) != len(want) {
		t.Fatalf("payload %s", b)
	}
	for k, v := range want {
		if back[k] != v {
			t.Fatalf("payload %s: %s is %v, want %v", b, k, back[k], v)
		}
	}
}

func TestLinkReportDue(t *testing.T) {
	now := time.Unix(1000, 0)
	ms := time.Millisecond
	sent := linkReport{sent: true, rtt: 50 * ms, route: "relay-ws", upgrades: 0, at: now.Add(-time.Second)}
	old := sent
	old.at = now.Add(-linkReportInterval)
	cases := []struct {
		name     string
		last     linkReport
		rtt      time.Duration
		samples  int
		route    string
		upgrades int
		lobby    bool
		want     bool
	}{
		{"too few pings", linkReport{}, 50 * ms, linkReportSamples - 1, "relay-ws", 0, true, false},
		{"no route yet", linkReport{}, 50 * ms, linkReportSamples, "", 0, true, false},
		{"first report, lobby", linkReport{}, 50 * ms, linkReportSamples, "relay-ws", 0, true, true},
		{"first report, session", linkReport{}, 50 * ms, linkReportSamples, "relay-ws", 0, false, true},
		{"same, soon", sent, 50 * ms, 9, "relay-ws", 0, true, false},
		{"route changed, at once", sent, 50 * ms, 9, "direct-srflx", 1, true, true},
		{"route changed, session", sent, 20 * ms, 9, "direct-srflx", 1, false, true},
		{"upgrades changed", sent, 50 * ms, 9, "relay-ws", 2, false, true},
		{"rtt moved too soon", sent, 90 * ms, 9, "relay-ws", 0, true, false},
		{"rtt moved, lobby", old, 90 * ms, 9, "relay-ws", 0, true, true},
		{"rtt moved, session", old, 90 * ms, 9, "relay-ws", 0, false, false},
		{"rtt moved a little", old, 55 * ms, 9, "relay-ws", 0, true, false},
	}
	for _, c := range cases {
		if got := linkReportDue(c.last, c.rtt, c.samples, c.route, c.upgrades, c.lobby, now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTelemetryModulesParse(t *testing.T) {
	for _, name := range []string{"ikemen_telemetry.lua", "ikemen_lobby.lua", "ikemen_session.lua"} {
		path := filepath.Join("..", "nakama", "modules", name)
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parse.Parse(f, path)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
}

// The lobby keeps the route of the current pairing, shows it in its state
// and logs one line when the pairing's route is first reported and one when
// it changes, whichever player reports it.
func TestTelemetryLobbyLogsEachPairingsRoute(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, `
OP_LOBBY_COMMAND = 0x494B4C13
OP_LOBBY_STATE = 0x494B4C12
local lobby = require("ikemen_lobby")
local ctx = { match_id = "lobby.node", env = {} }
lstate = lobby.match_init(ctx, { creator = "a", game = "", build = "" })
local d = new_dispatcher()
d.match_label_update = function() end
lstate = lobby.match_join(ctx, d, 1, lstate, { presence("a"), presence("b"), presence("c") })
lstate.current_match = { player1 = "a", player2 = "b" }
lstate.match_seq = 4
lstate.phase = "playing"
lstate.phase_tick = 1

local function dispatcher()
    local d = new_dispatcher()
    d.match_label_update = function() end
    return d
end
function send_link(from, body)
    d = dispatcher()
    lstate = lobby.match_loop(ctx, d, 2, lstate, { message(presence(from), body, OP_LOBBY_COMMAND) })
    local current = nil
    for _, m in ipairs(d.sent) do
        if m.op == OP_LOBBY_STATE then
            current = nk_decode(m.data).current
        end
    end
    return current
end
nk_decode = require("nakama").json_decode

-- An older game: no route, nothing logged, no route in the state.
local cur = send_link("a", '{"kind":"link","peer":"b","rtt":40}')
check(cur ~= nil and cur.link == 40 and cur.link_route == "" and cur.link_kind == "", "older report: %s", require("nakama").json_encode(cur or {}))
-- Telemetry: first route, then the other player, then a change.
cur = send_link("a", '{"kind":"link","peer":"b","rtt":90,"route":"relay-ws","upgrades":0}')
check(cur ~= nil and cur.link_route == "relay-ws" and cur.link_kind == "relay", "relay report")
send_link("b", '{"kind":"link","peer":"a","rtt":91,"route":"relay-ws","upgrades":0}')
cur = send_link("b", '{"kind":"link","peer":"a","rtt":30,"route":"direct-prflx","upgrades":1}')
check(cur ~= nil and cur.link_route == "direct-prflx" and cur.link_kind == "direct" and cur.link == 30, "direct report")
send_link("a", '{"kind":"link","peer":"b","rtt":31,"route":"direct-prflx","upgrades":1}')
-- Not playing, or not a route: ignored.
send_link("c", '{"kind":"link","peer":"a","rtt":31,"route":"direct-host"}')
send_link("a", '{"kind":"link","peer":"b","rtt":31,"route":"Direct host\nfake"}')
-- The next pairing of the same players logs again.
lstate.match_seq = 5
send_link("b", '{"kind":"link","peer":"a","rtt":30,"route":"direct-prflx","upgrades":0}')
`)
	if n := m.logged("logger_info", "ikemen lobby: pairing"); n != 3 {
		t.Fatalf("%d lines, want 3: %q", n, m.logs)
	}
	for _, line := range []string{
		"ikemen lobby: pairing seq=4 a-b route=relay-ws rtt=90 upgrades=0 id=lobby.node",
		"ikemen lobby: pairing seq=4 a-b route=direct-prflx rtt=30 upgrades=1 id=lobby.node",
		"ikemen lobby: pairing seq=5 a-b route=direct-prflx rtt=30 upgrades=0 id=lobby.node",
	} {
		if m.logged("logger_info", line) != 1 {
			t.Errorf("missing %q in %q", line, m.logs)
		}
	}
}

// A session logs the route its players report and passes the report on to
// nobody; its other messages still go to the other player.
func TestTelemetrySessionLogsRoute(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, `
OP_LOBBY_COMMAND = 0x494B4C13
local session = require("ikemen_session")
local ctx = { match_id = "session.node", env = {} }
local expected = { { presence = presence("a") }, { presence = presence("b") } }
local s = session.match_init(ctx, { mode = "unranked", expected_users = expected })
local d = new_dispatcher()
s = session.match_join(ctx, d, 1, s, { presence("a"), presence("b") })
local function send(from, body, op)
    d = new_dispatcher()
    s = session.match_loop(ctx, d, 2, s, { message(presence(from), body, op or OP_LOBBY_COMMAND) })
    return #d.sent
end
check(send("a", '{"kind":"link","peer":"b","rtt":40}') == 0, "an older report was passed on")
check(send("a", '{"kind":"link","peer":"b","rtt":80,"route":"relay-udp","upgrades":0}') == 0, "a report was passed on")
send("b", '{"kind":"link","peer":"a","rtt":81,"route":"relay-udp","upgrades":0}')
send("b", '{"kind":"link","peer":"a","rtt":20,"route":"direct-srflx","upgrades":1}')
send("a", '{"kind":"link","peer":"stranger","rtt":20,"route":"direct-host"}')
check(send("a", "other", 7) == 1, "other data was not passed on")
`)
	if n := m.logged("logger_info", "ikemen session: pairing"); n != 2 {
		t.Fatalf("%d lines, want 2: %q", n, m.logs)
	}
	for _, line := range []string{
		"ikemen session: pairing seq=1 a-b route=relay-udp rtt=80 upgrades=0 id=session.node",
		"ikemen session: pairing seq=1 a-b route=direct-srflx rtt=20 upgrades=1 id=session.node",
	} {
		if m.logged("logger_info", line) != 1 {
			t.Errorf("missing %q in %q", line, m.logs)
		}
	}
}

// Live (NAKAMA_LIVE=1, see nakama_live_test.go): a lobby keeps the route a
// player of the current match reports and shows it to the members.
func TestNakamaLive_LobbyLinkRoute(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	join(t, b, recB, id)
	ready(t, a, true)
	ready(t, b, true)
	waitLobby(t, b, "playing", lobbyPhase("playing"))
	peer := b.Session().UserID
	if err := a.SendLobbyCommand("link", linkPayload(peer, 88*time.Millisecond, "relay-ws", 0)); err != nil {
		t.Fatal(err)
	}
	waitLobby(t, b, "relay route", func(st map[string]any) bool {
		cur, _ := st["current"].(map[string]any)
		return cur != nil && cur["link_route"] == "relay-ws" && cur["link_kind"] == "relay" && cur["link"] == 88.0
	})
	if err := b.SendLobbyCommand("link", linkPayload(a.Session().UserID, 31*time.Millisecond, "direct-srflx", 1)); err != nil {
		t.Fatal(err)
	}
	waitLobby(t, a, "direct route", func(st map[string]any) bool {
		cur, _ := st["current"].(map[string]any)
		return cur != nil && cur["link_route"] == "direct-srflx" && cur["link_kind"] == "direct" && cur["link"] == 31.0
	})
}

// Live: a matchmaker session takes its players' "link" reports (it logs
// them) and does not pass them on to the other player.
func TestNakamaLive_SessionLinkRouteNotRelayed(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "unranked", Game: "g-telemetry", GameBuild: "1.0.0", Region: "r"}
	_ = a.Matchmake(req)
	_ = b.Matchmake(req)
	ev, ok := recA.waitFor(t, 20*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	if !ok {
		t.Fatal("no match")
	}
	evb, _ := recB.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	join(t, a, recA, ev.Matchmaker.MatchID)
	join(t, b, recB, evb.Matchmaker.MatchID)
	if err := a.SendLobbyCommand("link", linkPayload(b.Session().UserID, 40*time.Millisecond, "direct-host", 0)); err != nil {
		t.Fatal(err)
	}
	// Other data still reaches the other player, after the report.
	if err := a.SendMatchData(0x7E57, []byte(`{"probe":1}`), true); err != nil {
		t.Fatal(err)
	}
	if _, ok := recB.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool {
		return ev.Type == "match_data" && ev.MatchData != nil && ev.MatchData.OpCode == 0x7E57
	}); !ok {
		t.Fatal("the probe did not arrive")
	}
	if got := matchDataEvents(recB, nakamaLobbyCommandOp); len(got) != 0 {
		t.Fatalf("the session passed the link report on: %d messages", len(got))
	}
}
