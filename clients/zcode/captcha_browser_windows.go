//go:build windows

package zcode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// browserCandidates lists where Windows keeps Edge and Chrome.  Edge ships
// with Windows 10 and 11, which is why the JWT channel can work out of the box
// without bundling a runtime.
func browserCandidates() []string {
	var out []string
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		root := strings.TrimSpace(os.Getenv(env))
		if root == "" {
			continue
		}
		out = append(out,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	return append(out, "msedge.exe", "chrome.exe")
}

// configureBrowserCommand leaves the window creation policy at the OS default:
// the browser window is real, only moved off-screen by an argument.
func configureBrowserCommand(*exec.Cmd) {}

// killBrowserTree closes the throwaway profile's whole process tree.  The
// unique --user-data-dir is the safety boundary: the operator's own browser
// windows belong to a different tree, so /T cannot reach them.
func killBrowserTree(pid int, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err == nil || logf == nil {
		return
	}
	// Edge often exits by itself as soon as the page has reported.  taskkill
	// then answers 128 ("process not found"), which is success for this
	// cleanup rather than a problem worth a log line.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 128 {
		return
	}
	msg := strings.TrimSpace(string(out))
	logf("zcode: could not close the captcha browser (pid %d): %v %s", pid, err, msg)
}
