package main

// Unit tests of watching lobby matches (replay stream ids, match time and
// info, per-stream buffers and segments, live replay readiness, the content
// fingerprint), the direct round trip over the P2P path, connection types and
// RPC errors.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type recordingSink struct {
	headers []ReplayStreamHeader
	chunks  []ReplayStreamChunk
	ends    []ReplayStreamEnd
	resets  []ReplayStreamReset
}

func (s *recordingSink) PublishReplayHeader(h ReplayStreamHeader) { s.headers = append(s.headers, h) }
func (s *recordingSink) PublishReplayChunk(c ReplayStreamChunk)   { s.chunks = append(s.chunks, c) }
func (s *recordingSink) PublishReplayEnd(e ReplayStreamEnd)       { s.ends = append(s.ends, e) }
func (s *recordingSink) ReplayStreamReset(r ReplayStreamReset)    { s.resets = append(s.resets, r) }

func testChunk(stream string, start, count int32) ReplayStreamChunk {
	payload := make([]byte, int(count)*REPLAY_NUM_INPUTS*REPLAY_INPUT_BYTES)
	for i := range payload {
		payload[i] = byte(int(start) + i)
	}
	return ReplayStreamChunk{StartFrame: start, FrameCount: count, Payload: payload, Stream: stream}
}

func TestReplayBufferKeepsOneStream(t *testing.T) {
	b := NewNakamaReplayBuffer()
	b.ApplyHeader(ReplayStreamHeader{Stream: "s1", DelayFrames: 60, MatchInfo: json.RawMessage(`{"seq":1}`)})
	if err := b.ApplyChunk(testChunk("s1", 0, 30)); err != nil {
		t.Fatal(err)
	}
	if err := b.ApplyChunk(testChunk("s2", 30, 30)); err != nil {
		t.Fatal(err)
	}
	if got := b.BufferedThrough(); got != 30 {
		t.Fatalf("another stream's chunk was stored: buffered %d", got)
	}
	// Chunks without a stream id (older publishers) are accepted.
	_ = b.ApplyChunk(testChunk("", 30, 30))
	if got := b.BufferedThrough(); got != 60 {
		t.Fatalf("buffered %d, want 60", got)
	}
	b.ApplyEnd(ReplayStreamEnd{FinalFrame: 60, Stream: "s2"})
	if _, ended := b.FinalFrame(); ended {
		t.Fatal("another stream's end ended this one")
	}
	b.ApplyEnd(ReplayStreamEnd{FinalFrame: 60, Stream: "s1"})
	if final, ended := b.FinalFrame(); !ended || final != 60 {
		t.Fatalf("end not applied: %d %v", final, ended)
	}
	if h := b.Header(); h == nil || string(h.MatchInfo) != `{"seq":1}` {
		t.Fatalf("header match info %v", h)
	}
	b.ApplyReset(ReplayStreamReset{Reason: "other", Stream: "s2"})
	if b.ResetReason() != "" {
		t.Fatal("another stream's reset reset this one")
	}
	b.ApplyReset(ReplayStreamReset{Reason: "rollback", Stream: "s1"})
	if b.ResetReason() != "rollback" {
		t.Fatalf("reset reason %q", b.ResetReason())
	}
	// A withdrawn stream stays known, and its data is refused.
	if b.Stream() != "s1" || b.BufferedThrough() != 0 {
		t.Fatalf("after the reset: stream %q, buffered %d", b.Stream(), b.BufferedThrough())
	}
	_ = b.ApplyChunk(testChunk("s1", 0, 30))
	if b.BufferedThrough() != 0 {
		t.Fatal("a withdrawn stream took a chunk")
	}
}

func TestRollbackReplayStreamCarriesMatch(t *testing.T) {
	sink := &recordingSink{}
	r := NewRollbackReplayStream(10, sink)
	r.SetDelay(2)
	info := json.RawMessage(`{"kind":"lobby","seq":4}`)
	r.Begin(ReplayStreamStart{Seed: 7, PreMatchTime: 11, MatchTime: 1500, Stage: "stages/kfm.def", Info: info, Segment: 2})
	if len(sink.headers) != 1 {
		t.Fatalf("headers %d", len(sink.headers))
	}
	h := sink.headers[0]
	if h.Stream == "" || h.MatchTime != 1500 || h.DelayFrames != 120 || h.Seed != 7 || h.PreMatchTime != 11 ||
		h.Stage != "stages/kfm.def" || string(h.MatchInfo) != string(info) || h.Segment != 2 {
		t.Fatalf("header %+v", h)
	}
	rs := &RollbackSession{netTime: 200,
		replayInputs:       make([][REPLAY_NUM_INPUTS]InputBits, 200),
		replayAnalogInputs: make([][REPLAY_NUM_INPUTS][6]int8, 200)}
	// Frames become eligible one at a time, as the match records them; only
	// whole 30-frame chunks go out.
	for netTime := int32(121); netTime <= 200; netTime++ {
		rs.netTime = netTime
		r.PublishReady(rs)
	}
	published := int32(0)
	for _, c := range sink.chunks {
		if c.Stream != h.Stream {
			t.Fatalf("chunk without the stream id: %q", c.Stream)
		}
		if c.FrameCount != 30 || c.StartFrame != published {
			t.Fatalf("chunk %d+%d, want whole chunks in order", c.StartFrame, c.FrameCount)
		}
		published = c.StartFrame + c.FrameCount
	}
	if published != 60 {
		t.Fatalf("published through %d, want 60 (80 frames eligible: 200 frames, 2 s delay)", published)
	}
	r.End(200, rs)
	if len(sink.ends) != 1 || sink.ends[0].Stream != h.Stream || sink.ends[0].FinalFrame != 200 {
		t.Fatalf("end %+v", sink.ends)
	}
	last := sink.chunks[len(sink.chunks)-1]
	if last.StartFrame+last.FrameCount != 200 {
		t.Fatalf("remaining frames not published: last chunk ends at %d", last.StartFrame+last.FrameCount)
	}
	// A later stream gets a new id.
	r.Begin(ReplayStreamStart{Seed: 8, Info: info})
	if sink.headers[1].Stream == h.Stream {
		t.Fatal("two streams share an id")
	}
	r.NoteTruncate(0)
	r.PublishReady(&RollbackSession{netTime: 300,
		replayInputs:       make([][REPLAY_NUM_INPUTS]InputBits, 300),
		replayAnalogInputs: make([][REPLAY_NUM_INPUTS][6]int8, 300)})
	r.NoteTruncate(10)
	if len(sink.resets) != 1 || sink.resets[0].Stream != sink.headers[1].Stream {
		t.Fatalf("reset %+v", sink.resets)
	}
}

func TestNakamaClientKeepsRecentStreams(t *testing.T) {
	n := NewNakamaClient(NakamaConfig{})
	match1 := json.RawMessage(`{"kind":"lobby","seq":1}`)
	n.applyReplayHeader(ReplayStreamHeader{Stream: "a", MatchInfo: match1})
	first := n.ReplayBuffer()
	n.applyReplayHeader(ReplayStreamHeader{Stream: "a", MatchInfo: match1})
	if n.ReplayBuffer() != first {
		t.Fatal("the same stream's header replaced its buffer")
	}
	if n.NextReplayBuffer(first) != nil {
		t.Fatal("a next stream before one arrived")
	}
	// A Turns match's next character: the same match, the next segment.
	n.applyReplayHeader(ReplayStreamHeader{Stream: "b", MatchInfo: match1, Segment: 1})
	second := n.ReplayBuffer()
	if second == first || n.NextReplayBuffer(first) != second {
		t.Fatal("the next stream is not found")
	}
	if first.Stream() != "a" {
		t.Fatal("a new stream changed the previous buffer")
	}
	if n.WatchReplayBuffer() != first {
		t.Fatal("watching does not start from the match's first stream")
	}
	// Data goes to the buffer of its stream, not to the newest one.
	if err := n.replayBufferFor("a").ApplyChunk(testChunk("a", 0, 30)); err != nil {
		t.Fatal(err)
	}
	if first.BufferedThrough() != 30 || second.BufferedThrough() != 0 {
		t.Fatalf("buffered %d and %d", first.BufferedThrough(), second.BufferedThrough())
	}
	if n.replayBufferFor("unknown") != nil {
		t.Fatal("a buffer for a stream whose header never arrived")
	}
	// Another match starts: its stream does not follow this match's streams.
	n.applyReplayHeader(ReplayStreamHeader{Stream: "x", MatchInfo: json.RawMessage(`{"kind":"lobby","seq":2}`)})
	third := n.ReplayBuffer()
	if n.NextReplayBuffer(second) != nil || n.WatchReplayBuffer() != third {
		t.Fatal("streams of two matches were joined")
	}
	for i := 0; i < nakamaReplayHistory; i++ {
		n.applyReplayHeader(ReplayStreamHeader{Stream: fmt.Sprintf("s%d", i)})
	}
	if len(n.replayHistory) != nakamaReplayHistory || n.replayBufferFor("a") != nil {
		t.Fatalf("history keeps %d streams", len(n.replayHistory))
	}
}

// A member who starts watching during a Turns match's second character has
// the live second stream first; the lobby then sends the whole match again.
func TestWatchingLateStartsFromTheFirstStream(t *testing.T) {
	n := NewNakamaClient(NakamaConfig{})
	match := json.RawMessage(`{"kind":"lobby","seq":7}`)
	n.applyReplayHeader(ReplayStreamHeader{Stream: "t2", MatchInfo: match, Segment: 1})
	if n.WatchReplayBuffer().Header() != nil {
		t.Fatal("watching would start from the second character's stream")
	}
	// The lobby's copy: the first stream, then the second again.
	n.applyReplayHeader(ReplayStreamHeader{Stream: "t1", MatchInfo: match})
	_ = n.replayBufferFor("t1").ApplyChunk(testChunk("t1", 0, 30))
	n.applyReplayHeader(ReplayStreamHeader{Stream: "t2", MatchInfo: match, Segment: 1})
	_ = n.replayBufferFor("t2").ApplyChunk(testChunk("t2", 0, 30))
	target := n.WatchReplayBuffer()
	if target.Stream() != "t1" || target.BufferedThrough() != 30 {
		t.Fatalf("watching starts from %q with %d frames", target.Stream(), target.BufferedThrough())
	}
	next := n.NextReplayBuffer(target)
	if next == nil || next.Stream() != "t2" || next.BufferedThrough() != 30 {
		t.Fatal("the second stream does not follow the first")
	}
	if len(n.replayHistory) != 2 {
		t.Fatalf("%d buffers for two streams", len(n.replayHistory))
	}
}

// The content fingerprint must not change with a client's first match: a
// member who has played and one who has not play each other, and watch each
// other's matches, in the same lobby.
func TestContentFingerprintIgnoresPlayHistory(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	motifDef := write("system.def", "[Info]\n")
	fightDef := write("fight.def", "[Info]\n")
	selectDef := write("select.def", "[Characters]\nkfm\n")
	commonFx := write("gofx.def", "[Info]\nprefix = go\n")
	charFx := write("kfmfx.def", "[Info]\nprefix = kfmfx\n")

	savedMotifDef, savedSelect, savedFight := sys.motif.Def, sys.motif.Files.Select, sys.fightScreen.def
	savedFx, savedFfx := sys.cfg.Common.Fx, sys.ffx
	defer func() {
		sys.motif.Def, sys.motif.Files.Select, sys.fightScreen.def = savedMotifDef, savedSelect, savedFight
		sys.cfg.Common.Fx, sys.ffx = savedFx, savedFfx
	}()
	sys.motif.Def, sys.motif.Files.Select, sys.fightScreen.def = motifDef, selectDef, fightDef
	sys.cfg.Common.Fx = map[string][]string{"Fx": {commonFx}}
	sys.ffx = map[string]*FightFx{"f": {}}

	fresh := sys.currentContentFingerprint()
	// What a match leaves loaded: the configured Common FX and a character's FX.
	sys.ffx["go"] = &FightFx{fileName: commonFx}
	sys.ffx["kfmfx"] = &FightFx{fileName: charFx, isCharFX: true}
	if played := sys.currentContentFingerprint(); played != fresh {
		t.Fatalf("the fingerprint changed after a match: %s, then %s", fresh, played)
	}
	// Different Common FX content is a content difference.
	write("gofx.def", "[Info]\nprefix = go2\n")
	if changed := sys.currentContentFingerprint(); changed == fresh {
		t.Fatal("changed Common FX content kept the fingerprint")
	}
}

// Stream data belongs to the Nakama match it came through: joining another
// match (every lobby numbers its matches from 1) forgets it, so a member
// never watches another lobby's match, and one who rejoins asks the lobby
// for the whole stream again.
func TestJoiningAnotherMatchForgetsStreams(t *testing.T) {
	n := NewNakamaClient(NakamaConfig{})
	n.handleJSON([]byte(`{"match":{"match_id":"lobby-a"}}`))
	n.applyReplayHeader(ReplayStreamHeader{Stream: "a2", MatchInfo: json.RawMessage(`{"kind":"lobby","seq":2}`)})
	old := n.WatchReplayBuffer()
	if old.Stream() != "a2" {
		t.Fatal("no stream to watch")
	}
	n.handleJSON([]byte(`{"match":{"match_id":"lobby-a"}}`))
	if n.WatchReplayBuffer() != old {
		t.Fatal("joining the same match again forgot its stream")
	}
	n.handleJSON([]byte(`{"match":{"match_id":"lobby-b"}}`))
	if n.WatchReplayBuffer().Header() != nil || n.replayBufferFor("a2") != nil {
		t.Fatal("another lobby's stream is still there")
	}
	if old.ResetReason() == "" {
		t.Fatal("the old stream was not withdrawn")
	}
}

func TestReplaySegmentNumbers(t *testing.T) {
	n := NewNakamaClient(NakamaConfig{})
	var got []int32
	for _, roundNo := range []int32{1, 2, 3, 1, 1, 2} {
		got = append(got, n.nextReplaySegment(roundNo))
	}
	if fmt.Sprint(got) != "[0 1 2 0 0 1]" {
		t.Fatalf("segments %v", got)
	}
}

func TestLiveReplayReady(t *testing.T) {
	b := NewNakamaReplayBuffer()
	if liveReplayReady(b) {
		t.Fatal("ready without a header")
	}
	b.ApplyHeader(ReplayStreamHeader{Stream: "s", DelayFrames: 180})
	_ = b.ApplyChunk(testChunk("s", 0, 30))
	if liveReplayReady(b) {
		t.Fatal("ready with half a second buffered")
	}
	_ = b.ApplyChunk(testChunk("s", 30, 30))
	if !liveReplayReady(b) {
		t.Fatal("not ready with a second buffered")
	}
	short := NewNakamaReplayBuffer()
	short.ApplyHeader(ReplayStreamHeader{Stream: "s"})
	_ = short.ApplyChunk(testChunk("s", 0, 20))
	short.ApplyEnd(ReplayStreamEnd{FinalFrame: 20, Stream: "s"})
	if !liveReplayReady(short) {
		t.Fatal("an ended stream is not ready")
	}
	short.Reset("x")
	if liveReplayReady(short) {
		t.Fatal("a withdrawn stream is ready")
	}
}

func TestLiveReplayCatchUp(t *testing.T) {
	b := NewNakamaReplayBuffer()
	b.ApplyHeader(ReplayStreamHeader{Stream: "s", DelayFrames: 60, InputCount: REPLAY_NUM_INPUTS, InputBytes: REPLAY_INPUT_BYTES})
	for f := int32(0); f < 600; f += 30 {
		_ = b.ApplyChunk(testChunk("s", f, 30))
	}
	rf, err := NewLiveReplayFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if rf.liveCatchingUp() {
		t.Fatal("catching up before the match started")
	}
	rf.liveStarted = true
	if !rf.liveCatchingUp() {
		t.Fatal("600 frames behind and not catching up")
	}
	rf.liveFrame = 450 // 150 behind: still catching up (stops at 90)
	if !rf.liveCatchingUp() {
		t.Fatal("stopped catching up too early")
	}
	rf.liveFrame = 520
	if rf.liveCatchingUp() {
		t.Fatal("still catching up 80 frames behind")
	}
	rf.liveFrame = 450
	if rf.liveCatchingUp() {
		t.Fatal("started again below the start threshold")
	}

	// The end sends the players' last seconds (the delay) at once: a
	// spectator on time does not fast-forward through them.
	e := NewNakamaReplayBuffer()
	e.ApplyHeader(ReplayStreamHeader{Stream: "e", DelayFrames: 180, InputCount: REPLAY_NUM_INPUTS, InputBytes: REPLAY_INPUT_BYTES})
	for f := int32(0); f < 1200; f += 30 {
		_ = e.ApplyChunk(testChunk("e", f, 30))
	}
	e.ApplyEnd(ReplayStreamEnd{Stream: "e", FinalFrame: 1200})
	live, err := NewLiveReplayFile(e)
	if err != nil {
		t.Fatal(err)
	}
	live.liveStarted, live.liveDelay = true, 180
	live.liveFrame = 960 // 240 frames before the end, 60 behind the live edge
	if live.liveCatchingUp() {
		t.Fatal("fast-forwarding through the players' last seconds")
	}
	live.liveFrame = 600 // 420 behind the live edge (1200 - 180)
	if !live.liveCatchingUp() {
		t.Fatal("a late spectator does not catch up after the end")
	}
}

func TestP2PMuxMeasuresRoundTrip(t *testing.T) {
	a, b := p2pPair(t, nil)
	a.mu.Lock()
	m := a.mux
	a.mu.Unlock()
	if m == nil {
		t.Fatal("no mux after the handshake")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = m.sendPing()
		time.Sleep(20 * time.Millisecond)
		if _, samples := a.RTT(); samples >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pings were not answered")
		}
	}
	rtt, _ := a.RTT()
	if rtt <= 0 || rtt > 200*time.Millisecond {
		t.Fatalf("loopback round trip %v", rtt)
	}
	_ = b
}

func TestConnectionTypeNames(t *testing.T) {
	for in, want := range map[string]string{"wired": "wired", "Ethernet": "wired", "WI-FI": "wifi", "wlan": "wifi",
		"cellular": "mobile", "mobile": "mobile", "fiber": "", "": ""} {
		if got := normalizeNetKind(in); got != want {
			t.Errorf("normalizeNetKind(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"wlan0": "wifi", "wlp3s0": "wifi", "wlx00c0ca": "wifi", "eth0": "wired",
		"enp5s0": "wired", "eno1": "wired", "rmnet_data0": "mobile", "ccmni0": "mobile", "pdp_ip0": "mobile",
		"wwan0": "mobile", "en0": "", "tun0": "", "docker0": ""} {
		if got := netKindFromName(in); got != want {
			t.Errorf("netKindFromName(%q) = %q, want %q", in, got, want)
		}
	}
	listing := "\nHardware Port: Ethernet\nDevice: en0\nEthernet Address: 00:00\n\n" +
		"Hardware Port: Wi-Fi\nDevice: en1\nEthernet Address: 00:01\n\n" +
		"Hardware Port: iPhone USB\nDevice: en5\n\nHardware Port: Thunderbolt Bridge\nDevice: bridge0\n"
	for dev, want := range map[string]string{"en0": "wired", "en1": "wifi", "en5": "mobile", "bridge0": "wired", "en9": ""} {
		if got := darwinPortKind(listing, dev); got != want {
			t.Errorf("darwinPortKind(%q) = %q, want %q", dev, got, want)
		}
	}
	for desc, want := range map[string]bool{"Intel(R) Ethernet Connection (7) I219-V": false,
		"Realtek PCIe GbE Family Controller": false, "Hyper-V Virtual Ethernet Adapter": true,
		"TAP-Windows Adapter V9": true, "Fortinet SSL VPN Virtual Ethernet Adapter": true,
		"VirtualBox Host-Only Ethernet Adapter": true, "Npcap Loopback Adapter": true} {
		if got := virtualAdapterDescription(desc); got != want {
			t.Errorf("virtualAdapterDescription(%q) = %v, want %v", desc, got, want)
		}
	}
}

func TestNakamaRPCError(t *testing.T) {
	err := newNakamaRPCError("ikemen_account_name", 409, []byte(`{"error":"That name is taken","message":"That name is taken","code":6}`))
	if err.Error() != "That name is taken" || err.Code != 6 {
		t.Fatalf("%q code %d", err.Error(), err.Code)
	}
	if nakamaRPCErrorCode(fmt.Errorf("rename: %w", err)) != 6 {
		t.Fatal("code lost through wrapping")
	}
	// The body of an RPC called with ?unwrap: "error" is an object.
	unwrapped := newNakamaRPCError("x", 400, []byte(`{"code":3,"error":{},"message":"Names have 2 to 16"}`))
	if unwrapped.Code != 3 || unwrapped.Error() != "Names have 2 to 16" {
		t.Fatalf("%q code %d", unwrapped.Error(), unwrapped.Code)
	}
	if nakamaRPCErrorCode(errors.New("plain")) != 0 {
		t.Fatal("code of a plain error")
	}
	raw := newNakamaRPCError("x", 502, []byte("bad gateway"))
	if raw.Code != 0 || raw.Error() != `nakama rpc "x" failed: HTTP 502: bad gateway` {
		t.Fatalf("%q", raw.Error())
	}
}
