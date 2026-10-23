package cbr

import "sort"

// Store is one character's learned data: cases plus the models built from
// them. One store serves every difficulty setting; gating happens at
// selection, so raising difficulty exposes what was already learned.
type Store struct {
	Char        string
	Cases       []*Case
	Reach       *ReachModel
	Trans       *TransitionModel
	Routes      map[uint64]*RouteStats
	Precedent   map[int32]int32 // move -> most repetitions in one human chain
	HumanRoutes map[uint64]bool
	// Actions pools outcomes per action in a situation (see actionKey), so
	// value is learned across every instance of an action rather than from
	// each recorded instance's own few trials.
	Actions map[uint64]*ActionStat
	// CmdStates counts, per state entered, the commands active at entry
	// (see command.go). It resolves assisted commands and hypotheses.
	CmdStates map[int32]map[string]float32
	// ChangeStateAI is set once this character's own AI has been seen
	// entering attack states with no command active, in recorded play or
	// during a move the CBR layer played. It is a record for inspection: the
	// AI's recorded cases are discarded for the match it was seen in (the
	// states it entered are kept as hypotheses), and how the character is
	// driven does not change.
	ChangeStateAI bool
	Hypotheses    map[int32]*Hypothesis
	// Baseline is the base AI's record by situation (see baseline.go): what
	// the character's own AI achieves where the CBR layer leaves it in
	// control, for comparison with the CBR layer's own plays.
	Baseline map[uint64]*BaselineStat
	// Played is the other side of that comparison: what the CBR layer's own
	// plays of each move achieved, by the move and the stance it is played
	// from (see playKey).
	Played map[uint64]*PlayStat
	// BaseFormat is the layout Baseline and Played are kept in (see
	// baseFormat). Records kept in an older layout are dropped on load.
	BaseFormat int

	global       *globalModels
	dirtyRemoved bool
	touched      bool // used since the last save
	byID         map[uint64]*Case
	byState      [5][]*Case
	usage        map[uint64]*usage
	total        float32
	evThresh     [9]float32
	evDirty      bool
	maxID        uint64
}

type usage struct {
	last  int64
	times int32
}

// NewStore returns an empty store for a character.
func NewStore(char string) *Store {
	s := &Store{
		Char:        char,
		Reach:       newReachModel(),
		Trans:       newTransitionModel(),
		Routes:      map[uint64]*RouteStats{},
		Precedent:   map[int32]int32{},
		HumanRoutes: map[uint64]bool{},
		Actions:     map[uint64]*ActionStat{},
		CmdStates:   map[int32]map[string]float32{},
		Hypotheses:  map[int32]*Hypothesis{},
		Baseline:    map[uint64]*BaselineStat{},
		Played:      map[uint64]*PlayStat{},
		BaseFormat:  baseFormat,
	}
	s.index()
	return s
}

// compact drops cases marked removed since the last index.
func (s *Store) compact() {
	live := s.Cases[:0]
	for _, c := range s.Cases {
		if !c.Removed {
			live = append(live, c)
		}
	}
	for i := len(live); i < len(s.Cases); i++ {
		s.Cases[i] = nil
	}
	s.Cases = live
	s.dirtyRemoved = false
	s.index()
}

// ActionStat is the pooled outcome record of one action in one situation.
// PTrials and PSum are the results of the CBR layer's own plays of the
// action from any stance, credited by Params.applyPlayed. The comparison
// with the base AI reads the plays kept by stance instead (see PlayStat), and
// falls back to these only for a move with none of those.
type ActionStat struct {
	Trials   float32
	ValueSum float32
	ByOpp    map[string]OppStat
	PTrials  float32
	PSum     float32
}

// PlayStat is what the CBR layer's own plays of a move achieved, alone,
// without demonstrations: how many plays, the sum of their results, both
// decayed like the rest and never shrunk toward a prior, and how many plays
// the character's own AI took over from (see driver.contest), which count
// for nothing else. It is what a move has proved against the base AI (see
// baseline.go).
type PlayStat struct {
	Trials   float32
	Sum      float32
	Contests float32
}

// playKey identifies the record of the CBR layer's plays of a case's move:
// its action (see actionKey) and the stance it is played from, which is the
// one it was recorded in (see gateFilter): the character's own state type,
// and whether it is being hit or guarding (see selfStun). The action alone
// does not say it: a step back on the ground and a drift back in the air are
// the same movement, and the same button can start the same move standing
// or from a crouch. The stance takes bits 48 to 52, above the move.
func playKey(c *Case) uint64 {
	return actionKey(c) | uint64(c.StartStateType&7)<<48 | selfStun(&c.Start)<<51
}

// played returns the record of the CBR layer's plays of a case's move,
// creating it.
func (s *Store) played(c *Case) *PlayStat {
	k := playKey(c)
	r := s.Played[k]
	if r == nil {
		r = &PlayStat{}
		s.Played[k] = r
	}
	return r
}

// playRecord returns what the CBR layer's plays of a case's move achieved,
// for the comparison with the base AI. A move with no plays kept by stance
// takes the plays credited to its action as a whole (ActionStat.PTrials and
// PSum), if any.
func (s *Store) playRecord(c *Case) PlayStat {
	var r PlayStat
	if pr := s.Played[playKey(c)]; pr != nil {
		r = *pr
	}
	if r.Trials == 0 {
		if a := s.Actions[actionKey(c)]; a != nil {
			r.Trials, r.Sum = a.PTrials, a.PSum
		}
	}
	return r
}

// addPlay records one result r of the CBR layer's own play of a case.
func (s *Store) addPlay(p *Params, c *Case, r float32) {
	pr := s.played(c)
	pr.Trials = pr.Trials*p.Decay + 1
	pr.Sum = pr.Sum*p.Decay + r
}

// creditPlay adds credit to the record of a case's plays without counting a
// new play, for the round's terminal reward.
func (s *Store) creditPlay(c *Case, r float32) {
	s.played(c).Sum += r
}

// contestPlay notes that the character's own AI took over from a play of a
// case, decayed like a play.
func (s *Store) contestPlay(p *Params, c *Case) {
	pr := s.played(c)
	pr.Contests = pr.Contests*p.Decay + 1
}

// PlayString describes a played-record key (see playKey), for inspection.
func PlayString(k uint64) string {
	return ActionString(k&(1<<48-1)) + ", " + stanceString(k>>48&7, k>>51&3)
}

const actionBandPx = 40

// distanceBand is actionKey's coarse distance to the enemy: 40-pixel bands
// of its position ahead (band 4 within 40 pixels either way, higher bands
// ahead, lower behind), 4 with no enemy.
func distanceBand(f *Feat) uint64 {
	band := int64(4)
	if f.Enemy.Present {
		band = int64(f.Enemy.RelX/actionBandPx) + 4
	}
	if band < 0 {
		band = 0
	}
	if band > 15 {
		band = 15
	}
	return uint64(band)
}

// actionKey identifies what a case does and in what kind of situation: the
// move it executes (or, for movement, its direction class), the enemy's
// state type, a coarse distance band, the exchange context and whether the
// case starts with control or cancels out of an action. Value is pooled at
// this level. The context matters because the same move is worth very
// different amounts as a whiff punish, a combo part, pressure on a guarding
// enemy or a neutral poke; pooled without it, a move a demonstrator used
// mostly as a punish looks good in neutral.
func actionKey(c *Case) uint64 {
	move := int64(c.MoveRef)
	if c.Kind == KindMovement {
		move = -1 - int64(movementClass(c))
	}
	k := uint64(uint32(int32(move)))<<16 |
		uint64(c.Start.Enemy.State&7)<<4 | distanceBand(&c.Start) |
		uint64(exchangeContext(&c.Start))<<8
	if c.StartCtrl {
		k |= 1 << 11
	}
	if c.Kind == KindMovement {
		k |= 1 << 12
	}
	return k
}

// exchangeContext classifies the enemy's situation: 0 free, 1 attacking,
// 2 in hitstun, 3 guarding an attack.
func exchangeContext(f *Feat) uint8 {
	e := &f.Enemy
	switch {
	case !e.Present:
		return 0
	case e.beingHit():
		return 2
	case e.BlockStun > 0:
		return 3
	case e.Move == MtAttack:
		return 1
	}
	return 0
}

// movementClass is a movement case's dominant direction: 0 neutral,
// 1 forward, 2 back, 3 up, 4 down.
func movementClass(c *Case) int {
	var n [5]int
	for _, b := range c.Inputs {
		switch d := b.Dir(); {
		case d >= 7:
			n[3]++
		case d <= 3:
			n[4]++
		case d == 6:
			n[1]++
		case d == 4:
			n[2]++
		default:
			n[0]++
		}
	}
	best := 0
	for i := range n {
		if n[i] > n[best] {
			best = i
		}
	}
	return best
}

func (s *Store) action(c *Case) *ActionStat {
	k := actionKey(c)
	a := s.Actions[k]
	if a == nil {
		a = &ActionStat{}
		s.Actions[k] = a
	}
	return a
}

func (s *Store) index() {
	s.byID = make(map[uint64]*Case, len(s.Cases))
	for i := range s.byState {
		s.byState[i] = s.byState[i][:0]
	}
	if s.usage == nil {
		s.usage = map[uint64]*usage{}
	}
	s.total = 0
	for _, c := range s.Cases {
		if c.Removed {
			continue
		}
		s.byID[c.ID] = c
		st := c.StartStateType
		if int(st) >= len(s.byState) {
			st = StUnknown
		}
		s.byState[st] = append(s.byState[st], c)
		s.total += c.Outcome.Trials
		if c.ID > s.maxID {
			s.maxID = c.ID
		}
	}
	s.evDirty = true
}

// Len returns the number of live cases.
func (s *Store) Len() int { return len(s.byID) }

// Get returns a live case by ID.
func (s *Store) Get(id uint64) *Case { return s.byID[id] }

// Admit adds newly recorded cases and folds them into the models.
func (s *Store) Admit(p *Params, cases []*Case) {
	seenChain := map[uint64]bool{}
	for _, c := range cases {
		if c.Removed || len(c.Inputs) == 0 {
			continue
		}
		s.foldModels(c, seenChain)
	}
	// Novelty is labelled after the models include this batch's human data.
	for _, c := range cases {
		if c.Removed || len(c.Inputs) == 0 {
			continue
		}
		s.labelNovelty(c)
		if c.Outcome.Trials > 0 {
			a := s.action(c)
			w := p.DemoWeight
			if exchangeContext(&c.Start) == 2 {
				w = p.DemoComboWeight
			}
			a.Trials += c.Outcome.Trials * w
			a.ValueSum += c.Outcome.ValueSum * w
		}
		s.Cases = append(s.Cases, c)
	}
	s.index()
}

func (s *Store) foldModels(c *Case, seenChain map[uint64]bool) {
	if c.ExecAttack && c.Reach.Valid {
		s.Reach.Add(c.MoveRef, c.Reach.EnemyState, c.Reach.DX, c.Reach.Contact)
	}
	if c.ExecAttack && c.CancelFrom != 0 {
		s.Trans.Add(c.CancelFrom, c.MoveRef, c.CancelTime)
	}
	if c.ChainID != 0 && !seenChain[c.ChainID] {
		seenChain[c.ChainID] = true
		if c.Route.Key != 0 && len(c.Route.Links) > 0 {
			rs := s.Routes[c.Route.Key]
			if rs == nil {
				rs = &RouteStats{}
				s.Routes[c.Route.Key] = rs
			}
			rs.add(c.Route.Links)
		}
		if c.Source.IsHuman() {
			if c.Route.Key != 0 {
				s.HumanRoutes[c.Route.Key] = true
			}
			t := &c.Technique
			if t.RepeatedMove != 0 && t.ChainRepetitions > s.Precedent[t.RepeatedMove] {
				s.Precedent[t.RepeatedMove] = t.ChainRepetitions
			}
		}
	}
}

// labelNovelty marks chains that go beyond human precedent. The label is
// for the developer-facing discovery report; it never restricts behaviour.
func (s *Store) labelNovelty(c *Case) {
	t := &c.Technique
	t.HumanPrecedent = s.Precedent[t.RepeatedMove]
	if t.RepeatedMove != 0 && t.ChainRepetitions > t.HumanPrecedent {
		t.Novel = true
		t.NoveltyMargin = t.ChainRepetitions - t.HumanPrecedent
	}
	if !c.Source.IsHuman() && c.Route.Key != 0 && !s.HumanRoutes[c.Route.Key] {
		t.Novel = true
	}
}

// candidates returns live cases whose start state type is compatible.
func (s *Store) candidates(st StateType) []*Case {
	if int(st) >= len(s.byState) {
		return nil
	}
	return s.byState[st]
}

// resetUsage clears the reuse record at a round boundary. The reuse cost
// discourages repetition within a round; carried across rounds it would
// steadily push selection toward worse-matching cases.
func (s *Store) resetUsage() {
	for k := range s.usage {
		delete(s.usage, k)
	}
}

func (s *Store) markUsed(id uint64, tick int64) {
	u := s.usage[id]
	if u == nil {
		u = &usage{}
		s.usage[id] = u
	}
	u.last = tick
	u.times++
}

// reuseCost is the recovered caseReuse penalty: recent use within the
// repetition window costs the full weight, frequent use adds half again.
func (s *Store) reuseCost(p *Params, id uint64, tick int64) float32 {
	u := s.usage[id]
	if u == nil {
		return 0
	}
	var c float32
	if tick-u.last < int64(p.RepetitionFrames) {
		c += 1
	}
	c += 0.5 * clampf(float32(u.times)/10, 0, 1)
	return p.CaseReuseWeight * c
}

// evidenceThreshold returns the minimum evidence visible at a knowledge
// level: the top fraction of cases by evidence, where the fraction grows
// from KnowledgeFloor at level 1 to everything at level 8.
func (s *Store) evidenceThreshold(p *Params, level int) float32 {
	if level >= 8 {
		return -1
	}
	if level < 1 {
		level = 1
	}
	if s.evDirty {
		ev := make([]float32, 0, len(s.byID))
		for _, c := range s.byID {
			ev = append(ev, c.Evidence(p))
		}
		sort.Slice(ev, func(i, j int) bool { return ev[i] < ev[j] })
		for k := 1; k <= 8; k++ {
			f := p.knowledgeFrac(k)
			if len(ev) == 0 || k == 8 {
				s.evThresh[k] = -1
				continue
			}
			idx := int(float32(len(ev)) * (1 - f))
			if idx >= len(ev) {
				idx = len(ev) - 1
			}
			if idx < 0 {
				idx = 0
			}
			s.evThresh[k] = ev[idx]
		}
		s.evDirty = false
	}
	return s.evThresh[level]
}

// Prune removes non-human cases that have been tried enough to know they
// are poor, then keeps the store within Params.MaxCases. Selection cost is
// linear in the number of cases (about half a microsecond per case per
// selection on a 2.8 GHz server core), so the budget bounds the per-frame
// cost of driving. Over budget, non-human cases go first, lowest value
// first, until they are at most MaxNonHumanShare of the budget; then the
// oldest human cases, sparing trial cases. Below the budget nothing is
// evicted for being non-human: a character with only CPU data keeps it.
func (s *Store) Prune(p *Params) int {
	removed := 0
	type rule struct {
		minTrials float32
		floor     float32
	}
	rules := map[Source]rule{
		SrcAuthoredAI:  {8, 0},
		SrcEngineAI:    {20, 0.2},
		SrcSynthesized: {8, 0},
	}
	var nonHuman, human []*Case
	for _, c := range s.Cases {
		if c.Removed {
			continue
		}
		if !c.Source.IsHuman() {
			if r, ok := rules[c.Source]; ok && c.Outcome.Trials >= r.minTrials && c.Value(p) < r.floor {
				c.Removed = true
				removed++
				continue
			}
			nonHuman = append(nonHuman, c)
			continue
		}
		human = append(human, c)
	}
	live := len(nonHuman) + len(human)
	if p.MaxCases > 0 && live > p.MaxCases {
		over := live - p.MaxCases
		if limit := int(p.MaxNonHumanShare * float32(p.MaxCases)); len(nonHuman) > limit {
			n := len(nonHuman) - limit
			if n > over {
				n = over
			}
			sort.Slice(nonHuman, func(i, j int) bool { return nonHuman[i].Value(p) < nonHuman[j].Value(p) })
			for _, c := range nonHuman[:n] {
				c.Removed = true
			}
			removed += n
			over -= n
		}
		if over > 0 {
			sort.Slice(human, func(i, j int) bool { return human[i].ID < human[j].ID })
			for _, c := range human {
				if over == 0 {
					break
				}
				if c.Source == SrcTrial {
					continue
				}
				c.Removed = true
				removed++
				over--
			}
		}
	}
	if removed > 0 || s.dirtyRemoved {
		s.compact()
	}
	// Route statistics are kept only for routes some live case belongs to.
	used := map[uint64]bool{}
	for _, c := range s.Cases {
		if c.Route.Key != 0 {
			used[c.Route.Key] = true
		}
	}
	for k := range s.Routes {
		if !used[k] {
			delete(s.Routes, k)
		}
	}
	for k := range s.HumanRoutes {
		if !used[k] {
			delete(s.HumanRoutes, k)
		}
	}
	return removed
}
