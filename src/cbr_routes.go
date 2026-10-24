package main

// Route table and moment check for the CBR AI (package cbr): which of a
// character's state changes a person can make, and, when the character's
// own AI changes its state, whether a person could have made that change at
// that very moment or only a ChangeState can reach it (cheating).
//
// While a character compiles, the compiler notes for each condition what it
// checks (see cbrExprScan): the button commands, and whether each must be
// active (command = "x") or not (command != "x", or under a !); AILevel and
// AILevelF, and from the text right after them whether they need the level
// to be 0 or above it; ZSS local variables; and anything that makes
// evaluating it unsafe or not repeatable: an assignment (:=), or InputTime,
// which reads the raw input. At the end of the compile, cbrBuildRoutes
// walks every state, CNS and ZSS alike (the command file's [Statedef -1],
// the state files, the common states and ZSS functions), and lists each
// ChangeState and SelfState controller as a route: the state it is in, its
// target when that is a fixed number, its trigger groups (in CNS each
// triggerN together with the triggerall lines; in ZSS the conditions of the
// blocks around it, an else branch counting as its condition being false),
// whether it can run in hitpause, its persistent counters, and the
// assignments that run before it in its state, which it may read: ZSS local
// variables (let, also from a function's return values), and VarSet and
// VarAdd of the character's own variables (Kung Fu Man's "combo condition",
// var(1) in its CNS command file, works out each frame whether a special may
// cancel). A person route is a route with a fixed target, not redirected to
// another character, with a trigger group that needs a command to be active
// and does not need the AI level above 0.
//
// At run time, only offline and only for root characters the CBR layer
// watches, ChangeState and SelfState (bytecode.go) label a state change of
// such a character just before they make it, a helper's redirected change
// included (cbrBeginChange in cbr_hook.go):
//
//   - Input: a person route fired, by a command in the real input buffer.
//     To tell, the group with that command is evaluated again with AILevel
//     reading 0 and only the commands really in the buffer active (no
//     cheat); it must still hold.
//   - Auto: a route with no command and no AI level inside a move (a
//     follow-up, the return to standing); another character's code (a
//     throw's custom state, TargetState); and the changes the engine makes
//     itself (hit and guard states, landing), except its built-in walk,
//     jump, crouch and guard, which are Input with no command.
//   - Otherwise the character's AI decided: a route gated on the AI level; a
//     person route whose command only the engine AI's cheat made true; a
//     route with no command and no AI level that fires in [Statedef -1], -2
//     or -3 while the character has control or is attacking (a cancel); or
//     a helper's redirected change while the character has control. Then the
//     moment check evaluates, right then, the routes into the same state that
//     need a command: only those whose state runs now (a negative state, or
//     the current one) and whose persistent counters let them, with AILevel
//     reading 0, that group's own commands active and every other command
//     inactive, and the real conditions otherwise (control, state type, the
//     current state and its time, power, move contact), after running again
//     the assignments before the route that it may read. In hitpause, a
//     route that does not run during it counts as it will run when hitpause
//     ends, when its commands stay in the input buffer through hitpause, as
//     they do by default. Reachable when one holds, with its command; Cheat
//     when none does; Unknown when a person route that might could not be
//     evaluated.
//
// The check changes nothing in the game: it never evaluates a condition with
// an assignment, nor runs a function that does more than set local
// variables; it saves and restores the random seed, the running state and
// character, the bytecode stack, the local variable frame and its stack, the
// console, the nearest-enemy lists (which some triggers build on first use),
// the character's variables (when VarSets run again) and its minus and
// controller index; commands and AILevel are answered by cbrCheck instead of
// the input buffer and the AI level. A condition that panics counts as not
// checkable. It runs only when a watched character's state changes, so a
// frame on which no state changes costs nothing beyond a flag test in each
// command and AILevel trigger.
//
// Limits:
//   - A condition that compares a command in another way than = or != (say
//     command = "x" = 0) is read as needing the command active.
//   - The AI level's need is read from the text right after AILevel; the
//     moment check does not rely on it, since it evaluates with AILevel
//     reading 0, but the person routes listed in the table do.
//   - A route whose target is not a fixed number is not evaluated: when one
//     could run, a change no other route reaches is Unknown.
//   - A ZSS route inside a loop or a function is not evaluated, nor one that
//     reads a local variable set in a loop or by an unsafe assignment.
//   - Of the code that runs before a route in its state, only the
//     assignments above run again: a variable another controller sets (a
//     VarSet redirected to the character, VarRandom, a map) keeps the value
//     it has at that moment.
//   - An AI that is latched in a variable and changes state from a positive
//     state's own code with no command (not from control in -1, -2 or -3) is
//     not told apart from the move's own follow-up: it is Auto.
//   - A helper's change that the character's own code or the engine makes
//     look like a person's (an unpaused AI pressing the command through the
//     engine AI's button jam) is Input, as a person's would be.

import (
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

// cbrAINeed is what an AILevel or AILevelF in a condition asks of the AI
// level, as far as the text right after it tells.
type cbrAINeed int8

const (
	cbrAIUnclear cbrAINeed = iota // compared in some other way
	cbrAIZero                     // AILevel = 0, !AILevel: holds for a person
	cbrAIAbove                    // AILevel, AILevel > 0: holds for the AI only
)

func (n cbrAINeed) negated() cbrAINeed {
	switch n {
	case cbrAIZero:
		return cbrAIAbove
	case cbrAIAbove:
		return cbrAIZero
	}
	return n
}

// cbrCmdRef is a command a condition checks; neg is set when the condition
// holds while the command is not active.
type cbrCmdRef struct {
	name string
	neg  bool
}

// cbrExprScan is what the compiler noted while it compiled one condition.
type cbrExprScan struct {
	cmds      []cbrCmdRef
	ai        []cbrAINeed
	assigns   bool // := on a variable or a map: evaluating it changes them
	localVars bool // a ZSS local variable, which only the running state holds
	rawInput  bool // InputTime, which reads the raw input buffer
	random    bool // Random or RandomRange
}

// cbrDraft is one trigger group of a CNS ChangeState or SelfState: its
// triggerall lines and the lines of one triggerN, with what each checks.
type cbrDraft struct {
	exprs []BytecodeExp
	scans []cbrExprScan
}

// cbrRouteScan is what the compiler keeps for the route table while it
// compiles one character.
type cbrRouteScan struct {
	cur   cbrExprScan             // the condition being compiled
	exprs map[*OpCode]cbrExprScan // block conditions and let values, by their code
	cns   map[*byte][]cbrDraft    // CNS ChangeState and SelfState, by their code
	safe  map[*byte]bool          // VarSet and VarAdd whose parameters are safe
}

// cbrBegin starts noting a condition, returning what was being noted.
func (c *CharCompiler) cbrBegin() cbrExprScan {
	prev := c.cbr.cur
	c.cbr.cur = cbrExprScan{}
	return prev
}

// cbrEnd returns what was noted since cbrBegin and goes back to prev.
func (c *CharCompiler) cbrEnd(prev cbrExprScan) cbrExprScan {
	s := c.cbr.cur
	c.cbr.cur = prev
	return s
}

// cbrMark is a point in the current condition's notes, for cbrNegate.
type cbrMark struct{ cmds, ai int }

func (c *CharCompiler) cbrMark() cbrMark {
	return cbrMark{len(c.cbr.cur.cmds), len(c.cbr.cur.ai)}
}

// cbrNegate turns around what the commands and AI levels noted since m ask:
// the expression they are in was negated with !.
func (c *CharCompiler) cbrNegate(m cbrMark) {
	s := &c.cbr.cur
	for i := m.cmds; i >= 0 && i < len(s.cmds); i++ {
		s.cmds[i].neg = !s.cmds[i].neg
	}
	for i := m.ai; i >= 0 && i < len(s.ai); i++ {
		s.ai[i] = s.ai[i].negated()
	}
}

func (c *CharCompiler) cbrNoteCommand(name string, neg bool) {
	c.cbr.cur.cmds = append(c.cbr.cur.cmds, cbrCmdRef{name, neg})
}

// cbrNoteAILevel notes an AILevel or AILevelF, with rest the text after it.
func (c *CharCompiler) cbrNoteAILevel(rest string) {
	c.cbr.cur.ai = append(c.cbr.cur.ai, cbrAINeedOf(rest))
}

// cbrAINeedOf reads what the AI level must be from the text right after
// AILevel: = 0, <= 0 or < 1 need it at 0; nothing (a plain truth test),
// != 0, > 0, >= 1 or = a positive number need it above 0.
func cbrAINeedOf(rest string) cbrAINeed {
	s := strings.TrimLeft(rest, " \t")
	op := ""
	for _, o := range []string{"!=", ">=", "<=", "=", ">", "<"} {
		if strings.HasPrefix(s, o) {
			op, s = o, strings.TrimLeft(s[len(o):], " \t")
			break
		}
	}
	if op == "" {
		for _, end := range []string{"&&", "||", "^^", ")", ",", "]"} {
			if strings.HasPrefix(s, end) {
				return cbrAIAbove
			}
		}
		if s == "" {
			return cbrAIAbove
		}
		return cbrAIUnclear
	}
	if op == "=" && len(s) > 0 && (s[0] == '[' || s[0] == '(') {
		// A range: above 0 when its low end is
		lo, ok := cbrLeadingNumber(strings.TrimLeft(s[1:], " \t"))
		if ok && (lo >= 1 || lo >= 0 && s[0] == '(') {
			return cbrAIAbove
		}
		return cbrAIUnclear
	}
	n, ok := cbrLeadingNumber(s)
	if !ok {
		return cbrAIUnclear
	}
	switch op {
	case "=":
		if n == 0 {
			return cbrAIZero
		}
		if n > 0 {
			return cbrAIAbove
		}
	case "!=":
		if n == 0 {
			return cbrAIAbove
		}
	case ">":
		if n >= 0 {
			return cbrAIAbove
		}
	case ">=":
		if n > 0 {
			return cbrAIAbove
		}
	case "<":
		if n > 0 && n <= 1 {
			return cbrAIZero
		}
	case "<=":
		if n >= 0 && n < 1 {
			return cbrAIZero
		}
	}
	return cbrAIUnclear
}

// cbrLeadingNumber reads the number at the start of s, if there is one.
func cbrLeadingNumber(s string) (float64, bool) {
	end := 0
	for end < len(s) && (s[end] >= '0' && s[end] <= '9' || s[end] == '.' ||
		end == 0 && s[end] == '-') {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.ParseFloat(s[:end], 64)
	return n, err == nil
}

// cbrNoteBlock keeps what a compiled expression checks, by its code: a ZSS
// block's condition or let value, a function call's arguments, a CNS
// controller's whole trigger.
func (c *CharCompiler) cbrNoteBlock(exp BytecodeExp, s cbrExprScan) {
	if len(exp) == 0 {
		return
	}
	if c.cbr.exprs == nil {
		c.cbr.exprs = map[*OpCode]cbrExprScan{}
	}
	c.cbr.exprs[unsafe.SliceData(exp)] = s
}

// cbrNoteCNS keeps the trigger groups of a CNS ChangeState or SelfState, as
// the trigger code is assembled from them (stateCompileCNS): the groups in
// order until a missing trigger number, leaving out a group with a line that
// is always false and ending with one whose lines are all always true.
func (c *CharCompiler) cbrNoteCNS(sctrl StateController, all []BytecodeExp,
	allScans []cbrExprScan, trig [][]BytecodeExp, trigScans [][]cbrExprScan,
	trexist []int8) {
	// The controller's whole trigger, as the block around it holds it: what
	// matters of it is whether evaluating it is safe.
	if c.block != nil {
		var whole cbrExprScan
		for _, ss := range append([][]cbrExprScan{allScans}, trigScans...) {
			for _, s := range ss {
				whole.assigns = whole.assigns || s.assigns
				whole.rawInput = whole.rawInput || s.rawInput
				whole.localVars = whole.localVars || s.localVars
				whole.random = whole.random || s.random
			}
		}
		c.cbrNoteBlock(c.block.trigger, whole)
	}
	key := cbrCtrlKey(sctrl)
	if key == nil {
		return
	}
	var groups []cbrDraft
	for i := range trig {
		if i >= len(trexist) || trexist[i] == 0 {
			break
		}
		if trexist[i] < 0 || i >= len(trigScans) || len(trigScans[i]) != len(trig[i]) {
			continue
		}
		d := cbrDraft{
			exprs: append(append([]BytecodeExp(nil), all...), trig[i]...),
			scans: append(append([]cbrExprScan(nil), allScans...), trigScans[i]...),
		}
		groups = append(groups, d)
		if len(trig[i]) == 0 {
			break
		}
	}
	if c.cbr.cns == nil {
		c.cbr.cns = map[*byte][]cbrDraft{}
	}
	c.cbr.cns[key] = groups
}

// cbrNoteParams notes, for a VarSet or VarAdd, whether its parameters,
// compiled while s was noted, are safe to evaluate again.
func (c *CharCompiler) cbrNoteParams(sctrl StateController, s cbrExprScan) {
	x, ok := sctrl.(varSet)
	if !ok || len(x) == 0 || s.assigns || s.rawInput {
		return
	}
	if c.cbr.safe == nil {
		c.cbr.safe = map[*byte]bool{}
	}
	c.cbr.safe[unsafe.SliceData([]byte(x))] = true
}

// cbrCtrlKey identifies a ChangeState or SelfState controller by its
// compiled code, which every copy of the controller shares; nil for any
// other controller.
func cbrCtrlKey(sc StateController) *byte {
	switch x := sc.(type) {
	case changeState:
		if len(x) > 0 {
			return unsafe.SliceData([]byte(x))
		}
	case selfState:
		if len(x) > 0 {
			return unsafe.SliceData([]byte(x))
		}
	}
	return nil
}

// cbrInFunction is the owner state of a route inside a ZSS function.
const cbrInFunction = math.MinInt32

// cbrCond is one condition of a route: exp must evaluate to want.
type cbrCond struct {
	exp  BytecodeExp
	want bool
}

// cbrGroup is one trigger group of a route: all its conditions must hold.
type cbrGroup struct {
	conds   []cbrCond
	cmds    []string // commands that must be active
	ranks   []int    // how much each of cmds is the group's main command
	notCmds []string // commands that must not be active
	main    string   // the main button command of cmds
	ai      bool     // reads the AI level
	aiAbove bool     // needs the AI level above 0
	random  bool
	locals  bool // reads ZSS local variables
	// checkable: every condition may be evaluated, and gives the same
	// answer when evaluated again in the same moment (Random aside).
	checkable bool
}

// person reports whether the group is a person's way into the route's
// target: it needs a command and not the AI level above 0.
func (g *cbrGroup) person() bool {
	return len(g.cmds) > 0 && !g.aiAbove
}

// cbrRoute is one ChangeState or SelfState controller.
type cbrRoute struct {
	id       int32
	owner    int32 // the state it is in, or cbrInFunction
	self     bool  // SelfState
	redirect bool  // redirectid: it changes another character's state
	foreign  bool  // readplayerid: it enters another player's states
	fixed    bool  // the target is a fixed number
	target   int32
	groups   []cbrGroup
	anyCmd   bool    // some group needs a command
	anyAI    bool    // some group reads the AI level
	person   bool    // a person route (fixed or not)
	hitpause bool    // it can run during hitpause
	persist  []int32 // persistent counters on its way, in its state
	// pre are the assignments that run before the route in its state (see
	// cbrLocalStep), which the moment check runs again, in a frame of numVars
	// local variables of its own, before it evaluates the route's conditions;
	// preOK is cleared when a local variable's one could not be run again
	// safely, so that the routes after it that read local variables cannot be
	// checked.
	pre     []cbrLocalStep
	preOK   bool
	preVars bool // some of pre sets the character's variables
	numVars int32
}

// cbrLocalStep is an assignment that runs before a route in its state when
// the conditions of the blocks around it hold: a ZSS local variable's (let),
// or a VarSet or VarAdd of the character's own variables, as a CNS command
// file's "combo condition" (Kung Fu Man's var(1)) is worked out each frame
// before the specials that read it.
type cbrLocalStep struct {
	conds []cbrCond
	// varAssign or CallFunction with return values; or varSet (vars set)
	ctrl StateController
	vars bool
}

// cbrRouteSet is one character's route table.
type cbrRouteSet struct {
	pn     int
	states map[int32]StateBytecode // the compiled states it lists
	routes []cbrRoute
	byCtrl map[*byte]int32
	// into lists, by fixed target, the routes into it that need a command,
	// not redirected; loose the person routes whose target is not fixed.
	into  map[int32][]int32
	loose []int32
	table cbr.RouteTable
	// evalState is the working state while conditions are evaluated: the
	// command names they check are read from its player's string pool.
	evalState StateBytecode
}

// cbrRouteSets holds each player's route table, built when its character
// compiles.
var cbrRouteSets [MaxPlayerNo]*cbrRouteSet

func cbrClearRoutes(pn int) {
	if pn >= 0 && pn < len(cbrRouteSets) {
		cbrRouteSets[pn] = nil
	}
}

// cbrRoutesFor returns player pn's route table, or nil when it has none or
// it was built for other states than the ones the player has now.
func cbrRoutesFor(pn int) *cbrRouteSet {
	if pn < 0 || pn >= len(cbrRouteSets) || pn >= len(sys.cgi) {
		return nil
	}
	set := cbrRouteSets[pn]
	if set == nil || set.states == nil || sys.cgi[pn].states == nil ||
		reflect.ValueOf(set.states).UnsafePointer() != reflect.ValueOf(sys.cgi[pn].states).UnsafePointer() {
		return nil
	}
	return set
}

// cbrBuildRoutes builds player pn's route table from its compiled states
// and functions, with what the compiler noted in scan.
func cbrBuildRoutes(pn int, states map[int32]StateBytecode,
	funcs map[string]BytecodeFunction, cmdl *CommandList, scan *cbrRouteScan) {
	if pn < 0 || pn >= len(cbrRouteSets) {
		return
	}
	set := &cbrRouteSet{
		pn:        pn,
		states:    states,
		byCtrl:    map[*byte]int32{},
		into:      map[int32][]int32{},
		table:     cbr.RouteTable{Person: map[int32][]string{}},
		evalState: StateBytecode{playerNo: pn},
	}
	w := &cbrWalker{set: set, scan: scan, cmdl: cmdl, funcs: funcs, pure: map[string]bool{}}
	nos := make([]int, 0, len(states))
	for no := range states {
		nos = append(nos, int(no))
	}
	sort.Ints(nos)
	for _, no := range nos {
		sb := states[int32(no)]
		w.owner, w.numVars, w.locals, w.localsOK = int32(no), sb.numVars, nil, true
		w.block(&sb.block, cbrPath{hp: true})
	}
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// A function's calls already checked hitpause.
		w.owner, w.numVars, w.locals, w.localsOK = cbrInFunction, funcs[name].numVars, nil, false
		fn := StateBlock{ctrls: funcs[name].ctrls, ctrlsIgnorehitpause: true,
			ignorehitpause: -1, persistentIndex: -1}
		w.block(&fn, cbrPath{hp: true, loop: true})
	}
	for i := range set.routes {
		r := &set.routes[i]
		if r.redirect || r.foreign {
			continue
		}
		if r.fixed && r.anyCmd {
			set.into[r.target] = append(set.into[r.target], r.id)
		}
		if !r.person {
			continue
		}
		if !r.fixed {
			set.loose = append(set.loose, r.id)
			set.table.Unreadable = true
			continue
		}
		for gi := range r.groups {
			g := &r.groups[gi]
			if g.person() && g.main != "" && !cbrHas(set.table.Person[r.target], g.main) {
				set.table.Person[r.target] = append(set.table.Person[r.target], g.main)
			}
		}
	}
	cbrRouteSets[pn] = set
}

// cbrPath is what a walk down the blocks of a state has passed: the
// conditions of the blocks, whether every block lets its code run in
// hitpause, whether a loop or a function is around, and the persistent
// counters on the way.
type cbrPath struct {
	conds   []cbrCond
	hp      bool
	loop    bool
	persist []int32
}

type cbrWalker struct {
	set   *cbrRouteSet
	scan  *cbrRouteScan
	cmdl  *CommandList
	funcs map[string]BytecodeFunction
	pure  map[string]bool // functions known to be pure (see pureFunc)
	owner int32
	// numVars is the state's number of local variables; locals the local
	// variable assignments passed so far in it, and localsOK cleared once
	// one that cannot be run again safely was passed.
	numVars  int32
	locals   []cbrLocalStep
	localsOK bool
}

// scanOf returns what the compiler noted for a compiled expression.
func (w *cbrWalker) scanOf(exp BytecodeExp) (cbrExprScan, bool) {
	if len(exp) == 0 {
		return cbrExprScan{}, true
	}
	s, ok := w.scan.exprs[unsafe.SliceData(exp)]
	return s, ok
}

// safeExpr reports whether evaluating exp changes nothing and reads nothing
// a check cannot reproduce.
func (w *cbrWalker) safeExpr(exp BytecodeExp) bool {
	s, ok := w.scanOf(exp)
	return ok && !s.assigns && !s.rawInput
}

// pureCtrls reports whether running ctrls only sets local variables:
// assignments to locals, ifs and switches (no loops) with safe conditions,
// and calls of pure functions.
func (w *cbrWalker) pureCtrls(ctrls []StateController, depth int) bool {
	for _, sc := range ctrls {
		switch x := sc.(type) {
		case varAssign:
			if !w.safeExpr(x.be) {
				return false
			}
		case StateExpr:
			if !w.safeExpr(BytecodeExp(x)) {
				return false
			}
		case StateBlock:
			if !w.pureBlock(&x, depth) {
				return false
			}
		case CallFunction:
			if !w.safeExpr(x.arg) || !w.pureFunc(x.name, depth+1) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (w *cbrWalker) pureBlock(b *StateBlock, depth int) bool {
	if b.loopBlock || !w.safeExpr(b.trigger) || !w.pureCtrls(b.ctrls, depth) {
		return false
	}
	return b.elseBlock == nil || w.pureBlock(b.elseBlock, depth)
}

// pureFunc reports whether calling the ZSS function name only sets local
// variables (its own, and the caller's through its return values).
func (w *cbrWalker) pureFunc(name string, depth int) bool {
	if p, ok := w.pure[name]; ok {
		return p
	}
	fn, ok := w.funcs[name]
	if !ok || depth > 8 {
		return false
	}
	w.pure[name] = false // while it is being looked at
	p := w.pureCtrls(fn.ctrls, depth)
	w.pure[name] = p
	return p
}

// local notes an assignment passed in the state's code, a step to run again
// before the routes after it: a local variable's or a function call's with
// return values, or a VarSet or VarAdd of the character's own variables.
// A local variable step that cannot be run again safely leaves the routes
// after it not checkable when they read local variables; a VarSet that
// cannot is left out, the variable keeping the value it has then.
func (w *cbrWalker) local(sc StateController, p cbrPath) {
	safeConds := !p.loop
	for _, cd := range p.conds {
		safeConds = safeConds && w.safeExpr(cd.exp)
	}
	switch x := sc.(type) {
	case varAssign:
		w.localsOK = w.localsOK && safeConds && w.safeExpr(x.be)
	case CallFunction:
		if len(x.ret) == 0 {
			return // it sets no local variable of the state
		}
		w.localsOK = w.localsOK && safeConds && w.safeExpr(x.arg) && w.pureFunc(x.name, 0)
	case varSet:
		scb := StateControllerBase(x)
		kind, ok := cbrParamExp(scb, varSet_sctrltype)
		set, fixed := cbrConstInt(kind)
		if safeConds && ok && fixed && (set == 0 || set == 1) &&
			!scb.hasParam(varSet_redirectid) && len(x) > 0 && w.scan.safe[unsafe.SliceData([]byte(x))] {
			w.locals = append(w.locals, cbrLocalStep{conds: p.conds, ctrl: sc, vars: true})
		}
		return
	default:
		return
	}
	if w.localsOK {
		w.locals = append(w.locals, cbrLocalStep{conds: p.conds, ctrl: sc})
	}
}

// block lists the routes in b, and in its else branch, as StateBlock.Run
// runs them.
func (w *cbrWalker) block(b *StateBlock, p cbrPath) {
	p.hp = p.hp && b.ignorehitpause >= -1
	if b.persistentIndex >= 0 {
		p.persist = append(append([]int32(nil), p.persist...), b.persistentIndex)
	}
	in := p
	if b.loopBlock {
		in.loop = true
	} else if len(b.trigger) > 0 {
		in.conds = append(append([]cbrCond(nil), p.conds...), cbrCond{b.trigger, true})
	}
	for _, sc := range b.ctrls {
		if sb, ok := sc.(StateBlock); ok {
			w.block(&sb, in)
		} else if key := cbrCtrlKey(sc); key != nil {
			w.route(sc, key, in, b.ctrlsIgnorehitpause)
		} else {
			w.local(sc, in)
		}
	}
	if b.elseBlock != nil && !b.loopBlock {
		ep := p
		if len(b.trigger) > 0 {
			ep.conds = append(append([]cbrCond(nil), p.conds...), cbrCond{b.trigger, false})
		}
		w.block(b.elseBlock, ep)
	}
}

// route lists one ChangeState or SelfState.
func (w *cbrWalker) route(sc StateController, key *byte, p cbrPath, ctrlsIhp bool) {
	var scb StateControllerBase
	self := false
	switch x := sc.(type) {
	case changeState:
		scb = StateControllerBase(x)
	case selfState:
		scb, self = StateControllerBase(x), true
	}
	r := cbrRoute{
		id:       int32(len(w.set.routes)),
		owner:    w.owner,
		self:     self,
		redirect: scb.hasParam(changeState_redirectid),
		foreign:  self && scb.hasParam(changeState_readplayerid),
		hitpause: p.hp && ctrlsIhp,
		persist:  p.persist,
		pre:      w.locals[:len(w.locals):len(w.locals)],
		preOK:    w.localsOK,
		numVars:  w.numVars,
	}
	if exp, ok := cbrParamExp(scb, changeState_value); ok {
		r.target, r.fixed = cbrConstInt(exp)
	}
	checkable := !p.loop && w.owner != cbrInFunction
	if drafts, ok := w.scan.cns[key]; ok {
		for _, d := range drafts {
			conds := make([]cbrCond, len(d.exprs))
			scans := make([]*cbrExprScan, len(d.exprs))
			for i := range d.exprs {
				conds[i] = cbrCond{d.exprs[i], true}
				scans[i] = &d.scans[i]
			}
			r.groups = append(r.groups, w.group(conds, scans, checkable))
		}
	} else {
		scans := make([]*cbrExprScan, len(p.conds))
		for i, cd := range p.conds {
			if s, ok := w.scan.exprs[unsafe.SliceData(cd.exp)]; ok {
				scans[i] = &s
			}
		}
		r.groups = []cbrGroup{w.group(p.conds, scans, checkable)}
	}
	for _, st := range r.pre {
		r.preVars = r.preVars || st.vars
	}
	for gi := range r.groups {
		g := &r.groups[gi]
		if g.locals && !r.preOK {
			g.checkable = false
		}
		r.anyCmd = r.anyCmd || len(g.cmds) > 0
		r.anyAI = r.anyAI || g.ai
		r.person = r.person || g.person() && !r.redirect && !r.foreign
	}
	w.set.byCtrl[key] = r.id
	w.set.routes = append(w.set.routes, r)
}

// group makes one trigger group from its conditions and what each checks
// (nil when nothing was noted, which leaves the group not checkable).
func (w *cbrWalker) group(conds []cbrCond, scans []*cbrExprScan, checkable bool) cbrGroup {
	g := cbrGroup{conds: conds, checkable: checkable}
	for i, s := range scans {
		if s == nil {
			g.checkable = false
			continue
		}
		want := conds[i].want
		for _, ref := range s.cmds {
			if ref.neg != want {
				if !cbrHas(g.cmds, ref.name) {
					g.cmds = append(g.cmds, ref.name)
				}
			} else if !cbrHas(g.notCmds, ref.name) {
				g.notCmds = append(g.notCmds, ref.name)
			}
		}
		for _, need := range s.ai {
			g.ai = true
			if !want {
				need = need.negated()
			}
			if need == cbrAIAbove {
				g.aiAbove = true
			}
		}
		if s.assigns || s.rawInput {
			g.checkable = false
		}
		g.random = g.random || s.random
		g.locals = g.locals || s.localVars
	}
	// A command the group needs both active and not counts as needed.
	notCmds := g.notCmds[:0]
	for _, n := range g.notCmds {
		if !cbrHas(g.cmds, n) {
			notCmds = append(notCmds, n)
		}
	}
	g.notCmds = notCmds
	g.ranks = make([]int, len(g.cmds))
	best := -1
	for i, n := range g.cmds {
		g.ranks[i] = cbrCommandRank(w.cmdl, n)
		if g.ranks[i] > best {
			best, g.main = g.ranks[i], n
		}
	}
	return g
}

// cbrCommandRank ranks a command as a route's main command: one with a
// button press before one with directions only, then the longer one.
func cbrCommandRank(cmdl *CommandList, name string) int {
	if cmdl == nil {
		return 0
	}
	i, ok := cmdl.Names[name]
	if !ok || i < 0 || i >= len(cmdl.Commands) || len(cmdl.Commands[i]) == 0 {
		return 0
	}
	steps := cmdl.Commands[i][0].steps
	rank := len(steps)
	for _, st := range steps {
		for _, k := range st.keys {
			if k.IsButtonPress() {
				return 1000 + rank
			}
		}
	}
	return rank
}

func cbrHas(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// cbrParamExp returns a controller parameter's first expression.
func cbrParamExp(scb StateControllerBase, id byte) (BytecodeExp, bool) {
	for i := 0; i+1 < len(scb); {
		pid, n := scb[i], int(scb[i+1])
		i += 2
		for m := 0; m < n; m++ {
			if i+4 > len(scb) {
				return nil, false
			}
			l := int(*(*int32)(unsafe.Pointer(&scb[i])))
			i += 4
			if i+l > len(scb) {
				return nil, false
			}
			if pid == id && m == 0 {
				// As StateControllerBase.run reads it
				return (*(*BytecodeExp)(unsafe.Pointer(&scb)))[i : i+l], true
			}
			i += l
		}
	}
	return nil, false
}

// cbrConstInt reads a compiled constant integer expression.
func cbrConstInt(exp BytecodeExp) (int32, bool) {
	switch {
	case len(exp) == 2 && exp[0] == OC_int8:
		return int32(int8(exp[1])), true
	case len(exp) == 5 && exp[0] == OC_int:
		return *(*int32)(unsafe.Pointer(&exp[1])), true
	}
	return 0, false
}

// cbrEntry is the label a watched character's state change gets.
type cbrEntry struct {
	kind   cbr.EntryKind
	cmd    string
	helper bool
}

// cbrCheckState is the state of a moment check while it evaluates
// conditions. The command and AILevel triggers (bytecode.go) read it for
// the checked player's characters: AILevel reads 0, and a command is
// active only when it is in press, or, with real, when it is in the real
// input buffer (the engine AI's cheat does not count).
type cbrCheckState struct {
	on    bool
	pn    int
	real  bool
	press []string
}

var cbrCheck cbrCheckState

// cbrCheckCommand answers the command trigger for one of the checked
// player's characters while a check runs. list and at are the command
// lists the trigger reads the name from and checks the buffer in.
func cbrCheckCommand(c *Char, list, at int, name string) bool {
	if !c.keyctrl[0] || c.cmd == nil {
		return false
	}
	if !cbrCheck.real {
		return cbrHas(cbrCheck.press, name)
	}
	if list < 0 || list >= len(c.cmd) || at < 0 || at >= len(c.cmd) {
		return false
	}
	pos, ok := c.cmd[list].Names[name]
	if !ok {
		return false
	}
	for _, cm := range c.cmd[at].At(pos) {
		if cm.curbuftime > 0 {
			return true
		}
	}
	return false
}

// cbrBufferedRank returns, of the commands names that are in c's real input
// buffer, the one with the highest rank, or ok=false when none is.
func cbrBufferedRank(c *Char, names []string, ranks []int) (best string, ok bool) {
	if c.cmd == nil || !c.keyctrl[0] || c.playerNo < 0 || c.playerNo >= len(c.cmd) {
		return "", false
	}
	cl := &c.cmd[c.playerNo]
	top := -1
	for i, n := range names {
		pos, found := cl.Names[n]
		if !found {
			continue
		}
		for _, cm := range cl.At(pos) {
			if cm.curbuftime > 0 {
				if i < len(ranks) && ranks[i] > top {
					top, best, ok = ranks[i], n, true
				}
				break
			}
		}
	}
	return best, ok
}

// cbrCharCache is what evaluating a trigger may change on a character: its
// nearest-enemy lists, built on first use, and its last P2.
type cbrCharCache struct {
	c          *Char
	near, p2   []int32
	p2EnemyBak int32
}

// cbrEvalScratch is reused by every evaluation, so that it does not
// allocate once it has grown.
var cbrEvalScratch []cbrCharCache

// eval evaluates group g of route r on c as r's state's code would, with the
// check's commands (real, or only press active) and AILevel reading 0 for
// c's player, and restores everything the evaluation may have touched.
// With replay, a group that reads ZSS local variables gets a frame of its
// own, in which the local variable assignments before r (r.pre) run first;
// without it, it reads those of the state running now, which must be r's.
// failed is set when evaluating panicked.
func (set *cbrRouteSet) eval(c *Char, r *cbrRoute, g *cbrGroup, real bool,
	press []string, replay bool) (ok, failed bool) {
	seed, ws, wc, minus := sys.randseed, sys.workingState, sys.workingChar, c.minus
	stack, console, nearChanged := len(sys.bcStack), sys.consoleText, sys.charList.enemyNearChanged
	vars, varStack, sctrl := sys.bcVar, sys.bcVarStack, c.currentSctrlIndex
	saved := cbrEvalScratch[:0]
	for _, ch := range sys.charList.runOrder {
		if ch != nil {
			saved = append(saved, cbrCharCache{ch, ch.enemyNearList, ch.p2EnemyList, ch.p2EnemyBackup})
		}
	}
	cbrEvalScratch = saved
	prevCheck := cbrCheck
	defer func() {
		if recover() != nil {
			ok, failed = false, true
		}
		cbrCheck = prevCheck
		sys.randseed, sys.workingState, sys.workingChar, c.minus = seed, ws, wc, minus
		if len(sys.bcStack) > stack {
			sys.bcStack = sys.bcStack[:stack]
		}
		sys.bcVar, sys.bcVarStack, c.currentSctrlIndex = vars, varStack, sctrl
		sys.consoleText, sys.charList.enemyNearChanged = console, nearChanged
		for _, s := range saved {
			s.c.enemyNearList, s.c.p2EnemyList, s.c.p2EnemyBackup = s.near, s.p2, s.p2EnemyBak
		}
	}()
	cbrCheck = cbrCheckState{on: true, pn: c.playerNo, real: real, press: press}
	sys.workingState, sys.workingChar = &set.evalState, c
	switch {
	case r.owner >= 0:
		c.minus = 0
	case r.owner == -10:
		c.minus = -4
	case r.owner >= -4:
		c.minus = int8(r.owner)
	}
	if replay && (g.locals || len(r.pre) > 0) {
		// A function call needs the bytecode stack empty, as it is between
		// controllers.
		if g.locals && !r.preOK || stack != 0 {
			return false, true
		}
		if r.preVars {
			cbrSaveVars(c)
			defer cbrRestoreVars(c)
		}
		// Run as when hitpause ends, as the route is (see runsNow)
		sys.bcVar = sys.bcVarStack.Alloc(int(r.numVars))
		for _, st := range r.pre {
			if cbrCondsHold(c, st.conds) {
				st.ctrl.Run(c, nil)
			}
		}
	}
	return cbrCondsHold(c, g.conds), false
}

// cbrVarScratch holds a character's variables while a check's VarSets run
// again, reused so that it does not allocate once it has grown.
var cbrVarScratch struct {
	vars, sysVars   map[int32]int32
	fvars, sysFvars map[int32]float32
}

func cbrSaveVars(c *Char) {
	s := &cbrVarScratch
	if s.vars == nil {
		s.vars, s.sysVars = map[int32]int32{}, map[int32]int32{}
		s.fvars, s.sysFvars = map[int32]float32{}, map[int32]float32{}
	}
	cbrCopyMap(s.vars, c.cnsvar)
	cbrCopyMap(s.sysVars, c.cnssysvar)
	cbrCopyMap(s.fvars, c.cnsfvar)
	cbrCopyMap(s.sysFvars, c.cnssysfvar)
}

// cbrRestoreVars puts back the variables cbrSaveVars saved, in the same maps.
func cbrRestoreVars(c *Char) {
	s := &cbrVarScratch
	cbrCopyMap(c.cnsvar, s.vars)
	cbrCopyMap(c.cnssysvar, s.sysVars)
	cbrCopyMap(c.cnsfvar, s.fvars)
	cbrCopyMap(c.cnssysfvar, s.sysFvars)
}

// cbrCopyMap makes dst hold what src holds; a nil dst is left alone.
func cbrCopyMap[V int32 | float32](dst, src map[int32]V) {
	if dst == nil {
		return
	}
	clear(dst)
	for k, v := range src {
		dst[k] = v
	}
}

// cbrCondsHold evaluates conditions on c.
func cbrCondsHold(c *Char, conds []cbrCond) bool {
	for _, cd := range conds {
		if cd.exp.evalB(c) != cd.want {
			return false
		}
	}
	return true
}

// runsNow reports whether route r's code would run for c at this moment:
// its state runs now (a negative state, as Char.actionRun runs them, or c's
// current state, its own), and its persistent counters let it. sure is
// false for a route inside a ZSS function, where this cannot be told.
// afterHitpause is set when c is in hitpause and r does not run during it:
// it runs as soon as hitpause ends, when nothing else changes first.
func (set *cbrRouteSet) runsNow(c *Char, r *cbrRoute) (runs, sure, afterHitpause bool) {
	ok := false
	switch {
	case r.owner == cbrInFunction:
		return false, false, false
	case r.owner == -4 || r.owner == -10:
		ok = true
	case c.pauseBool:
		return false, true, false
	case r.owner == -3:
		ok = c.ss.sb.playerNo == c.playerNo && (c.isPlayerType() || c.keyctrl[2])
	case r.owner == -2:
		ok = c.isPlayerType() || c.keyctrl[1]
	case r.owner == -1:
		ok = c.ss.sb.playerNo == c.playerNo && c.keyctrl[0]
	case r.owner >= 0:
		ok = c.ss.no == r.owner && c.ss.sb.playerNo == c.playerNo
	}
	if !ok {
		return false, true, false
	}
	// Negative states' controllers always run once per frame; a positive
	// state's counters are those of its current run.
	if r.owner >= 0 {
		ps := c.ss.sb.ctrlsps
		for _, idx := range r.persist {
			if idx >= 0 && int(idx) < len(ps) {
				v := ps[idx]
				if v != math.MaxInt32 {
					v--
				}
				if v > 0 {
					return false, true, false
				}
			}
		}
	}
	return true, true, c.hitPause() && !r.hitpause
}

// cbrKeptThroughHitpause reports whether every one of the commands stays
// in c's input buffer through hitpause (buffer.hitpause, as by default), so
// that a person's command still comes out as hitpause ends.
func cbrKeptThroughHitpause(c *Char, names []string) bool {
	if c.cmd == nil || c.playerNo < 0 || c.playerNo >= len(c.cmd) {
		return false
	}
	cl := &c.cmd[c.playerNo]
	for _, n := range names {
		pos, ok := cl.Names[n]
		if !ok {
			return false
		}
		for _, cm := range cl.At(pos) {
			if !cm.buffer_hitpause {
				return false
			}
		}
	}
	return true
}

// inputEntry reports whether route r, which just fired on c (the character
// whose code ran it), fired as a person route by a command in the real
// input buffer: a group with a command in the buffer holds with AILevel
// reading 0 and only the buffer's commands active. A group that cannot be
// evaluated, or draws Random, counts when it does not need the AI level
// above 0.
func (set *cbrRouteSet) inputEntry(c *Char, r *cbrRoute) (cbrEntry, bool) {
	for gi := range r.groups {
		g := &r.groups[gi]
		cmd, buffered := cbrBufferedRank(c, g.cmds, g.ranks)
		if !buffered {
			continue
		}
		if g.checkable {
			ok, failed := set.eval(c, r, g, true, nil, false)
			if ok {
				return cbrEntry{kind: cbr.EntryInput, cmd: cmd}, true
			}
			// A Random it draws again may come out otherwise than the one
			// that let the route fire.
			if !failed && !g.random {
				continue
			}
		}
		if !g.aiAbove {
			return cbrEntry{kind: cbr.EntryInput, cmd: cmd}, true
		}
	}
	return cbrEntry{}, false
}

// momentCheck is run when c's own AI changes c's state to target: it
// evaluates, at this moment, the routes into target that need a command. A
// person route that does not hold but draws Random makes the answer Unknown
// rather than Cheat.
func (set *cbrRouteSet) momentCheck(c *Char, target int32) cbrEntry {
	unsure := false
	for _, id := range set.into[target] {
		r := &set.routes[id]
		runs, sure, afterHitpause := set.runsNow(c, r)
		if !sure {
			unsure = unsure || r.person
			continue
		}
		if !runs {
			continue
		}
		for gi := range r.groups {
			g := &r.groups[gi]
			if len(g.cmds) == 0 || afterHitpause && !cbrKeptThroughHitpause(c, g.cmds) {
				continue
			}
			if !g.checkable {
				unsure = unsure || g.person()
				continue
			}
			ok, failed := set.eval(c, r, g, false, g.cmds, true)
			if ok {
				return cbrEntry{kind: cbr.EntryReachable, cmd: g.main}
			}
			// One that draws Random might have held with another draw.
			unsure = unsure || (failed || g.random) && g.person()
		}
	}
	for _, id := range set.loose {
		if runs, sure, _ := set.runsNow(c, &set.routes[id]); runs || !sure {
			unsure = true
			break
		}
	}
	if unsure {
		return cbrEntry{kind: cbr.EntryUnknown}
	}
	return cbrEntry{kind: cbr.EntryCheat}
}

// classify labels a state change of the watched root c to target, which
// the controller identified by key makes, run by char (c itself, or a
// helper redirecting to it). It runs before the change.
func (set *cbrRouteSet) classify(char, c *Char, key *byte, target int32) cbrEntry {
	if char.playerNo != c.playerNo || sys.workingState == nil ||
		sys.workingState.playerNo != c.playerNo {
		// Another player's code: its throw's custom state, say.
		return cbrEntry{kind: cbr.EntryAuto}
	}
	id, ok := set.byCtrl[key]
	if !ok || id < 0 || int(id) >= len(set.routes) {
		return cbrEntry{}
	}
	r := &set.routes[id]
	helper := char != c
	if r.foreign {
		// Into another player's states, as its throw would put c
		return cbrEntry{kind: cbr.EntryAuto, helper: helper}
	}
	if !r.anyCmd && !r.anyAI {
		decided := false
		if helper {
			decided = c.ctrl()
		} else if r.owner >= -3 && r.owner <= -1 {
			decided = c.ctrl() || c.ss.moveType == MT_A
		}
		if !decided {
			return cbrEntry{kind: cbr.EntryAuto, helper: helper}
		}
	} else if e, ok := set.inputEntry(char, r); ok {
		e.helper = helper
		return e
	}
	e := set.momentCheck(c, target)
	e.helper = helper
	return e
}

// cbrEngineEntry labels a state change the engine made itself, not a
// controller: its built-in walk, jump, air jump, crouch and guard, which go
// by the directions in the input buffer and need control, are Input;
// anything else (hit and guard states, landing, getting up) is Auto.
func cbrEngineEntry(c *Char) cbrEntry {
	switch c.ss.no {
	case 10, 20, 40, 45, 120:
		if c.ctrl() {
			return cbrEntry{kind: cbr.EntryInput}
		}
	}
	return cbrEntry{kind: cbr.EntryAuto}
}
