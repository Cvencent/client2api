package browser

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CleanupStaleProfiles removes throwaway browser profiles that were left behind
// by a crash or a forced process kill. It only touches directories created by
// Launch (the prof- prefix), only ones older than maxAge, and never one the
// caller reports as in use.
//
// The sweep is deliberately conservative: profile cleanup must never delete a
// live browser's state or an operator's own directory.
func CleanupStaleProfiles(root string, maxAge time.Duration, inUse func(string) bool) (int, error) {
	root = strings.TrimSpace(root)
	if root == "" || maxAge < 0 {
		return 0, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "prof-") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if inUse != nil && inUse(path) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}
