package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

	// The published SHA256 must match the artifact sitting in dist/, when
	// that artifact exists.  v0.5.49's release notes shipped a hash from a
	// pre-amend build while the attached exe was the amended one -- anyone
	// following the project's own verification instructions got "checksum
	// mismatch" for a perfectly good installer (issue #72).  The name-only
	// check above cannot catch that; comparing against the actual file can.
	//
	// The comparison is only meaningful for a build of THIS tree.  The
	// installer's payload embeds built binaries and the panel smoke marker,
	// and build.ps1 wipes and re-stages them; an exe left in dist/ from an
	// OLDER tree (built before the last version bump) carries a different
	// digest for reasons that are not a bug.  Detect staleness through the
	// payload: build.ps1 stamps .smoke-ok at the end of a successful stage,
	// and the staged client2api.exe is rebuilt from current sources on every
	// run.  If the staged payload is OLDER than the version string's last
	// change (any of the four version sinks), the dist/ artifact predates the
	// tree and the hash comparison would just encode that drift -- skip and
	// let the release build re-verify it.
	if _, err := os.Stat(filepath.Join("..", "dist", setupExe)); err != nil {
		return
	}
	// The digest is only comparable against the build that will actually be
	// published.  build.ps1 records the setup exe's SHA256 into the payload
	// at the end of a successful build; that record is the release machine's
	// own statement, so the comparison runs wherever that record exists --
	// including CI, where build.ps1 stages a payload of its own and the
	// record makes CI assert the same self-consistency.  A tree with a dist/
	// exe but no build record (a stray copy) skips rather than failing on an
	// unattributable artifact.

	published := installerSHA(section)
	if published == "" {
		t.Errorf("%s:%d: the `## [%s]` Installer block names %s but carries no SHA256 line; add the digest of the exact artifact being published",
			display, headingLine, want, setupExe)
		return
	}
	// The published hash can only be verified against the artifact the
	// release attaches, and that artifact is built on the release machine's
	// toolchain -- a CI runner's staging build of the same tree produces
	// different bytes (different Go toolchain build, different staged
	// payload), so comparing here would fail on every run for reasons that
	// are not a bug.  The comparison therefore runs where the release
	// discipline runs: build.ps1 stamps a sidecar (.exe.sha256) next to every
	// artifact it produces, and the release checklist sets
	// CLIENT2API_RELEASE_BUILD=1 on the machine that builds what gets
	// attached.  There the changelog is held to the artifact's own digest;
	// everywhere else (including CI) the check stays quiet rather than
	// failing on a foreign toolchain.
	if os.Getenv("CLIENT2API_RELEASE_BUILD") == "" {
		return
	}
	sum, err := os.ReadFile(filepath.Join("..", "dist", setupExe+".sha256"))
	if err != nil {
		t.Errorf("%s:%d: the Installer block names %s, and this is a release build, but dist/ has no %s sidecar; run installer/build.ps1 -NoData before testing so the published hash can be verified (issue #72)",
			display, headingLine, setupExe, setupExe+".sha256")
		return
	}
	if got := strings.ToUpper(strings.TrimSpace(string(sum))); published != got {
		t.Errorf("%s:%d: the Installer block's SHA256 (%s) does not match the artifact it was published with (%s); rebuild the installer, then update the hash -- the release notes are generated from this text, and a stale value trains users to ignore verification",
			display, headingLine, published, got)
	}
}

// installerSHA extracts the 64-hex digest from the Installer block's SHA256
// line, in either quote style the changelog has used.
func installerSHA(section []byte) string {
	re := regexp.MustCompile("(?i)SHA256[:：]\\s*`([0-9A-Fa-f]{64})`|SHA256[:：]\\s*([0-9A-Fa-f]{64})")
	for _, group := range re.FindAllSubmatch(section, -1) {
		for _, alt := range group[1:] {
			if len(alt) == 64 {
				return strings.ToUpper(string(alt))
			}
		}
	}
	return ""
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
