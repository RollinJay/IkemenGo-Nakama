//go:build !android

package main

// Only Android has an on-screen keyboard to open for a text box. Elsewhere SDL
// keeps text input on from the start.
func screenKeyboardOpen()   {}
func screenKeyboardClose()  {}
func screenKeyboardUpdate() {}
