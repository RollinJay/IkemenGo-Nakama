package main

// The UDP relay path (nakama_udprelay.go) against the relay program's
// package (nakama/udp-relay/relay) on a localhost port, the RPC's ticket as
// the client reads it, and the server module's tickets
// (nakama/modules/ikemen_relay.lua) checked by the relay's Verify.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"

	udprelay "github.com/ikemen-engine/Ikemen-GO/nakama/udp-relay/relay"
)

var udpRelayTestSecret = []byte("udp-relay-test-secret-0123456789")

// startUDPRelay runs a relay on a random localhost port.
func startUDPRelay(t *testing.T, cfg udprelay.Config) (*udprelay.Server, *net.UDPConn) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Secret = udpRelayTestSecret
	srv, err := udprelay.New(conn, cfg)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { conn.Close() })
	return srv, conn
}

func udpRelayTestTicket(addr string, side int, peer string) udpRelayTicket {
	sess := udprelay.SessionID(udpRelayTestSecret, "match.node", "alice", "bob")
	exp := time.Now().Unix() + 60
	return udpRelayTicket{Addr: addr, Token: udprelay.Sign(udpRelayTestSecret, sess, side, exp), Expires: exp, Peer: peer, Session: sess}
}

// udpRelayTestCrypts are the two players' p2pCrypts: each seals with the key
// the other opens with.
func udpRelayTestCrypts(t *testing.T) (a, b *atomic.Pointer[p2pCrypt]) {
	t.Helper()
	k1, k2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	ca, err := newP2PCrypt(k1, k2)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := newP2PCrypt(k2, k1)
	if err != nil {
		t.Fatal(err)
	}
	a, b = &atomic.Pointer[p2pCrypt]{}, &atomic.Pointer[p2pCrypt]{}
	a.Store(ca)
	b.Store(cb)
	return a, b
}

type udpRelayInbox struct {
	crypt *atomic.Pointer[p2pCrypt]
	got   chan []byte
}

func newUDPRelayInbox(c *atomic.Pointer[p2pCrypt]) *udpRelayInbox {
	return &udpRelayInbox{crypt: c, got: make(chan []byte, 64)}
}

func (in *udpRelayInbox) deliver(b []byte) {
	if d, ok := in.crypt.Load().open(b); ok {
		in.got <- d
	}
}

func (in *udpRelayInbox) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case d := <-in.got:
		if string(d) != want {
			t.Fatalf("got %q, want %q", d, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%q did not arrive", want)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Two players' paths meet in the relay and carry sealed datagrams both ways;
// the relay sees only sealed bytes.
func TestUDPRelayEndToEnd(t *testing.T) {
	srv, conn := startUDPRelay(t, udprelay.Config{})
	addr := conn.LocalAddr().String()
	ca, cb := udpRelayTestCrypts(t)
	ina, inb := newUDPRelayInbox(ca), newUDPRelayInbox(cb)

	pa, err := dialUDPRelay(udpRelayTestTicket(addr, 0, "bob"), ina.deliver)
	if err != nil {
		t.Fatal(err)
	}
	a := pa.(*p2pUDPRelayPath)
	defer a.close()
	if a.name() != "relay-udp" || a.isPaired() {
		t.Fatalf("path %q paired %v before bob joined", a.name(), a.isPaired())
	}
	// No keys yet: nothing goes out unsealed.
	if err := a.write([]byte("x"), a.routeAddr()); !errors.Is(err, errP2PNoKeys) {
		t.Fatalf("write without keys: %v", err)
	}
	a.setCrypt(ca)

	b, err := dialUDPRelayPath(udpRelayTestTicket(addr, 1, "alice"), inb.deliver)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	b.setCrypt(cb)
	waitFor(t, "both sides paired", func() bool { return a.isPaired() && b.isPaired() })

	if err := a.write([]byte("hello bob"), a.routeAddr()); err != nil {
		t.Fatal(err)
	}
	inb.expect(t, "hello bob")
	if err := b.write([]byte("hello alice"), b.routeAddr()); err != nil {
		t.Fatal(err)
	}
	ina.expect(t, "hello alice")
	for i := 0; i < 50; i++ {
		_ = a.write([]byte("frame "+strconv.Itoa(i)), nil)
	}
	for i := 0; i < 50; i++ {
		inb.expect(t, "frame "+strconv.Itoa(i))
	}
	if st := srv.Stats(); st.Sessions != 1 || st.Forwarded < 52 {
		t.Fatalf("relay stats %+v", st)
	}
	a.close()
	if err := a.write([]byte("late"), nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
}

// udpNAT forwards a client's datagrams to the relay from a socket of its
// own, as a NAT does, and can map the client anew (another source port).
type udpNAT struct {
	t      *testing.T
	front  *net.UDPConn
	target *net.UDPAddr
	mu     sync.Mutex
	back   *net.UDPConn
	client *net.UDPAddr
}

func newUDPNAT(t *testing.T, target *net.UDPAddr) *udpNAT {
	front, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	n := &udpNAT{t: t, front: front, target: target}
	n.rebind()
	t.Cleanup(func() {
		front.Close()
		n.mu.Lock()
		n.back.Close()
		n.mu.Unlock()
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			k, from, err := front.ReadFromUDP(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			n.client = from
			back := n.back
			n.mu.Unlock()
			_, _ = back.WriteToUDP(buf[:k], target)
		}
	}()
	return n
}

// rebind maps the client anew: what it sends leaves from another port, and
// what comes to the old one is lost.
func (n *udpNAT) rebind() {
	back, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		n.t.Fatal(err)
	}
	n.mu.Lock()
	old := n.back
	n.back = back
	n.mu.Unlock()
	if old != nil {
		old.Close()
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			k, _, err := back.ReadFromUDP(buf)
			if err != nil {
				return
			}
			n.mu.Lock()
			client := n.client
			n.mu.Unlock()
			if client != nil {
				_, _ = n.front.WriteToUDP(buf[:k], client)
			}
		}
	}()
}

// A player behind a NAT that maps it anew mid-session joins again on the
// relay's word, and the session goes on.
func TestUDPRelayNATRebinding(t *testing.T) {
	_, conn := startUDPRelay(t, udprelay.Config{})
	nat := newUDPNAT(t, conn.LocalAddr().(*net.UDPAddr))
	ca, cb := udpRelayTestCrypts(t)
	ina, inb := newUDPRelayInbox(ca), newUDPRelayInbox(cb)
	a, err := dialUDPRelayPath(udpRelayTestTicket(nat.front.LocalAddr().String(), 0, "bob"), ina.deliver)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	a.setCrypt(ca)
	b, err := dialUDPRelayPath(udpRelayTestTicket(conn.LocalAddr().String(), 1, "alice"), inb.deliver)
	if err != nil {
		t.Fatal(err)
	}
	defer b.close()
	b.setCrypt(cb)
	waitFor(t, "both sides paired", func() bool { return a.isPaired() && b.isPaired() })
	_ = a.write([]byte("before"), nil)
	inb.expect(t, "before")

	nat.rebind()
	// The first datagram from the new port is refused ("not joined"), which
	// makes the path join again; later ones go through.
	deadline := time.Now().Add(3 * time.Second)
	for {
		_ = a.write([]byte("after"), nil)
		select {
		case d := <-inb.got:
			if string(d) != "after" {
				t.Fatalf("got %q", d)
			}
			_ = b.write([]byte("back"), nil)
			ina.expect(t, "back")
			return
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not recover from the NAT's new mapping")
		}
	}
}

// A forged or expired ticket fails at once; a relay that does not answer
// fails within the dial timeout.
func TestUDPRelayDialFailures(t *testing.T) {
	_, conn := startUDPRelay(t, udprelay.Config{})
	tk := udpRelayTestTicket(conn.LocalAddr().String(), 0, "bob")
	tk.Token = strings.Replace(tk.Token, "v1.", "v1.0", 1)
	if _, err := dialUDPRelay(tk, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("forged ticket: %v", err)
	}
	tk = udpRelayTestTicket(conn.LocalAddr().String(), 0, "bob")
	tk.Token = udprelay.Sign(udpRelayTestSecret, tk.Session, 0, time.Now().Unix()-5)
	if _, err := dialUDPRelay(tk, nil); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("expired ticket: %v", err)
	}
	silent, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	start := time.Now()
	if _, err := dialUDPRelay(udpRelayTestTicket(silent.LocalAddr().String(), 0, "bob"), nil); err == nil {
		t.Fatal("a silent relay was joined")
	}
	if d := time.Since(start); d > udpRelayDialTimeout+time.Second {
		t.Fatalf("gave up after %v", d)
	}
}

func TestUDPRelayParseReply(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tk, err := parseUDPRelayReply([]byte(`{"udp":{"addr":":17360","token":"v1.x","expires":1800000300,"session":"s"}}`), "bob", "nakama.example", now)
	if err != nil || tk.Addr != "nakama.example:17360" || tk.Token != "v1.x" || tk.Expires != 1800000300 || tk.Peer != "bob" || tk.Session != "s" {
		t.Fatalf("ticket %+v, %v", tk, err)
	}
	tk, err = parseUDPRelayReply([]byte(`{"udp":{"addr":"[2001:db8::1]:9","token":"t","expires":1.8000003e9,"session":"s"}}`), "bob", "h", now)
	if err != nil || tk.Addr != "[2001:db8::1]:9" || tk.Expires != 1800000300 {
		t.Fatalf("ticket %+v, %v", tk, err)
	}
	if _, err := parseUDPRelayReply([]byte(`{"match_id":"x.node"}`), "bob", "h", now); !errors.Is(err, errNakamaNoUDPRelay) {
		t.Fatalf("websocket relay answer: %v", err)
	}
	for _, body := range []string{
		`not json`,
		`{"udp":{"addr":"","token":"t","expires":1800000300}}`,
		`{"udp":{"addr":"h:1","token":"","expires":1800000300}}`,
		`{"udp":{"addr":"h","token":"t","expires":1800000300}}`,
		`{"udp":{"addr":"h:1","token":"t"}}`,
		`{"udp":{"addr":"h:1","token":"t","expires":1799999999}}`,
		`{"udp":{"addr":"h:1","token":"` + strings.Repeat("t", 300) + `","expires":1800000300}}`,
	} {
		if _, err := parseUDPRelayReply([]byte(body), "bob", "h", now); err == nil || errors.Is(err, errNakamaNoUDPRelay) {
			t.Errorf("%s: %v", body, err)
		}
	}
}

// RequestUDPRelay calls ikemen_relay with udp=true and reads the ticket; a
// server without a UDP relay, or without the module, says so.
func TestUDPRelayRequest(t *testing.T) {
	var answer atomic.Value
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/rpc/"+nakamaRelayRPC {
			http.Error(w, `{"code":5,"message":"RPC function not found"}`, http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got.Store(body)
		a := answer.Load().(string)
		if a == "404" {
			http.Error(w, `{"code":5,"message":"RPC function not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(a))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	n, _ := relayTestClient(t, 64)
	n.mu.Lock()
	n.cfg.Host, n.cfg.Port, n.cfg.RequestTTL = u.Hostname(), port, 2*time.Second
	n.session = &NakamaSession{Token: "token", UserID: "me"}
	n.mu.Unlock()

	exp := time.Now().Unix() + 300
	answer.Store(`{"udp":{"addr":":17360","token":"tok","expires":` + strconv.FormatInt(exp, 10) + `,"session":"s"}}`)
	tk, err := n.RequestUDPRelay("match.node", "peer")
	if err != nil || tk.Addr != net.JoinHostPort(u.Hostname(), "17360") || tk.Token != "tok" || tk.Peer != "peer" {
		t.Fatalf("ticket %+v, %v", tk, err)
	}
	body := got.Load().(map[string]any)
	if body["match_id"] != "match.node" || body["peer_id"] != "peer" || body["udp"] != true {
		t.Fatalf("RPC body %v", body)
	}
	answer.Store(`{"match_id":"3b4e28ba-2fa1-11d2-883f-0016d3cca427.nakama1"}`)
	if _, err := n.RequestUDPRelay("match.node", "peer"); !errors.Is(err, errNakamaNoUDPRelay) {
		t.Fatalf("server without a UDP relay: %v", err)
	}
	answer.Store("404")
	if _, err := n.RequestUDPRelay("match.node", "peer"); !errors.Is(err, errNakamaNoServerRelay) {
		t.Fatalf("server without the module: %v", err)
	}
}

// The server module issues tickets the relay accepts, for the pair alone,
// with the same session for both and a side each, and makes no relay match
// for them; without the relay's settings it answers as before.
func TestUDPRelayLuaTickets(t *testing.T) {
	m := loadRelayLua(t)
	m.l.SetGlobal("go_hmac", m.l.NewFunction(func(l *lua.LState) int {
		h := hmac.New(sha256.New, []byte(l.CheckString(2)))
		h.Write([]byte(l.CheckString(1)))
		l.Push(lua.LString(h.Sum(nil)))
		return 1
	}))
	m.l.SetGlobal("go_hex", m.l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString(hex.EncodeToString([]byte(l.CheckString(1)))))
		return 1
	}))
	m.run(t, `
local nk = require("nakama")
nk.hmac_sha256_hash = go_hmac
nk.base16_encode = go_hex
local parent = { match_id = "parent.node" }
local parent_state = {}
signal_targets["parent.node"] = function(data)
    return relay.answer(parent, parent_state, data, function(x, y)
        return (x == "alice" and y == "bob") or (x == "bob" and y == "alice")
    end)
end
function call_udp(user_id, env, payload)
    local ok, result = pcall(rpcs.ikemen_relay, { user_id = user_id, env = env }, payload)
    if ok then
        return result, nil
    end
    if type(result) == "table" then
        return nil, result[2]
    end
    return nil, tostring(result)
end
`)
	call := func(user, env, payload string) (string, string) {
		t.Helper()
		m.run(t, `res, code = call_udp("`+user+`", `+env+`, '`+payload+`')`)
		return m.l.GetGlobal("res").String(), m.l.GetGlobal("code").String()
	}
	created := func() int { m.run(t, `n_created = #created`); return int(m.l.GetGlobal("n_created").(lua.LNumber)) }
	env := `{ IKEMEN_UDP_RELAY_ADDR = ":17360", IKEMEN_UDP_RELAY_SECRET = "` + string(udpRelayTestSecret) + `" }`
	now := time.Now()
	sides := map[int]string{}
	var session string
	for _, user := range []string{"alice", "bob"} {
		peer := map[string]string{"alice": "bob", "bob": "alice"}[user]
		res, code := call(user, env, `{"match_id":"parent.node","peer_id":"`+peer+`","udp":true}`)
		if code != "nil" {
			t.Fatalf("%s: error %s", user, code)
		}
		tk, err := parseUDPRelayReply([]byte(res), peer, "nakama.example", now)
		if err != nil {
			t.Fatalf("%s: %s: %v", user, res, err)
		}
		if tk.Addr != "nakama.example:17360" || tk.Expires < now.Unix()+250 {
			t.Fatalf("%s: ticket %+v", user, tk)
		}
		sess, side, err := udprelay.Verify(udpRelayTestSecret, tk.Token, now)
		if err != nil || sess != tk.Session {
			t.Fatalf("%s: the relay refuses %q: %v", user, tk.Token, err)
		}
		if want := udprelay.SessionID(udpRelayTestSecret, "parent.node", "alice", "bob"); sess != want {
			t.Fatalf("%s: session %s, the relay's derivation %s", user, sess, want)
		}
		if session != "" && sess != session {
			t.Fatal("the two players got different sessions")
		}
		session = sess
		sides[side] = user
	}
	if sides[0] != "alice" || sides[1] != "bob" {
		t.Fatalf("sides %v", sides)
	}
	if n := created(); n != 0 {
		t.Fatalf("UDP tickets made %d relay matches", n)
	}
	// Someone the session did not pair gets nothing.
	if res, code := call("mallory", env, `{"match_id":"parent.node","peer_id":"alice","udp":true}`); code != "9" {
		t.Fatalf("mallory: %s %s", res, code)
	}
	// Without the relay's settings, or with a short secret, udp=true gets the
	// websocket relay as before; and without udp, too.
	for _, e := range []string{`{}`, `{ IKEMEN_UDP_RELAY_ADDR = ":1", IKEMEN_UDP_RELAY_SECRET = "short" }`, env} {
		payload := `{"match_id":"parent.node","peer_id":"bob","udp":true}`
		if e == env {
			payload = `{"match_id":"parent.node","peer_id":"bob"}`
		}
		res, code := call("alice", e, payload)
		if code != "nil" || !strings.HasPrefix(res, `{"match_id":"`) {
			t.Fatalf("env %s: %s %s", e, res, code)
		}
	}
	if n := created(); n != 1 {
		t.Fatalf("websocket relays made: %d", n)
	}
}
