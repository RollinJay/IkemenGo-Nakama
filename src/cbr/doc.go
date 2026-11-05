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
// similarity band of the best match (with the base AI on, of those the
// comparison with it lets through; see the base AI below), ranks them by
// learned value, and plays the best back. When nothing is close enough it
// declines, and the character's own AI keeps control.
//
// # The base AI
//
// With Params.BaseAI set, as it is by default, the AI the character plays with
// is the base AI (see baseline.go), and it plays whenever the CBR layer has
// not taken over. This works with any kind of character AI: a character with
// an AI of its own keeps playing with it, and one without plays with the
// engine's AI, as it would without the CBR layer. The CBR layer takes over
// only with a move that has proved better than the base AI in the same kind of
// situation: the move's results when the CBR layer played it, graded like any
// chain (damage dealt, weighted by how the exchange ended, minus damage
// taken), beat the base AI's record there by the take-over margin, a setting
// of Low, Medium or High. The margin says how much better per exchange a move
// must have proved; at first a move must clear a higher bar, which comes down
// as it is played more (see Params.TakeoverMargin). The adapter pauses the
// character's AI only on the frames the CBR layer gives the input, so the AI
// plays again as soon as the move is over.
//
// A situation is the enemy's state type, the distance band, how the exchange
// stands, whether the character has control, its own stance (being hit and
// guarding apart) and the AI level (see baselineKey). Each play of a move is
// kept under the situation it was played in, whatever situation its case was
// recorded in, so a move proves itself only where its own plays were made
// (see playKey), and only once they number a few and were credited with a
// few exchanges' worth of results between them, so that one exchange proves
// nothing (see Params.proves). Plays whose command did not come out, and
// plays the character's own AI took over from, are not the move's plays. The
// base AI's record it must beat is the one of the situation and, once it
// holds enough results, also the one of the moments the move was on offer
// there, among the closest matches; until then the situation's alone, at
// twice the margin (see Store.against).
//
// The comparison comes before the similarity band (see chooseWhy), so a
// closer case the comparison turns down (a walk that matches the base AI's
// walk) does not push a move that proved better out of the choice. That
// lets the CBR layer take over during and after movement: while the base AI
// walks, runs or jumps with control, the CBR layer looks again every couple
// of frames and at once in each new situation (see driver.input), and a move
// that proved better at the distance the character comes to can take over
// there. In a movement without control (a dash, the start of a jump, a
// landing), only a command verified from that very state may take over (see
// moveWithoutControl and Verdict); otherwise the choice waits for the first
// frame with control.
//
// Now and then the CBR layer tries a move it has not proved yet, so that the
// move can prove itself: at about one in twenty of the moments it could try
// one (counting one each time the situation changes), more often for a move
// that looks promising, and a few times a round at most (see
// Params.TakeoverExplore). Moves whose command nobody was seen to input are
// also tested, apart from that (see Commands).
//
// The CBR layer learns moves from people playing the character, and from
// CPU play: by the engine's AI, by a character AI that inputs commands, and
// by a character AI that changes states directly (the most common kind),
// whose moves are checked as described under Commands. A driven player's
// base AI record, the results of the CBR layer's plays and what its test
// plays found are kept whether or not the player is also recorded
// (Engine.Record), since they are what the comparison and the tests need;
// recording decides which play new cases come from.
//
// An AI that keeps itself switched on in a variable (one a long command
// turns on, or one that notes AILevel once and then only checks its
// variable), or that never reads AILevel, cannot be paused; when such an AI
// changes the character's state during a move the CBR layer is playing (an
// attack, a cancel, a dash or a jump), which the adapter's labels show (see
// CharSnap.Entry), that move is usually dropped without counting against
// it, and a move such an AI keeps taking over from in a situation is not
// tried there again (see Params.TakeoverContestMax, and the adapter's notes
// in src/cbr_hook.go).
//
// # Commands
//
// When the adapter reports command activity, recording uses it: an action
// needs a command with a button or several steps; a state change inside a
// move with no such command is the move's own continuation. Commands that AI
// activates without inputs (AssertCommand, the engine AI's command cheat)
// are recorded with the command's canonical inputs, supplied by the adapter
// (see Motion).
//
// A character AI that changes states directly (ChangeState in state -1,
// say) enters its moves with no command at all. The adapter labels each
// state change of a character the CBR layer watches (see EntryKind): from
// the character's command and state files the engine knows which state
// changes a person's command can make, and when the character's own AI
// decides one, the engine checks at that very moment whether a person's
// command would have made the same change. When one would have
// (EntryReachable), the decision is recorded as a case of the character's
// AI (SrcCharacterAI), with that command's canonical inputs and the result
// the AI got with the move. When none would have (EntryCheat), only a
// direct state change reaches the move, which a person cannot make: it is
// cheating, and it never becomes a case, nor does anything in its exchange.
//
// A case whose command nobody was seen to input, a character AI's or a
// hypothesis (below), takes over only where the CBR layer's own plays have
// verified its command: the CBR layer inputs the command in a similar spot
// (the same state entered with the same command, with control or from the
// same state, in the same stance, with enough power; see Verdict), and the
// move must come out. These test plays come on top of the exploration, at most
// Params.VerifyPerRound a round for each character, and stop once the spots
// that come up are settled: verified, or blocked after three failures there
// (failed test plays, or moment checks that found no person's command
// open). Exploration may try such a case as well, and its play verifies the
// command the same way. A state no person's command leads into at all is
// blocked from the start, and such a case is never played in a blocked
// spot. A verified move still has to prove better than the base AI, and
// test plays count toward that proof. The CBR layer only ever inputs
// commands, so a move that only a direct state change reaches never comes
// out for it.
//
// Changes the engine could not check (EntryUnknown: a target worked out
// while the game runs, say), and all of them without the adapter's labels,
// are unexplained by any command: a character showing them has its
// AI-sourced cases for the match discarded, except those of the character's
// AI the engine checked, and the states it entered are kept as hypotheses.
// Once a command has been seen bringing such a state about often enough
// (see Params.HypothesisMinEvidence), a case is synthesized for it and
// tested the same way.
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
