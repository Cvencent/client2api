//go:build windows

package egress

import (
	"os/exec"
	"time"
)

func stopProcess(cmd *exec.Cmd, done <-chan struct{}, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(grace):
	}
}
