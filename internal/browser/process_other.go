//go:build !windows

package browser

// closeBrowserProfile is a no-op off Windows.  The launched Chromium process
// remains the process tree's leader there, so the existing Process.Kill in
// Close is sufficient.
func closeBrowserProfile(string, string) {}
