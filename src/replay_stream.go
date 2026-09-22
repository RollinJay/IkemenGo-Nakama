package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
}

// ReplayStreamChunk is a contiguous range of resolved rollback inputs. Payload
// uses the same per-controller encoding as the on-disk replay format.
type ReplayStreamChunk struct {
	Sequence   uint64 `json:"sequence"`
	StartFrame int32  `json:"start_frame"`
	FrameCount int32  `json:"frame_count"`
	Payload    []byte `json:"payload"`
}

// ReplayStreamEnd marks the final authoritative input frame of a live stream.
type ReplayStreamEnd struct {
	FinalFrame int32 `json:"final_frame"`
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
	b.finalFrame = end.FinalFrame
	b.ended = true
	b.advanceContiguousLocked()
	b.notifyLocked()
	b.mu.Unlock()
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
	ReplayStreamReset(string)
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

func (r *RollbackReplayStream) Begin(header *ReplayHeader, seed, preMatchTime int32) {
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
		Seed:         seed,
		PreMatchTime: preMatchTime,
	}
	if header != nil {
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

	for r.publishedThrough < eligibleThrough {
		end := r.publishedThrough + r.chunkFrames
		if end > eligibleThrough {
			end = eligibleThrough
		}
		chunk, err := encodeReplayStreamChunk(rs, r.sequence, r.publishedThrough, end)
		if err != nil {
			log.Printf("Rollback replay stream encode failed at frame %d: %v", r.publishedThrough, err)
			r.invalid = true
			r.sink.ReplayStreamReset("encode_failed")
			return
		}
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
			r.sink.ReplayStreamReset(fmt.Sprintf("rollback_before_published_frame:%d", time))
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
					r.sink.ReplayStreamReset("encode_failed")
				}
				return
			}
			if r.sink != nil {
				r.sink.PublishReplayChunk(chunk)
			}
			r.sequence++
			r.publishedThrough = end
		}
	}
	if r.sink != nil {
		r.sink.PublishReplayEnd(ReplayStreamEnd{FinalFrame: finalFrame})
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
