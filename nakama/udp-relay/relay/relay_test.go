package udprelay

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef-test-secret")

type sent struct {
	b  []byte
	to netip.AddrPort
}

// testRelay is a Server on a fake clock whose datagrams are recorded.
type testRelay struct {
	*Server
	now  time.Time
	out  []sent
	sess string
}

func newTestRelay(t *testing.T, cfg Config) *testRelay {
	t.Helper()
	r := &testRelay{now: time.Unix(1_800_000_000, 0)}
	cfg.Secret = testSecret
	cfg.Now = func() time.Time { return r.now }
	srv, err := New(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv.send = func(b []byte, to netip.AddrPort) {
		r.out = append(r.out, sent{append([]byte(nil), b...), to})
	}
	r.Server = srv
	r.sess = SessionID(testSecret, "match.node", "alice", "bob")
	return r
}

func (r *testRelay) take() []sent {
	out := r.out
	r.out = nil
	return out
}

// join joins side of the test session from from, through the cookie
// exchange when the relay asks for it; the answers after the cookie stay in
// r.out.
func (r *testRelay) join(side int, from netip.AddrPort) {
	r.joinToken(Sign(testSecret, r.sess, side, r.now.Unix()+60), from)
}

func (r *testRelay) joinToken(token string, from netip.AddrPort) {
	before := len(r.out)
	r.Handle(append([]byte{MsgJoin}, token...), from)
	if len(r.out) == before+1 && r.out[before].b[0] == MsgCookie && r.out[before].to == from {
		cookie := r.out[before].b[1:]
		r.out = r.out[:before]
		r.Handle([]byte(string(MsgJoin)+token+"|"+string(cookie)), from)
	}
}

var (
	addrA  = netip.MustParseAddrPort("192.0.2.1:4000")
	addrB  = netip.MustParseAddrPort("198.51.100.2:5000")
	addrA2 = netip.MustParseAddrPort("192.0.2.1:4001")
	addrX  = netip.MustParseAddrPort("203.0.113.9:6000")
)

func TestTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	sess := SessionID(testSecret, "m.node", "b", "a")
	if sess != SessionID(testSecret, "m.node", "a", "b") || len(sess) != 32 {
		t.Fatalf("session %q depends on the order of the players", sess)
	}
	tok := Sign(testSecret, sess, 1, now.Unix()+10)
	s, side, err := Verify(testSecret, tok, now)
	if err != nil || s != sess || side != 1 {
		t.Fatalf("Verify = %q %d %v", s, side, err)
	}
	if _, _, err := Verify(testSecret, tok, now.Add(11*time.Second)); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired token: %v", err)
	}
	if _, _, err := Verify([]byte("another-secret-of-16"), tok, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("other secret: %v", err)
	}
	bad := []string{
		"",
		"v1",
		strings.Replace(tok, ".1.", ".0.", 1), // side changed
		tok[:len(tok)-1] + "0",
		Sign(testSecret, "short", 0, now.Unix()+10),
		Sign(testSecret, strings.Repeat("z", 32), 0, now.Unix()+10),
		Sign(testSecret, sess, 2, now.Unix()+10),
		strings.Replace(Sign(testSecret, sess, 0, now.Unix()+10), "v1.", "v2.", 1),
		strings.Repeat("a", MaxToken+1),
	}
	for _, b := range bad {
		if _, _, err := Verify(testSecret, b, now); err == nil {
			t.Errorf("Verify accepted %q", b)
		}
	}
	if _, err := New(nil, Config{Secret: []byte("short")}); err == nil {
		t.Error("a short secret was accepted")
	}
}

func TestPairingAndForwarding(t *testing.T) {
	r := newTestRelay(t, Config{})
	// Data before joining goes nowhere; the answer says to join.
	r.Handle([]byte("Dhello"), addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgError, ErrNotJoined}) || out[0].to != addrA {
		t.Fatalf("data before joining: %v", out)
	}
	r.join(0, addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StateWaiting}) {
		t.Fatalf("first join: %v", out)
	}
	// Alone in the session: data is dropped, silently.
	r.Handle([]byte("Dhello"), addrA)
	if out := r.take(); len(out) != 0 {
		t.Fatalf("data without a peer: %v", out)
	}
	r.join(1, addrB)
	out := r.take()
	if len(out) != 2 || out[0].to != addrA || !bytes.Equal(out[0].b, []byte{MsgAck, StatePaired}) ||
		out[1].to != addrB || !bytes.Equal(out[1].b, []byte{MsgAck, StatePaired}) {
		t.Fatalf("second join: %v", out)
	}
	r.Handle([]byte("Dfrom a"), addrA)
	r.Handle([]byte("Dfrom b"), addrB)
	out = r.take()
	if len(out) != 2 || out[0].to != addrB || string(out[0].b) != "Dfrom a" || out[1].to != addrA || string(out[1].b) != "Dfrom b" {
		t.Fatalf("forwarding: %v", out)
	}
	// A stranger's data goes nowhere, and unknown types are dropped.
	r.Handle([]byte("Dspoof"), addrX)
	r.Handle([]byte("Zjunk"), addrA)
	for _, o := range r.take() {
		if o.to != addrX || len(o.b) != 2 {
			t.Fatalf("stranger's data: %v", o)
		}
	}
	// A rejoin from a new address (NAT rebinding) moves the side.
	r.join(0, addrA2)
	r.take()
	r.Handle([]byte("Dto a"), addrB)
	r.Handle([]byte("Dold a"), addrA)
	out = r.take()
	if len(out) != 2 || out[0].to != addrA2 || string(out[0].b) != "Dto a" || out[1].to != addrA || out[1].b[0] != MsgError {
		t.Fatalf("after rebinding: %v", out)
	}
	if st := r.Stats(); st.Sessions != 1 || st.Forwarded != 3 {
		t.Fatalf("stats %+v", st)
	}
}

// No answer is ever larger than what it answers.
func TestNoAmplification(t *testing.T) {
	r := newTestRelay(t, Config{})
	inputs := [][]byte{{MsgJoin}, {MsgJoin, 'x'}, []byte("Jv1.bad"), {MsgData}, {MsgData, 1}, {MsgKeepalive}, {MsgKeepalive, 0}, {'?', 1, 2},
		append([]byte{MsgJoin}, Sign(testSecret, r.sess, 0, r.now.Unix()+60)...)}
	for _, in := range inputs {
		r.Handle(in, addrX)
		for _, o := range r.take() {
			if len(o.b) > len(in) {
				t.Errorf("answer %q to %q is larger", o.b, in)
			}
		}
	}
}

func TestExpiredAndForgedJoins(t *testing.T) {
	r := newTestRelay(t, Config{})
	r.Handle(append([]byte{MsgJoin}, Sign(testSecret, r.sess, 0, r.now.Unix()-1)...), addrA)
	r.Handle(append([]byte{MsgJoin}, Sign([]byte("not the relay's secret!"), r.sess, 0, r.now.Unix()+60)...), addrB)
	out := r.take()
	if len(out) != 2 || !bytes.Equal(out[0].b, []byte{MsgError, ErrExpired}) || !bytes.Equal(out[1].b, []byte{MsgError, ErrBadToken}) {
		t.Fatalf("answers %v", out)
	}
	if st := r.Stats(); st.Sessions != 0 || st.Refused != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestRateLimits(t *testing.T) {
	r := newTestRelay(t, Config{PacketsPerSecond: 10, PacketBurst: 5, BytesPerSecond: 1000, ByteBurst: 100})
	r.join(0, addrA)
	r.join(1, addrB)
	r.take()
	for i := 0; i < 8; i++ {
		r.Handle([]byte("D1234"), addrA)
	}
	if n := len(r.take()); n != 5 {
		t.Fatalf("forwarded %d of 8 within a burst of 5", n)
	}
	r.now = r.now.Add(100 * time.Millisecond) // one more datagram
	r.Handle([]byte("D1234"), addrA)
	r.Handle([]byte("D1234"), addrA)
	if n := len(r.take()); n != 1 {
		t.Fatalf("forwarded %d after 100 ms at 10/s", n)
	}
	// The other side has its own allowance.
	r.Handle([]byte("Dok"), addrB)
	if n := len(r.take()); n != 1 {
		t.Fatalf("b forwarded %d", n)
	}
	// Bytes: a burst of 100.
	r.now = r.now.Add(time.Second)
	big := append([]byte{MsgData}, make([]byte, 59)...)
	r.Handle(big, addrB)
	r.Handle(big, addrB)
	if n := len(r.take()); n != 1 {
		t.Fatalf("forwarded %d datagrams of 60 bytes within 100", n)
	}
	// Datagrams over MaxDatagram are dropped.
	r.now = r.now.Add(time.Second)
	r.Handle(append([]byte{MsgData}, make([]byte, 2048)...), addrB)
	if n := len(r.take()); n != 0 {
		t.Fatal("an oversized datagram was forwarded")
	}
}

func TestIdleAndCapacity(t *testing.T) {
	r := newTestRelay(t, Config{Idle: 10 * time.Second, MaxSessions: 1})
	r.join(0, addrA)
	r.join(1, addrB)
	r.take()
	r.now = r.now.Add(6 * time.Second)
	r.Handle([]byte{MsgKeepalive}, addrA)
	r.now = r.now.Add(6 * time.Second)
	r.Sweep()
	// b was idle for 12 s, a for 6.
	r.Handle([]byte("Dx"), addrA)
	r.Handle([]byte("Dx"), addrB)
	out := r.take()
	if len(out) != 1 || out[0].to != addrB || out[0].b[0] != MsgError {
		t.Fatalf("after b idled: %v", out)
	}
	// The relay is full: another session is refused.
	other := SessionID(testSecret, "other.node", "c", "d")
	r.joinToken(Sign(testSecret, other, 0, r.now.Unix()+60), addrX)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgError, ErrFull}) {
		t.Fatalf("join of a full relay: %v", out)
	}
	r.now = r.now.Add(11 * time.Second)
	r.Sweep()
	if st := r.Stats(); st.Sessions != 0 {
		t.Fatalf("sessions after both idled: %d", st.Sessions)
	}
}

func TestAnswerRate(t *testing.T) {
	r := newTestRelay(t, Config{AnswerRate: 3})
	for i := 0; i < 10; i++ {
		r.Handle([]byte("Jforged"), addrX)
	}
	if n := len(r.take()); n != 6 {
		t.Fatalf("answered %d of 10 forged joins at 3/s, burst 6", n)
	}
}

// Junk from one place (a flood of data and forged joins) uses up only that
// place's answers: a valid join from elsewhere is still answered, and so is
// a valid join from the flooded prefix.
func TestJunkDoesNotStarveJoins(t *testing.T) {
	r := newTestRelay(t, Config{})
	for i := 0; i < 5000; i++ {
		r.Handle([]byte("Djunk"), addrX)
		r.Handle([]byte("Jforged"), addrX)
	}
	if n := len(r.take()); n > 10 {
		t.Fatalf("answered %d junk datagrams from one address", n)
	}
	r.join(0, addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StateWaiting}) {
		t.Fatalf("a valid join during the flood: %v", out)
	}
	sameNet := netip.MustParseAddrPort("203.0.113.10:7000")
	r.join(1, sameNet)
	if out := r.take(); len(out) < 1 || !bytes.Equal(out[len(out)-1].b, []byte{MsgAck, StatePaired}) {
		t.Fatalf("a valid join from the flooded prefix: %v", out)
	}
	// Many prefixes: the relay counts at most MaxPrefixes.
	r2 := newTestRelay(t, Config{MaxPrefixes: 4})
	for i := 0; i < 50; i++ {
		r2.Handle([]byte("Djunk"), netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 1}), 9))
	}
	if n := len(r2.prefixes); n != 4 {
		t.Fatalf("%d prefixes counted, want 4", n)
	}
	r2.now = r2.now.Add(11 * time.Second)
	r2.Sweep()
	if n := len(r2.prefixes); n != 0 {
		t.Fatalf("%d prefixes left after a quiet spell", n)
	}
}

// A join from an address must prove the address before it counts: a join
// with a valid token from a forged source only gets the cookie, sent to that
// source, and moves nothing; a cookie made for one address does not join
// from another.
func TestJoinProvesTheAddress(t *testing.T) {
	r := newTestRelay(t, Config{})
	r.join(0, addrA)
	r.join(1, addrB)
	r.take()
	victim := netip.MustParseAddrPort("192.0.2.200:80")
	token := Sign(testSecret, r.sess, 0, r.now.Unix()+60)
	join := append([]byte{MsgJoin}, token...)
	r.Handle(join, victim)
	out := r.take()
	if len(out) != 1 || out[0].to != victim || out[0].b[0] != MsgCookie || len(out[0].b) != 1+CookieLen || len(out[0].b) >= len(join) {
		t.Fatalf("a join from a new address: %v", out)
	}
	// The side stays where it proved it was.
	r.Handle([]byte("Dto a"), addrB)
	if out := r.take(); len(out) != 1 || out[0].to != addrA {
		t.Fatalf("data after a forged join went to %v", out)
	}
	// A cookie for another address does not prove this one.
	r.Handle([]byte("J"+token+"|"+string(out[0].b[1:])), addrX)
	if out := r.take(); len(out) != 1 || out[0].b[0] != MsgCookie || out[0].to != addrX {
		t.Fatalf("a borrowed cookie: %v", out)
	}
	// An old cookie stops working.
	cookie := string(r.Server.cookie(addrA2, r.now.Unix()/cookieWindow))
	r.now = r.now.Add(2 * cookieWindow * time.Second)
	r.Handle([]byte("J"+Sign(testSecret, r.sess, 0, r.now.Unix()+60)+"|"+cookie), addrA2)
	if out := r.take(); len(out) != 1 || out[0].b[0] != MsgCookie {
		t.Fatalf("a stale cookie: %v", out)
	}
	// The same side joining again from its address needs no cookie.
	r.Handle(append([]byte{MsgJoin}, Sign(testSecret, r.sess, 0, r.now.Unix()+60)...), addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StatePaired}) {
		t.Fatalf("a rejoin from the same address: %v", out)
	}
}

// An address that holds an active side of one session cannot be taken over
// by a join for another session, even with a cookie: it is refused, and the
// side stays. Once the side went quiet, the address moves on.
func TestJoinDoesNotKickAnotherSession(t *testing.T) {
	r := newTestRelay(t, Config{})
	r.join(0, addrA)
	r.join(1, addrB)
	r.take()
	other := SessionID(testSecret, "other.node", "c", "d")
	r.joinToken(Sign(testSecret, other, 0, r.now.Unix()+60), addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgError, ErrAddrInUse}) {
		t.Fatalf("a join for another session from an active address: %v", out)
	}
	r.Handle([]byte("Dstill a"), addrB)
	if out := r.take(); len(out) != 1 || out[0].to != addrA || string(out[0].b) != "Dstill a" {
		t.Fatalf("the side was kicked: %v", out)
	}
	r.now = r.now.Add(addrReclaimIdle + time.Second)
	r.Handle([]byte{MsgKeepalive, 0}, addrB)
	r.take()
	r.joinToken(Sign(testSecret, other, 0, r.now.Unix()+60), addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StateWaiting}) {
		t.Fatalf("a join from an address whose side went quiet: %v", out)
	}
	r.Handle([]byte("Dto a"), addrB)
	if out := r.take(); len(out) != 0 {
		t.Fatalf("data for the side that went away: %v", out)
	}
}

// A 2-byte keepalive is answered with the side's state, or, from an address
// that has not joined, with the word to join again.
func TestKeepaliveAnswers(t *testing.T) {
	r := newTestRelay(t, Config{})
	r.join(0, addrA)
	r.take()
	r.Handle([]byte{MsgKeepalive, 0}, addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StateWaiting}) {
		t.Fatalf("keepalive alone: %v", out)
	}
	r.join(1, addrB)
	r.take()
	r.Handle([]byte{MsgKeepalive, 0}, addrA)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgAck, StatePaired}) {
		t.Fatalf("keepalive paired: %v", out)
	}
	r.Handle([]byte{MsgKeepalive, 0}, addrX)
	if out := r.take(); len(out) != 1 || !bytes.Equal(out[0].b, []byte{MsgError, ErrNotJoined}) {
		t.Fatalf("keepalive from a stranger: %v", out)
	}
	r.Handle([]byte{MsgKeepalive}, addrX)
	if out := r.take(); len(out) != 0 {
		t.Fatalf("1-byte keepalive answered: %v", out)
	}
}

// Serve on a localhost socket: two UDP clients meet and talk.
func TestServeUDP(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(conn, Config{Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer conn.Close()
	sess := SessionID(testSecret, "m", "a", "b")
	dial := func(side int) *net.UDPConn {
		c, err := net.DialUDP("udp", nil, conn.LocalAddr().(*net.UDPAddr))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		token := Sign(testSecret, sess, side, time.Now().Unix()+60)
		_, _ = c.Write(append([]byte{MsgJoin}, token...))
		buf := make([]byte, 100)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(buf)
		if err != nil || n != 1+CookieLen || buf[0] != MsgCookie {
			t.Fatalf("no cookie: %q %v", buf[:n], err)
		}
		_, _ = c.Write([]byte(string(MsgJoin) + token + "|" + string(buf[1:n])))
		return c
	}
	read := func(c *net.UDPConn) []byte {
		buf := make([]byte, 100)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n]
	}
	a := dial(0)
	if b := read(a); !bytes.Equal(b, []byte{MsgAck, StateWaiting}) {
		t.Fatalf("a's ack %v", b)
	}
	b := dial(1)
	if got := read(a); !bytes.Equal(got, []byte{MsgAck, StatePaired}) {
		t.Fatalf("a heard %v", got)
	}
	if got := read(b); !bytes.Equal(got, []byte{MsgAck, StatePaired}) {
		t.Fatalf("b's ack %v", got)
	}
	_, _ = a.Write([]byte("Dping"))
	if got := read(b); string(got) != "Dping" {
		t.Fatalf("b got %q", got)
	}
}
