package cbr

import (
	"math/rand"
	"testing"
)

// The base AI with every kind of character AI. A character's own AI can be
// written in many ways (src/cbr_hook.go lists them), and the adapter pauses
// it only on the frames the CBR layer gives the input, and only if it reads
// AILevel there. These tests play each kind the simulator can model as the
// base AI, with a store learned from a player who cancels a jab that hits
// into the knockdown special, and check that the CBR layer does not make
// things worse:
//
//   - the base AI plays on the frames the CBR layer leaves to it, which are
//     nearly all of them, and starts about as many attacks as alone;
//   - every take-over that is not exploration is with a move whose own plays
//     by the CBR layer in the situation it takes over in beat the base AI's
//     record there, as the test counts those plays from the chains (see
//     playShadow), apart from the store's records the gate reads; and
//     exploration stays within its cap;
//   - learning is not corrupted: the base AI's record fills, also from AIs
//     that act only through the command cheat or not at all, driving creates
//     no case, and plays that fail to execute stay within a bound for each
//     kind;
//   - the result is not worse than the base AI's alone, in two bounds. With
//     exploration on, as it plays, the result may be worse by 30 a round
//     plus two and a half standard errors of the difference. Exploration
//     costs a little and varies a lot, so this bound catches only a gross
//     loss (a gate that takes over with anything, or a kind the CBR layer
//     breaks), not a loss of a few tens a round. Then the same rounds and as
//     many more are played again with exploration off, so that only moves
//     proved better take over: there the result may be worse by only 10 a
//     round plus one and a half standard errors, which catches a gate that
//     takes over with moves that are not really better, or a kind whose
//     take-overs do worse than their record says. Rounds without a take-over
//     play out exactly as alone, so the standard error there comes only from
//     the rounds the CBR layer changed (with nothing taking over it is 0,
//     and so must the difference be);
//   - nothing idles: the frames on which the character has control and gets
//     no input do not grow.

// aiKind is one kind of character AI as a base AI.
type aiKind struct {
	name string
	// base makes the base AI for a round; nil is a character with no AI at
	// all that also turns the engine AI's button jam and command cheat off.
	base func(r *rand.Rand) controller
	// Limits, in plays per hundred: how many may fail to execute (the
	// command did not come out), and how many may be aborted (failing or
	// being hit). For an AI that cancels mid-move, failures are expected
	// in the moves begun mid-move, where it cancels on top of them (see
	// Engine.watchDriven); ctrlFailPct bounds them in moves begun from
	// control.
	failPct, abortPct, ctrlFailPct float32
}

var aiKinds = []aiKind{
	// ChangeState AI in state -1 gated on AILevel, and AI that gates
	// commands (or AssertCommand) on AILevel: paused on the CBR layer's
	// frames. Helper AI reading root,AILevel or its own AILevel, and ZSS AI
	// reading aiLevel, are the same to the CBR layer.
	{name: "ailevel-changestate", base: func(r *rand.Rand) controller { return &stateChanger{rng: r} }, failPct: 2, abortPct: 50, ctrlFailPct: 2},
	{name: "ailevel-commands", base: func(r *rand.Rand) controller { return &poker{rng: r} }, failPct: 2, abortPct: 50, ctrlFailPct: 2},
	// AI kept on in a variable: latched once AILevel read above 0, or never
	// reading AILevel (IsHomeTeam, TeamSide). Not paused: it forces attacks
	// on top of the CBR layer's moves.
	{name: "latched", base: func(r *rand.Rand) controller { return &latched{stateChanger: stateChanger{rng: r}} }, failPct: 10, abortPct: 50, ctrlFailPct: 10},
	// The same with the moves people input switched off while it is on: the
	// CBR layer's commands start nothing.
	{name: "latched-moves-off", base: func(r *rand.Rand) controller {
		return &latched{stateChanger: stateChanger{rng: r}, movesOff: true}
	}, failPct: 40, abortPct: 60, ctrlFailPct: 40},
	// An older AI that a long command switches on through the engine AI's
	// command cheat, latched from then on.
	{name: "switch-code", base: func(r *rand.Rand) controller {
		return &switchCode{engine: cheater{rng: r}, on: latched{stateChanger: stateChanger{rng: r}}}
	}, failPct: 10, abortPct: 50, ctrlFailPct: 10},
	// The same, but it switches itself off while AILevel reads 0, so it is
	// paused, and on again only through its switch code: after a take-over
	// the character plays as the engine AI for a while.
	{name: "switch-code-resets", base: func(r *rand.Rand) controller {
		return &switchCodeOff{switchCode{engine: cheater{rng: r}, on: latched{stateChanger: stateChanger{rng: r}}}}
	}, failPct: 2, abortPct: 50, ctrlFailPct: 2},
	// No AI of its own, with the engine AI's button jam off: it acts only
	// through the command cheat, which the CBR layer's frames suppress.
	{name: "cheat-only", base: func(r *rand.Rand) controller { return &cheatOnly{rng: r} }, failPct: 2, abortPct: 50, ctrlFailPct: 2},
	// No AI of its own: the engine AI, button jam and command cheat.
	{name: "engine-ai", base: func(r *rand.Rand) controller { return &engineAI{rng: r} }, failPct: 2, abortPct: 50, ctrlFailPct: 2},
	// No AI of its own, with both NoAIButtonJam and NoAICheat: it idles
	// without the CBR layer too.
	{name: "no-ai", failPct: 2, abortPct: 50, ctrlFailPct: 2},
	// An AI kept on that cancels its own jab mid-move, without control, also
	// during the CBR layer's moves, which is not detected.
	{name: "mid-move-canceller", base: func(r *rand.Rand) controller { return &canceller{rng: r} }, failPct: 70, abortPct: 90, ctrlFailPct: 2},
	// AI in a helper that moves its root with a redirected ChangeState,
	// reading root,AILevel: paused on the CBR layer's frames, but each of its
	// attacks shows a frame after the state number changes (see lateChange).
	{name: "helper-redirect", base: func(r *rand.Rand) controller { return &helperAI{stateChanger{rng: r}} }, failPct: 2, abortPct: 50, ctrlFailPct: 2},
}

// kindStats is what a kind's rounds measured.
type kindStats struct {
	rounds, frames, driven int
	// still counts frames on which the character had control and got no
	// input from anyone; drivenStill those of them the CBR layer drove.
	still, drivenStill int
	// baseStarts counts attacks started on frames the base AI played.
	baseStarts int
	// Take-overs: plays the CBR layer began, those by exploration, and those
	// neither explored nor proved better (which must not happen).
	takeovers, explored, unproven int
	maxExplored                   int
	probes, contested             int
	// Rounds in which a switch-code AI switched on, and the frames that
	// took, added up.
	switchedOn, switchTicks int
	// Take-overs begun on the ground with control (fromCtrl), and those of
	// them whose first frame holds a direction the engine's built-in moves
	// act on (walk, jump, crouch), in all and in movement cases. For an AI
	// that switches those moves off on its own frames (NoWalk, NoJump), the
	// engine still reads its flags on that first frame (see cbrInput in
	// src/cbr_hook.go), and the move waits a frame.
	fromCtrl, dirFirst, dirFirstMove int
}

func (ks *kindStats) add(o *kindStats) {
	ks.rounds += o.rounds
	ks.frames += o.frames
	ks.driven += o.driven
	ks.still += o.still
	ks.drivenStill += o.drivenStill
	ks.baseStarts += o.baseStarts
	ks.takeovers += o.takeovers
	ks.explored += o.explored
	ks.unproven += o.unproven
	ks.maxExplored = max(ks.maxExplored, o.maxExplored)
	ks.probes += o.probes
	ks.contested += o.contested
	ks.switchedOn += o.switchedOn
	ks.switchTicks += o.switchTicks
	ks.fromCtrl += o.fromCtrl
	ks.dirFirst += o.dirFirst
	ks.dirFirstMove += o.dirFirstMove
}

// provedBetter reports whether the CBR layer's own plays of a case's move in
// situation sit, where it was just begun, beat the base AI's record there,
// counting those plays as the test does from the chains (see playShadow):
// at least TakeoverMinPlays of them, credited with at least as many
// exchanges' worth between them, with a mean above the base AI's means that
// the gate compares with (see Store.against). The gate asks for more (an
// advantage above the margin), so every take-over that is not exploration
// must pass.
func provedBetter(eng *Engine, ps *playerState, sh *playShadow, c *Case, sit uint64) bool {
	p := eng.p
	k := playKey(c, sit)
	a := sh.record(k)
	if n := p.decayed(p.TakeoverMinPlays) - 1e-3; a.Trials < n || a.Shares < n {
		return false
	}
	return a.Sum/a.Trials > ps.st.against(p, k, ps.drv.margin).strictest().b
}

// kindRound plays round r of player 0 with the kind's base AI against the
// defender, on the seeds overBase uses: driven by eng over the base AI, with
// the CBR layer's plays counted by sh, or with the base AI alone when eng is
// nil.
func kindRound(eng *Engine, k aiKind, r, seedBase int, ks *kindStats, sh *playShadow) roundResult {
	s := newSim(3600)
	s.cmdLayer = true
	opp := &defender{rng: rand.New(rand.NewSource(int64(seedBase + r)))}
	var base controller
	if k.base != nil {
		base = k.base(rand.New(rand.NewSource(int64(seedBase + 500 + r))))
	}
	var ps *playerState
	if eng != nil {
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps = eng.players[0]
		count := func(cc chainCase) {
			if cc.probe {
				ks.probes++
			}
			if cc.contested {
				ks.contested++
			}
		}
		final, evict := ps.ct.onFinal, ps.ct.onEvict
		ps.ct.onFinal = func(rc *resolvedChain) {
			for _, cc := range rc.ch.cases {
				count(cc)
			}
			final(rc)
		}
		ps.ct.onEvict = func(cc chainCase, tick int64) {
			count(cc)
			evict(cc, tick)
		}
		sh.hook(ps)
		eng.Frame(s.snapshot())
	}
	ks.rounds++
	var lastCur *Case
	var lastPlays int32
	explored := 0
	me := s.ch[0]
	for f := 0; f < 3600 && s.roundSt == 2; f++ {
		me.cbr = false
		in := [2]InputBits{0, opp.next(s, 1)}
		if eng != nil {
			if btn, ok := eng.Input(0, me.facingRight); ok {
				in[0] = FromEngine(btn, me.facingRight)
				me.cbr = true
				if d := ps.drv; d.cur != nil && (d.cur != lastCur || d.cur.Outcome.Plays != lastPlays) {
					ks.takeovers++
					sit := sh.began(ps, d.cur)
					switch {
					case int(d.explores) > explored:
						ks.explored++
						explored = int(d.explores)
					case !provedBetter(eng, ps, sh, d.cur, sit):
						ks.unproven++
					}
					lastCur, lastPlays = d.cur, d.cur.Outcome.Plays
					if me.ctrl && me.st != StAir {
						ks.fromCtrl++
						if in[0]&(InU|InD|InF|InB) != 0 {
							ks.dirFirst++
							if d.cur.Kind == KindMovement {
								ks.dirFirstMove++
							}
						}
					}
				}
				if c, ok := base.(contender); ok {
					c.contend(s, 0)
				}
			} else {
				sh.ended(ps)
			}
		}
		if !me.cbr && base != nil {
			in[0] = base.next(s, 0)
		}
		ks.frames++
		if me.cbr {
			ks.driven++
		}
		if me.ctrl && in[0] == 0 && me.assist == "" && me.force == 0 {
			ks.still++
			if me.cbr {
				ks.drivenStill++
			}
		}
		prevState, prevMove := me.state, me.mt
		// A helper's state change from the frame before, which shows now, is
		// the base AI's, whoever gives this frame's input.
		late := me.pending != 0
		s.step(in)
		if (!me.cbr || late) && me.mt == MtAttack && (prevMove != MtAttack || me.state != prevState) {
			ks.baseStarts++
		}
		if eng != nil {
			eng.Frame(s.snapshot())
		}
	}
	if eng != nil {
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		eng.RoundEnd(s.snapshot())
		sh.ended(ps)
	}
	ks.maxExplored = max(ks.maxExplored, explored)
	if sc, ok := base.(*switchCode); ok && sc.onTick != 0 {
		ks.switchedOn++
		ks.switchTicks += int(sc.onTick)
	}
	return result(s)
}

// playTotals adds up the CBR layer's plays of a store's cases, their failed
// executions and aborts, in all and for cases begun from control.
type playTotals struct {
	plays, fails, aborts int32
	ctrlPlays, ctrlFails int32
}

func totals(st *Store) (t playTotals) {
	for _, c := range st.Cases {
		o := &c.Outcome
		t.plays += o.Plays
		t.fails += o.ExecFails
		t.aborts += o.Aborts
		if c.StartCtrl {
			t.ctrlPlays += o.Plays
			t.ctrlFails += o.ExecFails
		}
	}
	return t
}

func (t playTotals) sub(o playTotals) playTotals {
	return playTotals{t.plays - o.plays, t.fails - o.fails, t.aborts - o.aborts, t.ctrlPlays - o.ctrlPlays, t.ctrlFails - o.ctrlFails}
}

// over reports whether n is more than pct per hundred of of.
func over(n, of int32, pct float32) bool { return float32(100*n) > pct*float32(of) }

func pct(n, of int) float32 {
	if of == 0 {
		return 0
	}
	return 100 * float32(n) / float32(of)
}

// baselineTrials is how many results the base AI's store-wide record holds.
func baselineTrials(st *Store) float32 {
	if b := st.Baseline[baseAll]; b != nil {
		return b.Trials
	}
	return 0
}

// TestEveryKindOfCharacterAI plays every kind of character AI as the base AI
// with the CBR layer on and checks the properties listed at the top of this
// file. The result is compared round by round on the same seeds, and may be
// worse than the base AI's alone by 30 per round plus two and a half
// standard errors of that difference; played again over twice as many rounds
// with exploration off, by 10 per round plus one and a half standard errors.
func TestEveryKindOfCharacterAI(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const recorded, learn, eval, evalOff = 8, 14, 8, 16
	for _, k := range aiKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			p := DefaultParams()
			eng := New(p, t.TempDir(), 7)
			eng.SetMotions("Rush", simMotions)
			recordWith(t, eng, SrcHumanLocal, recorded, 40, func(r *rand.Rand) controller { return &rushdown{rng: r} })
			st, _ := eng.Store("Rush")
			cases := st.Len()
			sh := newPlayShadow(p)

			var alone, learning, with kindStats
			diff := make([]roundResult, eval)
			var aloneRes, withRes []roundResult
			for r := 0; r < eval; r++ {
				aloneRes = append(aloneRes, kindRound(nil, k, r, 7000, &alone, nil))
			}
			before := totals(st)
			for r := 0; r < learn; r++ {
				kindRound(eng, k, r, 9000, &learning, sh)
			}
			for r := 0; r < eval; r++ {
				withRes = append(withRes, kindRound(eng, k, r, 7000, &with, sh))
				diff[r].net = withRes[r].net - aloneRes[r].net
			}
			pt := totals(st).sub(before)
			all := learning
			all.add(&with)
			aloneNet, _, _ := summarize(aloneRes)
			withNet, _, _ := summarize(withRes)
			gain, se, _ := summarize(diff)

			// The same rounds and as many more again with exploration off, so
			// that only moves proved better take over.
			var aloneMore, off kindStats
			aloneOff := append([]roundResult(nil), aloneRes...)
			for r := eval; r < evalOff; r++ {
				aloneOff = append(aloneOff, kindRound(nil, k, r, 7000, &aloneMore, nil))
			}
			p.TakeoverExplore, p.TakeoverExplorePromising = 0, 0
			diffOff := make([]roundResult, evalOff)
			for r := 0; r < evalOff; r++ {
				diffOff[r].net = kindRound(eng, k, r, 7000, &off, sh).net - aloneOff[r].net
			}
			gainOff, seOff, _ := summarize(diffOff)

			t.Logf("net per round: base AI alone %.0f, with the CBR layer %.0f (difference %+.0f, standard error %.0f)", aloneNet, withNet, gain, se)
			t.Logf("driven %.1f%% of frames; %d take-overs, %d by exploration (at most %d a round), %d not proved better",
				pct(with.driven, with.frames), with.takeovers, with.explored, all.maxExplored, all.unproven)
			t.Logf("still with control: alone %.1f%%, with %.1f%% (driven %.2f%%); base AI's attacks a round: alone %.1f, with %.1f",
				pct(alone.still, alone.frames), pct(with.still, with.frames), pct(with.drivenStill, with.frames),
				float32(alone.baseStarts)/float32(alone.rounds), float32(with.baseStarts)/float32(with.rounds))
			t.Logf("%d plays: %d failed to execute (from control %d of %d), %d aborted; %d contested; %d probes, base AI's record %.0f",
				pt.plays, pt.fails, pt.ctrlFails, pt.ctrlPlays, pt.aborts, all.contested, all.probes, baselineTrials(st))
			t.Logf("exploration off, %d rounds: %d take-overs (%d not proved better), driven %.1f%% of frames, difference %+.0f a round (standard error %.0f)",
				off.rounds, off.takeovers, off.unproven, pct(off.driven, off.frames), gainOff, seOff)
			t.Logf("take-overs begun on the ground with control: %d, of which %d first hold a walk, jump or crouch direction (%d in movement cases)",
				all.fromCtrl, all.dirFirst, all.dirFirstMove)
			if alone.switchedOn+with.switchedOn > 0 {
				t.Logf("switched on: alone in %d of %d rounds (frame %.0f on average), with the CBR layer in %d of %d (frame %.0f)",
					alone.switchedOn, alone.rounds, float32(alone.switchTicks)/float32(max(alone.switchedOn, 1)),
					with.switchedOn, with.rounds, float32(with.switchTicks)/float32(max(with.switchedOn, 1)))
				if with.switchedOn < with.rounds {
					t.Errorf("the switch-code AI switched on in only %d of %d rounds", with.switchedOn, with.rounds)
				}
			}

			// Take-overs only with moves proved better, and exploration
			// within its cap.
			if all.unproven+off.unproven > 0 {
				t.Errorf("%d take-overs with a move not proved better than the base AI", all.unproven+off.unproven)
			}
			if off.explored > 0 {
				t.Errorf("explored %d times with exploration off", off.explored)
			}
			if all.maxExplored > int(p.TakeoverExploreMax) {
				t.Errorf("explored %d times in a round, over the cap of %d", all.maxExplored, p.TakeoverExploreMax)
			}
			// The base AI keeps playing.
			if pct(with.driven, with.frames) > 10 {
				t.Errorf("the CBR layer drove %.1f%% of the frames", pct(with.driven, with.frames))
			}
			if float32(with.baseStarts) < 0.8*float32(alone.baseStarts) {
				t.Errorf("the base AI started %d attacks, against %d alone", with.baseStarts, alone.baseStarts)
			}
			// No learning corruption.
			if all.probes == 0 || baselineTrials(st) < 20 {
				t.Errorf("the base AI's record holds %.1f results from %d probes", baselineTrials(st), all.probes)
			}
			if st.Len() != cases {
				t.Errorf("driving changed the store's cases: %d, was %d", st.Len(), cases)
			}
			if over(pt.fails, pt.plays, k.failPct) || over(pt.ctrlFails, pt.ctrlPlays, k.ctrlFailPct) || over(pt.aborts, pt.plays, k.abortPct) {
				t.Errorf("of %d plays, %d failed to execute (from control %d of %d) and %d were aborted: over %.0f%% (%.0f%%) and %.0f%%",
					pt.plays, pt.fails, pt.ctrlFails, pt.ctrlPlays, pt.aborts, k.failPct, k.ctrlFailPct, k.abortPct)
			}
			// Not worse than the base AI alone, and no idling.
			if gain < -30-2.5*se {
				t.Errorf("the CBR layer made the base AI worse: %+.0f a round (standard error %.0f)", gain, se)
			}
			if gainOff < -10-1.5*seOff {
				t.Errorf("with exploration off, the moves proved better made the base AI worse: %+.0f a round (standard error %.0f)", gainOff, seOff)
			}
			if pct(with.still, with.frames) > pct(alone.still, alone.frames)+2 {
				t.Errorf("the character stands idle more: %.1f%% of frames, against %.1f%% alone",
					pct(with.still, with.frames), pct(alone.still, alone.frames))
			}
		})
	}
}

// TestMidMoveTakeOverIsCreditedToTheMove pins down the limit of contest
// detection. An AI that is not paused and changes the character's state
// during a move the CBR layer plays, without control in between and with no
// command active, is not told apart from the move's own transition into a
// later part, which looks the same to the CBR layer: an attack state
// followed by another. Neither drops the move, so the AI's cancel is
// credited to it (see Engine.watchDriven); only an attack the AI starts from
// control is (TestContestedPlayIsDropped).
func TestMidMoveTakeOverIsCreditedToTheMove(t *testing.T) {
	for _, tc := range []struct {
		name     string
		move, to int32
	}{
		{"the AI cancels a jab into a special", 200, 1100},
		{"a two-part special continues", 1100, 1101},
	} {
		p := withoutBaseAI()
		eng := New(p, t.TempDir(), 1)
		st, _ := eng.Store("A")
		start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
		c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: tc.move, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{InA, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start}
		st.Admit(p, []*Case{c})
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		ff := &frameFeeder{eng: eng}
		ff.frame(CharSnap{Ctrl: true, MoveType: MtIdle})
		if _, ok := eng.Input(0, true); !ok || ps.drv.cur != c {
			t.Fatalf("%s: setup: the case should play", tc.name)
		}
		ff.frame(CharSnap{StateNo: tc.move, MoveType: MtAttack, CBRDriven: true})
		eng.Input(0, true)
		ff.frame(CharSnap{StateNo: tc.to, StateTime: 0, MoveType: MtAttack, CBRDriven: true})
		if ps.drv.cur != c || ps.drv.waitCtrl || ps.unexplained != 0 {
			t.Fatalf("%s: the move is not dropped: playing %v, waiting %v, unexplained %d", tc.name, ps.drv.cur != nil, ps.drv.waitCtrl, ps.unexplained)
		}
		if c.Outcome.ExecFails != 0 {
			t.Fatalf("%s: its command came out", tc.name)
		}
	}
}

// TestHelperRedirectIsSeen: an AI in a helper that moves its root with a
// redirected ChangeState changes the root's state number at the end of a
// frame, while the root keeps its control and move type until its next
// frame, the state's first (see lateChange). Its actions fill the base AI's
// record as a root's own do; a move the CBR layer begins on top of one is
// dropped without counting against either, and one the AI makes while it
// should be paused is caught as a root's would be. A state that drops
// control later than its first frame is no action, as for a root.
func TestHelperRedirectIsSeen(t *testing.T) {
	standing := CharSnap{StateNo: 0, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	attack := func(time int32) CharSnap {
		return CharSnap{StateNo: 230, StateTime: time, StateType: StStand, MoveType: MtAttack}
	}
	// redirected is the state number changed by a helper: control and move
	// type are still the last state's.
	redirected := CharSnap{StateNo: 230, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	record := func(frames ...CharSnap) float32 {
		eng := New(DefaultParams(), t.TempDir(), 1)
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ff := &frameFeeder{eng: eng}
		for _, f := range frames {
			ff.frame(f)
		}
		ff.end()
		st, _ := eng.Store("A")
		return baselineTrials(st)
	}
	own := record(standing, standing, attack(1), attack(2), attack(3))
	if own == 0 {
		t.Fatal("setup: the root's own attack from control is probed")
	}
	if got := record(standing, standing, redirected, attack(1), attack(2)); got != own {
		t.Fatalf("a helper's redirected attack fills the base AI's record with %v results, the root's own with %v", got, own)
	}
	// Not actions: control dropped on a state's second frame, whether the
	// root entered it (StateTime 1 at once) or a helper did; a helper's
	// change into a state that keeps control; and an attack that has control
	// going on into its next part by itself on the last frame of a hitpause,
	// where StateTime stands still.
	keeps := CharSnap{StateNo: 230, StateTime: 1, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	withCtrl := CharSnap{StateNo: 220, StateTime: 1, StateType: StStand, MoveType: MtAttack, Ctrl: true}
	inHitpause, partTwo := withCtrl, CharSnap{StateNo: 221, StateType: StStand, MoveType: MtAttack, Ctrl: true}
	inHitpause.HitPause = 1
	for name, frames := range map[string][]CharSnap{
		"root's state drops control on its second frame":   {standing, standing, keeps, attack(2), attack(3)},
		"helper's state drops control on its second frame": {standing, standing, redirected, keeps, attack(2), attack(3)},
		"helper's state keeps control":                     {standing, standing, redirected, keeps, keeps},
		"a move's own next part after hitpause": {standing, withCtrl, inHitpause, partTwo,
			{StateNo: 221, StateTime: 1, StateType: StStand, MoveType: MtAttack}},
	} {
		if got := record(frames...); got != 0 {
			t.Errorf("%s: no action, but the base AI's record got %v results", name, got)
		}
	}

	// The CBR layer's moves: a jab whose press comes a frame after it
	// begins, so that its first frame completes no command. play feeds the
	// frames, asking the CBR layer for input before those marked CBRDriven.
	start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
	// driven counts the frames the CBR layer gave the input in the last play.
	driven := 0
	playAny := func(frames ...CharSnap) (*playerState, *Case) {
		p := withoutBaseAI()
		eng := New(p, t.TempDir(), 1)
		st, _ := eng.Store("A")
		c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 1, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{0, InA, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start}
		st.Admit(p, []*Case{c})
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ff := &frameFeeder{eng: eng}
		driven = 0
		for _, f := range frames {
			if f.CBRDriven {
				_, f.CBRDriven = eng.Input(0, true)
			}
			if f.CBRDriven {
				driven++
			}
			ff.frame(f)
		}
		return eng.players[0], c
	}
	play := func(name string, frames ...CharSnap) (*playerState, *Case) {
		ps, c := playAny(frames...)
		if c.Outcome.Plays == 0 {
			t.Fatalf("%s: setup: the jab should be played", name)
		}
		return ps, c
	}
	asked := func(f CharSnap) CharSnap {
		f.CBRDriven = true
		return f
	}
	// The base AI's helper moves the root on the base AI's frame, and its
	// attack shows on the next: the CBR layer does not begin the jab there,
	// where its input could not act, and leaves the frames to the base AI
	// (round 3 began the jab and dropped it as taken over).
	ps, c := playAny(standing, redirected, asked(attack(1)), asked(attack(2)), asked(attack(3)),
		asked(attack(4)), asked(attack(5)), asked(attack(6)), asked(attack(7)), asked(attack(8)))
	if c.Outcome.Plays != 0 || driven != 0 || ps.drv.cur != nil || ps.unexplained != 0 {
		t.Fatalf("base AI first: the jab is not begun on the base AI's action: %d plays, %d driven frames, playing %v, unexplained %d",
			c.Outcome.Plays, driven, ps.drv.cur != nil, ps.unexplained)
	}
	if c.Outcome.ExecFails != 0 || c.Outcome.Aborts != 0 {
		t.Fatalf("base AI first: nothing counts against the jab: %d failed, %d aborted", c.Outcome.ExecFails, c.Outcome.Aborts)
	}
	// With the engine's labels the same holds, also where the change does
	// not look late (made in hitpause, say): its label says a helper made it.
	routes := &RouteTable{Person: map[int32][]string{200: {"x"}, 230: {"y"}}}
	labelled := func(f CharSnap, k EntryKind, helper bool) CharSnap {
		f.Entry, f.EntryHelper, f.Routes = k, helper, routes
		if k == EntryReachable {
			f.EntryCommand = "y"
		}
		return f
	}
	pausedChange := labelled(redirected, EntryReachable, true)
	pausedChange.HitPause = 1
	for name, change := range map[string]CharSnap{
		"labelled helper change":             labelled(redirected, EntryReachable, true),
		"labelled helper change in hitpause": pausedChange,
	} {
		ps, c = playAny(labelled(standing, EntryNone, false), change, asked(labelled(attack(1), EntryNone, false)),
			asked(labelled(attack(2), EntryNone, false)), asked(labelled(attack(3), EntryNone, false)))
		if c.Outcome.Plays != 0 || driven != 0 {
			t.Fatalf("%s: the jab is not begun on the base AI's action: %d plays, %d driven frames", name, c.Outcome.Plays, driven)
		}
	}
	// A helper's change the base AI makes during the jab is caught by its
	// label, even when it takes control away at once (ctrl = 0), which looks
	// like no late change.
	ctrlZero := labelled(redirected, EntryReachable, true)
	ctrlZero.Ctrl = false
	ps, c = play("labelled, during the jab, ctrl = 0", labelled(standing, EntryNone, false), asked(ctrlZero),
		asked(labelled(attack(1), EntryNone, false)), asked(labelled(attack(2), EntryNone, false)))
	if ps.drv.cur != nil || !ps.drv.waitCtrl || ps.unexplained != 1 || c.Outcome.ExecFails != 0 {
		t.Fatalf("labelled, during the jab, ctrl = 0: dropped as the AI's: playing %v, waiting %v, unexplained %d, failed %d",
			ps.drv.cur != nil, ps.drv.waitCtrl, ps.unexplained, c.Outcome.ExecFails)
	}
	// A helper moves the root on the jab's first frame, with no command
	// active: the AI was not paused, as for a root's own attack.
	ps, c = play("during the jab", standing, asked(redirected), asked(attack(1)), asked(attack(2)))
	if ps.drv.cur != nil || ps.unexplained != 1 || c.Outcome.ExecFails != 0 {
		t.Fatalf("during the jab: dropped as the AI's: playing %v, unexplained %d, failed %d", ps.drv.cur != nil, ps.unexplained, c.Outcome.ExecFails)
	}
	// The CBR layer's own command, read by a helper (an input system in a
	// helper) into another state than the case's: the move's, never dropped.
	pressed := redirected
	pressed.Commands = []string{"x"}
	ps, _ = play("the CBR layer's command", standing, asked(pressed), asked(attack(1)), asked(attack(2)))
	if ps.unexplained != 0 || ps.drv.waitCtrl {
		t.Fatalf("the CBR layer's command: not the AI's: unexplained %d, waiting %v", ps.unexplained, ps.drv.waitCtrl)
	}
}

// TestHelperRedirectAIIsFlagged: the recorder sees a helper AI's redirected
// attacks as a root's: an AI source that enters attacks with no command
// active is flagged and its cases dropped, the states it enters kept as
// hypotheses; and a person's command that a helper reads (an input system in
// a helper) starts a command case for the state, with the press in it.
func TestHelperRedirectAIIsFlagged(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordWith(t, eng, SrcAuthoredAI, 2, 20, func(r *rand.Rand) controller { return &helperAI{stateChanger{rng: r}} })
	st, _ := eng.Store("Rush")
	if !st.ChangeStateAI || st.Len() != 0 {
		t.Fatalf("a helper AI's attack starts with no command must flag it and drop its cases: flagged %v, %d cases", st.ChangeStateAI, st.Len())
	}
	if h := st.Hypotheses[1000]; h == nil || h.Seen == 0 {
		t.Fatal("the state entered is kept as a hypothesis")
	}

	eng = New(DefaultParams(), t.TempDir(), 1)
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcHumanLocal)
	ff := &frameFeeder{eng: eng}
	standing := CharSnap{StateNo: 0, StateType: StStand, MoveType: MtIdle, Ctrl: true}
	for i := 0; i < 20; i++ {
		ff.frame(standing)
	}
	pressed := CharSnap{StateNo: 230, StateType: StStand, MoveType: MtIdle, Ctrl: true, Input: InA, Commands: []string{"x"}}
	ff.frame(pressed)
	for i := int32(1); i <= 20; i++ {
		ff.frame(CharSnap{StateNo: 230, StateTime: i, StateType: StStand, MoveType: MtAttack})
	}
	for i := 0; i < 20; i++ {
		ff.frame(standing)
	}
	ff.end()
	st, _ = eng.Store("A")
	found := false
	for _, c := range st.Cases {
		if c.Kind == KindCommand && c.MoveRef == 230 {
			found = true
			if c.ExecFrame < 1 || int(c.ExecFrame) >= len(c.Inputs) || c.Inputs[c.ExecFrame-1] != InA {
				t.Fatalf("the press comes the frame before the action shows: exec %d, inputs %v", c.ExecFrame, c.Inputs)
			}
		}
	}
	if !found {
		t.Fatal("a person's command read by a helper starts a command case")
	}
	if st.CmdStates[230]["x"] == 0 {
		t.Fatal("the command active when the helper changed the state is noted for it")
	}
}
