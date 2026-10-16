package main

import (
	"bytes"
	"io"
	"net"
	"testing"

	ggpo "github.com/ikemen-engine/ggpo"
)

// The ready bit (IB_READY) is never a key: not in the keys read from input
// bits, not a button for "any button" checks, not set from keys.
func TestReadyBitIsNoKey(t *testing.T) {
	if IB_READY&IB_keys != 0 || IB_READY&IB_anybutton != 0 {
		t.Fatalf("IB_READY %#x overlaps the keys %#x", IB_READY, IB_keys)
	}
	if IB_READY <= 0 || (IB_READY|IB_keys) <= 0 {
		t.Fatalf("IB_READY makes input bits negative (the end of a netplay input stream)")
	}
	for _, bits := range []InputBits{IB_READY, IB_READY | IB_A, IB_READY | IB_PU | IB_M} {
		keys := bits.BitsToKeys()
		want := (bits &^ IB_READY).BitsToKeys()
		if keys != want {
			t.Fatalf("%#x: keys %v, want %v", bits, keys, want)
		}
	}
	if keys := IB_READY.BitsToKeys(); keys != ([14]bool{}) {
		t.Fatalf("IB_READY alone reads as keys %v", keys)
	}
	var all [14]bool
	for i := range all {
		all[i] = true
	}
	var bits InputBits
	bits.KeysToBits(all)
	if bits&IB_READY != 0 || bits != IB_keys {
		t.Fatalf("all keys make %#x", bits)
	}
}

// A netplay buffer carries the ready bit next to the keys; reading the frame
// as keys, or as "any button", leaves it out.
func TestNetBufferReadyBit(t *testing.T) {
	saved := sys.inputRemap
	defer func() { sys.inputRemap = saved }()
	for i := range sys.inputRemap {
		sys.inputRemap[i] = i
	}
	nc := NewNetConnection()
	nc.host = true
	nc.locIn, nc.remIn = 0, 1
	// Frame 0: the local game raised its bit, the other game did not.
	nc.SetReady(true)
	nc.buf[0].writeNetBuffer(0, nc.readyBits())
	nc.buf[1].buf[0] = IB_A
	nc.buf[1].inpT = 1
	nc.buf[0].curT, nc.buf[1].curT = 0, 0
	if nc.buf[0].buf[0]&IB_READY == 0 {
		t.Fatalf("the frame sent has no ready bit: %#x", nc.buf[0].buf[0])
	}
	if nc.readNetInput(0) != ([14]bool{}) {
		t.Fatalf("the ready bit reads as keys: %v", nc.readNetInput(0))
	}
	if nc.AnyButton() != true {
		t.Fatalf("the other game's A is a button")
	}
	if !nc.Ready(1) || nc.Ready(2) || nc.Ready(0) {
		t.Fatalf("ready: 1 %v, 2 %v, both %v", nc.Ready(1), nc.Ready(2), nc.Ready(0))
	}
	// Frame 1: both bits up, no key.
	nc.buf[1].buf[1] = IB_READY
	nc.buf[1].inpT = 2
	nc.buf[0].writeNetBuffer(0, nc.readyBits())
	nc.buf[0].curT, nc.buf[1].curT = 1, 1
	if nc.AnyButton() {
		t.Fatalf("ready bits are a button")
	}
	if !nc.Ready(0) || !nc.Ready(1) || !nc.Ready(2) {
		t.Fatalf("both ready: %v %v %v", nc.Ready(0), nc.Ready(1), nc.Ready(2))
	}
	if nc.readNetInput(1) != ([14]bool{}) {
		t.Fatalf("the other game's ready bit reads as keys")
	}
	// Frame 2: the bit lowered; extra bits that are keys are not added.
	nc.SetReady(false)
	nc.buf[0].writeNetBuffer(0, IB_A)
	if nc.buf[0].buf[2] != 0 {
		t.Fatalf("writeNetBuffer added keys: %#x", nc.buf[0].buf[2])
	}
	// A frame not received yet is not ready.
	nc.buf[0].curT, nc.buf[1].curT = 2, 2
	if nc.Ready(2) || nc.Ready(0) {
		t.Fatalf("a missing frame is ready")
	}
}

// A session's replay records the ready bit with the keys, and plays it
// without keys.
func TestReplayReadyBit(t *testing.T) {
	saved := sys.inputRemap
	defer func() { sys.inputRemap = saved }()
	for i := range sys.inputRemap {
		sys.inputRemap[i] = i
	}
	var buf bytes.Buffer
	frames := [][REPLAY_NUM_INPUTS]InputBits{
		{IB_READY, 0},
		{IB_READY | IB_B, IB_READY},
	}
	for _, f := range frames {
		for _, b := range f {
			if err := writeReplayInput(&buf, b, [6]int8{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rf := &ReplayFile{}
	for n, f := range frames {
		for i := range rf.ibit {
			if err := readReplayInput(&buf, &rf.ibit[i], &rf.iaxes[i]); err != nil {
				t.Fatal(err)
			}
		}
		if rf.ibit != f {
			t.Fatalf("frame %d: %v, want %v", n, rf.ibit, f)
		}
		keys := rf.readReplayInput(0)
		if keys != (f[0] &^ IB_READY).BitsToKeys() {
			t.Fatalf("frame %d: keys %v", n, keys)
		}
		if got, want := rf.AnyButton(), f[0]&IB_anybutton != 0; got != want {
			t.Fatalf("frame %d: any button %v", n, got)
		}
		if got, want := rf.Ready(0), n == 1; got != want {
			t.Fatalf("frame %d: both ready %v", n, got)
		}
		if !rf.Ready(1) {
			t.Fatalf("frame %d: player 1 not ready", n)
		}
	}
}

// Rollback fights take their inputs from keys alone: a ready bit in a GGPO
// input is not a button either.
func TestRollbackReadyBitIsNoButton(t *testing.T) {
	saved := sys.inputRemap
	defer func() { sys.inputRemap = saved }()
	for i := range sys.inputRemap {
		sys.inputRemap[i] = i
	}
	rs := &RollbackSystem{ggpoInputs: []InputBits{IB_READY, IB_READY}, ggpoAnalogInputs: make([][6]int8, 2)}
	if rs.anyButton() {
		t.Fatalf("ready bits are a button")
	}
	if rs.readRollbackInput(0) != ([14]bool{}) {
		t.Fatalf("a ready bit reads as keys")
	}
}

// A Nakama session compares a strict setting list in which the rollback
// option is on, whatever the player's own option says.
func TestWithSyncValue(t *testing.T) {
	in := []SyncSetting{{Path: "Netplay.RollbackNetcode", Value: "0"}, {Path: "Other", Value: "x"}}
	out := withSyncValue(in, "Netplay.RollbackNetcode", "1")
	if in[0].Value != "0" {
		t.Fatalf("the input changed")
	}
	if out[0].Value != "1" || out[1].Value != "x" || len(out) != 2 {
		t.Fatalf("%v", out)
	}
	local := withSyncValue([]SyncSetting{{Path: "Netplay.RollbackNetcode", Value: "0"}}, "Netplay.RollbackNetcode", "1")
	remote := withSyncValue([]SyncSetting{{Path: "Netplay.RollbackNetcode", Value: "1"}}, "Netplay.RollbackNetcode", "1")
	if err := validateStrictCompatibility(local, remote, false); err != nil {
		t.Fatalf("delay against rollback in a Nakama session: %v", err)
	}
	direct := []SyncSetting{{Path: "Netplay.RollbackNetcode", Value: "0"}}
	if err := validateStrictCompatibility(direct, remote, false); err == nil {
		t.Fatalf("delay against rollback in a direct session was accepted")
	}
}

// A match the scripts ended (the online rematch choice) makes the rollback
// loop report it as quit (sys.esc), but does not cancel the session; Esc
// does.
func TestNetplayMatchEndedIsNoCancel(t *testing.T) {
	saved := sys.esc
	defer func() { sys.esc = saved }()
	nc := NewNetConnection()
	sys.esc = false
	if nc.cancelled() {
		t.Fatalf("cancelled without Esc")
	}
	sys.esc = true
	if !nc.cancelled() {
		t.Fatalf("Esc does not cancel")
	}
	nc.matchEnded = true
	if nc.cancelled() {
		t.Fatalf("a match the scripts ended cancels the session")
	}
}

// GGPO losing the other player ends the match as the scripts' endMatch
// does (sys.endMatch), but cancels the session, as Esc does; a match the
// scripts ended does not.
func TestRollbackPeerDisconnectCancels(t *testing.T) {
	savedEsc, savedEnd, savedPost, savedWarning := sys.esc, sys.endMatch, sys.postMatchFlg, sys.sessionWarning
	defer func() {
		sys.esc, sys.endMatch, sys.postMatchFlg, sys.sessionWarning = savedEsc, savedEnd, savedPost, savedWarning
	}()
	for _, disconnected := range []bool{false, true} {
		nc := NewNetConnection()
		session := NewRollbackSession(RollbackProperties{})
		rs := &RollbackSystem{session: &session, netConnection: nc}
		sys.esc, sys.endMatch, sys.postMatchFlg, sys.sessionWarning = false, false, false, ""
		if disconnected {
			session.OnEvent(&ggpo.Event{Code: ggpo.EventCodeDisconnectedFromPeer, Player: 2})
		} else {
			sys.endMatch = true // the scripts' endMatch()
		}
		if !sys.endMatch {
			t.Fatalf("disconnected %v: the match did not end", disconnected)
		}
		// simulateFrame reports the ended match as quit.
		sys.esc = true
		rs.noteMatchEnded()
		if nc.cancelled() != disconnected {
			t.Fatalf("disconnected %v: cancelled %v", disconnected, nc.cancelled())
		}
	}
}

func closedP2P(p *NakamaP2P) bool {
	select {
	case <-p.closed:
		return true
	default:
		return false
	}
}

// Losing the server's connection keeps the P2P path a netplay session runs
// on, and its match (both games of the session decide on the match id), and
// closes a path that carries no session, with the match; leaving the match
// without a connection forgets it, and Disconnect closes the path.
func TestNakamaConnectionLostKeepsSessionPath(t *testing.T) {
	for _, stream := range []bool{true, false} {
		c1, c2 := net.Pipe()
		go func() { _, _ = io.Copy(io.Discard, c2) }()
		n := NewNakamaClient(NakamaConfig{Host: "127.0.0.1"})
		p := &NakamaP2P{closed: make(chan struct{})}
		n.mu.Lock()
		n.conn, n.p2p, n.p2pStream, n.matchID = c1, p, stream, "m"
		n.mu.Unlock()
		n.connectionLost(c1)
		if n.IsConnected() {
			t.Fatalf("stream %v: still connected", stream)
		}
		if stream {
			if n.P2P() != p || closedP2P(p) {
				t.Fatalf("the session's path was closed")
			}
			if n.CurrentMatchID() != "m" {
				t.Fatalf("the session's match was left: %q", n.CurrentMatchID())
			}
			if err := n.LeaveMatch(); err != nil || n.CurrentMatchID() != "" {
				t.Fatalf("leaving the match without a connection: %v, %q", err, n.CurrentMatchID())
			}
			n.Disconnect()
			if n.P2P() != nil || !closedP2P(p) {
				t.Fatalf("Disconnect kept the path")
			}
		} else if n.P2P() != nil || !closedP2P(p) || n.CurrentMatchID() != "" {
			t.Fatalf("a path without a session was kept, or its match")
		}
		c2.Close()
	}
}
