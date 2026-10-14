//go:build !windows

package egress

import "os/exec"

func setHiddenProcess(cmd *exec.Cmd) {}
