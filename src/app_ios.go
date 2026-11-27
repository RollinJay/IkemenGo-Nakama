//go:build ios

package main

/*
typedef void (*ikemenAppMessageFn)(int command, int param);

static void ikemenCallAppMessage(void *fn, int command, int param) {
	((ikemenAppMessageFn)fn)(command, param);
}
*/
import "C"

import (
	"os"
	"path/filepath"
	"unsafe"
)

// appMessageHandler is the app's C function that takes sendToApp's commands,
// which it gives the game before it starts (IkemenSetAppMessageHandler in
// app_exports_ios.go). The game's loop and the app's UIKit code run on the
// same thread, the main one, so the handler runs at once.
var appMessageHandler unsafe.Pointer

// sendToApp sends the app a command (app_messages.go); without a handler, as
// with an app from before them, it goes nowhere.
func sendToApp(command, param int) {
	if fn := appMessageHandler; fn != nil {
		C.ikemenCallAppMessage(fn, C.int(command), C.int(param))
	}
}

// The app hides its on-screen buttons while the game has a real controller,
// besides asking the GameController framework, whose notifications don't
// reach it while the game runs on the main queue.
const appWantsControllerCount = true

// touchGlyphsDir is where the app's on-screen buttons find their glyph
// images: Library/Caches/touch-glyphs in the app's container (Go's
// os.UserCacheDir on iOS, and NSCachesDirectory in the app).
func touchGlyphsDir() string {
	dir, err := os.UserCacheDir()
	if err != nil || dir == "" {
		return ""
	}
	return filepath.Join(dir, "touch-glyphs")
}
