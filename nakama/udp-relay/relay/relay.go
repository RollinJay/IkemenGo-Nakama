// Package udprelay is the UDP relay of IKEMEN GO's P2P sessions: the
// fallback for two players whose games find no direct UDP path to each
// other. It forwards datagrams between the two players of a session over UDP,
// so a relayed fight does not ride the Nakama websocket (TCP), whose
// head-of-line blocking makes it stutter.
//
// The relay knows nothing of Nakama. The server module
// (nakama/modules/ikemen_relay.lua) hands each of the two players of a
// session a ticket: the relay's address and a token signed with a secret the
// module and the relay share (Sign). Each game sends the relay a join
// datagram with its token from the socket it will use; the relay pairs the
// two sides of the session and from then on forwards the data datagrams of
// each side to the address the other side joined from. It never reads the
// payloads, which the games seal end to end (p2pCrypt in src/nakama_p2ppath.go).
//
// The relay is no amplifier: it only forwards data to addresses that proved
// they hold a token and receive at that address, forwards what it gets byte
// for byte, and never answers a datagram with more bytes than it got. An
// address joins in two round trips: a join with a valid token from an
// address the side does not hold yet is answered with a cookie (an HMAC of
// the address and the time), and only a join that echoes it moves or pairs
// the side, so a forged source address can neither take over a side nor
// point the relay's stream at a victim. Answers are limited per source
// prefix (a /24, or a /64 for IPv6) and per session, so junk from one place
// cannot starve the answers to everybody else's joins. Each side is limited
// in datagrams and bytes per second, and a side that sends nothing for
// Config.Idle is forgotten.
//
// Wire format, every datagram starting with a type byte:
//
//	game -> relay   'J' token ['|' cookie]  join (or rejoin from a new address)
//	                'D' payload             data for the other side
//	                'K' 0                   keepalive (answered with 'A')
//	relay -> game   'C' cookie              prove the address: join again
//	                                        with this cookie
//	                'A' state               join acknowledged: 1 waiting,
//	                                        2 paired
//	                'E' code                join refused, or data from an
//	                                        address that has not joined
//	                                        (Err* codes)
//	                'D' payload             data from the other side
//
// Token (ASCII): "v1.<session>.<side>.<expires>.<mac>", where session is 32
// hex digits, side 0 or 1, expires the Unix time in seconds after which it no
// longer joins, and mac the hex HMAC-SHA256, under the secret, of everything
// before ".<mac>".
package udprelay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Datagram types.
const (
	MsgJoin      = 'J'
	MsgData      = 'D'
	MsgKeepalive = 'K'
	MsgAck       = 'A'
	MsgError     = 'E'
	MsgCookie    = 'C'
)

// Join states ('A').
const (
	StateWaiting = 1
	StatePaired  = 2
)

// Join refusals ('E').
const (
	ErrBadToken = 1
	ErrExpired  = 2
	ErrFull     = 3
	// ErrNotJoined answers data from an address that has not joined (the
	// game's NAT mapped it anew, or the relay forgot an idle side): the game
	// joins again.
	ErrNotJoined = 4
	// ErrAddrInUse refuses a join from an address that holds a side of
	// another session which is still active: the game joins from a new
	// socket.
	ErrAddrInUse = 5
)

// CookieLen is the length of a cookie ('C'), in hex digits. A join with a
// valid token is always longer than 'C' and the cookie.
const CookieLen = 32

// cookieWindow: a cookie is good for the time window it was made in and the
// one before.
const cookieWindow = 30

// addrReclaimIdle: a side of another session silent this long gives up its
// address to a join that proved it (the game behind it went away and its
// NAT gave the port to another game).
const addrReclaimIdle = 15 * time.Second

// MaxToken is the longest token the relay reads.
const MaxToken = 256

// Config tunes a Server. Zero fields take the defaults below.
type Config struct {
	// Secret signs the tokens; shared with the server module. Required.
	Secret []byte
	// Idle: a side that sends nothing for this long is forgotten, and a
	// session without sides ends. Default 60 s.
	Idle time.Duration
	// PacketsPerSecond and PacketBurst limit each side's datagrams;
	// BytesPerSecond and ByteBurst its bytes. What goes over is dropped. A
	// fight sends about 125 datagrams and a few KB a second each way.
	// Defaults 500/s burst 200, 256 KiB/s burst 128 KiB.
	PacketsPerSecond, PacketBurst float64
	BytesPerSecond, ByteBurst     float64
	// MaxSessions bounds the sessions held at once. Default 4096.
	MaxSessions int
	// MaxDatagram is the largest datagram read and forwarded. Default 2048.
	MaxDatagram int
	// AnswerRate bounds the answers to datagrams that hold no valid token
	// ('E'), a second, for each source prefix (a /24, or a /64), with a
	// burst of twice as many. Default 5.
	AnswerRate float64
	// JoinAnswerRate bounds the answers to joins with a valid token ('C',
	// 'A') a second for each source prefix, and SessionAnswerRate the
	// answers ('A') a second within each session. Defaults 50 and 20.
	JoinAnswerRate, SessionAnswerRate float64
	// MaxPrefixes bounds the source prefixes whose answers are counted at
	// once; past it, datagrams without a valid token from new prefixes go
	// unanswered until Sweep forgets quiet ones. Default 65536.
	MaxPrefixes int
	// Now is the clock (tests). Default time.Now.
	Now func() time.Time
}

func (c *Config) defaults() {
	if c.Idle <= 0 {
		c.Idle = 60 * time.Second
	}
	if c.PacketsPerSecond <= 0 {
		c.PacketsPerSecond = 500
	}
	if c.PacketBurst <= 0 {
		c.PacketBurst = 200
	}
	if c.BytesPerSecond <= 0 {
		c.BytesPerSecond = 256 * 1024
	}
	if c.ByteBurst <= 0 {
		c.ByteBurst = 128 * 1024
	}
	if c.MaxSessions <= 0 {
		c.MaxSessions = 4096
	}
	if c.MaxDatagram <= 0 {
		c.MaxDatagram = 2048
	}
	if c.AnswerRate <= 0 {
		c.AnswerRate = 5
	}
	if c.JoinAnswerRate <= 0 {
		c.JoinAnswerRate = 50
	}
	if c.SessionAnswerRate <= 0 {
		c.SessionAnswerRate = 20
	}
	if c.MaxPrefixes <= 0 {
		c.MaxPrefixes = 65536
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Sign returns the token that joins side (0 or 1) of session until expires
// (Unix seconds). The server module makes the same (ikemen_relay.lua).
func Sign(secret []byte, session string, side int, expires int64) string {
	payload := "v1." + session + "." + strconv.Itoa(side) + "." + strconv.FormatInt(expires, 10)
	return payload + "." + mac(secret, payload)
}

func mac(secret []byte, payload string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}

// SessionID is the session of the players a and b of match matchID, as the
// server module derives it: the first 32 hex digits of the HMAC of
// "session|<match>|<lesser id>|<greater id>".
func SessionID(secret []byte, matchID, a, b string) string {
	if b < a {
		a, b = b, a
	}
	return mac(secret, "session|"+matchID+"|"+a+"|"+b)[:32]
}

// Token errors.
var (
	ErrTokenInvalid = errors.New("udp relay: invalid token")
	ErrTokenExpired = errors.New("udp relay: token expired")
)

// Verify checks token at now and returns its session and side.
func Verify(secret []byte, token string, now time.Time) (session string, side int, err error) {
	if len(token) > MaxToken {
		return "", 0, ErrTokenInvalid
	}
	cut := strings.LastIndexByte(token, '.')
	if cut < 0 {
		return "", 0, ErrTokenInvalid
	}
	payload, sum := token[:cut], token[cut+1:]
	want := mac(secret, payload)
	if !hmac.Equal([]byte(sum), []byte(want)) {
		return "", 0, ErrTokenInvalid
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != "v1" || len(parts[1]) != 32 || (parts[2] != "0" && parts[2] != "1") {
		return "", 0, ErrTokenInvalid
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", 0, ErrTokenInvalid
	}
	expires, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return "", 0, ErrTokenInvalid
	}
	if now.Unix() > expires {
		return "", 0, ErrTokenExpired
	}
	return parts[1], int(parts[2][0] - '0'), nil
}

// bucket is a token bucket.
type bucket struct {
	level, rate, burst float64
	at                 time.Time
}

func newBucket(rate, burst float64, now time.Time) bucket {
	return bucket{level: burst, rate: rate, burst: burst, at: now}
}

func (b *bucket) take(n float64, now time.Time) bool {
	if dt := now.Sub(b.at).Seconds(); dt > 0 {
		b.level += dt * b.rate
		if b.level > b.burst {
			b.level = b.burst
		}
		b.at = now
	}
	if b.level < n {
		return false
	}
	b.level -= n
	return true
}

type side struct {
	addr     netip.AddrPort
	joined   bool
	lastSeen time.Time
	packets  bucket
	bytes    bucket
}

type session struct {
	id      string
	sides   [2]side
	answers bucket
}

// prefixLimit counts the answers to one source prefix.
type prefixLimit struct {
	errors, joins bucket
	lastSeen      time.Time
}

type sideRef struct {
	s    *session
	side int
}

// Stats counts what the relay did.
type Stats struct {
	Sessions, Joins, Forwarded, Dropped, Refused uint64
}

// Server is a running relay.
type Server struct {
	cfg  Config
	conn *net.UDPConn
	// send writes one datagram (conn's, or a test's).
	send func(b []byte, to netip.AddrPort)

	mu       sync.Mutex
	sessions map[string]*session
	byAddr   map[netip.AddrPort]sideRef
	prefixes map[netip.Prefix]*prefixLimit
	stats    Stats
}

// New makes a relay serving conn (nil for a test that calls Handle).
func New(conn *net.UDPConn, cfg Config) (*Server, error) {
	if len(cfg.Secret) < 16 {
		return nil, errors.New("udp relay: the secret must be at least 16 bytes")
	}
	cfg.defaults()
	s := &Server{
		cfg:      cfg,
		conn:     conn,
		sessions: map[string]*session{},
		byAddr:   map[netip.AddrPort]sideRef{},
		prefixes: map[netip.Prefix]*prefixLimit{},
	}
	if conn != nil {
		s.send = func(b []byte, to netip.AddrPort) { _, _ = conn.WriteToUDPAddrPort(b, to) }
	}
	return s, nil
}

// Serve reads and handles datagrams until the socket is closed, and forgets
// idle sides as it goes.
func (s *Server) Serve() error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		every := s.cfg.Idle / 4
		if every > 10*time.Second {
			every = 10 * time.Second
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				s.Sweep()
			}
		}
	}()
	buf := make([]byte, s.cfg.MaxDatagram+1)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A refused ICMP from a peer that went away, and the like.
			continue
		}
		s.Handle(buf[:n], from)
	}
}

// Stats returns the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	st.Sessions = uint64(len(s.sessions))
	return st
}

// Handle takes one datagram b from from. b is not kept.
func (s *Server) Handle(b []byte, from netip.AddrPort) {
	if len(b) == 0 || len(b) > s.cfg.MaxDatagram {
		s.mu.Lock()
		s.stats.Dropped++
		s.mu.Unlock()
		return
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	now := s.cfg.Now()
	switch b[0] {
	case MsgJoin:
		s.join(b, from, now)
	case MsgData:
		s.forward(b, from, now)
	case MsgKeepalive:
		s.keepalive(b, from, now)
	default:
		s.mu.Lock()
		s.stats.Dropped++
		s.mu.Unlock()
	}
}

// prefixOf is the prefix answers to addr are counted under: its /24, or
// its /64 for IPv6.
func prefixOf(addr netip.Addr) netip.Prefix {
	bits := 24
	if !addr.Is4() {
		bits = 64
	}
	p, _ := addr.Prefix(bits)
	return p
}

// limitOf returns the answer counters of to's prefix, nil when the relay
// counts MaxPrefixes already. Called with s.mu held.
func (s *Server) limitOf(to netip.AddrPort, now time.Time) *prefixLimit {
	key := prefixOf(to.Addr())
	l := s.prefixes[key]
	if l == nil {
		if len(s.prefixes) >= s.cfg.MaxPrefixes {
			return nil
		}
		l = &prefixLimit{
			errors: newBucket(s.cfg.AnswerRate, 2*s.cfg.AnswerRate, now),
			joins:  newBucket(s.cfg.JoinAnswerRate, s.cfg.JoinAnswerRate, now),
		}
		s.prefixes[key] = l
	}
	l.lastSeen = now
	return l
}

// answerError sends a 2-byte refusal to a datagram of 2 bytes or more that
// holds no valid token, within AnswerRate for to's prefix. Called with s.mu
// held.
func (s *Server) answerError(code byte, to netip.AddrPort, now time.Time) {
	if s.send == nil {
		return
	}
	if l := s.limitOf(to, now); l == nil || !l.errors.take(1, now) {
		return
	}
	s.send([]byte{MsgError, code}, to)
}

// answerJoin sends an answer to a join with a valid token, within
// JoinAnswerRate for to's prefix (and within the session's allowance when
// ses is not nil). It is never longer than the join. Called with s.mu held.
func (s *Server) answerJoin(b []byte, to netip.AddrPort, ses *session, now time.Time) {
	if s.send == nil {
		return
	}
	if l := s.limitOf(to, now); l != nil && !l.joins.take(1, now) {
		return
	}
	if ses != nil && !ses.answers.take(1, now) {
		return
	}
	s.send(b, to)
}

// cookie is the proof that a game receives at from: an HMAC of the address
// and a time window (cookieWindow seconds).
func (s *Server) cookie(from netip.AddrPort, window int64) string {
	return mac(s.cfg.Secret, "cookie|"+from.String()+"|"+strconv.FormatInt(window, 10))[:CookieLen]
}

func (s *Server) validCookie(c string, from netip.AddrPort, now time.Time) bool {
	if len(c) != CookieLen {
		return false
	}
	w := now.Unix() / cookieWindow
	return hmac.Equal([]byte(c), []byte(s.cookie(from, w))) || hmac.Equal([]byte(c), []byte(s.cookie(from, w-1)))
}

func (s *Server) join(b []byte, from netip.AddrPort, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b) < 2 {
		s.stats.Refused++
		return
	}
	token, cookie := string(b[1:]), ""
	if cut := strings.IndexByte(token, '|'); cut >= 0 {
		token, cookie = token[:cut], token[cut+1:]
	}
	id, sd, err := Verify(s.cfg.Secret, token, now)
	if err != nil {
		s.stats.Refused++
		code := byte(ErrBadToken)
		if errors.Is(err, ErrTokenExpired) {
			code = ErrExpired
		}
		s.answerError(code, from, now)
		return
	}
	ses := s.sessions[id]
	if ses != nil && ses.sides[sd].joined && ses.sides[sd].addr == from {
		// The side joins again from its address (a lost answer): no proof
		// needed, it gave it.
		me := &ses.sides[sd]
		me.lastSeen = now
		s.answerJoin([]byte{MsgAck, s.stateOf(ses, sd)}, from, ses, now)
		return
	}
	if !s.validCookie(cookie, from, now) {
		// A first join from this address: prove it receives here first.
		s.answerJoin(append([]byte{MsgCookie}, s.cookie(from, now.Unix()/cookieWindow)...), from, nil, now)
		return
	}
	if old, ok := s.byAddr[from]; ok && old.s != ses {
		// The address holds a side of another session. It moves on only
		// when that side went quiet (its game is gone and the NAT gave the
		// port away); an active side keeps it.
		if now.Sub(old.s.sides[old.side].lastSeen) < addrReclaimIdle {
			s.stats.Refused++
			s.answerJoin([]byte{MsgError, ErrAddrInUse}, from, nil, now)
			return
		}
		s.unjoin(old.s, old.side)
	}
	if ses == nil {
		if len(s.sessions) >= s.cfg.MaxSessions {
			s.stats.Refused++
			s.answerJoin([]byte{MsgError, ErrFull}, from, nil, now)
			return
		}
		ses = &session{id: id, answers: newBucket(s.cfg.SessionAnswerRate, s.cfg.SessionAnswerRate, now)}
		s.sessions[id] = ses
	}
	if old, ok := s.byAddr[from]; ok && old.s == ses && old.side != sd {
		// Both sides of one session from one address: the other side's game
		// moved on.
		s.unjoin(ses, old.side)
		if s.sessions[id] == nil {
			s.sessions[id] = ses
		}
	}
	me := &ses.sides[sd]
	// A first join, or a rejoin from a new address (the NAT mapped the game
	// anew): the old address gets nothing more.
	if me.joined {
		delete(s.byAddr, me.addr)
	}
	wasJoined := me.joined
	me.addr, me.joined = from, true
	if !wasJoined {
		me.packets = newBucket(s.cfg.PacketsPerSecond, s.cfg.PacketBurst, now)
		me.bytes = newBucket(s.cfg.BytesPerSecond, s.cfg.ByteBurst, now)
	}
	s.byAddr[from] = sideRef{s: ses, side: sd}
	s.stats.Joins++
	me.lastSeen = now
	other := &ses.sides[1-sd]
	if other.joined && s.send != nil && ses.answers.take(1, now) {
		// The other side learns at once that its peer is here. It proved
		// its address, and the answer is 2 bytes.
		s.send([]byte{MsgAck, StatePaired}, other.addr)
	}
	s.answerJoin([]byte{MsgAck, s.stateOf(ses, sd)}, from, ses, now)
}

// stateOf is side sd's join state: paired when the other side joined.
func (s *Server) stateOf(ses *session, sd int) byte {
	if ses.sides[1-sd].joined {
		return StatePaired
	}
	return StateWaiting
}

// keepalive keeps a side's address alive, and answers it with the side's
// state, so the game knows the relay still has it; from an address that has
// not joined, it says to join again. A 1-byte keepalive is not answered.
func (s *Server) keepalive(b []byte, from netip.AddrPort, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.byAddr[from]
	if !ok {
		if len(b) >= 2 {
			s.answerError(ErrNotJoined, from, now)
		}
		return
	}
	ref.s.sides[ref.side].lastSeen = now
	if len(b) >= 2 {
		s.answerJoin([]byte{MsgAck, s.stateOf(ref.s, ref.side)}, from, ref.s, now)
	}
}

func (s *Server) unjoin(ses *session, sd int) {
	sp := &ses.sides[sd]
	if sp.joined && s.byAddr[sp.addr].s == ses {
		delete(s.byAddr, sp.addr)
	}
	*sp = side{}
	if !ses.sides[0].joined && !ses.sides[1].joined {
		delete(s.sessions, ses.id)
	}
}

func (s *Server) forward(b []byte, from netip.AddrPort, now time.Time) {
	s.mu.Lock()
	ref, ok := s.byAddr[from]
	if !ok {
		// Not from an address that joined: dropped, and answered with 2
		// bytes (no more than came) so that a game whose address changed
		// joins again.
		s.stats.Dropped++
		if len(b) >= 2 {
			s.answerError(ErrNotJoined, from, now)
		}
		s.mu.Unlock()
		return
	}
	me := &ref.s.sides[ref.side]
	me.lastSeen = now
	other := ref.s.sides[1-ref.side]
	if !other.joined || len(b) < 2 || !me.packets.take(1, now) || !me.bytes.take(float64(len(b)), now) {
		s.stats.Dropped++
		s.mu.Unlock()
		return
	}
	s.stats.Forwarded++
	send := s.send
	s.mu.Unlock()
	if send != nil {
		send(b, other.addr)
	}
}

// Sweep forgets the sides idle for Config.Idle, the sessions left without
// sides, and the answer counters of quiet prefixes. Serve calls it every
// Idle/4, and at least every 10 s.
func (s *Server) Sweep() {
	now := s.cfg.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ses := range s.sessions {
		for sd := range ses.sides {
			if ses.sides[sd].joined && now.Sub(ses.sides[sd].lastSeen) > s.cfg.Idle {
				s.unjoin(ses, sd)
			}
		}
	}
	// The answer counters of prefixes quiet for 10 s are full again anyway.
	for key, l := range s.prefixes {
		if now.Sub(l.lastSeen) > 10*time.Second {
			delete(s.prefixes, key)
		}
	}
}
