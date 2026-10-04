package cbr

import (
	"math/rand"
	"testing"
)

// Regression tests for defects found in review.

// frameFeeder drives an engine with hand-built two-player snapshots.
type frameFeeder struct {
	eng  *Engine
	tick int64
}

func (ff *frameFeeder) frame(me CharSnap) {
	ff.tick++
	me.PlayerNo, me.TeamSide, me.FacingRight = 0, 0, true
	me.Life, me.LifeMax, me.Alive, me.CmdKnown = 1000, 1000, true, true
	if me.StateType == StUnknown {
		me.StateType = StStand
	}
	en := CharSnap{PlayerNo: 1, TeamSide: 1, Pos: [3]float32{150}, Life: 1000, LifeMax: 1000, Alive: true, Ctrl: true, StateType: StStand, MoveType: MtIdle}
	ff.eng.Frame(&Snapshot{World: WorldSnap{Tick: ff.tick, RoundState: 2, WinTeam: -1, StageLeft: -300, StageRight: 300},
		Players: []PlayerSnap{{Root: me, Present: true}, {Root: en, Present: true}}})
}

func (ff *frameFeeder) end() {
	ff.eng.RoundEnd(&Snapshot{World: WorldSnap{Tick: ff.tick + 1, RoundState: 3, WinTeam: -1}, Players: []PlayerSnap{
		{Root: CharSnap{TeamSide: 0, Life: 1000, LifeMax: 1000, Alive: true}, Present: true},
		{Root: CharSnap{TeamSide: 1, Pos: [3]float32{150}, Life: 1000, LifeMax: 1000, Alive: true}, Present: true}}})
}

// A case whose inputs end on its exec frame must still have a failed exec
// noted, and a hypothesis play must count toward its classification.
func TestExecFailAtLastInputIsNoted(t *testing.T) {
	p := DefaultParams()
	eng := New(p, t.TempDir(), 1)
	st, _ := eng.Store("Rush")
	c := &Case{ID: 1, Source: SrcSynthesized, Kind: KindCommand, ExecFrame: 2, MoveRef: 1234, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InD, InD | InF, InF | InA}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle,
		Start: Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 160}, TimeFrac: 1}}
	st.Admit(p, []*Case{c})
	s := newSim(600)
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	runRound(s, eng, [2]controller{nil, &defender{rng: rand.New(rand.NewSource(1))}}, 600)
	if c.Outcome.Plays == 0 {
		t.Fatal("setup: the case was never played")
	}
	if c.Outcome.ExecFails == 0 {
		t.Fatalf("%d plays never produced the state, but no failure was noted", c.Outcome.Plays)
	}
	if h := st.Hypotheses[1234]; h == nil || h.Attempts == 0 {
		t.Fatal("hypothesis plays must count toward its classification")
	}
}

// The better-case check must not switch away from a command case whose
// command has not come out yet; the exec check decides first.
func TestBetterCaseWaitsForExec(t *testing.T) {
	p := DefaultParams()
	st := NewStore("x")
	start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 120}, TimeFrac: 1}
	cmd := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 1234, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InA, 0, 0, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start}
	far := start
	far.Enemy.RelX = 200
	far.Enemy.State = StAir
	mov := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, Inputs: []InputBits{InF, InF, InF, InF}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: far}
	st.Admit(p, []*Case{cmd, mov})
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	self := &CharSnap{StateNo: 0, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	for tick := int64(1); tick <= 12; tick++ {
		cur := start
		if tick > 1 {
			cur = far
		}
		d.input(&WorldSnap{RoundState: 2, Tick: tick}, self, &cur)
		d.observe(self)
	}
	if cmd.Outcome.Plays == 0 || cmd.Outcome.ExecFails == 0 {
		t.Fatalf("the move never came out: plays=%d fails=%d", cmd.Outcome.Plays, cmd.Outcome.ExecFails)
	}
}

// A mid-action case starts at its recorded point in the action, not before.
func TestMidActionCaseNotEarly(t *testing.T) {
	p := DefaultParams()
	st := NewStore("x")
	start := Feat{RoundState: 2, Self: CharFeat{Present: true, StateNo: 200, StateTime: 10, Move: MtAttack, State: StStand}, Enemy: CharFeat{Present: true, RelX: 50}, TimeFrac: 1}
	c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 210, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InBtnB, 0, 0}, StartStateType: StStand, StartCtrl: false, StartStateNo: 200, StartMoveType: MtAttack, Start: start}
	st.Admit(p, []*Case{c})
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	for s := int32(1); s <= 12; s++ {
		cur := start
		cur.Self.StateTime = s
		self := &CharSnap{StateNo: 200, StateTime: s, StateType: StStand, MoveType: MtAttack}
		if _, ok := d.input(&WorldSnap{RoundState: 2, Tick: int64(s)}, self, &cur); ok {
			if s != start.Self.StateTime {
				t.Fatalf("started at state time %d, recorded at %d", s, start.Self.StateTime)
			}
			return
		}
	}
	t.Fatal("never started")
}

// With several opponents, a change of nearest enemy is not damage.
func TestNoPhantomDamageOnEnemySwitch(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcHumanLocal)
	var got []*resolvedChain
	ps := eng.players[0]
	prev := ps.ct.onFinal
	ps.ct.onFinal = func(rc *resolvedChain) { got = append(got, rc); prev(rc) }
	mk := func(tick int64, nearX, farX float32, attacking bool) *Snapshot {
		s := &Snapshot{World: WorldSnap{Tick: tick, RoundState: 2, MaxRoundTime: 99, CurRoundTime: 99, StageLeft: -300, StageRight: 300, WinTeam: -1}}
		me := CharSnap{PlayerNo: 0, TeamSide: 0, FacingRight: true, Life: 1000, LifeMax: 1000, Alive: true, Ctrl: !attacking, StateType: StStand, MoveType: MtIdle, CmdKnown: true}
		if attacking {
			me.MoveType, me.StateNo = MtAttack, 200
		}
		e1 := CharSnap{ID: 2, PlayerNo: 1, TeamSide: 1, Pos: [3]float32{nearX}, Life: 1000, LifeMax: 1000, Alive: true, Ctrl: true, StateType: StStand, MoveType: MtIdle}
		e2 := CharSnap{ID: 3, PlayerNo: 3, TeamSide: 1, Pos: [3]float32{farX}, Life: 300, LifeMax: 1000, Alive: true, Ctrl: true, StateType: StStand, MoveType: MtIdle}
		s.Players = []PlayerSnap{{Root: me, Present: true, Name: "A"}, {Root: e1, Present: true, Name: "B"}, {}, {Root: e2, Present: true, Name: "C"}}
		return s
	}
	tick := int64(1)
	for ; tick < 5; tick++ {
		eng.Frame(mk(tick, 50, 200, false))
	}
	for ; tick < 10; tick++ {
		eng.Frame(mk(tick, 50, 200, true))
	}
	for ; tick < 15; tick++ {
		eng.Frame(mk(tick, 200, 50, true))
	}
	for ; tick < 60; tick++ {
		eng.Frame(mk(tick, 200, 50, false))
	}
	eng.RoundEnd(mk(tick, 200, 50, false))
	for _, rc := range got {
		if rc.comp.DamageDealt > 0 {
			t.Fatalf("damage %.2f banked with no life lost", rc.comp.DamageDealt)
		}
	}
}

// A person's dash (F, release, F) is recorded with its whole motion.
func TestHumanDashKeepsWholeMotion(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("A", map[string]Motion{"FF": {Inputs: []InputBits{InF, InF, 0, InF}, Steps: 3}, "holdfwd": {Inputs: []InputBits{InF}, Steps: 1}})
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcHumanLocal)
	ff := &frameFeeder{eng: eng}
	for i := 0; i < 10; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	ff.frame(CharSnap{Input: InF, StateNo: 20, Ctrl: true, MoveType: MtIdle, Commands: []string{"holdfwd"}})
	ff.frame(CharSnap{Input: InF, StateNo: 20, Ctrl: true, MoveType: MtIdle, Commands: []string{"holdfwd"}})
	ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	ff.frame(CharSnap{Input: InF, StateNo: 100, MoveType: MtIdle, Commands: []string{"FF", "holdfwd"}})
	for i := 0; i < 20; i++ {
		ff.frame(CharSnap{StateNo: 100, MoveType: MtIdle})
	}
	for i := 0; i < 40; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	ff.end()
	st, _ := eng.Store("A")
	for _, c := range st.Cases {
		if c.Kind == KindCommand && c.MoveRef == 100 {
			in := c.Inputs[:c.ExecFrame+1]
			n := len(in)
			if n < 4 || in[n-4] != InF || in[n-3] != InF || in[n-2] != 0 || in[n-1] != InF {
				t.Fatalf("dash case must hold F, F, release, F before its exec frame: %v", in)
			}
			return
		}
	}
	t.Fatal("no dash case recorded")
}

// Frames the CBR layer drove are never part of a recorded case.
func TestBackdateStopsAtDrivenFrames(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("A", map[string]Motion{"a": {Inputs: []InputBits{InA}, Steps: 1}})
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcHumanLocal)
	ff := &frameFeeder{eng: eng}
	for i := 0; i < 5; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	for i := 0; i < 6; i++ {
		ff.frame(CharSnap{Input: InF, StateNo: 20, Ctrl: true, MoveType: MtIdle, CBRDriven: true})
	}
	ff.frame(CharSnap{Input: InF | InA, StateNo: 200, MoveType: MtAttack, Commands: []string{"a"}})
	for i := 0; i < 30; i++ {
		ff.frame(CharSnap{StateNo: 200, MoveType: MtAttack})
	}
	for i := 0; i < 40; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	ff.end()
	st, _ := eng.Store("A")
	found := false
	for _, c := range st.Cases {
		if c.Kind == KindCommand {
			found = true
			if c.ExecFrame != 0 {
				t.Fatalf("%d CBR-driven frames were recorded into the case", c.ExecFrame)
			}
		}
	}
	if !found {
		t.Fatal("no command case recorded")
	}
}

// jabber presses A near the opponent; on some of those frames the engine
// AI's cheat holds an unrelated command (a super it cannot afford).
type jabber struct{ rng *rand.Rand }

func (j *jabber) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	if !c.ctrl {
		return 0
	}
	if absf(o.x-c.x) > 60 {
		return InF
	}
	if c.input&InA != 0 {
		return 0
	}
	if j.rng.Intn(4) == 0 && c.power < 1000 {
		c.assist = "qcf_c"
	}
	return InA
}

// A command active by coincidence when a state is entered is not credited
// with the state once the match shows what really leads to it.
func TestCoincidentalCommandNotCredited(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcEngineAI, 1, 3, func(r *rand.Rand) controller { return &jabber{rng: r} })
	st, _ := eng.Store("Rush")
	jabs, wrong := 0, 0
	for _, c := range st.Cases {
		if c.Kind == KindCommand && c.MoveRef == 200 {
			jabs++
			if c.Command != "a" {
				wrong++
			}
		}
	}
	if jabs == 0 {
		t.Fatal("setup: no jab recorded")
	}
	if wrong > 0 {
		t.Fatalf("%d of %d jab cases credited to another command", wrong, jabs)
	}
}

// A command right after control returns is a decision made with control:
// back-dating must not reach into the action that just ended.
func TestBackdateStopsAtControlRegain(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("A", map[string]Motion{"a": {Inputs: []InputBits{InA}, Steps: 1}, "qcf_a": {Inputs: []InputBits{InD, InD | InF, InF | InA}, Steps: 4}})
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcHumanLocal)
	ff := &frameFeeder{eng: eng}
	for i := 0; i < 5; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	for round := 0; round < 3; round++ {
		ff.frame(CharSnap{Input: InA, StateNo: 200, MoveType: MtAttack, Commands: []string{"a"}})
		for i := 0; i < 14; i++ {
			ff.frame(CharSnap{StateNo: 200, StateTime: int32(i + 1), MoveType: MtAttack})
		}
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle}) // control back, neutral
	}
	ff.frame(CharSnap{Input: InA, StateNo: 200, MoveType: MtAttack, Commands: []string{"a"}})
	for i := 0; i < 40; i++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	}
	ff.end()
	st, _ := eng.Store("A")
	n := 0
	for _, c := range st.Cases {
		if c.Kind == KindCommand && c.MoveRef == 200 {
			n++
			if !c.StartCtrl || c.CancelFrom != 0 {
				t.Fatalf("a jab one frame after control returned must start from control (StartCtrl=%v CancelFrom=%d)", c.StartCtrl, c.CancelFrom)
			}
		}
	}
	if n < 3 {
		t.Fatalf("setup: expected the jabs recorded, got %d", n)
	}
	if len(st.Trans.Edges) != 0 {
		t.Fatal("jabs that follow each other after control returns are not cancels")
	}
}

// Damage an ally dealt is not the focus's.
func TestAllyDamageNotCredited(t *testing.T) {
	ct := newChainTracker(DefaultParams(), 0, new(uint64))
	snap := func(life int32, hitBy int) *Snapshot {
		return &Snapshot{World: WorldSnap{RoundState: 2}, Players: []PlayerSnap{
			{Present: true, Root: CharSnap{ID: 1, TeamSide: 0, Life: 1000, LifeMax: 1000, Alive: true}},
			{Present: true, Root: CharSnap{ID: 2, TeamSide: 1, Life: life, LifeMax: 1000, Alive: true, HitBy: hitBy}},
			{Present: true, Root: CharSnap{ID: 3, TeamSide: 0, Life: 1000, LifeMax: 1000, Alive: true}},
		}}
	}
	ct.opponentDamage(snap(1000, 0))
	if d, _ := ct.opponentDamage(snap(700, 3)); d != 0 {
		t.Fatalf("the ally (player 3) hit; credited %v", d)
	}
	if d, _ := ct.opponentDamage(snap(600, 1)); d < 0.09 || d > 0.11 {
		t.Fatalf("the focus (player 1) hit for 10%%; credited %v", d)
	}
}

// A command played from control re-presses only keys it pressed fresh in
// the recording; a mid-action case keeps the recorded input stream as is.
func TestFreshPressOnlyWhenRecordedFresh(t *testing.T) {
	p := DefaultParams()
	st := NewStore("x")
	mk := func(ctrl bool, held InputBits) *Case {
		f := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 60}, InBtn: []InputBits{held.Buttons()}, InDir: []uint8{held.Dir()}}
		c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 210, ExecAttack: true,
			Inputs: []InputBits{InF | InA, 0}, StartStateType: StStand, StartCtrl: ctrl, Start: f}
		if !ctrl {
			c.StartMoveType, c.StartStateNo = MtAttack, 200
		}
		return c
	}
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	self := &CharSnap{Input: InF | InA, Ctrl: true, StateType: StStand}
	d.begin(mk(true, 0), self, 1)
	if d.shift != 1 {
		t.Fatal("keys held now but not at the recorded decision must be released first")
	}
	d.begin(mk(true, InF|InA), self, 2)
	if d.shift != 0 {
		t.Fatal("keys held at the recorded decision stay held")
	}
	d.begin(mk(false, 0), self, 3)
	if d.shift != 0 {
		t.Fatal("a case begun mid-action is played as recorded")
	}
}

// TestMostRepeatedIsDeterministic: the repeated move of a chain whose moves
// tie does not depend on map order, so a match learned live and from its
// replay store the same technique record.
func TestMostRepeatedIsDeterministic(t *testing.T) {
	for i := 0; i < 50; i++ {
		ref, n := mostRepeated(map[int32]int32{1051: 1, 1050: 1, 200: 1, 1100: 1})
		if ref != 200 || n != 1 {
			t.Fatalf("tie: got %d x%d", ref, n)
		}
		ref, n = mostRepeated(map[int32]int32{1051: 2, 1050: 3, 200: 3})
		if ref != 200 || n != 3 {
			t.Fatalf("most repeated: got %d x%d", ref, n)
		}
	}
	if ref, n := mostRepeated(nil); ref != 0 || n != 0 {
		t.Fatalf("empty chain: got %d x%d", ref, n)
	}
}
