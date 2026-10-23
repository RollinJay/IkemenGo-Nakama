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
						k := coarseKey(situationOf(playKey(c), s.ch[0].ai))
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
	seen := map[uint64]bool{}
	for _, c := range st.Cases {
		k := playKey(c)
		a := st.playRecord(c)
		cancel := c.Kind == KindCommand && c.MoveRef == 1000 && !c.StartCtrl
		if !cancel || seen[k] || a.Trials < p.decayed(p.TakeoverMinPlays)-1e-3 {
			continue
		}
		seen[k], found = true, true
		key := situationOf(k, 0)
		b, n := st.baseline(p, key)
		t.Logf("%s: %.1f plays, mean %.3f against the base AI's %.3f over %.1f results", PlayString(k), a.Trials, a.Sum/a.Trials, b, n)
		if a.Sum/a.Trials < b+p.TakeoverMargin {
			t.Fatalf("the cancel's plays (%.3f) should beat the base AI (%.3f)", a.Sum/a.Trials, b)
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

// situationOf is the baseline situation a played-record key describes (see
// playKey), for a base AI at the given AI level.
func situationOf(play uint64, aiLevel float32) uint64 {
	const fields = 0xf | 7<<4 | 3<<8 | 1<<11 // band, enemy state, context, control
	return baseFine | play&fields | aiBand(aiLevel)<<13 |
		play>>48&7<<baseSelfShift | play>>51&3<<baseStunShift
}

// TestBaseKeptWhenBetter: the base AI is the player who cancels, and the
// store learned from the weak poker. Nothing the store holds beats the base
// AI, so the CBR layer rarely takes over, and the result is no worse than
// the base AI's alone. The learning takes 36 rounds: with at most
// TakeoverExploreMax explorations a round, the poker's moves need that long
// to be tried and found no better. Both are measured over 12 rounds, as the
// few moves still tried now and then change how a round goes.
func TestBaseKeptWhenBetter(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	eng := New(DefaultParams(), t.TempDir(), 7)
	recordAs(t, eng, 10, 40, basePoker)
	baseNet, _, _ := summarize(baseOnly(12, 7000, baseRushdown, oppDefender))
	overBase(eng, 36, 9000, lv, baseRushdown, oppDefender)
	after, ad := overBase(eng, 12, 7000, lv, baseRushdown, oppDefender)
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

// setPlayed gives the case's move n plays by the CBR layer from the case's
// stance, with the given mean result.
func setPlayed(p *Params, st *Store, c *Case, n, mean float32) {
	a := st.played(c)
	a.Trials = p.decayed(n)
	a.Sum = a.Trials * mean
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
	// Every exploration takes play from the base AI, so the defaults keep it
	// rare: about one new situation in twenty, at most three a round, and
	// at most one of those a move tried again.
	if q := DefaultParams(); q.TakeoverExplore > 0.05 || q.TakeoverExploreMax > 3 || q.TakeoverRetryMax > 1 {
		t.Fatalf("exploration by default: chance %v, %d a round, %d retries", q.TakeoverExplore, q.TakeoverExploreMax, q.TakeoverRetryMax)
	}
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
	if a := st.playRecord(c); a.Trials != 1 || !near(a.Sum, r) {
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
	before := st.playRecord(c)
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
	if got, want := st.playRecord(c).Sum, ((before.Sum*p.Decay+r*g*g)*p.Decay+r*g)*p.Decay+r; !near(got, want) {
		t.Fatalf("plays take gamma per played case after them, probes left out: %v, want %v", got, want)
	}

	// A probe or a played case that never joined an exchange resolves as
	// neutral, both counted; a contested play counts for nothing. Neither
	// was credited with any share of an exchange's reward.
	ps.ct.onEvict(chainCase{probe: true, key: k2}, 1)
	if b := st.Baseline[k2]; b.Trials < 1.99 || !near(b.ValueSum, r*p.Decay+p.Situation[CompNeutral]) || !near(b.Shares, p.Decay) {
		t.Fatalf("an evicted probe is a neutral result: %+v", b)
	}
	before = st.playRecord(c)
	ps.ct.onEvict(chainCase{caseID: 99}, 1)
	if a := st.playRecord(c); !near(a.Trials, before.Trials*p.Decay+1) || !near(a.Sum, before.Sum*p.Decay+p.Situation[CompNeutral]) {
		t.Fatalf("an evicted play is a neutral result: %+v after %+v", a, before)
	}
	before = st.playRecord(c)
	ps.ct.onEvict(chainCase{caseID: 99, contested: true}, 1)
	if a := st.playRecord(c); a != before {
		t.Fatal("a contested play counts for nothing")
	}

	// Probes take no recorded case's place in the lead-in.
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
			t.Fatal("a probe pushed a recorded case out of the lead-in")
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

// TestBaselinePersists: the base AI's record and the CBR layer's own plays,
// by stance, are saved; a store saved before they existed loads with them
// empty, and one that kept them in the older layout, with situations that
// mixed what is now kept apart, loads with them cleared.
func TestBaselinePersists(t *testing.T) {
	dir := t.TempDir()
	p := DefaultParams()
	st := NewStore("x")
	c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, Inputs: []InputBits{InA}, StartStateType: StStand}
	st.Admit(p, []*Case{c})
	k := baselineKey(&Feat{}, true, 5)
	st.addBaseline(p, k, 0.25, 0.5)
	st.addPlay(p, c, 0.5)
	st.contestPlay(p, c)
	p.applyPlayed(st.action(c), 0.4)
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
	if a := got.Played[playKey(c)]; a == nil || *a != (PlayStat{Trials: 1, Sum: 0.5, Contests: 1}) || len(got.Played) != 1 {
		t.Fatalf("played record after load: %+v (%d entries)", a, len(got.Played))
	}
	if a := got.Actions[actionKey(c)]; a == nil || a.PTrials != 1 || a.PSum != 0.4 {
		t.Fatalf("plays of the action from any stance after load: %+v", a)
	}

	// A store from before the base AI's record: both records load empty.
	st.Baseline, st.Played = nil, nil
	if err := SaveStore(LocalPath(dir, "old"), st); err != nil {
		t.Fatal(err)
	}
	old, err := LoadStore(LocalPath(dir, "old"), "old")
	if err != nil {
		t.Fatal(err)
	}
	if old.Baseline == nil || len(old.Baseline) != 0 || old.Played == nil || len(old.Played) != 0 {
		t.Fatal("an older store loads with an empty baseline and no plays")
	}

	// A store in the older layout (no BaseFormat): its records were kept by
	// situations without the stance, so they are dropped, and what it
	// learned otherwise is kept.
	st.Baseline, st.Played, st.BaseFormat = map[uint64]*BaselineStat{}, map[uint64]*PlayStat{}, 0
	st.addBaseline(p, k, 0.25, 0.5)
	st.addPlay(p, c, 0.5)
	st.action(c).Trials, st.action(c).ValueSum = 2, 0.3
	if err := SaveStore(LocalPath(dir, "older"), st); err != nil {
		t.Fatal(err)
	}
	older, err := LoadStore(LocalPath(dir, "older"), "older")
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Baseline) != 0 || len(older.Played) != 0 || older.BaseFormat != baseFormat {
		t.Fatalf("records in the older layout must be dropped: %d, %d, format %d", len(older.Baseline), len(older.Played), older.BaseFormat)
	}
	if a := older.Actions[actionKey(c)]; a == nil || a.PTrials != 0 || a.PSum != 0 || a.Trials != 2 || a.ValueSum != 0.3 {
		t.Fatalf("plays of the action are dropped, its value kept: %+v", a)
	}
	if older.Len() != 1 {
		t.Fatal("the cases are kept")
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
	// Its plays beat the base AI by 0.05: TakeoverRetryPlays such plays
	// would prove it (0.038 once shrunk, above the margin of 0.03), so only
	// the shrinking holds it back. It is promising, and the larger chance
	// may try it again. Plays 0.02 better would not prove it (0.015).
	if got, g := try(gate{key: key, promising: true, retry: true}); got != c || !g.retried {
		t.Fatalf("a move whose plays would soon prove it is promising: %v %+v", got, g)
	}
	setPlayed(p, d.st, c, 3, 0.02)
	if got, _ := try(gate{key: key, promising: true, retry: true}); got != nil {
		t.Fatal("plays that would not prove a move are not promising")
	}
	if got, _ := try(gate{key: key, explore: true, retry: true}); got != c {
		t.Fatal("they may still be tried again at the ordinary chance")
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
	// So does being unable to prove itself in TakeoverRetryPlays plays even
	// if it kept doing as well: where decisions seldom lead into an exchange
	// (2% of one on average), a move 0.002 better per decision, as
	// demonstrated and as played, would need hundreds of plays.
	d, c, cur, self, key = gateFixture(p)
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		d.st.Baseline[k] = &BaselineStat{Trials: 200, ValueSum: 0, Shares: 4}
	}
	c.Outcome = Outcome{Trials: 1, ValueSum: 0.002}
	setPlayed(p, d.st, c, 3, 0.002)
	if got, _ := try(gate{key: key, explore: true, retry: true}); got != nil {
		t.Fatal("a move that could not prove itself is not tried again")
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
// demonstrated result would prove it better than an established record of
// the base AI within TakeoverRetryPlays plays is tried with the larger
// chance TakeoverExplorePromising; others, and any move where the base AI's
// record is too thin to compare with, keep the ordinary chance.
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
	// Where decisions seldom lead into an exchange (2% of one on average),
	// a move demonstrated 0.005 better per decision beats the margin there
	// (0.0006), but ten plays would be a fifth of an exchange's worth, far
	// from proving it: it is not promising.
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		d.st.Baseline[k] = &BaselineStat{Trials: 200, ValueSum: 0, Shares: 4}
	}
	c.Outcome = Outcome{Trials: 1, ValueSum: 0.005}
	if try(gate{key: key, promising: true}) != nil {
		t.Fatal("a move that would take many more plays to prove itself is not promising")
	}
	if try(gate{key: key, explore: true}) != c {
		t.Fatal("it may still be tried at the ordinary chance")
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
// situation get, and the evidence is counted in exchanges the same way.
func TestMarginIsPerExchange(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	// Nine plays 0.035 better: shrunk to 0.026, under the margin of 0.03 for
	// decisions that end their exchange.
	setPlayed(p, d.st, c, 9, 0.035)
	setBase(d.st, key, 1000, 0)
	if sh := d.st.baselineShare(p, key); !near(sh, 1) {
		t.Fatalf("decisions that end their exchange take a share of 1, got %v", sh)
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != nil {
		t.Fatal("0.026 is under the full margin")
	}
	// Where the base AI's decisions take half of an exchange's reward on
	// average, the margin is half as large, 0.015, and an exchange's worth of
	// plays is two plays, so the shrinking counts twice as many: the nine
	// plays shrink to 0.021, above the margin.
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		d.st.Baseline[k].Shares = 500
	}
	if sh := d.st.baselineShare(p, key); sh < 0.5 || sh > 0.51 {
		t.Fatalf("share %v, want about 0.5", sh)
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != c {
		t.Fatal("0.021 beats the margin scaled to half an exchange")
	}
	// Three such plays are only one and a half exchanges' worth: shrunk to
	// 0.012, under the halved margin. (Shrunk as if they were three
	// exchanges, they would make 0.017 and take over.)
	setPlayed(p, d.st, c, 3, 0.035)
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != nil {
		t.Fatal("three plays at half an exchange each are too little evidence")
	}
	// With nothing known of a situation, the share is a whole exchange.
	if sh := NewStore("y").baselineShare(p, key); sh != 1 {
		t.Fatalf("share with no record %v, want 1", sh)
	}
}

// TestTakeoverMarginSettings: the store learned from the player who
// cancels, and the base AI is that player missing one cancel in five
// (hesitant), so the CBR layer's cancel is a little better than the base
// AI's follow-ups: by about 0.025 a follow-up, between the Low margin
// (0.015) and the Medium one (0.03). After learning at each setting, with
// exploration off so that only moves that proved better take over, the
// settings order how often the CBR layer takes over: Low at least as often
// as Medium, Medium at least as often as High, and Low more often than High.
// Low accepts the small edge and takes over on many follow-ups, to the
// character's gain; at the default Medium margin the CBR layer keeps off
// them.
func TestTakeoverMarginSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	baseNet, _, _ := summarize(baseOnly(6, 7000, baseHesitant, oppDefender))
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
					overBaseAt(eng, 30, 9000+int(seed)*100, lv, lvl, baseHesitant, oppDefender)
					eng.Params().TakeoverExplore = 0
					after, ad := overBaseAt(eng, 6, 7000, lv, lvl, baseHesitant, oppDefender)
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
				t.Fatalf("seed %d: at Medium the CBR layer took over on %.0f%% of follow-ups from a base AI only a little worse at them",
					seed, 100*r.dec.share(comboSit))
			}
		}
	}
	if lowNet := runs[0][1].net + runs[1][1].net; lowNet <= 2*baseNet {
		t.Fatalf("at Low, taking over with the cancel should gain over the base AI alone: %.0f a round against %.0f", lowNet/2, baseNet)
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

// TestLeadInIsTheSameForBothLayers: a decision leaves the lead-in by one rule
// whichever layer made it, once leadInMax decisions of either layer have
// followed it, so a decision in the same place before an exchange gets the
// same credit whether it was the CBR layer's or the base AI's. Here a step
// the CBR layer played and ten decisions of the base AI come before an
// exchange, all in the same situation, twenty frames apart.
func TestLeadInIsTheSameForBothLayers(t *testing.T) {
	p := DefaultParams()
	p.Decay = 1 // plain sums, so credits can be compared exactly
	f := Feat{RoundState: 2, Self: CharFeat{Present: true, State: StStand}, Enemy: CharFeat{Present: true, RelX: 150, State: StStand}}
	k := baselineKey(&f, true, 4)
	comp := Completion{Result: CompKnockdown, Quality: p.quality(CompKnockdown), DamageDealt: 0.2}
	r := p.reward(&comp)
	// run puts the CBR layer's step at position walk among eleven decisions
	// (-1 for none) and resolves the exchange that follows them. It returns
	// the step's credit and the base AI's results by situation.
	run := func(walk int, keys func(i int) uint64) (float32, map[uint64]*BaselineStat) {
		eng := New(p, t.TempDir(), 1)
		eng.SetPlayer(0, "A", "B")
		ps := eng.players[0]
		c := &Case{ID: 99, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, Inputs: []InputBits{InF},
			StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: f}
		ps.st.Admit(p, []*Case{c})
		for i := 0; i <= 10; i++ {
			cc := chainCase{probe: true, key: keys(i), startTick: int64(20 * i)}
			if i == walk {
				cc = chainCase{caseID: 99, startTick: int64(20 * i)}
			}
			ps.ct.addCase(cc)
		}
		ch := &chain{}
		for _, cc := range ps.ct.leadIn {
			ch.add(cc)
		}
		ps.ct.leadIn = ps.ct.leadIn[:0]
		eng.finalizeChain(ps, &resolvedChain{ch: ch, comp: comp})
		return ps.st.playRecord(c).Sum, ps.st.Baseline
	}
	same := func(int) uint64 { return k }

	// The reviewers' case: the step 200 frames before the exchange, the ten
	// decisions of the base AI after it. Only the last leadInMax decisions
	// lead into the exchange: the step is as neutral as the base AI's first
	// six decisions.
	got, base := run(0, same)
	if b := base[k]; got != p.Situation[CompNeutral] || b == nil || b.Trials != 10 || !near(b.ValueSum, leadInMax*r) {
		t.Fatalf("a step long before the exchange is neutral, as the base AI's decisions there are: step %v, base AI %+v (want %v over 10)", got, b, leadInMax*r)
	}

	// Each place before the exchange gets the same credit whoever fills it:
	// the step at position i gets what the base AI's decision there gets
	// when all eleven are the base AI's (each in a situation of its own, to
	// tell them apart).
	own := func(i int) uint64 { return k + uint64(i)<<4 } // a different enemy state type for each
	_, all := run(-1, own)
	for i := 0; i <= 10; i++ {
		got, _ := run(i, same)
		want := all[own(i)].ValueSum
		if !near(got, want) {
			t.Fatalf("position %d: the CBR layer's step gets %v, the base AI's decision there %v", i, got, want)
		}
	}

	// So in the same situation, wherever the step falls, it is worth on
	// average what the base AI's decisions there are worth: leadInMax of
	// the eleven places lead into the exchange.
	var steps, probes float32
	for i := 0; i <= 10; i++ {
		got, _ := run(i, same)
		steps += got
		probes += all[own(i)].ValueSum
	}
	if !near(steps/11, probes/11) || !near(steps/11, leadInMax*r/11) {
		t.Fatalf("the CBR layer's step averages %v, the base AI's decisions %v, want %v", steps/11, probes/11, leadInMax*r/11)
	}

	// With the base AI off there are no probes, and recorded and played
	// cases count together as before.
	q := withoutBaseAI()
	ct := newChainTracker(q, 0, new(uint64))
	var evicted []chainCase
	ct.onEvict = func(cc chainCase, _ int64) { evicted = append(evicted, cc) }
	ct.addCase(chainCase{caseID: 1})
	for i := 0; i < leadInMax; i++ {
		ct.addCase(chainCase{rec: &Case{}})
	}
	if len(evicted) != 1 || evicted[0].caseID != 1 {
		t.Fatalf("with the base AI off, recorded cases push a played one out: %+v", evicted)
	}
	// With it on, recorded cases keep a count of their own.
	ct = newChainTracker(DefaultParams(), 0, new(uint64))
	evicted = nil
	ct.onEvict = func(cc chainCase, _ int64) { evicted = append(evicted, cc) }
	ct.addCase(chainCase{caseID: 1})
	for i := 0; i < leadInMax; i++ {
		ct.addCase(chainCase{rec: &Case{}})
	}
	if len(evicted) != 0 {
		t.Fatalf("with the base AI on, recorded cases count apart from decisions: %+v", evicted)
	}
	ct.addCase(chainCase{probe: true})
	for i := 1; i < leadInMax; i++ {
		ct.addCase(chainCase{caseID: uint64(i + 1)})
	}
	if len(evicted) != 1 || evicted[0].caseID != 1 {
		t.Fatalf("leadInMax decisions of either layer push a played case out: %+v", evicted)
	}
}

// TestHitAndGuardAreApart: being hit and guarding are different situations
// for the base AI's record, and a move recorded while guarding is compared
// with what the base AI does while guarding. Doing nothing in blockstun is
// what the base AI does there too; it must not take over because the base
// AI does badly while being comboed.
func TestHitAndGuardAreApart(t *testing.T) {
	p := DefaultParams()
	guard := Feat{RoundState: 2,
		Self:  CharFeat{Present: true, State: StStand, Move: MtHit, BlockStun: 10, StateNo: 150, StateTime: 3},
		Enemy: CharFeat{Present: true, RelX: 60, State: StStand, Move: MtAttack, HitboxActive: true}}
	hit := guard
	hit.Self.BlockStun, hit.Self.HitStun, hit.Self.StateNo = 0, 10, 5000
	gk, hk := baselineKey(&guard, false, 8), baselineKey(&hit, false, 8)
	if gk == hk || coarseKey(gk) == coarseKey(hk) {
		t.Fatal("being hit and guarding must be apart in the situation and its coarser one")
	}
	for k, want := range map[uint64]string{gk: "self standing and guarding,", hk: "self standing and being hit,", coarseKey(gk): "self standing and guarding,"} {
		if got := SituationString(k); !strings.HasPrefix(got, want) {
			t.Fatalf("SituationString(%#x) = %q, want it to start with %q", k, got, want)
		}
	}
	free := guard
	free.Self = CharFeat{Present: true, State: StStand, Ctrl: true}
	if selfStun(&free) != 0 || selfStun(&guard) != 2 || selfStun(&hit) != 1 {
		t.Fatal("selfStun: 0 free, 1 being hit, 2 guarding")
	}

	// The reviewers' case: the base AI's decisions with the enemy attacking
	// and no control were 32 while being hit (mean -0.079) and 12 while
	// guarding (mean -0.007); the CBR layer holding still in blockstun was
	// played 16 times for 0. Kept together, the base AI's mean would be about
	// -0.06, and holding still would take over by 0.05.
	st := NewStore("x")
	still := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, MoveRef: 150, Inputs: []InputBits{InB, InB, InB},
		StartStateType: StStand, StartCtrl: false, StartMoveType: MtHit, StartStateNo: 150, Start: guard}
	still.Outcome = Outcome{Trials: 1, ValueSum: 0}
	st.Admit(p, []*Case{still})
	setPlayed(p, st, still, 16, 0)
	for i := 0; i < 32; i++ {
		st.addBaseline(p, hk, -0.079, 1)
	}
	for i := 0; i < 12; i++ {
		st.addBaseline(p, gk, -0.007, 1)
	}
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	guarding := &CharSnap{StateNo: 150, StateTime: 3, StateType: StStand, MoveType: MtHit, BlockStun: 10, AILevel: 8}
	if got, _ := d.chooseWhy(&guard, guarding, 1, nil, nil); got != still {
		t.Fatal("setup: the case should match while guarding")
	}
	if b, n := st.baseline(p, gk); b < -0.03 || n < 11 {
		t.Fatalf("the guarding situation's record is the base AI's guarding: mean %.3f over %.1f", b, n)
	}
	if got, _ := d.chooseWhy(&guard, guarding, 1, nil, &gate{key: gk}); got != nil {
		t.Fatal("holding still in blockstun, as the base AI does, must not take over")
	}
	// Mixed into one record, as before, it would.
	mixed := NewStore("y")
	mixed.Admit(p, []*Case{still})
	setPlayed(p, mixed, still, 16, 0)
	for i := 0; i < 32; i++ {
		mixed.addBaseline(p, gk, -0.079, 1)
	}
	for i := 0; i < 12; i++ {
		mixed.addBaseline(p, gk, -0.007, 1)
	}
	dm := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, mixed, newChainTracker(p, 0, new(uint64)))
	if got, _ := dm.chooseWhy(&guard, guarding, 1, nil, &gate{key: gk}); got != still {
		t.Fatal("setup: against the mixed record, holding still would take over")
	}

	// A move recorded while guarding is not considered while being hit, and
	// the other way round: its plays are kept by the stance it was recorded
	// in.
	if playKey(still) == playKey(&Case{Kind: KindMovement, Inputs: still.Inputs, StartStateType: StStand, Start: hit}) {
		t.Fatal("plays while guarding and while being hit must be kept apart")
	}
	why := map[string]int{}
	if got, _ := d.chooseWhy(&guard, guarding, 1, why, &gate{key: hk}); got != nil || why[RejExecCond] != 1 {
		t.Fatalf("a move recorded while guarding is not compared where the character is being hit: %v %v", got, why)
	}
}

// TestMovesAreKeptByStance: the CBR layer's plays of a move are kept by the
// stance it is played from. A step back on the ground and a drift back in
// the air are the same movement, but the drift has never been played: the
// walk's plays prove nothing about it.
func TestMovesAreKeptByStance(t *testing.T) {
	p := DefaultParams()
	ground := Feat{RoundState: 2, Self: CharFeat{Present: true, State: StStand}, Enemy: CharFeat{Present: true, RelX: 60, State: StStand}}
	air := ground
	air.Self.State, air.Self.RelY = StAir, -40
	walkBack := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, MoveRef: 21,
		Inputs: []InputBits{InB, InB, InB}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: ground}
	drift := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, MoveRef: 50,
		Inputs: []InputBits{InB, InB, InB}, StartStateType: StAir, StartCtrl: true, StartMoveType: MtIdle, Start: air}
	walkBack.Outcome = Outcome{Trials: 1, ValueSum: 0.05}
	drift.Outcome = Outcome{Trials: 1, ValueSum: 0.05}
	st := NewStore("x")
	st.Admit(p, []*Case{walkBack, drift})
	if actionKey(walkBack) != actionKey(drift) || playKey(walkBack) == playKey(drift) {
		t.Fatal("setup: the same action, kept apart by stance")
	}
	if !strings.HasSuffix(PlayString(playKey(drift)), "self airborne") || !strings.HasPrefix(PlayString(playKey(drift)), "movement back,") {
		t.Fatalf("PlayString: %q", PlayString(playKey(drift)))
	}
	// Five plays of the walk averaging 0.06; on the ground the base AI does
	// as well, in the air its record is 0.
	st.addPlay(p, walkBack, 0.06)
	for i := 1; i < 5; i++ {
		st.addPlay(p, walkBack, 0.06)
	}
	gk, ak := baselineKey(&ground, true, 8), baselineKey(&air, true, 8)
	setBase(st, gk, 100, 0.06)
	for i := 0; i < 50; i++ {
		st.addBaseline(p, ak, 0, 1)
	}
	d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
	inAir := &CharSnap{StateType: StAir, MoveType: MtIdle, Ctrl: true, AILevel: 8, StateNo: 50}
	onGround := &CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true, AILevel: 8}
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, nil); got != drift {
		t.Fatal("setup: the drift matches in the air")
	}
	if got, _ := d.chooseWhy(&ground, onGround, 1, nil, &gate{key: gk}); got != nil {
		t.Fatal("on the ground the walk is no better than the base AI")
	}
	if rec := st.playRecord(drift); rec.Trials != 0 {
		t.Fatalf("the drift has not been played: %+v", rec)
	}
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, &gate{key: ak}); got != nil {
		t.Fatal("the walk's plays on the ground must not let the drift take over in the air")
	}
	// Played in the air as well as the walk was on the ground, it does.
	setPlayed(p, st, drift, 5, 0.06)
	if got, _ := d.chooseWhy(&air, inAir, 1, nil, &gate{key: ak}); got != drift {
		t.Fatal("the drift's own plays in the air prove it there")
	}

	// A move recorded standing is not played from a crouch: its plays would
	// be kept as standing ones but compared with the base AI crouching.
	crouch := ground
	crouch.Self.State = StCrouch
	crouching := &CharSnap{StateType: StCrouch, MoveType: MtIdle, Ctrl: true, AILevel: 8}
	if got, _ := d.chooseWhy(&crouch, crouching, 1, nil, nil); got != walkBack {
		t.Fatal("setup: without the base AI, the walk matches from a crouch")
	}
	setPlayed(p, st, walkBack, 5, 0.5)
	if got, _ := d.chooseWhy(&crouch, crouching, 1, nil, &gate{key: baselineKey(&crouch, true, 8)}); got != nil {
		t.Fatal("with the base AI, a move takes over only in the stance it was recorded in")
	}
}

// TestOneExchangeProvesNothingInNeutral: where the base AI's decisions
// seldom lead into an exchange (here 2% of an exchange's reward on
// average), the evidence is counted in exchanges: ten plays, one of which
// led into an exchange worth 0.05, are a fifth of an exchange's worth, and
// do not prove a move at Medium or High.
func TestOneExchangeProvesNothingInNeutral(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	for _, k := range []uint64{key, coarseKey(key), baseAll} {
		d.st.Baseline[k] = &BaselineStat{Trials: 200, ValueSum: 0, Shares: 4}
	}
	for i := 0; i < 10; i++ {
		r := float32(0)
		if i == 4 {
			r = 0.05
		}
		d.st.addPlay(p, c, r)
	}
	share := d.st.baselineShare(p, key)
	if share < 0.019 || share > 0.021 {
		t.Fatalf("setup: share %v, want about 0.02", share)
	}
	rec := d.st.playRecord(c)
	for lvl := 1; lvl <= 3; lvl++ {
		d.margin = p.takeoverMargin(lvl)
		got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key})
		_, margin, _, _ := d.st.bar(p, key, d.margin)
		t.Logf("setting %d: margin %.3f per exchange, %.5f per decision; advantage %.5f (plays shrunk as plays, as before: %.5f); takes over: %v",
			lvl, d.margin, margin, p.advantage(rec, 0, share), rec.Sum/(p.TakeoverShrink+rec.Trials), got == c)
		if lvl >= 2 && got != nil {
			t.Fatalf("setting %d: one exchange among ten plays in neutral must not prove a move", lvl)
		}
	}
}

// TestContestedMovesAreNotTriedAgain: when the character's own AI takes over
// from a move the CBR layer is trying (an AI that cannot be paused, starting
// an attack of its own), the play counts for nothing. After
// TakeoverContestMax such plays the move is not tried again, so that the
// exploration is not spent on plays that can never count; a move that has
// proved better still takes over.
func TestContestedMovesAreNotTriedAgain(t *testing.T) {
	p := DefaultParams()
	d, c, cur, self, key := gateFixture(p)
	setBase(d.st, key, 10, 0)
	contest := func() {
		d.cur = c
		d.contest()
		d.waitCtrl = false
	}
	for i := 0; i < int(p.TakeoverContestMax); i++ {
		if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key, explore: true}); got != c {
			t.Fatalf("contest %d: the move should still be tried", i)
		}
		contest()
	}
	if rec := d.st.playRecord(c); rec.Trials != 0 || rec.Contests < p.TakeoverContestMax-0.1 {
		t.Fatalf("contested plays are noted and count for nothing: %+v", rec)
	}
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key, explore: true, promising: true}); got != nil {
		t.Fatal("a move contested TakeoverContestMax times is not tried again")
	}
	// Nor is a move played a few times without proving better tried again.
	setPlayed(p, d.st, c, 3, 0.02)
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key, explore: true, retry: true}); got != nil {
		t.Fatal("a contested move is not retried")
	}
	// A move that proved better still takes over.
	setPlayed(p, d.st, c, 5, 0.2)
	if got, _ := d.chooseWhy(cur, self, 1, nil, &gate{key: key}); got != c {
		t.Fatal("a move that proved better takes over, contested or not")
	}
	// With the base AI off nothing is noted.
	q := withoutBaseAI()
	d, c, _, _, _ = gateFixture(q)
	d.cur = c
	d.contest()
	if len(d.st.Played) != 0 {
		t.Fatal("with the base AI off, contests are not noted")
	}
}
