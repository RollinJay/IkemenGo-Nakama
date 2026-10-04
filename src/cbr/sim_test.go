package cbr

import (
	"math/rand"
)

// A small deterministic fighting-game simulator for tests. It reproduces the
// mechanics the CBR package depends on: control, attack startup/active/
// recovery, cancels from a connected normal, motion inputs, guarding (with
// low attacks needing a crouching guard), hitstun, blockstun, knockdown and
// wake-up, damage scaling, meter, and a round timer.

type simMove struct {
	no                        int32
	startup, active, recovery int32
	rng                       float32
	dmg                       int32
	hitstun, blockstun        int32
	low, knockdown            bool
	cost                      int32
	next                      int32 // state the move continues into by itself; 0 for none
}

var simMoves = map[int32]simMove{
	200:  {200, 4, 3, 8, 65, 30, 14, 10, false, false, 0, 0},
	400:  {400, 5, 3, 10, 75, 35, 15, 10, true, false, 0, 0},
	1000: {1000, 10, 4, 20, 110, 80, 20, 14, false, true, 0, 0},
	// A two-part special: 1100 continues into 1101 with no input.
	1100: {1100, 6, 3, 2, 70, 40, 16, 10, false, false, 0, 1101},
	1101: {1101, 2, 4, 14, 75, 50, 20, 12, false, true, 0, 0},
	3000: {3000, 5, 6, 30, 120, 250, 30, 20, false, true, 1000, 0},
}

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
	// engine AI's command cheat do; force enters an attack state directly,
	// as ChangeState AI does.
	cmds   []string
	assist string
	force  int32
}

func (c *simChar) setState(no int32, st StateType, mt MoveType, ctrl bool) {
	c.state, c.stateTime, c.st, c.mt, c.ctrl = no, 0, st, mt, ctrl
	c.moveHit, c.moveGuarded, c.contacted = 0, 0, false
}

type sim struct {
	cmdLayer bool
	ch       [2]*simChar
	tick     int64
	time     int32
	maxTime  int32
	round    int32
	roundSt  int32
	winTeam  int32
	combo    [2]int32
	session  uint64
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
func (c *simChar) motion236(n int) bool {
	want := []uint8{2, 3, 6}
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
// assisted one.
func (c *simChar) commands() []string {
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
	if c.assist != "" {
		dup := false
		for _, n := range out {
			dup = dup || n == c.assist
		}
		add(!dup, c.assist)
	}
	return out
}

func (s *sim) control(c, o *simChar) {
	c.cmds = c.commands()
	c.assist = ""
	has := func(n string) bool {
		for _, x := range c.cmds {
			if x == n {
				return true
			}
		}
		return false
	}
	if c.force != 0 {
		if c.ctrl && c.st != StAir {
			s.attack(c, c.force)
		}
		c.force = 0
		return
	}
	canCancel := c.mt == MtAttack && c.state == 200 && c.contacted
	if c.ctrl && c.st != StAir {
		switch {
		case has("qcf_c") && c.power >= 1000:
			s.attack(c, 3000)
		case has("qcf_a"):
			s.attack(c, 1000)
		case has("qcf_b"):
			s.attack(c, 1100)
		case has("a"):
			s.attack(c, 200)
		case has("d_b"):
			s.attack(c, 400)
		case c.input&InU != 0:
			c.setState(40, StAir, MtIdle, false)
			c.vy = -9
		case c.input&InD != 0:
			if c.state != 11 {
				c.setState(11, StCrouch, MtIdle, true)
			}
		case c.input&InF != 0:
			if c.state != 20 {
				c.setState(20, StStand, MtIdle, true)
			}
		case c.input&InB != 0:
			if c.state != 21 {
				c.setState(21, StStand, MtIdle, true)
			}
		default:
			if c.state != 0 {
				c.setState(0, StStand, MtIdle, true)
			}
		}
	} else if canCancel {
		switch {
		case has("qcf_c") && c.power >= 1000:
			s.attack(c, 3000)
		case has("qcf_a"):
			s.attack(c, 1000)
		}
	}
}

func (s *sim) attack(c *simChar, no int32) {
	m := simMoves[no]
	st := StStand
	if m.low {
		st = StCrouch
	}
	c.setState(no, st, MtAttack, false)
	c.power -= m.cost
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
	case c.state == 40:
		c.y += c.vy
		c.vy += 0.6
		if c.y >= 0 {
			c.y, c.vy = 0, 0
			c.setState(52, StStand, MtIdle, false)
		}
	case c.state == 52:
		if c.stateTime >= 3 {
			c.setState(0, StStand, MtIdle, true)
		}
	case c.mt == MtAttack:
		m := simMoves[c.state]
		if c.stateTime >= m.startup+m.active+m.recovery {
			if m.next != 0 {
				s.attack(c, m.next)
			} else {
				c.setState(0, StStand, MtIdle, true)
			}
		}
	case c.state == 5000:
		c.hitstun--
		if c.hitstun <= 0 {
			c.hitstun = 0
			c.setState(0, StStand, MtIdle, true)
		}
	case c.state == 150:
		c.blockstun--
		if c.blockstun <= 0 {
			c.blockstun = 0
			c.setState(0, StStand, MtIdle, true)
		}
	case c.state == 5100:
		c.downTime--
		if c.downTime <= 0 {
			c.setState(5120, StStand, MtIdle, false)
		}
	case c.state == 5120:
		if c.stateTime >= 10 {
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
// means the engine drives that player through eng.Input.
func runRound(s *sim, eng *Engine, ctl [2]controller, frames int) {
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
