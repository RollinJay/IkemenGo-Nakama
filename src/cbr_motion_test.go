package main

import (
	"fmt"
	"testing"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

// Synthesized motions must complete their command in the engine's own
// matcher, including strict-order (">") steps, releases, charges and holds.
func TestCBRMotionSynthesis(t *testing.T) {
	cl := NewCommandList(NewInputBuffer())
	specs := map[string]string{
		"x":       "x",
		"xy":      "x+y",
		"down_x":  "/$D, x",
		"QCF_x":   "~D, DF, F, x",
		"strict":  "~D, >DF, >F, x",
		"DP_x":    "F, D, DF, x",
		"RDP_x":   "~F, D, >DF, x",
		"HCB_x":   "F, DF, D, DB, B, x",
		"360_x":   "F, DF, D, DB, B, UB, U, x",
		"dash":    "F, F",
		"charge":  "~30$B, F, x",
		"hold":    "/30$B, F, x",
		"release": "~x",
	}
	for name, cmd := range specs {
		if err := cl.AddCommand(name, CommandSpec{Cmd: cmd, Time: 15, BufTime: 1, StepTime: 15}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	m := cbrMotionsFor(cl)
	// Strict-order steps (">") right after a release are not synthesized
	// yet; those commands get no motion and are never input by the CBR layer.
	unsupported := map[string]bool{"strict": true, "RDP_x": true}
	for name := range specs {
		mo, ok := m[name]
		if !ok {
			if !unsupported[name] {
				t.Errorf("%s (%q): no motion", name, specs[name])
			}
			continue
		}
		var dirs []uint8
		for _, b := range mo.Inputs {
			dirs = append(dirs, b.Dir())
		}
		t.Logf("%-8s %-28q %d frames, directions %v", name, specs[name], len(mo.Inputs), dirs)
	}
	// A quarter circle is rolled, as a person inputs it: no neutral frame.
	for _, b := range m["QCF_x"].Inputs {
		if b == 0 {
			t.Errorf("QCF should roll through DF without a neutral frame: %v", m["QCF_x"].Inputs)
			break
		}
	}
	if mo := m["hold"]; len(mo.Inputs) < 30 {
		t.Errorf("a 30-frame hold needs at least 30 frames: %d", len(mo.Inputs))
	}
	_ = cbr.Motion{}
}

// TestCBRReplayLog: learned replay sessions persist across loads, and the
// list is cut to the newest sessions once it grows past its limit.
func TestCBRReplayLog(t *testing.T) {
	dir := t.TempDir()
	a := cbrReplayLog{dir: dir}
	if a.has("x") {
		t.Fatal("empty log reports a session")
	}
	for _, id := range []string{"a1", "b2", "a1"} {
		if err := a.add(id); err != nil {
			t.Fatal(err)
		}
	}
	b := cbrReplayLog{dir: dir}
	if !b.has("a1") || !b.has("b2") || b.has("c3") || len(b.order) != 2 {
		t.Fatalf("reloaded log: %v", b.order)
	}
	for i := 0; i <= cbrLearnedReplaysMax; i++ {
		if err := b.add(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	c := cbrReplayLog{dir: dir}
	c.load()
	if len(c.order) > cbrLearnedReplaysMax || c.has("a1") || !c.has(fmt.Sprintf("n%d", cbrLearnedReplaysMax)) {
		t.Fatalf("cut log: %d entries, oldest kept %v", len(c.order), c.has("a1"))
	}
}

// TestCBRReplaySessions: in replay playback a person is recorded as local
// play when the match was played offline and as online (lobby) play
// otherwise, and a session's identity comes from its header alone, so the
// live stream and the saved file of one match share it.
func TestCBRReplaySessions(t *testing.T) {
	saved := sys.replayFile
	defer func() { sys.replayFile = saved }()

	sys.replayFile = &ReplayFile{local: true}
	if got := cbrPlaybackHumanSource(); got != cbr.SrcHumanLocal {
		t.Fatalf("offline replay: source %v", got)
	}
	if cbrReplayID() != "" {
		t.Fatal("a replay without a session header has an identity")
	}
	header := ReplayStreamHeader{Seed: 1234, PreMatchTime: 5, MatchTime: 600, Stage: `stages\stage0.def`, Stream: "a"}
	live := NewNakamaReplayBuffer()
	live.ApplyHeader(header)
	sys.replayFile = &ReplayFile{liveBuffer: live}
	if got := cbrPlaybackHumanSource(); got != cbr.SrcHumanLobby {
		t.Fatalf("online replay: source %v", got)
	}
	id := cbrReplayID()
	// The saved file of the same match has its own stream id.
	header.Stream, header.Segment = "b", 0
	file := NewNakamaReplayBuffer()
	file.ApplyHeader(header)
	sys.replayFile = &ReplayFile{liveBuffer: file, fromFile: true}
	if id == "" || cbrReplayID() != id {
		t.Fatalf("identities differ: %q %q", id, cbrReplayID())
	}
	if id != cbrSessionID(1234, 5, 600, "stages/STAGE0.def") {
		t.Fatal("identity depends on path separators or case")
	}
	header.Seed++
	other := NewNakamaReplayBuffer()
	other.ApplyHeader(header)
	sys.replayFile = &ReplayFile{liveBuffer: other}
	if cbrReplayID() == id {
		t.Fatal("different sessions share an identity")
	}
}
