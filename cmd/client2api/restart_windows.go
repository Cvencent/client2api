//go:build windows

package main

import "syscall"

// hiddenProcessAttr makes the replacement process invisible.  The gateway ships
// as a GUI-subsystem image, but the restart child is started while the installer
// may be mid-upgrade, and an explicit flag removes any chance of a console or
// taskbar entry flashing on the operator's desktop.
func hiddenProcessAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
