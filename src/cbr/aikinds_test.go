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
// into the knockdown special, and check that the CBR layer never makes
// things worse:
//
//   - the base AI plays on the frames the CBR layer leaves to it, which are
//     nearly all of them, and starts about as many attacks as alone;
//   - every take-over that is not exploration is with a move whose own plays
//     by the CBR layer beat the base AI's record in that situation, and
//     exploration stays within its cap;
//   - learning is not corrupted: the base AI's record fills, also from AIs
//     that act only through the command cheat or not at all, driving creates
//     no case, and plays that fail to execute stay within a bound for each
//     kind;
//   - the result is not worse than the base AI's alone, beyond a tolerance;
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
}

// provedBetter reports whether the CBR layer's own plays of a case's action
// beat the base AI's record in the situation it is chosen in: at least
// TakeoverMinPlays of them, with a mean above the base AI's mean there. The
// gate asks for more (an advantage above the margin), so every take-over
// that is not exploration must pass.
func provedBetter(eng *Engine, ps *playerState, c *Case) bool {
	p := eng.p
	a := ps.st.Actions[actionKey(c)]
	if a == nil || a.PTrials < p.decayed(p.TakeoverMinPlays)-1e-3 {
		return false
	}
	b, _ := ps.st.baseline(p, baselineKey(&ps.lastFeat, ps.lastSelf.Ctrl, ps.lastSelf.AILevel))
	return a.PSum/a.PTrials > b
}

// kindRound plays round r of player 0 with the kind's base AI against the
// defender, on the seeds overBase uses: driven by eng over the base AI, or
// with the base AI alone when eng is nil.
func kindRound(eng *Engine, k aiKind, r, seedBase int, ks *kindStats) roundResult {
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
					switch {
					case int(d.explores) > explored:
						ks.explored++
						explored = int(d.explores)
					case !provedBetter(eng, ps, d.cur):
						ks.unproven++
					}
					lastCur, lastPlays = d.cur, d.cur.Outcome.Plays
				}
				if c, ok := base.(contender); ok {
					c.contend(s, 0)
				}
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
		s.step(in)
		if !me.cbr && me.mt == MtAttack && (prevMove != MtAttack || me.state != prevState) {
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
// standard errors of that difference.
func TestEveryKindOfCharacterAI(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const recorded, learn, eval = 8, 14, 8
	for _, k := range aiKinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			p := DefaultParams()
			eng := New(p, t.TempDir(), 7)
			eng.SetMotions("Rush", simMotions)
			recordWith(t, eng, SrcHumanLocal, recorded, 40, func(r *rand.Rand) controller { return &rushdown{rng: r} })
			st, _ := eng.Store("Rush")
			cases := st.Len()

			var alone, learning, with kindStats
			diff := make([]roundResult, eval)
			var aloneRes, withRes []roundResult
			for r := 0; r < eval; r++ {
				aloneRes = append(aloneRes, kindRound(nil, k, r, 7000, &alone))
			}
			before := totals(st)
			for r := 0; r < learn; r++ {
				kindRound(eng, k, r, 9000, &learning)
			}
			for r := 0; r < eval; r++ {
				withRes = append(withRes, kindRound(eng, k, r, 7000, &with))
				diff[r].net = withRes[r].net - aloneRes[r].net
			}
			pt := totals(st).sub(before)
			all := learning
			all.add(&with)
			aloneNet, _, _ := summarize(aloneRes)
			withNet, _, _ := summarize(withRes)
			gain, se, _ := summarize(diff)

			t.Logf("net per round: base AI alone %.0f, with the CBR layer %.0f (difference %+.0f, standard error %.0f)", aloneNet, withNet, gain, se)
			t.Logf("driven %.1f%% of frames; %d take-overs, %d by exploration (at most %d a round), %d not proved better",
				pct(with.driven, with.frames), with.takeovers, with.explored, all.maxExplored, all.unproven)
			t.Logf("still with control: alone %.1f%%, with %.1f%% (driven %.2f%%); base AI's attacks a round: alone %.1f, with %.1f",
				pct(alone.still, alone.frames), pct(with.still, with.frames), pct(with.drivenStill, with.frames),
				float32(alone.baseStarts)/float32(alone.rounds), float32(with.baseStarts)/float32(with.rounds))
			t.Logf("%d plays: %d failed to execute (from control %d of %d), %d aborted; %d contested; %d probes, base AI's record %.0f",
				pt.plays, pt.fails, pt.ctrlFails, pt.ctrlPlays, pt.aborts, all.contested, all.probes, baselineTrials(st))
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
			if all.unproven > 0 {
				t.Errorf("%d take-overs with a move not proved better than the base AI", all.unproven)
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
