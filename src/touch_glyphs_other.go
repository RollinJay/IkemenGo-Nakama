//go:build !android

package main

// Only the Android app has on-screen buttons to show glyphs on.
func touchControlsUpdate() {}

// Nor an editor for them.
func touchEditButtons() bool { return false }
