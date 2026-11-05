package cbr

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// Tests of taking over during and after movement (see chooseWhy and
// driver.input): when the base AI walks, runs or jumps in, the CBR layer
// takes over at the distance where a move of its own proved better than the
// base AI there, on the first frame or two in that distance band; a closer
// case of the walk itself does not hide that move; with the same AI on both
// sides nothing takes over during movement; and in a movement without
// control (a dash) only a command verified from that very state takes over.

// approach is how a mover comes in on the enemy.
type approach int

const (
	byWalk approach = iota
	byRun
	byDash
	byJump
)

func (a approach) String() string { return [...]string{"walk", "run", "dash", "jump"}[a] }

// setup switches on in s what the approach needs (see sim.run, sim.dash and
// sim.air), with the command layer and the engine's labels.
func (a approach) setup(s *sim) {
	s.cmdLayer, s.entries = true, true
	switch a {
	case byRun:
		s.run = true
	case byDash:
		s.dash = true
	case byJump:
		s.air = true
	}
}

// Where a mover acts: it stops and pokes within moverNear pixels, runs from
// farther than runFrom, dashes from dashFrom or closer, jumps from jumpFrom
// or closer, and after stayFrames close by backs off for a while.
const (
	moverNear  = 45
	runFrom    = 100
	dashFrom   = 140
	jumpFrom   = 120
	stayFrames = 50
)

// mover is a base AI that comes in on the enemy by its approach from far,
// pokes with the jab now and then when close, and after a while backs off to
// come in again: walking, running (a double tap forward once far), dashing
// (a double tap once within dashFrom) or jumping forward (once within
// jumpFrom, doing nothing in the air). Against an enemy that guards high
// attacks its jab does little. With sweep set it sweeps (down and B) from
// sweepFrom pixels instead of coming in any closer, and with airAttack it
// presses A in the air within airFrom pixels: the demonstrators of the
// better move at a distance.
type mover struct {
	rng       *rand.Rand
	how       approach
	sweep     bool
	airAttack bool
	plan      []InputBits
	stay      int // frames spent close by
	back      int // frames still to back off
}

const (
	sweepFrom = 72
	airFrom   = 60
)

func (m *mover) next(s *sim, me int) InputBits {
	if len(m.plan) > 0 {
		b := m.plan[0]
		m.plan = m.plan[1:]
		return b
	}
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	if c.st == StAir {
		if m.airAttack && c.ctrl && dist <= airFrom && c.input&InA == 0 {
			return InA
		}
		return 0
	}
	if !c.ctrl {
		return 0
	}
	switch {
	case m.back > 0:
		m.back--
		return InB
	case m.sweep && dist <= sweepFrom:
		m.back = 40 + m.rng.Intn(30)
		return InD | InBtnB
	case dist <= moverNear:
		if m.stay++; m.stay > stayFrames {
			m.stay, m.back = 0, 40+m.rng.Intn(30)
			return InB
		}
		if m.rng.Intn(10) == 0 && c.input&InA == 0 {
			return InA
		}
		return 0
	}
	switch m.how {
	case byRun:
		if c.state != 100 && dist > runFrom {
			m.plan = []InputBits{InF} // forward, let go, forward: the double tap
			return 0
		}
	case byDash:
		if dist <= dashFrom && dist > moverNear+dashFrames*dashSpeed/2 {
			m.plan = []InputBits{InF}
			return 0
		}
	case byJump:
		if dist <= jumpFrom {
			return InU | InF
		}
	}
	return InF
}

// moverFactory makes movers of an approach, as base AIs or demonstrators.
func moverFactory(how approach, sweep, air bool) oppFactory {
	return func(r *rand.Rand) controller { return &mover{rng: r, how: how, sweep: sweep, airAttack: air} }
}

// moveTake is one play the CBR layer began: why it was let through (proved,
// explored or tested), what the character was doing on the frame it was
// chosen from (moving with control, in a movement without control, or
// neither), its state then, the move, the situation it was begun in, and
// how many frames the character had been in that situation before it.
type moveTake struct {
	kind  string
	doing string
	state int32
	move  uint64
	sit   uint64
	delay int
	// own is set when the move is what the base AI would have done on that
	// frame (see intender).
	own bool
}

// intender is a base AI that can tell, without acting, what it would do on
// the next frame: as a move (see moveKey), or 0 when it cannot say.
type intender interface {
	intent(s *sim, me int) uint64
}

// movementMove is the move (see moveKey) of movement of a direction class
// (see movementClass).
func movementMove(class int32) uint64 { return uint64(uint32(-1-class))<<1 | 1 }

// moveRun is what moveRounds measured.
type moveRun struct {
	takes          []moveTake
	frames, driven int
	results        []roundResult
	tests          []int32 // test plays a round
}

// moving reports what self, observed after prev, was doing: "moving" with
// control (its position changed, or in the air), "no control" in a movement
// of its own without control (idle, not hit and not guarding), or "other".
func doing(prev, self *CharSnap) string {
	switch {
	case self.Ctrl && (self.Pos != prev.Pos || self.StateType == StAir):
		return "moving"
	case !self.Ctrl && self.MoveType == MtIdle && self.HitStun == 0 && self.BlockStun == 0:
		return "no control"
	}
	return "other"
}

// gateWatch checks, as takeoverWatch does, every take-over the gate let
// through as proved: the tests' own account of the move's plays in the
// situation it took over in (see playShadow) must prove it better there
// than the base AI, as the gate measures. It reads the records as they were
// when the gate chose: a play begun pushes the oldest decision out of the
// lead-in, which credits that decision (see chainTracker.trim), so the
// records the gate read can change in the very call that begins the play.
// So before each input call, note records each move has in the situation
// the character is in.
type gateWatch struct {
	sh               *playShadow
	proved, unproven int
	bad              []string

	sit    uint64 // the situation noted
	shadow map[uint64]PlayStat
	tmp    *Store // the base AI's records noted, in a store of their own
}

func newGateWatch(p *Params) *gateWatch {
	return &gateWatch{sh: newPlayShadow(p), shadow: map[uint64]PlayStat{}, tmp: NewStore("noted")}
}

// note notes the records of the moves of st in situation sit.
func (w *gateWatch) note(st *Store, moves []uint64, sit uint64) {
	w.sit = sit
	clear(w.shadow)
	clear(w.tmp.Baseline)
	clear(w.tmp.Offered)
	for _, k := range []uint64{sit, coarseKey(sit), baseAll} {
		if b := st.Baseline[k]; b != nil {
			c := *b
			w.tmp.Baseline[k] = &c
		}
	}
	for _, m := range moves {
		k := sit&sitMask | m<<sitBits
		w.shadow[k] = w.sh.record(k)
		if o := st.Offered[k]; o != nil {
			c := *o
			w.tmp.Offered[k] = &c
		}
	}
}

// check notes a play of c the CBR layer has just begun, and checks it on
// the noted records when it was let through as proved.
func (w *gateWatch) check(eng *Engine, ps *playerState, c *Case, proved bool) {
	sit := w.sh.began(ps, c)
	if !proved {
		return
	}
	w.proved++
	p := eng.p
	k := playKey(c, sit)
	rec := w.sh.record(k)
	cmp := ps.st.against(p, k, ps.drv.margin)
	if sit == w.sit {
		rec, cmp = w.shadow[k], w.tmp.against(p, k, ps.drv.margin)
	}
	if p.proves(rec) && p.beats(cmp, rec, 1) {
		return
	}
	w.unproven++
	if len(w.bad) < 8 {
		w.bad = append(w.bad, fmt.Sprintf("%s: its own plays there %.1f (%.1f exchanges' worth), mean %.4f; base AI %+v",
			PlayString(k), rec.Trials, rec.Shares, rec.Sum/max(rec.Trials, 1e-6), cmp.bases[:cmp.n]))
	}
}

// moves lists the moves of a store's cases (see moveKey).
func moves(st *Store) []uint64 {
	seen := map[uint64]bool{}
	var out []uint64
	for _, c := range st.Cases {
		if m := moveKey(c); !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// moveRounds plays n rounds of player 0 driven by the CBR layer over base,
// against opp, on sims set up by setup, noting every play the CBR layer
// begins, and checking with w, when not nil, every one let through as
// proved (see gateWatch).
func moveRounds(eng *Engine, n, seedBase int, setup func(*sim), base, opp oppFactory, w *gateWatch) *moveRun {
	run := &moveRun{}
	for r := 0; r < n; r++ {
		s := newSim(3600)
		setup(s)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		d := ps.drv
		if w != nil {
			w.sh.hook(ps)
		}
		ctl := opp(rand.New(rand.NewSource(int64(seedBase + r))))
		b := base(rand.New(rand.NewSource(int64(seedBase + 500 + r))))
		eng.Frame(s.snapshot())
		var last *Case
		var lastPlays, explores, tests int32
		var prev CharSnap
		sit, since := uint64(0), 0
		mv := moves(ps.st)
		for f := 0; f < 3600 && s.roundSt == 2; f++ {
			if k := baselineKey(&ps.lastFeat, ps.lastSelf.Ctrl, ps.lastSelf.AILevel); k != sit {
				sit, since = k, 0
			} else {
				since++
			}
			if w != nil {
				w.note(ps.st, mv, sit)
			}
			var want uint64 // what the base AI would do now, if it can say
			if it, ok := b.(intender); ok {
				want = it.intent(s, 0)
			}
			var in [2]InputBits
			s.ch[0].cbr = false
			if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
				in[0] = FromEngine(btn, s.ch[0].facingRight)
				s.ch[0].cbr = true
				run.driven++
				if c := d.cur; c != nil && (c != last || c.Outcome.Plays != lastPlays) {
					last, lastPlays = c, c.Outcome.Plays
					tk := moveTake{kind: "proved", doing: doing(&prev, &ps.lastSelf), state: ps.lastSelf.StateNo,
						move: moveKey(c), sit: d.curKey, delay: since}
					tk.own = want != 0 && tk.move == want
					switch {
					case d.tests > tests:
						tk.kind = "tested"
					case d.explores > explores:
						tk.kind = "explored"
					}
					tests, explores = d.tests, d.explores
					run.takes = append(run.takes, tk)
					if w != nil {
						w.check(eng, ps, c, tk.kind == "proved")
					}
				}
			} else {
				in[0] = b.next(s, 0)
				if w != nil {
					w.sh.ended(ps)
				}
			}
			in[1] = ctl.next(s, 1)
			run.frames++
			prev = ps.lastSelf
			s.step(in)
			eng.Frame(s.snapshot())
		}
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		run.tests = append(run.tests, d.tests)
		eng.RoundEnd(s.snapshot())
		if w != nil {
			w.sh.ended(ps)
		}
		run.results = append(run.results, result(s))
	}
	return run
}

// count counts the plays of a kind ("" for any) begun while doing doing
// ("" for anything).
func (r *moveRun) count(kind, doing string) int {
	n := 0
	for _, tk := range r.takes {
		if (kind == "" || tk.kind == kind) && (doing == "" || tk.doing == doing) {
			n++
		}
	}
	return n
}

// ownProved counts the take-overs let through as proved, begun while the
// character moved (with control or without), with the move the base AI
// would have made then (see intender).
func (r *moveRun) ownProved() int {
	n := 0
	for _, tk := range r.takes {
		if tk.kind == "proved" && tk.doing != "other" && tk.own {
			n++
		}
	}
	return n
}

// delays lists the delays of the plays of a kind begun while moving.
func (r *moveRun) delays(kind string) []int {
	var out []int
	for _, tk := range r.takes {
		if tk.kind == kind && tk.doing == "moving" {
			out = append(out, tk.delay)
		}
	}
	sort.Ints(out)
	return out
}

// table describes the plays begun, by kind and what the character was
// doing, with the moves and distance bands of those begun while moving.
func (r *moveRun) table() string {
	var b strings.Builder
	for _, kind := range []string{"proved", "tested", "explored"} {
		fmt.Fprintf(&b, "%s: moving %d, no control %d, other %d", kind, r.count(kind, "moving"), r.count(kind, "no control"), r.count(kind, "other"))
		moves := map[string]int{}
		for _, tk := range r.takes {
			if tk.kind == kind && tk.doing != "other" {
				moves[fmt.Sprintf("%s (%s, %s from state %d)", MoveString(tk.move), bandString(tk.sit&15), selfString(tk.sit), tk.state)]++
			}
		}
		var keys []string
		for k := range moves {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "; %s x%d", k, moves[k])
		}
		b.WriteString("\n")
	}
	return b.String()
}

// moversAlone plays n rounds of base alone against opp, as moveRounds does.
func moversAlone(n, seedBase int, setup func(*sim), base, opp oppFactory) []roundResult {
	var out []roundResult
	for r := 0; r < n; r++ {
		s := newSim(3600)
		setup(s)
		runRound(s, nil, [2]controller{base(rand.New(rand.NewSource(int64(seedBase + 500 + r)))), opp(rand.New(rand.NewSource(int64(seedBase + r))))}, 3600)
		out = append(out, result(s))
	}
	return out
}

// recordMovers records rounds of player 0 played by demo against opp as a
// person's play, on sims set up by setup.
func recordMovers(t *testing.T, eng *Engine, rounds int, seed int64, setup func(*sim), demo, opp oppFactory) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		setup(s)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		if err := eng.Record(0, SrcHumanLocal); err != nil {
			t.Fatal(err)
		}
		runRound(s, eng, [2]controller{demo(rand.New(rand.NewSource(seed + int64(r)))), opp(rand.New(rand.NewSource(seed + 77 + int64(r))))}, 3600)
	}
}

// stillFeat is a standing character with control facing a standing enemy
// rel pixels ahead, with dirs as its input history (every frame the same).
func stillFeat(rel float32, dir InputBits) Feat {
	f := Feat{RoundState: 2, TimeFrac: 1,
		Self:  CharFeat{Present: true, State: StStand, Move: MtIdle, Ctrl: true},
		Enemy: CharFeat{Present: true, RelX: rel, State: StStand, Move: MtIdle, Ctrl: true}}
	for i := 0; i < 12; i++ {
		f.InDir = append(f.InDir, dir.Dir())
		f.InBtn = append(f.InBtn, dir.Buttons())
	}
	return f
}

// TestProvedMovesAreNotHiddenByTheWalk: the base AI walks forward, holding
// forward. A case of a walk recorded the same way matches it more closely
// than a sweep, by more than the similarity band. The walk has not proved
// better than the base AI; the sweep has, in this very situation. Applied
// before the gate, the band would leave the sweep out, and the base AI would
// keep walking; the gate comes first, so the sweep takes over. Two ways the
// sweep can be farther: recorded standing still (its input before was
// neutral, where the base AI's walk holds forward), with eight more moves
// recorded right after a walk that would crowd it off the moves on offer
// too, which the input the base AI held before a take-over must not do to a
// case begun with control (see takeoverDist); or recorded the same way, late
// in a round, with the enemy's back to a wall and after an exchange that left
// the character ahead, so that only the gate's coming first lets it through.
func TestProvedMovesAreNotHiddenByTheWalk(t *testing.T) {
	p := DefaultParams()
	p.TakeoverExplore, p.TakeoverExplorePromising, p.BaseExplore = 0, 0, 0
	cur := stillFeat(70, InF)
	elsewhere := stillFeat(72, InF)
	elsewhere.TimeFrac, elsewhere.Wall, elsewhere.FrameAdv, elsewhere.Initiator, elsewhere.LifeLead = 0, 1, 20, 1, 0.5
	for _, tc := range []struct {
		name  string
		start Feat
		more  bool
	}{{"input history", stillFeat(72, 0), true}, {"other features", elsewhere, false}} {
		t.Run(tc.name, func(t *testing.T) {
			sweep := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 400, Command: "d_b", ExecAttack: true,
				ExecClass: StCrouch, Inputs: []InputBits{InD | InBtnB, InD, 0}, StartStateType: StStand, StartCtrl: true,
				StartMoveType: MtIdle, Start: tc.start}
			walk := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, MoveRef: 20,
				Inputs: []InputBits{InF, InF, InF, InF}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle,
				StartStateNo: 20, Start: stillFeat(71, InF)}
			sweep.Outcome = Outcome{Trials: 1, ValueSum: 0.2}
			walk.Outcome = Outcome{Trials: 1, ValueSum: 0.05}
			cases := []*Case{sweep, walk}
			for i := 0; tc.more && i < offerMax; i++ {
				c := &Case{ID: uint64(10 + i), Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: int32(2000 + i),
					Command: "a", ExecAttack: true, ExecClass: StStand, Inputs: []InputBits{InA, 0}, StartStateType: StStand,
					StartCtrl: true, StartMoveType: MtIdle, Start: stillFeat(74, InF)}
				c.Outcome = Outcome{Trials: 1, ValueSum: 0.05}
				cases = append(cases, c)
			}
			st := NewStore("x")
			st.Admit(p, cases)
			key := baselineKey(&cur, true, 8)
			setBase(st, key, 50, 0)
			setPlayedAt(p, st, sweep, key, 10, 0.2)
			d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, st, newChainTracker(p, 0, new(uint64)))
			self := &CharSnap{StateNo: 20, StateType: StStand, MoveType: MtIdle, Ctrl: true, AILevel: 8}

			ds, dw := p.distance(&cur, &sweep.Start), p.distance(&cur, &walk.Start)
			t.Logf("distance to the walk %.3f, to the sweep %.3f (%.3f without the input history), similarity band %.1f",
				dw, ds, p.takeoverDist(&cur, sweep, ds), p.SimilarityBand)
			if ds <= dw+p.SimilarityBand || ds > p.ConfidenceThreshold {
				t.Fatal("setup: the sweep should match within the threshold but outside the band of the walk")
			}
			if !tc.more && p.takeoverDist(&cur, sweep, ds) <= p.takeoverDist(&cur, walk, dw)+p.SimilarityBand {
				t.Fatal("setup: without the input history too, the sweep should be outside the band of the walk")
			}
			if got, _ := d.choose(&cur, self, 1); got == sweep {
				t.Fatal("setup: by similarity alone, the sweep is not among the closest")
			}
			why := map[string]int{}
			g := &gate{key: key}
			got, _ := d.chooseWhy(&cur, self, 1, why, g)
			if got != sweep {
				t.Fatalf("the sweep, proved better here, should take over from the walk: got %v, why %v", got, why)
			}
			if !g.onOffer(playKey(sweep, key)) {
				t.Fatal("the sweep should be on offer")
			}
			// Once the sweep plays, the better-case check re-scores it the way it
			// was ranked: without the input history, which would otherwise count
			// against it as if it no longer matched as well.
			d.cur = sweep
			if cs := d.currentScore(&cur); math.Abs(float64(cs-d.lastBest)) > 1e-5 {
				t.Fatalf("the sweep re-scored at %.4f, chosen at %.4f", cs, d.lastBest)
			}
			d.cur = nil
			// Where the sweep proved nothing, the base AI keeps walking.
			far := stillFeat(150, InF)
			g = &gate{key: baselineKey(&far, true, 8)}
			if got, _ := d.chooseWhy(&far, self, 1, nil, g); got != nil || !g.held {
				t.Fatalf("where nothing proved better, the base AI keeps control: %v held=%v", got, g.held)
			}
		})
	}
}

// demoScript is a scripted demonstrator for one approach: it comes in by
// the approach, then does the better move once, then idles.
type demoScript struct {
	how  approach
	done bool
	wait int // neutral frames before the sweep
}

func (m *demoScript) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	switch {
	case m.done:
		return 0
	case m.how == byJump:
		if c.st == StAir {
			if c.ctrl && dist <= airFrom {
				m.done = true
				return InA
			}
			return 0
		}
		if c.ctrl && dist <= jumpFrom {
			return InU | InF
		}
		return InF
	case dist > sweepFrom:
		if m.how == byRun && c.state != 100 && dist > runFrom && c.input&InF != 0 {
			return 0
		}
		return InF
	case m.wait < 12:
		m.wait++
		return 0
	}
	m.done = true
	return InD | InBtnB
}

// TestTakeoverAtTheDistanceWhileMoving: a person's sweep, recorded standing
// within sweepFrom pixels of the enemy, has proved better than the base AI
// in that distance band (its plays there are given), and nothing else has.
// The base AI walks, or runs, in from far, and the CBR layer takes over with
// the sweep in that band while the base AI is still moving, as soon as the
// sweep is within its reach there (as the reach model knows it, which the
// band's first few pixels are not): the band change is a new situation,
// which ends any wait, and within the band the CBR layer looks again every
// movingRetryFrames frames (see driver.input). Likewise for an air attack,
// recorded in a jump that has control in the air, while the base AI jumps
// in and does nothing in the air.
func TestTakeoverAtTheDistanceWhileMoving(t *testing.T) {
	for _, how := range []approach{byWalk, byRun, byJump} {
		t.Run(how.String(), func(t *testing.T) {
			p := DefaultParams()
			p.TakeoverExplore, p.TakeoverExplorePromising, p.BaseExplore = 0, 0, 0
			eng := New(p, t.TempDir(), 1)
			eng.SetMotions("Rush", simMotionsMove)
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Record(0, SrcHumanLocal)
			s := newSim(3600)
			how.setup(s)
			runRound(s, eng, [2]controller{&demoScript{how: how}, idle{}}, 240)
			st, _ := eng.Store("Rush")
			want := int32(400)
			if how == byJump {
				want = 600
			}
			var move *Case
			for _, c := range st.Cases {
				if c.MoveRef == want && c.StartCtrl {
					move = c
				}
			}
			if move == nil {
				t.Fatalf("setup: the demonstrator's move %d was not recorded", want)
			}
			key := baselineKey(&move.Start, true, 0)
			setBase(st, key, 50, 0)
			setPlayedAt(p, st, move, key, 10, 0.3)
			t.Logf("the move proved better in: %s", SituationString(key))
			// inReach mirrors chooseWhy's reach filter: an attack is not begun
			// where the reach model knows it would not connect.
			inReach := func(cur *Feat) bool {
				dx := cur.Enemy.RelX
				if move.Reach.Valid && move.Start.Enemy.Present {
					dx -= move.Start.Enemy.RelX - move.Reach.DX
				}
				prob, known := st.connect(move.MoveRef, cur.Enemy.State, dx, minReachSamples(8))
				return !known || prob >= 0.15
			}

			// Drive over the mover, from far.
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
			ps := eng.players[0]
			s = newSim(3600)
			how.setup(s)
			s.ch[0].x = -150
			base := &mover{rng: rand.New(rand.NewSource(1)), how: how}
			eng.Frame(s.snapshot())
			entered, ready, took := -1, -1, -1
			var tookFrom CharSnap
			var prev CharSnap
			for f := 0; f < 300 && took < 0; f++ {
				if baselineKey(&ps.lastFeat, ps.lastSelf.Ctrl, ps.lastSelf.AILevel) == key {
					if entered < 0 {
						entered = f
					}
					if ready < 0 && inReach(&ps.lastFeat) {
						ready = f
					}
				}
				var in [2]InputBits
				s.ch[0].cbr = false
				if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
					in[0], s.ch[0].cbr = FromEngine(btn, s.ch[0].facingRight), true
					if ps.drv.cur == move && took < 0 {
						took, tookFrom = f, ps.lastSelf
						if doing(&prev, &ps.lastSelf) != "moving" {
							t.Errorf("the take-over should come while the base AI moves: state %d, control %v", ps.lastSelf.StateNo, ps.lastSelf.Ctrl)
						}
					}
				} else {
					in[0] = base.next(s, 0)
				}
				prev = ps.lastSelf
				s.step(in)
				eng.Frame(s.snapshot())
			}
			if ready < 0 || took < 0 {
				t.Fatalf("the CBR layer should take over in the band: entered on frame %d, in reach on frame %d, took over on frame %d", entered, ready, took)
			}
			t.Logf("entered the band on frame %d, came within the move's reach on frame %d, took over on frame %d from state %d at %.0f px: %d frames after entering the band, %d after coming within reach",
				entered, ready, took, tookFrom.StateNo, absf(s.ch[1].x-tookFrom.Pos[0]), took-entered, took-ready)
			// The band's first frame is a new situation, looked at at once;
			// within the band, the CBR layer looks again every
			// movingRetryFrames frames.
			if took < ready || took-ready >= movingRetryFrames {
				t.Fatalf("the take-over came %d frames after the move came within reach, not within movingRetryFrames (%d)", took-ready, movingRetryFrames)
			}
		})
	}
}

// TestRecheckWhileMoving: while the base AI walks with control and nothing
// has proved better, the CBR layer looks again every movingRetryFrames
// frames, and at once on entering a new distance band, whatever its last
// look found: moves that matched but did not prove better (the store of a
// recorded player), or nothing at all (an empty store). Standing still, it
// keeps the longer waits.
func TestRecheckWhileMoving(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(fmt.Sprint("recorded ", recorded), func(t *testing.T) {
			p := DefaultParams()
			p.TakeoverExplore, p.TakeoverExplorePromising, p.BaseExplore = 0, 0, 0
			eng := New(p, t.TempDir(), 1)
			eng.SetMotions("Rush", simMotionsMove)
			if recorded {
				recordMovers(t, eng, 2, 40, byWalk.setup, moverFactory(byWalk, true, false), oppLowBlind)
			}
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
			ps := eng.players[0]
			d := ps.drv
			s := newSim(3600)
			byWalk.setup(s)
			s.ch[0].x = -200
			eng.Frame(s.snapshot())
			var walking, standing []int64 // frames a choice was made on
			var bandAt []int64            // frames the character entered a new band on
			held := 0                     // choices that found moves but none better
			band := distanceBand(&ps.lastFeat)
			for f := 0; f < 160; f++ {
				before := d.lastDecline
				in := [2]InputBits{}
				s.ch[0].cbr = false
				// Walk in to 40 px, then stand.
				walk := absf(s.ch[1].x-s.ch[0].x) > 40
				if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
					in[0], s.ch[0].cbr = FromEngine(btn, s.ch[0].facingRight), true
				} else if walk {
					in[0] = InF
				}
				if d.lastDecline != before {
					if d.held {
						held++
					}
					if ps.lastSelf.StateNo == 20 {
						walking = append(walking, d.lastDecline)
					} else {
						standing = append(standing, d.lastDecline)
					}
				}
				s.step(in)
				eng.Frame(s.snapshot())
				if b := distanceBand(&ps.lastFeat); b != band {
					band = b
					bandAt = append(bandAt, ps.lastWorld.Tick)
				}
			}
			gaps := func(ticks []int64) (most int64) {
				for i := 1; i < len(ticks); i++ {
					most = max(most, ticks[i]-ticks[i-1])
				}
				return most
			}
			t.Logf("choices while walking at ticks %v; new bands at %v; choices standing at %v; %d found moves none of which was better",
				walking, bandAt, standing, held)
			if recorded != (held > 0) {
				t.Fatalf("setup: with a recorded store moves should match, with an empty one none: %d", held)
			}
			if len(walking) < 10 || gaps(walking) > movingRetryFrames {
				t.Fatalf("while walking, the CBR layer should look again at least every %d frames: %d looks, longest gap %d",
					movingRetryFrames, len(walking), gaps(walking))
			}
			for _, b := range bandAt {
				found := false
				for _, w := range walking {
					found = found || w == b+1
				}
				if b < walking[len(walking)-1] && !found {
					t.Fatalf("no look on the frame after entering a new band at tick %d: %v", b, walking)
				}
			}
			var still []int64 // choices once the walk is over
			for _, c := range standing {
				if c > walking[len(walking)-1] {
					still = append(still, c)
				}
			}
			if len(still) < 2 || gaps(still) <= movingRetryFrames {
				t.Fatalf("standing still, the waits stay longer: %v", still)
			}
		})
	}
}

// TestTakeoverDuringMovement: a person who comes in the same way as the base
// AI but uses a better move at a distance it passes through teaches the CBR
// layer that move: a sweep from sweepFrom pixels against an enemy that never
// guards low, after walking or running in, and an air attack after jumping
// in against one that guards nothing. After learning over the base AI, the
// CBR layer takes over with it while the base AI is still moving, and does
// better than the base AI alone. Every take-over let through as proved is
// proved by the move's own plays in the situation it took over in (see
// takeoverWatch).
func TestTakeoverDuringMovement(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type scenario struct {
		how approach
		opp oppFactory
	}
	for _, sc := range []scenario{{byWalk, oppLowBlind}, {byRun, oppLowBlind}, {byJump, oppSparse}} {
		sc := sc
		t.Run(sc.how.String(), func(t *testing.T) {
			t.Parallel()
			p := DefaultParams()
			eng := New(p, t.TempDir(), 7)
			eng.SetMotions("Rush", simMotionsMove)
			demo := moverFactory(sc.how, sc.how != byJump, sc.how == byJump)
			base := moverFactory(sc.how, false, false)
			recordMovers(t, eng, 10, 40, sc.how.setup, demo, sc.opp)
			w := newGateWatch(p)
			learn := moveRounds(eng, 30, 9000, sc.how.setup, base, sc.opp, w)
			eval := moveRounds(eng, 10, 7000, sc.how.setup, base, sc.opp, w)
			alone := moversAlone(10, 7000, sc.how.setup, base, sc.opp)
			an, _, _ := summarize(alone)
			wn, _, _ := summarize(eval.results)
			t.Logf("net per round: base AI alone %.0f, with the CBR layer %.0f; driven %.1f%% of frames", an, wn, 100*float32(eval.driven)/float32(eval.frames))
			t.Logf("learning plays begun:\n%s", learn.table())
			t.Logf("evaluation plays begun:\n%s", eval.table())
			t.Logf("frames into the situation before a proved take-over while moving: %v", eval.delays("proved"))
			t.Logf("take-overs let through as proved: %d, not proved by the move's own plays there: %d %v", w.proved, w.unproven, w.bad)
			if w.unproven > 0 {
				t.Errorf("%d take-overs with a move its own plays there did not prove better:\n%s", w.unproven, strings.Join(w.bad, "\n"))
			}
			if eval.count("proved", "moving") == 0 {
				t.Fatal("the CBR layer never took over during the movement")
			}
			if wn <= an {
				t.Errorf("taking over during the movement should do better than the base AI alone: %.0f vs %.0f", wn, an)
			}
		})
	}
}

// zonerMover is the zoner (see sameai_test.go) coming in by an approach: it
// backs off to range, waits, then comes in walking, running (a double tap
// once farther than runFrom), dashing (once within dashFrom) or jumping
// forward (once within zonerJumpFrom, doing nothing in the air), and jabs,
// and cancels the jab into the special when it hits. Against an enemy that
// guards nothing (sparse) it wins nearly every exchange.
type zonerMover struct {
	zoner
	how approach
}

// zonerJumpFrom is where the zoner mover jumps: a jump goes about 93 pixels
// forward, so it lands within its jab's range, as quickly as walking there
// and no more exposed on the way.
const zonerJumpFrom = 150

func (z *zonerMover) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	if c.st == StAir {
		return 0
	}
	if dist := absf(o.x - c.x); len(z.plan) == 0 && z.phase == 2 && c.ctrl && dist > 62 && o.state != 5100 && o.state != 5120 {
		switch {
		case z.how == byRun && c.state != 100 && dist > runFrom,
			z.how == byDash && dist <= dashFrom && dist > 120:
			z.plan = []InputBits{InF} // forward, let go, forward: the double tap
			return 0
		case z.how == byJump && dist <= zonerJumpFrom:
			return InU | InF
		}
	}
	return z.zoner.next(s, me)
}

// intent is what the zoner mover would do on the next frame, as its next
// does it, without acting: back off, wait, come in by its approach (a walk
// or run forward, a dash, a jump) or jab; 0 while it cancels a jab that hit
// or has no control on the ground.
func (z *zonerMover) intent(s *sim, me int) uint64 {
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	switch {
	case len(z.plan) > 0 || c.state == 200 && c.contacted && c.moveHit > 0:
		return 0
	case c.st == StAir:
		return movementMove(0)
	case !c.ctrl:
		return 0
	}
	switch z.phase {
	case 0:
		if dist < 220 && c.x > -290 && c.x < 290 {
			return movementMove(2)
		}
		return movementMove(0)
	case 1:
		return movementMove(0)
	}
	switch {
	case o.state == 5100 || o.state == 5120:
		return movementMove(2)
	case dist <= 62:
		return uint64(200) << 1
	case z.how == byDash && dist <= dashFrom && dist > 120:
		return uint64(105) << 1
	case z.how == byJump && dist <= zonerJumpFrom:
		return uint64(40) << 1
	}
	return movementMove(1)
}

// TestSameAIOwnMovementProvesNothing: the base AI is the person the store
// was learned from, the zoner coming in on an enemy that guards nothing by
// walking, running, dashing or jumping. Faster looks while it moves, and the
// comparison with the base AI before the similarity band, must not let the
// base AI's own movement, played by the CBR layer, prove better than the
// base AI making it: no take-over as proved during the movement (with
// control or without) is with the move the base AI would have made on that
// frame (see intender), on two seeds, in learning and evaluation. Every
// take-over let through as proved is proved by the move's own plays there
// (see takeoverWatch), and the result is not worse than the base AI's alone.
//
// Other moves of its own can take over during its movement, and do where
// they are better there, as the test logs: with other seeds (the store
// recorded on seed 40, learned from seed 9000), the walking zoner's case of
// a walk in and a jab did better from 80-119 pixels than its back-off after
// a knockdown, against an enemy that never fights back. A real edge by the
// reward, of its own moves played at other moments. With a jump from 120 pixels, which
// lands it close in and gets it poked in the air, the jumper did better
// walking in (884 a round alone, 1000 with the CBR layer); but then the CBR
// layer's jab at 40-62 pixels, the zoner's own next move there, also
// "proved" better, since the zoner on its own never stands at that distance
// and the base AI's record there comes from its back-offs alone, the bias
// TestSameAINeutralProvesNothing has at one setting too. So the jumper here
// jumps from zonerJumpFrom.
func TestSameAIOwnMovementProvesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for _, how := range []approach{byWalk, byRun, byDash, byJump} {
		for _, seed := range []int64{7, 9} {
			how, seed := how, seed
			t.Run(fmt.Sprintf("%s/seed%d", how, seed), func(t *testing.T) {
				t.Parallel()
				p := DefaultParams()
				eng := New(p, t.TempDir(), seed)
				eng.SetMotions("Rush", simMotionsMove)
				zm := func(r *rand.Rand) controller { return &zonerMover{zoner: zoner{rng: r}, how: how} }
				recordMovers(t, eng, 10, 40+seed, how.setup, zm, oppSparse)
				w := newGateWatch(p)
				learn := moveRounds(eng, 30, 9000+int(seed)*100, how.setup, zm, oppSparse, w)
				eval := moveRounds(eng, 10, 7000, how.setup, zm, oppSparse, w)
				an, _, _ := summarize(moversAlone(10, 7000, how.setup, zm, oppSparse))
				wn, _, _ := summarize(eval.results)
				t.Logf("learning plays begun:\n%s", learn.table())
				t.Logf("evaluation plays begun:\n%s", eval.table())
				t.Logf("net per round: alone %.0f, with the CBR layer %.0f; driven %.1f%% of frames in evaluation; %d take-overs let through as proved, %d not proved there; with the base AI's own movement: %d in learning, %d in evaluation",
					an, wn, 100*float32(eval.driven)/float32(eval.frames), w.proved, w.unproven, learn.ownProved(), eval.ownProved())
				for _, s := range proven(eng, 2, outsideCombos) {
					t.Logf("    proved better: %s", s)
				}
				if n := learn.ownProved() + eval.ownProved(); n > 0 {
					t.Errorf("%d take-overs as proved better, during its movement, with what the base AI would have done", n)
				}
				if w.unproven > 0 {
					t.Errorf("%d take-overs not proved by the move's own plays there:\n%s", w.unproven, strings.Join(w.bad, "\n"))
				}
				if wn < an-30 {
					t.Errorf("the CBR layer made the base AI worse: %.0f a round, alone %.0f", wn, an)
				}
			})
		}
	}
}

// TestDashCancelOnlyOnceVerified: the base AI dashes in with a dash that
// takes control away. A person's case cancels the dash into the special
// with a quarter circle forward and A (begun on the dash's third frame), and
// its plays there are given as better than the base AI's. A case of a
// person's play, but begun in a movement without control: it takes over
// only once the CBR layer's own input of its command has brought the special
// about from the dash (see Verdict), and before that only as a test play,
// within the round's test budget. Where the character has no such route (the
// dash cannot be cancelled), its test plays fail, the spot is blocked after
// three, and it never takes over. A movement case begun in the dash, which
// has no command to verify, never plays there.
func TestDashCancelOnlyOnceVerified(t *testing.T) {
	for _, cancel := range []bool{true, false} {
		t.Run(fmt.Sprint("cancel route ", cancel), func(t *testing.T) {
			p := DefaultParams()
			p.TakeoverExplore, p.TakeoverExplorePromising, p.BaseExplore = 0, 0, 0
			eng := New(p, t.TempDir(), 1)
			eng.SetMotions("Rush", simMotionsMove)
			setup := func(s *sim) {
				byDash.setup(s)
				s.dashCancel = cancel
			}
			// The moment to begin the cancel from: the base AI's own dash on
			// its third frame.
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
			ps := eng.players[0]
			s := newSim(3600)
			setup(s)
			base := &mover{rng: rand.New(rand.NewSource(1)), how: byDash}
			eng.Frame(s.snapshot())
			var at Feat
			var atSelf CharSnap
			for f := 0; f < 300 && atSelf.StateNo != 105; f++ {
				eng.Input(0, s.ch[0].facingRight)
				s.step([2]InputBits{base.next(s, 0), 0})
				eng.Frame(s.snapshot())
				if ps.lastSelf.StateNo == 105 && ps.lastSelf.StateTime == 3 {
					at, atSelf = ps.lastFeat, ps.lastSelf
					at.InDir, at.InBtn = append([]uint8(nil), at.InDir...), append([]InputBits(nil), at.InBtn...)
				}
			}
			eng.RoundEnd(s.snapshot())
			if atSelf.StateNo != 105 {
				t.Fatal("setup: the base AI should dash")
			}
			st, _ := eng.Store("Rush")
			dc := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, Command: "qcf_a", MoveRef: 1000, ExecAttack: true,
				ExecClass: StStand, Inputs: []InputBits{InD, InD | InF, InF | InA, 0}, ExecFrame: 2, StartStateType: StStand,
				StartStateNo: 105, StartMoveType: MtIdle, Start: at}
			dc.Outcome = Outcome{Trials: 1, ValueSum: 0.3}
			mv := &Case{ID: 2, Source: SrcHumanLocal, Kind: KindMovement, ExecFrame: -1, MoveRef: 105,
				Inputs: []InputBits{InF, InF, InF}, StartStateType: StStand, StartStateNo: 105, StartMoveType: MtIdle, Start: at}
			mv.Outcome = Outcome{Trials: 1, ValueSum: 0.3}
			// A second case of the same cancel, from the dash's fourth frame: a
			// verdict is the spot's, whichever case it came from.
			dc2 := *dc
			dc2.ID, dc2.Start.Self.StateTime = 3, 4
			st.Admit(p, []*Case{dc, &dc2, mv})
			key := baselineKey(&at, false, 0)
			setBase(st, key, 50, 0)
			setPlayedAt(p, st, dc, key, 10, 0.3)
			setPlayedAt(p, st, mv, key, 10, 0.3)
			spot := spotKey(1000, "qcf_a", &atSelf)

			var tested, testedBlocked, provedBefore, provedAfter, movementPlays int
			var testsByRound []int32
			for r := 0; r < 4; r++ {
				eng.SetPlayer(0, "Rush", "Guard")
				eng.SetPlayer(1, "Guard", "Rush")
				eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
				ps = eng.players[0]
				d := ps.drv
				s = newSim(3600)
				setup(s)
				base = &mover{rng: rand.New(rand.NewSource(int64(10 + r))), how: byDash}
				eng.Frame(s.snapshot())
				var last *Case
				var lastPlays, tests int32
				for f := 0; f < 3600 && s.roundSt == 2; f++ {
					var in [2]InputBits
					s.ch[0].cbr = false
					verdict := st.verdictAt(spot, ps.lastSelf.Power)
					verified := verdict == verdictVerified
					if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
						in[0], s.ch[0].cbr = FromEngine(btn, s.ch[0].facingRight), true
						if c := d.cur; c != nil && (c != last || c.Outcome.Plays != lastPlays) {
							last, lastPlays = c, c.Outcome.Plays
							switch {
							case c == mv && ps.lastSelf.StateNo == 105:
								movementPlays++
							case c.MoveRef != 1000 || c.Command != "qcf_a":
							case d.tests > tests:
								tested++
								if verdict == verdictBlocked {
									testedBlocked++
								}
							case verified:
								provedAfter++
							default:
								provedBefore++
							}
							tests = d.tests
						}
					} else {
						in[0] = base.next(s, 0)
					}
					s.step(in)
					eng.Frame(s.snapshot())
				}
				if s.roundSt == 2 {
					s.roundSt = 3
				}
				testsByRound = append(testsByRound, d.tests)
				eng.RoundEnd(s.snapshot())
			}
			v := st.Verdicts[spot]
			t.Logf("dash cancel: %d test plays (by round %v), %d take-overs as proved before its spot was verified, %d after; spot %+v; verdict at 0 power: %v",
				tested, testsByRound, provedBefore, provedAfter, v, st.verdictAt(spot, 0))
			for _, n := range testsByRound {
				if n > p.VerifyPerRound {
					t.Errorf("%d test plays in a round, over the budget of %d", n, p.VerifyPerRound)
				}
			}
			if provedBefore > 0 {
				t.Errorf("the dash cancel took over %d times before it was verified from the dash", provedBefore)
			}
			if testedBlocked > 0 {
				t.Errorf("the dash cancel was tested %d times where its spot was already blocked", testedBlocked)
			}
			if movementPlays > 0 {
				t.Errorf("a movement case begun in the dash played there %d times", movementPlays)
			}
			if tested == 0 {
				t.Fatal("the dash cancel should be tested")
			}
			// Without the route, three failures block the spot at their power and
			// below; at a higher power (the jab's hits add some) it is open
			// again, and tested again within the budget, as more power may let a
			// command work.
			switch {
			case cancel && (v == nil || v.Successes == 0 || provedAfter == 0):
				t.Fatal("with the route, the test should verify the cancel, which then takes over")
			case !cancel && (provedAfter > 0 || v == nil || v.Successes > 0 || v.Fails < 3 || st.verdictAt(spot, 0) != verdictBlocked):
				t.Fatalf("without the route, the cancel should fail its tests and be blocked: %d tests, %d take-overs", tested, provedAfter)
			}
		})
	}
}

// BenchmarkFrameWhileMoving measures a frame of the CBR layer (observing it,
// and the input for the next) over a base AI that walks in and out all the
// time, with a store learned from it, in ns per frame.
func BenchmarkFrameWhileMoving(b *testing.B) {
	eng := New(DefaultParams(), b.TempDir(), 1)
	setup := byRun.setup
	mv := moverFactory(byRun, false, false)
	for r := 0; r < 4; r++ {
		s := newSim(3600)
		setup(s)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Record(0, SrcHumanLocal)
		runRound(s, eng, [2]controller{mv(rand.New(rand.NewSource(int64(r)))), &lowBlind{rng: rand.New(rand.NewSource(int64(50 + r)))}}, 3600)
	}
	b.ResetTimer()
	for i := 0; i < b.N; {
		s := newSim(3600)
		setup(s)
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		base := mv(rand.New(rand.NewSource(int64(i))))
		opp := &lowBlind{rng: rand.New(rand.NewSource(int64(i + 1)))}
		eng.Frame(s.snapshot())
		for ; i < b.N && s.roundSt == 2; i++ {
			var in [2]InputBits
			s.ch[0].cbr = false
			if btn, ok := eng.Input(0, s.ch[0].facingRight); ok {
				in[0], s.ch[0].cbr = FromEngine(btn, s.ch[0].facingRight), true
			} else {
				in[0] = base.next(s, 0)
			}
			in[1] = opp.next(s, 1)
			s.step(in)
			eng.Frame(s.snapshot())
		}
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		b.StopTimer()
		eng.RoundEnd(s.snapshot())
		b.StartTimer()
	}
}

// benchDecide measures one take-over decision with the base AI on (see
// decide: the gate over every candidate within the confidence threshold,
// then the band), over a store grown to n cases by replicating recorded
// cases with jittered positions, as BenchmarkChoose does.
func benchDecide(b *testing.B, n int) {
	eng := New(DefaultParams(), b.TempDir(), 1)
	for r := 0; r < 4; r++ {
		s := newSim(3600)
		rng := rand.New(rand.NewSource(int64(r)))
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Record(0, SrcHumanLocal)
		runRound(s, eng, [2]controller{&rushdown{rng: rng}, &defender{rng: rng}}, 3600)
	}
	st, _ := eng.Store("Rush")
	base := append([]*Case(nil), st.Cases...)
	rng := rand.New(rand.NewSource(9))
	var extra []*Case
	for len(base)+len(extra) < n {
		c := *base[rng.Intn(len(base))]
		c.ID = uint64(1e9) + uint64(len(extra))
		c.Start.Enemy.RelX += float32(rng.Intn(40) - 20)
		extra = append(extra, &c)
	}
	st.Admit(eng.p, extra)
	s := newSim(3600)
	eng.SetPlayer(0, "Rush", "Guard")
	eng.SetPlayer(1, "Guard", "Rush")
	eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
	eng.Frame(s.snapshot())
	ps := eng.players[0]
	b.ReportMetric(float64(st.Len()), "cases")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps.drv.decide(&ps.lastFeat, &ps.lastSelf, int64(i))
	}
}

func BenchmarkDecide1k(b *testing.B) { benchDecide(b, 1000) }
func BenchmarkDecide5k(b *testing.B) { benchDecide(b, 5000) }
