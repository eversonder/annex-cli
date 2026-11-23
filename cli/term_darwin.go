//go:build darwin

package cli

import "syscall"

const ioctlGet, ioctlSet = syscall.TIOCGETA, syscall.TIOCSETA
