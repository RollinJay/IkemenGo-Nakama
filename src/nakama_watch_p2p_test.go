package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuin/gopher-lua/parse"
)

func TestWatchFramesRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	msgs := []struct {
		op   uint32
		body string
	}{{nakamaReplayHeaderOp, `{"stream":"s"}`}, {nakamaReplayChunkOp, `{"stream":"s","start":0}`}, {nakamaReplayEndOp, ``}}
	for _, m := range msgs {
		buf.Write(encodeWatchFrame(m.op, []byte(m.body)))
	}
	for _, m := range msgs {
		op, body, err := readWatchFrame(&buf)
		if err != nil || op != m.op || string(body) != m.body {
			t.Fatalf("read %#x %q %v; want %#x %q", op, body, err, m.op, m.body)
		}
	}
	if _, _, err := readWatchFrame(&buf); err != io.EOF {
		t.Fatalf("after the last frame: %v", err)
	}
	// A cut frame is an error, not a message.
	frame := encodeWatchFrame(nakamaReplayChunkOp, []byte(`{"stream":"s"}`))
	if _, _, err := readWatchFrame(bytes.NewReader(frame[:len(frame)-2])); err == nil {
		t.Fatal("read a cut frame")
	}
	// So is a length past the limit, before anything is allocated for it.
	huge := encodeWatchFrame(nakamaReplayChunkOp, nil)
	huge[0] = 0x7f
	if _, _, err := readWatchFrame(bytes.NewReader(huge)); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("huge frame: %v", err)
	}
}

func TestWatchLogKeepsTheMatchFromItsFirstHeader(t *testing.T) {
	var l watchLog
	l.add([]byte("old"), true)
	l.add([]byte("h0"), true)
	l.add([]byte("c0"), false)
	l.add([]byte("h1"), false) // a later Turns stream continues the match
	if len(l.frames) != 3 || string(l.frames[0]) != "h0" || string(l.frames[2]) != "h1" || l.bytes != 6 {
		t.Fatalf("kept %q (%d bytes)", l.frames, l.bytes)
	}
	// Past the limits nothing more is kept, and what was kept goes: a
	// watcher could not get the whole match from it.
	for i := 0; i < watchLogMessages; i++ {
		l.add([]byte("c"), false)
	}
	if !l.truncated || len(l.frames) != 0 || l.bytes != 0 {
		t.Fatalf("over the message limit: truncated %v, %d frames", l.truncated, len(l.frames))
	}
	l.add([]byte("c"), false)
	if len(l.frames) != 0 {
		t.Fatal("kept a frame after the limit")
	}
	// The next match starts over.
	l.add([]byte("h0"), true)
	if l.truncated || len(l.frames) != 1 {
		t.Fatalf("next match: truncated %v, %d frames", l.truncated, len(l.frames))
	}
	l.add(make([]byte, watchLogBytes), false)
	if !l.truncated {
		t.Fatal("over the byte limit, still kept")
	}
}

func TestNakamaP2PTakesOnlySignalsOfItsPurpose(t *testing.T) {
	p, err := NewNakamaP2P("lobby", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("me", "peer")
	watch := P2PSignal{Magic: p2pMagic, MatchID: "lobby", Token: "watch-token", Key: testP2PKey(t), From: "peer", To: "me", Purpose: p2pPurposeWatch}
	if err := p.HandleSignal(watch); err != nil {
		t.Fatal(err)
	}
	if p.remoteToken != "" {
		t.Fatal("the pairing's session took a watch link's signal")
	}
	pairing := watch
	pairing.Purpose, pairing.Token = "", "pairing-token"
	if err := p.HandleSignal(pairing); err != nil || p.remoteToken != "pairing-token" {
		t.Fatalf("the pairing's signal was not taken: %v", err)
	}
	if got := p.localSignal().Purpose; got != "" {
		t.Fatalf("the pairing's session signals purpose %q", got)
	}
	w, err := NewNakamaP2P("lobby", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetPeer("me", "peer")
	w.SetPurpose(p2pPurposeWatch)
	if err := w.HandleSignal(pairing); err != nil || w.remoteToken != "" {
		t.Fatal("a watch link took the pairing's signal")
	}
	if err := w.HandleSignal(watch); err != nil || w.remoteToken != "watch-token" {
		t.Fatalf("the watch link's signal was not taken: %v", err)
	}
	if got := w.localSignal().Purpose; got != p2pPurposeWatch {
		t.Fatalf("a watch link signals purpose %q", got)
	}
}

// watchTestClient is a client with a session user id and a lobby state in
// which host plays opponent in match seq 4 and watcher watches.
func watchTestClient(t *testing.T, self string) (*NakamaClient, chan []byte) {
	t.Helper()
	n, q := relayTestClient(t, 1024)
	n.mu.Lock()
	n.session = &NakamaSession{UserID: self}
	n.mu.Unlock()
	setWatchLobby(n, "host", "opponent", true, "host", "opponent", "watcher", "other")
	return n, q
}

func setWatchLobby(n *NakamaClient, player1, player2 string, streaming bool, members ...string) {
	setWatchLobbySeq(n, 4, player1, player2, streaming, members...)
}

func setWatchLobbySeq(n *NakamaClient, seq int, player1, player2 string, streaming bool, members ...string) {
	list := []any{}
	for _, m := range members {
		list = append(list, map[string]any{"user_id": m})
	}
	state := map[string]any{"members": list, "phase": "playing"}
	if player1 != "" {
		state["current"] = map[string]any{"player1": player1, "player2": player2, "seq": float64(seq), "watch": streaming}
	}
	n.applyLobbyMessage(n.CurrentMatchID(), nakamaLobbyStateOp, state)
}

func signalFrom(n *NakamaClient, sender string, s P2PSignal) {
	body, _ := json.Marshal(s)
	n.handleJSON([]byte(fmt.Sprintf(`{"match_data":{"match_id":"session-match","op_code":"%d","data":"%s","presence":{"user_id":"%s"}}}`,
		nakamaSignalOp, base64.StdEncoding.EncodeToString(body), sender)))
}

func TestWatchSignalsNeverReachThePairingSession(t *testing.T) {
	noLadderExtras(t)
	n, _ := watchTestClient(t, "host")
	watch := P2PSignal{Magic: p2pMagic, MatchID: "session-match", Token: "watcher-token", Key: testP2PKey(t), To: "host", Purpose: p2pPurposeWatch}
	// Before the pairing's session starts, its slot for the peer's early
	// signal keeps pairing signals only.
	signalFrom(n, "opponent", watch)
	n.mu.Lock()
	pending := n.pendingSignal
	n.mu.Unlock()
	if pending != nil {
		t.Fatal("a watch signal took the pairing's early-signal slot")
	}
	p, err := NewNakamaP2P("session-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("host", "opponent")
	n.mu.Lock()
	n.p2p = p
	n.mu.Unlock()
	signalFrom(n, "opponent", watch)
	signalFrom(n, "watcher", watch)
	if p.remoteToken != "" {
		t.Fatal("the pairing's session took a watch signal")
	}
	// The opponent plays the match: it gets no watch link.
	n.watchP2P.mu.Lock()
	_, opp := n.watchP2P.hosts["opponent"]
	link := n.watchP2P.hosts["watcher"]
	n.watchP2P.mu.Unlock()
	if opp {
		t.Fatal("the host opened a watch link to its opponent")
	}
	if link == nil {
		t.Fatal("the host did not open a watch link to the watcher")
	}
	if link.p.remoteToken != "watcher-token" || link.p.purpose != p2pPurposeWatch || link.p.relayer != nil {
		t.Fatalf("watch link: token %q, purpose %q, relay %v", link.p.remoteToken, link.p.purpose, link.p.relayer)
	}
	// The pairing's own signal still reaches its session.
	pairing := watch
	pairing.Purpose, pairing.Token = "", "opponent-token"
	signalFrom(n, "opponent", pairing)
	if p.remoteToken != "opponent-token" {
		t.Fatal("the pairing's signal did not reach its session")
	}
	n.watchP2P.closeAll(false)
}

func TestWatchHostServesOnlyItsMatchWatchers(t *testing.T) {
	n, _ := watchTestClient(t, "host")
	w := n.watchP2P
	cases := []struct {
		from string
		want bool
	}{{"watcher", true}, {"other", true}, {"opponent", false}, {"host", false}, {"stranger", false}, {"", false}}
	for _, c := range cases {
		if _, ok := w.mayServe(c.from); ok != c.want {
			t.Errorf("mayServe(%q) = %v, want %v", c.from, ok, c.want)
		}
	}
	// Not the match's host: nobody.
	setWatchLobby(n, "opponent", "host", true, "host", "opponent", "watcher")
	if _, ok := w.mayServe("watcher"); ok {
		t.Error("player 2 serves watchers")
	}
	// No match: nobody.
	setWatchLobby(n, "", "", false, "host", "watcher")
	if _, ok := w.mayServe("watcher"); ok {
		t.Error("serves watchers without a match")
	}
	// Not publishing: nobody.
	setWatchLobby(n, "host", "opponent", true, "host", "opponent", "watcher")
	n.SetReplayPublish(NakamaReplayPublish{Disabled: true})
	if _, ok := w.mayServe("watcher"); ok {
		t.Error("serves watchers while not publishing")
	}
	// A full house: the ninth watcher is turned away (and watches through
	// the lobby).
	n.SetReplayPublish(NakamaReplayPublish{})
	w.mu.Lock()
	for i := 0; i < watchLinkMax; i++ {
		w.hosts[fmt.Sprint("w", i)] = &watchHostLink{done: make(chan struct{})}
	}
	w.mu.Unlock()
	signalFrom(n, "watcher", P2PSignal{Magic: p2pMagic, MatchID: "session-match", Token: "t", Key: testP2PKey(t), To: "host", Purpose: p2pPurposeWatch})
	w.mu.Lock()
	_, opened := w.hosts["watcher"]
	w.mu.Unlock()
	if opened {
		t.Fatal("opened more than watchLinkMax watch links")
	}
}

// watchLobbyRouter passes signals between test clients as a lobby does (to
// the member named, with the sender's presence) and records lobby commands.
// It relays no stream: what the watcher gets came over the watch link.
type watchLobbyRouter struct {
	mu       sync.Mutex
	clients  map[string]*NakamaClient
	commands []string
}

func (r *watchLobbyRouter) run(t *testing.T, self string, q chan []byte, done chan struct{}) {
	for {
		var body []byte
		select {
		case body = <-q:
		case <-done:
			return
		}
		var env struct {
			Send *struct {
				OpCode uint32 `json:"op_code"`
				Data   []byte `json:"data"`
			} `json:"match_data_send"`
		}
		if json.Unmarshal(body, &env) != nil || env.Send == nil {
			continue
		}
		switch env.Send.OpCode {
		case nakamaSignalOp:
			var s P2PSignal
			_ = json.Unmarshal(env.Send.Data, &s)
			r.mu.Lock()
			to := r.clients[s.To]
			r.mu.Unlock()
			if to != nil {
				signalFrom(to, self, s)
			}
		case nakamaLobbyCommandOp:
			r.mu.Lock()
			r.commands = append(r.commands, self+":"+string(env.Send.Data))
			r.mu.Unlock()
		}
	}
}

func (r *watchLobbyRouter) waitCommand(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, c := range r.commands {
			if c == want {
				r.mu.Unlock()
				return
			}
		}
		r.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t.Fatalf("no %s among the lobby commands %q", want, r.commands)
}

func (r *watchLobbyRouter) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.commands {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func watchLinkPair(t *testing.T) (*NakamaClient, *NakamaClient, *watchLobbyRouter) {
	t.Helper()
	noLadderExtras(t)
	stubP2PAddresses(t, func() ([]net.IP, error) { return []net.IP{net.IPv4(127, 0, 0, 1)}, nil }, routeIPv4Address)
	host, hq := watchTestClient(t, "host")
	viewer, vq := watchTestClient(t, "watcher")
	r := &watchLobbyRouter{clients: map[string]*NakamaClient{"host": host, "watcher": viewer}}
	done := make(chan struct{})
	t.Cleanup(func() {
		host.watchP2P.closeAll(false)
		viewer.watchP2P.closeAll(false)
		close(done)
	})
	go r.run(t, "host", hq, done)
	go r.run(t, "watcher", vq, done)
	return host, viewer, r
}

func watchHeader(stream string, segment int32) ReplayStreamHeader {
	info, _ := json.Marshal(map[string]any{"kind": "lobby", "seq": 4})
	return ReplayStreamHeader{Version: replayStreamVersion, FrameRate: 60, InputCount: REPLAY_NUM_INPUTS,
		InputBytes: REPLAY_INPUT_BYTES, DelayFrames: 60, Seed: 3, Stream: stream, MatchTime: 900, MatchInfo: info, Segment: segment}
}

func waitWatchBuffered(t *testing.T, c *NakamaClient, stream string, frames int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b := c.replayBufferFor(stream); b != nil && b.BufferedThrough() >= frames {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := int32(-1)
	if b := c.replayBufferFor(stream); b != nil {
		got = b.BufferedThrough()
	}
	t.Fatalf("stream %q buffered %d, want %d", stream, got, frames)
}

func TestWatchLinkSendsTheMatchDirectly(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	sink := &nakamaReplaySink{client: host}
	// The match started before the watcher connected: the host sends what
	// it kept of it first.
	sink.PublishReplayHeader(watchHeader("seg0", 0))
	sink.PublishReplayChunk(testChunk("seg0", 0, 30))
	sink.PublishReplayChunk(testChunk("seg0", 30, 30))
	if viewer.watchP2P.ViewerTransport() != "server" {
		t.Fatal("transport before the link")
	}
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	waitWatchBuffered(t, viewer, "seg0", 60)
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	if got := viewer.watchP2P.ViewerTransport(); got != "p2p" {
		t.Fatalf("transport %q", got)
	}
	if got := host.Status().WatchLinks; got != 1 {
		t.Fatalf("host serves %d watch links", got)
	}
	// Then what it publishes, a later Turns stream included.
	sink.PublishReplayChunk(testChunk("seg0", 60, 30))
	sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: 90, Stream: "seg0"})
	sink.PublishReplayHeader(watchHeader("seg1", 1))
	sink.PublishReplayChunk(testChunk("seg1", 0, 30))
	waitWatchBuffered(t, viewer, "seg0", 90)
	waitWatchBuffered(t, viewer, "seg1", 30)
	if final, ended := viewer.replayBufferFor("seg0").FinalFrame(); !ended || final != 90 {
		t.Fatalf("seg0 end %d %v", final, ended)
	}
	if next := viewer.NextReplayBuffer(viewer.replayBufferFor("seg0")); next == nil || next.Stream() != "seg1" {
		t.Fatal("the Turns streams are not chained")
	}
	// Asking for the same host again keeps the link.
	viewer.watchP2P.mu.Lock()
	before := viewer.watchP2P.viewer
	viewer.watchP2P.mu.Unlock()
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	viewer.watchP2P.mu.Lock()
	after := viewer.watchP2P.viewer
	viewer.watchP2P.mu.Unlock()
	if before != after {
		t.Fatal("a second start replaced the open link")
	}
	if host.P2P() != nil {
		t.Fatal("the watch link touched the pairing's session")
	}
}

func TestWatchLinkLostFallsBackToTheLobby(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	sink := &nakamaReplaySink{client: host}
	sink.PublishReplayHeader(watchHeader("lost", 0))
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	// The host's side goes away (it left, or the path broke): the watcher
	// has the lobby relay the stream again, from its start.
	host.watchP2P.closeAll(false)
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":false}`)
	r.waitCommand(t, `watcher:{"kind":"watch","seq":4}`)
	if got := viewer.watchP2P.ViewerTransport(); got != "server" {
		t.Fatalf("transport %q after the link ended", got)
	}
}

func TestWatchLinkStoppedMidMatchRecordsTheGap(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	sink := &nakamaReplaySink{client: host}
	sink.PublishReplayHeader(watchHeader("stop", 0))
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	// Stopped on purpose while the match it carried goes on: what the host
	// publishes until the lobby takes over comes by neither path. The
	// watcher stopped watching, so the gap is only recorded; watching the
	// match again asks the lobby for it from its start, which clears it.
	viewer.watchP2P.StopWatchLink()
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":false}`)
	if got := viewer.watchP2P.HoleSeq(); got != 4 {
		t.Fatalf("gap seq %d after a deliberate stop, want 4", got)
	}
	if r.has(`watcher:{"kind":"watch",`) {
		t.Fatal("a deliberate stop asked the lobby to resend the match")
	}
	viewer.watchP2P.noteWatchCommand(map[string]any{"seq": 4})
	if got := viewer.watchP2P.HoleSeq(); got != 0 {
		t.Fatalf("gap seq %d after asking for the match again", got)
	}
	if got := viewer.watchP2P.ViewerTransport(); got != "server" {
		t.Fatalf("transport %q after the stop", got)
	}
	// The host notices the watcher left (its stream closed) and drops the link.
	deadline := time.Now().Add(5 * time.Second)
	for host.watchP2P.HostLinks() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the host kept the link of a watcher who stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartWatchLinkRefusals(t *testing.T) {
	viewer, _ := watchTestClient(t, "watcher")
	for _, host := range []string{"", "watcher"} {
		if err := viewer.watchP2P.StartWatchLink(host); err == nil {
			t.Errorf("StartWatchLink(%q) started", host)
		}
	}
	var nilWatch *nakamaWatchP2P
	if nilWatch.StartWatchLink("host") == nil || nilWatch.HostLinks() != 0 || nilWatch.ViewerTransport() != "server" {
		t.Error("a nil watch runner")
	}
	nilWatch.publish(nakamaReplayHeaderOp, nil, true)
	nilWatch.StopWatchLink()
	nilWatch.closeAll(false)
}

// The lobby module is checked by the live tests against a real server; this
// at least keeps it parsing without one.
func TestLobbyModuleParses(t *testing.T) {
	path := filepath.Join("..", "nakama", "modules", "ikemen_lobby.lua")
	f, err := os.Open(path)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if _, err := parse.Parse(f, path); err != nil {
		t.Fatal(err)
	}
}

func TestWatchBackfillKeepsHeadersAndTheNewest(t *testing.T) {
	if got := watchBackfill(nil); len(got) != 0 {
		t.Fatalf("backfill of nothing: %d frames", len(got))
	}
	h0 := encodeWatchFrame(nakamaReplayHeaderOp, []byte("h0"))
	old := encodeWatchFrame(nakamaReplayChunkOp, make([]byte, 20<<10))
	h1 := encodeWatchFrame(nakamaReplayHeaderOp, []byte("h1"))
	mid := encodeWatchFrame(nakamaReplayChunkOp, make([]byte, 20<<10))
	newest := encodeWatchFrame(nakamaReplayChunkOp, make([]byte, 10<<10))
	got := watchBackfill([][]byte{h0, old, h1, mid, newest})
	want := [][]byte{h0, h1, mid, newest}
	if len(got) != len(want) {
		t.Fatalf("backfill of %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
	if watchFrameOp(h1) != nakamaReplayHeaderOp || watchFrameOp([]byte{1}) != 0 {
		t.Fatal("watchFrameOp")
	}
}

func TestWatchBackfillIsPacedOneLinkAtATime(t *testing.T) {
	w := newNakamaWatchP2P(nil)
	w.backfillRate = 20 << 10
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go io.Copy(io.Discard, b)
	link := &watchHostLink{done: make(chan struct{})}
	var frames [][]byte
	for i := 0; i < 4; i++ {
		frames = append(frames, encodeWatchFrame(nakamaReplayChunkOp, make([]byte, 10<<10)))
	}
	// Another link is sending its backfill: this one waits.
	w.backfillSlot <- struct{}{}
	result := make(chan bool, 1)
	go func() { result <- w.sendBackfill(link, a, frames) }()
	select {
	case <-result:
		t.Fatal("sent a backfill while another link sent its own")
	case <-time.After(300 * time.Millisecond):
	}
	<-w.backfillSlot
	released := time.Now()
	if ok := <-result; !ok {
		t.Fatal("the backfill failed")
	}
	// 40 KB at 20 KB/s with a 20 KB burst: about a second, not at once.
	if elapsed := time.Since(released); elapsed < 800*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("the backfill took %v", elapsed)
	}
	select {
	case w.backfillSlot <- struct{}{}:
		<-w.backfillSlot
	default:
		t.Fatal("the backfill kept its slot")
	}
	// A link that ends stops its backfill.
	ended := &watchHostLink{done: make(chan struct{})}
	ended.markDone()
	if w.sendBackfill(ended, a, frames) {
		t.Fatal("an ended link sent its backfill")
	}
}

// slowCloseConn is a link's stream whose Close takes a while, as a P2P
// stream's does (p2pStream.Close).
type slowCloseConn struct {
	net.Conn
	closed chan struct{}
}

func (c *slowCloseConn) Close() error {
	time.Sleep(300 * time.Millisecond)
	close(c.closed)
	return nil
}

func TestWatchPublishEndsALaggingLinkOffTheGameThread(t *testing.T) {
	n, _ := watchTestClient(t, "host")
	w := n.watchP2P
	slow := &slowCloseConn{closed: make(chan struct{})}
	link := &watchHostLink{peer: "watcher", wake: make(chan struct{}, 1), done: make(chan struct{}), subscribed: true, stream: slow}
	link.queued = watchLinkQueueBytes
	w.mu.Lock()
	w.hosts["watcher"] = link
	w.mu.Unlock()
	begun := time.Now()
	w.publish(nakamaReplayChunkOp, []byte(`{}`), false)
	if took := time.Since(begun); took > 100*time.Millisecond {
		t.Fatalf("publish waited %v for a lagging link to close", took)
	}
	w.mu.Lock()
	_, kept := w.hosts["watcher"]
	w.mu.Unlock()
	if kept || !link.isDone() {
		t.Fatal("the lagging link was not ended at once")
	}
	select {
	case <-slow.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the lagging link's stream was never closed")
	}
}

func TestWatchCloseAllInTheBackground(t *testing.T) {
	n, _ := watchTestClient(t, "host")
	w := n.watchP2P
	(&nakamaReplaySink{client: n}).PublishReplayHeader(watchHeader("kept", 0))
	slow := &slowCloseConn{closed: make(chan struct{})}
	link := &watchHostLink{peer: "watcher", wake: make(chan struct{}, 1), done: make(chan struct{}), stream: slow}
	w.mu.Lock()
	w.hosts["watcher"] = link
	w.mu.Unlock()
	begun := time.Now()
	// The connection was lost while the match goes on over the players'
	// own path: the links go, what was kept of the match stays.
	w.closeAll(true)
	if took := time.Since(begun); took > 100*time.Millisecond {
		t.Fatalf("closeAll waited %v for the links to close", took)
	}
	w.mu.Lock()
	frames, seq, links := len(w.log.frames), w.logSeq, len(w.hosts)
	w.mu.Unlock()
	if !link.isDone() || links != 0 {
		t.Fatal("closeAll left a link")
	}
	if frames != 1 || seq != 4 {
		t.Fatalf("after a lost connection the log keeps %d frames of seq %d", frames, seq)
	}
	// Leaving the lobby forgets it.
	w.closeAll(false)
	w.mu.Lock()
	frames, seq = len(w.log.frames), w.logSeq
	w.mu.Unlock()
	if frames != 0 || seq != 0 {
		t.Fatalf("after leaving the log keeps %d frames of seq %d", frames, seq)
	}
	select {
	case <-slow.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the link's stream was never closed")
	}
}

func waitHostLinks(t *testing.T, host *NakamaClient, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for host.watchP2P.HostLinks() != want {
		if time.Now().After(deadline) {
			t.Fatalf("the host serves %d watch links, want %d", host.watchP2P.HostLinks(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchLinkSendsNoBurstOfTheKeptMatch(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	sink := &nakamaReplaySink{client: host}
	sink.PublishReplayHeader(watchHeader("big", 0))
	var last int32
	for frame := int32(0); ; frame += 30 {
		sink.PublishReplayChunk(testChunk("big", frame, 30))
		last = frame
		host.watchP2P.mu.Lock()
		kept := host.watchP2P.log.bytes
		host.watchP2P.mu.Unlock()
		if kept > 4*watchBackfillTail {
			break
		}
	}
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	// The header and the newest messages came; the start of the match did
	// not (the lobby relayed it already).
	b := viewer.replayBufferFor("big")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b.mu.RLock()
		_, newest := b.frames[last]
		_, first := b.frames[0]
		b.mu.RUnlock()
		if first {
			t.Fatal("the link sent the whole kept match")
		}
		if newest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the newest kept messages did not come over the link")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Then the live messages.
	sink.PublishReplayChunk(testChunk("big", last+30, 30))
	deadline = time.Now().Add(5 * time.Second)
	for {
		b.mu.RLock()
		_, live := b.frames[last+30]
		b.mu.RUnlock()
		if live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a live message did not come over the link")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchHostSendsNoLogOfAnOldMatch(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	sink := &nakamaReplaySink{client: host}
	sink.PublishReplayHeader(watchHeader("old", 0))
	sink.PublishReplayChunk(testChunk("old", 0, 30))
	// The lobby started the host's next match; its first header is yet to
	// come.
	for _, c := range []*NakamaClient{host, viewer} {
		setWatchLobbySeq(c, 5, "host", "opponent", true, "host", "opponent", "watcher", "other")
	}
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	waitHostLinks(t, host, 1, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	if viewer.replayBufferFor("old") != nil || r.has(`watcher:{"kind":"watch_p2p","on":true}`) {
		t.Fatal("the link sent the previous match")
	}
	sink.PublishReplayHeader(watchHeader("new", 0))
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	if viewer.replayBufferFor("new") == nil {
		t.Fatal("the new match's header did not come over the link")
	}
}

func TestWatchHostDropsALinkToAPlayer(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	(&nakamaReplaySink{client: host}).PublishReplayHeader(watchHeader("m", 0))
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	waitHostLinks(t, host, 1, 5*time.Second)
	// The watcher plays the host's next match: no linger for it.
	setWatchLobbySeq(host, 5, "host", "watcher", false, "host", "opponent", "watcher", "other")
	waitHostLinks(t, host, 0, 4*time.Second)
}

// quickViewerTries makes a watcher's tries short: each session gives up
// after timeout, and the next starts after 100 ms.
func quickViewerTries(w *nakamaWatchP2P, timeout, retryFor time.Duration) {
	w.tune = func(p *NakamaP2P) { p.timeout = timeout }
	w.retryDelay = 100 * time.Millisecond
	w.retryFor = retryFor
}

func TestWatchLinkWaitsForTheHostToServe(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	// The lobby starts the link with the stream, maybe before the host
	// publishes: the host turns its signals away until it does.
	host.SetReplayPublish(NakamaReplayPublish{Disabled: true})
	quickViewerTries(viewer.watchP2P, 700*time.Millisecond, 20*time.Second)
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	viewer.watchP2P.mu.Lock()
	first := viewer.watchP2P.viewer.session()
	viewer.watchP2P.mu.Unlock()
	time.Sleep(1500 * time.Millisecond) // past the first session's timeout
	viewer.watchP2P.mu.Lock()
	trying := viewer.watchP2P.viewer
	viewer.watchP2P.mu.Unlock()
	if trying == nil || trying.isDone() {
		t.Fatal("the watcher gave up while the host did not serve yet")
	}
	if trying.session() == first {
		t.Fatal("the watcher did not try again with a new session")
	}
	if got := viewer.watchP2P.ViewerTransport(); got != "server" || host.watchP2P.HostLinks() != 0 || r.has(`watcher:{"kind":"watch_p2p"`) {
		t.Fatalf("before the host serves: transport %q, %d host links", got, host.watchP2P.HostLinks())
	}
	host.SetReplayPublish(NakamaReplayPublish{})
	(&nakamaReplaySink{client: host}).PublishReplayHeader(watchHeader("late", 0))
	r.waitCommand(t, `watcher:{"kind":"watch_p2p","on":true}`)
	if got := viewer.watchP2P.ViewerTransport(); got != "p2p" {
		t.Fatalf("transport %q", got)
	}
}

func TestWatchLinkGivesUpQuietly(t *testing.T) {
	host, viewer, r := watchLinkPair(t)
	host.SetReplayPublish(NakamaReplayPublish{Disabled: true})
	quickViewerTries(viewer.watchP2P, 300*time.Millisecond, time.Second)
	if err := viewer.watchP2P.StartWatchLink("host"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		viewer.watchP2P.mu.Lock()
		v := viewer.watchP2P.viewer
		viewer.watchP2P.mu.Unlock()
		if v == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the watcher kept trying past its bound")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r.has(`watcher:{"kind":"watch`) || viewer.watchP2P.ViewerTransport() != "server" {
		t.Fatal("a link that never connected told the lobby something")
	}
}
