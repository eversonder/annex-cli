//go:build darwin || linux

package cli

import (
	"syscall"
	"unsafe"
)

func termios(fd uintptr, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// isTerminal is whether fd is a terminal a person types at.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	return termios(fd, ioctlGet, &t) == nil
}

// makeRaw hands every key to the program as it is pressed, unechoed, and
// answers how to put the terminal back. Output is left as it was, so a
// newline still starts a new line.
func makeRaw(fd uintptr) (restore func(), err error) {
	var old syscall.Termios
	if err := termios(fd, ioctlGet, &old); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.BRKINT | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.IXON
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := termios(fd, ioctlSet, &raw); err != nil {
		return nil, err
	}
	return func() { termios(fd, ioctlSet, &old) }, nil
}
