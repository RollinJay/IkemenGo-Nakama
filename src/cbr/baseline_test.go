package cbr

import (
	"math"
	"math/rand"
	"testing"
)

// Tests of the base AI (baseline.go): the character's own AI plays unless
// the CBR layer has a move that proved better than it.

// withoutBaseAI returns the default parameters with the base AI off: the CBR
// layer plays whatever it matches, which tests of playback itself need.
func withoutBaseAI() *Params {
	p := DefaultParams()
	p.BaseAI = false
	return p
}

var (
	basePoker    oppFactory = func(r *rand.Rand) controller { return &poker{rng: r} }
	baseRushdown oppFactory = func(r *rand.Rand) controller { return &rushdown{rng: r} }
)

func result(s *sim) roundResult {
	res := roundResult{dealt: 1000 - s.ch[1].life, net: s.ch[0].life - s.ch[1].life}
	switch {
	case s.ch[1].life < s.ch[0].life:
		res.outcome = 1
	case s.ch[1].life > s.ch[0].life:
		res.outcome = -1
	}
	return res
}

// baseOnly plays n rounds of base alone against opp, on the seeds overBase
// uses.
func baseOnly(n, seedBase int, base, opp oppFactory) []roundResult {
	var out []roundResult
	for r := 0; r < n; r++ {
		s := newSim(3600)
		runRound(s, nil, [2]controller{base(rand.New(rand.NewSource(int64(seedBase + 500 + r)))), opp(rand.New(rand.NewSource(int64(seedBase + r))))}, 3600)
		out = append(out, result(s))
	}
	return out
}

// decisions counts the decisions credited in resolved chains: the CBR
// layer's plays and the base AI's probes, in all and by coarse situation
// (see coarseKey).
type decisions struct {
	plays, probes int
	by            map[uint64][2]int
}

// share is the CBR layer's part of the decisions, in all or (with key) in
// one coarse situation.
func (d *decisions) share(key ...uint64) float32 {
	n := [2]int{d.plays, d.probes}
	if len(key) > 0 {
		n = d.by[key[0]]
	}
	if n[0]+n[1] == 0 {
		return 0
	}
	return float32(n[0]) / float32(n[0]+n[1])
}

// overBase plays n rounds with player 0 driven by the CBR layer over base,
// which plays whenever the CBR layer does not take over, and counts the
// decisions.
func overBase(eng *Engine, n, seedBase int, lv Levels, base, opp oppFactory) (out []roundResult, dec *decisions) {
	dec = &decisions{by: map[uint64][2]int{}}
	for r := 0; r < n; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, lv)
		ps := eng.players[0]
		final := ps.ct.onFinal
		ps.ct.onFinal = func(rc *resolvedChain) {
			for _, cc := range rc.ch.cases {
				switch {
				case cc.probe:
					dec.probes++
					v := dec.by[coarseKey(cc.key)]
					v[1]++
					dec.by[coarseKey(cc.key)] = v
				case cc.caseID != 0:
					dec.plays++
					if c := ps.st.Get(cc.caseID); c != nil {
						k := coarseKey(situationOf(actionKey(c), s.ch[0].ai))
						v := dec.by[k]
						v[0]++
						dec.by[k] = v
					}
				}
			}
			final(rc)
		}
		runRoundBase(s, eng, [2]controller{nil, opp(rand.New(rand.NewSource(int64(seedBase + r))))},
			[2]controller{base(rand.New(rand.NewSource(int64(seedBase + 500 + r))))}, 3600)
		out = append(out, result(s))
	}
	return out, dec
}

// recordAs records player 0 played by ctl against the defender as a
// person's play.
func recordAs(t *testing.T, eng *Engine, rounds int, seed int64, ctl oppFactory) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		if err := eng.Record(0, SrcHumanLocal); err != nil {
			t.Fatal(err)
		}
		runRound(s, eng, [2]controller{ctl(rand.New(rand.NewSource(seed + int64(r)))), oppDefender(rand.New(rand.NewSource(seed + 77 + int64(r))))}, 3600)
	}
}

// comboSit is the coarse situation of a follow-up: mid-action, with the
// enemy in hitstun, at AI level 0 (the simulator's).
var comboSit = coarseKey(baseFine | 2<<8)

// TestTakeoverWhenBetter: the base AI is a weak poker that never follows a
// jab up, and the store learned from a player who cancels a jab that hits
// into the knockdown special. After learning, the CBR layer takes over with
// the cancel, which proved better than the poker, and the result is clearly
// better than the poker's alone. It takes over on a large share of the
// follow-up decisions (the rest are hits at a point in the jab no recorded
// cancel starts from), and its neutral play, which proved no better than
// the poker's, leaves the poker in control there.
func TestTakeoverWhenBetter(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	eng := New(DefaultParams(), t.TempDir(), 7)
	recordAs(t, eng, 10, 40, baseRushdown)
	baseNet, _, _ := summarize(baseOnly(6, 7000, basePoker, oppDefender))
	before, bd := overBase(eng, 6, 7000, lv, basePoker, oppDefender)
	overBase(eng, 30, 9000, lv, basePoker, oppDefender)
	after, ad := overBase(eng, 6, 7000, lv, basePoker, oppDefender)
	bn, _, _ := summarize(before)
	an, _, _ := summarize(after)
	t.Logf("net per round: poker alone %.0f; with the CBR layer before learning %.0f, after %.0f", baseNet, bn, an)
	t.Logf("CBR layer's share of decisions before learning %.1f%% (follow-ups %.0f%%), after %.1f%% (follow-ups %.0f%%)",
		100*bd.share(), 100*bd.share(comboSit), 100*ad.share(), 100*ad.share(comboSit))
	if an < baseNet+150 {
		t.Fatalf("taking over with what proved better should beat the base AI clearly: %.0f vs %.0f", an, baseNet)
	}
	if ad.share(comboSit) < 0.3 || ad.share(comboSit) < bd.share(comboSit)+0.2 {
		t.Fatalf("the CBR layer should take over on many more follow-ups after learning: %.0f%% from %.0f%%",
			100*ad.share(comboSit), 100*bd.share(comboSit))
	}
	// The cancel proved itself: the CBR layer's own plays of it beat the
	// base AI's record in that situation by more than the margin.
	st, _ := eng.Store("Rush")
	p := eng.Params()
	found := false
	for k, a := range st.Actions {
		cancel := int32(uint32(k>>16)) == 1000 && k&(1<<12) == 0 && k&(1<<11) == 0
		if !cancel || a.PTrials < p.decayed(p.TakeoverMinPlays)-1e-3 {
			continue
		}
		found = true
		b, _ := st.baseline(p, situationOf(k, 0))
		t.Logf("%s: %.1f plays, mean %.3f against the base AI's %.3f", ActionString(k), a.PTrials, a.PSum/a.PTrials, b)
		if a.PSum/a.PTrials < b+p.TakeoverMargin {
			t.Fatalf("the cancel's plays (%.3f) should beat the base AI (%.3f)", a.PSum/a.PTrials, b)
		}
	}
	if !found {
		t.Fatal("the cancel into the special was never proved")
	}
}

// situationOf is the baseline situation an action key describes, for a base
// AI at the given AI level.
func situationOf(action uint64, aiLevel float32) uint64 {
	const fields = 0xf | 7<<4 | 3<<8 | 1<<11 // band, enemy state, context, control
	return baseFine | action&fields | aiBand(aiLevel)<<13
}

// TestBaseKeptWhenBetter: the base AI is the player who cancels, and the
// store learned from the weak poker. Nothing the store holds beats the base
// AI, so the CBR layer rarely takes over, and the result is no worse than
// the base AI's alone.
func TestBaseKeptWhenBetter(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	eng := New(DefaultParams(), t.TempDir(), 7)
	recordAs(t, eng, 10, 40, basePoker)
	baseNet, _, _ := summarize(baseOnly(6, 7000, baseRushdown, oppDefender))
	overBase(eng, 12, 9000, lv, baseRushdown, oppDefender)
	after, ad := overBase(eng, 6, 7000, lv, baseRushdown, oppDefender)
	an, _, _ := summarize(after)
	t.Logf("net per round: base AI alone %.0f, with the CBR layer after learning %.0f (%.1f%% of decisions, follow-ups %.0f%%)",
		baseNet, an, 100*ad.share(), 100*ad.share(comboSit))
	if ad.share() > 0.05 {
		t.Fatalf("the CBR layer took over too often from a better base AI: %.1f%% of decisions", 100*ad.share())
	}
	if an < baseNet-30 {
		t.Fatalf("the CBR layer made a better base AI worse: %.0f vs %.0f", an, baseNet)
	}
}

// gateFixture is a store holding one jab case that matches the current
// state exactly, with a demonstrated result of 0.2, and a driver over it at
// AI level 8.
func gateFixture(p *Params) (d *driver, c *Case, cur *Feat, self *CharSnap, key uint64) {
	st := NewStore("x")
	f := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 60, State: StStand}}
	c = &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InA, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: f}
	c.Outcome = Outcome{Trials: 1, ValueSum: 0.2}
	st.Admit(p, []*Case{c})
	d = newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	self = &CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true, AILevel: 8}
	return d, c, &f, self, baselineKey(&f, true, 8)
}

// setPlayed gives the case's action n plays by the CBR layer with the given
// mean result.
func setPlayed(p *Params, st *Store, c *Case, n, mean float32) {
	a := st.action(c)
	a.PTrials = p.decayed(n)
	a.PSum = a.PTrials * mean
}

// setBase gives a situation, its coarser one and the store n results of the
// base AI with the given mean.
func setBase(st *Store, key uint64, n, mean float32) {
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		st.Baseline[k] = &BaselineStat{Trials: n, ValueSum: n * mean}
	}
}

func near(a, b float32) bool { return math.Abs(float64(a-b)) < 1e-4 }

// TestGateDecisions checks the comparison with the base AI on hand-built
// records.
func TestGateDecisions(t *testing.T) {
	p := DefaultParams()
	choose := func(d *driver, cur *Feat, self *CharSnap, g *gate) (*Case, map[string]int) {
		why := map[string]int{}
		c, _ := d.chooseWhy(cur, self, 1, why, g)
		return c, why
	}

	d, c, cur, self, key := gateFixture(p)
	st := d.st
	// Without the gate (the base AI off, or a direct choice) the case plays.
	if got, _ := d.choose(cur, self, 1); got != c {
		t.Fatal("setup: the case should match")
	}
	// Never played: it has proved nothing.
	g := &gate{key: key}
	if got, why := choose(d, cur, self, g); got != nil || !g.held || why[RejNotBetter] != 1 {
		t.Fatalf("an unplayed move must not take over: %v held=%v why=%v", got, g.held, why)
	}
	// Three plays averaging 0.1 against no record of the base AI: the base
	// AI counts as BaselineDefault (0), and with no record of it the margin
	// doubles. The advantage, 3*0.1/(3+3) less decay, is 0.05: under 0.06.
	setPlayed(p, st, c, 3, 0.1)
	if b, n := st.baseline(p, key); b != p.BaselineDefault || n != 0 {
		t.Fatalf("no record: the base AI's mean is the default, got %v over %v", b, n)
	}
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("with no record of the base AI the margin doubles")
	}
	// Ten results of the base AI averaging 0: the margin is the setting's.
	setBase(st, key, 10, 0)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != c {
		t.Fatal("a move that proved better by the margin takes over")
	}
	// The margin settings: Low 0.015 and Medium 0.03 pass 0.05, High 0.06
	// does not.
	for lvl, want := range map[int]bool{1: true, 2: true, 3: false, 0: true} {
		d.margin = p.takeoverMargin(lvl)
		if got, _ := choose(d, cur, self, &gate{key: key}); (got == c) != want {
			t.Fatalf("margin level %d (%.3f): took over %v, want %v", lvl, d.margin, got == c, want)
		}
	}
	d.margin = p.TakeoverMargin
	// A thin record of the base AI (under BaselineMinTrials) doubles the
	// margin again.
	setBase(st, key, 4, 0)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("a thin record of the base AI doubles the margin")
	}
	setBase(st, key, 5, 0)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != c {
		t.Fatal("BaselineMinTrials results are enough")
	}
	// Two plays prove nothing, however good.
	setPlayed(p, st, c, 2, 1)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("a move needs TakeoverMinPlays plays")
	}
	// A base AI as good as the move keeps control.
	setPlayed(p, st, c, 3, 0.1)
	setBase(st, key, 50, 0.1)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("a move no better than the base AI must not take over")
	}

	// Shrinkage: a situation's mean is pulled toward its coarser record,
	// that toward the store's, and that toward the default.
	st.Baseline = map[uint64]*BaselineStat{key: {Trials: 5, ValueSum: 1}}
	if b, n := st.baseline(p, key); !near(b, 1.0/10) || n != 5 {
		t.Fatalf("shrunk mean %v over %v, want 0.1 over 5", b, n)
	}
	st.Baseline[coarseKey(key)] = &BaselineStat{Trials: 5, ValueSum: -1}
	if b, _ := st.baseline(p, key); !near(b, (5*(-1.0/10)+1)/10) {
		t.Fatalf("shrunk toward the coarser record: %v", b)
	}

	// Exploration: an unproven move whose demonstrated result (0.2) beats
	// the base AI may be tried, one that does not may not; a hypothesis,
	// which has no result, may.
	d, c, cur, self, key = gateFixture(p)
	st = d.st
	g = &gate{key: key, explore: true}
	if got, _ := choose(d, cur, self, g); got != c || !g.explored {
		t.Fatal("exploration tries an unproven move that beat the base AI when demonstrated")
	}
	setBase(st, key, 50, 0.3)
	if got, _ := choose(d, cur, self, &gate{key: key, explore: true}); got != nil {
		t.Fatal("exploration skips a move demonstrated worse than the base AI")
	}
	c.Source, c.Outcome = SrcSynthesized, Outcome{}
	if got, _ := choose(d, cur, self, &gate{key: key, explore: true}); got != c {
		t.Fatal("a hypothesis is tried by exploration")
	}
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("a hypothesis is tried only by exploration")
	}

	// BaseExplore: a move that proved better is left to the base AI.
	d, c, cur, self, key = gateFixture(p)
	setPlayed(p, d.st, c, 3, 0.1)
	setBase(d.st, key, 10, 0)
	g = &gate{key: key, yield: true}
	if got, why := choose(d, cur, self, g); got != nil || !g.held || why[RejBaseExplore] != 1 {
		t.Fatalf("yield: %v held=%v why=%v", got, g.held, why)
	}
}

// TestExplorationIsRareAndCapped: exploration is drawn once per new decision
// situation from the driver's seeded random source, at most
// TakeoverExploreMax times a round, and a proven move is occasionally left
// to the base AI with a probe.
func TestExplorationIsRareAndCapped(t *testing.T) {
	p := DefaultParams()
	p.TakeoverExplore, p.TakeoverExploreMax = 1, 3
	d, _, cur, self, _ := gateFixture(p)
	tried := 0
	for i := int32(0); i < 10; i++ {
		self.StateNo = i // a new decision situation each time
		for k := 0; k < 3; k++ {
			// Retries in the same situation draw nothing new.
			if d.decide(cur, self, int64(i)) != nil {
				tried++
				break
			}
		}
	}
	if tried != 3 {
		t.Fatalf("explored %d times in a round, want the cap of 3", tried)
	}
	d.roundEnd()
	self.StateNo = 100
	if d.decide(cur, self, 100) == nil {
		t.Fatal("the cap is per round")
	}

	// At the default rate, a fixed seed gives the same draws, about one new
	// situation in twenty.
	run := func() []bool {
		q := DefaultParams()
		q.TakeoverExploreMax = 1 << 20
		d, _, cur, self, _ := gateFixture(q)
		var out []bool
		for i := int32(0); i < 2000; i++ {
			self.StateNo = i
			out = append(out, d.decide(cur, self, int64(i)) != nil)
		}
		return out
	}
	a, b := run(), run()
	n := 0
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("exploration must follow the driver's seed")
		}
		if a[i] {
			n++
		}
	}
	if n < 60 || n > 140 {
		t.Fatalf("explored %d of 2000 new situations, want about %d", n, int(DefaultParams().TakeoverExplore*2000))
	}

	// BaseExplore leaves a proven move to the base AI, which a probe follows.
	q := DefaultParams()
	q.BaseExplore = 1
	d, c, cur, self, key := gateFixture(q)
	setPlayed(q, d.st, c, 3, 0.1)
	setBase(d.st, key, 10, 0)
	if d.decide(cur, self, 1) != nil || !d.held {
		t.Fatal("BaseExplore must leave the situation to the base AI")
	}
	if len(d.ct.leadIn) != 1 || !d.ct.leadIn[0].probe || d.ct.leadIn[0].key != key {
		t.Fatalf("the base AI's decision gets a probe: %+v", d.ct.leadIn)
	}
	d.decide(cur, self, 2)
	if len(d.ct.leadIn) != 1 {
		t.Fatal("one probe per decision situation")
	}
	q.BaseExplore = 0
	self.StateNo = 1
	if d.decide(cur, self, 3) != c {
		t.Fatal("without BaseExplore the proven move takes over")
	}

	// When the CBR layer takes over in a situation it first left to the base
	// AI, before the situation changes, the probe is withdrawn: the decision
	// was not the base AI's.
	q = DefaultParams()
	q.TakeoverExplore, q.BaseExplore = 0, 0
	d, c, cur, self, key = gateFixture(q)
	setBase(d.st, key, 10, 0)
	if d.decide(cur, self, 1) != nil || len(d.ct.leadIn) != 1 {
		t.Fatal("setup: an unproven move leaves the base AI in control, with a probe")
	}
	setPlayed(q, d.st, c, 3, 0.1)
	if d.decide(cur, self, 2) != c || len(d.ct.leadIn) != 0 {
		t.Fatalf("the probe must be withdrawn when the CBR layer takes over: %+v", d.ct.leadIn)
	}
}

// TestProbesCreditBaseline: probes take the share of a chain's reward a
// played case in their place would, into the base AI's record, without
// changing the credit of the cases around them, taking a case's place in
// the lead-in or counting toward the horizon.
func TestProbesCreditBaseline(t *testing.T) {
	p := DefaultParams()
	eng := New(p, t.TempDir(), 1)
	eng.SetPlayer(0, "A", "B")
	ps := eng.players[0]
	c := &Case{ID: 99, Source: SrcHumanLocal, Kind: KindCommand, Inputs: []InputBits{InA}, StartStateType: StStand}
	ps.st.Admit(p, []*Case{c})
	k1 := baselineKey(&Feat{Enemy: CharFeat{Present: true, RelX: 50}}, true, 4)
	k2 := baselineKey(&Feat{Enemy: CharFeat{Present: true, RelX: 50, HitStun: 5}}, false, 4)
	// A recorded case (the base AI's decision, recorded too) does not count
	// among the decisions compared with the base AI.
	rc := &resolvedChain{
		ch:   &chain{cases: []chainCase{{probe: true, key: k1}, {rec: &Case{}}, {caseID: 99}, {probe: true, key: k2}}, probes: 2},
		comp: Completion{Result: CompKnockdown, Quality: p.quality(CompKnockdown), DamageDealt: 0.2},
	}
	r, g := p.reward(&rc.comp), p.ComboGamma
	eng.finalizeChain(ps, rc)
	st := ps.st
	if b := st.Baseline[k2]; b == nil || b.Trials != 1 || !near(b.ValueSum, r) {
		t.Fatalf("the last decision takes the whole reward %v: %+v", r, b)
	}
	if b := st.Baseline[k1]; b == nil || !near(b.ValueSum, r*g*g) {
		t.Fatalf("a probe two decisions back takes gamma squared: %+v", b)
	}
	if b := st.Baseline[baseAll]; b == nil || b.Trials < 1.99 {
		t.Fatalf("every probe counts in the store-wide record: %+v", b)
	}
	if b := st.Baseline[coarseKey(k1)]; b == nil || b.Trials != 1 {
		t.Fatalf("and in its coarser situation: %+v", b)
	}
	a := st.action(c)
	if a.PTrials != 1 || !near(a.PSum, r*g) {
		t.Fatalf("the played case's record against the base AI counts the probe after it: %+v", a)
	}
	if c.Outcome.Trials != 1 || !near(c.Outcome.ValueSum, r) {
		t.Fatalf("probes leave the played case's value as it was: %+v", c.Outcome)
	}
	if st.Len() != 1 {
		t.Fatal("probes are not cases")
	}

	// A probe or a played case that never joined an exchange resolves as
	// neutral, both counted; a contested play counts for nothing.
	ps.ct.onEvict(chainCase{probe: true, key: k2}, 1)
	if b := st.Baseline[k2]; b.Trials < 1.99 || !near(b.ValueSum, r*p.Decay+p.Situation[CompNeutral]) {
		t.Fatalf("an evicted probe is a neutral result: %+v", b)
	}
	ps.ct.onEvict(chainCase{caseID: 99}, 1)
	if a.PTrials < 1.99 || !near(a.PSum, r*g*p.Decay+p.Situation[CompNeutral]) {
		t.Fatalf("an evicted play is a neutral result: %+v", a)
	}
	pt, psum := a.PTrials, a.PSum
	ps.ct.onEvict(chainCase{caseID: 99, contested: true}, 1)
	if a.PTrials != pt || a.PSum != psum {
		t.Fatal("a contested play counts for nothing")
	}

	// Probes take no case's place in the lead-in.
	ct := newChainTracker(p, 0, new(uint64))
	var evicted []chainCase
	ct.onEvict = func(cc chainCase, _ int64) { evicted = append(evicted, cc) }
	rec := &Case{}
	ct.addCase(chainCase{rec: rec})
	for i := 0; i < 10; i++ {
		ct.addCase(chainCase{probe: true})
	}
	if len(evicted) != 10-leadInMax {
		t.Fatalf("%d evicted, want %d probes", len(evicted), 10-leadInMax)
	}
	for _, e := range evicted {
		if !e.probe {
			t.Fatal("a probe pushed a case out of the lead-in")
		}
	}

	// Probes do not count toward the horizon.
	p2 := DefaultParams()
	p2.HorizonFrames, p2.HorizonCases = 0, 3
	ct = newChainTracker(p2, 0, new(uint64))
	tr := newTracker(p2, 0)
	timeouts := 0
	ct.onFinal = func(rc *resolvedChain) {
		if rc.comp.Result == CompTimeout {
			timeouts++
		}
	}
	s := newSim(3600)
	for f := 0; f < 60; f++ {
		s.tick++
		s.ch[1].setState(5000, StStand, MtHit, false)
		s.ch[1].hitstun = 5
		s.ch[0].setState(200, StStand, MtAttack, false)
		snap := s.snapshot()
		tr.observe(snap)
		ct.step(snap, tr)
		ct.addCase(chainCase{probe: true})
	}
	if timeouts != 0 {
		t.Fatal("probes alone must not reach the horizon")
	}
	for i := 0; i < 3; i++ {
		ct.addCase(chainCase{caseID: uint64(i + 1)})
	}
	snap := s.snapshot()
	tr.observe(snap)
	ct.step(snap, tr)
	if timeouts != 1 {
		t.Fatal("cases still reach the horizon")
	}
}

// TestBaselineFillsInPlay: with recording off, the base AI's record fills
// during the match from its own actions, and from an AI that changes states
// directly, whose recorded cases are discarded; no case is created.
func TestBaselineFillsInPlay(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	s := newSim(1800)
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	runRoundBase(s, eng, [2]controller{nil, &defender{rng: rand.New(rand.NewSource(1))}},
		[2]controller{&poker{rng: rand.New(rand.NewSource(2))}}, 1800)
	st, _ := eng.Store("Rush")
	if b := st.Baseline[baseAll]; b == nil || b.Trials < 5 {
		t.Fatalf("the base AI's actions should fill its record: %+v", b)
	}
	if st.Len() != 0 {
		t.Fatalf("probes created %d cases", st.Len())
	}

	eng = New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	s = newSim(3600)
	s.cmdLayer = true
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Record(0, SrcAuthoredAI)
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	runRoundBase(s, eng, [2]controller{nil, &defender{rng: rand.New(rand.NewSource(3))}},
		[2]controller{&stateChanger{rng: rand.New(rand.NewSource(4))}}, 3600)
	st, _ = eng.Store("Rush")
	if b := st.Baseline[baseAll]; b == nil || b.Trials < 5 {
		t.Fatalf("an AI that changes states directly still gets a record: %+v", b)
	}
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
	if !st.ChangeStateAI || st.Len() != 0 {
		t.Fatalf("setup: the AI should be flagged and its cases discarded (flag %v, %d cases)", st.ChangeStateAI, st.Len())
	}
	if b := st.Baseline[baseAll]; b == nil || b.Trials < 5 {
		t.Fatal("the base AI's record outlasts the discarded cases")
	}
}

// TestTakeoverMarginLevels: the margin settings Low, Medium and High are
// half, once and twice Params.TakeoverMargin; anything else is Medium.
func TestTakeoverMarginLevels(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetPlayer(0, "A", "B")
	if eng.TakeoverMargin(0) != 0 {
		t.Fatal("no margin while not driven")
	}
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	if eng.TakeoverMargin(0) != 0.03 {
		t.Fatalf("a new driver starts at Medium: %v", eng.TakeoverMargin(0))
	}
	for lvl, want := range map[int]float32{1: 0.015, 2: 0.03, 3: 0.06, 0: 0.03, 4: 0.03, -1: 0.03} {
		eng.SetTakeoverMargin(0, lvl)
		if got := eng.TakeoverMargin(0); got != want {
			t.Fatalf("level %d: margin %v, want %v", lvl, got, want)
		}
	}
}

// TestContestedPlayIsDropped: when the character's own AI starts an attack
// with no command during a move the CBR layer plays, the move is dropped
// without counting against it, its result is not credited, and the CBR
// layer waits for control to return.
func TestContestedPlayIsDropped(t *testing.T) {
	p := withoutBaseAI()
	eng := New(p, t.TempDir(), 1)
	st, _ := eng.Store("A")
	start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
	c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InA, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start}
	st.Admit(p, []*Case{c})
	eng.SetPlayer(0, "A", "B")
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	ps := eng.players[0]
	ff := &frameFeeder{eng: eng}
	for k := 0; k < 2; k++ {
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
		if _, ok := eng.Input(0, true); !ok || ps.drv.cur != c {
			t.Fatal("setup: the case should play")
		}
		// The character's AI forces an attack the case did not input.
		ff.frame(CharSnap{StateNo: 1000, MoveType: MtAttack, CBRDriven: true})
		if ps.drv.cur != nil {
			t.Fatal("the contested play must be dropped")
		}
		if _, ok := eng.Input(0, true); ok {
			t.Fatal("the CBR layer waits for control to return")
		}
		for i := 0; i < 30; i++ {
			ff.frame(CharSnap{StateNo: 1000, MoveType: MtAttack})
		}
	}
	ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
	ff.end()
	if c.Outcome.Aborts != 0 || c.Outcome.ExecFails != 0 {
		t.Fatalf("nothing counts against a contested play: %+v", c.Outcome)
	}
	if c.Outcome.Trials != 0 || st.action(c).PTrials != 0 {
		t.Fatalf("a contested play's result is not credited: %+v %+v", c.Outcome, st.action(c))
	}
	if !st.ChangeStateAI {
		t.Fatal("repeated forced attacks mark the AI as direct state change")
	}
}

// TestSwitchedOnAIIsNotHeldAgainstMoves: a base AI that the AILevel mask
// cannot pause forces attacks during the CBR layer's moves. Those moves are
// dropped instead of failing, and the AI is noted as direct state change. A
// forced attack on the very frame a move presses its button is explained by
// that button and still counts as a failure; there are few of those
// (without the drop, about one play in six fails here).
func TestSwitchedOnAIIsNotHeldAgainstMoves(t *testing.T) {
	eng := New(withoutBaseAI(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcHumanLocal, 3, 30, func(r *rand.Rand) controller { return &rushdown{rng: r} })
	st, _ := eng.Store("Rush")
	driven := 0
	for r := 0; r < 3; r++ {
		s := newSim(3600)
		s.cmdLayer = true
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		base := &switchedOn{stateChanger{rng: rand.New(rand.NewSource(int64(5 + r)))}}
		d := runRoundBase(s, eng, [2]controller{nil, &defender{rng: rand.New(rand.NewSource(int64(60 + r)))}}, [2]controller{base}, 3600)
		driven += d[0]
	}
	var plays, fails int32
	for _, c := range st.Cases {
		plays += c.Outcome.Plays
		fails += c.Outcome.ExecFails
	}
	t.Logf("driven frames %d, plays %d, failed %d", driven, plays, fails)
	if plays == 0 {
		t.Fatal("setup: the CBR layer never played")
	}
	if !st.ChangeStateAI {
		t.Fatal("forced attacks during the CBR layer's moves mark the AI as direct state change")
	}
	if fails*8 > plays {
		t.Fatalf("moves the AI took over from were counted as failing: %d of %d plays", fails, plays)
	}
}

// TestBaselinePersists: the base AI's record and the CBR layer's own plays
// are saved, and a store saved before they existed loads with them empty.
func TestBaselinePersists(t *testing.T) {
	dir := t.TempDir()
	p := DefaultParams()
	st := NewStore("x")
	c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, Inputs: []InputBits{InA}, StartStateType: StStand}
	st.Admit(p, []*Case{c})
	k := baselineKey(&Feat{}, true, 5)
	st.addBaseline(p, k, 0.25)
	p.applyPlayed(st.action(c), 0.5)
	if err := SaveStore(LocalPath(dir, "x"), st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStore(LocalPath(dir, "x"), "x")
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Baseline[k]; b == nil || b.Trials != 1 || b.ValueSum != 0.25 || len(got.Baseline) != 3 {
		t.Fatalf("baseline after load: %+v (%d entries)", b, len(got.Baseline))
	}
	if a := got.Actions[actionKey(c)]; a == nil || a.PTrials != 1 || a.PSum != 0.5 {
		t.Fatalf("played record after load: %+v", a)
	}
	st.Baseline = nil
	if err := SaveStore(LocalPath(dir, "old"), st); err != nil {
		t.Fatal(err)
	}
	old, err := LoadStore(LocalPath(dir, "old"), "old")
	if err != nil {
		t.Fatal(err)
	}
	if old.Baseline == nil || len(old.Baseline) != 0 {
		t.Fatal("an older store loads with an empty baseline")
	}
}
