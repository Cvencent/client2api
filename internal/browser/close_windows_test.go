//go:build windows

package browser

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCloseKillsTheRelaunchedBrowserTree reproduces Edge's startup shape:
// the process exec.Cmd started exits after handing the profile to a child.
// Close must kill the child too, because that child is the actual browser.
func TestCloseKillsTheRelaunchedBrowserTree(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "prof-close-test")
	inner := fmt.Sprintf(`ping -t 127.0.0.1 >nul & rem --user-data-dir=%s`, profile)

	outer := exec.Command("cmd.exe", "/D", "/C", "start", "", "/B", "cmd.exe", "/D", "/C", inner)
	if err := outer.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- outer.Wait() }()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("launcher did not exit cleanly: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = outer.Process.Kill()
		t.Fatal("launcher did not exit")
	}

	if !waitForBrowserProcess(t, profile, true, 5*time.Second) {
		killBrowserProcessesForTest(profile)
		t.Fatal("the child process did not start")
	}
	defer killBrowserProcessesForTest(profile)

	b := &Browser{cmd: outer, exe: "cmd.exe", profile: profile}
	b.Close()

	if !waitForBrowserProcess(t, profile, false, 5*time.Second) {
		t.Fatal("Close left the relaunched browser process running")
	}
}

func waitForBrowserProcess(t *testing.T, profile string, want bool, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := browserProcessRunning(profile)
		if err != nil {
			t.Fatalf("inspect processes: %v", err)
		}
		if running == want {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	running, err := browserProcessRunning(profile)
	if err != nil {
		t.Fatalf("inspect processes: %v", err)
	}
	return running == want
}

// browserProcessRunning is intentionally independent of the production
// cleanup path: the test must be able to see a leaked child even when the
// production code is still wrong.
func browserProcessRunning(profile string) (bool, error) {
	script := `$m = Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like '*` +
		strings.ReplaceAll(profile, `'`, `''`) + `*' }; if ($m) { '1' } else { '0' }`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

func killBrowserProcessesForTest(profile string) {
	script := `Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like '*` +
		strings.ReplaceAll(profile, `'`, `''`) + `*' } | ForEach-Object { taskkill /PID ` +
		`$_.ProcessId /T /F | Out-Null }`
	_ = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}
