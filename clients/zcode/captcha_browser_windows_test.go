//go:build windows

package zcode

import (
	"testing"
)

// TestGuardBrowserWindowIsSafeForTheCurrentProcess pins that the Win32 window
// walk runs without panicking and only ever touches a window whose command
// line carries the profile it was given.  The current process owns no
// top-level window in a test binary, so this exercises the enumeration and
// the filter without moving anything.
func TestGuardBrowserWindowIsSafeForTheCurrentProcess(t *testing.T) {
	guardBrowserWindow("", nil, nil) // empty profile is the documented no-op
	guardBrowserWindow("not-a-real-profile", nil, nil)
}

// TestKillBrowserProfileIsAPinForTheProfileCleanupContract pins the entry
// point the captcha mint leans on: cleanup is addressed by the unique
// --user-data-dir, not by the process id that started the browser.  Edge
// relaunches its browser process, so the original pid names the wrong tree;
// the profile name keeps matching the real one.  A profile nothing is using
// must match zero processes and return quietly.
func TestKillBrowserProfileIsAPinForTheProfileCleanupContract(t *testing.T) {
	killBrowserProfile("not-a-real-profile", nil)
}
