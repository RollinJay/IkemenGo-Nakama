package cbr

import (
	"math"
	"math/rand"
)

// The driver plays cases back for a CBR-driven player. It emits one frame
// of input per call, converted to the character's current facing, and
// declines when it has nothing confident, or nothing that proved better
// than the base AI (see baseline.go), so the character's own AI keeps
// control.

const (
	retryFrames       = 4   // wait after declining before trying again
	betterCheckFrames = 2   // how often an interruptible case is re-evaluated
	execSlackFrames   = 4   // frames past ExecFrame before a missing command aborts
	defaultLinkSD     = 1.5 // timing spread assumed for an unmeasured link
	// movingRetryFrames is the wait after declining while the character
	// moves with control, with the base AI on: walking, running, drifting
	// in the air (see driver.moving). A new situation ends it at once.
	movingRetryFrames = 2
)

type driver struct {
	p      *Params
	player int
	levels Levels
	opp    string
	rng    *rand.Rand
	st     *Store
	ct     *chainTracker

	cur *Case
	// curKey is the situation the current case is played in, where its result
	// counts against the base AI's (0 with the base AI off; see playKey), and
	// curStart the start tick its chain entry has (see begin).
	curKey      uint64
	curStart    int64
	sched       []InputBits
	pos         int
	shift       int // neutral frames put before the case's inputs (see begin)
	perturbed   bool
	execSeen    bool
	execNoted   bool  // the current case's exec result has been recorded
	lastDecline int64 // tick of the last choice that found nothing
	declined    bool
	declineNo   int32 // state and control at that decline: a change ends the back-off
	declineCtl  bool
	lastCheck   int64 // tick of the last better-case check

	// The comparison with the base AI (see baseline.go): margin is the
	// take-over margin; held is set when the last choice left the base AI in
	// control although moves matched; explores counts this round's
	// explorations, and retries those of them that tried a move again; sit
	// is the current decision situation, with what was drawn for it (whether
	// it may still explore, with sitDraw deciding for which moves, and whether
	// it is left to the base AI) and whether a probe follows it; spanned is
	// set when a play went into it and counts it as one of its decisions (the
	// play of case spanID begun at spanStart, see follow); waitCtrl holds off
	// until control returns after the character's own AI took over from a
	// play.
	margin     float32
	held       bool
	explores   int32
	retries    int32
	sit        decisionSit
	sitSeen    bool
	sitExplore bool
	sitDraw    float32
	sitYield   bool
	sitProbed  bool
	probeTick  int64
	spanned    bool
	spanID     uint64
	spanStart  int64
	waitCtrl   bool

	cpCache      map[uint64]float32
	lastBest     float32
	lastBestDist float32
	gateKeys     []uint64 // gateFilter's scratch

	// Verifying commands (see Verdict): tests counts this round's test plays
	// (see Params.VerifyPerRound); vkey is the spot of the current play's
	// command, where it was begun, and vpow the power it was begun at; hit is
	// set when the character was hit or guarded during the play, which then
	// says nothing of its command. prev is the frame observed before the
	// last one.
	tests    int32
	vkey     VerdictKey
	vpow     int32
	hit      bool
	prev     CharSnap
	prevSeen bool
	// moved is set when the last frame observed put the character somewhere
	// else than the frame before it (see moving).
	moved bool

	// Leftover input (see carryOver): hist holds the character's last
	// carryFrames inputs, the newest just before histAt; carry is set when
	// the current play began right after input that could complete another
	// command with the case's own, and other when something else came out
	// during the play. releases and releaseFrames count the plays begun with
	// neutral input to let such input go, and the frames that took; carried
	// counts the failed plays put down to such input.
	hist                             [carryFrames]InputBits
	histAt, histN                    int
	carry, other                     bool
	releases, releaseFrames, carried int
	// motions are the character's commands' canonical inputs (see
	// Engine.SetMotions); carrySeq and carryLead are carryOver's scratch.
	motions             map[string]Motion
	carrySeq, carryLead []InputBits
}

// carryFrames is how many of the character's last inputs the driver keeps
// to find leftover input (see carryOver).
const carryFrames = 32

func newDriver(p *Params, player int, lv Levels, seed int64, st *Store, ct *chainTracker) *driver {
	return &driver{p: p, player: player, levels: lv.clamp(), rng: rand.New(rand.NewSource(seed)), st: st, ct: ct,
		margin: p.TakeoverMargin}
}

// observe is called once per logical frame with the frame's result.
func (d *driver) observe(self *CharSnap) {
	d.observeOwn(self)
	d.moved = d.prevSeen && self.Pos != d.prev.Pos
	d.prev, d.prevSeen = *self, true
}

// moving reports whether the character, as last observed (self), moves with
// control: it walks, runs, crouch-walks or is in the air with control, so
// the distance to the enemy, and with it the situation, keeps changing. The
// velocity says so where the engine reports it, the position otherwise.
func (d *driver) moving(self *CharSnap) bool {
	return self.Ctrl && (d.moved || self.Vel[0] != 0 || self.Vel[1] != 0 || self.StateType == StAir)
}

// moveWithoutControl reports whether the character is in a movement of its
// own without control, neither attacking, being hit nor guarding: a dash or
// a hop that takes control away, the start of a jump, a landing, getting
// up. With the base AI on, only a command whose inputs were seen to work
// from that very state may take over there (see chooseWhy); anything else
// waits for the first frame with control.
func moveWithoutControl(self *CharSnap) bool {
	return !self.Ctrl && self.MoveType == MtIdle && self.HitStun == 0 && self.BlockStun == 0
}

// observeOwn is observe's part about the case being played, before self
// becomes the frame before.
func (d *driver) observeOwn(self *CharSnap) {
	prev, seen := &d.prev, d.prevSeen
	d.hist[d.histAt] = self.Input
	d.histAt = (d.histAt + 1) % carryFrames
	d.histN = min(d.histN+1, carryFrames)

	c := d.cur
	changed := seen && (self.Entry != EntryNone || !labelled(self) && self.StateNo != prev.StateNo)
	if c != nil && self.CBRDriven {
		if self.MoveType == MtHit {
			d.hit = true
		}
		// Something other than the move came out of the CBR layer's input.
		if changed && self.StateNo != c.MoveRef &&
			(self.MoveType == MtAttack || self.Entry == EntryInput && self.EntryCommand != "") {
			d.other = true
		}
	}
	// The state must have been entered during the case: a case that repeats
	// the move the character is already in counts only a fresh entry. One
	// the character's own AI decided is not the case's (see watchDriven).
	own := c != nil && c.ExecFrame >= 0 && self.StateNo == c.MoveRef && self.StateTime <= int32(d.pos)
	if own && !d.execNoted && !self.Entry.AIDecision() {
		// The case's command came out of the CBR layer's input as a person's
		// would: by the engine's label, or without labels, with the command
		// active. That verifies it where the case was begun (see Verdict);
		// another command that brought the state about is verified there
		// instead.
		switch {
		case !self.CBRDriven:
		case self.Entry == EntryInput && self.EntryCommand != "":
			k := d.vkey
			k.Command = self.EntryCommand
			d.st.successAt(k, d.vpow)
		case c.Command != "" && !labelled(self) && hasCommand(self.Commands, c.Command):
			d.st.successAt(d.vkey, d.vpow)
		}
		d.execSeen = true
		d.noteExec(true)
	} else if changed && self.CBRDriven && self.Entry == EntryInput && self.EntryCommand != "" && !own {
		// Any other state the CBR layer's input brought about as a person's
		// command: the command works from the moment before.
		d.st.successAt(spotKey(self.StateNo, self.EntryCommand, prev), prev.Power)
	}
}

// hasCommand reports whether name is among cmds.
func hasCommand(cmds []string, name string) bool {
	for _, n := range cmds {
		if n == name {
			return true
		}
	}
	return false
}

// noteExec records once per play whether the case's command came out.
func (d *driver) noteExec(came bool) {
	c := d.cur
	if c == nil || d.execNoted || c.ExecFrame < 0 {
		return
	}
	d.execNoted = true
	if !came {
		c.Outcome.ExecFails++
	}
	if c.Source == SrcSynthesized {
		d.p.resolveHypothesisPlay(d.st, c, came)
	}
}

// input returns this frame's input from the case being played, starting a
// new case when none is active. ok is false when the driver declines. The
// state given is the last observed frame, which is the latest complete one
// when the engine resolves input for the next.
func (d *driver) input(w *WorldSnap, self *CharSnap, cur *Feat) (InputBits, bool) {
	if w.RoundState != 2 {
		d.cur = nil
		return 0, false
	}
	tick := w.Tick + 1

	if c := d.cur; c != nil {
		if d.p.BaseAI {
			d.follow(cur, self, tick)
		}
		execPending := c.ExecFrame >= 0 && !d.execSeen
		switch {
		case self.BeingHit() && c.StartMoveType != MtHit:
			d.abort()
		case execPending && d.pos > int(c.ExecFrame)+d.shift+execSlackFrames:
			d.execFailed()
		case !execPending && d.pos >= len(d.sched):
			d.cur = nil
		case !execPending && self.Ctrl && tick-d.lastCheck >= betterCheckFrames:
			// The recovered better-case check: while the character can act
			// and the case has nothing committed left, switch when another
			// case beats this one, re-scored against the current state, by
			// BetterCaseThreshold. With the base AI on, only to a case that
			// proved better than the base AI here.
			d.lastCheck = tick
			g := &gate{key: baselineKey(cur, self.Ctrl, self.AILevel)}
			if nc, _ := d.chooseWhy(cur, self, tick, nil, g); nc != nil && nc != c && d.lastBest+d.p.BetterCaseThreshold < d.currentScore(cur) {
				d.begin(nc, self, tick, d.playSit(cur, self))
			}
		}
	}
	if d.cur == nil {
		if d.waitCtrl {
			if !self.Ctrl {
				return 0, false
			}
			d.waitCtrl = false
		}
		// After a decline, wait a few frames before searching again unless
		// the situation changed; mid-action cases need frame-exact starts,
		// so while busy every frame is tried. When moves matched but none
		// proved better than the base AI, the wait with control is longer,
		// and it also ends when the situation compared with the base AI
		// changes (the enemy starts an attack, say), where a move may have
		// proved better. While the base AI moves the character with control
		// (see moving), the distance keeps changing: the wait is short
		// whatever the last choice found, and ends as soon as the character
		// enters a new situation (the next distance band, say), where a move
		// may have proved better from that distance.
		wait := int64(retryFrames)
		if d.held {
			wait = int64(d.p.NotBetterBackoff)
		}
		moving := d.p.BaseAI && d.moving(self)
		if moving {
			wait = movingRetryFrames
		}
		if !self.Ctrl {
			wait = 1
		}
		unchanged := self.StateNo == d.declineNo && self.Ctrl == d.declineCtl
		if unchanged && (d.held || moving) {
			unchanged = baselineKey(cur, self.Ctrl, self.AILevel) == d.sit.key
		}
		if d.declined && unchanged && tick-d.lastDecline < wait {
			return 0, false
		}
		c := d.decide(cur, self, tick)
		if c == nil {
			d.lastDecline, d.declined = tick, true
			d.declineNo, d.declineCtl = self.StateNo, self.Ctrl
			return 0, false
		}
		d.declined = false
		d.begin(c, self, tick, d.playSit(cur, self))
	}
	if d.pos >= len(d.sched) {
		// Inputs done, command not yet out: hold neutral until it shows or
		// the exec check gives up.
		d.pos++
		return 0, true
	}
	b := d.sched[d.pos]
	d.pos++
	return b, true
}

// playSit is the situation a play begun now is made in, where its result
// counts against the base AI's (see playKey): the decision situation the
// base AI is measured in, or 0 with the base AI off, when plays are compared
// with nothing.
func (d *driver) playSit(cur *Feat, self *CharSnap) uint64 {
	if !d.p.BaseAI {
		return 0
	}
	return baselineKey(cur, self.Ctrl, self.AILevel)
}

// begin starts playing case c in situation sit (see playSit).
func (d *driver) begin(c *Case, self *CharSnap, tick int64, sit uint64) {
	d.unspan(tick)
	d.cur, d.curKey = c, sit
	d.pos = 0
	d.execSeen, d.execNoted = false, false
	c.Outcome.Plays++
	d.sched, d.perturbed = d.schedule(c)
	// A command played from control needs the same fresh presses it was
	// recorded with. When its first frame presses a key the character holds
	// now but did not hold at the recorded decision frame, release
	// everything for a frame first. Cases begun mid-action continue the
	// recorded input stream as it was and are left alone.
	d.shift = 0
	if c.ExecFrame >= 0 && c.StartCtrl && len(d.sched) > 0 {
		var held InputBits
		if k := len(c.Start.InBtn); k > 0 {
			held |= c.Start.InBtn[k-1]
		}
		if k := len(c.Start.InDir); k > 0 {
			held |= dirBits(c.Start.InDir[k-1])
		}
		if d.sched[0]&self.Input&^held != 0 {
			d.sched = append([]InputBits{0}, d.sched...)
			d.shift = 1
		}
	}
	// Directions held just before that could make another command of the
	// case's own are let go first (see carryOver).
	release, carry := d.carryOver(c)
	if release > d.shift {
		d.sched = append(make([]InputBits, release-d.shift, release-d.shift+len(d.sched)), d.sched...)
		d.shift = release
		d.releases++
		d.releaseFrames += release
	}
	d.carry, d.other, d.hit = carry, false, false
	d.vkey, d.vpow = spotKey(c.MoveRef, c.Command, self), self.Power
	d.st.markUsed(c.ID, tick)
	d.curStart = tick + int64(d.shift) + int64(maxi32(c.ExecFrame, 0))
	d.ct.addCase(chainCase{
		caseID:      c.ID,
		startTick:   d.curStart,
		stateNo:     c.MoveRef,
		isAttack:    c.ExecAttack,
		attackClass: c.ExecClass,
		perturbed:   d.perturbed,
		level:       int8(d.levels.Execution),
		key:         sit,
	})
}

func (d *driver) abort() {
	if d.cur != nil {
		d.cur.Outcome.Aborts++
	}
	d.cur = nil
}

// execFailed ends a play whose command did not come out: these inputs do
// not reproduce the action from this state, and whatever the character did
// instead is not one of the move's plays. When the play began right after
// input that could complete another command with the case's own (see
// carryOver) and something else came out, the failure is that leftover
// input's: it counts neither against the case (see Outcome.unreproducible)
// nor against its command's spot. A failure from a clean start, with no
// such input and without the character being hit or guarding, counts
// against the spot (see Verdict).
func (d *driver) execFailed() {
	c := d.cur
	switch {
	case d.carry && d.other:
		d.execNoted = true
		d.carried++
	default:
		d.noteExec(false)
		if c.Command != "" && !d.carry && !d.hit {
			d.st.failAt(d.vkey, d.vpow)
		}
	}
	d.ct.fail(c.ID, d.curStart)
	d.abort()
}

// carryOver works out how many frames of neutral input to put before
// command case c, begun from control, so that input the character gave just
// before cannot complete another command together with the case's own. The
// engine's command buffer keeps recent input: after the base AI walked
// forward, the strong palm's quarter circle forward reads as forward, down,
// down-forward, the Kung Fu Upper's motion, which Kung Fu Man's command file
// checks first. So the canonical motions of the character's commands with a
// button (see Engine.SetMotions) are matched against the input up to the
// case's command (see completes). The input just before is leftover input
// when, followed by the case's own inputs, it completes such a motion of
// another command than the case's, which the case's inputs did not complete
// when recorded, after what the recorded player held before them (its
// lead-in; nothing, for a case with a command's canonical inputs).
//
// The release is the fewest frames that leave no such command complete in
// the frames where another command could still be completing: the last
// MaxMotionFrames, taken as the longest a command's time is, and two more,
// as the engine counts a command's time from its first step, which in a
// motion is mostly letting go of a direction (~F), a frame after it was last
// held, and still takes the last step on the frame after the time is up. So
// measured with the engine's own command matcher: after a walk forward, a
// quarter circle forward with a button reads as an Upper whose command time
// is 15 until the walk's last frame is 17 frames before the button. It is
// at most enough to take all of the input before out of those frames.
// carry reports whether there was leftover input at all, looked for over
// all carryFrames frames kept, as a command's time may be longer. A case
// begun mid-action gets no release, as its inputs go on from the action it
// began in at the frame they were recorded at, but its leftover input is
// found all the same. Nothing is done while the motions are unknown, and
// only with the base AI on, whose input comes just before a move the CBR
// layer begins.
func (d *driver) carryOver(c *Case) (release int, carry bool) {
	if !d.p.BaseAI || len(d.motions) == 0 || c.ExecFrame < 0 || int(c.ExecFrame) >= len(c.Inputs) {
		return 0, false
	}
	w := int(d.p.MaxMotionFrames) + 2
	own := c.Inputs[:c.ExecFrame+1]
	// What came before the case's inputs when it was recorded.
	lead := d.carryLead[:0]
	if c.Command == "" {
		for i, dr := range c.Start.InDir {
			b := dirBits(dr)
			if i < len(c.Start.InBtn) {
				b |= c.Start.InBtn[i]
			}
			lead = append(lead, b)
		}
	}
	lead = append(lead, own...)
	d.carryLead = lead
	// The character's input just before (all the driver keeps), oldest
	// first, then neutral input for as long as the release, then the case's
	// inputs.
	seq := func(release int) []InputBits {
		s := d.carrySeq[:0]
		for age := d.histN; age >= 1; age-- {
			s = append(s, d.hist[(d.histAt-age+carryFrames)%carryFrames])
		}
		for i := 0; i < release; i++ {
			s = append(s, 0)
		}
		s = append(s, own...)
		d.carrySeq = s
		return s
	}
	// Leftover input is looked for over all the frames kept, as a command's
	// time may be longer than MaxMotionFrames: a failure then is put down to
	// it all the same (see execFailed). The release goes by w frames: once it
	// takes all of the input before out of them, what is left completes
	// nothing the recorded case did not.
	if !d.leftover(c, seq(0), lead, carryFrames) {
		return 0, false
	}
	if !c.StartCtrl {
		return 0, true
	}
	for release = 0; release < w-len(own); release++ {
		if !d.leftover(c, seq(release), lead, w) {
			break
		}
	}
	return release, true
}

// leftover reports whether seq, input up to case c's command, completes the
// motion of another command with a button than c's that lead, what came
// before c's inputs when recorded, does not (see carryOver). Commands
// without a button (holding a direction, a dash's double tap) are not moves
// that leftover input could turn the case into.
func (d *driver) leftover(c *Case, seq, lead []InputBits, w int) bool {
	for name, m := range d.motions {
		if name != c.Command && m.hasButton() && completes(m, seq, w) && !completes(m, lead, w) {
			return true
		}
	}
	return false
}

// completes reports whether the command of motion m comes out on the last
// frame of seq, as far as its canonical inputs tell: its directions in order
// among those of the last w frames (repeats, and anything in between,
// ignored) and the buttons of its last frame held on the last frame.
func completes(m Motion, seq []InputBits, w int) bool {
	if len(m.Inputs) == 0 || len(seq) == 0 {
		return false
	}
	if btn := m.Inputs[len(m.Inputs)-1].Buttons(); seq[len(seq)-1]&btn != btn {
		return false
	}
	if len(seq) > w {
		seq = seq[len(seq)-w:]
	}
	i, last := 0, uint8(5)
	for _, b := range m.Inputs {
		dr := b.Dir()
		if dr == 5 || dr == last {
			continue
		}
		last = dr
		for i < len(seq) && seq[i].Dir() != dr {
			i++
		}
		if i == len(seq) {
			return false
		}
		i++
	}
	return true
}

// contest drops the case being played because the character's own AI took
// over from it (see Engine.watchDriven). Unlike an abort, nothing counts
// against the case, and its chain credit is left out, since its inputs were
// not what moved the character. The contest is noted on the record of the
// move's plays in the situation it was played in, so that a move such an AI
// keeps taking over from is not tried again and again (see gateFilter). The
// driver then waits for control to return before choosing again.
func (d *driver) contest() {
	if d.cur == nil {
		return
	}
	d.ct.contest(d.cur.ID)
	if d.p.BaseAI && d.curKey != 0 {
		d.st.contestPlay(d.p, d.cur, d.curKey)
	}
	d.cur = nil
	d.waitCtrl = true
}

// roundEnd clears what lasts only a round.
func (d *driver) roundEnd() {
	d.cur, d.cpCache = nil, nil
	d.explores, d.retries, d.tests = 0, 0, 0
	d.sitSeen, d.waitCtrl, d.spanned = false, false, false
}

// schedule copies a case's inputs and, below execution 8, shifts each combo
// link's press by noise drawn from that link's measured human spread scaled
// by the execution level. The first command press is the decision itself
// and is never shifted.
func (d *driver) schedule(c *Case) ([]InputBits, bool) {
	out := append([]InputBits(nil), c.Inputs...)
	ns := float64(d.p.noiseScale(d.levels.Execution))
	if ns == 0 || c.Kind != KindCommand {
		return out, false
	}
	links := c.Route.Links
	if rs := d.st.Routes[c.Route.Key]; rs != nil {
		links = rs.LinkStats()
	}
	perturbed := false
	press := 0
	var prev InputBits
	for i := 0; i < len(c.Inputs); i++ {
		pr := c.Inputs[i].Presses(prev)
		prev = c.Inputs[i]
		if pr == 0 {
			continue
		}
		idx := int(c.LinkOffset) + press
		press++
		if i <= int(c.ExecFrame) {
			continue
		}
		sd := defaultLinkSD
		if idx < len(links) && links[idx].Variance > 0 {
			sd = float64(links[idx].Variance)
		}
		k := int(math.Round(d.rng.NormFloat64() * sd * ns))
		if k == 0 {
			continue
		}
		hold := 0
		for j := i; j < len(c.Inputs) && c.Inputs[j]&pr == pr; j++ {
			hold++
		}
		for j := i; j < i+hold && j < len(out); j++ {
			out[j] &^= pr
		}
		for j := i + k; j < i+k+hold; j++ {
			if j >= 0 && j < len(out) {
				out[j] |= pr
			}
		}
		perturbed = true
	}
	return out, perturbed
}

func maxi32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
