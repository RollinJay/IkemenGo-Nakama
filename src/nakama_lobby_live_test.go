package main

// Live tests of the lobby module (nakama/modules/ikemen_lobby.lua): room
// codes and private lobbies, ready states and scheduling, host actions,
// chat, P2P signal routing and tournaments. Enable with NAKAMA_LIVE=1 (see
// nakama_live_test.go for the server flags).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func createLobbyWith(t *testing.T, c *NakamaClient, settings map[string]any) (string, string) {
	t.Helper()
	id, code, err := c.CreateLobbyWithCode(context.Background(), NakamaLobbyConfig{Settings: settings})
	if err != nil {
		t.Fatalf("CreateLobbyWithCode: %v", err)
	}
	if len(code) != 6 {
		t.Fatalf("room ID %q", code)
	}
	return id, code
}

func joinWith(t *testing.T, c *NakamaClient, rec *evRecorder, matchID string, metadata map[string]string) {
	t.Helper()
	if err := c.JoinMatchWithMetadata(matchID, "", metadata); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "match_joined" }); !ok {
		t.Fatalf("no match_joined; last error %q", c.Status().LastError)
	}
}

func waitLobby(t *testing.T, c *NakamaClient, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last map[string]any
	for time.Now().Before(deadline) {
		if st, _, ok := c.LobbyState(); ok {
			last = st
			if pred(st) {
				return st
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := json.Marshal(last)
	t.Fatalf("lobby never reached %q; last state %s", what, b)
	return nil
}

func lobbyPhase(phase string) func(map[string]any) bool {
	return func(st map[string]any) bool { return st["phase"] == phase }
}

func currentPair(st map[string]any) (string, string, int) {
	cur, _ := st["current"].(map[string]any)
	if cur == nil {
		return "", "", 0
	}
	seq, _ := cur["seq"].(float64)
	return fmt.Sprint(cur["player1"]), fmt.Sprint(cur["player2"]), int(seq)
}

func memberField(st map[string]any, userID, field string) any {
	members, _ := st["members"].([]any)
	for _, m := range members {
		mm, _ := m.(map[string]any)
		if mm["user_id"] == userID {
			return mm[field]
		}
	}
	return nil
}

func waitLobbyEvent(t *testing.T, c *NakamaClient, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range c.LobbyEvents() {
			if pred(ev) {
				return ev
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no lobby event %q", what)
	return nil
}

func ready(t *testing.T, c *NakamaClient, on bool) {
	t.Helper()
	if err := c.SendLobbyCommand("ready", map[string]any{"ready": on}); err != nil {
		t.Fatal(err)
	}
}

func listLabel(t *testing.T, c *NakamaClient, matchID string) map[string]any {
	t.Helper()
	matches, err := c.ListMatches(context.Background(), true, "+label.kind:ikemen-lobby", "", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	decodeLobbyLabels(matches)
	for _, m := range matches {
		if m["match_id"] == matchID {
			label, _ := m["label_data"].(map[string]any)
			return label
		}
	}
	return nil
}

// A private lobby is found by its room ID only and refuses joins without it.
func TestNakamaLive_LobbyRoomIDAndPrivateJoin(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	id, code := createLobbyWith(t, a, map[string]any{"name": "secret room", "private": true})
	joinWith(t, a, recA, id, map[string]string{"code": code})

	found, err := b.FindLobby(context.Background(), strings.ToLower(code[:3])+"-"+code[3:])
	if err != nil || found != id {
		t.Fatalf("FindLobby(%s) = %q, %v; want %q", code, found, err, id)
	}
	if _, err := b.FindLobby(context.Background(), "ZZZZZZ"); err == nil {
		t.Fatal("an unknown room ID was found")
	}

	// Without the room ID the join is refused.
	b.ClearLastError()
	if err := b.JoinMatch(id, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := recB.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool { return ev.Type == "match_joined" }); ok {
		t.Fatal("joined a private lobby without its room ID")
	}
	if !strings.Contains(b.Status().LastError, "room ID") {
		t.Fatalf("join error %q", b.Status().LastError)
	}
	joinWith(t, b, recB, id, map[string]string{"code": code})
	st := waitLobby(t, a, "two members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 2
	})
	if st["code"] != code || st["host"] != a.Session().UserID {
		t.Fatalf("state code/host: %v %v", st["code"], st["host"])
	}
	label := listLabel(t, b, id)
	if label == nil || label["visibility"] != "private" || label["name"] != nil || label["code"] != nil {
		t.Fatalf("private lobby label shows details: %v", label)
	}
	t.Logf("private lobby %s: found by room ID, refused without it, label %v", code, label)
}

// A public lobby's label carries what lobby search shows.
func TestNakamaLive_LobbyPublicLabel(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, _ := liveClient(t)
	id, code := createLobbyWith(t, a, map[string]any{
		"name": "High Score Challenge", "comment": "Everyone is welcome!", "size": 6,
		"format": "winner_stays", "max_games": 5, "rounds": 3, "time": 99,
	})
	joinWith(t, a, recA, id, nil)
	if err := a.SendLobbyCommand("member", map[string]any{"region": "US", "ping": 40}); err != nil {
		t.Fatal(err)
	}
	var label map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		label = listLabel(t, b, id)
		if label != nil && label["players"] == float64(1) && label["region"] == "US" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	want := map[string]any{
		"name": "High Score Challenge", "comment": "Everyone is welcome!", "code": code,
		"visibility": "public", "size": float64(6), "players": float64(1), "format": "winner_stays_on",
		"max_games": float64(5), "rounds": float64(3), "time": float64(99), "phase": "waiting", "open": "yes",
		"region": "US",
	}
	for k, v := range want {
		if label[k] != v {
			t.Fatalf("label[%s] = %v, want %v (label %v)", k, label[k], v, label)
		}
	}
	if members, _ := label["members"].([]any); len(members) != 1 {
		t.Fatalf("label members %v", label["members"])
	}
}

// A lobby created with the first protocol's fields (name, format, max_games,
// max_players, rules.rounds) and no settings table gets those settings.
func TestNakamaLive_LobbyFirstProtocolCreate(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	id, _, err := a.CreateLobbyWithCode(context.Background(), NakamaLobbyConfig{
		Name: "Old Style", Format: "winner_stays_on", MaxPlayers: 4, MaxGames: 5,
		Rules: map[string]any{"rounds": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	joinWith(t, a, recA, id, nil)
	st := waitLobby(t, a, "state", func(st map[string]any) bool { return st["settings"] != nil })
	set, _ := st["settings"].(map[string]any)
	want := map[string]any{"name": "Old Style", "format": "winner_stays_on", "size": float64(4),
		"max_games": float64(5), "rounds": float64(2)}
	for k, v := range want {
		if set[k] != v {
			t.Fatalf("settings[%s] = %v, want %v (settings %v)", k, set[k], v, set)
		}
	}
}

// Only ready members are paired; a member who is not ready keeps their place
// and is skipped; after a result the lobby shows the results for its
// interval and then starts the next pairing.
func TestNakamaLive_LobbyReadyQueue(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"interval": 3})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	joinWith(t, c, recC, id, nil)
	waitLobby(t, c, "three members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 3
	})

	ready(t, a, true)
	time.Sleep(400 * time.Millisecond)
	if st, _, _ := c.LobbyState(); st["phase"] != "waiting" {
		t.Fatalf("a match started with one ready member: %v", st["phase"])
	}
	ready(t, b, true)
	st := waitLobby(t, c, "A vs B", lobbyPhase("playing"))
	p1, p2, seq := currentPair(st)
	if p1 != ida || p2 != idb {
		t.Fatalf("pairing %s vs %s, want A vs B", short(p1), short(p2))
	}
	if memberField(st, ida, "state") != "playing" || memberField(st, idc, "state") != "standby" {
		t.Fatalf("member states %v %v", memberField(st, ida, "state"), memberField(st, idc, "state"))
	}

	if err := a.ReportLobbyResultSeq(ida, idb, int64(seq)); err != nil {
		t.Fatal(err)
	}
	_ = b.ReportLobbyResultSeq(ida, idb, int64(seq)) // the other player's report is ignored
	st = waitLobby(t, c, "results", lobbyPhase("results"))
	res, _ := st["last_result"].(map[string]any)
	if res["winner"] != ida || res["winner_name"] == "" {
		t.Fatalf("last result %v", res)
	}
	if memberField(st, ida, "wins") != float64(1) || memberField(st, idb, "losses") != float64(1) {
		t.Fatalf("records %v %v", memberField(st, ida, "wins"), memberField(st, idb, "losses"))
	}
	// C, first in line but not ready, is skipped; readying up during the
	// results puts C into the announced pairing.
	nxt, _ := st["next"].(map[string]any)
	if nxt["player1"] != ida || nxt["player2"] != idb {
		t.Fatalf("next pairing %v, want A vs B while C is not ready", nxt)
	}
	ready(t, c, true)
	st = waitLobby(t, a, "C announced next", func(st map[string]any) bool {
		n, _ := st["next"].(map[string]any)
		return n != nil && n["player1"] == idc
	})
	st = waitLobby(t, a, "C vs A playing", func(st map[string]any) bool {
		p1, _, s := currentPair(st)
		return st["phase"] == "playing" && s == seq+1 && p1 == idc
	})
	p1, p2, _ = currentPair(st)
	if p1 != idc || p2 != ida {
		t.Fatalf("after the results: %s vs %s, want C vs A", short(p1), short(p2))
	}
	t.Logf("ready gating, results phase and rotation: A beat B, then C vs A")
}

// A pair that cannot connect is not paired again; both keep their turn.
func TestNakamaLive_LobbyPairFailedSkipsPair(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"interval": 3})
	for _, p := range []struct {
		c *NakamaClient
		r *evRecorder
	}{{a, recA}, {b, recB}, {c, recC}} {
		joinWith(t, p.c, p.r, id, nil)
		ready(t, p.c, true)
		time.Sleep(150 * time.Millisecond)
	}
	st := waitLobby(t, c, "A vs B", lobbyPhase("playing"))
	p1, p2, seq := currentPair(st)
	if p1 != ida || p2 != idb {
		t.Fatalf("first pairing %s vs %s", short(p1), short(p2))
	}
	if err := b.SendLobbyCommand("pair_failed", map[string]any{"seq": seq, "reason": "p2p"}); err != nil {
		t.Fatal(err)
	}
	st = waitLobby(t, c, "no contest", lobbyPhase("results"))
	res, _ := st["last_result"].(map[string]any)
	if res["no_contest"] != true || res["reason"] != "p2p" {
		t.Fatalf("result %v", res)
	}
	st = waitLobby(t, c, "next match", func(st map[string]any) bool {
		_, _, s := currentPair(st)
		return st["phase"] == "playing" && s == seq+1
	})
	p1, p2, _ = currentPair(st)
	if p1 != ida || p2 != idc {
		t.Fatalf("after A-B failed: %s vs %s, want A vs C", short(p1), short(p2))
	}
}

// The host changes settings, removes members and hands over the lobby;
// other members cannot. The next member in join order becomes host when
// the host leaves.
func TestNakamaLive_LobbyHostActions(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"name": "hosted"})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	joinWith(t, c, recC, id, nil)
	waitLobby(t, a, "three members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 3
	})
	_ = b.LobbyEvents()

	if err := b.SendLobbyCommand("settings", map[string]any{"settings": map[string]any{"comment": "hijack"}}); err != nil {
		t.Fatal(err)
	}
	waitLobbyEvent(t, b, "non-host settings refused", func(ev map[string]any) bool {
		return ev["kind"] == "error" && strings.Contains(fmt.Sprint(ev["error"]), "host")
	})
	if err := a.SendLobbyCommand("settings", map[string]any{"settings": map[string]any{"comment": "welcome", "size": 3, "format": "bracket"}}); err != nil {
		t.Fatal(err)
	}
	st := waitLobby(t, b, "settings applied", func(st map[string]any) bool {
		s, _ := st["settings"].(map[string]any)
		return s["comment"] == "welcome"
	})
	set, _ := st["settings"].(map[string]any)
	if set["size"] != float64(3) || set["format"] != "bracket" || set["name"] != "hosted" {
		t.Fatalf("settings %v", set)
	}
	if err := a.SendLobbyCommand("settings", map[string]any{"settings": map[string]any{"size": 2}}); err != nil {
		t.Fatal(err)
	}
	waitLobbyEvent(t, a, "size below member count refused", func(ev map[string]any) bool {
		return ev["kind"] == "error"
	})

	if err := a.SendLobbyCommand("kick", map[string]any{"user_id": idc}); err != nil {
		t.Fatal(err)
	}
	waitLobbyEvent(t, c, "kicked", func(ev map[string]any) bool { return ev["kind"] == "kicked" })
	waitLobby(t, b, "C removed", func(st map[string]any) bool {
		return memberField(st, idc, "user_id") == nil
	})
	c.ClearLastError()
	_ = c.JoinMatch(id, "")
	if _, ok := recC.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool { return ev.Type == "match_joined" }); ok {
		t.Fatal("a removed member joined again")
	}

	_ = a.LeaveMatch()
	st = waitLobby(t, b, "B is host", func(st map[string]any) bool { return st["host"] == idb })
	if memberField(st, idb, "host") != true || memberField(st, ida, "user_id") != nil {
		t.Fatalf("members after the host left: %v", st["members"])
	}
}

// Chat reaches every member with the sender's name and is rate limited; a
// P2P signal addressed to one member reaches only that member.
func TestNakamaLive_LobbyChatAndSignalRouting(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, nil)
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	joinWith(t, c, recC, id, nil)
	waitLobby(t, c, "three members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 3
	})
	_ = a.LobbyEvents()
	_ = b.LobbyEvents()
	_ = c.LobbyEvents()

	if err := a.SendLobbyCommand("chat", map[string]any{"text": "  hello\x07 lobby  "}); err != nil {
		t.Fatal(err)
	}
	for _, cl := range []*NakamaClient{b, c} {
		ev := waitLobbyEvent(t, cl, "chat", func(ev map[string]any) bool { return ev["kind"] == "chat" })
		if ev["text"] != "hello lobby" || ev["username"] != a.Session().Username {
			t.Fatalf("chat event %v", ev)
		}
	}
	_ = a.SendLobbyCommand("chat", map[string]any{"text": "again"})
	waitLobbyEvent(t, a, "rate limit", func(ev map[string]any) bool { return ev["kind"] == "error" })

	signal, _ := json.Marshal(P2PSignal{Magic: p2pMagic, MatchID: id, Token: "t", From: a.Session().UserID, To: b.Session().UserID})
	if err := a.SendMatchData(nakamaSignalOp, signal, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := recB.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool {
		return ev.Type == "p2p_signal" || (ev.Type == "match_data" && uint32(ev.MatchData.OpCode) == nakamaSignalOp)
	}); !ok {
		t.Fatal("the addressed member did not get the signal")
	}
	time.Sleep(300 * time.Millisecond)
	if evs := matchDataEvents(recC, nakamaSignalOp); len(evs) != 0 {
		t.Fatalf("a third member received a signal addressed to someone else (%d)", len(evs))
	}
}

// A round robin with start = host waits for the host; its entrants are the
// members ready at the start, and it finishes after every pairing.
func TestNakamaLive_LobbyRoundRobinHostStart(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"format": "round_robin", "start": "host", "interval": 3})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	joinWith(t, c, recC, id, nil)
	ready(t, a, true)
	ready(t, b, true)
	ready(t, c, true)
	time.Sleep(500 * time.Millisecond)
	if st, _, _ := a.LobbyState(); st["phase"] != "waiting" || st["started"] == true {
		t.Fatalf("the tournament started without the host: %v", st["phase"])
	}
	if err := b.SendLobbyCommand("start", nil); err != nil {
		t.Fatal(err)
	}
	waitLobbyEvent(t, b, "non-host start refused", func(ev map[string]any) bool { return ev["kind"] == "error" })
	if err := a.SendLobbyCommand("start", nil); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{ida, idb}, {ida, idc}, {idb, idc}}
	seq := 0
	for i, pair := range want {
		st := waitLobby(t, a, fmt.Sprintf("match %d", i+1), func(st map[string]any) bool {
			_, _, s := currentPair(st)
			return st["phase"] == "playing" && s > seq
		})
		p1, p2, s := currentPair(st)
		seq = s
		if p1 != pair[0] || p2 != pair[1] {
			t.Fatalf("match %d: %s vs %s", i+1, short(p1), short(p2))
		}
		reporter := map[string]*NakamaClient{ida: a, idb: b, idc: c}[p1]
		if err := reporter.ReportLobbyResultSeq(p1, p2, int64(s)); err != nil {
			t.Fatal(err)
		}
	}
	st := waitLobby(t, a, "finished", lobbyPhase("finished"))
	tour, _ := st["tournament"].(map[string]any)
	if tour["finished"] != true {
		t.Fatalf("tournament %v", tour)
	}
	if memberField(st, ida, "wins") != float64(2) {
		t.Fatalf("A wins %v", memberField(st, ida, "wins"))
	}
	// A won both matches: the round robin is A's.
	if tour["winner"] != ida || tour["winner_name"] != a.Session().Username || tour["tied"] != nil {
		t.Fatalf("round robin winner %v %v, tied %v", tour["winner"], tour["winner_name"], tour["tied"])
	}
	// The end of a tournament sets everyone not ready.
	for _, id := range []string{ida, idb, idc} {
		if memberField(st, id, "ready") != false {
			t.Fatalf("%s still ready after the tournament", short(id))
		}
	}

	// A second round robin in which every entrant wins once ends in a tie.
	ready(t, a, true)
	ready(t, b, true)
	ready(t, c, true)
	waitLobby(t, a, "all ready", func(st map[string]any) bool {
		return memberField(st, ida, "ready") == true && memberField(st, idb, "ready") == true && memberField(st, idc, "ready") == true
	})
	if err := a.SendLobbyCommand("start", nil); err != nil {
		t.Fatal(err)
	}
	winners := map[[2]string]string{{ida, idb}: ida, {ida, idc}: idc, {idb, idc}: idb}
	for i := range want {
		st := waitLobby(t, a, fmt.Sprintf("second tournament, match %d", i+1), func(st map[string]any) bool {
			_, _, s := currentPair(st)
			return st["phase"] == "playing" && s > seq
		})
		p1, p2, s := currentPair(st)
		seq = s
		w := winners[[2]string{p1, p2}]
		l := p1
		if w == p1 {
			l = p2
		}
		if w == "" {
			t.Fatalf("unexpected pairing %s vs %s", short(p1), short(p2))
		}
		if err := map[string]*NakamaClient{ida: a, idb: b, idc: c}[w].ReportLobbyResultSeq(w, l, int64(s)); err != nil {
			t.Fatal(err)
		}
	}
	st = waitLobby(t, a, "second tournament finished", lobbyPhase("finished"))
	tour, _ = st["tournament"].(map[string]any)
	tied, _ := tour["tied"].([]any)
	if tour["winner"] != nil || len(tied) != 3 {
		t.Fatalf("tied round robin: winner %v, tied %v", tour["winner"], tour["tied"])
	}
	t.Logf("round robin won by A, then a three-way tie %v", tour["tied_names"])
}

// A bracket entrant who leaves during their match forfeits it.
func TestNakamaLive_LobbyBracketForfeit(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	ida, idb := a.Session().UserID, b.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"format": "bracket", "interval": 3})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, a, "bracket match", lobbyPhase("playing"))
	if p1, p2, _ := currentPair(st); p1 != ida || p2 != idb {
		t.Fatalf("bracket pairing %s vs %s", short(p1), short(p2))
	}
	_ = b.LeaveMatch()
	st = waitLobby(t, a, "forfeit", lobbyPhase("results"))
	res, _ := st["last_result"].(map[string]any)
	if res["winner"] != ida || res["forfeit"] != true {
		t.Fatalf("result %v", res)
	}
	st = waitLobby(t, a, "finished", lobbyPhase("finished"))
	tour, _ := st["tournament"].(map[string]any)
	if tour["winner"] != ida {
		t.Fatalf("tournament winner %v", tour["winner"])
	}
}

// With start = auto, a finished tournament waits until every member has
// readied up again, and then the next one starts by itself.
func TestNakamaLive_LobbyTournamentAutoRestart(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	ida, idb := a.Session().UserID, b.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"format": "bracket", "start": "auto", "interval": 3})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, a, "first tournament", lobbyPhase("playing"))
	_, _, seq := currentPair(st)
	if err := a.ReportLobbyResultSeq(ida, idb, int64(seq)); err != nil {
		t.Fatal(err)
	}
	st = waitLobby(t, a, "finished", lobbyPhase("finished"))
	if memberField(st, ida, "ready") != false || memberField(st, idb, "ready") != false {
		t.Fatal("members still ready after the tournament")
	}
	ready(t, a, true)
	time.Sleep(500 * time.Millisecond)
	if st, _, _ := a.LobbyState(); st["phase"] != "finished" {
		t.Fatalf("a tournament started before everyone was ready: %v", st["phase"])
	}
	ready(t, b, true)
	st = waitLobby(t, a, "second tournament", func(st map[string]any) bool {
		_, _, s := currentPair(st)
		return st["phase"] == "playing" && s > seq
	})
	if tour, _ := st["tournament"].(map[string]any); tour["finished"] != false || tour["winner"] != nil {
		t.Fatalf("second tournament state %v", tour)
	}
}

// The client measures its round trip to the server with a realtime ping.
func TestNakamaLive_Ping(t *testing.T) {
	fixedEnabled(t)
	a, _ := liveClient(t)
	if err := a.Ping(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.Status().RTT == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.Status().RTT <= 0 {
		t.Fatal("no pong")
	}
	t.Logf("server round trip %v", a.Status().RTT)
}

// A match abandoned mid-way sends neither player straight into another one.
func TestNakamaLive_LobbyAbortedMatchUnreadies(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	ida, idb := a.Session().UserID, b.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"interval": 3})
	joinWith(t, a, recA, id, nil)
	joinWith(t, b, recB, id, nil)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, a, "playing", lobbyPhase("playing"))
	_, _, seq := currentPair(st)
	if err := a.SendLobbyCommand("no_contest", map[string]any{"seq": seq, "reason": "aborted"}); err != nil {
		t.Fatal(err)
	}
	st = waitLobby(t, b, "results", lobbyPhase("results"))
	if memberField(st, ida, "ready") != false || memberField(st, idb, "ready") != false {
		t.Fatalf("players still ready after an abandoned match: %v %v", memberField(st, ida, "ready"), memberField(st, idb, "ready"))
	}
	waitLobby(t, b, "waiting", lobbyPhase("waiting"))
}

// A player who cancels the connection sits out; the pair stays eligible.
func TestNakamaLive_LobbyCancelledConnection(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	id, _ := createLobbyWith(t, a, map[string]any{"interval": 3})
	for _, p := range []struct {
		c *NakamaClient
		r *evRecorder
	}{{a, recA}, {b, recB}, {c, recC}} {
		joinWith(t, p.c, p.r, id, nil)
		ready(t, p.c, true)
		time.Sleep(150 * time.Millisecond)
	}
	st := waitLobby(t, a, "A vs B", lobbyPhase("playing"))
	_, _, seq := currentPair(st)
	if err := b.SendLobbyCommand("pair_failed", map[string]any{"seq": seq, "reason": "cancel"}); err != nil {
		t.Fatal(err)
	}
	st = waitLobby(t, a, "results", lobbyPhase("results"))
	if memberField(st, idb, "ready") != false || memberField(st, ida, "ready") != true {
		t.Fatalf("after B cancelled: A ready %v, B ready %v", memberField(st, ida, "ready"), memberField(st, idb, "ready"))
	}
	st = waitLobby(t, a, "next match", func(st map[string]any) bool {
		_, _, s := currentPair(st)
		return st["phase"] == "playing" && s == seq+1
	})
	if p1, p2, _ := currentPair(st); !((p1 == ida && p2 == idc) || (p1 == idc && p2 == ida)) {
		t.Fatalf("after the cancel: %s vs %s, want A and C", short(p1), short(p2))
	}
	ready(t, b, true)
	_ = a.SendLobbyCommand("no_contest", map[string]any{"seq": seq + 1, "reason": "draw"})
	st = waitLobby(t, a, "A vs B allowed again", func(st map[string]any) bool {
		p1, p2, s := currentPair(st)
		return st["phase"] == "playing" && s == seq+2 && ((p1 == ida && p2 == idb) || (p1 == idb && p2 == ida) || (p1 == idc && p2 == idb) || (p1 == idb && p2 == idc))
	})
	_ = st
}
