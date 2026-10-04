package cbr

// The recorder splits one player's frame stream into cases.
//
// A command case starts when the player takes an action that removes
// control or cancels one attack into another. Its start is back-dated to
// the beginning of the motion that produced it, so replaying the case from
// its start reproduces the whole input. A movement case covers neutral play
// between commands and is split on movement transitions, so spacing and
// approach are represented rather than folded into whatever came next.
//
// Frames on which the CBR layer supplied the input are never recorded:
// learning from its own output would narrow the model toward its habits.
//
// When the adapter reports command activity (CharSnap.CmdKnown), it also
// decides what counts as an action: a state change inside an attack with no
// command active is the move's own automatic transition, not a cancel; an
// attack started from control with no command active is a direct state
// change by the character's AI (see command.go).

type frameRec struct {
	tick      int64
	feat      Feat
	input     InputBits
	stateNo   int32
	stateType StateType
	moveType  MoveType
	ctrl      bool
	hitPause  int32
	driven    bool // the CBR layer supplied this frame's input
}

type recorder struct {
	p        *Params
	player   int
	source   Source
	opponent string
	ids      *uint64
	ct       *chainTracker
	st       *Store
	motions  map[string]Motion

	ring []frameRec
	rh   int
	rn   int

	open      *Case
	openStart int64
	reachCase *Case

	// Assisted command cases replace recorded inputs: tail is the synthesized
	// input for the exec frame, and after it the case holds neutral input,
	// since an assisted source's raw input is not what drove it.
	tail             InputBits
	hasTail          bool
	neutralAfterExec bool

	// skip holds recording after an unexplained action until control
	// returns. log is the match-long record shared by the round recorders
	// of one player slot (see matchLog).
	skip bool
	log  *matchLog

	// cmdFloor is the earliest tick a new command case may start at: just
	// after the previous command's exec frame. recent holds this round's
	// movement cases, which a back-dated command may cut into.
	cmdFloor int64
	recent   []recentCase

	started      bool
	prevStateNo  int32
	prevMoveType MoveType
	prevCtrl     bool
	prevState    StateType
	prevHoriz    uint8

	out []*Case
}

// ringFrames is how many frames the recorder keeps for back-dating. Motions
// synthesized for assisted commands can include a charge, so it exceeds
// MaxMotionFrames.
const ringFrames = 64

func newRecorder(p *Params, player int, src Source, ids *uint64, ct *chainTracker, st *Store, motions map[string]Motion, log *matchLog) *recorder {
	n := int(p.MaxMotionFrames) + 2
	if n < ringFrames+2 {
		n = ringFrames + 2
	}
	if log == nil {
		log = &matchLog{source: src}
	}
	r := &recorder{p: p, player: player, source: src, ids: ids, ct: ct, st: st, motions: motions, ring: make([]frameRec, n), log: log}
	return r
}

// matchLog is what one player slot's recording accumulates over a match.
// Assisted sources' cases and command-map observations wait here until the
// match ends, so a source found to drive states directly (flagged) can be
// discarded whole; hypotheses are kept either way.
type matchLog struct {
	source      Source
	unexplained int32
	flagged     bool
	// pending holds this match's command-map observations, by state and
	// command; they count toward command resolution during the match and
	// are committed to the store at its end.
	pending map[int32]map[string]float32
	staged  []*Case
	// active keeps the commands that were active when each staged assisted
	// case was recorded, to check its attribution against the whole match's
	// observations before it is committed.
	active map[uint64][]string
	hypo   []*Case
}

func (l *matchLog) note(state int32, names []string) {
	if l.pending == nil {
		l.pending = map[int32]map[string]float32{}
	}
	m := l.pending[state]
	if m == nil {
		m = map[string]float32{}
		l.pending[state] = m
	}
	for _, n := range names {
		m[n]++
	}
}

func (r *recorder) push(fr frameRec) {
	r.ring[r.rh] = fr
	r.rh = (r.rh + 1) % len(r.ring)
	if r.rn < len(r.ring) {
		r.rn++
	}
}

// at returns the ring entry for tick, if still held.
func (r *recorder) at(tick int64) (*frameRec, bool) {
	n := len(r.ring)
	for i := 0; i < r.rn; i++ {
		e := &r.ring[(r.rh-1-i+n)%n]
		if e.tick == tick {
			return e, true
		}
		if e.tick < tick {
			break
		}
	}
	return nil, false
}

// motionStart scans back from the frame before tick over held, non-neutral
// input and returns the first frame of the motion. It crosses up to gaps
// short neutral gaps when non-neutral input lies beyond them, for motions
// that tap the same direction twice (a dash's F, release, F), and never
// reaches more than maxBack frames back, into frames the CBR layer drove,
// or outside the fight.
func (r *recorder) motionStart(tick int64, gaps int, maxBack int64) int64 {
	start := tick
	gap := 0
	n := len(r.ring)
	for i := 0; i < r.rn; i++ {
		e := &r.ring[(r.rh-1-i+n)%n]
		if e.tick >= tick {
			continue
		}
		if tick-e.tick > maxBack || e.driven || e.feat.RoundState != 2 {
			break
		}
		if e.input.Neutral() {
			if gaps == 0 || gap >= maxTapGap {
				break
			}
			gap++
			continue
		}
		if gap > 0 {
			gaps--
			gap = 0
		}
		start = e.tick
	}
	return start
}

// maxTapGap is the longest neutral gap crossed inside a double tap.
const maxTapGap = 6

// tapGaps counts the neutral gaps in a canonical motion that separate two
// presses of the same direction.
func tapGaps(m []InputBits) int {
	n := 0
	var last InputBits
	inGap := false
	for _, b := range m {
		d := b.Directions()
		switch {
		case b.Neutral():
			inGap = last != 0
		case inGap:
			if d == last {
				n++
			}
			inGap = false
			last = d
		default:
			last = d
		}
	}
	return n
}

func horizClass(b InputBits) uint8 {
	switch b.Dir() {
	case 4, 1, 7:
		return 1
	case 6, 3, 9:
		return 2
	}
	return 0
}

// step consumes one frame. f is the player's feature set for this frame.
func (r *recorder) step(s *Snapshot, f Feat) {
	me := s.Player(r.player)
	if me == nil {
		return
	}
	self := &me.Root
	tick := s.World.Tick
	r.push(frameRec{
		tick: tick, feat: f, input: self.Input,
		stateNo: self.StateNo, stateType: self.StateType, moveType: self.MoveType, ctrl: self.Ctrl,
		hitPause: self.HitPause, driven: self.CBRDriven,
	})

	if r.log.flagged {
		// Nothing more is recorded from this source this match, but the
		// states it enters directly are still collected as hypotheses.
		if r.started && self.CmdKnown && !self.CBRDriven && s.World.RoundState == 2 &&
			self.StateNo != r.prevStateNo && r.prevCtrl && !self.Ctrl &&
			self.MoveType == MtAttack && !r.significant(self.Commands) {
			r.st.hypothesis(self.StateNo).Seen++
			r.hypothesisCase(self, tick)
		}
		r.started = true
		r.remember(self)
		return
	}
	if self.CBRDriven || s.World.RoundState != 2 {
		r.closeOpen(tick)
		r.remember(self)
		return
	}
	if !r.started {
		r.remember(self)
		r.started = true
		r.openMovement(tick)
		r.appendInput(self.Input)
		return
	}

	changed := self.StateNo != r.prevStateNo
	landing := r.prevState == StAir && self.StateType != StAir && self.MoveType == MtIdle
	fromCtrl := r.prevCtrl && !self.Ctrl
	cancel := r.prevMoveType == MtAttack && self.MoveType == MtAttack
	command := changed && self.MoveType != MtHit && !landing && (fromCtrl || cancel)
	regain := !r.prevCtrl && self.Ctrl

	r.trackReach(self)

	// An action needs a significant command: one with a button or more than
	// one step. Direction holds are active nearly all the time and explain
	// only the engine's own direction-driven states (walk, jump, guard).
	significant := false
	if command && self.CmdKnown {
		if len(self.Commands) > 0 {
			if r.source.IsHuman() {
				r.st.noteCommand(self.StateNo, self.Commands)
			} else {
				r.log.note(self.StateNo, self.Commands)
			}
		}
		significant = r.significant(self.Commands)
		switch {
		case significant:
		case fromCtrl && self.MoveType == MtAttack:
			r.unexplainedStart(s, self, f, tick)
			r.remember(self)
			return
		case !fromCtrl:
			command = false // the move's own transition, not a cancel
		}
	}
	if r.skip {
		if !regain {
			r.remember(self)
			return
		}
		r.skip = false
	}

	length := int32(0)
	if r.open != nil {
		length = int32(tick - r.openStart)
	}

	switch {
	case command:
		if significant && r.source.assisted() {
			r.openAssisted(s, self, f, tick)
		} else {
			r.openCommand(s, self, f, tick)
		}
	case regain:
		r.closeOpen(tick)
		r.openMovement(tick)
	case r.open == nil:
		r.openMovement(tick)
	case r.open.Kind == KindMovement && r.movementSplit(self, length):
		r.closeOpen(tick)
		r.openMovement(tick)
	case length >= r.p.MaxCaseFrames:
		r.closeOpen(tick)
		r.openMovement(tick)
	}
	r.appendInput(self.Input)
	r.remember(self)
}

// significant reports whether any of the active commands has a button or
// more than one step. A command without a synthesized motion counts, since
// nothing is known about it.
func (r *recorder) significant(active []string) bool { return significant(active, r.motions) }

func significant(active []string, motions map[string]Motion) bool {
	for _, n := range active {
		m, ok := motions[n]
		if !ok || m.hasButton() || m.Steps > 1 {
			return true
		}
	}
	return false
}

// unexplainedStart handles an attack started from control with no command
// active. A person's input always shows in the command layer, so this only
// counts against AI sources.
func (r *recorder) unexplainedStart(s *Snapshot, self *CharSnap, f Feat, tick int64) {
	r.closeOpen(tick)
	r.skip = true
	if r.source.IsHuman() {
		return
	}
	r.log.unexplained++
	r.st.hypothesis(self.StateNo).Seen++
	r.hypothesisCase(self, tick)
	if r.log.unexplained >= r.p.ChangeStateFlagCount {
		r.flag()
	}
}

// flag marks the source as direct-state-change AI for the rest of the
// match and drops what it recorded. The engine discards cases it staged
// from this recorder earlier in the match.
func (r *recorder) flag() {
	r.log.flagged = true
	r.st.ChangeStateAI = true
	r.open = nil
	r.reachCase = nil
	r.out = nil
}

// hypothesisCase stores a synthesized case for a state entered by direct
// state change, when the command map knows which command leads there.
func (r *recorder) hypothesisCase(self *CharSnap, tick int64) {
	h := r.st.hypothesis(self.StateNo)
	if h.CheatOnly {
		return
	}
	name, ok := r.st.hypothesisCommand(self.StateNo, r.motions, r.p.HypothesisMinEvidence, r.log.pending)
	if !ok {
		return
	}
	h.Command = name
	n := r.st.liveHypotheses(self.StateNo)
	for _, c := range r.log.hypo {
		if c.MoveRef == self.StateNo {
			n++
		}
	}
	if n >= r.p.HypothesisCasesPerState {
		return
	}
	m := r.motions[name].Inputs
	start := tick - int64(len(m)-1)
	fr, ok := r.at(start)
	if !ok || r.lastDriven(start, tick) >= start || !r.inFight(start-1) {
		return
	}
	c := r.newCase(KindCommand, fr)
	c.Source = SrcSynthesized
	c.Command = name
	c.Inputs = append([]InputBits(nil), m...)
	c.ExecFrame = int32(len(m) - 1)
	c.MoveRef = self.StateNo
	c.ExecAttack, c.ExecClass = true, self.StateType
	c.Completion = Completion{Result: CompUnknown}
	r.log.hypo = append(r.log.hypo, c)
}

// trackReach resolves the contact result of the most recent attack case:
// hit or guarded as soon as contact registers, whiff when the attack state
// ends without contact.
func (r *recorder) trackReach(self *CharSnap) {
	c := r.reachCase
	if c == nil {
		return
	}
	if self.StateNo == c.MoveRef && self.MoveType == MtAttack {
		switch {
		case self.MoveHit > 0:
			c.Reach.Contact, c.Reach.Valid = 1, true
			r.reachCase = nil
		case self.MoveGuarded > 0:
			c.Reach.Contact, c.Reach.Valid = 2, true
			r.reachCase = nil
		}
		return
	}
	c.Reach.Contact, c.Reach.Valid = 0, true
	r.reachCase = nil
}

func (r *recorder) remember(self *CharSnap) {
	r.prevStateNo = self.StateNo
	r.prevMoveType = self.MoveType
	r.prevCtrl = self.Ctrl
	r.prevState = self.StateType
	r.prevHoriz = horizClass(self.Input)
}

func (r *recorder) movementSplit(self *CharSnap, length int32) bool {
	if length < r.p.MinCaseFrames {
		return false
	}
	if length >= r.p.MovementCaseFrames {
		return true
	}
	if horizClass(self.Input) != r.prevHoriz {
		return true
	}
	if (self.StateType == StAir) != (r.prevState == StAir) {
		return true
	}
	return false
}

func (r *recorder) appendInput(b InputBits) {
	if r.open == nil {
		return
	}
	switch {
	case r.hasTail:
		b, r.hasTail = r.tail, false
	case r.neutralAfterExec:
		b = 0
	}
	r.open.Inputs = append(r.open.Inputs, b)
}

// humanMotionSlack is how many frames beyond a command's canonical motion a
// person's input of it may take, for back-dating a long motion (a charge).
const humanMotionSlack = 6

// lastDriven returns the latest held tick in [from, to) whose input the CBR
// layer supplied, or from-1 when there is none.
func (r *recorder) lastDriven(from, to int64) int64 {
	n := len(r.ring)
	for i := 0; i < r.rn; i++ {
		e := &r.ring[(r.rh-1-i+n)%n]
		if e.tick < from {
			break
		}
		if e.tick < to && e.driven {
			return e.tick
		}
	}
	return from - 1
}

// inFight reports whether the held frame at tick was a fight frame (round
// state 2). Cases are only selected in the fight, so one must not take its
// decision state from the intro.
func (r *recorder) inFight(tick int64) bool {
	e, ok := r.at(tick)
	return !ok || e.feat.RoundState == 2
}

// decisionFrame returns the state the decision to play the case was made
// from: the frame before its first input was applied. A snapshot shows the
// state after that frame's input, so matching at playback, which happens
// before the next input is applied, must compare against the prior frame.
func (r *recorder) decisionFrame(start *frameRec) *frameRec {
	if prev, ok := r.at(start.tick - 1); ok {
		return prev
	}
	return start
}

func (r *recorder) newCase(kind CaseKind, start *frameRec) *Case {
	start = r.decisionFrame(start)
	*r.ids++
	c := &Case{
		ID:             *r.ids,
		Source:         r.source,
		Kind:           kind,
		Opponent:       r.opponent,
		ExecFrame:      -1,
		StartStateType: start.stateType,
		StartCtrl:      start.ctrl,
		StartStateNo:   start.stateNo,
		StartMoveType:  start.moveType,
		StartHitPause:  start.hitPause,
		Tick:           start.tick,
	}
	c.Start = start.feat
	return c
}

func (r *recorder) openMovement(tick int64) {
	fr, ok := r.at(tick)
	if !ok {
		return
	}
	c := r.newCase(KindMovement, fr)
	r.open, r.openStart = c, tick
	r.neutralAfterExec, r.hasTail = false, false
	r.recent = append(r.recent, recentCase{c, tick})
	if len(r.recent) > maxRecent {
		r.recent = r.recent[len(r.recent)-maxRecent:]
	}
	r.ct.addCase(chainCase{rec: c, startTick: tick, stateNo: fr.stateNo})
}

// recentCase is a movement case still open to truncation, with the tick of
// its first input.
type recentCase struct {
	c     *Case
	start int64
}

// maxRecent bounds how many movement cases a back-dated command may cut
// into. Movement cases are at least MinCaseFrames long, so this covers well
// over the longest back-dating window.
const maxRecent = 32

// cutFrom makes room for a command case starting at tick: every movement
// case recorded this round is truncated to end before tick (a movement case
// lying entirely after tick is left empty and dropped when finalized), and
// the open case is closed there. Callers keep tick at or after cmdFloor, so
// no command case loses its own action.
func (r *recorder) cutFrom(tick int64) {
	for _, rc := range r.recent {
		keep := int(tick - rc.start)
		if keep < 0 {
			keep = 0
		}
		if keep < len(rc.c.Inputs) {
			rc.c.Inputs = rc.c.Inputs[:keep]
		}
	}
	r.closeAt(tick)
}

// openCommand starts a command case for an action observed at tick,
// back-dated to the start of its motion.
func (r *recorder) openCommand(s *Snapshot, self *CharSnap, f Feat, tick int64) {
	// When the active command is known, its canonical motion says how far
	// back a person's input of it can reach and whether it taps a direction
	// twice across a neutral gap.
	gaps, maxBack := 0, int64(r.p.MaxMotionFrames)
	if self.CmdKnown && len(self.Commands) > 0 {
		if name, ok := r.st.commandFor(self.StateNo, self.Commands, r.motions, self.MoveType == MtAttack, r.log.pending); ok {
			m := r.motions[name].Inputs
			gaps = tapGaps(m)
			if b := int64(len(m) + humanMotionSlack); b > maxBack {
				maxBack = b
			}
		}
	}
	start := r.motionStart(tick, gaps, maxBack)
	// Never back into the previous command's own action.
	if start < r.cmdFloor {
		start = r.cmdFloor
	}
	fr, ok := r.at(start)
	if !ok {
		start = tick
		fr, ok = r.at(tick)
		if !ok {
			return
		}
	}
	r.cutFrom(start)

	c := r.newCase(KindCommand, fr)
	c.ExecFrame = int32(tick - start)
	c.MoveRef = self.StateNo
	// Inputs from the motion start up to the frame before tick come from the
	// ring; this frame's input is appended by step.
	for t := start; t < tick; t++ {
		if e, ok := r.at(t); ok {
			c.Inputs = append(c.Inputs, e.input)
		}
	}
	r.open, r.openStart = c, start
	r.cmdFloor = tick + 1
	r.neutralAfterExec, r.hasTail = false, false
	r.attackInfo(c, self, f, tick)
}

// openAssisted starts a command case for an assisted source, with the
// inputs of the command that caused the action instead of the raw ones,
// back-dated by the motion's length. A motion that would have to begin
// inside the previous command's own action is faster than a person could
// input it in sequence, and one that overlaps frames the CBR layer drove is
// not this source's play; neither is recorded.
func (r *recorder) openAssisted(s *Snapshot, self *CharSnap, f Feat, tick int64) {
	name, ok := r.st.commandFor(self.StateNo, self.Commands, r.motions, self.MoveType == MtAttack, r.log.pending)
	if !ok {
		r.openCommand(s, self, f, tick)
		return
	}
	mo := r.motions[name]
	if !mo.hasButton() && mo.Steps <= 1 {
		// A direction hold the raw input itself completed.
		r.openCommand(s, self, f, tick)
		return
	}
	m := mo.Inputs
	start := tick - int64(len(m)-1)
	fr, ok := r.at(start)
	if !ok || start < r.cmdFloor || r.lastDriven(start, tick) >= start || !r.inFight(start-1) {
		r.closeOpen(tick)
		r.skip = true
		return
	}
	r.cutFrom(start)

	c := r.newCase(KindCommand, fr)
	c.Command = name
	c.ExecFrame = int32(tick - start)
	c.MoveRef = self.StateNo
	c.Inputs = append(c.Inputs, m[:len(m)-1]...)
	if r.log.active == nil {
		r.log.active = map[uint64][]string{}
	}
	r.log.active[c.ID] = append([]string(nil), self.Commands...)
	r.open, r.openStart = c, start
	r.cmdFloor = tick + 1
	r.tail, r.hasTail = m[len(m)-1], true
	// After an assisted attack command the raw input is not what drove the
	// character; after a direction motion (a dash) it may still be.
	r.neutralAfterExec = mo.hasButton()
	r.attackInfo(c, self, f, tick)
}

// attackInfo fills a new command case's attack fields and joins it to the
// chain tracker.
func (r *recorder) attackInfo(c *Case, self *CharSnap, f Feat, tick int64) {
	attack := self.MoveType == MtAttack
	// A cancel: the action began straight out of another attack. Its origin
	// and the point in that attack are what the cancel graph learns.
	if attack && r.prevMoveType == MtAttack && r.prevStateNo != self.StateNo {
		c.CancelFrom = r.prevStateNo
		if e, ok := r.at(tick - 1); ok {
			c.CancelTime = e.feat.Self.StateTime
		}
	}
	class := StUnknown
	if attack {
		class = self.StateType
	}
	c.ExecAttack, c.ExecClass = attack, class
	if attack && f.Enemy.Present {
		c.Reach = ReachRec{DX: f.Enemy.RelX, DY: f.Enemy.RelY, EnemyState: f.Enemy.State, Contact: -1}
		r.reachCase = c
	}
	r.ct.addCase(chainCase{rec: c, startTick: tick, stateNo: self.StateNo, isAttack: attack, attackClass: class})
}

// closeAt truncates the open case so it ends just before tick.
func (r *recorder) closeAt(tick int64) {
	if r.open == nil {
		return
	}
	keep := int(tick - r.openStart)
	if keep < 0 {
		keep = 0
	}
	if keep < len(r.open.Inputs) {
		r.open.Inputs = r.open.Inputs[:keep]
	}
	r.open = nil
	r.hasTail, r.neutralAfterExec = false, false
}

func (r *recorder) closeOpen(tick int64) { r.closeAt(tick) }

// finalizeCase writes a resolved chain's grade into one recorded case.
// pos is the case's index in its chain, n the chain's length.
func (r *recorder) finalizeCase(c *Case, rc *resolvedChain, pos, n int) {
	if c == r.open {
		r.closeOpen(rc.endTick)
	}
	if r.log.flagged {
		return
	}
	c.ChainID = rc.ch.id
	c.ChainPos = int32(pos)
	for _, pt := range rc.ch.pressTicks {
		if pt <= c.Tick {
			c.LinkOffset++
		}
	}
	if c.Kind == KindCommand && (c.ExecFrame < 0 || int(c.ExecFrame) >= len(c.Inputs)) {
		// Defensive: a command case must contain its exec frame.
		c.Kind, c.ExecFrame, c.ExecAttack = KindMovement, -1, false
	}
	c.Completion = rc.comp
	c.Technique = rc.tech
	c.Route = rc.route
	if len(c.Inputs) == 0 {
		c.Removed = true
		return
	}
	if c.Source != SrcTrial {
		disc := pow32(r.p.ComboGamma, n-1-pos)
		c.Outcome.Trials = 1
		c.Outcome.ValueSum = r.p.reward(&rc.comp) * disc
		c.Outcome.LastTick = rc.endTick
	}
	r.out = append(r.out, c)
}

// finalizeLoose resolves a neutral case that never joined an exchange.
func (r *recorder) finalizeLoose(c *Case, tick int64) {
	if c == r.open {
		r.closeOpen(tick)
	}
	if r.log.flagged {
		return
	}
	if len(c.Inputs) == 0 {
		c.Removed = true
		return
	}
	c.Completion = Completion{Result: CompNeutral, Quality: r.p.quality(CompNeutral), Resolved: true}
	if c.Source != SrcTrial {
		c.Outcome.Trials = 1
		c.Outcome.ValueSum = r.p.Situation[CompNeutral]
		c.Outcome.LastTick = tick
	}
	r.out = append(r.out, c)
}

func (r *recorder) take() []*Case {
	out := r.out
	r.out = nil
	return out
}

func pow32(b float32, n int) float32 {
	r := float32(1)
	for i := 0; i < n; i++ {
		r *= b
	}
	return r
}
