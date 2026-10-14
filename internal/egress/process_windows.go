//go:build windows

package egress

import (
	"os/exec"
	"syscall"
)

func setHiddenProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
