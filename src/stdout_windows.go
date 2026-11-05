//go:build windows
// +build windows

package main

import (
	"os"
	"runtime"
	"syscall"
)

const ATTACH_PARENT_PROCESS = ^uint32(0) // (DWORD)-1

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")
	// https://docs.microsoft.com/en-us/windows/console/attachconsole
	procAttachConsole = modkernel32.NewProc("AttachConsole")
)

// replacedStreams holds the files taken out of os.Stdin, os.Stdout and
// os.Stderr. Left to the garbage collector, each would close its handle,
// which may be the handle of the file that replaced it (a stream the game was
// started with redirected to a file or a pipe) or a number a newer handle has
// taken since.
var replacedStreams []*os.File

// setStdStream points stream, &os.Stdin, &os.Stdout or &os.Stderr, at handle h.
func setStdStream(stream **os.File, h uintptr, name string) {
	replacedStreams = append(replacedStreams, *stream)
	*stream = os.NewFile(h, name)
}

func init() {
	attachParentConsole()
	LogMessage("Ikemen, GO!")
	LogMessage("Version: %v Build Time: %v", Version, BuildTime)
	runtime.LockOSThread()
}

// attachParentConsole attaches the game, a windowed program, to the console
// of the program that started it, if it has one, for its output to show there.
func attachParentConsole() {
	syscall.Syscall(procAttachConsole.Addr(), 1, uintptr(ATTACH_PARENT_PROCESS), 0, 0)

	hout, _ := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	herr, _ := syscall.GetStdHandle(syscall.STD_ERROR_HANDLE)
	setStdStream(&os.Stdout, uintptr(hout), "/dev/stdout")
	setStdStream(&os.Stderr, uintptr(herr), "/dev/stderr")
}
