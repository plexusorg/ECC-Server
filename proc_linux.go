package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unsafe"
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

func termios(fd uintptr, req uintptr, t *syscall.Termios) syscall.Errno {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t)))
	return e
}

// readSecret reads one line from standard input: with the prompt and without echo from a terminal, as it is when
// piped. Echo comes back even if the prompt is interrupted.
func readSecret(prompt string) (string, error) {
	fd := os.Stdin.Fd()
	var old syscall.Termios
	if termios(fd, syscall.TCGETS, &old) != 0 {
		return readLine(os.Stdin)
	}
	hidden := old
	hidden.Lflag &^= syscall.ECHO
	fmt.Fprint(os.Stderr, prompt)
	if e := termios(fd, syscall.TCSETS, &hidden); e != 0 {
		return "", e
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		if _, ok := <-sig; ok {
			termios(fd, syscall.TCSETS, &old)
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		}
	}()
	line, err := readLine(os.Stdin)
	signal.Stop(sig)
	close(sig)
	termios(fd, syscall.TCSETS, &old)
	fmt.Fprintln(os.Stderr)
	return line, err
}

func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err == io.EOF && line != "" {
		err = nil
	}
	return strings.TrimRight(line, "\r\n"), err
}
