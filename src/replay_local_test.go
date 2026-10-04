package main

import (
	"encoding/json"
	"math"
	"testing"
)

// localReplayTestInput gives input slots 0 and 1 keyboards (keys 1000+n and
// 2000+n for button n) and restores the system's input state afterwards.
func localReplayTestInput(t *testing.T) {
	t.Helper()
	keyConfig, joystickConfig, keyState := sys.keyConfig, sys.joystickConfig, sys.keyState
	remap, gameRunning, debugAccel := sys.inputRemap, sys.gameRunning, sys.debugAccel
	gameSpeed, gameSpeedStep := sys.cfg.Options.GameSpeed, sys.cfg.Options.GameSpeedStep
	t.Cleanup(func() {
		sys.keyConfig, sys.joystickConfig, sys.keyState = keyConfig, joystickConfig, keyState
		sys.inputRemap, sys.gameRunning, sys.debugAccel = remap, gameRunning, debugAccel
		sys.cfg.Options.GameSpeed, sys.cfg.Options.GameSpeedStep = gameSpeed, gameSpeedStep
		sys.roundResetFlg, sys.reloadFlg, sys.loadStateFlag = false, false, false
		lastMatchReplayFile = ""
	})
	sys.keyConfig = make([]KeyConfig, 2)
	for slot := range sys.keyConfig {
		var keys [14]int
		for n := range keys {
			keys[n] = 1000*(slot+1) + n
		}
		sys.keyConfig[slot].Joy = -1
		sys.keyConfig[slot].set(keys)
	}
	sys.joystickConfig = nil
	sys.keyState = make(map[Key]bool)
	sys.resetRemapInput()
	sys.gameRunning = true
	sys.debugAccel = 1
	sys.cfg.Options.GameSpeed, sys.cfg.Options.GameSpeedStep = 0, 5
}

// hold sets button n (0 up ... 4 a ... 10 start) of input slot to down.
func hold(slot, n int, down bool) {
	sys.keyState[Key(1000*(slot+1)+n)] = down
}

func readLocalReplay(t *testing.T) *matchReplayData {
	t.Helper()
	if lastMatchReplayFile == "" {
		t.Fatal("no match replay saved")
	}
	data, err := readMatchReplay(lastMatchReplayFile)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The recorder keeps one frame per pass of the loop where the replay moves
// on, when the match ran on it: what every read of an input slot got during
// it, the first reading of the slot.
func TestLocalReplayFrames(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	info := json.RawMessage(`{"kind":"local","selection":{"p1":[{"name":"Kyo"}],"p2":[{"name":"Iori"}]}}`)
	if err := r.arm(t.TempDir(), info); err != nil {
		t.Fatal(err)
	}
	r.synchronize()
	if !r.pending {
		t.Fatal("synchronize did not start a session")
	}
	r.begin(nil)
	if !r.open {
		t.Fatal("no session began")
	}

	// Frame 0: slot 0 holds A. Two reads of slot 0 (two characters sharing a
	// controller) get the same reading, even after the button is let go.
	hold(0, 4, true)
	r.pass(true)
	b, _, ok := r.input(0)
	if !ok || !b[4] {
		t.Fatalf("frame 0 slot 0: %v %v", b, ok)
	}
	hold(0, 4, false)
	if b, _, _ = r.input(0); !b[4] {
		t.Fatal("a second read of the frame got a new reading")
	}
	r.advance()

	// A paused pass: nothing is kept, and the pause menu reads the
	// controllers directly.
	hold(1, 5, true)
	r.pass(false)
	if _, _, ok := r.input(1); ok {
		t.Fatal("a paused pass read the recorded frame")
	}
	r.advance()

	// A pass that runs the match without reading an input (a slowdown pass
	// without a tick): an empty frame.
	r.pass(true)
	r.advance()

	// Two passes before the loop moves on (the game speed is above normal):
	// one frame, read once.
	r.pass(true)
	if b, _, _ = r.input(1); !b[5] {
		t.Fatal("frame 2 slot 1: B not held")
	}
	hold(1, 5, false)
	r.pass(true)
	if b, _, _ = r.input(1); !b[5] {
		t.Fatal("the second pass of frame 2 got a new reading")
	}
	if pressed, ok := r.anyButton(); !ok || !pressed {
		t.Fatal("anyButton does not see frame 2's B")
	}
	r.advance()

	// The last frame, the loop ending before it moves on: kept by end.
	hold(0, 10, true)
	r.pass(true)
	if pressed, _ := r.anyButton(); !pressed {
		t.Fatal("anyButton does not see Start")
	}
	r.end()
	r.finish()

	data := readLocalReplay(t)
	if len(data.Sessions) != 1 || data.Frames != 4 {
		t.Fatalf("%d sessions, %d frames; want 1, 4", len(data.Sessions), data.Frames)
	}
	want := [4][2]InputBits{{IB_A, 0}, {0, 0}, {0, IB_B}, {IB_S, 0}}
	for f, w := range want {
		got, ok := data.Sessions[0].Frame(int32(f))
		if !ok || got.Inputs[0] != w[0] || got.Inputs[1] != w[1] {
			t.Errorf("frame %d: %v, want slots 0 and 1 %v", f, got.Inputs, w)
		}
	}
	h := data.Sessions[0].Header()
	if h.Seed != r.seed || h.Params == nil || h.Context == nil || h.Rules == nil || h.ReplayHeader == nil {
		t.Fatalf("header %+v", h)
	}
}

// Each runMatch of a game is a session of the same match; something that
// changes the match from outside it ends the recording.
func TestLocalReplaySessionsAndInterrupt(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
		t.Fatal(err)
	}
	run := func(frames int) {
		for i := 0; i < frames; i++ {
			r.pass(true)
			r.input(0)
			r.advance()
		}
	}
	// A Turns match: two runMatch.
	r.synchronize()
	r.begin(nil)
	run(10)
	r.end()
	r.synchronize()
	r.begin(nil)
	run(5)
	// A hotkey resets the round: the frames before it are saved.
	before := r.watch()
	r.pass(true)
	r.input(0)
	sys.roundResetFlg = true
	r.check(before)
	if r.open || r.armed {
		t.Fatal("the recording continued after the round was reset")
	}
	// The rest of the game is not recorded.
	r.end()
	r.synchronize()
	if r.pending {
		t.Fatal("a session began after the recording stopped")
	}
	r.finish()
	data := readLocalReplay(t)
	if len(data.Sessions) != 2 || data.Frames != 16 {
		t.Fatalf("%d sessions, %d frames; want 2, 16 (10, then 5 and the interrupted pass)", len(data.Sessions), data.Frames)
	}
	if s := data.Sessions[1].Header().Segment; s != 1 {
		t.Fatalf("second session is segment %d", s)
	}
}

// A pause that starts in the middle of a frame (the game runs faster than
// normal: a frame is two passes) draws frames without keeping one; the
// frame's second pass, after the pause, completes it with the same reading.
func TestLocalReplayPauseInsideFrame(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
		t.Fatal(err)
	}
	r.synchronize()
	r.begin(nil)
	hold(0, 4, true)
	r.pass(true) // the frame's first pass: it catches up, and draws nothing
	r.input(0)
	hold(0, 4, false)
	for i := 0; i < 3; i++ { // the pause menu opened: paused passes, drawn
		r.pass(false)
		r.advance()
	}
	r.pass(true) // the frame's second pass
	if b, _, _ := r.input(0); !b[4] {
		t.Fatal("the frame's second pass got a new reading")
	}
	r.advance()
	r.pass(true)
	r.input(0)
	r.advance()
	r.end()
	r.finish()
	data := readLocalReplay(t)
	if data.Frames != 2 {
		t.Fatalf("%d frames, want 2", data.Frames)
	}
	if f, _ := data.Sessions[0].Frame(0); f.Inputs[0] != IB_A {
		t.Fatalf("frame 0: %v", f.Inputs[0])
	}
}

// A match longer than a replay holds is saved up to that length.
func TestLocalReplayFrameLimit(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
		t.Fatal(err)
	}
	r.synchronize()
	r.begin(nil)
	// Earlier sessions of the match saved all but 3 frames of the limit.
	r.rec.frames = matchReplayMaxFrames - 3
	for i := 0; i < 5; i++ {
		r.pass(true)
		r.input(0)
		r.advance()
	}
	if r.open || r.armed {
		t.Fatal("still recording past the limit")
	}
	r.finish()
	if data := readLocalReplay(t); data.Frames != 3 {
		t.Fatalf("%d frames saved, want 3", data.Frames)
	}
}

// A character resetting the round or restarting the match (the MatchRestart
// controller acts only in local play) ends the recording, as does a
// character loading a state saved from outside the match; the character's
// own saved states replay.
func TestLocalReplayCharacterChanges(t *testing.T) {
	t.Cleanup(func() { sys.saveStateFlag = false })
	// record runs a pass per step, and reports whether the recording stopped.
	record := func(t *testing.T, steps ...func(r *localReplayRecorder)) bool {
		t.Helper()
		localReplayTestInput(t)
		r := &localReplayRecorder{}
		if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
			t.Fatal(err)
		}
		r.synchronize()
		r.begin(nil)
		for _, step := range steps {
			r.pass(true)
			r.input(0)
			step(r)
			// The loop carries the requests out and clears them.
			sys.saveStateFlag, sys.loadStateFlag, sys.roundResetFlg, sys.reloadFlg = false, false, false, false
			r.advance()
		}
		stopped := !r.open
		r.finish()
		return stopped
	}
	inMatch := func(set *bool) func(r *localReplayRecorder) {
		return func(r *localReplayRecorder) {
			before := r.watch()
			*set = true
			r.checkMatch(before)
		}
	}
	outside := func(set *bool) func(r *localReplayRecorder) {
		return func(r *localReplayRecorder) {
			before := r.watch()
			*set = true
			r.check(before)
		}
	}
	none := func(*localReplayRecorder) {}
	for name, c := range map[string]struct {
		passes  []func(r *localReplayRecorder)
		stopped bool
		frames  int32
	}{
		"reset round": {[]func(*localReplayRecorder){none, inMatch(&sys.roundResetFlg), none}, true, 2},
		"restart":     {[]func(*localReplayRecorder){inMatch(&sys.reloadFlg), none}, true, 1},
		"own state":   {[]func(*localReplayRecorder){inMatch(&sys.saveStateFlag), inMatch(&sys.loadStateFlag), none}, false, 3},
		"hotkey save": {[]func(*localReplayRecorder){outside(&sys.saveStateFlag), none, inMatch(&sys.loadStateFlag), none}, true, 3},
		"resaved":     {[]func(*localReplayRecorder){outside(&sys.saveStateFlag), inMatch(&sys.saveStateFlag), inMatch(&sys.loadStateFlag)}, false, 3},
	} {
		t.Run(name, func(t *testing.T) {
			if stopped := record(t, c.passes...); stopped != c.stopped {
				t.Fatalf("stopped %v, want %v", stopped, c.stopped)
			}
			if data := readLocalReplay(t); data.Frames != c.frames {
				t.Fatalf("%d frames saved, want %d", data.Frames, c.frames)
			}
		})
	}
}

// A match run by replay timing (a recorded local match, a replay) pauses
// between any two passes, holds its frame time while paused, and moves it on
// after a pass that ran the match although a pause began during it.
func TestReplayTimingPause(t *testing.T) {
	paused, frameStep, open, turbo := sys.paused, sys.frameStepFlag, localReplay.open, sys.turbo
	tickCount, oldTickCount, tickCountF, lastTick := sys.tickCount, sys.oldTickCount, sys.tickCountF, sys.lastTick
	nextAddTime, oldNextAddTime := sys.nextAddTime, sys.oldNextAddTime
	t.Cleanup(func() {
		sys.paused, sys.frameStepFlag, localReplay.open, sys.turbo = paused, frameStep, open, turbo
		sys.tickCount, sys.oldTickCount, sys.tickCountF, sys.lastTick = tickCount, oldTickCount, tickCountF, lastTick
		sys.nextAddTime, sys.oldNextAddTime = nextAddTime, oldNextAddTime
	})
	// Half speed (the slow motion after a KO): a pass without a tick.
	sys.turbo = 0.5
	sys.tickCount, sys.oldTickCount, sys.tickCountF, sys.nextAddTime = 3, 3, 3.5, 0.5
	sys.paused, sys.frameStepFlag = true, false
	localReplay.open = false
	if sys.debugPaused() {
		t.Fatal("unrecorded play pauses between ticks")
	}
	if !sys.tickNextFrame() {
		t.Fatal("unrecorded play: the next frame does not tick")
	}
	localReplay.open = true
	if !sys.debugPaused() || !sys.replayTiming() {
		t.Fatal("a recorded match does not pause between ticks")
	}
	// Nothing ticks while it is paused, drawing included.
	if sys.tickFrame() || sys.tickNextFrame() {
		t.Fatal("a paused recorded match ticks")
	}
	// Paused passes hold the frame time.
	for i := 0; i < 3; i++ {
		if !sys.nextFrameTime(false) || sys.tickCountF != 3.5 || sys.tickCount != 3 || sys.oldTickCount != 3 {
			t.Fatalf("a paused pass moved the frame time: %v %d %d", sys.tickCountF, sys.tickCount, sys.oldTickCount)
		}
	}
	// A pass that ran the match before the pause began moves it on.
	sys.nextFrameTime(true)
	if sys.tickCountF != 4 || sys.tickCount != 4 || sys.oldTickCount != 3 {
		t.Fatalf("the pass's frame time: %v %d %d", sys.tickCountF, sys.tickCount, sys.oldTickCount)
	}
}

// A match run by replay timing keeps what it reads that code run while it is
// paused can change, as it was when the pause started: the menus' reading of
// the players' input (the dialogues and scripts read it as the match runs)
// and the random numbers.
func TestHoldPausedState(t *testing.T) {
	lists, open, seed := sys.commandLists, localReplay.open, sys.randseed
	t.Cleanup(func() { sys.commandLists, localReplay.open, pausedState, sys.randseed = lists, open, nil, seed })
	cl := NewCommandList(NewInputBuffer())
	cl.Commands = [][]Command{{{name: "a", curtime: 2, completed: []bool{false, true}, stepTimers: []int32{3}}}}
	sys.commandLists = []*CommandList{cl}
	buffer, reader := cl.Buffer, cl.Buffer.InputReader
	cl.Buffer.ab, cl.Buffer.InputReader.SocdFirst[0] = 1, true
	sys.randseed = 1234

	pausedState = nil
	localReplay.open = true
	sys.holdPausedState(true)
	// The pause menu reads the controllers; a script draws random numbers.
	cl.Buffer.ab, cl.Buffer.mb, cl.Buffer.InputReader.SocdFirst[0] = 7, 1, false
	cl.Commands[0][0].curtime, cl.Commands[0][0].completed[1] = 9, false
	Rand(0, 99)
	sys.holdPausedState(true)
	sys.holdPausedState(false)
	if cl.Buffer != buffer || cl.Buffer.InputReader != reader {
		t.Fatal("the buffers were replaced")
	}
	if cl.Buffer.ab != 1 || cl.Buffer.mb != 0 || !cl.Buffer.InputReader.SocdFirst[0] {
		t.Fatalf("buffer %+v, reader %+v", *cl.Buffer, *cl.Buffer.InputReader)
	}
	if c := cl.Commands[0][0]; c.curtime != 2 || !c.completed[1] || c.stepTimers[0] != 3 {
		t.Fatalf("command %+v", c)
	}
	if sys.randseed != 1234 {
		t.Fatalf("random seed %d, want 1234", sys.randseed)
	}

	// Unrecorded play keeps what the menu read.
	localReplay.open = false
	sys.holdPausedState(true)
	cl.Buffer.ab = 7
	sys.holdPausedState(false)
	if cl.Buffer.ab != 7 || pausedState != nil {
		t.Fatal("unrecorded play restored the menus' input")
	}
}

// A frame whose passes the loop left for a paused pass (the player quit from
// the pause menu) is not kept: its replay would run the rest of it.
func TestLocalReplayEndsOnPausedPass(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
		t.Fatal(err)
	}
	r.synchronize()
	r.begin(nil)
	r.pass(true)
	r.input(0)
	r.advance()
	r.pass(true) // the next frame's first pass
	r.input(0)
	r.pass(false) // paused; the match ends from the pause menu
	r.end()
	r.finish()
	if data := readLocalReplay(t); data.Frames != 1 {
		t.Fatalf("%d frames, want 1", data.Frames)
	}
}

// IsHost is false in local play and in its replay; a replay of an online
// match has the host the match had.
func TestIsHostInReplays(t *testing.T) {
	savedFile, savedLevels := sys.replayFile, sys.aiLevel
	t.Cleanup(func() { sys.replayFile, sys.aiLevel = savedFile, savedLevels })
	sys.aiLevel = [len(sys.aiLevel)]float32{}
	c := &Char{playerNo: 0}
	sys.replayFile = &ReplayFile{local: true}
	if c.isHost() {
		t.Fatal("a local match's replay has a host")
	}
	sys.replayFile = &ReplayFile{}
	if !c.isHost() {
		t.Fatal("an online match's replay has no host")
	}
}

// The game speed changing, or the input slots, ends the recording before
// the match runs again.
func TestLocalReplayUnreplayableChanges(t *testing.T) {
	for name, change := range map[string]func(){
		"speed": func() { sys.cfg.Options.GameSpeed++ },
		"accel": func() { sys.debugAccel = 2 },
		"slots": func() { sys.inputRemap[0], sys.inputRemap[1] = 1, 0 },
	} {
		t.Run(name, func(t *testing.T) {
			localReplayTestInput(t)
			var r localReplayRecorder
			if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
				t.Fatal(err)
			}
			r.synchronize()
			r.begin(nil)
			r.pass(true)
			r.advance()
			change()
			r.pass(true)
			if r.open {
				t.Fatal("still recording")
			}
			r.finish()
			if data := readLocalReplay(t); data.Frames != 1 {
				t.Fatalf("%d frames saved, want 1", data.Frames)
			}
		})
	}
}

// A replay reads its next frame only when the match ran on the current one.
func TestLiveReplayHoldsFrameWhilePaused(t *testing.T) {
	localReplayTestInput(t)
	var r localReplayRecorder
	if err := r.arm(t.TempDir(), json.RawMessage(`{"kind":"local"}`)); err != nil {
		t.Fatal(err)
	}
	r.synchronize()
	r.begin(nil)
	for f := 0; f < 3; f++ {
		hold(0, 4+f, true)
		r.pass(true)
		r.input(0)
		r.advance()
		hold(0, 4+f, false)
	}
	r.end()
	r.finish()
	data := readLocalReplay(t)
	rf, err := NewLiveReplayFile(data.Sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	rf.fromFile = true
	rf.liveSynchronize()
	if rf.ibit[0] != IB_A {
		t.Fatalf("frame 0: %v", rf.ibit[0])
	}
	rf.frameUsed = true
	rf.Update()
	if rf.ibit[0] != IB_B {
		t.Fatalf("frame 1: %v", rf.ibit[0])
	}
	// Paused: the loop moves on without running the match.
	rf.Update()
	rf.Update()
	if rf.ibit[0] != IB_B {
		t.Fatalf("a paused replay moved to %v", rf.ibit[0])
	}
	rf.frameUsed = true
	rf.Update()
	if rf.ibit[0] != IB_C {
		t.Fatalf("frame 2: %v", rf.ibit[0])
	}
}

// The context and game parameters a replay recorded replace the ones the
// screens set.
func TestReplayMatchContextAndParams(t *testing.T) {
	savedContext := *currentMatchContext()
	savedDialogue := sys.motif.di.enabled
	savedParams := sys.sel.gameParams
	defer func() {
		savedContext.apply()
		sys.motif.di.enabled = savedDialogue
		sys.sel.gameParams = savedParams
	}()
	sys.sel.gameParams = newGameParams()
	savedCount, savedSessionCount := sys.persistRoundCount, sessionRoundCount
	defer func() { sys.persistRoundCount, sessionRoundCount = savedCount, savedSessionCount }()
	c := &ReplayMatchContext{MatchNo: 4, Home: 1, ConsecutiveWins: [2]int32{3, 0}, ScoreStart: [2]float32{12500, 0}, PersistRoundCount: 6}
	c.apply()
	// runMatch records the round count before its first round counts.
	sessionRoundCount = sys.persistRoundCount
	if got := currentMatchContext(); *got != *c {
		t.Fatalf("context %+v, want %+v", *got, *c)
	}
	(&ReplayMatchContext{MatchNo: 2, Home: 7}).apply()
	if sys.home != 1 {
		t.Fatal("a home side no match has was applied")
	}
	p := &ReplayMatchParams{PersistLife: true, PersistRounds: true, TurnsOffset: [2]int32{2, 0}, Dialogue: false}
	p.apply()
	if got := currentMatchParams(); *got != *p {
		t.Fatalf("params %+v, want %+v", *got, *p)
	}
	(&ReplayMatchParams{TurnsOffset: [2]int32{-3, 1}, Dialogue: true}).apply()
	if sys.sel.gameParams.TurnsOffset != [2]int32{2, 1} {
		t.Fatalf("turns offset %v", sys.sel.gameParams.TurnsOffset)
	}
	var none *ReplayMatchParams
	none.apply()
	var noContext *ReplayMatchContext
	noContext.apply()
}

// A player's state at the start of a session is restored exactly, floats
// included; a table larger than a real one is left out.
func TestMatchStartStateRoundTrip(t *testing.T) {
	saved := sys.chars
	defer func() { sys.chars = saved }()
	sys.chars = [len(sys.chars)][]*Char{}
	c := &Char{playerNo: 2, controller: ^2, life: 640, lifeMax: 1000, power: 2500, powerMax: 3000, dizzyPoints: 5, dizzyPointsMax: 1000,
		guardPoints: 7, guardPointsMax: 1000, redLife: 700,
		cnsvar: map[int32]int32{1: 5, 59: -2}, cnsfvar: map[int32]float32{3: float32(math.Pi)},
		cnssysvar: map[int32]int32{0: 9}, cnssysfvar: map[int32]float32{1: -0.1},
		mapArray: map[string]float32{"combo": 1.25}}
	sys.chars[2] = []*Char{c}
	states := captureMatchStartState()
	if len(states) != 1 || states[0].Player != 3 {
		t.Fatalf("states %+v", states)
	}
	encoded, err := json.Marshal(states)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []ReplayPlayerState
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	// A player the CPU controls whose AI level was set to 0 after it was
	// loaded stays under the CPU's control.
	fresh := &Char{playerNo: 2, controller: 2, life: 1000, lifeMax: 1000, cnsvar: map[int32]int32{1: 0}}
	sys.chars[2] = []*Char{fresh}
	huge := make(map[int32]int32, matchReplayMaxVars+1)
	for i := int32(0); i <= matchReplayMaxVars; i++ {
		huge[i] = i
	}
	decoded = append(decoded, ReplayPlayerState{Player: 3, Life: 1, Vars: huge}, ReplayPlayerState{Player: 9})
	applyMatchStartState(decoded[:1])
	if fresh.controller != ^2 || fresh.life != 640 || fresh.power != 2500 || fresh.redLife != 700 || fresh.cnsvar[59] != -2 ||
		fresh.cnsfvar[3] != float32(math.Pi) || fresh.cnssysfvar[1] != -0.1 || fresh.mapArray["combo"] != 1.25 {
		t.Fatalf("restored %+v", fresh)
	}
	applyMatchStartState(decoded[1:])
	if fresh.life != 1 || len(fresh.cnsvar) != 2 {
		t.Fatalf("an oversized table was applied (%d vars)", len(fresh.cnsvar))
	}
}
