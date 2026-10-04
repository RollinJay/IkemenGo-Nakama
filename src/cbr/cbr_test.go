package cbr

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInputFacingRoundTrip(t *testing.T) {
	for facing := 0; facing < 2; facing++ {
		fr := facing == 1
		for mask := 0; mask < 1<<14; mask += 37 {
			var btn EngineButtons
			for i := 0; i < 14; i++ {
				btn[i] = mask&(1<<i) != 0
			}
			if got := ToEngine(FromEngine(btn, fr), fr); got != btn {
				t.Fatalf("facingRight=%v mask=%b: round trip %v != %v", fr, mask, got, btn)
			}
		}
	}
	// Holding toward the opponent is forward regardless of side.
	var right EngineButtons
	right[3] = true
	if FromEngine(right, true)&InF == 0 || FromEngine(right, false)&InB == 0 {
		t.Fatal("right should be forward facing right and back facing left")
	}
	if (InD|InF).Dir() != 3 || (InU|InB).Dir() != 7 || InputBits(0).Dir() != 5 {
		t.Fatal("numpad directions wrong")
	}
}

// recordRounds records player 0 (rushdown) against the defender.
func recordRounds(t *testing.T, eng *Engine, rounds int, seed int64) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		rng := rand.New(rand.NewSource(seed + int64(r)))
		if err := eng.SetPlayer(0, "Rush", "Guard"); err != nil {
			t.Fatal(err)
		}
		if err := eng.SetPlayer(1, "Guard", "Rush"); err != nil {
			t.Fatal(err)
		}
		if err := eng.Record(0, SrcHumanLocal); err != nil {
			t.Fatal(err)
		}
		runRound(s, eng, [2]controller{&rushdown{rng: rng}, &defender{rng: rng}}, 3600)
	}
}

func TestRecordingProducesGradedCases(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	recordRounds(t, eng, 3, 10)
	st, _ := eng.Store("Rush")
	if st.Len() == 0 {
		t.Fatal("no cases recorded")
	}
	kinds := map[CaseKind]int{}
	results := map[CompletionKind]int{}
	moves := map[int32]int{}
	var special *Case
	for _, c := range st.Cases {
		kinds[c.Kind]++
		results[c.Completion.Result]++
		if c.Kind == KindCommand {
			moves[c.MoveRef]++
			if c.ExecFrame < 0 || int(c.ExecFrame) >= len(c.Inputs) {
				t.Fatalf("command case %d: exec frame %d outside %d inputs", c.ID, c.ExecFrame, len(c.Inputs))
			}
			if c.MoveRef == 1000 && special == nil {
				special = c
			}
		}
		if !c.Completion.Resolved {
			t.Fatalf("case %d unresolved", c.ID)
		}
	}
	t.Logf("cases=%d kinds=%v results=%v moves=%v", st.Len(), kinds, results, moves)
	if kinds[KindCommand] == 0 || kinds[KindMovement] == 0 {
		t.Fatalf("expected both command and movement cases: %v", kinds)
	}
	if moves[200] == 0 {
		t.Fatal("no light-punch cases")
	}
	if special == nil {
		t.Fatal("no special-move case: motion back-dating or cancel detection failed")
	}
	// The special's case must contain its whole motion up to the press, so
	// replaying from the case start reproduces it.
	var dirs []uint8
	for _, b := range special.Inputs[:special.ExecFrame+1] {
		if d := b.Dir(); len(dirs) == 0 || dirs[len(dirs)-1] != d {
			dirs = append(dirs, d)
		}
	}
	if !containsSeq(dirs, []uint8{2, 3, 6}) {
		t.Fatalf("special case inputs lack the 236 motion: %v", dirs)
	}
	if special.Inputs[special.ExecFrame]&InA == 0 {
		t.Fatal("special case lacks the button at its exec frame")
	}
	if results[CompKnockdown] == 0 {
		t.Fatalf("no knockdown completions recorded: %v", results)
	}
}

func containsSeq(have, want []uint8) bool {
	k := 0
	for _, d := range have {
		if d == want[k] {
			k++
			if k == len(want) {
				return true
			}
		}
	}
	return false
}

func TestKnockdownComboIsValuedAboveDroppedPoke(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	recordRounds(t, eng, 4, 20)
	st, _ := eng.Store("Rush")
	p := eng.Params()
	var kd, other []float32
	for _, c := range st.Cases {
		if c.Kind != KindCommand || c.Outcome.Trials == 0 {
			continue
		}
		v := c.Outcome.ValueSum / c.Outcome.Trials
		if c.MoveRef == 1000 && c.Completion.Result == CompKnockdown {
			kd = append(kd, v)
		} else if c.Completion.DamageDealt == 0 {
			other = append(other, v)
		}
	}
	if len(kd) == 0 || len(other) == 0 {
		t.Fatalf("need both groups: knockdown=%d nodamage=%d", len(kd), len(other))
	}
	if mean(kd) <= mean(other) {
		t.Fatalf("knockdown specials (%.3f) should outvalue no-damage commands (%.3f)", mean(kd), mean(other))
	}
	_ = p
}

func mean(v []float32) float32 {
	var s float32
	for _, x := range v {
		s += x
	}
	return s / float32(len(v))
}

func TestRetrievalFindsOriginatingCase(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	recordRounds(t, eng, 2, 30)
	st, _ := eng.Store("Rush")
	p := eng.Params()
	for _, c := range st.Cases[:min(len(st.Cases), 200)] {
		if d := p.distance(&c.Start, &c.Start); d != 0 {
			t.Fatalf("case %d: self distance %v", c.ID, d)
		}
	}
	// With value, exploration and reuse switched off, selection is pure
	// similarity: choosing from a recorded case's own start state must find
	// a case at distance zero (itself or an identical one).
	pure := *p
	pure.ValueWeight, pure.ExplorationWeight, pure.CaseReuseWeight = 0, 0, 0
	ct := newChainTracker(&pure, 0, new(uint64))
	drv := newDriver(&pure, 0, Levels{8, 8, 8}, 1, st, ct)
	found := 0
	for _, c := range st.Cases {
		// A whiffed attack is legitimately filtered by the reach model from
		// its own start state; test the cases that connected.
		if c.Kind != KindCommand || !c.StartCtrl || (c.ExecAttack && c.Reach.Contact == 0) {
			continue
		}
		self := &CharSnap{StateType: c.StartStateType, Ctrl: c.StartCtrl, MoveType: c.StartMoveType, StateNo: c.StartStateNo, StateTime: c.Start.Self.StateTime}
		got, dist := drv.choose(&c.Start, self, 1<<40)
		if got == nil {
			t.Fatalf("case %d: nothing chosen from its own start state", c.ID)
		}
		if dist != 0 {
			t.Fatalf("case %d: chose case %d at distance %v, want 0", c.ID, got.ID, dist)
		}
		found++
		if found > 50 {
			break
		}
	}
	if found == 0 {
		t.Fatal("no command cases to test")
	}
	// With the full score, value may prefer a near neighbour, but never one
	// beyond the confidence threshold.
	full := newDriver(p, 0, Levels{8, 8, 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	for _, c := range st.Cases[:min(len(st.Cases), 100)] {
		self := &CharSnap{StateType: c.StartStateType, Ctrl: c.StartCtrl, MoveType: c.StartMoveType, StateNo: c.StartStateNo, StateTime: c.Start.Self.StateTime}
		if got, dist := full.choose(&c.Start, self, 1<<40); got != nil && dist > p.ConfidenceThreshold {
			t.Fatalf("chose a case at distance %v past the threshold", dist)
		}
	}
}

func TestDrivenPlayerPlaysAndLearns(t *testing.T) {
	dir := t.TempDir()
	eng := New(DefaultParams(), dir, 7)
	recordRounds(t, eng, 6, 40)
	st, _ := eng.Store("Rush")
	before := st.total

	var dealt int32
	driven := 0
	for r := 0; r < 4; r++ {
		s := newSim(3600)
		rng := rand.New(rand.NewSource(int64(900 + r)))
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		// Count frames the CBR layer drove.
		ctl := [2]controller{nil, &defender{rng: rng}}
		frames := 0
		eng.Frame(s.snapshot())
		for f := 0; f < 3600 && s.roundSt == 2; f++ {
			var in [2]InputBits
			s.ch[0].cbr = false
			if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
				in[0] = FromEngine(btn, s.ch[0].facingRight)
				s.ch[0].cbr = true
				frames++
			}
			in[1] = ctl[1].next(s, 1)
			s.step(in)
			eng.Frame(s.snapshot())
		}
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		eng.RoundEnd(s.snapshot())
		dealt += 1000 - s.ch[1].life
		driven += frames
	}
	t.Logf("driven frames=%d damage dealt=%d trials %.1f -> %.1f", driven, dealt, before, st.total)
	if driven == 0 {
		t.Fatal("CBR never drove the player")
	}
	if dealt == 0 {
		t.Fatal("driven player dealt no damage")
	}
	if st.total <= before {
		t.Fatal("playing cases did not add trials")
	}
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LocalPath(dir, "Rush")); err != nil {
		t.Fatal("store not saved:", err)
	}
}

func TestExecutionNoiseScalesWithLevel(t *testing.T) {
	p := DefaultParams()
	if p.noiseScale(8) != 0 || p.noiseScale(1) != p.NoiseScaleMax {
		t.Fatalf("noise endpoints wrong: %v %v", p.noiseScale(8), p.noiseScale(1))
	}
	for l := 2; l <= 8; l++ {
		if p.noiseScale(l) >= p.noiseScale(l-1) {
			t.Fatalf("noise not decreasing at %d", l)
		}
	}
	c := &Case{Kind: KindCommand, ExecFrame: 0}
	for i := 0; i < 60; i++ {
		var b InputBits
		if i%6 == 0 {
			b = InA
		}
		c.Inputs = append(c.Inputs, b)
	}
	st := NewStore("x")
	ct := newChainTracker(p, 0, new(uint64))
	hi := newDriver(p, 0, Levels{Execution: 8}, 1, st, ct)
	if out, pert := hi.schedule(c); pert || !reflect.DeepEqual(out, c.Inputs) {
		t.Fatal("execution 8 must replay canonical timing")
	}
	lo := newDriver(p, 0, Levels{Execution: 1}, 1, st, ct)
	shifted := 0
	for i := 0; i < 20; i++ {
		if _, pert := lo.schedule(c); pert {
			shifted++
		}
	}
	if shifted == 0 {
		t.Fatal("execution 1 never perturbed timing")
	}
	// The first press is the decision itself and never moves.
	for i := 0; i < 20; i++ {
		out, _ := lo.schedule(c)
		if out[0]&InA == 0 {
			t.Fatal("decision press moved")
		}
	}
}

func TestCompletionProbabilityAndAmbition(t *testing.T) {
	p := DefaultParams()
	links := []LinkStat{{Attempts: 10, Successes: 6}, {Attempts: 10, Successes: 9}}
	if p.completionProb(links, 8) != 1 {
		t.Fatal("execution 8 should always complete")
	}
	hiProb, loProb := p.completionProb(links, 6), p.completionProb(links, 1)
	if !(hiProb > loProb && loProb > 0) {
		t.Fatalf("completion should fall with execution: 6=%v 1=%v", hiProb, loProb)
	}
	if ambitionExponent(8) != 1 || ambitionExponent(1) <= ambitionExponent(4) {
		t.Fatal("ambition exponent should rise as ambition falls")
	}
}

func TestPerturbedDropsDoNotLowerValue(t *testing.T) {
	p := DefaultParams()
	eng := New(p, t.TempDir(), 1)
	eng.SetPlayer(0, "A", "B")
	ps := eng.players[0]
	c := &Case{ID: 99, Source: SrcHumanLocal, Kind: KindCommand, Inputs: []InputBits{InA}, StartStateType: StStand}
	ps.st.Admit(p, []*Case{c})
	before := c.Value(p)
	rc := &resolvedChain{
		ch:   &chain{cases: []chainCase{{caseID: 99, perturbed: true, level: 2}}},
		comp: Completion{Result: CompDropped, Quality: p.quality(CompDropped), DamageDealt: 0.05, DamageTaken: 0.3},
	}
	eng.finalizeChain(ps, rc)
	if c.Value(p) != before {
		t.Fatalf("perturbed drop changed value %v -> %v", before, c.Value(p))
	}
	if c.Outcome.ByLevel[2].Drops != 1 {
		t.Fatal("perturbed drop not recorded for calibration")
	}
	rc.ch.cases[0].perturbed = false
	eng.finalizeChain(ps, rc)
	if c.Value(p) >= before {
		t.Fatal("an unperturbed drop should lower value")
	}
}

func TestKnowledgeGatingIsSubtractive(t *testing.T) {
	p := DefaultParams()
	st := NewStore("x")
	var cases []*Case
	for i := 0; i < 100; i++ {
		c := &Case{ID: uint64(i + 1), Source: SrcHumanLocal, Kind: KindMovement, Inputs: []InputBits{0}, StartStateType: StStand, StartCtrl: true}
		c.Outcome.Trials = float32(i)
		cases = append(cases, c)
	}
	st.Admit(p, cases)
	lo, hi := st.evidenceThreshold(p, 1), st.evidenceThreshold(p, 8)
	if hi != -1 {
		t.Fatal("knowledge 8 must see everything")
	}
	visible := 0
	for _, c := range cases {
		if c.Evidence(p) >= lo {
			visible++
		}
	}
	want := int(p.knowledgeFrac(1) * 100)
	if visible < want-2 || visible > want+2 {
		t.Fatalf("knowledge 1 sees %d of 100, want about %d", visible, want)
	}
}

func TestTechniqueGates(t *testing.T) {
	p := DefaultParams()
	if p.gateOpen(TechReset, Levels{Execution: 8, Knowledge: 7}) {
		t.Fatal("reset must need knowledge 8")
	}
	if p.gateOpen(TechReset, Levels{Execution: 7, Knowledge: 8}) {
		t.Fatal("reset must need execution 8")
	}
	if !p.gateOpen(TechReset|TechOTGRelaunch|TechLoop, Levels{Execution: 8, Knowledge: 8}) {
		t.Fatal("tier 9 must open the gates")
	}
	if p.gateOpen(TechOTGFinish, Levels{Execution: 8, Knowledge: 7}) {
		t.Fatal("OTGs in combos, finishers included, are top-tier only")
	}
	if !p.gateOpen(0, Levels{Execution: 1, Knowledge: 1}) {
		t.Fatal("ungated cases are open at every level")
	}
}

func TestReachInwardFallback(t *testing.T) {
	m := newReachModel()
	for i := 0; i < 5; i++ {
		m.Add(200, StStand, 40, 1)
		m.Add(200, StStand, 60, 1)
	}
	m.Add(200, StStand, 90, 0)
	if p, known := m.Connect(200, StStand, 40, 3); !known || p != 1 {
		t.Fatalf("close range: %v %v", p, known)
	}
	if p, known := m.Connect(200, StStand, 150, 1); !known || p != 0 {
		t.Fatalf("beyond farthest connection must be 0: %v", p)
	}
	if _, known := m.Connect(999, StStand, 10, 1); known {
		t.Fatal("unknown move must not filter")
	}
	m.SetGeometricMax(200, StStand, 50)
	if p, _ := m.Connect(200, StStand, 60, 1); p != 0 {
		t.Fatal("geometry bound must cap reach")
	}
}

func TestTerminalSharesOnlyReachLateRound(t *testing.T) {
	p := DefaultParams()
	if sh := p.terminalShares([]*resolvedChain{{ch: &chain{timeFrac0: 0.05}}}, true, false); sh[0] != 0 {
		t.Fatal("terminal credit is off by default")
	}
	p.TerminalWeight = 0.3
	early := &resolvedChain{ch: &chain{timeFrac0: 0.9}}
	mid := &resolvedChain{ch: &chain{timeFrac0: 0.4}}
	late := &resolvedChain{ch: &chain{timeFrac0: 0.05}}
	sh := p.terminalShares([]*resolvedChain{early, mid, late}, true, false)
	if sh[0] != 0 {
		t.Fatalf("an early-round chain must get no terminal credit: %v", sh[0])
	}
	if !(sh[2] > 0 && sh[1] > 0 && sh[2] > sh[1]) {
		t.Fatalf("late-round chains get credit rising toward the end: %v", sh)
	}
	lost := p.terminalShares([]*resolvedChain{late}, false, true)
	if lost[0] >= 0 {
		t.Fatal("a loss must give negative credit")
	}
	p.ClockEnabled = false
	if off := p.terminalShares([]*resolvedChain{late}, true, false); off[0] != 0 {
		t.Fatal("clock off: no terminal credit")
	}
	inf := &resolvedChain{ch: &chain{timeFrac0: -1}}
	p.ClockEnabled = true
	if sh := p.terminalShares([]*resolvedChain{inf}, true, false); sh[0] != 0 {
		t.Fatal("infinite timer: no terminal credit")
	}
}

func TestStorePersistence(t *testing.T) {
	dir := t.TempDir()
	eng := New(DefaultParams(), dir, 1)
	recordRounds(t, eng, 2, 50)
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
	orig, _ := eng.Store("Rush")
	got, err := LoadStore(LocalPath(dir, "Rush"), "Rush")
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != orig.Len() {
		t.Fatalf("loaded %d cases, saved %d", got.Len(), orig.Len())
	}
	for _, c := range orig.Cases {
		g := got.Get(c.ID)
		if g == nil {
			t.Fatalf("case %d missing after load", c.ID)
		}
		if !reflect.DeepEqual(g.Inputs, c.Inputs) || g.Completion != c.Completion || g.MoveRef != c.MoveRef {
			t.Fatalf("case %d differs after load", c.ID)
		}
	}
	if len(got.Reach.Bins) != len(orig.Reach.Bins) || len(got.Routes) != len(orig.Routes) {
		t.Fatal("models differ after load")
	}
	// Corrupt data is refused rather than half-loaded.
	bad := filepath.Join(dir, "bad.cbr")
	os.WriteFile(bad, []byte("not gzip"), 0o644)
	if _, err := LoadStore(bad, "x"); err == nil {
		t.Fatal("corrupt store loaded")
	}
}

func TestAggregateExportMergeApply(t *testing.T) {
	dir := t.TempDir()
	eng := New(DefaultParams(), dir, 1)
	recordRounds(t, eng, 2, 60)
	st, _ := eng.Store("Rush")
	a := st.ExportAggregate(0)
	if len(a.Reach) == 0 {
		t.Fatal("aggregate has no reach data")
	}
	capped := st.ExportAggregate(10)
	if capped.Weight > 10.5 {
		t.Fatalf("contributor cap not applied: %v", capped.Weight)
	}
	merged := &Aggregate{}
	MergeAggregate(merged, a)
	MergeAggregate(merged, a)
	for k, bins := range a.Reach {
		for i := range bins {
			if merged.Reach[k][i].Hits != 2*bins[i].Hits {
				t.Fatal("merge does not sum counts")
			}
		}
	}
	path := GlobalPath(dir, "Fresh")
	if err := SaveAggregate(path, merged); err != nil {
		t.Fatal(err)
	}
	back, err := LoadAggregate(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewStore("Fresh")
	if err := fresh.ApplyGlobal(back); err != nil {
		t.Fatal(err)
	}
	// A store with no local data answers reach queries from the global layer.
	for k := range st.Reach.Bins {
		move, cls := int32(k>>3), StateType(k&7)
		if _, known := fresh.connect(move, cls, 30, 1); !known {
			t.Fatal("global layer not consulted")
		}
		break
	}
	// Exports never include the global layer.
	if len(fresh.ExportAggregate(0).Reach) != 0 {
		t.Fatal("global data leaked into a local export")
	}
}

func TestHorizonResolvesNonTerminatingChain(t *testing.T) {
	p := DefaultParams()
	p.HorizonFrames = 50
	ct := newChainTracker(p, 0, new(uint64))
	tr := newTracker(p, 0)
	var resolved []*resolvedChain
	ct.onFinal = func(rc *resolvedChain) { resolved = append(resolved, rc) }
	s := newSim(3600)
	// Keep the defender in hitstun forever: an exchange that never ends.
	for f := 0; f < 120; f++ {
		s.tick++
		s.ch[1].setState(5000, StStand, MtHit, false)
		s.ch[1].hitstun = 5
		s.ch[0].setState(200, StStand, MtAttack, false)
		snap := s.snapshot()
		tr.observe(snap)
		ct.step(snap, tr)
	}
	timeouts := 0
	for _, rc := range resolved {
		if rc.comp.Result == CompTimeout {
			timeouts++
		}
	}
	if timeouts == 0 {
		t.Fatalf("no timeout resolution among %d chains", len(resolved))
	}
}

func TestGuardingIsNotBeingHit(t *testing.T) {
	g := CharSnap{MoveType: MtHit, BlockStun: 8, StateType: StStand}
	if g.BeingHit() {
		t.Fatal("a guarding character must not count as hit")
	}
	h := CharSnap{MoveType: MtHit, HitStun: 8, StateType: StStand}
	if !h.BeingHit() {
		t.Fatal("hitstun must count as hit")
	}
}

func TestCBRDrivenFramesAreNotRecorded(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 3)
	recordRounds(t, eng, 3, 70)
	st, _ := eng.Store("Rush")
	n := st.Len()
	// Drive and record the same player: nothing new may be recorded.
	s := newSim(1200)
	rng := rand.New(rand.NewSource(5))
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	eng.Record(0, SrcHumanLocal)
	driven := 0
	eng.Frame(s.snapshot())
	for f := 0; f < 1200 && s.roundSt == 2; f++ {
		var in [2]InputBits
		s.ch[0].cbr = false
		if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
			in[0] = FromEngine(btn, s.ch[0].facingRight)
			s.ch[0].cbr = true
			driven++
		} else {
			in[0] = 0
		}
		in[1] = (&defender{rng: rng}).next(s, 1)
		s.step(in)
		eng.Frame(s.snapshot())
	}
	s.roundSt = 3
	eng.RoundEnd(s.snapshot())
	added := 0
	for _, c := range st.Cases[n:] {
		_ = c
		added++
	}
	if driven > 0 && added > 0 {
		// Only frames the layer declined may be recorded; those are
		// genuine (here: idle) input, not CBR output.
		for _, c := range st.Cases[n:] {
			if c.Kind == KindCommand {
				t.Fatalf("a command case was recorded from CBR-driven play (case %d)", c.ID)
			}
		}
	}
	t.Logf("driven=%d added=%d", driven, added)
}

func TestKnockoutChainIsGradedAsRoundEnd(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	s := newSim(20000) // long enough to end by knockout
	rng := rand.New(rand.NewSource(10))
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Record(0, SrcHumanLocal)
	runRound(s, eng, [2]controller{&rushdown{rng: rng}, &defender{rng: rng}}, 20000)
	if s.ch[1].life > 0 {
		t.Fatal("setup: the round should end by knockout")
	}
	st, _ := eng.Store("Rush")
	n := 0
	for _, c := range st.Cases {
		if c.Completion.Result == CompRoundEnd {
			n++
			if c.Completion.DamageDealt <= 0 {
				t.Fatal("the knockout chain must bank its damage, including the last hit")
			}
		}
	}
	if n == 0 {
		t.Fatal("a round won by knockout must grade its last chain as a round end")
	}
}

func TestDamageIsMeasuredOnEveryOpponent(t *testing.T) {
	ct := newChainTracker(DefaultParams(), 0, new(uint64))
	snap := func(l1, l2 int32) *Snapshot {
		return &Snapshot{World: WorldSnap{RoundState: 2}, Players: []PlayerSnap{
			{Present: true, Root: CharSnap{ID: 1, TeamSide: 0, Life: 1000, LifeMax: 1000, Alive: true}},
			{Present: true, Root: CharSnap{ID: 2, TeamSide: 1, Life: l1, LifeMax: 1000, Alive: l1 > 0, Pos: [3]float32{50}}},
			{Present: true, Root: CharSnap{ID: 3, TeamSide: 1, Life: l2, LifeMax: 1000, Alive: l2 > 0, Pos: [3]float32{200}}},
			{Present: true, Root: CharSnap{ID: 4, TeamSide: -1, Life: 500, LifeMax: 1000, Alive: true}},
		}}
	}
	ct.opponentDamage(snap(1000, 300))
	if d, ko := ct.opponentDamage(snap(1000, 300)); d != 0 || ko {
		t.Fatalf("no life lost, no damage: %v %v", d, ko)
	}
	if d, ko := ct.opponentDamage(snap(900, 0)); d < 0.39 || d > 0.41 || !ko {
		t.Fatalf("damage on both opponents and the knockout count: %v %v", d, ko)
	}
	if s := snap(900, 0); s.Enemy(0) != 1 || len(s.Opponents(0)) != 2 {
		t.Fatal("a character with no team side is nobody's opponent")
	}
}

func TestStoreFileNamesAreDistinct(t *testing.T) {
	a, b := fileName("chars/kfm/kfm.def"), fileName("chars/kfm2/kfm.def")
	if a == b {
		t.Fatal("characters with the same base name need distinct files")
	}
	if x, y := fileName("春麗"), fileName("豪鬼"); x == y {
		t.Fatal("names outside ASCII must not collide")
	}
	if got := fileName("chars/kfm/kfm.def"); got[:4] != "kfm-" {
		t.Fatalf("readable part should be the definition's base name: %q", got)
	}
}
