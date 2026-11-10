//go:build android

package main

import (
	"sync/atomic"

	"github.com/veandco/go-sdl2/sdl"
)

// keyboardRequested is set by the Android app's keyboard button, on the app's
// own thread (nativeShowKeyboard).
var keyboardRequested atomic.Bool

// keyboardButtonsDown remembers whether a controller held A or Start at the
// last frame, so that only a new press opens the keyboard again.
var keyboardButtonsDown bool

// screenKeyboardOpen shows the on-screen keyboard for a text box that opened,
// and tells the app, which shows its keyboard button meanwhile.
func screenKeyboardOpen() {
	showScreenKeyboard()
	keyboardRequested.Store(false)
	keyboardButtonsDown = controllerConfirmDown()
	sendToApp(appMsgTextEntry, 1)
}

func screenKeyboardClose() {
	sdl.StopTextInput()
	sendToApp(appMsgTextEntry, 0)
}

// screenKeyboardUpdate opens the keyboard again, after Back closed it, when the
// app's keyboard button asks or a controller's A or Start is pressed.
func screenKeyboardUpdate() {
	down := controllerConfirmDown()
	pressed := down && !keyboardButtonsDown
	keyboardButtonsDown = down
	if keyboardRequested.Swap(false) || pressed {
		if !sdl.IsTextInputActive() {
			showScreenKeyboard()
		}
	}
}

// showScreenKeyboard starts SDL's text input, which opens the keyboard. SDL's
// text field goes at the bottom of the screen, so that Android lifts the game
// above the keyboard rather than covering a text line drawn low, as the
// lobby's chat is.
func showScreenKeyboard() {
	if w, h := sys.window.GetSize(); w > 0 && h > 0 {
		sdl.SetTextInputRect(&sdl.Rect{X: 0, Y: int32(h - h/10), W: int32(w), H: int32(h / 10)})
	}
	sdl.StartTextInput()
}

// controllerConfirmDown reports whether any controller, the touch controls
// included, holds A or Start.
func controllerConfirmDown() bool {
	for i, c := range input.controllers {
		state := input.controllerstate[i]
		if c == nil || state == nil {
			continue
		}
		if state.Buttons[sdl.CONTROLLER_BUTTON_A] != 0 || state.Buttons[sdl.CONTROLLER_BUTTON_START] != 0 {
			return true
		}
	}
	return false
}
