package main

import (
	"strings"
	"sync/atomic"
)

// keyboardCover is how many pixels at the bottom of the window the on-screen
// keyboard covers, which the Android app reports (nativeSetKeyboardCover); 0
// elsewhere. The game draws itself in the part of the window above it
// (Window.GetScaledViewportSize), so that a text box and what is typed in it
// stay in sight, wherever a screenpack draws them.
var keyboardCover atomic.Int32

// textEntryIdleFrames is how many frames without a read of typed text end a
// text box. A text box (main.f_drawInput, the lobby's editText) reads it every
// frame, except in frames that handle a key such as Backspace.
const textEntryIdleFrames = 10

// textEntry knows whether a Lua text box is open: one is while Lua reads typed
// text (getKeyText). On Android, the on-screen keyboard opens and closes with
// it (screenKeyboardOpen and screenKeyboardClose).
type textEntry struct {
	open    bool
	wasRead bool // getKeyText ran since the last update
	idle    int  // frames since getKeyText last ran
}

// read notes that Lua read typed text.
func (t *textEntry) read() {
	t.wasRead = true
	t.idle = 0
	if !t.open {
		t.open = true
		screenKeyboardOpen()
	}
}

// update runs once per frame, before the event poll.
func (t *textEntry) update() {
	if !t.open {
		return
	}
	if t.wasRead {
		t.wasRead = false
	} else if t.idle++; t.idle >= textEntryIdleFrames {
		t.open = false
		screenKeyboardClose()
		return
	}
	screenKeyboardUpdate()
}

// menuButtonIndex gives the place of each game button a screenpack's menu
// keys name (menu.done.key and the like) in a player's 14 inputs, as
// GetJoystickState and LocalInput return them. Directions are left out.
var menuButtonIndex = map[string]int{
	"a": 4, "b": 5, "c": 6, "x": 7, "y": 8, "z": 9,
	"s": 10, "d": 11, "w": 12, "m": 13,
}

// menuKeyHeld reports whether state, a player's 14 inputs, holds one of the
// game buttons that keys names, such as a screenpack's menu.done.key. Case
// matters: "b" is a button, and "B" the back direction.
func menuKeyHeld(state [14]bool, keys []string) bool {
	for _, k := range keys {
		if i, ok := menuButtonIndex[strings.TrimSpace(k)]; ok && state[i] {
			return true
		}
	}
	return false
}
