package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The packaging gate that caught the 0.1.5 dead-panel regression lives in three
// places, and none of them is useful on its own:
//
//   - build.ps1 runs it while the installer is being produced;
//   - installer/setup/install.go runs it before and after the files land on the
//     operator's disk, with a rollback when the second run says no;
//   - CI runs it on every push so a broken panel cannot even reach a tag.
//
// These assertions are text checks on purpose.  They cannot prove the gate
// works -- the real proof is panelsmoke starting a browser -- but they do stop
// someone from quietly deleting one of the three call sites while the other two
// keep passing.

func gateRead(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestBuildRunsThePanelSmokeTestBeforePacking pins build.ps1: it has to compile
// ./cmd/panelsmoke, ship it in the payload and run it against the staged gateway
// before the installer is built.  A build that only checks after the setup .exe
// exists is already too late.
func TestBuildRunsThePanelSmokeTestBeforePacking(t *testing.T) {
	ps := gateRead(t, "build.ps1")
	for _, want := range []string{
		"./cmd/panelsmoke",
		"'panelsmoke.exe'",
		"-exe (Join-Path $PayloadDirectory 'client2api.exe')",
		"'.smoke-ok'",
		"$SkipSmoke",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("build.ps1 no longer mentions %q", want)
		}
	}

	smoke := strings.Index(ps, "running the panel smoke test")
	installer := strings.Index(ps, "'building the installer'")
	if smoke < 0 {
		t.Fatal("build.ps1 never runs the panel smoke test")
	}
	if installer < 0 {
		t.Fatal("build.ps1 no longer builds the installer where this test expects")
	}
	if smoke > installer {
		t.Error("the panel smoke test must run before the installer is built, not after")
	}
	if marker := strings.Index(ps, "recorded build smoke hash"); marker < 0 || marker > installer {
		t.Error("the smoke-gated payload hash must be recorded before the installer is built")
	}
}

// TestInstallSelfChecksBeforeAndAfterWriting pins the ordering inside
// install(): the candidate is checked while the running service is still up, the
// rollback snapshot is taken before the program files are replaced, and the file
// that landed on disk is checked again afterwards.
func TestInstallSelfChecksBeforeAndAfterWriting(t *testing.T) {
	body := gateRead(t, filepath.Join("setup", "install.go"))

	steps := []struct {
		name string
		text string
	}{
		{"pre-install self-check", "warning, smokeErr := smokeTestPanel(candidate)"},
		{"stop the gateway", "stopRunningApp()"},
		{"rollback snapshot", "takeInstallSnapshot(dir, !opt.noData)"},
		{"write program files", "writePayload(dir, !opt.noData)"},
		{"post-install self-check", "warning, err = smokeTestPanel(filepath.Join(dir, appExe))"},
		{"rollback helper", "func rollbackInstall("},
	}
	idx := map[string]int{}
	for _, s := range steps {
		i := strings.Index(body, s.text)
		if i < 0 {
			t.Fatalf("install.go no longer contains the %s step (%q)", s.name, s.text)
		}
		idx[s.name] = i
	}

	if idx["pre-install self-check"] > idx["stop the gateway"] {
		t.Error("the pre-install self-check must run before the running service is stopped")
	}
	if idx["rollback snapshot"] > idx["write program files"] {
		t.Error("the rollback snapshot must be taken before the program files are written")
	}
	if idx["write program files"] > idx["post-install self-check"] {
		t.Error("the post-install self-check must read the files the install just wrote")
	}
}

// TestCIRunsThePanelSmokeTest pins the third call site.  Even when nobody builds
// the installer locally, a push to main or a tag must not be able to publish a
// gateway whose panel does not boot.
func TestCIRunsThePanelSmokeTest(t *testing.T) {
	wf := gateRead(t, filepath.Join("..", ".github", "workflows", "go-binaries.yml"))
	for _, want := range []string{"./cmd/panelsmoke", "panelsmoke -exe"} {
		if !strings.Contains(wf, want) {
			t.Errorf("go-binaries.yml no longer mentions %q", want)
		}
	}
}
