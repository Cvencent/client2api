//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateShortcutPersistsTargetArgumentsAndWorkingDir guards the COM vtable
// layout in shortcut_windows.go.  That bug is nasty because it does not fail:
// a shifted vtable still writes a .lnk file and the install reports success,
// but the link points at the install directory instead of the exe and carries
// no arguments.  Reading the raw file keeps the check independent of the very
// struct that is under test.
func TestCreateShortcutPersistsTargetArgumentsAndWorkingDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "appdir", "client2api.exe")
	work := filepath.Join(dir, "workdir")
	icon := filepath.Join(dir, "appdir", "client2api.ico")
	args := `-config "` + filepath.Join(work, "configs", "client2api.json") +
		`" -data-dir "` + filepath.Join(work, "data") + `"`
	link := filepath.Join(dir, "client2api.lnk")

	// The shell only records the full target path once the file exists and it
	// can turn it into an item list; a real install always has the payload on
	// disk by the time the shortcuts are written, so mirror that here.
	for _, f := range []string{target, icon} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := createShortcut(link, target, args, work, icon, "probe"); err != nil {
		t.Fatalf("createShortcut: %v", err)
	}
	raw, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("shortcut file is empty")
	}

	for _, want := range []struct{ what, text string }{
		{"target", target},
		{"arguments", args},
		{"working directory", work},
		{"icon", icon},
	} {
		if !linkContains(raw, want.text) {
			t.Errorf("shortcut does not carry the %s %q", want.what, want.text)
		}
	}
}

// linkContains reports whether the raw bytes of a .lnk hold s, in either the
// narrow or the UTF-16LE encoding the shell picks for string data.
func linkContains(raw []byte, s string) bool {
	if bytes.Contains(raw, []byte(s)) {
		return true
	}
	wide := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r > 0xffff {
			return false
		}
		wide = append(wide, byte(r), byte(r>>8))
	}
	return bytes.Contains(raw, wide)
}
