package main

// Live integration tests for the Nakama client, server modules and P2P
// handshake. They run against a real Nakama (3.41) started with this tree's
// nakama/modules and:
//   --socket.max_message_size_bytes 65536 --session.token_expiry_sec 8
//   --matchmaker.interval_sec 2
// Enable with NAKAMA_LIVE=1.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type evRecorder struct {
	mu  sync.Mutex
	evs []NakamaEvent
	ch  chan NakamaEvent
}

func newRecorder() *evRecorder { return &evRecorder{ch: make(chan NakamaEvent, 512)} }

func (r *evRecorder) fn(ev NakamaEvent) {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	select {
	case r.ch <- ev:
	default:
	}
}

func (r *evRecorder) waitFor(t *testing.T, timeout time.Duration, pred func(NakamaEvent) bool) (NakamaEvent, bool) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-r.ch:
			if pred(ev) {
				return ev, true
			}
		case <-deadline:
			return NakamaEvent{}, false
		}
	}
}

func randID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "review-" + hex.EncodeToString(b)
}

func liveClient(t *testing.T) (*NakamaClient, *evRecorder) {
	t.Helper()
	c := NewNakamaClient(NakamaConfig{Host: "127.0.0.1", Port: 7350, ServerKey: "defaultkey", DeviceID: randID(), Game: "g", GameBuild: "1.0.0", Region: "r", RequestTTL: 5 * time.Second})
	rec := newRecorder()
	c.SetEventHandler(rec.fn)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(c.Disconnect)
	return c, rec
}

func fixedEnabled(t *testing.T) {
	if os.Getenv("NAKAMA_LIVE") != "1" {
		t.Skip("set NAKAMA_LIVE=1 to run against a live Nakama")
	}
}

func matchDataEvents(rec *evRecorder, op uint32) []NakamaEvent {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []NakamaEvent
	for _, ev := range rec.evs {
		if ev.Type == "match_data" && ev.MatchData != nil && uint32(ev.MatchData.OpCode) == op {
			out = append(out, ev)
		}
	}
	return out
}

func lastPairing(t *testing.T, rec *evRecorder) map[string]any {
	t.Helper()
	evs := matchDataEvents(rec, nakamaLobbyPairingOp)
	if len(evs) == 0 {
		t.Fatal("no pairing received")
	}
	m, _ := evs[len(evs)-1].Payload.(map[string]any)
	return m
}

func createLobby(t *testing.T, c *NakamaClient, format string) string {
	t.Helper()
	id, err := c.CreateLobby(context.Background(), NakamaLobbyConfig{Name: "t", Format: format, MaxGames: 3,
		Settings: map[string]any{"interval": 3}})
	if err != nil {
		t.Fatalf("CreateLobby: %v", err)
	}
	return id
}

func join(t *testing.T, c *NakamaClient, rec *evRecorder, matchID string) {
	t.Helper()
	if err := c.JoinMatch(matchID, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "match_joined" }); !ok {
		t.Fatalf("no match_joined; connected=%v", c.IsConnected())
	}
}

func TestNakamaLive_ConnectRPCJoinChat(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	if a.Session().UserID == "" {
		t.Fatal("user id not parsed from JWT")
	}
	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	if err := a.JoinChat("review-room", 1, false, false); err != nil {
		t.Fatal(err)
	}
	ev, ok := recA.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "channel_joined" })
	if !ok || ev.ChannelID == "" {
		t.Fatal("no channel id")
	}
	if err := a.SendChat(ev.ChannelID, map[string]string{"message": "hi"}); err != nil {
		t.Fatal(err)
	}
	msg, ok := recA.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "chat_message" })
	if !ok {
		t.Fatalf("no chat echo; connected=%v", a.IsConnected())
	}
	t.Logf("user=%s lobby=%s chat content=%v connected=%v", a.Session().UserID[:8], id[:8], msg.ChannelMessage["content"], a.IsConnected())
}

func TestNakamaLive_TokenRefresh(t *testing.T) {
	fixedEnabled(t)
	a, _ := liveClient(t)
	first := a.Session().Token
	time.Sleep(9 * time.Second) // server started with token_expiry_sec=8
	if _, err := a.GetRating(context.Background(), "g"); err != nil {
		t.Fatalf("RPC after expiry: %v", err)
	}
	if a.Session().Token == first {
		t.Fatal("token was not refreshed")
	}
	t.Log("RPC after token expiry succeeded with a refreshed token")
}

func TestNakamaLive_DeviceIDPersists(t *testing.T) {
	fixedEnabled(t)
	dir := t.TempDir()
	old := sys.baseDir
	sys.baseDir = dir
	defer func() { sys.baseDir = old }()
	id1, err := loadOrCreateNakamaDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := loadOrCreateNakamaDeviceID()
	if id1 != id2 {
		t.Fatalf("device id changed: %s vs %s", id1, id2)
	}
}

func TestNakamaLive_MatchmakerRankedServerElo(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "ranked", Elo: 3000, EloRange: 100, Game: "g", GameBuild: "1.0.0", Region: "r", RankedBestOf: 5, RankedSwitchSides: true}
	_ = a.Matchmake(req)
	_ = b.Matchmake(req)
	ev, ok := recA.waitFor(t, 20*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	if !ok {
		t.Fatal("no match")
	}
	_, _ = recB.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	u := ev.Matchmaker.Users[0]
	t.Logf("properties mode=%v best_of=%v elo(server)=%v", u.Properties["mode"], u.Properties["ranked_best_of"], u.Properties["elo"])
	if u.Properties["mode"] != "ranked" || u.Properties["elo"] != float64(1000) {
		t.Fatalf("unexpected properties %v", u.Properties)
	}
	join(t, a, recA, ev.Matchmaker.MatchID)
}

func TestNakamaLive_MatchmakerEloWindow(t *testing.T) {
	fixedEnabled(t)
	ctx := context.Background()
	a, recA := liveClient(t)
	// Raise A's stored rating by settling wins against helper accounts.
	for i := 0; i < 14; i++ {
		h, _ := liveClient(t)
		mid := "boost-" + randID()
		_, _ = a.SubmitResult(ctx, NakamaResultRequest{MatchID: mid, OpponentID: h.Session().UserID, Game: "g5", Result: 1})
		_, _ = h.SubmitResult(ctx, NakamaResultRequest{MatchID: mid, OpponentID: a.Session().UserID, Game: "g5", Result: 0})
		h.Disconnect()
	}
	body, _ := a.GetRating(ctx, "g5")
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "ranked", Elo: 1000, EloRange: 100, Game: "g5", GameBuild: "1.0.0", Region: "r"}
	_ = a.Matchmake(req) // A claims 1000; the server uses the stored rating instead
	_ = b.Matchmake(req)
	_, matched := recA.waitFor(t, 10*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	_ = recB
	t.Logf("A stored rating %s; fresh B (1000), range 100: matched=%v", body, matched)
	if matched {
		t.Fatal("players outside the server-side Elo window were matched")
	}
}

func TestNakamaLive_LobbyScheduling(t *testing.T) {
	fixedEnabled(t)
	var cs []*NakamaClient
	var recs []*evRecorder
	for i := 0; i < 4; i++ {
		c, r := liveClient(t)
		cs, recs = append(cs, c), append(recs, r)
	}
	id := createLobby(t, cs[0], "queue")
	for i := 0; i < 4; i++ {
		join(t, cs[i], recs[i], id)
		time.Sleep(250 * time.Millisecond)
	}
	ids := make([]string, 4)
	for i := range cs {
		ids[i] = cs[i].Session().UserID
		ready(t, cs[i], true) // only ready members are paired
	}
	waitLobby(t, cs[2], "A vs B", lobbyPhase("playing"))
	_ = cs[3].LeaveMatch() // D leaves while queued
	time.Sleep(400 * time.Millisecond)
	cs[0].ReportLobbyResult(ids[0], ids[1])
	cs[1].ReportLobbyResult(ids[0], ids[1]) // duplicate report from the other player
	waitLobby(t, cs[2], "second match", func(st map[string]any) bool {
		_, _, seq := currentPair(st)
		return st["phase"] == "playing" && seq == 2
	})
	p := lastPairing(t, recs[2])
	t.Logf("after D left and A beat B (reported twice): %v vs %v seq=%v queue=%v", short(p["player1"]), short(p["player2"]), p["seq"], p["queue"])
	if p["player1"] == ids[3] || p["player2"] == ids[3] {
		t.Fatal("departed player paired")
	}
	if p["player1"] != ids[2] || p["player2"] != ids[0] {
		t.Fatalf("expected C vs A, got %v vs %v", p["player1"], p["player2"])
	}
}

func short(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func TestNakamaLive_WinnerStaysNoDoubleCount(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	id := createLobby(t, a, "winner_stays_on")
	join(t, a, recA, id)
	join(t, b, recB, id)
	ready(t, a, true)
	ready(t, b, true)
	waitLobby(t, a, "A vs B", lobbyPhase("playing"))
	ida, idb := a.Session().UserID, b.Session().UserID
	a.ReportLobbyResult(ida, idb)
	b.ReportLobbyResult(ida, idb)
	waitLobby(t, a, "rematch", func(st map[string]any) bool {
		_, _, seq := currentPair(st)
		return st["phase"] == "playing" && seq == 2
	})
	p := lastPairing(t, recA)
	t.Logf("after one game reported by both: winner_streak=%v seq=%v", p["winner_streak"], p["seq"])
	if p["winner_streak"] != float64(1) {
		t.Fatalf("streak double counted: %v", p["winner_streak"])
	}
}

func TestNakamaLive_EloSettlesOnce(t *testing.T) {
	fixedEnabled(t)
	a, _ := liveClient(t)
	b, _ := liveClient(t)
	ida, idb := a.Session().UserID, b.Session().UserID
	mid := "set-" + randID()
	ctx := context.Background()
	if _, err := a.SubmitResult(ctx, NakamaResultRequest{MatchID: mid, OpponentID: idb, Game: "g3", Result: 1}); err != nil {
		t.Fatal(err)
	}
	// Retry + simultaneous submissions.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = b.SubmitResult(ctx, NakamaResultRequest{MatchID: mid, OpponentID: ida, Game: "g3", Result: 0})
		}()
		go func() {
			defer wg.Done()
			_, _ = a.SubmitResult(ctx, NakamaResultRequest{MatchID: mid, OpponentID: idb, Game: "g3", Result: 1})
		}()
	}
	wg.Wait()
	body, err := a.GetRating(ctx, "g3")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("A rating after 1 match + 8 concurrent/retried submissions: %s", body)
	if !strings.Contains(string(body), `"games":1`) {
		t.Fatalf("expected exactly one settlement")
	}
}

func TestNakamaLive_SessionLeaveResolves(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "unranked", Game: "g4", GameBuild: "1.0.0", Region: "r"}
	_ = a.Matchmake(req)
	_ = b.Matchmake(req)
	ev, ok := recA.waitFor(t, 20*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	if !ok {
		t.Fatal("no match")
	}
	evb, _ := recB.waitFor(t, 2*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
	join(t, a, recA, ev.Matchmaker.MatchID)
	join(t, b, recB, evb.Matchmaker.MatchID)
	_ = b.LeaveMatch()
	time.Sleep(400 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{"version": 1, "action": "rematch", "round": 1, "snapshot": map[string]any{"version": 1, "stageNo": 1, "p1teammode": "single", "p2teammode": "single", "p1": []any{map[string]any{"ref": 1, "pal": 1}}, "p2": []any{map[string]any{"ref": 2, "pal": 1}}}})
	_ = a.SendMatchData(nakamaRematchChoiceOp, body, true)
	d, ok := recA.waitFor(t, 3*time.Second, func(ev NakamaEvent) bool { return ev.Type == "rematch_decision" })
	if !ok {
		t.Fatal("no decision after opponent left")
	}
	t.Logf("decision after opponent left: %v", d.Payload)
}

func TestNakamaLive_P2PLateStarter(t *testing.T) {
	fixedEnabled(t)
	// A starts and publishes before B exists; B starts later. With buffering +
	// periodic re-signal both sides must still connect.
	var mu sync.Mutex
	var aSignals []P2PSignal
	a, err := NewNakamaP2P("m", 0, func(s P2PSignal) error { mu.Lock(); aSignals = append(aSignals, s); mu.Unlock(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	mu.Lock()
	aSignals = nil // everything A published before B existed is lost
	mu.Unlock()
	b, err := NewNakamaP2P("m", 0, func(s P2PSignal) error { return a.HandleSignal(s) })
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// Relay A's periodic re-signals to B from now on.
	go func() {
		for i := 0; i < 40; i++ {
			time.Sleep(100 * time.Millisecond)
			mu.Lock()
			pending := append([]P2PSignal(nil), aSignals...)
			aSignals = nil
			mu.Unlock()
			for _, s := range pending {
				_ = b.HandleSignal(s)
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatalf("A not ready: %v", err)
	}
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("B not ready: %v", err)
	}
	t.Logf("late starter connected: A->%v B->%v", a.RemoteAddr(), b.RemoteAddr())
}

// matchPair matchmakes two clients and joins both to the resulting match.
func matchPair(t *testing.T) (a, b *NakamaClient) {
	t.Helper()
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	req := NakamaMatchmakerRequest{Mode: "unranked", Elo: 1000, EloRange: 100, Game: "p2p-" + randID()}
	if err := a.Matchmake(req); err != nil {
		t.Fatal(err)
	}
	if err := b.Matchmake(req); err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		c   *NakamaClient
		rec *evRecorder
	}{{a, recA}, {b, recB}} {
		ev, ok := x.rec.waitFor(t, 40*time.Second, func(ev NakamaEvent) bool { return ev.Type == "matchmaker_matched" })
		if !ok {
			t.Fatal("no matchmaker_matched")
		}
		join(t, x.c, x.rec, ev.Matchmaker.MatchID)
	}
	return a, b
}

func waitP2PReady(t *testing.T, clients ...*NakamaClient) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for _, c := range clients {
		for !c.P2PReady() {
			if time.Now().After(deadline) {
				t.Fatalf("P2P not ready: %+v", c.Status())
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

// A matchmade pair agrees on the host, opens the P2P path through Nakama
// signalling, and runs the netplay session handshake and a GGPO channel on it.
func TestNakamaLive_MatchmakerP2PSession(t *testing.T) {
	fixedEnabled(t)
	a, b := matchPair(t)
	pa, okA := a.Pairing()
	pb, okB := b.Pairing()
	if !okA || !okB || pa.Host == pb.Host {
		t.Fatalf("pairings disagree on the host: %+v %+v", pa, pb)
	}
	if pa.PeerUserID != b.Session().UserID || pb.PeerUserID != a.Session().UserID {
		t.Fatalf("pairing peers: %+v %+v", pa, pb)
	}
	if err := a.StartP2P(0, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.StartP2P(0, nil); err != nil {
		t.Fatal(err)
	}
	waitP2PReady(t, a, b)

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
		t.Fatalf("session data: %v %v", got, err)
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
	if _, err := gA.WriteTo([]byte("rollback"), remoteA); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 32)
	_ = gB.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _, err := gB.ReadFrom(buf); err != nil || string(buf[:n]) != "rollback" {
		t.Fatalf("gameplay datagram: %q %v", buf[:n], err)
	}

	// Leaving the session on one side ends it on the other within a second or so.
	start := time.Now()
	_ = streamA.Close()
	_ = streamB.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := streamB.Read(make([]byte, 1)); err == nil {
		t.Fatal("peer stream still open")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("peer only timed out")
	}
	t.Logf("host=%v; close reached the peer after %v", pa.Host, time.Since(start))
}

// In a lobby every member receives every signal. Signals name their sender
// and receiver, so a third member's handshake does not disturb the pair.
func TestNakamaLive_LobbyP2PSignalsAreAddressed(t *testing.T) {
	fixedEnabled(t)
	a, recA := liveClient(t)
	b, recB := liveClient(t)
	c, recC := liveClient(t)
	id := createLobby(t, a, "queue")
	join(t, a, recA, id)
	join(t, b, recB, id)
	join(t, c, recC, id)
	a.SetPairing(id, b.Session().UserID, true)
	b.SetPairing(id, a.Session().UserID, false)
	c.SetPairing(id, "someone-else", true)
	for _, cl := range []*NakamaClient{c, a, b} {
		if err := cl.StartP2P(0, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitP2PReady(t, a, b)
	pa, pb, pc := a.P2P(), b.P2P(), c.P2P()
	pa.mu.Lock()
	aRemote := pa.remoteToken
	pa.mu.Unlock()
	pb.mu.Lock()
	bRemote := pb.remoteToken
	pb.mu.Unlock()
	if aRemote != pb.token || bRemote != pa.token {
		t.Fatalf("pair used the wrong peer: a->%s (b=%s c=%s)", aRemote, pb.token, pc.token)
	}
	if c.P2PReady() {
		t.Fatal("the third member connected to someone")
	}
}
