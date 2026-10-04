package cbr

import (
	"math/rand"
	"testing"
)

// recordWith records player 0 with ctl against the defender, with the
// simulator's command layer on, for the given rounds, then ends the match.
func recordWith(t *testing.T, eng *Engine, src Source, rounds int, seed int64, mk func(*rand.Rand) controller) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		s.cmdLayer = true
		rng := rand.New(rand.NewSource(seed + int64(r)))
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		if err := eng.Record(0, src); err != nil {
			t.Fatal(err)
		}
		runRound(s, eng, [2]controller{mk(rng), &defender{rng: rand.New(rand.NewSource(seed + 500 + int64(r)))}}, 3600)
	}
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
}

func hasPrefix(in, motion []InputBits) bool {
	if len(in) < len(motion) {
		return false
	}
	for i := range motion {
		if in[i] != motion[i] {
			return false
		}
	}
	return true
}

func TestAssistedCommandRecordedWithMotion(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcEngineAI, 4, 10, func(r *rand.Rand) controller { return &cheater{rng: r} })
	st, _ := eng.Store("Rush")
	if st.ChangeStateAI {
		t.Fatal("assisted commands are not direct state changes")
	}
	motion := simMotions["qcf_a"].Inputs
	n := 0
	for _, c := range st.Cases {
		if c.Kind != KindCommand || c.MoveRef != 1000 {
			continue
		}
		n++
		if c.Command != "qcf_a" || int(c.ExecFrame) != len(motion)-1 || !hasPrefix(c.Inputs, motion) {
			t.Fatalf("assisted case must carry the command's motion ending at its exec frame: cmd=%q exec=%d inputs=%v", c.Command, c.ExecFrame, c.Inputs[:min(len(c.Inputs), 6)])
		}
		for _, b := range c.Inputs[c.ExecFrame+1:] {
			if b != 0 {
				t.Fatal("after the exec frame an assisted case holds neutral input")
			}
		}
	}
	if n == 0 {
		t.Fatal("no assisted special was recorded")
	}

	// Played back, the synthesized motion produces the special.
	plays, fails := int32(0), int32(0)
	for r := 0; r < 4; r++ {
		s := newSim(3600)
		s.cmdLayer = true
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		runRound(s, eng, [2]controller{nil, &defender{rng: rand.New(rand.NewSource(int64(70 + r)))}}, 3600)
	}
	for _, c := range st.Cases {
		if c.MoveRef == 1000 && c.Kind == KindCommand {
			plays += c.Outcome.Plays
			fails += c.Outcome.ExecFails
		}
	}
	if plays == 0 {
		t.Fatal("the driver never played an assisted special")
	}
	if fails*4 > plays {
		t.Fatalf("synthesized motions should reproduce the special: %d of %d plays failed", fails, plays)
	}
}

func TestChangeStateAIIsDiscarded(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcAuthoredAI, 2, 20, func(r *rand.Rand) controller { return &stateChanger{rng: r} })
	st, _ := eng.Store("Rush")
	if !st.ChangeStateAI {
		t.Fatal("attack starts with no command must flag the AI")
	}
	if st.Len() != 0 {
		t.Fatalf("a flagged source's cases are discarded for the match, got %d", st.Len())
	}
	if h := st.Hypotheses[1000]; h == nil || h.Seen == 0 {
		t.Fatal("the state entered directly is kept as a hypothesis")
	}
	if !eng.ChangeStateAI("Rush") {
		t.Fatal("the flag is visible to the adapter")
	}
}

func TestHumanCommandsResolveHypotheses(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	// Human play shows which command leads to the special.
	recordWith(t, eng, SrcHumanLocal, 3, 30, func(r *rand.Rand) controller { return &rushdown{rng: r} })
	st, _ := eng.Store("Rush")
	if name, ok := st.hypothesisCommand(1000, simMotions, 3, nil); !ok || name != "qcf_a" {
		t.Fatalf("the command map should resolve the special to its motion command, got %q", name)
	}
	before := st.Len()
	// ChangeState AI entering the special now yields synthesized cases.
	recordWith(t, eng, SrcAuthoredAI, 2, 40, func(r *rand.Rand) controller { return &stateChanger{rng: r} })
	var hyp []*Case
	for _, c := range st.Cases {
		if c.Source == SrcSynthesized {
			hyp = append(hyp, c)
		}
	}
	if len(hyp) == 0 || len(hyp) > DefaultParams().HypothesisCasesPerState {
		t.Fatalf("expected 1..%d hypothesis cases, got %d (store %d -> %d)", DefaultParams().HypothesisCasesPerState, len(hyp), before, st.Len())
	}
	if st.Len() != before+len(hyp) {
		t.Fatal("only hypotheses are kept from a flagged source")
	}
	// A hypothesis case's inputs reproduce the state from neutral.
	c := hyp[0]
	if c.Command != "qcf_a" || !hasPrefix(c.Inputs, simMotions["qcf_a"].Inputs) {
		t.Fatalf("hypothesis case must carry the resolved motion: %q %v", c.Command, c.Inputs)
	}
	s := newSim(3600)
	s.cmdLayer = true
	for _, b := range c.Inputs {
		s.step([2]InputBits{b, 0})
	}
	if s.ch[0].state != 1000 {
		t.Fatalf("hypothesis inputs should enter 1000, got %d", s.ch[0].state)
	}
}

func TestCheatOnlyClassification(t *testing.T) {
	p := DefaultParams()
	st := NewStore("x")
	c := &Case{ID: 1, Source: SrcSynthesized, Kind: KindCommand, MoveRef: 1234, Inputs: []InputBits{InA}, Start: Feat{RoundState: 2}, StartStateType: StStand}
	st.Admit(p, []*Case{c})
	for i := int32(0); i < p.HypothesisMaxAttempts; i++ {
		p.resolveHypothesisPlay(st, c, false)
	}
	h := st.Hypotheses[1234]
	if !h.CheatOnly || !c.Removed {
		t.Fatal("a hypothesis that never comes out is classified cheat-only and its cases removed")
	}
	st.compact()
	if st.Len() != 0 {
		t.Fatal("removed hypothesis cases leave the store at the next compaction")
	}
}

func TestAutomaticTransitionIsNotACancel(t *testing.T) {
	count := func(cmdLayer bool) (part1, part2 int) {
		eng := New(DefaultParams(), t.TempDir(), 1)
		eng.SetMotions("Rush", simMotions)
		for r := 0; r < 3; r++ {
			s := newSim(3600)
			s.cmdLayer = cmdLayer
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Record(0, SrcHumanLocal)
			runRound(s, eng, [2]controller{&twoPart{rng: rand.New(rand.NewSource(int64(r)))}, &defender{rng: rand.New(rand.NewSource(int64(90 + r)))}}, 3600)
		}
		st, _ := eng.Store("Rush")
		for _, c := range st.Cases {
			if c.Kind != KindCommand {
				continue
			}
			switch c.MoveRef {
			case 1100:
				part1++
			case 1101:
				part2++
			}
		}
		return
	}
	p1, p2 := count(true)
	if p1 == 0 {
		t.Fatal("the two-part special was not recorded")
	}
	if p2 != 0 {
		t.Fatalf("with command activity known, the move's own continuation is not a cancel: %d cases", p2)
	}
	if _, p2 := count(false); p2 == 0 {
		t.Fatal("without command activity the continuation is indistinguishable from a cancel (control)")
	}
}

func TestCommandResolutionPrefersSpecificOnTies(t *testing.T) {
	st := NewStore("x")
	for i := 0; i < 5; i++ {
		st.noteCommand(1000, []string{"a", "qcf_a"})
		st.noteCommand(200, []string{"a"})
	}
	if n, _ := st.commandFor(1000, []string{"a", "qcf_a"}, simMotions, true, nil); n != "qcf_a" {
		t.Fatalf("a motion and its button appear together; the motion wins: %q", n)
	}
	if n, _ := st.commandFor(200, []string{"a", "qcf_a"}, simMotions, true, nil); n != "a" {
		t.Fatalf("a state seen only with the button resolves to the button: %q", n)
	}
	// With no history, the most specific active command is taken.
	if n, _ := st.commandFor(3000, []string{"c", "qcf_c"}, simMotions, true, nil); n != "qcf_c" {
		t.Fatalf("no history: most specific, got %q", n)
	}
}

func TestHumanPrecedenceWithinBand(t *testing.T) {
	p := DefaultParams()
	p.ValueWeight, p.CaseReuseWeight = 0, 0
	st := NewStore("x")
	start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 60}}
	mk := func(id uint64, src Source, relx float32) *Case {
		f := start
		f.Enemy.RelX = relx
		return &Case{ID: id, Source: src, Kind: KindMovement, ExecFrame: -1, Inputs: []InputBits{InF}, Start: f, StartStateType: StStand, StartCtrl: true}
	}
	engineCase := mk(1, SrcEngineAI, 60) // exact match
	humanCase := mk(2, SrcHumanLocal, 75)
	st.Admit(p, []*Case{engineCase, humanCase})
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	self := &CharSnap{StateType: StStand, Ctrl: true}
	if c, _ := d.choose(&start, self, 0); c != humanCase {
		t.Fatal("a human case within the band is preferred over a closer engine case")
	}
	p.HumanPrecedence = 0
	if c, _ := d.choose(&start, self, 0); c != engineCase {
		t.Fatal("without precedence the closer case wins")
	}
}
