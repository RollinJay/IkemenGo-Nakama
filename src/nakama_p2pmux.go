package main

import (
	"encoding/binary"
	"errors"
	"fmt"
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

	// The mux pings every route it knows, not only the one it sends on: the
	// pongs, which the peer sends back on the route the ping came on, measure
	// each route's round trip and show which routes still work. It pings
	// every p2pProbeInterval while the session runs on a relay, in its first
	// p2pProbeFastFor and for as long after a change of route, and every
	// p2pProbeSlowEvery-th interval otherwise.
	p2pProbeInterval  = 250 * time.Millisecond
	p2pProbeSlowEvery = 4
	p2pProbeFastFor   = 30 * time.Second
	// A route nothing arrived on for this long is quiet: when it is the one
	// the session sends on, the session moves to a route that still works
	// (the relay, when it is still open), or asks for the relay again
	// (p2pMuxHooks.lost).
	p2pRouteQuiet = 2500 * time.Millisecond
	// A route takes over from a lower-ranked one (a direct route from a
	// relay) once this many pongs came back on it in a row, and its round
	// trip is at most p2pUpgradeMargin (or a quarter) above the current
	// route's.
	p2pUpgradePongs  = 3
	p2pUpgradeMargin = 20 * time.Millisecond
	// Between routes of the same rank (two direct routes: LAN and public
	// address, a VPN and the internet), the session moves to one that is
	// faster by p2pFasterMargin (or a fifth) on p2pFasterSamples pongs or
	// more, at most once every p2pSwitchDwell.
	p2pFasterMargin  = 15 * time.Millisecond
	p2pFasterSamples = 5
	p2pSwitchDwell   = 5 * time.Second
)

// nakamaP2PHostLabel stands in for the host address on the guest's rollback
// session when the session runs over the Nakama P2P stream.
const nakamaP2PHostLabel = "nakama-p2p"

var errP2PMuxClosed = errors.New("p2p socket is closed")

// p2pMux reads the punched socket and routes datagrams to channels. The
// session's other paths (the IPv6 socket, the relays) hand it what they read
// (dispatch), and it sends on its active route, which it changes as routes
// start and stop working (see p2pRouteStat).
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
	known    []*p2pRouteStat
	active   p2pRoute
	channels map[byte]*p2pChannel
	heard    sync.Once
	// activeSince is when the active route became active, lastSwitch when it
	// last changed (zero before), switches how often it changed, and
	// lostSent whether hooks.lost ran for the current quiet spell.
	activeSince time.Time
	lastSwitch  time.Time
	switches    int
	lostSent    bool
	// probeInterval and routeQuiet are p2pProbeInterval and p2pRouteQuiet
	// (tests shorten them before start).
	probeInterval time.Duration
	routeQuiet    time.Duration

	runOnce   sync.Once
	closed    chan struct{}
	closeOnce sync.Once
	lastSend  atomic.Int64
	lastRecv  atomic.Int64

	// epoch is the clock pings carry; started is when the mux started.
	epoch   time.Time
	started time.Time
}

// p2pRouteStat is what the mux knows of one verified route: its round trip,
// smoothed as TCP does (srtt) from the pongs that came back on it, how many
// pongs came in a row without a quiet gap (streak), and when the peer's
// pongs, datagrams, and game data (stream or GGPO) last arrived on it.
type p2pRouteStat struct {
	route    p2pRoute
	srtt     time.Duration
	samples  int
	streak   int
	lastPong time.Time
	lastRecv time.Time
	lastData time.Time
}

// p2pMuxHooks connect a mux to its session: control takes the handshake
// packets the peer sends, open opens a sealed datagram run read from the
// IPv4 socket (p2pCrypt), and heard runs once, at the peer's first data,
// which shows its game finished the handshake too. unverified runs when the
// peer's data comes on a route this side has not verified, and dropped it:
// the session checks that route (NakamaP2P.recheck). switched runs when the
// active route changed, and lost when it went quiet with no other route
// working. A nil open takes datagrams as they are (tests).
type p2pMuxHooks struct {
	control    func(data []byte, from p2pRoute)
	open       func(sealed []byte) ([]byte, bool)
	heard      func()
	unverified func(from p2pRoute)
	switched   func(from, to p2pRoute)
	lost       func()
}

func newP2PMux(conn *net.UDPConn, udp *p2pUDPPath, remote *net.UDPAddr, route p2pRoute, hooks p2pMuxHooks) *p2pMux {
	r := *remote
	now := time.Now()
	m := &p2pMux{
		conn:          conn,
		udp:           udp,
		hooks:         hooks,
		remote:        &r,
		active:        route,
		activeSince:   now,
		channels:      make(map[byte]*p2pChannel),
		closed:        make(chan struct{}),
		epoch:         now,
		started:       now,
		probeInterval: p2pProbeInterval,
		routeQuiet:    p2pRouteQuiet,
	}
	m.lastRecv.Store(now.UnixNano())
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

// switchCount is how often the active route changed.
func (m *p2pMux) switchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.switches
}

// statLocked returns the entry of a known route, or nil.
func (m *p2pMux) statLocked(route p2pRoute) *p2pRouteStat {
	for _, k := range m.known {
		if k.route.equal(route) {
			return k
		}
	}
	return nil
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
	if m.statLocked(route) != nil {
		return
	}
	if len(m.known) < p2pMaxRoutes {
		m.known = append(m.known, &p2pRouteStat{route: route})
	}
}

// forget drops a route that closed (a relay the session left), unless it is
// the active one.
func (m *p2pMux) forget(route p2pRoute) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.equal(route) {
		return
	}
	for i, k := range m.known {
		if k.route.equal(route) {
			m.known = append(m.known[:i], m.known[i+1:]...)
			return
		}
	}
}

// prefer makes a verified route the route to send on at once when it ranks
// higher than the active one (a UDP route instead of a relay).
func (m *p2pMux) prefer(route p2pRoute) {
	if !route.valid() {
		return
	}
	m.addKnown(route)
	m.mu.Lock()
	var from p2pRoute
	switched := false
	if m.active.valid() && p2pPathRank(route.path) > p2pPathRank(m.active.path) {
		from, switched = m.active, true
		m.setActiveLocked(route, time.Now(), "works too")
	}
	m.mu.Unlock()
	if switched {
		m.notifySwitch(from, route)
	}
}

// consider takes a route the handshake verified after the session started.
// Before a match runs on the session (no GGPO channel), a route that ranks
// higher than the active one takes over at once, as prefer does; during a
// match, only once its pongs show it works steadily and is not slower
// (recordPong), so a route that answered one check and then fails does not
// stall the match.
func (m *p2pMux) consider(route p2pRoute) {
	m.mu.Lock()
	match := m.channels[p2pTagGGPO] != nil
	m.mu.Unlock()
	if match {
		m.addKnown(route)
		return
	}
	m.prefer(route)
}

// setActiveLocked makes route the active one.
func (m *p2pMux) setActiveLocked(route p2pRoute, now time.Time, why string) {
	log.Printf("Nakama P2P: %s %s; sending on it instead of %s", route, why, m.active)
	m.active = route
	m.activeSince = now
	m.lastSwitch = now
	m.switches++
	m.lostSent = false
}

func (m *p2pMux) notifySwitch(from, to p2pRoute) {
	if m.hooks.switched != nil {
		m.hooks.switched(from, to)
	}
}

func (m *p2pMux) isKnown(route p2pRoute) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statLocked(route) != nil
}

// touch records a datagram of the peer's on a route, and reports whether
// the route is known (verified).
func (m *p2pMux) touch(from p2pRoute, tag byte) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statLocked(from)
	if st == nil {
		return false
	}
	st.lastRecv = now
	if tag == p2pTagStream || tag == p2pTagGGPO {
		st.lastData = now
	}
	return true
}

// routeActivity reports when anything, and when game data, last arrived on
// a known route (zero times for an unknown one).
func (m *p2pMux) routeActivity(route p2pRoute) (recv, data time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.statLocked(route); st != nil {
		return st.lastRecv, st.lastData
	}
	return time.Time{}, time.Time{}
}

// activeLive reports whether something arrived on the active route lately
// (or it became active only lately).
func (m *p2pMux) activeLive() bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.statLocked(m.active)
	if st == nil {
		return false
	}
	last := st.lastRecv
	if m.activeSince.After(last) {
		last = m.activeSince
	}
	return now.Sub(last) <= m.routeQuiet
}

// recordPong folds one round trip measured on route into its entry, and
// moves the session to a better route when the pongs show one (see
// betterLocked).
func (m *p2pMux) recordPong(from p2pRoute, sample time.Duration) {
	if sample <= 0 || sample > 10*time.Second {
		return
	}
	now := time.Now()
	m.mu.Lock()
	st := m.statLocked(from)
	if st == nil {
		m.mu.Unlock()
		return
	}
	if st.samples == 0 {
		st.srtt = sample
	} else {
		st.srtt += (sample - st.srtt) / 8
	}
	st.samples++
	if !st.lastPong.IsZero() && now.Sub(st.lastPong) <= m.routeQuiet {
		st.streak++
	} else {
		st.streak = 1
	}
	st.lastPong = now
	prev, next, switched := m.evaluateLocked(now)
	m.mu.Unlock()
	if switched {
		m.notifySwitch(prev, next)
	}
}

// evaluateLocked moves the session to the best route that answers steadily
// and is better than the active one (betterLocked).
func (m *p2pMux) evaluateLocked(now time.Time) (from, to p2pRoute, switched bool) {
	cur := m.statLocked(m.active)
	if cur == nil {
		return
	}
	var best *p2pRouteStat
	for _, s := range m.known {
		if s == cur || s.streak < p2pUpgradePongs || now.Sub(s.lastPong) > m.routeQuiet || !m.betterLocked(s, cur, now) {
			continue
		}
		if best == nil || p2pPathRank(s.route.path) > p2pPathRank(best.route.path) ||
			(p2pPathRank(s.route.path) == p2pPathRank(best.route.path) && s.srtt < best.srtt) {
			best = s
		}
	}
	if best == nil {
		return
	}
	from = m.active
	why := fmt.Sprintf("answers steadily (%v against %v)", best.srtt.Round(time.Millisecond), cur.srtt.Round(time.Millisecond))
	m.setActiveLocked(best.route, now, why)
	return from, best.route, true
}

// betterLocked reports whether s should take over from cur: a higher rank
// when its round trip is not worse by more than a margin, or the same rank
// when it is clearly faster.
func (m *p2pMux) betterLocked(s, cur *p2pRouteStat, now time.Time) bool {
	rs, rc := p2pPathRank(s.route.path), p2pPathRank(cur.route.path)
	switch {
	case rs > rc:
		if cur.samples == 0 {
			return true
		}
		margin := cur.srtt / 4
		if margin < p2pUpgradeMargin {
			margin = p2pUpgradeMargin
		}
		return s.srtt <= cur.srtt+margin
	case rs == rc:
		if (!m.lastSwitch.IsZero() && now.Sub(m.lastSwitch) < p2pSwitchDwell) || s.samples < p2pFasterSamples || cur.samples < p2pFasterSamples {
			return false
		}
		margin := cur.srtt / 5
		if margin < p2pFasterMargin {
			margin = p2pFasterMargin
		}
		return s.srtt+margin < cur.srtt
	}
	return false
}

// checkRoutes runs every probe interval: when nothing arrived on the active
// route for routeQuiet, the session moves to the best route something still
// arrives on (the relay, while it is open), or, when there is none, tells
// the session once (hooks.lost), which asks for the relay again.
func (m *p2pMux) checkRoutes() {
	now := time.Now()
	m.mu.Lock()
	cur := m.statLocked(m.active)
	if cur == nil {
		m.mu.Unlock()
		return
	}
	last := cur.lastRecv
	if m.activeSince.After(last) {
		last = m.activeSince
	}
	if now.Sub(last) <= m.routeQuiet {
		m.lostSent = false
		m.mu.Unlock()
		return
	}
	var best *p2pRouteStat
	for _, s := range m.known {
		if s == cur || s.lastRecv.IsZero() || now.Sub(s.lastRecv) > m.routeQuiet {
			continue
		}
		if best == nil || p2pPathRank(s.route.path) > p2pPathRank(best.route.path) ||
			(p2pPathRank(s.route.path) == p2pPathRank(best.route.path) && s.samples > 0 && (best.samples == 0 || s.srtt < best.srtt)) {
			best = s
		}
	}
	if best != nil {
		from := m.active
		m.setActiveLocked(best.route, now, fmt.Sprintf("still works while %s went quiet", from))
		m.mu.Unlock()
		m.notifySwitch(from, best.route)
		return
	}
	report := !m.lostSent
	m.lostSent = true
	m.mu.Unlock()
	if report {
		log.Printf("Nakama P2P: nothing arrived over %s for %v, and no other route works", cur.route, m.routeQuiet)
		if m.hooks.lost != nil {
			go m.hooks.lost()
		}
	}
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
		if !m.touch(from, data[0]) {
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
			m.handleControl(data[1:], from)
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
	ticker := time.NewTicker(m.probeInterval)
	defer ticker.Stop()
	// A ping at once tells the peer this side is ready (p2pMuxHooks.heard).
	m.pingRoutes()
	for tick := 1; ; tick++ {
		select {
		case <-m.closed:
			return
		case <-ticker.C:
			// The pings also keep the NAT mappings alive; the keepalive only
			// goes out when nothing could be sent.
			if m.probeFast() || tick%p2pProbeSlowEvery == 0 {
				m.pingRoutes()
			}
			if time.Since(time.Unix(0, m.lastSend.Load())) >= p2pKeepaliveInterval {
				_ = m.send(p2pTagControl, []byte{p2pCtrlKeepalive})
			}
			m.checkRoutes()
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

// probeFast reports whether the routes are pinged every probe interval: while
// the session runs on a relay, and for a while after it started or changed
// route.
func (m *p2pMux) probeFast() bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	return p2pIsRelay(m.active.path) || now.Sub(m.started) < p2pProbeFastFor ||
		(!m.lastSwitch.IsZero() && now.Sub(m.lastSwitch) < p2pProbeFastFor)
}

// pingRoutes pings every known route, the active one first.
func (m *p2pMux) pingRoutes() {
	m.mu.Lock()
	routes := make([]p2pRoute, 0, len(m.known))
	routes = append(routes, m.active)
	for _, k := range m.known {
		if !k.route.equal(m.active) {
			routes = append(routes, k.route)
		}
	}
	m.mu.Unlock()
	for _, r := range routes {
		_ = m.sendPingOn(r)
	}
}

// sendPing pings the peer on the active route.
func (m *p2pMux) sendPing() error { return m.sendPingOn(m.activeRoute()) }

// sendPingOn pings the peer on route; its pong comes back on that route.
func (m *p2pMux) sendPingOn(route p2pRoute) error {
	msg := make([]byte, 9)
	msg[0] = p2pCtrlPing
	binary.LittleEndian.PutUint64(msg[1:], uint64(time.Since(m.epoch)))
	return m.sendOn(route, p2pTagControl, msg)
}

// rtt returns the active route's smoothed round trip and how many pongs it
// is made of.
func (m *p2pMux) rtt() (time.Duration, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.statLocked(m.active); st != nil {
		return st.srtt, st.samples
	}
	return 0, 0
}

// handleControl handles a control message that came on route from. A pong
// goes back on the route its ping came on, so each route's round trip is
// measured on that route (recordPong).
func (m *p2pMux) handleControl(data []byte, from p2pRoute) {
	if len(data) < 1 {
		return
	}
	switch data[0] {
	case p2pCtrlPing:
		if len(data) >= 9 {
			pong := append([]byte{p2pCtrlPong}, data[1:9]...)
			if !from.valid() || m.sendOn(from, p2pTagControl, pong) != nil {
				_ = m.send(p2pTagControl, pong)
			}
		}
	case p2pCtrlPong:
		if len(data) >= 9 {
			sent := time.Duration(binary.LittleEndian.Uint64(data[1:9]))
			m.recordPong(from, time.Since(m.epoch)-sent)
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
	return m.sendOn(m.activeRoute(), tag, payload)
}

// sendOn writes one tagged datagram on route.
func (m *p2pMux) sendOn(route p2pRoute, tag byte, payload []byte) error {
	select {
	case <-m.closed:
		return errP2PMuxClosed
	default:
	}
	if !route.valid() {
		return errors.New("p2p: no route")
	}
	pkt := make([]byte, 1+len(payload))
	pkt[0] = tag
	copy(pkt[1:], payload)
	if err := route.send(pkt); err != nil {
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
