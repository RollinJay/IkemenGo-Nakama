package main

import (
	"encoding/json"
	"log"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// Local match replays. An offline match (local versus, or a match against
// the CPU in a mode that asks for it) is saved as a match replay like an
// online one (replay_match.go), and plays back the same way. A replay reads a
// frame of inputs per frame drawn (System.update) in which the match ran; the
// recorder keeps its frames at the same points.
//
// The scripts arm the recorder for the next game() (replayRecordLocal). The
// engine then, for each runMatch (a Turns match has one per character
// change):
//
//   - seeds the random numbers at synchronize, as netplay does, and records
//     the seed;
//   - once the round is set up, starts a session with the sync settings, the
//     rules, the game parameters, the input slots, the AI levels, the match
//     context and the players' state (recordMatchStartState);
//   - makes the match read its inputs as the replay will: each local input
//     slot is read once per frame, when the match first reads it, and
//     everything that reads an input slot in that frame (characters sharing
//     a controller, the intro and win pose skips, the fight's dialogues) gets
//     that reading. Button assist is applied per input slot, as online;
//   - keeps that frame once it is drawn, unless the match did not run in it
//     (it was paused);
//   - saves the session when runMatch returns, and stops when game() returns.
//
// A recorded match runs at 60 frames per second, as online matches and their
// replays do (the Framerate setting does not apply to it), so that its frames
// are the ones the replay plays. Like a replay, it runs by replay timing
// (System.replayTiming): pausing it, with the pause menu, the Pause key or a
// frame step, does not change how it runs, at any game speed.
//
// What changes the match from outside it cannot be replayed: a debug key or
// debug console command, loading a saved state, resetting the round or
// restarting the match from a menu, a change to the game speed, to the input
// slots or to a sync setting. Neither can a character resetting the round or
// restarting the match (the MatchRestart controller does not act in a
// replay), or loading a state saved from outside the match, nor a script
// pausing the match to wait for its players. The recording then stops: the
// frames before it are saved, and the rest of the game() is not recorded.

type localReplayRecorder struct {
	rec     matchReplayRecorder
	armed   bool // the current game() is recorded
	pending bool // runMatch synchronized; the session begins once set up
	open    bool // a session is being recorded
	// sessions is how many sessions of the current game() began.
	sessions int32
	// fingerprint is the content fingerprint, taken when the game() was
	// armed; header, the sync header, when its first session began.
	fingerprint string
	header      *ReplayHeader
	// The session's start, taken at synchronize, and what must not change
	// while it is recorded.
	seed, preMatchTime, matchTime int32
	logicSpeed                    int32
	remap                         []int
	// The session's frames.
	inputs [][REPLAY_NUM_INPUTS]InputBits
	axes   [][REPLAY_NUM_INPUTS][6]int8
	// The frame the match is running: live when the current loop pass runs
	// the match, used when a pass ran it since the last frame was kept,
	// sampled once its inputs were read. recheck: the settings are compared
	// before the match runs again (it was paused, or a hotkey ran).
	live, used, sampled, recheck bool
	frame                        [REPLAY_NUM_INPUTS]InputBits
	frameAxes                    [REPLAY_NUM_INPUTS][6]int8
	readers                      [REPLAY_NUM_INPUTS]InputReader
	// foreignState: the saved state (the SaveState and LoadState controllers,
	// the F9 and F10 hotkeys) was saved from outside the match.
	foreignState bool
}

var localReplay localReplayRecorder

// arm records the next game() to dir, described by info.
func (r *localReplayRecorder) arm(dir string, info json.RawMessage) error {
	if err := r.rec.start(dir); err != nil {
		return err
	}
	r.rec.setInfo(info)
	r.armed = true
	r.pending, r.open = false, false
	r.sessions = 0
	r.header = nil
	r.fingerprint = sys.currentContentFingerprint()
	return nil
}

// synchronize seeds a recorded match's random numbers at the start of
// runMatch, as netplay's synchronize does; playing the replay applies the
// same seed at the same point.
func (r *localReplayRecorder) synchronize() {
	if !r.armed || !sys.gameRunning || r.open {
		return
	}
	seed := int32(time.Now().UnixNano() & 0x7fffffff)
	if seed == 0 {
		seed = 1
	}
	Srand(seed)
	r.seed, r.preMatchTime, r.matchTime = seed, sys.preMatchTime, sys.matchTime
	r.pending = true
}

// begin starts the session once the round is set up; start is the players'
// state then.
func (r *localReplayRecorder) begin(start []ReplayPlayerState) {
	if !r.pending {
		return
	}
	r.pending = false
	if r.header == nil {
		r.header = sys.localReplayHeader(r.fingerprint)
		if r.header == nil {
			r.armed = false
			return
		}
	}
	// A recorded match runs at the game's speed: a speed set with the debug
	// speed keys is reset (and a debug key stops the recording).
	sys.debugAccel = 1
	stage := ""
	if sys.stage != nil {
		stage = sys.stage.def
	}
	r.sessions++
	// The first session begins the match; the others continue it (a Turns
	// character change, or a restart by a character).
	r.rec.beginSession(ReplayStreamStart{
		Header:       r.header,
		Seed:         r.seed,
		PreMatchTime: r.preMatchTime,
		MatchTime:    r.matchTime,
		Stage:        stage,
		Rules:        currentMatchRules(),
		InputRemap:   currentInputRemap(),
		AILevels:     currentAILevels(),
		Context:      currentMatchContext(),
		StartState:   start,
		Params:       currentMatchParams(),
		Local:        true,
	}, r.sessions)
	r.logicSpeed = sys.gameLogicSpeed()
	r.remap = currentInputRemap()
	r.inputs, r.axes = r.inputs[:0], r.axes[:0]
	r.live, r.used, r.sampled, r.recheck = false, false, false, false
	r.readers = [REPLAY_NUM_INPUTS]InputReader{}
	r.foreignState = false
	r.open = true
}

// pass starts a pass of the runMatch loop: live when the match runs in it
// (it is not paused). What must not change while the match is recorded is
// checked before the match runs again.
func (r *localReplayRecorder) pass(live bool) {
	r.live = live
	if !r.open {
		return
	}
	if !live {
		r.recheck = true
		return
	}
	switch {
	case sys.gameLogicSpeed() != r.logicSpeed:
		r.interrupt("the game speed changed")
		return
	case !r.sameInputRemap():
		r.interrupt("the input slots changed")
		return
	case r.recheck && !r.sameSettings():
		r.interrupt("a setting changed")
		return
	}
	r.recheck = false
	r.used = true
}

// ranOutside notes that code outside the match ran (a hotkey): the settings
// are compared before the match runs again.
func (r *localReplayRecorder) ranOutside() {
	if r.open {
		r.recheck = true
	}
}

// sameSettings reports whether the sync settings are still the session's.
func (r *localReplayRecorder) sameSettings() bool {
	strict, err := collectSyncSettings(&sys.cfg, syncStrict)
	if err != nil {
		return false
	}
	host, err := collectSyncSettings(&sys.cfg, syncHost)
	if err != nil {
		return false
	}
	return equalSyncSettings(strict, r.header.Strict) && equalSyncSettings(host, r.header.Host)
}

// sample reads every local input slot for the frame, once.
func (r *localReplayRecorder) sample() {
	if r.sampled {
		return
	}
	for in := range r.frame {
		var ib InputBits
		ib.KeysToBits(r.readers[in].LocalInput(in))
		r.frame[in] = ib
		r.frameAxes[in] = localAxes(in)
	}
	r.sampled = true
}

// input is what local input slot in reads in the current frame of a
// recorded match; ok is false when the match is not recorded or not running
// (the pause menu reads the controllers directly).
func (r *localReplayRecorder) input(in int) (buttons [14]bool, axes [6]int8, ok bool) {
	if !r.open || !r.live {
		return buttons, axes, false
	}
	if in < 0 || in >= REPLAY_NUM_INPUTS {
		return buttons, axes, true
	}
	r.sample()
	return r.frame[in].BitsToKeys(), r.frameAxes[in], true
}

// anyButton is whether a button is held on any input slot in the current
// frame of a recorded match, as its replay tells (ok false otherwise).
func (r *localReplayRecorder) anyButton() (pressed, ok bool) {
	if !r.open || !r.live {
		return false, false
	}
	r.sample()
	for _, b := range r.frame {
		if b&IB_anybutton != 0 {
			return true, true
		}
	}
	return false, true
}

// advance moves to the next frame after a frame drawn, where the replay reads
// its next one: the current frame is kept when the match ran on it. A paused
// pass draws a frame in the middle of one when the game runs faster than
// normal (the frame's passes resume after the pause).
func (r *localReplayRecorder) advance() {
	if !r.open || !r.used || !r.live {
		return
	}
	if !r.room() {
		r.used = false
		r.interrupt("the match is longer than the most a replay holds")
		return
	}
	r.keep()
}

// room reports whether a replay holds another frame of the match.
func (r *localReplayRecorder) room() bool {
	return r.rec.savedFrames()+len(r.inputs) < matchReplayMaxFrames
}

func (r *localReplayRecorder) keep() {
	if !r.sampled {
		// Nothing read the inputs: the replay will not either.
		r.frame = [REPLAY_NUM_INPUTS]InputBits{}
		r.frameAxes = [REPLAY_NUM_INPUTS][6]int8{}
	}
	r.inputs = append(r.inputs, r.frame)
	r.axes = append(r.axes, r.frameAxes)
	r.used, r.sampled = false, false
}

// end saves the session: runMatch returned.
func (r *localReplayRecorder) end() {
	r.pending = false
	if !r.open {
		return
	}
	// The frame the match ran on last, unless the loop ended on a paused pass
	// before the frame was drawn (its replay would run the rest of it).
	if r.used && r.live && r.room() {
		r.keep()
	}
	r.open, r.live = false, false
	rs := &RollbackSession{replayInputs: r.inputs, replayAnalogInputs: r.axes}
	r.rec.endSession(rs, len(r.inputs))
}

// interrupt stops recording: something outside the match changed it. The
// frames the match ran before are saved.
func (r *localReplayRecorder) interrupt(reason string) {
	if !r.open {
		return
	}
	log.Printf("Match replay: recording stopped after %d frames: %s", len(r.inputs)+btoiInt(r.used && r.live && r.room()), reason)
	r.end()
	r.armed = false
}

// finish stops recording: game() returned.
func (r *localReplayRecorder) finish() {
	r.end()
	r.armed = false
	r.rec.stop()
	r.inputs, r.axes = nil, nil
	r.header = nil
}

// matchFlags are the requests the runMatch loop carries out after a pass:
// saving or loading a state, resetting the round, restarting the match.
type matchFlags struct{ save, load, roundReset, reload bool }

// watch returns the flags; check (code run outside the match) and checkMatch
// (the match itself) compare them afterwards.
func (r *localReplayRecorder) watch() matchFlags {
	return matchFlags{sys.saveStateFlag, sys.loadStateFlag, sys.roundResetFlg, sys.reloadFlg}
}

// check stops recording when code run outside the match since watch (hotkeys,
// menus, the debug console) loaded a saved state, reset the round or
// restarted the match. A state it saved is not in the replay.
func (r *localReplayRecorder) check(before matchFlags) {
	if !r.open {
		return
	}
	now := r.watch()
	switch {
	case now.load && !before.load:
		r.interrupt("a saved state was loaded")
	case now.roundReset && !before.roundReset:
		r.interrupt("the round was reset")
	case now.reload && !before.reload:
		r.interrupt("the match was restarted")
	case now.save && !before.save:
		r.foreignState = true
	}
}

// checkMatch stops recording when a character reset the round or restarted
// the match since watch (the MatchRestart controller does not in a replay), or
// loaded a state saved from outside the match. A state saved by a character
// replaces that one.
func (r *localReplayRecorder) checkMatch(before matchFlags) {
	if !r.open {
		return
	}
	now := r.watch()
	switch {
	case now.roundReset && !before.roundReset:
		r.interrupt("a character reset the round")
	case now.reload && !before.reload:
		r.interrupt("a character restarted the match")
	case now.save && !before.save:
		// The loop saves, and ignores a load in the same pass.
		r.foreignState = false
	case now.load && !before.load && r.foreignState:
		r.interrupt("a character loaded a state saved outside the match")
	}
}

// localAxes are the analog axes of local input slot in, as the match reads
// them (zero without a joystick).
func localAxes(in int) [6]int8 {
	if in < 0 || in >= len(sys.joystickConfig) {
		return [6]int8{}
	}
	joy := sys.joystickConfig[in].Joy
	if joy < 0 || joy >= input.GetMaxJoystickCount() || !input.IsJoystickPresent(joy) ||
		joy >= len(input.controllerstate) || input.controllerstate[joy] == nil {
		return [6]int8{}
	}
	return input.controllerstate[joy].Axes
}

// sameInputRemap reports whether the players still read the input slots
// they read when the session began.
func (r *localReplayRecorder) sameInputRemap() bool {
	for i, slot := range r.remap {
		if i >= len(sys.inputRemap) || sys.inputRemap[i] != slot {
			return false
		}
	}
	return true
}

func equalSyncSettings(a, b []SyncSetting) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func btoiInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// localReplayHeader is the sync header of a local match: this game's
// settings, which a replay applies while it plays, and its content
// fingerprint.
func (s *System) localReplayHeader(fingerprint string) *ReplayHeader {
	strict, err := collectSyncSettings(&s.cfg, syncStrict)
	if err != nil {
		log.Printf("Match replay: %v", err)
		return nil
	}
	host, err := collectSyncSettings(&s.cfg, syncHost)
	if err != nil {
		log.Printf("Match replay: %v", err)
		return nil
	}
	return &ReplayHeader{
		FormatVersion:      replayFormatVersion,
		SyncVersion:        syncConfigVersion,
		Strict:             strict,
		Host:               host,
		ContentFingerprint: fingerprint,
	}
}

func localReplayScriptInit(l *lua.LState) {
	luaRegister(l, "replayRecordLocal", func(l *lua.LState) int {
		/*Save the next offline game (the fight `game()` runs next, every
		round and Turns character of it) as a match replay.
		@function replayRecordLocal
		@tparam string dir The directory for the file.
		@tparam table info The match's description, as for `replaySetMatchInfo`.*/
		var info json.RawMessage
		if !nilArg(l, 2) {
			data, err := json.Marshal(luaRematchValueToAny(l.Get(2)))
			if err != nil {
				l.RaiseError("encode match info: %v", err)
			}
			info = data
		}
		if len(info) == 0 {
			l.RaiseError("a local match replay needs a description")
		}
		if sys.netConnection != nil || sys.rollback.session != nil || sys.replayFile != nil {
			l.RaiseError("cannot record a local match during an online session or a replay")
		}
		if err := localReplay.arm(strArg(l, 1), info); err != nil {
			l.RaiseError("cannot save replays to %s: %v", strArg(l, 1), err)
		}
		return 0
	})
	luaRegister(l, "replayLastMatchFile", func(l *lua.LState) int {
		/*The file of the last match replay saved, online or local.
		@function replayLastMatchFile
		@treturn string path `""` when none was saved.*/
		l.Push(lua.LString(lastMatchReplayFile))
		return 1
	})
}
