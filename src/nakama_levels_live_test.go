package main

// Live tests of ranked levels, leaderboards and the ranked notifications
// (nakama/modules/ikemen_levels.lua and ikemen_rating.lua): the level
// fields of the answers, the leaderboard entries and notifications of a
// settlement by the players or by a moderator, the notice of a dispute,
// nothing for a void set, the leaderboard's order and pages, and that
// players cannot write to it. Enable with NAKAMA_LIVE=1 (see
// nakama_live_test.go for the server flags).
//
// NAKAMA_LEVELS says which IKEMEN_LEVELS the server's runtime.env has:
//   (empty)   none: the plain Elo ratings, as for the other live tests;
//   example   liveLevelsExample below;
//   bad       liveLevelsBad below, which is not valid.
// Most tests run in every mode and expect its levels; those of one mode
// skip the others. With NAKAMA_LOG naming the server's log file, they also
// check the line the server logs about the settings when it starts.
//
// TestNakamaLive_LeaderboardEntryRestored deletes a leaderboard record with
// Nakama's console API: NAKAMA_CONSOLE_PORT (7351), NAKAMA_CONSOLE_USER and
// NAKAMA_CONSOLE_PASSWORD (Nakama's defaults: admin, password). It skips
// when it cannot sign in.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The IKEMEN_LEVELS of a server for NAKAMA_LEVELS=example: the README's
// example as the default, a game of its own, a game near the cap and a game
// with a label of its own.
const liveLevelsExample = `{"default":{"start":9001,"top":1000000000,"label":"Power level"},"games":{"lv-small":{"start":100,"top":10000,"label":"Rank"},"lv-cap":{"start":8900000000000000,"top":9000000000000000},"lv-label":{"label":"Score"}}}`

// The IKEMEN_LEVELS of a server for NAKAMA_LEVELS=bad: top is less than
// start.
const liveLevelsBad = `{"default":{"start":9001,"top":10,"label":"Power level"}}`

func levelsMode() string {
	return os.Getenv("NAKAMA_LEVELS")
}

func levelsModeOnly(t *testing.T, mode string) {
	t.Helper()
	if levelsMode() != mode {
		t.Skipf("needs a server whose IKEMEN_LEVELS is NAKAMA_LEVELS=%s's (see nakama_levels_live_test.go)", mode)
	}
}

type liveLevelEntry struct {
	start, top float64 // 0: the plain Elo ratings
	label      string
}

// liveLevelsOf is the entry a game's levels come from in this mode.
func liveLevelsOf(game string) liveLevelEntry {
	if levelsMode() == "example" {
		switch game {
		case "lv-small":
			return liveLevelEntry{100, 10000, "Rank"}
		case "lv-cap":
			return liveLevelEntry{8.9e15, 9e15, "Power level"}
		case "lv-label":
			return liveLevelEntry{9001, 1e9, "Score"}
		}
		return liveLevelEntry{9001, 1e9, "Power level"}
	}
	return liveLevelEntry{0, 0, "Rating"}
}

// liveLevel is the level, its text and its label that the server shows for
// an Elo rating of game.
func liveLevel(game string, elo float64) (float64, string, string) {
	e := liveLevelsOf(game)
	var v float64
	if e.start == 0 {
		v = math.Floor(elo + 0.5)
	} else {
		v = expectedLevel(e.start, e.top, elo)
	}
	return v, groupedDigits(v), e.label
}

func signedDigits(v float64) string {
	if v >= 0 {
		return "+" + groupedDigits(v)
	}
	return groupedDigits(v)
}

// eloAfter is the winner's and the loser's new rating, as ikemen_rating.lua
// computes them.
func eloAfter(w, l float64) (float64, float64) {
	ew := 1 / (1 + math.Pow(10, (l-w)/400))
	el := 1 - ew
	return w + 32*(1-ew), l + 32*(0-el)
}

// num reads a JSON number: float64 (json.Unmarshal) or json.Number (the
// notifications' content).
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f
		}
	case int64:
		return float64(x)
	}
	return math.NaN()
}

// rankedNotifications are the ranked notifications rec has received for a
// set: their payloads.
func rankedNotifications(rec *evRecorder, matchID string) []map[string]any {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []map[string]any
	for _, ev := range rec.evs {
		p, _ := ev.Payload.(map[string]any)
		if ev.Type != "notification" || p == nil {
			continue
		}
		content, _ := p["content"].(map[string]any)
		if content != nil && content["match_id"] == matchID {
			out = append(out, p)
		}
	}
	return out
}

// waitNotification waits up to 5 seconds for the notification of subject
// for the set, and returns its content. It must be the only ranked
// notification of the set with that subject.
func waitNotification(t *testing.T, rec *evRecorder, subject, matchID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var found []map[string]any
		for _, p := range rankedNotifications(rec, matchID) {
			if p["subject"] == subject {
				found = append(found, p)
			}
		}
		if len(found) > 1 {
			t.Fatalf("%d notifications %s for set %s: %v", len(found), subject, matchID, found)
		}
		if len(found) == 1 {
			code := map[string]int64{"ikemen_ranked_settled": 4101, "ikemen_ranked_disputed": 4102}[subject]
			if found[0]["code"] != code || found[0]["persistent"] != true {
				t.Fatalf("notification %v", found[0])
			}
			return found[0]["content"].(map[string]any)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no notification %s for set %s", subject, matchID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// checkSettled checks a player's ikemen_ranked_settled content.
func checkSettled(t *testing.T, who string, c map[string]any, matchID, game string, won bool, before, after float64, by string, wins, losses int) {
	t.Helper()
	lb, lbText, label := liveLevel(game, before)
	la, laText, _ := liveLevel(game, after)
	want := map[string]any{
		"match_id": matchID, "session": strings.SplitN(matchID, "#", 2)[0], "game": game, "won": won, "settled_by": by, "label": label,
		"level_before_text": lbText, "level_text": laText, "change_text": signedDigits(la - lb),
	}
	for k, v := range want {
		if c[k] != v {
			t.Fatalf("%s: %s = %v, want %v (%v)", who, k, c[k], v, c)
		}
	}
	for k, v := range map[string]float64{"rating_before": before, "rating": after, "level_before": lb, "level": la, "change": la - lb,
		"wins": float64(wins), "losses": float64(losses)} {
		if num(c[k]) != v {
			t.Fatalf("%s: %s = %v, want %v (%v)", who, k, c[k], v, c)
		}
	}
	t.Logf("%s: %s %s -> %s (%s)", who, label, lbText, laText, c["change_text"])
}

// checkLevelFields checks the level fields of an answer for an Elo rating
// (the label too, except in a player of the moderators' list, whose set has
// the label).
func checkLevelFields(t *testing.T, what string, out map[string]any, game string, elo float64, prefix string) {
	t.Helper()
	lv, text, label := liveLevel(game, elo)
	if num(out[prefix+"rating"]) != elo || num(out[prefix+"level"]) != lv || out[prefix+"level_text"] != text {
		t.Fatalf("%s: %s rating %v level %v %v, want %v %v %q", what, prefix, out[prefix+"rating"], out[prefix+"level"], out[prefix+"level_text"], elo, lv, text)
	}
	if prefix == "" && !strings.HasPrefix(what, "the moderators' list") && out["label"] != label {
		t.Fatalf("%s: label %v, want %q", what, out["label"], label)
	}
}

func leaderboardRPC(t *testing.T, c *NakamaClient, payload map[string]any) (map[string]any, error) {
	t.Helper()
	body, err := c.CallRPC(context.Background(), "ikemen_leaderboard", payload)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("leaderboard answer %q: %v", body, err)
	}
	return out, nil
}

func leaderboardOf(t *testing.T, c *NakamaClient, payload map[string]any) map[string]any {
	t.Helper()
	out, err := leaderboardRPC(t, c, payload)
	if err != nil {
		t.Fatalf("ikemen_leaderboard %v: %v", payload, err)
	}
	return out
}

// checkRecord checks a leaderboard record.
func checkRecord(t *testing.T, what string, r any, userID, name, game string, elo float64, wins, losses, rank int) {
	t.Helper()
	m, _ := r.(map[string]any)
	lv, text, _ := liveLevel(game, elo)
	if m == nil || m["user_id"] != userID || m["name"] != name || num(m["level"]) != lv || m["level_text"] != text ||
		num(m["wins"]) != float64(wins) || num(m["losses"]) != float64(losses) || (rank > 0 && num(m["rank"]) != float64(rank)) {
		t.Fatalf("%s: record %v, want %s %q level %v %q %d-%d rank %d", what, r, userID, name, lv, text, wins, losses, rank)
	}
	if len(m) != 7 {
		t.Fatalf("%s: record fields %v", what, m)
	}
}

// onlineNameOf is what the leaderboard calls a player: the display name, or
// the username until the player chooses a name, cut to 16 characters.
func onlineNameOf(t *testing.T, c *NakamaClient) string {
	t.Helper()
	acc, err := c.GetAccount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := acc.DisplayName
	if name == "" {
		name = acc.Username
	}
	if r := []rune(name); len(r) > 16 {
		name = string(r[:16])
	}
	return name
}

// settledSet plays a ranked set of game between two new players, A winning
// it by both players' claims, and checks the answers, the notifications and
// the leaderboard entries. A has chosen an online name, B has not.
func settledSet(t *testing.T, game string) (p *rankedPairing) {
	t.Helper()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	if _, err := a.SetAccountName(context.Background(), "Lv "+randID()[7:13]); err != nil {
		t.Fatal(err)
	}
	p = rankedPairOf(t, a, b, recA, recB, game, 1)
	ida, idb := a.Session().UserID, b.Session().UserID
	if out, err := claim(a, p.matchID, 1, ida); err != nil || out["status"] != "open" || out["level"] != nil {
		t.Fatalf("first claim: %v %v", out, err)
	}
	if n := rankedNotifications(recA, p.matchID); len(n) != 0 {
		t.Fatalf("a notification before the set settled: %v", n)
	}
	wElo, lElo := eloAfter(1000, 1000)
	out, err := claim(b, p.matchID, 1, ida)
	if err != nil || out["status"] != "settled" {
		t.Fatalf("second claim: %v %v", out, err)
	}
	checkLevelFields(t, "the settling claim's answer", out, game, lElo, "")
	checkLevelFields(t, "the settling claim's answer", out, game, wElo, "opponent_")
	again, err := claim(a, p.matchID, 1, ida)
	if err != nil || again["settled"] != true {
		t.Fatalf("a claim sent again: %v %v", again, err)
	}
	checkLevelFields(t, "a claim sent again", again, game, wElo, "")

	checkSettled(t, "A", waitNotification(t, recA, "ikemen_ranked_settled", p.matchID), p.matchID, game, true, 1000, wElo, "players", 1, 0)
	checkSettled(t, "B", waitNotification(t, recB, "ikemen_ranked_settled", p.matchID), p.matchID, game, false, 1000, lElo, "players", 0, 1)
	for _, rec := range []*evRecorder{recA, recB} {
		if n := rankedNotifications(rec, p.matchID); len(n) != 1 {
			t.Fatalf("ranked notifications of the set: %v", n)
		}
	}

	nameA, nameB := onlineNameOf(t, a), onlineNameOf(t, b)
	boardA := leaderboardOf(t, a, map[string]any{"game": game})
	boardB := leaderboardOf(t, b, map[string]any{"game": game})
	_, _, label := liveLevel(game, 1000)
	if boardA["label"] != label {
		t.Fatalf("leaderboard label %v", boardA["label"])
	}
	checkRecord(t, "A's own record", boardA["mine"], ida, nameA, game, wElo, 1, 0, 0)
	checkRecord(t, "B's own record", boardB["mine"], idb, nameB, game, lElo, 0, 1, 0)
	if num(boardA["mine"].(map[string]any)["rank"]) >= num(boardB["mine"].(map[string]any)["rank"]) {
		t.Fatalf("A ranks below B: %v %v", boardA["mine"], boardB["mine"])
	}
	if r := ratingOf(t, a, game); num(r["rating"]) != wElo || r["games"] != float64(1) {
		t.Fatalf("A's rating %v", r)
	} else {
		checkLevelFields(t, "A's ikemen_get_rating", r, game, wElo, "")
	}
	return p
}

// A new player's answer carries the level of the rating new players start
// with, and the game's label; a game without settled sets has an empty
// leaderboard, and a player without sets no record.
func TestNakamaLive_LevelsAnswer(t *testing.T) {
	fixedEnabled(t)
	a, _ := liveClient(t)
	games := []string{"lv-" + randID()}
	if levelsMode() == "example" {
		games = append(games, "lv-small", "lv-label", "lv-cap")
	}
	for _, game := range games {
		r := ratingOf(t, a, game)
		if r["games"] != float64(0) || r["wins"] != float64(0) || r["losses"] != float64(0) {
			t.Fatalf("%s: %v", game, r)
		}
		checkLevelFields(t, game, r, game, 1000, "")
		if len(r) != 7 {
			t.Fatalf("%s: fields %v", game, r)
		}
		t.Logf("%s: %v %v", game, r["label"], r["level_text"])
	}
	board := leaderboardOf(t, a, map[string]any{"game": games[0]})
	if records, ok := board["records"].([]any); !ok || len(records) != 0 || board["cursor"] != "" || board["mine"] != nil {
		t.Fatalf("a game without sets: %v", board)
	}
	_, _, label := liveLevel(games[0], 1000)
	if board["label"] != label {
		t.Fatalf("label %v", board["label"])
	}
	_, err := a.CallRPC(context.Background(), "ikemen_get_rating", map[string]any{})
	wantRPCError(t, "ikemen_get_rating without game", err, 3, "game is required")
}

// A set settled by its players: both players' entries on the game's
// leaderboard, and a notification to each with their level before and
// after.
func TestNakamaLive_LevelsSettled(t *testing.T) {
	fixedEnabled(t)
	game := "lv-" + randID()
	p := settledSet(t, game)
	// The game's leaderboard has the two players, the winner first.
	board := leaderboardOf(t, p.b, map[string]any{"game": game})
	records, _ := board["records"].([]any)
	wElo, lElo := eloAfter(1000, 1000)
	if len(records) != 2 || board["cursor"] != "" {
		t.Fatalf("leaderboard %v", board)
	}
	checkRecord(t, "first", records[0], p.a.Session().UserID, onlineNameOf(t, p.a), game, wElo, 1, 0, 1)
	checkRecord(t, "second", records[1], p.b.Session().UserID, onlineNameOf(t, p.b), game, lElo, 0, 1, 2)
	if levelsMode() == "example" {
		// A game of its own.
		settledSet(t, "lv-small")
	}
}

// Levels near the cap of 9,000,000,000,000,000 are exact, in the answers,
// the notifications and the leaderboard.
func TestNakamaLive_LevelsNearCap(t *testing.T) {
	fixedEnabled(t)
	levelsModeOnly(t, "example")
	p := settledSet(t, "lv-cap")
	c := waitNotification(t, p.recA, "ikemen_ranked_settled", p.matchID)
	if c["level_before_text"] != "8,900,000,000,000,000" || c["level"] != json.Number(fmt.Sprintf("%d", int64(expectedLevel(8.9e15, 9e15, 1016)))) {
		t.Fatalf("near the cap: %v", c)
	}
	t.Logf("near the cap: %v -> %v (%v)", c["level_before_text"], c["level_text"], c["change_text"])
}

// A disputed set: both players are told; a moderator's settlement then
// writes both leaderboard entries and notifies both, as the players'
// would. The moderators' list has each player's level.
func TestNakamaLive_LevelsModeratorSettle(t *testing.T) {
	fixedEnabled(t)
	game := "lv-" + randID()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	p := rankedPairOf(t, a, b, recA, recB, game, 1)
	ida, idb := a.Session().UserID, b.Session().UserID
	if _, err := claim(a, p.matchID, 1, ida); err != nil {
		t.Fatal(err)
	}
	if out, err := claim(b, p.matchID, 1, idb); err != nil || out["status"] != "disputed" {
		t.Fatalf("disagreeing claims: %v %v", out, err)
	}
	for who, rec := range map[string]*evRecorder{"A": recA, "B": recB} {
		c := waitNotification(t, rec, "ikemen_ranked_disputed", p.matchID)
		if c["reason"] != "claims_disagree" || c["game"] != game || num(c["set"]) != 1 || c["session"] != p.matchID {
			t.Fatalf("%s: dispute notice %v", who, c)
		}
	}
	set := rankedSet(t, p.matchID)
	_, _, label := liveLevel(game, 1000)
	if set["label"] != label {
		t.Fatalf("the list's label: %v", set)
	}
	for _, pl := range set["players"].([]any) {
		checkLevelFields(t, "the moderators' list", pl.(map[string]any), game, 1000, "")
	}
	if board := leaderboardOf(t, a, map[string]any{"game": game}); len(board["records"].([]any)) != 0 {
		t.Fatalf("a disputed set wrote the leaderboard: %v", board)
	}
	res, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "winner": idb})
	if err != nil || res["status"] != "settled" {
		t.Fatalf("settle: %v %v", res, err)
	}
	wElo, lElo := eloAfter(1000, 1000)
	checkSettled(t, "B", waitNotification(t, recB, "ikemen_ranked_settled", p.matchID), p.matchID, game, true, 1000, wElo, "moderator", 1, 0)
	checkSettled(t, "A", waitNotification(t, recA, "ikemen_ranked_settled", p.matchID), p.matchID, game, false, 1000, lElo, "moderator", 0, 1)
	board := leaderboardOf(t, a, map[string]any{"game": game})
	records, _ := board["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("leaderboard %v", board)
	}
	checkRecord(t, "first", records[0], idb, onlineNameOf(t, b), game, wElo, 1, 0, 1)
	checkRecord(t, "second", records[1], ida, onlineNameOf(t, a), game, lElo, 0, 1, 2)
	checkRecord(t, "A's own", board["mine"], ida, onlineNameOf(t, a), game, lElo, 0, 1, 2)
	set = rankedSet(t, p.matchID)
	for _, pl := range set["players"].([]any) {
		m := pl.(map[string]any)
		elo := lElo
		if m["user_id"] == idb {
			elo = wElo
		}
		checkLevelFields(t, "the moderators' list after the settlement", m, game, elo, "")
	}
}

// A void set sends nothing and writes no leaderboard entry.
func TestNakamaLive_LevelsVoidNotifiesNothing(t *testing.T) {
	fixedEnabled(t)
	game := "lv-" + randID()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	p := rankedPairOf(t, a, b, recA, recB, game, 1)
	if _, err := claim(a, p.matchID, 1, a.Session().UserID); err != nil {
		t.Fatal(err)
	}
	if res, err := serverRPC(t, "ikemen_ranked_settle", map[string]any{"match_id": p.matchID, "void": true}); err != nil || res["status"] != "void" {
		t.Fatalf("void: %v %v", res, err)
	}
	time.Sleep(2 * time.Second)
	for _, rec := range []*evRecorder{recA, recB} {
		if n := rankedNotifications(rec, p.matchID); len(n) != 0 {
			t.Fatalf("a void set sent %v", n)
		}
	}
	if board := leaderboardOf(t, a, map[string]any{"game": game}); len(board["records"].([]any)) != 0 || board["mine"] != nil {
		t.Fatalf("a void set wrote the leaderboard: %v", board)
	}
}

// liveLadder is a few players of a game and the ratings, wins and losses
// the server should have for them.
type liveLadder struct {
	game         string
	c            []*NakamaClient
	rec          []*evRecorder
	elo          []float64
	wins, losses []int
}

func newLiveLadder(t *testing.T, players int) *liveLadder {
	t.Helper()
	l := &liveLadder{game: "lv-" + randID(), c: make([]*NakamaClient, players), rec: make([]*evRecorder, players),
		elo: make([]float64, players), wins: make([]int, players), losses: make([]int, players)}
	for i := range l.c {
		l.c[i], l.rec[i] = liveClient(t)
		l.elo[i] = 1000
	}
	return l
}

// play has player w beat player l in a ranked set, by both players' claims.
func (lad *liveLadder) play(t *testing.T, w, l int) {
	t.Helper()
	p := rankedPairOf(t, lad.c[w], lad.c[l], lad.rec[w], lad.rec[l], lad.game, 1)
	for _, cl := range []*NakamaClient{lad.c[w], lad.c[l]} {
		if _, err := claim(cl, p.matchID, 1, lad.c[w].Session().UserID); err != nil {
			t.Fatal(err)
		}
	}
	lad.elo[w], lad.elo[l] = eloAfter(lad.elo[w], lad.elo[l])
	lad.wins[w]++
	lad.losses[l]++
	waitNotification(t, lad.rec[w], "ikemen_ranked_settled", p.matchID)
}

// The leaderboard lists the highest rating first, page by page, with the
// caller's own record; the server's http_key reads it too, without one.
func TestNakamaLive_LeaderboardOrderPaging(t *testing.T) {
	fixedEnabled(t)
	lad := newLiveLadder(t, 4)
	game, c, elo, wins, losses := lad.game, lad.c, lad.elo, lad.wins, lad.losses
	if _, err := c[0].SetAccountName(context.Background(), "Leader"); err != nil {
		t.Fatal(err)
	}
	play := func(w, l int) { lad.play(t, w, l) }
	// 1 beats 2, 1 beats 3, 3 beats 4: 1, 3, 2, 4, all apart.
	play(0, 1)
	play(0, 2)
	play(2, 3)
	order := []int{0, 2, 1, 3}
	t.Logf("ratings %v", elo)
	names := make([]string, 4)
	for i := range c {
		names[i] = onlineNameOf(t, c[i])
	}
	check := func(what string, r any, i, rank int) {
		checkRecord(t, what, r, c[i].Session().UserID, names[i], game, elo[i], wins[i], losses[i], rank)
	}
	first := leaderboardOf(t, c[1], map[string]any{"game": game, "limit": 2})
	page, _ := first["records"].([]any)
	if len(page) != 2 || first["cursor"] == "" {
		t.Fatalf("first page %v", first)
	}
	check("first page", page[0], order[0], 1)
	check("first page", page[1], order[1], 2)
	check("mine", first["mine"], 1, 3)
	second := leaderboardOf(t, c[1], map[string]any{"game": game, "limit": 2, "cursor": first["cursor"]})
	page, _ = second["records"].([]any)
	if len(page) != 2 || second["cursor"] != "" {
		t.Fatalf("second page %v", second)
	}
	check("second page", page[0], order[2], 3)
	check("second page", page[1], order[3], 4)
	// Every record at once (the default limit is 20).
	all := leaderboardOf(t, c[3], map[string]any{"game": game})
	if records := all["records"].([]any); len(records) != 4 || all["cursor"] != "" {
		t.Fatalf("all %v", all)
	}
	check("mine", all["mine"], 3, 4)
	// With the http_key: no player, no record of one's own.
	server, err := serverRPC(t, "ikemen_leaderboard", map[string]any{"game": game, "limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if records := server["records"].([]any); len(records) != 3 || server["mine"] != nil || server["cursor"] == "" {
		t.Fatalf("with the http_key: %v", server)
	}
	check("with the http_key", server["records"].([]any)[0], 0, 1)
	for _, bad := range []map[string]any{{}, {"game": game, "limit": 0}, {"game": game, "limit": 101}, {"game": game, "limit": 2.5},
		{"game": strings.Repeat("g", 115)}} {
		_, err := leaderboardRPC(t, c[0], bad)
		if nakamaRPCErrorCode(err) != 3 {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	t.Logf("leaderboard: %v", all["records"])
}

// The leaderboard follows the exact ratings, which the levels come from,
// also when two ratings round to the same whole number: after 1 beats 2
// and 2 then beats 3 twice, 1 (1016) and 2 (1015.93, with more wins) both
// have about 1016, and 1 is listed first.
func TestNakamaLive_LeaderboardOrderExact(t *testing.T) {
	fixedEnabled(t)
	lad := newLiveLadder(t, 3)
	lad.play(t, 0, 1)
	lad.play(t, 1, 2)
	lad.play(t, 1, 2)
	elo := lad.elo
	if math.Floor(elo[0]+0.5) != math.Floor(elo[1]+0.5) || elo[0] <= elo[1] || lad.wins[1] <= lad.wins[0] {
		t.Fatalf("ratings %v, wins %v: not the case this test is for", elo, lad.wins)
	}
	board := leaderboardOf(t, lad.c[2], map[string]any{"game": lad.game})
	records, _ := board["records"].([]any)
	if len(records) != 3 {
		t.Fatalf("leaderboard %v", board)
	}
	for rank, i := range []int{0, 1, 2} {
		checkRecord(t, fmt.Sprintf("rank %d", rank+1), records[rank], lad.c[i].Session().UserID, onlineNameOf(t, lad.c[i]),
			lad.game, elo[i], lad.wins[i], lad.losses[i], rank+1)
	}
	t.Logf("ratings %v: leaderboard %v", elo, records)
}

// Players cannot write their own scores to the leaderboards, nor delete
// them.
func TestNakamaLive_LeaderboardRefusesPlayers(t *testing.T) {
	fixedEnabled(t)
	game := "lv-" + randID()
	p := settledSet(t, game)
	id := "ikemen_rating." + game
	code, out := playerStorage(t, p.b, http.MethodPost, "/v2/leaderboard/"+id, map[string]any{"score": "999999", "subscore": "99"})
	if code == http.StatusOK || !strings.Contains(fmt.Sprint(out["message"]), "authoritative") {
		t.Fatalf("a player's write: %d %v", code, out)
	}
	t.Logf("a player's write is refused: %d %v", code, out["message"])
	code, out = playerStorage(t, p.a, http.MethodDelete, "/v2/leaderboard/"+id, nil)
	t.Logf("a player's delete: %d %v", code, out)
	wElo, lElo := eloAfter(1000, 1000)
	board := leaderboardOf(t, p.b, map[string]any{"game": game})
	records, _ := board["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("leaderboard %v", board)
	}
	checkRecord(t, "first", records[0], p.a.Session().UserID, onlineNameOf(t, p.a), game, wElo, 1, 0, 1)
	checkRecord(t, "second", records[1], p.b.Session().UserID, onlineNameOf(t, p.b), game, lElo, 0, 1, 2)
}

func consoleRequest(t *testing.T, method, path, token string, payload any) (int, []byte) {
	t.Helper()
	port := 7351
	if p, err := strconv.Atoi(os.Getenv("NAKAMA_CONSOLE_PORT")); err == nil && p > 0 {
		port = p
	}
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// A player whose rating has no leaderboard entry (a rating from before the
// leaderboards) gets one from ikemen_get_rating.
func TestNakamaLive_LeaderboardEntryRestored(t *testing.T) {
	fixedEnabled(t)
	code, body := consoleRequest(t, http.MethodPost, "/v2/console/authenticate", "", map[string]string{
		"username": envOr("NAKAMA_CONSOLE_USER", "admin"), "password": envOr("NAKAMA_CONSOLE_PASSWORD", "password")})
	var auth struct {
		Token string `json:"token"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &auth) != nil || auth.Token == "" {
		t.Skipf("cannot sign in to the console: %d %s", code, body)
	}
	game := "lv-" + randID()
	p := settledSet(t, game)
	ida := p.a.Session().UserID
	id := "ikemen_rating." + game
	if code, body := consoleRequest(t, http.MethodDelete, "/v2/console/leaderboard/"+id+"/owner/"+ida, auth.Token, nil); code != http.StatusOK {
		t.Fatalf("console delete: %d %s", code, body)
	}
	if board := leaderboardOf(t, p.a, map[string]any{"game": game}); board["mine"] != nil || len(board["records"].([]any)) != 1 {
		t.Fatalf("after the delete: %v", board)
	}
	r := ratingOf(t, p.a, game)
	wElo, _ := eloAfter(1000, 1000)
	checkLevelFields(t, "ikemen_get_rating", r, game, wElo, "")
	board := leaderboardOf(t, p.a, map[string]any{"game": game})
	checkRecord(t, "restored", board["mine"], ida, onlineNameOf(t, p.a), game, wElo, 1, 0, 1)
	if len(board["records"].([]any)) != 2 {
		t.Fatalf("leaderboard %v", board)
	}
	// A player without sets gets no entry.
	c, _ := liveClient(t)
	ratingOf(t, c, game)
	if board := leaderboardOf(t, c, map[string]any{"game": game}); board["mine"] != nil || len(board["records"].([]any)) != 2 {
		t.Fatalf("a player without sets: %v", board)
	}
}

// The settings in each mode: the values the answers carry, and the line
// the server logs (with NAKAMA_LOG).
func TestNakamaLive_LevelsSettings(t *testing.T) {
	fixedEnabled(t)
	a, _ := liveClient(t)
	game := "lv-" + randID()
	r := ratingOf(t, a, game)
	var line string
	switch levelsMode() {
	case "":
		if r["level"] != float64(1000) || r["level_text"] != "1,000" || r["label"] != "Rating" {
			t.Fatalf("no settings: %v", r)
		}
		line = `"ikemen levels: levels are the plain Elo ratings, labelled \"Rating\" (IKEMEN_LEVELS is not set)"`
	case "example":
		if r["level"] != float64(9001) || r["level_text"] != "9,001" || r["label"] != "Power level" {
			t.Fatalf("example: %v", r)
		}
		if s := ratingOf(t, a, "lv-small"); s["level"] != float64(100) || s["label"] != "Rank" {
			t.Fatalf("lv-small: %v", s)
		}
		if s := ratingOf(t, a, "lv-label"); s["level"] != float64(9001) || s["label"] != "Score" {
			t.Fatalf("lv-label: %v", s)
		}
		line = `"ikemen levels: levels are start 9001, top 1000000000, labelled \"Power level\"; game \"lv-cap\": start 8900000000000000, top 9000000000000000, labelled \"Power level\"; game \"lv-label\": start 9001, top 1000000000, labelled \"Score\"; game \"lv-small\": start 100, top 10000, labelled \"Rank\""`
	case "bad":
		// Not valid: the plain Elo ratings.
		if r["level"] != float64(1000) || r["level_text"] != "1,000" || r["label"] != "Rating" {
			t.Fatalf("bad settings: %v", r)
		}
		line = `"ikemen levels: IKEMEN_LEVELS is not valid (default: top must be more than start): levels are the plain Elo ratings, labelled \"Rating\", for every game"`
	default:
		t.Fatalf("NAKAMA_LEVELS=%s: example, bad or nothing", levelsMode())
	}
	path := os.Getenv("NAKAMA_LOG")
	if path == "" {
		t.Logf("NAKAMA_LOG not set: the server's log line is not checked")
		return
	}
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), `"msg":`+line); n != 1 {
		t.Fatalf("the log has %d lines %s", n, line)
	}
	t.Logf("logged once: %s", line)
}
