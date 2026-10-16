package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// The game's scripts parse (gopher-lua's parser, as the engine loads them).
func TestScriptsParse(t *testing.T) {
	files, _ := filepath.Glob("../external/script/*.lua")
	mods, _ := filepath.Glob("../external/mods/*.lua")
	files = append(files, mods...)
	if len(files) < 5 {
		t.Fatalf("scripts: %v", files)
	}
	for _, f := range files {
		src, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parse.Parse(src, f)
		src.Close()
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// The screen after a ranked set (online.f_rankedSetOver, external/script/
// online.lua) run by two games in step, as a netplay session's menus run:
// each refresh() is one frame of both games; the frame k a game draws
// carries the ready bit it set a few frames before (its input delay); a
// game's answer arrives at a time of its own. Both games must leave the
// wait on the same frame, whatever the delays and arrival times.

// lockstep is the two games' session: the ready bit each game sent for each
// frame, and how many frames each has run.
type lockstep struct {
	mu      sync.Mutex
	cond    *sync.Cond
	delay   [2]int
	flag    [2]bool
	sent    [2]map[int]bool
	written [2]int // frames written: frames below are known
	frames  [2]int // frames run
	done    [2]bool
	left    [2]int // the frame from which a game has left the session (-1: never)
}

func newLockstep(d0, d1 int) *lockstep {
	ls := &lockstep{delay: [2]int{d0, d1}, left: [2]int{-1, -1}}
	ls.cond = sync.NewCond(&ls.mu)
	for g := 0; g < 2; g++ {
		ls.sent[g] = map[int]bool{}
		ls.written[g] = ls.delay[g]
	}
	return ls
}

func (ls *lockstep) hasLeft(g, frame int) bool {
	return ls.done[g] || (ls.left[g] >= 0 && frame >= ls.left[g])
}

// refresh runs game g's next frame: it sends its bit for the frame its delay
// puts it in, then waits for the other game's bit of the frame it runs.
func (ls *lockstep) refresh(g int) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	o := 1 - g
	k := ls.frames[g]
	ls.sent[g][k+ls.delay[g]] = ls.flag[g]
	ls.written[g] = k + ls.delay[g] + 1
	ls.cond.Broadcast()
	for ls.written[o] <= k && !ls.hasLeft(o, k) && !ls.hasLeft(g, k) {
		ls.cond.Wait()
	}
	ls.frames[g] = k + 1
}

// current returns the bits of the frame game g ran last.
func (ls *lockstep) current(g int) (bool, bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	k := ls.frames[g] - 1
	if k < 0 {
		return false, false
	}
	return ls.sent[g][k], ls.sent[1-g][k]
}

func (ls *lockstep) running(g int) bool {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	k := ls.frames[g]
	return !ls.hasLeft(1-g, k) && !ls.hasLeft(g, k)
}

// waitGame is one game's Lua state with online.lua and the stand-ins of the
// engine functions it calls.
type waitGame struct {
	l      *lua.LState
	out    []string
	answer int // the frame (of this game's own) at which its answer arrives; -1 never
}

func newWaitGame(t *testing.T, ls *lockstep, g, answer int, seconds float64) *waitGame {
	wg := &waitGame{l: lua.NewState(), answer: answer}
	l := wg.l
	l.SetGlobal("print", l.NewFunction(func(l *lua.LState) int {
		parts := []string{}
		for i := 1; i <= l.GetTop(); i++ {
			parts = append(parts, l.ToStringMeta(l.Get(i)).String())
		}
		wg.out = append(wg.out, strings.Join(parts, "\t"))
		return 0
	}))
	l.SetGlobal("refresh", l.NewFunction(func(l *lua.LState) int {
		ls.refresh(g)
		ls.mu.Lock()
		k := ls.frames[g]
		ls.mu.Unlock()
		if k == wg.answer {
			// The answer to the claim comes (between two frames, as the
			// engine runs Nakama's events).
			handlers := l.GetGlobal("handlers").(*lua.LTable)
			fn := handlers.RawGetString("ranked_result")
			payload := l.NewTable()
			payload.RawSetString("set_id", lua.LString("m.lv"))
			payload.RawSetString("status", lua.LString("settled"))
			payload.RawSetString("final", lua.LTrue)
			result := l.NewTable()
			result.RawSetString("won", lua.LBool(g == 0))
			result.RawSetString("label", lua.LString("Rating"))
			result.RawSetString("level_before_text", lua.LString("1,000"))
			result.RawSetString("level_text", lua.LString("1,016"))
			result.RawSetString("change_text", lua.LString("+16"))
			payload.RawSetString("result", result)
			ev := l.NewTable()
			ev.RawSetString("payload", payload)
			if err := l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, lua.LString("ranked_result"), ev); err != nil {
				panic(err)
			}
		}
		return 0
	}))
	nk := l.NewTable()
	handlers := l.NewTable()
	l.SetGlobal("handlers", handlers)
	nk.RawSetString("on", l.NewFunction(func(l *lua.LState) int {
		handlers.RawSetString(l.CheckString(1), l.Get(2))
		return 0
	}))
	nk.RawSetString("setReady", l.NewFunction(func(l *lua.LState) int {
		ls.mu.Lock()
		ls.flag[g] = l.ToBool(1)
		ls.mu.Unlock()
		return 0
	}))
	nk.RawSetString("ready", l.NewFunction(func(l *lua.LState) int {
		a, b := ls.current(g)
		l.Push(lua.LBool(a && b))
		return 1
	}))
	nk.RawSetString("sessionFrame", l.NewFunction(func(l *lua.LState) int {
		ls.mu.Lock()
		k := ls.frames[g]
		ls.mu.Unlock()
		l.Push(lua.LNumber(k))
		l.Push(lua.LBool(ls.running(g)))
		return 2
	}))
	nk.RawSetString("rankedSet", l.NewFunction(func(l *lua.LState) int {
		rs := l.NewTable()
		rs.RawSetString("match_id", lua.LString("m.lv"))
		rs.RawSetString("set", lua.LNumber(1))
		l.Push(rs)
		return 1
	}))
	nk.RawSetString("submitRankedSet", l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LTrue)
		l.Push(lua.LString("m.lv"))
		return 2
	}))
	l.SetGlobal("nakama", nk)
	if err := l.DoString(`hook = {add = function() end, run = function() end}`); err != nil {
		t.Fatal(err)
	}
	if err := l.DoFile("../external/script/online.lua"); err != nil {
		t.Fatal(err)
	}
	l.SetGlobal("online", l.Get(-1))
	l.Pop(1)
	if err := l.DoString(fmt.Sprintf("online.rankedWaitSeconds = %g", seconds)); err != nil {
		t.Fatal(err)
	}
	return wg
}

var waitEnded = regexp.MustCompile(`the wait ended \(([a-z, ]+)\) after (\d+) frames`)

// run plays the screen after the set; it returns how it ended and after how
// many frames.
func (wg *waitGame) run(ls *lockstep, g int) (string, int, error) {
	defer func() {
		ls.mu.Lock()
		ls.done[g] = true
		ls.cond.Broadcast()
		ls.mu.Unlock()
	}()
	if err := wg.l.DoString(`online.f_submitRankedSet(); online.f_rankedSetOver()`); err != nil {
		return "", 0, err
	}
	for _, line := range wg.out {
		if m := waitEnded.FindStringSubmatch(line); m != nil {
			var n int
			fmt.Sscan(m[2], &n)
			return m[1], n, nil
		}
	}
	return "", 0, fmt.Errorf("no end of the wait in %q", wg.out)
}

func runWait(t *testing.T, d0, d1, a0, a1 int) ([2]string, [2]int) {
	ls := newLockstep(d0, d1)
	games := [2]*waitGame{newWaitGame(t, ls, 0, a0, 10), newWaitGame(t, ls, 1, a1, 10)}
	var how [2]string
	var frames [2]int
	var errs [2]error
	var wg sync.WaitGroup
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			how[g], frames[g], errs[g] = games[g].run(ls, g)
		}(g)
	}
	wg.Wait()
	for g := 0; g < 2; g++ {
		games[g].l.Close()
		if errs[g] != nil {
			t.Fatalf("game %d: %v", g, errs[g])
		}
	}
	return how, frames
}

func TestRankedWaitInStep(t *testing.T) {
	// Answers soon after the set: both leave once both bits are up.
	how, frames := runWait(t, 2, 0, 3, 40)
	if how != [2]string{"ready", "ready"} || frames[0] != frames[1] {
		t.Fatalf("%v %v", how, frames)
	}
	if frames[0] < 40 || frames[0] > 45 {
		t.Fatalf("left after %d frames", frames[0])
	}
	// No answer: both wait 600 frames.
	how, frames = runWait(t, 1, 3, -1, -1)
	if how != [2]string{"timeout", "timeout"} || frames != [2]int{600, 600} {
		t.Fatalf("%v %v", how, frames)
	}
	// One answer only: 600 frames too.
	how, frames = runWait(t, 0, 2, 10, -1)
	if how != [2]string{"timeout", "timeout"} || frames != [2]int{600, 600} {
		t.Fatalf("%v %v", how, frames)
	}
	// Any delays and arrival times: the same frame in both games.
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 60; i++ {
		d0, d1 := r.Intn(4), r.Intn(4)
		a0, a1 := r.Intn(640)-20, r.Intn(640)-20
		how, frames := runWait(t, d0, d1, a0, a1)
		if how[0] != how[1] || frames[0] != frames[1] {
			t.Fatalf("delays %d %d, answers at %d %d: %v %v", d0, d1, a0, a1, how, frames)
		}
	}
}

// The other player leaves the session: this game waits alone, for its own
// answer or until the time is up.
func TestRankedWaitAlone(t *testing.T) {
	// The other game sent its frames (no ready bit) up to frame 30, then left.
	ls := newLockstep(1, 1)
	ls.left[1], ls.written[1] = 30, 30
	game := newWaitGame(t, ls, 0, 50, 10)
	defer game.l.Close()
	how, frames, err := game.run(ls, 0)
	if err != nil {
		t.Fatal(err)
	}
	if how != "answer, alone" || frames < 50 || frames > 52 {
		t.Fatalf("%q after %d frames", how, frames)
	}
	ls = newLockstep(1, 1)
	ls.left[1], ls.written[1] = 30, 30
	game2 := newWaitGame(t, ls, 0, -1, 0.05)
	defer game2.l.Close()
	how, _, err = game2.run(ls, 0)
	if err != nil {
		t.Fatal(err)
	}
	if how != "timeout, alone" {
		t.Fatalf("%q", how)
	}
}
