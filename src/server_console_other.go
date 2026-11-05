//go:build !windows

package main

import "time"

// openServerConsole gives -server-wizard and -server a console window of their
// own on Windows, where the game is a windowed program. Elsewhere they run in
// the terminal that started them.
func openServerConsole(title string, input bool) func(wait time.Duration) {
	return func(time.Duration) {}
}
