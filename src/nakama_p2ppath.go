package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"net"
	"sync"
	"sync/atomic"
)

// A P2P session reaches the peer over one of several paths, tried in turn
// while the handshake runs (see punchLoop):
//
//	direct  the IPv4 UDP socket: host and STUN candidates, and the candidate
//	        a router opened with PCP, NAT-PMP or UPnP
//	ipv6    a second UDP socket for IPv6 host candidates, which need no NAT
//	relay   a match on the Nakama server that the two players join, used
//	        when no UDP path works: the server forwards each datagram over
//	        the players' realtime sockets
//
// Every path carries the same datagrams (handshake JSON and the mux's tagged
// frames), sealed (p2pCrypt), so the handshake and the mux do not depend on
// which one works.
type p2pPath interface {
	name() string
	write(b []byte, to net.Addr) error
}

// p2pPathRank orders paths: a session moves from the relay to a UDP path
// when both turn out to work, since the relay adds the server's round trip.
func p2pPathRank(p p2pPath) int {
	if _, ok := p.(*p2pRelayPath); ok {
		return 1
	}
	return 2
}

// p2pRoute is a path and the peer's address on it.
type p2pRoute struct {
	path p2pPath
	addr net.Addr
}

func (r p2pRoute) valid() bool { return r.path != nil && r.addr != nil }

func (r p2pRoute) equal(o p2pRoute) bool {
	if r.path != o.path || r.addr == nil || o.addr == nil {
		return false
	}
	a, aok := r.addr.(*net.UDPAddr)
	b, bok := o.addr.(*net.UDPAddr)
	if aok && bok {
		return a.Port == b.Port && a.IP.Equal(b.IP)
	}
	return r.addr.String() == o.addr.String()
}

func (r p2pRoute) send(b []byte) error { return r.path.write(b, r.addr) }

func (r p2pRoute) String() string {
	if !r.valid() {
		return "none"
	}
	return r.path.name() + " " + r.addr.String()
}

// p2pCrypt seals every datagram this side sends to the peer, and opens every
// datagram the peer sends, with AES-256-GCM under keys only the two players
// have (p2pKeys), one for each direction. A datagram anybody else made, or
// one this side sent that comes back to it, does not open, on any path: the
// relay's other members and whoever sees the UDP packets can neither read
// nor change what the players send, nor add to it.
type p2pCrypt struct {
	sealer cipher.AEAD
	opener cipher.AEAD
}

func newP2PCrypt(seal, open []byte) (*p2pCrypt, error) {
	sealer, err := newP2PAEAD(seal)
	if err != nil {
		return nil, err
	}
	opener, err := newP2PAEAD(open)
	if err != nil {
		return nil, err
	}
	return &p2pCrypt{sealer: sealer, opener: opener}, nil
}

func newP2PAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns b sealed behind a random nonce.
func (c *p2pCrypt) seal(b []byte) ([]byte, error) {
	size := c.sealer.NonceSize()
	nonce := make([]byte, size, size+len(b)+c.sealer.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.sealer.Seal(nonce, nonce, b, nil), nil
}

// open returns the datagram the peer sealed in b.
func (c *p2pCrypt) open(b []byte) ([]byte, bool) {
	size := c.opener.NonceSize()
	if len(b) < size+c.opener.Overhead() {
		return nil, false
	}
	data, err := c.opener.Open(nil, b[:size], b[size:], nil)
	return data, err == nil && len(data) > 0
}

// errP2PNoKeys: nothing goes to the peer before its signal gave the keys.
var errP2PNoKeys = errors.New("p2p: no keys for the other player yet")

// sealFor seals b with the session's p2pCrypt.
func sealFor(crypt *atomic.Pointer[p2pCrypt], b []byte) ([]byte, error) {
	if crypt == nil {
		return nil, errP2PNoKeys
	}
	c := crypt.Load()
	if c == nil {
		return nil, errP2PNoKeys
	}
	return c.seal(b)
}

// p2pUDPPath is one of the session's UDP sockets.
type p2pUDPPath struct {
	label string
	conn  *net.UDPConn
	crypt *atomic.Pointer[p2pCrypt] // the session's (NakamaP2P.crypt)
}

func (p *p2pUDPPath) name() string { return p.label }

func (p *p2pUDPPath) write(b []byte, to net.Addr) error {
	addr, ok := to.(*net.UDPAddr)
	if !ok {
		return net.InvalidAddrError("p2p UDP path needs a UDP address")
	}
	sealed, err := sealFor(p.crypt, b)
	if err != nil {
		return err
	}
	_, err = p.conn.WriteToUDP(sealed, addr)
	return err
}

// p2pRelayer joins the relay a session falls back to. The Nakama client
// implements it; deliver receives each datagram the peer sends.
type p2pRelayer interface {
	JoinRelay(req p2pRelayRequest, deliver func([]byte)) (p2pRelayLink, error)
}

// p2pRelayRequest names the relay of a session.
type p2pRelayRequest struct {
	// MatchID is the session's match, a lobby or a matchmaker session, which
	// paired the two players and makes the server's relay for them.
	MatchID string
	// Name is the Nakama relayed match the two players meet in instead
	// (p2pKeys.relayName).
	Name string
	// Peer is the other player's user id: only its datagrams are delivered.
	Peer string
	// Server: both games ask for the server's relay first (both signals say
	// so, P2PSignal.ServerRelay).
	Server bool
}

// p2pRelayLink is a joined relay: Send forwards one datagram to the peer,
// Close leaves the relay's match.
type p2pRelayLink interface {
	Send(b []byte) error
	Close() error
}

// p2pRelayAddr stands for the peer on the relay path.
type p2pRelayAddr struct{ peer string }

func (a p2pRelayAddr) Network() string { return "nakama-relay" }
func (a p2pRelayAddr) String() string  { return "relay:" + a.peer }

// p2pRelayPath sends datagrams through the joined relay.
type p2pRelayPath struct {
	mu    sync.Mutex
	link  p2pRelayLink
	crypt *atomic.Pointer[p2pCrypt] // the session's (NakamaP2P.crypt)
}

func (p *p2pRelayPath) name() string { return "relay" }

func (p *p2pRelayPath) write(b []byte, _ net.Addr) error {
	p.mu.Lock()
	link := p.link
	p.mu.Unlock()
	if link == nil {
		return net.ErrClosed
	}
	sealed, err := sealFor(p.crypt, b)
	if err != nil {
		return err
	}
	return link.Send(sealed)
}

func (p *p2pRelayPath) close() {
	p.mu.Lock()
	link := p.link
	p.link = nil
	p.mu.Unlock()
	if link != nil {
		_ = link.Close()
	}
}
