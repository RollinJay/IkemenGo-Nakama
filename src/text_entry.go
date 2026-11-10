package main

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
