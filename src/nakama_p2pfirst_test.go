package main

// Direct first, relay as the fallback: the ladder against the real UDP relay
// (nakama/udp-relay/relay) and its recovery, a route that breaks in one
// direction only, the router's port, route names and route-tagged pings.

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	udprelay "github.com/ikemen-engine/Ikemen-GO/nakama/udp-relay/relay"
	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

// realUDPRelayer hands out tickets to a real relay running on localhost, as
// ikemen_relay.lua does, on the relay's clock.
type realUDPRelayer struct {
	fakeRelayer
	addr    string
	clock   func() time.Time
	ttl     time.Duration
	tickets *atomic.Int32
}

func (r realUDPRelayer) RequestUDPRelay(matchID, peer string) (udpRelayTicket, error) {
	r.tickets.Add(1)
	session := udprelay.SessionID(udpRelayTestSecret, matchID, r.user, peer)
	side := 0
	if peer < r.user {
		side = 1
	}
	exp := r.clock().Add(r.ttl).Unix()
	return udpRelayTicket{Addr: r.addr, Token: udprelay.Sign(udpRelayTestSecret, session, side, exp), Expires: exp, Peer: peer, Session: session}, nil
}

// Two players whose direct routes fail meet in the real UDP relay, through
// dialUDPRelay, and play on it. When the relay forgets them (idle sweep, a
// restart), their paths join again with the same tickets; when it forgets
// them after their tickets expired, the paths die and the sessions ask for
// new tickets and carry on, on the UDP relay again.
func TestP2PLadderRecoversOnTheRealUDPRelay(t *testing.T) {
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	srv, conn := startUDPRelay(t, udprelay.Config{Now: clock, SessionAnswerRate: 200, JoinAnswerRate: 200, AnswerRate: 50})
	hub := newFakeRelayHub()
	var tickets atomic.Int32
	a, b, n := natPair(t, false, false, nil)
	ttl := time.Hour
	a.SetRelay(realUDPRelayer{fakeRelayer{hub, "user-a"}, conn.LocalAddr().String(), clock, ttl, &tickets})
	b.SetRelay(realUDPRelayer{fakeRelayer{hub, "user-b"}, conn.LocalAddr().String(), clock, ttl, &tickets})
	for _, p := range []*NakamaP2P{a, b} {
		p.mu.Lock()
		p.peerServerRelay = true
		p.mu.Unlock()
		p.relayLinger = 300 * time.Millisecond
	}
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	waitRoute(t, a, "relay-udp", 3*time.Second)
	waitRoute(t, b, "relay-udp", 3*time.Second)
	cross := ggpoPair(t, a, b)
	cross("on the UDP relay")
	if got := tickets.Load(); got != 2 {
		t.Fatalf("%d tickets, want 2", got)
	}

	// The relay forgets both sides: they join again with their tickets.
	offset.Store(int64(2 * time.Minute))
	srv.Sweep()
	if st := srv.Stats(); st.Sessions != 0 {
		t.Fatalf("%d sessions after the sweep", st.Sessions)
	}
	cross("after the relay forgot them")
	if got := tickets.Load(); got != 2 {
		t.Fatalf("%d tickets after rejoining, want still 2", got)
	}

	// Forgotten after the tickets expired: new tickets, and on the UDP
	// relay again.
	offset.Store(int64(2 * ttl))
	srv.Sweep()
	deadline := time.Now().Add(10 * time.Second)
	onLivePath := func(p *NakamaP2P) bool {
		path := p.activePath()
		p.mu.Lock()
		defer p.mu.Unlock()
		return path != nil && path == p.udpRelay && !p2pPathDead(path)
	}
	for tickets.Load() < 4 || !onLivePath(a) || !onLivePath(b) {
		if time.Now().After(deadline) {
			t.Fatalf("%d tickets, routes %q and %q; want new tickets and the UDP relay", tickets.Load(), a.Route(), b.Route())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cross("with new tickets")
	// Off the websocket relay, should the sessions have taken it meanwhile.
	deadline = time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		ws := a.relay
		a.mu.Unlock()
		if ws == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the websocket relay stayed open on the UDP relay")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A UDP relay path the relay refuses after it joined (the ticket expired on
// a rejoin) is dead, and a dead path is replaced, not kept.
func TestUDPRelayPathDiesWhenRefused(t *testing.T) {
	var offset atomic.Int64
	clock := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	srv, conn := startUDPRelay(t, udprelay.Config{Now: clock})
	sess := udprelay.SessionID(udpRelayTestSecret, "m", "a", "b")
	exp := time.Now().Add(time.Minute).Unix()
	tk := udpRelayTicket{Addr: conn.LocalAddr().String(), Token: udprelay.Sign(udpRelayTestSecret, sess, 0, exp), Expires: exp, Session: sess}
	path, err := dialUDPRelay(tk, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	defer p2pClosePath(path)
	if p2pPathDead(path) {
		t.Fatal("dead at once")
	}
	offset.Store(int64(time.Hour))
	srv.Sweep()
	// The keepalive's answer says to join again, and the join is refused.
	waitFor(t, "the path to die", func() bool {
		// A data datagram gets the answer at once.
		_ = path.write([]byte("x"), nil)
		return p2pPathDead(path)
	})
}

// A direct route that breaks in one direction only: the side that still
// receives the peer's data notices too, by its missing pongs, and both
// sides move to the relay, which they had left.
func TestP2POneWayLossMovesBothSidesToTheRelay(t *testing.T) {
	hub := newFakeRelayHub()
	a, b, n := natPair(t, false, false, hub)
	a.relayLinger, b.relayLinger = 200*time.Millisecond, 200*time.Millisecond
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	cross := ggpoPair(t, a, b)
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
	waitRoute(t, b, "direct", 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		left := a.relay == nil
		a.mu.Unlock()
		b.mu.Lock()
		left = left && b.relay == nil
		b.mu.Unlock()
		if left {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still on the relay as well")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cross("direct")
	// b's datagrams to a are lost; a's reach b.
	bIP := "203.0.113.1"
	n.dropFrom.Store(&bIP)
	waitRoute(t, a, "relay", 5*time.Second)
	waitRoute(t, b, "relay", 5*time.Second)
	cross("on the relay after a one-way break")
}

// A session that finds a direct route within extrasDelay never asks the
// router for a port.
func TestP2PDirectSessionAsksNoRouterPort(t *testing.T) {
	a, b, _ := natPair(t, false, false, nil)
	var asked atomic.Int32
	stubP2PRouter(t, func(h *p2pRouterHooks) {
		h.mapPort = func(context.Context, int) (*portmap.Mapping, error) {
			asked.Add(1)
			return nil, errors.New("no router in tests")
		}
	})
	a.extrasDelay, b.extrasDelay = 300*time.Millisecond, 300*time.Millisecond
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	time.Sleep(600 * time.Millisecond)
	if n := asked.Load(); n != 0 {
		t.Fatalf("the router was asked %d times by sessions that run direct", n)
	}
}

// Only overlay networks' own ranges and adapters count as a VPN: ordinary
// private ranges and the CGNAT range of phones do not.
func TestP2PVPNClassification(t *testing.T) {
	prev := p2pVPNAdapterNets
	p2pVPNAdapterNets = func() []*net.IPNet { return nil }
	t.Cleanup(func() { p2pVPNAdapterNets = prev })
	for ip, want := range map[string]bool{"25.1.2.3": true, "26.4.5.6": true, "10.147.17.3": true, "fd7a:115c:a1e0::5": true,
		"172.22.0.5": false, "172.25.1.1": false, "100.72.1.2": false, "fd12:3456::1": false, "192.168.1.5": false} {
		if got := p2pVPNAddress(net.ParseIP(ip)); got != want {
			t.Errorf("p2pVPNAddress(%s) = %v, want %v", ip, got, want)
		}
	}
	for name, want := range map[string]bool{"Hamachi": true, "Radmin VPN": true, "tailscale0": true, "wg0": true, "ztc3qdlmvx": true,
		"ZeroTier One [8056c2e21c000001]": true, "tun0": false, "tap0": false, "utun3": false, "wlan0": false, "zt": false, "wgx": false} {
		if got := p2pVPNAdapterName(name); got != want {
			t.Errorf("p2pVPNAdapterName(%q) = %v, want %v", name, got, want)
		}
	}
}

// A pong counts for the route its ping went on, and only when it came back
// on it; a pong of a peer that predates the route tag counts where it came.
func TestP2PMuxPongsCountOnlyOnTheirRoute(t *testing.T) {
	conn := listenLocalUDP(t)
	udp := &p2pUDPPath{label: "direct", conn: conn}
	route := func(port int) p2pRoute {
		return p2pRoute{path: udp, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}
	}
	x, y := route(5101), route(5102)
	m := newP2PMux(conn, udp, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9}, x, p2pMuxHooks{})
	m.addKnown(y)
	pong := func(tag *uint32) []byte {
		msg := make([]byte, 9, 13)
		msg[0] = p2pCtrlPong
		binary.LittleEndian.PutUint64(msg[1:], uint64(time.Since(m.epoch)-5*time.Millisecond))
		if tag != nil {
			msg = binary.LittleEndian.AppendUint32(msg, *tag)
		}
		return msg
	}
	samples := func(r p2pRoute) int {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.statLocked(r).samples
	}
	tagX := p2pRouteTag(x)
	m.handleControl(pong(&tagX), y)
	if samples(y) != 0 || samples(x) != 0 {
		t.Fatalf("a pong of x's ping booked on y: x %d, y %d", samples(x), samples(y))
	}
	m.handleControl(pong(&tagX), x)
	tagY := p2pRouteTag(y)
	m.handleControl(pong(&tagY), y)
	m.handleControl(pong(nil), y)
	if samples(x) != 1 || samples(y) != 2 {
		t.Fatalf("samples x %d, y %d; want 1 and 2", samples(x), samples(y))
	}
}
