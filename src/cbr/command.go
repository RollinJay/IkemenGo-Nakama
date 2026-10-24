package cbr

import "sort"

// Commands and motions.
//
// Two kinds of play activate commands without the inputs that complete
// them: authored AI using AssertCommand, and the engine AI, which for an
// AI-controlled character treats one random command per frame as active
// (Ikemen's "AI cheating" for commands longer than one button). The state
// change is real and obeys the character's own command conditions, but the
// motion's input time is skipped and nothing in the raw input shows it.
// Command cases from those sources are therefore recorded with the
// command's canonical inputs instead of the raw ones, back-dated by the
// motion's length. That restores the commitment a person pays, so a move
// that only works because the motion was skipped fails when played.
//
// A third kind enters states with no command at all: character AI written
// as ChangeState in state -1. The engine labels each state change of a
// character the CBR layer watches (CharSnap.Entry), and when the
// character's own AI made it, checks at that very moment whether a person's
// command would have made the same change. When one would have
// (EntryReachable), the decision is recorded as a case of the character's
// AI (SrcCharacterAI) with that command's canonical inputs and the result
// the AI got with the move, as for an assisted source. When none would have
// (EntryCheat), the move is only reachable by changing the state directly,
// which a person cannot do: it never becomes a case, and the check counts
// toward the verdict of its spot (see Verdict). Either way the rest of the
// AI's play is recorded as usual.
//
// Without those labels (another engine, or a change the engine could not
// check, EntryUnknown), such transitions are unexplained by any command, and
// a character showing them has its AI-sourced cases for the match
// discarded. The states it enters are kept as hypotheses: when human or
// assisted play has shown which command leads to a state, a synthesized
// case for it is stored and tried, and its plays show whether the state is
// reachable with inputs or only by direct state change.
//
// A case whose command nobody was seen to input, a character AI's or a
// hypothesis, takes over only where the CBR layer's own test plays have
// verified that its command works (see Verdict).

// Motion is a command's canonical input sequence, completing on its last
// frame. Steps is the command's step count, used to prefer the more
// specific of several active commands.
type Motion struct {
	Inputs []InputBits
	Steps  int
}

func (m Motion) hasButton() bool {
	for _, b := range m.Inputs {
		if b.Buttons() != 0 {
			return true
		}
	}
	return false
}

// Hypothesis is a state the character's AI was seen entering from control
// with no command active.
type Hypothesis struct {
	Seen      int32  // command-free entries observed
	Command   string // command resolved from the command map; "" while unresolved
	Attempts  int32  // plays of its synthesized cases that resolved
	Successes int32  // plays in which the state came out
	CheatOnly bool   // its command never produced the state when played
}

// noteCommand counts the commands active when a state was entered. Over
// time the count shows which command leads to which state.
func (s *Store) noteCommand(state int32, active []string) {
	if len(active) == 0 {
		return
	}
	m := s.CmdStates[state]
	if m == nil {
		m = map[string]float32{}
		s.CmdStates[state] = m
	}
	for _, n := range active {
		m[n]++
	}
}

// commandFor picks the command that most plausibly caused entry into state
// from those active. Only commands with a known motion qualify, and for an
// attack, commands with a button are preferred. Among the rest, those seen
// with the state nearly as often as the most frequent one (within
// cmdTieFrac) are treated as equally likely, and the most specific of them
// wins: a motion command and its own button always appear together, and the
// motion is what a state reached by both needs.
func (s *Store) commandFor(state int32, active []string, motions map[string]Motion, attack bool, pending map[int32]map[string]float32) (string, bool) {
	btn := false
	if attack {
		for _, n := range active {
			if m, ok := motions[n]; ok && m.hasButton() {
				btn = true
				break
			}
		}
	}
	var names []string
	for _, n := range active {
		if m, ok := motions[n]; ok && len(m.Inputs) > 0 && (!btn || m.hasButton()) {
			names = append(names, n)
		}
	}
	return pickCommand(names, mergeCounts(s.CmdStates[state], pending[state]), motions, 0)
}

// mergeCounts adds a match's pending observations to the stored counts.
func mergeCounts(a, b map[string]float32) map[string]float32 {
	if len(b) == 0 {
		return a
	}
	out := make(map[string]float32, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] += v
	}
	return out
}

const cmdTieFrac = 0.9

// pickCommand chooses among names by count, treating counts within
// cmdTieFrac of the highest as ties broken by specificity. Names counted
// fewer than minCount times are ignored.
func pickCommand(names []string, counts map[string]float32, motions map[string]Motion, minCount float32) (string, bool) {
	top := float32(0)
	for _, n := range names {
		if c := counts[n]; c > top {
			top = c
		}
	}
	best := ""
	var bestM Motion
	for _, n := range names {
		c := counts[n]
		if c < minCount || c < top*cmdTieFrac {
			continue
		}
		m := motions[n]
		if best == "" || moreSpecific(m, n, bestM, best) {
			best, bestM = n, m
		}
	}
	return best, best != ""
}

func moreSpecific(a Motion, an string, b Motion, bn string) bool {
	if a.Steps != b.Steps {
		return a.Steps > b.Steps
	}
	if len(a.Inputs) != len(b.Inputs) {
		return len(a.Inputs) > len(b.Inputs)
	}
	return an < bn
}

// hypothesisCommand resolves a hypothesis state to the command seen leading
// to it, if that has been seen at least minCount times and has a motion
// with a button.
func (s *Store) hypothesisCommand(state int32, motions map[string]Motion, minCount float32, pending map[int32]map[string]float32) (string, bool) {
	counts := mergeCounts(s.CmdStates[state], pending[state])
	names := make([]string, 0, len(counts))
	for n := range counts {
		if m, ok := motions[n]; ok && m.hasButton() {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return pickCommand(names, counts, motions, minCount)
}

func (s *Store) hypothesis(state int32) *Hypothesis {
	h := s.Hypotheses[state]
	if h == nil {
		h = &Hypothesis{}
		s.Hypotheses[state] = h
	}
	return h
}

// liveHypotheses counts stored synthesized cases for a state that are still
// being tried.
func (s *Store) liveHypotheses(state int32) int {
	n := 0
	for _, c := range s.byID {
		if c.Source == SrcSynthesized && c.MoveRef == state && !c.Removed && !c.Outcome.unreproducible() {
			n++
		}
	}
	return n
}

// resolveHypothesisPlay records whether a played hypothesis case produced
// its state, and classifies the state as reachable only by direct state
// change once enough plays have all failed.
func (p *Params) resolveHypothesisPlay(s *Store, c *Case, came bool) {
	h := s.hypothesis(c.MoveRef)
	h.Attempts++
	if came {
		h.Successes++
	}
	if !h.CheatOnly && h.Successes == 0 && h.Attempts >= p.HypothesisMaxAttempts {
		h.CheatOnly = true
		for _, hc := range s.byID {
			if hc.Source == SrcSynthesized && hc.MoveRef == c.MoveRef {
				hc.Removed = true
			}
		}
		s.dirtyRemoved = true
	}
}

// Verdicts: whether a person's command works in a spot.
//
// A spot (VerdictKey) is a state entered with a command, from a kind of
// moment: with control, or else from the exact state the character is in
// (the attack a cancel comes out of, say), in one stance (standing,
// crouching, in the air). What the character's power allows is kept within
// the spot rather than as part of it, since more power never keeps a
// person's command from working and less never helps: a super that needs a
// bar is verified at a bar or more, and found out below it, without being
// tested again at every power in between.
//
// Two kinds of evidence go into a spot's verdict:
//
//   - The engine's moment checks of the character's own AI's decisions (see
//     EntryKind): one that found the person's route open (EntryReachable)
//     shows the command could have worked there at that power; one that
//     found no person's route into the state open (EntryCheat) counts as a
//     failure there for every command that leads into it.
//   - The CBR layer's own plays (Engine.Frame and the driver): one whose
//     input brought the state about on a frame it gave the input, as a
//     person's route with the command (EntryInput, and without the engine's
//     labels, with the command active then), is a success; one whose command
//     did not come out, from a clean start (not taken over, not hit, with no
//     leftover input that could have made another command, see
//     driver.carryOver), a failure.
//
// A spot is verified at a power once its inputs brought the state about
// there and the power is at least what the command needs: the least power
// at which it was seen to work, in any spot (see Store.powerNeed). It is
// blocked at a power once three failures there came at that power or above,
// unless verified: cheating, when no moment check found the route open there
// at that power or below, and otherwise unreproducible (the command works
// there, but its inputs do not bring it about). Every other spot is open: a
// case needing verification (see Case.mustVerify) is tested there, within
// Params.VerifyPerRound test plays a round, or tried by exploration, but
// does not take over. A success settles its own spot only. A state no
// person's route leads into at all (CharSnap.Routes) is blocked everywhere
// from the start.

// VerdictKey is a spot (see Verdicts above).
type VerdictKey struct {
	State   int32  // the state entered
	Command string // the person's command that enters it
	// From is -1 for a moment with control; without control, the state the
	// character is in.
	From   int32
	Stance StateType
}

// spotKey is the spot of entering state with command cmd from the moment
// at, the last frame before the change.
func spotKey(state int32, cmd string, at *CharSnap) VerdictKey {
	from := int32(-1)
	if !at.Ctrl {
		from = at.StateNo
	}
	return VerdictKey{State: state, Command: cmd, From: from, Stance: at.StateType}
}

// Verdict is what is known of a person's command in one spot (see Verdicts
// above). Powers are only meaningful once their counts are non-zero.
type Verdict struct {
	// Moment checks of the character's AI's own decisions here: how many
	// found the person's route open, and how many found no route into the
	// state open; ReachPower is the lowest power at which one was open.
	Reachable, Cheat int32
	ReachPower       int32
	// The CBR layer's plays: how many succeeded, and the lowest power at
	// which one did; how many failed from a clean start.
	Successes, Fails int32
	OKPower          int32
	// FailTop holds the highest powers of the failures (clean failed plays
	// and moment checks that found no route open), highest first, as many as
	// there are up to three.
	FailTop [3]int32
}

// verdictState is a spot's verdict at one power.
type verdictState uint8

const (
	verdictOpen verdictState = iota
	verdictVerified
	verdictBlocked
)

// failures is how many failures FailTop holds.
func (v *Verdict) failures() int {
	return int(min(v.Fails+v.Cheat, int32(len(v.FailTop))))
}

// addFailure keeps power among FailTop's highest; call it before counting
// the failure.
func (v *Verdict) addFailure(power int32) {
	n := v.failures()
	i := n
	if i == len(v.FailTop) {
		i--
		if power <= v.FailTop[i] {
			return
		}
	}
	for ; i > 0 && v.FailTop[i-1] < power; i-- {
		v.FailTop[i] = v.FailTop[i-1]
	}
	v.FailTop[i] = power
}

// needKey is a state entered with a command, in any spot.
type needKey struct {
	state int32
	cmd   string
}

// powerNeed returns the least power the command cmd was seen to work with
// entering state, in any spot: at a moment check that found its route open
// or a play its inputs brought the state about in. ok is false when it was
// never seen to work.
func (s *Store) powerNeed(state int32, cmd string) (need int32, ok bool) {
	if s.need == nil {
		s.need = map[needKey]int32{}
		for k, v := range s.Verdicts {
			if v.Reachable > 0 {
				s.lowerNeed(k, v.ReachPower)
			}
			if v.Successes > 0 {
				s.lowerNeed(k, v.OKPower)
			}
		}
	}
	need, ok = s.need[needKey{state, cmd}]
	return need, ok
}

// lowerNeed notes that the command of spot k worked at power.
func (s *Store) lowerNeed(k VerdictKey, power int32) {
	if s.need == nil {
		return // built from the verdicts on first use
	}
	nk := needKey{k.State, k.Command}
	if n, ok := s.need[nk]; !ok || power < n {
		s.need[nk] = power
	}
}

// verdict returns a spot's verdict, creating it.
func (s *Store) verdict(k VerdictKey) *Verdict {
	v := s.Verdicts[k]
	if v == nil {
		v = &Verdict{}
		s.Verdicts[k] = v
	}
	return v
}

// reachAt counts a moment check that found the person's route of spot k
// open, at power.
func (s *Store) reachAt(k VerdictKey, power int32) {
	v := s.verdict(k)
	if v.Reachable == 0 || power < v.ReachPower {
		v.ReachPower = power
	}
	v.Reachable++
	s.lowerNeed(k, power)
}

// cheatAt counts a moment check that found no person's route into the state
// of spot k open, at power.
func (s *Store) cheatAt(k VerdictKey, power int32) {
	v := s.verdict(k)
	v.addFailure(power)
	v.Cheat++
}

// successAt counts a play whose inputs brought the state of spot k about as
// a person's command, begun at power.
func (s *Store) successAt(k VerdictKey, power int32) {
	v := s.verdict(k)
	if v.Successes == 0 || power < v.OKPower {
		v.OKPower = power
	}
	v.Successes++
	s.lowerNeed(k, power)
}

// failAt counts a play in spot k, begun at power from a clean start, whose
// command did not come out.
func (s *Store) failAt(k VerdictKey, power int32) {
	v := s.verdict(k)
	v.addFailure(power)
	v.Fails++
}

// verdictAt is the verdict of spot k at power.
func (s *Store) verdictAt(k VerdictKey, power int32) verdictState {
	v := s.Verdicts[k]
	if v == nil {
		return verdictOpen
	}
	if v.Successes > 0 {
		need := v.OKPower
		if n, ok := s.powerNeed(k.State, k.Command); ok && n < need {
			need = n
		}
		if power >= need {
			return verdictVerified
		}
	}
	if v.failures() == len(v.FailTop) && v.FailTop[len(v.FailTop)-1] >= power {
		return verdictBlocked
	}
	return verdictOpen
}

// cheating reports whether spot k, blocked at power, is cheating rather than
// unreproducible: no moment check found the person's route open there at
// that power or below.
func (s *Store) cheating(k VerdictKey, power int32) bool {
	v := s.Verdicts[k]
	return s.verdictAt(k, power) == verdictBlocked && !(v.Reachable > 0 && power >= v.ReachPower)
}

// check is the verdict of case c's command at the moment self, before c is
// begun there (see Verdicts above).
func (s *Store) check(c *Case, self *CharSnap) verdictState {
	if cmds, known := self.Routes.PersonCommands(c.MoveRef); known && len(cmds) == 0 {
		return verdictBlocked
	}
	return s.verdictAt(spotKey(c.MoveRef, c.Command, self), self.Power)
}

// noteMoment counts the engine's moment check of a decision of the
// character's own AI shown on self, made from the moment prev, the frame
// before (see Verdicts above).
func (s *Store) noteMoment(prev, self *CharSnap) {
	switch self.Entry {
	case EntryReachable:
		if self.EntryCommand != "" {
			s.reachAt(spotKey(self.StateNo, self.EntryCommand, prev), prev.Power)
		}
	case EntryCheat:
		cmds, _ := self.Routes.PersonCommands(self.StateNo)
		for _, cmd := range cmds {
			s.cheatAt(spotKey(self.StateNo, cmd, prev), prev.Power)
		}
	}
}
