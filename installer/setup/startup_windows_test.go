//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The operator's "开机自动启动" choice has to survive an upgrade.  The wizard
// used to start from an unchecked box every time, so an upgrade looked like it
// had forgotten the setting: the shortcut was still there, but the next click
// on "安装" would have discarded it silently.
func TestStartupStateSurvivesAnUpgrade(t *testing.T) {
	cases := []struct {
		name     string
		stored   bool
		hasKey   bool
		shortcut bool
		want     bool
	}{
		{"first run shows unchecked", false, false, false, false},
		{"remembered on", true, true, false, true},
		{"remembered off beats a stray shortcut", false, true, true, false},
		{"existing shortcut is enough for old installs", false, false, true, true},
		{"remembered on with no shortcut still pre-checks", true, true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := startupDefault(tc.stored, tc.hasKey, tc.shortcut); got != tc.want {
				t.Fatalf("startupDefault(stored=%v, hasKey=%v, shortcut=%v) = %v, want %v",
					tc.stored, tc.hasKey, tc.shortcut, got, tc.want)
			}
		})
	}
}

func TestStartupShortcutExistsFindsTheLink(t *testing.T) {
	dir := t.TempDir()
	if startupShortcutExistsIn(dir) {
		t.Fatal("empty directory reported a startup shortcut")
	}
	link := filepath.Join(dir, startupLNK)
	if err := os.WriteFile(link, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !startupShortcutExistsIn(dir) {
		t.Fatal("startup shortcut was not detected")
	}
}

// An upgrade where the operator unticks autostart has to remove the shortcut
// the previous version created; otherwise the remembered "off" and the
// link left in the Startup folder would contradict each other.
func TestClearStartupShortcutRemovesTheLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, startupLNK)
	if err := os.WriteFile(link, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	clearStartupShortcutIn(dir)
	if startupShortcutExistsIn(dir) {
		t.Fatal("startup shortcut survived clearStartupShortcutIn")
	}
}
