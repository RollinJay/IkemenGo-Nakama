package cbr

import (
	"math/rand"
	"testing"
)

// BenchmarkChoose measures one selection over a store grown to n cases by
// replicating recorded cases with jittered positions.
func benchChoose(b *testing.B, n int) {
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
		ps.drv.choose(&ps.lastFeat, &ps.lastSelf, int64(i))
	}
}

func BenchmarkChoose1k(b *testing.B)  { benchChoose(b, 1000) }
func BenchmarkChoose5k(b *testing.B)  { benchChoose(b, 5000) }
func BenchmarkChoose20k(b *testing.B) { benchChoose(b, 20000) }
