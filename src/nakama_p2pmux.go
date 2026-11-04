package main

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	kcp "github.com/xtaci/kcp-go/v5"
)

// After the NAT handshake, the hole-punched UDP socket is shared by all P2P
// traffic for the rest of the Nakama session. The first byte of each datagram
// says what it carries:
//
//	'{'            handshake JSON; late hellos are answered with acks
//	p2pTagControl  keepalives, round-trip pings and stream close notices
//	p2pTagStream   the netplay session stream (NetConnection over KCP)
//	p2pTagGGPO     GGPO rollback datagrams, one channel per match
//
// The tags cannot collide with handshake or STUN packets: JSON starts with
// '{' and a STUN message's first byte is always below 0x40.
const (
	p2pTagControl byte = 0xA0
	p2pTagStream  byte = 0xA1
	p2pTagGGPO    byte = 0xA2

	p2pCtrlKeepalive byte = 0x01
	p2pCtrlBye       byte = 0x02
	// A ping carries the sender's 8-byte clock reading; the peer returns it
	// in a pong, and the sender measures the round trip. Peers that predate
	// pings ignore them (and still count them as traffic).
	p2pCtrlPing byte = 0x03
	p2pCtrlPong byte = 0x04

	// A keepalive goes out when nothing else was sent for this long, so the
	// NAT mapping survives loading screens and other quiet phases.
	p2pKeepaliveInterval = 2 * time.Second
	// A peer's close notice closes the local stream after this delay, so data
	// the peer flushed just before closing can still arrive.
	p2pByeGrace = 750 * time.Millisecond
	// With keepalives every p2pKeepaliveInterval, this much silence means the
	// peer is gone (closed without notice, crashed, or the path broke). The
	// stream is then closed, the way a TCP reset ends a TCP session.
	p2pPeerSilenceTimeout = 8 * time.Second

	p2pChannelQueue = 1024
	// KCP segment size. With the tag byte, the seal (p2pCrypt: 28 bytes) and
	// UDP/IPv6 headers the datagram stays under 1280 bytes, the IPv6 minimum
	// and below common tunnel and PPPoE MTUs.
	p2pStreamMTU = 1200
	// The routes a session accepts data from (p2pMux.known), and the routes
	// its handshake keeps (NakamaP2P.checks), are at most this many.
	p2pMaxRoutes = 16
)

// nakamaP2PHostLabel stands in for the host address on the guest's rollback
// session when the session runs over the Nakama P2P stream.
const nakamaP2PHostLabel = "nakama-p2p"

var errP2PMuxClosed = errors.New("p2p socket is closed")

// p2pMux reads the punched socket and routes datagrams to channels. The
// session's other paths (the IPv6 socket, the relay) hand it what they read
// (dispatch), and it sends on the route the handshake completed on (active).
type p2pMux struct {
	conn  *net.UDPConn // the IPv4 socket, which run reads
	udp   *p2pUDPPath  // conn's path, for the routes of what run reads
	hooks p2pMuxHooks
	// remote is the peer's address for KCP and GGPO. On the IPv4 socket it is
	// the peer's real address; on the other paths it is the peer's IPv4
	// address or a stand-in (see NakamaP2P.peerIPv4), since the rollback
	// session takes only an IPv4 peer. Either way every send goes out on the
	// active route, whatever address it names.
	remote *net.UDPAddr

	mu       sync.Mutex
	known    []p2pRoute
	active   p2pRoute
	channels map[byte]*p2pChannel
	heard    sync.Once

	runOnce   sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	lastSend  atomic.Int64
	lastRecv  atomic.Int64

	// Round trip over the punched path: smoothed as TCP does (srtt), from
	// pings sent every p2pKeepaliveInterval/2. epoch is the clock pings carry.
	epoch      time.Time
	rttMu      sync.Mutex
	srtt       time.Duration
	rttSamples int
}

// p2pMuxHooks connect a mux to its session: control takes the handshake
// packets the peer sends, open opens a sealed datagram run read from the
// IPv4 socket (p2pCrypt), and heard runs once, at the peer's first data,
// which shows its game finished the handshake too. unverified runs when the
// peer's data comes on a route this side has not verified, and dropped it:
// the session checks that route (NakamaP2P.recheck). A nil open takes
// datagrams as they are (tests).
type p2pMuxHooks struct {
	control    func(data []byte, from p2pRoute)
	open       func(sealed []byte) ([]byte, bool)
	heard      func()
	unverified func(from p2pRoute)
}

func newP2PMux(conn *net.UDPConn, udp *p2pUDPPath, remote *net.UDPAddr, route p2pRoute, hooks p2pMuxHooks) *p2pMux {
	r := *remote
	m := &p2pMux{
		conn:     conn,
		udp:      udp,
		hooks:    hooks,
		remote:   &r,
		active:   route,
		channels: make(map[byte]*p2pChannel),
		closed:   make(chan struct{}),
		epoch:    time.Now(),
	}
	m.lastRecv.Store(time.Now().UnixNano())
	m.addKnown(route)
	return m
}

func (m *p2pMux) remoteAddr() *net.UDPAddr {
	r := *m.remote
	return &r
}

// activeRoute is the route the session sends on.
func (m *p2pMux) activeRoute() p2pRoute {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// addKnown records a route the handshake verified (NakamaP2P.verify): it
// works both ways, so the peer's data is accepted from it. The peer may send
// on another route than the one this side sends on (LAN versus hairpinned
// public address, or the relay while this side sends directly), so data from
// any verified route is accepted.
func (m *p2pMux) addKnown(route p2pRoute) {
	if !route.valid() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.known {
		if k.equal(route) {
			return
		}
	}
	if len(m.known) < p2pMaxRoutes {
		m.known = append(m.known, route)
	}
}

// prefer makes a verified UDP route the route to send on instead of the
// relay.
func (m *p2pMux) prefer(route p2pRoute) {
	if !route.valid() {
		return
	}
	m.addKnown(route)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.valid() && p2pPathRank(route.path) > p2pPathRank(m.active.path) {
		log.Printf("Nakama P2P: %s works too; sending on it instead of %s", route, m.active)
		m.active = route
	}
}

func (m *p2pMux) isKnown(route p2pRoute) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.known {
		if k.equal(route) {
			return true
		}
	}
	return false
}

// start launches the reader once. readMu is held for the reader's lifetime so
// the handshake code can never read the socket at the same time.
func (m *p2pMux) start(readMu *sync.Mutex) {
	m.runOnce.Do(func() {
		go m.run(readMu)
		go m.keepalive()
	})
}

func (m *p2pMux) run(readMu *sync.Mutex) {
	readMu.Lock()
	defer readMu.Unlock()
	defer m.shutdown()
	// The handshake may have left a read deadline behind.
	_ = m.conn.SetReadDeadline(time.Time{})
	buf := make([]byte, 64*1024)
	for {
		n, from, err := m.conn.ReadFromUDP(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				_ = m.conn.SetReadDeadline(time.Time{})
				continue
			}
			return
		}
		data := buf[:n]
		if m.hooks.open != nil {
			var ok bool
			if data, ok = m.hooks.open(data); !ok {
				continue
			}
		}
		m.dispatch(data, p2pRoute{path: m.udp, addr: from})
	}
}

// dispatch handles one datagram from the peer, opened, read on any of the
// session's paths. It copies what it keeps, so the caller may reuse data.
func (m *p2pMux) dispatch(data []byte, from p2pRoute) {
	if len(data) == 0 {
		return
	}
	switch data[0] {
	case '{':
		if m.hooks.control != nil {
			m.hooks.control(append([]byte(nil), data...), from)
		}
	case p2pTagControl, p2pTagStream, p2pTagGGPO:
		if !m.isKnown(from) {
			// The peer's data, since it opened, on a route the peer verified
			// and this side has not: this side's check of it may have been
			// lost.
			if m.hooks.unverified != nil {
				m.hooks.unverified(from)
			}
			return
		}
		m.lastRecv.Store(time.Now().UnixNano())
		if m.hooks.heard != nil {
			m.heard.Do(m.hooks.heard)
		}
		if data[0] == p2pTagControl {
			m.handleControl(data[1:])
			return
		}
		m.mu.Lock()
		ch := m.channels[data[0]]
		m.mu.Unlock()
		if ch != nil {
			ch.deliver(append([]byte(nil), data[1:]...))
		}
	}
}

func (m *p2pMux) keepalive() {
	ticker := time.NewTicker(p2pKeepaliveInterval / 2)
	defer ticker.Stop()
	// A ping at once tells the peer this side is ready (p2pMuxHooks.heard).
	_ = m.sendPing()
	for {
		select {
		case <-m.closed:
			return
		case <-ticker.C:
			// The ping also keeps the NAT mapping alive; the keepalive only goes
			// out when the ping could not be sent.
			_ = m.sendPing()
			if time.Since(time.Unix(0, m.lastSend.Load())) >= p2pKeepaliveInterval {
				_ = m.send(p2pTagControl, []byte{p2pCtrlKeepalive})
			}
			if time.Since(time.Unix(0, m.lastRecv.Load())) >= p2pPeerSilenceTimeout {
				m.mu.Lock()
				ch := m.channels[p2pTagStream]
				m.mu.Unlock()
				if ch != nil {
					log.Printf("Nakama P2P: nothing received over %s for %v; closing the netplay stream", m.activeRoute(), p2pPeerSilenceTimeout)
					_ = ch.Close()
				}
			}
		}
	}
}

func (m *p2pMux) sendPing() error {
	msg := make([]byte, 9)
	msg[0] = p2pCtrlPing
	binary.LittleEndian.PutUint64(msg[1:], uint64(time.Since(m.epoch)))
	return m.send(p2pTagControl, msg)
}

// addRTTSample folds one measured round trip into the smoothed value.
func (m *p2pMux) addRTTSample(sample time.Duration) {
	if sample <= 0 || sample > 10*time.Second {
		return
	}
	m.rttMu.Lock()
	defer m.rttMu.Unlock()
	if m.rttSamples == 0 {
		m.srtt = sample
	} else {
		m.srtt += (sample - m.srtt) / 8
	}
	m.rttSamples++
}

// rtt returns the smoothed round trip and how many pings it is made of.
func (m *p2pMux) rtt() (time.Duration, int) {
	m.rttMu.Lock()
	defer m.rttMu.Unlock()
	return m.srtt, m.rttSamples
}

func (m *p2pMux) handleControl(data []byte) {
	if len(data) < 1 {
		return
	}
	switch data[0] {
	case p2pCtrlPing:
		if len(data) >= 9 {
			pong := append([]byte{p2pCtrlPong}, data[1:9]...)
			_ = m.send(p2pTagControl, pong)
		}
	case p2pCtrlPong:
		if len(data) >= 9 {
			sent := time.Duration(binary.LittleEndian.Uint64(data[1:9]))
			m.addRTTSample(time.Since(m.epoch) - sent)
		}
	case p2pCtrlBye:
		if len(data) < 5 {
			return
		}
		conv := binary.LittleEndian.Uint32(data[1:5])
		m.mu.Lock()
		ch := m.channels[p2pTagStream]
		m.mu.Unlock()
		if ch != nil && ch.conv == conv {
			time.AfterFunc(p2pByeGrace, func() { _ = ch.Close() })
		}
	}
}

// send writes one tagged datagram on the active route. There is one peer,
// so every channel's traffic goes to it, whatever address KCP or GGPO named.
func (m *p2pMux) send(tag byte, payload []byte) error {
	select {
	case <-m.closed:
		return errP2PMuxClosed
	default:
	}
	pkt := make([]byte, 1+len(payload))
	pkt[0] = tag
	copy(pkt[1:], payload)
	if err := m.activeRoute().send(pkt); err != nil {
		return err
	}
	m.lastSend.Store(time.Now().UnixNano())
	return nil
}

func (m *p2pMux) sendBye(conv uint32) {
	msg := []byte{p2pCtrlBye, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(msg[1:], conv)
	for i := 0; i < 3; i++ {
		_ = m.send(p2pTagControl, msg)
	}
}

// attach opens the channel for tag, replacing (and closing) any previous one.
func (m *p2pMux) attach(tag byte, conv uint32) (*p2pChannel, error) {
	select {
	case <-m.closed:
		return nil, errP2PMuxClosed
	default:
	}
	ch := &p2pChannel{
		mux:      m,
		tag:      tag,
		conv:     conv,
		queue:    make(chan []byte, p2pChannelQueue),
		done:     make(chan struct{}),
		dlNotify: make(chan struct{}),
	}
	m.mu.Lock()
	old := m.channels[tag]
	m.channels[tag] = ch
	m.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	if tag == p2pTagStream {
		m.lastRecv.Store(time.Now().UnixNano())
	}
	return ch, nil
}

func (m *p2pMux) detach(ch *p2pChannel) {
	m.mu.Lock()
	if m.channels[ch.tag] == ch {
		delete(m.channels, ch.tag)
	}
	m.mu.Unlock()
}

// shutdown runs when the socket closes: every channel reader gets an error.
func (m *p2pMux) shutdown() {
	m.closeOnce.Do(func() {
		close(m.closed)
		m.mu.Lock()
		chans := make([]*p2pChannel, 0, len(m.channels))
		for _, ch := range m.channels {
			chans = append(chans, ch)
		}
		m.mu.Unlock()
		for _, ch := range chans {
			_ = ch.Close()
		}
	})
}

// p2pChannel is one tag's view of the shared socket. It implements
// net.PacketConn so GGPO and KCP can use it like a socket of their own.
// Every packet it returns is reported as coming from the peer's canonical
// address, which is also where GGPO and KCP send.
type p2pChannel struct {
	mux  *p2pMux
	tag  byte
	conv uint32

	queue     chan []byte
	done      chan struct{}
	closeOnce sync.Once

	dlMu     sync.Mutex
	deadline time.Time
	dlNotify chan struct{}
}

func (c *p2pChannel) deliver(pkt []byte) {
	select {
	case <-c.done:
	case c.queue <- pkt:
	default:
		// Full queue: drop, as a congested socket would. KCP and GGPO both
		// recover from loss.
	}
}

func (c *p2pChannel) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c.dlMu.Lock()
		deadline := c.deadline
		notify := c.dlNotify
		c.dlMu.Unlock()

		var timeout <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}
		select {
		case pkt := <-c.queue:
			if timer != nil {
				timer.Stop()
			}
			return copy(b, pkt), c.mux.remoteAddr(), nil
		case <-c.done:
			if timer != nil {
				timer.Stop()
			}
			return 0, nil, net.ErrClosed
		case <-timeout:
			return 0, nil, os.ErrDeadlineExceeded
		case <-notify:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

func (c *p2pChannel) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	if to, ok := addr.(*net.UDPAddr); !ok || to == nil {
		return 0, errors.New("p2p channel needs a UDP destination")
	}
	if err := c.mux.send(c.tag, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *p2pChannel) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		c.mux.detach(c)
	})
	return nil
}

func (c *p2pChannel) LocalAddr() net.Addr { return c.mux.conn.LocalAddr() }

func (c *p2pChannel) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *p2pChannel) SetReadDeadline(t time.Time) error {
	c.dlMu.Lock()
	c.deadline = t
	close(c.dlNotify)
	c.dlNotify = make(chan struct{})
	c.dlMu.Unlock()
	return nil
}

// Writes go straight to the kernel socket and never block for long.
func (c *p2pChannel) SetWriteDeadline(time.Time) error { return nil }

// p2pStream is the netplay session stream: KCP over the stream channel.
// It satisfies net.Conn, so NetConnection uses it exactly like its TCP socket.
type p2pStream struct {
	*kcp.UDPSession
	ch        *p2pChannel
	mux       *p2pMux
	conv      uint32
	closeOnce sync.Once
}

func openP2PStream(m *p2pMux, conv uint32) (*p2pStream, error) {
	ch, err := m.attach(p2pTagStream, conv)
	if err != nil {
		return nil, err
	}
	sess, err := kcp.NewConn3(conv, m.remoteAddr(), nil, 0, 0, ch)
	if err != nil {
		_ = ch.Close()
		return nil, err
	}
	// NetConnection reads byte counts, not messages, so KCP runs in stream
	// mode. The rest is KCP's low-latency profile: every write is sent at
	// once, ACKs are immediate, fast resend after two skipped ACKs, and no
	// congestion window (the traffic is a few hundred bytes per second).
	sess.SetStreamMode(true)
	sess.SetWriteDelay(false)
	sess.SetNoDelay(1, 10, 2, 1)
	sess.SetACKNoDelay(true)
	sess.SetWindowSize(128, 128)
	sess.SetMtu(p2pStreamMTU)
	return &p2pStream{UDPSession: sess, ch: ch, mux: m, conv: conv}, nil
}

func (s *p2pStream) Read(b []byte) (int, error) {
	n, err := s.UDPSession.Read(b)
	return n, p2pStreamError(err)
}

func (s *p2pStream) Write(b []byte) (int, error) {
	n, err := s.UDPSession.Write(b)
	return n, p2pStreamError(err)
}

// Close ends the stream at once, as closing the TCP socket does: writes made
// after this point fail, and the peer is told the stream is closed.
// NetConnection depends on that. When a session is aborted, its sender
// goroutine still writes the end-of-input marker after the state change; if
// the marker reached the peer, the peer would treat the abort as an orderly
// phase change and wait for the next synchronize instead of ending.
func (s *p2pStream) Close() error {
	s.closeOnce.Do(func() {
		_ = s.UDPSession.Close()
		// Close's final flush goes out through a goroutine that still uses
		// the channel; give it a moment before the channel closes.
		time.Sleep(10 * time.Millisecond)
		s.mux.sendBye(s.conv)
		_ = s.ch.Close()
	})
	return nil
}

// p2pStreamError maps KCP's wrapped timeout to os.ErrDeadlineExceeded, which
// satisfies the plain net.Error check NetConnection uses for polling reads.
func p2pStreamError(err error) error {
	if err == nil {
		return nil
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return os.ErrDeadlineExceeded
	}
	return err
}

// p2pConv derives the KCP conversation id from both handshake tokens, so the
// two peers agree on it and a stale session cannot be mistaken for this one.
func p2pConv(tokenA, tokenB, purpose string) uint32 {
	if tokenB < tokenA {
		tokenA, tokenB = tokenB, tokenA
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(tokenA))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(tokenB))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(purpose))
	return h.Sum32()
}
