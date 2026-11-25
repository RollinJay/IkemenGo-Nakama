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
