package cbr

import "sort"

// CharFeat is one character's comparison features, expressed in the focus
// character's frame: origin at the focus root, +x toward the focus front.
type CharFeat struct {
	Present      bool
	RelX, RelY   float32
	VelX, VelY   float32
	State        StateType
	Move         MoveType
	Ctrl         bool
	HitStun      int32
	BlockStun    int32
	StateNo      int32
	StateTime    int32
	LifeFrac     float32
	PowerFrac    float32
	HitboxActive bool
}

// Feat is the comparison state of one frame from one player's point of view.
type Feat struct {
	Self, Enemy  CharFeat
	Allies       []CharFeat
	SelfHelpers  []CharFeat
	EnemyHelpers []CharFeat

	InDir []uint8     // recent directions, oldest first
	InBtn []InputBits // recent buttons, oldest first

	Wall       int8 // 0 none; -1 focus has its back to a wall; +1 enemy has its back to a wall
	RoundState int32
	TimeFrac   float32 // -1 when the timer is infinite
	LifeLead   float32 // focus life fraction minus enemy life fraction
	FrameAdv   int32
	Initiator  int8 // +1 focus started the current exchange, -1 enemy, 0 neither
	ComboMoves int32
	Pressure   bool
	GotHit     bool
	DidHit     bool
	Contact    int8 // the focus's current attack: 0 no contact, 1 hit, 2 guarded
	Order      uint64
	OrderLen   uint8
	Vars       []float32
}

// tracker keeps per-player history needed for features that depend on
// earlier frames: the input buffer, frame advantage and exchange initiator.
type tracker struct {
	p      *Params
	player int

	hist  []InputBits
	head  int
	count int

	inExchange   bool
	selfRegain   int64
	enemyRegain  int64
	prevSelfCtl  bool
	prevEnemyCtl bool
	frameAdv     int32
	initiator    int8
	seeded       bool
}

func newTracker(p *Params, player int) *tracker {
	n := p.InputHistory
	if n <= 0 {
		n = 12
	}
	return &tracker{p: p, player: player, hist: make([]InputBits, n)}
}

func (t *tracker) reset() {
	for i := range t.hist {
		t.hist[i] = 0
	}
	t.head, t.count = 0, 0
	t.inExchange = false
	t.frameAdv, t.initiator = 0, 0
	t.seeded = false
}

// observe advances history by one frame. Call once per logical frame,
// before features for that frame.
func (t *tracker) observe(s *Snapshot) {
	me := s.Player(t.player)
	if me == nil {
		return
	}
	t.hist[t.head] = me.Root.Input
	t.head = (t.head + 1) % len(t.hist)
	if t.count < len(t.hist) {
		t.count++
	}

	ei := s.Enemy(t.player)
	if ei < 0 {
		return
	}
	en := &s.Players[ei].Root
	self := &me.Root
	tick := s.World.Tick

	selfBusy := self.MoveType == MtAttack || self.MoveType == MtHit || self.HitStun > 0 || self.BlockStun > 0
	enemyBusy := en.MoveType == MtAttack || en.MoveType == MtHit || en.HitStun > 0 || en.BlockStun > 0

	if !t.seeded {
		t.prevSelfCtl, t.prevEnemyCtl = self.Ctrl, en.Ctrl
		t.seeded = true
	}

	if !t.inExchange && (selfBusy || enemyBusy) {
		t.inExchange = true
		t.selfRegain, t.enemyRegain = -1, -1
		switch {
		case self.MoveType == MtAttack && en.MoveType != MtAttack:
			t.initiator = 1
		case en.MoveType == MtAttack && self.MoveType != MtAttack:
			t.initiator = -1
		default:
			t.initiator = 0
		}
	}
	if t.inExchange {
		if self.Ctrl && !t.prevSelfCtl {
			t.selfRegain = tick
		}
		if en.Ctrl && !t.prevEnemyCtl {
			t.enemyRegain = tick
		}
		if self.Ctrl && en.Ctrl && !selfBusy && !enemyBusy {
			if t.selfRegain >= 0 || t.enemyRegain >= 0 {
				sr, er := t.selfRegain, t.enemyRegain
				if sr < 0 {
					sr = tick
				}
				if er < 0 {
					er = tick
				}
				adv := er - sr
				if adv > 60 {
					adv = 60
				} else if adv < -60 {
					adv = -60
				}
				t.frameAdv = int32(adv)
			}
			t.inExchange = false
		}
	}
	t.prevSelfCtl, t.prevEnemyCtl = self.Ctrl, en.Ctrl
}

// features computes the comparison state for the current frame.
func (t *tracker) features(s *Snapshot) Feat {
	var f Feat
	me := s.Player(t.player)
	if me == nil {
		return f
	}
	self := &me.Root
	sign := float32(1)
	if !self.FacingRight {
		sign = -1
	}
	ox, oy := self.Pos[0], self.Pos[1]

	conv := func(c *CharSnap, isSelf bool) CharFeat {
		cf := CharFeat{
			Present:      true,
			VelX:         c.Vel[0] * sign,
			VelY:         c.Vel[1],
			State:        c.StateType,
			Move:         c.MoveType,
			Ctrl:         c.Ctrl,
			HitStun:      c.HitStun,
			BlockStun:    c.BlockStun,
			StateNo:      c.StateNo,
			StateTime:    c.StateTime,
			LifeFrac:     frac(c.Life, c.LifeMax),
			PowerFrac:    frac(c.Power, c.PowerMax),
			HitboxActive: c.HitboxActive,
		}
		if isSelf {
			cf.RelY = c.Pos[1] // own height above ground
		} else {
			cf.RelX = (c.Pos[0] - ox) * sign
			cf.RelY = c.Pos[1] - oy
		}
		return cf
	}

	f.Self = conv(self, true)
	for i := range me.Helpers {
		if me.Helpers[i].Alive {
			f.SelfHelpers = append(f.SelfHelpers, conv(&me.Helpers[i], false))
		}
	}

	ei := s.Enemy(t.player)
	var en *CharSnap
	if ei >= 0 {
		ep := &s.Players[ei]
		en = &ep.Root
		f.Enemy = conv(en, false)
		for i := range ep.Helpers {
			if ep.Helpers[i].Alive {
				f.EnemyHelpers = append(f.EnemyHelpers, conv(&ep.Helpers[i], false))
			}
		}
	}
	for _, ai := range s.Allies(t.player) {
		f.Allies = append(f.Allies, conv(&s.Players[ai].Root, false))
	}

	// Input history, oldest first.
	f.InDir = make([]uint8, t.count)
	f.InBtn = make([]InputBits, t.count)
	n := len(t.hist)
	for i := 0; i < t.count; i++ {
		b := t.hist[(t.head-t.count+i+n*2)%n]
		f.InDir[i] = b.Dir()
		f.InBtn[i] = b.Buttons()
	}

	w := &s.World
	f.RoundState = w.RoundState
	f.TimeFrac = w.TimeFrac()
	if en != nil {
		f.LifeLead = frac(self.Life, self.LifeMax) - frac(en.Life, en.LifeMax)
	}
	f.FrameAdv = t.frameAdv
	f.Initiator = t.initiator
	if self.TeamSide >= 0 && self.TeamSide < 2 {
		f.ComboMoves = w.ComboCount[self.TeamSide]
	}
	f.GotHit = self.BeingHit()
	f.DidHit = self.MoveHit > 0 && self.MoveHit <= 10
	switch {
	case self.MoveType == MtAttack && self.MoveHit > 0:
		f.Contact = 1
	case self.MoveType == MtAttack && self.MoveGuarded > 0:
		f.Contact = 2
	}
	if en != nil {
		f.Pressure = en.BlockStun > 0 && (self.MoveType == MtAttack || (self.MoveGuarded > 0 && self.MoveGuarded <= 30))
		f.Wall = wallState(self, en, w, t.p.NearWallDist)
	}
	f.Order, f.OrderLen = orderSignature(&f)
	if len(t.p.Vars) > 0 {
		f.Vars = make([]float32, len(t.p.Vars))
		for i, d := range t.p.Vars {
			if d.Float {
				f.Vars[i] = self.FloatVars[d.Index]
			} else {
				f.Vars[i] = float32(self.IntVars[d.Index])
			}
		}
	}
	return f
}

// wallState reports which side, if either, is near a stage edge with the
// other character in front of it.
func wallState(self, en *CharSnap, w *WorldSnap, nearFrac float32) int8 {
	width := w.StageRight - w.StageLeft
	if width <= 0 {
		return 0
	}
	near := width * nearFrac
	selfBack := w.StageLeft
	if !self.FacingRight {
		selfBack = w.StageRight
	}
	if absf(self.Pos[0]-selfBack) <= near {
		return -1
	}
	// The enemy's back is the edge on the far side of it from the focus.
	enBack := w.StageRight
	if en.Pos[0] < self.Pos[0] {
		enBack = w.StageLeft
	}
	if absf(en.Pos[0]-enBack) <= near {
		return 1
	}
	return 0
}

// orderSignature packs the left-to-right order of all objects into a code
// sequence: 1 self, 2 enemy, 3 own helper, 4 enemy helper, 5 ally.
func orderSignature(f *Feat) (uint64, uint8) {
	type obj struct {
		x    float32
		code uint8
	}
	objs := []obj{{0, 1}}
	if f.Enemy.Present {
		objs = append(objs, obj{f.Enemy.RelX, 2})
	}
	for _, h := range f.SelfHelpers {
		objs = append(objs, obj{h.RelX, 3})
	}
	for _, h := range f.EnemyHelpers {
		objs = append(objs, obj{h.RelX, 4})
	}
	for _, a := range f.Allies {
		objs = append(objs, obj{a.RelX, 5})
	}
	sort.SliceStable(objs, func(i, j int) bool { return objs[i].x < objs[j].x })
	if len(objs) > 21 {
		objs = objs[:21]
	}
	var sig uint64
	for i, o := range objs {
		sig |= uint64(o.code) << (3 * uint(i))
	}
	return sig, uint8(len(objs))
}
