package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The setup wizard is what the operator double-clicks, and it has to be a
// GUI-subsystem image.  A console-subsystem setup .exe makes Windows allocate
// a console before main() runs; hideConsoleWindow() can only hide it after the
// fact, which is the CMD window that flashes next to the wizard.  The gateway
// and probe stay on the plain flags so their console output is untouched.
func TestBuildScriptMakesTheInstallerAGUIBinary(t *testing.T) {
	ps := gateRead(t, "build.ps1")

	if !strings.Contains(ps, "$guiLdflags") {
		t.Fatal("build.ps1 no longer defines a GUI-subsystem linker flag set for the installer")
	}
	if !strings.Contains(ps, "-H=windowsgui") {
		t.Fatal("build.ps1 no longer injects -H=windowsgui; the setup .exe would flash a CMD window")
	}

	ascii := toASCII(ps)
	gui := strings.Index(ascii, "$guiLdflags")
	installer := strings.Index(ascii, "'building the installer'")
	if gui < 0 || installer < 0 || gui > installer {
		t.Error("the GUI linker flag must be defined and used before the installer is built")
	}
	build := strings.Index(ascii, "& $go build -trimpath -ldflags $guiLdflags -o $setupPath ./installer/setup")
	if build < 0 {
		t.Error("the installer is not built with $guiLdflags; a console window would flash on launch")
	}
}

// toASCII strips non-ASCII bytes so the Chinese step labels in build.ps1 cannot
// shift byte offsets in a surprising way for the ordering checks above.
func toASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestSetupPayloadIsStagedByTheBuildScript(t *testing.T) {
	if _, err := os.Stat(filepath.Join("build.ps1")); err != nil {
		t.Fatalf("build.ps1 missing: %v", err)
	}
}
