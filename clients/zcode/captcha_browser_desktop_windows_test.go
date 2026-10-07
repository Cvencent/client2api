//go:build windows

package zcode

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// TestStartCaptchaBrowserUsesAnIsolatedDesktop proves the browser process is
// created on the module's hidden desktop rather than the operator's desktop.
// A child of the test binary reports which desktop it inherited; if the launch
// path falls back to the normal desktop, that report names "Default".
func TestStartCaptchaBrowserUsesAnIsolatedDesktop(t *testing.T) {
	const (
		helperEnv = "ZCODE_CAPTCHA_DESKTOP_HELPER"
		reportEnv = "ZCODE_CAPTCHA_DESKTOP_REPORT"
	)
	if os.Getenv(helperEnv) == "1" {
		got, err := currentThreadDesktopName()
		if err != nil {
			_ = os.WriteFile(os.Getenv(reportEnv), []byte("error: "+err.Error()), 0o600)
			os.Exit(2)
		}
		_ = os.WriteFile(os.Getenv(reportEnv), []byte(got), 0o600)
		os.Exit(0)
	}

	report := filepath.Join(t.TempDir(), "desktop.txt")
	t.Setenv(helperEnv, "1")
	t.Setenv(reportEnv, report)
	proc, err := startCaptchaBrowser(os.Args[0], []string{"-test.run=^TestStartCaptchaBrowserUsesAnIsolatedDesktop$"})
	if err != nil {
		t.Fatalf("startCaptchaBrowser: %v", err)
	}
	waitErr := proc.Wait()
	raw, readErr := os.ReadFile(report)
	if waitErr != nil {
		t.Fatalf("child did not inherit the hidden desktop: %v (%s)", waitErr, raw)
	}
	if readErr != nil {
		t.Fatalf("read child desktop: %v", readErr)
	}
	if got := string(raw); got != captchaBrowserDesktopName {
		t.Fatalf("child desktop = %q, want %q", got, captchaBrowserDesktopName)
	}
}

func currentThreadDesktopName() (string, error) {
	const uoiName = 2

	user32 := syscall.NewLazyDLL("user32.dll")
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getCurrentThreadID := kernel32.NewProc("GetCurrentThreadId")
	getThreadDesktop := user32.NewProc("GetThreadDesktop")
	getUserObjectInformationW := user32.NewProc("GetUserObjectInformationW")

	tid, _, _ := getCurrentThreadID.Call()
	h, _, _ := getThreadDesktop.Call(tid)
	if h == 0 {
		return "", syscall.GetLastError()
	}

	var needed uint32
	ok, _, callErr := getUserObjectInformationW.Call(
		h,
		uoiName,
		0,
		0,
		uintptr(unsafe.Pointer(&needed)),
	)
	if needed == 0 {
		return "", callErr
	}
	buf := make([]uint16, (needed+1)/2)
	ok, _, callErr = getUserObjectInformationW.Call(
		h,
		uoiName,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(needed),
		uintptr(unsafe.Pointer(&needed)),
	)
	if ok == 0 {
		return "", callErr
	}
	return syscall.UTF16ToString(buf), nil
}
