package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

const (
	replayStreamVersion      uint16 = 1
	replayStreamFPS                 = 60
	replayStreamDelaySeconds        = 10
	replayStreamChunkFrames         = 30
)

// ReplayStreamHeader contains everything a remote deterministic spectator needs
// to reconstruct an ongoing rollback match from its input stream.
type ReplayStreamHeader struct {
	Version      uint16        `json:"version"`
	FrameRate    int           `json:"frame_rate"`
	InputCount   int           `json:"input_count"`
	InputBytes   int           `json:"input_bytes"`
	DelayFrames  int32         `json:"delay_frames"`
	Seed         int32         `json:"seed"`
	PreMatchTime int32         `json:"pre_match_time"`
	ReplayHeader *ReplayHeader `json:"replay_header,omitempty"`
	// Stream identifies the stream; its chunks and end carry the same id, so
	// a buffer never mixes the inputs of two matches. Empty from publishers
	// that predate it.
	Stream string `json:"stream,omitempty"`
	// MatchTime is sys.matchTime on the match's first frame. A rollback match
	// starts at the netplay session's frame count, and the FightTime and
	// GameTime triggers read it, so a spectator starts from the same value.
	MatchTime int32 `json:"match_time,omitempty"`
	// Stage is the definition file of the stage the match is played on (a
	// random stage choice resolved).
	Stage string `json:"stage,omitempty"`
	// MatchInfo is what the publishing game tells spectators about the match
	// (nakama.setReplayInfo). The lobby screens send the pairing's seq and the
	// resolved selection: characters, palettes and team modes.
	MatchInfo json.RawMessage `json:"match_info,omitempty"`
	// Segment numbers the streams of one match: a Turns match runs one
	// rollback session, and publishes one stream, per character change. The
	// first stream is 0; the streams of a match share its MatchInfo.
	Segment int32 `json:"segment,omitempty"`
	// Rules are the round time, timer speed, rounds to win and draws the
	// match ran with. Empty from publishers that predate them.
	Rules *ReplayMatchRules `json:"rules,omitempty"`
	// InputRemap is the input slot each of the first InputCount players
	// read (sys.inputRemap): a ranked set that switches sides swaps them
	// between matches. AILevels is each player's AI level (0 for a player,
	// above 0 for the CPU, such as a Simul partner). Empty from publishers
	// that predate them.
	InputRemap []int     `json:"input_remap,omitempty"`
	AILevels   []float32 `json:"ai_levels,omitempty"`
	// Context is what the screens set around the match that its characters
	// can read: the match number, the home side, consecutive wins and the
	// score before the match. Empty from publishers that predate it.
	Context *ReplayMatchContext `json:"context,omitempty"`
	// StartState is each player's state when the session started (match
	// replay files only; see replay_match.go).
	StartState []ReplayPlayerState `json:"start_state,omitempty"`
	// Params are the game parameters the fight was loaded with that its
	// simulation depends on. Empty from recorders that predate them.
	Params *ReplayMatchParams `json:"params,omitempty"`
	// Local is set for a match played offline (replay_local.go), which had no
	// netplay host (the IsHost trigger).
	Local bool `json:"local,omitempty"`
}

// ReplayStreamStart describes the match a stream starts with.
type ReplayStreamStart struct {
	Header       *ReplayHeader
	Seed         int32
	PreMatchTime int32
	// MatchTime is sys.matchTime on the match's first frame.
	MatchTime  int32
	Stage      string
	Info       json.RawMessage
	Segment    int32
	Rules      *ReplayMatchRules
	InputRemap []int
	AILevels   []float32
	Context    *ReplayMatchContext
	StartState []ReplayPlayerState
	Params     *ReplayMatchParams
	Local      bool
}

// ReplayMatchParams are the game parameters the screens load a fight with
// (loadStart) that its simulation depends on: whether life, rounds and music
// carry over from the run's previous match (characters can read these), the
// first Turns member each side fights with (a survival run skips members
// already defeated), and whether characters' dialogues play.
type ReplayMatchParams struct {
	PersistLife   bool     `json:"persist_life"`
	PersistRounds bool     `json:"persist_rounds"`
	PersistMusic  bool     `json:"persist_music"`
	TurnsOffset   [2]int32 `json:"turns_offset"`
	Dialogue      bool     `json:"dialogue"`
}

// currentMatchParams returns the parameters of the fight about to start.
func currentMatchParams() *ReplayMatchParams {
	p := &ReplayMatchParams{Dialogue: sys.motif.di.enabled}
	if gp := sys.sel.gameParams; gp != nil {
		p.PersistLife, p.PersistRounds, p.PersistMusic = gp.PersistLife, gp.PersistRounds, gp.PersistMusic
		p.TurnsOffset = gp.TurnsOffset
	}
	return p
}

// apply sets the parameters of the fight about to start; nil leaves them.
func (p *ReplayMatchParams) apply() {
	if p == nil {
		return
	}
	if gp := sys.sel.gameParams; gp != nil {
		gp.PersistLife, gp.PersistRounds, gp.PersistMusic = p.PersistLife, p.PersistRounds, p.PersistMusic
		for side, off := range p.TurnsOffset {
			if off >= 0 {
				gp.TurnsOffset[side] = off
			}
		}
	}
	sys.motif.di.enabled = p.Dialogue
}

// ReplayMatchContext is what the screens set around a match that its
// characters can read through triggers: the match number (MatchNo), the home
// side (IsHomeTeam), each side's consecutive wins (ConsecutiveWins) and its
// score before the match (ScoreTotal). An arcade or survival run carries them
// from match to match, and the rounds counted in a run whose rounds persist
// (the round the fight screen calls).
type ReplayMatchContext struct {
	MatchNo           int32      `json:"match_no"`
	Home              int        `json:"home"`
	ConsecutiveWins   [2]int32   `json:"consecutive_wins"`
	ScoreStart        [2]float32 `json:"score_start"`
	PersistRoundCount int32      `json:"persist_round_count,omitempty"`
}

// sessionRoundCount is sys.persistRoundCount before the current runMatch set
// up its first round, which counts it.
var sessionRoundCount int32

// currentMatchContext returns the context of the match about to start.
func currentMatchContext() *ReplayMatchContext {
	c := &ReplayMatchContext{
		MatchNo:           sys.matchNo,
		Home:              sys.home,
		ConsecutiveWins:   sys.consecutiveWins,
		ScoreStart:        sys.scoreStart,
		PersistRoundCount: sessionRoundCount,
	}
	for i, v := range c.ScoreStart {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			c.ScoreStart[i] = 0
		}
	}
	return c
}

// apply sets the context of the match about to start; nil leaves it.
func (c *ReplayMatchContext) apply() {
	if c == nil {
		return
	}
	sys.matchNo = c.MatchNo
	if c.Home == 0 || c.Home == 1 {
		sys.home = c.Home
	}
	sys.consecutiveWins = c.ConsecutiveWins
	sys.scoreStart = c.ScoreStart
	sys.persistRoundCount = c.PersistRoundCount
}

// currentInputRemap returns the input slot each player who reads a replay
// slot reads now.
func currentInputRemap() []int {
	n := REPLAY_NUM_INPUTS
	if n > len(sys.inputRemap) {
		n = len(sys.inputRemap)
	}
	return append([]int(nil), sys.inputRemap[:n]...)
}

// currentAILevels returns each player's AI level now.
func currentAILevels() []float32 {
	return append([]float32(nil), sys.aiLevel[:]...)
}

// applyReplayInputRemap makes each player read the input slot it read in
// the recorded match. A slot outside the replay's (a replay file can come
// from anyone) leaves that player's as it is.
func applyReplayInputRemap(remap []int) {
	for i, slot := range remap {
		if i >= len(sys.inputRemap) || i >= REPLAY_NUM_INPUTS {
			break
		}
		if slot >= 0 && slot < REPLAY_NUM_INPUTS {
			sys.inputRemap[i] = slot
		}
	}
}

// ReplayMatchRules are the rules a match ran with, as the engine held them
// on its first frame: the round time and the ticks per timer count, and the
// rounds each side needed to win and the draws it could have. The screens
// that set a fight up derive them from the options, select.def parameters,
// command-line flags or lobby settings; a spectator or a replay applies them
// as recorded, so that its fight runs to the same rules whatever those say
// where it plays.
type ReplayMatchRules struct {
	RoundTime      int32    `json:"round_time"`
	FramesPerCount int32    `json:"frames_per_count"`
	MatchWins      [2]int32 `json:"match_wins"`
	MaxDraws       [2]int32 `json:"max_draws"`
}

// currentMatchRules returns the rules of the match about to start.
func currentMatchRules() *ReplayMatchRules {
	return &ReplayMatchRules{
		RoundTime:      sys.maxRoundTime,
		FramesPerCount: sys.curFramesPerCount,
		MatchWins:      sys.matchWins,
		MaxDraws:       sys.maxDraws,
	}
}

// apply sets the rules of the match about to start. A value no match could
// have (a replay file can come from anyone) leaves the rule as it is.
func (r *ReplayMatchRules) apply() {
	if r == nil {
		return
	}
	if r.RoundTime >= -1 {
		sys.maxRoundTime = r.RoundTime
	}
	if r.FramesPerCount > 0 {
		sys.curFramesPerCount = r.FramesPerCount
	}
	for side := range r.MatchWins {
		if r.MatchWins[side] >= 1 {
			sys.matchWins[side] = r.MatchWins[side]
		}
		if r.MaxDraws[side] >= -1 {
			sys.maxDraws[side] = r.MaxDraws[side]
		}
	}
}

// ReplayStreamChunk is a contiguous range of resolved rollback inputs. Payload
// uses the same per-controller encoding as the on-disk replay format.
type ReplayStreamChunk struct {
	Sequence   uint64 `json:"sequence"`
	StartFrame int32  `json:"start_frame"`
	FrameCount int32  `json:"frame_count"`
	Payload    []byte `json:"payload"`
	Stream     string `json:"stream,omitempty"`
}

// ReplayStreamEnd marks the final authoritative input frame of a live stream.
type ReplayStreamEnd struct {
	FinalFrame int32  `json:"final_frame"`
	Stream     string `json:"stream,omitempty"`
}

// ReplayStreamReset withdraws a stream: a rollback went past frames that were
// already published, or the publisher could not encode them.
type ReplayStreamReset struct {
	Reason string `json:"reason"`
	Stream string `json:"stream,omitempty"`
}

func newReplayStreamID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ReplayInputFrame is the decoded deterministic input state for one simulation
// frame. The shape mirrors RollbackSession's in-memory replay arrays.
type ReplayInputFrame struct {
	Inputs [REPLAY_NUM_INPUTS]InputBits
	Axes   [REPLAY_NUM_INPUTS][6]int8
}

// NakamaReplayBuffer retains the resolved input history delivered through a
// Nakama lobby. A spectator that has been present since the replay header was
// published can start from frame zero and remain behind the live match without
// requiring game-state snapshots.
type NakamaReplayBuffer struct {
	mu          sync.RWMutex
	header      *ReplayStreamHeader
	frames      map[int32]ReplayInputFrame
	nextFrame   int32
	finalFrame  int32
	ended       bool
	resetReason string
	notify      chan struct{}
}

func NewNakamaReplayBuffer() *NakamaReplayBuffer {
	return &NakamaReplayBuffer{frames: make(map[int32]ReplayInputFrame), notify: make(chan struct{})}
}

func (b *NakamaReplayBuffer) notifyLocked() {
	old := b.notify
	b.notify = make(chan struct{})
	close(old)
}

func (b *NakamaReplayBuffer) Reset(reason string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.header = nil
	b.frames = make(map[int32]ReplayInputFrame)
	b.nextFrame = 0
	b.finalFrame = 0
	b.ended = false
	b.resetReason = reason
	b.notifyLocked()
	b.mu.Unlock()
}

// ApplyReset empties the buffer when reset withdraws its stream. The header
// stays, so the withdrawn stream is still known (and its data is refused).
func (b *NakamaReplayBuffer) ApplyReset(reset ReplayStreamReset) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sameStreamLocked(reset.Stream) {
		return
	}
	b.frames = make(map[int32]ReplayInputFrame)
	b.nextFrame = 0
	b.finalFrame = 0
	b.ended = false
	b.resetReason = reset.Reason
	if b.resetReason == "" {
		b.resetReason = "reset"
	}
	b.notifyLocked()
}

func (b *NakamaReplayBuffer) ApplyHeader(header ReplayStreamHeader) {
	if b == nil {
		return
	}
	b.mu.Lock()
	copyHeader := header
	if header.ReplayHeader != nil {
		copyReplayHeader := *header.ReplayHeader
		copyReplayHeader.Strict = cloneSyncSettings(header.ReplayHeader.Strict)
		copyReplayHeader.Host = cloneSyncSettings(header.ReplayHeader.Host)
		copyHeader.ReplayHeader = &copyReplayHeader
	}
	copyHeader.MatchInfo = append(json.RawMessage(nil), header.MatchInfo...)
	b.header = &copyHeader
	b.frames = make(map[int32]ReplayInputFrame)
	b.nextFrame = 0
	b.finalFrame = 0
	b.ended = false
	b.resetReason = ""
	b.notifyLocked()
	b.mu.Unlock()
}

func (b *NakamaReplayBuffer) ApplyChunk(chunk ReplayStreamChunk) error {
	if b == nil {
		return errors.New("nil replay buffer")
	}
	if chunk.StartFrame < 0 || chunk.FrameCount < 0 {
		return errors.New("invalid replay chunk frame range")
	}
	expected := int(chunk.FrameCount) * REPLAY_NUM_INPUTS * REPLAY_INPUT_BYTES
	if len(chunk.Payload) != expected {
		return fmt.Errorf("invalid replay chunk payload size: got %d want %d", len(chunk.Payload), expected)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.sameStreamLocked(chunk.Stream) || b.resetReason != "" {
		return nil
	}
	for frame := 0; frame < int(chunk.FrameCount); frame++ {
		var decoded ReplayInputFrame
		off := frame * REPLAY_NUM_INPUTS * REPLAY_INPUT_BYTES
		for controller := 0; controller < REPLAY_NUM_INPUTS; controller++ {
			cell := chunk.Payload[off : off+REPLAY_INPUT_BYTES]
			decoded.Inputs[controller] = InputBits(int16(binary.LittleEndian.Uint16(cell[:2])))
			for axis := 0; axis < 6; axis++ {
				decoded.Axes[controller][axis] = int8(cell[2+axis])
			}
			off += REPLAY_INPUT_BYTES
		}
		b.frames[chunk.StartFrame+int32(frame)] = decoded
	}
	b.advanceContiguousLocked()
	b.notifyLocked()
	return nil
}

func (b *NakamaReplayBuffer) ApplyEnd(end ReplayStreamEnd) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if !b.sameStreamLocked(end.Stream) || b.resetReason != "" {
		b.mu.Unlock()
		return
	}
	b.finalFrame = end.FinalFrame
	b.ended = true
	b.advanceContiguousLocked()
	b.notifyLocked()
	b.mu.Unlock()
}

// sameStreamLocked reports whether data of stream belongs in this buffer.
// Data without a stream id, or a buffer whose header has none, is accepted
// as before stream ids existed.
func (b *NakamaReplayBuffer) sameStreamLocked(stream string) bool {
	return stream == "" || b.header == nil || b.header.Stream == "" || b.header.Stream == stream
}

// Stream returns the id of the buffered stream ("" before its header).
func (b *NakamaReplayBuffer) Stream() string {
	if b == nil {
		return ""
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.header == nil {
		return ""
	}
	return b.header.Stream
}

func (b *NakamaReplayBuffer) advanceContiguousLocked() {
	for {
		if _, ok := b.frames[b.nextFrame]; !ok {
			return
		}
		b.nextFrame++
	}
}

func (b *NakamaReplayBuffer) Header() *ReplayStreamHeader {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.header == nil {
		return nil
	}
	out := *b.header
	return &out
}

func (b *NakamaReplayBuffer) BufferedThrough() int32 {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.nextFrame
}

func (b *NakamaReplayBuffer) FinalFrame() (int32, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.finalFrame, b.ended
}

func (b *NakamaReplayBuffer) ResetReason() string {
	if b == nil {
		return ""
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.resetReason
}

func (b *NakamaReplayBuffer) Frame(frame int32) (ReplayInputFrame, bool) {
	if b == nil {
		return ReplayInputFrame{}, false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	value, ok := b.frames[frame]
	return value, ok
}

func (b *NakamaReplayBuffer) WaitForFrame(frame int32, timeout time.Duration) bool {
	if b == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		b.mu.RLock()
		if _, ok := b.frames[frame]; ok {
			b.mu.RUnlock()
			return true
		}
		if b.ended && frame >= b.finalFrame {
			b.mu.RUnlock()
			return false
		}
		notify := b.notify
		b.mu.RUnlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
}

// ReplayStreamSink is intentionally transport-agnostic. The Nakama client can
// implement it without coupling rollback/replay logic to a websocket library.
// Implementations must not block the simulation thread.
type ReplayStreamSink interface {
	PublishReplayHeader(ReplayStreamHeader)
	PublishReplayChunk(ReplayStreamChunk)
	PublishReplayEnd(ReplayStreamEnd)
	ReplayStreamReset(ReplayStreamReset)
}

// RollbackReplayStream publishes only frames that have aged past the spectator
// settlement window. With the default ten second delay this is 600 frames at
// 60 FPS. A rollback older than the already-published boundary invalidates the
// stream because previously sent data cannot be retracted from spectators.
type RollbackReplayStream struct {
	sink             ReplayStreamSink
	delayFrames      int32
	chunkFrames      int32
	publishedThrough int32
	sequence         uint64
	started          bool
	invalid          bool
	header           ReplayStreamHeader
}

func NewRollbackReplayStream(delaySeconds int, sink ReplayStreamSink) *RollbackReplayStream {
	if delaySeconds <= 0 {
		delaySeconds = replayStreamDelaySeconds
	}
	return &RollbackReplayStream{
		sink:        sink,
		delayFrames: int32(delaySeconds * replayStreamFPS),
		chunkFrames: replayStreamChunkFrames,
	}
}

func (r *RollbackReplayStream) SetSink(sink ReplayStreamSink) {
	if r == nil {
		return
	}
	r.sink = sink
}

// SetDelay sets the publication delay of the next Begin. Rollback never goes
// back more than GGPO's prediction window (8 frames), so one second is safe.
func (r *RollbackReplayStream) SetDelay(seconds int) {
	if r == nil || seconds <= 0 {
		return
	}
	r.delayFrames = int32(seconds * replayStreamFPS)
}

// Begin starts publishing a match; the header tells spectators about it.
func (r *RollbackReplayStream) Begin(start ReplayStreamStart) {
	if r == nil {
		return
	}
	r.publishedThrough = 0
	r.sequence = 0
	r.started = true
	r.invalid = false
	r.header = ReplayStreamHeader{
		Version:      replayStreamVersion,
		FrameRate:    replayStreamFPS,
		InputCount:   REPLAY_NUM_INPUTS,
		InputBytes:   REPLAY_INPUT_BYTES,
		DelayFrames:  r.delayFrames,
		Seed:         start.Seed,
		PreMatchTime: start.PreMatchTime,
		Stream:       newReplayStreamID(),
		MatchTime:    start.MatchTime,
		Stage:        start.Stage,
		MatchInfo:    append(json.RawMessage(nil), start.Info...),
		Segment:      start.Segment,
		Rules:        start.Rules,
		InputRemap:   start.InputRemap,
		AILevels:     start.AILevels,
		Context:      start.Context,
		Params:       start.Params,
	}
	if header := start.Header; header != nil {
		copyHeader := *header
		copyHeader.Strict = cloneSyncSettings(header.Strict)
		copyHeader.Host = cloneSyncSettings(header.Host)
		r.header.ReplayHeader = &copyHeader
	}
	if r.sink != nil {
		r.sink.PublishReplayHeader(r.header)
	}
}

func (r *RollbackReplayStream) PublishReady(rs *RollbackSession) {
	if r == nil || rs == nil || !r.started || r.invalid || r.sink == nil {
		return
	}
	eligibleThrough := rs.netTime - r.delayFrames
	if eligibleThrough <= r.publishedThrough {
		return
	}
	if r.publishedThrough < 0 {
		r.publishedThrough = 0
	}
	if eligibleThrough > int32(len(rs.replayInputs)) {
		eligibleThrough = int32(len(rs.replayInputs))
	}
	if eligibleThrough <= r.publishedThrough {
		return
	}

	// Whole chunks only: frames become eligible one per frame, and a message
	// per frame would multiply the relay's traffic (and a lobby's stored
	// stream) by the chunk size. End publishes the rest.
	if r.chunkFrames <= 0 {
		r.chunkFrames = replayStreamChunkFrames
	}
	for eligibleThrough-r.publishedThrough >= r.chunkFrames {
		end := r.publishedThrough + r.chunkFrames
		chunk, err := encodeReplayStreamChunk(rs, r.sequence, r.publishedThrough, end)
		if err != nil {
			log.Printf("Rollback replay stream encode failed at frame %d: %v", r.publishedThrough, err)
			r.invalid = true
			r.sink.ReplayStreamReset(ReplayStreamReset{Reason: "encode_failed", Stream: r.header.Stream})
			return
		}
		chunk.Stream = r.header.Stream
		r.sink.PublishReplayChunk(chunk)
		r.sequence++
		r.publishedThrough = end
	}
}

func (r *RollbackReplayStream) NoteTruncate(time int32) {
	if r == nil || !r.started || r.invalid {
		return
	}
	if time < r.publishedThrough {
		r.invalid = true
		if r.sink != nil {
			r.sink.ReplayStreamReset(ReplayStreamReset{Reason: fmt.Sprintf("rollback_before_published_frame:%d", time), Stream: r.header.Stream})
		}
	}
}

func (r *RollbackReplayStream) End(finalFrame int32, rs *RollbackSession) {
	if r == nil || !r.started || r.invalid {
		return
	}
	if finalFrame < 0 {
		finalFrame = 0
	}
	if rs != nil {
		maxFrame := int32(len(rs.replayInputs))
		if analogFrames := int32(len(rs.replayAnalogInputs)); analogFrames < maxFrame {
			maxFrame = analogFrames
		}
		if finalFrame > maxFrame {
			finalFrame = maxFrame
		}
		if r.publishedThrough < 0 {
			r.publishedThrough = 0
		}
		for r.publishedThrough < finalFrame {
			end := r.publishedThrough + r.chunkFrames
			if end > finalFrame {
				end = finalFrame
			}
			chunk, err := encodeReplayStreamChunk(rs, r.sequence, r.publishedThrough, end)
			if err != nil {
				log.Printf("Rollback replay stream final encode failed at frame %d: %v", r.publishedThrough, err)
				r.invalid = true
				if r.sink != nil {
					r.sink.ReplayStreamReset(ReplayStreamReset{Reason: "encode_failed", Stream: r.header.Stream})
				}
				return
			}
			chunk.Stream = r.header.Stream
			if r.sink != nil {
				r.sink.PublishReplayChunk(chunk)
			}
			r.sequence++
			r.publishedThrough = end
		}
	}
	if r.sink != nil {
		r.sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: finalFrame, Stream: r.header.Stream})
	}
	r.started = false
}

func encodeReplayStreamChunk(rs *RollbackSession, sequence uint64, startFrame, endFrame int32) (ReplayStreamChunk, error) {
	if rs == nil {
		return ReplayStreamChunk{}, fmt.Errorf("nil rollback session")
	}
	if startFrame < 0 || endFrame < startFrame || int(endFrame) > len(rs.replayInputs) || int(endFrame) > len(rs.replayAnalogInputs) {
		return ReplayStreamChunk{}, fmt.Errorf("invalid replay stream range %d:%d", startFrame, endFrame)
	}
	frameCount := int(endFrame - startFrame)
	payload := make([]byte, 0, frameCount*REPLAY_NUM_INPUTS*REPLAY_INPUT_BYTES)
	var frameBuf [REPLAY_INPUT_BYTES]byte
	for frame := int(startFrame); frame < int(endFrame); frame++ {
		for controller := 0; controller < REPLAY_NUM_INPUTS; controller++ {
			binary.LittleEndian.PutUint16(frameBuf[:2], uint16(rs.replayInputs[frame][controller]))
			for axis := 0; axis < len(rs.replayAnalogInputs[frame][controller]); axis++ {
				frameBuf[2+axis] = byte(rs.replayAnalogInputs[frame][controller][axis])
			}
			payload = append(payload, frameBuf[:]...)
		}
	}
	return ReplayStreamChunk{
		Sequence:   sequence,
		StartFrame: startFrame,
		FrameCount: int32(frameCount),
		Payload:    payload,
	}, nil
}

// EncodeJSON is a convenience for transports such as Nakama's JSON websocket
// adapter. Payload bytes are encoded by encoding/json as base64 automatically.
func (c ReplayStreamChunk) EncodeJSON() ([]byte, error) {
	return json.Marshal(c)
}

// PayloadBase64 avoids exposing encoding policy to transport implementations.
func (c ReplayStreamChunk) PayloadBase64() string {
	return base64.StdEncoding.EncodeToString(c.Payload)
}
