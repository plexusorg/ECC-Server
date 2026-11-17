package main

import (
	"errors"
	"syscall"
)

func installOwnerExited(pid int) bool {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER means that this PID does not exist. Access denied is not evidence of exit.
		return errors.Is(err, syscall.Errno(87))
	}
	defer syscall.CloseHandle(h)
	status, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && status == syscall.WAIT_OBJECT_0
}
