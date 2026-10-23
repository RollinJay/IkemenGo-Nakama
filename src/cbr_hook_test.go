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

// TestCBRMaskEndsWithTheDrivenFrame: the NoAILevel flag that pauses the
// character's AI holds only while a frame the CBR layer drives runs. It is
// cleared once the frame's code has run, and when the next frame starts
// without the mask, also for a WinMugen-version character in hitpause, whose
// AssertSpecial flags the reset keeps. A NoAILevel the character asserted
// itself is left as the engine would leave it. Helpers follow their root.
func TestCBRMaskEndsWithTheDrivenFrame(t *testing.T) {
	cbrTestRuntime(t)
	savedCgi := sys.cgi[0]
	t.Cleanup(func() { sys.cgi[0] = savedCgi })
	s := &sysCBR.slots[0]
	s.active = true
	// frame runs a character's frame start (Char.actionPrepare); a helper
	// is used, as player-only resets need a stage.
	frame := func(c *Char, driven bool, hitpause int32) bool {
		s.driven = driven
		c.hitPauseTime = hitpause
		c.actionPrepare()
		return c.asf(ASF_noailevel)
	}
	for _, v := range []struct {
		name      string
		mugen     [2]uint16
		ikemen    bool
		keepsFlag bool // AssertSpecial flags last through hitpause
	}{
		{"winmugen", [2]uint16{0, 0}, false, true},
		{"mugen 1.0", [2]uint16{1, 0}, false, false},
		{"ikemen", [2]uint16{1, 1}, true, false},
	} {
		sys.cgi[0].mugenver = v.mugen
		sys.cgi[0].ikemenver = [3]uint16{}
		if v.ikemen {
			sys.cgi[0].ikemenver[1] = 1
		}
		newHelper := func() *Char {
			c := &Char{id: 7, minus: 3, helperIndex: 1}
			return c
		}
		for _, hp := range []int32{0, 5} {
			// A driven frame masks; the next frame, the base AI's, does not,
			// with or without the end-of-frame clear in between.
			for _, endClear := range []bool{true, false} {
				c := newHelper()
				if !frame(c, true, hp) {
					t.Fatalf("%s, hitpause %d: a driven frame must mask AILevel", v.name, hp)
				}
				if endClear {
					cbrUnmaskAll()
					if c.asf(ASF_noailevel) {
						t.Fatalf("%s, hitpause %d: the mask must end with the frame", v.name, hp)
					}
				}
				if frame(c, false, hp) {
					t.Fatalf("%s, hitpause %d (end clear %v): AILevel still masked on the base AI's frame", v.name, hp, endClear)
				}
				if len(sysCBR.masked) != 0 {
					t.Fatalf("%s: marks left over: %d", v.name, len(sysCBR.masked))
				}
			}
			// Driven frames in a row keep the mask.
			c := newHelper()
			frame(c, true, hp)
			cbrUnmaskAll()
			if !frame(c, true, hp) {
				t.Fatalf("%s, hitpause %d: a second driven frame must mask again", v.name, hp)
			}
			cbrUnmaskAll()
		}
		// The character's own NoAILevel, asserted before hitpause, is left as
		// the engine leaves it: kept through hitpause only where flags are.
		c := newHelper()
		c.setASF(ASF_noailevel)
		frame(c, true, 5)
		cbrUnmaskAll()
		if got := c.asf(ASF_noailevel); got != v.keepsFlag {
			t.Fatalf("%s: the character's own NoAILevel after a driven frame in hitpause: %v, want %v", v.name, got, v.keepsFlag)
		}
		c.setASF(ASF_noailevel)
		if got := frame(c, false, 5); got != v.keepsFlag {
			t.Fatalf("%s: the character's own NoAILevel on a base frame in hitpause: %v, want %v", v.name, got, v.keepsFlag)
		}
	}
	// A helper of another player is not masked; nor is anything once the
	// slot is no longer driven by the CBR layer.
	other := &Char{id: 8, minus: 3, helperIndex: 1, playerNo: 1}
	s.driven = true
	other.actionPrepare()
	if other.asf(ASF_noailevel) {
		t.Fatal("another player's helper must not be masked")
	}
	c := &Char{id: 9, minus: 3, helperIndex: 1}
	c.actionPrepare()
	s.active = false
	c.actionPrepare()
	if c.asf(ASF_noailevel) {
		t.Fatal("the mask must end when the slot is no longer driven")
	}
}

// TestCBRButtonJamStaysOffAfterDrivenFrames: on the first frame the
// character's own AI plays after the CBR layer's, an AI that had the engine
// AI's button jam off on its last frame keeps it off, through InputUpdate,
// although the flag InputUpdate reads is from the CBR layer's frame; the
// frame stays the AI's. Without NoAIButtonJam the jam plays as usual.
func TestCBRButtonJamStaysOffAfterDrivenFrames(t *testing.T) {
	cbrTestRuntime(t)
	savedChars, savedAI, savedLevel, savedIntro := sys.chars, sys.aiInput, sys.aiLevel, sys.intro
	t.Cleanup(func() { sys.chars, sys.aiInput, sys.aiLevel, sys.intro = savedChars, savedAI, savedLevel, savedIntro })
	root := &Char{controller: -1}
	sys.chars = [len(sys.chars)][]*Char{{root}}
	sys.intro = 0
	sys.aiLevel[0] = 8
	s := &sysCBR.slots[0]
	s.active = true // the engine has no player 0, so it declines every frame

	// jammed runs InputUpdate with the engine AI's jam holding up, as the
	// AI's own flag of the frame before (jamOff) allows, and reports whether
	// up reached the input buffer.
	cl := NewCommandList(NewInputBuffer())
	jammed := func(jamOff bool) bool {
		sys.aiInput[0] = AiInput{dir: 0, dirt: 30}
		root.unsetASF(ASF_noaibuttonjam)
		if jamOff {
			root.setASF(ASF_noaibuttonjam)
		}
		cl.Buffer.Reset()
		cl.InputUpdate(root, -1)
		return cl.Buffer.Ub > 0
	}
	if !jammed(false) {
		t.Fatal("setup: the jam should press up")
	}

	// The AI's frame with NoAIButtonJam, then a frame the CBR layer gives
	// the input (the paused AI asserts nothing), then the AI's again.
	root.setASF(ASF_noaibuttonjam)
	cbrEndFrame()
	if !s.noJam || s.wasDriven {
		t.Fatalf("after the AI's frame: noJam %v wasDriven %v", s.noJam, s.wasDriven)
	}
	s.driven = true
	root.unsetASF(ASF_noaibuttonjam)
	cbrEndFrame()
	if !s.noJam || !s.wasDriven {
		t.Fatalf("after the CBR layer's frame: noJam %v wasDriven %v", s.noJam, s.wasDriven)
	}
	if jammed(false) {
		t.Fatal("the jam the AI keeps off must stay off on its first frame after the CBR layer's")
	}
	if s.driven || cbrMaskAILevel(root) {
		t.Fatal("that frame is the AI's: nothing is paused")
	}
	// Only that first frame: the AI's own flag holds from then on.
	cbrEndFrame()
	if s.wasDriven {
		t.Fatal("the frame after is not after a driven frame")
	}
	if jammed(true) {
		t.Fatal("the AI's own NoAIButtonJam keeps the jam off")
	}
	if !jammed(false) {
		t.Fatal("an AI that lets the jam play gets it")
	}
	// An AI that leaves the jam on gets it after the CBR layer's frames too.
	root.unsetASF(ASF_noaibuttonjam)
	cbrEndFrame()
	s.driven = true
	cbrEndFrame()
	if !jammed(false) {
		t.Fatal("an AI without NoAIButtonJam gets the jam after the CBR layer's frames")
	}
}
