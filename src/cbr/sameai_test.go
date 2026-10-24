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

// gateRounds plays n rounds as overBaseAt does, on the same seeds (so a
// round plays out exactly as there), and measures them. With a watch, every
// take-over is checked as it begins (see takeoverWatch).
func gateRounds(eng *Engine, n, seedBase, margin int, base, opp oppFactory, w *takeoverWatch) *gateRun {
	run := &gateRun{dec: &decisions{by: map[uint64][2]int{}}}
	var res []roundResult
	for r := 0; r < n; r++ {
		s := newSim(3600)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		eng.SetTakeoverMargin(0, margin)
		ps := eng.players[0]
		run.dec.hook(ps)
		if w != nil {
			w.sh.hook(ps)
		}
		ctl := opp(rand.New(rand.NewSource(int64(seedBase + r))))
		b := base(rand.New(rand.NewSource(int64(seedBase + 500 + r))))
		eng.Frame(s.snapshot())
		var last *Case
		var lastPlays, explores int32
		for f := 0; f < 3600 && s.roundSt == 2; f++ {
			var in [2]InputBits
			s.ch[0].cbr = false
			if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
				in[0] = FromEngine(btn, s.ch[0].facingRight)
				s.ch[0].cbr = true
				run.driven++
				if d := ps.drv; d.cur != nil && (d.cur != last || d.cur.Outcome.Plays != lastPlays) {
					last, lastPlays = d.cur, d.cur.Outcome.Plays
					if w != nil {
						w.check(eng, ps, d.cur, d.explores > explores)
					}
					explores = d.explores
				}
			} else {
				in[0] = b.next(s, 0)
				if w != nil {
					w.sh.ended(ps)
				}
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
		if w != nil {
			w.sh.ended(ps)
		}
		res = append(res, result(s))
	}
	run.rounds = n
	run.net, _, _ = summarize(res)
	return run
}

// takeoverWatch checks every take-over the gate let through as proved better
// (every one that is not an exploration) in the situations keep accepts:
// the tests' own account of the move's plays in the situation it took over
// in (see playShadow) must prove it better there than the base AI, as the
// gate measures (see Params.proves and Params.advantage, and Store.against
// for the base AI's side).
type takeoverWatch struct {
	sh       *playShadow
	keep     func(sit uint64) bool
	proved   int // take-overs checked
	unproven int // of them, those the move's own plays there do not prove
	bad      []string
}

func newTakeoverWatch(p *Params, keep func(sit uint64) bool) *takeoverWatch {
	return &takeoverWatch{sh: newPlayShadow(p), keep: keep}
}

// check notes a play of c the CBR layer has just begun, and checks it when it
// is a take-over that is not an exploration.
func (w *takeoverWatch) check(eng *Engine, ps *playerState, c *Case, explored bool) {
	sit := w.sh.began(ps, c)
	if explored || !w.keep(sit) {
		return
	}
	w.proved++
	p := eng.p
	k := playKey(c, sit)
	rec := w.sh.record(k)
	cmp := ps.st.against(p, k, ps.drv.margin)
	if p.proves(rec) && p.beats(cmp, rec, 1) {
		return
	}
	w.unproven++
	if len(w.bad) < 8 {
		w.bad = append(w.bad, fmt.Sprintf("%s: its own plays there %.1f (%.1f exchanges' worth), mean %.4f; the store's record %+v; base AI %+v",
			PlayString(k), rec.Trials, rec.Shares, rec.Sum/max(rec.Trials, 1e-6), ps.st.playRecord(k), cmp.bases[:cmp.n]))
	}
}

// playScenario records the scenario's player, learns over the base AI and
// evaluates, at a take-over margin setting and seed, watching every
// take-over in both with w when it is not nil.
func playScenario(t *testing.T, sc gateScenario, seed int64, margin int, w *takeoverWatch) (*Engine, *gateRun) {
	eng := New(DefaultParams(), t.TempDir(), seed)
	recordVsOpp(t, eng, 10, 40+seed, sc.src, sc.opp)
	gateRounds(eng, sc.learn, 9000+int(seed)*100, margin, sc.base, sc.opp, w)
	run := gateRounds(eng, 6, 7000, margin, sc.base, sc.opp, w)
	run.alone, _, _ = summarize(baseOnly(6, 7000, sc.base, sc.opp))
	return eng, run
}

// proven lists the moves the store's plays prove better than the base AI at
// a margin setting, each in the situation its plays were made in, as the
// gate compares them, for situations the keep function accepts.
func proven(eng *Engine, margin int, keep func(sit uint64) bool) []string {
	st, _ := eng.Store("Rush")
	p := eng.Params()
	var out []string
	for k, rec := range st.Played {
		if !keep(k&sitMask) || !p.proves(*rec) {
			continue
		}
		cmp := st.against(p, k, p.takeoverMargin(margin))
		if p.beats(cmp, *rec, 1) {
			s := fmt.Sprintf("%s: %.1f plays (%.1f exchanges' worth), mean %.4f", PlayString(k), rec.Trials, rec.Shares, rec.Sum/rec.Trials)
			for _, x := range cmp.bases[:cmp.n] {
				s += fmt.Sprintf("; against the base AI's %.4f (share %.2f): advantage %.4f, margin %.4f", x.b, x.share, p.advantage(*rec, x.b, x.share), x.margin)
			}
			out = append(out, s)
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
// then. Every take-over the gate let through as proved, in learning and in
// evaluation, must also be proved by the move's own plays in the situation
// it took over in, as the tests count them (see takeoverWatch): plays made
// elsewhere prove nothing there. Low is reported too for the zoner and the
// dancer: it accepts smaller edges, by design.
func TestSameAINeutralProvesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	scenarios := []gateScenario{
		{"zoner", baseZoner, baseZoner, oppSparse, 30},
		{"dancer", baseDancer, baseDancer, oppDefender, 30},
		{"spacer", baseSpacer, baseSpacer, oppDefender, 30},
		{"rushdown", baseRushdown, baseRushdown, oppDefender, 30},
	}
	// The lowest margin setting played for each scenario.
	lowest := map[string]int{"zoner": 1, "dancer": 1, "spacer": 2, "rushdown": 2}
	seeds := []int64{7, 9}
	type found struct {
		run    *gateRun
		proven []string
		watch  *takeoverWatch
	}
	got := map[string]*found{}
	name := func(sc gateScenario, seed int64, margin int) string {
		return fmt.Sprintf("%s/seed%d/margin%d", sc.name, seed, margin)
	}
	t.Run("runs", func(t *testing.T) {
		for _, sc := range scenarios {
			for _, seed := range seeds {
				for margin := lowest[sc.name]; margin <= 3; margin++ {
					sc, seed, margin := sc, seed, margin
					r := &found{watch: newTakeoverWatch(DefaultParams(), outsideCombos)}
					got[name(sc, seed, margin)] = r
					t.Run(name(sc, seed, margin), func(t *testing.T) {
						t.Parallel()
						eng, run := playScenario(t, sc, seed, margin, r.watch)
						r.run, r.proven = run, proven(eng, margin, outsideCombos)
					})
				}
			}
		}
	})
	for _, sc := range scenarios {
		for _, seed := range seeds {
			for margin := lowest[sc.name]; margin <= 3; margin++ {
				r := got[name(sc, seed, margin)]
				if r.run == nil {
					t.Fatal("a run did not finish")
				}
				t.Logf("%s: net per round %.0f (base AI alone %.0f); CBR layer drove %.1f%% of frames, %.1f%% of decisions (follow-ups %.0f%%), %.1f explorations a round; %d moves outside combos proved better; %d take-overs outside combos let through as proved over %d rounds, %d not proved by the move's own plays there",
					name(sc, seed, margin), r.run.net, r.run.alone, 100*r.run.drivenShare(), 100*r.run.dec.share(), 100*r.run.dec.share(comboSit),
					r.run.explorePerRound(), len(r.proven), r.watch.proved, sc.learn+r.run.rounds, r.watch.unproven)
				for _, s := range r.proven {
					t.Logf("    proved better: %s", s)
				}
				for _, s := range r.watch.bad {
					t.Logf("    not proved there: %s", s)
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
				if r.watch.unproven > 0 {
					t.Errorf("%s: %d take-overs outside combos with a move its own plays there did not prove better:\n%s",
						name(sc, seed, margin), r.watch.unproven, strings.Join(r.watch.bad, "\n"))
				}
			}
		}
	}
}

// TestSameAICancelIsNoBetter: the rushdown is both the source of the store
// and the base AI, so the CBR layer's cancel into the special is the base
// AI's own. Where the CBR layer plays the cancel, the base AI's result for
// that hit is missing from its record of the follow-ups, so that record can
// lean toward the hits where no cancel starts; compared with it alone, the
// cancel once looked better than the base AI's own cancel (round 2, at the
// Low margin, with up to 6 explorations and 3 retries a round). Compared
// where the cancel was on offer as well (see Store.against), it does not:
// learning with those exploration caps at Low, no record of the cancel
// proves better, and with exploration off the CBR layer takes over on
// hardly any follow-up.
func TestSameAICancelIsNoBetter(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	cancel := uint64(1000) << 1 // moveKey of a command case into state 1000
	for _, seed := range []int64{7, 9} {
		seed := seed
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			t.Parallel()
			p := DefaultParams()
			p.TakeoverExploreMax, p.TakeoverRetryMax = 6, 3
			eng := New(p, t.TempDir(), seed)
			recordAs(t, eng, 10, 40+seed, baseRushdown)
			overBaseAt(eng, 30, 9000+int(seed)*100, Levels{Execution: 8, Knowledge: 8}, 1, baseRushdown, oppDefender)
			st, _ := eng.Store("Rush")
			m := p.takeoverMargin(1)
			found := 0
			for k, a := range st.Played {
				if k>>sitBits != cancel || k&(1<<11) != 0 || a.Trials == 0 {
					continue
				}
				found++
				cmp := st.against(p, k, m)
				t.Logf("%s: %.1f plays, mean %.4f; the base AI there %+v", PlayString(k), a.Trials, a.Sum/a.Trials, cmp.bases[:cmp.n])
				if p.proves(*a) && p.beats(cmp, *a, 1) {
					t.Errorf("the base AI's own cancel proved better than the base AI at Low: %s", PlayString(k))
				}
			}
			if found == 0 {
				t.Fatal("setup: the cancel should have been tried in the follow-ups")
			}
			p.TakeoverExplore, p.TakeoverExplorePromising = 0, 0
			_, dec := overBaseAt(eng, 6, 7000, Levels{Execution: 8, Knowledge: 8}, 1, baseRushdown, oppDefender)
			t.Logf("exploration off: %.1f%% of decisions, follow-ups %.0f%%", 100*dec.share(), 100*dec.share(comboSit))
			if dec.share(comboSit) > 0.05 {
				t.Errorf("the CBR layer took over on %.0f%% of follow-ups from the base AI's own cancel", 100*dec.share(comboSit))
			}
		})
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
		watch  *takeoverWatch
	}
	rows := make([][2][4]row, len(scenarios))
	all := func(uint64) bool { return true }
	t.Run("runs", func(t *testing.T) {
		for i, sc := range scenarios {
			for j, seed := range []int64{7, 9} {
				for margin := 1; margin <= 3; margin++ {
					i, j, sc, seed, margin := i, j, sc, seed, margin
					t.Run(fmt.Sprintf("%d/seed%d/margin%d", i, seed, margin), func(t *testing.T) {
						t.Parallel()
						w := newTakeoverWatch(DefaultParams(), all)
						eng, run := playScenario(t, sc, seed, margin, w)
						rows[i][j][margin] = row{run, proven(eng, margin, all), w}
					})
				}
			}
		}
	})
	t.Log("| Scenario | Seed | Base alone | Setting | Net per round | Driven frames | Share of decisions | Follow-ups | Explorations a round | Proved better | Proved take-overs (not proved there) |")
	t.Log("|---|---|---|---|---|---|---|---|---|---|---|")
	for i, sc := range scenarios {
		for j, seed := range []int64{7, 9} {
			for margin := 1; margin <= 3; margin++ {
				r := rows[i][j][margin]
				if r.run == nil {
					continue
				}
				t.Logf("| %s | %d | %.0f | %s | %.0f | %.1f%% | %.2f%% | %.0f%% | %.1f | %d | %d (%d) |", sc.name, seed, r.run.alone,
					[...]string{"", "Low", "Medium", "High"}[margin], r.run.net, 100*r.run.drivenShare(), 100*r.run.dec.share(),
					100*r.run.dec.share(comboSit), r.run.explorePerRound(), len(r.proven), r.watch.proved, r.watch.unproven)
				for _, s := range r.proven {
					t.Logf("    %s", s)
				}
				for _, s := range r.watch.bad {
					t.Logf("    not proved there: %s", s)
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
				contested[playKey(before, ps.drv.curKey)]++
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
