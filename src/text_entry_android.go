//go:build android

package main

import (
	"sync/atomic"

	"github.com/veandco/go-sdl2/sdl"
)

// keyboardRequested is set by the Android app's keyboard button, on the app's
// own thread (nativeShowKeyboard).
var keyboardRequested atomic.Bool

// confirmDown and cancelDown remember whether a controller held one of the
// title menu's done keys, and one of its cancel keys, at the last frame, so
// that only a new press counts. cancelPressed is a cancel key that went down
// while the box was open, and cancels it as it comes up.
var confirmDown, cancelDown, cancelPressed bool

// screenKeyboardOpen shows the on-screen keyboard for a text box that opened,
// and tells the app, which shows its keyboard button meanwhile.
func screenKeyboardOpen() {
	showScreenKeyboard()
	keyboardRequested.Store(false)
	confirmDown, cancelDown = textBoxButtons()
	cancelPressed = false
	sendToApp(appMsgTextEntry, 1)
}

func screenKeyboardClose() {
	sdl.StopTextInput()
	sendToApp(appMsgTextEntry, 0)
}

// screenKeyboardUpdate works a text box from controllers, the touch controls
// among them, with the buttons that confirm and go back in the game's menus.
// While the keyboard is closed, the title menu's done keys open it again, as
// the app's keyboard button does: by default any attack button or Start, in
// HDBZ a punch. Its cancel keys cancel the box, as Escape does: by default
// Menu, in HDBZ a kick. (A TV remote is not a controller: it presses the
// app's buttons.)
//
// A cancel key cancels as it comes up, not as it goes down. The menus under
// the box carry on in the same frame once it closes, and each would take the
// press as its own back button: one kick would go back through every menu.
func screenKeyboardUpdate() {
	done, cancel := textBoxButtons()
	confirmed := done && !confirmDown
	if cancel && !cancelDown {
		cancelPressed = true
	}
	cancelled := cancelPressed && !cancel
	confirmDown, cancelDown = done, cancel
	if cancelled {
		cancelPressed = false
		sys.esc = true
		return
	}
	if keyboardRequested.Swap(false) || confirmed {
		if !sdl.IsTextInputActive() {
			showScreenKeyboard()
		}
	}
}

// textBoxButtons reports whether a controller holds one of the title menu's
// done keys, and one of its cancel keys, through its player's mapping. The
// keyboard is left out: the on-screen keyboard's letters reach the game as
// keys too (SDL simulates them), and would press the buttons mapped to them.
func textBoxButtons() (done, cancel bool) {
	menu := &sys.motif.TitleInfo.Menu
	for _, kc := range sys.joystickConfig {
		state := GetJoystickState(kc)
		done = done || menuKeyHeld(state, menu.Done.Key)
		cancel = cancel || menuKeyHeld(state, menu.Cancel.Key)
	}
	return done, cancel
}

// showScreenKeyboard starts SDL's text input, which opens the keyboard. SDL's
// text field goes at the top of the screen, which the keyboard leaves alone,
// so that Android doesn't lift the game to show it: a prompt drawn high would
// go off the top. The game draws itself above the keyboard instead
// (keyboardCover), the lobby's chat line included.
func showScreenKeyboard() {
	if w, h := sys.window.GetSize(); w > 0 && h > 0 {
		sdl.SetTextInputRect(&sdl.Rect{X: 0, Y: 0, W: int32(w), H: int32(h / 10)})
	}
	sdl.StartTextInput()
}
