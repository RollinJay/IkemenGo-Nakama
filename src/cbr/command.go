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
// as ChangeState in state -1. Its transitions are unexplained by any
// command, and a character showing them has its AI-sourced cases for the
// match discarded. The states it enters are kept as hypotheses: when human
// or assisted play has shown which command leads to a state, a synthesized
// case for it is stored and tried, and its plays show whether the state is
// reachable with inputs or only by direct state change.

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
