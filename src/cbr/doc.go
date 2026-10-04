// Package cbr implements case-based reasoning AI for IKEMEN GO.
//
// The package is engine-agnostic: it imports nothing from the engine and
// sees the game only through Snapshot values that an adapter in the engine
// fills once per logical frame. All engine coupling lives in that adapter.
//
// # Overview
//
// Play is recorded as a stream of snapshots, split into cases (a decision
// point plus the inputs that followed it), and grouped into chains (a
// sequence of cases forming one exchange or combo). Each chain is graded
// when it resolves: the ender kind and the resulting state produce a
// completion quality, which multiplies the damage the chain banked. That
// value is credited back across the chain's cases, and pooled per action in
// a situation (see actionKey).
//
// At runtime the engine asks for input for a CBR-driven player. The
// selector filters cases by hard constraints (round state, technique gates,
// knowledge visibility, execution conditions, reach), keeps those within a
// similarity band of the best match, ranks them by learned value, and plays
// the best back. When nothing is close enough it declines and the engine's
// own AI keeps control.
//
// # Commands
//
// When the adapter reports command activity, recording uses it: an action
// needs a command with a button or several steps; a state change inside a
// move with no such command is the move's own continuation. Commands that AI
// activates without inputs (AssertCommand, the engine AI's command cheat)
// are recorded with the command's canonical inputs, supplied by the adapter
// (see Motion). Attack states entered with no command at all mark the
// character's AI as direct state change: its cases for the match are
// discarded and the states it entered are kept as hypotheses to test.
//
// # Difficulty
//
// Three dials, each 1 to 8:
//
//   - Execution scales input-timing noise drawn from measured human
//     variance per combo link.
//   - Ambition weights routes by their probability of completing at the
//     current execution level.
//   - Knowledge restricts which learned data is visible at all.
//
// Tier 9 is execution 8 with knowledge 8; gated techniques (resets, OTG
// relaunches, loops and similar) unlock only there by default.
//
// # Determinism
//
// The selector uses its own seeded random source. Offline this has no
// bearing on the engine's state. In netplay the engine must not call Frame
// from inside the rollback simulation step, and a CBR-driven slot must be
// owned by one peer whose output is sent as that slot's input.
package cbr
