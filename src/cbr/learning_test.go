package cbr

import (
	"math/rand"
	"os"
	"testing"
)

// Learning tests: a scripted player (rushdown) is recorded against a
// scripted opponent, then the CBR layer plays in its place against the same
// opponent on fixed seeds.

type oppFactory func(rng *rand.Rand) controller

// roundResult is one driven round: net damage (dealt minus taken) and the
// outcome (1 win, 0 draw, -1 loss).
type roundResult struct {
	dealt, net int32
	outcome    int
}

// playDrivenVs plays n rounds with player 0 driven by the CBR layer.
func playDrivenVs(eng *Engine, n, seedBase int, lv Levels, opp oppFactory) []roundResult {
	var out []roundResult
	for r := 0; r < n; r++ {
		s := newSim(3600)
		oc := opp(rand.New(rand.NewSource(int64(seedBase + r))))
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Drive(0, lv)
		runRound(s, eng, [2]controller{nil, oc}, 3600)
		res := roundResult{dealt: 1000 - s.ch[1].life, net: s.ch[0].life - s.ch[1].life}
		switch {
		case s.ch[1].life < s.ch[0].life:
			res.outcome = 1
		case s.ch[1].life > s.ch[0].life:
			res.outcome = -1
		}
		out = append(out, res)
	}
	return out
}

func recordVs(t *testing.T, eng *Engine, rounds int, seed int64, opp oppFactory) {
	t.Helper()
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		rng := rand.New(rand.NewSource(seed + int64(r)))
		eng.SetPlayer(0, "Rush", "Guard")
		eng.SetPlayer(1, "Guard", "Rush")
		eng.Record(0, SrcHumanLocal)
		runRound(s, eng, [2]controller{&rushdown{rng: rng}, opp(rand.New(rand.NewSource(seed + 77 + int64(r))))}, 3600)
	}
}

func summarize(rs []roundResult) (net, se, win float32) {
	var sum, sq float64
	wins := 0
	for _, r := range rs {
		sum += float64(r.net)
		if r.outcome > 0 {
			wins++
		}
	}
	m := sum / float64(len(rs))
	for _, r := range rs {
		d := float64(r.net) - m
		sq += d * d
	}
	if len(rs) > 1 {
		se = sqrt32(float32(sq/float64(len(rs)-1))) / sqrt32(float32(len(rs)))
	}
	return float32(m), se, float32(wins) / float32(len(rs))
}

var (
	oppDefender oppFactory = func(r *rand.Rand) controller { return &defender{rng: r} }
	oppLowBlind oppFactory = func(r *rand.Rand) controller { return &lowBlind{rng: r} }
)

func pureRetrieval(p *Params) { p.ValueWeight, p.ExplorationWeight, p.CaseReuseWeight = 0, 0, 0 }

// TestValueLearningImprovesPlay: after recording the scripted player, the
// CBR layer plays 30 rounds against the same guarding opponent and learns
// from their outcomes. On fixed evaluation seeds it should then deal clearly
// more net damage than pure retrieval, which only reproduces the recorded
// player's choices.
func TestValueLearningImprovesPlay(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	run := func(mod func(*Params), learn int) (float32, float32) {
		p := DefaultParams()
		if mod != nil {
			mod(p)
		}
		eng := New(p, t.TempDir(), 7)
		recordVs(t, eng, 20, 40, oppDefender)
		playDrivenVs(eng, learn, 9000, lv, oppDefender)
		net, _, win := summarize(playDrivenVs(eng, 10, 7000, lv, oppDefender))
		return net, win
	}
	pureNet, pureWin := run(pureRetrieval, 0)
	net, win := run(nil, 30)
	t.Logf("net damage per round: pure retrieval %.0f (win %.0f%%), defaults after 30 learning rounds %.0f (win %.0f%%)", pureNet, 100*pureWin, net, 100*win)
	if net < pureNet+100 {
		t.Fatalf("learning should improve on retrieval: %.0f vs %.0f", net, pureNet)
	}
}

// TestPlaybackFidelityFloor: pure retrieval against the opponent it was
// recorded against should reproduce a fair share of the recorded player's
// damage. A fall below the floor means cases no longer replay as recorded.
func TestPlaybackFidelityFloor(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	const rounds = 12
	var base int32
	for r := 0; r < rounds; r++ {
		s := newSim(3600)
		runRound(s, nil, [2]controller{&rushdown{rng: rand.New(rand.NewSource(int64(r)))}, &defender{rng: rand.New(rand.NewSource(int64(900 + r)))}}, 3600)
		base += 1000 - s.ch[1].life
	}
	p := DefaultParams()
	pureRetrieval(p)
	eng := New(p, t.TempDir(), 7)
	recordRounds(t, eng, 20, 40)
	var dealt int32
	for _, r := range playDrivenVs(eng, rounds, 900, Levels{Execution: 8, Knowledge: 8}, oppDefender) {
		dealt += r.dealt
	}
	t.Logf("damage per round over %d rounds: scripted %d, CBR pure retrieval %d (%.0f%%)",
		rounds, base/rounds, dealt/rounds, 100*float32(dealt)/float32(base))
	if float32(dealt) < 0.35*float32(base) {
		t.Fatalf("fidelity below floor: %d of %d", dealt, base)
	}
}

// TestLearningCurve is the measurement harness behind the default value
// settings. It is slow (minutes) and runs only when CBR_LEARNING_CURVE names
// an opponent: defender or lowBlind. For each variant it records 20 rounds,
// evaluates 10 rounds, plays 30 learning rounds, and evaluates the same 10
// rounds again, over 4 independent trials.
func TestLearningCurve(t *testing.T) {
	opps := map[string]oppFactory{"defender": oppDefender, "lowBlind": oppLowBlind}
	name := os.Getenv("CBR_LEARNING_CURVE")
	of, ok := opps[name]
	if !ok {
		t.Skip("set CBR_LEARNING_CURVE to defender or lowBlind")
	}
	variants := []struct {
		name string
		mod  func(p *Params)
	}{
		{"pure retrieval", pureRetrieval},
		{"defaults", func(p *Params) {}},
		{"defaults, exploration 0.05", func(p *Params) { p.ExplorationWeight = 0.05 }},
		{"defaults, neutral demonstrations weighted 0", func(p *Params) { p.DemoWeight = 0 }},
		{"defaults, late-round terminal credit", func(p *Params) { p.TerminalWeight = 0.3 }},
	}
	lv := Levels{Execution: 8, Knowledge: 8}
	for _, v := range variants {
		var before, after []roundResult
		for trial := 0; trial < 4; trial++ {
			p := DefaultParams()
			v.mod(p)
			eng := New(p, t.TempDir(), int64(7+trial))
			recordVs(t, eng, 20, int64(40+trial*100), of)
			evalSeed := 7000 + trial*100
			before = append(before, playDrivenVs(eng, 10, evalSeed, lv, of)...)
			playDrivenVs(eng, 30, 9000+trial*1000, lv, of)
			after = append(after, playDrivenVs(eng, 10, evalSeed, lv, of)...)
		}
		bn, bse, bw := summarize(before)
		an, ase, aw := summarize(after)
		t.Logf("%-9s %-38s net %4.0f±%2.0f -> %4.0f±%2.0f   win%% %3.0f -> %3.0f", name, v.name, bn, bse, an, ase, 100*bw, 100*aw)
	}
}
