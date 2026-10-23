package cbr

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
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
	return overBaseAt(eng, n, seedBase, lv, 2, base, opp)
}

// overBaseAt is overBase at a take-over margin setting (1 Low, 2 Medium,
// 3 High).
func overBaseAt(eng *Engine, n, seedBase int, lv Levels, margin int, base, opp oppFactory) (out []roundResult, dec *decisions) {
	dec = &decisions{by: map[uint64][2]int{}}
	for r := 0; r < n; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, lv)
		eng.SetTakeoverMargin(0, margin)
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
						k := coarseKey(situationOf(actionKey(c), c.StartStateType, s.ch[0].ai))
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

// comboSit is the coarse situation of a follow-up: standing mid-action,
// with the enemy in hitstun, at AI level 0 (the simulator's).
var comboSit = coarseKey(baseFine | 2<<8 | uint64(StStand)<<baseSelfShift)

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
		key := situationOf(k, StStand, 0)
		b, n := st.baseline(p, key)
		t.Logf("%s: %.1f plays, mean %.3f against the base AI's %.3f over %.1f results", ActionString(k), a.PTrials, a.PSum/a.PTrials, b, n)
		if a.PSum/a.PTrials < b+p.TakeoverMargin {
			t.Fatalf("the cancel's plays (%.3f) should beat the base AI (%.3f)", a.PSum/a.PTrials, b)
		}
		// What the cancel was compared with is the base AI's own record of
		// the follow-ups, which holds enough results to count, shrunk toward
		// the coarser records as Store.baseline says.
		rec := st.Baseline[key]
		if rec == nil || rec.Trials < p.decayed(p.BaselineMinTrials)-1e-3 {
			t.Fatalf("the base AI's record of the follow-ups should hold at least %v results: %+v", p.BaselineMinTrials, rec)
		}
		want := p.BaselineDefault
		for _, k := range []uint64{baseAll, coarseKey(key), key} {
			if r := st.Baseline[k]; r != nil {
				want = (p.BaselineShrink*want + r.ValueSum) / (p.BaselineShrink + r.Trials)
			}
		}
		if !near(b, want) || n != rec.Trials || rec.ValueSum/rec.Trials <= p.BaselineDefault {
			t.Fatalf("the comparison must use the base AI's record (mean %.3f over %.1f): got %.3f over %.1f, want %.3f",
				rec.ValueSum/rec.Trials, rec.Trials, b, n, want)
		}
	}
	if !found {
		t.Fatal("the cancel into the special was never proved")
	}
}

// situationOf is the baseline situation an action key describes, for a
// character in the given state type with a base AI at the given AI level.
func situationOf(action uint64, self StateType, aiLevel float32) uint64 {
	const fields = 0xf | 7<<4 | 3<<8 | 1<<11 // band, enemy state, context, control
	return baseFine | action&fields | aiBand(aiLevel)<<13 | uint64(self&7)<<baseSelfShift
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
	f := Feat{RoundState: 2, Self: CharFeat{Present: true, State: StStand}, Enemy: CharFeat{Present: true, RelX: 60, State: StStand}}
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
// base AI with the given mean, each a decision that ended its exchange (so
// the margin applies in full).
func setBase(st *Store, key uint64, n, mean float32) {
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		st.Baseline[k] = &BaselineStat{Trials: n, ValueSum: n * mean, Shares: n}
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
	// among the decisions compared with the base AI, and neither does a
	// probe: only the CBR layer's played cases after an entry reduce its
	// share.
	rc := &resolvedChain{
		ch:   &chain{cases: []chainCase{{probe: true, key: k1}, {rec: &Case{}}, {caseID: 99}, {probe: true, key: k2}}, probes: 2},
		comp: Completion{Result: CompKnockdown, Quality: p.quality(CompKnockdown), DamageDealt: 0.2},
	}
	r, g := p.reward(&rc.comp), p.ComboGamma
	eng.finalizeChain(ps, rc)
	st := ps.st
	if b := st.Baseline[k2]; b == nil || b.Trials != 1 || !near(b.ValueSum, r) || !near(b.Shares, 1) {
		t.Fatalf("the last decision takes the whole reward %v: %+v", r, b)
	}
	if b := st.Baseline[k1]; b == nil || !near(b.ValueSum, r*g) || !near(b.Shares, g) {
		t.Fatalf("a probe with one played case after it takes gamma: %+v", b)
	}
	if b := st.Baseline[baseAll]; b == nil || b.Trials < 1.99 || !near(b.Shares, g*p.Decay+1) {
		t.Fatalf("every probe counts in the store-wide record: %+v", b)
	}
	if b := st.Baseline[coarseKey(k1)]; b == nil || b.Trials != 1 {
		t.Fatalf("and in its coarser situation: %+v", b)
	}
	a := st.action(c)
	if a.PTrials != 1 || !near(a.PSum, r) {
		t.Fatalf("the played case's record against the base AI leaves out the probe after it: %+v", a)
	}
	if c.Outcome.Trials != 1 || !near(c.Outcome.ValueSum, r) {
		t.Fatalf("probes leave the played case's value as it was: %+v", c.Outcome)
	}
	if st.Len() != 1 {
		t.Fatal("probes are not cases")
	}
	// However many decisions the base AI makes after an entry (one at every
	// change of state while it has control), its share stays the one a
	// played case in its place would take; each played case after it still
	// costs a factor of gamma.
	k3 := baselineKey(&Feat{Enemy: CharFeat{Present: true, RelX: 130}}, true, 4)
	k4 := baselineKey(&Feat{Enemy: CharFeat{Present: true, RelX: 170}}, true, 4)
	before := *st.action(c)
	eng.finalizeChain(ps, &resolvedChain{
		ch: &chain{cases: []chainCase{{probe: true, key: k3}, {probe: true}, {caseID: 99}, {probe: true}, {probe: true},
			{probe: true, key: k4}, {caseID: 99}, {caseID: 99}}, probes: 5},
		comp: rc.comp,
	})
	if b := st.Baseline[k3]; b == nil || !near(b.ValueSum, r*g*g*g) {
		t.Fatalf("three played cases after a probe: gamma cubed, the probes between left out: %+v", b)
	}
	if b := st.Baseline[k4]; b == nil || !near(b.ValueSum, r*g*g) {
		t.Fatalf("two played cases after a probe: gamma squared: %+v", b)
	}
	if got, want := a.PSum, ((before.PSum*p.Decay+r*g*g)*p.Decay+r*g)*p.Decay+r; !near(got, want) {
		t.Fatalf("plays take gamma per played case after them, probes left out: %v, want %v", got, want)
	}
	*a = before

	// A probe or a played case that never joined an exchange resolves as
	// neutral, both counted; a contested play counts for nothing. Neither
	// was credited with any share of an exchange's reward.
	ps.ct.onEvict(chainCase{probe: true, key: k2}, 1)
	if b := st.Baseline[k2]; b.Trials < 1.99 || !near(b.ValueSum, r*p.Decay+p.Situation[CompNeutral]) || !near(b.Shares, p.Decay) {
		t.Fatalf("an evicted probe is a neutral result: %+v", b)
	}
	ps.ct.onEvict(chainCase{caseID: 99}, 1)
	if a.PTrials < 1.99 || !near(a.PSum, r*p.Decay+p.Situation[CompNeutral]) {
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
	st.addBaseline(p, k, 0.25, 0.5)
	p.applyPlayed(st.action(c), 0.5)
	if err := SaveStore(LocalPath(dir, "x"), st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadStore(LocalPath(dir, "x"), "x")
	if err != nil {
		t.Fatal(err)
	}
	if b := got.Baseline[k]; b == nil || b.Trials != 1 || b.ValueSum != 0.25 || b.Shares != 0.5 || len(got.Baseline) != 3 {
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

// TestAirMovesMeetTheAirRecord: the base AI's decisions in the air are kept
// apart from its decisions on the ground at the same distance, so a move
// played in the air is compared with what the base AI does in the air.
func TestAirMovesMeetTheAirRecord(t *testing.T) {
	p := DefaultParams()
	ground := Feat{RoundState: 2, Self: CharFeat{Present: true, State: StStand}, Enemy: CharFeat{Present: true, RelX: 60, State: StStand}}
	air := ground
	air.Self.State, air.Self.RelY = StAir, -40
	gk, ak := baselineKey(&ground, true, 8), baselineKey(&air, true, 8)
	if gk == ak || coarseKey(gk) == coarseKey(ak) {
		t.Fatal("the character's own state type must be part of the situation and its coarser one")
	}
	for k, want := range map[uint64]string{gk: "self standing,", ak: "self airborne,", coarseKey(ak): "self airborne,"} {
		if got := SituationString(k); !strings.HasPrefix(got, want) {
			t.Fatalf("SituationString(%#x) = %q, want it to start with %q", k, got, want)
		}
	}
	// An air move and a ground move with the same plays, three averaging
	// 0.09 each.
	fixture := func() (*driver, *Case, *Case) {
		st := NewStore("x")
		jump := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 600, ExecAttack: true, ExecClass: StAir,
			Inputs: []InputBits{InA, 0, 0}, StartStateType: StAir, StartCtrl: true, StartMoveType: MtIdle, Start: air}
		jab := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{InA, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: ground}
		jump.Outcome = Outcome{Trials: 1, ValueSum: 0.2}
		jab.Outcome = jump.Outcome
		st.Admit(p, []*Case{jump, jab})
		setPlayed(p, st, jump, 3, 0.09)
		setPlayed(p, st, jab, 3, 0.09)
		return newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64))), jump, jab
	}
	inAir := &CharSnap{StateType: StAir, MoveType: MtIdle, Ctrl: true, AILevel: 8}
	onGround := &CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true, AILevel: 8}

	// The base AI does nothing much on the ground at this distance (80
	// results worth 0) but jumps in well (20 results worth 0.12). Sharing
	// one record, the two would average 0.024, and the air move would take
	// over although the base AI does better in the air.
	d, jump, jab := fixture()
	for i := 0; i < 80; i++ {
		d.st.addBaseline(p, gk, 0, 1)
	}
	for i := 0; i < 20; i++ {
		d.st.addBaseline(p, ak, 0.12, 1)
	}
	if b, n := d.st.baseline(p, ak); b < 0.1 || n < 19 {
		t.Fatalf("the air situation's record is the base AI's air play: mean %.3f over %.1f", b, n)
	}
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, &gate{key: ak}); got != nil {
		t.Fatal("an air move worse than the base AI's air play must not take over")
	}
	if got, _ := d.chooseWhy(&ground, onGround, 1, nil, &gate{key: gk}); got != jab {
		t.Fatal("the same plays on the ground beat the base AI's ground play and take over")
	}

	// With ground results only, the air situation has no record of its own:
	// the base AI's ground play does not count as its results there, so the
	// margin doubles (0.045 is under 0.06) until it has enough air results.
	d, jump, _ = fixture()
	for i := 0; i < 100; i++ {
		d.st.addBaseline(p, gk, 0, 1)
	}
	if _, n := d.st.baseline(p, ak); n != 0 {
		t.Fatalf("ground results counted in the air: %v", n)
	}
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, &gate{key: ak}); got != nil {
		t.Fatal("against no air record, the margin doubles")
	}
	for i := 0; i < 5; i++ {
		d.st.addBaseline(p, ak, 0, 1)
	}
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, &gate{key: ak}); got != jump {
		t.Fatal("against an air record as good as the ground one, the air move takes over")
	}
}

// TestLandingIsNotABaseDecision: a jump's landing takes control away (state
// 52 has none) whatever the AI does, so it adds no probe, as the recorder
// does not count it as an action either; an action the base AI starts from
// control, on the ground or in the air, does.
func TestLandingIsNotABaseDecision(t *testing.T) {
	// feed plays frames with the base AI in control and returns the base
	// AI's record once the round has ended.
	feed := func(frames ...CharSnap) map[uint64]*BaselineStat {
		eng := New(DefaultParams(), t.TempDir(), 1)
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ff := &frameFeeder{eng: eng}
		for _, f := range frames {
			ff.frame(f)
		}
		ff.end()
		st, _ := eng.Store("A")
		return st.Baseline
	}
	jumping := CharSnap{StateNo: 50, StateType: StAir, MoveType: MtIdle, Ctrl: true, Pos: [3]float32{0, -30}}
	landed := CharSnap{StateNo: 52, StateType: StStand, MoveType: MtIdle}
	standing := CharSnap{StateNo: 0, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	if rec := feed(jumping, jumping, landed, standing); len(rec) != 0 {
		t.Fatalf("a landing added a probe: %d records", len(rec))
	}
	rec := feed(standing, CharSnap{StateNo: 200, StateType: StStand, MoveType: MtAttack})
	if b := rec[baseAll]; b == nil || b.Trials != 1 {
		t.Fatalf("an attack the base AI starts from control adds one probe: %+v", b)
	}
	rec = feed(jumping, CharSnap{StateNo: 600, StateType: StAir, MoveType: MtAttack, Pos: [3]float32{0, -28}})
	if b := rec[baseAll]; b == nil || b.Trials != 1 {
		t.Fatalf("an air attack the base AI starts from control adds one probe: %+v", b)
	}
	for k := range rec {
		if k != baseAll && !strings.HasPrefix(SituationString(k), "self airborne,") {
			t.Fatalf("the air attack's decision is kept as an air decision: %s", SituationString(k))
		}
	}
}

// TestBackoffEndsWhenTheSituationChanges: after holding back because nothing
// proved better, the CBR layer waits NotBetterBackoff frames with control,
// but looks again at once when the situation compared with the base AI
// changes, here the enemy starting an attack, where a move has proved better.
func TestBackoffEndsWhenTheSituationChanges(t *testing.T) {
	p := DefaultParams()
	p.TakeoverExplore, p.BaseExplore = 0, 0
	run := func(change bool) int64 {
		d, c, cur, self, key := gateFixture(p)
		setPlayed(p, d.st, c, 3, 0)
		setBase(d.st, key, 10, 0.1) // the move is worse here: held
		w := &WorldSnap{RoundState: 2}
		if _, ok := d.input(w, self, cur); ok || !d.held {
			t.Fatal("setup: the CBR layer should hold back")
		}
		next := *cur
		if change {
			next.Enemy.Move, next.Enemy.HitboxActive = MtAttack, true
		}
		// Where the enemy attacks (or, without the change, once the base
		// AI's record here turns bad), the base AI does badly and the move
		// has proved better.
		setBase(d.st, baselineKey(&next, true, 8), 10, -0.2)
		for w.Tick = 1; w.Tick < 20; w.Tick++ {
			if _, ok := d.input(w, self, &next); ok {
				return w.Tick
			}
		}
		return -1
	}
	if at := run(true); at != 1 {
		t.Fatalf("a change of situation should end the back-off at once, took over at tick %d", at)
	}
	if at := run(false); at != int64(p.NotBetterBackoff) {
		t.Fatalf("with the situation unchanged, the back-off lasts NotBetterBackoff frames: took over at tick %d", at)
	}
}

// TestUngradedCasesMayBeExplored: a case with no result at all, such as a
// route recorded in trial mode (whose recording is not graded), has nothing
// to compare with the base AI and may be tried by exploration, like a
// hypothesis; it may not take over otherwise.
func TestUngradedCasesMayBeExplored(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	// A trial route is a continuation: the enemy is in a combo.
	cur.ComboMoves, cur.DidHit = 1, true
	c.Start.ComboMoves, c.Start.DidHit = 1, true
	key = baselineKey(cur, true, 8)
	c.Source, c.Outcome = SrcTrial, Outcome{}
	setBase(d.st, key, 50, 0.3)
	if !explorable(c, 0.3) {
		t.Fatal("a case with no result may be explored")
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key, explore: true}); got != c {
		t.Fatal("exploration tries a trial route")
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != nil {
		t.Fatal("a trial route is tried only by exploration until it proves itself")
	}
	// Once graded, its result counts like any other.
	c.Outcome = Outcome{Trials: 1, ValueSum: 0.1}
	if explorable(c, 0.3) {
		t.Fatal("a graded case worse than the base AI may not be explored")
	}
}

// TestUnprovenMovesAreRetried: a move whose first plays did not prove it
// better is tried again by exploration until it has TakeoverRetryPlays
// plays, unless its plays so far fell short of the base AI's by more than
// the margin; moves not played yet come first, and at most
// TakeoverRetryMax explorations a round try a move again. With the retries,
// the margin settings decide which of two moves with a real edge take over.
func TestUnprovenMovesAreRetried(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	setBase(d.st, key, 10, 0)
	try := func(g gate) (*Case, gate) {
		got, _ := d.chooseWhy(cur, self, 1, nil, &g)
		return got, g
	}
	// Three plays 0.05 better than the base AI: shrunk to 0.025, under the
	// Medium margin of 0.03.
	setPlayed(p, d.st, c, 3, 0.05)
	if got, _ := try(gate{key: key}); got != nil {
		t.Fatal("setup: three plays at 0.05 do not prove the move at Medium")
	}
	if got, g := try(gate{key: key, explore: true, retry: true}); got != c || !g.explored || !g.retried {
		t.Fatalf("a move not proved yet is tried again: %v %+v", got, g)
	}
	if got, _ := try(gate{key: key, explore: true}); got != nil {
		t.Fatal("trying a move again needs the retry allowance")
	}
	// Plays that fell short of the base AI by more than the margin end it.
	setPlayed(p, d.st, c, 3, -0.04)
	if got, _ := try(gate{key: key, explore: true, retry: true}); got != nil {
		t.Fatal("a move clearly worse than the base AI is not tried again")
	}
	// So does reaching TakeoverRetryPlays plays.
	setPlayed(p, d.st, c, p.TakeoverRetryPlays, 0.03)
	if got, _ := try(gate{key: key, explore: true, retry: true}); got != nil {
		t.Fatal("a move with TakeoverRetryPlays plays has had its chance")
	}

	// A move not played yet comes before one being tried again.
	d, c, cur, self, key = gateFixture(p)
	setBase(d.st, key, 10, 0)
	setPlayed(p, d.st, c, 3, 0.05)
	fresh := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 400, ExecAttack: true, ExecClass: StStand,
		Inputs: []InputBits{InD | InBtnB, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: c.Start, Outcome: c.Outcome}
	d.st.Admit(p, []*Case{fresh})
	if got, g := try(gate{key: key, explore: true, retry: true}); got != fresh || g.retried {
		t.Fatalf("an untried move comes first: %v %+v", got, g)
	}

	// At most TakeoverRetryMax explorations a round try a move again.
	q := DefaultParams()
	q.TakeoverExplore, q.TakeoverExploreMax, q.BaseExplore = 1, 100, 0
	d, c, cur, self, key = gateFixture(q)
	setBase(d.st, key, 10, 0)
	setPlayed(q, d.st, c, 3, 0.05)
	count := func() int {
		n := 0
		for i := int32(0); i < 10; i++ {
			self.StateNo = i
			if d.decide(cur, self, int64(i)) != nil {
				n++
			}
		}
		return n
	}
	if n := count(); n != int(q.TakeoverRetryMax) {
		t.Fatalf("tried again %d times in a round, want TakeoverRetryMax (%d)", n, q.TakeoverRetryMax)
	}
	d.roundEnd()
	if n := count(); n != int(q.TakeoverRetryMax) {
		t.Fatalf("the retry allowance is per round: %d", n)
	}

	// The margin settings: a move 0.045 better than the base AI (per
	// exchange, its plays ending exchanges as the base AI's do) takes over at
	// Low after its first three plays, at Medium only once retries have given
	// it enough plays for the shrinking to matter less, and never at High.
	for lvl, want := range map[int][2]bool{1: {true, true}, 2: {false, true}, 3: {false, false}} {
		d, c, cur, self, key = gateFixture(p)
		setBase(d.st, key, 50, 0)
		d.margin = p.takeoverMargin(lvl)
		for i, n := range []float32{3, 9} {
			setPlayed(p, d.st, c, n, 0.045)
			if got, _ := try(gate{key: key}); (got == c) != want[i] {
				t.Fatalf("margin level %d after %v plays: took over %v, want %v", lvl, n, got == c, want[i])
			}
		}
	}
}

// TestPromisingMovesAreTriedSooner: a move not played yet whose
// demonstrated result beats an established record of the base AI by more
// than the margin is tried with the larger chance TakeoverExplorePromising;
// others, and any move where the base AI's record is too thin to compare
// with, keep the ordinary chance.
func TestPromisingMovesAreTriedSooner(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p) // demonstrated 0.2
	try := func(g gate) *Case {
		got, _ := d.chooseWhy(cur, self, 1, nil, &g)
		return got
	}
	setBase(d.st, key, 10, 0)
	if try(gate{key: key, promising: true}) != c {
		t.Fatal("a move demonstrated 0.2 better than the base AI is promising")
	}
	setBase(d.st, key, 10, 0.18)
	if try(gate{key: key, promising: true}) != nil {
		t.Fatal("a move demonstrated only 0.02 better is not promising")
	}
	if try(gate{key: key, explore: true}) != c {
		t.Fatal("it may still be tried at the ordinary chance")
	}
	setBase(d.st, key, 3, 0)
	if try(gate{key: key, promising: true}) != nil {
		t.Fatal("against a thin record of the base AI, nothing is promising")
	}

	// In play: with an established record, the promising move is tried in
	// about one new situation in four, an ordinary one in about one in
	// twenty.
	rate := func(base float32) int {
		q := DefaultParams()
		q.TakeoverExploreMax, q.BaseExplore = 1<<20, 0
		d, _, cur, self, key := gateFixture(q)
		setBase(d.st, key, 10, base)
		n := 0
		for i := int32(0); i < 2000; i++ {
			self.StateNo = i
			if d.decide(cur, self, int64(i)) != nil {
				n++
			}
		}
		return n
	}
	if n := rate(0); n < 400 || n > 600 {
		t.Fatalf("a promising move was tried in %d of 2000 new situations, want about %d", n, int(p.TakeoverExplorePromising*2000))
	}
	if n := rate(0.18); n < 60 || n > 140 {
		t.Fatalf("an ordinary move was tried in %d of 2000 new situations, want about %d", n, int(p.TakeoverExplore*2000))
	}
}

// TestMarginIsPerExchange: the margin is set per exchange and the results
// are per decision, credited with their share of their exchange's reward,
// so the margin is scaled by the share the base AI's decisions in the
// situation get.
func TestMarginIsPerExchange(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	// Three plays 0.04 better: shrunk to 0.02, under the margin of 0.03 for
	// decisions that end their exchange.
	setPlayed(p, d.st, c, 3, 0.04)
	setBase(d.st, key, 1000, 0)
	if sh := d.st.baselineShare(p, key); !near(sh, 1) {
		t.Fatalf("decisions that end their exchange take a share of 1, got %v", sh)
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != nil {
		t.Fatal("0.02 is under the full margin")
	}
	// Where the base AI's decisions take half of an exchange's reward on
	// average, the margin is half as large, 0.015.
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		d.st.Baseline[k].Shares = 500
	}
	if sh := d.st.baselineShare(p, key); sh < 0.5 || sh > 0.51 {
		t.Fatalf("share %v, want about 0.5", sh)
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != c {
		t.Fatal("0.02 beats the margin scaled to half an exchange")
	}
	// With nothing known of a situation, the share is a whole exchange.
	if sh := NewStore("y").baselineShare(p, key); sh != 1 {
		t.Fatalf("share with no record %v, want 1", sh)
	}
}

// TestTakeoverMarginSettings: the store learned from the player who
// cancels, and that player is also the base AI, so the CBR layer's moves are
// the base AI's own. At the default Medium margin the CBR layer keeps off
// the follow-ups, and the settings order how often it takes over: Low at
// least as often as Medium, Medium at least as often as High, and Low more
// often than High. Low accepts the small edge the CBR layer's cancel shows
// over the base AI's follow-ups as a whole (the cancel is played only where
// a recorded cancel starts, while the base AI's record also counts hits that
// came too late to cancel), so it takes over on many follow-ups.
func TestTakeoverMarginSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	baseNet, _, _ := summarize(baseOnly(6, 7000, baseRushdown, oppDefender))
	seeds := []int64{7, 9}
	type run struct {
		net float32
		dec *decisions
	}
	runs := make([][4]run, len(seeds))
	t.Run("runs", func(t *testing.T) {
		for i, seed := range seeds {
			for lvl := 1; lvl <= 3; lvl++ {
				i, seed, lvl := i, seed, lvl
				t.Run(fmt.Sprintf("seed%d/margin%d", seed, lvl), func(t *testing.T) {
					t.Parallel()
					eng := New(DefaultParams(), t.TempDir(), seed)
					recordAs(t, eng, 10, 40+seed, baseRushdown)
					overBaseAt(eng, 30, 9000+int(seed)*100, lv, lvl, baseRushdown, oppDefender)
					after, ad := overBaseAt(eng, 6, 7000, lv, lvl, baseRushdown, oppDefender)
					an, _, _ := summarize(after)
					runs[i][lvl] = run{an, ad}
				})
			}
		}
	})
	var plays, all, fuPlays, fuAll [4]int
	for i, seed := range seeds {
		for lvl := 1; lvl <= 3; lvl++ {
			r := runs[i][lvl]
			if r.dec == nil {
				t.Fatal("a run did not finish")
			}
			fu := r.dec.by[comboSit]
			t.Logf("seed %d, margin %d: net per round %.0f (base AI alone %.0f), %.1f%% of decisions, follow-ups %.0f%%",
				seed, lvl, r.net, baseNet, 100*r.dec.share(), 100*r.dec.share(comboSit))
			plays[lvl] += r.dec.plays
			all[lvl] += r.dec.plays + r.dec.probes
			fuPlays[lvl] += fu[0]
			fuAll[lvl] += fu[0] + fu[1]
			if lvl == 2 && r.dec.share(comboSit) >= 0.2 {
				t.Fatalf("seed %d: at Medium the CBR layer took over on %.0f%% of follow-ups from a base AI that plays them as well",
					seed, 100*r.dec.share(comboSit))
			}
		}
	}
	share := func(n, d int) float32 { return float32(n) / float32(max(d, 1)) }
	low, med, high := share(plays[1], all[1]), share(plays[2], all[2]), share(plays[3], all[3])
	t.Logf("share of decisions over both seeds: Low %.2f%%, Medium %.2f%%, High %.2f%%; follow-ups %.0f%%, %.0f%%, %.0f%%",
		100*low, 100*med, 100*high, 100*share(fuPlays[1], fuAll[1]), 100*share(fuPlays[2], fuAll[2]), 100*share(fuPlays[3], fuAll[3]))
	if low < med || med < high || low <= high {
		t.Fatalf("Low should take over at least as often as Medium, Medium as High, and Low more often than High: %.2f%%, %.2f%%, %.2f%%",
			100*low, 100*med, 100*high)
	}
	if share(fuPlays[1], fuAll[1]) <= share(fuPlays[2], fuAll[2]) {
		t.Fatal("Low should take over on more follow-ups than Medium")
	}
}
