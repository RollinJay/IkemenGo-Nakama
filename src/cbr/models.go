package cbr

import "math"

// The models answer questions about the character rather than about one
// recorded decision: what a move reaches, which cancels exist, how hard a
// route's links are, and how far humans have taken a repeated move. They
// are built from admitted cases and merge across players as counts.

const (
	reachBinPx   = 8
	reachBinsMax = 64 // 512px of forward distance
)

// ReachBin counts results of one move at one distance band.
type ReachBin struct {
	Hits, Whiffs, Blocks int32
}

func (b ReachBin) total() int32 { return b.Hits + b.Whiffs + b.Blocks }

// ReachModel is the empirical hit envelope per move and enemy state class.
type ReachModel struct {
	// key: moveRef<<3 | state class
	Bins map[int64][]ReachBin
	// Geometric upper bound per key, in pixels; 0 when unset. Observation
	// may narrow the envelope inside it but never widen it.
	GeoMax map[int64]float32
}

func newReachModel() *ReachModel {
	return &ReachModel{Bins: map[int64][]ReachBin{}, GeoMax: map[int64]float32{}}
}

func reachKey(move int32, st StateType) int64 { return int64(move)<<3 | int64(st&7) }

func reachBin(dx float32) int {
	if dx < 0 {
		dx = 0
	}
	b := int(dx / reachBinPx)
	if b >= reachBinsMax {
		b = reachBinsMax - 1
	}
	return b
}

// Add records one contact result.
func (m *ReachModel) Add(move int32, st StateType, dx float32, contact int8) {
	k := reachKey(move, st)
	bins := m.Bins[k]
	if bins == nil {
		bins = make([]ReachBin, reachBinsMax)
		m.Bins[k] = bins
	}
	b := &bins[reachBin(dx)]
	switch contact {
	case 1:
		b.Hits++
	case 2:
		b.Blocks++
	case 0:
		b.Whiffs++
	}
}

// Connect estimates the probability that move connects (hit or guarded) at
// forward distance dx against an enemy in state st. known is false when the
// model holds no data for the move and class, in which case callers must not
// filter on it. Bins with fewer than minSamples observations are unknown;
// an unknown bin beyond the farthest observed connection resolves inward,
// to "does not reach", so low knowledge plays short rather than whiffing
// from out of range.
func (m *ReachModel) Connect(move int32, st StateType, dx float32, minSamples int32) (prob float32, known bool) {
	k := reachKey(move, st)
	bins := m.Bins[k]
	if bins == nil {
		return 0, false
	}
	if g := m.GeoMax[k]; g > 0 && dx > g {
		return 0, true
	}
	farthest := -1
	for i := range bins {
		if bins[i].Hits+bins[i].Blocks > 0 {
			farthest = i
		}
	}
	if farthest < 0 {
		// Only whiffs observed.
		return 0, true
	}
	bi := reachBin(dx)
	if bi > farthest {
		return 0, true
	}
	for i := bi; i >= 0; i-- {
		b := bins[i]
		if b.total() >= minSamples && b.total() > 0 {
			return float32(b.Hits+b.Blocks) / float32(b.total()), true
		}
	}
	// Nothing well attested at or inside this distance: the move connected
	// somewhere farther out, so it reaches here.
	return 1, true
}

// SetGeometricMax sets the upper bound derived from collision geometry.
func (m *ReachModel) SetGeometricMax(move int32, st StateType, px float32) {
	m.GeoMax[reachKey(move, st)] = px
}

// RouteStats aggregates link timing across repeats of one route.
type RouteStats struct {
	N     int32
	Links []LinkAcc
}

// LinkAcc accumulates one link's timing with Welford's method.
type LinkAcc struct {
	N         int32
	Mean, M2  float64
	Attempts  int32
	Successes int32
}

func (r *RouteStats) add(links []LinkStat) {
	if len(links) > len(r.Links) {
		r.Links = append(r.Links, make([]LinkAcc, len(links)-len(r.Links))...)
	}
	r.N++
	for i, l := range links {
		a := &r.Links[i]
		a.Attempts += l.Attempts
		a.Successes += l.Successes
		if l.Successes > 0 {
			a.N++
			x := float64(l.CanonicalFrame)
			d := x - a.Mean
			a.Mean += d / float64(a.N)
			a.M2 += d * (x - a.Mean)
		}
	}
}

// LinkStats returns the aggregated per-link statistics.
func (r *RouteStats) LinkStats() []LinkStat {
	out := make([]LinkStat, len(r.Links))
	for i, a := range r.Links {
		v := float32(0)
		if a.N > 1 {
			v = float32(math.Sqrt(a.M2 / float64(a.N-1)))
		}
		out[i] = LinkStat{CanonicalFrame: int32(a.Mean + 0.5), Variance: v, Attempts: a.Attempts, Successes: a.Successes}
	}
	return out
}

// TransitionModel is the observed cancel graph: which attack was cancelled
// into which, and at what frame of the first.
type TransitionModel struct {
	Edges map[int64]*Edge // from<<32 | uint32(to)
	From  map[int32]int32 // total cancels observed out of a move
}

// Edge counts one observed cancel.
type Edge struct {
	Count    int32
	MinFrame int32
	MaxFrame int32
}

func newTransitionModel() *TransitionModel {
	return &TransitionModel{Edges: map[int64]*Edge{}, From: map[int32]int32{}}
}

func edgeKey(from, to int32) int64 { return int64(from)<<32 | int64(uint32(to)) }

// Add records a cancel from one attack into another at a frame of the first.
func (t *TransitionModel) Add(from, to, frame int32) {
	k := edgeKey(from, to)
	e := t.Edges[k]
	if e == nil {
		e = &Edge{MinFrame: frame, MaxFrame: frame}
		t.Edges[k] = e
	}
	e.Count++
	if frame < e.MinFrame {
		e.MinFrame = frame
	}
	if frame > e.MaxFrame {
		e.MaxFrame = frame
	}
	t.From[from]++
}

// Allows reports whether a cancel from -> to is consistent with what has
// been observed. A move with no observed cancels at all does not restrict;
// otherwise the edge must exist with at least minCount observations.
func (t *TransitionModel) Allows(from, to int32, minCount int32) bool {
	if t.From[from] == 0 {
		return true
	}
	e := t.Edges[edgeKey(from, to)]
	return e != nil && e.Count >= minCount
}
