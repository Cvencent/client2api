//go:build !windows

package zcode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

// browserCandidates lists the usual Chromium-family install locations on
// macOS and the PATH names on Linux.  The Windows installers are the supported
// target; these exist so a development machine behaves the same way.
func browserCandidates() []string {
	if runtime.GOOS == "darwin" {
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			filepath.Join(os.Getenv("HOME"), "Applications", "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
		}
	}
	return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"}
}

// configureBrowserCommand puts the browser in its own process group so the
// whole tree can be killed when the mint finishes.
func configureBrowserCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killBrowserTree(pid int, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	_ = logf
	if err := syscall.Kill(-pid, syscall.SIGKILL); err == nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// guardBrowserWindow is a no-op off Windows: the off-screen placement comes
// from --window-position, and there is no equivalent of the Win32 clamp that
// makes it necessary to re-assert the position there.
func guardBrowserWindow(string, <-chan struct{}, func(string, ...any)) {}

// killBrowserProfile mirrors the Windows cleanup entry point.  Off Windows
// the process group kill in killBrowserTree already covers relaunches, so
// this stays a no-op.
func killBrowserProfile(string, func(string, ...any)) {}
