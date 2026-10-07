//go:build windows

package zcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProcessCommandLineReadsOwnCommandLine pins the Windows handle contract
// that makes both the off-screen guard and profile cleanup work.  Passing a
// bare PID to NtQueryInformationProcess returns STATUS_INVALID_HANDLE, so the
// test must fail if the implementation regresses to that call shape.
func TestProcessCommandLineReadsOwnCommandLine(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	line, ok := processCommandLine(uintptr(os.Getpid()))
	if !ok {
		t.Fatal("processCommandLine could not read the current process")
	}
	if !strings.Contains(strings.ToLower(line), strings.ToLower(filepath.Base(exe))) {
		t.Fatalf("processCommandLine = %q, want the current executable path", line)
	}
}
func TestCaptchaProfileTokenExtractsTheProfileName(t *testing.T) {
	line := `"C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe" --user-data-dir=C:\\Temp\\zcode-captcha-1234567890 --no-first-run`
	if got := captchaProfileToken.FindString(line); got != "zcode-captcha-1234567890" {
		t.Fatalf("captchaProfileToken = %q, want the throwaway profile name", got)
	}
}
