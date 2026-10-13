package browser

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCleanupStaleProfilesRemovesOnlyOldIdleDirs pins the safe half of the
// startup sweep: an old profile with no owner is reclaimable, but a recent one
// and a directory whose name is not one of ours are not collateral damage.
func TestCleanupStaleProfilesRemovesOnlyOldIdleDirs(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "prof-old")
	recent := filepath.Join(root, "prof-recent")
	other := filepath.Join(root, "keep-me")
	for _, dir := range []string{old, recent, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	removed, err := CleanupStaleProfiles(root, 24*time.Hour, nil)
	if err != nil {
		t.Fatalf("CleanupStaleProfiles: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed %d profiles, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old profile still exists: %v", err)
	}
	for _, dir := range []string{recent, other} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was removed: %v", dir, err)
		}
	}
}

// TestCleanupStaleProfilesSkipsInUseDirs keeps the startup sweep from killing a
// browser that is still running under one of its own profiles.
func TestCleanupStaleProfilesSkipsInUseDirs(t *testing.T) {
	root := t.TempDir()
	inUse := filepath.Join(root, "prof-in-use")
	if err := os.MkdirAll(inUse, 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(inUse, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	inUseCheck := func(path string) bool { return path == inUse }
	removed, err := CleanupStaleProfiles(root, 24*time.Hour, inUseCheck)
	if err != nil {
		t.Fatalf("CleanupStaleProfiles: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed %d in-use profiles, want 0", removed)
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Fatalf("in-use profile was removed: %v", err)
	}
}
