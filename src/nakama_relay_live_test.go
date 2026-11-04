package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ikemen-engine/Ikemen-GO/src/portmap"
)

// A matchmade pair whose UDP paths do not work connects through the server's
// relay (nakama/modules/ikemen_relay.lua), and runs the netplay session
// handshake and a GGPO channel over it. The session match stays the clients'
// match.
func TestNakamaLive_MatchmakerP2PRelaySession(t *testing.T) {
	fixedEnabled(t)
	relaySessionLive(t, true)
}

// Two games whose signals do not offer the server's relay, as with a game
// from before it, connect through a Nakama relayed match instead, which they
// create under the same name, on a server that has the relay module too.
func TestNakamaLive_MatchmakerP2PRelayedMatchSession(t *testing.T) {
	fixedEnabled(t)
	relaySessionLive(t, false)
}

func relaySessionLive(t *testing.T, offerServerRelay bool) {
	// Keep the test off this machine's IPv6 addresses (both clients run
	// here, and would reach each other over them) and off the real router.
	prevIPv6, prevMap := p2pIPv6Addresses, p2pMapPort
	p2pIPv6Addresses = func() ([]net.IP, error) { return nil, nil }
	p2pMapPort = func(context.Context, int) (*portmap.Mapping, error) { return nil, errors.New("no router in tests") }
	t.Cleanup(func() { p2pIPv6Addresses, p2pMapPort = prevIPv6, prevMap })

	a, b := matchPair(t)
	pa, _ := a.Pairing()
	pb, _ := b.Pairing()
	sessionMatch := a.CurrentMatchID()
	if sessionMatch == "" || sessionMatch != b.CurrentMatchID() {
		t.Fatalf("session matches %q and %q", sessionMatch, b.CurrentMatchID())
	}

	// StartP2P, with one difference: each side offers only a localhost port
	// nobody listens on, as if its NAT dropped everything, so the handshake
	// has to reach the relay step.
	start := func(c *NakamaClient, peer string) *NakamaP2P {
		p, err := NewNakamaP2P(sessionMatch, 0, func(s P2PSignal) error {
			s.ServerRelay = s.ServerRelay && offerServerRelay
			return c.PublishSignal(s)
		})
		if err != nil {
			t.Fatal(err)
		}
		p.SetRelay(c)
		p.SetPeer(c.Session().UserID, peer)
		p.relayDelay = 2 * time.Second
		p.local = []P2PCandidate{{Type: "srflx", Address: "127.0.0.1", Port: deadUDPPort(t)}}
		c.mu.Lock()
		c.p2p = p
		pending := c.pendingSignal
		c.pendingSignal = nil
		c.mu.Unlock()
		if pending != nil {
			_ = p.HandleSignal(*pending)
		}
		p.resignal()
		go p.punchLoop()
		return p
	}
	t0 := time.Now()
	p1 := start(a, pa.PeerUserID)
	p2 := start(b, pb.PeerUserID)
	waitP2PReady(t, a, b)
	if p1.Route() != "relay" || p2.Route() != "relay" {
		t.Fatalf("routes %q and %q, want relay", p1.Route(), p2.Route())
	}
	t.Logf("connected over the relay %v after the start", time.Since(t0).Round(time.Millisecond))
	if a.CurrentMatchID() != sessionMatch || b.CurrentMatchID() != sessionMatch {
		t.Fatalf("the relay replaced the session match: %q, %q", a.CurrentMatchID(), b.CurrentMatchID())
	}
	if st := a.Status(); st.P2PRoute != "relay" || st.P2PStage != "relay" {
		t.Fatalf("status route %q stage %q", st.P2PRoute, st.P2PStage)
	}
	for _, c := range []*NakamaClient{a, b} {
		c.relayMu.Lock()
		var relays []string
		for id := range c.relayByMatch {
			relays = append(relays, id)
		}
		_, server := c.relayServer[strings.Join(relays, "")]
		c.relayMu.Unlock()
		if len(relays) != 1 || server != offerServerRelay || nakamaRelayedMatchID(relays[0]) == offerServerRelay {
			t.Fatalf("relays %q (the server's: %v), want the server's relay: %v", relays, server, offerServerRelay)
		}
	}

	streamA, err := a.OpenP2PStream()
	if err != nil {
		t.Fatal(err)
	}
	streamB, err := b.OpenP2PStream()
	if err != nil {
		t.Fatal(err)
	}
	ncA, ncB := NewNetConnection(), NewNetConnection()
	ncA.AttachStream(streamA, pa.Host)
	ncB.AttachStream(streamB, pb.Host)
	deadline := time.Now().Add(10 * time.Second)
	for !ncA.hasConn() || !ncB.hasConn() {
		if time.Now().After(deadline) {
			t.Fatalf("session handshake: %v / %v", ncA.AttachError(), ncB.AttachError())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := ncA.writeJSON(map[string]string{"from": "a"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	_ = streamB.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := ncB.readJSON(&got); err != nil || got["from"] != "a" {
		t.Fatalf("session data over the relay: %v %v", got, err)
	}

	gA, remoteA, err := a.P2PGameplayConn()
	if err != nil {
		t.Fatal(err)
	}
	gB, _, err := b.P2PGameplayConn()
	if err != nil {
		t.Fatal(err)
	}
	defer gA.Close()
	defer gB.Close()
	if remoteA.IP.To4() == nil {
		t.Fatalf("GGPO peer address %v is not IPv4", remoteA)
	}
	// Round trips of GGPO-sized datagrams through the server.
	buf := make([]byte, 64)
	var total time.Duration
	const rounds = 20
	for i := 0; i < rounds; i++ {
		sent := time.Now()
		if _, err := gA.WriteTo([]byte("rollback"), remoteA); err != nil {
			t.Fatal(err)
		}
		_ = gB.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, from, err := gB.ReadFrom(buf)
		if err != nil || string(buf[:n]) != "rollback" {
			t.Fatalf("gameplay datagram over the relay: %q %v", buf[:n], err)
		}
		if _, err := gB.WriteTo(buf[:n], from); err != nil {
			t.Fatal(err)
		}
		_ = gA.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, _, err := gA.ReadFrom(buf); err != nil {
			t.Fatal(err)
		}
		total += time.Since(sent)
	}
	t.Logf("mean round trip through the relay: %v", (total / rounds).Round(10*time.Microsecond))

	// Closing the session leaves the relayed match.
	_ = streamA.Close()
	_ = streamB.Close()
	a.StopP2P()
	b.StopP2P()
	a.relayMu.Lock()
	left := len(a.relayByMatch) == 0 && len(a.relayByCID) == 0
	a.relayMu.Unlock()
	if !left {
		t.Fatal("the client still holds the relay after the session ended")
	}
}

// relayDeliveries records what each player's relay delivers.
type relayDeliveries struct {
	mu  sync.Mutex
	got map[string][]string
}

func (d *relayDeliveries) to(who string) func([]byte) {
	return func(b []byte) {
		d.mu.Lock()
		if d.got == nil {
			d.got = map[string][]string{}
		}
		d.got[who] = append(d.got[who], string(b))
		d.mu.Unlock()
	}
}

func (d *relayDeliveries) of(who string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.got[who]...)
}

// waitDelivered waits until who's relay delivered last.
func (d *relayDeliveries) waitDelivered(t *testing.T, who, last string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := d.of(who)
		if len(got) > 0 && got[len(got)-1] == last {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's relay delivered %q, not %q", who, got, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func joinServerRelay(t *testing.T, c *NakamaClient, matchID, peer string, deliver func([]byte)) p2pRelayLink {
	t.Helper()
	link, err := c.JoinRelay(p2pRelayRequest{MatchID: matchID, Name: "ikemen-p2p-relay-" + randID(), Peer: peer, Server: true}, deliver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = link.Close() })
	return link
}

// The server's relay of a matchmade pair is theirs: only the two players get
// it, nobody else can join it, it passes each player's relay data to the
// other within the limits, and it ends when a player leaves.
func TestNakamaLive_ServerRelayIsThePairs(t *testing.T) {
	fixedEnabled(t)
	a, b := matchPair(t)
	c, recC := liveClient(t)
	session := a.CurrentMatchID()
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID

	relay, err := a.requestServerRelay(session, idb)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := b.requestServerRelay(session, ida); err != nil || again != relay || !nakamaAuthoritativeMatchID(relay) {
		t.Fatalf("relays %q and %q (%v), want one authoritative match", relay, again, err)
	}
	for _, x := range []struct {
		who     string
		c       *NakamaClient
		matchID string
		peer    string
	}{
		{"a stranger", c, session, ida},
		{"a player, with a stranger", a, session, idc},
		{"a player, with itself", a, session, ida},
		{"a player, through the relay itself", a, relay, idb},
		{"a player, through a match that is not running", a, "00000000-0000-4000-8000-000000000000." + strings.SplitN(session, ".", 2)[1], idb},
	} {
		if id, err := x.c.requestServerRelay(x.matchID, x.peer); nakamaRPCErrorCode(err) != 9 {
			t.Fatalf("%s: relay %q, error %v; want a refusal (9)", x.who, id, err)
		}
	}
	// The lobby list does not show it, and nobody else can join it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listed, err := c.ListMatches(ctx, true, "+label.kind:ikemen-lobby", "", 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range listed {
		if m["match_id"] == relay {
			t.Fatalf("the lobby list shows the relay: %v", m)
		}
	}
	if err := c.JoinMatch(relay, ""); err != nil {
		t.Fatal(err)
	}
	ev, ok := recC.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "error" || ev.Type == "match_joined" })
	if !ok || ev.Type != "error" || !strings.Contains(ev.Error.Error(), "two other players") {
		t.Fatalf("a stranger's join of the relay: %+v", ev)
	}

	var d relayDeliveries
	linkA := joinServerRelay(t, a, session, idb, d.to("a"))
	linkB := joinServerRelay(t, b, session, ida, d.to("b"))
	if err := linkA.Send([]byte("hello b")); err != nil {
		t.Fatal(err)
	}
	if err := linkB.Send([]byte("hello a")); err != nil {
		t.Fatal(err)
	}
	d.waitDelivered(t, "b", "hello b")
	d.waitDelivered(t, "a", "hello a")
	// A datagram over the size limit goes nowhere.
	if err := a.sendEnvelope("match_data_send", map[string]any{"match_id": relay, "op_code": nakamaRelayOp, "data": bytes.Repeat([]byte("x"), 3000)}); err != nil {
		t.Fatal(err)
	}
	_ = linkA.Send([]byte("after the big one"))
	if got := d.waitDelivered(t, "b", "after the big one"); len(got) != 2 {
		t.Fatalf("b's relay delivered %d datagrams, want 2 (not the one over the size limit)", len(got))
	}

	// A burst gets 16 datagrams through at once, then 5 a tick; a steady
	// stream of 50 a second gets through whole.
	for i := 0; i < 40; i++ {
		_ = a.sendEnvelope("match_data_send", map[string]any{"match_id": relay, "op_code": nakamaRelayOp, "data": []byte(fmt.Sprintf("burst %d", i))})
	}
	time.Sleep(500 * time.Millisecond)
	burst := len(d.of("b")) - 2
	if burst < 16 || burst >= 40 {
		t.Fatalf("%d datagrams of a burst of 40 passed, want 16 and a few more", burst)
	}
	for i := 0; i < 50; i++ {
		_ = linkA.Send([]byte(fmt.Sprintf("steady %d", i)))
		time.Sleep(20 * time.Millisecond)
	}
	got := d.waitDelivered(t, "b", "steady 49")
	if steady := len(got) - 2 - burst; steady != 50 {
		t.Fatalf("%d datagrams of a steady 50 passed", steady)
	}

	// A player who leaves ends the relay: nobody joins it again, and the
	// session makes the pair a new one.
	_ = linkB.Close()
	time.Sleep(300 * time.Millisecond)
	if _, err := a.joinRelay(p2pRelayRequest{Peer: idb}, func([]byte) {}, relay, "match_join", map[string]any{"match_id": relay}); err == nil {
		t.Fatal("rejoined the relay after a player left it")
	}
	if fresh, err := b.requestServerRelay(session, ida); err != nil || fresh == relay {
		t.Fatalf("relay after the end %q (%v), want a new one", fresh, err)
	}
}

// A lobby gets the relay for the pairing playing now, and for nobody else.
func TestNakamaLive_LobbyServerRelay(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	ida, idb, idc := a.Session().UserID, b.Session().UserID, c.Session().UserID
	lobby, _ := createLobbyWith(t, a, map[string]any{"interval": 3})
	joinWith(t, a, recA, lobby, nil)
	joinWith(t, b, recB, lobby, nil)
	joinWith(t, c, recC, lobby, nil)
	waitLobby(t, c, "three members", func(st map[string]any) bool {
		m, _ := st["members"].([]any)
		return len(m) == 3
	})
	if _, err := a.requestServerRelay(lobby, idb); nakamaRPCErrorCode(err) != 9 {
		t.Fatalf("a relay before any pairing plays: %v", err)
	}
	ready(t, a, true)
	ready(t, b, true)
	st := waitLobby(t, c, "A vs B", lobbyPhase("playing"))
	if p1, p2, _ := currentPair(st); p1 != ida || p2 != idb {
		t.Fatalf("pairing %s vs %s, want A vs B", short(p1), short(p2))
	}
	relay, err := b.requestServerRelay(lobby, ida)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		who  string
		c    *NakamaClient
		peer string
	}{{"a member who is not playing", c, ida}, {"a player, with a member who is not playing", a, idc}} {
		if id, err := x.c.requestServerRelay(lobby, x.peer); nakamaRPCErrorCode(err) != 9 {
			t.Fatalf("%s: relay %q, error %v; want a refusal (9)", x.who, id, err)
		}
	}
	var d relayDeliveries
	linkA := joinServerRelay(t, a, lobby, idb, d.to("a"))
	linkB := joinServerRelay(t, b, lobby, ida, d.to("b"))
	_ = linkA.Send([]byte("from a"))
	_ = linkB.Send([]byte("from b"))
	d.waitDelivered(t, "b", "from a")
	d.waitDelivered(t, "a", "from b")
	a.relayMu.Lock()
	_, joined := a.relayByMatch[relay]
	a.relayMu.Unlock()
	if !joined || a.CurrentMatchID() != lobby {
		t.Fatalf("a's relay %q joined %v; a's match %q, want the lobby", relay, joined, a.CurrentMatchID())
	}
}

// Nakama answers a call of an RPC nobody registered with "not found", which
// the client takes for a server without the relay module; the relay RPC's own
// refusals are not that.
func TestNakamaLive_RelayRPCMissing(t *testing.T) {
	fixedEnabled(t)
	c, _ := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.CallRPC(ctx, "ikemen_no_such_rpc", map[string]string{}); !nakamaRPCMissing(err) {
		t.Fatalf("an RPC nobody registered: %v", err)
	}
	if _, err := c.requestServerRelay("00000000-0000-4000-8000-000000000000.nakama", "someone"); err == nil || errors.Is(err, errNakamaNoServerRelay) {
		t.Fatalf("a refused relay: %v", err)
	}
}
