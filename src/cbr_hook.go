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
//   char.go    actionPrepare():   cbrMaskAILevel() after the AssertSpecial reset
//
// Everything is inert until a script enables it with cbrEnable(true).

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
	rootID int32 // detects Turns member changes

	// Per round: configured is set once the slot's character has been
	// assigned (at round start, or when it first enters the fight, as a Tag
	// partner does); active is set when the CBR layer drives the slot; char
	// is the character's store key; mask keeps the character's own AI off
	// because that AI was seen changing states directly.
	configured bool
	active     bool
	char       string
	mask       bool

	// Per frame: driven is set when this frame's input came from the CBR
	// layer, with btn the input given, for the slot's key-controlled helpers.
	driven bool
	btn    [14]bool
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
// (replay_local.go) will be played back the same way, so its CPU players are
// the engine's own AI, and the CBR layer leaves the match exactly as the
// replay will reproduce it. Observation still runs in both, so they can be
// learned from.
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
// fire for a command of a character the CBR layer drives this round. On
// frames the CBR layer supplies the input it never fires, since it would
// play random motion commands on top of that input. On fallback frames it
// still plays the engine AI's moves, except commands longer than any move a
// person inputs: older character AI uses such commands as switches that
// turn itself on. Called from Char.command.
func cbrSuppressAICheat(c *Char, cl []Command) bool {
	s := cbrRootSlot(c)
	if s == nil || !s.active || !cbrCanDrive() {
		return false
	}
	return s.driven || len(cl) == 0 || len(cl[0].steps) > cbrMaxSynthSteps
}

// cbrMaskAILevel reports whether a character's AILevel trigger must read 0.
// While the CBR layer supplies its input, the character's own AI code stays
// off so it does not act on top of that input; when that AI was seen
// changing states directly, it also stays off on frames the CBR layer
// declines, and the engine AI is the fallback. Called from
// Char.actionPrepare, which sets the engine's NoAILevel flag.
func cbrMaskAILevel(c *Char) bool {
	s := cbrRootSlot(c)
	return s != nil && s.active && (s.driven || s.mask) && cbrCanDrive()
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
// command cheat under the same conditions Char.command applies.
func cbrActiveCommands(c *Char, cl *CommandList) []string {
	var out []string
	cheat := -1
	if c.controller < 0 && !c.asf(ASF_noaicheat) {
		cheat = int(c.cpucmd)
	}
	for i, cmds := range cl.Commands {
		if len(cmds) == 0 {
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
	if !cbrLive() {
		return
	}
	eng := sysCBR.eng
	cbrBuildSnapshot(&sysCBR.snap)
	s := &sysCBR.snap

	if !sysCBR.roundOpen && s.World.RoundState == 2 {
		for i := range sysCBR.slots {
			sl := &sysCBR.slots[i]
			sl.configured, sl.active, sl.mask = false, false, false
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
	for i := range sysCBR.slots {
		sl := &sysCBR.slots[i]
		// Driven play can reveal the character's own AI changing states
		// directly; keep it off from then on.
		if sl.active && !sl.mask && eng.ChangeStateAI(sl.char) {
			sl.mask = true
		}
		// Input for the next frame is decided anew in InputUpdate.
		sl.driven = false
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
	slot.char = name
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
		lv := slot.levels
		if lv.Execution == 0 {
			// Execution follows the CPU's AI level.
			lv.Execution = int(math.Round(float64(s.Players[pn].Root.AILevel)))
		}
		eng.Drive(pn, lv)
		slot.active = true
		slot.mask = eng.ChangeStateAI(name)
	}
}

// cbrInput is called from InputUpdate's AI branch. It returns the CBR
// layer's input for an AI-controlled character it drives, or ok=false to
// leave the engine's AI in control. The input is converted to left/right
// with the forward/back flip the engine applies this frame. A helper with
// its own command list (a key-controlled helper) gets the root's input, as
// a person's would.
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
	return btn, ok
}

// cbrMatchEnd saves what was learned. Deferred from runMatch.
func cbrMatchEnd() {
	sysCBR.roundOpen = false
	for i := range sysCBR.slots {
		s := &sysCBR.slots[i]
		s.driven, s.configured, s.active, s.mask, s.rootID = false, false, false, false, 0
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
	// cbrDrive(pn, execution, knowledge[, ambition]): puts player pn under CBR
	// control whenever it is CPU-controlled, with the given 1-8 dials.
	// Execution 0 follows the CPU's AI level; ambition 0 follows execution.
	// Execution and knowledge both at 8 is the level-9 tier.
	luaRegister(l, "cbrDrive", func(l *lua.LState) int {
		pn := int(numArg(l, 1)) - 1
		if pn < 0 || pn >= len(sysCBR.slots) {
			return 0
		}
		lv := cbr.Levels{Execution: int(numArg(l, 2)), Knowledge: int(numArg(l, 3))}
		if l.GetTop() >= 4 {
			lv.Ambition = int(numArg(l, 4))
		}
		sysCBR.slots[pn].drive = true
		sysCBR.slots[pn].levels = lv
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
