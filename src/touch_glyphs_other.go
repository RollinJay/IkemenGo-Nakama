//go:build !android

package main

// Only the Android app has on-screen buttons to show glyphs on.
func touchControlsUpdate() {}
