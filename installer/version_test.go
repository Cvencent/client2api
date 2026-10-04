package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// cmd/client2api/main.go is the single source of truth for the version; the
	// Makefile and the release workflow parse the same line.
	sourceVersionPattern = regexp.MustCompile(`(?m)^var version = "([^"]+)"`)
	// The installer's packaging default that must track it.
	setupVersionPattern = regexp.MustCompile(`(?m)^var version = "([^"]+)"`)
)

// TestInstallerDefaultTracksTheSourceVersion pins the version literal in
// installer/setup/main.go to cmd/client2api/main.go.  build.ps1 always forwards
// the real value with -X, so a drift here would only surface when someone
// builds the installer by hand -- exactly the case that would otherwise ship a
// setup .exe labelled with a stale version.
func TestInstallerDefaultTracksTheSourceVersion(t *testing.T) {
	mainGo, err := os.ReadFile(filepath.Join("..", "cmd", "client2api", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/client2api/main.go: %v", err)
	}
	want := matchOrFail(t, sourceVersionPattern, mainGo, `cmd/client2api/main.go has no var version declaration`)

	setup, err := os.ReadFile(filepath.Join("setup", "main.go"))
	if err != nil {
		t.Fatalf("read installer/setup/main.go: %v", err)
	}
	got := matchOrFail(t, setupVersionPattern, setup, `installer/setup/main.go has no var version default`)

	if got != want {
		t.Errorf("installer/setup/main.go version default = %q, cmd/client2api/main.go = %q", got, want)
	}
}

// TestEmbeddedPayloadDirectoryExists guards the //go:embed directive: without a
// file in installer/setup/payload the package does not compile at all, and the
// failure message ("cannot embed directory payload: contains no embeddable
// files") does not point at the fix.
func TestEmbeddedPayloadDirectoryExists(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("setup", "payload"))
	if err != nil {
		t.Fatalf("read installer/setup/payload: %v", err)
	}
	// A freshly staged payload has real files; a fresh checkout has only the
	// placeholder.  Either satisfies go:embed.
	if len(entries) == 0 {
		t.Fatal("installer/setup/payload is empty; //go:embed all:payload needs at least the .gitkeep placeholder")
	}
}

// TestBuildScriptStagesEveryPayloadInput keeps build.ps1 and the repo from
// drifting apart: renaming one of these inputs without updating the script
// would otherwise ship an installer that is missing a file it advertises.
func TestBuildScriptStagesEveryPayloadInput(t *testing.T) {
	script, err := os.ReadFile("build.ps1")
	if err != nil {
		t.Fatalf("read installer/build.ps1: %v", err)
	}
	body := string(script)

	required := []string{
		"client2api.exe",
		"probe.exe",
		"README.md",
		"LICENSE",
		"client2api.ico",
		"client2api.json",
		"client2api.example.json",
		"./cmd/client2api",
		"./cmd/probe",
		"./installer/setup",
	}
	for _, name := range required {
		if !strings.Contains(body, name) {
			t.Errorf("installer/build.ps1 never mentions %s, which the installer payload depends on", name)
		}
	}
}

// TestIconGeneratorOutputExists makes sure the icon the resource script embeds
// is actually checked in; windres fails with a bare "file not found" otherwise.
func TestIconGeneratorOutputExists(t *testing.T) {
	info, err := os.Stat("client2api.ico")
	if err != nil {
		t.Fatalf("client2api.ico missing; run: go run ./installer/icongen -out installer/client2api.ico: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("client2api.ico is empty")
	}
}

func matchOrFail(t *testing.T, pattern *regexp.Regexp, data []byte, message string) string {
	t.Helper()
	match := pattern.FindSubmatch(data)
	if match == nil {
		t.Fatal(message)
	}
	return string(match[1])
}
