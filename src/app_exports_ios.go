//go:build ios

package main

// The functions the iOS app calls (third_party/ikemen-ios's TouchControls),
// declared for it in the generated libikemen.h. This file's preamble goes into
// that header, so it holds no definitions.

import "C"

import "unsafe"

// IkemenSetAppMessageHandler gives the game the app's function, a
// void (*)(int command, int param), that takes its commands (sendToApp).
// The app calls it before SDL_main.
//
//export IkemenSetAppMessageHandler
func IkemenSetAppMessageHandler(fn unsafe.Pointer) {
	appMessageHandler = fn
}

// IkemenShowKeyboard is the app's keyboard button, shown while a text box is
// open and the on-screen keyboard closed: the next frame opens the keyboard
// again (textEntry).
//
//export IkemenShowKeyboard
func IkemenShowKeyboard() {
	keyboardRequested.Store(true)
}

// IkemenSetKeyboardCover is the app telling how much of the game's view the
// on-screen keyboard covers: covered points at the bottom of its height, 0
// once the keyboard closes. The game draws itself above it (keyboardCover).
//
//export IkemenSetKeyboardCover
func IkemenSetKeyboardCover(covered, height C.int) {
	var pixels int32
	if sys.window != nil && sys.window.Window != nil {
		_, pixels = sys.window.GetDrawablePixelSize()
	}
	keyboardCover.Store(keyboardCoverPixels(int32(covered), int32(height), pixels))
}
