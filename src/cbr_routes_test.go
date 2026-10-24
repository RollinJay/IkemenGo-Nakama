package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unsafe"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

// cbrRouteWorld is a small character compiled from text into player 1's
// slot, with the CBR layer watching it, for the route table tests.
type cbrRouteWorld struct {
	t      testing.TB
	root   *Char
	states map[int32]StateBytecode
	set    *cbrRouteSet
	slot   *cbrSlot
}

// cbrTestCmd is the command file the test characters share; the tests add
// its [Statedef -1].
const cbrTestCmd = `
[Defaults]
command.time = 15
command.buffer.time = 1

[Command]
name = "x"
command = x

[Command]
name = "y"
command = y

[Command]
name = "qcf_x"
command = ~D, DF, F, x

[Command]
name = "super"
command = ~D, DF, F, D, DF, F, x

[Command]
name = "holdfwd"
command = /$F

[Command]
name = "holddown"
command = /$D
`

// cbrTestStates is a CNS state file with the states the tests change to.
// State 220's own code goes on to 221 at once, and state 200 has a cancel
// into 400 of its own.
const cbrTestStates = `
[Statedef 0]
type = S
movetype = I
ctrl = 1

[State 0, none]
type = Null
trigger1 = 1

[Statedef 200]
type = S
movetype = A
ctrl = 0

[State 200, own cancel]
type = ChangeState
value = 400
trigger1 = command = "y" && movecontact

[Statedef 210]
type = S
movetype = A
ctrl = 0

[Statedef 220]
type = S
movetype = A
ctrl = 0

[State 220, follow-up]
type = ChangeState
trigger1 = time = 0
value = 221

[Statedef 221]
type = S
movetype = A
ctrl = 0

[Statedef 400]
type = S
movetype = A
ctrl = 0

[Statedef 1000]
type = S
movetype = A
ctrl = 0

[Statedef 3000]
type = S
movetype = A
ctrl = 0

[Statedef 3100]
type = S
movetype = A
ctrl = 0

[Statedef 9000]
type = S
movetype = I
ctrl = 0

[State 9000, helper moves the root]
type = ChangeState
trigger1 = var(1) = 1
redirectid = 56
value = 200

[State 9000, helper AI]
type = ChangeState
trigger1 = root,AILevel && var(2) = 1
redirectid = 56
value = 3000
`

// cbrTestNegOne is the [Statedef -1] of the CNS test character: each AI
// route gated on its own variable, so that one test frame fires one route.
const cbrTestNegOne = `
[State -1, AI jab]
type = ChangeState
value = 200
triggerall = AILevel > 0
trigger1 = var(1) = 1

[State -1, AI only]
type = ChangeState
value = 3000
trigger1 = AILevel && var(2) = 1

[State -1, AI super]
type = ChangeState
value = 3100
trigger1 = AILevel && var(3) = 1

[State -1, AI to the guarded state]
type = ChangeState
value = 1000
trigger1 = AILevel && var(4) = 1

[State -1, AI cancel]
type = ChangeState
value = 210
trigger1 = AILevel && stateno = 200 && var(5) = 1
ignorehitpause = 1

[State -1, latched cancel]
type = ChangeState
value = 210
trigger1 = var(6) = 1 && stateno = 200

[State -1, AI into a move with a follow-up]
type = ChangeState
value = 220
trigger1 = AILevel && var(7) = 1

[State -1, latched from control]
type = ChangeState
value = 200
trigger1 = var(8) = 1 && ctrl

[State -1, AI to the assigning route's state]
type = ChangeState
value = 400
trigger1 = AILevel && var(10) = 1

[State -1, super]
type = ChangeState
value = 3100
triggerall = command = "super"
trigger1 = power >= 1000 && ctrl

[State -1, cancel]
type = ChangeState
value = 210
trigger1 = command = "x" && stateno = 200 && movecontact

[State -1, jab]
type = ChangeState
value = 200
triggerall = command = "x"
trigger1 = ctrl

[State -1, qcf]
type = ChangeState
value = 1000
triggerall = command = "qcf_x" && command != "holddown"
trigger1 = ctrl && power >= 0

[State -1, assigns]
type = ChangeState
value = 400
trigger1 = command = "y" && ctrl && (var(9) := 5)

[State -1, AI command]
type = ChangeState
value = 221
triggerall = AILevel > 0
trigger1 = command = "holdfwd"
`

// cbrCompileWorld compiles a character whose command file is cbrTestCmd
// with negOne as its [Statedef -1], and whose state file is st (ZSS when
// zss is set), into player 1's slot, and sets it up as a CPU-controlled
// root the CBR layer watches, standing in state 0 with control. Everything
// is restored when the test ends.
func cbrCompileWorld(t testing.TB, negOne, st string, zss bool) *cbrRouteWorld {
	t.Helper()
	cbrTestRuntime(t)
	savedChars, savedCgi, savedCommon := sys.chars, sys.cgi[0], sys.cfg.Common
	savedPool, savedSets, savedIgnore := sys.stringPool[0], cbrRouteSets, sys.ignoreMostErrors
	savedList, savedLevel, savedSeed := sys.charList, sys.aiLevel, sys.randseed
	savedWS, savedWC, savedConsole := sys.workingState, sys.workingChar, sys.consoleText
	savedRound := sys.fightScreen.round
	t.Cleanup(func() {
		sys.fightScreen.round = savedRound
		sys.chars, sys.cgi[0], sys.cfg.Common = savedChars, savedCgi, savedCommon
		sys.stringPool[0], cbrRouteSets, sys.ignoreMostErrors = savedPool, savedSets, savedIgnore
		sys.charList, sys.aiLevel, sys.randseed = savedList, savedLevel, savedSeed
		sys.workingState, sys.workingChar, sys.consoleText = savedWS, savedWC, savedConsole
		cbrCheck = cbrCheckState{}
	})
	dir := t.TempDir()
	stName := "test.cns"
	if zss {
		stName = "test.zss"
	}
	files := map[string]string{
		"test.def": "[Info]\nname = cbrtest\nikemenversion = 1.0\n\n[Files]\ncmd = test.cmd\nst = " + stName + "\n",
		"test.cmd": cbrTestCmd + "\n[Statedef -1]\n" + negOne,
		stName:     st,
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root := &Char{id: 56, playerNo: 0, controller: -1, localcoord: 320, localscl: 1,
		life: 1000, lifeMax: 1000, powerMax: 3000, cpucmd: -1, facing: 1}
	root.keyctrl = [4]bool{true, true, true, true}
	root.cnsvar, root.cnsfvar = map[int32]int32{}, map[int32]float32{}
	sys.chars = [len(sys.chars)][]*Char{{root}}
	sys.cgi[0] = newCharGlobalInfo()
	sys.cfg.Common.Cmd, sys.cfg.Common.States = nil, nil
	sys.ignoreMostErrors = true
	sys.fightScreen.round = &FightScreenRound{} // the round is in progress
	states, err := newCharCompiler().Compile(0, filepath.Join(dir, "test.def"), map[string]float32{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	sys.cgi[0].states = states
	sys.charList = CharList{idMap: map[int32]*Char{root.id: root}, runOrder: []*Char{root}}
	sys.aiLevel[0] = 8
	w := &cbrRouteWorld{t: t, root: root, states: states, set: cbrRoutesFor(0), slot: &sysCBR.slots[0]}
	if w.set == nil {
		t.Fatal("no route table for the compiled character")
	}
	w.slot.routes, w.slot.watched = w.set, true
	cbrWatchAny()
	w.enter(0)
	return w
}

// enter puts the root in state no, its own, at time 10, standing with
// control in state 0 and without it elsewhere, out of hitpause, with no
// move contact, no variables set and no command in its input buffer.
func (w *cbrRouteWorld) enter(no int32) {
	c := w.root
	c.ss.sb = w.states[no]
	c.ss.sb.ctrlsps = make([]int32, len(c.ss.sb.ctrlsps))
	c.ss.no, c.ss.prevno, c.ss.time = no, 0, 10
	c.ss.stateType, c.ss.moveType = ST_S, MT_I
	c.unsetSCF(SCF_ctrl)
	if no == 0 {
		c.setSCF(SCF_ctrl)
	} else {
		c.ss.moveType = MT_A
	}
	c.mctime, c.mctype, c.hitPauseTime = 0, MC_Hit, 0
	c.minus = 3
	clear(c.cnsvar)
	w.press()
}

// press puts the commands in the root's real input buffer, as a person's
// input completing them would, and takes every other out.
func (w *cbrRouteWorld) press(names ...string) {
	cl := &w.root.cmd[0]
	for i := range cl.Commands {
		for j := range cl.Commands[i] {
			cl.Commands[i][j].curbuftime = 0
		}
	}
	for _, n := range names {
		i, ok := cl.Names[n]
		if !ok {
			w.t.Fatalf("no command %q", n)
		}
		cl.Commands[i][0].curbuftime = 1
	}
}

// person makes the root a person's: AILevel 0, input from the buffer only.
func (w *cbrRouteWorld) person() {
	w.root.controller = 0
	sys.aiLevel[0] = 0
}

// frame starts a new frame for the labels.
func (w *cbrRouteWorld) frame() {
	sysCBR.frame++
}

// run runs the root's state no as Char.actionRun runs a negative state, or
// its current state when no is the current state, and returns the label
// the root's state change got this frame (none when it did not change) and
// the state it is in.
func (w *cbrRouteWorld) run(no int32) (cbrEntry, int32) {
	w.t.Helper()
	c := w.root
	sb, ok := w.states[no]
	if no >= 0 {
		c.minus = 0
		c.ss.sb.run(c)
	} else {
		if !ok {
			w.t.Fatalf("no state %d", no)
		}
		c.minus = int8(no)
		sb.run(c)
		c.stateChange2()
	}
	c.minus = 3
	return w.label(), c.ss.no
}

// label returns the label of the root's state change this frame.
func (w *cbrRouteWorld) label() cbrEntry {
	if w.slot.entryFrame != sysCBR.frame || w.slot.entryID != w.root.id {
		return cbrEntry{}
	}
	return w.slot.entry
}

// decide runs a frame of [Statedef -1] with AI variable v set, from state
// from (with control in state 0), and returns the label.
func (w *cbrRouteWorld) decide(from int32, v int32, setup func()) (cbrEntry, int32) {
	w.t.Helper()
	w.enter(from)
	if v > 0 {
		w.root.cnsvar[v] = 1
	}
	if setup != nil {
		setup()
	}
	w.frame()
	return w.run(-1)
}

func (e cbrEntry) String() string {
	return fmt.Sprintf("%v %q helper=%v", e.kind, e.cmd, e.helper)
}

// TestCBRRouteTable: the table lists, by target, the person routes of the
// command and state files with their main button command (the button, not
// the held direction), none for a state only the AI's routes enter, and
// none for a route gated on the AI level above 0.
func TestCBRRouteTable(t *testing.T) {
	w := cbrCompileWorld(t, cbrTestNegOne+`
[State -1, forward jab]
type = ChangeState
value = 210
triggerall = command = "holdfwd" && command = "x"
trigger1 = ctrl

[State -1, AI command]
type = ChangeState
value = 221
triggerall = AILevel > 0
trigger1 = command = "y"
`, cbrTestStates, false)
	for _, tc := range []struct {
		state int32
		cmds  []string
		known bool
	}{
		{200, []string{"x"}, true},
		{210, []string{"x"}, true},
		{400, []string{"y"}, true}, // state 200's own cancel, and the assigning route
		{1000, []string{"qcf_x"}, true},
		{3100, []string{"super"}, true},
		{3000, nil, true}, // only the AI's routes enter it
		{221, nil, true},  // a follow-up, and a route gated on the AI level
		{5, nil, true},
	} {
		cmds, known := w.set.table.PersonCommands(tc.state)
		if !reflect.DeepEqual(cmds, tc.cmds) || known != tc.known {
			t.Errorf("state %d: commands %v known %v, want %v %v", tc.state, cmds, known, tc.cmds, tc.known)
		}
	}
	// What each route records: a fixed target, commands with whether they
	// must be active, the AI level, an assignment, hitpause.
	find := func(target int32, cmd string) *cbrRoute {
		for i := range w.set.routes {
			r := &w.set.routes[i]
			if r.target == target && r.fixed && (cmd == "" && !r.anyCmd || cmd != "" && len(r.groups) > 0 && cbrHas(r.groups[0].cmds, cmd)) {
				return r
			}
		}
		t.Fatalf("no route into %d with %q", target, cmd)
		return nil
	}
	if r := find(1000, "qcf_x"); r.owner != -1 || r.anyAI || !r.person || !reflect.DeepEqual(r.groups[0].notCmds, []string{"holddown"}) || !r.groups[0].checkable {
		t.Errorf("qcf route: %+v", r)
	}
	if r := find(400, "y"); r.owner != -1 || r.groups[0].checkable {
		t.Errorf("the route that assigns a variable must not be checkable: %+v", r.groups[0])
	}
	if r := find(400, ""); r.groups[0].aiAbove != true || !r.anyAI {
		t.Errorf("AI route into 400: %+v", r.groups[0])
	}
	for i := range w.set.routes {
		r := &w.set.routes[i]
		if r.target == 210 && !r.anyCmd && r.hitpause != r.anyAI {
			t.Errorf("only the AI cancel has ignorehitpause: route %d anyAI %v hitpause %v", r.id, r.anyAI, r.hitpause)
		}
		if r.target == 200 && r.redirect != (r.owner == 9000) {
			t.Errorf("route %d: redirect %v in state %d", r.id, r.redirect, r.owner)
		}
	}
	if r := find(221, ""); r.owner != 220 {
		t.Errorf("follow-up in state %d", r.owner)
	}
	// The same pointer on every frame, in the snapshot.
	var a, b cbr.CharSnap
	cbrFillChar(&a, w.root, true)
	cbrFillChar(&b, w.root, true)
	if a.Routes == nil || a.Routes != b.Routes || a.Routes != &w.set.table {
		t.Fatalf("snapshot route table: %p %p", a.Routes, b.Routes)
	}
}

// TestCBRAINeedOf: how the text after AILevel reads.
func TestCBRAINeedOf(t *testing.T) {
	for rest, want := range map[string]cbrAINeed{
		"":            cbrAIAbove,
		" && ctrl":    cbrAIAbove,
		") || x":      cbrAIAbove,
		" = 0":        cbrAIZero,
		"=0 && ctrl":  cbrAIZero,
		" != 0":       cbrAIAbove,
		" > 0":        cbrAIAbove,
		" >= 1":       cbrAIAbove,
		" >= 0":       cbrAIUnclear,
		" < 1":        cbrAIZero,
		" <= 0":       cbrAIZero,
		" = 8":        cbrAIAbove,
		" = [1, 8]":   cbrAIAbove,
		" = [0, 8]":   cbrAIUnclear,
		" * 2 > 3":    cbrAIUnclear,
		" = var(59)":  cbrAIUnclear,
		" < 5":        cbrAIUnclear,
		" > -1":       cbrAIUnclear, // always true
		" = (0, 8]":   cbrAIAbove,
		" <= 0.5":     cbrAIZero,
		"\t&& random": cbrAIAbove,
	} {
		if got := cbrAINeedOf(rest); got != want {
			t.Errorf("AILevel%q: %d, want %d", rest, got, want)
		}
	}
}

// TestCBRMomentCheck: each kind of state change of a watched character gets
// its label, the AI's decisions by evaluating, at that moment, the person
// routes into the same state.
func TestCBRMomentCheck(t *testing.T) {
	w := cbrCompileWorld(t, cbrTestNegOne, cbrTestStates, false)
	c := w.root
	for _, tc := range []struct {
		name   string
		from   int32
		v      int32 // the AI variable that fires a route
		setup  func()
		want   cbrEntry
		target int32
	}{
		{"AI jab with control: the person's jab needs control", 0, 1, nil,
			cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 200},
		{"AI jab without control", 0, 1, func() { c.unsetSCF(SCF_ctrl) },
			cbrEntry{kind: cbr.EntryCheat}, 200},
		{"AI-only state", 0, 2, nil, cbrEntry{kind: cbr.EntryCheat}, 3000},
		{"AI super below the power", 0, 3, func() { c.power = 500 },
			cbrEntry{kind: cbr.EntryCheat}, 3100},
		{"AI super at the power", 0, 3, func() { c.power = 1000 },
			cbrEntry{kind: cbr.EntryReachable, cmd: "super"}, 3100},
		{"AI into the state of a route that assigns", 0, 10, nil,
			cbrEntry{kind: cbr.EntryUnknown}, 400},
		{"AI cancel after contact", 200, 5, func() { c.mctime = 3 },
			cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 210},
		{"AI cancel without contact", 200, 5, nil,
			cbrEntry{kind: cbr.EntryCheat}, 210},
		{"AI cancel in hitpause: the person's comes out as it ends", 200, 5,
			func() { c.mctime, c.hitPauseTime = 3, 5 },
			cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 210},
		{"latched cancel (no command, no AI level) from an attack", 200, 6,
			func() { c.mctime = 3 }, cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 210},
		{"latched cancel without contact", 200, 6, nil,
			cbrEntry{kind: cbr.EntryCheat}, 210},
		{"latched from control", 0, 8, nil,
			cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 200},
		{"AI into a move whose own code follows up at once", 0, 7, nil,
			cbrEntry{kind: cbr.EntryCheat}, 221},
		{"person's jab", 0, 0, func() { w.person(); w.press("x") },
			cbrEntry{kind: cbr.EntryInput, cmd: "x"}, 200},
		{"person's quarter circle", 0, 0, func() { w.person(); w.press("qcf_x") },
			cbrEntry{kind: cbr.EntryInput, cmd: "qcf_x"}, 1000},
		{"person's quarter circle while holding down is not it", 0, 0,
			func() { w.person(); w.press("qcf_x", "holddown") }, cbrEntry{}, 0},
		{"the AI's jam pressing the jab is input", 0, 0, func() { w.press("x") },
			cbrEntry{kind: cbr.EntryInput, cmd: "x"}, 200},
		{"the engine AI's cheat making the quarter circle true", 0, 0, func() {
			c.cpucmd = int32(c.cmd[0].Names["qcf_x"])
		}, cbrEntry{kind: cbr.EntryReachable, cmd: "qcf_x"}, 1000},
		{"person's route that assigns", 0, 0, func() { w.person(); w.press("y") },
			cbrEntry{kind: cbr.EntryInput, cmd: "y"}, 400},
		{"the AI's command route, its command pressed by the jam", 0, 0, func() { w.press("holdfwd") },
			cbrEntry{kind: cbr.EntryCheat}, 221},
	} {
		sys.aiLevel[0], c.controller, c.power, c.cpucmd = 8, -1, 0, -1
		got, state := w.decide(tc.from, tc.v, tc.setup)
		if tc.v == 7 {
			// The new state's own code runs in the same frame, as
			// Char.changeStateEx runs it outside negative states.
			if state != 220 {
				t.Fatalf("%s: state %d", tc.name, state)
			}
			got, state = w.run(220)
			if state != 221 {
				t.Fatalf("%s: the follow-up did not come: state %d", tc.name, state)
			}
		}
		if got != tc.want || state != tc.target && tc.target != 0 {
			t.Errorf("%s: %v into %d, want %v into %d", tc.name, got, state, tc.want, tc.target)
		}
		if tc.name == "AI into the state of a route that assigns" && c.cnsvar[9] != 0 {
			t.Errorf("%s: the check evaluated the assignment: var(9) = %d", tc.name, c.cnsvar[9])
		}
	}
	// A follow-up on a later frame is part of the move.
	w.enter(220)
	w.root.ss.time = 0
	w.frame()
	if got, state := w.run(220); got.kind != cbr.EntryAuto || state != 221 {
		t.Errorf("follow-up: %v into %d", got, state)
	}
	// The state's own cancel, a person route in the current state.
	w.enter(200)
	w.person()
	w.press("y")
	c.mctime = 2
	w.frame()
	if got, state := w.run(200); got.kind != cbr.EntryInput || got.cmd != "y" || state != 400 {
		t.Errorf("own cancel: %v into %d", got, state)
	}
	// Changes the engine makes: a hit is part of the move; the built-in walk
	// from control is input.
	sys.aiLevel[0], c.controller = 8, -1
	w.enter(0)
	w.frame()
	c.changeStateEx(5000, 0, -1, 0, "")
	if got := w.label(); got.kind != cbr.EntryAuto {
		t.Errorf("hit: %v", got)
	}
	w.enter(0)
	w.frame()
	c.changeState(20, -1, -1, "")
	if got := w.label(); got.kind != cbr.EntryInput || got.cmd != "" {
		t.Errorf("built-in walk: %v", got)
	}
	// Not watched: no label.
	w.slot.watched = false
	w.decide(0, 1, nil)
	if got := w.label(); got.kind != cbr.EntryNone {
		t.Errorf("unwatched: %v", got)
	}
}

// TestCBRHelperRedirectIsLabelled: a helper's ChangeState redirected to the
// watched root is labelled as the root's change, made by a helper, and
// classified like the root's own: from control it is the AI's decision.
func TestCBRHelperRedirectIsLabelled(t *testing.T) {
	w := cbrCompileWorld(t, cbrTestNegOne, cbrTestStates, false)
	c := w.root
	h := &Char{id: 57, playerNo: 0, helperIndex: 1, controller: -1, localcoord: 320, localscl: 1}
	h.cnsvar, h.cnsfvar = map[int32]int32{}, map[int32]float32{}
	sys.chars[0] = append(sys.chars[0], h)
	sys.charList.idMap[h.id] = h
	sys.charList.runOrder = append(sys.charList.runOrder, h)
	runHelper := func(v int32) cbrEntry {
		clear(h.cnsvar)
		h.cnsvar[v] = 1
		h.ss.sb = w.states[9000]
		h.ss.no = 9000
		h.minus = 0
		w.frame()
		sb := w.states[9000]
		sb.run(h)
		h.minus = 3
		return w.label()
	}
	for _, tc := range []struct {
		name  string
		ctrl  bool
		v     int32
		want  cbrEntry
		state int32
	}{
		{"from control", true, 1, cbrEntry{kind: cbr.EntryReachable, cmd: "x", helper: true}, 200},
		{"without control", false, 1, cbrEntry{kind: cbr.EntryAuto, helper: true}, 200},
		{"gated on the AI level", true, 2, cbrEntry{kind: cbr.EntryCheat, helper: true}, 3000},
	} {
		w.enter(0)
		if !tc.ctrl {
			c.unsetSCF(SCF_ctrl)
		}
		if got := runHelper(tc.v); got != tc.want || c.ss.no != tc.state {
			t.Errorf("%s: %v into %d, want %v into %d", tc.name, got, c.ss.no, tc.want, tc.state)
		}
	}
}

// cbrTestZSS is a ZSS state file with the same states and routes in
// [StateDef -1] as ZSS writes them: nested ifs, else branches, a switch,
// local variables set by let and by a function's return value, as the stock
// ZSS Kung Fu Man's command.zss does, and a function that does more than set
// local variables.
const cbrTestZSS = `
[Function Combo() ret]
let ret = 0;
if ctrl || (stateNo = 200 && moveContact) {
	let ret = 1;
}

[Function Busy() ret]
let ret = 1;
mapSet{map: "busy"; value: 1}

[StateDef 0; type: S; moveType: I; ctrl: 1;]
var(20) := random;
if enemyNear(1),life > 0 {
	var(21) := 1;
}

[StateDef 200; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 210; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 230; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 400; type: S; moveType: A; ctrl: 0;]
persistent(0) {
	if command = "x" {
		changeState{value: 210}
	}
	var(41) := var(41) + 1;
}
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 1000; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 3000; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef 3100; type: S; moveType: A; ctrl: 0;]
if time >= 2 {
	changeState{value: 0; ctrl: 1}
}

[StateDef -1;]
if AILevel > 0 {
	switch var(1) {
	case 1:
		changeState{value: 200}
	case 2:
		changeState{value: 3000}
	case 3:
		changeState{value: 3100}
	case 4:
		changeState{value: 1000}
	case 5:
		if stateNo = 200 {
			changeState{value: 210}
		}
	case 6:
		changeState{value: 400}
	case 7:
		changeState{value: 230}
	case 8:
		changeState{value: 210}
	}
}
if !AILevel && command = "super" && power >= 1000 {
	changeState{value: 3100}
}
if command = "x" {
	if stateNo = 200 && moveContact {
		changeState{value: 210}
	} else if ctrl {
		changeState{value: 200}
	}
}
let canCombo = call Combo();
if command = "qcf_x" && $canCombo {
	changeState{value: 1000}
}
let ok = ctrl;
if $ok && command = "y" {
	changeState{value: 400}
}
if command = "holddown" && p2BodyDist x > 10 && p2BodyDist x < 100 {
	changeState{value: 3100}
}
let busy = call Busy();
if $busy && command = "holdfwd" {
	changeState{value: 230}
}
`

// TestCBRMomentCheckZSS: the same checks for a ZSS character, whose routes'
// conditions are the blocks around them.
func TestCBRMomentCheckZSS(t *testing.T) {
	w := cbrCompileWorld(t, "", cbrTestZSS, true)
	c := w.root
	for state, want := range map[int32][]string{200: {"x"}, 210: {"x"}, 3100: {"super", "holddown"}, 1000: {"qcf_x"}, 400: {"y"}, 230: {"holdfwd"}, 3000: nil} {
		if got, known := w.set.table.PersonCommands(state); !reflect.DeepEqual(got, want) || !known {
			t.Errorf("state %d: %v %v, want %v", state, got, known, want)
		}
	}
	for _, tc := range []struct {
		name  string
		from  int32
		v     int32
		setup func()
		want  cbrEntry
		state int32
	}{
		{"AI jab with control", 0, 1, nil, cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 200},
		{"AI jab without control", 0, 1, func() { c.unsetSCF(SCF_ctrl) }, cbrEntry{kind: cbr.EntryCheat}, 200},
		{"AI-only state", 0, 2, nil, cbrEntry{kind: cbr.EntryCheat}, 3000},
		{"AI super below the power", 0, 3, func() { c.power = 999 }, cbrEntry{kind: cbr.EntryCheat}, 3100},
		{"AI super at the power", 0, 3, func() { c.power = 1000 }, cbrEntry{kind: cbr.EntryReachable, cmd: "super"}, 3100},
		{"AI special from control: Combo() says it combos", 0, 4, nil, cbrEntry{kind: cbr.EntryReachable, cmd: "qcf_x"}, 1000},
		{"AI special cancelling a jab that hit", 200, 4, func() { c.mctime = 2 }, cbrEntry{kind: cbr.EntryReachable, cmd: "qcf_x"}, 1000},
		{"AI special out of a jab that missed", 200, 4, nil, cbrEntry{kind: cbr.EntryCheat}, 1000},
		{"AI into a state a let variable's route enters", 0, 6, nil, cbrEntry{kind: cbr.EntryReachable, cmd: "y"}, 400},
		{"... without control", 0, 6, func() { c.unsetSCF(SCF_ctrl) }, cbrEntry{kind: cbr.EntryCheat}, 400},
		{"AI into a state only a route after a busy function enters", 0, 7, nil, cbrEntry{kind: cbr.EntryUnknown}, 230},
		{"person's special (function and local variable, live)", 0, 0, func() { w.person(); w.press("qcf_x") }, cbrEntry{kind: cbr.EntryInput, cmd: "qcf_x"}, 1000},
		{"AI cancel after contact", 200, 5, func() { c.mctime = 1 }, cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 210},
		{"AI cancel without contact", 200, 5, nil, cbrEntry{kind: cbr.EntryCheat}, 210},
		{"person's jab (else branch)", 0, 0, func() { w.person(); w.press("x") }, cbrEntry{kind: cbr.EntryInput, cmd: "x"}, 200},
		{"AI out of a state whose own cancel has not run yet", 400, 8, nil, cbrEntry{kind: cbr.EntryReachable, cmd: "x"}, 210},
		{"AI out of a state whose own cancel ran once already (persistent 0)", 400, 8, func() {
			// The state's first frame ran its persistent block, nothing pressed
			c.ss.time = 0
			c.minus = 0
			c.ss.sb.run(c)
			c.minus = 3
			if c.ss.no != 400 {
				t.Fatalf("setup: the state's first frame left it: %d", c.ss.no)
			}
		}, cbrEntry{kind: cbr.EntryCheat}, 210},
	} {
		sys.aiLevel[0], c.controller, c.power = 8, -1, 0
		w.enter(tc.from)
		if tc.v > 0 {
			c.cnsvar[1] = tc.v
		}
		if tc.setup != nil {
			tc.setup()
		}
		w.frame()
		got, state := w.run(-1)
		if got != tc.want || state != tc.state {
			t.Errorf("%s: %v into %d, want %v into %d", tc.name, got, state, tc.want, tc.state)
		}
	}
}

// cbrRealCharCompiler sets up the engine to compile real characters from
// base (a folder holding chars/ and data/ as the game's) with the common
// files of the default configuration, and returns a function compiling one
// into player 1's slot as a CPU-controlled root standing in state 0.
func cbrRealCharCompiler(t testing.TB, base string) func(def string) (*Char, map[int32]StateBytecode, error) {
	t.Helper()
	t.Chdir(base)
	cbrTestRuntime(t)
	savedChars, savedCgi, savedCommon := sys.chars, sys.cgi[0], sys.cfg.Common
	savedPool, savedSets, savedIgnore := sys.stringPool[0], cbrRouteSets, sys.ignoreMostErrors
	savedList, savedLevel, savedRound, savedIntro := sys.charList, sys.aiLevel, sys.fightScreen.round, sys.intro
	savedFfx := sys.ffx
	t.Cleanup(func() {
		sys.chars, sys.cgi[0], sys.cfg.Common = savedChars, savedCgi, savedCommon
		sys.stringPool[0], cbrRouteSets, sys.ignoreMostErrors = savedPool, savedSets, savedIgnore
		sys.charList, sys.aiLevel, sys.fightScreen.round, sys.intro = savedList, savedLevel, savedRound, savedIntro
		sys.ffx = savedFfx
		cbrCheck = cbrCheckState{}
	})
	sys.cfg.Common.Cmd = map[string][]string{"cmd": {"data/common.cmd"}}
	sys.cfg.Common.States = map[string][]string{"states": {"data/functions.zss", "data/action.zss",
		"data/demo.zss", "data/dizzy.zss", "data/guardbreak.zss", "data/score.zss", "data/system.zss",
		"data/tag.zss", "data/training.zss"}}
	sys.fightScreen.round, sys.intro = &FightScreenRound{}, 0
	// The common effects' prefix, which the common states use
	sys.ffx = map[string]*FightFx{"go": {}}
	// The common constants, as Char.load reads them for each character
	constants := map[string]float32{}
	text, err := LoadText("data/common.const")
	if err != nil {
		t.Fatal(err)
	}
	lines, li := SplitAndTrim(text, "\n"), 0
	is, _, _ := ReadIniSection(lines, &li)
	for key, value := range is {
		constants[key] = float32(Atof(value))
	}
	return func(def string) (*Char, map[int32]StateBytecode, error) {
		root := &Char{id: 56, playerNo: 0, controller: -1, localcoord: 320, localscl: 1,
			life: 1000, lifeMax: 1000, powerMax: 3000, cpucmd: -1}
		root.keyctrl = [4]bool{true, true, true, true}
		root.cnsvar, root.cnsfvar = map[int32]int32{}, map[int32]float32{}
		root.cnssysvar, root.cnssysfvar = map[int32]int32{}, map[int32]float32{}
		root.mapArray = map[string]float32{}
		sys.chars = [len(sys.chars)][]*Char{{root}}
		sys.cgi[0] = newCharGlobalInfo()
		sys.ignoreMostErrors = true
		states, err := newCharCompiler().Compile(0, def, constants)
		if err == nil {
			sys.cgi[0].states = states
		}
		return root, states, err
	}
}

// TestCBRRoutesOfRealCharacters compiles the Kung Fu Man copies of the
// in-game runs (one per kind of character AI, CNS and ZSS), with the
// common files the default configuration loads, and checks their route
// tables, and, with the real code of [Statedef -1], the label of the
// AILevel AI's hit-confirm cancel. It needs those files: set
// CBR_ROUTE_CHARS to a folder holding chars/ and data/ as the game's.
func TestCBRRoutesOfRealCharacters(t *testing.T) {
	base := os.Getenv("CBR_ROUTE_CHARS")
	if base == "" {
		t.Skip("CBR_ROUTE_CHARS is not set")
	}
	defs, _ := filepath.Glob(filepath.Join(base, "chars", "*", "kfm*.def"))
	if len(defs) == 0 {
		t.Fatalf("no characters in %s", base)
	}
	compile := cbrRealCharCompiler(t, base)
	for _, def := range defs {
		name := filepath.Base(filepath.Dir(def))
		root, states, err := compile(def)
		if err != nil {
			t.Errorf("%s: compile: %v", name, err)
			continue
		}
		set := cbrRoutesFor(0)
		if set == nil {
			t.Errorf("%s: no route table", name)
			continue
		}
		person, unchecked, fixed := 0, 0, 0
		for i := range set.routes {
			r := &set.routes[i]
			if r.person {
				person++
				if r.fixed {
					fixed++
				}
				for gi := range r.groups {
					if r.groups[gi].person() && !r.groups[gi].checkable {
						unchecked++
					}
				}
			}
		}
		t.Logf("%s: %d routes, %d person routes (%d with a fixed target), %d person groups not checkable, %d states with person routes, unreadable %v",
			name, len(set.routes), person, fixed, unchecked, len(set.table.Person), set.table.Unreadable)
		for _, tc := range []struct {
			state int32
			cmd   string
		}{{200, "x"}, {1000, "QCF_x"}, {1100, "upper_x"}, {3000, "TripleKFPalm"}} {
			cmds, known := set.table.PersonCommands(tc.state)
			if !known || !cbrHas(cmds, tc.cmd) {
				t.Errorf("%s: state %d: person commands %v (known %v), want %s among them", name, tc.state, cmds, known, tc.cmd)
			}
		}
		// The palm's person route is checkable: for the ZSS characters
		// through the replay of Combo() in command.zss. Not for the z-axis
		// copy, whose specials read a local variable worked out from the
		// analog stick with an assignment (let rx = map(right_x) := ...).
		for _, id := range set.into[1000] {
			r := &set.routes[id]
			for gi := range r.groups {
				if g := &r.groups[gi]; cbrHas(g.cmds, "QCF_x") && !g.checkable && name != "kfm_zaxis" {
					t.Errorf("%s: the palm's route %d is not checkable", name, id)
				}
			}
		}
		w := &cbrRouteWorld{t: t, root: root, states: states, set: set, slot: &sysCBR.slots[0]}
		sys.charList = CharList{idMap: map[int32]*Char{root.id: root}, runOrder: []*Char{root}}
		sys.aiLevel[0] = 8
		if name == "kfm" || name == "kfm_zss" {
			// The moment check on the stock command files' own routes: the
			// CNS one's var(1) combo condition and the ZSS one's Combo()
			// and !aiLevelF gate are worked out again at that moment.
			for _, tc := range []struct {
				from   int32
				target int32
				setup  func()
				want   cbrEntry
			}{
				{0, 1000, nil, cbrEntry{kind: cbr.EntryReachable, cmd: "QCF_x"}},
				{0, 1000, func() { root.unsetSCF(SCF_ctrl) }, cbrEntry{kind: cbr.EntryCheat}},
				{200, 1000, func() { root.mctype, root.mctime = MC_Hit, 2 }, cbrEntry{kind: cbr.EntryReachable, cmd: "QCF_x"}},
				{200, 1000, nil, cbrEntry{kind: cbr.EntryCheat}},
				{0, 3000, func() { root.power = 500 }, cbrEntry{kind: cbr.EntryCheat}},
				{0, 3000, func() { root.power = 1000 }, cbrEntry{kind: cbr.EntryReachable, cmd: "TripleKFPalm"}},
			} {
				w.enter(tc.from)
				root.power = 0
				if tc.setup != nil {
					tc.setup()
				}
				vars := fmt.Sprint(root.cnsvar)
				if got := set.momentCheck(root, tc.target); got != tc.want {
					t.Errorf("%s: from %d into %d: %v, want %v", name, tc.from, tc.target, got, tc.want)
				}
				if fmt.Sprint(root.cnsvar) != vars {
					t.Errorf("%s: the check changed the variables: %v, was %v", name, root.cnsvar, vars)
				}
			}
		}
		if name != "k1_ailevel" {
			continue
		}
		// Its AI cancels a jab that hit into the palm (AI: hit-confirm),
		// with a ChangeState whose target is a variable: a person could,
		// since the palm's route cancels moves in 200-299 on contact.
		w.slot.routes, w.slot.watched = set, true
		cbrWatchAny()
		var got cbrEntry
		for seed := int32(1); seed < 200 && got.kind == cbr.EntryNone; seed++ {
			w.enter(200)
			c := root
			c.mctype, c.mctime = MC_Hit, 2
			sys.randseed = seed
			w.frame()
			c.minus = -1
			sb := states[-1]
			sb.run(c)
			c.minus = 3
			got = w.label()
			if got.kind != cbr.EntryNone && c.ss.no != 1000 {
				t.Fatalf("%s: the AI went to %d (%v)", name, c.ss.no, got)
			}
		}
		if got.kind != cbr.EntryReachable || got.cmd != "QCF_x" {
			t.Errorf("%s: hit-confirm into the palm: %v, want reachable QCF_x", name, got)
		}
		w.slot.watched = false
		cbrWatchAny()
	}
}

// cbrDetStates are the states of the determinism test's character: each
// move returns to standing with control after two frames, and standing
// draws a random number into var(20) every frame, so that a Random the
// check drew and did not give back would show on the next frame. State 0
// also reads its nearest enemy, of which there is none (a console warning),
// as one of the person routes does.
const cbrDetStates = `
[Statedef 0]
type = S
movetype = I
ctrl = 1

[State 0, draw]
type = VarSet
trigger1 = 1
var(20) = random

[State 0, no second enemy]
type = VarSet
trigger1 = enemynear(1),life > 0
var(21) = 1
` + `
[Statedef 200]
type = S
movetype = A
ctrl = 0

[State 200, own cancel]
type = ChangeState
value = 400
trigger1 = command = "y" && movecontact

[State 200, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 210]
type = S
movetype = A
ctrl = 0

[State 210, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 220]
type = S
movetype = A
ctrl = 0

[State 220, follow-up]
type = ChangeState
trigger1 = time = 0
value = 221

[Statedef 221]
type = S
movetype = A
ctrl = 0

[State 221, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 400]
type = S
movetype = A
ctrl = 0

[State 400, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 1000]
type = S
movetype = A
ctrl = 0

[State 1000, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 3000]
type = S
movetype = A
ctrl = 0

[State 3000, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2

[Statedef 3100]
type = S
movetype = A
ctrl = 0

[State 3100, back]
type = ChangeState
value = 0
ctrl = 1
trigger1 = time >= 2
`

// cbrDetNegOne adds person routes whose check draws Random and reads a
// missing nearest enemy to the CNS test character's [Statedef -1], and a
// combo condition worked out by VarSet, as Kung Fu Man's command file does.
const cbrDetNegOne = `
[State -1, combo condition reset]
type = VarSet
trigger1 = 1
var(30) = 0

[State -1, combo condition]
type = VarSet
trigger1 = ctrl || (stateno = 200 && movecontact)
var(30) = 1
` + cbrTestNegOne + `
[State -1, lucky]
type = ChangeState
value = 3000
trigger1 = command = "holdfwd" && random < 500 && var(30)

[State -1, nearest]
type = ChangeState
value = 3000
trigger1 = command = "holddown" && enemynear(1),life > 0

[State -1, close]
type = ChangeState
value = 3000
trigger1 = command = "y" && p2bodydist x < 100 && ctrl
`

// cbrFingerprint is what the determinism test compares after each frame:
// the character's state, flags, variables, power and command buffer, the
// random seed, the console, the nearest-enemy lists and the engine's
// working state.
func cbrFingerprint(c *Char) string {
	var b []byte
	b = fmt.Appendf(b, "state %d prev %d time %d ctrl %v sys %#x asf %#x power %d minus %d sctrl %d\n",
		c.ss.no, c.ss.prevno, c.ss.time, c.ctrl(), c.systemFlag, c.assertFlag, c.power, c.minus, c.currentSctrlIndex)
	b = fmt.Appendf(b, "vars %v fvars %v sysvars %v maps %v\n", c.cnsvar, c.cnsfvar, c.cnssysvar, c.mapArray)
	b = fmt.Appendf(b, "seed %d stack %d varstack %d console %d %q\n",
		sys.randseed, len(sys.bcStack), len(sys.bcVarStack), len(sys.consoleText), sys.consoleText)
	b = fmt.Appendf(b, "near %v p2 %v p2bak %d changed %v\n",
		c.enemyNearList, c.p2EnemyList, c.p2EnemyBackup, sys.charList.enemyNearChanged)
	for _, o := range sys.charList.runOrder {
		if o != c {
			b = fmt.Appendf(b, "other %d near %v p2 %v\n", o.id, o.enemyNearList, o.p2EnemyList)
		}
	}
	cl := &c.cmd[0]
	for i := range cl.Commands {
		for j := range cl.Commands[i] {
			cm := &cl.Commands[i][j]
			b = fmt.Appendf(b, "%s %d/%d/%d ", cm.name, cm.curbuftime, cm.curtime, cm.cursteptime)
		}
	}
	buf := *cl.Buffer
	buf.InputReader = nil // a pointer, different in each run
	b = fmt.Appendf(b, "\nbuffer %+v\n", buf)
	return string(b)
}

// TestCBRCheckChangesNothing: the same frames played with the CBR layer
// watching (every state change labelled, the AI's checked at that moment)
// and without leave the character, the random seed, the console and the
// command buffer exactly the same after every frame, for a CNS and a ZSS
// character, while the checks do happen.
func TestCBRCheckChangesNothing(t *testing.T) {
	savedRows := sys.cfg.Debug.ConsoleRows
	t.Cleanup(func() { sys.cfg.Debug.ConsoleRows = savedRows })
	sys.cfg.Debug.ConsoleRows = 1000 // keep the warnings the code prints
	for _, tc := range []struct {
		name, negOne, st string
		zss              bool
	}{
		{"cns", cbrDetNegOne, cbrDetStates, false},
		// The ZSS character's AI goes by the value of var(1)
		{"zss", "", cbrTestZSS, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			play := func(watch bool) ([]string, map[cbr.EntryKind]int) {
				w := cbrCompileWorld(t, tc.negOne, tc.st, tc.zss)
				c := w.root
				c.mapArray = map[string]float32{}
				// An opponent, so that the nearest-enemy lists the triggers
				// build are there to compare.
				c.teamside = 0
				e := &Char{id: 60, playerNo: 1, teamside: 1, localcoord: 320, localscl: 1, life: 1000, facing: -1}
				e.pos[0] = 50
				sys.chars[1] = []*Char{e}
				sys.charList.idMap[e.id] = e
				sys.charList.runOrder = append(sys.charList.runOrder, e)
				t.Cleanup(func() { sys.chars[1] = nil })
				w.slot.watched = watch
				cbrWatchAny()
				sys.randseed = 12345
				sys.consoleText = nil
				names := make([]string, 0, len(c.cmd[0].Names))
				for n := range c.cmd[0].Names {
					names = append(names, n)
				}
				sort.Strings(names)
				r := rand.New(rand.NewSource(7))
				kinds := map[cbr.EntryKind]int{}
				var prints []string
				for f := 0; f < 600; f++ {
					// The script: AI variables, the AI level, commands in the
					// buffer, the engine AI's cheat, contact, power.
					for v := int32(1); v <= 10; v++ {
						delete(c.cnsvar, v)
					}
					if r.Intn(3) == 0 {
						if tc.zss {
							c.cnsvar[1] = int32(1 + r.Intn(7))
						} else {
							c.cnsvar[int32(1+r.Intn(10))] = 1
						}
					}
					if f/50%2 == 0 {
						sys.aiLevel[0], c.controller = 8, -1
					} else {
						sys.aiLevel[0], c.controller = 0, 0
					}
					var pressed []string
					for _, n := range names {
						if r.Intn(6) == 0 {
							pressed = append(pressed, n)
						}
					}
					w.press(pressed...)
					c.cpucmd = -1
					if r.Intn(4) == 0 {
						c.cpucmd = int32(r.Intn(len(names)))
					}
					c.mctype, c.mctime = MC_Hit, int32(r.Intn(2)*2)
					c.power = int32(r.Intn(1500))
					// The opponent moves, so the nearest-enemy lists are built
					// anew on their first use in the frame.
					e.setPosX(float32(30+r.Intn(60)), false)
					if r.Intn(20) == 0 {
						c.hitPauseTime = 3
					}
					// A frame of the character's code: [Statedef -1], then
					// its state's.
					w.frame()
					c.minus = -1
					sb := w.states[-1]
					sb.run(c)
					c.stateChange2()
					c.minus = 0
					c.ss.sb.run(c)
					c.minus = 3
					if c.hitPauseTime > 0 {
						c.hitPauseTime--
					} else {
						c.ss.time++
					}
					if e := w.label(); e.kind != cbr.EntryNone {
						kinds[e.kind]++
					}
					prints = append(prints, cbrFingerprint(c))
				}
				return prints, kinds
			}
			off, offKinds := play(false)
			on, onKinds := play(true)
			if !strings.Contains(off[len(off)-1], "nearest enemy") {
				t.Fatalf("setup: the code's console warnings are missing: %s", off[len(off)-1])
			}
			if len(offKinds) != 0 {
				t.Fatalf("labels without watching: %v", offKinds)
			}
			for f := range off {
				if off[f] != on[f] {
					t.Fatalf("frame %d differs with the check on:\noff: %s\non:  %s", f, off[f], on[f])
				}
			}
			t.Logf("%d frames identical; labels with the check on: %v", len(on), onKinds)
			for _, k := range []cbr.EntryKind{cbr.EntryInput, cbr.EntryAuto, cbr.EntryReachable, cbr.EntryCheat, cbr.EntryUnknown} {
				if onKinds[k] == 0 {
					t.Errorf("no %v label: the script must exercise every label", k)
				}
			}
		})
	}
}

// BenchmarkCBRStateChangeHook measures what watching costs: a frame of the
// character's [Statedef -1] in which no state changes ("quiet"), and one in
// which the AI changes the state and the change is labelled and checked
// ("AI decision"), with the CBR layer watching the character and without.
// With CBR_ROUTE_CHARS set (see TestCBRRoutesOfRealCharacters) it also
// measures the stock CNS and ZSS Kung Fu Man's [Statedef -1], and their
// moment check into the Kung Fu Palm.
func BenchmarkCBRStateChangeHook(b *testing.B) {
	for _, watch := range []bool{false, true} {
		suffix := "/off"
		if watch {
			suffix = "/watching"
		}
		b.Run("quiet"+suffix, func(b *testing.B) {
			w := cbrCompileWorld(b, cbrTestNegOne, cbrTestStates, false)
			w.slot.watched = watch
			cbrWatchAny()
			c := w.root
			w.enter(0)
			sb := w.states[-1]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.minus = -1
				sb.run(c)
			}
			c.minus = 3
			if c.ss.no != 0 {
				b.Fatalf("the state changed: %d", c.ss.no)
			}
		})
		b.Run("AI decision"+suffix, func(b *testing.B) {
			w := cbrCompileWorld(b, cbrTestNegOne, cbrTestStates, false)
			w.slot.watched = watch
			cbrWatchAny()
			c := w.root
			sb := w.states[-1]
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				c.ss.sb, c.ss.no = w.states[0], 0
				c.setSCF(SCF_ctrl)
				c.cnsvar[1] = 1
				c.minus = -1
				sb.run(c)
			}
			c.minus = 3
			if c.ss.no != 200 || watch && w.slot.entry.kind != cbr.EntryReachable {
				b.Fatalf("state %d, label %v", c.ss.no, w.slot.entry)
			}
		})
	}
	base := os.Getenv("CBR_ROUTE_CHARS")
	if base == "" {
		return
	}
	compile := cbrRealCharCompiler(b, base)
	for _, def := range []string{"chars/kfm/kfm.def", "chars/kfm_zss/kfm_zss.def"} {
		name := filepath.Base(filepath.Dir(def))
		for _, watch := range []bool{false, true} {
			suffix := "/off"
			if watch {
				suffix = "/watching"
			}
			b.Run(name+" quiet"+suffix, func(b *testing.B) {
				root, states, err := compile(def)
				if err != nil {
					b.Fatal(err)
				}
				w := &cbrRouteWorld{t: b, root: root, states: states, set: cbrRoutesFor(0), slot: &sysCBR.slots[0]}
				sys.charList = CharList{idMap: map[int32]*Char{root.id: root}, runOrder: []*Char{root}}
				w.slot.routes, w.slot.watched = w.set, watch
				cbrWatchAny()
				w.enter(0)
				sb := states[-1]
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					root.minus = -1
					sb.run(root)
				}
				root.minus = 3
				if root.ss.no != 0 {
					b.Fatalf("the state changed: %d", root.ss.no)
				}
			})
		}
		b.Run(name+" moment check into the palm", func(b *testing.B) {
			root, states, err := compile(def)
			if err != nil {
				b.Fatal(err)
			}
			w := &cbrRouteWorld{t: b, root: root, states: states, set: cbrRoutesFor(0), slot: &sysCBR.slots[0]}
			sys.charList = CharList{idMap: map[int32]*Char{root.id: root}, runOrder: []*Char{root}}
			w.enter(0)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if e := w.set.momentCheck(root, 1000); e.kind != cbr.EntryReachable {
					b.Fatalf("%v", e)
				}
			}
		})
	}
}

// TestCBRMomentCheckWorksOutComboConditions: a person route that reads a
// variable worked out by a VarSet earlier in its state each frame (Kung Fu
// Man's "combo condition" var(1)) is checked with the variable worked out
// again at that moment, although the AI's own route runs first and the
// variable still holds last frame's value; the variable is left as it was.
func TestCBRMomentCheckWorksOutComboConditions(t *testing.T) {
	w := cbrCompileWorld(t, `
[State -1, AI first]
type = ChangeState
value = 1000
trigger1 = AILevel && var(4) = 1

[State -1, combo condition reset]
type = VarSet
trigger1 = 1
var(30) = 0

[State -1, combo condition]
type = VarSet
trigger1 = ctrl
trigger2 = stateno = 200 && movecontact
var(30) = 1

[State -1, palm]
type = ChangeState
value = 1000
triggerall = command = "qcf_x"
trigger1 = var(30)
`, cbrTestStates, false)
	c := w.root
	for _, tc := range []struct {
		name  string
		from  int32
		last  int32 // var(30) as last frame left it
		setup func()
		want  cbr.EntryKind
	}{
		{"from control, worked out last frame without", 0, 0, nil, cbr.EntryReachable},
		{"cancelling a jab that hit", 200, 0, func() { c.mctime = 2 }, cbr.EntryReachable},
		{"out of a jab that missed, last frame's value says yes", 200, 1, nil, cbr.EntryCheat},
	} {
		got, state := w.decide(tc.from, 4, func() {
			c.cnsvar[30] = tc.last
			if tc.setup != nil {
				tc.setup()
			}
		})
		if got.kind != tc.want || state != 1000 {
			t.Errorf("%s: %v into %d, want %v", tc.name, got, state, tc.want)
		}
		if c.cnsvar[30] != tc.last {
			t.Errorf("%s: the check left var(30) = %d, was %d", tc.name, c.cnsvar[30], tc.last)
		}
	}
}

// TestCBRCheckRestoresEngineState: a moment check that runs local variable
// assignments and a function again, draws Random and builds the
// nearest-enemy lists gives back exactly what it found: the local variable
// frame and stack, the random seed, the running state and character, the
// character's minus and controller index, the console and every
// character's nearest-enemy lists.
func TestCBRCheckRestoresEngineState(t *testing.T) {
	w := cbrCompileWorld(t, "", cbrTestZSS, true)
	c := w.root
	c.teamside = 0
	e := &Char{id: 60, playerNo: 1, teamside: 1, localcoord: 320, localscl: 1, life: 1000, facing: -1}
	e.pos[0] = 50
	sys.chars[1] = []*Char{e}
	t.Cleanup(func() { sys.chars[1] = nil })
	sys.charList.idMap[e.id] = e
	sys.charList.runOrder = append(sys.charList.runOrder, e)
	sys.charList.enemyNearChanged = true
	savedRows := sys.cfg.Debug.ConsoleRows
	t.Cleanup(func() { sys.cfg.Debug.ConsoleRows = savedRows })
	sys.cfg.Debug.ConsoleRows = 100
	sb := StateBytecode{playerNo: 0}
	vars := make([]BytecodeValue, 3, 3)
	vars[1] = BytecodeInt(7)
	stack := make(BytecodeStack, 2, 4)
	sys.bcVar, sys.bcVarStack, sys.bcStack = vars, stack, sys.bcStack[:0]
	sys.randseed, sys.workingState, sys.workingChar = 99, &sb, e
	sys.consoleText = []string{"kept"}
	c.minus, c.currentSctrlIndex = -1, 7
	near, p2 := make([]int32, 0, 2), make([]int32, 0, 2)
	c.enemyNearList, c.p2EnemyList = near, p2
	w.enter(0)
	c.minus = -1
	type check struct {
		target int32
		want   cbr.EntryKind
	}
	for _, ck := range []check{{1000, cbr.EntryReachable}, {3100, cbr.EntryReachable}, {230, cbr.EntryUnknown}} {
		if got := w.set.momentCheck(c, ck.target); got.kind != ck.want {
			t.Errorf("into %d: %v, want %v", ck.target, got, ck.want)
		}
		same := func(a, b []int32) bool {
			return len(a) == len(b) && cap(a) == cap(b) && unsafe.SliceData(a) == unsafe.SliceData(b)
		}
		switch {
		case len(sys.bcVar) != 3 || unsafe.SliceData(sys.bcVar) != unsafe.SliceData(vars) || sys.bcVar[1].ToI() != 7:
			t.Errorf("into %d: local variable frame %v", ck.target, sys.bcVar)
		case len(sys.bcVarStack) != 2 || unsafe.SliceData(sys.bcVarStack) != unsafe.SliceData(stack):
			t.Errorf("into %d: local variable stack len %d", ck.target, len(sys.bcVarStack))
		case len(sys.bcStack) != 0:
			t.Errorf("into %d: bytecode stack %v", ck.target, sys.bcStack)
		case sys.randseed != 99 || sys.workingState != &sb || sys.workingChar != e:
			t.Errorf("into %d: seed %d, working state %p, working char %p", ck.target, sys.randseed, sys.workingState, sys.workingChar)
		case c.minus != -1 || c.currentSctrlIndex != 7:
			t.Errorf("into %d: minus %d, controller index %d", ck.target, c.minus, c.currentSctrlIndex)
		case len(sys.consoleText) != 1 || sys.consoleText[0] != "kept":
			t.Errorf("into %d: console %q", ck.target, sys.consoleText)
		case !same(c.enemyNearList, near) || !same(c.p2EnemyList, p2) || len(e.p2EnemyList) != 0 || !sys.charList.enemyNearChanged:
			t.Errorf("into %d: nearest-enemy lists %v %v, opponent's %v, changed %v", ck.target, c.enemyNearList, c.p2EnemyList, e.p2EnemyList, sys.charList.enemyNearChanged)
		case cbrCheck.on:
			t.Errorf("into %d: the check is still on", ck.target)
		}
	}
}
