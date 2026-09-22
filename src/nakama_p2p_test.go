package main

import (
	"net"
	"testing"
	"time"
)

func TestNakamaP2PHandleSignalIgnoresOwnToken(t *testing.T) {
	p, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	p.mu.Lock()
	token := p.token
	p.mu.Unlock()

	if err := p.HandleSignal(P2PSignal{
		Magic:   p2pMagic,
		MatchID: "test-match",
		Token:   token,
		Candidates: []P2PCandidate{{
			Type:    "host",
			Address: "127.0.0.1",
			Port:    p.LocalPort(),
		}},
	}); err != nil {
		t.Fatalf("own signal should be ignored: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.remoteToken != "" || len(p.remote) != 0 {
		t.Fatal("own signal populated remote P2P state")
	}
}

func TestNakamaP2PPunchesBetweenTwoPeers(t *testing.T) {
	p1, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p1.Close()
	p2, err := NewNakamaP2P("test-match", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	p1.mu.Lock()
	tok1 := p1.token
	p1.mu.Unlock()
	port1 := p1.LocalPort()
	p2.mu.Lock()
	tok2 := p2.token
	p2.mu.Unlock()
	port2 := p2.LocalPort()

	if err := p1.HandleSignal(P2PSignal{
		Magic: p2pMagic, MatchID: "test-match", Token: tok2,
		Candidates: []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: port2}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p2.HandleSignal(P2PSignal{
		Magic: p2pMagic, MatchID: "test-match", Token: tok1,
		Candidates: []P2PCandidate{{Type: "host", Address: "127.0.0.1", Port: port1}},
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{}, 2)
	go func() { p1.sendPunches(); done <- struct{}{} }()
	go func() { p2.sendPunches(); done <- struct{}{} }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 1 did not finish P2P punch")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 2 did not finish P2P punch")
	}

	select {
	case <-p1.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 1 did not become ready")
	}
	select {
	case <-p2.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("peer 2 did not become ready")
	}

	a1 := p1.RemoteAddr()
	a2 := p2.RemoteAddr()
	if a1 == nil || a2 == nil {
		t.Fatal("P2P handshake did not record both remote addresses")
	}
	if !a1.IP.Equal(net.ParseIP("127.0.0.1")) || a1.Port != port2 {
		t.Fatalf("peer 1 remote=%v, want 127.0.0.1:%d", a1, port2)
	}
	if !a2.IP.Equal(net.ParseIP("127.0.0.1")) || a2.Port != port1 {
		t.Fatalf("peer 2 remote=%v, want 127.0.0.1:%d", a2, port1)
	}
}
