package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

// stubP2PAddresses replaces the interface listing and the route lookup that
// localIPv4Addresses uses until the test ends.
func stubP2PAddresses(t *testing.T, list func() ([]net.IP, error), route func() (net.Addr, error)) {
	t.Helper()
	oldList, oldRoute := p2pInterfaceAddresses, p2pRouteAddress
	t.Cleanup(func() { p2pInterfaceAddresses, p2pRouteAddress = oldList, oldRoute })
	p2pInterfaceAddresses, p2pRouteAddress = list, route
}

// errNetlink is the error net.Interfaces returns on Android 11 and later.
var errNetlink = errors.New("route ip+net: netlinkrib: permission denied")

func TestNakamaP2PLocalIPv4AddressesUsesInterfaces(t *testing.T) {
	routeCalled := false
	stubP2PAddresses(t,
		func() ([]net.IP, error) {
			return []net.IP{net.ParseIP("10.0.0.2").To4(), net.ParseIP("192.168.1.5").To4()}, nil
		},
		func() (net.Addr, error) {
			routeCalled = true
			return &net.UDPAddr{IP: net.ParseIP("172.16.0.9"), Port: 40000}, nil
		})
	got, err := localIPv4Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "10.0.0.2" || got[1].String() != "192.168.1.5" {
		t.Fatalf("localIPv4Addresses() = %v, want [10.0.0.2 192.168.1.5]", got)
	}
	if routeCalled {
		t.Fatal("the route fallback ran although the interfaces had addresses")
	}
}

func TestNakamaP2PLocalIPv4AddressesFallsBackToRoute(t *testing.T) {
	route := func() (net.Addr, error) {
		return &net.UDPAddr{IP: net.ParseIP("100.64.3.7"), Port: 40000}, nil
	}
	for name, list := range map[string]func() ([]net.IP, error){
		"listing fails": func() ([]net.IP, error) { return nil, errNetlink },
		"no addresses":  func() ([]net.IP, error) { return nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			stubP2PAddresses(t, list, route)
			got, err := localIPv4Addresses()
			if err != nil {
				t.Fatalf("localIPv4Addresses() error = %v, want the fallback address", err)
			}
			if len(got) != 1 || got[0].String() != "100.64.3.7" || len(got[0]) != net.IPv4len {
				t.Fatalf("localIPv4Addresses() = %v, want [100.64.3.7]", got)
			}
		})
	}
}

func TestNakamaP2PLocalIPv4AddressesFallbackFails(t *testing.T) {
	routes := map[string]func() (net.Addr, error){
		"route fails": func() (net.Addr, error) { return nil, errors.New("network is unreachable") },
		"loopback":    func() (net.Addr, error) { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, nil },
		"unspecified": func() (net.Addr, error) { return &net.UDPAddr{IP: net.IPv4zero, Port: 1}, nil },
		"IPv6":        func() (net.Addr, error) { return &net.UDPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1}, nil },
		"no address":  func() (net.Addr, error) { return nil, nil },
	}
	for name, route := range routes {
		t.Run(name, func(t *testing.T) {
			// The listing's error is returned unchanged.
			stubP2PAddresses(t, func() ([]net.IP, error) { return nil, errNetlink }, route)
			got, err := localIPv4Addresses()
			if !errors.Is(err, errNetlink) || len(got) != 0 {
				t.Fatalf("localIPv4Addresses() = %v, %v; want no addresses and %v", got, err, errNetlink)
			}
			// A listing without addresses stays empty, without an error.
			stubP2PAddresses(t, func() ([]net.IP, error) { return nil, nil }, route)
			got, err = localIPv4Addresses()
			if err != nil || len(got) != 0 {
				t.Fatalf("localIPv4Addresses() = %v, %v; want no addresses and no error", got, err)
			}
		})
	}
}

func TestNakamaP2PGatherCandidatesWithRouteFallback(t *testing.T) {
	stubP2PAddresses(t,
		func() ([]net.IP, error) { return nil, errNetlink },
		func() (net.Addr, error) { return &net.UDPAddr{IP: net.ParseIP("192.168.43.20"), Port: 40000}, nil })
	p, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	got, err := p.gatherCandidates(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := P2PCandidate{Type: "host", Address: "192.168.43.20", Port: p.LocalPort()}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("gatherCandidates() = %v, want [%v]", got, want)
	}
}

// Without an IPv4 address the handshake still starts: IPv6 and the relay
// may connect the players, and the signal carries the token and key they
// need.
func TestNakamaP2PStartsWithoutAnyIPv4Address(t *testing.T) {
	stubP2PAddresses(t,
		func() ([]net.IP, error) { return nil, errNetlink },
		func() (net.Addr, error) { return nil, errors.New("network is unreachable") })
	signals := make(chan P2PSignal, 4)
	p, err := NewNakamaP2P("test-match", 0, func(s P2PSignal) error { signals <- s; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case s := <-signals:
		if s.Token == "" || s.Key == "" || len(s.Candidates) != 0 {
			t.Fatalf("signal %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no signal")
	}
	if state := p.State(); state != "connecting" {
		t.Fatalf("State() = %q, want connecting", state)
	}
}

// The real route lookup sends nothing; it only needs a default route, which
// a sandbox may not have.
func TestNakamaP2PRouteIPv4Address(t *testing.T) {
	addr, err := routeIPv4Address()
	if err != nil {
		t.Skipf("no route for internet traffic here: %v", err)
	}
	if ip := netAddrIP(addr); ip == nil || ip.To4() == nil {
		t.Fatalf("routeIPv4Address() = %v, want an IPv4 UDP address", addr)
	}
}

func TestNakamaP2PHandleSignalIgnoresOwnToken(t *testing.T) {
	p, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.mu.Lock()
	token := p.token
	p.mu.Unlock()

	if err := p.HandleSignal(P2PSignal{
		Magic:   p2pMagic,
		MatchID: "test-match",
		Token:   token,
		Candidates: []P2PCandidate{{
			Type:    "host",
			Address: "127.0.0.1",
			Port:    p.LocalPort(),
		}},
	}); err != nil {
		t.Fatalf("own signal should be ignored: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteToken != "" || len(p.remote) != 0 {
		t.Fatal("own signal populated remote P2P state")
	}
}

func TestNakamaP2PPunchesBetweenTwoPeers(t *testing.T) {
	p1, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	p1.mu.Lock()
	tok1 := p1.token
	p1.mu.Unlock()
	port1 := p1.LocalPort()
	p2.mu.Lock()
	tok2 := p2.token
	p2.mu.Unlock()
	port2 := p2.LocalPort()

	if err := p1.HandleSignal(P2PSignal{
		Magic: p2pMagic, MatchID: "test-match", Token: tok2, Key: p2.publicKey(),
		Candidates: []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: port2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p2.HandleSignal(P2PSignal{
		Magic: p2pMagic, MatchID: "test-match", Token: tok1, Key: p1.publicKey(),
		Candidates: []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: port1}},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{}, 2)
	go func() { p1.sendPunches(); done <- struct{}{} }()
	go func() { p2.sendPunches(); done <- struct{}{} }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 1 did not finish P2P punch")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 2 did not finish P2P punch")
	}

	select {
	case <-p1.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 1 did not become ready")
	}
	select {
	case <-p2.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 2 did not become ready")
	}

	a1 := p1.RemoteAddr()
	a2 := p2.RemoteAddr()
	if a1 == nil || a2 == nil {
		t.Fatal("P2P handshake did not record both remote addresses")
	}
	if !a1.IP.Equal(net.ParseIP("127.0.0.1")) || a1.Port != port2 {
		t.Fatalf("peer 1 remote=%v, want 127.0.0.1:%d", a1, port2)
	}
	if !a2.IP.Equal(net.ParseIP("127.0.0.1")) || a2.Port != port1 {
		t.Fatalf("peer 2 remote=%v, want 127.0.0.1:%d", a2, port1)
	}
}

// vnet is a simulated internet for the connection ladder: each player's IPv4
// path sends through its NAT (p2pUDPPath.sendHook), which maps and filters
// as a real one does, and what gets through is handed to the other player's
// session as coming from the address the sender's NAT showed. Nothing goes
// through real sockets, so a test chooses the NATs' behaviour exactly.
type vnet struct {
	mu      sync.Mutex
	nats    map[string]*vnat // by public IP
	blocked atomic.Bool      // drop every datagram (the direct routes break)
	// dropFrom, when set, is a public IP whose datagrams are dropped: one
	// direction breaks, the other works.
	dropFrom atomic.Pointer[string]
}

// vnat is one player's NAT. A cone NAT keeps one public port for every
// destination; a symmetric one gives each new destination the next port, by
// step. Both filter as a port-restricted NAT: a datagram gets in only from
// an address the player sent to from that port.
type vnat struct {
	net       *vnet
	ip        net.IP
	symmetric bool
	step      int
	next      int            // the next port a symmetric NAT gives
	port      int            // a cone NAT's port
	byDest    map[string]int // a symmetric NAT's ports, by destination
	allowed   map[string]bool
	p         *NakamaP2P
	inbox     chan vpacket
}

type vpacket struct {
	b    []byte
	from *net.UDPAddr
}

func newVNet() *vnet { return &vnet{nats: map[string]*vnat{}} }

// attach puts p behind a NAT with public address ip, base port port.
func (n *vnet) attach(t *testing.T, p *NakamaP2P, ip string, symmetric bool, port int) *vnat {
	t.Helper()
	x := &vnat{net: n, ip: net.ParseIP(ip).To4(), symmetric: symmetric, step: 1, next: port, port: port,
		byDest: map[string]int{}, allowed: map[string]bool{}, p: p, inbox: make(chan vpacket, 4096)}
	n.mu.Lock()
	n.nats[x.ip.String()] = x
	n.mu.Unlock()
	p.udp4.sendHook = func(b []byte, to *net.UDPAddr) error { x.send(b, to); return nil }
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			select {
			case <-done:
				return
			case pkt := <-x.inbox:
				p.receive(pkt.b, p2pRoute{path: p.udp4, addr: pkt.from})
			}
		}
	}()
	return x
}

// mapping returns the public port the NAT sends to dest from (n.mu held).
func (x *vnat) mapping(dest *net.UDPAddr) int {
	if !x.symmetric {
		return x.port
	}
	key := dest.String()
	if port, ok := x.byDest[key]; ok {
		return port
	}
	port := x.next
	x.next += x.step
	x.byDest[key] = port
	return port
}

// stun returns what two STUN servers would answer, and sets the player's
// srflx candidate and NAT as gatherCandidates would.
func (x *vnat) stun() {
	x.net.mu.Lock()
	var mapped []*net.UDPAddr
	for _, server := range []string{"192.0.2.10:3478", "192.0.2.11:3478"} {
		dest, _ := net.ResolveUDPAddr("udp4", server)
		mapped = append(mapped, &net.UDPAddr{IP: x.ip, Port: x.mapping(dest)})
	}
	x.net.mu.Unlock()
	x.p.mu.Lock()
	x.p.local = []P2PCandidate{{Type: "srflx", Address: x.ip.String(), Port: mapped[len(mapped)-1].Port}}
	x.p.nat = p2pClassifyNAT(mapped)
	x.p.mu.Unlock()
}

func (x *vnat) send(b []byte, to *net.UDPAddr) {
	if x.net.blocked.Load() {
		return
	}
	if d := x.net.dropFrom.Load(); d != nil && *d == x.ip.String() {
		return
	}
	x.net.mu.Lock()
	port := x.mapping(to)
	x.allowed[fmt.Sprintf("%d|%s", port, to)] = true
	dst := x.net.nats[to.IP.To4().String()]
	from := &net.UDPAddr{IP: x.ip, Port: port}
	ok := dst != nil && dst.admits(to.Port, from)
	x.net.mu.Unlock()
	if ok {
		select {
		case dst.inbox <- vpacket{b: append([]byte(nil), b...), from: from}:
		default:
		}
	}
}

// admits reports whether a datagram from 'from' to the NAT's public port gets
// in (n.mu held).
func (x *vnat) admits(port int, from *net.UDPAddr) bool {
	open := !x.symmetric && port == x.port
	if x.symmetric {
		for _, p := range x.byDest {
			if p == port {
				open = true
				break
			}
		}
	}
	return open && x.allowed[fmt.Sprintf("%d|%s", port, from)]
}

// natPair makes two players, a behind a NAT at 198.51.100.1 and b behind one
// at 203.0.113.1, that took each other's signals (STUN done), with short
// ladder steps; hub, when set, is their relay.
func natPair(t *testing.T, aSymmetric, bSymmetric bool, hub *fakeRelayHub) (*NakamaP2P, *NakamaP2P, *vnet) {
	t.Helper()
	noLadderExtras(t)
	n := newVNet()
	var mu sync.Mutex
	peers := map[string]*NakamaP2P{}
	signalTo := func(user string) func(P2PSignal) error {
		return func(s P2PSignal) error {
			mu.Lock()
			p := peers[user]
			mu.Unlock()
			if p != nil {
				go p.HandleSignal(s)
			}
			return nil
		}
	}
	a, err := NewNakamaP2P("nat-match", 0, signalTo("b"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNakamaP2P("nat-match", 0, signalTo("a"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	a.SetPeer("user-a", "user-b")
	b.SetPeer("user-b", "user-a")
	for _, p := range []*NakamaP2P{a, b} {
		p.relayDelay, p.timeout = 200*time.Millisecond, 10*time.Second
		p.muxProbe, p.muxQuiet = 50*time.Millisecond, 600*time.Millisecond
		p.upgradeFast = 100 * time.Millisecond
	}
	if hub != nil {
		a.SetRelay(hub.relayer("user-a"))
		b.SetRelay(hub.relayer("user-b"))
	}
	n.attach(t, a, "198.51.100.1", aSymmetric, 40000).stun()
	n.attach(t, b, "203.0.113.1", bSymmetric, 50000).stun()
	mu.Lock()
	peers["a"], peers["b"] = a, b
	mu.Unlock()
	if err := a.HandleSignal(b.localSignal()); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleSignal(a.localSignal()); err != nil {
		t.Fatal(err)
	}
	return a, b, n
}

// A player behind a NAT that maps each destination to a new port (a mobile
// hotspot, CGNAT) against one behind a port-restricted NAT: the second
// sends hellos to the ports the first's NAT is about to give, and so opens
// its own NAT for the first's hellos. Without the NAT in the signal, the
// prediction does not happen and no direct route works.
func TestP2PPredictsThePortsOfASymmetricNAT(t *testing.T) {
	a, b, _ := natPair(t, true, false, nil)
	if a.nat == nil || a.nat.Type != "symmetric" || a.nat.Delta != 1 || b.nat == nil || b.nat.Type != "cone" {
		t.Fatalf("NATs %+v and %+v", a.nat, b.nat)
	}
	if b.peerNAT == nil || b.peerNAT.Type != "symmetric" {
		t.Fatalf("b took the NAT %+v from a's signal", b.peerNAT)
	}
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.RouteName() != "direct-srflx" || b.RouteName() != "direct-predicted" {
		t.Fatalf("route names %q and %q, want direct-srflx and direct-predicted", a.RouteName(), b.RouteName())
	}
	exchangeStreams(t, a, b)

	t.Run("without prediction", func(t *testing.T) {
		a, b, _ := natPair(t, true, false, nil)
		// a's game does not know its NAT, so its signals do not say.
		a.mu.Lock()
		a.nat = nil
		a.mu.Unlock()
		b.mu.Lock()
		b.peerNAT = nil
		b.mu.Unlock()
		a.timeout, b.timeout = time.Second, time.Second
		go a.punchLoop()
		go b.punchLoop()
		select {
		case <-a.ready:
			t.Fatal("connected through a symmetric NAT without prediction: the simulated NATs do not filter")
		case <-b.ready:
			t.Fatal("connected through a symmetric NAT without prediction: the simulated NATs do not filter")
		case <-a.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("did not give up")
		}
	})
}

// Two cone NATs connect directly, at the STUN addresses.
func TestP2PConnectsThroughTwoConeNATs(t *testing.T) {
	a, b, _ := natPair(t, false, false, nil)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.RouteName() != "direct-srflx" || b.RouteName() != "direct-srflx" || a.Upgrades() != 0 {
		t.Fatalf("route names %q and %q, %d upgrades", a.RouteName(), b.RouteName(), a.Upgrades())
	}
}

// ggpoPair opens a GGPO channel on both sessions and returns a check that
// a datagram crosses them both ways.
func ggpoPair(t *testing.T, a, b *NakamaP2P) func(msg string) {
	t.Helper()
	ga, toB, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	gb, toA, err := b.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ga.Close(); gb.Close() })
	return func(msg string) {
		t.Helper()
		for _, dir := range []struct {
			from, to net.PacketConn
			addr     net.Addr
		}{{ga, gb, toB}, {gb, ga, toA}} {
			// GGPO resends; a datagram lost on a route that just broke is
			// sent again.
			got := false
			buf := make([]byte, 64)
			for try := 0; try < 20 && !got; try++ {
				if _, err := dir.from.WriteTo([]byte(msg), dir.addr); err != nil {
					t.Fatal(err)
				}
				_ = dir.to.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
				for {
					n, _, err := dir.to.ReadFrom(buf)
					if err != nil {
						break
					}
					if string(buf[:n]) == msg {
						got = true
						break
					}
				}
			}
			if !got {
				t.Fatalf("GGPO datagram %q did not cross (routes %s, %s)", msg, a.RouteName(), b.RouteName())
			}
		}
	}
}

// The direct routes do not work at first, so the session starts on the
// relay; a match runs on it. Once the direct routes work, the session
// moves to one mid-match, on the same GGPO channels, and leaves the relay a
// while later.
func TestP2PUpgradesFromTheRelayMidMatch(t *testing.T) {
	hub := newFakeRelayHub()
	a, b, n := natPair(t, false, false, hub)
	a.relayLinger, b.relayLinger = 300*time.Millisecond, 300*time.Millisecond
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.RouteName() != "relay-ws" || b.RouteName() != "relay-ws" {
		t.Fatalf("route names %q and %q, want the relay", a.RouteName(), b.RouteName())
	}
	cross := ggpoPair(t, a, b)
	cross("on the relay")
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
	waitRoute(t, b, "direct", 5*time.Second)
	if a.RouteName() != "direct-srflx" || a.Upgrades() != 1 || b.Upgrades() != 1 {
		t.Fatalf("route name %q, upgrades %d and %d", a.RouteName(), a.Upgrades(), b.Upgrades())
	}
	cross("after the upgrade")
	// The relay is left once no game data went over it for a while.
	deadline := time.Now().Add(p2pRelayDataQuiet + 3*time.Second)
	for {
		a.mu.Lock()
		left := a.relay == nil
		a.mu.Unlock()
		if left {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("still on the relay as well")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cross("after leaving the relay")
}

// The direct route goes quiet mid-match: the session falls back to the
// relay, which it kept open, and moves back once the direct route works
// again.
func TestP2PFallsBackToTheRelayWhenTheDirectRouteGoesQuiet(t *testing.T) {
	hub := newFakeRelayHub()
	a, b, n := natPair(t, false, false, hub)
	a.relayLinger, b.relayLinger = time.Minute, time.Minute
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	cross := ggpoPair(t, a, b)
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
	waitRoute(t, b, "direct", 5*time.Second)
	cross("direct")
	n.blocked.Store(true)
	waitRoute(t, a, "relay", 3*time.Second)
	waitRoute(t, b, "relay", 3*time.Second)
	cross("back on the relay")
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
	waitRoute(t, b, "direct", 5*time.Second)
	cross("direct again")
	if a.Upgrades() != 2 {
		t.Fatalf("%d upgrades, want 2 (the fall back to the relay does not count)", a.Upgrades())
	}
}

// A session that left the relay, and whose direct route then goes quiet
// with no other route working, joins the relay again.
func TestP2PJoinsTheRelayAgainWhenTheDirectRouteIsLost(t *testing.T) {
	hub := newFakeRelayHub()
	a, b, n := natPair(t, false, false, hub)
	a.relayLinger, b.relayLinger = 100*time.Millisecond, 100*time.Millisecond
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
	waitRoute(t, b, "direct", 5*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		left := a.relay == nil && !a.relayJoining
		a.mu.Unlock()
		if left {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay was not left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	hub.mu.Lock()
	joins := len(hub.joins)
	hub.mu.Unlock()
	n.blocked.Store(true)
	waitRoute(t, a, "relay", 5*time.Second)
	waitRoute(t, b, "relay", 5*time.Second)
	hub.mu.Lock()
	rejoins := len(hub.joins) - joins
	hub.mu.Unlock()
	if rejoins != 2 {
		t.Fatalf("%d joins after the direct route was lost, want both players again", rejoins)
	}
	ggpoPair(t, a, b)("on the relay again")
}

// The relay's timer counts from the peer's keys, not from this side's
// start; IPv6 is tried from the start, and the router's port only once no
// direct route worked within extrasDelay.
func TestP2PLadderTimersCountFromThePeersKeys(t *testing.T) {
	noLadderExtras(t)
	var mu sync.Mutex
	var ipv6At, mapAt time.Time
	stubP2PRouter(t, func(h *p2pRouterHooks) {
		h.ipv6 = func() ([]net.IP, error) {
			mu.Lock()
			ipv6At = time.Now()
			mu.Unlock()
			return nil, nil
		}
		h.mapPort = func(context.Context, int) (*portmap.Mapping, error) {
			mu.Lock()
			mapAt = time.Now()
			mu.Unlock()
			return nil, errors.New("no router in tests")
		}
	})
	hub := newFakeRelayHub()
	p, err := NewNakamaP2P("timer-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("user-a", "user-b")
	p.SetRelay(hub.relayer("user-a"))
	p.relayDelay = 200 * time.Millisecond
	p.extrasDelay = 300 * time.Millisecond
	start := time.Now()
	go p.punchLoop()
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	ipv6, mapped := ipv6At, mapAt
	mu.Unlock()
	if ipv6.IsZero() || ipv6.Sub(start) > 100*time.Millisecond {
		t.Fatalf("IPv6 after %v; want it from the start", ipv6.Sub(start))
	}
	if mapped.IsZero() || mapped.Sub(start) < p.extrasDelay || mapped.Sub(start) > p.extrasDelay+200*time.Millisecond {
		t.Fatalf("router asked after %v; want it after the extras delay %v", mapped.Sub(start), p.extrasDelay)
	}
	hub.mu.Lock()
	early := len(hub.joins)
	hub.mu.Unlock()
	if early != 0 || p.Stage() == "relay" {
		t.Fatalf("%d relay joins (stage %s) before the peer's keys", early, p.Stage())
	}
	keysAt := time.Now()
	if err := p.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "timer-match", Token: "peer-token", Key: testP2PKey(t), From: "user-b", To: "user-a",
		Candidates: []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		hub.mu.Lock()
		joined := len(hub.joins)
		hub.mu.Unlock()
		if joined > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the relay was not joined after the peer's keys")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d := time.Since(keysAt); d < p.relayDelay {
		t.Fatalf("relay joined %v after the keys, before its delay %v", d, p.relayDelay)
	}
}

// fakeUDPRelay stands in for the server's UDP relay: the two paths dialled
// with tickets of one session deliver to each other.
type fakeUDPRelay struct {
	mu       sync.Mutex
	paths    map[string][]*fakeUDPRelayPath
	tickets  int
	refuseWS bool
}

type fakeUDPRelayPath struct {
	relay    *fakeUDPRelay
	session  string
	crypt    atomic.Pointer[atomic.Pointer[p2pCrypt]]
	onPacket func([]byte)
	closed   atomic.Bool
}

func (f *fakeUDPRelayPath) name() string                         { return "relay-udp" }
func (f *fakeUDPRelayPath) setCrypt(c *atomic.Pointer[p2pCrypt]) { f.crypt.Store(c) }
func (f *fakeUDPRelayPath) routeAddr() net.Addr                  { return p2pUDPRelayAddr{session: f.session} }
func (f *fakeUDPRelayPath) close()                               { f.closed.Store(true) }
func (f *fakeUDPRelayPath) write(b []byte, _ net.Addr) error {
	if f.closed.Load() {
		return net.ErrClosed
	}
	sealed, err := sealFor(f.crypt.Load(), b)
	if err != nil {
		return err
	}
	f.relay.mu.Lock()
	others := append([]*fakeUDPRelayPath(nil), f.relay.paths[f.session]...)
	f.relay.mu.Unlock()
	for _, o := range others {
		if o != f && !o.closed.Load() {
			go o.onPacket(sealed)
		}
	}
	return nil
}

// relayer is a p2pRelayer of hub that also hands out UDP relay tickets.
type fakeUDPRelayer struct {
	fakeRelayer
	udp *fakeUDPRelay
}

func (r fakeUDPRelayer) RequestUDPRelay(matchID, peer string) (udpRelayTicket, error) {
	r.udp.mu.Lock()
	r.udp.tickets++
	r.udp.mu.Unlock()
	return udpRelayTicket{Addr: "192.0.2.50:17360", Token: r.user, Expires: time.Now().Add(time.Minute).Unix(), Peer: peer, Session: matchID}, nil
}

// The ladder tries the server's UDP relay before the websocket one, and a
// session that works on it never joins the websocket relay.
func TestP2PLadderPrefersTheUDPRelay(t *testing.T) {
	udp := &fakeUDPRelay{paths: map[string][]*fakeUDPRelayPath{}}
	prevDial := p2pDialUDPRelay
	p2pDialUDPRelay = func(ticket udpRelayTicket, onPacket func([]byte)) (p2pPath, error) {
		f := &fakeUDPRelayPath{relay: udp, session: ticket.Session, onPacket: onPacket}
		udp.mu.Lock()
		udp.paths[ticket.Session] = append(udp.paths[ticket.Session], f)
		udp.mu.Unlock()
		return f, nil
	}
	t.Cleanup(func() { p2pDialUDPRelay = prevDial })
	hub := newFakeRelayHub()
	a, b, n := natPair(t, false, false, nil)
	a.SetRelay(fakeUDPRelayer{fakeRelayer{hub, "user-a"}, udp})
	b.SetRelay(fakeUDPRelayer{fakeRelayer{hub, "user-b"}, udp})
	for _, p := range []*NakamaP2P{a, b} {
		p.mu.Lock()
		p.peerServerRelay = true
		p.mu.Unlock()
	}
	n.blocked.Store(true)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.RouteName() != "relay-udp" || b.RouteName() != "relay-udp" || a.Route() != "relay-udp" {
		t.Fatalf("route names %q and %q, want the UDP relay", a.RouteName(), b.RouteName())
	}
	exchangeStreams(t, a, b)
	time.Sleep(p2pUDPRelayGrace + 500*time.Millisecond)
	hub.mu.Lock()
	ws := len(hub.joins)
	hub.mu.Unlock()
	if ws != 0 || udp.tickets != 2 {
		t.Fatalf("%d websocket relay joins, %d UDP tickets; want only the UDP relay", ws, udp.tickets)
	}
	// It ranks below a direct route: the session moves off it.
	n.blocked.Store(false)
	waitRoute(t, a, "direct", 5*time.Second)
}

// Routes are ranked by what their pongs measure: a direct route takes over
// from the relay after a few pongs in a row, unless it is much slower; of
// two direct routes, the clearly faster one wins.
func TestP2PMuxRanksRoutesByRoundTrip(t *testing.T) {
	conn := listenLocalUDP(t)
	udp := &p2pUDPPath{label: "direct", conn: conn}
	relay := p2pRoute{path: &p2pRelayPath{}, addr: p2pRelayAddr{peer: "b"}}
	route := func(port int) p2pRoute {
		return p2pRoute{path: udp, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}}
	}
	var switches [][2]p2pRoute
	m := newP2PMux(conn, udp, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9}, relay, p2pMuxHooks{
		switched: func(from, to p2pRoute) { switches = append(switches, [2]p2pRoute{from, to}) },
	})
	slow, fast := route(5001), route(5002)
	m.addKnown(slow)
	m.addKnown(fast)
	for i := 0; i < 4; i++ {
		m.recordPong(relay, 40*time.Millisecond)
	}
	// Much slower than the relay: no upgrade.
	for i := 0; i < 4; i++ {
		m.recordPong(slow, 200*time.Millisecond)
	}
	if !m.activeRoute().equal(relay) {
		t.Fatalf("moved to %v, much slower than the relay", m.activeRoute())
	}
	m.recordPong(fast, 30*time.Millisecond)
	m.recordPong(fast, 30*time.Millisecond)
	if !m.activeRoute().equal(relay) {
		t.Fatal("moved after two pongs")
	}
	m.recordPong(fast, 30*time.Millisecond)
	if !m.activeRoute().equal(fast) || len(switches) != 1 || !switches[0][0].equal(relay) {
		t.Fatalf("active %v after three pongs, switches %v", m.activeRoute(), switches)
	}
	// The same rank: only a clearly faster route, and not right after a
	// change.
	m.mu.Lock()
	m.lastSwitch = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	lan := route(5003)
	m.addKnown(lan)
	for i := 0; i < 2*p2pFasterSamples; i++ {
		m.recordPong(fast, 30*time.Millisecond)
		m.recordPong(lan, 25*time.Millisecond)
	}
	if !m.activeRoute().equal(fast) {
		t.Fatalf("moved to %v, faster by only 5ms", m.activeRoute())
	}
	lan2 := route(5004)
	m.addKnown(lan2)
	for i := 0; i < 2*p2pFasterSamples; i++ {
		m.recordPong(lan2, 2*time.Millisecond)
	}
	if !m.activeRoute().equal(lan2) || m.switchCount() != 2 {
		t.Fatalf("active %v, %d switches; want the much faster route", m.activeRoute(), m.switchCount())
	}
}

// fakeSTUN answers binding requests with a mapped address of its choosing.
func fakeSTUN(t *testing.T, port int) *net.UDPAddr {
	t.Helper()
	c := listenLocalUDP(t)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 20 {
				continue
			}
			resp := make([]byte, 32)
			binary.BigEndian.PutUint16(resp[0:2], stunBindingSuccess)
			binary.BigEndian.PutUint16(resp[2:4], 12)
			copy(resp[4:20], buf[4:20])
			binary.BigEndian.PutUint16(resp[20:22], stunXORMappedAddress)
			binary.BigEndian.PutUint16(resp[22:24], 8)
			resp[25] = 0x01
			binary.BigEndian.PutUint16(resp[26:28], uint16(port)^uint16(stunMagicCookie>>16))
			binary.BigEndian.PutUint32(resp[28:32], binary.BigEndian.Uint32([]byte{203, 0, 113, 7})^stunMagicCookie)
			_, _ = c.WriteToUDP(resp, from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr)
}

// Every STUN server is asked, and their answers classify the NAT.
func TestP2PGatherCandidatesClassifiesTheNAT(t *testing.T) {
	stubP2PAddresses(t, func() ([]net.IP, error) { return nil, nil }, func() (net.Addr, error) { return nil, errors.New("no route") })
	for _, tc := range []struct {
		ports []int
		want  P2PNATInfo
	}{
		{[]int{40000, 40000}, P2PNATInfo{Type: "cone", IP: "203.0.113.7", Port: 40000}},
		{[]int{40000, 40002}, P2PNATInfo{Type: "symmetric", IP: "203.0.113.7", Port: 40002, Delta: 2}},
	} {
		p, err := NewNakamaP2P("stun-match", 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		servers := []string{fakeSTUN(t, tc.ports[0]).String(), fakeSTUN(t, tc.ports[1]).String()}
		local, err := p.gatherCandidates(context.Background(), servers)
		p.Close()
		if err != nil {
			t.Fatal(err)
		}
		srflx := 0
		for _, c := range local {
			if c.Type == "srflx" {
				srflx++
			}
		}
		if p.nat == nil || *p.nat != tc.want || srflx != len(uniqueInts(tc.ports)) {
			t.Fatalf("ports %v: NAT %+v, candidates %v; want %+v", tc.ports, p.nat, local, tc.want)
		}
		if s := p.localSignal(); s.NAT == nil || *s.NAT != tc.want {
			t.Fatalf("the signal carries the NAT %+v", s.NAT)
		}
	}
}

func uniqueInts(in []int) map[int]bool {
	out := map[int]bool{}
	for _, v := range in {
		out[v] = true
	}
	return out
}

// Routes are named by how they reach the peer.
func TestP2PRouteNames(t *testing.T) {
	a, _ := signedPair(t)
	a.remote = []P2PCandidate{
		{Type: "host", Address: "192.168.1.20", Port: 7000},
		{Type: "srflx", Address: "203.0.113.5", Port: 7001},
		{Type: "upnp", Address: "203.0.113.5", Port: 7002},
		{Type: "host6", Address: "2001:db8::5", Port: 7003},
	}
	a.predicted[p2pAddrKey(&net.UDPAddr{IP: net.ParseIP("203.0.113.5"), Port: 7010})] = true
	udp := func(ip string, port int) p2pRoute {
		return p2pRoute{path: a.udp4, addr: &net.UDPAddr{IP: net.ParseIP(ip), Port: port}}
	}
	for want, r := range map[string]p2pRoute{
		"direct-host":      udp("192.168.1.20", 7000),
		"direct-srflx":     udp("203.0.113.5", 7001),
		"direct-router":    udp("203.0.113.5", 7002),
		"direct-ipv6":      udp("2001:db8::5", 7003),
		"direct-predicted": udp("203.0.113.5", 7010),
		"direct-prflx":     udp("203.0.113.5", 7011),
		"direct-vpn":       udp("26.12.0.4", 7000),
		"relay-ws":         {path: &p2pRelayPath{}, addr: p2pRelayAddr{peer: "b"}},
		"relay-udp":        {path: &fakeUDPRelayPath{}, addr: p2pUDPRelayAddr{session: "s"}},
	} {
		if got := a.classifyRoute(r); got != want {
			t.Errorf("%v named %q, want %q", r, got, want)
		}
	}
	if a.RouteName() != "" || a.Upgrades() != 0 {
		t.Fatalf("route name %q before the handshake", a.RouteName())
	}
}
