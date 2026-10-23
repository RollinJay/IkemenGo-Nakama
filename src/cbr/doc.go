// Package cbr implements case-based reasoning (CBR) AI for IKEMEN GO.
//
// It is a reimplementation and refinement of the original CBR AI for
// IKEMEN GO by KDing (https://github.com/KDing0), who makes CBR AIs for
// many games. Comments in this package that mention the original build,
// or values recovered from it, mean KDing's CBR AI.
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
// the best back. When nothing is close enough it declines, and the
// character's own AI keeps control.
//
// # The base AI
//
// The character's own AI is the base AI, always (see baseline.go). This
// works with any kind of character AI: a character with an AI of its own
// keeps it as the base, and a character without one falls back to the
// engine's AI, as before. The CBR layer takes over only with a move that has
// proved better than that AI in the same kind of situation: the move's
// results when the CBR layer played it, graded like any chain (damage dealt,
// weighted by how the exchange ended, minus damage taken), beat the AI's
// record there by the take-over margin, a setting of Low, Medium or High.
// The margin says how much better per exchange a move must have proved; at
// first a move must clear a higher bar, which comes down as it is played
// more (see Params.TakeoverMargin). The adapter pauses the character's AI
// only on the frames the CBR layer gives the input, so the AI plays again as
// soon as the move is over. Now and then (at about one in twenty decision
// points, more often for a move that looks promising, up to a limit per
// round) the CBR layer tries a move it has not proved yet, so that it can.
// An AI that keeps itself switched on in a variable (one a long command
// turns on, or one that notes AILevel once and then only checks its
// variable), or that never reads AILevel, cannot be paused; when such an AI
// starts an attack during a move the CBR layer is playing, that move is
// usually dropped without counting against it (see the adapter's notes in
// src/cbr_hook.go).
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
// discarded and the states it entered are kept as hypotheses, which the CBR
// layer tests by exploration.
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
