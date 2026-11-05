//go:build windows

package main

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procAllocConsole     = modkernel32.NewProc("AllocConsole")
	procFreeConsole      = modkernel32.NewProc("FreeConsole")
	procSetConsoleTitleW = modkernel32.NewProc("SetConsoleTitleW")
)

// openServerConsole gives -server-wizard (input) and -server a console window
// of their own. The game is a windowed program, which Command Prompt and
// PowerShell do not wait for: they show their prompt again at once and read
// the keyboard alongside the wizard. A stream the game was started with
// redirected (to a file, a pipe or a device such as NUL) stays so, and when
// the one the mode depends on (the wizard's input, the server's output) is
// redirected, the mode keeps the streams it was started with. The function
// returned asks, in the mode's window, for Enter before the window closes,
// waiting at most wait when wait is positive.
func openServerConsole(title string, input bool) func(wait time.Duration) {
	// syscall holds the handles the game was started with, from before it
	// attached to the console of the program that started it.
	started := [3]windows.Handle{windows.Handle(syscall.Stdin), windows.Handle(syscall.Stdout), windows.Handle(syscall.Stderr)}
	return openServerConsoleFrom(title, input, started)
}

// openServerConsoleFrom is openServerConsole for a game started with the
// standard handles started: input, output and error.
func openServerConsoleFrom(title string, input bool, started [3]windows.Handle) func(wait time.Duration) {
	depends := started[1]
	if input {
		depends = started[0]
	}
	if redirectedStream(depends) {
		return func(time.Duration) {}
	}
	LogMessage("%s continues in a window of its own.", title)
	// A process has one console at most.
	procFreeConsole.Call()
	if r, _, _ := procAllocConsole.Call(); r == 0 {
		attachParentConsole()
		return func(time.Duration) {}
	}
	if p, err := windows.UTF16PtrFromString(title); err == nil {
		procSetConsoleTitleW.Call(uintptr(unsafe.Pointer(p)))
	}
	streams := [3]struct {
		std    uint32
		file   **os.File
		device string
		name   string
	}{
		{windows.STD_INPUT_HANDLE, &os.Stdin, "CONIN$", "/dev/stdin"},
		{windows.STD_OUTPUT_HANDLE, &os.Stdout, "CONOUT$", "/dev/stdout"},
		{windows.STD_ERROR_HANDLE, &os.Stderr, "CONOUT$", "/dev/stderr"},
	}
	for i, s := range streams {
		h := started[i]
		if !redirectedStream(h) {
			var err error
			if h, err = openConsoleDevice(s.device); err != nil {
				continue
			}
			setStdStream(s.file, uintptr(h), s.name)
		}
		windows.SetStdHandle(s.std, h)
	}
	return waitForEnter
}

// redirectedStream reports whether h, a handle the game was started with,
// leads somewhere other than a console: a file, a pipe or a device such as NUL.
func redirectedStream(h windows.Handle) bool {
	if h == 0 || h == windows.InvalidHandle {
		return false
	}
	var mode uint32
	if windows.GetConsoleMode(h, &mode) == nil {
		return false
	}
	_, err := windows.GetFileType(h)
	return err == nil
}

// openConsoleDevice opens the console's input (CONIN$) or screen (CONOUT$).
func openConsoleDevice(name string) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
}

// waitForEnter asks, in the console window, for Enter before the window
// closes with the game, waiting at most wait when wait is positive.
func waitForEnter(wait time.Duration) {
	in, errIn := openConsoleDevice("CONIN$")
	out, errOut := openConsoleDevice("CONOUT$")
	if errIn != nil || errOut != nil {
		return
	}
	message := "Press Enter to close this window."
	var timeout <-chan time.Time
	if wait > 0 {
		message = fmt.Sprintf("Press Enter to close this window, which closes by itself in %d seconds.", int(wait/time.Second))
		timeout = time.After(wait)
	}
	fmt.Fprintf(os.NewFile(uintptr(out), "CONOUT$"), "\n%s\n", message)
	entered := make(chan struct{})
	go func() {
		bufio.NewReader(os.NewFile(uintptr(in), "CONIN$")).ReadString('\n')
		close(entered)
	}()
	select {
	case <-entered:
	case <-timeout:
	}
}
