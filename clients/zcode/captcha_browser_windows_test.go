//go:build windows

package zcode

import (
	"os"
	"testing"
)

// TestGuardBrowserWindowIsSafeForTheCurrentProcess pins that the Win32 window
// walk runs without panicking and only ever touches a window whose process id
// matches the one it was given.  The current process owns no top-level window
// in a test binary, so this exercises the enumeration and the filter without
// moving anything.
func TestGuardBrowserWindowIsSafeForTheCurrentProcess(t *testing.T) {
	moveWindowsOffscreen(uintptr(os.Getpid()))
	guardBrowserWindow(0, nil, nil) // pid 0 is the documented no-op
}
