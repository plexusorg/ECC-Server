package main

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

func childAttr() *syscall.SysProcAttr { return nil }

// ownChildren puts the host in a job object that kills its processes when the host's last handle closes, so a host
// stopped any way never leaves llama-server running. Children inherit the job.
func ownChildren() error {
	k := syscall.NewLazyDLL("kernel32.dll")
	job, _, err := k.NewProc("CreateJobObjectW").Call(0, 0)
	if job == 0 {
		return fmt.Errorf("CreateJobObject: %w", err)
	}
	// JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var info struct {
		PerProcessUserTimeLimit int64
		PerJobUserTimeLimit     int64
		LimitFlags              uint32
		MinimumWorkingSetSize   uintptr
		MaximumWorkingSetSize   uintptr
		ActiveProcessLimit      uint32
		Affinity                uintptr
		PriorityClass           uint32
		SchedulingClass         uint32
		IoCounters              [6]uint64
		ProcessMemoryLimit      uintptr
		JobMemoryLimit          uintptr
		PeakProcessMemoryUsed   uintptr
		PeakJobMemoryUsed       uintptr
	}
	const jobObjectExtendedLimitInformation = 9
	const jobObjectLimitKillOnJobClose = 0x2000
	info.LimitFlags = jobObjectLimitKillOnJobClose
	if ok, _, err := k.NewProc("SetInformationJobObject").Call(job, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); ok == 0 {
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}
	self, _ := syscall.GetCurrentProcess()
	if ok, _, err := k.NewProc("AssignProcessToJobObject").Call(job, uintptr(self)); ok == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	return nil
}

func stopChild(cmd *exec.Cmd, exited <-chan struct{}) {
	_ = cmd.Process.Kill()
	<-exited
}
