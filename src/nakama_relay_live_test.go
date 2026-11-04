package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

// A matchmade pair whose UDP paths do not work connects through a relayed
// match on the live server, and runs the netplay session handshake and a
// GGPO channel over it. The session match stays the clients' match.
func TestNakamaLive_MatchmakerP2PRelaySession(t *testing.T) {
	fixedEnabled(t)
	// Keep the test off this machine's IPv6 addresses (both clients run
	// here, and would reach each other over them) and off the real router.
	prevIPv6, prevMap := p2pIPv6Addresses, p2pMapPort
	p2pIPv6Addresses = func() ([]net.IP, error) { return nil, nil }
	p2pMapPort = func(context.Context, int) (*portmap.Mapping, error) { return nil, errors.New("no router in tests") }
	t.Cleanup(func() { p2pIPv6Addresses, p2pMapPort = prevIPv6, prevMap })

	a, b := matchPair(t)
	pa, _ := a.Pairing()
	pb, _ := b.Pairing()
	sessionMatch := a.CurrentMatchID()
	if sessionMatch == "" || sessionMatch != b.CurrentMatchID() {
		t.Fatalf("session matches %q and %q", sessionMatch, b.CurrentMatchID())
	}

	// StartP2P, with one difference: each side offers only a localhost port
	// nobody listens on, as if its NAT dropped everything, so the handshake
	// has to reach the relay step.
	start := func(c *NakamaClient, peer string) *NakamaP2P {
		p, err := NewNakamaP2P(sessionMatch, 0, func(s P2PSignal) error { return c.PublishSignal(s) })
		if err != nil {
			t.Fatal(err)
		}
		p.SetRelay(c)
		p.SetPeer(c.Session().UserID, peer)
		p.relayDelay = 2 * time.Second
		p.local = []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}
		c.mu.Lock()
		c.p2p = p
		pending := c.pendingSignal
		c.pendingSignal = nil
		c.mu.Unlock()
		if pending != nil {
			_ = p.HandleSignal(*pending)
		}
		p.resignal()
		go p.punchLoop()
		return p
	}
	t0 := time.Now()
	p1 := start(a, pa.PeerUserID)
	p2 := start(b, pb.PeerUserID)
	waitP2PReady(t, a, b)
	if p1.Route() != "relay" || p2.Route() != "relay" {
		t.Fatalf("routes %q and %q, want relay", p1.Route(), p2.Route())
	}
	t.Logf("connected over the relay %v after the start", time.Since(t0).Round(time.Millisecond))
	if a.CurrentMatchID() != sessionMatch || b.CurrentMatchID() != sessionMatch {
		t.Fatalf("the relay replaced the session match: %q, %q", a.CurrentMatchID(), b.CurrentMatchID())
	}
	if st := a.Status(); st.P2PRoute != "relay" || st.P2PStage != "relay" {
		t.Fatalf("status route %q stage %q", st.P2PRoute, st.P2PStage)
	}

	streamA, err := a.OpenP2PStream()
	if err != nil {
		t.Fatal(err)
	}
	streamB, err := b.OpenP2PStream()
	if err != nil {
		t.Fatal(err)
	}
	ncA, ncB := NewNetConnection(), NewNetConnection()
	ncA.AttachStream(streamA, pa.Host)
	ncB.AttachStream(streamB, pb.Host)
	deadline := time.Now().Add(10 * time.Second)
	for !ncA.hasConn() || !ncB.hasConn() {
		if time.Now().After(deadline) {
			t.Fatalf("session handshake: %v / %v", ncA.AttachError(), ncB.AttachError())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ncA.writeJSON(map[string]string{"from": "a"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	_ = streamB.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ncB.readJSON(&got); err != nil || got["from"] != "a" {
		t.Fatalf("session data over the relay: %v %v", got, err)
	}

	gA, remoteA, err := a.P2PGameplayConn()
	if err != nil {
		t.Fatal(err)
	}
	gB, _, err := b.P2PGameplayConn()
	if err != nil {
		t.Fatal(err)
	}
	defer gA.Close()
	defer gB.Close()
	if remoteA.IP.To4() == nil {
		t.Fatalf("GGPO peer address %v is not IPv4", remoteA)
	}
	// Round trips of GGPO-sized datagrams through the server.
	buf := make([]byte, 64)
	var total time.Duration
	const rounds = 20
	for i := 0; i < rounds; i++ {
		sent := time.Now()
		if _, err := gA.WriteTo([]byte("rollback"), remoteA); err != nil {
			t.Fatal(err)
		}
		_ = gB.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, from, err := gB.ReadFrom(buf)
		if err != nil || string(buf[:n]) != "rollback" {
			t.Fatalf("gameplay datagram over the relay: %q %v", buf[:n], err)
		}
		if _, err := gB.WriteTo(buf[:n], from); err != nil {
			t.Fatal(err)
		}
		_ = gA.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := gA.ReadFrom(buf); err != nil {
			t.Fatal(err)
		}
		total += time.Since(sent)
	}
	t.Logf("mean round trip through the relay: %v", (total / rounds).Round(10*time.Microsecond))

	// Closing the session leaves the relayed match.
	_ = streamA.Close()
	_ = streamB.Close()
	a.StopP2P()
	b.StopP2P()
	a.relayMu.Lock()
	left := len(a.relayByMatch) == 0 && len(a.relayByCID) == 0
	a.relayMu.Unlock()
	if !left {
		t.Fatal("the client still holds the relay after the session ended")
	}
}
