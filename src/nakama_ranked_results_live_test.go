package main

// Live tests of what a settled ranked set keeps (ikemen_rating.lua): each
// player's rating and level before and after the set in its record, in the
// answer to a claim (the claim that settles it, and a claim sent again),
// in the moderators' list, and the claims the game sends again until their
// answer is final (nakama_ranked_queue.go). Enable with NAKAMA_LIVE=1 (see
// nakama_live_test.go and nakama_ranked_live_test.go).

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// resultOf returns an answer's or a list's result object.
func resultOf(t *testing.T, what string, v any) map[string]any {
	t.Helper()
	r, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: no result: %v", what, v)
	}
	for _, key := range []string{"rating_before", "rating", "level_before", "level", "change"} {
		if _, ok := r[key].(float64); !ok {
			t.Fatalf("%s: %s is %v", what, key, r[key])
		}
	}
	for _, key := range []string{"level_before_text", "level_text", "change_text", "label"} {
		if s, ok := r[key].(string); !ok || s == "" {
			t.Fatalf("%s: %s is %v", what, key, r[key])
		}
	}
	if r["level"].(float64)-r["level_before"].(float64) != r["change"].(float64) {
		t.Fatalf("%s: change %v", what, r)
	}
	return r
}

func sameResult(t *testing.T, what string, a, b map[string]any) {
	t.Helper()
	for _, key := range []string{"won", "label", "rating_before", "rating", "level_before", "level", "level_before_text", "level_text", "change", "change_text"} {
		if a[key] != b[key] {
			t.Fatalf("%s: %s %v and %v", what, key, a[key], b[key])
		}
	}
}

// settledNotification waits for a player's ikemen_ranked_settled
// notification of the set and returns its content.
func settledNotification(t *testing.T, rec *evRecorder, id string) map[string]any {
	t.Helper()
	ev, ok := rec.waitFor(t, 10*time.Second, func(ev NakamaEvent) bool {
		p, _ := ev.Payload.(map[string]any)
		c, _ := p["content"].(map[string]any)
		return ev.Type == "notification" && p["subject"] == "ikemen_ranked_settled" && c["match_id"] == id
	})
	if !ok {
		t.Fatalf("no ikemen_ranked_settled for %s", id)
	}
	c := ev.Payload.(map[string]any)["content"].(map[string]any)
	// The notification's numbers are json.Number: as float64 here.
	out := map[string]any{}
	for k, v := range c {
		if n, ok := v.(json.Number); ok {
			f, _ := n.Float64()
			out[k] = f
		} else {
			out[k] = v
		}
	}
	return out
}

func TestNakamaLive_RankedResultsKept(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	first, err := claim(p.a, p.matchID, 1, ida)
	if err != nil || first["status"] != "open" || first["result"] != nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	second, err := claim(p.b, p.matchID, 1, ida)
	if err != nil || second["status"] != "settled" {
		t.Fatalf("second claim: %v %v", second, err)
	}
	rb := resultOf(t, "B's settling answer", second["result"])
	if rb["won"] != false || rb["rating"].(float64) >= rb["rating_before"].(float64) {
		t.Fatalf("B lost: %v", rb)
	}
	if rb["rating"] != second["rating"] {
		t.Fatalf("B's rating %v, result %v", second["rating"], rb)
	}
	// A claim sent again, by the player whose first answer was "open".
	again, err := claim(p.a, p.matchID, 1, ida)
	if err != nil || again["status"] != "settled" || again["settled"] != true {
		t.Fatalf("A's claim sent again: %v %v", again, err)
	}
	ra := resultOf(t, "A's repeated answer", again["result"])
	if ra["won"] != true || ra["rating"].(float64) <= ra["rating_before"].(float64) {
		t.Fatalf("A won: %v", ra)
	}
	if ra["rating_before"] != rb["rating_before"] {
		// Both started at the game's initial rating.
		t.Fatalf("before: A %v, B %v", ra["rating_before"], rb["rating_before"])
	}
	t.Logf("A: %v %s -> %s (%s); B: %s -> %s (%s)", ra["label"], ra["level_before_text"], ra["level_text"], ra["change_text"],
		rb["level_before_text"], rb["level_text"], rb["change_text"])
	// The same as the notifications.
	sameResult(t, "A's notification", ra, settledNotification(t, p.recA, p.matchID))
	sameResult(t, "B's notification", rb, settledNotification(t, p.recB, p.matchID))
	// B's claim sent again has the same result as B's settling answer.
	bAgain, err := claim(p.b, p.matchID, 1, ida)
	if err != nil {
		t.Fatal(err)
	}
	sameResult(t, "B's repeated answer", rb, resultOf(t, "B's repeated answer", bAgain["result"]))
	// The record keeps both, in the moderators' list.
	set := rankedSet(t, p.matchID)
	for _, pl := range set["players"].([]any) {
		m := pl.(map[string]any)
		r := resultOf(t, "the list's "+m["user_id"].(string), m["result"])
		switch m["user_id"] {
		case ida:
			sameResult(t, "A in the list", ra, r)
		case idb:
			sameResult(t, "B in the list", rb, r)
		default:
			t.Fatalf("player %v", m)
		}
	}
	// A later set changes the players' ratings, not this set's results.
	q := rankedPairOf(t, p.a, p.b, p.recA, p.recB, p.game, 1)
	if _, err := claim(q.a, q.matchID, 1, idb); err != nil {
		t.Fatal(err)
	}
	if out, err := claim(q.b, q.matchID, 1, idb); err != nil || out["status"] != "settled" {
		t.Fatalf("the later set: %v %v", out, err)
	}
	later, err := claim(p.a, p.matchID, 1, ida)
	if err != nil {
		t.Fatal(err)
	}
	sameResult(t, "A's answer after a later set", ra, resultOf(t, "A's answer after a later set", later["result"]))
	if later["rating"] == ra["rating"] {
		t.Fatalf("the answer's rating is the rating now: %v", later)
	}
}

// A set a moderator settles keeps its results too.
func TestNakamaLive_RankedResultsKeptByModerator(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if _, err := claim(p.a, p.matchID, 1, ida); err != nil {
		t.Fatal(err)
	}
	if out, err := claim(p.b, p.matchID, 1, idb); err != nil || out["status"] != "disputed" {
		t.Fatalf("disagreeing claims: %v %v", out, err)
	}
	if set := rankedSet(t, p.matchID); set["players"].([]any)[0].(map[string]any)["result"] != nil {
		t.Fatalf("a disputed set has results: %v", set)
	}
	if _, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": idb}); err != nil {
		t.Fatal(err)
	}
	again, err := claim(p.b, p.matchID, 1, idb)
	if err != nil || again["status"] != "settled" {
		t.Fatalf("B's claim sent again: %v %v", again, err)
	}
	rb := resultOf(t, "B's answer", again["result"])
	if rb["won"] != true {
		t.Fatalf("B won: %v", rb)
	}
	sameResult(t, "B's notification", rb, settledNotification(t, p.recB, p.matchID))
	for _, pl := range rankedSet(t, p.matchID)["players"].([]any) {
		m := pl.(map[string]any)
		r := resultOf(t, "the list", m["result"])
		if (m["user_id"] == idb) != (r["won"] == true) {
			t.Fatalf("list %v", m)
		}
	}
}

// The game's queue sends a claim until its answer is final: the first
// answer is "open", the one 5 seconds later "settled", with the result.
// It sends as the account that played, with a client of its own when the
// game restarted (no client).
func TestNakamaLive_RankedClaimQueue(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida := p.a.Session().UserID
	q := &rankedClaimQueue{path: filepath.Join(t.TempDir(), "ranked_pending.json")}
	answers := make(chan rankedAnswer, 8)
	q.setListener(func(a rankedAnswer) { answers <- a })
	next := func() rankedAnswer {
		select {
		case a := <-answers:
			return a
		case <-time.After(30 * time.Second):
			t.Fatal("no answer")
		}
		return rankedAnswer{}
	}
	// As after a restart: the queue has the server and the device id alone.
	q.add(rankedPendingClaim{
		SetID: p.matchID, MatchID: p.matchID, Set: 1, Winner: ida, UserID: ida,
		Server: rankedPendingServer{Host: "127.0.0.1", Port: livePort(), ServerKey: "defaultkey", DeviceID: p.a.cfg.DeviceID},
	}, nil)
	if a := next(); a.Status != "open" || a.Final {
		t.Fatalf("first: %+v %s", a, a.Body)
	}
	if out, err := claim(p.b, p.matchID, 1, ida); err != nil || out["status"] != "settled" {
		t.Fatalf("B: %v %v", out, err)
	}
	start := time.Now()
	a := next()
	if a.Status != "settled" || !a.Final || a.Sends != 2 {
		t.Fatalf("second: %+v %s", a, a.Body)
	}
	if d := time.Since(start); d < 4*time.Second || d > 8*time.Second {
		t.Fatalf("sent again after %v", d)
	}
	r := resultOf(t, "the queue's answer", a.luaPayload()["result"])
	if r["won"] != true {
		t.Fatalf("result %v", r)
	}
	if len(q.pending()) != 0 {
		t.Fatalf("still queued")
	}
	// Another account cannot send it: refused (final), not sent as someone else.
	q.add(rankedPendingClaim{
		SetID: p.matchID, MatchID: p.matchID, Set: 1, Winner: ida, UserID: "00000000-0000-0000-0000-00000000beef",
		Server: rankedPendingServer{Host: "127.0.0.1", Port: livePort(), ServerKey: "defaultkey", DeviceID: p.a.cfg.DeviceID},
	}, nil)
	if a := next(); a.Status != "refused" || !a.Final {
		t.Fatalf("another account: %+v %v", a, a.Err)
	}
	// The client the set was played with sends it when it is signed in as
	// the account.
	q.add(rankedPendingClaim{
		SetID: p.matchID, MatchID: p.matchID, Set: 1, Winner: ida, UserID: ida,
		Server: rankedPendingServer{Host: "127.0.0.1", Port: 1, ServerKey: "defaultkey"},
	}, p.a)
	if a := next(); a.Status != "settled" || !a.Final {
		t.Fatalf("with the session's client: %+v %v", a, a.Err)
	}
}

// A claim that did not count, on a set that is no longer open, is answered
// with what became of the set: here a set a moderator settled before the
// player's claim came. Its answer is final, "settled" (the server's refusal,
// code 9, without the set's result), not a refusal of the claim.
func TestNakamaLive_RankedClaimQueueClosedSet(t *testing.T) {
	fixedEnabled(t)
	p := rankedPair(t, 3)
	ida, idb := p.a.Session().UserID, p.b.Session().UserID
	if out, err := claim(p.a, p.matchID, 1, ida); err != nil || out["status"] != "open" {
		t.Fatalf("A: %v %v", out, err)
	}
	if _, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": ida}); err != nil {
		t.Fatal(err)
	}
	q := &rankedClaimQueue{path: filepath.Join(t.TempDir(), "ranked_pending.json")}
	answers := make(chan rankedAnswer, 2)
	q.setListener(func(a rankedAnswer) { answers <- a })
	q.add(rankedPendingClaim{SetID: p.matchID, MatchID: p.matchID, Set: 1, Winner: ida, UserID: idb}, p.b)
	select {
	case a := <-answers:
		if a.Status != "settled" || !a.Final || nakamaRPCErrorCode(a.Err) != 9 {
			t.Fatalf("B's claim: %+v %v", a, a.Err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no answer")
	}
	if len(q.pending()) != 0 {
		t.Fatalf("still queued")
	}
}
