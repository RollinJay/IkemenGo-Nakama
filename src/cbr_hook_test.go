package main

import (
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

// cbrTestRuntime gives the CBR layer an engine and an offline match in
// which it may drive, and restores everything when the test ends.
func cbrTestRuntime(t testing.TB) *cbr.Engine {
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
	noJam := func() bool { return s.aiFlags&ASF_noaibuttonjam != 0 }
	if !noJam() || s.wasDriven {
		t.Fatalf("after the AI's frame: noJam %v wasDriven %v", noJam(), s.wasDriven)
	}
	s.driven = true
	root.unsetASF(ASF_noaibuttonjam)
	cbrEndFrame()
	if !noJam() || !s.wasDriven {
		t.Fatalf("after the CBR layer's frame: noJam %v wasDriven %v", noJam(), s.wasDriven)
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

// TestCBRBuiltInMovesKeepTheAIsFlags: on the first frame the character's
// own AI plays after the CBR layer's, the engine's built-in moves see the
// flags the AI asserted on its last frame, through InputUpdate and
// Char.actionPrepare, although the flags they read are from the CBR layer's
// frame, when the AI was paused. So an AI that leaves the engine AI's button
// jam on but switches off the built-in jump and walk only while AILevel
// reads above 0 (as the stock ZSS Kung Fu Man's does) does not jump or walk
// on the jam there. The flags are only added, never cleared, and the usual
// reset clears them before the character's code runs.
func TestCBRBuiltInMovesKeepTheAIsFlags(t *testing.T) {
	cbrTestRuntime(t)
	savedChars, savedAI, savedLevel, savedIntro := sys.chars, sys.aiInput, sys.aiLevel, sys.intro
	savedCgi, savedIgnore, savedRound := sys.cgi[0], sys.ignoreMostErrors, sys.fightScreen.round
	t.Cleanup(func() {
		sys.chars, sys.aiInput, sys.aiLevel, sys.intro = savedChars, savedAI, savedLevel, savedIntro
		sys.cgi[0], sys.ignoreMostErrors, sys.fightScreen.round = savedCgi, savedIgnore, savedRound
	})
	sys.fightScreen.round = &FightScreenRound{} // the round is in progress
	root := &Char{controller: -1, minus: 3, localcoord: 320, localscl: 1, life: 1000}
	root.cmd = []CommandList{*NewCommandList(NewInputBuffer())}
	root.keyctrl[0] = true
	sys.chars = [len(sys.chars)][]*Char{{root}}
	sys.cgi[0].mugenver, sys.cgi[0].ikemenver = [2]uint16{1, 1}, [3]uint16{0, 1}
	sys.intro = 0
	sys.aiLevel[0] = 8
	sys.ignoreMostErrors = true // the test character has no states to enter
	s := &sysCBR.slots[0]
	s.active = true // the engine has no player 0, so it declines every frame

	// start runs the start of a frame of the character's AI, standing with
	// control in state 0, with the engine AI's jam holding dir (0 up, 2
	// right, 4 down, 6 left): InputUpdate, then actionPrepare, whose built-in
	// moves act on the input. It reports the state they put the character
	// in, and which flags the character had when they did.
	start := func(dir int32) (state int32, flags AssertSpecialFlag) {
		root.ss.no, root.ss.prevno, root.ss.time, root.ss.stateType = 0, 0, 10, ST_S
		root.setSCF(SCF_ctrl)
		sys.aiInput[0] = AiInput{dir: dir, dirt: 30}
		root.cmd[0].Buffer.Reset()
		root.cmd[0].InputUpdate(root, -1)
		flags = root.assertFlag
		root.actionPrepare()
		return root.ss.no, flags
	}
	// end ends a frame: the character's code asserts flags (none on the CBR
	// layer's frames, where the AI is paused), and the CBR layer notes the
	// frame.
	end := func(driven bool, asserted AssertSpecialFlag) {
		root.assertFlag |= asserted
		s.driven = driven
		cbrEndFrame()
	}
	if got, _ := start(0); got != 40 {
		t.Fatalf("setup: the jam holding up should jump, got state %d", got)
	}
	if got, _ := start(2); got != 20 {
		t.Fatalf("setup: the jam holding right should walk, got state %d", got)
	}

	// The AI's frame switches the built-in jump and walk off, then the CBR
	// layer gives the input for a frame, then the AI plays again.
	for _, tc := range []struct {
		name  string
		dir   int32
		moved int32
		flag  AssertSpecialFlag
	}{
		{"jump", 0, 40, ASF_nojump | ASF_noairjump},
		{"walk", 2, 20, ASF_nowalk | ASF_nobrake},
	} {
		start(tc.dir)
		end(false, tc.flag)
		start(tc.dir)
		end(true, 0)
		got, flags := start(tc.dir)
		if got != 0 {
			t.Fatalf("%s: the AI switches the built-in %s off, but the jam made it on its first frame after the CBR layer's (state %d)", tc.name, tc.name, got)
		}
		if flags&tc.flag != tc.flag {
			t.Fatalf("%s: the built-in moves saw flags %#x, want %#x set", tc.name, flags, tc.flag)
		}
		if root.asf(tc.flag) {
			t.Fatalf("%s: the reset must clear the flags before the character's code runs", tc.name)
		}
		// From then on the AI's own flags hold: it asserted nothing on that
		// frame, so the jam moves it on the next.
		end(false, 0)
		if got, _ := start(tc.dir); got != tc.moved {
			t.Fatalf("%s: on the AI's next frame the flags are its own, got state %d", tc.name, got)
		}
		end(false, 0)
	}

	// An AI that asserted nothing gets the jam's moves after the CBR layer's
	// frames too: nothing is added that it did not have.
	start(0)
	end(false, 0)
	start(0)
	end(true, 0)
	if got, _ := start(0); got != 40 {
		t.Fatalf("an AI without NoJump jumps on the jam after the CBR layer's frames, got state %d", got)
	}
	end(false, 0)

	// Guarding, with an attack near and the jam holding back: the CBR
	// layer's frame left the character able to guard, but an AI that
	// switched standing guard off on its last frame does not guard, and
	// walks back as it would without the CBR layer (back walks when guarding
	// is off); one that switched only crouching guard off guards.
	root.inguarddist = true
	guard := func() int32 {
		root.setSCF(SCF_guard) // as the frame before left it
		got, _ := start(6)
		return got
	}
	if got := guard(); got != 120 {
		t.Fatalf("setup: the jam holding back should guard, got state %d", got)
	}
	for _, tc := range []struct {
		flag AssertSpecialFlag
		want int32
	}{{ASF_nostandguard, 20}, {ASF_nocrouchguard, 120}} {
		end(false, 0)
		guard()
		end(false, tc.flag)
		guard()
		end(true, 0)
		if got := guard(); got != tc.want {
			t.Fatalf("flag %#x standing: state %d on the AI's first frame after the CBR layer's, want %d", tc.flag, got, tc.want)
		}
	}
	end(false, 0)
	root.inguarddist = false

	// Every flag of the built-in moves is carried over, and a flag the
	// character asserted on the CBR layer's frame (its own code, not its AI)
	// is kept.
	for _, f := range []AssertSpecialFlag{ASF_nojump, ASF_noairjump, ASF_nowalk, ASF_nobrake, ASF_nocrouch, ASF_nostand, ASF_nohardcodedkeys} {
		start(4)
		end(false, f)
		start(4)
		end(true, ASF_noautoturn)
		if _, flags := start(4); flags&f == 0 || flags&ASF_noautoturn == 0 {
			t.Fatalf("flag %#x: the built-in moves saw flags %#x", f, flags)
		}
		end(false, 0)
	}
}

// TestCBRBackWalkAfterDrivenFrames: on the second frame the character's own
// AI plays after the CBR layer's, a back walk the AI gets without the CBR
// layer (standing guard switched off by the AI, an attack near, the engine
// AI's jam holding back) still comes out. The engine's walk reads
// NoStandGuard a frame late (Char.prevNoStandGuard), so the guard flags are
// given back with the move flags on the first base frame.
func TestCBRBackWalkAfterDrivenFrames(t *testing.T) {
	cbrTestRuntime(t)
	savedChars, savedAI, savedLevel, savedIntro := sys.chars, sys.aiInput, sys.aiLevel, sys.intro
	savedCgi, savedIgnore, savedRound := sys.cgi[0], sys.ignoreMostErrors, sys.fightScreen.round
	t.Cleanup(func() {
		sys.chars, sys.aiInput, sys.aiLevel, sys.intro = savedChars, savedAI, savedLevel, savedIntro
		sys.cgi[0], sys.ignoreMostErrors, sys.fightScreen.round = savedCgi, savedIgnore, savedRound
	})
	sys.fightScreen.round = &FightScreenRound{}
	root := &Char{controller: -1, minus: 3, localcoord: 320, localscl: 1, life: 1000}
	root.cmd = []CommandList{*NewCommandList(NewInputBuffer())}
	root.keyctrl[0] = true
	sys.chars = [len(sys.chars)][]*Char{{root}}
	sys.cgi[0].mugenver, sys.cgi[0].ikemenver = [2]uint16{1, 1}, [3]uint16{0, 1}
	sys.intro = 0
	sys.aiLevel[0] = 8
	sys.ignoreMostErrors = true
	s := &sysCBR.slots[0]
	s.active = true // the engine has no player 0, so it declines every frame
	root.inguarddist = true
	// frame runs a frame's start with the jam holding back, standing with
	// control, and its end: the AI asserts NoStandGuard on its own frames,
	// and the engine works out guarding from the flags.
	frame := func(driven bool) int32 {
		root.ss.no, root.ss.prevno, root.ss.time, root.ss.stateType = 0, 0, 10, ST_S
		root.setSCF(SCF_ctrl)
		sys.aiInput[0] = AiInput{dir: 6, dirt: 30}
		root.cmd[0].Buffer.Reset()
		root.cmd[0].InputUpdate(root, -1)
		root.actionPrepare()
		got := root.ss.no
		if !driven {
			root.assertFlag |= ASF_nostandguard
		}
		if root.asf(ASF_nostandguard) {
			root.unsetSCF(SCF_guard)
		} else {
			root.setSCF(SCF_guard)
		}
		s.driven = driven
		cbrEndFrame()
		return got
	}
	var alone []int32
	for i := 0; i < 6; i++ {
		alone = append(alone, frame(false))
	}
	if alone[5] != 20 {
		t.Fatalf("setup: the AI alone walks back, got %v", alone)
	}
	var with []int32
	for _, d := range []bool{false, false, false, true, false, false, false} {
		with = append(with, frame(d))
	}
	for i := 4; i < len(with); i++ {
		if with[i] != 20 {
			t.Fatalf("base frame %d after the CBR layer's: state %d, the AI alone walks back (states %v)", i-3, with[i], with)
		}
	}
}

// TestCBRSnapshotEntry: the label of a watched root's state change is in
// the snapshot of the frame it was made on, and only that frame; the route
// table pointer is on every frame, for the root and its helpers alike.
func TestCBRSnapshotEntry(t *testing.T) {
	w := cbrCompileWorld(t, cbrTestNegOne, cbrTestStates, false)
	c := w.root
	c.teamside = 0
	h := &Char{id: 57, playerNo: 0, helperIndex: 1, teamside: 0}
	sys.chars[0] = append(sys.chars[0], h)
	snap := func() *cbr.PlayerSnap {
		cbrBuildSnapshot(&sysCBR.snap)
		return &sysCBR.snap.Players[0]
	}
	w.enter(0)
	c.cnsvar[1] = 1
	w.frame()
	w.run(-1)
	p := snap()
	if p.Root.Entry != cbr.EntryReachable || p.Root.EntryCommand != "x" || p.Root.EntryHelper || p.Root.StateNo != 200 {
		t.Fatalf("frame of the change: %v %q helper %v state %d", p.Root.Entry, p.Root.EntryCommand, p.Root.EntryHelper, p.Root.StateNo)
	}
	routes := p.Root.Routes
	if routes == nil || len(p.Helpers) != 1 || p.Helpers[0].Routes != routes || p.Helpers[0].Entry != cbr.EntryNone {
		t.Fatalf("route table: root %p helpers %+v", routes, p.Helpers)
	}
	cbrEndFrame()
	p = snap()
	if p.Root.Entry != cbr.EntryNone || p.Root.EntryCommand != "" || p.Root.Routes != routes {
		t.Fatalf("next frame: %v %q, routes %p (was %p)", p.Root.Entry, p.Root.EntryCommand, p.Root.Routes, routes)
	}
	// Another root in the slot (a Turns member) does not get the label.
	w.enter(0)
	c.cnsvar[1] = 1
	w.frame()
	w.run(-1)
	w.slot.entryID = 99
	if p = snap(); p.Root.Entry != cbr.EntryNone {
		t.Fatalf("label for another character: %v", p.Root.Entry)
	}
}
