package cbr

// Source is where a case's play came from. Selection priors and admission
// rules key on it.
type Source uint8

const (
	SrcUnknown Source = iota
	SrcHumanLocal
	SrcHumanLobby
	SrcRanked
	SrcRankedTop
	SrcTrial
	SrcAuthoredAI
	SrcEngineAI
	SrcSynthesized
)

// IsHuman reports whether the source is a person's play.
func (s Source) IsHuman() bool {
	switch s {
	case SrcHumanLocal, SrcHumanLobby, SrcRanked, SrcRankedTop, SrcTrial:
		return true
	}
	return false
}

// assisted reports whether the source can activate commands without the
// matching inputs: AssertCommand in authored AI, and the engine AI's command
// cheat, which activates motion commands for AI-controlled characters. Its
// recorded raw inputs do not reproduce those commands, so command cases from
// it are recorded with synthesized inputs (see Motion).
func (s Source) assisted() bool {
	return s == SrcAuthoredAI || s == SrcEngineAI
}

// CaseKind distinguishes command-anchored cases from movement cases.
type CaseKind uint8

const (
	KindCommand CaseKind = iota
	KindMovement
)

// CompletionKind classifies how a chain ended. Ender and result are recorded
// separately (see Completion), since a super ender causing a knockdown is
// both.
type CompletionKind uint8

const (
	CompUnknown CompletionKind = iota
	CompKnockdown
	CompSpecial
	CompSuper
	CompAdvantage
	CompReset
	CompRoundEnd
	CompDropped
	CompPunished
	CompNeutral
	CompTimeout
	CompNormal
)

func (k CompletionKind) String() string {
	switch k {
	case CompKnockdown:
		return "knockdown"
	case CompSpecial:
		return "special"
	case CompSuper:
		return "super"
	case CompAdvantage:
		return "advantage"
	case CompReset:
		return "reset"
	case CompRoundEnd:
		return "round_end"
	case CompDropped:
		return "dropped"
	case CompPunished:
		return "punished"
	case CompNeutral:
		return "neutral"
	case CompTimeout:
		return "timeout"
	case CompNormal:
		return "normal"
	}
	return "unknown"
}

// Technique flags, stored as a bit set on Technique.Flags.
const (
	TechOTGFinish uint32 = 1 << iota
	TechOTGRelaunch
	TechReset
	TechUnblockable
	TechAmbiguousCross
	_
	TechSafejump
	TechThrowLoop
	TechOptionSelect
	TechInstantAir
	TechLoop
)

// Completion records how the chain containing a case resolved.
type Completion struct {
	Ender        CompletionKind // last move's kind: special, super or normal
	Result       CompletionKind // resulting state
	Quality      float32        // multiplier on banked damage
	EndFrameAdv  int32
	OpponentDown bool
	PressureKept bool
	RoundEnded   bool
	MeterDelta   float32
	DamageDealt  float32 // fraction of the enemy's max life
	DamageTaken  float32 // fraction of own max life
	EvalFrame    int32   // frames after chain end at which evaluation resolved
	Resolved     bool
}

// Technique records techniques detected in the chain and loop/novelty data.
type Technique struct {
	Flags            uint32
	ChainRepetitions int32 // most recurrences of one move reference in the chain
	RepeatedMove     int32 // the move reference that recurred most
	ChainDuration    int32
	Novel            bool  // no human-sourced precedent for this chain shape
	HumanPrecedent   int32 // max repetitions of the repeated move in human data; 0 = none
	NoveltyMargin    int32
}

// LinkStat is timing data for one press within a route.
type LinkStat struct {
	CanonicalFrame int32
	Variance       float32 // frames, across successful human attempts
	Attempts       int32
	Successes      int32
}

// SuccessRate returns the measured success rate, or 1 when unmeasured.
func (l LinkStat) SuccessRate() float32 {
	if l.Attempts <= 0 {
		return 1
	}
	return clampf(float32(l.Successes)/float32(l.Attempts), 0.01, 1)
}

// Route holds properties measured for the chain a case starts or belongs to.
type Route struct {
	Key            uint64 // hash of the chain's attack sequence; groups repeats of a route
	DurationFrames int32
	TimerTicks     int32 // round-timer ticks consumed; excludes super pause and pause
	DamageScaled   float32
	Links          []LinkStat
}

// TrialSetup is the placement a trial was recorded from. Trial cases carry
// route evidence but never selection value (see Outcome).
type TrialSetup struct {
	SelfPos, DummyPos [2]float32
	DummyState        StateType
	DummyGuardMode    int32
	SelfPower         float32
	DummyLife         float32
}

// ReachRec is the contact result of a case's attack and where the enemy was
// when it started. Contact is -1 while unresolved, 0 whiff, 1 hit, 2 guarded.
type ReachRec struct {
	DX, DY     float32
	EnemyState StateType
	Contact    int8
	Valid      bool
}

// LevelStat collects outcomes excluded from value because injected
// execution noise caused them, for calibrating the noise curve.
type LevelStat struct {
	Attempts int32
	Drops    int32
}

// Outcome is a case's learned value. Trials and sums decay with recency.
// Plays counts how often the CBR layer started the case; Aborts how often it
// was cut short; ExecFails how often its command did not come out, which
// means the inputs do not reproduce the action from that state.
type Outcome struct {
	Trials    float32
	ValueSum  float32
	ValueSq   float32
	Plays     int32
	Aborts    int32
	ExecFails int32
	LastTick  int64
	ByLevel   map[int8]LevelStat
	ByOpp     map[string]OppStat
}

// unreproducible reports a case whose command repeatedly fails to come out
// when played.
func (o *Outcome) unreproducible() bool {
	return o.ExecFails >= 3 && o.ExecFails*2 > o.Plays
}

// OppStat is a per-opponent value record.
type OppStat struct {
	Trials   float32
	ValueSum float32
}

// Case is a decision point and the inputs that followed it.
type Case struct {
	ID       uint64
	Source   Source
	Kind     CaseKind
	Opponent string

	Start Feat

	// Inputs from the decision point onward, facing-relative, one per frame.
	Inputs []InputBits
	// Index into Inputs at which the self-initiated state change occurred;
	// -1 for a movement case.
	ExecFrame int32

	// Execution conditions at the decision point.
	StartStateType StateType
	StartCtrl      bool
	StartStateNo   int32
	StartMoveType  MoveType
	StartHitPause  int32

	MoveRef    int32 // state entered at ExecFrame
	ExecAttack bool  // the state entered at ExecFrame is an attack
	ExecClass  StateType
	Reach      ReachRec
	// CancelFrom is the attack state the action cancelled out of, and
	// CancelTime the state time there; 0 when the action did not begin
	// straight out of another attack.
	CancelFrom int32
	CancelTime int32
	// Command names the command whose canonical inputs replaced the recorded
	// ones (assisted sources and hypotheses); empty when the inputs are as
	// recorded.
	Command string

	ChainID  uint64
	ChainPos int32
	// LinkOffset is how many presses of the chain came before this case, so
	// its own presses index the chain's route link statistics.
	LinkOffset int32

	Outcome    Outcome
	Completion Completion
	Technique  Technique
	Route      Route
	Trial      *TrialSetup

	AILevel float32 // 0 when a human played it
	Tick    int64
	Session uint64
	Removed bool
}

// Evidence is how well attested a case is: its decayed trial count plus its
// source prior weight. Knowledge gating ranks cases by it.
func (c *Case) Evidence(p *Params) float32 {
	pr := p.Priors[c.Source]
	return c.Outcome.Trials + pr.Weight
}

// Value is the case's value estimate with shrinkage toward its source prior.
// Trial cases carry no measured selection value until used in a match.
func (c *Case) Value(p *Params) float32 {
	pr := p.Priors[c.Source]
	d := pr.Weight + c.Outcome.Trials
	if d <= 0 {
		return pr.Value
	}
	return (pr.Weight*pr.Value + c.Outcome.ValueSum) / d
}

// IsTrial reports whether the case came from trial mode.
func (c *Case) IsTrial() bool { return c.Source == SrcTrial }
