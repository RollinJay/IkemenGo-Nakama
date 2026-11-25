package main

import (
	"os"
	"reflect"
	"testing"

	"github.com/yuin/gopher-lua/parse"
	"gopkg.in/ini.v1"
)

// selectInfoWith reads [Select Info] from the built-in motif, then the extra
// lines, as loadMotif assigns them.
func selectInfoWith(t *testing.T, extra ...string) Motif {
	t.Helper()
	f, err := LoadINIText(preprocessINIContent(NormalizeNewlines(string(defaultMotif))), ini.LoadOptions{
		InsensitiveSections: true, SkipUnrecognizableLines: true, PreserveSurroundedQuote: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := f.GetSection("Select Info")
	if err != nil {
		t.Fatal(err)
	}
	var m Motif
	for _, key := range sec.Keys() {
		if err := assignField(&m, parseQueryPath("select_info."+key.Name()), key.Value(), ""); err != nil {
			t.Fatalf("%s: %v", key.Name(), err)
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		if err := assignField(&m, parseQueryPath("select_info."+extra[i]), extra[i+1], ""); err != nil {
			t.Fatalf("%s: %v", extra[i], err)
		}
	}
	return m
}

// Double-tapping is off by default, so other screenpacks keep their keys.
func TestSelectSlotDoubleTapDefaultOff(t *testing.T) {
	dt := selectInfoWith(t).SelectInfo.Cell.Slot.DoubleTap
	for _, k := range dt.Key {
		if k != "" {
			t.Fatalf("cell.slot.doubletap.key = %q, want empty", dt.Key)
		}
	}
	if dt.Time != 15 {
		t.Errorf("cell.slot.doubletap.time = %d, want 15", dt.Time)
	}
}

func TestSelectSlotDoubleTapParsed(t *testing.T) {
	dt := selectInfoWith(t, "cell.slot.doubletap.key", "s", "cell.slot.doubletap.time", "12").SelectInfo.Cell.Slot.DoubleTap
	if !reflect.DeepEqual(dt.Key, []string{"s"}) || dt.Time != 12 {
		t.Errorf("doubletap = %+v, want key [s] time 12", dt)
	}
}

// The select screen scripts holding the double tap and [Codes] still parse.
func TestSelectScriptsParse(t *testing.T) {
	for _, name := range []string{"../external/script/main.lua", "../external/script/start.lua"} {
		f, err := os.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = parse.Parse(f, name)
		f.Close()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The select.def [Command] strings HDBZ and the docs use parse with the
// engine's command parser, and ResetName clears a command's progress.
func TestSelectCommandStrings(t *testing.T) {
	cl := NewCommandList(NewInputBuffer())
	for i, s := range []string{"~s, ~s", "~D, DF, F, s", "~D, DF, F, x", "/s, x+y+z"} {
		name := "selectcommand" + string(rune('1'+i))
		if err := cl.AddCommand(name, CommandSpec{Cmd: s, Time: 15, BufTime: 1}); err != nil {
			t.Errorf("%q: %v", s, err)
			continue
		}
		c := &cl.Commands[cl.Names[name]][0]
		c.curtime, c.curbuftime = 3, 1
		for j := range c.completed {
			c.completed[j] = true
		}
		cl.ResetName(name)
		if c.curtime != 0 || c.curbuftime != 0 || cl.GetState(name) {
			t.Errorf("%q: ResetName left curtime=%d buftime=%d", s, c.curtime, c.curbuftime)
		}
		for j, done := range c.completed {
			if done {
				t.Errorf("%q: step %d still complete after ResetName", s, j)
			}
		}
	}
}
