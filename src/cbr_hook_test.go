package main

import (
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

// cbrTestRuntime gives the CBR layer an engine and an offline match in
// which it may drive, and restores everything when the test ends.
func cbrTestRuntime(t *testing.T) *cbr.Engine {
	t.Helper()
	saved := sysCBR
	rb, net, rf, armed := sys.rollback.session, sys.netConnection, sys.replayFile, localReplay.armed
	t.Cleanup(func() {
		sysCBR = saved
		sys.rollback.session, sys.netConnection, sys.replayFile, localReplay.armed = rb, net, rf, armed
	})
	eng := cbr.New(cbr.DefaultParams(), t.TempDir(), 1)
	sysCBR = cbrRuntime{enabled: true, eng: eng}
	sys.rollback.session, sys.netConnection, sys.replayFile, localReplay.armed = nil, nil, nil, false
	return eng
}

// TestCBRPausesAIOnlyOnDrivenFrames: the character's own AI (AILevel) and
// the engine AI's command cheat are off only on frames the CBR layer gives
// the input, for any command, long switch codes included, and never where
// the CBR layer may not drive.
func TestCBRPausesAIOnlyOnDrivenFrames(t *testing.T) {
	cbrTestRuntime(t)
	c := &Char{} // player 1's root
	short := []Command{{steps: make([]CommandStep, 3)}}
	long := []Command{{steps: make([]CommandStep, cbrMaxSynthSteps+1)}}
	s := &sysCBR.slots[0]
	for _, tc := range []struct{ active, driven, want bool }{
		{false, false, false},
		{false, true, false},
		{true, false, false}, // the base AI's frames
		{true, true, true},   // the CBR layer's
	} {
		s.active, s.driven = tc.active, tc.driven
		if got := cbrMaskAILevel(c); got != tc.want {
			t.Errorf("active=%v driven=%v: AILevel masked %v, want %v", tc.active, tc.driven, got, tc.want)
		}
		for _, cl := range [][]Command{short, long} {
			if got := cbrSuppressAICheat(c, cl); got != tc.want {
				t.Errorf("active=%v driven=%v, %d steps: cheat suppressed %v, want %v", tc.active, tc.driven, len(cl[0].steps), got, tc.want)
			}
		}
	}
	s.active, s.driven = true, true
	for name, set := range map[string]func(on bool){
		"disabled": func(on bool) { sysCBR.enabled = !on },
		"rollback": func(on bool) {
			sys.rollback.session = nil
			if on {
				sys.rollback.session = &RollbackSession{}
			}
		},
		"netplay": func(on bool) {
			sys.netConnection = nil
			if on {
				sys.netConnection = &NetConnection{}
			}
		},
		"replay": func(on bool) {
			sys.replayFile = nil
			if on {
				sys.replayFile = &ReplayFile{}
			}
		},
		"recorded": func(on bool) { localReplay.armed = on },
	} {
		set(true)
		if cbrMaskAILevel(c) || cbrSuppressAICheat(c, short) {
			t.Errorf("%s: the CBR layer may not drive, so nothing is paused", name)
		}
		set(false)
	}
	if !cbrMaskAILevel(c) {
		t.Fatal("setup: conditions restored")
	}
}

// TestCBRSwitchCodesAreNotCommands: a command longer than any move a person
// inputs (an older AI's switch code) never counts as an active command,
// whether completed or picked by the engine AI's cheat.
func TestCBRSwitchCodesAreNotCommands(t *testing.T) {
	cbrTestRuntime(t)
	cl := NewCommandList(NewInputBuffer())
	for name, cmd := range map[string]string{
		"x":      "x",
		"qcf_x":  "~D, DF, F, x",
		"switch": "a, b, c, x, y, z, a, b, c, x, y, z",
	} {
		if err := cl.AddCommand(name, CommandSpec{Cmd: cmd, Time: 60, BufTime: 1, StepTime: 15}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if n := len(cl.Commands[cl.Names["switch"]][0].steps); n <= cbrMaxSynthSteps {
		t.Fatalf("setup: the switch code has %d steps", n)
	}
	c := &Char{}
	for _, name := range []string{"x", "switch"} {
		cl.Commands[cl.Names[name]][0].curbuftime = 1
	}
	if got := cbrActiveCommands(c, cl); len(got) != 1 || got[0] != "x" {
		t.Fatalf("completed commands: %v, want [x]", got)
	}
	for _, name := range []string{"x", "switch"} {
		cl.Commands[cl.Names[name]][0].curbuftime = 0
	}
	c.controller = -1 // AI-controlled: the engine AI's cheat picks a command
	c.cpucmd = int32(cl.Names["switch"])
	if got := cbrActiveCommands(c, cl); len(got) != 0 {
		t.Fatalf("a switch code picked by the cheat is not a command: %v", got)
	}
	c.cpucmd = int32(cl.Names["qcf_x"])
	if got := cbrActiveCommands(c, cl); len(got) != 1 || got[0] != "qcf_x" {
		t.Fatalf("a motion picked by the cheat on a base AI frame: %v", got)
	}
	sysCBR.slots[0].active, sysCBR.slots[0].driven = true, true
	if got := cbrActiveCommands(c, cl); len(got) != 0 {
		t.Fatalf("the cheat does not fire on the CBR layer's frames: %v", got)
	}
}

// TestCBRDriveMargin: cbrDrive's optional fifth argument sets the take-over
// margin, Low, Medium or High, which reaches the engine when the slot
// starts driving; a missing or invalid one is Medium.
func TestCBRDriveMargin(t *testing.T) {
	eng := cbrTestRuntime(t)
	l := lua.NewState()
	defer l.Close()
	cbrRegisterLua(l)
	for _, tc := range []struct {
		call   string
		margin int
		want   float32
	}{
		{"cbrDrive(1, 8, 8, 8, 1)", 1, 0.015},
		{"cbrDrive(1, 8, 8, 8, 2)", 2, 0.03},
		{"cbrDrive(1, 8, 8, 8, 3)", 3, 0.06},
		{"cbrDrive(1, 8, 8, 8)", 2, 0.03},
		{"cbrDrive(1, 8, 8)", 2, 0.03},
		{"cbrDrive(1, 8, 8, 8, nil)", 2, 0.03},
		{"cbrDrive(1, 8, 8, 8, 0)", 2, 0.03},
		{"cbrDrive(1, 8, 8, 8, 4)", 2, 0.03},
		{"cbrDrive(1, 8, 8, 8, 'High')", 2, 0.03},
	} {
		if err := l.DoString(tc.call); err != nil {
			t.Fatalf("%s: %v", tc.call, err)
		}
		slot := &sysCBR.slots[0]
		if !slot.drive || slot.margin != tc.margin {
			t.Fatalf("%s: slot drive=%v margin %d, want %d", tc.call, slot.drive, slot.margin, tc.margin)
		}
		if err := eng.SetPlayer(0, "chars/a/a.def", "chars/b/b.def"); err != nil {
			t.Fatal(err)
		}
		slot.active = false
		cbrStartDrive(0, slot, 4)
		if !slot.active || !eng.Driving(0) {
			t.Fatalf("%s: the slot should be driven", tc.call)
		}
		if got := eng.TakeoverMargin(0); got != tc.want {
			t.Fatalf("%s: take-over margin %v, want %v", tc.call, got, tc.want)
		}
	}
}
