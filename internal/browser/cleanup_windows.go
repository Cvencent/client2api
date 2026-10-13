//go:build windows

package browser

import (
	"path/filepath"
	"strings"
)

// ProfileInUse reports whether any Chromium-family process is still using the
// profile directory. The cleanup sweep calls it before deleting a stale
// directory, so a crash-resume cannot take a live browser's state with it.
func ProfileInUse(profile string) bool {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return false
	}
	profile = strings.ToLower(filepath.Clean(profile))
	for _, image := range []string{"msedge.exe", "chrome.exe", "chromium.exe"} {
		for _, pid := range browserProcessIDs(image, profile) {
			if pid != 0 {
				return true
			}
		}
	}
	return false
}
