package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
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

func TestNakamaP2PStartFailsWithoutAnyAddress(t *testing.T) {
	stubP2PAddresses(t,
		func() ([]net.IP, error) { return nil, errNetlink },
		func() (net.Addr, error) { return nil, errors.New("network is unreachable") })
	p, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Start(context.Background(), nil); !errors.Is(err, errNetlink) {
		t.Fatalf("Start() error = %v, want %v", err, errNetlink)
	}
	if state := p.State(); state != "failed" {
		t.Fatalf("State() = %q after a failed Start, want failed", state)
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
		Magic: p2pMagic, MatchID: "test-match", Token: tok2,
		Candidates: []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: port2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p2.HandleSignal(P2PSignal{
		Magic: p2pMagic, MatchID: "test-match", Token: tok1,
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
