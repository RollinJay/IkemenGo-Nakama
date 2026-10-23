package main

// Engine adapter for the CBR AI (package cbr). This is the only file that
// touches both the engine and the CBR package: it fills cbr.Snapshot from
// engine state once per logical frame, routes AI input through the CBR
// layer, reports command activity, synthesizes each command's canonical
// inputs, and exposes the Lua API. The engine calls it from six places:
//
//   system.go  action():          cbrFrame() after globalCollision/globalTick
//   system.go  runMatch():        defer cbrMatchEnd()
//   input.go   InputUpdate:       cbrInput() in the AI branch
//   script.go  systemScriptInit:  cbrRegisterLua(l)
//   char.go    command():         cbrSuppressAICheat() in the AI cheat test
//   char.go    actionPrepare():   cbrPrepareAILevel() after the AssertSpecial reset
//
// The character's own AI is the base, always. The CBR layer takes over only
// with a move that has proved better than that AI in the same kind of
// situation (see package cbr, baseline.go), and pauses the AI only on the
// frames it gives the input. On those frames, while the character's code
// runs, AILevel and AILevelF (and Lua's aiLevel) read 0, the engine AI's
// command cheat does not fire, and the CBR layer's input replaces the engine
// AI's button jam. On every other frame the character's AI plays as it would
// without the CBR layer, and the CBR layer learns from those frames how the
// base AI does (probes, in baseline.go): from every action the base AI
// starts from control, whether the character's code makes it in the root or
// in a helper (a ChangeState redirected to the root), or the engine AI's
// button jam and command cheat do. What it cannot tell apart is listed under
// the limits below.
//
// Which AIs this pauses depends on how they are written:
//
//   - Paused: AI code that reads AILevel on every frame, in any state and in
//     CNS or ZSS alike, whether in the root or in its helpers (their own
//     AILevel or root,AILevel); AI that acts through commands gated on
//     AILevel, or through the engine AI's command cheat; and a character
//     with no AI of its own, which plays as the engine AI.
//   - Not paused: AI that keeps itself switched on in a variable, whether a
//     long command turns it on (an older AI's switch code, which the cheat
//     picks now and then on the frames the base AI plays) or it notes
//     AILevel once and then checks only its variable; and AI that never reads
//     AILevel (IsHomeTeam, TeamSide or a variable instead). Such an AI acts
//     on top of the CBR layer's input.
//
// When an AI that is not paused starts an attack from control during a move
// the CBR layer is playing, with no command active, the move is dropped
// (contested): it counts as neither an abort nor a failed execution, gets
// no credit, and the CBR layer waits for control to return before choosing
// again. An attack a helper starts with a redirected ChangeState shows a
// frame after its state number does (see lateChange in package cbr); it is
// judged by the commands active on the frame the helper made it, and when
// the base AI made it on its own frame just before the CBR layer began a
// move, that move is dropped the same way, and nothing counts against the
// AI either. Nothing else such an AI does during a move is detected, so the
// move is credited with it: a cancel without control, an attack on a frame
// a command is active (the CBR layer's own button press, say), or a state
// that is not an attack (a dash or a jump). When that keeps the move's own
// command from coming out, the play counts as a failed execution, and a
// case whose command fails three times, in more than half its plays, is not
// played again. An AI that switches off the moves people input while it is
// on makes every command the CBR layer inputs fail that way.
//
// The NoAILevel flag that pauses the AI is cleared again as soon as the
// frame's code has run (cbrFrame), and when the character's next frame
// starts without the mask, since WinMugen-version characters keep
// AssertSpecial flags through hitpause; Lua scripts and the next frame
// therefore read the real AILevel. On the first frame the AI plays after the
// CBR layer's, the engine reads, before the character's code runs, the
// AssertSpecial flags of the CBR layer's frame, when the AI was paused and
// asserted none: NoAIButtonJam as it resolves the input, and the flags that
// switch off its built-in walk, jump, crouch and guard as it moves the
// character on that input. cbrInput gives the AI back, for that frame, those
// of them it had on its last frame (see there), so an AI that asserts them
// only while AILevel reads above 0 and leaves the engine AI's jam on (as the
// stock ZSS Kung Fu Man's does) gets no random walk, jump or guard there.
//
// Limits:
//
//   - Mid-move cancels: an AI that is not paused and changes the character's
//     state during a move the CBR layer plays, without control in between
//     and with no command active, looks the same as the move's own change
//     into a later part, so the play is credited with what the AI did, or
//     counts as a failed execution when the AI keeps the move's command from
//     coming out (see TestMidMoveTakeOverIsCreditedToTheMove in package cbr).
//   - Helper-only attacks: an attack only a helper makes while the root goes
//     on with what it does (a striker, a projectile a helper fires) counts in
//     the result of whatever decision is under way, but is not itself
//     recorded as a decision of the base AI, nor seen as one that takes over
//     from a move the CBR layer plays; only changes of the root's state are.
//   - Helpers' state changes: one a helper makes with a redirected
//     ChangeState is seen only when the new state takes control away on its
//     first frame, the frame right after the state number changed (not
//     across hitpause or a pause), and when nothing else changed the state
//     first (the root's own built-in walk on that frame, say). The same
//     pattern from something other than a helper, such as an opponent's
//     TargetState into a state that is not a hit state, counts as the
//     character's own action, as it does when the opponent's code runs
//     before the character's.
//   - First CBR frame: on the first frame of a move the CBR layer plays, the
//     engine still reads the flags the AI asserted on its last frame, so the
//     AI's NoWalk, NoJump, NoCrouch or NoHardcodedKeys holds back the move's
//     own walk, jump or crouch for that frame, which then starts a frame
//     late. Flags the character set are never cleared. In the simulator's
//     runs of every kind of AI (aikinds_test.go), a third to a half of the
//     take-overs from control began by holding such a direction, and all of
//     those were commands that begin with down (a quarter circle, a
//     crouching kick), whose command still comes out; none was a walk or a
//     jump.
//   - An AI that resets itself whenever AILevel reads 0 starts over after
//     each take-over.
//   - Code meant for a person (gated on AILevel reading 0) runs on the CBR
//     layer's frames, which is what lets the CBR layer's commands work.
//
// Everything is inert until a script enables it with cbrEnable(true).
//
// The CBR AI is based on KDing's original CBR AI for IKEMEN GO
// (https://github.com/KDing0).

import (
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"

	"github.com/ikemen-engine/Ikemen-GO/src/cbr"
)

const cbrDataDir = "save/cbr"

type cbrSlot struct {
	record bool
	source cbr.Source
	drive  bool
	levels cbr.Levels
	margin int   // take-over margin setting: 1 Low, 2 Medium, 3 High
	rootID int32 // detects Turns member changes

	// Per round: configured is set once the slot's character has been
	// assigned (at round start, or when it first enters the fight, as a Tag
	// partner does); active is set when the CBR layer drives the slot.
	configured bool
	active     bool

	// Per frame: driven is set when this frame's input came from the CBR
	// layer, with btn the input given, for the slot's key-controlled helpers.
	// wasDriven is driven of the frame before; aiFlags holds the flags of
	// cbrAIFlags the root had at the end of the last frame its own AI played
	// (see cbrInput).
	driven    bool
	btn       [14]bool
	wasDriven bool
	aiFlags   AssertSpecialFlag
}

// cbrMoveFlags are the AssertSpecial flags that switch off the engine's
// built-in moves: walking, jumping, air jumping, crouching, standing up,
// braking, and all of them with guarding (NoHardcodedKeys). The engine reads
// them at the start of a character's frame (Char.actionPrepare), before the
// flags are reset and the character's code asserts them anew, so it reads
// the ones the character asserted on its frame before. cbrGuardFlags switch
// guarding off while standing, crouching or in the air; the built-in guard
// at the start of a frame goes by whether the character could guard at the
// end of the frame before (SCF_guard), which the engine worked out from them
// then. cbrAIFlags adds the engine AI's button jam switch (NoAIButtonJam),
// which the engine reads even earlier, as it resolves the AI's input.
const (
	cbrMoveFlags = ASF_nojump | ASF_noairjump | ASF_nowalk | ASF_nobrake |
		ASF_nocrouch | ASF_nostand | ASF_nohardcodedkeys
	cbrGuardFlags = ASF_nostandguard | ASF_nocrouchguard | ASF_noairguard
	cbrAIFlags    = cbrMoveFlags | cbrGuardFlags | ASF_noaibuttonjam
)

// cbrGuardOff reports whether flags switch guarding off in state type st.
func cbrGuardOff(flags AssertSpecialFlag, st StateType) bool {
	switch st {
	case ST_S:
		return flags&ASF_nostandguard != 0
	case ST_C:
		return flags&ASF_nocrouchguard != 0
	case ST_A:
		return flags&ASF_noairguard != 0
	}
	return false
}

type cbrRuntime struct {
	eng       *cbr.Engine
	enabled   bool
	snap      cbr.Snapshot
	roundOpen bool
	session   uint64
	slots     [MaxPlayerNo]cbrSlot
	lastErr   string
	// With an automatic source, which kinds of player are recorded.
	skipHumans, skipCPU bool
	// skipReplays: nothing is recorded during replay playback. replayID is
	// the replayed session being recorded, marked learned when its match
	// ends; replays holds the sessions already learned.
	skipReplays bool
	replayID    string
	replays     cbrReplayLog
	// masked holds the characters whose NoAILevel flag the adapter set, with
	// their IDs (see cbrPrepareAILevel).
	masked map[*Char]int32
}

var sysCBR cbrRuntime

// cbrLive reports whether live observation and driving may run. Online
// matches are excluded: a Nakama match, like any netplay session, sets
// sys.netConnection, and sys.rollback.session with rollback netcode
// (nakama_script.go). Rollback re-runs frames, so anything observed inside it
// would be seen twice, and in delay-based netplay both machines run the AI
// from stores that differ. Online matches are learned from their replays
// instead. The offline desync test, which also runs a rollback session, is
// excluded with them.
func cbrLive() bool {
	return sysCBR.enabled && sysCBR.eng != nil &&
		sys.rollback.session == nil && sys.netConnection == nil
}

// cbrCanDrive additionally rules out the matches that must play back from
// their recorded inputs alone. Replay playback re-runs CPU players' AI
// locally, and CBR input depends on this machine's store, so driving there
// would make the replay diverge. A local match being recorded
// (replay_local.go) will be played back the same way, so its CPU players
// play with their own AI alone, and the CBR layer leaves the match exactly as
// the replay will reproduce it. Observation still runs in both, so they can
// be learned from.
func cbrCanDrive() bool {
	return cbrLive() && sys.replayFile == nil && !localReplay.armed
}

func cbrEngine() *cbr.Engine {
	if sysCBR.eng == nil {
		// The CBR layer never runs in netplay, so its randomness need not be
		// reproducible across machines.
		sysCBR.eng = cbr.New(cbr.DefaultParams(), cbrDataDir, time.Now().UnixNano())
	}
	return sysCBR.eng
}

// cbrCharKey identifies a character's learned data: its definition file,
// which, unlike the display name, is unique per installed character.
func cbrCharKey(c *Char) string {
	if def := c.gi().def; def != "" {
		return strings.ToLower(filepath.ToSlash(def))
	}
	return c.name
}

// Replays (a match replay file, a lobby match watched live, or a netplay
// session's replay) are recorded like live play, with two differences. Human
// players are recorded as online play (lobby) unless the match was played
// offline; a replay on this machine proves nothing about how it was made, so
// none is trusted as ranked. And each replayed session is learned once: it is
// identified by its seed, start times and stage, which the live stream and
// the saved file of one match share, and the sessions learned are kept in
// save/cbr/learned_replays.txt. A local match recorded live while it is saved
// as a replay counts as that replay learned.

const (
	cbrLearnedReplaysFile = "learned_replays.txt"
	// The list keeps the newest sessions: past cbrLearnedReplaysMax it is
	// cut to the newest cbrLearnedReplaysKeep.
	cbrLearnedReplaysMax  = 20000
	cbrLearnedReplaysKeep = 10000
)

type cbrReplayLog struct {
	dir    string // cbrDataDir when empty
	loaded bool
	ids    map[string]bool
	order  []string
}

func (r *cbrReplayLog) folder() string {
	if r.dir != "" {
		return r.dir
	}
	return cbrDataDir
}

func (r *cbrReplayLog) path() string {
	return filepath.Join(r.folder(), cbrLearnedReplaysFile)
}

func (r *cbrReplayLog) load() {
	if r.loaded {
		return
	}
	r.loaded = true
	r.ids = map[string]bool{}
	data, err := os.ReadFile(r.path())
	if err != nil {
		return
	}
	for _, id := range strings.Fields(string(data)) {
		if !r.ids[id] {
			r.ids[id] = true
			r.order = append(r.order, id)
		}
	}
}

func (r *cbrReplayLog) has(id string) bool {
	r.load()
	return r.ids[id]
}

func (r *cbrReplayLog) add(id string) error {
	r.load()
	if r.ids[id] {
		return nil
	}
	r.ids[id] = true
	r.order = append(r.order, id)
	if err := os.MkdirAll(r.folder(), 0o755); err != nil {
		return err
	}
	if len(r.order) > cbrLearnedReplaysMax {
		for _, old := range r.order[:len(r.order)-cbrLearnedReplaysKeep] {
			delete(r.ids, old)
		}
		r.order = append([]string(nil), r.order[len(r.order)-cbrLearnedReplaysKeep:]...)
		return os.WriteFile(r.path(), []byte(strings.Join(r.order, "\n")+"\n"), 0o644)
	}
	f, err := os.OpenFile(r.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// cbrSessionID identifies a replay session by what its header records.
func cbrSessionID(seed, preMatchTime, matchTime int32, stage string) string {
	f := fnv.New64a()
	stage = strings.ToLower(strings.ReplaceAll(stage, "\\", "/"))
	fmt.Fprintf(f, "%d|%d|%d|%s", seed, preMatchTime, matchTime, stage)
	return fmt.Sprintf("%016x", f.Sum64())
}

// cbrReplayID identifies the session a replay is playing, or "" for a
// netplay session's replay, which carries no session header.
func cbrReplayID() string {
	rf := sys.replayFile
	if rf == nil || rf.liveBuffer == nil {
		return ""
	}
	h := rf.liveBuffer.Header()
	if h == nil {
		return ""
	}
	return cbrSessionID(h.Seed, h.PreMatchTime, h.MatchTime, h.Stage)
}

// cbrLocalRecordingID identifies the session of a local match being saved as
// a replay (replay_local.go), as its replay will identify it, or "".
func cbrLocalRecordingID() string {
	r := &localReplay
	if !r.open {
		return ""
	}
	stage := ""
	if sys.stage != nil {
		stage = sys.stage.def
	}
	return cbrSessionID(r.seed, r.preMatchTime, r.matchTime, stage)
}

// cbrPlaybackHumanSource is the automatic source of a human player in a
// replay: local for a match played offline, lobby otherwise.
func cbrPlaybackHumanSource() cbr.Source {
	if rf := sys.replayFile; rf != nil && rf.local {
		return cbr.SrcHumanLocal
	}
	return cbr.SrcHumanLobby
}

// cbrPlaybackRecordable reports whether the replay playing may be recorded:
// replays are not switched off, and its session was not learned before.
func cbrPlaybackRecordable() bool {
	if sysCBR.skipReplays {
		return false
	}
	id := cbrReplayID()
	if id == "" {
		return true
	}
	if sysCBR.replays.has(id) {
		return false
	}
	sysCBR.replayID = id
	return true
}

// cbrRootSlot returns the slot of a character's player, for roots and
// helpers alike.
func cbrRootSlot(c *Char) *cbrSlot {
	if c == nil || c.playerNo < 0 || c.playerNo >= len(sysCBR.slots) {
		return nil
	}
	return &sysCBR.slots[c.playerNo]
}

// cbrSuppressAICheat reports whether the engine AI's command cheat must not
// fire for a command of a character the CBR layer drives this round: on
// frames the CBR layer supplies the input, since it would play random motion
// commands on top of that input. On every other frame the base AI plays,
// cheat included, so an older AI can still switch itself on with a long
// command. Called from Char.command.
func cbrSuppressAICheat(c *Char, _ []Command) bool {
	s := cbrRootSlot(c)
	return s != nil && s.active && s.driven && cbrCanDrive()
}

// cbrMaskAILevel reports whether a character's AILevel trigger must read 0:
// on frames the CBR layer supplies the input, so the character's own AI
// code pauses and does not act on top of it. A helper follows its root.
func cbrMaskAILevel(c *Char) bool {
	s := cbrRootSlot(c)
	return s != nil && s.active && s.driven && cbrCanDrive()
}

// cbrPrepareAILevel applies cbrMaskAILevel at the start of a character's
// frame. Called from Char.actionPrepare after the AssertSpecial reset. On a
// frame the CBR layer gives the input it sets the engine's NoAILevel flag,
// and notes that it did unless the character had asserted the flag itself.
// On any other frame it clears the flag it set: the reset does not, during
// hitpause, for WinMugen-version characters, which keep AssertSpecial flags
// through hitpause.
func cbrPrepareAILevel(c *Char) {
	if !cbrMaskAILevel(c) {
		cbrUnmaskAILevel(c)
		return
	}
	if !c.asf(ASF_noailevel) {
		if sysCBR.masked == nil {
			sysCBR.masked = map[*Char]int32{}
		}
		sysCBR.masked[c] = c.id
	}
	c.setASF(ASF_noailevel)
}

// cbrUnmaskAILevel clears the NoAILevel flag the adapter set on a character,
// if it did.
func cbrUnmaskAILevel(c *Char) {
	if len(sysCBR.masked) == 0 {
		return
	}
	if id, ok := sysCBR.masked[c]; ok {
		delete(sysCBR.masked, c)
		if c.id == id {
			c.unsetASF(ASF_noailevel)
		}
	}
}

// cbrUnmaskAll clears every NoAILevel flag the adapter set, once the frame's
// character code has run, so that what reads AILevel between frames (Lua
// scripts, such as the debug keys' AI switch) reads the real level.
func cbrUnmaskAll() {
	for c, id := range sysCBR.masked {
		if c.id == id {
			c.unsetASF(ASF_noailevel)
		}
	}
	clear(sysCBR.masked)
}

// cbrScale converts a character's local position units to 320-wide game
// space, so distances compare across characters of any localcoord.
func cbrScale(c *Char) float32 {
	if sys.gameWidth <= 0 {
		return c.localscl
	}
	return c.localscl * 320 / float32(sys.gameWidth)
}

func cbrStateType(st StateType) cbr.StateType {
	switch st {
	case ST_S:
		return cbr.StStand
	case ST_C:
		return cbr.StCrouch
	case ST_A:
		return cbr.StAir
	case ST_L:
		return cbr.StLying
	}
	return cbr.StUnknown
}

func cbrMoveType(mt MoveType) cbr.MoveType {
	switch mt {
	case MT_I:
		return cbr.MtIdle
	case MT_A:
		return cbr.MtAttack
	case MT_H:
		return cbr.MtHit
	}
	return cbr.MtUnknown
}

// cbrReadInput reads a root's resolved input for this frame from its input
// buffer. Back and forward are taken from the buffer's own B/F, which the
// engine derived with the forward/back flip in force that frame, so they
// match how the character's commands read the input.
func cbrReadInput(c *Char) cbr.InputBits {
	if c.cmd == nil || c.playerNo < 0 || c.playerNo >= len(c.cmd) {
		return 0
	}
	ib := c.cmd[c.playerNo].Buffer
	if ib == nil {
		return 0
	}
	btn := cbr.EngineButtons{
		ib.Ub > 0, ib.Db > 0, ib.Bb > 0, ib.Fb > 0,
		ib.ab > 0, ib.bb > 0, ib.cb > 0, ib.xb > 0, ib.yb > 0, ib.zb > 0,
		ib.sb > 0, ib.db > 0, ib.wb > 0, ib.mb > 0,
	}
	// With facingRight set, FromEngine reads L as back and R as forward.
	return cbr.FromEngine(btn, true)
}

// cbrCommandList returns a root's own command list.
func cbrCommandList(c *Char) *CommandList {
	if c.cmd == nil || c.playerNo < 0 || c.playerNo >= len(c.cmd) {
		return nil
	}
	return &c.cmd[c.playerNo]
}

// cbrActiveCommands lists a root's commands active this frame: completed by
// input or asserted (buffer time left), or chosen by the engine AI's
// command cheat under the same conditions Char.command applies. Commands
// longer than any move a person inputs are left out: older character AI
// uses them as switches that turn itself on, and one active by chance must
// not pass for the command behind a move.
func cbrActiveCommands(c *Char, cl *CommandList) []string {
	var out []string
	cheat := -1
	if c.controller < 0 && !c.asf(ASF_noaicheat) {
		cheat = int(c.cpucmd)
	}
	for i, cmds := range cl.Commands {
		if len(cmds) == 0 || len(cmds[0].steps) > cbrMaxSynthSteps {
			continue
		}
		active := false
		for j := range cmds {
			if cmds[j].curbuftime > 0 {
				active = true
				break
			}
		}
		if !active && i == cheat && !cbrSuppressAICheat(c, cmds) {
			steps := cmds[0].steps
			active = len(steps) > 1 || (len(steps) > 0 && len(steps[0].keys) > 1)
		}
		if active {
			out = append(out, cmds[0].name)
		}
	}
	return out
}

func cbrFillChar(d *cbr.CharSnap, c *Char, root bool) {
	scl := cbrScale(c)
	*d = cbr.CharSnap{
		ID:           c.id,
		PlayerNo:     c.playerNo,
		TeamSide:     c.teamside,
		HelperID:     c.helperId,
		IsHelper:     c.helperIndex != 0,
		Pos:          [3]float32{c.pos[0] * scl, c.pos[1] * scl, c.pos[2] * scl},
		Vel:          [3]float32{c.vel[0] * scl, c.vel[1] * scl, c.vel[2] * scl},
		FacingRight:  c.facing > 0,
		StateNo:      c.ss.no,
		PrevStateNo:  c.ss.prevno,
		StateTime:    c.ss.time,
		StateType:    cbrStateType(c.ss.stateType),
		MoveType:     cbrMoveType(c.ss.moveType),
		Ctrl:         c.ctrl(),
		MoveHit:      c.moveHit(),
		MoveGuarded:  c.moveGuarded(),
		MoveContact:  c.moveContact(),
		Life:         c.life,
		LifeMax:      c.lifeMax,
		Power:        c.power,
		PowerMax:     c.powerMax,
		Dizzy:        c.dizzyPoints,
		DizzyMax:     c.dizzyPointsMax,
		Guard:        c.guardPoints,
		GuardMax:     c.guardPointsMax,
		RedLife:      c.redLife,
		Juggle:       c.juggle,
		AnimNo:       c.animNo,
		HitboxActive: c.ss.moveType == MT_A && c.hitdef.attr > 0,
		Alive:        c.life > 0 && !c.csf(CSF_destroy),
	}
	if c.ss.moveType == MT_H && c.ghv.hittime > 0 {
		if c.ghv.guarded {
			d.BlockStun = c.ghv.hittime
		} else {
			d.HitStun = c.ghv.hittime
		}
	}
	d.HitPause = c.hitPauseTime
	// The last hitter's player number. GetHitVar keeps it after the hit ends
	// and holds -2 until the first hit of the round; helpers and projectiles
	// carry their owner's player number.
	if c.ghv.playerno >= 0 {
		d.HitBy = c.ghv.playerno + 1
	}
	for i := range d.Pos {
		d.Pos[i] = cbrFinite(d.Pos[i])
		d.Vel[i] = cbrFinite(d.Vel[i])
	}
	if root {
		d.Input = cbrReadInput(c)
		// The AI level as configured, for a CPU-controlled root.
		if c.playerNo >= 0 && c.playerNo < len(sys.aiLevel) && c.controller < 0 {
			d.AILevel = sys.aiLevel[c.playerNo]
		}
		if c.playerNo >= 0 && c.playerNo < len(sysCBR.slots) {
			d.CBRDriven = sysCBR.slots[c.playerNo].driven
		}
		if cl := cbrCommandList(c); cl != nil {
			d.Commands, d.CmdKnown = cbrActiveCommands(c, cl), true
		}
		if vars := sysCBR.eng.Params().Vars; len(vars) > 0 {
			d.IntVars = map[int32]int32{}
			d.FloatVars = map[int32]float32{}
			for _, v := range vars {
				if v.Float {
					d.FloatVars[v.Index] = c.cnsfvar[v.Index]
				} else {
					d.IntVars[v.Index] = c.cnsvar[v.Index]
				}
			}
		}
	}
}

func cbrFinite(v float32) float32 {
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		return 0
	}
	return v
}

// cbrPresent reports whether a root takes part in the fight: not a Turns
// member waiting (or still loading in the background), and not a character
// without a team side, such as one attached to the stage.
func cbrPresent(c *Char) bool {
	return c != nil && c.teamside >= 0 && !c.scf(SCF_disabled) && !c.scf(SCF_standby) && !c.csf(CSF_destroy)
}

func cbrBuildSnapshot(dst *cbr.Snapshot) {
	w := &dst.World
	w.Tick = int64(sys.matchTime)
	w.RoundNo = sys.roundNo
	w.RoundState = sys.roundState()
	w.Intro = sys.intro
	w.CurRoundTime = sys.curRoundTime
	w.MaxRoundTime = sys.maxRoundTime
	camScl := float32(1)
	if sys.gameWidth > 0 {
		camScl = 320 / float32(sys.gameWidth)
	}
	w.StageLeft = sys.cam.minLeft * camScl
	w.StageRight = sys.cam.maxRight * camScl
	w.ComboCount = sys.comboCount
	w.SuperPause = sys.supertime > 0
	w.Pause = sys.pausetime > 0
	w.WinTeam = int32(sys.winTeam)

	if cap(dst.Players) < len(sys.chars) {
		dst.Players = make([]cbr.PlayerSnap, len(sys.chars))
	}
	dst.Players = dst.Players[:len(sys.chars)]
	for pn := range sys.chars {
		ps := &dst.Players[pn]
		helpers := ps.Helpers[:0]
		*ps = cbr.PlayerSnap{Helpers: helpers}
		p := sys.chars[pn]
		if len(p) == 0 || !cbrPresent(p[0]) {
			continue
		}
		root := p[0]
		ps.Present = true
		ps.Name = cbrCharKey(root)
		if pn < len(sysCBR.slots) {
			s := &sysCBR.slots[pn]
			if s.rootID != 0 && s.rootID != root.id {
				sysCBR.session++
			}
			s.rootID = root.id
		}
		cbrFillChar(&ps.Root, root, true)
		for _, h := range p[1:] {
			if h == nil || h.csf(CSF_destroy) {
				continue
			}
			var hs cbr.CharSnap
			cbrFillChar(&hs, h, false)
			ps.Helpers = append(ps.Helpers, hs)
		}
	}
	w.Session = sysCBR.session
}

// cbrFrame is called once per logical frame, after collisions resolve.
func cbrFrame() {
	cbrUnmaskAll()
	if !cbrLive() {
		return
	}
	eng := sysCBR.eng
	cbrBuildSnapshot(&sysCBR.snap)
	s := &sysCBR.snap

	if !sysCBR.roundOpen && s.World.RoundState == 2 {
		for i := range sysCBR.slots {
			sl := &sysCBR.slots[i]
			sl.configured, sl.active, sl.wasDriven, sl.aiFlags = false, false, false, 0
		}
		sysCBR.roundOpen = true
	}
	if sysCBR.roundOpen {
		// Characters join at round start, or when they first enter the
		// fight during the round (a Tag partner tagging in).
		for pn := range s.Players {
			if pn < len(sysCBR.slots) && s.Players[pn].Present && !sysCBR.slots[pn].configured {
				cbrConfigure(pn, s)
			}
		}
	}
	eng.Frame(s)
	if sysCBR.roundOpen && s.World.RoundState >= 3 {
		eng.RoundEnd(s)
		sysCBR.roundOpen = false
	}
	cbrEndFrame()
}

// cbrEndFrame ends the frame for every slot: it notes, after a frame the
// character's own AI played, which of the flags in cbrAIFlags the AI had
// asserted, for the first frame it plays after the CBR layer's (see
// cbrInput). Input for the next frame is decided anew in InputUpdate.
func cbrEndFrame() {
	for i := range sysCBR.slots {
		sl := &sysCBR.slots[i]
		if !sl.driven && i < len(sys.chars) && len(sys.chars[i]) > 0 && sys.chars[i][0] != nil {
			sl.aiFlags = sys.chars[i][0].assertFlag & cbrAIFlags
		}
		sl.wasDriven, sl.driven = sl.driven, false
	}
}

// cbrConfigure assigns player pn's character and applies its slot's
// settings for the rest of the round.
func cbrConfigure(pn int, s *cbr.Snapshot) {
	eng := sysCBR.eng
	slot := &sysCBR.slots[pn]
	slot.configured = true
	if !slot.record && !slot.drive {
		return
	}
	name := s.Players[pn].Name
	if pn < len(sys.chars) && len(sys.chars[pn]) > 0 {
		eng.SetMotions(name, cbrMotions(sys.chars[pn][0]))
	}
	opp := ""
	if ei := s.Enemy(pn); ei >= 0 {
		opp = s.Players[ei].Name
	}
	if err := eng.SetPlayer(pn, name, opp); err != nil {
		sysCBR.lastErr = err.Error()
		return
	}
	cpu := s.Players[pn].Root.AILevel > 0
	if slot.record {
		src := slot.source
		record := true
		if src == cbr.SrcUnknown {
			src = cbr.SrcHumanLocal
			record = !sysCBR.skipHumans
			if cpu {
				src = cbr.SrcEngineAI
				record = !sysCBR.skipCPU
			} else if sys.replayFile != nil {
				src = cbrPlaybackHumanSource()
			}
		}
		if record {
			if sys.replayFile != nil {
				record = cbrPlaybackRecordable()
			} else if id := cbrLocalRecordingID(); id != "" {
				// The match is being saved as a replay: what is learned now is
				// that replay's content, so playing it back adds nothing new.
				sysCBR.replayID = id
			}
		}
		if record {
			eng.Record(pn, src)
		}
	}
	if slot.drive && cpu && cbrCanDrive() {
		cbrStartDrive(pn, slot, s.Players[pn].Root.AILevel)
	}
}

// cbrStartDrive puts player pn under CBR control for the round with its
// slot's settings, its character already assigned to the engine.
func cbrStartDrive(pn int, slot *cbrSlot, aiLevel float32) {
	eng := sysCBR.eng
	lv := slot.levels
	if lv.Execution == 0 {
		// Execution follows the CPU's AI level.
		lv.Execution = int(math.Round(float64(aiLevel)))
	}
	if err := eng.Drive(pn, lv); err != nil {
		sysCBR.lastErr = err.Error()
		return
	}
	eng.SetTakeoverMargin(pn, slot.margin)
	slot.active = true
}

// cbrInput is called from InputUpdate's AI branch. It returns the CBR
// layer's input for an AI-controlled character it drives, or ok=false to
// leave the character's own AI in control, with the engine AI's button jam
// as the character leaves it. The input is converted to left/right with the
// forward/back flip the engine applies this frame. A helper with its own
// command list (a key-controlled helper) gets the root's input, as a
// person's would.
//
// On the first frame the character's own AI plays after the CBR layer's,
// the engine reads AssertSpecial flags the character asserted on the frame
// before, when its AI was paused and asserted nothing: InputUpdate reads
// NoAIButtonJam, and Char.actionPrepare the flags of cbrMoveFlags, and
// whether the character could guard, before it moves the character on its
// input (walk, jump, crouch, guard). An AI that asserts them only while
// AILevel reads above 0 and leaves the button jam on would get random walks,
// jumps and guards there that it never makes without the CBR layer. So on
// that frame cbrInput gives the character back the flags its AI had at the
// end of its last frame, and takes guarding away if those flags switched it
// off in the character's state type. It only ever adds such restrictions,
// never clearing a flag the character has, and the usual reset in
// actionPrepare clears them before the character's code runs, which asserts
// its own (the engine works out guarding anew after that code). As
// NoAIButtonJam is read before cbrInput runs, the jam is kept off by
// returning no buttons with ok=true; the frame stays the AI's.
func cbrInput(c *Char) (btn [14]bool, ok bool) {
	s := cbrRootSlot(c)
	if s == nil || !s.active || !cbrCanDrive() {
		return btn, false
	}
	if c.helperIndex != 0 {
		return s.btn, s.driven
	}
	if !cbrPresent(c) {
		return btn, false // waiting out of the fight (a Tag partner)
	}
	btn, ok = sysCBR.eng.Input(c.playerNo, !c.fbFlip)
	s.driven, s.btn = ok, btn
	if !ok && s.wasDriven {
		c.setASF(s.aiFlags & cbrMoveFlags)
		if cbrGuardOff(s.aiFlags, c.ss.stateType) {
			c.unsetSCF(SCF_guard)
		}
		if s.aiFlags&ASF_noaibuttonjam != 0 {
			return [14]bool{}, true
		}
	}
	return btn, ok
}

// cbrMatchEnd saves what was learned. Deferred from runMatch.
func cbrMatchEnd() {
	sysCBR.roundOpen = false
	cbrUnmaskAll()
	for i := range sysCBR.slots {
		s := &sysCBR.slots[i]
		s.driven, s.configured, s.active, s.rootID = false, false, false, 0
		s.wasDriven, s.aiFlags = false, 0
	}
	id := sysCBR.replayID
	sysCBR.replayID = ""
	if sysCBR.eng == nil {
		return
	}
	if err := sysCBR.eng.MatchEnd(); err != nil {
		sysCBR.lastErr = err.Error()
		LogMessage("CBR: %v", err)
		return
	}
	if id != "" {
		if err := sysCBR.replays.add(id); err != nil {
			sysCBR.lastErr = err.Error()
			LogMessage("CBR: %v", err)
		}
	}
}

func cbrSourceFromString(s string) cbr.Source {
	switch s {
	case "human", "local":
		return cbr.SrcHumanLocal
	case "lobby":
		return cbr.SrcHumanLobby
	case "ranked":
		return cbr.SrcRanked
	case "ranked_top":
		return cbr.SrcRankedTop
	case "trial":
		return cbr.SrcTrial
	case "authored":
		return cbr.SrcAuthoredAI
	case "engine":
		return cbr.SrcEngineAI
	}
	return cbr.SrcUnknown
}

func cbrRegisterLua(l *lua.LState) {
	// cbrEnable(on): turns the layer on or off.
	luaRegister(l, "cbrEnable", func(l *lua.LState) int {
		sysCBR.enabled = boolArg(l, 1)
		if sysCBR.enabled {
			cbrEngine()
		}
		return 0
	})
	// cbrRecord(pn[, source]): records player pn (1-based) from the next
	// round. source: "human", "lobby", "ranked", "ranked_top", "trial",
	// "authored", "engine"; omitted picks engine for a CPU-controlled player
	// and human for a person (lobby in a replay of an online match), subject
	// to cbrRecordWho.
	luaRegister(l, "cbrRecord", func(l *lua.LState) int {
		pn := int(numArg(l, 1)) - 1
		if pn < 0 || pn >= len(sysCBR.slots) {
			return 0
		}
		sysCBR.slots[pn].record = true
		sysCBR.slots[pn].source = cbr.SrcUnknown
		if l.GetTop() >= 2 {
			sysCBR.slots[pn].source = cbrSourceFromString(strArg(l, 2))
		}
		return 0
	})
	// cbrRecordWho(humans, cpu[, replays]): with an automatic source, whether
	// human and CPU-controlled players are recorded; and whether anything is
	// recorded during replay playback (match replays and lobby matches
	// watched live). All are by default.
	luaRegister(l, "cbrRecordWho", func(l *lua.LState) int {
		sysCBR.skipHumans = !boolArg(l, 1)
		sysCBR.skipCPU = !boolArg(l, 2)
		sysCBR.skipReplays = l.GetTop() >= 3 && !boolArg(l, 3)
		return 0
	})
	// cbrDrive(pn, execution, knowledge[, ambition[, margin]]): puts player pn
	// under CBR control whenever it is CPU-controlled, with the given 1-8
	// dials. Execution 0 follows the CPU's AI level; ambition 0 follows
	// execution. Execution and knowledge both at 8 is the level-9 tier. The
	// character's own AI stays the base, and margin is how much better a move
	// must have proved for the CBR layer to take over with it: 1 Low, 2 Medium
	// (also when missing or not 1-3), 3 High.
	luaRegister(l, "cbrDrive", func(l *lua.LState) int {
		pn := int(numArg(l, 1)) - 1
		if pn < 0 || pn >= len(sysCBR.slots) {
			return 0
		}
		lv := cbr.Levels{Execution: int(numArg(l, 2)), Knowledge: int(numArg(l, 3))}
		if l.GetTop() >= 4 {
			lv.Ambition = int(numArg(l, 4))
		}
		margin := 2
		if v, ok := l.Get(5).(lua.LNumber); ok && v >= 1 && v <= 3 {
			margin = int(v)
		}
		sysCBR.slots[pn].drive = true
		sysCBR.slots[pn].levels = lv
		sysCBR.slots[pn].margin = margin
		return 0
	})
	// cbrStop(pn): stops recording and driving player pn.
	luaRegister(l, "cbrStop", func(l *lua.LState) int {
		pn := int(numArg(l, 1)) - 1
		if pn < 0 || pn >= len(sysCBR.slots) {
			return 0
		}
		sysCBR.slots[pn] = cbrSlot{}
		if sysCBR.eng != nil {
			sysCBR.eng.StopRecord(pn)
			sysCBR.eng.StopDrive(pn)
		}
		return 0
	})
	// cbrStatus(): enabled flag and the last error, if any.
	luaRegister(l, "cbrStatus", func(l *lua.LState) int {
		t := l.NewTable()
		t.RawSetString("enabled", lua.LBool(sysCBR.enabled))
		t.RawSetString("error", lua.LString(sysCBR.lastErr))
		l.Push(t)
		return 1
	})
}
