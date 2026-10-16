package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRankedRetryDelay(t *testing.T) {
	want := []time.Duration{0, 5 * time.Second, 15 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}
	for sends, w := range want {
		if got := rankedRetryDelay(sends); got != w {
			t.Fatalf("after %d sends: %v, want %v", sends, got, w)
		}
	}
}

func testPendingClaim(id string, created int64) rankedPendingClaim {
	return rankedPendingClaim{
		SetID: id, MatchID: id, Set: 1, Winner: "w", UserID: "u",
		Server:  rankedPendingServer{Host: "127.0.0.1", Port: 7350, ServerKey: "defaultkey"},
		Created: created,
	}
}

// The file: written, read back, claims a day old left out, a broken file
// ignored, no claims no file.
func TestRankedPendingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "save", "ranked_pending.json")
	now := time.Unix(1_800_000_000, 0)
	fresh := testPendingClaim("a.lv", now.Add(-time.Hour).Unix())
	fresh.Sends = 3
	fresh.Server.DeviceID = "profile-device"
	old := testPendingClaim("b.lv#2", now.Add(-25*time.Hour).Unix())
	old.Set = 2
	if err := writeRankedPending(path, []rankedPendingClaim{fresh, old}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	claims, expired, err := readRankedPending(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0] != fresh {
		t.Fatalf("claims %+v", claims)
	}
	if len(expired) != 1 || expired[0] != old {
		t.Fatalf("expired %+v", expired)
	}
	// No file: no claims, no error.
	if err := writeRankedPending(path, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
	if c, e, err := readRankedPending(path, now); err != nil || c != nil || e != nil {
		t.Fatalf("missing file: %v %v %v", c, e, err)
	}
	// Broken files: an error and no claims.
	for _, text := range []string{"{", `{"version": 2, "claims": []}`, `[1, 2]`} {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		if c, _, err := readRankedPending(path, now); err == nil || c != nil {
			t.Fatalf("%q: %v %v", text, c, err)
		}
	}
	// A claim without what sending it needs is left out.
	if err := os.WriteFile(path, []byte(`{"version":1,"claims":[{"set_id":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _, err := readRankedPending(path, now); err != nil || len(c) != 0 {
		t.Fatalf("incomplete claim: %v %v", c, err)
	}
}

func TestClassifyRankedAnswer(t *testing.T) {
	cases := []struct {
		body   string
		err    error
		status string
		final  bool
	}{
		{`{"accepted":true,"status":"open"}`, nil, "open", false},
		{`{"accepted":true,"status":"settled"}`, nil, "settled", true},
		{`{"accepted":true,"status":"disputed","reason":"claims_disagree"}`, nil, "disputed", true},
		{`not json`, nil, "error", false},
		{``, errors.New("dial tcp: connection refused"), "error", false},
		{``, &NakamaRPCError{Code: 14, Message: "unavailable"}, "error", false},
		{``, &NakamaRPCError{Code: 10, Message: "retry"}, "error", false},
		{``, &NakamaRPCError{Code: 16, Message: "no session"}, "error", false},
		{``, &NakamaRPCError{Code: 9, Message: "ranked set x is void: it takes no more claims"}, "void", true},
		{``, &NakamaRPCError{Code: 9, Message: "ranked set x is settled: it takes no more claims"}, "settled", true},
		{``, &NakamaRPCError{Code: 9, Message: "ranked set x#2 is disputed: it takes no more claims"}, "disputed", true},
		{``, &NakamaRPCError{Code: 9, Message: "your claim for this set is already recorded, with the other winner"}, "refused", true},
		{``, &NakamaRPCError{Code: 9, Message: "match m is not a ranked session"}, "refused", true},
		{``, &NakamaRPCError{Code: 7, Message: "you were not a player"}, "refused", true},
		{``, &NakamaRPCError{Code: 5, Message: "no set"}, "refused", true},
		{``, &NakamaRPCError{Code: 3, Message: "bad"}, "refused", true},
	}
	for _, c := range cases {
		status, final := classifyRankedAnswer([]byte(c.body), c.err)
		if status != c.status || final != c.final {
			t.Fatalf("%q %v: %s %v, want %s %v", c.body, c.err, status, final, c.status, c.final)
		}
	}
}

// testQueue is a queue with a clock the test moves and a send that answers
// from a script.
type testQueue struct {
	q       *rankedClaimQueue
	mu      sync.Mutex
	now     time.Time
	sent    []string
	answers []string // "" for no answer (an error)
	events  chan rankedAnswer
}

func newTestQueue(t *testing.T, path string, answers ...string) *testQueue {
	tq := &testQueue{now: time.Unix(1_800_000_000, 0), answers: answers, events: make(chan rankedAnswer, 32)}
	tq.q = &rankedClaimQueue{path: path}
	tq.q.now = func() time.Time {
		tq.mu.Lock()
		defer tq.mu.Unlock()
		return tq.now
	}
	tq.q.send = func(c *rankedQueued) ([]byte, error) {
		tq.mu.Lock()
		defer tq.mu.Unlock()
		tq.sent = append(tq.sent, c.SetID)
		if len(tq.answers) == 0 {
			return nil, errors.New("no answer")
		}
		a := tq.answers[0]
		tq.answers = tq.answers[1:]
		if a == "" {
			return nil, errors.New("connection refused")
		}
		return []byte(a), nil
	}
	tq.q.setListener(func(a rankedAnswer) { tq.events <- a })
	return tq
}

func (tq *testQueue) advance(d time.Duration) {
	tq.mu.Lock()
	tq.now = tq.now.Add(d)
	tq.mu.Unlock()
	tq.q.kickWorker()
}

func (tq *testQueue) event(t *testing.T) rankedAnswer {
	t.Helper()
	select {
	case a := <-tq.events:
		return a
	case <-time.After(5 * time.Second):
		t.Fatalf("no answer")
	}
	return rankedAnswer{}
}

func (tq *testQueue) noEvent(t *testing.T) {
	t.Helper()
	select {
	case a := <-tq.events:
		t.Fatalf("unexpected send: %+v", a)
	case <-time.After(150 * time.Millisecond):
	}
}

// A claim goes at once, then 5, 15 and 60 seconds later, kept in the file
// meanwhile, until its answer is final.
func TestRankedClaimQueueSchedule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranked_pending.json")
	tq := newTestQueue(t, path, `{"status":"open"}`, "", "", `{"status":"open"}`, `{"status":"settled","result":{"won":true}}`)
	tq.q.add(testPendingClaim("s.lv", 0), nil)
	if a := tq.event(t); a.Status != "open" || a.Final || a.Sends != 1 {
		t.Fatalf("first send: %+v", a)
	}
	if c, _, _ := readRankedPending(path, tq.now); len(c) != 1 || c[0].Sends != 1 {
		t.Fatalf("file after the first send: %+v", c)
	}
	tq.advance(4 * time.Second)
	tq.noEvent(t)
	tq.advance(time.Second)
	if a := tq.event(t); a.Status != "error" || a.Final || a.Sends != 2 {
		t.Fatalf("after 5 s: %+v", a)
	}
	tq.advance(14 * time.Second)
	tq.noEvent(t)
	tq.advance(time.Second)
	if a := tq.event(t); a.Status != "error" || a.Sends != 3 {
		t.Fatalf("after 15 s more: %+v", a)
	}
	tq.advance(59 * time.Second)
	tq.noEvent(t)
	tq.advance(time.Second)
	if a := tq.event(t); a.Status != "open" || a.Sends != 4 {
		t.Fatalf("after 60 s more: %+v", a)
	}
	// Connecting again sends it at once.
	tq.q.reconnected()
	a := tq.event(t)
	if a.Status != "settled" || !a.Final || a.Sends != 5 {
		t.Fatalf("after reconnecting: %+v", a)
	}
	if p := a.luaPayload(); p["set_id"] != "s.lv" || p["final"] != true || p["status"] != "settled" {
		t.Fatalf("payload %v", p)
	}
	if len(tq.q.pending()) != 0 {
		t.Fatalf("still queued: %+v", tq.q.pending())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
}

// Claims left by an earlier run go when the game starts; those a day old
// are dropped, and so is a claim that becomes a day old.
func TestRankedClaimQueueResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranked_pending.json")
	now := time.Unix(1_800_000_000, 0)
	keep := testPendingClaim("keep.lv", now.Add(-23*time.Hour-59*time.Minute).Unix())
	keep.Sends = 7
	old := testPendingClaim("old.lv", now.Add(-48*time.Hour).Unix())
	if err := writeRankedPending(path, []rankedPendingClaim{keep, old}); err != nil {
		t.Fatal(err)
	}
	tq := newTestQueue(t, path, `{"status":"open"}`)
	tq.q.resume()
	a := tq.event(t)
	if a.SetID != "keep.lv" || a.Status != "open" || a.Sends != 8 {
		t.Fatalf("resumed: %+v", a)
	}
	if c, _, _ := readRankedPending(path, now); len(c) != 1 || c[0].SetID != "keep.lv" {
		t.Fatalf("file: %+v", c)
	}
	// Its next send would come after its day: it is dropped instead.
	tq.advance(60 * time.Second)
	if a := tq.event(t); a.Status != "expired" || !a.Final {
		t.Fatalf("a day old: %+v", a)
	}
	if len(tq.q.pending()) != 0 {
		t.Fatalf("still queued")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
	// A broken file is ignored.
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	tq2 := newTestQueue(t, path)
	tq2.q.resume()
	tq2.noEvent(t)
	if len(tq2.q.pending()) != 0 {
		t.Fatalf("claims from a broken file")
	}
	// ... and replaced at the next change.
	tq2.answers = []string{`{"status":"disputed"}`}
	tq2.q.add(testPendingClaim("new.lv", 0), nil)
	if a := tq2.event(t); a.Status != "disputed" || !a.Final {
		t.Fatalf("new claim: %+v", a)
	}
}

// A claim of the same set (from the same account) replaces the queued one.
func TestRankedClaimQueueReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ranked_pending.json")
	tq := newTestQueue(t, path, `{"status":"open"}`, `{"status":"open"}`)
	tq.q.add(testPendingClaim("s.lv", 0), nil)
	tq.event(t)
	tq.q.add(testPendingClaim("s.lv", 0), nil)
	tq.event(t)
	if p := tq.q.pending(); len(p) != 1 || p[0].Sends != 1 {
		t.Fatalf("pending %+v", p)
	}
}
