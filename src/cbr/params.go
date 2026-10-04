package cbr

// Weights are the similarity weights. The self, helper, enemy and
// enemy-helper groups carry the defaults recovered from the original CBR
// build; ally, clock, life-lead and power weights are additions.
//
// The recovered roundState weight (100) was a hard filter written as a
// weight and is implemented as a filter instead. The recovered unitOrder
// weight was 0 and the metric is omitted.
type Weights struct {
	RelX, RelY             float32
	VelX, VelY             float32
	InputDir, InputBtn     float32
	Airborne, Lying        float32
	Hit, Block, Attack     float32
	NearWall               float32
	MoveID, PressureMoveID float32
	GetHit, DidHit         float32
	FrameAdv               float32
	FrameAdvInitiator      float32
	ComboSimilarity        float32
	ObjectOrder            float32

	HelperRelX, HelperRelY float32
	HelperVelX, HelperVelY float32

	EnemyVelX, EnemyVelY              float32
	EnemyAirborne, EnemyLying         float32
	EnemyHit, EnemyBlock, EnemyAttack float32
	EnemyMoveID, EnemyPressureMoveID  float32

	EnemyHelperRelX, EnemyHelperRelY float32
	EnemyHelperVelX, EnemyHelperVelY float32

	AllyRelX, AllyRelY             float32
	AllyVelX, AllyVelY             float32
	AllyAirborne, AllyAttack       float32
	AllyHit, AllyBlock, AllyMoveID float32

	TimeFrac float32
	LifeLead float32
	Power    float32
}

// Prior is the optimistic starting value of a source, expressed as a value
// and a weight in pseudo-trials. Measured outcomes wash it out.
type Prior struct {
	Value  float32
	Weight float32
}

// TechniqueGate is the minimum execution level and knowledge level at which
// a technique may be selected.
type TechniqueGate struct {
	MinExecution int
	MinKnowledge int
}

// Params holds every tunable. DefaultParams returns the recovered values
// where they exist and documented starting points elsewhere.
type Params struct {
	W Weights

	// Normalisers (recovered).
	MaxXPos            float32
	MaxYPos            float32
	MaxVel             float32
	InputHistory       int
	MaxHitstunDiff     int32
	MaxBlockstunDiff   int32
	MaxAttackStateDiff int32
	NearWallDist       float32 // fraction of stage width
	ComboLength        float32

	// Selection. Distances are raw weighted sums, as in the original build,
	// so the recovered thresholds keep their scale.
	BetterCaseThreshold   float32 // recovered; margin needed to switch mid-case
	TopSelectionThreshold float32 // recovered; tie band for random choice (0 = best only)
	CaseReuseWeight       float32 // recovered caseReuse weight
	RepetitionFrames      int32   // recovered; recent-use window for the reuse cost
	ConfidenceThreshold   float32 // max distance to accept a case; above it the base AI keeps control
	// Selection is two-stage: cases within SimilarityBand of the best match
	// compete on value, exploration and reuse; distance inside the band
	// counts at BandDistanceWeight. Value never pulls in a worse match.
	SimilarityBand     float32
	BandDistanceWeight float32
	ValueWeight        float32
	ExplorationWeight  float32
	// HumanPrecedence is added to the score of non-human candidates when a
	// human-sourced case is also within the similarity band: where people
	// covered the situation, their play is preferred; where they did not,
	// whatever exists competes normally.
	HumanPrecedence float32
	// DemoWeight scales how much a recorded demonstration's outcome counts
	// toward its action's pooled value, relative to an outcome the CBR layer
	// measured playing the action itself. Demonstrated outcomes carry the
	// demonstrator's situational choices with them (a move used mostly as a
	// punish looks good everywhere), so they are trusted less than outcomes
	// of the layer's own play. DemoComboWeight applies instead to combo
	// continuations (the enemy already in hitstun), where the situation is
	// well defined and a demonstrated route's damage is a fair estimate.
	DemoWeight      float32
	DemoComboWeight float32

	// Store budget (see Store.Prune).
	MaxCases         int
	MaxNonHumanShare float32

	// Segmentation.
	MinCaseFrames      int32
	MaxCaseFrames      int32
	MaxMotionFrames    int32 // how far a case is back-dated to the start of a motion
	MovementCaseFrames int32 // movement cases split at least this often in neutral

	// Chains.
	HorizonFrames int32 // forced resolution of a chain that never ends
	HorizonCases  int32
	ResetWindow   int32 // frames after a reset-completion in which a new mixup counts as a reset

	// Outcome.
	ComboGamma     float32 // credit decay backward along a chain
	RoundGamma     float32 // terminal credit decay backward along a round, per chain
	Decay          float32 // recency decay applied per update
	TakenWeight    float32
	MeterWeight    float32
	TerminalWin    float32
	TerminalLoss   float32
	TerminalWeight float32
	// Clock: chains in the late-round pressure zone receive a larger share of
	// the terminal reward, so what wins on the clock is what gets learned.
	ClockEnabled       bool
	ClockWeightMax     float32
	ClockPressureFloor float32

	Quality   map[CompletionKind]float32
	Situation map[CompletionKind]float32

	Priors map[Source]Prior

	// Difficulty.
	NoiseScaleMax  float32 // timing-noise multiplier at execution 1
	NoiseCurve     float32
	KnowledgeFloor float32 // fraction of data visible at knowledge 1
	Gates          map[uint32]TechniqueGate

	// Loops and novelty.
	LoopMinRepeats int32

	// Commands (see command.go). ChangeStateFlagCount is how many attack
	// starts with no command active mark an AI-controlled character's AI as
	// direct state change for the match. A hypothesis needs its command seen
	// with the state HypothesisMinEvidence times before a case is
	// synthesized for it; at most HypothesisCasesPerState such cases are
	// kept per state, and HypothesisMaxAttempts failed plays with no success
	// classify the state as reachable only by direct state change.
	ChangeStateFlagCount    int32
	HypothesisMinEvidence   float32
	HypothesisCasesPerState int
	HypothesisMaxAttempts   int32

	// Variables whose values gate case selection.
	Vars []VarDecl
}

// VarDecl declares a character variable that matters for case selection,
// the successor of the original build's CBRVariableImportance.
type VarDecl struct {
	Float     bool
	Index     int32
	Tolerance float32 // differences up to this cost nothing
	Range     float32 // difference at which the full cost applies
	MaxCost   float32
}

// DefaultParams returns the recovered defaults and documented starting
// points for everything added since.
func DefaultParams() *Params {
	p := &Params{
		W: Weights{
			RelX: 1, RelY: 1,
			VelX: 0.25, VelY: 0.25,
			InputDir: 1, InputBtn: 1,
			Airborne: 1, Lying: 1,
			Hit: 1, Block: 1, Attack: 1,
			NearWall:          0.3,
			MoveID:            0.5,
			PressureMoveID:    0.8,
			GetHit:            1,
			DidHit:            1,
			FrameAdv:          0.3,
			FrameAdvInitiator: 0.1,
			ComboSimilarity:   1,
			ObjectOrder:       0.3,

			HelperRelX: 0.5, HelperRelY: 0.5,
			HelperVelX: 0.25, HelperVelY: 0.25,

			EnemyVelX: 0.25, EnemyVelY: 0.25,
			EnemyAirborne: 1, EnemyLying: 1,
			EnemyHit: 1, EnemyBlock: 1, EnemyAttack: 1,
			EnemyMoveID: 0.5, EnemyPressureMoveID: 0.8,

			EnemyHelperRelX: 1, EnemyHelperRelY: 1,
			EnemyHelperVelX: 0.25, EnemyHelperVelY: 0.25,

			AllyRelX: 0.5, AllyRelY: 0.5,
			AllyVelX: 0.15, AllyVelY: 0.15,
			AllyAirborne: 0.5, AllyAttack: 0.5,
			AllyHit: 0.5, AllyBlock: 0.5, AllyMoveID: 0.3,

			TimeFrac: 0.3,
			LifeLead: 0.3,
			Power:    0.3,
		},

		MaxXPos:            300,
		MaxYPos:            200,
		MaxVel:             10,
		InputHistory:       12,
		MaxHitstunDiff:     10,
		MaxBlockstunDiff:   10,
		MaxAttackStateDiff: 20,
		NearWallDist:       0.13,
		ComboLength:        20,

		BetterCaseThreshold:   0.3,
		TopSelectionThreshold: 0,
		CaseReuseWeight:       0.5,
		RepetitionFrames:      60,
		ConfidenceThreshold:   6,
		SimilarityBand:        1,
		BandDistanceWeight:    0.5,
		ValueWeight:           4,
		ExplorationWeight:     0,
		HumanPrecedence:       1,
		DemoWeight:            1,
		DemoComboWeight:       1,

		MaxCases:         6000,
		MaxNonHumanShare: 0.4,

		MinCaseFrames:      2,
		MaxCaseFrames:      90,
		MaxMotionFrames:    15,
		MovementCaseFrames: 30,

		HorizonFrames: 600,
		HorizonCases:  40,
		ResetWindow:   20,

		ComboGamma:     0.9,
		RoundGamma:     0.98,
		Decay:          0.995,
		TakenWeight:    1,
		MeterWeight:    0.1,
		TerminalWin:    1,
		TerminalLoss:   -1,
		TerminalWeight: 0,

		ClockEnabled:       true,
		ClockWeightMax:     0.5,
		ClockPressureFloor: 0.5,

		Quality: map[CompletionKind]float32{
			CompRoundEnd:  1.25,
			CompKnockdown: 1.0,
			CompAdvantage: 0.85,
			CompReset:     0.85,
			CompNeutral:   0.6,
			CompTimeout:   0.3,
			CompDropped:   0.25,
			CompPunished:  0,
		},
		Situation: map[CompletionKind]float32{
			CompRoundEnd:  0.1,
			CompKnockdown: 0.05,
			CompAdvantage: 0.03,
			CompReset:     0.03,
			CompNeutral:   0,
			CompTimeout:   -0.02,
			CompDropped:   -0.03,
			CompPunished:  -0.05,
		},

		Priors: map[Source]Prior{
			SrcRankedTop:   {0.75, 30},
			SrcTrial:       {0.65, 24},
			SrcRanked:      {0.62, 22},
			SrcHumanLocal:  {0.60, 20},
			SrcHumanLobby:  {0.55, 18},
			SrcAuthoredAI:  {0.30, 10},
			SrcEngineAI:    {0.10, 5},
			SrcSynthesized: {0.05, 3},
			SrcUnknown:     {0.05, 3},
		},

		NoiseScaleMax:  2.5,
		NoiseCurve:     1.2,
		KnowledgeFloor: 0.2,
		Gates: map[uint32]TechniqueGate{
			TechOTGFinish:      {8, 8},
			TechOTGRelaunch:    {8, 8},
			TechReset:          {8, 8},
			TechUnblockable:    {8, 8},
			TechAmbiguousCross: {8, 8},
			TechSafejump:       {8, 8},
			TechOptionSelect:   {8, 8},
			TechThrowLoop:      {8, 8},
			TechLoop:           {8, 8},
			TechInstantAir:     {7, 7},
		},

		LoopMinRepeats: 3,

		ChangeStateFlagCount:    2,
		HypothesisMinEvidence:   3,
		HypothesisCasesPerState: 5,
		HypothesisMaxAttempts:   3,
	}
	return p
}
