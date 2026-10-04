package cbr

import (
	"fmt"
	"strconv"
	"strings"
)

// Aggregates are the distributable form of what a character's store has
// learned: statistics, not cases. They hold no player identity and merge by
// summing counts, so a server can combine many installations' contributions
// without replaying anything. JSON keeps them readable from Nakama's Lua
// runtime.

// AggregateVersion is the current aggregate format version.
const AggregateVersion = 1

// Aggregate is one character's distributable statistics.
type Aggregate struct {
	Version   int                   `json:"v"`
	Char      string                `json:"char"`
	Reach     map[string][]ReachBin `json:"reach,omitempty"`     // "move:class"
	GeoMax    map[string]float32    `json:"geo_max,omitempty"`   // "move:class"
	Edges     map[string]Edge       `json:"edges,omitempty"`     // "from:to"
	Routes    map[string][]LinkStat `json:"routes,omitempty"`    // route key, hex
	Precedent map[string]int32      `json:"precedent,omitempty"` // move
	Weight    float32               `json:"weight"`              // observations represented
}

func keyMoveClass(k int64) string {
	return strconv.FormatInt(k>>3, 10) + ":" + strconv.FormatInt(k&7, 10)
}

func parseMoveClass(s string) (int64, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("bad key %q", s)
	}
	m, err := strconv.ParseInt(a, 10, 32)
	if err != nil {
		return 0, err
	}
	c, err := strconv.ParseInt(b, 10, 8)
	if err != nil || c < 0 || c > 7 {
		return 0, fmt.Errorf("bad class in %q", s)
	}
	return m<<3 | c, nil
}

func parseEdge(s string) (int64, error) {
	a, b, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("bad edge %q", s)
	}
	f, err := strconv.ParseInt(a, 10, 32)
	if err != nil {
		return 0, err
	}
	t, err := strconv.ParseInt(b, 10, 32)
	if err != nil {
		return 0, err
	}
	return edgeKey(int32(f), int32(t)), nil
}

// ExportAggregate builds this installation's contribution from its local
// models only, never from a previously received global aggregate, which
// would count everyone twice. When the local data represents more than
// maxWeight observations, counts are scaled down so one prolific player
// cannot dominate the merged model.
func (s *Store) ExportAggregate(maxWeight float32) *Aggregate {
	a := &Aggregate{
		Version:   AggregateVersion,
		Char:      s.Char,
		Reach:     map[string][]ReachBin{},
		GeoMax:    map[string]float32{},
		Edges:     map[string]Edge{},
		Routes:    map[string][]LinkStat{},
		Precedent: map[string]int32{},
	}
	var total float32
	for _, bins := range s.Reach.Bins {
		for _, b := range bins {
			total += float32(b.total())
		}
	}
	for _, e := range s.Trans.Edges {
		total += float32(e.Count)
	}
	scale := float32(1)
	if maxWeight > 0 && total > maxWeight {
		scale = maxWeight / total
	}
	sc := func(n int32) int32 { return int32(float32(n)*scale + 0.5) }
	for k, bins := range s.Reach.Bins {
		out := make([]ReachBin, len(bins))
		for i, b := range bins {
			out[i] = ReachBin{Hits: sc(b.Hits), Whiffs: sc(b.Whiffs), Blocks: sc(b.Blocks)}
		}
		a.Reach[keyMoveClass(k)] = out
	}
	for k, g := range s.Reach.GeoMax {
		a.GeoMax[keyMoveClass(k)] = g
	}
	for k, e := range s.Trans.Edges {
		from, to := int32(k>>32), int32(uint32(k))
		a.Edges[strconv.Itoa(int(from))+":"+strconv.Itoa(int(to))] = Edge{Count: sc(e.Count), MinFrame: e.MinFrame, MaxFrame: e.MaxFrame}
	}
	for k, rs := range s.Routes {
		a.Routes[strconv.FormatUint(k, 16)] = rs.LinkStats()
	}
	for m, n := range s.Precedent {
		a.Precedent[strconv.Itoa(int(m))] = n
	}
	a.Weight = total * scale
	return a
}

// MergeAggregate adds src into dst. Counts sum; frame windows widen;
// precedent takes the maximum. Route link statistics combine as weighted
// means and pooled spreads.
func MergeAggregate(dst, src *Aggregate) {
	if dst.Reach == nil {
		dst.Reach = map[string][]ReachBin{}
	}
	if dst.GeoMax == nil {
		dst.GeoMax = map[string]float32{}
	}
	if dst.Edges == nil {
		dst.Edges = map[string]Edge{}
	}
	if dst.Routes == nil {
		dst.Routes = map[string][]LinkStat{}
	}
	if dst.Precedent == nil {
		dst.Precedent = map[string]int32{}
	}
	for k, bins := range src.Reach {
		d := dst.Reach[k]
		if len(d) < len(bins) {
			d = append(d, make([]ReachBin, len(bins)-len(d))...)
		}
		for i, b := range bins {
			d[i].Hits += b.Hits
			d[i].Whiffs += b.Whiffs
			d[i].Blocks += b.Blocks
		}
		dst.Reach[k] = d
	}
	for k, g := range src.GeoMax {
		if cur, ok := dst.GeoMax[k]; !ok || g < cur {
			dst.GeoMax[k] = g
		}
	}
	for k, e := range src.Edges {
		d, ok := dst.Edges[k]
		if !ok {
			dst.Edges[k] = e
			continue
		}
		d.Count += e.Count
		if e.MinFrame < d.MinFrame {
			d.MinFrame = e.MinFrame
		}
		if e.MaxFrame > d.MaxFrame {
			d.MaxFrame = e.MaxFrame
		}
		dst.Edges[k] = d
	}
	for k, links := range src.Routes {
		dst.Routes[k] = mergeLinks(dst.Routes[k], links)
	}
	for k, n := range src.Precedent {
		if n > dst.Precedent[k] {
			dst.Precedent[k] = n
		}
	}
	dst.Weight += src.Weight
	if dst.Version == 0 {
		dst.Version = AggregateVersion
	}
	if dst.Char == "" {
		dst.Char = src.Char
	}
}

func mergeLinks(a, b []LinkStat) []LinkStat {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	out := make([]LinkStat, n)
	for i := 0; i < n; i++ {
		var x, y LinkStat
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		wx, wy := float32(x.Successes), float32(y.Successes)
		out[i].Attempts = x.Attempts + y.Attempts
		out[i].Successes = x.Successes + y.Successes
		if wx+wy > 0 {
			mx, my := float32(x.CanonicalFrame), float32(y.CanonicalFrame)
			mean := (wx*mx + wy*my) / (wx + wy)
			out[i].CanonicalFrame = int32(mean + 0.5)
			// Pooled spread including the shift between means.
			v := (wx*(x.Variance*x.Variance+(mx-mean)*(mx-mean)) + wy*(y.Variance*y.Variance+(my-mean)*(my-mean))) / (wx + wy)
			out[i].Variance = sqrt32(v)
		}
	}
	return out
}

// globalModels is a received aggregate decoded for lookups. Local data
// outweighs it: global counts enter lookups at globalWeight.
type globalModels struct {
	reach     map[int64][]ReachBin
	geo       map[int64]float32
	edges     map[int64]Edge
	from      map[int32]int32
	routes    map[uint64][]LinkStat
	precedent map[int32]int32
}

const globalWeight = 0.5

// ApplyGlobal installs a received aggregate as this store's global layer.
// It is never written back into the local data.
func (s *Store) ApplyGlobal(a *Aggregate) error {
	if a == nil {
		s.global = nil
		return nil
	}
	if a.Version > AggregateVersion {
		return fmt.Errorf("cbr: aggregate version %d is newer than supported %d", a.Version, AggregateVersion)
	}
	g := &globalModels{
		reach: map[int64][]ReachBin{}, geo: map[int64]float32{}, edges: map[int64]Edge{},
		from: map[int32]int32{}, routes: map[uint64][]LinkStat{}, precedent: map[int32]int32{},
	}
	for k, bins := range a.Reach {
		key, err := parseMoveClass(k)
		if err != nil {
			return err
		}
		if len(bins) > reachBinsMax {
			bins = bins[:reachBinsMax]
		}
		g.reach[key] = bins
	}
	for k, v := range a.GeoMax {
		key, err := parseMoveClass(k)
		if err != nil {
			return err
		}
		g.geo[key] = v
	}
	for k, e := range a.Edges {
		key, err := parseEdge(k)
		if err != nil {
			return err
		}
		g.edges[key] = e
		g.from[int32(key>>32)] += e.Count
	}
	for k, l := range a.Routes {
		key, err := strconv.ParseUint(k, 16, 64)
		if err != nil {
			return err
		}
		g.routes[key] = l
	}
	for k, n := range a.Precedent {
		m, err := strconv.ParseInt(k, 10, 32)
		if err != nil {
			return err
		}
		g.precedent[int32(m)] = n
	}
	s.global = g
	return nil
}

// connect queries reach across the local and global layers.
func (s *Store) connect(move int32, st StateType, dx float32, minSamples int32) (float32, bool) {
	k := reachKey(move, st)
	local := s.Reach.Bins[k]
	var gb []ReachBin
	var geo float32 = s.Reach.GeoMax[k]
	if s.global != nil {
		gb = s.global.reach[k]
		if g := s.global.geo[k]; g > 0 && (geo == 0 || g < geo) {
			geo = g
		}
	}
	if local == nil && gb == nil {
		return 0, false
	}
	if geo > 0 && dx > geo {
		return 0, true
	}
	n := len(local)
	if len(gb) > n {
		n = len(gb)
	}
	conn := make([]float32, n)
	tot := make([]float32, n)
	for i := 0; i < n; i++ {
		if i < len(local) {
			conn[i] += float32(local[i].Hits + local[i].Blocks)
			tot[i] += float32(local[i].total())
		}
		if i < len(gb) {
			conn[i] += globalWeight * float32(gb[i].Hits+gb[i].Blocks)
			tot[i] += globalWeight * float32(gb[i].total())
		}
	}
	farthest := -1
	for i := range conn {
		if conn[i] > 0 {
			farthest = i
		}
	}
	if farthest < 0 {
		return 0, true
	}
	bi := reachBin(dx)
	if bi > farthest {
		return 0, true
	}
	for i := bi; i >= 0; i-- {
		if tot[i] >= float32(minSamples) && tot[i] > 0 {
			return conn[i] / tot[i], true
		}
	}
	return 1, true
}

// allowsCancel queries the cancel graph across both layers.
func (s *Store) allowsCancel(from, to int32, minCount int32) bool {
	k := edgeKey(from, to)
	fromTotal := float32(s.Trans.From[from])
	count := float32(0)
	if e := s.Trans.Edges[k]; e != nil {
		count = float32(e.Count)
	}
	if s.global != nil {
		fromTotal += float32(s.global.from[from])
		if e, ok := s.global.edges[k]; ok {
			count += globalWeight * float32(e.Count)
		}
	}
	if fromTotal == 0 {
		return true
	}
	return count >= float32(minCount)
}

func sqrt32(v float32) float32 {
	if v <= 0 {
		return 0
	}
	x := v
	for i := 0; i < 20; i++ {
		x = 0.5 * (x + v/x)
	}
	return x
}
