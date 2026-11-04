package main

import (
	"os/exec"
	"syscall"
	"time"
)

// childAttr has the kernel kill llama-server when the host dies, so a crashed host never leaves one running.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func ownChildren() error { return nil }

func stopChild(cmd *exec.Cmd, exited <-chan struct{}) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}
