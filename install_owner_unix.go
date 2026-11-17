//go:build !windows

package main

import (
	"errors"
	"syscall"
)

func installOwnerExited(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
