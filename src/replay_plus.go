// Replay++ by jay_ts (https://github.com/saintjr2004/Ikemen-GO-ReplayPlusPlus): fast forward,
// rewind, pause, frame stepping and taking over a replay. Ported to this fork:
// snapshots are freed through the engine's state pools, not Go arenas.

// REPLAY++ MODULE by jay_ts
// Go Functions

package main

import (
	"io"
	"log"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// attach to the main-thread task queue because you do not touch the lua VM from another thread
func init() {
	var attach func()
	attach = func() {
		sys.mainThreadTask <- func() {
			if sys.luaLState == nil {
				go func() { time.Sleep(20 * time.Millisecond); attach() }()
				return
			}
			replayRegister(sys.luaLState)
		}
	}
	go attach()

	// pump input callbacks so we can still move thru the pause menu even
	// though gameplay is stopped
	go replayPausedInputPump()
}

var replayPumpPending atomic.Bool

// pump input detection during pause menu
func replayPausedInputPump() {
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	for range ticker.C {
		if !replayPumpPending.CompareAndSwap(false, true) {
			continue
		}
		select {
		case sys.mainThreadTask <- func() {
			defer replayPumpPending.Store(false)
			if sys.luaLState != nil && sys.replayFile != nil && sys.replayFile.file != nil &&
				sys.paused && (rp.playbackPaused || sys.motif.me.active) {
				if _, err := ExecFunc(sys.luaLState, "replayPumpTick"); err != nil {
					log.Printf("nuts. replay lua input poll failed: %v", err)
				}
			}
		}:
		default:
			replayPumpPending.Store(false)
		}
	}
}

// snapshot struct
type replaySnapshot struct {
	state  *GameState
	id     int
	tick   int
	round  int32
	match  int32
	offset int64
	bits   [REPLAY_NUM_INPUTS]InputBits
	axes   [REPLAY_NUM_INPUTS][6]int8
}

// replay manager struct definition
type replayManager struct {
	file                 *ReplayFile
	controllerSlot       int
	originalReplay       *os.File
	takeoverReplay       *os.File
	activeSide           int
	pendingSide          int
	menuOrigin           *replaySnapshot
	history              []*replaySnapshot
	nextID               int
	lastLoadID           int
	lastRecordedTick     int
	lastPolledFrame      int32
	takeoverTicks        int
	maxTakeoverTicks     int
	resetPending         bool
	roundFadePaused      bool
	historyInterval      int
	maxHistory           int
	rewindFrames         int
	rewindRepeat         int
	fastForwardFrames    int
	fastForwardRepeat    int
	holdDelay            int
	fastForwardRate      float64
	seekTarget           int
	seekDirection        int
	seekFromHold         bool
	menuWasPaused        bool
	prevAccel            float32
	prevPause            bool
	menuFrozen           bool
	menuOverride         bool
	exitRequested        bool
	exitReady            bool
	originalPause        bool
	originalEnabled      bool
	originalEscOpens     bool
	originalPM           bool
	unfrozenNextTime     float32
	playbackPaused       bool
	rewindAutoPause      bool
	seeking              bool
	menuCommand          int
	pauseCommand         int
	rewindCommand        int
	fastForwardCommand   int
	menuKeyWasDown       bool
	pauseKeyWasDown      bool
	rewindHeldTicks      int
	fastForwardHeldTicks int
	rewindPressedAt      time.Time
	forwardPressedAt     time.Time
	rewindRepeatedAt     time.Time
	forwardRepeatedAt    time.Time

	// stuff to hide the default fast-forward buttons
	fastForwardMasked bool
	savedKeys         []KeyConfig
	savedJoysticks    []KeyConfig
	menuHeld          [14]int
	menuUsed          [14]bool
}

// replay manager variable
var rp = replayManager{
	controllerSlot:     1,
	nextID:             10000,
	lastPolledFrame:    -1,
	maxTakeoverTicks:   1800,
	historyInterval:    20,
	maxHistory:         180,
	rewindFrames:       120,
	rewindRepeat:       8,
	fastForwardFrames:  120,
	fastForwardRepeat:  8,
	holdDelay:          12,
	fastForwardRate:    4,
	menuCommand:        13, // m
	pauseCommand:       10, // s
	rewindCommand:      2,  // L
	fastForwardCommand: 3,  // R
}

// check if conditions are valid for extended replay features
func replayMatchValid() bool {
	// Replays played from files only: a live watched stream (liveBuffer) only
	// has the frames published so far, and replay_live.go's catch-up and stall
	// handling own its playback.
	return sys.replayFile != nil && sys.replayFile.file != nil && sys.replayFile.liveBuffer == nil &&
		sys.gameRunning && !sys.postMatchFlg && !sys.fightLoopEnd &&
		!sys.endMatch && !sys.matchOver() &&
		sys.stage != nil && sys.rollback.session == nil && sys.netConnection == nil &&
		!sys.motif.vi.active && !sys.motif.wi.active && !sys.motif.co.active &&
		!sys.storyboard.active && !sys.motif.di.active && !sys.motif.ch.active
}

// free snapshot from memory
func replayFreeSnapshot(s *replaySnapshot) {
	if s == nil {
		return
	}
	sys.savePool.Free(s.id)
	s.state = nil
}

// remove temp copy of replay
func replayDiscardTemp() {
	if rp.originalReplay != nil && sys.replayFile != nil {
		sys.replayFile.file = rp.originalReplay
	}
	if rp.takeoverReplay != nil {
		name := rp.takeoverReplay.Name()
		_ = rp.takeoverReplay.Close()
		_ = os.Remove(name)
	}
	rp.originalReplay = nil
	rp.takeoverReplay = nil
}

// make temp copy of replay
func replayMakeTemp() bool {
	if sys.replayFile == nil || sys.replayFile.file == nil {
		return false
	}
	orig := sys.replayFile.file
	pos, err := orig.Seek(0, io.SeekCurrent)
	if err != nil {
		return false
	}
	tmp, err := os.CreateTemp("", "ikemen-jayreplay-takeover-*.tmp")
	if err != nil {
		return false
	}
	if _, err = orig.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(tmp, orig)
	}
	_, seekErr := orig.Seek(pos, io.SeekStart)
	if err == nil {
		err = seekErr
	}
	if err == nil {
		_, err = tmp.Seek(pos, io.SeekStart)
	}
	if err != nil {
		name := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(name)
		log.Printf("nuts. unable to make takeover copy: %v", err)
		return false
	}
	rp.originalReplay = orig
	rp.takeoverReplay = tmp
	sys.replayFile.file = tmp
	return true
}

type replayCommonStateOverride struct {
	key      string
	path     string
	previous []string
	hadKey   bool
	active   bool
}

var replayCommonState replayCommonStateOverride
var replayCommonFx replayCommonStateOverride

func replayCommonStatePath(path string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"))
}

func replayCommonStateName(path string) string {
	path = replayCommonStatePath(path)
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func replayRestoreCommonEntry(entries map[string][]string, entry *replayCommonStateOverride) {
	if !entry.active {
		return
	}
	previous := *entry
	*entry = replayCommonStateOverride{}
	if entries == nil {
		return
	}
	current, exists := entries[previous.key]
	if exists && len(current) != 0 &&
		replayCommonStatePath(current[len(current)-1]) == replayCommonStatePath(previous.path) {
		if previous.hadKey {
			entries[previous.key] = append([]string(nil), previous.previous...)
		} else {
			delete(entries, previous.key)
		}
	}
}

func replayRestoreCommonState() {
	replayRestoreCommonEntry(sys.cfg.Common.States, &replayCommonState)
	replayRestoreCommonEntry(sys.cfg.Common.Fx, &replayCommonFx)
}

func replayAppendCommonEntry(entries map[string][]string, key, path string, entry *replayCommonStateOverride) {
	for _, files := range entries {
		for _, file := range files {
			if replayCommonStatePath(file) == replayCommonStatePath(path) ||
				replayCommonStateName(file) == replayCommonStateName(path) {
				return
			}
		}
	}
	previous, hadKey := entries[key]
	entries[key] = append(append([]string(nil), previous...), path)
	*entry = replayCommonStateOverride{
		key: key, path: path, previous: append([]string(nil), previous...), hadKey: hadKey, active: true,
	}
}

func replayIncludeCommonState(statePath, fxPath string) bool {
	if sys.replayFile == nil || sys.replayFile.file == nil ||
		strings.TrimSpace(statePath) == "" || strings.TrimSpace(fxPath) == "" {
		return false
	}
	if replayCommonState.active || replayCommonFx.active {
		replayRestoreCommonState()
	}
	if sys.cfg.Common.States == nil {
		sys.cfg.Common.States = make(map[string][]string)
	}
	if sys.cfg.Common.Fx == nil {
		sys.cfg.Common.Fx = make(map[string][]string)
	}
	replayAppendCommonEntry(sys.cfg.Common.Fx, "zz_jay_replay_inputdisplay", fxPath, &replayCommonFx)
	replayAppendCommonEntry(sys.cfg.Common.States, "zz_jay_replay_inputdisplay", statePath, &replayCommonState)
	return true
}

// cleanup replay manager
func replayCleanup() {
	rp.controllerSlot = 1
	replayDiscardTemp()
	replayUnmaskFastForward()
	rp.exitRequested = false
	rp.exitReady = false
	if rp.menuOverride {
		sys.sel.gameParams.PauseMenu = rp.originalPause
		sys.motif.me.enabled = rp.originalEnabled
		sys.cfg.Config.EscOpensMenu = rp.originalEscOpens
		if pm := sys.motif.me.pauseMenu(&sys.motif); pm != nil {
			pm.Enabled = rp.originalPM
		}
		rp.menuOverride = false
	}
	rp.activeSide = 0
	rp.pendingSide = 0
	rp.resetPending = false
	rp.roundFadePaused = false
	if rp.menuOrigin != nil {
		replayFreeSnapshot(rp.menuOrigin)
		rp.menuOrigin = nil
	}
	replayClearHistory()
	rp.file = nil
	rp.lastRecordedTick = -1
	rp.lastPolledFrame = -1
	if rp.menuFrozen && sys.nextAddTime == 0 {
		sys.nextAddTime = 1
	}
	rp.menuFrozen = false
	rp.playbackPaused = false
	rp.rewindAutoPause = false
	rp.menuWasPaused = false
	rp.seeking = false
	rp.seekTarget = 0
	rp.seekDirection = 0
	rp.seekFromHold = false
	rp.rewindHeldTicks = 0
	rp.fastForwardHeldTicks = 0
	rp.rewindPressedAt = time.Time{}
	rp.forwardPressedAt = time.Time{}
	rp.rewindRepeatedAt = time.Time{}
	rp.forwardRepeatedAt = time.Time{}
	rp.menuHeld = [14]int{}
	rp.menuUsed = [14]bool{}
	rp.menuKeyWasDown = false
	rp.pauseKeyWasDown = false
	if rp.prevAccel > 0 {
		sys.debugAccel = rp.prevAccel
	}
	rp.prevAccel = 0
}

// save a snapshot for future reference
func replaySave() *replaySnapshot {
	rf := sys.replayFile
	if rf == nil || rf.file == nil {
		return nil
	}
	off, err := rf.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil
	}
	rp.nextID++
	id := rp.nextID
	old := sys.savePool.curStateID
	sys.savePool.curStateID = id
	gs := NewGameState()
	gs.SaveState(id)
	sys.savePool.curStateID = old
	return &replaySnapshot{state: gs, id: id, tick: sys.tickCount,
		round: sys.roundNo, match: sys.matchNo, offset: off,
		bits: rf.ibit, axes: rf.iaxes}
}

// restore snapshot of replay
func replayRestore(s *replaySnapshot) bool {
	if s == nil || s.state == nil || sys.replayFile == nil || sys.replayFile.file == nil {
		return false
	}
	if _, err := sys.replayFile.file.Seek(s.offset, io.SeekStart); err != nil {
		log.Printf("nuts. unable to seek to snapshot offset: %v", err)
		return false
	}
	old := sys.loadPool.curStateID
	rp.nextID++
	loadID := rp.nextID
	sys.loadPool.curStateID = loadID
	s.state.LoadState(loadID)
	// The objects the previous load took from the pool are no longer the
	// game's: hand them back (this fork has no arenas to free).
	if rp.lastLoadID != 0 && rp.lastLoadID != loadID {
		sys.loadPool.Free(rp.lastLoadID)
	}
	rp.lastLoadID = loadID
	sys.loadPool.curStateID = old
	sys.replayFile.ibit = s.bits
	sys.replayFile.iaxes = s.axes
	sys.esc = false
	sys.escPending = false
	return true
}

// stock fast forward masking
func replayMaskFastForward() {
	if rp.fastForwardMasked {
		return
	}
	rp.savedKeys = append([]KeyConfig(nil), sys.keyConfig...)
	rp.savedJoysticks = append([]KeyConfig(nil), sys.joystickConfig...)
	for i := range sys.keyConfig {
		kc := &sys.keyConfig[i]
		kc.bA, kc.bB, kc.bC = int(KeyUnknown), int(KeyUnknown), int(KeyUnknown)
		kc.bX, kc.bY, kc.bZ = int(KeyUnknown), int(KeyUnknown), int(KeyUnknown)
	}
	for i := range sys.joystickConfig {
		kc := &sys.joystickConfig[i]
		kc.bA, kc.bB, kc.bC = -1, -1, -1
		kc.bX, kc.bY, kc.bZ = -1, -1, -1
	}
	rp.fastForwardMasked = true
}

// return stock fast forward feature
func replayUnmaskFastForward() {
	if !rp.fastForwardMasked {
		return
	}
	copy(sys.keyConfig, rp.savedKeys)
	copy(sys.joystickConfig, rp.savedJoysticks)
	rp.savedKeys, rp.savedJoysticks = nil, nil
	rp.fastForwardMasked = false
}

// get inputs from the player who selected this replay
func replayLiveControllerInput() [14]bool {
	var keys, joy [14]bool
	idx := rp.controllerSlot - 1
	if idx < 0 {
		idx = 0
	}
	if len(rp.savedKeys) > idx {
		keys = GetKeyboardState(rp.savedKeys[idx])
	} else if len(sys.keyConfig) > idx {
		keys = GetKeyboardState(sys.keyConfig[idx])
	}
	if len(rp.savedJoysticks) > idx {
		joy = GetJoystickState(rp.savedJoysticks[idx])
	} else if len(sys.joystickConfig) > idx {
		joy = GetJoystickState(sys.joystickConfig[idx])
	}
	var out [14]bool
	for i := range out {
		out[i] = keys[i] || joy[i]
	}
	return out
}

// get menu inputs
func replaySampleMenuInput() {
	keys := replayLiveControllerInput()
	if len(rp.savedJoysticks) >= rp.controllerSlot {
		joy := rp.savedJoysticks[rp.controllerSlot-1].Joy
		if joy >= 0 && input.IsJoystickPresent(joy) {
			axes := input.GetJoystickAxes(joy)
			threshold := sys.cfg.Input.ControllerStickSensitivity
			keys[0] = keys[0] || axes[1] < -threshold
			keys[1] = keys[1] || axes[1] > threshold
			keys[2] = keys[2] || axes[0] < -threshold
			keys[3] = keys[3] || axes[0] > threshold
		}
	}
	for i, down := range keys {
		if down {
			rp.menuHeld[i]++
		} else {
			rp.menuHeld[i] = 0
		}
	}
	rp.menuUsed = [14]bool{}
}

// resolve .def bindings as Ikemen command names
func replayCommandIndex(token string) int {
	switch strings.TrimSpace(token) {
	case "U":
		return 0
	case "D":
		return 1
	case "L":
		return 2
	case "R":
		return 3
	case "a":
		return 4
	case "b":
		return 5
	case "c":
		return 6
	case "x":
		return 7
	case "y":
		return 8
	case "z":
		return 9
	case "s":
		return 10
	case "m":
		return 13
	}
	return -1
}

// check for stock command
func replayCommandDown(liveController [14]bool, command int) bool {
	return command >= 0 && command < len(liveController) && liveController[command]
}

// stock menu key stuff
func replayMenuIndex(token string) int {
	switch strings.TrimSpace(token) {
	case "LS_Y-":
		return 0
	case "LS_Y+":
		return 1
	case "B", "LS_X-":
		return 2
	case "F", "LS_X+":
		return 3
	case "d":
		return 11
	case "w":
		return 12
	}
	return replayCommandIndex(token)
}

// parse menu input
func replayPhysicalMenuInput(L *lua.LState) int {
	if sys.replayFile == nil || !sys.motif.me.active || rp.activeSide != 0 {
		L.Push(lua.LBool(false))
		return 1
	}
	var testToken func(lua.LValue) bool
	testToken = func(value lua.LValue) bool {
		switch v := value.(type) {
		case lua.LString:
			idx := replayMenuIndex(string(v))
			if idx < 0 || rp.menuUsed[idx] {
				return false
			}
			count := rp.menuHeld[idx]
			pressed := count == 1
			if idx < 4 && count > 1 && sys.cfg.Input.UiRepeatRate > 0 {
				delay := int(sys.cfg.Input.UiRepeatDelay)
				rate := int(sys.cfg.Input.UiRepeatRate)
				pressed = count >= 1+delay && (count-(1+delay))%rate == 0
			}
			if pressed {
				rp.menuUsed[idx] = true
				return true
			}
		case *lua.LTable:
			found := false
			v.ForEach(func(_, item lua.LValue) {
				if !found && testToken(item) {
					found = true
				}
			})
			return found
		}
		return false
	}
	for i := 2; i <= L.GetTop(); i++ {
		if testToken(L.Get(i)) {
			L.Push(lua.LBool(true))
			return 1
		}
	}
	L.Push(lua.LBool(false))
	return 1
}

// override for allowing pause menu during replay
func replayAllowMenu() {
	if !rp.menuOverride {
		rp.originalPause = sys.sel.gameParams.PauseMenu
		rp.originalEnabled = sys.motif.me.enabled
		rp.originalEscOpens = sys.cfg.Config.EscOpensMenu
		if pm := sys.motif.me.pauseMenu(&sys.motif); pm != nil {
			rp.originalPM = pm.Enabled
		}
		rp.menuOverride = true
	}
	sys.sel.gameParams.PauseMenu = true
	sys.motif.me.enabled = true
	sys.cfg.Config.EscOpensMenu = true
	if pm := sys.motif.me.pauseMenu(&sys.motif); pm != nil {
		pm.Enabled = true
	}
}

// open normal menu
func replayOpenNativeMenu() {
	replayAllowMenu()
	sys.motif.me.reset(&sys.motif)
	oldEsc := sys.esc
	sys.esc = true
	sys.motif.me.init(&sys.motif)
	sys.esc = oldEsc
}

// return to main menu
func replayReturnToMenu() {
	if rp.menuOrigin == nil || rp.resetPending || rp.activeSide == 0 {
		return
	}
	rp.resetPending = true
	rp.roundFadePaused = false
	rp.playbackPaused = rp.menuWasPaused
	rp.seeking = false
	replayOpenNativeMenu()
	sys.paused = true
	sys.oldNextAddTime = 0
	sys.nextAddTime = 0
}

// finish takeover
func replayFinishReturn() {
	if !rp.resetPending || !sys.motif.me.active ||
		sys.motif.me.state != ME_OpeningIn {
		return
	}
	replayDiscardTemp()
	if !replayRestore(rp.menuOrigin) {
		log.Printf("nuts. takeover return failed to restore its snapshot")
	}
	replayAllowMenu()
	sys.motif.me.active = true
	sys.motif.me.initialized = true
	sys.motif.me.state = ME_OpeningIn
	sys.motif.me.reopenLock = false
	rp.activeSide = 0
	rp.pendingSide = 0
	rp.takeoverTicks = 0
	rp.lastRecordedTick = -1
	rp.resetPending = false
	rp.roundFadePaused = false
	rp.playbackPaused = rp.menuWasPaused
	if sys.fightScreen.round != nil && sys.fightScreen.round.fadeOut != nil {
		sys.fightScreen.round.fadeOut.reset()
	}
	sys.paused = true
	sys.oldNextAddTime = 0
	sys.nextAddTime = 0
}

// open replay overrides
func replayOpen() bool {
	if rp.resetPending || sys.motif.me.active {
		return false
	}
	if rp.activeSide != 0 {
		replayReturnToMenu()
		return true
	}
	if !replayMatchValid() {
		return false
	}

	replayEndRewindHold()

	rp.seeking = false
	rp.seekFromHold = false
	rp.rewindHeldTicks = 0
	rp.fastForwardHeldTicks = 0
	rp.menuWasPaused = rp.playbackPaused
	if rp.menuOrigin != nil {
		replayFreeSnapshot(rp.menuOrigin)
	}
	rp.menuOrigin = replaySave()
	if rp.menuOrigin == nil {
		return false
	}

	if rp.prevAccel > 0 {
		sys.debugAccel = rp.prevAccel
	}
	// use the built-in MotifMenu fade/init path without closing ReplayFile.
	replayOpenNativeMenu()
	return true
}

// fight screen end fades are different. we gotta account for that.
func replayRoundFinished() bool {
	return sys.roundEnded() || sys.endMatch ||
		sys.fightLoopEnd || sys.postMatchFlg
}

// handle round end scenario for takeover and standard input
func replayHandleRoundEnd() bool {
	if !replayRoundFinished() || rp.menuOrigin == nil {
		return false
	}
	sys.oldNextAddTime = 0
	if sys.replayFile != nil {
		sys.replayFile.ibit = [REPLAY_NUM_INPUTS]InputBits{}
	}
	if sys.fightScreen.round != nil && sys.fightScreen.round.fadeOut != nil {
		f := sys.fightScreen.round.fadeOut
		if f.isActive() {
			rp.roundFadePaused = true
			if f.timeRemaining > 0 {
				return true
			}
			replayReturnToMenu()
			return true
		}
	}

	if sys.roundOver() || sys.endMatch || sys.fightLoopEnd || sys.postMatchFlg {
		replayReturnToMenu()
	}

	return true
}

// request close and begin fade.
func replayRequestExit() bool {
	if rp.menuOrigin == nil || sys.replayFile == nil ||
		!sys.motif.me.active || rp.activeSide != 0 ||
		rp.pendingSide != 0 || rp.exitRequested {
		return false
	}
	rp.exitRequested = true
	return true
}

// start replay with active sides
func replayBegin(side int) bool {
	if side < 1 || side > 2 || rp.menuOrigin == nil || !sys.motif.me.active || rp.resetPending {
		return false
	}
	rp.pendingSide = side
	// takeover itself runs normally. the menuWasPaused flag retains the origin state
	rp.playbackPaused = false
	if rp.prevAccel > 0 {
		sys.debugAccel = rp.prevAccel
	}
	return true
}

// accelerate frames via the existing System.await() frame scheduler
func replayAccelerateFrame() {
	if rp.fastForwardRate <= 1 || sys.motif.me.active ||
		rp.activeSide != 0 || rp.pendingSide != 0 {
		return
	}

	normal := time.Second / time.Duration(sys.gameRenderSpeed())
	accelerated := time.Duration(float64(normal) / rp.fastForwardRate)
	if accelerated < time.Millisecond {
		accelerated = time.Millisecond
	}
	if accelerated < normal {
		sys.redrawWait.nextTime = sys.redrawWait.nextTime.Add(accelerated - normal)
	}
}

// checkpoints are valid only inside one continuous round. it resets when it ends.
func replayClearHistory() {
	for _, snap := range rp.history {
		replayFreeSnapshot(snap)
	}
	rp.history = nil
	rp.lastRecordedTick = -1
}

// check replay history for rewind
func replayHistoryCurrent() bool {
	if len(rp.history) == 0 {
		return true
	}
	first := rp.history[0]
	last := rp.history[len(rp.history)-1]
	if first == nil || last == nil ||
		first.round != sys.roundNo || last.round != sys.roundNo ||
		first.match != sys.matchNo || last.match != sys.matchNo ||
		last.tick > sys.tickCount {
		return false
	}
	// a backwards file cursor without a controlled seek means a
	// round reset or an unrelated state load happened. not ideal to reuse that history again
	if sys.replayFile != nil && sys.replayFile.file != nil {
		if off, err := sys.replayFile.file.Seek(0, io.SeekCurrent); err == nil && off < last.offset {
			return false
		}
	}
	return true
}

// create snapshot intervals
func replaySeekBack(frames int) bool {
	if frames < 1 || sys.motif.me.active || rp.activeSide != 0 ||
		rp.pendingSide != 0 || !replayMatchValid() || len(rp.history) == 0 {
		return false
	}
	if rp.seeking {
		return false
	}
	if !replayHistoryCurrent() {
		// no eligible checkpoint in this round yet
		replayClearHistory()
		replayRecordHistory(true)
		return false
	}
	target := sys.tickCount - frames
	if target < rp.history[0].tick {
		target = rp.history[0].tick
	}
	if target >= sys.tickCount {
		return false
	}
	var snap *replaySnapshot
	for _, s := range rp.history {
		if s.round != sys.roundNo || s.match != sys.matchNo {
			break
		}
		if s.tick <= target {
			snap = s
		} else {
			break
		}
	}
	if snap == nil || !replayRestore(snap) {
		return false
	}

	for len(rp.history) > 0 && rp.history[len(rp.history)-1].tick > snap.tick {
		last := len(rp.history) - 1
		replayFreeSnapshot(rp.history[last])
		rp.history = rp.history[:last]
	}
	rp.lastRecordedTick = -1
	rp.seekTarget = target
	rp.seekDirection = -1
	rp.seekFromHold = false
	rp.seeking = snap.tick < target
	if !rp.seeking {
		replaySetPhysicalPause(rp.playbackPaused)
	} else {
		replaySetPhysicalPause(false)
	}
	return true
}

// seek forward replay
func replaySeekForward(frames int, held bool) bool {
	if frames < 1 || !replayMatchValid() || sys.motif.me.active ||
		rp.activeSide != 0 || rp.pendingSide != 0 || rp.seeking {
		return false
	}
	rp.seekTarget = sys.tickCount + frames
	rp.seekDirection = 1
	rp.seekFromHold = held
	rp.seeking = true
	replaySetPhysicalPause(false)
	return true
}

// pause game logic
func replaySetPhysicalPause(pause bool) {
	sys.paused = pause
	if pause {
		sys.oldNextAddTime = 0
		sys.nextAddTime = 0
	} else {
		sys.oldNextAddTime = 1
		sys.nextAddTime = 1
	}
}

// record history for rewinding
func replayRecordHistory(force bool) {
	if !replayMatchValid() || sys.motif.me.active || rp.activeSide != 0 {
		return
	}
	if !replayHistoryCurrent() {
		replayClearHistory()
	}
	if !force && sys.tickCount == rp.lastRecordedTick {
		return
	}
	rp.lastRecordedTick = sys.tickCount
	if len(rp.history) != 0 && rp.history[len(rp.history)-1].tick >= sys.tickCount {
		return
	}
	if !force && len(rp.history) != 0 &&
		sys.tickCount-rp.history[len(rp.history)-1].tick < rp.historyInterval {
		return
	}
	if snap := replaySave(); snap != nil {
		rp.history = append(rp.history, snap)
		for len(rp.history) > rp.maxHistory {
			replayFreeSnapshot(rp.history[0])
			rp.history = rp.history[1:]
		}
	}
}

// one frame seek
func replayTickSeek(liveController [14]bool) bool {
	if !rp.seeking {
		return false
	}

	if rp.seekDirection == 1 && rp.seekFromHold &&
		!replayCommandDown(liveController, rp.fastForwardCommand) {
		rp.seeking = false
		replaySetPhysicalPause(rp.playbackPaused)
		return true
	}
	if sys.tickCount >= rp.seekTarget {
		rp.seeking = false
		rp.seekFromHold = false
		replaySetPhysicalPause(rp.playbackPaused)
		// anchor this exact target for future one-frame steps.
		replayRecordHistory(true)
		return true
	}
	replaySetPhysicalPause(false)
	replayRecordHistory(false)
	replayAccelerateFrame()
	return true
}

// rewind owns a temporary logical pause so it looks better kind of.
func replayBeginRewindHold() {
	if !rp.playbackPaused {
		rp.rewindAutoPause = true
		rp.playbackPaused = true
		if !rp.seeking {
			replaySetPhysicalPause(true)
		}
	}
}
func replayEndRewindHold() {
	if !rp.rewindAutoPause {
		return
	}
	rp.rewindAutoPause = false
	rp.playbackPaused = false
	if !rp.seeking && !sys.motif.me.active && rp.activeSide == 0 {
		replaySetPhysicalPause(false)
	}
}

// first press rewinds one frame (recreate frame step logic), but held inputs advance by specified amount.
func replayHeldRepeat(firstDown, lastRepeat time.Time, now time.Time, delay, interval int) bool {
	if firstDown.IsZero() || now.Sub(firstDown) < time.Duration(delay)*time.Second/60 {
		return false
	}
	return lastRepeat.IsZero() || now.Sub(lastRepeat) >= time.Duration(interval)*time.Second/60
}

// scrub input polling
func replayPollScrub(liveController [14]bool) {
	if sys.motif.me.active || rp.activeSide != 0 || rp.pendingSide != 0 ||
		!replayMatchValid() {
		replayEndRewindHold()
		rp.rewindHeldTicks = 0
		rp.fastForwardHeldTicks = 0
		return
	}
	now := time.Now()
	rewDown := replayCommandDown(liveController, rp.rewindCommand)
	ffDown := replayCommandDown(liveController, rp.fastForwardCommand)
	if rewDown && !ffDown {
		if rp.rewindHeldTicks == 0 {
			replayBeginRewindHold()
		}
		rp.rewindHeldTicks++
		rp.fastForwardHeldTicks = 0
		rp.forwardPressedAt = time.Time{}
		rp.forwardRepeatedAt = time.Time{}
		if rp.rewindHeldTicks == 1 {
			rp.rewindPressedAt = now
			rp.rewindRepeatedAt = time.Time{}
			replaySeekBack(1)
		} else if replayHeldRepeat(rp.rewindPressedAt, rp.rewindRepeatedAt,
			now, rp.holdDelay, rp.rewindRepeat) {
			if replaySeekBack(rp.rewindFrames) {
				rp.rewindRepeatedAt = now
			}
		}
	} else if ffDown && !rewDown {
		replayEndRewindHold()
		rp.fastForwardHeldTicks++
		rp.rewindHeldTicks = 0
		rp.rewindPressedAt = time.Time{}
		rp.rewindRepeatedAt = time.Time{}
		if rp.fastForwardHeldTicks == 1 {
			rp.forwardPressedAt = now
			rp.forwardRepeatedAt = time.Time{}
			if rp.playbackPaused {
				replaySeekForward(1, false)
			}
		} else if rp.playbackPaused {
			if replayHeldRepeat(rp.forwardPressedAt, rp.forwardRepeatedAt,
				now, rp.holdDelay, rp.fastForwardRepeat) {
				if replaySeekForward(rp.fastForwardFrames, true) {
					rp.forwardRepeatedAt = now
				}
			}
		}
		if !rp.playbackPaused && !rp.seeking {
			replayAccelerateFrame()
		}
	} else {
		replayEndRewindHold()
		rp.fastForwardHeldTicks = 0
		rp.rewindHeldTicks = 0
		rp.rewindPressedAt = time.Time{}
		rp.forwardPressedAt = time.Time{}
		rp.rewindRepeatedAt = time.Time{}
		rp.forwardRepeatedAt = time.Time{}
	}
}

// poll replay control keys
func replayPollKeys(liveController [14]bool) {
	menuDown := replayCommandDown(liveController, rp.menuCommand)
	pauseDown := replayCommandDown(liveController, rp.pauseCommand)
	open := menuDown && !rp.menuKeyWasDown
	pause := pauseDown && !rp.pauseKeyWasDown
	rp.menuKeyWasDown = menuDown
	rp.pauseKeyWasDown = pauseDown
	if open {
		replayOpen()
	} else if pause && !sys.motif.me.active && rp.activeSide == 0 && rp.pendingSide == 0 {
		replayTogglePlaybackPause()
	}
}

// replay playback pause handling
func replayTogglePlaybackPause() {
	if rp.rewindAutoPause {
		rp.rewindAutoPause = false
		if !rp.seeking {
			replaySetPhysicalPause(true)
		}
		return
	}
	rp.playbackPaused = !rp.playbackPaused
	rp.seeking = false
	rp.seekFromHold = false
	replaySetPhysicalPause(rp.playbackPaused)
}

// called after Motif.step() from an additional lua callback.
func replayTick() {
	if sys.replayFile == nil || sys.replayFile.file == nil {
		replayRestoreCommonState()
		if rp.file != nil {
			replayCleanup()
		}
		return
	}
	if rp.file != sys.replayFile {
		controller := rp.controllerSlot
		replayCleanup()
		rp.controllerSlot = controller
		rp.file = sys.replayFile
		rp.prevAccel = sys.debugAccel
		rp.lastRecordedTick = -1
	}

	if rp.lastPolledFrame == sys.frameCounter {
		return
	}
	rp.lastPolledFrame = sys.frameCounter

	if rp.exitRequested {
		if sys.motif.me.state == ME_ClosingIn {
			sys.motif.me.reset(&sys.motif)
			sys.paused = false
			rp.exitRequested = false
			rp.exitReady = true
		}
		return
	}

	if rp.resetPending {
		sys.oldNextAddTime = 0
		sys.nextAddTime = 0
		replayFinishReturn()
		return
	}
	// outside takeover, never poll replay controls on non-fight screens.
	// this is because takeover during non fight is stupid and can cause massive desync issues
	if !replayMatchValid() && rp.activeSide == 0 {
		// keep stock fast forward buttons disabled
		replayMaskFastForward()
		return
	}
	replayMaskFastForward()
	liveController := replayLiveControllerInput()
	if rp.activeSide != 0 && replayHandleRoundEnd() {
		return
	}

	if rp.activeSide == 0 || (!replayRoundFinished() && !rp.roundFadePaused) {
		replayPollKeys(liveController)
	}
	if rp.resetPending {
		return
	}

	// a recording may also reach EOF during a takeover with no round end. not ideal if we let this happen
	if rp.activeSide != 0 && !rp.roundFadePaused {
		if info, err := sys.replayFile.file.Stat(); err == nil {
			pos, err := sys.replayFile.file.Seek(0, io.SeekCurrent)
			if err == nil && pos >= info.Size()-int64(REPLAY_NUM_INPUTS*REPLAY_INPUT_BYTES) &&
				!replayRoundFinished() {
				replayReturnToMenu()
				return
			}
		}
	}

	// the recorded players are not the people operating the pause menu.
	if sys.motif.me.active {
		if !rp.menuFrozen {
			rp.unfrozenNextTime = sys.nextAddTime
			rp.menuFrozen = true
		}
		sys.oldNextAddTime = 0
		sys.nextAddTime = 0
		replaySampleMenuInput()
	} else if rp.menuFrozen {
		rp.menuFrozen = false

		if rp.pendingSide == 0 && rp.activeSide == 0 {
			replaySetPhysicalPause(rp.playbackPaused)
		} else if sys.nextAddTime == 0 {
			sys.nextAddTime = 1
		}
	}

	if rp.pendingSide != 0 {
		if !sys.motif.me.active && !sys.paused {
			side := rp.pendingSide
			if replayRestore(rp.menuOrigin) && replayMakeTemp() {
				rp.activeSide = side
				rp.takeoverTicks = 0
				rp.lastRecordedTick = -1
				rp.roundFadePaused = false
				sys.paused = false

				replayPatchTakeoverFrame()
			} else {
				rp.pendingSide = 0

				replayOpenNativeMenu()
				sys.paused = true
			}
			rp.pendingSide = 0
		}
		return
	}

	if rp.activeSide != 0 {
		if rp.takeoverTicks >= rp.maxTakeoverTicks {
			replayReturnToMenu()
			return
		}
		if !sys.paused && !sys.motif.me.active {
			// do not change the recorded frame while a completed round is waiting for outro fade.
			replayPatchTakeoverFrame()
			if sys.tickCount != rp.lastRecordedTick {
				rp.takeoverTicks++
				rp.lastRecordedTick = sys.tickCount
			}
		}
		return
	}

	if sys.motif.me.active {
		replayEndRewindHold()
		rp.rewindHeldTicks = 0
		rp.fastForwardHeldTicks = 0
		return
	}

	replayPollScrub(liveController)
	if replayTickSeek(liveController) {
		return
	}
	if sys.paused || rp.playbackPaused {
		return
	}

	replayRecordHistory(false)
}

// takeover frame overwrite
func replayPatchTakeoverFrame() {
	if rp.takeoverReplay == nil || rp.activeSide < 1 {
		return
	}
	rf := sys.replayFile
	idx := rp.activeSide - 1
	if idx >= len(sys.inputRemap) || idx >= len(sys.commandLists) ||
		len(sys.commandLists) == 0 || sys.commandLists[idx] == nil ||
		sys.commandLists[idx].Buffer == nil {
		return
	}
	remap := sys.inputRemap[idx]
	if remap < 0 || remap >= REPLAY_NUM_INPUTS {
		return
	}
	reader := sys.commandLists[idx].Buffer.InputReader
	buttons := replayLiveControllerInput()
	if sys.cfg.Input.ButtonAssist {
		assisted := reader.ButtonAssistCheck([9]bool{
			buttons[4], buttons[5], buttons[6], buttons[7], buttons[8],
			buttons[9], buttons[10], buttons[11], buttons[12],
		})
		copy(buttons[4:13], assisted[:])
	}
	buttons[13] = false // replay menu key is not a character attack obviously
	var bits InputBits
	bits.KeysToBits(buttons)
	axes := reader.LocalAnalogInput(rp.controllerSlot - 1)

	// immediate cache update covers the first frame after restoring origin.
	rf.ibit[remap] = bits
	rf.iaxes[remap] = axes
	pos, err := rp.takeoverReplay.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	if !rf.atReplaySyncMarker() {
		var inputBytes [REPLAY_INPUT_BYTES]byte
		inputBytes[0], inputBytes[1] = byte(bits), byte(bits>>8)
		for j, val := range axes {
			inputBytes[2+j] = byte(val)
		}
		if _, err := rp.takeoverReplay.WriteAt(inputBytes[:], pos+int64(remap*REPLAY_INPUT_BYTES)); err != nil {
			log.Printf("nuts. takeover frame write failed: %v", err)
		}
	}
	for i := idx; i < len(sys.chars); i += 2 {
		if len(sys.chars[i]) == 0 {
			continue
		}
		sys.aiLevel[i] = 0
		for _, ch := range sys.chars[i] {
			if ch != nil {
				ch.controller = i
			}
		}
	}
}

// rewind replay
func replayRewind() bool {
	return replaySeekBack(rp.rewindFrames)
}

// register lua functions
func replayRegister(l *lua.LState) {
	luaRegister(l, "replayIncludeCommonState", func(L *lua.LState) int {
		fxPath := ""
		if L.GetTop() >= 2 {
			fxPath = strArg(L, 2)
		}
		L.Push(lua.LBool(replayIncludeCommonState(strArg(L, 1), fxPath)))
		return 1
	})
	luaRegister(l, "replayInstall", func(L *lua.LState) int {
		if sys.cfg.Common.Lua == nil {
			sys.cfg.Common.Lua = make(map[string][]string)
		}
		sys.cfg.Common.Lua["zz_replay_takeover"] = []string{"require('external.mods.jayreplay++.replay').tick()"}
		L.Push(lua.LBool(true))
		return 1
	})
	luaRegister(l, "replayConfigure", func(L *lua.LState) int {
		rp.maxTakeoverTicks = int(math.Min(1800, math.Max(1, numArg(L, 1))))
		rp.historyInterval = int(math.Max(1, numArg(L, 2)))
		rp.maxHistory = int(math.Max(2, numArg(L, 3)))
		rp.rewindFrames = int(math.Max(1, numArg(L, 4)))
		if L.GetTop() >= 5 {
			rp.menuCommand = replayCommandIndex(strArg(L, 5))
		}
		if L.GetTop() >= 6 {
			rp.pauseCommand = replayCommandIndex(strArg(L, 6))
		}
		if L.GetTop() >= 7 {
			rp.rewindCommand = replayCommandIndex(strArg(L, 7))
		}
		if L.GetTop() >= 8 {
			rp.rewindRepeat = int(math.Max(1, numArg(L, 8)))
		}
		if L.GetTop() >= 9 {
			rp.fastForwardCommand = replayCommandIndex(strArg(L, 9))
		}
		if L.GetTop() >= 10 {
			rp.fastForwardFrames = int(math.Max(1, math.Min(36000, numArg(L, 10))))
		}
		if L.GetTop() >= 11 {
			rp.fastForwardRepeat = int(math.Max(1, numArg(L, 11)))
		}
		if L.GetTop() >= 12 {
			rp.holdDelay = int(math.Max(2, numArg(L, 12)))
		}
		if L.GetTop() >= 13 {
			rp.fastForwardRate = math.Max(1, math.Min(16, numArg(L, 13)))
		}
		liveController := replayLiveControllerInput()
		rp.menuKeyWasDown = replayCommandDown(liveController, rp.menuCommand)
		rp.pauseKeyWasDown = replayCommandDown(liveController, rp.pauseCommand)
		return 0
	})
	luaRegister(l, "replaySetController", func(L *lua.LState) int {
		controller := int(numArg(L, 1))
		if controller != 2 {
			controller = 1
		}
		rp.controllerSlot = controller
		live := replayLiveControllerInput()
		rp.menuKeyWasDown = replayCommandDown(live, rp.menuCommand)
		rp.pauseKeyWasDown = replayCommandDown(live, rp.pauseCommand)
		L.Push(lua.LNumber(controller))
		return 1
	})
	luaRegister(l, "replayTick", func(L *lua.LState) int {
		replayTick()
		ready := rp.exitReady
		rp.exitReady = false
		L.Push(lua.LBool(ready))
		return 1
	})
	luaRegister(l, "replayRequestExit", func(L *lua.LState) int {
		L.Push(lua.LBool(replayRequestExit()))
		return 1
	})
	luaRegister(l, "replayOpen", func(L *lua.LState) int { L.Push(lua.LBool(replayOpen())); return 1 })
	luaRegister(l, "replayBegin", func(L *lua.LState) int { L.Push(lua.LBool(replayBegin(int(numArg(L, 1))))); return 1 })
	luaRegister(l, "replayReturn", func(*lua.LState) int { replayReturnToMenu(); return 0 })
	luaRegister(l, "replayMenuPhase", func(L *lua.LState) int {
		phase := ""
		if sys.replayFile != nil && sys.motif.me.active {
			switch sys.motif.me.state {
			case ME_OpeningOut:
				phase = "openingout"
			case ME_OpeningIn:
				phase = "openingin"
			case ME_Open:
				phase = "open"
			case ME_ClosingOut:
				phase = "closingout"
			case ME_ClosingIn:
				phase = "closingin"
			}
		}
		L.Push(lua.LString(phase))
		return 1
	})
	luaRegister(l, "replayMenuInput", replayPhysicalMenuInput)
	luaRegister(l, "replayRewind", func(L *lua.LState) int { L.Push(lua.LBool(replayRewind())); return 1 })
	luaRegister(l, "replayFastForward", func(L *lua.LState) int {
		L.Push(lua.LBool(replaySeekForward(int(math.Max(1, numArg(L, 1))), false)))
		return 1
	})
	luaRegister(l, "replayPause", func(L *lua.LState) int {
		if replayMatchValid() && !sys.motif.me.active && rp.activeSide == 0 {
			replayTogglePlaybackPause()
		}
		return 0
	})
	luaRegister(l, "replayIsPlaying", func(L *lua.LState) int {
		L.Push(lua.LBool(replayMatchValid() && !sys.motif.me.active && rp.activeSide == 0))
		return 1
	})
	luaRegister(l, "replayFinishExit", func(*lua.LState) int {
		replayCleanup()
		sys.fightScreen.round.fadeOut.reset()
		sys.uiResetTokenGuard()
		sys.endMatch = true
		return 0
	})
	luaRegister(l, "replayClean", func(*lua.LState) int {
		replayRestoreCommonState()
		replayCleanup()
		return 0
	})
}
