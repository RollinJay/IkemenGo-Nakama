package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSession is a rollback session whose resolved inputs follow a pattern
// from base, frames long.
func fakeSession(frames int, base int) *RollbackSession {
	rs := &RollbackSession{}
	rs.replayInputs = make([][REPLAY_NUM_INPUTS]InputBits, frames)
	rs.replayAnalogInputs = make([][REPLAY_NUM_INPUTS][6]int8, frames)
	for f := 0; f < frames; f++ {
		for c := 0; c < REPLAY_NUM_INPUTS; c++ {
			rs.replayInputs[f][c] = InputBits((f + base + c) % 4096)
			rs.replayAnalogInputs[f][c][c%6] = int8((f + c) % 100)
		}
	}
	return rs
}

func matchReplayFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// A match of two rollback sessions (a Turns character change) is saved to
// one file, named after the players, and reads back with its description,
// its rules and every input. A match without a description is not saved.
func TestMatchReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	var r matchReplayRecorder
	if err := r.start(dir); err != nil {
		t.Fatal(err)
	}
	info := json.RawMessage(`{"kind":"lobby","players":[{"side":1,"user_id":"u1","display_name":"KAI","tag":"7841"},` +
		`{"side":2,"user_id":"u2","display_name":"El/Pele:ador","tag":"6321"}],` +
		`"selection":{"p1teammode":"turns","p2teammode":"single","p1":[{"char":"kfm","def":"","pal":1}],"p2":[{"char":"kfm","def":"","pal":2}]}}`)
	r.setInfo(info)
	header := &ReplayHeader{SyncVersion: syncConfigVersion, ContentFingerprint: "abc"}
	rules := &ReplayMatchRules{RoundTime: 5940, FramesPerCount: 60, MatchWins: [2]int32{2, 1}, MaxDraws: [2]int32{1, -1}}
	remap := []int{1, 0, 2, 3, 4, 5, 6, 7}
	ai := []float32{0, 0, 4, 6, 0, 0, 0, 0}
	first, second := fakeSession(1500, 0), fakeSession(700, 7)
	r.beginSession(ReplayStreamStart{Header: header, Seed: 42, PreMatchTime: 3, MatchTime: 120, Stage: "stages/a.def", Rules: rules, InputRemap: remap, AILevels: ai}, 1)
	r.endSession(first, 1500)
	r.beginSession(ReplayStreamStart{Header: header, Seed: 43, MatchTime: 1620, Stage: "stages/a.def", Rules: rules, InputRemap: remap, AILevels: ai}, 2)
	r.endSession(second, 700)
	// The next match has no description: it is not saved.
	r.setInfo(nil)
	r.beginSession(ReplayStreamStart{Header: header, Seed: 44, Stage: "stages/a.def"}, 1)
	r.endSession(fakeSession(10, 1), 10)
	// The one after it is, in a file of its own.
	r.setInfo(json.RawMessage(`{"kind":"match"}`))
	r.beginSession(ReplayStreamStart{Header: header, Seed: 45, Stage: "stages/a.def"}, 1)
	r.endSession(fakeSession(20, 2), 20)
	r.stop()

	files := matchReplayFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("files %v, want 2", files)
	}
	var named, other string
	for _, f := range files {
		if strings.Contains(f, "KAI (7841) vs El_Pele_ador (6321)") {
			named = f
		} else {
			other = f
		}
	}
	if named == "" {
		t.Fatalf("no file named after the players: %v", files)
	}
	if data, err := readMatchReplay(other); err != nil || len(data.Sessions) != 1 || data.Sessions[0].Header().Seed != 45 {
		t.Fatalf("the described match after the undescribed one: %v", err)
	}
	if !isMatchReplayFile(named) {
		t.Fatal("not recognized as a match replay")
	}
	data, err := readMatchReplay(named)
	if err != nil {
		t.Fatal(err)
	}
	if data.Match.Format != matchReplayFormat || data.Match.Engine != Version || string(data.Match.Info) != string(info) {
		t.Fatalf("match record %+v", data.Match)
	}
	if len(data.Sessions) != 2 || data.Frames != 2200 {
		t.Fatalf("%d sessions, %d frames", len(data.Sessions), data.Frames)
	}
	for i, want := range []struct {
		rs     *RollbackSession
		frames int32
		seed   int32
	}{{first, 1500, 42}, {second, 700, 43}} {
		b := data.Sessions[i]
		h := b.Header()
		if h.Seed != want.seed || h.Segment != int32(i) || h.Stage != "stages/a.def" || h.ReplayHeader == nil || h.ReplayHeader.ContentFingerprint != "abc" {
			t.Fatalf("session %d header %+v", i, h)
		}
		if h.Rules == nil || *h.Rules != *rules {
			t.Fatalf("session %d rules %+v, want %+v", i, h.Rules, rules)
		}
		if fmt.Sprint(h.InputRemap) != fmt.Sprint(remap) || fmt.Sprint(h.AILevels) != fmt.Sprint(ai) {
			t.Fatalf("session %d input slots %v, AI levels %v", i, h.InputRemap, h.AILevels)
		}
		if final, ended := b.FinalFrame(); !ended || final != want.frames || b.BufferedThrough() != want.frames {
			t.Fatalf("session %d: final %d ended %v buffered %d", i, final, ended, b.BufferedThrough())
		}
		for f := int32(0); f < want.frames; f += 97 {
			got, ok := b.Frame(f)
			if !ok || got.Inputs != want.rs.replayInputs[f] || got.Axes != want.rs.replayAnalogInputs[f] {
				t.Fatalf("session %d frame %d differs", i, f)
			}
		}
	}
}

// A file cut short (the game stopped while writing its second session)
// still plays its first session.
func TestMatchReplayTruncated(t *testing.T) {
	dir := t.TempDir()
	var r matchReplayRecorder
	if err := r.start(dir); err != nil {
		t.Fatal(err)
	}
	r.setInfo(json.RawMessage(`{"kind":"match"}`))
	r.beginSession(ReplayStreamStart{Seed: 1, Stage: "s.def"}, 1)
	r.endSession(fakeSession(900, 0), 900)
	path := matchReplayFiles(t, dir)[0]
	sizeFirst := fileSize(t, path)
	r.beginSession(ReplayStreamStart{Seed: 2, Stage: "s.def"}, 2)
	r.endSession(fakeSession(900, 3), 900)
	sizeBoth := fileSize(t, path)
	if err := os.Truncate(path, sizeFirst+(sizeBoth-sizeFirst)/2); err != nil {
		t.Fatal(err)
	}
	data, err := readMatchReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	if final, _ := data.Sessions[0].FinalFrame(); final != 900 {
		t.Fatalf("first session: %d frames", final)
	}
	if len(data.Sessions) > 1 {
		if final, ended := data.Sessions[1].FinalFrame(); !ended || final > 900 {
			t.Fatalf("second session: %d frames (ended %v)", final, ended)
		}
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// The content a match depends on: a character's definition, commands,
// states and animations, and the stage. A changed or missing file is listed.
func TestMatchReplayContent(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return filepath.ToSlash(p)
	}
	def := write("kfm.def", "[Info]\nname = \"Kung Fu Man\"\n[Files]\ncmd = kfm.cmd\ncns = kfm.cns\nst = kfm.cns\nst1 = extra.st\nanim = kfm.air\nsprite = kfm.sff\n")
	write("kfm.cmd", "[Command]\n")
	write("kfm.cns", "[Statedef 0]\n")
	write("extra.st", "[Statedef 1]\n")
	air := write("kfm.air", "[Begin Action 0]\n")
	stage := write("stage.def", "[Info]\n")
	spec := matchContentSpec{chars: [2][]string{{def}, {def}}, stage: stage}
	recorded := spec.manifest()
	labels := map[string]bool{}
	for _, it := range recorded {
		labels[it.Label] = true
	}
	for _, want := range []string{"p1.1 def", "p1.1 cmd", "p1.1 cns", "p1.1 st", "p1.1 st1", "p1.1 anim", "p2.1 cns", "stage"} {
		if !labels[want] {
			t.Fatalf("no %q in %+v", want, recorded)
		}
	}
	if labels["p1.1 sprite"] {
		t.Fatal("sprites do not affect the simulation")
	}
	if diffs := compareReplayContent(recorded, spec.manifest()); len(diffs) != 0 {
		t.Fatalf("same content: %+v", diffs)
	}
	write("extra.st", "[Statedef 1]\nchanged\n")
	if err := os.Remove(air); err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, d := range compareReplayContent(recorded, spec.manifest()) {
		status[d.Label] = d.Status
	}
	if status["p1.1 st1"] != "changed" || status["p2.1 st1"] != "changed" || status["p1.1 anim"] != "missing" || len(status) != 4 {
		t.Fatalf("differences %v", status)
	}
}

// The replay of the whole session (replayRecord) is removed when saving
// stops if each match of the session has a match replay, and kept when a
// match had no description (only that file keeps it).
func TestMatchReplaySessionFile(t *testing.T) {
	for _, tc := range []struct {
		name      string
		infos     []string
		wantKept  bool
		wantFiles int
	}{
		{"all described", []string{`{"kind":"match"}`, `{"kind":"match"}`}, false, 2},
		{"no match played", nil, false, 0},
		{"one undescribed", []string{`{"kind":"match"}`, ``}, true, 1},
		// A match whose session failed to start has no frames and no file.
		{"no frames", []string{`{"kind":"match"}`, `{"kind":"none"}`}, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			session := filepath.Join(dir, "session.replay")
			var r matchReplayRecorder
			if err := r.start(dir); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(session, []byte("IKRPLCFG"), 0o644); err != nil {
				t.Fatal(err)
			}
			r.setSessionFile(session)
			for i, info := range tc.infos {
				r.setInfo(json.RawMessage(info))
				r.beginSession(ReplayStreamStart{Seed: int32(i), Stage: "s.def"}, 1)
				frames := 30
				if strings.Contains(info, "none") {
					frames = 0
				}
				r.endSession(fakeSession(30, i), frames)
			}
			r.stop()
			_, err := os.Stat(session)
			if kept := err == nil; kept != tc.wantKept {
				t.Fatalf("session replay kept %v, want %v", kept, tc.wantKept)
			}
			files := 0
			for _, f := range matchReplayFiles(t, dir) {
				if isMatchReplayFile(f) {
					files++
				}
			}
			if files != tc.wantFiles {
				t.Fatalf("%d match replays, want %d", files, tc.wantFiles)
			}
		})
	}
	// Without match replays (no replaySaveMatches), the session file is not
	// the recorder's to remove.
	dir := t.TempDir()
	session := filepath.Join(dir, "session.replay")
	if err := os.WriteFile(session, []byte("IKRPLCFG"), 0o644); err != nil {
		t.Fatal(err)
	}
	var r matchReplayRecorder
	r.setSessionFile(session)
	r.stop()
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("session replay removed without match replays: %v", err)
	}
}

// A match whose file cannot be written leaves the session's own replay in
// place: it is the only copy of that match.
func TestMatchReplayWriteFailureKeepsSession(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "replays")
	session := filepath.Join(root, "session.replay")
	if err := os.WriteFile(session, []byte("IKRPLCFG"), 0o644); err != nil {
		t.Fatal(err)
	}
	var r matchReplayRecorder
	if err := r.start(dir); err != nil {
		t.Fatal(err)
	}
	r.setSessionFile(session)
	r.setInfo(json.RawMessage(`{"kind":"match"}`))
	r.beginSession(ReplayStreamStart{Seed: 1, Stage: "s.def"}, 1)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.endSession(fakeSession(30, 0), 30)
	r.stop()
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("session replay removed after a failed write: %v", err)
	}
}

// Two games saving to one folder get a file each.
func TestCreateReplayFileExclusive(t *testing.T) {
	dir := t.TempDir()
	a, err := createReplayFile(dir, "2026-09-29_17h05m03s A vs B", []byte("one"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := createReplayFile(dir, "2026-09-29_17h05m03s A vs B", []byte("two"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !strings.HasSuffix(b, "A vs B (2).replay") {
		t.Fatalf("paths %q and %q", a, b)
	}
	if data, _ := os.ReadFile(a); string(data) != "one" {
		t.Fatalf("first file holds %q", data)
	}
}

// A name that arrives from the server after the match began is filled in
// when the match is saved; names the description has stay.
func TestWithMatchNames(t *testing.T) {
	savedNames, savedUsers := onlineMatchNames, onlineMatchUsers
	defer func() { onlineMatchNames, onlineMatchUsers = savedNames, savedUsers }()
	setOnlineMatchNames("KAI", "ElPeleador", "u1", "u2")
	info := json.RawMessage(`{"kind":"match","players":[{"side":1,"user_id":"u1","display_name":"","tag":"7841"},` +
		`{"side":2,"user_id":"u2","display_name":"Kept"}],"roundtime":99}`)
	var got struct {
		Players []struct {
			DisplayName string `json:"display_name"`
			Tag         string `json:"tag"`
		} `json:"players"`
		RoundTime json.Number `json:"roundtime"`
	}
	if err := json.Unmarshal(withMatchNames(info), &got); err != nil {
		t.Fatal(err)
	}
	if got.Players[0].DisplayName != "KAI" || got.Players[0].Tag != "7841" || got.Players[1].DisplayName != "Kept" || got.RoundTime != "99" {
		t.Fatalf("filled description %+v", got)
	}
	if string(withMatchNames(json.RawMessage(`{"players":[{"side":1,"user_id":"u9"}]}`))) != `{"players":[{"side":1,"user_id":"u9"}]}` {
		t.Fatal("a description without a known name changed")
	}
}

// Paths a replay file names are read only inside the game's folder, and only
// from regular files.
func TestReplayPathChecks(t *testing.T) {
	for p, want := range map[string]bool{
		"chars/kfm/kfm.def": true, "data/common.air": true, "kfm.def": true,
		"": false, "/etc/passwd": false, "../outside.def": false, "chars/../../x.def": false,
		`chars\..\..\x.def`: false, "C:/x.def": false, `\\server\share\x.def`: false,
	} {
		if got := gamePath(p); got != want {
			t.Errorf("gamePath(%q) = %v, want %v", p, got, want)
		}
	}
	dir := t.TempDir()
	regular := filepath.Join(dir, "a.cns")
	if err := os.WriteFile(regular, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !contentFileOK(regular) || contentFileOK(dir) || contentFileOK(filepath.Join(dir, "missing")) {
		t.Fatal("regular file, folder or missing file judged wrongly")
	}
	// A device never ends: it is not read (where the system has one).
	if st, err := os.Stat("/dev/zero"); err == nil && !st.Mode().IsRegular() && contentFileOK("/dev/zero") {
		t.Fatal("a device would be read")
	}
	spec := matchContentSpec{common: []string{"/dev/zero"}}
	done := make(chan []ReplayContentItem, 1)
	go func() { done <- spec.manifest() }()
	select {
	case items := <-done:
		for _, it := range items {
			if it.Hash != "" {
				t.Fatalf("hashed %s", it.File)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("hashing a device did not return")
	}
}

// A match longer than a file may hold is left to the session's own replay,
// and a file over the limit (made by other means) does not read back.
func TestMatchReplayFrameLimit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "replays")
	session := filepath.Join(root, "session.replay")
	if err := os.WriteFile(session, []byte("IKRPLCFG"), 0o644); err != nil {
		t.Fatal(err)
	}
	var r matchReplayRecorder
	if err := r.start(dir); err != nil {
		t.Fatal(err)
	}
	r.setSessionFile(session)
	r.setInfo(json.RawMessage(`{"kind":"match"}`))
	long := fakeSession(matchReplayMaxFrames/2+10, 0)
	for i := int32(1); i <= 2; i++ {
		r.beginSession(ReplayStreamStart{Seed: i, Stage: "s.def"}, i)
		r.endSession(long, len(long.replayInputs))
	}
	r.stop()
	if _, err := os.Stat(session); err != nil {
		t.Fatalf("the session replay of an over-long match was removed: %v", err)
	}
	path := matchReplayFiles(t, dir)[0]
	if data, err := readMatchReplay(path); err != nil || len(data.Sessions) != 1 {
		t.Fatalf("the sessions within the limit: %v", err)
	}
	// Another session appended past the limit.
	header := &ReplayStreamHeader{Version: replayStreamVersion, Stream: "extra"}
	extra, err := encodeMatchReplaySession(nil, header, long, len(long.replayInputs))
	if err != nil {
		t.Fatal(err)
	}
	if err := appendReplayFile(path, extra); err != nil {
		t.Fatal(err)
	}
	if _, err := readMatchReplay(path); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("a file over the limit read back: %v", err)
	}
}

// Data past the decompressed limit (filler a real replay does not have)
// stops the reading.
func TestMatchReplayDataLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "filler.replay")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	fmt.Fprintf(gz, "{\"type\":\"match\",\"format\":%q,\"version\":1}\n", matchReplayFormat)
	filler := []byte(strings.Repeat("\n", 1<<20))
	for written := 0; written <= matchReplayMaxBytes; written += len(filler) {
		if _, err := gz.Write(filler); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readMatchReplay(path); !errors.Is(err, errMatchReplayTooLarge) {
		t.Fatalf("read %v, want the data limit", err)
	}
}

// The recorded input slots replace the local ones for the players who read
// replay input; a slot outside the replay's leaves a player's as it is.
func TestApplyReplayInputRemap(t *testing.T) {
	saved := sys.inputRemap
	defer func() { sys.inputRemap = saved }()
	for i := range sys.inputRemap {
		sys.inputRemap[i] = i
	}
	applyReplayInputRemap([]int{1, 0, 99, -1, 4})
	want := []int{1, 0, 2, 3, 4}
	for i, w := range want {
		if sys.inputRemap[i] != w {
			t.Fatalf("inputRemap %v, want %v first", sys.inputRemap, want)
		}
	}
	long := make([]int, len(sys.inputRemap)+4)
	applyReplayInputRemap(long)
	for i := REPLAY_NUM_INPUTS; i < len(sys.inputRemap); i++ {
		if sys.inputRemap[i] != i {
			t.Fatalf("player %d, who reads no replay slot, remapped to %d", i+1, sys.inputRemap[i])
		}
	}
	applyReplayInputRemap(nil)
	if sys.inputRemap[0] != 0 {
		t.Fatal("no recorded slots changed a player's")
	}
}

// Recorded rules replace the ones the screens set; values no match could
// have leave a rule as it is.
func TestReplayMatchRulesApply(t *testing.T) {
	saved := [4]any{sys.maxRoundTime, sys.curFramesPerCount, sys.matchWins, sys.maxDraws}
	defer func() {
		sys.maxRoundTime = saved[0].(int32)
		sys.curFramesPerCount = saved[1].(int32)
		sys.matchWins = saved[2].([2]int32)
		sys.maxDraws = saved[3].([2]int32)
	}()
	sys.maxRoundTime, sys.curFramesPerCount = 5940, 60
	sys.matchWins, sys.maxDraws = [2]int32{2, 2}, [2]int32{1, 1}
	recorded := currentMatchRules()
	(&ReplayMatchRules{RoundTime: -1, FramesPerCount: 120, MatchWins: [2]int32{3, 1}, MaxDraws: [2]int32{-1, 0}}).apply()
	if sys.maxRoundTime != -1 || sys.curFramesPerCount != 120 || sys.matchWins != [2]int32{3, 1} || sys.maxDraws != [2]int32{-1, 0} {
		t.Fatalf("applied: time %d fpc %d wins %v draws %v", sys.maxRoundTime, sys.curFramesPerCount, sys.matchWins, sys.maxDraws)
	}
	recorded.apply()
	if *currentMatchRules() != *recorded {
		t.Fatalf("restored %+v, want %+v", *currentMatchRules(), *recorded)
	}
	(&ReplayMatchRules{RoundTime: -5, FramesPerCount: 0, MatchWins: [2]int32{0, -3}, MaxDraws: [2]int32{-2, 4}}).apply()
	if sys.maxRoundTime != 5940 || sys.curFramesPerCount != 60 || sys.matchWins != [2]int32{2, 2} || sys.maxDraws != [2]int32{1, 4} {
		t.Fatalf("invalid values applied: time %d fpc %d wins %v draws %v", sys.maxRoundTime, sys.curFramesPerCount, sys.matchWins, sys.maxDraws)
	}
	var none *ReplayMatchRules
	none.apply()
}

func TestMatchReplayFileName(t *testing.T) {
	date := time.Date(2026, 9, 29, 17, 5, 3, 0, time.UTC)
	for info, want := range map[string]string{
		``: "2026-09-29_17h05m03s",
		`{"players":[{"side":1,"display_name":"KAI"}]}`:                                                              "2026-09-29_17h05m03s",
		`{"players":[{"side":2,"display_name":"カイ","tag":"0042"},{"side":1,"username":"a<b>:c","display_name":""}]}`: "2026-09-29_17h05m03s a_b__c vs カイ (0042)",
		`{"players":[{"side":1,"display_name":"..hidden"},{"side":2,"display_name":"x"}]}`:                           "2026-09-29_17h05m03s ..hidden vs x",
		// Two players against the CPU, and a local match: characters' names
		// where a side has no player.
		`{"players":[{"side":1,"display_name":"KAI","tag":"7841"},{"side":1,"display_name":"El"}],"selection":{"p1":[{"name":"Kyo"},{"name":"Mai"}],"p2":[{"name":"Iori"}]}}`: "2026-09-29_17h05m03s KAI (7841) & El vs Iori",
		`{"selection":{"p1":[{"name":"Kyo"}],"p2":[{"name":"Iori/2"},{"name":"Mai"}]}}`:                                                                                       "2026-09-29_17h05m03s Kyo vs Iori_2",
		`{"selection":{"p1":[{"name":"Kyo"}],"p2":[]}}`:                                                                                                                       "2026-09-29_17h05m03s",
	} {
		if got := matchReplayBaseName(date, json.RawMessage(info)); got != want {
			t.Errorf("name for %s = %q, want %q", info, got, want)
		}
	}
}
