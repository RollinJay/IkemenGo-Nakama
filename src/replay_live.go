package main

import (
	"log"
	"time"
)

// Live replays: watching a lobby match from its replay stream (see
// replay_stream.go). A spectator loads the same fight as the players (the
// stream header's MatchInfo names it) and plays it with the players'
// resolved inputs instead of local controllers:
//
//   - The stream starts at the match's first frame, so the screens before
//     the fight consume no frames; Synchronize, at the start of runMatch,
//     starts consuming them, and applies the rules the header carries
//     (round time, rounds to win) over the ones the screens set, and the
//     input slot each player read.
//   - A Turns match runs one runMatch, and one rollback session and stream,
//     per character change. Each later Synchronize during the same game moves
//     to the stream the players published next.
//   - The next frame is read once a frame is drawn (update) in which the
//     match ran on the current one: a paused replay keeps its place. A replay
//     runs by replay timing (System.replayTiming): pausing it does not change
//     how it runs.
//   - Frames that are late are waited for (the window stays responsive), and
//     a spectator far behind plays at 4x speed until it has caught up.

const (
	// A spectator starts once this many frames are buffered (or the stream
	// has ended): a second of cushion against network jitter.
	liveReplayStartFrames = 60
	// Late frames (the players' connection stalled) and a Turns match's next
	// stream are waited for this long, plus the stream's delay, before
	// watching ends.
	liveReplayStallLimit = 20 * time.Second
	// A spectator further behind the newest frame received than
	// liveReplayCatchUpStart plays at 4x until liveReplayCatchUpStop frames
	// are left, so that watching late ends close to the match.
	liveReplayCatchUpStart = 240
	liveReplayCatchUpStop  = 90
)

// liveReplayReady reports whether a live stream can start playing.
func liveReplayReady(buffer *NakamaReplayBuffer) bool {
	header := buffer.Header()
	if header == nil || buffer.ResetReason() != "" {
		return false
	}
	buffered := buffer.BufferedThrough()
	if finalFrame, ended := buffer.FinalFrame(); ended && buffered >= finalFrame {
		return true
	}
	need := int32(liveReplayStartFrames)
	if header.DelayFrames < need {
		need = header.DelayFrames
	}
	return buffered >= need
}

// playingMatchReplay reports whether a match replay plays (a match replay
// file or a watched stream), whose inputs are only its players': a session
// replay replays its menus too.
func (s *System) playingMatchReplay() bool {
	return s.replayFile != nil && s.replayFile.liveBuffer != nil
}

// isClosed reports whether the replay was closed (its file, or the live
// stream after a failure).
func (rf *ReplayFile) isClosed() bool {
	return rf == nil || (rf.file == nil && rf.liveBuffer == nil)
}

// exhausted reports whether the current match has no more input: the replay
// was closed or the live stream of this runMatch has ended.
func (rf *ReplayFile) exhausted() bool {
	return rf.isClosed() || rf.liveEnded
}

// liveCatchingUp reports whether a live replay plays fast to catch up. A
// match replay file has nothing to catch up with.
func (rf *ReplayFile) liveCatchingUp() bool {
	if rf == nil || rf.liveBuffer == nil || !rf.liveStarted || rf.liveEnded || rf.fromFile {
		return false
	}
	// How far behind the players the spectator is: the newest frame received,
	// or, once the stream has ended, the frame that was the newest while it
	// ran. The end sends the players' last seconds (the delay) at once; they
	// are not a backlog, and play at normal speed.
	edge := rf.liveBuffer.BufferedThrough()
	if finalFrame, ended := rf.liveBuffer.FinalFrame(); ended {
		edge = finalFrame - rf.liveDelay
	}
	behind := edge - rf.liveFrame
	if rf.liveCatchUp {
		rf.liveCatchUp = behind > liveReplayCatchUpStop
	} else {
		rf.liveCatchUp = behind > liveReplayCatchUpStart
	}
	return rf.liveCatchUp
}

// liveStallLimit is how long missing frames are waited for: a stream's
// first frames arrive the delay after its header.
func (rf *ReplayFile) liveStallLimit() time.Duration {
	return liveReplayStallLimit + time.Duration(rf.liveDelay)*time.Second/replayStreamFPS
}

// liveFail ends watching: Esc for the screens, and the replay closes.
func (rf *ReplayFile) liveFail(format string, args ...any) {
	log.Printf(format, args...)
	sys.esc = true
	rf.Close()
}

func (rf *ReplayFile) liveSynchronize() {
	if rf.liveStarted {
		// The game's closing synchronize, after the match: nothing follows.
		if !sys.gameRunning {
			return
		}
		// A Turns character change: the players' next rollback session
		// published a stream of its own.
		next := rf.waitNextLiveSegment()
		if next == nil {
			rf.liveFail("Live replay: the match's next stream did not arrive")
			return
		}
		rf.liveBuffer = next
		rf.liveFrame = 0
		rf.liveEnded = false
		rf.liveCatchUp = false
	}
	header := rf.liveBuffer.Header()
	if header == nil {
		rf.liveFail("Live replay synchronization failed: missing replay header")
		return
	}
	Srand(header.Seed)
	rf.liveDelay = header.DelayFrames
	rf.preMatchTime = header.PreMatchTime
	sys.preMatchTime = header.PreMatchTime
	// runMatch has just set matchTime to 0; the players' rollback match
	// started from the netplay session's frame count.
	sys.matchTime = header.MatchTime
	// The rules the players' match ran with, before the round starts, its
	// context (match number, home side, consecutive wins, score), its game
	// parameters and the input slot each player read (a stream from a
	// publisher that predates them keeps the local ones). The players' own
	// state is applied once the round is set up (applyStartState).
	header.Rules.apply()
	header.Context.apply()
	header.Params.apply()
	applyReplayInputRemap(header.InputRemap)
	// A frame of inputs is a frame of the match at its speed: the debug speed
	// keys do not apply (they are off during replays).
	sys.debugAccel = 1
	rf.frameUsed = false
	rf.local = header.Local
	rf.liveStarted = true
	rf.liveUpdate()
	log.Printf("Live replay synchronized: stream=%s seed=%d pmTime=%d matchTime=%d delay=%d",
		header.Stream, header.Seed, header.PreMatchTime, header.MatchTime, header.DelayFrames)
}

// waitNextLiveSegment waits for the stream that follows the current one in
// the same match (the same MatchInfo, the next Segment), keeping the window
// responsive. A match replay file has it already.
func (rf *ReplayFile) waitNextLiveSegment() *NakamaReplayBuffer {
	if rf.fromFile {
		if len(rf.fileSegments) == 0 {
			return nil
		}
		next := rf.fileSegments[0]
		rf.fileSegments = rf.fileSegments[1:]
		return next
	}
	if sys.nakama == nil {
		return nil
	}
	deadline := time.Now().Add(rf.liveStallLimit())
	for time.Now().Before(deadline) {
		if next := sys.nakama.NextReplayBuffer(rf.liveBuffer); next != nil {
			return next
		}
		time.Sleep(50 * time.Millisecond)
		if !sys.eventUpdate() || sys.esc {
			return nil
		}
	}
	return nil
}

func (rf *ReplayFile) liveUpdate() bool {
	if !rf.liveStarted || rf.liveEnded {
		return !sys.gameEnd
	}
	deadline := time.Now().Add(rf.liveStallLimit())
	for !rf.liveBuffer.WaitForFrame(rf.liveFrame, 100*time.Millisecond) {
		if finalFrame, ended := rf.liveBuffer.FinalFrame(); ended && rf.liveFrame >= finalFrame {
			// The runMatch loop stops here (exhausted); a Turns match
			// continues with the next stream at the next Synchronize.
			rf.liveEnded = true
			return !sys.gameEnd
		}
		if reason := rf.liveBuffer.ResetReason(); reason != "" {
			rf.liveFail("Live replay withdrawn at frame %d: %s", rf.liveFrame, reason)
			return !sys.gameEnd
		}
		if time.Now().After(deadline) {
			rf.liveFail("Live replay stalled waiting for input frame %d", rf.liveFrame)
			return !sys.gameEnd
		}
		if !sys.eventUpdate() || sys.esc {
			rf.liveFail("Live replay stopped at frame %d", rf.liveFrame)
			return !sys.gameEnd
		}
	}
	frame, ok := rf.liveBuffer.Frame(rf.liveFrame)
	if !ok {
		rf.liveFail("Live replay lost input frame %d", rf.liveFrame)
		return !sys.gameEnd
	}
	rf.ibit = frame.Inputs
	rf.iaxes = frame.Axes
	rf.liveFrame++
	return !sys.gameEnd
}
