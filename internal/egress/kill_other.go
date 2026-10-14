//go:build !windows

package egress

import (
	"os/exec"
	"syscall"
	"time"
)

func stopProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	if done == nil {
		return
	}
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	_ = cmd.Process.Kill()
}
