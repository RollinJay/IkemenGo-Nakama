package main

// Live tests of direct watch links (nakama_watch_p2p.go) against a lobby
// module: the lobby skips the members whose link carries the stream
// ("watch_p2p"), and a link opened through the lobby's signal relay sends
// the match peer to peer and falls back to the lobby when it drops. Enable
// with NAKAMA_LIVE=1 and NAKAMA_PORT (see nakama_live_test.go).

import (
	"net"
	"os"
	"testing"
	"time"
)

// watchLiveEnabled is fixedEnabled, and also wants NAKAMA_PORT set: these
// tests open watch links and lobbies, which should never land on a server
// people play on through the default port.
func watchLiveEnabled(t *testing.T) {
	t.Helper()
	fixedEnabled(t)
	if p := os.Getenv("NAKAMA_PORT"); p == "" || p == "7350" {
		t.Skip("set NAKAMA_PORT to a scratch server's port (not 7350) for the watch link live tests")
	}
}

// watchLiveMatch starts a lobby with watching on, three members and a and b
// playing; it returns the match's host (player 1), the other player, the
// watcher c, the lobby id and the match's seq.
func watchLiveMatch(t *testing.T, name string) (host, other, c *NakamaClient, id string, seq int) {
	t.Helper()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id, _ = createLobbyWith(t, a, map[string]any{"name": name, "interval": 3, "watch": 1})
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, c, "a-b playing", lobbyPhase("playing"))
	p1, _, seq := currentPair(st)
	host, other = a, b
	if p1 != a.Session().UserID {
		host, other = b, a
	}
	return host, other, c, id, seq
}

func noFrames(t *testing.T, c *NakamaClient, stream string, wait time.Duration) {
	t.Helper()
	time.Sleep(wait)
	if b := c.replayBufferFor(stream); b != nil && b.BufferedThrough() > 0 {
		t.Fatalf("stream %q reached a member whose watch link carries it (%d frames)", stream, b.BufferedThrough())
	}
}

// The lobby module's side alone: a member who says its watch link carries
// the stream is skipped by the relay until it says the link ended, and a new
// match relays to it again.
func TestNakamaLive_LobbyWatchP2PSkipsRelay(t *testing.T) {
	watchLiveEnabled(t)
	host, other, c, id, seq := watchLiveMatch(t, "watch-p2p")
	d, recD := liveClient(t)
	join(t, d, recD, id)
	if err := c.SendLobbyCommand("watch_p2p", map[string]any{"on": true}); err != nil {
		t.Fatal(err)
	}
	// The other player cannot use it to receive the stream.
	_ = other.SendLobbyCommand("watch_p2p", map[string]any{"on": true})
	time.Sleep(300 * time.Millisecond)
	sendStream(t, host, "skip1", seq, 60, false)
	waitBuffered(t, d, "member without a link", "skip1", 60)
	noFrames(t, c, "skip1", 300*time.Millisecond)
	if other.replayBufferFor("skip1") != nil {
		t.Fatal("the other player received the stream")
	}
	// The link ended: the relay goes on, and the kept stream fills the gap.
	_ = c.SendLobbyCommand("watch_p2p", map[string]any{"on": false})
	_ = c.SendLobbyCommand("watch", map[string]any{"seq": seq})
	waitBuffered(t, c, "back on the relay", "skip1", 60)
	// A new match clears the flag.
	_ = c.SendLobbyCommand("watch_p2p", map[string]any{"on": true})
	time.Sleep(300 * time.Millisecond)
	if err := host.ReportLobbyResultSeq(host.Session().UserID, other.Session().UserID, int64(seq)); err != nil {
		t.Fatal(err)
	}
	st := waitLobby(t, c, "next match", func(st map[string]any) bool {
		_, _, s := currentPair(st)
		return s > seq && st["phase"] == "playing"
	})
	p1, p2, seq2 := currentPair(st)
	var nextHost *NakamaClient
	for _, m := range []*NakamaClient{host, other, c, d} {
		if m.Session().UserID == p1 {
			nextHost = m
		}
	}
	if nextHost == nil || p1 == c.Session().UserID || p2 == c.Session().UserID {
		t.Skipf("c plays the next match (%s-%s); the flag's reset is not observable", p1, p2)
	}
	sendStream(t, nextHost, "skip2", seq2, 30, false)
	waitBuffered(t, c, "relayed again after a new match", "skip2", 30)
}

// A real link: signals through the lobby, the stream peer to peer, and the
// lobby's relay again once the link drops.
func TestNakamaLive_LobbyWatchLink(t *testing.T) {
	watchLiveEnabled(t)
	// Host candidates only: no IPv6 or router ports in a test.
	noLadderExtras(t)
	stubP2PAddresses(t, func() ([]net.IP, error) { return []net.IP{net.IPv4(127, 0, 0, 1)}, nil }, routeIPv4Address)
	host, _, c, id, seq := watchLiveMatch(t, "watch-link")
	d, recD := liveClient(t)
	join(t, d, recD, id)
	// The match started before the watcher connected.
	sendStream(t, host, "link1", seq, 60, false)
	waitBuffered(t, d, "member without a link", "link1", 60)
	if err := c.watchP2P.StartWatchLink(host.Session().UserID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for c.watchP2P.ViewerTransport() != "p2p" || host.Status().WatchLinks != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("no watch link: transport %q, host links %d", c.watchP2P.ViewerTransport(), host.Status().WatchLinks)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The lobby stops relaying to c once told; the link carries the rest.
	time.Sleep(300 * time.Millisecond)
	sink := &nakamaReplaySink{client: host}
	for f := int32(60); f < 120; f += 30 {
		sink.PublishReplayChunk(testChunk("link1", f, 30))
	}
	waitBuffered(t, c, "over the link", "link1", 120)
	waitBuffered(t, d, "through the lobby", "link1", 120)
	// The host drops the link: c tells the lobby and gets the stream from it.
	host.watchP2P.mu.Lock()
	links := []*watchHostLink{}
	for _, l := range host.watchP2P.hosts {
		links = append(links, l)
	}
	host.watchP2P.mu.Unlock()
	for _, l := range links {
		host.watchP2P.endHost(l)
	}
	deadline = time.Now().Add(20 * time.Second)
	for c.watchP2P.ViewerTransport() != "server" {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not notice the link dropped")
		}
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	for f := int32(120); f < 180; f += 30 {
		sink.PublishReplayChunk(testChunk("link1", f, 30))
	}
	waitBuffered(t, c, "through the lobby after the link", "link1", 180)
}
