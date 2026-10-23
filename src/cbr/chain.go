package cbr

// A chain is a sequence of cases forming one exchange, prefixed by the
// neutral movement cases that led into it. Chains are graded when they
// resolve, and the grade is credited backward across their cases.
//
// A chain can also hold probes: markers of the base AI's decisions (see
// baseline.go), credited like the CBR layer's played cases into the base
// AI's record instead of a case. Probes are not cases: they do not count
// toward the horizon, do not change the backward credit of recorded and
// played cases (see split), and take no recorded case's place in the
// lead-in.
//
// The lead-in. Before an exchange, the lead-in keeps the last leadInMax
// neutral decisions that might have led into it; an older one leaves it and
// resolves as neutral (see chainTracker.onEvict). With the base AI on, the
// two sides of the comparison with the base AI leave it by one rule: the
// base AI's decisions (probes) and the moves the CBR layer played count
// alike, so that a decision leaves the lead-in once leadInMax decisions of
// either layer have followed it, whichever layer made it. A step the CBR
// layer played long before an exchange, with many of the base AI's
// decisions after it, is then as neutral as a step of the base AI's own in
// its place would be. Without that, the CBR layer's moves would wait for the
// next exchange while the base AI's decisions in between rotated out, and
// the base AI's own moves, played by the CBR layer, would measure better
// than the base AI. Recorded cases, the recorded player's demonstrations,
// keep a count of their own, as they would with no CBR layer playing. With
// the base AI off there are no probes, and recorded and played cases count
// together.

const (
	leadInMax = 4  // neutral decisions kept as a chain prefix (see addCase)
	otgWindow = 20 // frames a knockdown stays open for an OTG follow-up
)

type chainCase struct {
	rec         *Case  // recorded case, or nil
	caseID      uint64 // played case, or 0
	startTick   int64
	stateNo     int32
	isAttack    bool
	attackClass StateType
	perturbed   bool // injected execution noise affected this case
	level       int8 // execution level when played
	// contested: the character's own AI took over during the play (see
	// Engine.watchDriven), so its result is not the case's.
	contested bool
	// probe marks a decision of the base AI in situation key (see
	// baselineKey) rather than a case.
	probe bool
	key   uint64
}

type chain struct {
	id        uint64
	session   uint64
	startTick int64
	cases     []chainCase
	probes    int32 // how many of cases are probes

	selfLifeMax  int32
	powerMax     int32
	prevSelfLife int32
	power0       int32
	lastPower    int32

	dealt, taken  float32
	lastDealtTick int64
	lastTakenTick int64
	frames        int32
	timerTicks    int32

	moveCount       map[int32]int32
	inAttack        bool
	lastAttackRef   int32
	lastAttackSpent bool
	lastAttackClass StateType
	attackPower     int32

	sawDown       bool
	downTick      int64
	otg, relaunch bool
	enemyKO       bool // an opposing character was knocked out during the chain
	enemyCtrlHit  bool // enemy had control at some point after the first hit
	firstHitTick  int64
	punished      bool
	timedOut      bool
	resetFollowup bool

	pressTicks []int64
	timeFrac0  float32
}

type resolvedChain struct {
	ch        *chain
	comp      Completion
	tech      Technique
	route     Route
	endTick   int64
	resetOK   bool
	waitUntil int64
}

type chainTracker struct {
	p      *Params
	player int
	ids    *uint64

	active  *chain
	leadIn  []chainCase
	pending []*resolvedChain
	round   []*resolvedChain

	prevInExchange bool
	prevEnemyState StateType
	prevInput      InputBits
	session        uint64
	sessionSeen    bool
	// oppLife is each opposing root's life on the previous frame, by
	// character ID. Damage is measured on every opponent, not just the
	// nearest one, so a change of nearest enemy or a knockout (which removes
	// the enemy from Enemy) does not lose or invent damage.
	oppLife map[int32]int32

	onFinal func(rc *resolvedChain)
	onEvict func(cc chainCase, tick int64)
}

func newChainTracker(p *Params, player int, ids *uint64) *chainTracker {
	return &chainTracker{p: p, player: player, ids: ids, oppLife: map[int32]int32{}}
}

// opponentDamage returns the damage the focus dealt to all opposing roots
// since the previous frame, each as a fraction of its own maximum life, and
// whether one of them was knocked out on this frame. Damage an ally dealt
// (the opponent's last hitter was another player) is not the focus's.
func (t *chainTracker) opponentDamage(s *Snapshot) (dealt float32, ko bool) {
	for _, i := range s.Opponents(t.player) {
		r := &s.Players[i].Root
		prev, seen := t.oppLife[r.ID]
		t.oppLife[r.ID] = r.Life
		if !seen || r.LifeMax <= 0 {
			continue
		}
		if d := prev - r.Life; d > 0 && (r.HitBy == 0 || r.HitBy == t.player+1) {
			dealt += float32(d) / float32(r.LifeMax)
			if prev > 0 && r.Life <= 0 {
				ko = true
			}
		}
	}
	return dealt, ko
}

func (t *chainTracker) newID() uint64 {
	*t.ids++
	return *t.ids
}

// addCase attaches a newly opened case to the active chain, or to the
// lead-in when no exchange is in progress.
func (t *chainTracker) addCase(cc chainCase) {
	if cc.isAttack {
		for _, pr := range t.pending {
			if pr.resetOK && cc.startTick <= pr.waitUntil && cc.attackClass != pr.ch.lastAttackClass && cc.attackClass != StUnknown {
				pr.comp.Result = CompReset
				pr.comp.Quality = t.p.quality(CompReset)
				pr.tech.Flags |= TechReset
				pr.resetOK = false
				if t.active != nil {
					t.active.resetFollowup = true
				}
			}
		}
	}
	if t.active != nil {
		t.active.add(cc)
		return
	}
	t.leadIn = append(t.leadIn, cc)
	// Past leadInMax entries that count together (see leadInGroup), the
	// oldest of them leaves the lead-in.
	g := t.leadInGroup(cc)
	n := 0
	for _, l := range t.leadIn {
		if t.leadInGroup(l) == g {
			n++
		}
	}
	if n <= leadInMax {
		return
	}
	for i, ev := range t.leadIn {
		if t.leadInGroup(ev) == g {
			t.leadIn = append(t.leadIn[:i], t.leadIn[i+1:]...)
			if t.onEvict != nil {
				t.onEvict(ev, cc.startTick)
			}
			return
		}
	}
}

// leadInGroup tells which lead-in entries count together toward leadInMax
// (see the lead-in at the top of this file): with the base AI on, the
// decisions compared with each other (probes and played cases) apart from
// recorded cases; with it off, probes (there are none then) apart from
// cases.
func (t *chainTracker) leadInGroup(cc chainCase) bool {
	if t.p.BaseAI {
		return cc.rec == nil
	}
	return cc.probe
}

func (ch *chain) add(cc chainCase) {
	ch.cases = append(ch.cases, cc)
	if cc.probe {
		ch.probes++
	}
}

// withdraw removes a probe added at tick for situation key, if it is still
// in the active chain or the lead-in.
func (t *chainTracker) withdraw(key uint64, tick int64) {
	drop := func(cases []chainCase) ([]chainCase, bool) {
		for i := len(cases) - 1; i >= 0; i-- {
			if cc := cases[i]; cc.probe && cc.key == key && cc.startTick == tick {
				return append(cases[:i], cases[i+1:]...), true
			}
		}
		return cases, false
	}
	if t.active != nil {
		var ok bool
		if t.active.cases, ok = drop(t.active.cases); ok {
			t.active.probes--
			return
		}
	}
	t.leadIn, _ = drop(t.leadIn)
}

// contest marks the latest play of a case still in a chain as contested.
func (t *chainTracker) contest(caseID uint64) {
	mark := func(cases []chainCase) bool {
		for i := len(cases) - 1; i >= 0; i-- {
			if cases[i].caseID == caseID && !cases[i].probe {
				cases[i].contested = true
				return true
			}
		}
		return false
	}
	if t.active != nil && mark(t.active.cases) {
		return
	}
	mark(t.leadIn)
}

// split gives each of a chain's entries its place in the backward credit.
// Recorded and played cases count among themselves, as if there were no
// probes: pos[i] is case i's position among them and n their number. For the
// comparison with the base AI, only the CBR layer's played cases count:
// after[i] is how many of them follow entry i. Probes do not count, so that a
// probe and a move played in its place take the same share whatever the base
// AI does next: the base AI gets a probe at every change of state while it
// has control, many more decisions than the CBR layer makes, which makes one
// per move it plays, and counting them would hand the base AI's decisions
// smaller shares than the moves compared with them. Recorded cases do not
// count either, since they repeat the base AI's decisions when it is
// recorded as well.
func (ch *chain) split() (pos, after []int, n int) {
	pos = make([]int, len(ch.cases))
	after = make([]int, len(ch.cases))
	k := 0
	for i := len(ch.cases) - 1; i >= 0; i-- {
		cc := &ch.cases[i]
		if cc.probe || cc.caseID != 0 {
			after[i] = k
		}
		if !cc.probe && cc.caseID != 0 {
			k++
		}
	}
	for i := range ch.cases {
		if !ch.cases[i].probe {
			pos[i] = n
			n++
		}
	}
	return pos, after, n
}

func (t *chainTracker) start(s *Snapshot, self, en *CharSnap) {
	ch := &chain{
		id:            t.newID(),
		session:       s.World.Session,
		startTick:     s.World.Tick,
		selfLifeMax:   self.LifeMax,
		powerMax:      self.PowerMax,
		prevSelfLife:  self.Life,
		power0:        self.Power,
		lastPower:     self.Power,
		moveCount:     map[int32]int32{},
		lastDealtTick: -1,
		lastTakenTick: -1,
		firstHitTick:  -1,
		timeFrac0:     s.World.TimeFrac(),
	}
	for _, cc := range t.leadIn {
		ch.add(cc)
	}
	t.leadIn = t.leadIn[:0]
	t.active = ch
}

// step advances chain state by one frame. tr must already have observed s.
func (t *chainTracker) step(s *Snapshot, tr *tracker) {
	me := s.Player(t.player)
	if me == nil {
		return
	}
	self := &me.Root
	w := &s.World
	tick := w.Tick

	if !t.sessionSeen {
		t.session, t.sessionSeen = w.Session, true
	} else if w.Session != t.session {
		t.session = w.Session
		t.resolve(s, self, nil, false, 0)
		t.flushLeadIn(tick)
		t.finalizeAll()
		for k := range t.oppLife {
			delete(t.oppLife, k)
		}
	}

	dealt, ko := t.opponentDamage(s)
	var en *CharSnap
	if ei := s.Enemy(t.player); ei >= 0 {
		en = &s.Players[ei].Root
	}

	if t.active == nil && tr.inExchange && w.RoundState == 2 && en != nil {
		t.start(s, self, en)
	}

	if ch := t.active; ch != nil {
		t.update(ch, s, self, en, dealt, ko)
		roundEnded := w.RoundState >= 3
		exchangeEnded := t.prevInExchange && !tr.inExchange
		knockdownDone := en != nil && ch.sawDown && en.StateType == StLying && tick-ch.downTick >= otgWindow
		horizon := (t.p.HorizonFrames > 0 && ch.frames >= t.p.HorizonFrames) ||
			(t.p.HorizonCases > 0 && int32(len(ch.cases))-ch.probes >= t.p.HorizonCases)
		switch {
		case roundEnded:
			t.resolve(s, self, en, true, advantageNow(self, en))
		case exchangeEnded:
			t.resolve(s, self, en, false, tr.frameAdv)
		case knockdownDone:
			t.resolve(s, self, en, false, advantageNow(self, en))
		case horizon:
			ch.timedOut = true
			t.resolve(s, self, en, false, advantageNow(self, en))
			if tr.inExchange && en != nil {
				t.start(s, self, en)
			}
		}
	}

	// Finalize resolved chains whose reset window has passed.
	keep := t.pending[:0]
	for _, pr := range t.pending {
		if !pr.resetOK || tick > pr.waitUntil {
			t.finalize(pr)
		} else {
			keep = append(keep, pr)
		}
	}
	t.pending = keep

	t.prevInExchange = tr.inExchange
	if en != nil {
		t.prevEnemyState = en.StateType
	}
	t.prevInput = self.Input
}

func (t *chainTracker) update(ch *chain, s *Snapshot, self, en *CharSnap, dealt float32, ko bool) {
	tick := s.World.Tick
	ch.frames++
	if !s.World.TimerFrozen() {
		ch.timerTicks++
	}

	if dealt > 0 {
		ch.dealt += dealt
		ch.lastDealtTick = tick
		if ch.firstHitTick < 0 {
			ch.firstHitTick = tick
		}
		if en != nil && (t.prevEnemyState == StLying || en.StateType == StLying) {
			ch.otg = true
			ch.downTick = tick
		}
	}
	if ko {
		ch.enemyKO = true
	}
	if d := ch.prevSelfLife - self.Life; d > 0 && ch.selfLifeMax > 0 {
		ch.taken += float32(d) / float32(ch.selfLifeMax)
		ch.lastTakenTick = tick
	}
	ch.prevSelfLife = self.Life

	if en != nil && ch.firstHitTick >= 0 && en.Ctrl {
		ch.enemyCtrlHit = true
	}

	if self.MoveType == MtAttack {
		if !ch.inAttack || self.StateNo != ch.lastAttackRef {
			ch.inAttack = true
			ch.lastAttackRef = self.StateNo
			ch.lastAttackClass = self.StateType
			ch.lastAttackSpent = false
			ch.attackPower = self.Power
			ch.moveCount[self.StateNo]++
		} else if self.Power < ch.attackPower {
			ch.lastAttackSpent = true
		}
	} else {
		ch.inAttack = false
	}
	ch.lastPower = self.Power

	if en != nil {
		if en.StateType == StLying && t.prevEnemyState != StLying {
			ch.sawDown = true
			ch.downTick = tick
		}
		if ch.sawDown && t.prevEnemyState == StLying && en.StateType == StAir && self.MoveType == MtAttack {
			ch.relaunch = true
			ch.sawDown = false
		}
	}

	if self.Input.Presses(t.prevInput) != 0 {
		ch.pressTicks = append(ch.pressTicks, tick)
	}
}

// resolve grades the active chain and queues it for finalization.
func (t *chainTracker) resolve(s *Snapshot, self, en *CharSnap, roundEnded bool, adv int32) {
	ch := t.active
	if ch == nil {
		return
	}
	t.active = nil
	w := &s.World

	enemyKO := ch.enemyKO || (en != nil && en.Life <= 0)
	selfKO := self.Life <= 0
	pressure := en != nil && en.BlockStun > 0
	ch.punished = ch.taken > 0 && ch.lastTakenTick > ch.lastDealtTick

	tr := Completion{
		DamageDealt: ch.dealt,
		DamageTaken: ch.taken,
		RoundEnded:  roundEnded,
		Resolved:    true,
	}
	if ch.powerMax > 0 {
		tr.MeterDelta = float32(ch.lastPower-ch.power0) / float32(ch.powerMax)
	}
	tr.EndFrameAdv = adv
	tr.OpponentDown = ch.sawDown || (en != nil && en.StateType == StLying)
	tr.PressureKept = pressure
	if ch.lastAttackRef != 0 || ch.inAttack {
		tr.Ender = enderKind(ch.lastAttackRef, ch.lastAttackSpent)
	}
	tr.Result = resultState(ch, adv, roundEnded, enemyKO, selfKO, pressure)
	tr.Quality = t.p.quality(tr.Result)

	tech := Technique{ChainDuration: ch.frames}
	tech.RepeatedMove, tech.ChainRepetitions = mostRepeated(ch.moveCount)
	if ch.otg {
		if ch.relaunch {
			tech.Flags |= TechOTGRelaunch
		} else {
			tech.Flags |= TechOTGFinish
		}
	}
	if tech.ChainRepetitions >= t.p.LoopMinRepeats && ch.firstHitTick >= 0 && !ch.enemyCtrlHit {
		tech.Flags |= TechLoop
	}
	if ch.resetFollowup {
		tech.Flags |= TechReset
	}

	route := Route{
		Key:            routeKey(ch),
		DurationFrames: ch.frames,
		TimerTicks:     ch.timerTicks,
		DamageScaled:   ch.dealt,
	}
	dropped := tr.Result == CompDropped || tr.Result == CompPunished
	for i, pt := range ch.pressTicks {
		ls := LinkStat{CanonicalFrame: int32(pt - ch.startTick), Attempts: 1, Successes: 1}
		if dropped && i == len(ch.pressTicks)-1 {
			ls.Successes = 0
		}
		route.Links = append(route.Links, ls)
	}

	rc := &resolvedChain{ch: ch, comp: tr, tech: tech, route: route, endTick: w.Tick}
	rc.resetOK = !roundEnded && !tr.OpponentDown && ch.dealt > 0 &&
		(tr.Result == CompAdvantage || tr.Result == CompNeutral)
	if rc.resetOK {
		rc.waitUntil = w.Tick + int64(t.p.ResetWindow)
		t.pending = append(t.pending, rc)
	} else {
		t.finalize(rc)
	}
}

// routeKey hashes the chain's attack sequence (FNV-1a over state numbers).
func routeKey(ch *chain) uint64 {
	h := uint64(14695981039346656037)
	n := 0
	for _, cc := range ch.cases {
		if !cc.isAttack {
			continue
		}
		v := uint32(cc.stateNo)
		for i := 0; i < 4; i++ {
			h ^= uint64(byte(v >> (8 * uint(i))))
			h *= 1099511628211
		}
		n++
	}
	if n == 0 {
		return 0
	}
	return h
}

// advantageNow estimates frame advantage at the moment of resolution from
// the remaining stun and control of both sides.
func advantageNow(self, en *CharSnap) int32 {
	if en == nil {
		return 0
	}
	selfLock := self.HitStun + self.BlockStun
	enLock := en.HitStun + en.BlockStun
	if !self.Ctrl && selfLock == 0 && self.MoveType == MtAttack {
		selfLock = 1
	}
	if !en.Ctrl && enLock == 0 && en.MoveType == MtAttack {
		enLock = 1
	}
	if en.StateType == StLying {
		enLock += 30
	}
	if self.StateType == StLying {
		selfLock += 30
	}
	return enLock - selfLock
}

func (t *chainTracker) finalize(rc *resolvedChain) {
	t.round = append(t.round, rc)
	if t.onFinal != nil {
		t.onFinal(rc)
	}
}

func (t *chainTracker) finalizeAll() {
	for _, pr := range t.pending {
		t.finalize(pr)
	}
	t.pending = t.pending[:0]
}

func (t *chainTracker) flushLeadIn(tick int64) {
	for _, cc := range t.leadIn {
		if t.onEvict != nil {
			t.onEvict(cc, tick)
		}
	}
	t.leadIn = t.leadIn[:0]
}

// endRound resolves everything open and returns the round's chains in
// order, then clears them.
func (t *chainTracker) endRound(s *Snapshot) []*resolvedChain {
	if me := s.Player(t.player); me != nil && t.active != nil {
		var en *CharSnap
		if ei := s.Enemy(t.player); ei >= 0 {
			en = &s.Players[ei].Root
		}
		adv := int32(0)
		if en != nil {
			adv = advantageNow(&me.Root, en)
		}
		t.resolve(s, &me.Root, en, true, adv)
	}
	t.active = nil
	t.flushLeadIn(s.World.Tick)
	t.finalizeAll()
	out := t.round
	t.round = nil
	t.prevInExchange = false
	return out
}

// mostRepeated returns the move that recurs most often in a chain and how
// often. Ties go to the lowest move reference, so the result does not depend
// on map order: a match learned live and from its replay records the same.
func mostRepeated(counts map[int32]int32) (ref, n int32) {
	for r, c := range counts {
		if c > n || (c == n && r < ref) {
			ref, n = r, c
		}
	}
	return ref, n
}
