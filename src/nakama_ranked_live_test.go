package main

// Live tests of ranked sets (nakama/modules/ikemen_ranked.lua,
// ikemen_rating.lua, ikemen_moderation.lua and the ranked part of
// ikemen_session.lua): which results the server accepts, one rating change
// per set, disputes and their settlement by moderators, and the players'
// replays the server keeps. Enable with NAKAMA_LIVE=1 (see
// nakama_live_test.go for the server flags).
//
// The moderator RPCs are called with the server's runtime http_key:
// NAKAMA_HTTP_KEY, or Nakama's default ("defaulthttpkey"). NAKAMA_PORT
// names the server's port when it is not 7350.
//
// Some tests need more than the server flags of nakama_live_test.go:
//   - TestNakamaLive_RankedTimeouts and TestNakamaLive_RankedReplayByteLimit
//     need a server whose runtime.env has IKEMEN_RANKED_CLAIM_MINUTES=0.05,
//     IKEMEN_RANKED_REPLAY_DAYS=0.0001 and
//     IKEMEN_RANKED_REPLAY_MAX_BYTES=40000, and run when
//     NAKAMA_RANKED_LIMITS=1 says so. Run them by themselves on such a
//     server (-test.run 'TestNakamaLive_Ranked(Timeouts|ReplayByteLimit)'):
//     the other tests need the default limits.
//   - TestNakamaLive_RankedMakeDisputed is a helper: with
//     NAKAMA_RANKED_DISPUTED=<n> it leaves n disputed sets with both
//     players' replays on the server (for testing moderation tools), and
//     prints their ids.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func liveHTTPKey() string {
	if key := os.Getenv("NAKAMA_HTTP_KEY"); key != "" {
		return key
	}
	return "defaulthttpkey"
}

// serverRPC calls a server-to-server RPC with the runtime http_key.
func serverRPC(t *testing.T, id string, payload any) (map[string]any, error) {
	t.Helper()
	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/rpc/%s?http_key=%s&unwrap", livePort(), id, liveHTTPKey())
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, newNakamaRPCError(id, resp.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s: decode %q: %v", id, data, err)
	}
	return out, nil
}

// findRankedSet reads a set's record through ikemen_ranked_list, or nil when
// the server has none.
func findRankedSet(t *testing.T, id string) map[string]any {
	t.Helper()
	out, err := serverRPC(t, "ikemen_ranked_list", map[string]any{"match_id": id})
	if err != nil {
		t.Fatalf("list %s: %v", id, err)
	}
	sets, _ := out["sets"].([]any)
	if len(sets) > 1 {
		t.Fatalf("set %s: %v", id, out)
	}
	if len(sets) == 0 {
		return nil
	}
	return sets[0].(map[string]any)
}

// rankedSet reads a set's record, which must exist.
func rankedSet(t *testing.T, id string) map[string]any {
	t.Helper()
	set := findRankedSet(t, id)
	if set == nil {
		t.Fatalf("no ranked set %s", id)
	}
	return set
}

// waitSet waits up to 10 seconds for the set to be recorded and for pred to
// hold: the session changes a set's record as it handles the players'
// messages.
func waitSet(t *testing.T, id, what string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		set := findRankedSet(t, id)
		if set != nil && pred(set) {
			return set
		}
		if time.Now().After(deadline) {
			t.Fatalf("set %s: %s: %v", id, what, set)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type rankedPairing struct {
	a, b       *NakamaClient
	recA, recB *evRecorder
	matchID    string
	game       string
	order      [2]string // user ids in the matchmaker's order
}

// rankedPairOf matchmakes two clients into a ranked session of a game of
// their own and returns it, with the players in the matchmaker's order.
func rankedPairOf(t *testing.T, a, b *NakamaClient, recA, recB *evRecorder, game string, bestOf int) *rankedPairing {
	t.Helper()
	req := NakamaMatchmakerRequest{Mode: "ranked", Elo: 1000, EloRange: 100, Game: game, GameBuild: "1.0.0", Region: "r", RankedBestOf: bestOf}
	if err := a.Matchmake(req); err != nil {
		t.Fatal(err)
	}
	if err := b.Matchmake(req); err != nil {
		t.Fatal(err)
	}
	p := &rankedPairing{a: a, b: b, recA: recA, recB: recB, game: game}
	for _, rec := range []*evRecorder{recA, recB} {
		ev, ok := rec.waitFor(t, 30*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
		if !ok {
			t.Fatal("no matchmaker_matched")
		}
		if p.matchID != "" && ev.Matchmaker.MatchID != p.matchID {
			t.Fatalf("the two players got different sessions: %s, %s", p.matchID, ev.Matchmaker.MatchID)
		}
		p.matchID = ev.Matchmaker.MatchID
		p.order = [2]string{ev.Matchmaker.Users[0].Presence.UserID, ev.Matchmaker.Users[1].Presence.UserID}
	}
	return p
}

func rankedPair(t *testing.T, bestOf int) *rankedPairing {
	t.Helper()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	return rankedPairOf(t, a, b, recA, recB, "rk-"+randID(), bestOf)
}

func (p *rankedPairing) joinBoth(t *testing.T) {
	t.Helper()
	join(t, p.a, p.recA, p.matchID)
	join(t, p.b, p.recB, p.matchID)
}

func claim(c *NakamaClient, matchID string, set int, winner string) (map[string]any, error) {
	body, err := c.SubmitRankedSet(context.Background(), NakamaRankedClaim{MatchID: matchID, Set: set, Winner: winner})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out, nil
}

func wantRPCError(t *testing.T, what string, err error, code int, text string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted", what)
	}
	if got := nakamaRPCErrorCode(err); got != code || !strings.Contains(err.Error(), text) {
		t.Fatalf("%s: error %d %q, want %d containing %q", what, got, err.Error(), code, text)
	}
	t.Logf("%s: refused (%d) %q", what, code, err.Error())
}

func ratingOf(t *testing.T, c *NakamaClient, game string) map[string]any {
	t.Helper()
	body, err := c.GetRating(context.Background(), game)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	return out
}

func TestNakamaLive_RankedSetRecorded(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 5)
	set := rankedSet(t, p.matchID)
	players, _ := set["players"].([]any)
	if len(players) != 2 || players[0].(map[string]any)["user_id"] != p.order[0] || players[1].(map[string]any)["user_id"] != p.order[1] {
		t.Fatalf("players %v, matchmaker order %v", players, p.order)
	}
	if set["status"] != "open" || set["best_of"] != float64(5) || set["game"] != p.game || set["build"] != "1.0.0" || set["set"] != float64(1) {
		t.Fatalf("record %v", set)
	}
	if name, _ := players[0].(map[string]any)["username"].(string); name == "" {
		t.Fatalf("no username: %v", players)
	}
	t.Logf("ranked session %s recorded: players in matchmaker order, best of 5, open", p.matchID[:8])
}

func TestNakamaLive_RankedRejectsUnknownMatch(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	_, err := claim(a, "set-"+randID(), 1, a.Session().UserID)
	wantRPCError(t, "made-up match id", err, 5, "no ranked set is recorded")
	// The old request shape is held to the same rule.
	_, err = a.SubmitResult(context.Background(), NakamaResultRequest{MatchID: "set-" + randID(), OpponentID: "x", Game: "g", Result: 1})
	wantRPCError(t, "made-up match id, first protocol", err, 5, "no ranked set is recorded")
	// A lobby is not a ranked session.
	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	_, err = claim(a, id, 1, a.Session().UserID)
	wantRPCError(t, "lobby", err, 9, "lobby matches are not rated")
}

func TestNakamaLive_RankedRejectsUnranked(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "unranked", Game: "ur-" + randID(), GameBuild: "1.0.0", Region: "r"}
	_ = a.Matchmake(req)
	_ = b.Matchmake(req)
	ev, ok := recA.waitFor(t, 30*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	if !ok {
		t.Fatal("no match")
	}
	_, _ = recB.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	join(t, a, recA, ev.Matchmaker.MatchID)
	_, err := claim(a, ev.Matchmaker.MatchID, 1, a.Session().UserID)
	wantRPCError(t, "unranked session", err, 9, "unranked session")
}

func TestNakamaLive_RankedRejectsNonPlayer(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	c, _ := liveClient(t)
	_, err := claim(c, p.matchID, 1, p.order[0])
	wantRPCError(t, "a third account", err, 7, "not a player")
	// A player naming someone else as the winner.
	_, err = claim(p.a, p.matchID, 1, c.Session().UserID)
	wantRPCError(t, "a winner who did not play", err, 3, "winner must be")
	// A set the session does not have.
	_, err = claim(p.a, p.matchID, 2, p.order[0])
	wantRPCError(t, "set 2 before it started", err, 5, "has no set 2")
	if set := rankedSet(t, p.matchID); len(set["claims"].([]any)) != 0 {
		t.Fatalf("a refused claim was kept: %v", set["claims"])
	}
}

// Ratings change once per set: both players' claims, retried and sent at
// the same time, settle it once.
func TestNakamaLive_EloSettlesOnce(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if out, err := claim(p.a, p.matchID, 1, ida); err != nil || out["status"] != "open" {
		t.Fatalf("first claim: %v %v", out, err)
	}
	// Retry + simultaneous submissions.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := claim(p.b, p.matchID, 1, ida); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := p.a.SubmitResult(context.Background(), NakamaResultRequest{MatchID: p.matchID, OpponentID: idb, Game: p.game, Result: 1}); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("retried claims failed: %v", failures)
	}
	ra, rb := ratingOf(t, p.a, p.game), ratingOf(t, p.b, p.game)
	t.Logf("after one set + 8 concurrent/retried claims: A %v, B %v", ra, rb)
	if ra["games"] != float64(1) || ra["wins"] != float64(1) || rb["games"] != float64(1) || rb["losses"] != float64(1) {
		t.Fatalf("expected exactly one settlement")
	}
	set := rankedSet(t, p.matchID)
	if set["status"] != "settled" || set["winner"] != ida || set["settled_by"] != "players" {
		t.Fatalf("set %v", set)
	}
	// Closed now: the other winner is refused, a repeated claim answered.
	_, err := claim(p.b, p.matchID, 1, idb)
	wantRPCError(t, "a claim after settlement", err, 9, "settled")
	if out, err := claim(p.b, p.matchID, 1, ida); err != nil || out["settled"] != true {
		t.Fatalf("repeated claim: %v %v", out, err)
	}
}

func TestNakamaLive_RankedDisputeSettledByModerator(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if _, err := claim(p.a, p.matchID, 1, ida); err != nil {
		t.Fatal(err)
	}
	out, err := claim(p.b, p.matchID, 1, idb)
	if err != nil || out["status"] != "disputed" {
		t.Fatalf("disagreeing claims: %v %v", out, err)
	}
	if r := ratingOf(t, p.a, p.game); r["games"] != float64(0) {
		t.Fatalf("a disputed set changed a rating: %v", r)
	}
	// The moderators' list of disputed sets has it.
	listed, err := serverRPC(t, "ikemen_ranked_list", map[string]any{"status": "disputed", "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	sets, _ := listed["sets"].([]any)
	for _, s := range sets {
		m := s.(map[string]any)
		if m["status"] != "disputed" {
			t.Fatalf("listed %v", m)
		}
		if m["match_id"] == p.matchID {
			found = true
			claims := m["claims"].([]any)
			if len(claims) != 2 || m["reason"] != "claims_disagree" {
				t.Fatalf("disputed set %v", m)
			}
		}
	}
	// Newest first: the set just disputed is on the first page.
	if !found {
		t.Fatalf("the disputed set is not listed")
	}
	all, err := serverRPC(t, "ikemen_ranked_list", map[string]any{"status": "all", "limit": 5})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := all["sets"].([]any)
	if len(page) == 0 || page[0].(map[string]any)["match_id"] != p.matchID {
		t.Fatalf("the newest set is not first: %v", page)
	}
	for i := 1; i < len(page); i++ {
		if page[i].(map[string]any)["created"].(float64) > page[i-1].(map[string]any)["created"].(float64) {
			t.Fatalf("sets are not newest first: %v", page)
		}
	}
	if len(page) == 5 && all["cursor"] == "" {
		t.Fatalf("a full page without a cursor: %v", all)
	}
	set := rankedSet(t, p.matchID)
	if set["status"] != "disputed" {
		t.Fatalf("set %v", set)
	}
	// A moderator settles it for B.
	res, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": idb, "note": "B won the set; checked the replay"})
	if err != nil || res["status"] != "settled" {
		t.Fatalf("settle: %v %v", res, err)
	}
	ra, rb := ratingOf(t, p.a, p.game), ratingOf(t, p.b, p.game)
	if ra["losses"] != float64(1) || rb["wins"] != float64(1) || rb["games"] != float64(1) {
		t.Fatalf("ratings after the moderator's settlement: A %v B %v", ra, rb)
	}
	set = rankedSet(t, p.matchID)
	if set["status"] != "settled" || set["winner"] != idb || set["settled_by"] != "moderator" || !strings.Contains(set["note"].(string), "checked") {
		t.Fatalf("set %v", set)
	}
	_, err = serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": ida})
	wantRPCError(t, "settling twice", err, 9, "already")
	_, err = serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true})
	wantRPCError(t, "voiding a settled set", err, 9, "already")
	if rb := ratingOf(t, p.b, p.game); rb["games"] != float64(1) {
		t.Fatalf("a refused settlement changed a rating: %v", rb)
	}
	t.Logf("disputed set listed, settled by a moderator once: A %v, B %v", ra["rating"], rb["rating"])
}

func TestNakamaLive_RankedVoid(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida := p.a.Session().UserID
	if _, err := claim(p.a, p.matchID, 1, ida); err != nil {
		t.Fatal(err)
	}
	_, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID})
	wantRPCError(t, "neither winner nor void", err, 3, "either")
	_, err = serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": "someone-else"})
	wantRPCError(t, "a winner who did not play", err, 3, "winner must be")
	res, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true, "note": "played on a modified character"})
	if err != nil || res["status"] != "void" {
		t.Fatalf("void: %v %v", res, err)
	}
	set := rankedSet(t, p.matchID)
	if set["status"] != "void" || set["note"] != "played on a modified character" {
		t.Fatalf("set %v", set)
	}
	_, err = claim(p.b, p.matchID, 1, ida)
	wantRPCError(t, "a claim for a void set", err, 9, "void")
	_, err = serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true})
	wantRPCError(t, "voiding twice", err, 9, "already")
	if r := ratingOf(t, p.a, p.game); r["games"] != float64(0) {
		t.Fatalf("a void set changed a rating: %v", r)
	}
	_, err = serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": "set-" + randID(), "void": true})
	wantRPCError(t, "an unknown set", err, 5, "no ranked set")
}

// The moderator RPCs refuse players' sessions.
func TestNakamaLive_RankedModeratorRPCsRefusePlayers(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	for _, call := range []struct {
		id      string
		payload map[string]any
	}{
		{"ikemen_ranked_list", map[string]any{"status": "all"}},
		{"ikemen_ranked_replay", map[string]any{"match_id": p.matchID}},
		{"ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": p.a.Session().UserID}},
		{"ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true}},
	} {
		_, err := p.a.CallRPC(context.Background(), call.id, call.payload)
		wantRPCError(t, call.id+" with a player's session", err, 7, "http_key")
	}
	if set := rankedSet(t, p.matchID); set["status"] != "open" {
		t.Fatalf("a player changed the set: %v", set)
	}
	// And a claim needs a player's session.
	_, err := serverRPC(t, "ikemen_submit_result", map[string]any{"match_id": p.matchID, "winner": p.order[0]})
	wantRPCError(t, "a claim without a player's session", err, 16, "player")
}

// A moderator who settles a set while its second claim comes settles it, or
// the claims do: the ratings change once, for the winner the set records.
func TestNakamaLive_RankedModeratorRacesClaim(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if _, err := claim(p.a, p.matchID, 1, ida); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	settledByModerator := 0
	var unexpected []string
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			// B agrees with A: settled, or refused once the moderator has
			// settled the set.
			out, err := claim(p.b, p.matchID, 1, ida)
			if err != nil && (nakamaRPCErrorCode(err) != 9 || !strings.Contains(err.Error(), "settled")) {
				mu.Lock()
				unexpected = append(unexpected, "claim: "+err.Error())
				mu.Unlock()
			} else if err == nil && out["status"] != "settled" {
				mu.Lock()
				unexpected = append(unexpected, fmt.Sprintf("claim answered %v", out))
				mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			// The moderator names B: settled once, refused afterwards.
			out, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": idb})
			mu.Lock()
			defer mu.Unlock()
			if err == nil && out["status"] == "settled" {
				settledByModerator++
			} else if err == nil || nakamaRPCErrorCode(err) != 9 || !strings.Contains(err.Error(), "already") {
				unexpected = append(unexpected, fmt.Sprintf("settle: %v %v", out, err))
			}
		}()
	}
	wg.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("unexpected answers: %v", unexpected)
	}
	set := rankedSet(t, p.matchID)
	ra, rb := ratingOf(t, p.a, p.game), ratingOf(t, p.b, p.game)
	if set["status"] != "settled" || ra["games"] != float64(1) || rb["games"] != float64(1) {
		t.Fatalf("set %v; ratings A %v, B %v", set, ra, rb)
	}
	switch set["settled_by"] {
	case "moderator":
		if settledByModerator != 1 || set["winner"] != idb || rb["wins"] != float64(1) || ra["losses"] != float64(1) {
			t.Fatalf("settled by the moderator (%d times): set %v; ratings A %v, B %v", settledByModerator, set, ra, rb)
		}
	case "players":
		if settledByModerator != 0 || set["winner"] != ida || ra["wins"] != float64(1) || rb["losses"] != float64(1) {
			t.Fatalf("settled by the players (moderator %d times): set %v; ratings A %v, B %v", settledByModerator, set, ra, rb)
		}
	default:
		t.Fatalf("set %v", set)
	}
	t.Logf("settled once, by the %s: A %v, B %v", set["settled_by"], ra["rating"], rb["rating"])
}

// playerStorage calls Nakama's storage API with a player's session.
func playerStorage(t *testing.T, c *NakamaClient, method, path string, payload any) (int, map[string]any) {
	t.Helper()
	session, err := c.freshSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", livePort(), path), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+session.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// The ranked records are the server's: a player cannot read them, and what
// a player writes into their collections with Nakama's storage API (which
// Nakama allows unless a before-hook refuses it, as Nakama Matchmaker's
// community module does) changes nothing.
func TestNakamaLive_RankedStorageIsTheServers(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida := p.a.Session().UserID
	// Without a user id, a player reads and lists the system's objects
	// (those that are public).
	code, out := playerStorage(t, p.a, http.MethodPost, "/v2/storage", map[string]any{"object_ids": []any{
		map[string]any{"collection": "ikemen_ranked_sets", "key": p.matchID}}})
	if objects, _ := out["objects"].([]any); code != http.StatusOK || len(objects) != 0 {
		t.Fatalf("a player read the set's record: %d %v", code, out)
	}
	for _, collection := range []string{"ikemen_ranked_sets", "ikemen_ranked_set_index", "ikemen_ranked_replays", "ikemen_ranked_replay_expiry", "ikemen_settlement"} {
		code, out := playerStorage(t, p.a, http.MethodGet, "/v2/storage/"+collection+"?limit=100", nil)
		if objects, _ := out["objects"].([]any); code != http.StatusOK || len(objects) != 0 {
			t.Fatalf("a player listed %s: %d %v", collection, code, out)
		}
	}
	forged := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	code, out = playerStorage(t, p.a, http.MethodPut, "/v2/storage", map[string]any{"objects": []any{
		map[string]any{"collection": "ikemen_ranked_sets", "key": p.matchID, "value": forged(map[string]any{"match_id": p.matchID, "status": "void"})},
		map[string]any{"collection": "ikemen_ranked_set_index", "key": "0000000000000:" + p.matchID, "value": forged(map[string]any{"match_id": p.matchID})},
		map[string]any{"collection": "ikemen_settlement", "key": p.matchID, "value": forged(map[string]any{"winner": ida})},
		map[string]any{"collection": "ikemen_ranked_replay_expiry", "key": "000000000001:" + p.matchID, "value": forged(map[string]any{"key": p.matchID + ":" + ida})},
	}})
	if code != http.StatusOK {
		// A server with a guard (Nakama Matchmaker's community module).
		if code != http.StatusForbidden || !strings.Contains(fmt.Sprint(out["message"]), "belongs to the server") {
			t.Fatalf("the player's writes: %d %v", code, out)
		}
		t.Logf("the server refuses players' writes to these collections: %d %v", code, out["message"])
	} else {
		t.Logf("the server stores players' own objects in these collections; the modules ignore them")
	}
	// The player deletes "the set": only their own object, if any, goes.
	if code, out := playerStorage(t, p.a, http.MethodPut, "/v2/storage/delete", map[string]any{"object_ids": []any{
		map[string]any{"collection": "ikemen_ranked_sets", "key": p.matchID}}}); code != http.StatusOK && code != http.StatusForbidden {
		t.Fatalf("delete: %d %v", code, out)
	}
	if set := rankedSet(t, p.matchID); set["status"] != "open" {
		t.Fatalf("the set changed: %v", set)
	}
	listed, err := serverRPC(t, "ikemen_ranked_list", map[string]any{"status": "all", "limit": 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range listed["sets"].([]any) {
		if s.(map[string]any)["match_id"] == p.matchID {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the set is listed %d times", seen)
	}
	// The player's own "settlement marker" does not keep the set from
	// settling once.
	for _, c := range []*NakamaClient{p.a, p.b} {
		if _, err := claim(c, p.matchID, 1, ida); err != nil {
			t.Fatal(err)
		}
	}
	if set := rankedSet(t, p.matchID); set["status"] != "settled" {
		t.Fatalf("set %v", set)
	}
	if r := ratingOf(t, p.a, p.game); r["games"] != float64(1) {
		t.Fatalf("rating %v", r)
	}
}

// rankedChunk is a chunk of frames whose inputs tell the stream (salt) and
// the frame apart.
func rankedChunk(stream string, start, count int32, salt byte) ReplayStreamChunk {
	payload := make([]byte, int(count)*REPLAY_NUM_INPUTS*REPLAY_INPUT_BYTES)
	for f := int32(0); f < count; f++ {
		for c := 0; c < REPLAY_NUM_INPUTS; c++ {
			off := (int(f)*REPLAY_NUM_INPUTS + c) * REPLAY_INPUT_BYTES
			binary.LittleEndian.PutUint16(payload[off:], uint16(start+f)^uint16(salt)<<8^uint16(c))
			payload[off+2] = salt
		}
	}
	return ReplayStreamChunk{StartFrame: start, FrameCount: count, Payload: payload, Stream: stream}
}

// rankedMatchInfo is a match description as replay.lua writes it.
func rankedMatchInfo(p *rankedPairing, game int) json.RawMessage {
	info, _ := json.Marshal(map[string]any{
		"version": 1, "kind": "match", "ranked": true, "mode": "netplayversus", "names": true, "coop": false,
		"selection": map[string]any{"p1teammode": "single", "p2teammode": "single",
			"p1": []any{map[string]any{"ref": 1, "pal": 1, "char": "kfm", "def": "chars/kfm/kfm.def", "name": "Kung Fu Man"}},
			"p2": []any{map[string]any{"ref": 2, "pal": 2, "char": "kfm_zss", "def": "chars/kfm_zss/kfm_zss.def", "name": "Kung Fu Man"}}},
		"players": []any{map[string]any{"side": 1, "user_id": p.order[0]}, map[string]any{"side": 2, "user_id": p.order[1]}},
		"game":    game,
	})
	return info
}

// publishRankedStream publishes one stream through the session as a
// player's game does: header, chunks of 30 frames and, when end, the end
// with the match's content list.
func publishRankedStream(t *testing.T, c *NakamaClient, info json.RawMessage, segment, frames int32, salt byte, end bool) string {
	t.Helper()
	sink := &nakamaReplaySink{client: c}
	stream := newReplayStreamID()
	sink.PublishReplayHeader(ReplayStreamHeader{Version: replayStreamVersion, FrameRate: 60, InputCount: REPLAY_NUM_INPUTS,
		InputBytes: REPLAY_INPUT_BYTES, DelayFrames: 120, Seed: int32(salt), Stream: stream, MatchTime: 900, MatchInfo: info,
		Segment: segment, Stage: "stages/kfm.def", Engine: "live-test"})
	for f := int32(0); f < frames; f += 30 {
		n := int32(30)
		if f+n > frames {
			n = frames - f
		}
		sink.PublishReplayChunk(rankedChunk(stream, f, n, salt))
		if (f/30)%8 == 7 {
			time.Sleep(30 * time.Millisecond) // the session's input queue holds 128 messages
		}
	}
	if end {
		sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: frames, Stream: stream,
			Content: []ReplayContentItem{{Label: "fight", File: "data/fight.def", Hash: "0123456789abcdef"}}})
	}
	return stream
}

// sendSetFinal sends the post-match choice that ends a set (as ik_rematch.lua
// does after the set's last match).
func sendSetFinal(t *testing.T, c *NakamaClient, action string, round int) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"version": 1, "action": action, "round": round, "set_final": true})
	if err := c.SendMatchData(nakamaRematchChoiceOp, body, true); err != nil {
		t.Fatal(err)
	}
}

// exportReplay fetches a kept replay through ikemen_ranked_replay and reads
// it as the game does.
func exportReplay(t *testing.T, setID, userID string) ([]*matchReplayData, map[string]any) {
	t.Helper()
	payload := map[string]any{"match_id": setID}
	if userID != "" {
		payload["user_id"] = userID
	}
	out, err := serverRPC(t, "ikemen_ranked_replay", payload)
	if err != nil {
		t.Fatalf("export %s %s: %v", setID, userID, err)
	}
	data, err := base64.StdEncoding.DecodeString(out["replay"].(string))
	if err != nil {
		t.Fatal(err)
	}
	name := out["file_name"].(string)
	if !strings.HasSuffix(name, ".replay") || strings.ContainsAny(name, "/\\") {
		t.Fatalf("file name %q", name)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isMatchReplayFile(path) {
		t.Fatalf("%s is not a match replay to the game", name)
	}
	matches, err := readMatchReplayFile(path)
	if err != nil {
		t.Fatalf("the game cannot read %s: %v", name, err)
	}
	return matches, out
}

// checkFrames compares a session's inputs with what rankedChunk sent.
func checkFrames(t *testing.T, b *NakamaReplayBuffer, frames int32, salt byte) {
	t.Helper()
	final, ended := b.FinalFrame()
	if !ended || final != frames || b.BufferedThrough() != frames {
		t.Fatalf("session holds %d frames (final %d, ended %v), want %d", b.BufferedThrough(), final, ended, frames)
	}
	want := NewNakamaReplayBuffer()
	want.ApplyHeader(ReplayStreamHeader{Stream: "x"})
	for f := int32(0); f < frames; f += 30 {
		n := int32(30)
		if f+n > frames {
			n = frames - f
		}
		c := rankedChunk("x", f, n, salt)
		if err := want.ApplyChunk(c); err != nil {
			t.Fatal(err)
		}
	}
	for f := int32(0); f < frames; f++ {
		got, _ := b.Frame(f)
		exp, _ := want.Frame(f)
		if got != exp {
			t.Fatalf("frame %d differs", f)
		}
	}
}

// Both players' streams are kept, stored once the set is decided and each
// stream has ended, and exported as a match replay file of every match of
// the set (a Turns match with a session per character change).
func TestNakamaLive_RankedReplaysStoredAndExported(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	p.joinBoth(t)
	host, other := p.a, p.b
	if p.order[0] != p.a.Session().UserID {
		host, other = p.b, p.a
	}
	m1, m2 := rankedMatchInfo(p, 1), rankedMatchInfo(p, 2)
	// Match 1: 300 frames. Match 2: a Turns match, 150 frames then 95 more.
	for _, x := range []struct {
		c    *NakamaClient
		salt byte
	}{{host, 1}, {other, 2}} {
		publishRankedStream(t, x.c, m1, 0, 300, x.salt, true)
	}
	time.Sleep(300 * time.Millisecond)
	if len(rankedSet(t, p.matchID)["replays"].([]any)) != 0 {
		t.Fatal("a replay was stored before the set was decided")
	}
	last := map[*NakamaClient]string{}
	for _, x := range []struct {
		c    *NakamaClient
		salt byte
	}{{host, 3}, {other, 4}} {
		publishRankedStream(t, x.c, m2, 0, 150, x.salt, true)
		last[x.c] = publishRankedStream(t, x.c, m2, 1, 95, x.salt+2, false)
	}
	// The set's last match is over: the players choose (set_final) while its
	// stream still runs, and the replays are stored when the streams end.
	// (The two players' messages come over two connections: the choice must
	// not overtake the other player's last header, which in a game starts a
	// match before the set can be decided.)
	time.Sleep(300 * time.Millisecond)
	sendSetFinal(t, host, "exit", 1)
	time.Sleep(300 * time.Millisecond)
	if len(rankedSet(t, p.matchID)["replays"].([]any)) != 0 {
		t.Fatal("a replay was stored before its last stream ended")
	}
	for _, c := range []*NakamaClient{host, other} {
		sink := &nakamaReplaySink{client: c}
		sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: 95, Stream: last[c]})
	}
	set := waitSet(t, p.matchID, "both replays stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
	for _, r := range set["replays"].([]any) {
		m := r.(map[string]any)
		if m["complete"] != true || m["bytes"].(float64) < 1000 {
			t.Fatalf("replay %v", m)
		}
	}
	// Both claims settle the set; the replays stay.
	winner := p.order[0]
	_, _ = claim(host, p.matchID, 1, winner)
	if out, err := claim(other, p.matchID, 1, winner); err != nil || out["status"] != "settled" {
		t.Fatalf("claims: %v %v", out, err)
	}
	for i, uid := range p.order {
		matches, out := exportReplay(t, p.matchID, uid)
		if len(matches) != 2 || out["matches"] != float64(2) || out["complete"] != true || out["user_id"] != uid {
			t.Fatalf("player %d's replay: %d matches, %v", i+1, len(matches), out)
		}
		salt := byte(1 + i)
		if len(matches[0].Sessions) != 1 || len(matches[1].Sessions) != 2 {
			t.Fatalf("sessions %d, %d", len(matches[0].Sessions), len(matches[1].Sessions))
		}
		checkFrames(t, matches[0].Sessions[0], 300, salt)
		checkFrames(t, matches[1].Sessions[0], 150, salt+2)
		checkFrames(t, matches[1].Sessions[1], 95, salt+4)
		var info map[string]any
		_ = json.Unmarshal(matches[1].Match.Info, &info)
		if info["game"] != float64(2) || info["selection"] == nil || matches[1].Match.Engine != "live-test" {
			t.Fatalf("match 2's record %+v", matches[1].Match)
		}
		if len(matches[0].Match.Content) != 1 || matches[0].Match.Content[0].Hash != "0123456789abcdef" {
			t.Fatalf("content %v", matches[0].Match.Content)
		}
		h := matches[1].Sessions[1].Header()
		if h.Stage != "stages/kfm.def" || h.Segment != 1 || h.Seed != int32(salt+4) {
			t.Fatalf("session header %+v", h)
		}
		t.Logf("player %d's replay: %s, %d bytes, 2 matches (1 and 2 sessions)", i+1, out["file_name"], int(out["bytes"].(float64)))
	}
	// Without a player, the first complete replay in the players' order.
	if _, out := exportReplay(t, p.matchID, ""); out["user_id"] != p.order[0] {
		t.Fatalf("default replay %v", out["user_id"])
	}
	_, err := serverRPC(t, "ikemen_ranked_replay", map[string]any{"match_id": p.matchID, "user_id": "someone-else"})
	wantRPCError(t, "a replay of a non-player", err, 3, "not a player")
}

// A player who leaves before the set is decided is noted on the set, and
// their replay (and the other player's, stored when the session ends) is
// incomplete.
func TestNakamaLive_RankedLeftEarly(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	p.joinBoth(t)
	info := rankedMatchInfo(p, 1)
	publishRankedStream(t, p.a, info, 0, 240, 7, false)
	publishRankedStream(t, p.b, info, 0, 240, 8, false)
	time.Sleep(300 * time.Millisecond)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if err := p.a.LeaveMatch(); err != nil {
		t.Fatal(err)
	}
	set := waitSet(t, p.matchID, "a left early", func(s map[string]any) bool {
		left, _ := s["left_early"].([]any)
		return len(left) == 1 && len(s["replays"].([]any)) == 1
	})
	if set["left_early"].([]any)[0] != ida {
		t.Fatalf("left early: %v", set["left_early"])
	}
	// The other player, left alone, leaves too: not early, and the session
	// ends with both replays stored.
	if err := p.b.LeaveMatch(); err != nil {
		t.Fatal(err)
	}
	set = waitSet(t, p.matchID, "session ended", func(s map[string]any) bool {
		return len(s["replays"].([]any)) == 2 && s["ended"] != nil
	})
	if len(set["left_early"].([]any)) != 1 {
		t.Fatalf("left early: %v", set["left_early"])
	}
	for _, r := range set["replays"].([]any) {
		if r.(map[string]any)["complete"] != false {
			t.Fatalf("replay %v", r)
		}
	}
	matches, out := exportReplay(t, p.matchID, ida)
	if len(matches) != 1 || out["complete"] != false {
		t.Fatalf("export %v", out)
	}
	// Without an end, the session holds the frames that arrived.
	if got := matches[0].Sessions[0].BufferedThrough(); got != 240 {
		t.Fatalf("incomplete replay holds %d frames", got)
	}
	// B, the remaining player, claims the set; A never does: a moderator
	// decides (here A's claim still counts while the set is open).
	if out, err := claim(p.b, p.matchID, 1, idb); err != nil || out["status"] != "open" {
		t.Fatalf("claim %v %v", out, err)
	}
	t.Logf("left early %s; replays incomplete; set open with one claim", ida[:8])
}

// "Play Another Set": the session records the next set, whose claims and
// replays are its own, while the first set's last stream still belongs to
// the first set.
func TestNakamaLive_RankedNextSet(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 1)
	p.joinBoth(t)
	info := rankedMatchInfo(p, 1)
	sa := publishRankedStream(t, p.a, info, 0, 120, 9, false)
	sb := publishRankedStream(t, p.b, info, 0, 120, 10, false)
	// Both choose to play another set after the set's match.
	body, _ := json.Marshal(map[string]any{"version": 1, "action": "new_set", "round": 1, "set_final": true})
	_ = p.a.SendMatchData(nakamaRematchChoiceOp, body, true)
	_ = p.b.SendMatchData(nakamaRematchChoiceOp, body, true)
	if _, ok := p.recA.waitFor(t, 5*time.Second, func(ev NakamaEvent) bool {
		m, _ := ev.Payload.(map[string]any)
		return ev.Type == "rematch_decision" && m["action"] == "new_set"
	}); !ok {
		t.Fatal("no new_set decision")
	}
	second := p.matchID + "#2"
	set2 := waitSet(t, second, "second set recorded", func(s map[string]any) bool { return s["status"] == "open" })
	if set2["set"] != float64(2) || set2["session"] != p.matchID || set2["best_of"] != float64(1) {
		t.Fatalf("second set %v", set2)
	}
	if set1 := rankedSet(t, p.matchID); set1["ended"] == nil {
		t.Fatalf("the first set has not ended: %v", set1)
	}
	// The first set's match ends now: its streams end, and its replays are
	// stored with the first set.
	for _, x := range []struct {
		c      *NakamaClient
		stream string
		salt   byte
	}{{p.a, sa, 9}, {p.b, sb, 10}} {
		sink := &nakamaReplaySink{client: x.c}
		sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: 120, Stream: x.stream})
	}
	waitSet(t, p.matchID, "first set's replays", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
	// The second set's match.
	publishRankedStream(t, p.a, rankedMatchInfo(p, 2), 0, 60, 11, true)
	publishRankedStream(t, p.b, rankedMatchInfo(p, 2), 0, 60, 12, true)
	sendSetFinal(t, p.a, "exit", 2)
	waitSet(t, second, "second set's replays", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
	m1, _ := exportReplay(t, p.matchID, p.a.Session().UserID)
	m2, _ := exportReplay(t, second, p.a.Session().UserID)
	checkFrames(t, m1[0].Sessions[0], 120, 9)
	checkFrames(t, m2[0].Sessions[0], 60, 11)
	// Each set is claimed and rated by itself: A wins the first, B the second.
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	for _, c := range []*NakamaClient{p.a, p.b} {
		if _, err := claim(c, p.matchID, 1, ida); err != nil {
			t.Fatal(err)
		}
		if _, err := claim(c, p.matchID, 2, idb); err != nil {
			t.Fatal(err)
		}
	}
	if rankedSet(t, p.matchID)["status"] != "settled" || rankedSet(t, second)["status"] != "settled" {
		t.Fatal("the sets did not settle")
	}
	ra := ratingOf(t, p.a, p.game)
	if ra["games"] != float64(2) || ra["wins"] != float64(1) || ra["losses"] != float64(1) {
		t.Fatalf("A after two sets: %v", ra)
	}
	t.Logf("two sets in one session, each rated once: A %v", ra)
}

// rankedLimits skips a test that needs the server's short limits.
func rankedLimits(t *testing.T) {
	t.Helper()
	if os.Getenv("NAKAMA_RANKED_LIMITS") != "1" {
		t.Skip("needs a server with IKEMEN_RANKED_CLAIM_MINUTES=0.05, IKEMEN_RANKED_REPLAY_DAYS=0.0001 and IKEMEN_RANKED_REPLAY_MAX_BYTES=40000 (NAKAMA_RANKED_LIMITS=1)")
	}
}

// Short limits (NAKAMA_RANKED_LIMITS=1): a set whose second claim does not
// come in time is disputed, and so is a set nobody claimed after a player
// left during its match; one that nobody claimed and whose players left
// before it began is void; replays expire.
func TestNakamaLive_RankedTimeouts(t *testing.T) {
	fixedEnabled(t)
	rankedLimits(t)
	one := rankedPair(t, 1)
	none := rankedPair(t, 1)
	left := rankedPair(t, 1)
	for _, p := range []*rankedPairing{one, none, left} {
		p.joinBoth(t)
	}
	publishRankedStream(t, one.a, rankedMatchInfo(one, 1), 0, 60, 1, true)
	sendSetFinal(t, one.a, "exit", 1)
	waitSet(t, one.matchID, "replay stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 1 })
	if _, err := claim(one.a, one.matchID, 1, one.a.Session().UserID); err != nil {
		t.Fatal(err)
	}
	publishRankedStream(t, left.a, rankedMatchInfo(left, 1), 0, 60, 2, false)
	waitSet(t, left.matchID, "match begun", func(s map[string]any) bool { return s["played"] == true })
	if s := rankedSet(t, none.matchID); s["played"] != false {
		t.Fatalf("a set without matches: %v", s)
	}
	for _, p := range []*rankedPairing{one, none, left} {
		_ = p.a.LeaveMatch()
		time.Sleep(100 * time.Millisecond)
		_ = p.b.LeaveMatch()
	}
	// 3 seconds for the second claim after the session ends.
	time.Sleep(4 * time.Second)
	_, err := claim(one.b, one.matchID, 1, one.a.Session().UserID)
	wantRPCError(t, "a claim after the time limit", err, 9, "disputed")
	if s := rankedSet(t, one.matchID); s["status"] != "disputed" || s["reason"] != "claim_timeout" {
		t.Fatalf("set with one claim: %v", s)
	}
	if s := rankedSet(t, none.matchID); s["status"] != "void" || s["reason"] != "no_claims" {
		t.Fatalf("set without claims or matches: %v", s)
	}
	if s := rankedSet(t, left.matchID); s["status"] != "disputed" || s["reason"] != "claim_timeout" || len(s["left_early"].([]any)) != 1 {
		t.Fatalf("set without claims, left during its match: %v", s)
	}
	// Replays are kept for 0.0001 days (8.6 seconds).
	time.Sleep(6 * time.Second)
	_, err = serverRPC(t, "ikemen_ranked_replay", map[string]any{"match_id": one.matchID})
	wantRPCError(t, "an expired replay", err, 5, "")
	if s := rankedSet(t, one.matchID); len(s["replays"].([]any)) != 0 {
		t.Fatalf("an expired replay is still listed: %v", s["replays"])
	}
}

// The byte limit (NAKAMA_RANKED_LIMITS=1: 40000 bytes per player and set):
// a stream that does not fit is cut at the limit, and the replay is kept as
// incomplete, with the frames that fit.
func TestNakamaLive_RankedReplayByteLimit(t *testing.T) {
	fixedEnabled(t)
	rankedLimits(t)
	p := rankedPair(t, 1)
	p.joinBoth(t)
	// 600 frames: 20 chunks of about 2.7 KB, after a header of about 1 KB.
	publishRankedStream(t, p.a, rankedMatchInfo(p, 1), 0, 600, 5, true)
	publishRankedStream(t, p.b, rankedMatchInfo(p, 1), 0, 90, 6, true)
	time.Sleep(300 * time.Millisecond)
	sendSetFinal(t, p.a, "exit", 1)
	set := waitSet(t, p.matchID, "both replays stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
	for _, r := range set["replays"].([]any) {
		m := r.(map[string]any)
		bytes := m["bytes"].(float64)
		if bytes > 40000 {
			t.Fatalf("replay over the limit: %v", m)
		}
		if want := m["user_id"] == p.b.Session().UserID; m["complete"] != want {
			t.Fatalf("replay %v: complete should be %v", m, want)
		}
	}
	matches, out := exportReplay(t, p.matchID, p.a.Session().UserID)
	got := matches[0].Sessions[0].BufferedThrough()
	if out["complete"] != false || got <= 0 || got >= 600 || got%30 != 0 {
		t.Fatalf("cut replay: %d frames, %v", got, out)
	}
	// The cut stream ends where its frames do: an end past them (the stream's
	// own end, which still fits) would stall the replay there.
	if final, ended := matches[0].Sessions[0].FinalFrame(); !ended || final != got {
		t.Fatalf("cut replay: %d frames, but its session ends at %d (ended %v)", got, final, ended)
	}
	whole, _ := exportReplay(t, p.matchID, p.b.Session().UserID)
	checkFrames(t, whole[0].Sessions[0], 90, 6)
	t.Logf("a stream of 600 frames was cut at %d frames (40000-byte limit); the other player's replay is complete", got)
}

// A player's game cannot fill the server with replay data faster than play
// makes it: the session keeps at most 256 KiB of a player's streams, plus
// 16 KiB for each second the session has lasted, over all its sets. Past
// that, the player's replay of the set is cut where its frames stop, and
// another set does not start a new allowance.
func TestNakamaLive_RankedReplayPaced(t *testing.T) {
	fixedEnabled(t)
	const allowance, rate = 256 * 1024, 16 * 1024
	start := time.Now()
	p := rankedPair(t, 1)
	p.joinBoth(t)
	// 9000 frames (two and a half minutes of play, about 800 KB) in about a
	// second, from A; B's game plays.
	publishRankedStream(t, p.a, rankedMatchInfo(p, 1), 0, 9000, 5, true)
	publishRankedStream(t, p.b, rankedMatchInfo(p, 1), 0, 90, 6, true)
	limit := float64(allowance) + rate*(time.Since(start).Seconds()+1)
	time.Sleep(300 * time.Millisecond)
	sendSetFinal(t, p.a, "new_set", 1)
	sendSetFinal(t, p.b, "new_set", 1)
	set := waitSet(t, p.matchID, "both replays stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
	for _, r := range set["replays"].([]any) {
		m := r.(map[string]any)
		if m["user_id"] == p.a.Session().UserID {
			if m["bytes"].(float64) > limit || m["complete"] != false {
				t.Fatalf("A's replay %v: more than %.0f bytes, or complete", m, limit)
			}
		} else if m["complete"] != true {
			t.Fatalf("B's replay %v", m)
		}
	}
	matches, _ := exportReplay(t, p.matchID, p.a.Session().UserID)
	got := matches[0].Sessions[0].BufferedThrough()
	if final, ended := matches[0].Sessions[0].FinalFrame(); got <= 0 || got >= 9000 || !ended || final != got {
		t.Fatalf("cut replay: %d frames, ends at %d", got, final)
	}
	checkFrames(t, matches[0].Sessions[0], got, 5)
	// The second set: A's allowance is what the seconds since have added.
	waitSet(t, p.matchID+"#2", "second set recorded", func(s map[string]any) bool { return s["status"] == "open" })
	cutAt := time.Now()
	publishRankedStream(t, p.a, rankedMatchInfo(p, 1), 0, 1800, 7, true)
	limit = rate * (time.Since(cutAt).Seconds() + 1)
	time.Sleep(300 * time.Millisecond)
	sendSetFinal(t, p.a, "exit", 2)
	set = waitSet(t, p.matchID+"#2", "A's replay stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 1 })
	if m := set["replays"].([]any)[0].(map[string]any); m["bytes"].(float64) > limit || m["complete"] != false {
		t.Fatalf("A's replay of the second set %v: more than %.0f bytes, or complete", m, limit)
	}
	t.Logf("9000 frames sent in about a second: %d kept; the next set's 1800 frames: %v bytes kept", got,
		set["replays"].([]any)[0].(map[string]any)["bytes"])
}

// A claim alone does not decide a set for the session: a player who claims
// a set during it and then leaves is noted as having left early, and that
// player's replay is incomplete.
func TestNakamaLive_RankedClaimThenLeave(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	p.joinBoth(t)
	info := rankedMatchInfo(p, 1)
	publishRankedStream(t, p.a, info, 0, 120, 1, true)
	publishRankedStream(t, p.b, info, 0, 120, 2, true)
	time.Sleep(300 * time.Millisecond)
	idb := p.b.Session().UserID
	if out, err := claim(p.b, p.matchID, 1, idb); err != nil || out["status"] != "open" {
		t.Fatalf("claim %v %v", out, err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := p.b.LeaveMatch(); err != nil {
		t.Fatal(err)
	}
	set := waitSet(t, p.matchID, "B left early", func(s map[string]any) bool {
		left, _ := s["left_early"].([]any)
		return len(left) == 1 && len(s["replays"].([]any)) == 1
	})
	r := set["replays"].([]any)[0].(map[string]any)
	if set["left_early"].([]any)[0] != idb || r["user_id"] != idb || r["complete"] != false {
		t.Fatalf("set %v", set)
	}
}

// A moderator's note has at most 500 characters, counted as characters, not
// as bytes.
func TestNakamaLive_RankedNoteCharacters(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 1)
	_, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true, "note": strings.Repeat("審", 501)})
	wantRPCError(t, "a note of 501 characters", err, 3, "500 characters")
	note := strings.Repeat("審", 500) // 1500 bytes
	if res, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true, "note": note}); err != nil || res["status"] != "void" {
		t.Fatalf("a note of 500 characters: %v %v", res, err)
	}
	if set := rankedSet(t, p.matchID); set["note"] != note {
		t.Fatalf("note %q", set["note"])
	}
}

// A helper for testing moderation tools: leaves NAKAMA_RANKED_DISPUTED
// disputed sets, each with both players' replays of a short match.
func TestNakamaLive_RankedMakeDisputed(t *testing.T) {
	fixedEnabled(t)
	n, _ := strconv.Atoi(os.Getenv("NAKAMA_RANKED_DISPUTED"))
	if n <= 0 {
		t.Skip("set NAKAMA_RANKED_DISPUTED=<n> to leave n disputed sets on the server")
	}
	for i := 0; i < n; i++ {
		p := rankedPair(t, 1)
		p.joinBoth(t)
		for j, c := range []*NakamaClient{p.a, p.b} {
			publishRankedStream(t, c, rankedMatchInfo(p, 1), 0, 120, byte(j+1), true)
		}
		sendSetFinal(t, p.a, "exit", 1)
		waitSet(t, p.matchID, "replays stored", func(s map[string]any) bool { return len(s["replays"].([]any)) == 2 })
		_, _ = claim(p.a, p.matchID, 1, p.a.Session().UserID)
		_, _ = claim(p.b, p.matchID, 1, p.b.Session().UserID)
		set := rankedSet(t, p.matchID)
		if set["status"] != "disputed" {
			t.Fatalf("set %v", set)
		}
		fmt.Printf("disputed ranked set %s: players %s (%s) and %s (%s)\n", p.matchID,
			p.a.Session().UserID, p.a.Session().Username, p.b.Session().UserID, p.b.Session().Username)
	}
}
