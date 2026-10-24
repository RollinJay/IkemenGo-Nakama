package cbr

// StateType mirrors the engine's state types.
type StateType uint8

const (
	StUnknown StateType = iota
	StStand
	StCrouch
	StAir
	StLying
)

// MoveType mirrors the engine's move types.
type MoveType uint8

const (
	MtUnknown MoveType = iota
	MtIdle
	MtAttack
	MtHit
)

// EntryKind says how a character's current state was entered, on the frame
// it was entered (see CharSnap.Entry). The engine works it out from the
// controller that changed the state: a "person route" is a state change
// whose conditions check a button command and do not need the AI level
// above 0, so a person playing the character can make it.
type EntryKind int8

const (
	// EntryNone: no state change this frame, or one the adapter could not
	// label (an engine or simulator that does not fill Entry).
	EntryNone EntryKind = iota
	// EntryInput: a person route fired, and its command was in the real
	// input buffer, put there by a person or by the CBR layer's input.
	// EntryCommand names the command; it is empty for the engine's built-in
	// walk, jump, crouch and guard, which go by the directions held.
	EntryInput
	// EntryAuto: part of the move itself, with no command and no decision of
	// the AI: a follow-up, the return to standing, a hit or guard state the
	// engine or the opponent's move sets.
	EntryAuto
	// EntryReachable: the character's AI decided (a change gated on the AI
	// level, a person route that fired only because the engine AI's random
	// command cheat made its command true, or a change made from control or
	// out of an attack with no command), and a person route into the same
	// state would have fired at that moment with EntryCommand pressed.
	EntryReachable
	// EntryCheat: the AI decided, and no person route into that state would
	// have fired at that moment, including states no person route enters.
	EntryCheat
	// EntryUnknown: the AI decided, but the person routes into that state
	// could not be checked (a target that is not a fixed number, conditions
	// that assign variables, and the like).
	EntryUnknown
)

func (k EntryKind) String() string {
	switch k {
	case EntryNone:
		return "none"
	case EntryInput:
		return "input"
	case EntryAuto:
		return "auto"
	case EntryReachable:
		return "reachable"
	case EntryCheat:
		return "cheat"
	case EntryUnknown:
		return "unknown"
	}
	return "invalid"
}

// AIDecision reports whether the character's own AI made the state change.
func (k EntryKind) AIDecision() bool {
	return k == EntryReachable || k == EntryCheat || k == EntryUnknown
}

// RouteTable lists, for one character, the button commands that lead into
// each state: the person routes of its command and state files. The engine
// builds it once when the character loads, and the same table is reported
// on every frame.
type RouteTable struct {
	// Person maps a state number to the commands of the person routes into
	// it, in the order the character's files list them: each route's main
	// button command when it needs several (a held direction and a button,
	// say). A state missing here has no person route with a fixed target.
	Person map[int32][]string
	// Unreadable is set when some person route's target is not a fixed
	// number, so it might lead into any state.
	Unreadable bool
}

// PersonCommands returns the commands of the person routes into state.
// known is false when the table cannot say: there is no table, or no person
// route with a fixed target leads there and some route's target is not a
// fixed number. known with no commands means no person route enters the
// state, so only a state change the AI makes directly reaches it.
func (t *RouteTable) PersonCommands(state int32) (cmds []string, known bool) {
	if t == nil {
		return nil, false
	}
	if cmds, ok := t.Person[state]; ok && len(cmds) > 0 {
		return cmds, true
	}
	return nil, !t.Unreadable
}

// CharSnap is one character or helper on one logical frame. The adapter
// fills it from engine state; every field is plain data.
type CharSnap struct {
	ID          int32
	PlayerNo    int
	TeamSide    int
	HelperID    int32 // 0 for a root character
	IsHelper    bool
	Pos         [3]float32
	Vel         [3]float32
	FacingRight bool

	StateNo     int32
	PrevStateNo int32
	StateTime   int32
	StateType   StateType
	MoveType    MoveType
	Ctrl        bool
	// HitPause is the remaining hitpause. State time does not advance during
	// hitpause, so it distinguishes frames that share a state time.
	HitPause int32
	// HitBy is the player number (1-based) of whoever hit this character
	// last, 0 when unknown. It attributes damage in team modes.
	HitBy int

	// HitStun is the remaining hit time while being hit and not guarding;
	// BlockStun the remaining hit time while guarding. Zero otherwise.
	HitStun   int32
	BlockStun int32

	// Frames since this character's attack made contact, from the engine's
	// MoveHit / MoveGuarded / MoveContact triggers. Zero when none.
	MoveHit     int32
	MoveGuarded int32
	MoveContact int32

	Life, LifeMax   int32
	Power, PowerMax int32
	Dizzy, DizzyMax int32
	Guard, GuardMax int32
	RedLife         int32
	Juggle          int32

	AnimNo   int32
	AnimElem int32

	// Raw engine input this frame (root characters only), converted with
	// FromEngine by the adapter.
	Input InputBits

	// Commands lists the commands active this frame by name (root characters
	// only): completed by input, asserted with AssertCommand, or activated
	// by the engine AI's command cheat. CmdKnown is set when the adapter
	// reports command activity, so an empty list means none was active.
	Commands []string
	CmdKnown bool

	// Declared important variables only (see VarDecl). Nil when none.
	IntVars   map[int32]int32
	FloatVars map[int32]float32

	AILevel      float32 // 0 for a human
	CBRDriven    bool    // input this frame came from the CBR layer
	HitboxActive bool
	Alive        bool

	// Entry says how the current state was entered, on the snapshot of the
	// frame in which it was entered, and is EntryNone on every other frame
	// (root characters the CBR layer watches only). On that frame StateTime
	// is 1 when the change was made before the engine advanced the state
	// time, which it does once the character's own code has run (that code,
	// the engine's built-in walk, jump, crouch and guard), and 0 when it was
	// made after (a hit, or a helper's or the opponent's code that runs
	// later); StateNo may equal the frame before's when the state was entered
	// again. When the state changed more than once in the frame, Entry is
	// for the last change, except that a follow-up the new state's own code
	// makes in the same frame keeps the label of the decision that entered
	// the move.
	Entry EntryKind
	// EntryCommand is the command of the person route behind Entry: the one
	// input (EntryInput), or the one that would have worked at that moment
	// (EntryReachable); for a route needing several, its main button
	// command.
	EntryCommand string
	// EntryHelper is set when a helper's controller made the change,
	// redirected to this character.
	EntryHelper bool
	// Routes is the character's route table: the same pointer on every
	// frame, nil when the adapter has none.
	Routes *RouteTable
}

// PlayerSnap is a root character plus its helpers.
type PlayerSnap struct {
	Root    CharSnap
	Helpers []CharSnap
	Name    string
	Present bool
}

// WorldSnap is match-level state on one logical frame.
type WorldSnap struct {
	Tick         int64
	RoundNo      int32
	RoundState   int32 // 0 pre-intro, 1 intro, 2 fight, 3 KO / time over, 4 win poses
	Intro        int32
	CurRoundTime int32 // engine's remaining round time; -1 for infinite
	MaxRoundTime int32 // <= 0 for infinite
	StageLeft    float32
	StageRight   float32
	ComboCount   [2]int32
	SuperPause   bool
	Pause        bool
	WinTeam      int32  // -1 while undecided
	Session      uint64 // changes at each rollback session or Turns member change
}

// Snapshot is one logical frame of the match. Players is indexed by the
// engine's player number.
type Snapshot struct {
	World   WorldSnap
	Players []PlayerSnap
}

// BeingHit reports a character in hitstun. Guard-hit states also carry move
// type H in MUGEN's common states, so a guarding character is not counted.
func (c *CharSnap) BeingHit() bool {
	return c.HitStun > 0 || (c.MoveType == MtHit && c.BlockStun == 0 && c.StateType != StLying && !c.Ctrl)
}

// Player returns the player at index n, or nil.
func (s *Snapshot) Player(n int) *PlayerSnap {
	if s == nil || n < 0 || n >= len(s.Players) || !s.Players[n].Present {
		return nil
	}
	return &s.Players[n]
}

// opposes reports whether p is a present root on the team side opposing
// me. Characters with no team side (stage-attached characters) oppose no one.
func opposes(me, p *PlayerSnap) bool {
	return p.Present && p.Root.TeamSide >= 0 && me.Root.TeamSide >= 0 && p.Root.TeamSide != me.Root.TeamSide
}

// Enemy returns the nearest living root on the other team side, or -1.
func (s *Snapshot) Enemy(n int) int {
	me := s.Player(n)
	if me == nil {
		return -1
	}
	best, bestD := -1, float32(0)
	for i := range s.Players {
		p := &s.Players[i]
		if i == n || !opposes(me, p) || !p.Root.Alive {
			continue
		}
		d := absf(p.Root.Pos[0] - me.Root.Pos[0])
		if best < 0 || d < bestD {
			best, bestD = i, d
		}
	}
	return best
}

// Allies returns present roots on the same team side other than n.
func (s *Snapshot) Allies(n int) []int {
	me := s.Player(n)
	if me == nil {
		return nil
	}
	var out []int
	for i := range s.Players {
		p := &s.Players[i]
		if i != n && p.Present && p.Root.TeamSide >= 0 && p.Root.TeamSide == me.Root.TeamSide && p.Root.Alive {
			out = append(out, i)
		}
	}
	return out
}

// Opponents returns every present root on the other team side, alive or
// not, for damage accounting.
func (s *Snapshot) Opponents(n int) []int {
	me := s.Player(n)
	if me == nil {
		return nil
	}
	var out []int
	for i := range s.Players {
		if i != n && opposes(me, &s.Players[i]) {
			out = append(out, i)
		}
	}
	return out
}

// TimerFrozen reports whether the round timer does not run this frame. The
// engine's timer stops during super pause and pause, so time spent there is
// not consumed clock.
func (w *WorldSnap) TimerFrozen() bool {
	return w.SuperPause || w.Pause || w.Intro != 0
}

// TimeFrac is the remaining round time as a fraction, or -1 when infinite.
func (w *WorldSnap) TimeFrac() float32 {
	if w.MaxRoundTime <= 0 || w.CurRoundTime < 0 {
		return -1
	}
	return clampf(float32(w.CurRoundTime)/float32(w.MaxRoundTime), 0, 1)
}

func absf(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func clampf(v, lo, hi float32) float32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func frac(n, d int32) float32 {
	if d <= 0 {
		return 0
	}
	return clampf(float32(n)/float32(d), 0, 1)
}
