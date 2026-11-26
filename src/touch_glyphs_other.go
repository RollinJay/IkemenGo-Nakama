//go:build !android && !ios

package main

// Only the Android and iOS apps have on-screen buttons to show glyphs on.
func touchControlsUpdate() {}

// Nor an editor for them.
func touchEditButtons() bool { return false }
