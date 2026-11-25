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
// they hold a token, forwards what it gets byte for byte, and answers a
// datagram with at most 2 bytes, never more than the datagram it answers.
// Each side is limited in datagrams and bytes per second, and a side that
// sends nothing for Config.Idle is forgotten.
//
// Wire format, every datagram starting with a type byte:
//
//	game -> relay   'J' token       join (or rejoin from a new address)
//	                'D' payload     data for the other side
//	                'K'             keepalive
//	relay -> game   'A' state       join acknowledged: 1 waiting, 2 paired
//	                'E' code        join refused, or data from an address
//	                                that has not joined (Err* codes)
//	                'D' payload     data from the other side
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
)

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
	// AnswerRate bounds the answers ('A', 'E') to joins, a second, over all
	// sources. Default 2000.
	AnswerRate float64
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
		c.AnswerRate = 2000
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
	id    string
	sides [2]side
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
	answers  bucket
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
	}
	s.answers = newBucket(cfg.AnswerRate, cfg.AnswerRate, cfg.Now())
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
		t := time.NewTicker(s.cfg.Idle / 4)
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
		s.mu.Lock()
		if ref, ok := s.byAddr[from]; ok {
			ref.s.sides[ref.side].lastSeen = now
		}
		s.mu.Unlock()
	default:
		s.mu.Lock()
		s.stats.Dropped++
		s.mu.Unlock()
	}
}

// answer sends a 2-byte answer to a datagram of 2 bytes or more, within
// AnswerRate. Called with s.mu held.
func (s *Server) answer(kind, code byte, to netip.AddrPort, now time.Time) {
	if s.send == nil || !s.answers.take(1, now) {
		return
	}
	s.send([]byte{kind, code}, to)
}

func (s *Server) join(b []byte, from netip.AddrPort, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b) < 2 {
		s.stats.Refused++
		return
	}
	id, sd, err := Verify(s.cfg.Secret, string(b[1:]), now)
	if err != nil {
		s.stats.Refused++
		code := byte(ErrBadToken)
		if errors.Is(err, ErrTokenExpired) {
			code = ErrExpired
		}
		s.answer(MsgError, code, from, now)
		return
	}
	ses := s.sessions[id]
	if ses == nil {
		if len(s.sessions) >= s.cfg.MaxSessions {
			s.stats.Refused++
			s.answer(MsgError, ErrFull, from, now)
			return
		}
		ses = &session{id: id}
		s.sessions[id] = ses
	}
	me := &ses.sides[sd]
	if !me.joined || me.addr != from {
		// A first join, or a rejoin from a new address (the NAT mapped the
		// game anew): the old address gets nothing more.
		if me.joined {
			delete(s.byAddr, me.addr)
		}
		if old, ok := s.byAddr[from]; ok && (old.s != ses || old.side != sd) {
			// The address held another side: it moved on.
			s.unjoin(old.s, old.side)
		}
		wasJoined := me.joined
		me.addr, me.joined = from, true
		if !wasJoined {
			me.packets = newBucket(s.cfg.PacketsPerSecond, s.cfg.PacketBurst, now)
			me.bytes = newBucket(s.cfg.BytesPerSecond, s.cfg.ByteBurst, now)
		}
		s.byAddr[from] = sideRef{s: ses, side: sd}
		s.stats.Joins++
		other := &ses.sides[1-sd]
		if other.joined && s.send != nil {
			// The other side learns at once that its peer is here. It
			// proved its address, and the answer is 2 bytes.
			s.send([]byte{MsgAck, StatePaired}, other.addr)
		}
	}
	me.lastSeen = now
	state := byte(StateWaiting)
	if ses.sides[1-sd].joined {
		state = StatePaired
	}
	s.answer(MsgAck, state, from, now)
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
			s.answer(MsgError, ErrNotJoined, from, now)
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

// Sweep forgets the sides idle for Config.Idle, and the sessions left
// without sides. Serve calls it every Idle/4.
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
}
