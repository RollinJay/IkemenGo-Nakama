package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
	lua "github.com/yuin/gopher-lua"
)

// The connection ladder (NakamaP2P.punchLoop): when the IPv4 path does not
// work, the handshake also tries IPv6 and a port the router opens, and then
// the relay through the server.

// fakeRelayHub stands in for the Nakama server's relayed matches: members
// that join the same name receive each other's datagrams, in order, on a
// goroutine of their own as the client's read loop would deliver them.
type fakeRelayHub struct {
	mu      sync.Mutex
	members map[string][]*fakeRelayMember
	joins   []string
}

type fakeRelayMember struct {
	hub     *fakeRelayHub
	name    string
	user    string
	queue   chan []byte
	closed  chan struct{}
	closeMu sync.Once
}

func newFakeRelayHub() *fakeRelayHub {
	return &fakeRelayHub{members: map[string][]*fakeRelayMember{}}
}

// relayer returns the p2pRelayer of one player of the hub.
func (h *fakeRelayHub) relayer(user string) p2pRelayer { return fakeRelayer{h, user} }

type fakeRelayer struct {
	hub  *fakeRelayHub
	user string
}

func (r fakeRelayer) JoinRelay(name, peer string, deliver func([]byte)) (p2pRelayLink, error) {
	m := &fakeRelayMember{hub: r.hub, name: name, user: r.user, queue: make(chan []byte, 256), closed: make(chan struct{})}
	r.hub.mu.Lock()
	r.hub.members[name] = append(r.hub.members[name], m)
	r.hub.joins = append(r.hub.joins, r.user+"@"+name)
	r.hub.mu.Unlock()
	go func() {
		for {
			select {
			case b := <-m.queue:
				deliver(b)
			case <-m.closed:
				return
			}
		}
	}()
	return m, nil
}

func (m *fakeRelayMember) Send(b []byte) error {
	m.hub.mu.Lock()
	others := append([]*fakeRelayMember(nil), m.hub.members[m.name]...)
	m.hub.mu.Unlock()
	for _, o := range others {
		if o == m {
			continue
		}
		select {
		case o.queue <- append([]byte(nil), b...):
		default:
		}
	}
	return nil
}

func (m *fakeRelayMember) Close() error {
	m.closeMu.Do(func() { close(m.closed) })
	return nil
}

// noLadderExtras keeps a test's handshake off the machine's real IPv6
// addresses and router.
func noLadderExtras(t *testing.T) {
	t.Helper()
	prevIPv6, prevMap := p2pIPv6Addresses, p2pMapPort
	p2pIPv6Addresses = func() ([]net.IP, error) { return nil, nil }
	p2pMapPort = func(context.Context, int) (*portmap.Mapping, error) { return nil, errors.New("no router in tests") }
	t.Cleanup(func() { p2pIPv6Addresses, p2pMapPort = prevIPv6, prevMap })
}

// deadUDPPort is a localhost port nothing listens on: datagrams to it are
// dropped, as a NAT drops what it was not expecting.
func deadUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}

// ladderPair makes two P2P objects whose signals reach each other, as the
// Nakama match passes them on, with short ladder steps. Each first knows only
// a dead IPv4 candidate of the other, so the direct path never works.
func ladderPair(t *testing.T, hub *fakeRelayHub) (*NakamaP2P, *NakamaP2P) {
	t.Helper()
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
	a, err := NewNakamaP2P("ladder-match", 0, signalTo("b"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNakamaP2P("ladder-match", 0, signalTo("a"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	mu.Lock()
	peers["a"], peers["b"] = a, b
	mu.Unlock()
	for _, p := range []*NakamaP2P{a, b} {
		p.extrasDelay, p.relayDelay, p.timeout = 150*time.Millisecond, 400*time.Millisecond, 10*time.Second
	}
	a.SetPeer("user-a", "user-b")
	b.SetPeer("user-b", "user-a")
	if hub != nil {
		a.SetRelay(hub.relayer("user-a"))
		b.SetRelay(hub.relayer("user-b"))
	}
	dead := []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}
	if err := a.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "ladder-match", Token: b.token, Key: b.publicKey(), Candidates: dead, From: "user-b", To: "user-a"}); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "ladder-match", Token: a.token, Key: a.publicKey(), Candidates: dead, From: "user-a", To: "user-b"}); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func waitReady(t *testing.T, ps ...*NakamaP2P) {
	t.Helper()
	for _, p := range ps {
		select {
		case <-p.ready:
		case <-p.closed:
			t.Fatal("the P2P handshake gave up")
		case <-time.After(8 * time.Second):
			t.Fatal("the P2P handshake did not complete")
		}
	}
}

// exchangeStreams checks that the netplay stream carries data both ways.
func exchangeStreams(t *testing.T, a, b *NakamaP2P) {
	t.Helper()
	sa, sb := openStreams(t, a, b)
	for _, dir := range []struct {
		from, to net.Conn
		msg      string
	}{{sa, sb, "hello from a"}, {sb, sa, "hello from b"}} {
		if _, err := dir.from.Write([]byte(dir.msg)); err != nil {
			t.Fatal(err)
		}
		_ = dir.to.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, len(dir.msg))
		if _, err := ioReadFull(dir.to, buf); err != nil || string(buf) != dir.msg {
			t.Fatalf("stream read %q, %v; want %q", buf, err, dir.msg)
		}
	}
}

func ioReadFull(c net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func TestP2PLadderFallsBackToTheRelay(t *testing.T) {
	noLadderExtras(t)
	hub := newFakeRelayHub()
	a, b := ladderPair(t, hub)
	start := time.Now()
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if d := time.Since(start); d < 400*time.Millisecond {
		t.Fatalf("connected after %v, before the relay step", d)
	}
	if a.Route() != "relay" || b.Route() != "relay" {
		t.Fatalf("routes %q and %q, want relay", a.Route(), b.Route())
	}
	if a.Stage() != "relay" {
		t.Fatalf("stage %q", a.Stage())
	}
	hub.mu.Lock()
	joins := append([]string(nil), hub.joins...)
	hub.mu.Unlock()
	name := a.keys.relayName
	if name != b.keys.relayName {
		t.Fatalf("the players derived relay names %q and %q", name, b.keys.relayName)
	}
	if len(joins) != 2 || !strings.HasSuffix(joins[0], "@"+name) || !strings.HasSuffix(joins[1], "@"+name) {
		t.Fatalf("relay joins %v, want both players in %s", joins, name)
	}
	// The rollback session still gets an IPv4 peer address.
	if ra := a.RemoteAddr(); ra == nil || ra.IP.To4() == nil {
		t.Fatalf("remote address %v", ra)
	}
	exchangeStreams(t, a, b)

	// A GGPO channel works over the relay too.
	ga, toB, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()
	gb, _, err := b.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	defer gb.Close()
	if _, err := ga.WriteTo([]byte("inputs"), toB); err != nil {
		t.Fatal(err)
	}
	_ = gb.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, _, err := gb.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "inputs" {
		t.Fatalf("GGPO channel read %q, %v", buf[:n], err)
	}
}

func TestP2PLadderGivesUpWithoutARelay(t *testing.T) {
	noLadderExtras(t)
	a, b := ladderPair(t, nil)
	for _, p := range []*NakamaP2P{a, b} {
		p.timeout = 900 * time.Millisecond
	}
	start := time.Now()
	go a.punchLoop()
	go b.punchLoop()
	select {
	case <-a.closed:
	case <-a.ready:
		t.Fatal("connected over a dead path")
	case <-time.After(5 * time.Second):
		t.Fatal("the handshake did not give up at its timeout")
	}
	if d := time.Since(start); d < a.timeout {
		t.Fatalf("gave up after %v, before its timeout %v", d, a.timeout)
	}
	if a.State() != "failed" {
		t.Fatalf("state %q", a.State())
	}
}

// A game that completed the handshake, while the peer's game never sends a
// first datagram, gives up: not before the grace that leaves the peer time
// to complete it too, nor long after.
func TestP2PGivesUpWhenThePeerNeverConfirms(t *testing.T) {
	a, b := signedPair(t)
	peer := listenLocalUDP(t)
	a.remote = []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: peer.LocalAddr().(*net.UDPAddr).Port}}
	a.interval, a.timeout, a.confirmGrace = 20*time.Millisecond, 300*time.Millisecond, time.Second
	start := time.Now()
	go a.punchLoop()
	// The peer acks the hellos and nothing else.
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := peer.ReadFromUDP(buf)
			if err != nil {
				return
			}
			data, ok := b.openDatagram(buf[:n])
			var p p2PPacket
			if ok && json.Unmarshal(data, &p) == nil && p.Kind == p2pHello {
				if ack, err := sealFor(&b.crypt, b.handshakePacket(p2pAck, p.Nonce)); err == nil {
					_, _ = peer.WriteToUDP(ack, from)
				}
			}
		}
	}()
	select {
	case <-a.ready:
	case <-time.After(a.timeout):
		t.Fatal("the handshake did not complete")
	}
	select {
	case <-a.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("still open after the grace")
	}
	if d := time.Since(start); d < a.confirmGrace {
		t.Fatalf("gave up after %v, within the grace %v", d, a.confirmGrace)
	}
}

func TestP2PLadderConnectsOverIPv6(t *testing.T) {
	probe, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	probe.Close()
	noLadderExtras(t)
	// Stand-ins for global addresses: the loopback reaches the other peer.
	p2pIPv6Addresses = func() ([]net.IP, error) { return []net.IP{net.IPv6loopback}, nil }
	a, b := ladderPair(t, nil)
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.Route() != "ipv6" || b.Route() != "ipv6" {
		t.Fatalf("routes %q and %q, want ipv6", a.Route(), b.Route())
	}
	if ra := a.RemoteAddr(); ra == nil || ra.IP.To4() == nil {
		t.Fatalf("remote address %v, want an IPv4 stand-in", ra)
	}
	exchangeStreams(t, a, b)
}

// Player A is behind a NAT that gives each destination a port of its own,
// so no address A signalled reaches it, while A reaches B. B must check the
// address A's packets come from, and A, which completes first, must keep
// answering until B has too.
func TestP2PLadderConnectsWhenOnlyOneSideCanReachTheOther(t *testing.T) {
	noLadderExtras(t)
	a, b := ladderPair(t, nil)
	a.local = []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}
	b.local = []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: b.LocalPort()}}
	if err := a.HandleSignal(b.localSignal()); err != nil {
		t.Fatal(err)
	}
	go a.punchLoop()
	go b.punchLoop()
	waitReady(t, a, b)
	if a.Route() != "direct" || b.Route() != "direct" {
		t.Fatalf("routes %q and %q, want direct", a.Route(), b.Route())
	}
	exchangeStreams(t, a, b)
}

func TestP2PLadderOffersTheRouterPort(t *testing.T) {
	noLadderExtras(t)
	var mapped sync.WaitGroup
	mapped.Add(1)
	p2pMapPort = func(_ context.Context, port int) (*portmap.Mapping, error) {
		defer mapped.Done()
		return &portmap.Mapping{Method: "upnp", ExternalIP: net.ParseIP("203.0.113.9"), ExternalPort: port + 1, InternalPort: port}, nil
	}
	signals := make(chan P2PSignal, 16)
	p, err := NewNakamaP2P("map-match", 0, func(s P2PSignal) error { signals <- s; return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.extrasDelay, p.relayDelay = 50*time.Millisecond, time.Minute
	p.local = []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: p.LocalPort()}}
	if err := p.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "map-match", Token: "peer-token", Key: testP2PKey(t),
		Candidates: []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}}); err != nil {
		t.Fatal(err)
	}
	go p.punchLoop()
	want := P2PCandidate{Type: "upnp", Address: "203.0.113.9", Port: p.LocalPort() + 1}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case s := <-signals:
			for _, c := range s.Candidates {
				if c == want {
					mapped.Wait()
					return
				}
			}
		case <-deadline:
			t.Fatalf("no signal offered the router's port %v", want)
		}
	}
}

func TestP2PMuxPrefersAUDPRouteOverTheRelay(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	udp := &p2pUDPPath{label: "direct", conn: conn}
	relayRoute := p2pRoute{path: &p2pRelayPath{}, addr: p2pRelayAddr{peer: "b"}}
	m := newP2PMux(conn, udp, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 9}, relayRoute, p2pMuxHooks{})
	if !m.activeRoute().equal(relayRoute) {
		t.Fatalf("active %v", m.activeRoute())
	}
	direct := p2pRoute{path: udp, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5555}}
	m.addKnown(direct)
	if !m.activeRoute().equal(relayRoute) || !m.isKnown(direct) {
		t.Fatalf("active %v; a known route is accepted, but not sent on", m.activeRoute())
	}
	m.prefer(direct)
	if !m.activeRoute().equal(direct) {
		t.Fatalf("active %v, want the UDP route", m.activeRoute())
	}
	m.prefer(relayRoute)
	if !m.activeRoute().equal(direct) || !m.isKnown(relayRoute) || !m.isKnown(direct) {
		t.Fatalf("active %v; the relay must not take over again, and both stay accepted", m.activeRoute())
	}
	for i := 0; i < 2*p2pMaxRoutes; i++ {
		m.addKnown(p2pRoute{path: udp, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6000 + i}})
	}
	if len(m.known) > p2pMaxRoutes {
		t.Fatalf("%d known routes", len(m.known))
	}
}

// testP2PKey is the public key of a player the test stands in for.
func testP2PKey(t *testing.T) string {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k.PublicKey().Bytes())
}

// signedPair makes two P2P objects that took each other's signals, without
// starting a handshake.
func signedPair(t *testing.T) (*NakamaP2P, *NakamaP2P) {
	t.Helper()
	a, err := NewNakamaP2P("keys-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewNakamaP2P("keys-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)
	if err := a.HandleSignal(b.localSignal()); err != nil {
		t.Fatal(err)
	}
	if err := b.HandleSignal(a.localSignal()); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestP2PKeysAgree(t *testing.T) {
	a, b := signedPair(t)
	ka, kb := a.keys, b.keys
	if !bytes.Equal(ka.handshake, kb.handshake) || ka.relayName != kb.relayName {
		t.Fatal("the two players derived different keys")
	}
	if !bytes.Equal(ka.seal, kb.open) || !bytes.Equal(kb.seal, ka.open) || bytes.Equal(ka.seal, ka.open) {
		t.Fatal("each player's seal key must be the other's open key, and differ from its own")
	}
	if !strings.HasPrefix(ka.relayName, "ikemen-p2p-relay-") || strings.Contains(ka.relayName, a.token) {
		t.Fatalf("relay name %q", ka.relayName)
	}
	// Someone who saw both signals has both tokens and both public keys, but
	// no private key: whatever key pair they use, they derive other keys.
	c, err := NewNakamaP2P("keys-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	secret, err := p2pAgree(c.key, b.publicKey())
	if err != nil {
		t.Fatal(err)
	}
	kc := p2pDeriveKeys(secret, "keys-match", a.token, b.token)
	if bytes.Equal(kc.handshake, ka.handshake) || kc.relayName == ka.relayName {
		t.Fatal("keys did not depend on the exchange's secret")
	}
	if err := a.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "keys-match", Token: "t", Key: "not hex"}); err == nil {
		t.Fatal("a signal without a valid key was taken")
	}
	if err := a.HandleSignal(P2PSignal{Magic: p2pMagic, MatchID: "keys-match", Token: "t", Key: strings.Repeat("00", 32)}); err == nil {
		t.Fatal("a signal with a low-order key was taken")
	}
}

// sealAs seals data as from's game sends it.
func sealAs(t *testing.T, from *NakamaP2P, data []byte) []byte {
	t.Helper()
	b, err := sealFor(&from.crypt, data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readPackets returns the handshake packets that arrive at sock for a short
// while, opened as to's game opens them.
func readPackets(t *testing.T, sock *net.UDPConn, to *NakamaP2P) []p2PPacket {
	t.Helper()
	var out []p2PPacket
	buf := make([]byte, 2048)
	_ = sock.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		n, _, err := sock.ReadFromUDP(buf)
		if err != nil {
			return out
		}
		data, ok := to.openDatagram(buf[:n])
		if !ok {
			t.Fatalf("a datagram did not open: %x", buf[:n])
		}
		var p p2PPacket
		if json.Unmarshal(data, &p) == nil {
			out = append(out, p)
		}
	}
}

func listenLocalUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestP2PDatagramsAreSealed(t *testing.T) {
	a, b := signedPair(t)
	peer := listenLocalUDP(t)
	route := p2pRoute{path: a.udp4, addr: peer.LocalAddr()}
	for i := 0; i < 2; i++ {
		if err := route.send([]byte("inputs")); err != nil {
			t.Fatal(err)
		}
	}
	var sent [][]byte
	buf := make([]byte, 2048)
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	for len(sent) < 2 {
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, append([]byte(nil), buf[:n]...))
	}
	if bytes.Contains(sent[0], []byte("inputs")) || bytes.Equal(sent[0], sent[1]) {
		t.Fatalf("sent %x; want two different sealed datagrams", sent)
	}
	if got, ok := b.openDatagram(sent[0]); !ok || string(got) != "inputs" {
		t.Fatalf("the peer opened %q, %v", got, ok)
	}
	if _, ok := a.openDatagram(sent[0]); ok {
		t.Fatal("a datagram sent back to its sender opened")
	}
	tampered := append([]byte(nil), sent[1]...)
	tampered[len(tampered)-1] ^= 1
	if _, ok := b.openDatagram(tampered); ok {
		t.Fatal("a changed datagram opened")
	}
	if _, ok := b.openDatagram([]byte("plain text from someone else")); ok {
		t.Fatal("an unsealed datagram opened")
	}
	// Someone with another key pair, who saw both signals, seals nothing the
	// players open.
	c, err := NewNakamaP2P("keys-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.HandleSignal(b.localSignal()); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.openDatagram(sealAs(t, c, []byte("inputs"))); ok {
		t.Fatal("a third party's datagram opened")
	}
	// Nothing goes out before the keys.
	d, err := NewNakamaP2P("keys-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := (p2pRoute{path: d.udp4, addr: peer.LocalAddr()}).send([]byte("x")); err == nil {
		t.Fatal("sent without keys")
	}
}

// A hello is answered and its route checked; only an ack that echoes the
// nonce of the hello sent on a route verifies it, and the session sends only
// on verified routes and takes data only from them.
func TestP2PHandshakeVerifiesRoutesWithChecks(t *testing.T) {
	a, b := signedPair(t)
	peer := listenLocalUDP(t)
	other := listenLocalUDP(t)
	direct := p2pRoute{path: a.udp4, addr: peer.LocalAddr()}
	elsewhere := p2pRoute{path: a.udp4, addr: other.LocalAddr()}
	var relaySent [][]byte
	var relayMu sync.Mutex
	relay := p2pRoute{path: &p2pRelayPath{crypt: &a.crypt, link: recordLink(func(p []byte) {
		relayMu.Lock()
		relaySent = append(relaySent, p)
		relayMu.Unlock()
	})}, addr: p2pRelayAddr{peer: "b"}}
	nonce := func(r p2pRoute) string { return p2pCheckNonce(b.keys.handshake, r.addr) }
	ack := func(r p2pRoute) []byte { return sealAs(t, b, b.handshakePacket(p2pAck, nonce(r))) }

	if a.receive(sealAs(t, b, b.handshakePacket(p2pHello, "peer-nonce")), direct) {
		t.Fatal("a hello alone completed the handshake")
	}
	got := readPackets(t, peer, b)
	if len(got) != 2 || got[0].Kind != p2pAck || got[0].Nonce != "peer-nonce" || got[1].Kind != p2pHello || got[1].Nonce != nonce(direct) {
		t.Fatalf("answer to a hello: %+v; want an ack echoing its nonce, then a check of the route", got)
	}
	// The ack a check of the direct route gets, copied to other routes.
	if a.receive(ack(direct), elsewhere) || a.receive(ack(direct), relay) {
		t.Fatal("an ack completed the handshake on a route its hello did not go to")
	}
	if !a.receive(ack(relay), relay) {
		t.Fatal("the relay's ack did not complete the handshake")
	}
	if a.Route() != "relay" || a.mux.isKnown(direct) || a.mux.isKnown(elsewhere) {
		t.Fatalf("route %q; want the relay, and data only from verified routes", a.Route())
	}
	a.receive(ack(direct), direct)
	if a.Route() != "direct" || !a.mux.isKnown(direct) || a.mux.isKnown(elsewhere) {
		t.Fatalf("route %q after the direct route's ack", a.Route())
	}
}

// Copies of a hello sent from many addresses are answered on at most
// p2pMaxRoutes of them.
func TestP2PHandshakeKeepsFewRoutes(t *testing.T) {
	a, b := signedPair(t)
	hello := sealAs(t, b, b.handshakePacket(p2pHello, "n"))
	for i := 0; i < 3*p2pMaxRoutes; i++ {
		a.receive(hello, p2pRoute{path: a.udp4, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: deadUDPPort(t)}})
	}
	a.mu.Lock()
	n := len(a.checks)
	a.mu.Unlock()
	if n != p2pMaxRoutes {
		t.Fatalf("%d routes kept", n)
	}
}

type recordLink func([]byte)

func (r recordLink) Send(b []byte) error { r(append([]byte(nil), b...)); return nil }
func (r recordLink) Close() error        { return nil }

// The server names who sent a match message; a signal that claims another
// sender is dropped before the handshake sees it.
func TestNakamaSignalSenderMustMatchThePresence(t *testing.T) {
	n, _ := relayTestClient(t, 64)
	p, err := NewNakamaP2P("session-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("me", "opponent")
	n.mu.Lock()
	n.p2p = p
	n.mu.Unlock()
	signal := func(sender string, s P2PSignal) {
		body, _ := json.Marshal(s)
		n.handleJSON([]byte(fmt.Sprintf(`{"match_data":{"match_id":"session-match","op_code":"%d","data":"%s","presence":{"user_id":"%s"}}}`,
			nakamaSignalOp, base64.StdEncoding.EncodeToString(body), sender)))
	}
	forged := P2PSignal{Magic: p2pMagic, MatchID: "session-match", Token: "intruder-token", Key: testP2PKey(t), From: "opponent", To: "me"}
	signal("intruder", forged)
	forged.From = ""
	signal("intruder", forged)
	// Without a sender the server named, nothing says who sent it.
	forged.From = "opponent"
	signal("", forged)
	forged.From = ""
	signal("", forged)
	body, _ := json.Marshal(forged)
	n.handleJSON([]byte(fmt.Sprintf(`{"match_data":{"match_id":"session-match","op_code":"%d","data":"%s"}}`,
		nakamaSignalOp, base64.StdEncoding.EncodeToString(body))))
	p.mu.Lock()
	remote := p.remoteToken
	p.mu.Unlock()
	if remote != "" {
		t.Fatalf("took a signal another member sent in the opponent's name (token %q)", remote)
	}
	signal("opponent", P2PSignal{Magic: p2pMagic, MatchID: "session-match", Token: "opponent-token", Key: testP2PKey(t), To: "me"})
	p.mu.Lock()
	remote = p.remoteToken
	p.mu.Unlock()
	if remote != "opponent-token" {
		t.Fatalf("the opponent's signal was not taken (token %q)", remote)
	}
}

// relayTestClient is a Nakama client whose realtime output lands in a queue
// the test reads, and whose input the test feeds to handleJSON.
func relayTestClient(t *testing.T, queue int) (*NakamaClient, chan []byte) {
	t.Helper()
	n := NewNakamaClient(NakamaConfig{})
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	q := make(chan []byte, queue)
	n.mu.Lock()
	n.conn, n.writeQueue, n.matchID = c1, q, "session-match"
	n.mu.Unlock()
	return n, q
}

func nextEnvelope(t *testing.T, q chan []byte) map[string]json.RawMessage {
	t.Helper()
	select {
	case body := <-q:
		var env map[string]json.RawMessage
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatal(err)
		}
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was sent")
	}
	return nil
}

func TestNakamaRelayJoinAndDatagrams(t *testing.T) {
	n, q := relayTestClient(t, 64)
	var got [][]byte
	var gotMu sync.Mutex
	type joinResult struct {
		link p2pRelayLink
		err  error
	}
	joined := make(chan joinResult, 1)
	go func() {
		link, err := n.JoinRelay("ikemen-p2p-relay-x", "peer", func(b []byte) {
			gotMu.Lock()
			got = append(got, b)
			gotMu.Unlock()
		})
		joined <- joinResult{link, err}
	}()
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	if !strings.HasPrefix(cid, "ikemen-relay-") || !bytes.Contains(env["match_create"], []byte(`"ikemen-p2p-relay-x"`)) {
		t.Fatalf("join request %s", env)
	}
	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"relay-1.","size":2}}`))
	r := <-joined
	if r.err != nil {
		t.Fatal(r.err)
	}
	if n.CurrentMatchID() != "session-match" {
		t.Fatalf("joining the relay made it the client's match: %q", n.CurrentMatchID())
	}

	data := func(user, payload string) string {
		return fmt.Sprintf(`{"match_data":{"match_id":"relay-1.","op_code":"%d","data":"%s","presence":{"user_id":"%s"}}}`,
			nakamaRelayOp, base64.StdEncoding.EncodeToString([]byte(payload)), user)
	}
	n.handleJSON([]byte(data("peer", "from the peer")))
	n.handleJSON([]byte(data("someone-else", "from a stranger")))
	if !n.handleRelay(map[string]json.RawMessage{"match_presence_event": json.RawMessage(`{"match_id":"relay-1."}`)}) {
		t.Fatal("the relay's presence event reached the client's handlers")
	}
	if n.handleRelay(map[string]json.RawMessage{"match_data": json.RawMessage(`{"match_id":"session-match","op_code":"1"}`)}) {
		t.Fatal("the session match's data was taken for the relay's")
	}
	gotMu.Lock()
	if len(got) != 1 || string(got[0]) != "from the peer" {
		t.Fatalf("delivered %q, want only the peer's datagram", got)
	}
	gotMu.Unlock()

	if err := r.link.Send([]byte("to the peer")); err != nil {
		t.Fatal(err)
	}
	env = nextEnvelope(t, q)
	var sent struct {
		MatchID string `json:"match_id"`
		OpCode  uint32 `json:"op_code"`
		Data    []byte `json:"data"`
	}
	if err := json.Unmarshal(env["match_data_send"], &sent); err != nil || sent.MatchID != "relay-1." ||
		sent.OpCode != nakamaRelayOp || string(sent.Data) != "to the peer" {
		t.Fatalf("relay send %s (%v)", env, err)
	}

	_ = r.link.Close()
	env = nextEnvelope(t, q)
	if !bytes.Contains(env["match_leave"], []byte(`"relay-1."`)) {
		t.Fatalf("close sent %s, want a match_leave of the relay", env)
	}
	// A closed relay's match is still a relayed match: what comes from it
	// is dropped, not handed to the client's handlers.
	if !n.handleRelay(map[string]json.RawMessage{"match_data": json.RawMessage(`{"match_id":"relay-1.","op_code":"` + fmt.Sprint(nakamaRelayOp) + `","data":"eA==","presence":{"user_id":"peer"}}`)}) {
		t.Fatal("a closed relay's data reached the client's handlers")
	}
	gotMu.Lock()
	if len(got) != 1 {
		t.Fatalf("a closed relay delivered %q", got)
	}
	gotMu.Unlock()
}

func TestNakamaRelaySendKeepsRoomForOtherMessages(t *testing.T) {
	n, q := relayTestClient(t, 8)
	r := &nakamaRelay{client: n, matchID: "relay-2.", peer: "peer"}
	sent := 0
	for i := 0; i < 8; i++ {
		if r.Send([]byte{1}) == nil {
			sent++
		}
	}
	if sent != 7 || len(q) != 7 {
		t.Fatalf("sent %d of 8 into a queue of 8 (holding %d); want the relay to stop at three quarters", sent, len(q))
	}
	if err := n.PublishSignal(P2PSignal{Magic: p2pMagic}); err != nil {
		t.Fatalf("a signal found no room: %v", err)
	}
}

func TestRunBeforeLuaExit(t *testing.T) {
	l := lua.NewState()
	defer l.Close()
	var order []string
	code := -1
	// A stand-in for gopher-lua's os.exit, which would end the test.
	l.GetGlobal("os").(*lua.LTable).RawSetString("exit", l.NewFunction(func(l *lua.LState) int {
		order = append(order, "exit")
		code = l.OptInt(1, 0)
		return 0
	}))
	runBeforeLuaExit(l, func() { order = append(order, "before") })
	if err := l.DoString("os.exit(3)"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "before" || order[1] != "exit" || code != 3 {
		t.Fatalf("calls %v, code %d", order, code)
	}
}

// A relayed match is the relay's: whatever another member sends on it, under
// any spelling of its id Nakama accepts, before the relay is joined or after
// it was left, never reaches the client's handlers (lobby state and events,
// rematch decisions). The client's own matches are authoritative.
func TestNakamaRelayedMatchDataReachesOnlyTheRelay(t *testing.T) {
	n, q := relayTestClient(t, 64)
	rec := newRecorder()
	n.SetEventHandler(rec.fn)
	data := func(matchID, user string, op uint32, payload string) []byte {
		return []byte(fmt.Sprintf(`{"match_data":{"match_id":"%s","op_code":"%d","data":"%s","presence":{"user_id":"%s"}}}`,
			matchID, op, base64.StdEncoding.EncodeToString([]byte(payload)), user))
	}
	decision := `{"kind":"ikemen-rematch-decision","version":1,"round":999,"action":"exit"}`
	lobbyState := `{"kind":"state","state":{"phase":"closed"}}`
	const relayID = "1b4e28ba-2fa1-11d2-883f-0016d3cca427."
	forged := func(id string) {
		n.handleJSON(data(id, "stranger", nakamaRematchDecisionOp, decision))
		n.handleJSON(data(id, "stranger", 0x494B_4C12, lobbyState))
		n.handleJSON([]byte(`{"match_presence_event":{"match_id":"` + id + `","joins":[{"user_id":"stranger"}]}}`))
	}

	forged(relayID) // before the relay is joined
	var got []string
	var gotMu sync.Mutex
	joined := make(chan p2pRelayLink, 1)
	go func() {
		link, err := n.JoinRelay("ikemen-p2p-relay-x", "peer", func(b []byte) {
			gotMu.Lock()
			got = append(got, string(b))
			gotMu.Unlock()
		})
		if err != nil {
			t.Error(err)
		}
		joined <- link
	}()
	env := nextEnvelope(t, q)
	var cid string
	_ = json.Unmarshal(env["cid"], &cid)
	n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"` + relayID + `","size":2}}`))
	link := <-joined
	bare := strings.TrimSuffix(relayID, ".")
	for _, id := range []string{relayID, strings.ToUpper(relayID), "{" + bare + "}.", "urn:uuid:" + relayID, strings.ReplaceAll(relayID, "-", "")} {
		forged(id)
		n.handleJSON(data(id, "peer", nakamaRelayOp, "peer as "+id))
	}
	_ = link.Close()
	_ = nextEnvelope(t, q) // the leave
	forged(relayID)        // after the relay was left

	gotMu.Lock()
	if len(got) != 1 || got[0] != "peer as "+relayID {
		t.Fatalf("the relay delivered %q; want only the peer's data under the id the server gave", got)
	}
	gotMu.Unlock()
	rec.mu.Lock()
	evs := append([]NakamaEvent(nil), rec.evs...)
	rec.mu.Unlock()
	if len(evs) != 0 {
		t.Fatalf("relayed-match messages reached the client's handlers: first %q", evs[0].Type)
	}
	if _, _, ok := n.LobbyState(); ok {
		t.Fatal("a relayed match set the lobby state")
	}
	// The client's own match still gets through.
	n.handleJSON(data("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee.nakama1", "opponent", nakamaRematchDecisionOp, decision))
	if _, ok := rec.waitFor(t, time.Second, func(ev NakamaEvent) bool { return ev.Type == "rematch_decision" }); !ok {
		t.Fatal("the client's own match data was dropped")
	}
}

// The answer to a relay join given up on leaves its match, unless a later
// join of the same relay waits for its own answer: the same name gives the
// same match, and leaving would take the client out of it.
func TestNakamaRelayLateAnswerKeepsARetry(t *testing.T) {
	n, q := relayTestClient(t, 64)
	answer := func(cid string) {
		n.handleJSON([]byte(`{"cid":"` + cid + `","match":{"match_id":"2b4e28ba-2fa1-11d2-883f-0016d3cca427.","size":2}}`))
	}
	retry := &nakamaRelay{client: n, cid: "ikemen-relay-retry", name: "ikemen-p2p-relay-y", joined: make(chan error, 1)}
	n.relayMu.Lock()
	n.relayByCID = map[string]*nakamaRelay{retry.cid: retry}
	n.relayByMatch = map[string]*nakamaRelay{}
	n.relayGivenUp = map[string]string{"ikemen-relay-first": "ikemen-p2p-relay-y", "ikemen-relay-other": "ikemen-p2p-relay-z"}
	n.relayMu.Unlock()
	answer("ikemen-relay-first")
	select {
	case body := <-q:
		t.Fatalf("left the relay a retry is joining: %s", body)
	case <-time.After(200 * time.Millisecond):
	}
	answer("ikemen-relay-other")
	if env := nextEnvelope(t, q); !bytes.Contains(env["match_leave"], []byte("2b4e28ba")) {
		t.Fatalf("an answer nobody waits for did not leave its match: %s", env)
	}
}

// Once connected, a session keeps its keys: a signal with another token or
// key changes nothing.
func TestP2PKeepsItsKeysOnceConnected(t *testing.T) {
	a, b := signedPair(t)
	relay := p2pRoute{path: &p2pRelayPath{crypt: &a.crypt, link: recordLink(func([]byte) {})}, addr: p2pRelayAddr{peer: "b"}}
	if !a.receive(sealAs(t, b, b.handshakePacket(p2pAck, p2pCheckNonce(b.keys.handshake, relay.addr))), relay) {
		t.Fatal("not connected")
	}
	keys, crypt := a.keys, a.crypt.Load()
	other := b.localSignal()
	other.Token, other.Key = "another-token", testP2PKey(t)
	if err := a.HandleSignal(other); err != nil {
		t.Fatal(err)
	}
	if a.keys != keys || a.crypt.Load() != crypt || a.remoteToken != b.token {
		t.Fatal("a signal re-keyed a connected session")
	}
}

// The mux starts as soon as a route is verified, wherever that happens:
// its first ping tells the peer this side is ready.
func TestP2PMuxRunsOnceVerified(t *testing.T) {
	a, b := signedPair(t)
	peer := listenLocalUDP(t)
	direct := p2pRoute{path: a.udp4, addr: peer.LocalAddr()}
	if !a.receive(sealAs(t, b, b.handshakePacket(p2pAck, p2pCheckNonce(b.keys.handshake, direct.addr))), direct) {
		t.Fatal("not connected")
	}
	buf := make([]byte, 2048)
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("no ping from the mux: %v", err)
		}
		if data, ok := b.openDatagram(buf[:n]); ok && len(data) > 1 && data[0] == p2pTagControl && data[1] == p2pCtrlPing {
			return
		}
	}
}

// A signal from the paired player's older game ends the handshake at once,
// saying why; one from anyone else changes nothing.
func TestP2POlderPeerEndsTheHandshake(t *testing.T) {
	p, err := NewNakamaP2P("old-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.SetPeer("me", "opponent")
	v1 := P2PSignal{Magic: p2pMagicV1, MatchID: "old-match", Token: "t", From: "someone-else", To: "me"}
	_ = p.HandleSignal(v1)
	if p.State() != "connecting" || p.OldPeer() {
		t.Fatalf("a stray old signal ended the handshake (%s)", p.State())
	}
	v1.From = "opponent"
	_ = p.HandleSignal(v1)
	if p.State() != "failed" || !p.OldPeer() {
		t.Fatalf("state %s, old peer %v", p.State(), p.OldPeer())
	}
}

// The peer sends on a UDP route once its own check of it was answered, and
// may move there from the relay after both handshakes ended, while this
// side's check of the route was lost. Its data on that route has this side
// check the route, at most once an interval, and counts once the check is
// answered.
func TestP2PDataOnAnUnverifiedRouteHasItChecked(t *testing.T) {
	a, b := signedPair(t)
	a.interval = 500 * time.Millisecond
	relay := p2pRoute{path: &p2pRelayPath{crypt: &a.crypt, link: recordLink(func([]byte) {})}, addr: p2pRelayAddr{peer: "b"}}
	if !a.receive(sealAs(t, b, b.handshakePacket(p2pAck, p2pCheckNonce(b.keys.handshake, relay.addr))), relay) {
		t.Fatal("not connected over the relay")
	}
	ga, _, err := a.OpenGameplay()
	if err != nil {
		t.Fatal(err)
	}
	defer ga.Close()
	peer := listenLocalUDP(t)
	aAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: a.LocalPort()}
	data := func(s string) { _, _ = peer.WriteToUDP(sealAs(t, b, append([]byte{p2pTagGGPO}, s...)), aAddr) }
	for i := 0; i < 5; i++ {
		data("dropped")
	}
	nonce := p2pCheckNonce(b.keys.handshake, peer.LocalAddr())
	if got := readPackets(t, peer, b); len(got) != 1 || got[0].Kind != p2pHello || got[0].Nonce != nonce {
		t.Fatalf("answer to the peer's data on a route this side never checked: %+v; want one check of the route", got)
	}
	// That check was lost: the next data, an interval later, has it sent again.
	time.Sleep(a.interval)
	data("dropped too")
	if got := readPackets(t, peer, b); len(got) != 1 || got[0].Kind != p2pHello || got[0].Nonce != nonce {
		t.Fatalf("answer to the peer's data an interval after a lost check: %+v; want the check again", got)
	}
	_, _ = peer.WriteToUDP(sealAs(t, b, b.handshakePacket(p2pAck, nonce)), aAddr)
	data("counted")
	buf := make([]byte, 64)
	_ = ga.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := ga.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "counted" {
		t.Fatalf("read %q, %v; want only what came after the check was answered", buf[:n], err)
	}
	if a.Route() != "direct" {
		t.Fatalf("route %q, want the UDP route the check verified", a.Route())
	}
}

// A port the router opened for a session closes with the session, and at
// once when the router grants it after another way already worked.
func TestP2PClosesTheRouterPort(t *testing.T) {
	noLadderExtras(t)
	var mu sync.Mutex
	var closed []*portmap.Mapping
	prevUnmap := p2pUnmapPort
	p2pUnmapPort = func(m *portmap.Mapping) error {
		mu.Lock()
		closed = append(closed, m)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { p2pUnmapPort = prevUnmap })
	closedNow := func() []*portmap.Mapping {
		mu.Lock()
		defer mu.Unlock()
		return append([]*portmap.Mapping(nil), closed...)
	}
	var granted []*portmap.Mapping
	p2pMapPort = func(_ context.Context, port int) (*portmap.Mapping, error) {
		m := &portmap.Mapping{Method: "pcp", ExternalIP: net.ParseIP("203.0.113.9"), ExternalPort: port, InternalPort: port}
		granted = append(granted, m)
		return m, nil
	}

	// Granted while the handshake goes on: the session keeps it until it
	// closes.
	a, b := signedPair(t)
	a.openRouterPort()
	if len(granted) != 1 || len(closedNow()) != 0 {
		t.Fatalf("granted %d, closed %d; want the port kept", len(granted), len(closedNow()))
	}
	a.Close()
	deadline := time.Now().Add(2 * time.Second)
	for len(closedNow()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := closedNow(); len(got) != 1 || got[0] != granted[0] {
		t.Fatalf("closed %v after the session closed; want its port, once", got)
	}

	// Granted once the session is ready: closed at once.
	relay := p2pRoute{path: &p2pRelayPath{crypt: &b.crypt, link: recordLink(func([]byte) {})}, addr: p2pRelayAddr{peer: "a"}}
	if !b.receive(sealAs(t, a, a.handshakePacket(p2pAck, p2pCheckNonce(a.keys.handshake, relay.addr))), relay) {
		t.Fatal("not connected over the relay")
	}
	b.openRouterPort()
	if got := closedNow(); len(granted) != 2 || len(got) != 2 || got[1] != granted[1] {
		t.Fatalf("closed %v; want the port granted after the handshake closed at once", got)
	}
	b.Close()
	time.Sleep(100 * time.Millisecond)
	if got := closedNow(); len(got) != 2 {
		t.Fatalf("closed %d ports; the session kept none to close", len(got))
	}

	// Granted after the session closed (the player gave up while the router
	// answered): closed at once.
	c, _ := signedPair(t)
	grant := p2pMapPort
	p2pMapPort = func(ctx context.Context, port int) (*portmap.Mapping, error) {
		c.Close()
		return grant(ctx, port)
	}
	c.openRouterPort()
	if got := closedNow(); len(granted) != 3 || len(got) != 3 || got[2] != granted[2] {
		t.Fatalf("closed %v; want the port granted after the session closed closed at once", got)
	}
}

// The game's Lua state closes the router ports P2P sessions opened before
// os.exit ends the game (runBeforeLuaExit, releaseRouterPorts).
func TestNakamaScriptInitClosesRouterPortsAtExit(t *testing.T) {
	var calls []string
	prevCloseAll := p2pCloseAllPorts
	p2pCloseAllPorts = func(timeout time.Duration) {
		if timeout <= 0 {
			t.Errorf("closing the router ports with timeout %v", timeout)
		}
		calls = append(calls, "release")
	}
	t.Cleanup(func() { p2pCloseAllPorts = prevCloseAll })
	l := lua.NewState()
	defer l.Close()
	defer rankedClaims.setListener(nil)
	// A stand-in for gopher-lua's os.exit, which would end the test.
	l.GetGlobal("os").(*lua.LTable).RawSetString("exit", l.NewFunction(func(*lua.LState) int {
		calls = append(calls, "exit")
		return 0
	}))
	nakamaScriptInit(l)
	if err := l.DoString("os.exit(0)"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != "release" || calls[1] != "exit" {
		t.Fatalf("calls %v; want the router ports released, then the exit", calls)
	}
}

// The peer's data replayed from many addresses has at most p2pMaxRoutes of
// them checked: a copied datagram draws few hellos.
func TestP2PRecheckKeepsFewRoutes(t *testing.T) {
	a, b := signedPair(t)
	relay := p2pRoute{path: &p2pRelayPath{crypt: &a.crypt, link: recordLink(func([]byte) {})}, addr: p2pRelayAddr{peer: "b"}}
	if !a.receive(sealAs(t, b, b.handshakePacket(p2pAck, p2pCheckNonce(b.keys.handshake, relay.addr))), relay) {
		t.Fatal("not connected over the relay")
	}
	keepalive := sealAs(t, b, []byte{p2pTagControl, p2pCtrlKeepalive})
	for i := 0; i < 3*p2pMaxRoutes; i++ {
		a.receive(keepalive, p2pRoute{path: a.udp4, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: deadUDPPort(t)}})
	}
	a.mu.Lock()
	n := len(a.checks)
	a.mu.Unlock()
	if n > p2pMaxRoutes {
		t.Fatalf("%d routes kept", n)
	}
}
