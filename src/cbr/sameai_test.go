package cbr

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"testing"
)

// Same-AI tests of the base AI (see baseline.go): the store is learned from
// a player, and that same player is the base AI, so the CBR layer's moves
// are the base AI's own and none of them is really better than it. The
// players here spend a long time in neutral, where most decisions never lead
// into an exchange: they back off, wait or walk about before coming in. A
// move must not prove better there by the way it is measured, and the CBR
// layer must leave nearly all the play to the base AI.

// zoner keeps away, waits at range, then walks in, jabs and cancels into the
// special when the jab hits.
type zoner struct {
	rng   *rand.Rand
	phase int
	wait  int
	plan  []InputBits
}

func (z *zoner) next(s *sim, me int) InputBits {
	if len(z.plan) > 0 {
		b := z.plan[0]
		z.plan = z.plan[1:]
		return b
	}
	c, o := s.ch[me], s.ch[1-me]
	if c.state == 200 && c.contacted && c.moveHit > 0 {
		z.plan = []InputBits{InD | InF, InF | InA}
		return InD
	}
	if !c.ctrl {
		return 0
	}
	dist := absf(o.x - c.x)
	switch z.phase {
	case 0: // back off to range
		if dist < 220 && c.x > -290 && c.x < 290 {
			return InB
		}
		z.phase, z.wait = 1, 60+z.rng.Intn(60)
		return 0
	case 1: // wait
		if z.wait > 0 {
			z.wait--
			return 0
		}
		z.phase = 2
		return InF
	}
	// Come in and jab.
	if o.state == 5100 || o.state == 5120 {
		z.phase = 0
		return InB
	}
	if dist > 62 {
		return InF
	}
	if c.input&InA != 0 {
		return 0
	}
	z.phase = 0
	return InA
}

// sparse stands, and now and then pokes when close; it guards nothing.
type sparse struct{ rng *rand.Rand }

func (d *sparse) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	if !c.ctrl {
		return 0
	}
	if absf(o.x-c.x) < 65 && d.rng.Intn(40) == 0 {
		if c.input&InA != 0 {
			return 0
		}
		return InA
	}
	return 0
}

// spacer is the rushdown with a neutral dance: on a quarter of its
// approaches it backs off for a while before coming in again.
type spacer struct {
	rng  *rand.Rand
	back int
	plan []InputBits
}

func (r *spacer) next(s *sim, me int) InputBits {
	if len(r.plan) > 0 {
		b := r.plan[0]
		r.plan = r.plan[1:]
		return b
	}
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	if c.state == 200 && c.contacted && c.moveHit > 0 {
		r.plan = []InputBits{InD | InF, InF | InA}
		return InD
	}
	if !c.ctrl {
		return 0
	}
	if r.back > 0 {
		r.back--
		return InB
	}
	switch {
	case o.state == 5100 || o.state == 5120:
		if dist > 50 {
			return InF
		}
		return 0
	case dist > 62:
		return InF
	case r.rng.Intn(4) == 0:
		r.back = 25 + r.rng.Intn(30)
		return InB
	case r.rng.Intn(6) == 0:
		return InD | InBtnB
	default:
		if c.input&InA != 0 {
			return 0
		}
		return InA
	}
}

// dancer is the spacer with a longer dance more often: half its approaches
// back off for 40 to 80 frames first.
type dancer struct{ spacer }

func (r *dancer) next(s *sim, me int) InputBits {
	if len(r.plan) > 0 || r.back > 0 {
		return r.spacer.next(s, me)
	}
	c, o := s.ch[me], s.ch[1-me]
	if c.ctrl && !(c.state == 200 && c.contacted) && absf(o.x-c.x) <= 62 && o.state != 5100 && o.state != 5120 && r.rng.Intn(2) == 0 {
		r.back = 40 + r.rng.Intn(40)
		return InB
	}
	return r.spacer.next(s, me)
}

// hesitant is the rushdown that misses one cancel in five: when its jab
// hits, it lets the hit go by without the special one time in five. The
// rushdown's own cancel is then a little better than it, by about a fifth
// of what a cancel adds.
type hesitant struct {
	rushdown
	skip bool // let this jab's hit go by
}

func (h *hesitant) next(s *sim, me int) InputBits {
	if c := s.ch[me]; len(h.plan) == 0 && c.state == 200 && c.contacted && c.moveHit > 0 {
		if c.moveHit == 1 {
			h.skip = h.rng.Intn(5) == 0
		}
		if h.skip {
			return 0
		}
	}
	return h.rushdown.next(s, me)
}

var (
	baseHesitant oppFactory = func(r *rand.Rand) controller { return &hesitant{rushdown: rushdown{rng: r}} }
	baseZoner    oppFactory = func(r *rand.Rand) controller { return &zoner{rng: r} }
	baseSpacer   oppFactory = func(r *rand.Rand) controller { return &spacer{rng: r} }
	baseDancer   oppFactory = func(r *rand.Rand) controller { return &dancer{spacer{rng: r}} }
	oppSparse    oppFactory = func(r *rand.Rand) controller { return &sparse{rng: r} }
)

// gateScenario is a store learned from src against opp, then base playing
// against opp with the CBR layer over it.
type gateScenario struct {
	name           string
	src, base, opp oppFactory
	learn          int // learning rounds
}

// gateRun is what a scenario measured over its evaluation rounds.
type gateRun struct {
	alone, net float32 // net damage per round: the base AI alone, and with the CBR layer
	rounds     int
	frames     int
	driven     int // frames the CBR layer gave the input
	explores   int // its explorations
	dec        *decisions
}

func (r *gateRun) drivenShare() float32 { return float32(r.driven) / float32(max(r.frames, 1)) }

func (r *gateRun) explorePerRound() float32 { return float32(r.explores) / float32(max(r.rounds, 1)) }

// recordVsOpp records player 0 played by ctl against opp as a person's play.
func recordVsOpp(t *testing.T, eng *Engine, rounds int, seed int64, ctl, opp oppFactory) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		if err := eng.Record(0, SrcHumanLocal); err != nil {
			t.Fatal(err)
		}
		runRound(s, eng, [2]controller{ctl(rand.New(rand.NewSource(seed + int64(r)))), opp(rand.New(rand.NewSource(seed + 77 + int64(r))))}, 3600)
	}
}

// evalOverBase plays n rounds as overBaseAt does, on the same seeds, and
// measures them.
func evalOverBase(eng *Engine, n, seedBase, margin int, base, opp oppFactory) *gateRun {
	run := &gateRun{dec: &decisions{by: map[uint64][2]int{}}}
	var res []roundResult
	for r := 0; r < n; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		eng.SetTakeoverMargin(0, margin)
		ps := eng.players[0]
		final := ps.ct.onFinal
		ps.ct.onFinal = func(rc *resolvedChain) {
			for _, cc := range rc.ch.cases {
				switch {
				case cc.probe:
					run.dec.probes++
					v := run.dec.by[coarseKey(cc.key)]
					v[1]++
					run.dec.by[coarseKey(cc.key)] = v
				case cc.caseID != 0:
					run.dec.plays++
					if c := ps.st.Get(cc.caseID); c != nil {
						k := coarseKey(situationOf(playKey(c), s.ch[0].ai))
						v := run.dec.by[k]
						v[0]++
						run.dec.by[k] = v
					}
				}
			}
			final(rc)
		}
		ctl := opp(rand.New(rand.NewSource(int64(seedBase + r))))
		b := base(rand.New(rand.NewSource(int64(seedBase + 500 + r))))
		eng.Frame(s.snapshot())
		for f := 0; f < 3600 && s.roundSt == 2; f++ {
			var in [2]InputBits
			s.ch[0].cbr = false
			if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
				in[0] = FromEngine(btn, s.ch[0].facingRight)
				s.ch[0].cbr = true
				run.driven++
			} else {
				in[0] = b.next(s, 0)
			}
			in[1] = ctl.next(s, 1)
			run.frames++
			s.step(in)
			eng.Frame(s.snapshot())
		}
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		run.explores += int(ps.drv.explores)
		eng.RoundEnd(s.snapshot())
		res = append(res, result(s))
	}
	run.rounds = n
	run.net, _, _ = summarize(res)
	return run
}

// playScenario records the scenario's player, learns over the base AI and
// evaluates, at a take-over margin setting and seed.
func playScenario(t *testing.T, sc gateScenario, seed int64, margin int) (*Engine, *gateRun) {
	eng := New(DefaultParams(), t.TempDir(), seed)
	recordVsOpp(t, eng, 10, 40+seed, sc.src, sc.opp)
	overBaseAt(eng, sc.learn, 9000+int(seed)*100, Levels{Execution: 8, Knowledge: 8}, margin, sc.base, sc.opp)
	run := evalOverBase(eng, 6, 7000, margin, sc.base, sc.opp)
	run.alone, _, _ = summarize(baseOnly(6, 7000, sc.base, sc.opp))
	return eng, run
}

// proven lists the moves the store's plays prove better than the base AI at
// a margin setting, each compared as the gate does in the situation its case
// was recorded in, for situations the keep function accepts.
func proven(eng *Engine, margin int, keep func(sit uint64) bool) []string {
	st, _ := eng.Store("Rush")
	p := eng.Params()
	minPlays := p.decayed(p.TakeoverMinPlays) - 1e-3
	seen := map[uint64]bool{}
	var out []string
	for _, c := range st.Cases {
		k := playKey(c)
		if seen[k] {
			continue
		}
		seen[k] = true
		sit := situationOf(k, 0)
		rec := st.playRecord(c)
		if !keep(sit) || rec.Trials < minPlays {
			continue
		}
		b, m, share, _ := st.bar(p, sit, p.takeoverMargin(margin))
		if adv := p.advantage(rec, b, share); adv > m {
			out = append(out, fmt.Sprintf("%s: %.1f plays, mean %.4f against the base AI's %.4f (share %.2f): advantage %.4f, margin %.4f",
				PlayString(k), rec.Trials, rec.Sum/rec.Trials, b, share, adv, m))
		}
	}
	sort.Strings(out)
	return out
}

// outsideCombos accepts the situations where the character is not hitting
// or pressuring the enemy: the enemy free, or attacking.
func outsideCombos(sit uint64) bool { return sit>>8&3 <= 1 }

// TestSameAINeutralProvesNothing: players that spend a long time in neutral
// are both the source of the store and the base AI. At the Medium and High
// margins no move outside the character's own combos and pressure proves
// better than the base AI (they are its own moves), and the CBR layer gives
// the input on only a small share of frames, mostly to try moves now and
// then. Low is reported too: it accepts smaller edges, by design.
func TestSameAINeutralProvesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	scenarios := []gateScenario{
		{"zoner", baseZoner, baseZoner, oppSparse, 30},
		{"dancer", baseDancer, baseDancer, oppDefender, 30},
	}
	seeds := []int64{7, 9}
	type found struct {
		run    *gateRun
		proven []string
	}
	got := map[string]*found{}
	name := func(sc gateScenario, seed int64, margin int) string {
		return fmt.Sprintf("%s/seed%d/margin%d", sc.name, seed, margin)
	}
	t.Run("runs", func(t *testing.T) {
		for _, sc := range scenarios {
			for _, seed := range seeds {
				for margin := 1; margin <= 3; margin++ {
					sc, seed, margin := sc, seed, margin
					r := &found{}
					got[name(sc, seed, margin)] = r
					t.Run(name(sc, seed, margin), func(t *testing.T) {
						t.Parallel()
						eng, run := playScenario(t, sc, seed, margin)
						r.run, r.proven = run, proven(eng, margin, outsideCombos)
					})
				}
			}
		}
	})
	for _, sc := range scenarios {
		for _, seed := range seeds {
			for margin := 1; margin <= 3; margin++ {
				r := got[name(sc, seed, margin)]
				if r.run == nil {
					t.Fatal("a run did not finish")
				}
				t.Logf("%s: net per round %.0f (base AI alone %.0f); CBR layer drove %.1f%% of frames, %.1f%% of decisions (follow-ups %.0f%%), %.1f explorations a round; %d moves outside combos proved better",
					name(sc, seed, margin), r.run.net, r.run.alone, 100*r.run.drivenShare(), 100*r.run.dec.share(), 100*r.run.dec.share(comboSit),
					r.run.explorePerRound(), len(r.proven))
				for _, s := range r.proven {
					t.Logf("    proved better: %s", s)
				}
				if margin == 1 {
					continue
				}
				if len(r.proven) > 0 {
					t.Errorf("%s: the base AI's own moves outside combos proved better than it:\n%s", name(sc, seed, margin), strings.Join(r.proven, "\n"))
				}
				if r.run.drivenShare() > 0.03 {
					t.Errorf("%s: the CBR layer drove %.1f%% of frames over its own moves", name(sc, seed, margin), 100*r.run.drivenShare())
				}
			}
		}
	}
}

// TestGateTable measures the comparison with the base AI in every scenario
// at every margin setting, for tuning. Set CBR_GATE_TABLE to run it.
func TestGateTable(t *testing.T) {
	if os.Getenv("CBR_GATE_TABLE") == "" {
		t.Skip("set CBR_GATE_TABLE to measure")
	}
	scenarios := []gateScenario{
		{"poker + CBR (store from rushdown)", baseRushdown, basePoker, oppDefender, 30},
		{"rushdown + CBR (store from poker)", basePoker, baseRushdown, oppDefender, 12},
		{"rushdown, same AI", baseRushdown, baseRushdown, oppDefender, 30},
		{"zoner, same AI", baseZoner, baseZoner, oppSparse, 30},
		{"spacer, same AI", baseSpacer, baseSpacer, oppDefender, 30},
		{"dancer, same AI", baseDancer, baseDancer, oppDefender, 30},
	}
	if s := os.Getenv("CBR_GATE_TABLE"); s != "all" {
		var keep []gateScenario
		for _, sc := range scenarios {
			if strings.Contains(sc.name, s) {
				keep = append(keep, sc)
			}
		}
		scenarios = keep
	}
	type row struct {
		run    *gateRun
		proven []string
	}
	rows := make([][2][4]row, len(scenarios))
	t.Run("runs", func(t *testing.T) {
		for i, sc := range scenarios {
			for j, seed := range []int64{7, 9} {
				for margin := 1; margin <= 3; margin++ {
					i, j, sc, seed, margin := i, j, sc, seed, margin
					t.Run(fmt.Sprintf("%d/seed%d/margin%d", i, seed, margin), func(t *testing.T) {
						t.Parallel()
						eng, run := playScenario(t, sc, seed, margin)
						rows[i][j][margin] = row{run, proven(eng, margin, func(uint64) bool { return true })}
					})
				}
			}
		}
	})
	t.Log("| Scenario | Seed | Base alone | Setting | Net per round | Driven frames | Share of decisions | Follow-ups | Explorations a round | Proved better |")
	t.Log("|---|---|---|---|---|---|---|---|---|---|")
	for i, sc := range scenarios {
		for j, seed := range []int64{7, 9} {
			for margin := 1; margin <= 3; margin++ {
				r := rows[i][j][margin]
				if r.run == nil {
					continue
				}
				t.Logf("| %s | %d | %.0f | %s | %.0f | %.1f%% | %.2f%% | %.0f%% | %.1f | %d |", sc.name, seed, r.run.alone,
					[...]string{"", "Low", "Medium", "High"}[margin], r.run.net, 100*r.run.drivenShare(), 100*r.run.dec.share(),
					100*r.run.dec.share(comboSit), r.run.explorePerRound(), len(r.proven))
				for _, s := range r.proven {
					t.Logf("    %s", s)
				}
			}
		}
	}
}

// TestContestedExplorationsStop: with a base AI that cannot be paused (it
// keeps itself on in a variable, and starts attacks of its own during the
// CBR layer's moves), a move it keeps taking over from is not explored more
// than TakeoverContestMax times, so the exploration is not spent round after
// round on plays that can never count.
func TestContestedExplorationsStop(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	p := DefaultParams()
	eng := New(p, t.TempDir(), 7)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcHumanLocal, 8, 40, func(r *rand.Rand) controller { return &rushdown{rng: r} })
	contested := map[uint64]int{} // contested explorations, by the move's played record
	var early, late, explores int
	const rounds = 24
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		s.cmdLayer = true
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		base := &latched{stateChanger: stateChanger{rng: rand.New(rand.NewSource(int64(600 + r)))}}
		opp := &defender{rng: rand.New(rand.NewSource(int64(700 + r)))}
		eng.Frame(s.snapshot())
		var exploring *Case // the move being tried by exploration, if any
		seen := int32(0)
		for f := 0; f < 3600 && s.roundSt == 2; f++ {
			me := s.ch[0]
			me.cbr = false
			in := [2]InputBits{0, opp.next(s, 1)}
			if btn, ok := eng.Input(0, me.facingRight); ok {
				in[0] = FromEngine(btn, me.facingRight)
				me.cbr = true
				if d := ps.drv; d.explores > seen {
					seen, exploring = d.explores, d.cur
				}
				base.contend(s, 0)
			} else {
				in[0] = base.next(s, 0)
			}
			s.step(in)
			before := ps.drv.cur
			eng.Frame(s.snapshot())
			if before != nil && before == exploring && ps.drv.cur == nil && ps.drv.waitCtrl {
				contested[playKey(before)]++
				if r < rounds/2 {
					early++
				} else {
					late++
				}
				exploring = nil
			}
			if ps.drv.cur != exploring {
				exploring = nil
			}
		}
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		explores += int(ps.drv.explores)
		eng.RoundEnd(s.snapshot())
	}
	t.Logf("%d explorations in %d rounds; contested: %d in the first half, %d in the second, over %d moves", explores, rounds, early, late, len(contested))
	if early == 0 {
		t.Fatal("setup: the base AI should take over from some explorations")
	}
	for k, n := range contested {
		if n > int(p.TakeoverContestMax) {
			t.Fatalf("%s was explored and taken over from %d times, more than TakeoverContestMax (%v)", PlayString(k), n, p.TakeoverContestMax)
		}
	}
	if late >= early {
		t.Fatalf("contested explorations should become rarer: %d, then %d", early, late)
	}
}
