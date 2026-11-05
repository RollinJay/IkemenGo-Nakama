//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRedirectedStream(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	file, err := os.Create(filepath.Join(t.TempDir(), "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	nul, err := os.OpenFile("NUL", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nul.Close()
	for _, c := range []struct {
		name string
		h    windows.Handle
		want bool
	}{
		{"pipe", windows.Handle(r.Fd()), true},
		{"file", windows.Handle(file.Fd()), true},
		{"NUL", windows.Handle(nul.Fd()), true},
		{"no handle", 0, false},
		{"invalid handle", windows.InvalidHandle, false},
	} {
		if got := redirectedStream(c.h); got != c.want {
			t.Errorf("%s: redirectedStream = %v, want %v", c.name, got, c.want)
		}
	}
}

// A mode whose main stream the game was started with redirected keeps its
// streams, and has no window to wait in.
func TestOpenServerConsoleKeepsRedirectedStreams(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	log, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	stdin, stdout, stderr := os.Stdin, os.Stdout, os.Stderr
	for _, c := range []struct {
		name    string
		input   bool
		started [3]windows.Handle
	}{
		{"-server-wizard < pipe", true, [3]windows.Handle{windows.Handle(r.Fd()), 0, 0}},
		{"-server > server.log", false, [3]windows.Handle{0, windows.Handle(log.Fd()), 0}},
	} {
		waitToClose := openServerConsoleFrom("test", c.input, c.started)
		if os.Stdin != stdin || os.Stdout != stdout || os.Stderr != stderr {
			t.Errorf("%s: the standard streams changed", c.name)
		}
		start := time.Now()
		waitToClose(time.Minute)
		if waited := time.Since(start); waited > time.Second {
			t.Errorf("%s: waited %v to close a window it does not have", c.name, waited)
		}
	}
}
