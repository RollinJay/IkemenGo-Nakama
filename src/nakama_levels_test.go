package main

// The levels of ranked ratings (nakama/modules/ikemen_levels.lua), run with
// gopher-lua, of which the Lua of Nakama's runtime is a fork, and a stand-in
// for Nakama's runtime module: the formula, the cap, the texts, and which
// IKEMEN_LEVELS values are valid. The live tests (nakama_levels_live_test.go)
// check the module in Nakama.

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"
)

// expectedLevel is ikemen_levels.lua's level, computed the same way.
func expectedLevel(start, top, elo float64) float64 {
	v := start * math.Pow(top/start, (elo-1000)/1000)
	if v < 4503599627370496 {
		v = math.Floor(v + 0.5)
	}
	return math.Max(1, math.Min(9e15, v))
}

// groupedDigits writes a whole number with thousands separators.
func groupedDigits(v float64) string {
	s := fmt.Sprintf("%d", int64(v))
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var parts []string
	head := len(s) % 3
	if head > 0 {
		parts = append(parts, s[:head])
	}
	for i := head; i < len(s); i += 3 {
		parts = append(parts, s[i:i+3])
	}
	return sign + strings.Join(parts, ",")
}

func jsonToLua(l *lua.LState, v any) lua.LValue {
	switch v := v.(type) {
	case nil:
		return lua.LNil
	case bool:
		return lua.LBool(v)
	case float64:
		return lua.LNumber(v)
	case string:
		return lua.LString(v)
	case []any:
		t := l.NewTable()
		for _, e := range v {
			t.Append(jsonToLua(l, e))
		}
		return t
	case map[string]any:
		t := l.NewTable()
		for k, e := range v {
			t.RawSetString(k, jsonToLua(l, e))
		}
		return t
	}
	return lua.LNil
}

type levelsModule struct {
	l    *lua.LState
	mod  *lua.LTable
	logs []string
}

// loadLevels runs ikemen_levels.lua with IKEMEN_LEVELS = env (the server's
// start, which logs the settings).
func loadLevels(t *testing.T, env string) *levelsModule {
	t.Helper()
	m := &levelsModule{l: lua.NewState()}
	t.Cleanup(m.l.Close)
	m.l.PreloadModule("nakama", func(l *lua.LState) int {
		nk := l.NewTable()
		nk.RawSetString("json_decode", l.NewFunction(func(l *lua.LState) int {
			var v any
			if err := json.Unmarshal([]byte(l.CheckString(1)), &v); err != nil {
				l.RaiseError("json_decode: %v", err)
			}
			l.Push(jsonToLua(l, v))
			return 1
		}))
		for _, name := range []string{"logger_info", "logger_warn", "logger_error"} {
			name := name
			nk.RawSetString(name, l.NewFunction(func(l *lua.LState) int {
				m.logs = append(m.logs, name+": "+l.CheckString(1))
				return 0
			}))
		}
		nk.RawSetString("run_once", l.NewFunction(func(l *lua.LState) int {
			fn := l.CheckFunction(1)
			ctx := l.NewTable()
			envTable := l.NewTable()
			if env != "" {
				envTable.RawSetString("IKEMEN_LEVELS", lua.LString(env))
			}
			ctx.RawSetString("env", envTable)
			if err := l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, ctx); err != nil {
				l.RaiseError("run_once: %v", err)
			}
			return 0
		}))
		l.Push(nk)
		return 1
	})
	_, file, _, _ := runtime.Caller(0)
	fn, err := m.l.LoadFile(filepath.Join(filepath.Dir(file), "..", "nakama", "modules", "ikemen_levels.lua"))
	if err != nil {
		t.Fatal(err)
	}
	m.l.Push(fn)
	if err := m.l.PCall(0, 1, nil); err != nil {
		t.Fatal(err)
	}
	m.mod = m.l.Get(-1).(*lua.LTable)
	m.l.Pop(1)
	m.l.SetGlobal("levels", m.mod)
	ctx := m.l.NewTable()
	envTable := m.l.NewTable()
	envTable.RawSetString("IKEMEN_LEVELS", lua.LString(env))
	ctx.RawSetString("env", envTable)
	m.l.SetGlobal("ctx", ctx)
	return m
}

// call runs a Lua expression of the module (levels, ctx) and returns its value.
func (m *levelsModule) call(t *testing.T, expr string) lua.LValue {
	t.Helper()
	if err := m.l.DoString("result = " + expr); err != nil {
		t.Fatalf("%s: %v", expr, err)
	}
	return m.l.GetGlobal("result")
}

func (m *levelsModule) level(t *testing.T, game string, elo float64) (float64, string, string) {
	t.Helper()
	lv := m.call(t, fmt.Sprintf("levels.level(ctx, %q, %v)", game, elo))
	text := m.call(t, fmt.Sprintf("levels.text(ctx, %q, %v)", game, elo))
	label := m.call(t, fmt.Sprintf("levels.label(ctx, %q)", game))
	return float64(lv.(lua.LNumber)), text.String(), label.String()
}

func TestLevelsPlainElo(t *testing.T) {
	m := loadLevels(t, "")
	for elo, want := range map[float64]string{1000: "1,000", 1016.4: "1,016", 984.5: "985", 2400: "2,400", 12: "12"} {
		lv, text, label := m.level(t, "g", elo)
		if text != want || lv != math.Floor(elo+0.5) || label != "Rating" {
			t.Fatalf("Elo %v: %v %q %q", elo, lv, text, label)
		}
	}
	if len(m.logs) != 1 || !strings.Contains(m.logs[0], `plain Elo ratings, labelled "Rating" (IKEMEN_LEVELS is not set)`) {
		t.Fatalf("logs %v", m.logs)
	}
}

// The example of nakama/README.md, Levels: start 9001, top 1,000,000,000.
func TestLevelsExample(t *testing.T) {
	m := loadLevels(t, `{"default": {"start": 9001, "top": 1000000000, "label": "Power level"},
		"games": {"small": {"start": 100, "top": 10000, "label": "Rank"}, "named": {"label": " Score "}}}`)
	want := map[float64]string{
		1000: "9,001", 1016: "", 1100: "", 1500: "", 2000: "1,000,000,000", 2200: "", 600: "", 0: "",
	}
	for elo, text := range want {
		lv, got, label := m.level(t, "hdbz", elo)
		exp := expectedLevel(9001, 1e9, elo)
		if lv != exp || got != groupedDigits(exp) || (text != "" && got != text) || label != "Power level" {
			t.Fatalf("Elo %v: %v %q %q, want %v", elo, lv, got, label, exp)
		}
		t.Logf("Elo %v -> %s", elo, got)
	}
	// The README's table, roughly.
	for _, c := range []struct {
		elo      float64
		low, top float64
	}{{1100, 28000, 29000}, {1500, 2.9e6, 3.1e6}, {2200, 9.5e9, 1.1e10}, {600, 85, 95}} {
		if lv, _, _ := m.level(t, "hdbz", c.elo); lv < c.low || lv > c.top {
			t.Fatalf("Elo %v: %v, not within %v to %v", c.elo, lv, c.low, c.top)
		}
	}
	// A game of its own, and one that takes start and top from the default.
	if lv, text, label := m.level(t, "small", 2000); lv != 10000 || text != "10,000" || label != "Rank" {
		t.Fatalf("small: %v %q %q", lv, text, label)
	}
	if lv, _, label := m.level(t, "named", 2000); lv != 1e9 || label != "Score" {
		t.Fatalf("named: %v %q", lv, label)
	}
	if !strings.Contains(m.logs[0], `start 9001, top 1000000000, labelled "Power level"; game "named": start 9001, top 1000000000, labelled "Score"; game "small": start 100, top 10000, labelled "Rank"`) {
		t.Fatalf("logs %v", m.logs)
	}
}

// Levels stay whole numbers from 1 to 9,000,000,000,000,000, and their texts
// exact up to there.
func TestLevelsCap(t *testing.T) {
	m := loadLevels(t, `{"default": {"start": 8900000000000000, "top": 9000000000000000},
		"games": {"wide": {"start": 1, "top": 9000000000000000}}}`)
	for _, elo := range []float64{1000, 1016, 983.7, 1500, 1999, 2000} {
		lv, text, _ := m.level(t, "g", elo)
		exp := expectedLevel(8.9e15, 9e15, elo)
		if lv != exp || text != groupedDigits(exp) || lv > 9e15 || lv != math.Floor(lv) {
			t.Fatalf("Elo %v: %v %q, want %v %q", elo, lv, text, exp, groupedDigits(exp))
		}
	}
	if _, text, _ := m.level(t, "g", 2000); text != "9,000,000,000,000,000" {
		t.Fatalf("top: %q", text)
	}
	for _, elo := range []float64{2001, 5000, 1e6} {
		if lv, text, _ := m.level(t, "g", elo); lv != 9e15 || text != "9,000,000,000,000,000" {
			t.Fatalf("Elo %v: %v %q", elo, lv, text)
		}
	}
	for _, elo := range []float64{-1e6, -50000} {
		if lv, text, _ := m.level(t, "wide", elo); lv != 1 || text != "1" {
			t.Fatalf("Elo %v: %v %q", elo, lv, text)
		}
	}
	// Above 2^52 every number is whole: no rounding to the next even one.
	for _, elo := range []float64{1985.3, 1990, 1996.71} {
		lv, _, _ := m.level(t, "wide", elo)
		if exp := expectedLevel(1, 9e15, elo); lv != exp {
			t.Fatalf("Elo %v: %v, want %v", elo, lv, exp)
		}
	}
	for _, c := range []struct {
		expr, want string
	}{
		{"levels.format(-1239)", "-1,239"},
		{"levels.format(0)", "0"},
		{"levels.format(999)", "999"},
		{"levels.format(1000)", "1,000"},
		{"levels.format(8999999999999999)", "8,999,999,999,999,999"},
		{"levels.change_text(0)", "+0"},
		{"levels.change_text(1239)", "+1,239"},
		{"levels.change_text(-1239)", "-1,239"},
	} {
		if got := m.call(t, c.expr).String(); got != c.want {
			t.Fatalf("%s = %q, want %q", c.expr, got, c.want)
		}
	}
}

// A value that is not valid is logged, and the plain Elo ratings are shown.
func TestLevelsInvalid(t *testing.T) {
	cases := map[string]string{
		`{"default": {"start": 9001, "top": 10}}`:                        "top must be more than start",
		`{"default": {"start": 0, "top": 10}}`:                           "start must be at least 1",
		`{"default": {"start": 9001}}`:                                   "start and top go together",
		`{"default": {"start": "9001", "top": 1e9}}`:                     "start and top go together",
		`{"default": {"start": 9001, "top": 9000000000000001}}`:          "top must be at most",
		`{"default": {"start": 9001, "top": 1e9, "lable": "x"}}`:         "unknown field lable",
		`{"default": {"label": ""}}`:                                     "label must have 1 to 32 characters",
		`{"default": {"label": "123456789012345678901234567890123"}}`:    "label must have 1 to 32 characters",
		`{"default": {"label": "a\u0007b"}}`:                             "control character",
		`{"default": {"label": 5}}`:                                      "label must be text",
		`{"defaults": {}}`:                                               "unknown field defaults",
		`{"games": {"": {"label": "x"}}}`:                                "is not a game id",
		`{"games": {"g": {"start": 5, "top": 4}}}`:                       `games["g"]: top must be more than start`,
		`{"games": {"g": 5}}`:                                            `games["g"] must be an object`,
		`{"games": 5}`:                                                   "games must be an object",
		`[1, 2]`:                                                         "unknown field 1",
		`{"default": {"start": 9001, "top": 1e9, "label": "Power level"`: "not a JSON object",
		`"text"`: "not a JSON object",
	}
	cases[`{"games": {"`+strings.Repeat("x", 115)+`": {}}}`] = "is not a game id"
	for env, want := range cases {
		m := loadLevels(t, env)
		if len(m.logs) != 1 || !strings.HasPrefix(m.logs[0], "logger_error: ikemen levels: IKEMEN_LEVELS is not valid (") ||
			!strings.Contains(m.logs[0], want) || !strings.Contains(m.logs[0], `levels are the plain Elo ratings, labelled "Rating", for every game`) {
			t.Fatalf("%s: logs %v, want %q", env, m.logs, want)
		}
		if lv, text, label := m.level(t, "g", 1016); lv != 1016 || text != "1,016" || label != "Rating" {
			t.Fatalf("%s: %v %q %q", env, lv, text, label)
		}
	}
	// Valid at the limits.
	ok := loadLevels(t, `{"default": {"start": 1, "top": 9000000000000000, "label": "12345678901234567890123456789012"},
		"games": {"`+strings.Repeat("x", 114)+`": {}}}`)
	if !strings.HasPrefix(ok.logs[0], "logger_info: ikemen levels: levels are start 1, top 9000000000000000") {
		t.Fatalf("logs %v", ok.logs)
	}
}
