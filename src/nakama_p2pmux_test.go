package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// p2pPair runs a real handshake between two local P2P objects. When via is
// set, each side sees the other at the given address instead of 127.0.0.1.
func p2pPair(t *testing.T, candidates func(a, b *NakamaP2P) (forA, forB P2PCandidate)) (*NakamaP2P, *NakamaP2P) {
	t.Helper()
	a, err := NewNakamaP2P("mux-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNakamaP2P("mux-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	forA := P2PCandidate{Type: "host", Address: "127.0.0.1", Port: b.LocalPort()}
	forB := P2PCandidate{Type: "host", Address: "127.0.0.1", Port: a.LocalPort()}
	if candidates != nil {
		forA, forB = candidates(a, b)
	}
	if err := a.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "mux-match", Token: b.token, Candidates: []P2PCandidate{forA}}); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "mux-match", Token: a.token, Candidates: []P2PCandidate{forB}}); err != nil {
		t.Fatal(err)
	}
	go a.punchLoop()
	go b.punchLoop()
	for _, p := range []*NakamaP2P{a, b} {
		select {
		case <-p.ready:
		case <-time.After(10 * time.Second):
			t.Fatal("P2P handshake did not complete")
		}
	}
	return a, b
}

func openStreams(t *testing.T, a, b *NakamaP2P) (net.Conn, net.Conn) {
	t.Helper()
	sa, err := a.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sa.Close(); sb.Close() })
	return sa, sb
}

func TestP2PStreamAndGameplayShareSocket(t *testing.T) {
	a, b := p2pPair(t, nil)
	sa, sb := openStreams(t, a, b)

	// Stream: both directions, larger than one KCP segment.
	payload := make([]byte, 5000)
	_, _ = rand.Read(payload)
	go func() { _, _ = sa.Write(payload) }()
	got := make([]byte, len(payload))
	_ = sb.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(sb, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("stream A->B: err=%v equal=%v", err, bytes.Equal(got, payload))
	}
	if _, err := sb.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 4)
	_ = sa.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(sa, reply); err != nil || string(reply) != "pong" {
		t.Fatalf("stream B->A: %q %v", reply, err)
	}

	// A read deadline surfaces as a plain net.Error timeout, which is what
	// NetConnection's polling reads check for.
	_ = sb.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	_, err := sb.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("stream deadline error = %T %v, want a net.Error timeout", err, err)
	}
	_ = sb.SetReadDeadline(time.Time{})

	// GGPO channel on the same socket, while the stream stays open.
	ga, ra, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	gb, rb, err := b.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ga.WriteTo([]byte("ggpo"), ra); err != nil {
		t.Fatal(err)
	}
	_ = gb.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, from, err := gb.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "ggpo" {
		t.Fatalf("gameplay A->B: %q %v", buf[:n], err)
	}
	if from.String() != rb.String() {
		t.Fatalf("gameplay packet reported from %v, want the canonical peer %v", from, rb)
	}
	_ = gb.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := gb.ReadFrom(buf); err == nil {
		t.Fatal("gameplay read returned data after its deadline")
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("gameplay deadline error = %v", err)
	}

	// Closing the GGPO channel (as GGPO does after each match) leaves the
	// stream working, and a new channel can be opened for the next match.
	_ = ga.Close()
	_ = gb.Close()
	if _, err := sa.Write([]byte("still")); err != nil {
		t.Fatal(err)
	}
	still := make([]byte, 5)
	_ = sb.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(sb, still); err != nil || string(still) != "still" {
		t.Fatalf("stream after gameplay close: %q %v", still, err)
	}
	ga2, ra2, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	gb2, _, err := b.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	defer ga2.Close()
	defer gb2.Close()
	_, _ = ga2.WriteTo([]byte("next"), ra2)
	_ = gb2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _, err := gb2.ReadFrom(buf); err != nil || string(buf[:n]) != "next" {
		t.Fatalf("second gameplay channel: %q %v", buf[:n], err)
	}
}

// Closing one side's stream reaches the other side as a read error (like a
// closed TCP connection) instead of a silent stall.
func TestP2PStreamCloseNotifiesPeer(t *testing.T) {
	a, b := p2pPair(t, nil)
	sa, sb := openStreams(t, a, b)
	if _, err := sa.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	_ = sb.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(sb, one); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = sa.Close()
	_ = sb.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := sb.Read(one)
	if err == nil {
		t.Fatal("peer stream stayed open after close")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("peer only noticed the close by timing out: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("close took %v to reach the peer", d)
	}
}

// A hello from a new address after the handshake (the peer's packets taking
// another path, or its acks were lost) is answered, and that address is then
// accepted for session traffic.
func TestP2PLateHelloIsAnsweredAndAccepted(t *testing.T) {
	a, b := p2pPair(t, nil)
	ga, _, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()

	other, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	aAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.LocalPort()}

	// Data from an address that never completed a handshake is ignored.
	_, _ = other.WriteToUDP(append([]byte{p2pTagGGPO}, "early"...), aAddr)
	_ = ga.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := ga.ReadFrom(make([]byte, 16)); err == nil {
		t.Fatalf("accepted %d bytes from an unknown address", n)
	}

	hello, _ := json.Marshal(p2PPacket{Magic: p2pMagic, Kind: p2pHello, MatchID: "mux-match", Token: b.token})
	_, _ = other.WriteToUDP(hello, aAddr)
	_ = other.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 512)
	n, _, err := other.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("no ack for a late hello: %v", err)
	}
	var ack p2PPacket
	if json.Unmarshal(buf[:n], &ack) != nil || ack.Kind != p2pAck || ack.Token != a.token {
		t.Fatalf("unexpected reply %q", buf[:n])
	}

	_, _ = other.WriteToUDP(append([]byte{p2pTagGGPO}, "via-other-path"...), aAddr)
	_ = ga.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err = ga.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "via-other-path" {
		t.Fatalf("data from the confirmed address: %q %v", buf[:n], err)
	}
}

// lossyRelay forwards datagrams between two endpoints and drops a share of them.
type lossyRelay struct {
	facingA, facingB *net.UDPConn
	lossPct          int64
	mu               sync.Mutex
	aAddr, bAddr     *net.UDPAddr
}

func newLossyRelay(t *testing.T, lossPct int64) *lossyRelay {
	t.Helper()
	fa, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := &lossyRelay{facingA: fa, facingB: fb, lossPct: lossPct}
	t.Cleanup(func() { fa.Close(); fb.Close() })
	go r.pump(fa, fb, true)
	go r.pump(fb, fa, false)
	return r
}

func (r *lossyRelay) pump(in, out *net.UDPConn, fromA bool) {
	buf := make([]byte, 65536)
	for {
		n, from, err := in.ReadFromUDP(buf)
		if err != nil {
			return
		}
		r.mu.Lock()
		if fromA {
			r.aAddr = from
		} else {
			r.bAddr = from
		}
		dst := r.bAddr
		if !fromA {
			dst = r.aAddr
		}
		r.mu.Unlock()
		if dst == nil {
			continue
		}
		if v, _ := rand.Int(rand.Reader, big.NewInt(100)); v.Int64() < r.lossPct {
			continue
		}
		_, _ = out.WriteToUDP(buf[:n], dst)
	}
}

// The netplay stream stays intact and in order with 20% packet loss.
func TestP2PStreamSurvivesPacketLoss(t *testing.T) {
	relay := newLossyRelay(t, 20)
	a, b := p2pPair(t, func(a, b *NakamaP2P) (P2PCandidate, P2PCandidate) {
		relay.mu.Lock()
		relay.bAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: b.LocalPort()}
		relay.aAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.LocalPort()}
		relay.mu.Unlock()
		forA := P2PCandidate{Type: "relay", Address: "127.0.0.1", Port: relay.facingA.LocalAddr().(*net.UDPAddr).Port}
		forB := P2PCandidate{Type: "relay", Address: "127.0.0.1", Port: relay.facingB.LocalAddr().(*net.UDPAddr).Port}
		return forA, forB
	})
	sa, sb := openStreams(t, a, b)

	// 600 frames of 8-byte inputs each way, the per-frame write NetConnection makes.
	const frames = 600
	send := func(c net.Conn, seed byte) {
		for i := 0; i < frames; i++ {
			frame := bytes.Repeat([]byte{seed + byte(i)}, REPLAY_INPUT_BYTES)
			if _, err := c.Write(frame); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	check := func(c net.Conn, seed byte, errs chan<- error) {
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		frame := make([]byte, REPLAY_INPUT_BYTES)
		for i := 0; i < frames; i++ {
			if _, err := io.ReadFull(c, frame); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(frame, bytes.Repeat([]byte{seed + byte(i)}, REPLAY_INPUT_BYTES)) {
				errs <- errors.New("frame out of order or corrupted")
				return
			}
		}
		errs <- nil
	}
	errs := make(chan error, 2)
	go send(sa, 1)
	go send(sb, 101)
	go check(sb, 1, errs)
	go check(sa, 101, errs)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// NetConnection runs its usual handshake and polling reads over the stream.
func TestNetConnectionAttachStream(t *testing.T) {
	a, b := p2pPair(t, nil)
	sa, sb := openStreams(t, a, b)
	host := NewNetConnection()
	guest := NewNetConnection()
	host.AttachStream(sa, true)
	guest.AttachStream(sb, false)
	deadline := time.Now().Add(10 * time.Second)
	for !host.hasConn() || !guest.hasConn() {
		if time.Now().After(deadline) {
			t.Fatalf("session handshake did not finish: host=%v guest=%v", host.AttachError(), guest.AttachError())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !host.host || guest.host {
		t.Fatal("host roles were not applied")
	}
	// Loading barrier style polling read: no data is not an error.
	if _, ok, err := guest.tryReadU8(); ok || err != nil {
		t.Fatalf("empty poll: ok=%v err=%v", ok, err)
	}
	if err := host.writeU8(netLoadingReadyToken); err != nil {
		t.Fatal(err)
	}
	var v byte
	var ok bool
	var err error
	for i := 0; i < 2000 && !ok; i++ {
		v, ok, err = guest.tryReadU8()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !ok || v != netLoadingReadyToken {
		t.Fatalf("poll read: ok=%v v=%#x", ok, v)
	}
	if err := guest.writeJSON(map[string]int{"n": 7}); err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	_ = host.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := host.readJSON(&got); err != nil || got["n"] != 7 {
		t.Fatalf("readJSON: %v %v", got, err)
	}
}

// A peer that disappears without closing (crash, killed process) ends the
// stream once keepalives stop arriving, instead of stalling until SyncTimeout.
func TestP2PStreamEndsWhenPeerGoesSilent(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the peer silence timeout")
	}
	a, b := p2pPair(t, nil)
	sa, _ := openStreams(t, a, b)
	start := time.Now()
	b.Close() // the socket goes away without a close notice
	_ = sa.SetReadDeadline(time.Now().Add(p2pPeerSilenceTimeout + 5*time.Second))
	_, err := sa.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("read succeeded after the peer vanished")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("stream was not closed within %v of silence", time.Since(start))
	}
	if d := time.Since(start); d < p2pPeerSilenceTimeout-time.Second || d > p2pPeerSilenceTimeout+3*time.Second {
		t.Fatalf("stream closed after %v, want about %v", d, p2pPeerSilenceTimeout)
	}
}

// A handshake that never completes is reported instead of hanging.
func TestNetConnectionAttachStreamReportsFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the handshake timeout")
	}
	a, b := p2pPair(t, nil)
	sa, _ := openStreams(t, a, b)
	guest := NewNetConnection()
	guest.AttachStream(sa, false) // the peer never sends its side
	deadline := time.Now().Add(netStreamHandshakeTimeout + 3*time.Second)
	for guest.AttachError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("no handshake error was reported")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if guest.hasConn() {
		t.Fatal("failed handshake left a connection attached")
	}
}

// Signals addressed to other lobby members are ignored.
func TestP2PSignalAddressing(t *testing.T) {
	p, err := NewNakamaP2P("lobby", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("me", "opponent")
	cand := []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: 9}}
	for _, sig := range []P2PSignal{
		{Magic: p2pMagic, MatchID: "lobby", Token: "t1", Candidates: cand, From: "opponent", To: "someone-else"},
		{Magic: p2pMagic, MatchID: "lobby", Token: "t2", Candidates: cand, From: "third-player", To: "me"},
	} {
		if err := p.HandleSignal(sig); err != nil {
			t.Fatal(err)
		}
		if p.remoteToken != "" {
			t.Fatalf("accepted a signal from %s to %s", sig.From, sig.To)
		}
	}
	if err := p.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "lobby", Token: "t3", Candidates: cand, From: "opponent", To: "me"}); err != nil {
		t.Fatal(err)
	}
	if p.remoteToken != "t3" {
		t.Fatal("the opponent's signal was not accepted")
	}
}

func TestMatchmakerPairingUsesUserOrder(t *testing.T) {
	var m NakamaMatchmakerMatched
	raw := `{"match_id":"m1","users":[{"presence":{"user_id":"u1","session_id":"s1"}},{"presence":{"user_id":"u2","session_id":"s2"}}],"self":{"presence":{"user_id":"u2","session_id":"s2"}}}`
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	p := matchmakerPairing(&m)
	if p == nil || p.Host || p.PeerUserID != "u1" || p.MatchID != "m1" {
		t.Fatalf("second user: %+v", p)
	}
	m.Self = m.Users[0]
	p = matchmakerPairing(&m)
	if p == nil || !p.Host || p.PeerUserID != "u2" {
		t.Fatalf("first user: %+v", p)
	}
}
