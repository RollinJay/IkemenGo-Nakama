package main

import (
	"math"
	"time"
)

// The lobby's watch window: a lobby member who is not playing watches the
// lobby's match in a rectangle of the room screen, and keeps chatting and
// using the room's menus around it.
//
// The engine runs one match at a time on sys, so the room cannot run beside
// the fight; it runs inside it instead. The watched match is an ordinary live
// replay (replay_live.go) whose game surface is the window, drawn through the
// border/presentation path (presentation.go, renderFrame): the canvas is the
// lobby's, which draws there from the 'game.border_bg' and 'game.border'
// hooks, and the room's logic runs once per frame from 'game.watch_tick'.
// While the window is set:
//
//   - Esc does not open the pause menu or end the match: Lua's esc() reports
//     it, for the room's menus and chat, and the room ends the match with
//     stopWatching.
//   - The menus' input (getInput, getInputTime) reads the local controllers
//     through command lists of its own; the UI command lists the watched match
//     reads (sys.commandLists, part of its state) are left alone.
//   - Only the lobby's own Lua gets that Esc and input: code run from the
//     window's hooks (inHook), or any Lua while no match runs (the lobby's
//     loop, refresh). Lua the watched match runs itself ([Common] Lua's
//     loop(), mods) reads the match's command lists and sys.esc, as it does
//     for the players, so that a watcher's keys can never change the match.
//     resetTokenGuard() from the lobby resets the window's own UI token guard.
//   - Loading is the engine's (watchIdleFrame keeps the room drawn), not the
//     VS screen's: gameOption('Config.VsScreenLoading') reads false and
//     loadStart validates as without it, for as long as the window is set.
//     Nothing is written to the config, so the lobby needs no
//     modifyGameOption around the watch.
//   - Catching up (4x speed), the room's tick and its menu input still step
//     at about 60 Hz of wall time (watchTickInterval).
//   - A netplay or rollback session is never watched: the window gives up
//     Esc and input while one exists, and game() clears a window left set
//     when one starts (watchSessionBackstop).
//   - Holding a button does not fast-forward the replay.
//   - The watched match's music and its character and fight screen sounds
//     play at the window's volume; the lobby's own sounds (Lua sndPlay) do not.
//   - Loading and waiting for the stream keep the lobby running and drawn
//     (watchIdleFrame), so the room never freezes.
type watchWindowState struct {
	active bool
	// The window in canvas pixels, and the game viewport the border path
	// had before it, restored when the window is cleared.
	pixels    [4]int32
	savedGame [4]int32
	// The watched match's volume, percent.
	volume int32
	// The room asked to stop watching (stopWatching).
	stop bool
	// Esc pressed since the last watch tick (escLatch), and Esc as Lua's
	// esc() reports it during the tick that follows.
	escLatch bool
	esc      bool
	// Engine frames (await) and the frame the hook and the local command
	// lists last ran in: they run once per frame wherever they are reached.
	frame     uint64
	tickFrame uint64
	ticked    bool
	uiFrame   uint64
	uiStepped bool
	// The menus' local input while the window is set, and the UI repeat and
	// last-input state Lua's getInput keeps for them: kept apart from the
	// engine's, so that a lobby press never consumes a token the watched
	// match's own reads (dialogue skip, win screens) need that frame.
	uiLists []*CommandList
	uiGuard uiInputGuard
	// A Lua menu read runs: uiRawInput reads uiLists (withLuaUIInput).
	luaInput bool
	// A hook is running: nothing it reaches runs it again.
	inHook bool
	// When the room's tick last ran, wall clock: catch-up frames do not
	// tick it faster than watchTickInterval.
	lastTick time.Time
	// The last error each hook logged, so that one failing every frame logs
	// once.
	hookErr map[string]string
}

// watchWindowPixels converts a rectangle in a screenpack's localcoord units
// (x, y, width, height) to canvas pixels, the way the screenpack's own
// elements are placed on a canvas of that size (see Rect.drawWindow): the
// localcoord's 4:3 part is scaled to 320x240 game units, centered on a wider
// canvas.
func watchWindowPixels(rect [4]float32, localcoord [2]int32, canvasW, canvasH int32) [4]int32 {
	lx, ly := float32(localcoord[0]), float32(localcoord[1])
	if lx <= 0 || ly <= 0 {
		lx, ly = 320, 240
	}
	v := lx
	if lx*3 > ly*4 {
		v = ly * 4 / 3
	}
	localScale := v / 320
	offsetX := localcoordOffsetX(float64(lx), float64(v))
	st := aspectStateForSize(Max(1, canvasW), Max(1, canvasH))

	x := rect[0]/localScale + offsetX + (st.gameWidth-320)/2
	y := rect[1]/localScale + st.gameHeight - 240
	w := rect[2] / localScale
	h := rect[3] / localScale
	// Edges are rounded separately, as screenRect does, to match the
	// screenpack's sprite edges.
	round := func(v float32) int32 { return int32(math.Round(float64(v))) }
	x1, y1 := round(x*st.widthScale), round(y*st.heightScale)
	x2, y2 := round((x+w)*st.widthScale), round((y+h)*st.heightScale)
	out := [4]int32{x1, y1, Max(1, x2-x1), Max(1, y2-y1)}
	// The window stays on the canvas: the game surface is a viewport.
	if clipped := intersectRect(out, [4]int32{0, 0, canvasW, canvasH}); clipped[2] > 0 && clipped[3] > 0 {
		out = clipped
	}
	return out
}

// fitAspectRect returns the largest rectangle of the given aspect
// (width/height) centered in rect: a fight wider or narrower than the window
// is letterboxed in it rather than stretched.
func fitAspectRect(rect [4]int32, aspect float32) [4]int32 {
	if aspect <= 0 || rect[2] <= 0 || rect[3] <= 0 {
		return rect
	}
	w, h := rect[2], rect[3]
	if float32(w) > float32(h)*aspect {
		w = Max(1, int32(math.Round(float64(float32(h)*aspect))))
	} else {
		h = Max(1, int32(math.Round(float64(float32(w)/aspect))))
	}
	return [4]int32{rect[0] + (rect[2]-w)/2, rect[1] + (rect[3]-h)/2, w, h}
}

// setWatchWindow sets the watch window, in the current screenpack's
// localcoord units, before the match starts: the fight's aspect is applied to
// it when the match has loaded (applyFightAspect).
func (s *System) setWatchWindow(rect [4]float32, volume int32) {
	w := &s.watchWindow
	if !w.active {
		w.savedGame = s.presentation.game
	}
	w.pixels = watchWindowPixels(rect, s.motif.Info.Localcoord, s.presentation.canvas[2], s.presentation.canvas[3])
	w.volume = Clamp(volume, 0, 100)
	w.active = true
	w.stop = false
	w.escLatch, w.esc = false, false
	w.ticked, w.uiStepped = false, false
	s.presentation.game = w.pixels
	s.resetWatchUILists()
}

// clearWatchWindow restores the presentation and input the window changed.
// The volume follows at the next sound tick (refresh), as the watched
// match's volume applies only while it runs.
func (s *System) clearWatchWindow() {
	w := &s.watchWindow
	if w.active {
		s.presentation.game = w.savedGame
	}
	w.active = false
	w.stop = false
	w.escLatch, w.esc = false, false
	w.uiLists = nil
	w.hookErr = nil
}

// fitWatchGameRect places the game surface in the window at the fight's
// aspect, and returns it.
func (s *System) fitWatchGameRect(aspect float32) [4]int32 {
	s.presentation.game = fitAspectRect(s.watchWindow.pixels, aspect)
	return s.presentation.game
}

// requestWatchStop ends the watched match at its next frame: the viewer left
// (stopWatching). Only a replay can be stopped, so that a request that comes
// too late never ends a match that is played.
func (s *System) requestWatchStop() {
	if s.watchWindow.active || s.replayFile != nil {
		s.watchWindow.stop = true
	}
}

func (s *System) watchStopRequested() bool {
	return s.watchWindow.stop && s.replayFile != nil
}

// endWatchedMatch ends a watched match as Esc used to: the viewer left
// (replayStopped), and the match ends as quit (sys.esc).
func (s *System) endWatchedMatch() {
	rf := s.replayFile
	if rf != nil {
		if rf.failure == "" {
			rf.viewerLeft = true
		}
		rf.Close()
	}
	s.esc = true
}

// watchMatchVolume returns the watched match's volume while the window is
// set and the match runs.
func (s *System) watchMatchVolume() (int32, bool) {
	if !s.watchWindow.active || !s.gameRunning {
		return 100, false
	}
	return s.watchWindow.volume, true
}

// scaleWatchVolume scales a fight sound's volume to the watched match's.
func (s *System) scaleWatchVolume(volumescale int32) int32 {
	if volume, ok := s.watchMatchVolume(); ok {
		return volumescale * volume / 100
	}
	return volumescale
}

// watchCapturesEsc reports whether Esc belongs to the lobby's menus rather
// than to the match.
func (s *System) watchCapturesEsc() bool {
	return s.watchWindow.active && !s.watchSessionActive()
}

// watchSessionActive reports a netplay or rollback session: the window never
// takes its Esc or input.
func (s *System) watchSessionActive() bool {
	return s.netConnection != nil || s.rollback.session != nil
}

// watchSessionBackstop clears a watch window left set when a netplay or
// rollback match starts: the lobby clears it after each watched match, so
// this is only for an error path that skipped that.
func (s *System) watchSessionBackstop() {
	if s.watchWindow.active && s.watchSessionActive() {
		LogMessage("Watch window: still set when a netplay match started; cleared")
		s.clearWatchWindow()
	}
}

// watchOwnsLua reports whether the Lua running now is the lobby's own around
// the window: a window hook, or Lua's own loop while no match runs. Lua the
// watched match runs ([Common] Lua, mods) is the match's.
func (s *System) watchOwnsLua() bool {
	w := &s.watchWindow
	return w.active && (w.inHook || !s.gameRunning) && !s.watchSessionActive()
}

// vsScreenLoading is Config.VsScreenLoading as loading sees it: off while
// the watch window is set, whose loading is the engine's (watchIdleFrame).
func (s *System) vsScreenLoading() bool {
	return s.cfg.Config.VsScreenLoading && !s.watchWindow.active
}

// captureWatchEsc runs after each event poll: an Esc from any source (the
// keyboard, Android's back button or on-screen keyboard) is the lobby's while
// the window is set, never the match's.
func (s *System) captureWatchEsc() {
	if !s.watchCapturesEsc() {
		return
	}
	// A watched replay that stopped or failed (liveFail, endWatchedMatch)
	// ends the match with sys.esc: that one is the match's, and an event
	// poll before the match loop sees it must leave it.
	if s.replayFile != nil && s.replayFile.isClosed() {
		return
	}
	if s.esc {
		s.watchWindow.escLatch = true
		s.esc = false
	}
	s.escPending = false
}

// luaEsc is Lua's esc(): the lobby's Esc while the window is set.
// Lua the watched match runs itself sees the match's sys.esc.
func (s *System) luaEsc() bool {
	if s.watchOwnsLua() {
		return s.watchWindow.esc
	}
	return s.esc
}

func (s *System) setLuaEsc(v bool) {
	if s.watchOwnsLua() {
		s.watchWindow.esc = v
		return
	}
	s.esc = v
}

// uiInputLists returns the command lists a Lua input read reads: the local
// ones inside withLuaUIInput for the lobby's own Lua.
func (s *System) uiInputLists() []*CommandList {
	if s.watchWindow.luaInput {
		return s.watchWindow.uiLists
	}
	return s.commandLists
}

// uiInputGuard is the per-frame UI input bookkeeping uiRawInput keeps.
type uiInputGuard struct {
	lastController int
	lastToken      string
	repeatToken    string
	repeatCtrl     int
	repeatFrame    int32
	consumeFrame   int32
}

func (s *System) swapUIInputGuard(g *uiInputGuard) {
	g.lastController, s.lastInputController = s.lastInputController, g.lastController
	g.lastToken, s.uiLastInputToken = s.uiLastInputToken, g.lastToken
	g.repeatToken, s.uiRepeatToken = s.uiRepeatToken, g.repeatToken
	g.repeatCtrl, s.uiRepeatController = s.uiRepeatController, g.repeatCtrl
	g.repeatFrame, s.uiRepeatFrame = s.uiRepeatFrame, g.repeatFrame
	g.consumeFrame, s.uiConsumeInputFrame = s.uiConsumeInputFrame, g.consumeFrame
}

// luaResetTokenGuard is Lua's resetTokenGuard(): the lobby's resets the
// window's own guard, never the one the watched match's reads use.
func (s *System) luaResetTokenGuard() {
	w := &s.watchWindow
	if w.luaInput || !s.watchOwnsLua() {
		// Inside withLuaUIInput the window's guard is the one in place.
		s.uiResetTokenGuard()
		return
	}
	w.uiGuard.consumeFrame = int32(w.frame) + 1
	w.uiGuard.lastToken = ""
	w.uiGuard.lastController = -1
}

// withLuaUIInput runs a Lua menu input read (getInput, getInputTime): for
// the lobby's own Lua while the watch window is set (watchOwnsLua) it reads
// the local lists with the window's own UI bookkeeping, and the engine's is
// left as it was. The window's guard counts the window's frames (which
// advance while loading too), not the match's frameCounter, which stands
// still while loading and would make every press then the same frame's.
func (s *System) withLuaUIInput(fn func()) {
	w := &s.watchWindow
	if len(w.uiLists) == 0 || !s.watchOwnsLua() || w.luaInput {
		fn()
		return
	}
	w.luaInput = true
	s.swapUIInputGuard(&w.uiGuard)
	frame := s.frameCounter
	s.frameCounter = int32(w.frame)
	defer func() {
		s.frameCounter = frame
		s.swapUIInputGuard(&w.uiGuard)
		w.luaInput = false
	}()
	fn()
}

// luaUIRawInput is uiRawInput for Lua's getInput.
func (s *System) luaUIRawInput(btns []string, controllerIdx int) (ok bool) {
	s.withLuaUIInput(func() { ok = s.uiRawInput(btns, controllerIdx) })
	return ok
}

func (s *System) resetWatchUILists() {
	w := &s.watchWindow
	w.uiGuard = uiInputGuard{lastController: -1, repeatCtrl: -1, repeatFrame: -1}
	w.uiLists = make([]*CommandList, len(s.commandLists))
	for i := range w.uiLists {
		cl := NewCommandList(NewInputBuffer())
		if err := s.uiApplyCommandRegistry(cl); err != nil {
			LogMessage("Watch window: menu commands: %v", err)
		}
		w.uiLists[i] = cl
	}
}

// stepWatchUILists reads the local controllers into the window's command
// lists, once per frame.
func (s *System) stepWatchUILists() {
	w := &s.watchWindow
	if !w.active || (w.uiStepped && w.uiFrame == w.frame) {
		return
	}
	w.uiStepped, w.uiFrame = true, w.frame
	if len(w.uiLists) != len(s.commandLists) {
		s.resetWatchUILists()
	}
	for i, cl := range w.uiLists {
		if cl == nil || cl.Buffer == nil {
			continue
		}
		// As stepCommandLists: commands step once the buffer has.
		if cl.localUIInputUpdate(i) {
			cl.Step(false, false, false, false, 0)
		}
	}
}

// watchRefreshFrame is a frame of Lua's own loop (refresh) while the window
// is set but no match runs (before launchFight reaches the fight, and
// after): the menus' local input steps, and esc() reports the Esc pressed
// since, as it does from the watch tick during the match.
func (s *System) watchRefreshFrame() {
	w := &s.watchWindow
	if !w.active || w.inHook {
		return
	}
	s.stepWatchUILists()
	if !s.gameRunning {
		w.esc = w.escLatch
		w.escLatch = false
	}
}

// runWatchHook runs a Lua hook for the window. An error is logged, once per
// message, not raised: a broken lobby script must not end the match.
func (s *System) runWatchHook(name string) {
	if s.luaLState == nil {
		return
	}
	w := &s.watchWindow
	prev := w.inHook
	w.inHook = true
	defer func() { w.inHook = prev }()
	err := s.luaLState.DoString("hook.run('" + name + "')")
	if err == nil {
		return
	}
	if w.hookErr == nil {
		w.hookErr = map[string]string{}
	}
	if msg := err.Error(); w.hookErr[name] != msg {
		w.hookErr[name] = msg
		LogMessage("Error executing Lua hook %s: %v", name, err)
	}
}

// runRenderHook runs a canvas hook of renderFrame: as it always did with a
// border, and logged instead of raised for the watch window.
func (s *System) runRenderHook(name string) {
	if s.watchWindow.active {
		s.runWatchHook(name)
		return
	}
	if err := s.luaLState.DoString("hook.run('" + name + "')"); err != nil {
		s.luaLState.RaiseError("Error executing Lua hook: %s\n%v", name, err)
	}
}

// runWatchTick runs 'game.watch_tick' once per engine frame while the window
// is set: the room's logic (poll, chat, menus). The Esc pressed since the
// last tick is what esc() reports during it.
func (s *System) runWatchTick() {
	w := &s.watchWindow
	if !w.active || w.inHook || (w.ticked && w.tickFrame == w.frame) {
		return
	}
	// Catching up, engine frames come 4x as fast: the room (its timers,
	// held-key repeat) keeps to about 60 Hz. The Esc latch waits for the
	// next tick.
	now := watchNow()
	if s.watchCatchingUp() && !w.lastTick.IsZero() && now.Sub(w.lastTick) < watchTickInterval {
		return
	}
	w.lastTick = now
	w.ticked, w.tickFrame = true, w.frame
	w.esc = w.escLatch
	w.escLatch = false
	s.stepWatchUILists()
	s.runWatchHook("game.watch_tick")
}

// watchTickInterval is the shortest wall time between two ticks of the room
// while the watched match catches up: 60 Hz, less a little for timer jitter.
const watchTickInterval = time.Second/60 - 2*time.Millisecond

// watchNow is the wall clock runWatchTick reads (tests replace it).
var watchNow = time.Now

// watchCatchingUp reports frames that run faster than real time: a live
// replay catching up with its stream. It reads the state update() last set
// (liveCatchingUp) rather than recomputing it.
func (s *System) watchCatchingUp() bool {
	rf := s.replayFile
	return rf != nil && rf.liveCatchUp && rf.liveStarted && !rf.liveEnded
}

// drawWatchCanvas draws the lobby's canvas hooks on a frame without a fight
// (loading, waiting for the stream). The window itself stays empty.
func (s *System) drawWatchCanvas() {
	if s.frameSkip || s.window == nil || s.window.renderingPaused {
		s.luaDiscardDrawQueue()
		return
	}
	outer := s.captureRenderSurfaceState()
	defer s.restoreRenderSurface(outer)
	s.beginRenderSurface(s.presentation.canvas, s.outerRenderState())
	s.luaDiscardDrawQueue()
	s.runWatchHook("game.border_bg")
	s.luaFlushDrawQueue()
	s.runWatchHook("game.border")
	s.luaFlushDrawQueue()
}

// watchIdleFrame is a frame of the lobby while the match is not drawn:
// loading, or a live replay waiting for its stream. It returns false when the
// engine is closing.
func (s *System) watchIdleFrame() bool {
	if s.watchWindow.inHook {
		return s.eventUpdate()
	}
	s.watchIdleStep()
	return s.await(s.gameRenderSpeed())
}

// watchIdleStep is watchIdleFrame's frame of the lobby: its tick, then its
// canvas.
func (s *System) watchIdleStep() {
	s.runWatchTick()
	// The lobby's background and animations step once per uiFrameCounter
	// (BGDef.step, Anim.Update), which nothing else advances while loading
	// or waiting. It is a UI counter, not match state.
	s.uiFrameCounter++
	s.drawWatchCanvas()
}

// cancelWatchedLoad ends a watched match the lobby stopped (stopWatching)
// while it loaded: the load is cancelled as Lua's loadCancel does, which
// drops what was loaded so far (a half-loaded character must never be
// reused by the next match) and leaves the loader idle, and the match ends
// as the viewer left it. watchStopRequested stays true for fight() to see.
func (s *System) cancelWatchedLoad() {
	s.loadCancel()
	s.endWatchedMatch()
}

// luaDrawQueue holds Lua draws set aside while others are flushed.
type luaDrawQueue struct {
	pre    []func()
	layers [][]func()
}

func (s *System) takeLuaDrawQueue() luaDrawQueue {
	q := luaDrawQueue{pre: s.luaDrawPreOps, layers: make([][]func(), len(s.luaDrawLayerOps))}
	copy(q.layers, s.luaDrawLayerOps[:])
	s.luaDrawPreOps = nil
	for i := range s.luaDrawLayerOps {
		s.luaDrawLayerOps[i] = nil
	}
	return q
}

// restoreLuaDrawQueue puts set-aside draws back before any queued since.
func (s *System) restoreLuaDrawQueue(q luaDrawQueue) {
	s.luaDrawPreOps = append(q.pre, s.luaDrawPreOps...)
	for i := range s.luaDrawLayerOps {
		if i < len(q.layers) {
			s.luaDrawLayerOps[i] = append(q.layers[i], s.luaDrawLayerOps[i]...)
		}
	}
}
