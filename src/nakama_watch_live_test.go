package main

// Live tests of the lobby's watching and connection features: connection
// types, direct round trips, the replay stream relay and the catch-up for
// late watchers. Enable with NAKAMA_LIVE=1 (see nakama_live_test.go for the
// server flags). Online names: nakama_names_live_test.go.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// snapshotEvents returns the events recorded so far.
func (r *evRecorder) snapshotEvents() []NakamaEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]NakamaEvent(nil), r.evs...)
}

func TestNakamaLive_LobbyConnectionAndLinks(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, map[string]any{"name": "links", "interval": 3})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	ua, ub, uc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	_ = a.SendLobbyCommand("member", map[string]any{"connection": "wifi"})
	_ = b.SendLobbyCommand("member", map[string]any{"connection": "fiber"})
	waitLobby(t, c, "a on wifi", func(st map[string]any) bool { return memberField(st, ua, "connection") == "wifi" })
	st, _, _ := c.LobbyState()
	if got := memberField(st, ub, "connection"); got != "" {
		t.Fatalf("unknown connection type kept: %v", got)
	}
	// Reports outside a pairing are ignored: only the two players of the
	// current match can report their round trip.
	_ = c.SendLobbyCommand("link", map[string]any{"peer": ua, "rtt": 1})
	_ = a.SendLobbyCommand("link", map[string]any{"peer": ub, "rtt": 7})
	ready(t, a, true)
	ready(t, b, true)
	waitLobby(t, c, "a-b playing", lobbyPhase("playing"))
	// A direct round trip between a and b, while they play.
	_ = a.SendLobbyCommand("link", map[string]any{"peer": ub, "rtt": 42})
	_ = a.SendLobbyCommand("link", map[string]any{"peer": "not-a-member", "rtt": 5})
	_ = a.SendLobbyCommand("link", map[string]any{"peer": ua, "rtt": 5})
	_ = c.SendLobbyCommand("link", map[string]any{"peer": ub, "rtt": 1})
	st = waitLobby(t, c, "link a-b", func(st map[string]any) bool {
		links, _ := memberField(st, ub, "links").(map[string]any)
		return links != nil && links[ua] == float64(42)
	})
	linksA, _ := memberField(st, ua, "links").(map[string]any)
	if len(linksA) != 1 || linksA[ub] != float64(42) {
		t.Fatalf("a's links %v", linksA)
	}
	if links, _ := memberField(st, uc, "links").(map[string]any); len(links) != 0 {
		t.Fatalf("c's links %v", links)
	}
	// The current pairing carries the players' round trip.
	cur, _ := st["current"].(map[string]any)
	if cur["link"] != float64(42) {
		t.Fatalf("current link %v", cur["link"])
	}
	// Leaving forgets the member's links.
	b.Disconnect()
	waitLobby(t, c, "b gone", func(st map[string]any) bool {
		links, _ := memberField(st, ua, "links").(map[string]any)
		return memberField(st, ub, "user_id") == nil && len(links) == 0
	})
	_ = uc
}

// sendStream publishes a stream through the lobby: header, chunks of 30
// frames and the end, as RollbackReplayStream does.
func sendStream(t *testing.T, c *NakamaClient, stream string, seq int, frames int32, end bool) {
	t.Helper()
	sendSegment(t, c, stream, seq, 0, frames, end)
}

// sendSegment publishes one of a match's streams (a Turns match publishes
// one per character change).
func sendSegment(t *testing.T, c *NakamaClient, stream string, seq int, segment int32, frames int32, end bool) {
	t.Helper()
	sink := &nakamaReplaySink{client: c}
	info, _ := json.Marshal(map[string]any{"kind": "lobby", "seq": seq})
	sink.PublishReplayHeader(ReplayStreamHeader{Version: replayStreamVersion, FrameRate: 60, InputCount: REPLAY_NUM_INPUTS,
		InputBytes: REPLAY_INPUT_BYTES, DelayFrames: 60, Seed: 3, Stream: stream, MatchTime: 900, MatchInfo: info,
		Segment: segment})
	for f := int32(0); f < frames; f += 30 {
		sink.PublishReplayChunk(testChunk(stream, f, 30))
	}
	if end {
		sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: frames, Stream: stream})
	}
}

func waitBuffered(t *testing.T, c *NakamaClient, what, stream string, frames int32) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		b := c.ReplayBuffer()
		if b.Stream() == stream && b.BufferedThrough() >= frames {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b := c.ReplayBuffer()
	t.Fatalf("%s: stream %q buffered %d, want %q %d", what, b.Stream(), b.BufferedThrough(), stream, frames)
}

func TestNakamaLive_LobbyWatchStream(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, map[string]any{"name": "watch", "interval": 3, "watch": 1})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, c, "a-b playing", lobbyPhase("playing"))
	p1, _, seq := currentPair(st)
	host := a
	if p1 != a.Session().UserID {
		host = b
	}
	// A header from someone who is not playing is not relayed.
	sendStream(t, c, "spoof", seq, 30, false)
	time.Sleep(300 * time.Millisecond)
	if a.ReplayBuffer().Stream() == "spoof" || b.ReplayBuffer().Stream() == "spoof" {
		t.Fatal("a stream from a member who is not playing was relayed")
	}
	sendStream(t, host, "live1", seq, 120, false)
	waitBuffered(t, c, "watcher present from the start", "live1", 120)
	st = waitLobby(t, c, "watchable", func(st map[string]any) bool {
		cur, _ := st["current"].(map[string]any)
		return cur != nil && cur["watch"] == true
	})
	other := b
	if host == b {
		other = a
	}
	if other.ReplayBuffer().Stream() == "live1" {
		t.Fatal("the other player received the stream")
	}
	// A late member catches up from the stored stream.
	d, recD := liveClient(t)
	join(t, d, recD, id)
	if err := d.SendLobbyCommand("watch", map[string]any{"seq": seq}); err != nil {
		t.Fatal(err)
	}
	waitBuffered(t, d, "late watcher", "live1", 120)
	hd := d.ReplayBuffer().Header()
	if hd == nil || hd.MatchTime != 900 || !strings.Contains(string(hd.MatchInfo), fmt.Sprintf(`"seq":%d`, seq)) {
		t.Fatalf("late header %+v", hd)
	}
	// Watching state.
	_ = d.SendLobbyCommand("watching", map[string]any{"watching": true})
	waitLobby(t, c, "d watching", func(st map[string]any) bool {
		return memberField(st, d.Session().UserID, "state") == "watching"
	})
	// The end reaches everyone watching, and the players' report ends the match.
	sink := &nakamaReplaySink{client: host}
	for f := int32(120); f < 240; f += 30 {
		sink.PublishReplayChunk(testChunk("live1", f, 30))
	}
	sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: 240, Stream: "live1"})
	for _, w := range []*NakamaClient{c, d} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if final, ended := w.ReplayBuffer().FinalFrame(); ended && final == 240 && w.ReplayBuffer().BufferedThrough() == 240 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the end did not arrive")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// The next match clears watching flags.
	if err := a.ReportLobbyResultSeq(a.Session().UserID, b.Session().UserID, int64(seq)); err != nil {
		t.Fatal(err)
	}
	waitLobby(t, c, "next match", func(st map[string]any) bool {
		_, _, s := currentPair(st)
		return s > seq && memberField(st, d.Session().UserID, "watching") == false
	})
	t.Logf("stream relayed to watchers only; a late watcher got it from the start")
}

// A Turns match publishes one stream per character change. A member who
// starts watching during the second character gets the first stream too,
// and watching starts from it.
func TestNakamaLive_LobbyWatchTurnsLate(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, map[string]any{"name": "turns", "interval": 3, "watch": 1})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, c, "a-b playing", lobbyPhase("playing"))
	p1, _, seq := currentPair(st)
	host := a
	if p1 != a.Session().UserID {
		host = b
	}
	sendSegment(t, host, "turn1", seq, 0, 120, true)
	sendSegment(t, host, "turn2", seq, 1, 60, false)
	check := func(w *NakamaClient, who string) {
		deadline := time.Now().Add(8 * time.Second)
		for {
			first := w.WatchReplayBuffer()
			next := w.NextReplayBuffer(first)
			final, ended := first.FinalFrame()
			if first.Stream() == "turn1" && first.BufferedThrough() == 120 && ended && final == 120 &&
				next != nil && next.Stream() == "turn2" && next.BufferedThrough() == 60 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: watching starts from %q (%d frames), next %v", who, first.Stream(), first.BufferedThrough(), next != nil)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	check(c, "member present from the start")
	d, recD := liveClient(t)
	join(t, d, recD, id)
	if err := d.SendLobbyCommand("watch", map[string]any{"seq": seq}); err != nil {
		t.Fatal(err)
	}
	check(d, "late watcher")
	t.Logf("both streams of the Turns match reached a late watcher; watching starts from the first")
}

// The lobby's rules for the stream and for spectators: the spectator delay
// is at most 10 seconds; only the pairing's player 1 (the netplay host)
// publishes; oversized messages are dropped; and a pairing whose player is
// still watching waits for them.
func TestNakamaLive_LobbyWatchRules(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, map[string]any{"name": "rules", "interval": 3, "watch": 30})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	st := waitLobby(t, c, "settings", func(st map[string]any) bool { return st["settings"] != nil })
	if set, _ := st["settings"].(map[string]any); set["watch"] != float64(10) {
		t.Fatalf("watch 30 became %v, want 10", set["watch"])
	}
	ready(t, a, true)
	ready(t, b, true)
	st = waitLobby(t, c, "a-b playing", lobbyPhase("playing"))
	p1, _, seq := currentPair(st)
	host, guest := a, b
	if p1 != a.Session().UserID {
		host, guest = b, a
	}
	// The guest's stream is not relayed.
	sendSegment(t, guest, "guest", seq, 0, 60, false)
	time.Sleep(400 * time.Millisecond)
	if c.WatchReplayBuffer().Header() != nil {
		t.Fatal("a stream from the pairing's player 2 was relayed")
	}
	// An oversized chunk is dropped; the host's other messages go through.
	sink := &nakamaReplaySink{client: host}
	info, _ := json.Marshal(map[string]any{"kind": "lobby", "seq": seq})
	sink.PublishReplayHeader(ReplayStreamHeader{Version: replayStreamVersion, FrameRate: 60, InputCount: REPLAY_NUM_INPUTS,
		InputBytes: REPLAY_INPUT_BYTES, DelayFrames: 60, Stream: "host", MatchInfo: info})
	big := testChunk("host", 0, 30)
	big.Payload = make([]byte, 12000)
	sink.PublishReplayChunk(big)
	sink.PublishReplayChunk(testChunk("host", 0, 30))
	deadline := time.Now().Add(5 * time.Second)
	for c.WatchReplayBuffer().BufferedThrough() < 30 {
		if time.Now().After(deadline) {
			t.Fatal("the host's stream did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, ev := range recC.snapshotEvents() {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "replay chunk") {
			t.Fatalf("the oversized chunk was relayed: %v", ev.Error)
		}
	}
	// c readies and watches; after a-b, the line puts c first, and the match
	// waits until c stops watching.
	ready(t, c, true)
	_ = c.SendLobbyCommand("watching", map[string]any{"watching": true})
	if err := host.ReportLobbyResultSeq(host.Session().UserID, guest.Session().UserID, int64(seq)); err != nil {
		t.Fatal(err)
	}
	uc := c.Session().UserID
	st = waitLobby(t, a, "c next, waiting", func(st map[string]any) bool {
		next, _ := st["next"].(map[string]any)
		return st["phase"] == "waiting" && next != nil && (next["player1"] == uc || next["player2"] == uc)
	})
	time.Sleep(time.Second)
	if st, _, _ := a.LobbyState(); st["phase"] != "waiting" {
		t.Fatalf("the match started while c was watching: phase %v", st["phase"])
	}
	_ = c.SendLobbyCommand("watching", map[string]any{"watching": false})
	st = waitLobby(t, a, "c playing", func(st map[string]any) bool {
		q1, q2, s := currentPair(st)
		return st["phase"] == "playing" && s > seq && (q1 == uc || q2 == uc)
	})
	t.Logf("watch 30 clamped to 10; player 2's stream and an oversized chunk dropped; the next match waited for c to stop watching")
}

func TestNakamaLive_LobbyWatchOff(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ := createLobbyWith(t, a, map[string]any{"name": "nowatch", "interval": 3, "watch": 0})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, c, "playing", lobbyPhase("playing"))
	if set, _ := st["settings"].(map[string]any); set["watch"] != float64(0) {
		t.Fatalf("watch setting %v", set["watch"])
	}
	_, _, seq := currentPair(st)
	sendStream(t, a, "off1", seq, 60, true)
	sendStream(t, b, "off2", seq, 60, true)
	time.Sleep(500 * time.Millisecond)
	if s := c.ReplayBuffer().Stream(); s != "" {
		t.Fatalf("a lobby without watching relayed stream %q", s)
	}
	if err := c.SendLobbyCommand("watch", map[string]any{"seq": seq}); err != nil {
		t.Fatal(err)
	}
	waitLobbyEvent(t, c, "nothing to watch", func(ev map[string]any) bool { return ev["kind"] == "error" })
}

// Two members who play each other report their direct round trip on their
// own once the P2P path is up.
func TestNakamaLive_LobbyP2PLinkReport(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	join(t, b, recB, id)
	waitLobby(t, a, "state", func(st map[string]any) bool { return len(st["members"].([]any)) == 2 })
	waitLobby(t, b, "state", func(st map[string]any) bool { return len(st["members"].([]any)) == 2 })
	// The lobby takes round trips only from the players of its current match.
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, a, "a-b playing", lobbyPhase("playing"))
	p1, _, _ := currentPair(st)
	a.SetPairing(id, b.Session().UserID, p1 == a.Session().UserID)
	b.SetPairing(id, a.Session().UserID, p1 == b.Session().UserID)
	for _, cl := range []*NakamaClient{a, b} {
		if err := cl.StartP2P(0, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitP2PReady(t, a, b)
	ua, ub := a.Session().UserID, b.Session().UserID
	deadline := time.Now().Add(12 * time.Second)
	for {
		st, _, _ := a.LobbyState()
		links, _ := memberField(st, ua, "links").(map[string]any)
		if rtt, ok := links[ub].(float64); ok {
			t.Logf("direct round trip reported: %v ms; status p2p_rtt %v", rtt, a.Status().P2PRTT)
			if rtt < 0 || rtt > 200 {
				t.Fatalf("loopback round trip %v ms", rtt)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no link reported; a rtt %v", a.Status().P2PRTT)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
