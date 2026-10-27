package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One-off probes -- a panel screenshot harness, a live WorkBuddy login check --
// used to live in cmd/*-temp/ as plain `package main`.  Because cmd/ is part of
// the module's default build surface, `go vet ./...`, `go build ./...` and
// `go test ./...` all compiled them, so a dependency bump or an interface
// change broke an unmaintained program at a location unrelated to the product
// code.  The ones still worth keeping now live under tools/<name>/ behind a
// `//go:build ignore` constraint, which keeps them out of `./...` while
// `go run <file>` still works.
//
// These are the same kind of static file assertions as the packaging gate in
// this package: they cannot prove what `./...` expands to, but they stop the
// rule from silently rotting.  See tools/README.md.

// TestNoBuildableTempProbesInCmd fails if a cmd/*-temp directory holds a Go file
// that would be compiled by `go ... ./...`.  The directory itself is not
// forbidden -- local scratch is fine -- but a *buildable* file there is not.
func TestNoBuildableTempProbesInCmd(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("..", "cmd", "*-temp"))
	if err != nil {
		t.Fatalf("glob cmd/*-temp: %v", err)
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if !hasIgnoreBuildConstraint(string(body)) {
				t.Errorf("%s is part of the default build surface; move it to tools/<name>/ with a `//go:build ignore` constraint (see tools/README.md)", path)
			}
		}
	}
}

// TestRetainedProbesLiveUnderTools pins where the two harnesses kept from the
// cmd/*-temp cleanup ended up, and that they carry the constraint that takes
// them out of `./...`.  If one is deleted on purpose, delete its entry here
// too; if it is moved, update both.
func TestRetainedProbesLiveUnderTools(t *testing.T) {
	for _, rel := range []string{
		"tools/uilook/main.go",
		"tools/wb-autologin-check/main.go",
	} {
		path := filepath.Join("..", filepath.FromSlash(rel))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		if !hasIgnoreBuildConstraint(string(body)) {
			t.Errorf("%s has no `//go:build ignore`, so it is back in the default build surface", rel)
		}
	}
}

// hasIgnoreBuildConstraint reports whether src carries a build constraint that
// excludes the file from ordinary builds.  Only the region before the package
// clause counts, so prose (like this file's own comments) cannot satisfy it.
func hasIgnoreBuildConstraint(src string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			return false
		}
		if trimmed == "//go:build ignore" || trimmed == "// +build ignore" {
			return true
		}
	}
	return false
}
