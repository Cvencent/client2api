//go:build !windows

package main

import "syscall"

// hiddenProcessAttr is a no-op on platforms without a native window concept;
// the Windows build is the only one the restart handoff runs on.
func hiddenProcessAttr() *syscall.SysProcAttr {
	return nil
}
