//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize mirrors struct winsize from <sys/ioctl.h>.
type winsize struct {
	rows    uint16
	columns uint16
	xpixels uint16
	ypixels uint16
}

// terminalWidth asks the kernel how wide the terminal is. ok is false when f is
// not a terminal, which is the normal case for a redirected stream.
//
// This is the one place the tool reaches past the portable standard library.
// The alternative, golang.org/x/term, would be the project's only third-party
// dependency, for twenty lines of code.
func terminalWidth(f *os.File) (int, bool) {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 || ws.columns == 0 {
		return 0, false
	}
	return int(ws.columns), true
}
