package main

import (
	"testing"
	"time"

	"github.com/veandco/go-sdl2/sdl"
	lua "github.com/yuin/gopher-lua"
)

// withWatchWindowState gives a test its own watch window, presentation,
// Esc, replay and menu input state, restored afterwards.
func withWatchWindowState(t *testing.T) {
	t.Helper()
	withKeyState(t)
	oldWatch, oldPres, oldBorder := sys.watchWindow, sys.presentation, sys.cfg.Video.Border
	oldLocal := sys.motif.Info.Localcoord
	oldEsc, oldPending, oldRunning := sys.esc, sys.escPending, sys.gameRunning
	oldReplay, oldLists, oldKeys, oldJoys := sys.replayFile, sys.commandLists, sys.keyConfig, sys.joystickConfig
	oldLua, oldConsume, oldFrame := sys.luaLState, sys.uiConsumeInputFrame, sys.frameCounter
	oldUIFrame, oldNet, oldSession := sys.uiFrameCounter, sys.netConnection, sys.rollback.session
	oldVsLoading, oldNow := sys.cfg.Config.VsScreenLoading, watchNow
	oldLastCtrl, oldLastTok := sys.lastInputController, sys.uiLastInputToken
	oldRepTok, oldRepCtrl, oldRepFrame := sys.uiRepeatToken, sys.uiRepeatController, sys.uiRepeatFrame
	t.Cleanup(func() {
		sys.uiFrameCounter, sys.netConnection, sys.rollback.session = oldUIFrame, oldNet, oldSession
		sys.cfg.Config.VsScreenLoading, watchNow = oldVsLoading, oldNow
		sys.lastInputController, sys.uiLastInputToken = oldLastCtrl, oldLastTok
		sys.uiRepeatToken, sys.uiRepeatController, sys.uiRepeatFrame = oldRepTok, oldRepCtrl, oldRepFrame
		sys.watchWindow, sys.presentation, sys.cfg.Video.Border = oldWatch, oldPres, oldBorder
		sys.motif.Info.Localcoord = oldLocal
		sys.esc, sys.escPending, sys.gameRunning = oldEsc, oldPending, oldRunning
		sys.replayFile, sys.commandLists, sys.keyConfig, sys.joystickConfig = oldReplay, oldLists, oldKeys, oldJoys
		sys.luaLState, sys.uiConsumeInputFrame, sys.frameCounter = oldLua, oldConsume, oldFrame
	})
	sys.watchWindow = watchWindowState{}
	sys.cfg.Video.Border.Enabled = false
	sys.presentation = presentationGeometry{canvas: [4]int32{0, 0, 640, 480}, game: [4]int32{0, 0, 640, 480}}
	sys.motif.Info.Localcoord = [2]int32{320, 240}
	sys.esc, sys.escPending, sys.gameRunning = false, false, true
	sys.replayFile = nil
	sys.commandLists = []*CommandList{NewCommandList(NewInputBuffer())}
	sys.keyConfig, sys.joystickConfig = nil, nil
	sys.luaLState = nil
	sys.uiConsumeInputFrame = 0
	sys.netConnection, sys.rollback.session = nil, nil
}

// inWatchHook runs fn as the lobby's hook code (runWatchHook) does.
func inWatchHook(fn func()) {
	prev := sys.watchWindow.inHook
	sys.watchWindow.inHook = true
	defer func() { sys.watchWindow.inHook = prev }()
	fn()
}

func hookLuaEsc() (v bool) {
	inWatchHook(func() { v = sys.luaEsc() })
	return v
}

func hookSetLuaEsc(v bool) { inWatchHook(func() { sys.setLuaEsc(v) }) }

func hookLuaUIRawInput(btns []string, controllerIdx int) (ok bool) {
	inWatchHook(func() { ok = sys.luaUIRawInput(btns, controllerIdx) })
	return ok
}

func TestWatchWindowPixels(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rect       [4]float32
		localcoord [2]int32
		canvas     [2]int32
		want       [4]int32
	}{
		// HDBZ: a 320x240 screenpack on a 640x480 game; 160x120 units are
		// a 320x240 window.
		{"hdbz", [4]float32{155, 20, 160, 120}, [2]int32{320, 240}, [2]int32{640, 480}, [4]int32{310, 40, 320, 240}},
		{"same size", [4]float32{100, 50, 640, 360}, [2]int32{1280, 720}, [2]int32{1280, 720}, [4]int32{100, 50, 640, 360}},
		// A 4:3 screenpack is centered on a widescreen canvas.
		{"4:3 on 16:9", [4]float32{0, 0, 320, 240}, [2]int32{320, 240}, [2]int32{1280, 720}, [4]int32{160, 0, 960, 720}},
		// A widescreen screenpack's left edge is the canvas's.
		{"16:9 on 16:9", [4]float32{0, 0, 427, 240}, [2]int32{427, 240}, [2]int32{1280, 720}, [4]int32{0, 0, 1281, 720}},
		{"no localcoord", [4]float32{160, 120, 160, 120}, [2]int32{0, 0}, [2]int32{320, 240}, [4]int32{160, 120, 160, 120}},
		// The window stays on the canvas.
		{"clipped", [4]float32{240, 200, 160, 120}, [2]int32{320, 240}, [2]int32{640, 480}, [4]int32{480, 400, 160, 80}},
	} {
		got := watchWindowPixels(tc.rect, tc.localcoord, tc.canvas[0], tc.canvas[1])
		// 427 units are not exactly 16:9; allow a pixel of rounding.
		if tc.name == "16:9 on 16:9" {
			if got[0] != 0 || got[1] != 0 || got[3] != 720 || got[2] < 1279 {
				t.Errorf("%s: got %v, want about %v", tc.name, got, tc.want)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFitAspectRect(t *testing.T) {
	rect := [4]int32{100, 50, 320, 240}
	if got := fitAspectRect(rect, 4.0/3.0); got != rect {
		t.Errorf("4:3 in 4:3 = %v, want %v", got, rect)
	}
	if got, want := fitAspectRect(rect, 16.0/9.0), [4]int32{100, 80, 320, 180}; got != want {
		t.Errorf("16:9 in 4:3 = %v, want %v", got, want)
	}
	if got, want := fitAspectRect(rect, 1), [4]int32{140, 50, 240, 240}; got != want {
		t.Errorf("1:1 in 4:3 = %v, want %v", got, want)
	}
	if got := fitAspectRect(rect, 0); got != rect {
		t.Errorf("no aspect = %v, want %v", got, rect)
	}
}

func TestWatchWindowSetAndClear(t *testing.T) {
	withWatchWindowState(t)
	if sys.presentationEnabled() {
		t.Fatal("presentation enabled without a border or window")
	}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 150)
	if !sys.presentationEnabled() || sys.borderConfigured() {
		t.Fatal("window: want presentation on, border off")
	}
	if got, want := sys.presentationGameRect(), [4]int32{310, 40, 320, 240}; got != want {
		t.Fatalf("game rect = %v, want %v", got, want)
	}
	if sys.watchWindow.volume != 100 {
		t.Errorf("volume = %d, want clamped to 100", sys.watchWindow.volume)
	}
	// A widescreen fight is letterboxed in the window.
	if got, want := sys.fitWatchGameRect(16.0/9.0), [4]int32{310, 70, 320, 180}; got != want {
		t.Errorf("fitted = %v, want %v", got, want)
	}
	// Setting it again keeps the viewport from before the first.
	sys.setWatchWindow([4]float32{0, 0, 160, 120}, 50)
	sys.clearWatchWindow()
	if sys.presentationEnabled() || sys.watchWindowActiveForTest() {
		t.Fatal("presentation still on after clear")
	}
	if got, want := sys.presentation.game, [4]int32{0, 0, 640, 480}; got != want {
		t.Errorf("game after clear = %v, want %v", got, want)
	}
}

func (s *System) watchWindowActiveForTest() bool { return s.watchWindow.active }

// Esc is the lobby's while the window is set: it never reaches the match
// (sys.esc, escPending), and esc() reports it for one watch tick.
func TestWatchWindowEscGating(t *testing.T) {
	withWatchWindowState(t)
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)

	OnKeyPressed(KeyEscape, 0)
	if sys.esc || sys.escPending {
		t.Fatalf("esc=%v pending=%v: Esc reached the match", sys.esc, sys.escPending)
	}
	if hookLuaEsc() {
		t.Fatal("esc() before the tick that reads it")
	}
	sys.runWatchTick()
	if !hookLuaEsc() {
		t.Fatal("esc() missed the Esc press")
	}
	// The same frame does not tick again.
	OnKeyReleased(KeyEscape, 0)
	hookSetLuaEsc(false)
	if hookLuaEsc() || sys.esc {
		t.Fatal("esc(false) did not clear the lobby's Esc, or touched the match's")
	}
	OnKeyPressed(KeyEscape, 0)
	sys.runWatchTick()
	if hookLuaEsc() {
		t.Fatal("ran twice in one frame")
	}
	sys.watchWindow.frame++
	sys.runWatchTick()
	if !hookLuaEsc() {
		t.Fatal("next frame's tick missed the Esc press")
	}
	sys.watchWindow.frame++
	sys.runWatchTick()
	if hookLuaEsc() {
		t.Fatal("Esc reported for more than one tick")
	}
	OnKeyReleased(KeyEscape, 0)

	// Outside a match (Lua's own loop, refresh), esc() reports the Esc of
	// the frame.
	sys.gameRunning = false
	OnKeyPressed(KeyEscape, 0)
	sys.watchWindow.frame++
	sys.watchRefreshFrame()
	if !sys.luaEsc() || sys.esc {
		t.Fatalf("refresh: esc()=%v match esc=%v", sys.luaEsc(), sys.esc)
	}
	sys.watchWindow.frame++
	sys.watchRefreshFrame()
	if sys.luaEsc() {
		t.Fatal("refresh: Esc reported for more than one frame")
	}
	OnKeyReleased(KeyEscape, 0)
	sys.gameRunning = true

	// A watched replay that failed or was stopped ends the match with
	// sys.esc; event polls leave that one alone.
	sys.replayFile = &ReplayFile{}
	sys.esc = true
	sys.captureWatchEsc()
	if !sys.esc {
		t.Fatal("a closed replay's Esc was taken by the lobby")
	}
	sys.replayFile, sys.esc = nil, false

	// An Esc set during an event poll by other sources (Android's on-screen
	// keyboard) is the lobby's too.
	sys.esc, sys.escPending = true, true
	sys.captureWatchEsc()
	if sys.esc || sys.escPending || !sys.watchWindow.escLatch {
		t.Fatalf("captured: esc=%v pending=%v latch=%v", sys.esc, sys.escPending, sys.watchWindow.escLatch)
	}

	// Without the window, Esc is the match's again.
	sys.clearWatchWindow()
	OnKeyPressed(KeyEscape, 0)
	if !sys.esc || !sys.escPending || !sys.luaEsc() {
		t.Fatalf("no window: esc=%v pending=%v", sys.esc, sys.escPending)
	}
}

// The menus read the local controllers while the window is set; the UI
// command lists the watched match reads are left alone.
func TestWatchWindowLocalMenuInput(t *testing.T) {
	withWatchWindowState(t)
	kc := KeyConfig{Joy: -1}
	kc.dD = int(sdl.K_DOWN)
	sys.keyConfig = []KeyConfig{kc}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	var lists []*CommandList
	inWatchHook(func() { sys.withLuaUIInput(func() { lists = sys.uiInputLists() }) })
	if len(lists) != 1 || lists[0] == sys.commandLists[0] {
		t.Fatal("menus do not read the window's own lists")
	}

	sys.keyState[sdl.K_DOWN] = true
	sys.frameCounter++
	sys.watchWindow.frame++
	sys.stepWatchUILists()
	// The engine's own reads (dialogue skip, win screens) keep reading the
	// match's lists: a watcher's press must not change the watched match.
	if sys.uiRawInput([]string{"D"}, -1) {
		t.Fatal("the engine read the local press")
	}
	engineGuard := [3]any{sys.uiRepeatFrame, sys.uiRepeatController, sys.uiRepeatToken}
	if !hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("local down press not read")
	}
	if sys.commandLists[0].Buffer.Db != 0 {
		t.Fatal("the match's UI command list was changed")
	}
	// Lua's read keeps its own repeat guard: the engine's is untouched, so a
	// lobby press never consumes a token the match reads that frame.
	if got := [3]any{sys.uiRepeatFrame, sys.uiRepeatController, sys.uiRepeatToken}; got != engineGuard {
		t.Fatalf("engine UI guard changed: %v -> %v", engineGuard, got)
	}
	if hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("the same token fired twice in one frame")
	}
	// Stepped once per frame.
	held := sys.watchWindow.uiLists[0].Buffer.Db
	sys.stepWatchUILists()
	if sys.watchWindow.uiLists[0].Buffer.Db != held {
		t.Fatal("stepped twice in one frame")
	}

	sys.clearWatchWindow()
	if len(sys.uiInputLists()) != 1 || sys.uiInputLists()[0] != sys.commandLists[0] {
		t.Fatal("menus still read the window's lists after clear")
	}
}

func TestWatchWindowStopAndVolume(t *testing.T) {
	withWatchWindowState(t)
	// Nothing to stop: no window, no replay.
	sys.requestWatchStop()
	if sys.watchWindow.stop {
		t.Fatal("stop requested without a watched match")
	}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 40)
	sys.requestWatchStop()
	if sys.watchStopRequested() {
		t.Fatal("stop applies without a replay")
	}
	rf := &ReplayFile{}
	sys.replayFile = rf
	if !sys.watchStopRequested() {
		t.Fatal("stop not requested")
	}
	sys.endWatchedMatch()
	if !rf.viewerLeft || !sys.esc {
		t.Fatalf("viewerLeft=%v esc=%v", rf.viewerLeft, sys.esc)
	}

	if got := sys.scaleWatchVolume(100); got != 40 {
		t.Errorf("fight sound volume = %d, want 40", got)
	}
	sys.gameRunning = false
	if got := sys.scaleWatchVolume(100); got != 100 {
		t.Errorf("volume outside the match = %d, want 100", got)
	}
	sys.clearWatchWindow()
	if sys.watchWindow.stop {
		t.Fatal("stop kept after clear")
	}
}

// game.watch_tick runs once per frame, and a failing hook is logged, never
// raised.
func TestWatchWindowTickHook(t *testing.T) {
	withWatchWindowState(t)
	l := lua.NewState()
	defer l.Close()
	sys.luaLState = l
	if err := l.DoString(`
		ticks = 0
		hook = {run = function(name)
			if name == 'game.watch_tick' then
				ticks = ticks + 1
				if fail then error('broken lobby') end
			end
		end}`); err != nil {
		t.Fatal(err)
	}
	ticks := func() int { return int(lua.LVAsNumber(l.GetGlobal("ticks"))) }

	sys.runWatchTick()
	if ticks() != 0 {
		t.Fatal("hook ran without a window")
	}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	sys.runWatchTick()
	sys.runWatchTick()
	if ticks() != 1 {
		t.Fatalf("ticks = %d in one frame, want 1", ticks())
	}
	l.SetGlobal("fail", lua.LTrue)
	sys.watchWindow.frame++
	sys.runWatchTick()
	sys.watchWindow.frame++
	sys.runWatchTick()
	if ticks() != 3 {
		t.Fatalf("ticks = %d, want 3", ticks())
	}
	if sys.watchWindow.inHook {
		t.Fatal("still marked in a hook after an error")
	}
}

// pressLocalDown sets the local down key and steps the window's lists on a
// new window frame (the match's frameCounter does not move, as while
// loading).
func pressLocalDown(down bool) {
	sys.keyState[sdl.K_DOWN] = down
	sys.watchWindow.frame++
	sys.stepWatchUILists()
}

func setupLocalDown(t *testing.T) {
	t.Helper()
	withWatchWindowState(t)
	kc := KeyConfig{Joy: -1}
	kc.dD = int(sdl.K_DOWN)
	sys.keyConfig = []KeyConfig{kc}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
}

// Lua the watched match runs itself ([Common] Lua's loop(), mods) reads the
// match's command lists and Esc, never the watcher's: only the lobby's hook
// code, or Lua while no match runs, gets the local ones.
func TestWatchWindowFightLuaReadsMatch(t *testing.T) {
	setupLocalDown(t)
	sys.commandLists[0].Buffer.Db = 1 // the match's press
	pressLocalDown(false)
	if !sys.luaUIRawInput([]string{"D"}, -1) {
		t.Fatal("fight-side getInput did not read the match's command lists")
	}
	var lists []*CommandList
	sys.withLuaUIInput(func() { lists = sys.uiInputLists() })
	if lists[0] != sys.commandLists[0] {
		t.Fatal("fight-side getInputTime does not read the match's lists")
	}
	if hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("the lobby's hook read the match's press")
	}
	// Outside a match, Lua's own loop is the lobby's.
	sys.gameRunning = false
	sys.withLuaUIInput(func() { lists = sys.uiInputLists() })
	if lists[0] != sys.watchWindow.uiLists[0] {
		t.Fatal("refresh-time Lua does not read the window's lists")
	}
	sys.gameRunning = true

	// Esc: fight-side Lua sees and sets the match's.
	sys.watchWindow.esc = true
	if sys.luaEsc() {
		t.Fatal("fight-side esc() reported the lobby's Esc")
	}
	sys.setLuaEsc(true)
	if !sys.esc || !hookLuaEsc() {
		t.Fatalf("fight-side esc(true): match esc=%v lobby esc=%v", sys.esc, hookLuaEsc())
	}
	hookSetLuaEsc(false)
	if !sys.esc || hookLuaEsc() {
		t.Fatal("the lobby's esc(false) touched the match's Esc")
	}
}

// The lobby's resetTokenGuard() resets the window's guard only, and its
// guard counts the window's frames, so presses while loading (frameCounter
// standing still) all register.
func TestWatchWindowTokenGuard(t *testing.T) {
	setupLocalDown(t)
	sys.frameCounter = 100
	sys.uiConsumeInputFrame, sys.lastInputController, sys.uiLastInputToken = 0, 3, "a"
	inWatchHook(sys.luaResetTokenGuard)
	if sys.uiConsumeInputFrame != 0 || sys.lastInputController != 3 || sys.uiLastInputToken != "a" {
		t.Fatalf("the engine's guard changed: consume=%d last=%d tok=%q",
			sys.uiConsumeInputFrame, sys.lastInputController, sys.uiLastInputToken)
	}
	pressLocalDown(true)
	if hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("read on the frame the reset consumes")
	}
	pressLocalDown(false)
	pressLocalDown(true)
	if !hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("press after the reset frame not read")
	}
	pressLocalDown(false)
	pressLocalDown(true)
	if !hookLuaUIRawInput([]string{"D"}, -1) {
		t.Fatal("second press while frameCounter stood still was dropped")
	}
	if sys.frameCounter != 100 || sys.uiConsumeInputFrame != 0 || sys.lastInputController != 3 {
		t.Fatal("the engine's frame or guard changed")
	}
	// Without the window, resetTokenGuard is the engine's as before.
	sys.clearWatchWindow()
	sys.luaResetTokenGuard()
	if sys.uiConsumeInputFrame != 101 || sys.lastInputController != -1 {
		t.Fatal("resetTokenGuard without a window did not reset the engine's guard")
	}
}

// Idle frames (loading, stalls) advance the UI frame counter so the lobby's
// background animates.
func TestWatchWindowIdleStepAnimates(t *testing.T) {
	withWatchWindowState(t)
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	before := sys.uiFrameCounter
	sys.watchIdleStep()
	sys.watchWindow.frame++
	sys.watchIdleStep()
	if sys.uiFrameCounter != before+2 {
		t.Fatalf("uiFrameCounter = %d, want %d", sys.uiFrameCounter, before+2)
	}
}

// Catching up (4x frames), the room ticks at about 60 Hz of wall time.
func TestWatchWindowTickCatchUpPace(t *testing.T) {
	withWatchWindowState(t)
	l := lua.NewState()
	defer l.Close()
	sys.luaLState = l
	if err := l.DoString(`ticks = 0
		hook = {run = function(name) if name == 'game.watch_tick' then ticks = ticks + 1 end end}`); err != nil {
		t.Fatal(err)
	}
	ticks := func() int { return int(lua.LVAsNumber(l.GetGlobal("ticks"))) }
	now := time.Unix(1000, 0)
	watchNow = func() time.Time { return now }
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	step := func(d time.Duration) {
		now = now.Add(d)
		sys.watchWindow.frame++
		sys.runWatchTick()
	}
	// Not catching up: every frame ticks, however fast.
	for i := 0; i < 4; i++ {
		step(4 * time.Millisecond)
	}
	if ticks() != 4 {
		t.Fatalf("ticks = %d, want 4", ticks())
	}
	sys.replayFile = &ReplayFile{liveStarted: true, liveCatchUp: true}
	for i := 0; i < 16; i++ { // 64 ms of 4 ms frames
		step(4 * time.Millisecond)
	}
	if got := ticks() - 4; got < 3 || got > 5 {
		t.Fatalf("catch-up ticks in 64 ms = %d, want about 4", got)
	}
	// The Esc pressed on a skipped frame waits for the next tick.
	sys.watchWindow.escLatch = true
	step(time.Millisecond)
	if sys.watchWindow.escLatch == false {
		t.Fatal("skipped tick consumed the Esc latch")
	}
}

// A netplay or rollback session never runs in the window: it gives up Esc
// and input, and the backstop clears it.
func TestWatchWindowSessionBackstop(t *testing.T) {
	setupLocalDown(t)
	sys.rollback.session = &RollbackSession{}
	if sys.watchCapturesEsc() {
		t.Fatal("Esc captured during a rollback session")
	}
	sys.watchWindow.esc = true
	if hookLuaEsc() {
		t.Fatal("hook esc() is the window's during a session")
	}
	var lists []*CommandList
	inWatchHook(func() { sys.withLuaUIInput(func() { lists = sys.uiInputLists() }) })
	if lists[0] != sys.commandLists[0] {
		t.Fatal("hook input is the window's during a session")
	}
	sys.watchSessionBackstop()
	if sys.watchWindow.active {
		t.Fatal("backstop left the window set")
	}
	if got, want := sys.presentation.game, [4]int32{0, 0, 640, 480}; got != want {
		t.Fatalf("game rect = %v, want %v", got, want)
	}
	// Without a session it does nothing.
	sys.rollback.session = nil
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	sys.watchSessionBackstop()
	if !sys.watchWindow.active || !sys.watchCapturesEsc() {
		t.Fatal("backstop cleared a window without a session")
	}
}

// The window's loading is the engine's, without touching the config.
func TestWatchWindowForcesEngineLoading(t *testing.T) {
	withWatchWindowState(t)
	sys.cfg.Config.VsScreenLoading = true
	if !sys.vsScreenLoading() {
		t.Fatal("VS screen loading off without a window")
	}
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	if sys.vsScreenLoading() || !sys.cfg.Config.VsScreenLoading {
		t.Fatal("window: want VS screen loading off, config untouched")
	}
	sys.clearWatchWindow()
	if !sys.vsScreenLoading() {
		t.Fatal("VS screen loading still off after clear")
	}
}

// A stop during loading cancels the load as loadCancel does (the loader is
// idle, partial data dropped) and ends the match as left; fight() still
// sees the stop.
func TestWatchWindowCancelLoad(t *testing.T) {
	withWatchWindowState(t)
	oldState, oldChars, oldCgi := sys.loader.state, sys.chars, sys.cgi
	oldStage, oldStageList, oldFfx := sys.stage, sys.stageList, sys.ffx
	t.Cleanup(func() {
		sys.loader.state, sys.chars, sys.cgi = oldState, oldChars, oldCgi
		sys.stage, sys.stageList, sys.ffx = oldStage, oldStageList, oldFfx
	})
	sys.setWatchWindow([4]float32{155, 20, 160, 120}, 50)
	rf := &ReplayFile{}
	sys.replayFile = rf
	sys.requestWatchStop()
	sys.loader.state = LS_Complete
	sys.cgi[0].states = map[int32]*StateBytecode{}
	sys.cancelWatchedLoad()
	if sys.loader.state != LS_NotYet {
		t.Fatalf("loader state = %v, want LS_NotYet", sys.loader.state)
	}
	if sys.cgi[0].states != nil {
		t.Fatal("partly loaded character data kept")
	}
	if !rf.viewerLeft || !sys.esc || !sys.watchStopRequested() {
		t.Fatalf("viewerLeft=%v esc=%v stop=%v", rf.viewerLeft, sys.esc, sys.watchStopRequested())
	}
}
