package cbr

import (
	"math/rand"
)

// A small deterministic fighting-game simulator for tests. It reproduces the
// mechanics the CBR package depends on: control, attack startup/active/
// recovery, cancels from a connected normal, motion inputs, guarding (with
// low attacks needing a crouching guard), hitstun, blockstun, knockdown and
// wake-up, damage scaling, meter, and a round timer. With sim.entries set it
// also labels each state change as the engine does (CharSnap.Entry), with a
// route table of its commands (simRoutes). Movement beyond walking and a
// plain jump is switched on per sim (see sim.run, sim.dash, sim.dashCancel
// and sim.air): a run with control, a dash without it that a person's route
// may cancel, and jumps with control in the air after their start, with an
// air attack, then a landing without control.

type simMove struct {
	no                        int32
	startup, active, recovery int32
	rng                       float32
	dmg                       int32
	hitstun, blockstun        int32
	low, knockdown            bool
	cost                      int32
	next                      int32 // state the move continues into by itself; 0 for none
	// air is set for an attack made in the air: it lasts until the landing.
	air bool
}

var simMoves = map[int32]simMove{
	200:  {200, 4, 3, 8, 65, 30, 14, 10, false, false, 0, 0, false},
	400:  {400, 5, 3, 10, 75, 35, 15, 10, true, false, 0, 0, false},
	1000: {1000, 10, 4, 20, 110, 80, 20, 14, false, true, 0, 0, false},
	// A two-part special: 1100 continues into 1101 with no input.
	1100: {1100, 6, 3, 2, 70, 40, 16, 10, false, false, 0, 1101, false},
	1101: {1101, 2, 4, 14, 75, 50, 20, 12, false, true, 0, 0, false},
	3000: {3000, 5, 6, 30, 120, 250, 30, 20, false, true, 1000, 0, false},
	// The Upper, which a forward, down, down-forward motion with A enters
	// when sim.upper is on: checked before the quarter circle forward, as
	// Kung Fu Man's command file checks its Kung Fu Upper before the palm.
	1200: {1200, 4, 4, 22, 60, 90, 20, 14, false, true, 0, 0, false},
	// A move no command enters: only a character AI's direct state change
	// reaches it (see changeStater).
	1500: {1500, 3, 3, 10, 70, 70, 20, 12, false, false, 0, 0, false},
	// The air attack, A with control in the air (sim.air).
	600: {600, 3, 8, 0, 55, 60, 22, 14, false, false, 0, 0, true},
}

// The simulator's movement (see sim.run, sim.dash and sim.air): a run goes
// runSpeed pixels a frame while forward is held; a dash goes dashSpeed
// pixels a frame for dashFrames frames without control, and its person's
// cancel route (sim.dashCancel) opens once it has run dashCancelFrom frames;
// a jump has no control for its first jumpStartFrames frames, then control
// in the air (state 50) until it lands. Landing (state 52) takes control
// away for 3 frames, as without the option.
const (
	runSpeed        = 6
	dashSpeed       = 7
	dashFrames      = 14
	dashCancelFrom  = 4
	jumpStartFrames = 3
)

// simRoute is a person's route of the simulator's: the command that enters
// state to, from control on the ground (fromCtrl), or out of a jab that made
// contact (cancel), with at least power. The movement routes need their
// option on (see sim.run, sim.dash, sim.dashCancel and sim.air): run and
// dash start from control on the ground, fromDash leaves a dash once it has
// run dashCancelFrom frames, and inAir starts from control in the air.
type simRoute struct {
	to                         int32
	cmd                        string
	fromCtrl                   bool
	cancel                     bool
	power                      int32
	needsUpperInput            bool
	run, dash, fromDash, inAir bool
}

// simRoutes are the simulator's person routes, as its command layer has them
// (see sim.control).
var simRoutes = []simRoute{
	{to: 3000, cmd: "qcf_c", fromCtrl: true, cancel: true, power: 1000},
	{to: 1200, cmd: "dp_a", fromCtrl: true, needsUpperInput: true},
	{to: 1000, cmd: "qcf_a", fromCtrl: true, cancel: true},
	{to: 1000, cmd: "qcf_a", fromDash: true},
	{to: 1100, cmd: "qcf_b", fromCtrl: true},
	{to: 105, cmd: "ff", fromCtrl: true, dash: true},
	{to: 100, cmd: "ff", fromCtrl: true, run: true},
	{to: 200, cmd: "a", fromCtrl: true},
	{to: 400, cmd: "d_b", fromCtrl: true},
	{to: 600, cmd: "a", inAir: true},
}

// simRouteTable is simRoutes as the engine reports a route table: each
// state's commands once, in the order of the routes.
var simRouteTable = func() *RouteTable {
	t := &RouteTable{Person: map[int32][]string{}}
	for _, r := range simRoutes {
		if !hasCommand(t.Person[r.to], r.cmd) {
			t.Person[r.to] = append(t.Person[r.to], r.cmd)
		}
	}
	return t
}()

// simMotionsMove is simMotions with the double tap forward that runs or
// dashes (see sim.run and sim.dash).
var simMotionsMove = func() map[string]Motion {
	m := map[string]Motion{"ff": {[]InputBits{InF, 0, InF}, 2}}
	for k, v := range simMotions {
		m[k] = v
	}
	return m
}()

// simMotionsUpper is simMotions with the Upper's command (see sim.upper).
var simMotionsUpper = func() map[string]Motion {
	m := map[string]Motion{"dp_a": {[]InputBits{InF, InD, InD | InF | InA}, 4}}
	for k, v := range simMotions {
		m[k] = v
	}
	return m
}()

// simMotions are the canonical inputs of the simulator's commands, as the
// adapter would synthesize them from a character's command definitions.
var simMotions = map[string]Motion{
	"a":     {[]InputBits{InA}, 1},
	"b":     {[]InputBits{InBtnB}, 1},
	"c":     {[]InputBits{InC}, 1},
	"d_b":   {[]InputBits{InD | InBtnB}, 1},
	"qcf_a": {[]InputBits{InD, InD | InF, InF | InA}, 4},
	"qcf_b": {[]InputBits{InD, InD | InF, InF | InBtnB}, 4},
	"qcf_c": {[]InputBits{InD, InD | InF, InF | InC}, 4},
}

type simChar struct {
	x, y, vy    float32
	vx          float32 // forward speed of a jump (sim.air)
	facingRight bool
	state       int32
	stateTime   int32
	st          StateType
	mt          MoveType
	ctrl        bool
	life, power int32
	hitstun     int32
	blockstun   int32
	moveHit     int32
	moveGuarded int32
	contacted   bool
	downTime    int32
	team        int
	hist        []InputBits // facing-relative input history, newest last
	input       InputBits
	cbr         bool
	ai          float32

	// Command layer (sim.cmdLayer): cmds are this frame's active commands;
	// assist activates a command with no input, as AssertCommand or the
	// engine AI's command cheat do; force enters an attack state directly
	// from control, as ChangeState AI does; cancelTo does the same from an
	// attack that connected, without control, as ChangeState AI cancelling
	// its own move does. movesOff keeps commands from starting moves (they
	// still show as active), as a character whose move code people use is
	// switched off while its own AI is on; walking, jumping and guarding,
	// which the engine does itself, still work.
	cmds     []string
	assist   string
	force    int32
	cancelTo int32
	movesOff bool
	// cheated is the command assist activated this frame with no input that
	// completes it, if any: a route it fires is the AI's decision.
	cheated string

	// The label of the state change shown on this frame (sim.entries), as
	// the engine gives it (CharSnap.Entry); EntryNone when the state did not
	// change.
	entry       EntryKind
	entryCmd    string
	entryHelper bool

	// redirect enters an attack state from control as force does, but the
	// way AI in a helper does it, with a ChangeState redirected to the
	// character: the helper runs after the character's own frame, so the
	// state number changes at the end of the frame while control and move
	// type stay as they were, and the state takes effect (pending) at the
	// start of the character's next frame.
	redirect int32
	pending  int32
}

func (c *simChar) setState(no int32, st StateType, mt MoveType, ctrl bool) {
	c.state, c.stateTime, c.st, c.mt, c.ctrl = no, 0, st, mt, ctrl
	c.moveHit, c.moveGuarded, c.contacted = 0, 0, false
}

// label labels the state change being made (see sim.entries).
func (c *simChar) label(k EntryKind, cmd string, helper bool) {
	c.entry, c.entryCmd, c.entryHelper = k, cmd, helper
}

// open reports whether route r would fire for c now with its command
// pressed, in s.
func (r *simRoute) open(s *sim, c *simChar) bool {
	if c.movesOff || r.needsUpperInput && !s.upper || c.power < r.power ||
		r.run && (!s.run || s.dash) || r.dash && !s.dash {
		return false
	}
	switch {
	case r.fromDash:
		return s.dashCancel && c.state == 105 && c.stateTime >= dashCancelFrom
	case r.inAir:
		return s.air && c.ctrl && c.st == StAir
	}
	fromCtrl := c.ctrl && c.st != StAir
	cancel := c.mt == MtAttack && c.state == 200 && c.contacted
	return r.fromCtrl && fromCtrl || r.cancel && cancel
}

// moment is the engine's moment check of a change into state no that the
// character's own AI decides now: whether a person's route into no would
// fire now with its command pressed (EntryReachable, with the command), or
// none would (EntryCheat).
func (s *sim) moment(c *simChar, no int32) (EntryKind, string) {
	for i := range simRoutes {
		if r := &simRoutes[i]; r.to == no && r.open(s, c) {
			return EntryReachable, r.cmd
		}
	}
	return EntryCheat, ""
}

// decided labels a change into no the character's own AI makes now by
// changing the state directly (see moment).
func (s *sim) decided(c *simChar, no int32, helper bool) {
	k, cmd := s.moment(c, no)
	c.label(k, cmd, helper)
}

type sim struct {
	cmdLayer bool
	// entries labels the state changes as the engine does (see simRoutes);
	// upper switches on the Upper's command (see simMoves), whose window
	// upperWindow is 12 frames when 0.
	entries     bool
	upper       bool
	upperWindow int
	ch          [2]*simChar
	tick        int64
	time        int32
	maxTime     int32
	round       int32
	roundSt     int32
	winTeam     int32
	combo       [2]int32
	session     uint64

	// Movement: with run, a double tap forward on the ground with control
	// (the "ff" command, forward, neutral, forward within 10 frames) runs
	// with control (state 100) while forward is held; with dash it dashes
	// without control instead (state 105, dashFrames frames), and with
	// dashCancel too a person's route cancels the dash into the special
	// with the quarter circle forward and A once it has run dashCancelFrom
	// frames. With air, a jump (holding up, with forward or back for its
	// direction) has control in the air after its first jumpStartFrames
	// frames (state 50), where A is the air attack (600); being hit in the
	// air puts the character down where it is.
	run, dash, dashCancel, air bool
}

func newSim(maxTime int32) *sim {
	s := &sim{maxTime: maxTime, time: maxTime, round: 1, roundSt: 2, winTeam: -1, session: 1}
	s.ch[0] = &simChar{x: -80, facingRight: true, life: 1000, team: 0}
	s.ch[1] = &simChar{x: 80, facingRight: false, life: 1000, team: 1}
	for _, c := range s.ch {
		c.setState(0, StStand, MtIdle, true)
	}
	return s
}

// motion reports whether the history ends with a 2,3,6 motion within n
// frames (facing-relative), ignoring repeats.
func (c *simChar) motion236(n int) bool { return c.motion(n, 2, 3, 6) }

// motion reports whether the history ends with the directions want, in
// order, within n frames (facing-relative), ignoring repeats and anything
// in between.
func (c *simChar) motion(n int, want ...uint8) bool {
	k := len(want) - 1
	for i := len(c.hist) - 1; i >= 0 && i >= len(c.hist)-n; i-- {
		d := c.hist[i].Dir()
		if d == want[k] {
			k--
			if k < 0 {
				return true
			}
		}
	}
	return false
}

func (c *simChar) pressed(b InputBits) bool {
	if len(c.hist) < 2 {
		return c.input&b != 0
	}
	return c.input&b != 0 && c.hist[len(c.hist)-2]&b == 0
}

func (s *sim) step(in [2]InputBits) {
	if s.roundSt != 2 {
		return
	}
	s.tick++
	for i, c := range s.ch {
		c.label(EntryNone, "", false)
		c.input = in[i]
		c.hist = append(c.hist, in[i])
		if len(c.hist) > 30 {
			c.hist = c.hist[1:]
		}
	}
	for i, c := range s.ch {
		s.control(c, s.ch[1-i])
	}
	for i, c := range s.ch {
		s.advance(c, s.ch[1-i])
	}
	for _, c := range s.ch {
		if no := c.redirect; no != 0 {
			c.redirect = 0
			if c.ctrl && c.st != StAir {
				s.decided(c, no, true)
				c.state, c.stateTime, c.pending = no, 0, no
			}
		}
	}
	// Resolve both attacks against the pre-hit state so simultaneous hits
	// trade instead of the first-processed player winning.
	a0, a1 := s.hits(s.ch[0], s.ch[1]), s.hits(s.ch[1], s.ch[0])
	if a0 {
		s.collide(s.ch[0], s.ch[1], 0)
	}
	if a1 {
		s.collide(s.ch[1], s.ch[0], 1)
	}
	// Facing.
	for i, c := range s.ch {
		o := s.ch[1-i]
		if c.ctrl && c.st != StAir {
			c.facingRight = o.x > c.x
		}
	}
	if s.time > 0 {
		s.time--
	}
	for i, c := range s.ch {
		if c.life <= 0 {
			c.life = 0
			s.roundSt, s.winTeam = 3, int32(1-i)
		}
	}
	if s.roundSt == 2 && s.time == 0 {
		s.roundSt = 3
		switch {
		case s.ch[0].life > s.ch[1].life:
			s.winTeam = 0
		case s.ch[1].life > s.ch[0].life:
			s.winTeam = 1
		}
	}
}

// commands lists the commands this frame's input completes, plus an
// assisted one. With s.upper it adds the Upper's command, with its window,
// and with s.run or s.dash the double tap forward.
func (c *simChar) commands(s *sim) []string {
	var out []string
	add := func(ok bool, n string) {
		if ok {
			out = append(out, n)
		}
	}
	m := c.motion236(12)
	add(c.pressed(InA), "a")
	add(c.pressed(InBtnB), "b")
	add(c.pressed(InC), "c")
	add(c.pressed(InBtnB) && c.input&InD != 0, "d_b")
	add(c.pressed(InA) && m, "qcf_a")
	add(c.pressed(InBtnB) && m, "qcf_b")
	add(c.pressed(InC) && m, "qcf_c")
	if s.upper {
		w := s.upperWindow
		if w == 0 {
			w = 12
		}
		add(c.pressed(InA) && c.motion(w, 6, 2, 3), "dp_a")
	}
	if s.run || s.dash {
		add(c.pressed(InF) && c.input&(InU|InD|InB) == 0 && c.motion(10, 6, 5, 6), "ff")
	}
	c.cheated = ""
	if c.assist != "" {
		dup := false
		for _, n := range out {
			dup = dup || n == c.assist
		}
		add(!dup, c.assist)
		if !dup {
			c.cheated = c.assist
		}
	}
	return out
}

func (s *sim) control(c, o *simChar) {
	c.cmds = c.commands(s)
	c.assist = ""
	if no := c.pending; no != 0 {
		c.pending = 0
		if c.state == no { // unless a hit came first
			// The helper's change, labelled when it was made, takes effect.
			s.attack(c, no)
			return
		}
	}
	// fire enters state no through the person's route with command n: the
	// input's, or the AI's when only its assist activated n.
	fire := func(no int32, n string) {
		if n == c.cheated {
			c.label(EntryReachable, n, false)
		} else {
			c.label(EntryInput, n, false)
		}
		s.attack(c, no)
	}
	has := func(n string) bool {
		if c.movesOff {
			return false
		}
		for _, x := range c.cmds {
			if x == n {
				return true
			}
		}
		return false
	}
	if c.force != 0 {
		if c.ctrl && c.st != StAir {
			s.decided(c, c.force, false)
			s.attack(c, c.force)
		}
		c.force = 0
		return
	}
	if no := c.cancelTo; no != 0 {
		c.cancelTo = 0
		if c.mt == MtAttack && c.contacted && c.state != no {
			s.decided(c, no, false)
			s.attack(c, no)
			return
		}
	}
	canCancel := c.mt == MtAttack && c.state == 200 && c.contacted
	if c.ctrl && c.st != StAir {
		switch {
		case has("qcf_c") && c.power >= 1000:
			fire(3000, "qcf_c")
		case s.upper && has("dp_a"):
			fire(1200, "dp_a")
		case has("qcf_a"):
			fire(1000, "qcf_a")
		case has("qcf_b"):
			fire(1100, "qcf_b")
		case s.dash && has("ff"):
			c.label(EntryInput, "ff", false)
			c.setState(105, StStand, MtIdle, false)
		case s.run && has("ff"):
			c.label(EntryInput, "ff", false)
			c.setState(100, StStand, MtIdle, true)
		case has("a"):
			fire(200, "a")
		case has("d_b"):
			fire(400, "d_b")
		case c.input&InU != 0:
			c.label(EntryInput, "", false)
			c.setState(40, StAir, MtIdle, false)
			c.vy = -9
			if s.air {
				switch {
				case c.input&InF != 0:
					c.vx = 3
				case c.input&InB != 0:
					c.vx = -2
				}
			}
		case c.input&InD != 0:
			if c.state != 11 {
				c.label(EntryInput, "", false)
				c.setState(11, StCrouch, MtIdle, true)
			}
		case c.input&InF != 0:
			// Holding forward keeps a run going; otherwise it walks.
			if c.state != 20 && c.state != 100 {
				c.label(EntryInput, "", false)
				c.setState(20, StStand, MtIdle, true)
			}
		case c.input&InB != 0:
			if c.state != 21 {
				c.label(EntryInput, "", false)
				c.setState(21, StStand, MtIdle, true)
			}
		default:
			if c.state != 0 {
				c.label(EntryAuto, "", false)
				c.setState(0, StStand, MtIdle, true)
			}
		}
	} else if canCancel {
		switch {
		case has("qcf_c") && c.power >= 1000:
			fire(3000, "qcf_c")
		case has("qcf_a"):
			fire(1000, "qcf_a")
		}
	} else if s.air && c.ctrl && c.st == StAir {
		if has("a") {
			fire(600, "a")
		}
	} else if s.dashCancel && c.state == 105 && c.stateTime >= dashCancelFrom {
		if has("qcf_a") {
			fire(1000, "qcf_a")
		}
	}
}

func (s *sim) attack(c *simChar, no int32) {
	m := simMoves[no]
	st := StStand
	switch {
	case m.low:
		st = StCrouch
	case m.air:
		st = StAir
	}
	c.setState(no, st, MtAttack, false)
	// A character AI's direct state change into a move it lacks the power
	// for leaves it with none, as the engine's power add does.
	c.power = max(c.power-m.cost, 0)
}

func (s *sim) advance(c, o *simChar) {
	c.stateTime++
	if c.moveHit > 0 {
		c.moveHit++
	}
	if c.moveGuarded > 0 {
		c.moveGuarded++
	}
	dir := float32(1)
	if !c.facingRight {
		dir = -1
	}
	switch {
	case c.state == 20:
		c.x += 3 * dir
	case c.state == 21:
		c.x -= 2 * dir
	case c.state == 100:
		c.x += runSpeed * dir
	case c.state == 105:
		c.x += dashSpeed * dir
		if c.stateTime >= dashFrames {
			c.label(EntryAuto, "", false)
			c.setState(0, StStand, MtIdle, true)
		}
	case c.state == 40 || c.state == 50 || c.st == StAir && simMoves[c.state].air:
		// In the air: a jump, with control after its start when sim.air is
		// on (state 50), or an air attack, until the landing.
		c.x += c.vx * dir
		c.y += c.vy
		c.vy += 0.6
		switch {
		case c.y >= 0:
			c.y, c.vy, c.vx = 0, 0, 0
			c.label(EntryAuto, "", false)
			c.setState(52, StStand, MtIdle, false)
		case s.air && c.state == 40 && c.stateTime >= jumpStartFrames:
			c.label(EntryAuto, "", false)
			c.setState(50, StAir, MtIdle, true)
		}
	case c.state == 52:
		if c.stateTime >= 3 {
			c.label(EntryAuto, "", false)
			c.setState(0, StStand, MtIdle, true)
		}
	case c.mt == MtAttack:
		m := simMoves[c.state]
		if c.stateTime >= m.startup+m.active+m.recovery {
			if m.next != 0 {
				// A follow-up the move makes itself in the frame it was
				// entered keeps the entry's label, as the engine's does.
				if c.entry == EntryNone {
					c.label(EntryAuto, "", false)
				}
				s.attack(c, m.next)
			} else {
				c.label(EntryAuto, "", false)
				c.setState(0, StStand, MtIdle, true)
			}
		}
	case c.state == 5000:
		c.hitstun--
		if c.hitstun <= 0 {
			c.hitstun = 0
			c.label(EntryAuto, "", false)
			c.setState(0, StStand, MtIdle, true)
		}
	case c.state == 150:
		c.blockstun--
		if c.blockstun <= 0 {
			c.blockstun = 0
			c.label(EntryAuto, "", false)
			c.setState(0, StStand, MtIdle, true)
		}
	case c.state == 5100:
		c.downTime--
		if c.downTime <= 0 {
			c.label(EntryAuto, "", false)
			c.setState(5120, StStand, MtIdle, false)
		}
	case c.state == 5120:
		if c.stateTime >= 10 {
			c.label(EntryAuto, "", false)
			c.setState(0, StStand, MtIdle, true)
		}
	}
	if c.x < -300 {
		c.x = -300
	}
	if c.x > 300 {
		c.x = 300
	}
}

// hits reports whether a's attack reaches d this frame.
func (s *sim) hits(a, d *simChar) bool {
	if a.mt != MtAttack || a.contacted {
		return false
	}
	m := simMoves[a.state]
	t := a.stateTime
	if t < m.startup || t >= m.startup+m.active {
		return false
	}
	dir := float32(1)
	if !a.facingRight {
		dir = -1
	}
	dist := (d.x - a.x) * dir
	return dist > 0 && dist <= m.rng && d.state != 5120 && d.state != 5100
}

func (s *sim) collide(a, d *simChar, ai int) {
	m := simMoves[a.state]
	dir := float32(1)
	if !a.facingRight {
		dir = -1
	}
	a.contacted = true
	guarding := d.st != StAir && (d.ctrl || d.state == 150) && d.input&InB != 0 &&
		(!m.low || d.input&InD != 0)
	d.label(EntryAuto, "", false)
	if s.air && d.st == StAir {
		d.y, d.vy, d.vx = 0, 0, 0 // hit out of the air: down where it is
	}
	if guarding {
		d.setState(150, StStand, MtHit, false)
		d.blockstun = m.blockstun
		a.moveGuarded = 1
		d.x += 6 * dir
		return
	}
	scale := 1 - 0.1*float32(s.combo[ai])
	if scale < 0.5 {
		scale = 0.5
	}
	d.life -= int32(float32(m.dmg) * scale)
	a.moveHit = 1
	a.power += 50
	if d.state == 5000 {
		s.combo[ai]++
	} else {
		s.combo[ai] = 1
	}
	if m.knockdown {
		d.setState(5100, StLying, MtHit, false)
		d.downTime = 40
		d.hitstun = 0
	} else {
		d.setState(5000, StStand, MtHit, false)
		d.hitstun = m.hitstun
	}
	d.x += 8 * dir
}

func (s *sim) snapshot() *Snapshot {
	out := &Snapshot{World: WorldSnap{
		Tick: s.tick, RoundNo: s.round, RoundState: s.roundSt,
		CurRoundTime: s.time, MaxRoundTime: s.maxTime,
		StageLeft: -300, StageRight: 300,
		ComboCount: s.combo, WinTeam: s.winTeam, Session: s.session,
	}}
	for i, c := range s.ch {
		out.Players = append(out.Players, PlayerSnap{Present: true, Name: "sim", Root: CharSnap{
			ID: int32(i + 1), PlayerNo: i, TeamSide: c.team,
			Pos: [3]float32{c.x, c.y, 0}, FacingRight: c.facingRight,
			StateNo: c.state, StateTime: c.stateTime, StateType: c.st, MoveType: c.mt, Ctrl: c.ctrl,
			HitStun: c.hitstun, BlockStun: c.blockstun,
			MoveHit: c.moveHit, MoveGuarded: c.moveGuarded,
			Life: c.life, LifeMax: 1000, Power: c.power, PowerMax: 3000,
			Input: c.input, CBRDriven: c.cbr, AILevel: c.ai, Alive: c.life > 0,
			HitboxActive: c.mt == MtAttack,
		}})
		if s.cmdLayer {
			r := &out.Players[i].Root
			r.Commands, r.CmdKnown = append([]string(nil), c.cmds...), true
		}
		if s.entries {
			r := &out.Players[i].Root
			r.Entry, r.EntryCommand, r.EntryHelper, r.Routes = c.entry, c.entryCmd, c.entryHelper, simRouteTable
		}
	}
	return out
}

// Scripted controllers stand in for a human or an engine AI.

type controller interface {
	next(s *sim, me int) InputBits
}

// rushdown approaches, pokes, and cancels a connected poke into the
// special, ending in a knockdown. Occasionally sweeps.
type rushdown struct {
	rng  *rand.Rand
	plan []InputBits
}

func (r *rushdown) next(s *sim, me int) InputBits {
	if len(r.plan) > 0 {
		b := r.plan[0]
		r.plan = r.plan[1:]
		return b
	}
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	if c.state == 200 && c.contacted && c.moveHit > 0 {
		r.plan = []InputBits{InD, InD | InF, InF | InA}
		b := r.plan[0]
		r.plan = r.plan[1:]
		return b
	}
	if !c.ctrl {
		return 0
	}
	switch {
	case o.state == 5100 || o.state == 5120:
		if dist > 50 {
			return InF
		}
		return 0
	case dist > 62:
		return InF
	case r.rng.Intn(6) == 0:
		return InD | InBtnB
	default:
		// Release then press so the press registers.
		if c.input&InA != 0 {
			return 0
		}
		return InA
	}
}

// poker is a weak player: it walks in and throws out single jabs at
// random, and never follows one up.
type poker struct{ rng *rand.Rand }

func (p *poker) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	if !c.ctrl {
		return 0
	}
	switch {
	case absf(o.x-c.x) > 60:
		return InF
	case p.rng.Intn(6) == 0:
		if c.input&InA != 0 {
			return 0
		}
		return InA
	}
	return 0
}

// defender guards when the opponent attacks nearby, pokes back at times,
// and drifts.
type defender struct{ rng *rand.Rand }

func (d *defender) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	if o.mt == MtAttack && dist < 140 {
		if o.st == StCrouch {
			return InB | InD
		}
		return InB
	}
	if !c.ctrl {
		return InB
	}
	switch r := d.rng.Intn(100); {
	case r < 4 && dist < 65:
		if c.input&InA != 0 {
			return 0
		}
		return InA
	case r < 30:
		return InB
	case r < 45:
		return InF
	}
	return 0
}

// runRound plays one round with the given controllers. A nil controller
// means the engine drives that player through eng.Input, and the player
// idles on frames the CBR layer declines.
func runRound(s *sim, eng *Engine, ctl [2]controller, frames int) {
	runRoundBase(s, eng, ctl, [2]controller{}, frames)
}

// contender is a base AI that the AILevel mask cannot pause, as an older AI
// switched on by a long command: contend runs on the frames the CBR layer
// gives the input, and may still change the character's state.
type contender interface {
	controller
	contend(s *sim, me int)
}

// runRoundBase is runRound with a base AI for each player the engine
// drives: base[i] plays player i on every frame the CBR layer declines, as
// the character's own AI does, and is not called on frames the CBR layer
// gives the input, as the AILevel mask pauses that AI (a contender still
// contends there). It returns how many frames the CBR layer drove each
// player.
func runRoundBase(s *sim, eng *Engine, ctl, base [2]controller, frames int) (driven [2]int) {
	if eng != nil {
		eng.Frame(s.snapshot())
	}
	for f := 0; f < frames && s.roundSt == 2; f++ {
		var in [2]InputBits
		for i := 0; i < 2; i++ {
			s.ch[i].cbr = false
			if ctl[i] != nil {
				in[i] = ctl[i].next(s, i)
				continue
			}
			if btn, ok := eng.Input(i, s.ch[i].facingRight); ok {
				in[i] = FromEngine(btn, s.ch[i].facingRight)
				s.ch[i].cbr = true
				driven[i]++
				if c, ok := base[i].(contender); ok {
					c.contend(s, i)
				}
			} else if base[i] != nil {
				in[i] = base[i].next(s, i)
			}
		}
		s.step(in)
		if eng != nil {
			eng.Frame(s.snapshot())
		}
	}
	if eng != nil {
		if s.roundSt == 2 {
			s.roundSt = 3
		}
		eng.RoundEnd(s.snapshot())
	}
	return driven
}

// lowBlind guards high attacks but never guards low: an exploitable hole
// that value learning should discover.
type lowBlind struct{ rng *rand.Rand }

func (d *lowBlind) next(s *sim, me int) InputBits {
	c, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - c.x)
	if o.mt == MtAttack && dist < 140 {
		return InB
	}
	if !c.ctrl {
		return InB
	}
	switch r := d.rng.Intn(100); {
	case r < 4 && dist < 65:
		if c.input&InA != 0 {
			return 0
		}
		return InA
	case r < 30:
		return InB
	case r < 45:
		return InF
	}
	return 0
}

// jammer holds random directions for random spans, like the engine AI's
// input jamming.
type jammer struct {
	dir InputBits
	t   int
}

func (j *jammer) next(rng *rand.Rand) InputBits {
	if j.t <= 0 {
		j.t = 1 + rng.Intn(20)
		dirs := []InputBits{0, 0, InB, InF, InD, InD | InB}
		j.dir = dirs[rng.Intn(len(dirs))]
	}
	j.t--
	return j.dir
}

// cheater stands in for the engine AI: it jams directions and activates
// the special's command with no motion when close, like Ikemen's AI command
// cheat or AssertCommand.
type cheater struct {
	rng *rand.Rand
	jam jammer
}

func (c *cheater) next(s *sim, me int) InputBits {
	ch, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - ch.x)
	if ch.ctrl && dist > 90 {
		return InF
	}
	if ch.ctrl && dist <= 75 && c.rng.Intn(6) == 0 {
		ch.assist = "qcf_a"
	}
	return c.jam.next(c.rng)
}

// stateChanger stands in for ChangeState AI: it enters the special's state
// directly, with no command.
type stateChanger struct {
	rng *rand.Rand
	jam jammer
}

func (c *stateChanger) next(s *sim, me int) InputBits {
	ch, o := s.ch[me], s.ch[1-me]
	dist := absf(o.x - ch.x)
	if ch.ctrl && dist > 90 {
		return InF
	}
	if ch.ctrl && dist <= 75 && c.rng.Intn(6) == 0 {
		ch.force = 1000
	}
	return c.jam.next(c.rng)
}

// switchedOn is a stateChanger that ignores the AILevel mask: it also
// forces the special on frames the CBR layer gives the input.
type switchedOn struct{ stateChanger }

func (c *switchedOn) contend(s *sim, me int) {
	ch, o := s.ch[me], s.ch[1-me]
	if ch.ctrl && absf(o.x-ch.x) <= 75 && c.rng.Intn(6) == 0 {
		ch.force = 1000
	}
}

// Base AIs for the kinds of character AI (see aikinds_test.go). A pausable
// one plays only on the frames the CBR layer leaves to it, as an AI written
// against AILevel does. An unpausable one (a contender) also acts on the
// frames the CBR layer gives the input, as an AI that keeps itself on in a
// variable does.

// latched keeps itself on in a variable, set once it saw AILevel above 0 or
// from AILevel never being read at all (IsHomeTeam, TeamSide): a stateChanger
// the AILevel mask cannot pause. With movesOff, the moves people input are
// switched off while it is on, so the CBR layer's commands start nothing.
type latched struct {
	stateChanger
	movesOff bool
}

func (c *latched) next(s *sim, me int) InputBits {
	s.ch[me].movesOff = c.movesOff
	return c.stateChanger.next(s, me)
}

func (c *latched) contend(s *sim, me int) {
	ch, o := s.ch[me], s.ch[1-me]
	ch.movesOff = c.movesOff
	if ch.ctrl && absf(o.x-ch.x) <= 75 && c.rng.Intn(6) == 0 {
		ch.force = 1000
	}
}

// switchCode is an older AI that a long command switches on. The engine
// AI's command cheat picks that command now and then, but only on frames the
// base AI plays, since the cheat does not fire on the CBR layer's frames.
// Until then the character plays as the engine AI (cheater), and from then
// on it is latched.
type switchCode struct {
	engine cheater
	on     latched
	onTick int64 // the frame it switched on; 0 while it is off
}

func (c *switchCode) next(s *sim, me int) InputBits {
	if c.onTick == 0 && c.engine.rng.Intn(30) == 0 {
		c.onTick = s.tick + 1
	}
	if c.onTick == 0 {
		return c.engine.next(s, me)
	}
	return c.on.next(s, me)
}

func (c *switchCode) contend(s *sim, me int) {
	if c.onTick != 0 {
		c.on.contend(s, me)
	}
}

// switchCodeOff is a switch-code AI that also switches itself off whenever
// AILevel reads 0, as on the CBR layer's frames, and on again only through
// its switch code: after each take-over the character plays as the engine
// AI until the cheat picks the switch code again.
type switchCodeOff struct{ switchCode }

func (c *switchCodeOff) contend(*sim, int) { c.onTick = 0 }

// cheatOnly has no AI of its own and turns the engine AI's button jam off
// (NoAIButtonJam), so it never walks: it acts only when the engine AI's
// command cheat picks one of its motion commands. The cheat never picks a
// single-button command for a character's root.
type cheatOnly struct{ rng *rand.Rand }

func (c *cheatOnly) next(s *sim, me int) InputBits {
	if ch := s.ch[me]; ch.ctrl && c.rng.Intn(25) == 0 {
		ch.assist = [...]string{"qcf_a", "qcf_b", "d_b"}[c.rng.Intn(3)]
	}
	return 0
}

// engineAI stands in for a character with no AI of its own, played by the
// engine AI: the button jam holds random directions and now and then a
// button, and the command cheat picks one of its motion commands at random.
type engineAI struct {
	rng  *rand.Rand
	jam  jammer
	held int // frames the jammed button is still held
}

func (c *engineAI) next(s *sim, me int) InputBits {
	if ch := s.ch[me]; ch.ctrl && c.rng.Intn(20) == 0 {
		ch.assist = [...]string{"qcf_a", "qcf_b", "d_b"}[c.rng.Intn(3)]
	}
	in := c.jam.next(c.rng)
	switch {
	case c.held > 0:
		c.held--
		in |= InA
	case c.rng.Intn(40) == 0:
		c.held = 1 + c.rng.Intn(5)
	}
	return in
}

// helperAI is a stateChanger that runs in a helper and moves its root with
// a redirected ChangeState, reading root,AILevel, so it is paused on the CBR
// layer's frames: its attacks start a frame late (see simChar.redirect).
type helperAI struct{ stateChanger }

func (c *helperAI) next(s *sim, me int) InputBits {
	in := c.stateChanger.next(s, me)
	if ch := s.ch[me]; ch.force != 0 {
		ch.redirect, ch.force = ch.force, 0
	}
	return in
}

// canceller keeps itself on, as latched does, and cancels its own jab into
// the two-part special by changing state directly when the jab hits, without
// control. It does so during the CBR layer's moves too, where taking over
// mid-move is not detected (see Engine.watchDriven).
type canceller struct{ rng *rand.Rand }

func (c *canceller) next(s *sim, me int) InputBits {
	c.contend(s, me)
	ch, o := s.ch[me], s.ch[1-me]
	switch {
	case !ch.ctrl:
		return 0
	case absf(o.x-ch.x) > 62:
		return InF
	case ch.input&InA != 0 || c.rng.Intn(4) != 0:
		return 0
	}
	return InA
}

func (c *canceller) contend(s *sim, me int) {
	if ch := s.ch[me]; ch.state == 200 && ch.contacted && ch.moveHit > 0 {
		ch.cancelTo = 1100
	}
}

// twoPart is a human-like player who approaches and throws out the
// two-part special with its motion.
type twoPart struct {
	rng  *rand.Rand
	plan []InputBits
}

func (r *twoPart) next(s *sim, me int) InputBits {
	if len(r.plan) > 0 {
		b := r.plan[0]
		r.plan = r.plan[1:]
		return b
	}
	c, o := s.ch[me], s.ch[1-me]
	if !c.ctrl {
		return 0
	}
	if absf(o.x-c.x) > 70 {
		return InF
	}
	if r.rng.Intn(8) == 0 {
		r.plan = []InputBits{InD | InF, InF | InBtnB, 0}
		return InD
	}
	return 0
}

// changeStater is ChangeState AI with every kind of move the engine's labels
// tell apart (see sim.entries), paused on the CBR layer's frames: it walks
// in and, close to the enemy, enters attack states directly, with no
// command. A person's command reaches the jab, the low kick and the special
// from control there (EntryReachable); none reaches the AI-only move 1500,
// nor the super without a bar of power, which the AI uses either way
// (EntryCheat then). When its jab makes contact it may cancel it into the
// special, which a person's cancel reaches too, or into the two-part
// special, which none does.
type changeStater struct{ rng *rand.Rand }

func (c *changeStater) next(s *sim, me int) InputBits {
	ch, o := s.ch[me], s.ch[1-me]
	if ch.state == 200 && ch.contacted && ch.stateTime == 8 && c.rng.Intn(2) == 0 {
		ch.cancelTo = [...]int32{1000, 1100}[c.rng.Intn(2)]
		return 0
	}
	if !ch.ctrl {
		return 0
	}
	if absf(o.x-ch.x) > 55 {
		return InF
	}
	if c.rng.Intn(5) == 0 {
		switch r := c.rng.Intn(20); {
		case r < 7:
			ch.force = 200
		case r < 11:
			ch.force = 400
		case r < 14:
			ch.force = 1000
		case r < 17:
			ch.force = 1500
		default:
			ch.force = 3000
		}
	}
	return 0
}

// walkIn walks forward until close to the enemy, then waits: the base AI of
// the leftover-input test, whose walk ends where the CBR layer takes over.
type walkIn struct{}

func (walkIn) next(s *sim, me int) InputBits {
	if ch, o := s.ch[me], s.ch[1-me]; ch.ctrl && absf(o.x-ch.x) > 55 {
		return InF
	}
	return 0
}
