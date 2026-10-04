package cbr

import (
	"math"
	"math/rand"
)

// The driver plays cases back for a CBR-driven player. It emits one frame
// of input per call, converted to the character's current facing, and
// declines when it has nothing confident so the engine's AI keeps control.

const (
	retryFrames       = 4   // wait after declining before trying again
	betterCheckFrames = 2   // how often an interruptible case is re-evaluated
	execSlackFrames   = 4   // frames past ExecFrame before a missing command aborts
	defaultLinkSD     = 1.5 // timing spread assumed for an unmeasured link
)

type driver struct {
	p      *Params
	player int
	levels Levels
	opp    string
	rng    *rand.Rand
	st     *Store
	ct     *chainTracker

	cur         *Case
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

	cpCache      map[uint64]float32
	lastBest     float32
	lastBestDist float32
}

func newDriver(p *Params, player int, lv Levels, seed int64, st *Store, ct *chainTracker) *driver {
	return &driver{p: p, player: player, levels: lv.clamp(), rng: rand.New(rand.NewSource(seed)), st: st, ct: ct}
}

// observe is called once per logical frame with the frame's result.
func (d *driver) observe(self *CharSnap) {
	// The state must have been entered during the case: a case that repeats
	// the move the character is already in counts only a fresh entry.
	if c := d.cur; c != nil && c.ExecFrame >= 0 && self.StateNo == c.MoveRef && self.StateTime <= int32(d.pos) {
		d.execSeen = true
		d.noteExec(true)
	}
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
		execPending := c.ExecFrame >= 0 && !d.execSeen
		switch {
		case self.BeingHit() && c.StartMoveType != MtHit:
			d.abort()
		case execPending && d.pos > int(c.ExecFrame)+d.shift+execSlackFrames:
			// The command did not come out: these inputs do not reproduce
			// the action from this state.
			d.noteExec(false)
			d.abort()
		case !execPending && d.pos >= len(d.sched):
			d.cur = nil
		case !execPending && self.Ctrl && tick-d.lastCheck >= betterCheckFrames:
			// The recovered better-case check: while the character can act
			// and the case has nothing committed left, switch when another
			// case beats this one, re-scored against the current state, by
			// BetterCaseThreshold.
			d.lastCheck = tick
			if nc, _ := d.choose(cur, self, tick); nc != nil && nc != c && d.lastBest+d.p.BetterCaseThreshold < d.currentScore(cur) {
				d.begin(nc, self, tick)
			}
		}
	}
	if d.cur == nil {
		// Mid-action cases need frame-exact starts, so retry every frame
		// while the character is busy; in neutral a short wait is fine.
		// After a decline, wait a few frames before searching again unless
		// the situation changed; mid-action cases need frame-exact starts,
		// so while busy every frame is tried.
		wait := int64(retryFrames)
		if !self.Ctrl {
			wait = 1
		}
		unchanged := self.StateNo == d.declineNo && self.Ctrl == d.declineCtl
		if d.declined && unchanged && tick-d.lastDecline < wait {
			return 0, false
		}
		c, _ := d.choose(cur, self, tick)
		if c == nil {
			d.lastDecline, d.declined = tick, true
			d.declineNo, d.declineCtl = self.StateNo, self.Ctrl
			return 0, false
		}
		d.declined = false
		d.begin(c, self, tick)
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

func (d *driver) begin(c *Case, self *CharSnap, tick int64) {
	d.cur = c
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
	d.st.markUsed(c.ID, tick)
	d.ct.addCase(chainCase{
		caseID:      c.ID,
		startTick:   tick + int64(d.shift) + int64(maxi32(c.ExecFrame, 0)),
		stateNo:     c.MoveRef,
		isAttack:    c.ExecAttack,
		attackClass: c.ExecClass,
		perturbed:   d.perturbed,
		level:       int8(d.levels.Execution),
	})
}

func (d *driver) abort() {
	if d.cur != nil {
		d.cur.Outcome.Aborts++
	}
	d.cur = nil
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
