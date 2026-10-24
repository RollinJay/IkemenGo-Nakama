package cbr

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// Tests of learning from a character AI that changes states directly, and
// of the test plays that verify its moves (see command.go): the simulator
// labels its state changes as the engine does (sim.entries), with a route
// table of its commands (simRoutes).

// labelledRound plays one round of player 0 with ctl (nil: the engine drives
// it, over base on the frames the CBR layer leaves to it) against opp, with
// the simulator's labels on, starting with power. watch, when not nil, is
// called on each frame the CBR layer gives the input, before the frame is
// played, and on each frame after it is observed. It returns the round's
// result and the frames the CBR layer drove.
func labelledRound(eng *Engine, ctl, base, opp controller, power int32, seed uint64, watch func(s *sim, before bool)) (roundResult, int) {
	s := newSim(3600)
	s.cmdLayer, s.entries = true, true
	s.session = seed
	s.ch[0].power = power
	if eng != nil {
		eng.Frame(s.snapshot())
	}
	driven := 0
	for f := 0; f < 3600 && s.roundSt == 2; f++ {
		me := s.ch[0]
		me.cbr = false
		in := [2]InputBits{0, opp.next(s, 1)}
		switch {
		case ctl != nil:
			in[0] = ctl.next(s, 0)
		default:
			if btn, ok := eng.Input(0, me.facingRight); ok {
				in[0], me.cbr = FromEngine(btn, me.facingRight), true
				driven++
				if watch != nil {
					watch(s, true)
				}
				// An AI that is not paused acts on the CBR layer's frames too.
				if c, ok := base.(contender); ok {
					c.contend(s, 0)
				}
			} else if base != nil {
				in[0] = base.next(s, 0)
			}
		}
		s.step(in)
		if eng != nil {
			eng.Frame(s.snapshot())
		}
		if watch != nil {
			watch(s, false)
		}
	}
	if eng != nil {
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		eng.RoundEnd(s.snapshot())
	}
	return result(s), driven
}

// roundPower is the power player 0 starts round r with: none, half a bar,
// or one to two bars, so that the super is in reach of a person's command
// in some rounds and not in others.
func roundPower(r int) int32 { return [...]int32{0, 500, 1000, 1500, 2000}[r%5] }

// recordLabelled records rounds of player 0 played by an AI from mk (none:
// the character idles), against lowBlind (oppLowBlind), which never guards
// low, as source src, then ends the match.
func recordLabelled(t *testing.T, eng *Engine, src Source, rounds int, seed int64, mk func(*rand.Rand) controller) {
	recordLabelledAgainst(t, eng, src, rounds, seed, mk, oppLowBlind)
}

// recordLabelledAgainst is recordLabelled against opp.
func recordLabelledAgainst(t *testing.T, eng *Engine, src Source, rounds int, seed int64, mk func(*rand.Rand) controller, opp oppFactory) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		if err := eng.Record(0, src); err != nil {
			t.Fatal(err)
		}
		var ctl controller = idle{}
		if mk != nil {
			ctl = mk(rand.New(rand.NewSource(seed + int64(r))))
		}
		labelledRound(eng, ctl, nil, opp(rand.New(rand.NewSource(seed+500+int64(r)))), roundPower(r), uint64(r+1), nil)
	}
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
}

// casesBySource counts a store's live command cases by source and move.
func casesBySource(st *Store) map[Source]map[int32]int {
	out := map[Source]map[int32]int{}
	for _, c := range st.Cases {
		if c.Removed || c.Kind != KindCommand {
			continue
		}
		if out[c.Source] == nil {
			out[c.Source] = map[int32]int{}
		}
		out[c.Source][c.MoveRef]++
	}
	return out
}

// verdictString lists a store's verdicts, for the logs.
func verdictString(st *Store) string {
	var lines []string
	for k, v := range st.Verdicts {
		from := "with control"
		if k.From >= 0 {
			from = "from state " + itoa(k.From)
		}
		lines = append(lines, "state "+itoa(k.State)+" by "+k.Command+", "+from+", stance "+itoa(int32(k.Stance))+": "+
			"reachable "+itoa(v.Reachable)+" (from power "+itoa(v.ReachPower)+"), cheat "+itoa(v.Cheat)+
			", came out "+itoa(v.Successes)+" (from power "+itoa(v.OKPower)+"), failed "+itoa(v.Fails))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func itoa(n int32) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

// TestVerdictsBySpotAndPower: a spot's verdict (see Verdict) comes from its
// own evidence, and power is a level within it: a success verifies its spot
// at the least power the command was seen to work with and above, three
// failures with no success block it at their power and below, a cheat found
// by the engine's moment check counts as a failure, and a state no person's
// route leads into is blocked everywhere.
func TestVerdictsBySpotAndPower(t *testing.T) {
	st := NewStore("x")
	standing := &CharSnap{Ctrl: true, StateType: StStand}
	c := &Case{Source: SrcCharacterAI, MoveRef: 3000, Command: "qcf_c"}
	at := func(power int32) verdictState {
		self := *standing
		self.Power = power
		return st.check(c, &self)
	}
	if at(0) != verdictOpen {
		t.Fatal("nothing known: open")
	}
	k := spotKey(3000, "qcf_c", standing)
	st.successAt(k, 1200)
	if at(1200) != verdictVerified || at(3000) != verdictVerified || at(1199) != verdictOpen {
		t.Fatalf("a success at 1200 verifies 1200 and above only: %v %v %v", at(1200), at(3000), at(1199))
	}
	st.failAt(k, 900)
	st.cheatAt(k, 300)
	if at(0) != verdictOpen {
		t.Fatal("two failures block nothing")
	}
	st.failAt(k, 600)
	if at(300) != verdictBlocked || at(0) != verdictBlocked || at(301) != verdictOpen || at(1200) != verdictVerified {
		t.Fatalf("three failures at 900, 600 and 300 block 300 and below: %v %v %v %v", at(300), at(0), at(301), at(1200))
	}
	if !st.cheating(k, 0) {
		t.Fatal("no person's route was seen open there: cheating")
	}
	st.failAt(k, 800)
	if at(600) != verdictBlocked || at(601) != verdictOpen {
		t.Fatalf("the three highest failures count: %v %v", at(600), at(601))
	}
	// The power the command needs, from wherever it was seen to work: a
	// moment check found its route open at 1000 in another spot.
	crouching := &CharSnap{Ctrl: true, StateType: StCrouch}
	st.reachAt(spotKey(3000, "qcf_c", crouching), 1000)
	if at(1000) != verdictVerified || at(999) != verdictOpen {
		t.Fatalf("verified down to the power the command needs: %v %v", at(1000), at(999))
	}
	st.reachAt(k, 100)
	if st.cheating(k, 200) || !st.cheating(k, 50) {
		t.Fatal("where the moment check found the route open, a blocked spot is unreproducible, not cheating")
	}
	// Other spots know nothing of it, and a store loaded from disk works
	// out the power needed again.
	fromJab := &CharSnap{StateNo: 200, StateType: StStand, MoveType: MtAttack}
	for _, self := range []*CharSnap{crouching, fromJab} {
		if st.check(c, self) != verdictOpen {
			t.Fatalf("another spot (%+v) is open", *self)
		}
	}
	path := t.TempDir() + "/x.cbr"
	if err := SaveStore(path, st); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadStore(path, "x")
	if err != nil {
		t.Fatal(err)
	}
	self := *standing
	self.Power = 100
	if loaded.check(c, &self) != verdictVerified || len(loaded.Verdicts) != len(st.Verdicts) {
		t.Fatalf("the verdicts are saved: %v, %d of %d", loaded.check(c, &self), len(loaded.Verdicts), len(st.Verdicts))
	}
	// No person's route into the state at all.
	routed := *standing
	routed.Routes = &RouteTable{Person: map[int32][]string{200: {"a"}}}
	if st.check(&Case{Source: SrcCharacterAI, MoveRef: 1500, Command: "x"}, &routed) != verdictBlocked {
		t.Fatal("a state no person's route leads into is blocked from the start")
	}
	if st.check(&Case{Source: SrcCharacterAI, MoveRef: 200, Command: "a"}, &routed) != verdictOpen {
		t.Fatal("a state a person's route leads into is open")
	}
	routed.Routes = &RouteTable{Unreadable: true}
	if st.check(&Case{Source: SrcCharacterAI, MoveRef: 1500, Command: "x"}, &routed) != verdictOpen {
		t.Fatal("where some route's target is not a fixed number, the table cannot tell")
	}
}

// TestChangeStateAITeachesReachableMoves: from a ChangeState AI recorded
// with the engine's labels, with no person's play at all, the decisions a
// person's command could have made become cases of the character's AI with
// that command's inputs, cancels included; those no person's command could
// have made never do, nor anything whose exchange they were part of; and
// the match is kept, not discarded.
func TestChangeStateAITeachesReachableMoves(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("Rush", simMotions)
	recordLabelled(t, eng, SrcEngineAI, 10, 40, func(r *rand.Rand) controller { return &changeStater{rng: r} })
	st, _ := eng.Store("Rush")
	by := casesBySource(st)
	t.Logf("command cases by source and move: %v; %d cases in all", by, st.Len())
	t.Logf("verdicts:\n%s", verdictString(st))
	ai := by[SrcCharacterAI]
	for _, move := range []int32{200, 400, 1000, 3000} {
		if ai[move] == 0 {
			t.Errorf("no case of the character's AI for move %d", move)
		}
	}
	if n := ai[1500] + ai[1100]; n > 0 {
		t.Errorf("%d cases for moves no person's command reaches there (1500, and 1100 as a cancel)", n)
	}
	for src, moves := range by {
		if moves[1500] > 0 {
			t.Errorf("%d cases of source %d for the AI-only move", moves[1500], src)
		}
	}
	cancels, super := 0, 0
	for _, c := range st.Cases {
		if c.Source != SrcCharacterAI || c.Kind != KindCommand {
			continue
		}
		m := simMotions[c.Command].Inputs
		if c.Command == "" || int(c.ExecFrame) != len(m)-1 || !hasPrefix(c.Inputs, m) {
			t.Fatalf("a case of the character's AI carries its command's inputs: %q, exec %d, inputs %v", c.Command, c.ExecFrame, c.Inputs)
		}
		if c.CancelFrom != 0 {
			cancels++
			if c.CancelFrom != 200 || c.MoveRef != 1000 || c.StartCtrl {
				t.Fatalf("a cancel case starts in the jab: from %d into %d, control %v", c.CancelFrom, c.MoveRef, c.StartCtrl)
			}
		}
		if c.MoveRef == 3000 {
			super++
			if c.Start.Self.PowerFrac*3000 < 1000-1 {
				t.Fatalf("the super's case was recorded without a bar of power: %.0f", c.Start.Self.PowerFrac*3000)
			}
		}
	}
	if cancels == 0 || super == 0 {
		t.Fatalf("expected cancel cases into the special and cases of the super: %d, %d", cancels, super)
	}
	// The engine's checks are counted per spot: the super was reachable with
	// a bar of power and cheating without.
	v := st.Verdicts[VerdictKey{State: 3000, Command: "qcf_c", From: -1, Stance: StStand}]
	if v == nil || v.Reachable == 0 || v.Cheat == 0 || v.ReachPower < 1000 || v.FailTop[0] >= 1000 {
		t.Fatalf("the super's spot: %+v", v)
	}
	if v := st.Verdicts[VerdictKey{State: 1100, Command: "qcf_b", From: 200, Stance: StStand}]; v == nil || v.Cheat == 0 || v.Reachable != 0 {
		t.Fatalf("a cancel into the two-part special is cheating: %+v", v)
	}
	// Not discarded: its walks were recorded too.
	movement := 0
	for _, c := range st.Cases {
		if c.Kind == KindMovement {
			movement++
		}
	}
	if movement == 0 {
		t.Fatal("the match's movement cases are kept")
	}
}

// TestCheatExchangesTeachNothing: a recorded exchange in which the
// character's AI made a move no person's command could have (a cancel into
// a state no person's cancel reaches) teaches nothing, not even the jab that
// began it, since the result is one no person could get; the same exchange
// with a cancel a person's command reaches teaches the jab and the cancel.
func TestCheatExchangesTeachNothing(t *testing.T) {
	record := func(cancel EntryKind, into int32, cmd string) *Store {
		eng := New(DefaultParams(), t.TempDir(), 1)
		eng.SetMotions("A", simMotions)
		eng.SetPlayer(0, "A", "B")
		eng.Record(0, SrcEngineAI)
		ff := &frameFeeder{eng: eng}
		frame := func(f CharSnap) {
			f.Routes = simRouteTable
			ff.frame(f)
		}
		for i := 0; i < 20; i++ {
			frame(CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true})
		}
		frame(CharSnap{StateNo: 200, StateTime: 1, MoveType: MtAttack, Entry: EntryReachable, EntryCommand: "a"})
		for i := int32(2); i <= 6; i++ {
			frame(CharSnap{StateNo: 200, StateTime: i, MoveType: MtAttack, MoveHit: i - 3})
		}
		frame(CharSnap{StateNo: into, StateTime: 1, MoveType: MtAttack, Entry: cancel, EntryCommand: cmd})
		for i := int32(2); i <= 20; i++ {
			frame(CharSnap{StateNo: into, StateTime: i, MoveType: MtAttack})
		}
		for i := 0; i < 40; i++ {
			frame(CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true})
		}
		ff.end()
		if err := eng.MatchEnd(); err != nil {
			t.Fatal(err)
		}
		st, _ := eng.Store("A")
		return st
	}
	st := record(EntryCheat, 1100, "")
	if n := casesBySource(st)[SrcCharacterAI]; len(n) != 0 {
		t.Fatalf("an exchange with a cheat teaches nothing: %v", n)
	}
	st = record(EntryReachable, 1000, "qcf_a")
	n := casesBySource(st)[SrcCharacterAI]
	if n[200] != 1 || n[1000] != 1 {
		t.Fatalf("the same exchange with a reachable cancel teaches the jab and the cancel: %v", n)
	}
}

// verifyStats is what a driver did over labelled rounds (see driveOver).
type verifyStats struct {
	rounds, frames, driven int
	// testsByRound holds each round's test plays.
	testsByRound []int32
	// Plays begun: tests, explorations, and the others (let through as
	// proved better), and of those, plays of a case whose command needed
	// verification (see Case.mustVerify) begun where it was not verified.
	tests, explored, proved, provedUnverified int
	// By move: plays begun, and those let through as proved.
	plays, provedBy map[int32]int
	// Plays of the super begun with less than a bar of power: by test or
	// exploration, and let through as proved.
	superLowTried, superLowProved int
	// Frames the CBR layer gave the input on which the character entered a
	// state no person's command reaches.
	cheatEntered int
	// Leftover input (see driver.carryOver): plays begun with a release, the
	// frames those took, and failed plays put down to leftover input.
	releases, releaseFrames, carried int
	// Plays the character's own AI took over from, as their chains resolved.
	contested int
	results   []roundResult
}

// driveOver plays rounds of player 0 driven by the CBR layer over the base
// AI from mk against lowBlind, as recordLabelled's rounds but on seeds from
// seedBase, and counts what the driver did.
func driveOver(eng *Engine, rounds, seedBase int, mk func(*rand.Rand) controller) *verifyStats {
	return driveOverAgainst(eng, rounds, seedBase, mk, oppLowBlind)
}

// driveOverAgainst is driveOver against opp; mk may be nil, for a character
// with no AI at all.
func driveOverAgainst(eng *Engine, rounds, seedBase int, mk func(*rand.Rand) controller, opp oppFactory) *verifyStats {
	vs := &verifyStats{plays: map[int32]int{}, provedBy: map[int32]int{}}
	for r := 0; r < rounds; r++ {
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		d := ps.drv
		var last *Case
		var lastPlays int32
		tests, explores := int32(0), int32(0)
		watch := func(s *sim, before bool) {
			me := s.ch[0]
			if !before {
				vs.frames++
				if me.cbr && me.entry != EntryNone && me.state == 1500 {
					vs.cheatEntered++
				}
				return
			}
			vs.driven++
			c := d.cur
			if c == nil || c == last && c.Outcome.Plays == lastPlays {
				return
			}
			last, lastPlays = c, c.Outcome.Plays
			vs.plays[c.MoveRef]++
			low := c.MoveRef == 3000 && ps.lastSelf.Power < 1000
			switch {
			case d.tests > tests:
				vs.tests++
				tests = d.tests
			case d.explores > explores:
				vs.explored++
				explores = d.explores
			default:
				vs.proved++
				vs.provedBy[c.MoveRef]++
				if c.mustVerify() && ps.st.check(c, &ps.lastSelf) != verdictVerified {
					vs.provedUnverified++
				}
				if low {
					vs.superLowProved++
				}
				return
			}
			if low {
				vs.superLowTried++
			}
		}
		var base controller
		if mk != nil {
			base = mk(rand.New(rand.NewSource(int64(seedBase + 500 + r))))
		}
		contested := int32(0)
		final := ps.ct.onFinal
		ps.ct.onFinal = func(rc *resolvedChain) {
			for _, cc := range rc.ch.cases {
				if cc.contested {
					contested++
				}
			}
			final(rc)
		}
		res, _ := labelledRound(eng, nil, base, opp(rand.New(rand.NewSource(int64(seedBase+r)))), roundPower(r), uint64(1000+r), watch)
		vs.contested += int(contested)
		vs.rounds++
		vs.testsByRound = append(vs.testsByRound, tests)
		vs.releases += d.releases
		vs.releaseFrames += d.releaseFrames
		vs.carried += d.carried
		vs.results = append(vs.results, res)
	}
	return vs
}

// aloneOver plays driveOver's rounds with the base AI alone.
func aloneOver(rounds, seedBase int, mk func(*rand.Rand) controller) []roundResult {
	var out []roundResult
	for r := 0; r < rounds; r++ {
		res, _ := labelledRound(nil, mk(rand.New(rand.NewSource(int64(seedBase+500+r)))), nil,
			&lowBlind{rng: rand.New(rand.NewSource(int64(seedBase + r)))}, roundPower(r), uint64(1000+r), nil)
		out = append(out, res)
	}
	return out
}

// TestChangeStateAIMovesAreVerifiedAndTakeOver: with no person's play at
// all, from a ChangeState AI recorded with the engine's labels, the CBR
// layer verifies the moves it learned with test plays, within the round's
// budget of them and less and less as the spots settle, and then takes over
// with the moves that prove better than that same AI: against an opponent
// that never guards low, the low kick it uses only now and then. It never
// plays the AI-only move, and the super that the AI also uses without power
// is verified only with a bar of power and never let through without.
func TestChangeStateAIMovesAreVerifiedAndTakeOver(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const recorded, learn, eval = 10, 30, 10
	mk := func(r *rand.Rand) controller { return &changeStater{rng: r} }
	p := DefaultParams()
	eng := New(p, t.TempDir(), 7)
	eng.SetMotions("Rush", simMotions)
	recordLabelled(t, eng, SrcEngineAI, recorded, 40, mk)
	st, _ := eng.Store("Rush")
	cases := st.Len()

	learning := driveOver(eng, learn, 9000, mk)
	alone := aloneOver(eval, 7000, mk)
	with := driveOver(eng, eval, 7000, mk)
	diff := make([]roundResult, eval)
	for r := range diff {
		diff[r].net = with.results[r].net - alone[r].net
	}
	aloneNet, _, _ := summarize(alone)
	withNet, _, _ := summarize(with.results)
	gain, se, _ := summarize(diff)
	t.Logf("net per round: base AI alone %.0f, with the CBR layer %.0f (difference %+.0f, standard error %.0f)", aloneNet, withNet, gain, se)
	for _, vs := range []*verifyStats{learning, with} {
		t.Logf("%d rounds: driven %.1f%% of frames; plays begun: %d tests, %d explored, %d proved (%v), by move %v; tests a round %v",
			vs.rounds, pct(vs.driven, vs.frames), vs.tests, vs.explored, vs.proved, vs.provedBy, vs.plays, vs.testsByRound)
		t.Logf("  super begun below a bar of power: %d tried, %d as proved; leftover input: %d releases, %.1f frames on average, %d failures put down to it",
			vs.superLowTried, vs.superLowProved, vs.releases, float32(vs.releaseFrames)/float32(max(vs.releases, 1)), vs.carried)
	}
	t.Logf("verdicts:\n%s", verdictString(st))

	all := []*verifyStats{learning, with}
	for _, vs := range all {
		for r, n := range vs.testsByRound {
			if n > p.VerifyPerRound {
				t.Errorf("round %d had %d test plays, over the budget of %d", r, n, p.VerifyPerRound)
			}
		}
		if vs.provedUnverified > 0 {
			t.Errorf("%d take-overs with a move not verified where it took over", vs.provedUnverified)
		}
		if vs.cheatEntered > 0 {
			t.Errorf("the CBR layer's input entered the AI-only move %d times", vs.cheatEntered)
		}
		if vs.superLowProved > 0 {
			t.Errorf("the super was let through as proved %d times without a bar of power", vs.superLowProved)
		}
	}
	// Tests stop as the spots settle: the first third of the learning
	// rounds holds most of them, and the evaluation rounds few.
	third := func(vs *verifyStats, i int) (n int32) {
		k := len(vs.testsByRound) / 3
		for _, x := range vs.testsByRound[i*k : (i+1)*k] {
			n += x
		}
		return n
	}
	if early, late := third(learning, 0), third(learning, 2); early == 0 || late*2 > early {
		t.Errorf("tests do not settle: %d in the first third of the learning rounds, %d in the last", early, late)
	}
	// The super: verified only with a bar of power, wherever it was, and
	// found out without one.
	for k, v := range st.Verdicts {
		if k.State == 3000 && v.Successes > 0 && v.OKPower < 1000 {
			t.Errorf("the super was verified with %d power, less than a bar: %+v %+v", v.OKPower, k, v)
		}
	}
	k := VerdictKey{State: 3000, Command: "qcf_c", From: -1, Stance: StStand}
	if v := st.Verdicts[k]; v == nil || v.Successes == 0 || st.verdictAt(k, 1000) != verdictVerified || st.verdictAt(k, 0) != verdictBlocked || !st.cheating(k, 0) {
		t.Fatalf("the super's spot with control is verified with a bar and cheating without: %+v", v)
	}
	// Moves proved better take over, and the CBR layer does better than the
	// AI alone.
	if with.provedBy[400] == 0 {
		t.Errorf("the low kick never took over as proved better")
	}
	if gain <= 0 {
		t.Errorf("the CBR layer made the AI no better: %+.0f a round (standard error %.0f)", gain, se)
	}
	if st.Len() != cases {
		t.Errorf("driving changed the store's cases: %d, was %d", st.Len(), cases)
	}
}

// TestLeftoverInputIsReleased: when the CBR layer begins a quarter circle
// forward right after the base AI walked forward, the walk's forward and the
// quarter circle read as forward, down, down-forward: a command checked
// first (the simulator's Upper, as Kung Fu Man's) comes out instead of the
// case's. The CBR layer holds neutral first, long enough for the forward to
// leave the command's window, and the case's command comes out every time.
// Where the character's window is longer than the CBR layer takes a motion
// to last, the Upper still comes out now and then; such failures are put
// down to the leftover input and count neither against the case nor against
// its command's spot (the cases the walk comes before are those begun with
// control; one begun in hitstun, a reversal, fails or not on its own).
func TestLeftoverInputIsReleased(t *testing.T) {
	run := func(window int) (plays, fails, releases, frames, carried int, st *Store) {
		p := DefaultParams()
		p.TakeoverExplore, p.TakeoverExplorePromising, p.TakeoverExploreMax = 1, 1, 1000
		eng := New(p, t.TempDir(), 3)
		eng.SetMotions("Rush", simMotionsUpper)
		// The engine AI's command cheat teaches the quarter circle with its
		// canonical inputs.
		for r := 0; r < 4; r++ {
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Record(0, SrcEngineAI)
			labelledRound(eng, &cheater{rng: rand.New(rand.NewSource(int64(60 + r)))}, nil,
				&defender{rng: rand.New(rand.NewSource(int64(80 + r)))}, 0, uint64(r+1), nil)
		}
		if err := eng.MatchEnd(); err != nil {
			t.Fatal(err)
		}
		st, _ = eng.Store("Rush")
		var qcf []*Case
		for _, c := range st.Cases {
			if c.MoveRef == 1000 && c.Command == "qcf_a" && c.StartCtrl {
				qcf = append(qcf, c)
			}
		}
		if len(qcf) == 0 {
			t.Fatal("setup: no quarter circle case was recorded")
		}
		for r := 0; r < 6; r++ {
			eng.SetPlayer(0, "Rush", "Guard")
			eng.SetPlayer(1, "Guard", "Rush")
			eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
			d := eng.players[0].drv
			s := newSim(3600)
			s.cmdLayer, s.entries, s.upper, s.upperWindow = true, true, true, window
			opp := &defender{rng: rand.New(rand.NewSource(int64(90 + r)))}
			runLabelled(s, eng, walkIn{}, opp)
			releases += d.releases
			frames += d.releaseFrames
			carried += d.carried
		}
		for _, c := range qcf {
			plays += int(c.Outcome.Plays)
			fails += int(c.Outcome.ExecFails)
		}
		return plays, fails, releases, frames, carried, st
	}
	plays, fails, releases, frames, carried, _ := run(0)
	t.Logf("window 12: %d plays of the quarter circle, %d failed; %d releases, %.1f frames on average; %d failures put down to leftover input",
		plays, fails, releases, float32(frames)/float32(max(releases, 1)), carried)
	if plays == 0 {
		t.Fatal("setup: the quarter circle should be played")
	}
	if fails > 0 || carried > 0 {
		t.Fatalf("with the release, the quarter circle comes out: %d of %d failed, %d put down to leftover input", fails, plays, carried)
	}
	if releases == 0 {
		t.Fatalf("setup: the quarter circle should be played right after the walk: %d plays, %d releases", plays, releases)
	}
	if float32(frames) > float32(releases)*float32(DefaultParams().MaxMotionFrames+2) {
		t.Fatalf("a release lasts at most MaxMotionFrames+2 frames: %d frames over %d", frames, releases)
	}
	plays, fails, releases, frames, carried, st := run(24)
	t.Logf("window 24: %d plays of the quarter circle, %d failed; %d releases, %.1f frames on average; %d failures put down to leftover input",
		plays, fails, releases, float32(frames)/float32(max(releases, 1)), carried)
	if fails > 0 {
		t.Fatalf("failures put down to leftover input do not count against the case: %d of %d", fails, plays)
	}
	if carried == 0 {
		t.Fatal("setup: with a longer window the Upper should still come out now and then")
	}
	for k, v := range st.Verdicts {
		if k.State == 1000 && k.From < 0 && v.Fails > 0 {
			t.Fatalf("nor against its command's spot: %+v %+v", k, v)
		}
	}
}

// runLabelled plays a round on s, set up by the caller, with player 0 driven
// over base and opp as player 1.
func runLabelled(s *sim, eng *Engine, base, opp controller) {
	eng.Frame(s.snapshot())
	for f := 0; f < 3600 && s.roundSt == 2; f++ {
		me := s.ch[0]
		me.cbr = false
		in := [2]InputBits{0, opp.next(s, 1)}
		if btn, ok := eng.Input(0, me.facingRight); ok {
			in[0], me.cbr = FromEngine(btn, me.facingRight), true
		} else {
			in[0] = base.next(s, 0)
		}
		s.step(in)
		eng.Frame(s.snapshot())
	}
	if s.roundSt == 2 {
		s.roundSt = 3
	}
	eng.RoundEnd(s.snapshot())
}

// TestContestsByLabel: with the engine's labels, a state change during a
// move the CBR layer plays is the character's own AI taking over exactly
// when its label says the AI decided it, whatever the control, the move type
// or the state: a cancel out of the move, which the old guess took for the
// move's own next part (see TestMidMoveTakeOverIsCreditedToTheMove), or a
// change into a state that is no attack. The move's own next part and a
// change the CBR layer's input made are never taken for one.
func TestContestsByLabel(t *testing.T) {
	routes := &RouteTable{Person: map[int32][]string{200: {"a"}, 1100: {"qcf_b"}}}
	for _, tc := range []struct {
		name      string
		to        CharSnap
		contested bool
	}{
		{"the AI cancels the jab, reachable", CharSnap{StateNo: 1100, MoveType: MtAttack, Entry: EntryReachable, EntryCommand: "qcf_b"}, true},
		{"the AI cancels the jab, cheating", CharSnap{StateNo: 1100, MoveType: MtAttack, Entry: EntryCheat}, true},
		{"the AI cancels the jab, unchecked", CharSnap{StateNo: 1100, MoveType: MtAttack, Entry: EntryUnknown}, true},
		{"the AI jumps out of the jab", CharSnap{StateNo: 40, StateType: StAir, MoveType: MtIdle, Entry: EntryReachable, EntryCommand: "up"}, true},
		{"the jab's own next part", CharSnap{StateNo: 201, MoveType: MtAttack, Entry: EntryAuto}, false},
		{"the CBR layer's command", CharSnap{StateNo: 1100, MoveType: MtAttack, Entry: EntryInput, EntryCommand: "qcf_b"}, false},
	} {
		p := withoutBaseAI()
		eng := New(p, t.TempDir(), 1)
		st, _ := eng.Store("A")
		start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
		c := &Case{ID: 1, Source: SrcHumanLocal, Kind: KindCommand, ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{InA, 0, 0, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start}
		st.Admit(p, []*Case{c})
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		ff := &frameFeeder{eng: eng}
		frame := func(f CharSnap) {
			f.Routes = routes
			ff.frame(f)
		}
		frame(CharSnap{Ctrl: true, MoveType: MtIdle})
		if _, ok := eng.Input(0, true); !ok || ps.drv.cur != c {
			t.Fatalf("%s: setup: the case should play", tc.name)
		}
		frame(CharSnap{StateNo: 200, StateTime: 1, MoveType: MtAttack, CBRDriven: true, Entry: EntryInput, EntryCommand: "a"})
		eng.Input(0, true)
		frame(CharSnap{StateNo: 200, StateTime: 2, MoveType: MtAttack, CBRDriven: true})
		eng.Input(0, true)
		to := tc.to
		to.StateTime, to.CBRDriven = 1, true
		frame(to)
		contested := ps.drv.cur == nil && ps.drv.waitCtrl && ps.unexplained == 1
		if contested != tc.contested || c.Outcome.ExecFails != 0 {
			t.Errorf("%s: taken over %v (playing %v, waiting %v, unexplained %d), want %v; %d failed",
				tc.name, contested, ps.drv.cur != nil, ps.drv.waitCtrl, ps.unexplained, tc.contested, c.Outcome.ExecFails)
		}
	}
}

// idle gives no input: the recording of a character with no AI at all.
type idle struct{}

func (idle) next(*sim, int) InputBits { return 0 }

// TestEveryKindLearnsWithLabels plays every kind of character AI the
// simulator models (see aiKinds), and the ChangeState AI of the tests above,
// with the engine's labels on and no person's play at all: each is recorded
// as the engine AI's play, as the game records a CPU player, and then plays
// as the base AI with the CBR layer driving over it. It reports what each
// kind teaches, how its moves that only a direct state change reaches are
// found out, how many test plays it takes, and how many plays fail, and
// checks for every kind that test plays stay within their budget, that no
// move takes over where its command is not verified, and that the CBR
// layer's input never enters a state no person's command reaches.
func TestEveryKindLearnsWithLabels(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	type kind struct {
		name string
		mk   func(*rand.Rand) controller
		opp  oppFactory
	}
	kinds := []kind{{"changestate-cheats", func(r *rand.Rand) controller { return &changeStater{rng: r} }, oppLowBlind}}
	for _, k := range aiKinds {
		kinds = append(kinds, kind{k.name, k.base, oppDefender})
	}
	const recorded, drive = 6, 12
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			t.Parallel()
			p := DefaultParams()
			eng := New(p, t.TempDir(), 7)
			eng.SetMotions("Rush", simMotions)
			recordLabelledAgainst(t, eng, SrcEngineAI, recorded, 40, k.mk, k.opp)
			st, _ := eng.Store("Rush")
			before := totals(st)
			vs := driveOverAgainst(eng, drive, 9000, k.mk, k.opp)
			pt := totals(st).sub(before)

			by := casesBySource(st)
			names := map[Source]string{SrcCharacterAI: "character AI", SrcEngineAI: "engine AI (assisted)", SrcSynthesized: "hypothesis"}
			var taught []string
			for src, moves := range by {
				n := names[src]
				if n == "" {
					n = "source " + itoa(int32(src))
				}
				taught = append(taught, n+" "+fmtMoves(moves))
			}
			sort.Strings(taught)
			moment, blocked, cheating, verified, open := int32(0), 0, 0, 0, 0
			for key, v := range st.Verdicts {
				moment += v.Cheat
				switch st.verdictAt(key, 0) {
				case verdictBlocked:
					blocked++
					if st.cheating(key, 0) {
						cheating++
					}
				case verdictVerified:
					verified++
				default:
					open++
				}
			}
			t.Logf("teaches: %s; flagged %v, %d hypotheses", strings.Join(taught, "; "), st.ChangeStateAI, len(st.Hypotheses))
			t.Logf("cheat moves: %d moment checks found no person's route; spots at no power: %d verified, %d blocked (%d cheating), %d open",
				moment, verified, blocked, cheating, open)
			t.Logf("test plays a round %v (%d in all), %d explored, %d proved; %d plays: %d failed, %d aborted; %d contested; driven %.1f%% of frames",
				vs.testsByRound, vs.tests, vs.explored, vs.proved, pt.plays, pt.fails, pt.aborts, vs.contested, pct(vs.driven, vs.frames))

			for r, n := range vs.testsByRound {
				if n > p.VerifyPerRound {
					t.Errorf("round %d had %d test plays, over the budget of %d", r, n, p.VerifyPerRound)
				}
			}
			if vs.provedUnverified > 0 {
				t.Errorf("%d take-overs with a move not verified where it took over", vs.provedUnverified)
			}
			if vs.cheatEntered > 0 {
				t.Errorf("the CBR layer's input entered a state no person's command reaches %d times", vs.cheatEntered)
			}
			if by[SrcCharacterAI][1500] > 0 {
				t.Errorf("%d cases for the AI-only move", by[SrcCharacterAI][1500])
			}
			// What each kind teaches: the AIs that change states directly, in
			// the root or from a helper, their moves a person's command
			// reaches; the one whose moves people input are switched off,
			// nothing, its decisions all found out.
			switch k.name {
			case "changestate-cheats", "ailevel-changestate", "latched", "switch-code", "switch-code-resets", "helper-redirect":
				if len(by[SrcCharacterAI]) == 0 {
					t.Errorf("a ChangeState AI teaches its reachable moves as cases of the character's AI")
				}
			case "latched-moves-off":
				if len(by[SrcCharacterAI]) != 0 || moment == 0 {
					t.Errorf("with the moves people input switched off, every decision is cheating: %v, %d found", by[SrcCharacterAI], moment)
				}
			}
		})
	}
}

// fmtMoves lists move counts in state order.
func fmtMoves(m map[int32]int) string {
	var states []int32
	for s := range m {
		states = append(states, s)
	}
	sort.Slice(states, func(i, j int) bool { return states[i] < states[j] })
	var parts []string
	for _, s := range states {
		parts = append(parts, itoa(s)+"×"+itoa(int32(m[s])))
	}
	return strings.Join(parts, " ")
}

// TestUnverifiedMovesOnlyTest: a move of the character's AI that proved
// better than the base AI still does not take over where its command has
// not been verified: there it is only played as a test, within the round's
// test plays. A success verifies its own spot, so then it takes over there,
// and not in another spot; three clean failures there find it out, and it
// is not even tested any more. A person's move needs no verification.
func TestUnverifiedMovesOnlyTest(t *testing.T) {
	p := DefaultParams()
	choose := func(d *driver, cur *Feat, self *CharSnap, g *gate) (*Case, map[string]int) {
		why := map[string]int{}
		c, _ := d.chooseWhy(cur, self, 1, why, g)
		return c, why
	}
	setup := func(src Source) (*driver, *Case, *Feat, *CharSnap, uint64) {
		d, c, cur, self, key := gateFixture(p)
		c.Source, c.Command = src, "a"
		setPlayed(p, d.st, c, 10, 0.2)
		setBase(d.st, key, 50, 0)
		return d, c, cur, self, key
	}
	d, c, cur, self, key := setup(SrcHumanLocal)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != c {
		t.Fatal("setup: a person's move that proved better takes over")
	}
	d, c, cur, self, key = setup(SrcCharacterAI)
	g := &gate{key: key}
	if got, why := choose(d, cur, self, g); got != nil || !g.held || why[RejUnverified] != 1 {
		t.Fatalf("an unverified move does not take over: %v held %v why %v", got, g.held, why)
	}
	g = &gate{key: key, test: true}
	if got, _ := choose(d, cur, self, g); got != c || !g.tested || g.explored {
		t.Fatalf("it is played as a test: %v tested %v explored %v", got, g.tested, g.explored)
	}
	crouching := *self
	crouching.StateType = StCrouch
	d.st.successAt(spotKey(200, "a", &crouching), 0)
	if got, _ := choose(d, cur, self, &gate{key: key}); got != nil {
		t.Fatal("a success in another spot verifies nothing here")
	}
	d.st.successAt(spotKey(200, "a", self), 0)
	g = &gate{key: key, test: true}
	if got, _ := choose(d, cur, self, g); got != c || g.tested {
		t.Fatalf("verified here, it takes over as proved better: %v tested %v", got, g.tested)
	}
	d, c, cur, self, key = setup(SrcCharacterAI)
	for i := 0; i < 3; i++ {
		d.st.failAt(spotKey(200, "a", self), 0)
	}
	if got, why := choose(d, cur, self, &gate{key: key, test: true, explore: true}); got != nil || why[RejCheat] != 1 {
		t.Fatalf("found out, it is not even tested: %v why %v", got, why)
	}
	// The driver keeps to the round's test plays.
	d, c, cur, self, _ = setup(SrcCharacterAI)
	tests := 0
	for i := int32(0); i < 10; i++ {
		self.StateNo = i // a new decision situation each time
		if d.decide(cur, self, int64(i)) == c {
			tests++
		}
	}
	if tests != int(p.VerifyPerRound) || d.tests != p.VerifyPerRound {
		t.Fatalf("%d test plays in a round, want %d", tests, p.VerifyPerRound)
	}
	d.roundEnd()
	if d.decide(cur, self, 20) != c {
		t.Fatal("the next round tests again")
	}
}

// TestTestPlaysVerifyOnlyTheirOwnCommand: a test play verifies its command
// in its spot only when the state comes out of the CBR layer's input as that
// command's person's route (EntryInput with the command): not when the
// character's own AI brought it about (which takes over from the play), nor
// through another command, which is noted as that command's success instead.
func TestTestPlaysVerifyOnlyTheirOwnCommand(t *testing.T) {
	routes := &RouteTable{Person: map[int32][]string{200: {"a", "b"}}}
	for _, tc := range []struct {
		name         string
		entry        EntryKind
		cmd          string
		mine, theirs int32 // successes of "a" where the play began, and of "b" there
	}{
		{"the play's own command", EntryInput, "a", 1, 0},
		{"another command", EntryInput, "b", 0, 1},
		{"the character's AI", EntryReachable, "a", 0, 0},
	} {
		p := DefaultParams()
		eng := New(p, t.TempDir(), 1)
		st, _ := eng.Store("A")
		start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
		c := &Case{ID: 1, Source: SrcCharacterAI, Kind: KindCommand, Command: "a", ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{InA, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start,
			Outcome: Outcome{Trials: 1, ValueSum: 0.2}}
		st.Admit(p, []*Case{c})
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ps := eng.players[0]
		ff := &frameFeeder{eng: eng}
		frame := func(f CharSnap) {
			f.Routes = routes
			ff.frame(f)
		}
		standing := CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true}
		frame(standing)
		if _, ok := eng.Input(0, true); !ok || ps.drv.cur != c || ps.drv.tests != 1 {
			t.Fatalf("%s: setup: the case should be played as a test", tc.name)
		}
		frame(CharSnap{StateNo: 200, StateTime: 1, MoveType: MtAttack, CBRDriven: true, Entry: tc.entry, EntryCommand: tc.cmd})
		mine, theirs := st.Verdicts[spotKey(200, "a", &standing)], st.Verdicts[spotKey(200, "b", &standing)]
		got := [2]int32{}
		if mine != nil {
			got[0] = mine.Successes
		}
		if theirs != nil {
			got[1] = theirs.Successes
		}
		if got != [2]int32{tc.mine, tc.theirs} {
			t.Errorf("%s: successes of the play's command and of the other one: %v, want %v", tc.name, got, [2]int32{tc.mine, tc.theirs})
		}
		if contested := ps.drv.cur == nil && ps.drv.waitCtrl; contested != (tc.entry == EntryReachable) {
			t.Errorf("%s: taken over %v", tc.name, contested)
		}
	}
}

// TestFlaggedMatchKeepsCheckedDecisions: a source flagged for changing states
// directly in ways the engine could not check (EntryUnknown) has the
// match's recordings discarded as without the labels, except the decisions
// the engine checked, which become cases of the character's AI all the same.
func TestFlaggedMatchKeepsCheckedDecisions(t *testing.T) {
	eng := New(DefaultParams(), t.TempDir(), 1)
	eng.SetMotions("A", simMotions)
	eng.SetPlayer(0, "A", "B")
	eng.Record(0, SrcEngineAI)
	ff := &frameFeeder{eng: eng}
	frame := func(f CharSnap) {
		f.Routes = simRouteTable
		ff.frame(f)
	}
	standing := func(n int) {
		for i := 0; i < n; i++ {
			frame(CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true, Input: InF})
		}
	}
	attack := func(no int32, k EntryKind, cmd string) {
		frame(CharSnap{StateNo: no, StateTime: 1, MoveType: MtAttack, Entry: k, EntryCommand: cmd})
		for i := int32(2); i <= 20; i++ {
			frame(CharSnap{StateNo: no, StateTime: i, MoveType: MtAttack})
		}
	}
	standing(30)
	attack(200, EntryReachable, "a")
	standing(60)
	attack(1000, EntryUnknown, "")
	standing(30)
	attack(1000, EntryUnknown, "")
	standing(30)
	ff.end()
	if err := eng.MatchEnd(); err != nil {
		t.Fatal(err)
	}
	st, _ := eng.Store("A")
	if !st.ChangeStateAI {
		t.Fatal("setup: two unchecked attack starts flag the source")
	}
	var kept []*Case
	for _, c := range st.Cases {
		kept = append(kept, c)
	}
	if len(kept) != 1 || kept[0].Source != SrcCharacterAI || kept[0].MoveRef != 200 {
		t.Fatalf("only the checked decision is kept: %d cases %+v", len(kept), kept)
	}
}

// TestLeftoverInputFound: after the character held forward, a quarter circle
// forward with A completes the Upper too (forward, down, down-forward with
// A); begun with control, the case first holds neutral until the forward is
// out of the frames a command's time can span, and begun mid-action, where
// its inputs must keep their recorded timing, it holds none but its
// leftover input is noted all the same. Holding back completes no other
// move, and nothing is done for a case whose first direction is the one
// held, nor while the motions are unknown or the base AI is off.
func TestLeftoverInputFound(t *testing.T) {
	qcf := simMotionsUpper["qcf_a"].Inputs
	mk := func(ctrl bool) *Case {
		return &Case{Source: SrcCharacterAI, Kind: KindCommand, Command: "qcf_a", MoveRef: 1000, ExecFrame: int32(len(qcf) - 1),
			Inputs: append(append([]InputBits(nil), qcf...), 0, 0), StartCtrl: ctrl}
	}
	carry := func(p *Params, motions map[string]Motion, held InputBits, c *Case) (int, bool) {
		d := newDriver(p, 0, Levels{Execution: 8, Knowledge: 8}, 1, NewStore("x"), newChainTracker(p, 0, new(uint64)))
		d.motions = motions
		for i := 0; i < 20; i++ {
			d.observe(&CharSnap{Input: 0})
		}
		for i := 0; i < 8; i++ {
			d.observe(&CharSnap{Input: held})
		}
		return d.carryOver(c)
	}
	// Commands that only hold a direction, as most command files have, are
	// no moves the leftover input could make.
	holds := map[string]Motion{"holdback": {[]InputBits{InB}, 1}, "holdfwd": {[]InputBits{InF}, 1}}
	for k, v := range simMotionsUpper {
		holds[k] = v
	}
	p := DefaultParams()
	w := int(p.MaxMotionFrames) + 2
	if r, c := carry(p, holds, InF, mk(true)); r != w-len(qcf) || !c {
		t.Fatalf("with control after forward: release %d (want %d), carry %v", r, w-len(qcf), c)
	}
	if r, c := carry(p, simMotionsUpper, InF, mk(false)); r != 0 || !c {
		t.Fatalf("mid-action after forward: release %d, carry %v; want none, and noted", r, c)
	}
	for name, tc := range map[string]struct {
		p       *Params
		motions map[string]Motion
		held    InputBits
	}{
		"after back":           {p, holds, InB},
		"after down":           {p, holds, InD},
		"without the Upper":    {p, simMotions, InF},
		"motions unknown":      {p, nil, InF},
		"with the base AI off": {withoutBaseAI(), simMotionsUpper, InF},
	} {
		if r, c := carry(tc.p, tc.motions, tc.held, mk(true)); r != 0 || c {
			t.Errorf("%s: release %d, carry %v; want none", name, r, c)
		}
	}
}

// TestTestFailureWhileGuardingSaysNothing: a test play whose command did not
// come out because the character had to guard counts as a failed execution
// of the case, as always, but not against its command's spot, which a
// person's command could reach all the same.
func TestTestFailureWhileGuardingSaysNothing(t *testing.T) {
	for _, guard := range []bool{false, true} {
		p := DefaultParams()
		eng := New(p, t.TempDir(), 1)
		st, _ := eng.Store("A")
		start := Feat{RoundState: 2, Enemy: CharFeat{Present: true, RelX: 150, State: StStand, Ctrl: true, Move: MtIdle, LifeFrac: 1}, TimeFrac: -1}
		c := &Case{ID: 1, Source: SrcCharacterAI, Kind: KindCommand, Command: "a", ExecFrame: 0, MoveRef: 200, ExecAttack: true, ExecClass: StStand,
			Inputs: []InputBits{InA, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, StartStateType: StStand, StartCtrl: true, StartMoveType: MtIdle, Start: start,
			Outcome: Outcome{Trials: 1, ValueSum: 0.2}}
		st.Admit(p, []*Case{c})
		eng.SetPlayer(0, "A", "B")
		eng.Drive(0, Levels{Execution: 8, Knowledge: 8})
		ff := &frameFeeder{eng: eng}
		standing := CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true, Routes: simRouteTable}
		ff.frame(standing)
		for i := 0; i < 10; i++ {
			if _, ok := eng.Input(0, true); !ok {
				break
			}
			f := standing
			f.CBRDriven = true
			if guard {
				f = CharSnap{StateNo: 150, StateTime: int32(i + 1), MoveType: MtHit, BlockStun: 10, CBRDriven: true, Routes: simRouteTable}
			}
			ff.frame(f)
		}
		v := st.Verdicts[spotKey(200, "a", &standing)]
		if c.Outcome.ExecFails != 1 {
			t.Fatalf("guarding %v: setup: the command did not come out, %d failures", guard, c.Outcome.ExecFails)
		}
		if failed := v != nil && v.Fails > 0; failed == guard {
			t.Fatalf("guarding %v: counted against the spot %v", guard, failed)
		}
	}
}

// TestAutoFollowUpIsNoCancel: with the engine's labels, a move's own next
// part (EntryAuto) is no cancel even when a command happens to be active,
// as the engine AI's button jam or a person mashing makes one; without the
// labels it is taken for one, as before.
func TestAutoFollowUpIsNoCancel(t *testing.T) {
	record := func(labels bool) int {
		eng := New(DefaultParams(), t.TempDir(), 1)
		eng.SetMotions("A", simMotions)
		eng.SetPlayer(0, "A", "B")
		eng.Record(0, SrcHumanLocal)
		ff := &frameFeeder{eng: eng}
		frame := func(f CharSnap) {
			if labels {
				f.Routes = simRouteTable
			} else {
				f.Entry, f.EntryCommand = EntryNone, ""
			}
			ff.frame(f)
		}
		for i := 0; i < 20; i++ {
			frame(CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true})
		}
		frame(CharSnap{StateNo: 1100, StateTime: 1, MoveType: MtAttack, Input: InBtnB, Commands: []string{"qcf_b"}, Entry: EntryInput, EntryCommand: "qcf_b"})
		for i := int32(2); i <= 11; i++ {
			frame(CharSnap{StateNo: 1100, StateTime: i, MoveType: MtAttack})
		}
		// The next part, while the jab's command is active.
		frame(CharSnap{StateNo: 1101, StateTime: 1, MoveType: MtAttack, Input: InA, Commands: []string{"a"}, Entry: EntryAuto})
		for i := int32(2); i <= 20; i++ {
			frame(CharSnap{StateNo: 1101, StateTime: i, MoveType: MtAttack})
		}
		for i := 0; i < 40; i++ {
			frame(CharSnap{StateType: StStand, MoveType: MtIdle, Ctrl: true})
		}
		ff.end()
		st, _ := eng.Store("A")
		return casesBySource(st)[SrcHumanLocal][1101]
	}
	if n := record(false); n != 1 {
		t.Fatalf("setup: without labels the next part is taken for a cancel: %d cases", n)
	}
	if n := record(true); n != 0 {
		t.Fatalf("with the label it is the move's own: %d cases", n)
	}
}
