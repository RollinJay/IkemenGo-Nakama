package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	lua "github.com/yuin/gopher-lua"
)

// Match replays. A match replay file holds one match, online (a rollback
// match) or local (replay_local.go): what the match needs to be played back
// without the menus that led to it (the characters, by their select.def
// entries and definition files, their palettes, the team modes, the stage,
// the rounds and the game mode), the players' accounts for an online match,
// the files its simulation depends on with their hashes, and the inputs of
// each of the match's sessions (one per Turns character change), with the
// state each session started from.
//
// Playback loads the fight from that description and plays it as a lobby
// spectator plays a live stream (replay_live.go), so a select.def or
// screenpack changed since the recording does not change what it loads, and
// the files the match depends on that changed are reported before it plays.
//
// The file is gzip-compressed JSON lines, one gzip member per session, so
// that the sessions written before a crash stay readable:
//
//	{"type":"match", "format":"ikemen-match-replay", "version":1, "engine":..., "date":..., "info":{...}, "content":[...]}
//	{"type":"session", "header":{...}}   a ReplayStreamHeader, one per session
//	{"type":"frames", ...}               a ReplayStreamChunk: up to 600 frames of inputs
//	{"type":"end", "final_frame":N}
//
// The match record comes first, with the first session. info is what the
// scripts described (replaySetMatchInfo); the engine does not interpret it
// beyond the characters' definition files and the players' names, and fills
// in a player's online name that arrived after the match began.

const (
	matchReplayFormat      = "ikemen-match-replay"
	matchReplayVersion     = 1
	matchReplayChunkFrames = 600
	// The longest line read back: a frames record of 600 frames is about
	// 52 KB, a match record a few KB.
	matchReplayMaxLine = 8 << 20
	// Limits on what a file holds (a replay can come from anyone): sessions
	// per match, frames (three hours, in all its sessions; a longer match is
	// left to the session's own replay), and data once decompressed.
	matchReplayMaxSessions = 64
	matchReplayMaxFrames   = 3 * 60 * 60 * 60
	matchReplayMaxBytes    = 128 << 20
)

// ReplayContentItem is a file a match's simulation depends on, with the start
// of its SHA-256 when the match was recorded ("" when it was missing).
type ReplayContentItem struct {
	Label string `json:"label"`
	File  string `json:"file,omitempty"`
	Hash  string `json:"hash,omitempty"`
}

// ReplayContentDiff is a content item that differs between a replay and this
// game: changed, missing (here) or added (not in the replay).
type ReplayContentDiff struct {
	Label  string `json:"label"`
	File   string `json:"file,omitempty"`
	Status string `json:"status"`
}

type matchReplayMatch struct {
	Type    string              `json:"type"`
	Format  string              `json:"format"`
	Version int                 `json:"version"`
	Engine  string              `json:"engine"`
	Date    string              `json:"date"`
	Info    json.RawMessage     `json:"info,omitempty"`
	Content []ReplayContentItem `json:"content,omitempty"`
}

type matchReplaySession struct {
	Type   string             `json:"type"`
	Header ReplayStreamHeader `json:"header"`
}

type matchReplayFrames struct {
	Type string `json:"type"`
	ReplayStreamChunk
}

type matchReplayEnd struct {
	Type       string `json:"type"`
	FinalFrame int32  `json:"final_frame"`
}

// matchReplayRecorder saves each rollback match of a netplay session to its
// own file while saving is on (replaySaveMatches); the local recorder
// (replay_local.go) saves an offline match through one of its own.
//
// A match the scripts did not describe (replaySetMatchInfo) is not saved:
// nothing could play it back. The session's own replay (replayRecord, the
// inputs of the whole session, menus included) keeps such a match, and a
// match whose file could not be written; it is removed when saving stops if
// every match of the session was saved here.
type matchReplayRecorder struct {
	mu          sync.Mutex
	dir         string          // "" when off
	next        json.RawMessage // the next match's description (replaySetMatchInfo)
	info        json.RawMessage // the current match's description
	date        time.Time       // the current match's start
	path        string          // the current match's file, once its first session ended
	failed      bool            // the current match could not be written
	skip        bool            // the current match has no description, and is not saved
	frames      int             // the current match's frames saved so far
	uncovered   bool            // a match of the session has no complete match replay
	sessionFile string          // the session's own replay (replayRecord)
	segment     int32
	header      *ReplayStreamHeader // the current match's latest session
	open        bool                // a session began and has not ended
	content     chan []ReplayContentItem
}

var matchReplay matchReplayRecorder

// lastMatchReplayFile is the file of the last match saved (online or local).
var lastMatchReplayFile string

// start saves the rollback matches that follow to dir, one file each.
func (r *matchReplayRecorder) start(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resetLocked()
	r.dir = dir
	return nil
}

// stop ends saving. A match whose sessions ended is already written. The
// session's own replay, which the caller has closed, is removed unless a
// match of the session is kept only there.
func (r *matchReplayRecorder) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir != "" && !r.uncovered && r.sessionFile != "" {
		if err := os.Remove(r.sessionFile); err == nil {
			log.Printf("Match replay: session replay %s removed (its matches have match replays)", r.sessionFile)
		} else if !os.IsNotExist(err) {
			log.Printf("Match replay: cannot remove the session replay %s: %v", r.sessionFile, err)
		}
	}
	r.resetLocked()
}

func (r *matchReplayRecorder) resetLocked() {
	r.dir = ""
	r.next = nil
	r.info = nil
	r.path = ""
	r.failed = false
	r.skip = false
	r.frames = 0
	r.uncovered = false
	r.sessionFile = ""
	r.segment = 0
	r.header = nil
	r.open = false
	r.content = nil
}

// setSessionFile names the session's own replay, while saving is on.
func (r *matchReplayRecorder) setSessionFile(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir != "" {
		r.sessionFile = path
	}
}

func (r *matchReplayRecorder) active() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dir != ""
}

// savedFrames is how many frames of the current match are saved.
func (r *matchReplayRecorder) savedFrames() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frames
}

// setInfo sets the description of the next match.
func (r *matchReplayRecorder) setInfo(info json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next = append(json.RawMessage(nil), info...)
}

// beginSession starts a rollback session of the match: a new match when
// roundNo is 1, otherwise the next session of the current one (Turns). The
// hashes of the files the match depends on are computed meanwhile.
func (r *matchReplayRecorder) beginSession(start ReplayStreamStart, roundNo int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" {
		return
	}
	if roundNo <= 1 || r.header == nil {
		r.info = r.next
		r.next = nil
		r.date = time.Now()
		r.path = ""
		r.failed = false
		r.frames = 0
		r.segment = 0
		r.content = nil
		r.skip = len(r.info) == 0
		if r.skip {
			r.uncovered = true
			log.Printf("Match replay: the match has no description; the session's replay keeps it")
		} else {
			spec := matchContentFor(matchInfoCharDefs(r.info), start.Stage)
			r.content = make(chan []ReplayContentItem, 1)
			ch := r.content
			go func() { ch <- spec.manifest() }()
		}
	} else {
		r.segment++
	}
	header := ReplayStreamHeader{
		Version:      replayStreamVersion,
		FrameRate:    replayStreamFPS,
		InputCount:   REPLAY_NUM_INPUTS,
		InputBytes:   REPLAY_INPUT_BYTES,
		Seed:         start.Seed,
		PreMatchTime: start.PreMatchTime,
		Stream:       newReplayStreamID(),
		MatchTime:    start.MatchTime,
		Stage:        start.Stage,
		Segment:      r.segment,
		Rules:        start.Rules,
		InputRemap:   start.InputRemap,
		AILevels:     start.AILevels,
		Context:      start.Context,
		StartState:   start.StartState,
		Params:       start.Params,
		Local:        start.Local,
	}
	if start.Header != nil {
		copyHeader := *start.Header
		copyHeader.Strict = cloneSyncSettings(start.Header.Strict)
		copyHeader.Host = cloneSyncSettings(start.Header.Host)
		header.ReplayHeader = &copyHeader
	}
	r.header = &header
	r.open = true
}

// endSession writes the session's resolved inputs, frames 0 to frameCount,
// after the match record when it is the match's first session. A session
// without frames (it failed to start) adds nothing, and does not make a
// file. A match whose file cannot be written is left to the session's own
// replay.
func (r *matchReplayRecorder) endSession(rs *RollbackSession, frameCount int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dir == "" || !r.open || rs == nil {
		return
	}
	r.open = false
	if r.failed || r.skip || frameCount <= 0 {
		return
	}
	if r.frames+frameCount > matchReplayMaxFrames {
		log.Printf("Match replay: the match is longer than %d frames; the session's replay keeps it", matchReplayMaxFrames)
		r.failed, r.uncovered = true, true
		return
	}
	header := r.header
	var match *matchReplayMatch
	if r.path == "" {
		info := withMatchNames(r.info)
		match = &matchReplayMatch{
			Type:    "match",
			Format:  matchReplayFormat,
			Version: matchReplayVersion,
			Engine:  Version,
			Date:    r.date.Format(time.RFC3339),
			Info:    info,
		}
		select {
		case match.Content = <-r.content:
		case <-time.After(5 * time.Second):
			log.Printf("Match replay: content hashes took too long; saving without them")
		}
		data, err := encodeMatchReplaySession(match, header, rs, frameCount)
		if err == nil {
			r.path, err = createReplayFile(r.dir, matchReplayBaseName(r.date, info), data)
		}
		if err != nil {
			log.Printf("Match replay: cannot save the match to %s: %v", r.dir, err)
			r.failed, r.uncovered = true, true
			return
		}
		lastMatchReplayFile = r.path
	} else {
		data, err := encodeMatchReplaySession(nil, header, rs, frameCount)
		if err == nil {
			err = appendReplayFile(r.path, data)
		}
		if err != nil {
			log.Printf("Match replay: cannot write %s: %v", r.path, err)
			r.failed, r.uncovered = true, true
			return
		}
	}
	r.frames += frameCount
	log.Printf("Match replay: session %d (%d frames) saved to %s", header.Segment, frameCount, r.path)
}

// withMatchNames returns info with the online names of its players that had
// none when the match was described, from the names the fight screen shows
// by now (the server's answer can arrive after the match began).
func withMatchNames(info json.RawMessage) json.RawMessage {
	if len(info) == 0 {
		return info
	}
	dec := json.NewDecoder(bytes.NewReader(info))
	dec.UseNumber()
	var desc map[string]any
	if dec.Decode(&desc) != nil {
		return info
	}
	players, _ := desc["players"].([]any)
	changed := false
	for _, p := range players {
		player, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := player["display_name"].(string); name != "" {
			continue
		}
		user, _ := player["user_id"].(string)
		for i, u := range onlineMatchUsers {
			if user != "" && u == user && onlineMatchNames[i] != "" {
				player["display_name"] = onlineMatchNames[i]
				changed = true
			}
		}
	}
	if !changed {
		return info
	}
	out, err := json.Marshal(desc)
	if err != nil {
		return info
	}
	return out
}

// createReplayFile writes data to a new file dir/base.replay, or dir/base
// (2).replay and so on when that one exists (another game can save to the
// same folder), and returns its path.
func createReplayFile(dir, base string, data []byte) (string, error) {
	for i := 1; i < 100; i++ {
		name := base + ".replay"
		if i > 1 {
			name = fmt.Sprintf("%s (%d).replay", base, i)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(path)
			return "", err
		}
		return filepath.ToSlash(path), nil
	}
	return "", fmt.Errorf("too many replays named %q", base)
}

// appendReplayFile adds data to the end of path.
func appendReplayFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// encodeMatchReplaySession returns one gzip member: the match record (the
// first session only), the session header, its frames and its end.
func encodeMatchReplaySession(match *matchReplayMatch, header *ReplayStreamHeader, rs *RollbackSession, frameCount int) ([]byte, error) {
	if frameCount > len(rs.replayInputs) {
		frameCount = len(rs.replayInputs)
	}
	if frameCount > len(rs.replayAnalogInputs) {
		frameCount = len(rs.replayAnalogInputs)
	}
	if frameCount < 0 {
		frameCount = 0
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)
	if match != nil {
		if err := enc.Encode(match); err != nil {
			return nil, err
		}
	}
	if err := enc.Encode(matchReplaySession{Type: "session", Header: *header}); err != nil {
		return nil, err
	}
	var sequence uint64
	for start := 0; start < frameCount; start += matchReplayChunkFrames {
		end := start + matchReplayChunkFrames
		if end > frameCount {
			end = frameCount
		}
		chunk, err := encodeReplayStreamChunk(rs, sequence, int32(start), int32(end))
		if err != nil {
			return nil, err
		}
		chunk.Stream = header.Stream
		if err := enc.Encode(matchReplayFrames{Type: "frames", ReplayStreamChunk: chunk}); err != nil {
			return nil, err
		}
		sequence++
	}
	if err := enc.Encode(matchReplayEnd{Type: "end", FinalFrame: int32(frameCount)}); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// Players' state at the start of a session

// matchReplayMaxVars is the most entries a recorded variable or map table
// may hold (a replay file can come from anyone).
const matchReplayMaxVars = 4096

// ReplayPlayerState is a player's state when a session starts, as far as the
// match's setup does not decide it: life and power (a survival run carries
// life from match to match), guard and dizzy points, red life, the
// character's variables and maps, which a character kept from a run's
// previous match keeps, and whether the CPU controls it (a script can
// change a player's AI level after the character is loaded, which the
// character's control does not follow). Float values are kept as their
// bits, exactly.
type ReplayPlayerState struct {
	Player         int               `json:"player"`
	CPU            bool              `json:"cpu"`
	Life           int32             `json:"life"`
	LifeMax        int32             `json:"life_max"`
	Power          int32             `json:"power"`
	PowerMax       int32             `json:"power_max"`
	DizzyPoints    int32             `json:"dizzy_points"`
	DizzyPointsMax int32             `json:"dizzy_points_max"`
	GuardPoints    int32             `json:"guard_points"`
	GuardPointsMax int32             `json:"guard_points_max"`
	RedLife        int32             `json:"red_life"`
	Vars           map[int32]int32   `json:"vars,omitempty"`
	FVars          map[int32]uint32  `json:"fvars,omitempty"`
	SysVars        map[int32]int32   `json:"sysvars,omitempty"`
	SysFVars       map[int32]uint32  `json:"sysfvars,omitempty"`
	Maps           map[string]uint32 `json:"maps,omitempty"`
}

// matchStartState is the players' state at the start of the session being
// recorded (captured once the round is set up; see recordMatchStartState).
var matchStartState []ReplayPlayerState

// captureMatchStartState returns the state of each player the match has.
func captureMatchStartState() []ReplayPlayerState {
	var out []ReplayPlayerState
	for pn, p := range sys.chars {
		if len(p) == 0 || p[0] == nil {
			continue
		}
		c := p[0]
		st := ReplayPlayerState{
			Player:         pn + 1,
			CPU:            c.controller < 0,
			Life:           c.life,
			LifeMax:        c.lifeMax,
			Power:          c.power,
			PowerMax:       c.powerMax,
			DizzyPoints:    c.dizzyPoints,
			DizzyPointsMax: c.dizzyPointsMax,
			GuardPoints:    c.guardPoints,
			GuardPointsMax: c.guardPointsMax,
			RedLife:        c.redLife,
			Vars:           copyIntVars(c.cnsvar),
			FVars:          floatVarBits(c.cnsfvar),
			SysVars:        copyIntVars(c.cnssysvar),
			SysFVars:       floatVarBits(c.cnssysfvar),
		}
		if len(c.mapArray) > 0 {
			st.Maps = make(map[string]uint32, len(c.mapArray))
			for k, v := range c.mapArray {
				st.Maps[k] = math.Float32bits(v)
			}
		}
		out = append(out, st)
	}
	return out
}

func copyIntVars(m map[int32]int32) map[int32]int32 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[int32]int32, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func floatVarBits(m map[int32]float32) map[int32]uint32 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[int32]uint32, len(m))
	for k, v := range m {
		out[k] = math.Float32bits(v)
	}
	return out
}

// applyMatchStartState gives each recorded player the state it had when the
// recorded session started. A player the replay did not record or this
// match does not have is left as the match set it up, and so is a table
// larger than a real one.
func applyMatchStartState(states []ReplayPlayerState) {
	for _, st := range states {
		pn := st.Player - 1
		if pn < 0 || pn >= len(sys.chars) || len(sys.chars[pn]) == 0 || sys.chars[pn][0] == nil {
			continue
		}
		c := sys.chars[pn][0]
		if st.CPU {
			c.controller = ^c.playerNo
		} else {
			c.controller = c.playerNo
		}
		c.life, c.lifeMax = st.Life, st.LifeMax
		c.power, c.powerMax = st.Power, st.PowerMax
		c.dizzyPoints, c.dizzyPointsMax = st.DizzyPoints, st.DizzyPointsMax
		c.guardPoints, c.guardPointsMax = st.GuardPoints, st.GuardPointsMax
		c.redLife = st.RedLife
		if len(st.Vars) <= matchReplayMaxVars {
			c.cnsvar = make(map[int32]int32, len(st.Vars))
			for k, v := range st.Vars {
				c.cnsvar[k] = v
			}
		}
		if len(st.SysVars) <= matchReplayMaxVars {
			c.cnssysvar = make(map[int32]int32, len(st.SysVars))
			for k, v := range st.SysVars {
				c.cnssysvar[k] = v
			}
		}
		if len(st.FVars) <= matchReplayMaxVars {
			c.cnsfvar = make(map[int32]float32, len(st.FVars))
			for k, v := range st.FVars {
				c.cnsfvar[k] = math.Float32frombits(v)
			}
		}
		if len(st.SysFVars) <= matchReplayMaxVars {
			c.cnssysfvar = make(map[int32]float32, len(st.SysFVars))
			for k, v := range st.SysFVars {
				c.cnssysfvar[k] = math.Float32frombits(v)
			}
		}
		if len(st.Maps) <= matchReplayMaxVars {
			c.mapArray = make(map[string]float32, len(st.Maps))
			for k, v := range st.Maps {
				c.mapArray[k] = math.Float32frombits(v)
			}
		}
	}
}

// recordMatchStartState captures the players' state for the recorders, once
// runMatch has set the round up: a rollback session's match replay and a
// local match replay include it.
func recordMatchStartState() {
	matchStartState = nil
	if matchReplay.active() || localReplay.pending {
		matchStartState = captureMatchStartState()
	}
	localReplay.begin(matchStartState)
}

// applyStartState gives the players the state the current session of a
// match replay recorded; a live stream carries none.
func (rf *ReplayFile) applyStartState() {
	if rf == nil || rf.liveBuffer == nil {
		return
	}
	if header := rf.liveBuffer.Header(); header != nil {
		applyMatchStartState(header.StartState)
	}
}

// matchReplayBaseName names a match's file after its date and its two sides:
// a side's players' names with their tags when the description has them
// ("... KAI (7841) vs ElPeleador (6321)", or "... KAI (7841) & ElPeleador
// (6321) vs Kyo" for two players against the CPU), otherwise the name of the
// side's first character ("... Kyo vs Iori").
func matchReplayBaseName(date time.Time, info json.RawMessage) string {
	base := date.Format("2006-01-02_15h04m05s")
	type character struct {
		Name string `json:"name"`
	}
	var desc struct {
		Players []struct {
			Side        int    `json:"side"`
			DisplayName string `json:"display_name"`
			Username    string `json:"username"`
			Tag         string `json:"tag"`
		} `json:"players"`
		Selection struct {
			P1 []character `json:"p1"`
			P2 []character `json:"p2"`
		} `json:"selection"`
	}
	if len(info) == 0 || json.Unmarshal(info, &desc) != nil {
		return base
	}
	var players [2][]string
	for _, p := range desc.Players {
		if p.Side < 1 || p.Side > 2 {
			continue
		}
		name := p.DisplayName
		if name == "" {
			name = p.Username
		}
		name = fileNamePart(name)
		if name == "" {
			continue
		}
		if tag := fileNamePart(p.Tag); tag != "" {
			name += " (" + tag + ")"
		}
		players[p.Side-1] = append(players[p.Side-1], name)
	}
	var sides [2]string
	for i, chars := range [2][]character{desc.Selection.P1, desc.Selection.P2} {
		if len(players[i]) > 0 {
			sides[i] = strings.Join(players[i], " & ")
		} else if len(chars) > 0 {
			sides[i] = fileNamePart(chars[0].Name)
		}
	}
	if sides[0] == "" || sides[1] == "" {
		return base
	}
	return base + " " + sides[0] + " vs " + sides[1]
}

// fileNamePart keeps a name's letters, digits, spaces, dashes, underscores
// and dots (others become "_"), at most 24 characters.
func fileNamePart(name string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(name) {
		if n == 24 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		n++
	}
	return strings.TrimRight(strings.TrimSpace(b.String()), ".")
}

// matchInfoCharDefs reads the characters' definition files from a match
// description: selection.p1 and selection.p2, lists of {def = ...}.
func matchInfoCharDefs(info json.RawMessage) [2][]string {
	var out [2][]string
	var desc struct {
		Selection struct {
			P1 []struct {
				Def string `json:"def"`
			} `json:"p1"`
			P2 []struct {
				Def string `json:"def"`
			} `json:"p2"`
		} `json:"selection"`
	}
	if len(info) == 0 || json.Unmarshal(info, &desc) != nil {
		return out
	}
	for _, c := range desc.Selection.P1 {
		out[0] = append(out[0], c.Def)
	}
	for _, c := range desc.Selection.P2 {
		out[1] = append(out[1], c.Def)
	}
	return out
}

// ---------------------------------------------------------------------------
// Content the simulation depends on

// matchContentSpec lists what a match depends on, with the common files
// resolved on the game's thread (they come from the configuration).
type matchContentSpec struct {
	chars    [2][]string
	stage    string
	fight    string
	motifDef string
	common   []string
}

// matchContentFor lists what a match with chars on stage depends on in this
// game: its fight screen and the common files the configuration names.
func matchContentFor(chars [2][]string, stage string) matchContentSpec {
	spec := matchContentSpec{chars: chars, stage: stage, fight: sys.fightScreen.def, motifDef: sys.motif.Def}
	dirs := []string{sys.motif.Def, sys.fightScreen.def, "", "data/"}
	seen := map[string]bool{}
	add := func(files map[string][]string) {
		for _, key := range SortedKeys(files) {
			for _, v := range files[key] {
				if fp := SearchFile(v, dirs); fp != "" && !seen[fp] {
					seen[fp] = true
					spec.common = append(spec.common, fp)
				}
			}
		}
	}
	add(sys.cfg.Common.States)
	add(sys.cfg.Common.Cmd)
	add(sys.cfg.Common.Const)
	add(sys.cfg.Common.Air)
	add(sys.cfg.Common.Fx)
	sort.Strings(spec.common)
	return spec
}

var charStateFileKey = regexp.MustCompile(`^st[0-9]*$`)

// charSimulationFiles lists a character's definition file and the files of
// its [Files] section that its simulation reads: commands, states and
// animations (with their collision boxes). Sprites, sounds and palettes are
// left out.
func charSimulationFiles(def, motifDef string) [][2]string {
	out := [][2]string{{"def", def}}
	if !contentFileOK(def) {
		return out
	}
	text, err := LoadText(def)
	if err != nil {
		return out
	}
	lines, i := SplitAndTrim(text, "\n"), 0
	for i < len(lines) {
		is, name, _ := ReadIniSection(lines, &i)
		if name != "files" {
			continue
		}
		dirs := []string{def, "", motifDef, "data/"}
		for _, key := range SortedKeys(is) {
			if key == "cmd" || key == "cns" || key == "stcommon" || key == "anim" || charStateFileKey.MatchString(key) {
				value := decodeShiftJIS(is[key])
				if strings.TrimSpace(value) == "" {
					continue
				}
				fp := SearchFile(value, dirs)
				if fp == "" {
					fp = StripComment(value)
				}
				out = append(out, [2]string{key, fp})
			}
		}
		break
	}
	return out
}

// manifest hashes the files of spec. It reads files only, and can run on any
// goroutine.
func (spec matchContentSpec) manifest() []ReplayContentItem {
	hashes := map[string]string{}
	hash := func(file string) string {
		if file == "" {
			return ""
		}
		if h, ok := hashes[file]; ok {
			return h
		}
		h := ""
		if contentFileOK(file) {
			if f, err := OpenFile(file); err == nil {
				sum := sha256.New()
				if _, err := io.Copy(sum, io.LimitReader(f, matchContentMaxFile)); err == nil {
					h = hex.EncodeToString(sum.Sum(nil))[:16]
				}
				f.Close()
			}
		}
		hashes[file] = h
		return h
	}
	var out []ReplayContentItem
	item := func(label, file string) {
		out = append(out, ReplayContentItem{Label: label, File: filepath.ToSlash(file), Hash: hash(file)})
	}
	item("fight", spec.fight)
	for i, file := range spec.common {
		item(fmt.Sprintf("common %d", i+1), file)
	}
	for side := 0; side < 2; side++ {
		for slot, def := range spec.chars[side] {
			if def == "" {
				continue
			}
			for _, f := range charSimulationFiles(def, spec.motifDef) {
				item(fmt.Sprintf("p%d.%d %s", side+1, slot+1, f[0]), f[1])
			}
		}
	}
	if spec.stage != "" {
		item("stage", spec.stage)
	}
	return out
}

// matchContentMaxFile is the largest file compared as match content (a
// character's states or animations are far smaller).
const matchContentMaxFile = 64 << 20

// contentFileOK reports whether file can be read as match content: a file
// within a zip archive, or a regular file of at most matchContentMaxFile
// bytes. The files compared can be named by a replay file from anyone, which
// could name a device or a pipe.
func contentFileOK(file string) bool {
	if isZip, _, _ := IsZipPath(filepath.ToSlash(file)); isZip {
		return true
	}
	st, err := os.Stat(file)
	return err == nil && st.Mode().IsRegular() && st.Size() <= matchContentMaxFile
}

// gamePath reports whether p names a file inside the game's folder: a
// relative path without ".." steps.
func gamePath(p string) bool {
	slashed := strings.ReplaceAll(p, `\`, "/")
	if p == "" || filepath.IsAbs(p) || strings.HasPrefix(slashed, "/") || filepath.VolumeName(p) != "" || (len(p) > 1 && p[1] == ':') {
		return false
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// compareReplayContent lists the items of recorded that local does not match.
func compareReplayContent(recorded, local []ReplayContentItem) []ReplayContentDiff {
	byLabel := map[string]ReplayContentItem{}
	for _, it := range local {
		byLabel[it.Label] = it
	}
	var out []ReplayContentDiff
	seen := map[string]bool{}
	for _, rec := range recorded {
		seen[rec.Label] = true
		loc, ok := byLabel[rec.Label]
		switch {
		case !ok || (loc.Hash == "" && rec.Hash != ""):
			out = append(out, ReplayContentDiff{Label: rec.Label, File: rec.File, Status: "missing"})
		case loc.Hash != rec.Hash:
			out = append(out, ReplayContentDiff{Label: rec.Label, File: loc.File, Status: "changed"})
		}
	}
	for _, loc := range local {
		if !seen[loc.Label] {
			out = append(out, ReplayContentDiff{Label: loc.Label, File: loc.File, Status: "added"})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reading

// matchReplayData is a match replay file as read: its match record and one
// buffer per rollback session, complete with its inputs.
type matchReplayData struct {
	Match    matchReplayMatch
	Sessions []*NakamaReplayBuffer
	Frames   int32
}

// isMatchReplayFile reports whether path is a match replay (gzip data); the
// replay files of whole netplay sessions start with "IKRPLCFG".
func isMatchReplayFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	_, err = io.ReadFull(f, magic[:])
	return err == nil && magic[0] == 0x1f && magic[1] == 0x8b
}

// readMatchReplay reads a match replay file. A session cut short (the game
// stopped while writing) ends at its last complete frame.
func readMatchReplay(path string) (*matchReplayData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return nil, fmt.Errorf("not a match replay: %w", err)
	}
	defer gz.Close()
	sc := bufio.NewScanner(&cappedReader{r: gz, left: matchReplayMaxBytes})
	sc.Buffer(make([]byte, 64<<10), matchReplayMaxLine)
	data := &matchReplayData{}
	haveMatch := false
	var current *NakamaReplayBuffer
	ended := false
	var frames int64 // in all the frames records
	closeSession := func() {
		if current != nil && !ended {
			current.ApplyEnd(ReplayStreamEnd{FinalFrame: current.BufferedThrough(), Stream: current.Stream()})
		}
		if current != nil {
			final, _ := current.FinalFrame()
			data.Frames += final
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var kind struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &kind); err != nil {
			// The last line of a file cut short is incomplete.
			if !sc.Scan() && sc.Err() != nil && len(data.Sessions) > 0 {
				break
			}
			return nil, fmt.Errorf("broken record: %w", err)
		}
		switch kind.Type {
		case "match":
			if haveMatch {
				continue
			}
			if err := json.Unmarshal(line, &data.Match); err != nil {
				return nil, fmt.Errorf("broken match record: %w", err)
			}
			if data.Match.Format != matchReplayFormat {
				return nil, fmt.Errorf("not a match replay (format %q)", data.Match.Format)
			}
			if data.Match.Version > matchReplayVersion {
				return nil, fmt.Errorf("the replay was saved by a newer version of the game (format %d)", data.Match.Version)
			}
			haveMatch = true
		case "session":
			if !haveMatch {
				return nil, errors.New("the replay has no match record")
			}
			var s matchReplaySession
			if err := json.Unmarshal(line, &s); err != nil {
				return nil, fmt.Errorf("broken session record: %w", err)
			}
			closeSession()
			if len(data.Sessions) >= matchReplayMaxSessions {
				return nil, fmt.Errorf("the replay has more than %d sessions", matchReplayMaxSessions)
			}
			current = NewNakamaReplayBuffer()
			current.ApplyHeader(s.Header)
			ended = false
			data.Sessions = append(data.Sessions, current)
		case "frames":
			if current == nil || ended {
				return nil, errors.New("frames outside a session")
			}
			var fr matchReplayFrames
			if err := json.Unmarshal(line, &fr); err != nil {
				return nil, fmt.Errorf("broken frames record: %w", err)
			}
			if fr.StartFrame < 0 || fr.FrameCount < 0 || int64(fr.StartFrame)+int64(fr.FrameCount) > matchReplayMaxFrames {
				return nil, fmt.Errorf("frames %d to %d are outside a session's %d", fr.StartFrame, int64(fr.StartFrame)+int64(fr.FrameCount), matchReplayMaxFrames)
			}
			if frames += int64(fr.FrameCount); frames > matchReplayMaxFrames {
				return nil, fmt.Errorf("the replay holds more than %d frames", matchReplayMaxFrames)
			}
			if err := current.ApplyChunk(fr.ReplayStreamChunk); err != nil {
				return nil, err
			}
		case "end":
			if current == nil || ended {
				return nil, errors.New("end outside a session")
			}
			var e matchReplayEnd
			if err := json.Unmarshal(line, &e); err != nil {
				return nil, fmt.Errorf("broken end record: %w", err)
			}
			current.ApplyEnd(ReplayStreamEnd{FinalFrame: e.FinalFrame, Stream: current.Stream()})
			ended = true
		}
	}
	// A file cut short (the game stopped while writing it) keeps the
	// sessions read so far.
	if err := sc.Err(); err != nil {
		if len(data.Sessions) == 0 || errors.Is(err, errMatchReplayTooLarge) {
			return nil, err
		}
		log.Printf("Match replay %s is cut short: %v", path, err)
	}
	closeSession()
	if !haveMatch {
		return nil, errors.New("the replay has no match record")
	}
	if len(data.Sessions) == 0 {
		return nil, errors.New("the replay has no inputs")
	}
	return data, nil
}

var errMatchReplayTooLarge = fmt.Errorf("the replay holds more than %d MB of data", matchReplayMaxBytes>>20)

// cappedReader reads at most left bytes of r, then fails.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errMatchReplayTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// startMatchReplay plays data from its first session: the players' settings
// from the session header apply, as for a lobby spectator.
func startMatchReplay(data *matchReplayData) error {
	rf, err := NewLiveReplayFile(data.Sessions[0])
	if err != nil {
		return err
	}
	rf.fileSegments = append([]*NakamaReplayBuffer(nil), data.Sessions[1:]...)
	rf.fromFile = true
	if err := sys.beginReplaySession(rf); err != nil {
		rf.Close()
		return err
	}
	sys.sessionWarning = ""
	sys.replayFile = rf
	return nil
}

// ---------------------------------------------------------------------------
// Lua

func matchReplayScriptInit(l *lua.LState) {
	luaRegister(l, "replaySaveMatches", func(l *lua.LState) int {
		/*Save each rollback match of the netplay session that follows as its own
		match replay file, or stop saving. A match is saved when it was
		described with `replaySetMatchInfo`. When the session is also recorded
		with `replayRecord` (called after this function), that file is removed
		as saving stops unless a match of the session has no match replay: it
		was not described, or its file could not be written.
		@function replaySaveMatches
		@tparam[opt] string dir The directory for the files; omitted or `""` stops saving.*/
		if nilArg(l, 1) || strArg(l, 1) == "" {
			matchReplay.stop()
			return 0
		}
		if err := matchReplay.start(strArg(l, 1)); err != nil {
			l.RaiseError("cannot save replays to %s: %v", strArg(l, 1), err)
		}
		return 0
	})
	luaRegister(l, "replaySetMatchInfo", func(l *lua.LState) int {
		/*Describe the next match for its match replay: the characters, stage
		and rules it needs, the players and anything else the scripts read back
		when it is played.
		@function replaySetMatchInfo
		@tparam[opt] table info The description, saved as JSON; `nil`: the next
		  match is not saved as a match replay.*/
		var info json.RawMessage
		if !nilArg(l, 1) {
			data, err := json.Marshal(luaRematchValueToAny(l.Get(1)))
			if err != nil {
				l.RaiseError("encode match info: %v", err)
			}
			info = data
		}
		matchReplay.setInfo(info)
		return 0
	})
	luaRegister(l, "replayMatchInfo", func(l *lua.LState) int {
		/*Read a match replay's description without playing it.
		@function replayMatchInfo
		@tparam string path The replay file.
		@treturn table|nil replay `nil` for a replay of a whole netplay session;
		  otherwise `info` (the scripts' description), `engine` (the version that
		  recorded it), `this_engine`, `same_engine`, `date`, `sessions`,
		  `frames` (the match's length), `stage` (its definition file),
		  `ai_levels` (each player's AI level at the start, by player number)
		  and `turns_offset` (for each side, how many of its Turns members
		  the fight skipped); the last two are absent from older files.
		@treturn string|nil error Why the file could not be read.*/
		path := strArg(l, 1)
		if !isMatchReplayFile(path) {
			l.Push(lua.LNil)
			return 1
		}
		data, err := readMatchReplay(path)
		if err != nil {
			l.Push(lua.LNil)
			l.Push(lua.LString(err.Error()))
			return 2
		}
		t := l.NewTable()
		t.RawSetString("info", jsonToLValue(l, data.Match.Info))
		t.RawSetString("engine", lua.LString(data.Match.Engine))
		t.RawSetString("this_engine", lua.LString(Version))
		t.RawSetString("same_engine", lua.LBool(data.Match.Engine == Version))
		t.RawSetString("date", lua.LString(data.Match.Date))
		t.RawSetString("sessions", lua.LNumber(len(data.Sessions)))
		t.RawSetString("frames", lua.LNumber(data.Frames))
		if header := data.Sessions[0].Header(); header != nil {
			t.RawSetString("stage", lua.LString(header.Stage))
			if len(header.AILevels) > 0 {
				levels := l.NewTable()
				for i, level := range header.AILevels {
					if i >= MaxPlayerNo {
						break
					}
					levels.RawSetInt(i+1, lua.LNumber(Clamp(level, 0, 8)))
				}
				t.RawSetString("ai_levels", levels)
			}
			if header.Params != nil {
				offsets := l.NewTable()
				for side, off := range header.Params.TurnsOffset {
					offsets.RawSetInt(side+1, lua.LNumber(Max(off, 0)))
				}
				t.RawSetString("turns_offset", offsets)
			}
		}
		l.Push(t)
		return 1
	})
	luaRegister(l, "replayMatchContent", func(l *lua.LState) int {
		/*Compare the files a match replay depends on with this game's.
		@function replayMatchContent
		@tparam string path The replay file.
		@tparam table chars `{p1 = {def, ...}, p2 = {def, ...}}`: this game's
		  definition files of the replay's characters, in the replay's order.
		@tparam string stage This game's definition file of the replay's stage.
		@treturn table differences A list of `{label, file, status}`: status is
		  `changed`, `missing` or `added`; label names the file's role, such as
		  `p1.1 cns`, `stage`, `fight` or `common 2`. Only files inside the
		  game's folder are read. Raises an error when the replay cannot be read.*/
		data, err := readMatchReplay(strArg(l, 1))
		if err != nil {
			l.RaiseError("%s", err.Error())
		}
		var chars [2][]string
		if t, ok := l.Get(2).(*lua.LTable); ok {
			for side := 0; side < 2; side++ {
				if list, ok := t.RawGetString(fmt.Sprintf("p%d", side+1)).(*lua.LTable); ok {
					for i := 1; i <= list.Len(); i++ {
						def := lua.LVAsString(list.RawGetInt(i))
						if !gamePath(def) {
							def = ""
						}
						chars[side] = append(chars[side], def)
					}
				}
			}
		}
		// The common files are compared at the paths the replay recorded:
		// playing applies the recording's settings, which name them. Only
		// paths inside the game's folder are read.
		stage := strArg(l, 3)
		if !gamePath(stage) {
			stage = ""
		}
		local := matchContentSpec{chars: chars, stage: stage, fight: sys.fightScreen.def, motifDef: sys.motif.Def}
		for _, it := range data.Match.Content {
			if strings.HasPrefix(it.Label, "common ") {
				file := it.File
				if !gamePath(file) {
					file = "" // not read: it shows as missing
				}
				local.common = append(local.common, file)
			}
		}
		out := l.NewTable()
		for _, d := range compareReplayContent(data.Match.Content, local.manifest()) {
			row := l.NewTable()
			row.RawSetString("label", lua.LString(d.Label))
			row.RawSetString("file", lua.LString(d.File))
			row.RawSetString("status", lua.LString(d.Status))
			out.Append(row)
		}
		l.Push(out)
		return 1
	})
	luaRegister(l, "replayMatchPlay", func(l *lua.LState) int {
		/*Start playing a match replay. The scripts then load the fight it
		describes (launchFight); its inputs play it.
		@function replayMatchPlay
		@tparam string path The replay file.*/
		if sys.replayFile != nil {
			l.RaiseError("a replay is already active")
		}
		if sys.netConnection != nil || sys.rollback.session != nil {
			l.RaiseError("cannot play a replay during an online match")
		}
		data, err := readMatchReplay(strArg(l, 1))
		if err != nil {
			l.RaiseError("%s", err.Error())
		}
		if err := startMatchReplay(data); err != nil {
			l.RaiseError("%s", err.Error())
		}
		l.Push(lua.LTrue)
		return 1
	})
}

// jsonToLValue decodes JSON into Lua values (numbers kept exact).
func jsonToLValue(l *lua.LState, raw json.RawMessage) lua.LValue {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return lua.LNil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return lua.LNil
	}
	return toLValue(l, v)
}
