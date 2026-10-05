//go:build windows

package zcode

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
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

// configureBrowserCommand asks Windows to create the process without showing a
// window.  Chromium often ignores the flag for its own browser window, which is
// why guardBrowserWindow exists as well, but when it is honoured the operator
// never sees even a flash.
func configureBrowserCommand(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetWindowPos             = user32.NewProc("SetWindowPos")

	// The callback is registered once: syscall.NewCallback leaks a slot per
	// call, and guardBrowserWindow polls many times per mint, so building it
	// per poll would exhaust the table after a few captchas.
	guardMu  sync.Mutex
	guardPID uintptr
	guardCB  = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if uintptr(pid) != guardPID {
			return 1
		}
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		const swpNoSize, swpNoZOrder, swpNoActivate = 0x0001, 0x0004, 0x0010
		// int32 through a variable: converting a negative constant straight to
		// uintptr is a compile error, and the API reads the low 32 bits as int.
		offscreen := int32(-32000)
		procSetWindowPos.Call(hwnd, 0,
			uintptr(offscreen), uintptr(offscreen), 0, 0,
			swpNoSize|swpNoZOrder|swpNoActivate)
		return 1
	})
)

// guardBrowserWindow keeps the throwaway captcha window off the operator's
// desktop.
//
// Passing --window-position=-32000,-32000 is not enough on Windows: Chromium
// clamps its initial placement back onto the work area, so the window lands on
// screen and the operator sees one pop up on every mint (a JWT call re-mints
// every captchaParamTTL, so during a chat that is once a minute).  An external
// SetWindowPos is honoured, so this walks the process's top-level windows for a
// while and parks each one far off-screen as soon as it appears.  It runs in
// its own goroutine and never blocks the mint.
func guardBrowserWindow(pid int, stop <-chan struct{}, logf func(string, ...any)) {
	if pid <= 0 {
		return
	}
	go func() {
		_ = logf
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			moveWindowsOffscreen(uintptr(pid))
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
}

func moveWindowsOffscreen(pid uintptr) {
	guardMu.Lock()
	guardPID = pid
	_, _, _ = procEnumWindows.Call(guardCB, 0)
	guardPID = 0
	guardMu.Unlock()
}

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
