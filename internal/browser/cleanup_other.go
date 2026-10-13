//go:build !windows

package browser

import "strings"

// ProfileInUse is a no-op on non-Windows hosts in this project. The profile
// directory is still safe to remove; the process scan only exists to protect a
// live browser on the Windows deployment this module targets.
func ProfileInUse(profile string) bool {
	return strings.TrimSpace(profile) == ""
}
