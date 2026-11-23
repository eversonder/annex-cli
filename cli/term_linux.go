//go:build linux

package cli

import "syscall"

const ioctlGet, ioctlSet = syscall.TCGETS, syscall.TCSETS
