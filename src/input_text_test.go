package main

import (
	"strings"
	"testing"

	"github.com/veandco/go-sdl2/sdl"
	lua "github.com/yuin/gopher-lua"
)

// withKeyState gives a test empty key and text state, restored afterwards.
func withKeyState(t *testing.T) {
	t.Helper()
	keyState, keyInput, keyString := sys.keyState, sys.keyInput, sys.keyString
	fresh, released, entry := sys.keyInputFresh, sys.keyInputReleased, sys.textEntry
	t.Cleanup(func() {
		sys.keyState, sys.keyInput, sys.keyString = keyState, keyInput, keyString
		sys.keyInputFresh, sys.keyInputReleased, sys.textEntry = fresh, released, entry
	})
	sys.keyState = make(map[Key]bool)
	sys.keyInput, sys.keyString = KeyUnknown, ""
	sys.keyInputFresh, sys.keyInputReleased = false, false
	sys.textEntry = textEntry{}
}

// typeKey sends a key as Android's on-screen keyboard does: its press, its
// text and its release, all in one event poll.
func typeKey(key Key, text string) {
	OnKeyPressed(key, 0)
	if text != "" {
		OnTextEntered(text)
	}
	OnKeyReleased(key, 0)
}

func TestKeyPressedAndReleasedInOnePollReachesLua(t *testing.T) {
	withKeyState(t)
	sys.beginKeyPoll()
	typeKey(sdl.K_RETURN, "")
	if sys.keyInput != sdl.K_RETURN {
		t.Fatalf("after a press and release in one poll, keyInput = %v, want Return", sys.keyInput)
	}
	sys.beginKeyPoll()
	if sys.keyInput != KeyUnknown {
		t.Errorf("at the next poll, keyInput = %v, want none", sys.keyInput)
	}
}

func TestKeyReleasedInALaterPollGoesAtOnce(t *testing.T) {
	withKeyState(t)
	sys.beginKeyPoll()
	OnKeyPressed(sdl.K_BACKSPACE, 0)
	sys.beginKeyPoll()
	if sys.keyInput != sdl.K_BACKSPACE {
		t.Fatalf("with the key held, keyInput = %v, want Backspace", sys.keyInput)
	}
	OnKeyReleased(sdl.K_BACKSPACE, 0)
	if sys.keyInput != KeyUnknown {
		t.Errorf("after its release in a later poll, keyInput = %v, want none", sys.keyInput)
	}
}

func TestTypedTextQueuesUntilReset(t *testing.T) {
	withKeyState(t)
	l := lua.NewState()
	defer l.Close()
	systemScriptInit(l)
	sys.beginKeyPoll()
	typeKey(sdl.K_a, "a")
	typeKey(sdl.K_b, "b")
	OnTextEntered("é") // outside ASCII: text with no key press
	if err := l.DoString(`typed = getKeyText(); resetKey(); after = getKeyText()`); err != nil {
		t.Fatal(err)
	}
	if got := l.GetGlobal("typed").String(); got != "abé" {
		t.Errorf("getKeyText() = %q, want %q", got, "abé")
	}
	if got := l.GetGlobal("after").String(); got != "" {
		t.Errorf("getKeyText() after resetKey() = %q, want nothing", got)
	}
	if !sys.textEntry.open {
		t.Error("reading typed text did not open a text box")
	}
}

func TestTypedTextIsCapped(t *testing.T) {
	withKeyState(t)
	OnTextEntered(strings.Repeat("x", maxKeyString))
	OnTextEntered("y")
	if len(sys.keyString) != maxKeyString || strings.Contains(sys.keyString, "y") {
		t.Errorf("queued %d bytes, want the first %d", len(sys.keyString), maxKeyString)
	}
}

func TestTextEntryLastsWhileLuaReadsText(t *testing.T) {
	withKeyState(t)
	entry := &sys.textEntry
	entry.read()
	entry.update()
	// Frames that handle a key such as Backspace skip the read.
	entry.update()
	entry.read()
	entry.update()
	for i := 1; i < textEntryIdleFrames; i++ {
		entry.update()
		if !entry.open {
			t.Fatalf("closed after %d frames without a read, want %d", i, textEntryIdleFrames)
		}
	}
	entry.update()
	if entry.open {
		t.Errorf("still open after %d frames without a read", textEntryIdleFrames)
	}
	entry.read()
	if !entry.open {
		t.Error("a new read did not open the text box again")
	}
}

func TestMenuKeysFollowTheScreenpack(t *testing.T) {
	var punch, kick, start, menu, left [14]bool
	punch[7] = true  // x
	kick[4] = true   // a
	start[10] = true // s
	menu[13] = true  // m
	left[2] = true   // L, a direction
	hdbzDone, hdbzCancel := []string{"x", "y", "z", "m"}, []string{"a", "b", "c"}
	defaultDone, defaultCancel := []string{"a", "b", "c", "x", "y", "z", "s"}, []string{"m"}
	for _, c := range []struct {
		name  string
		state [14]bool
		keys  []string
		want  bool
	}{
		{"HDBZ: a punch confirms", punch, hdbzDone, true},
		{"HDBZ: a kick does not confirm", kick, hdbzDone, false},
		{"HDBZ: a kick goes back", kick, hdbzCancel, true},
		{"HDBZ: Menu confirms", menu, hdbzDone, true},
		{"default: Start confirms", start, defaultDone, true},
		{"default: a kick confirms", kick, defaultDone, true},
		{"default: Menu goes back", menu, defaultCancel, true},
		{"default: a punch does not go back", punch, defaultCancel, false},
		{"a direction is no button", left, []string{"B", "b", "L"}, false},
		{"spaces around a key", punch, []string{" x "}, true},
	} {
		if got := menuKeyHeld(c.state, c.keys); got != c.want {
			t.Errorf("%s: menuKeyHeld = %v, want %v", c.name, got, c.want)
		}
	}
}
