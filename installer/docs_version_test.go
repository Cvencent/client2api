package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The version appears in a handful of hand-maintained places instead of the Go
// source alone, and only the Go ones are pinned elsewhere: version_test.go ties
// installer/setup/main.go to cmd/client2api/main.go, and build.ps1 reads the
// source literal directly.  Nothing looked at what users actually see, so the
// READMEs sat on 0.3.9 while the code and the tag had moved to 0.4.5, and the
// changelog's per-release Installer block (setup file name + SHA256) had no
// guard against a missing or mis-versioned entry either.
//
// These two checks turn that drift into a failing `go test ./installer/`, which
// the release gate runs.  They read files from the working tree only -- no git,
// no network -- so a bare CI checkout can run them.

var (
	// Tolerant on purpose: the label may be followed by a half- or full-width
	// colon and the version itself wrapped in markdown emphasis.  Only the
	// version is captured, and the match is confined to one line so a stray
	// number elsewhere in the README cannot satisfy it.
	readmeVersionPattern = regexp.MustCompile(`(?m)^(?:Current version|当前版本)[：:][^\n]*?(\d+\.\d+\.\d+)`)
	// A Keep a Changelog release heading, e.g. "## [0.4.5] - 2026-10-27".
	changelogHeadingPattern = regexp.MustCompile(`(?m)^##[ \t]+\[([^\]]+)\]`)
)

// TestReadmesAdvertiseTheSourceVersion keeps the version line in both READMEs
// equal to cmd/client2api/main.go.  The READMEs are shipped verbatim in the
// installer payload and are what a visitor reads first, so a stale number here
// is a release-page bug that no compiler can catch.
func TestReadmesAdvertiseTheSourceVersion(t *testing.T) {
	want := sourceVersion(t)

	for _, rel := range []string{
		filepath.Join("..", "README.md"),
		filepath.Join("..", "README.zh-CN.md"),
	} {
		display := filepath.ToSlash(rel)
		body, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("read %s: %v", display, err)
		}

		loc := readmeVersionPattern.FindSubmatchIndex(body)
		if loc == nil {
			t.Errorf("%s has no version marker; add a `Current version: **%s**.` / `当前版本：**%s**。` line next to the changelog link",
				display, want, want)
			continue
		}

		got := string(body[loc[2]:loc[3]])
		if got != want {
			t.Errorf("%s:%d advertises version %q, but cmd/client2api/main.go declares %q -- change that line to %s",
				display, lineOf(body, loc[0]), got, want, want)
		}
	}
}

// TestChangelogNamesTheInstallerForTheSourceVersion guards the changelog's
// Installer block.  The release notes are generated from this file, and the
// setup .exe is uploaded next to them, so a missing or mis-versioned entry is
// exactly the kind of manual slip that only shows up once the release is out.
func TestChangelogNamesTheInstallerForTheSourceVersion(t *testing.T) {
	want := sourceVersion(t)

	rel := filepath.Join("..", "CHANGELOG.md")
	display := filepath.ToSlash(rel)
	body, err := os.ReadFile(rel)
	if err != nil {
		t.Fatalf("read %s: %v", display, err)
	}

	section, headingLine, ok := changelogSection(body, want)
	if !ok {
		t.Errorf("%s has no `## [%s]` section; add the release heading (and its Installer block) before tagging v%s",
			display, want, want)
		return
	}

	setupExe := "client2api-setup-" + want + ".exe"
	if !bytes.Contains(section, []byte(setupExe)) {
		t.Errorf("%s:%d: the `## [%s]` section does not mention `%s`; add the Installer block naming the setup executable for this release",
			display, headingLine, want, setupExe)
	}
}

// sourceVersion reads the single source of truth, cmd/client2api/main.go.
func sourceVersion(t *testing.T) string {
	t.Helper()
	mainGo, err := os.ReadFile(filepath.Join("..", "cmd", "client2api", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/client2api/main.go: %v", err)
	}
	return matchOrFail(t, sourceVersionPattern, mainGo, `cmd/client2api/main.go has no var version declaration`)
}

// changelogSection returns the slice of CHANGELOG.md that makes up the release
// section for version (heading included, bounded by the next version heading or
// EOF), the heading's line number, and whether such a section exists.
func changelogSection(body []byte, version string) ([]byte, int, bool) {
	headings := changelogHeadingPattern.FindAllSubmatchIndex(body, -1)
	for i, m := range headings {
		if string(body[m[2]:m[3]]) != version {
			continue
		}
		start := m[0]
		end := len(body)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		return body[start:end], lineOf(body, start), true
	}
	return nil, 0, false
}

// lineOf reports the 1-based line number of a byte offset, so failures can point
// at the line to fix instead of the whole file.  It never hardcodes a line
// number: the offset comes from the regex match.
func lineOf(body []byte, offset int) int {
	return bytes.Count(body[:offset], []byte("\n")) + 1
}
