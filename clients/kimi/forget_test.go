package kimi

// The panel's delete button, end to end (accounts.go RemoveAccount).
//
// kimi's table mixes two kinds of row. The panel login this module performed
// itself (kimi-web) is a credential this module owns, so deleting it is a real
// sign-out. The rows that merely *report* state the CLI owns (`cli-login`, the
// probed `file:`/`env:` credentials) cannot be deleted -- the CLI's files are
// not ours to remove -- so they are forgotten instead, which must never become
// a way to hide a real CLI login forever.
//
// Every test here is offline: the credential locations point at temp
// directories, the "CLI" is a stub script, and the OAuth issuer is httptest.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// storedPanelToken plants a usable grant in this module's own token store, the
// way a completed device login would.
func storedPanelToken(t *testing.T, c *Client) string {
	t.Helper()
	tok := storedToken{
		AccessToken:  "tok-" + strings.Repeat("a", 24),
		RefreshToken: "ref-" + strings.Repeat("b", 24),
		ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(),
		TokenType:    "Bearer",
	}
	if err := c.saveToken(tok); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	path := c.tokenPath()
	if path == "" {
		t.Fatal("tokenPath is empty; the fixture has no DataDir")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the token file was not written: %v", err)
	}
	return path
}

// clientWithStubCLI builds a client whose CLI is an explicit stub path, so the
// `cli-login` row is live without depending on the machine's PATH.
func clientWithStubCLI(t *testing.T, extra map[string]any) (*Client, string, string) {
	t.Helper()
	isolateCredentials(t)
	dir := t.TempDir()
	bin := writeScript(t, dir, stubCLIName(), "exit 0\n")
	cfg := map[string]any{"binary": bin}
	for k, v := range extra {
		cfg[k] = v
	}
	c, dataDir := newClient(t, cfg)
	return c, dataDir, bin
}

// credsFileAt writes the on-disk shape the module accepts as CLI login
// evidence, with an expiry the caller controls so a "refresh" can be modeled.
func credsFileAt(t *testing.T, path, token, expires string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"access_token":"` + token + `","expires_at":"` + expires + `"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func accountsOrFatal(t *testing.T, c *Client) []core.AccountRecord {
	t.Helper()
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	return recs
}

func rejectsAccount(t *testing.T, recs []core.AccountRecord, id string) {
	t.Helper()
	for _, r := range recs {
		if r.ID == id {
			t.Fatalf("account %q is still listed: %+v", id, r)
		}
	}
}

// ---------------------------------------------------------------------------
// The panel login: a deletion that really deletes
// ---------------------------------------------------------------------------

func TestKimiRemoveThePanelLoginSignsOut(t *testing.T) {
	f := newFakeOAuth(t)
	c := deviceClient(t, f, nil)
	path := storedPanelToken(t, c)

	ctx := context.Background()
	findAccount(t, accountsOrFatal(t, c), webLoginID)

	if err := c.RemoveAccount(ctx, webLoginID); err != nil {
		t.Fatalf("RemoveAccount(%s): %v", webLoginID, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the stored token survived the sign-out: %v", err)
	}
	rejectsAccount(t, accountsOrFatal(t, c), webLoginID)

	// Deleting twice is not an error: the operator asked for the end state, not
	// for a specific transition.
	if err := c.RemoveAccount(ctx, webLoginID); err != nil {
		t.Fatalf("second RemoveAccount(%s): %v", webLoginID, err)
	}
}

// ---------------------------------------------------------------------------
// Rows the CLI owns: a deletion that remembers
// ---------------------------------------------------------------------------

func TestKimiDeletingTheCliRowHidesItWithoutTouchingTheCli(t *testing.T) {
	credPath := filepath.Join(t.TempDir(), "kimi-credentials.json")
	credsFileAt(t, credPath, "cli-token-1", "2099-01-02T03:04:05Z")
	c, dataDir, _ := clientWithStubCLI(t, map[string]any{"credential_files": []string{credPath}})

	ctx := context.Background()
	rows := accountsOrFatal(t, c)
	findAccount(t, rows, cliLoginID)
	findAccount(t, rows, "file:"+filepath.Base(credPath))

	if err := c.RemoveAccount(ctx, cliLoginID); err != nil {
		t.Fatalf("RemoveAccount(%s): %v", cliLoginID, err)
	}
	rejectsAccount(t, accountsOrFatal(t, c), cliLoginID)

	// The row is hidden, not destroyed: the CLI's own credential file is
	// exactly where the CLI left it.
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("this module touched the CLI's credential file: %v", err)
	}

	// The deletion is written down with the evidence that justified it, and it
	// is idempotent.
	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if st.Version != accountStateVersion {
		t.Errorf("state version = %d, want %d", st.Version, accountStateVersion)
	}
	if _, ok := st.Forgotten[cliLoginID]; !ok {
		t.Fatalf("the deletion was not remembered: %+v", st.Forgotten)
	}
	if err := c.RemoveAccount(ctx, cliLoginID); err != nil {
		t.Fatalf("second RemoveAccount(%s): %v", cliLoginID, err)
	}

	// A different CLI is now in play -- new evidence about that row -- so it
	// comes back on its own, with nobody un-forgetting it by hand.
	other := writeScript(t, t.TempDir(), stubCLIName(), "exit 0\n")
	c2 := newClientIn(t, dataDir, map[string]any{"binary": other, "credential_files": []string{credPath}})
	findAccount(t, accountsOrFatal(t, c2), cliLoginID)
}

func TestKimiForgettingACredentialRowEndsWhenTheCredentialIsRefreshed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kimi-creds.json")
	credsFileAt(t, path, "first-token", "2099-01-02T03:04:05Z")
	c, _, _ := clientWithStubCLI(t, map[string]any{"credential_files": []string{path}})

	ctx := context.Background()
	id := "file:" + filepath.Base(path)
	findAccount(t, accountsOrFatal(t, c), id)

	if err := c.RemoveAccount(ctx, id); err != nil {
		t.Fatalf("RemoveAccount(%s): %v", id, err)
	}
	// Unchanged evidence: the row stays hidden across reads.
	rejectsAccount(t, accountsOrFatal(t, c), id)
	rejectsAccount(t, accountsOrFatal(t, c), id)

	// The CLI refreshes its credential, which is new evidence about the
	// account, so the row is visible again without any operator action.  The
	// probe itself is cached for a few seconds, so the cache is dropped here
	// exactly as the TTL elapsing would do it; the deletion path invalidates
	// the cache on its own, which is why the reads above already saw the
	// deletion.
	credsFileAt(t, path, "second-token", "2099-06-07T08:09:10Z")
	c.run.invalidateCredCache()
	findAccount(t, accountsOrFatal(t, c), id)
}

func TestKimiEnablingAnAccountBringsAForgottenRowBack(t *testing.T) {
	c, _, _ := clientWithStubCLI(t, nil)
	ctx := context.Background()

	if err := c.RemoveAccount(ctx, cliLoginID); err != nil {
		t.Fatalf("RemoveAccount(%s): %v", cliLoginID, err)
	}
	rejectsAccount(t, accountsOrFatal(t, c), cliLoginID)

	if err := c.SetAccountEnabled(ctx, cliLoginID, true); err != nil {
		t.Fatalf("SetAccountEnabled(%s, true): %v", cliLoginID, err)
	}
	row := findAccount(t, accountsOrFatal(t, c), cliLoginID)
	if !row.Enabled {
		t.Errorf("row = %+v, want it enabled after an explicit enable", row)
	}
	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if _, ok := st.Forgotten[cliLoginID]; ok {
		t.Errorf("an explicit choice must drop the remembered deletion: %+v", st.Forgotten)
	}
}

func TestKimiRemoveRefusesAnUnknownAccount(t *testing.T) {
	c, _, _ := clientWithStubCLI(t, nil)

	err := c.RemoveAccount(context.Background(), "file:not-probed.json")
	if err == nil {
		t.Fatal("removing an account this module never reported must fail")
	}
	if !strings.Contains(err.Error(), "no such account") {
		t.Errorf("err = %v, want a `no such account` refusal", err)
	}
}

// ---------------------------------------------------------------------------
// The state file's own rules
// ---------------------------------------------------------------------------

func TestKimiAnOlderStateFileHidesNothing(t *testing.T) {
	c, dataDir, _ := clientWithStubCLI(t, nil)

	// A version 2 file predates `forgotten`, so a deletion key in one was put
	// there by hand or by a newer build and must not be trusted.
	body := `{"version":2,"enabled":{},"forgotten":{"cli-login":"bin=anything|assume=false|refs="}}`
	if err := os.WriteFile(filepath.Join(dataDir, accountsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if st.Forgotten != nil {
		t.Errorf("forgotten = %+v, want nil for a version 2 file", st.Forgotten)
	}
	if st.Version != 2 {
		t.Errorf("version = %d, want the file's own 2", st.Version)
	}
	findAccount(t, accountsOrFatal(t, c), cliLoginID)
}

// ---------------------------------------------------------------------------
// The login hint
// ---------------------------------------------------------------------------

func TestKimiStartLoginNamesTheAlreadyStoredAccount(t *testing.T) {
	f := newFakeOAuth(t)
	c := deviceClient(t, f, nil)
	path := storedPanelToken(t, c)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	// Signing in again replaces the token of the one panel account rather than
	// adding a second row, and the message has to say so: otherwise "I signed
	// in, where is the account?" has no answer in the UI.
	for _, want := range []string{"already stored", webLoginID, path} {
		if !strings.Contains(state.Message, want) {
			t.Errorf("message %q does not mention %q", state.Message, want)
		}
	}
}
