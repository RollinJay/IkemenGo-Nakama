//go:build android

package main

import (
	"sync/atomic"

	"github.com/veandco/go-sdl2/sdl"
)

// keyboardRequested is set by the Android app's keyboard button, on the app's
// own thread (nativeShowKeyboard).
var keyboardRequested atomic.Bool

// confirmDown and cancelDown remember whether a controller held A or Start,
// and Back, at the last frame, so that only a new press counts.
var confirmDown, cancelDown bool

// screenKeyboardOpen shows the on-screen keyboard for a text box that opened,
// and tells the app, which shows its keyboard button meanwhile.
func screenKeyboardOpen() {
	showScreenKeyboard()
	keyboardRequested.Store(false)
	confirmDown = controllerButtonDown(sdl.CONTROLLER_BUTTON_A, sdl.CONTROLLER_BUTTON_START)
	cancelDown = controllerButtonDown(sdl.CONTROLLER_BUTTON_BACK)
	sendToApp(appMsgTextEntry, 1)
}

func screenKeyboardClose() {
	sdl.StopTextInput()
	sendToApp(appMsgTextEntry, 0)
}

// screenKeyboardUpdate works a text box from controllers, the touch controls
// among them, while the keyboard is closed: A or Start, like the app's
// keyboard button, opens the keyboard again, and Back cancels the box, as
// Escape does. (A TV remote is not a controller: it presses the app's
// buttons.)
func screenKeyboardUpdate() {
	down := controllerButtonDown(sdl.CONTROLLER_BUTTON_A, sdl.CONTROLLER_BUTTON_START)
	confirmed := down && !confirmDown
	confirmDown = down
	down = controllerButtonDown(sdl.CONTROLLER_BUTTON_BACK)
	cancelled := down && !cancelDown
	cancelDown = down
	if cancelled {
		sys.esc = true
		return
	}
	if keyboardRequested.Swap(false) || confirmed {
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

// controllerButtonDown reports whether any controller, the touch controls
// included, holds one of buttons.
func controllerButtonDown(buttons ...sdl.GameControllerButton) bool {
	for i, c := range input.controllers {
		state := input.controllerstate[i]
		if c == nil || state == nil {
			continue
		}
		for _, b := range buttons {
			if state.Buttons[b] != 0 {
				return true
			}
		}
	}
	return false
}
