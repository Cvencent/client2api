package zcode

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProviderConfigCompletesTheTruncatedKeyFromTheCredentialStore pins the
// real desktop layout: config.json stores only the id half, while
// credentials.json holds the complete id.secret under a different account id.
// Discovery must complete the config entry, not publish a 32-character key that
// the monitor endpoint will reject.
func TestProviderConfigCompletesTheTruncatedKeyFromTheCredentialStore(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "61161790588087632"
	const complete = id + ".abcdefghijklmnop"
	cfg := `{"provider":{"builtin:bigmodel-coding-plan":{"name":"BigModel - Coding Plan","kind":"anthropic","options":{"apiKey":"` + id + `","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`
	creds := `{"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:` + id + `:api-key":"` + complete + `"}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}

	sources := discoverWithHome(t, home)
	var found *credentialSource
	for i := range sources {
		if sources[i].ID == "zcode-config:builtin:bigmodel-coding-plan" {
			found = &sources[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("config account missing; sources = %+v", sources)
	}
	if got := found.secret(); got != complete {
		t.Fatalf("config account secret length = %d, want the complete key", len(got))
	}
}

// discoverWithHome mirrors discover() but pins the home directory.
func discoverWithHome(t *testing.T, home string) []credentialSource {
	t.Helper()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	root := filepath.Join(home, ".zcode", "v2")
	credPath := filepath.Join(root, "credentials.json")
	secrets := credentialSecrets(credPath, nil)
	out := discoverFromProviderConfig(filepath.Join(root, "config.json"), secrets, nil)
	return append(out, discoverFromCredentials(credPath, nil)...)
}
