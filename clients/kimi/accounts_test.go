package kimi

// Offline tests for the three optional panel capabilities (accounts.go).
//
// Every test here is hermetic: the credential locations are pointed at a temp
// directory, PATH is emptied, and the "CLI" is a stub script.  The one test that
// touches the real machine is guarded by CLIENT2API_LIVE=1 and skips by default.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// noCLIOnPath makes every binary lookup miss, so the tests see the "the CLI is
// not installed" state.
func noCLIOnPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// newClientIn builds a second client over an existing DataDir, so a test can
// prove that state written by one client is read back by the next one.
func newClientIn(t *testing.T, dataDir string, cfg map[string]any) *Client {
	t.Helper()
	var raw json.RawMessage
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	cl, err := New(core.Deps{DataDir: dataDir, Config: raw, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", cl)
	}
	return c
}

// writeCredential writes the credential file the module accepts as evidence of
// a CLI login.  The token is fake and never leaves the test.
func writeCredential(t *testing.T, path, token string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"access_token":"` + token + `","expires_at":"2099-01-02T03:04:05Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubCLIName is the file name this module treats as "the CLI".
func stubCLIName() string {
	if runtime.GOOS == "windows" {
		return "kimi.cmd"
	}
	return "kimi"
}

// stubInWorkBin plants a working stub CLI in ~/.kimi-work/bin, the location
// Discover scans but binaryPath() deliberately does not search on its own.  The
// stub is therefore reachable only through an Import binding.
func stubInWorkBin(t *testing.T, fixture string) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".kimi-work", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := fixtureFile(t, t.TempDir(), fixture)
	if runtime.GOOS == "windows" {
		return writeScript(t, dir, stubCLIName(), "@echo off\r\ntype \""+fp+"\"\r\nexit /b 0\r\n")
	}
	return writeScript(t, dir, stubCLIName(), "#!/bin/sh\n/bin/cat \""+fp+"\"\nexit 0\n")
}

func findAccount(t *testing.T, recs []core.AccountRecord, id string) core.AccountRecord {
	t.Helper()
	for _, r := range recs {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("account %q not found in %+v", id, recs)
	return core.AccountRecord{}
}

func findDiscovered(t *testing.T, ds []core.DiscoveredCredential, path string) core.DiscoveredCredential {
	t.Helper()
	for _, d := range ds {
		if bindingKey(d.Path) == bindingKey(path) {
			return d
		}
	}
	t.Fatalf("%q not found in %+v", path, ds)
	return core.DiscoveredCredential{}
}

// closeChat runs one request and closes the stream, so no child process leaks
// between assertions.
func closeChat(t *testing.T, c *Client, text string) error {
	t.Helper()
	st, err := c.Chat(context.Background(), userReq("kimi", text))
	if err != nil {
		return err
	}
	return st.Close()
}

// ---------------------------------------------------------------------------
// Capabilities and the field schema
// ---------------------------------------------------------------------------

func TestKimiAdvertisesTheThreePanelCapabilities(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Manage || !caps.Import || !caps.Login {
		t.Fatalf("capabilities = %+v, want manage+import+login all true", caps)
	}
	if len(caps.Fields) != 0 {
		t.Fatalf("capabilities.Fields = %+v, want none", caps.Fields)
	}
	if _, ok := core.AsAccountManager(c); !ok {
		t.Error("AsAccountManager failed")
	}
	if _, ok := core.AsCredentialImporter(c); !ok {
		t.Error("AsCredentialImporter failed")
	}
	if _, ok := core.AsLoginProvider(c); !ok {
		t.Error("AsLoginProvider failed")
	}
}

func TestAccountFieldsIsEmptyOnPurpose(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	fields := c.AccountFields(context.Background())
	if fields == nil {
		t.Fatal("AccountFields returned nil; the contract wants an empty slice")
	}
	if len(fields) != 0 {
		t.Fatalf("fields = %+v, want none: kimi's credential is created by `kimi login`, not by typing a value", fields)
	}
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

func TestAccountsIsHonestWhenTheCLIIsMissing(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, dir := newClient(t, nil)

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	// A CLI-less install has no CLI login to report.  Listing `cli-login` anyway
	// produced a row an operator could neither use (no CLI to run) nor delete
	// (the module must not touch the CLI's files), so it must not be invented.
	if len(recs) != 0 {
		t.Fatalf("accounts = %+v, want none: a missing CLI is not an account", recs)
	}
	// Answering the question wrote nothing.
	if _, serr := os.Stat(filepath.Join(dir, accountsFileName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("listing accounts created state: %v", serr)
	}
	// A direct removal request is still refused rather than quietly accepted.
	if err := c.RemoveAccount(context.Background(), cliLoginID); err == nil {
		t.Error("RemoveAccount(cli-login) succeeded with no CLI installed")
	}
}

// A `cli-login` disable left behind by an earlier session must not outlive the
// CLI it refers to.  The row is not listed once the CLI is gone, so honouring
// the flag would switch off a route the operator can no longer see or re-enable.
func TestStaleCLILoginDisableDoesNotLingerAfterTheCLIVanishes(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, dir := newClient(t, nil)

	// Exactly what the panel wrote back when the CLI was installed and the
	// operator turned its login off.
	const state = `{
  "version": 1,
  "enabled": {
    "cli-login": false
  }
}`
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, accountsFileName), []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == cliLoginID {
			t.Fatalf("cli-login is listed again with no CLI installed: %+v", r)
		}
	}

	// And the stale flag must not be reported as an operator disable either: the
	// honest answer is "no CLI here, sign in from the panel".
	st := c.Status(context.Background())
	if st.Ready {
		t.Error("Ready = true with no CLI and no panel login")
	}
	if strings.Contains(st.Detail, cliLoginID) {
		t.Errorf("detail = %q, want it to stop naming the absent CLI login", st.Detail)
	}
	if !strings.Contains(st.Detail, "not found") {
		t.Errorf("detail = %q, want the CLI-not-found explanation", st.Detail)
	}
}

func TestAccountsReportsTheCLILoginWithoutLeakingTheToken(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}})

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	login := findAccount(t, recs, cliLoginID)
	if !login.Enabled {
		t.Error("cli-login should default to enabled once a credential was found")
	}
	if login.State != "ready" {
		t.Errorf("state = %q, want ready", login.State)
	}
	if login.Fields["binary"] != bin {
		t.Errorf("fields = %+v, want the binary path so the panel can show what would run", login.Fields)
	}

	// The evidence row sits next to the login row.
	ev := findAccount(t, recs, "file:kimi-code.json")
	if ev.State != "ready" || !ev.Enabled {
		t.Errorf("evidence row = %+v, want enabled ready", ev)
	}

	blob, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), token) {
		t.Fatalf("Accounts leaked the token: %s", blob)
	}
}

// TestAccountsCanHideTheEvidenceRows pins the "one account, not one row per
// credential" view: with show_credential_rows off the table keeps the cli-login
// row (which still names the file in its note) and drops the per-file evidence
// rows, so a CLI-owned store stops looking like a second account.
func TestAccountsCanHideTheEvidenceRows(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{
		"binary":               bin,
		"credential_files":     []string{cred},
		"show_credential_rows": false,
	})

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	login := findAccount(t, recs, cliLoginID)
	if login.State != "ready" {
		t.Errorf("state = %q, want ready: the CLI login still summarises the credential", login.State)
	}
	if !strings.Contains(login.Note, filepath.Base(cred)) {
		t.Errorf("note = %q, want it to still name the credential file", login.Note)
	}
	for _, r := range recs {
		if strings.HasPrefix(r.ID, "file:") || strings.HasPrefix(r.ID, "env:") {
			t.Errorf("record %q was listed even though show_credential_rows is false", r.ID)
		}
	}
}

func TestAccountsLeadsWithTheCLILoginEvenWhenAnAccountIsDisabled(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	if err := c.SetAccountEnabled(context.Background(), cliLoginID, false); err != nil {
		t.Fatal(err)
	}
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := findAccount(t, recs, cliLoginID)
	if a.Enabled {
		t.Error("the operator's disabled choice was not reflected")
	}
	if a.State != "ready" {
		t.Errorf("state = %q: disabled is not the same as broken", a.State)
	}
}

// ---------------------------------------------------------------------------
// Add / Remove
// ---------------------------------------------------------------------------

func TestAddAccountIsRefusedAndWritesNothing(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, dir := newClient(t, nil)

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		ID:     "typed",
		Fields: map[string]string{"token": "sk-nope"},
	})
	if err == nil {
		t.Fatalf("AddAccount succeeded and returned %+v; kimi has no store to add to", rec)
	}
	if !strings.Contains(err.Error(), "kimi login") {
		t.Errorf("error = %v, want it to name `kimi login`", err)
	}
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("error = %v, want it to wrap core.ErrUnsupported", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, accountsFileName)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("AddAccount left state behind: %v", serr)
	}
}

func TestRemoveAccountHidesTheCLILoginWithoutTouchingIt(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}})

	if err := c.RemoveAccount(context.Background(), cliLoginID); err != nil {
		t.Fatalf("RemoveAccount(cli-login): %v", err)
	}
	// The row leaves the table...
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == cliLoginID {
			t.Fatalf("the CLI row is still listed: %+v", r)
		}
	}
	// ...while the CLI's own file is untouched, byte for byte.  This module
	// hides the row; it never deletes another program's login.
	body, rerr := os.ReadFile(cred)
	if rerr != nil {
		t.Fatalf("the credential file was deleted: %v", rerr)
	}
	if !strings.Contains(string(body), token) {
		t.Error("the credential file was rewritten")
	}
	// Asking for the same end state twice is not an error: the row is already
	// gone, which is exactly what was wanted.
	if err := c.RemoveAccount(context.Background(), cliLoginID); err != nil {
		t.Errorf("the second RemoveAccount(cli-login) failed: %v", err)
	}
	if err := c.RemoveAccount(context.Background(), "nope"); err == nil {
		t.Error("RemoveAccount accepted an unknown id")
	}
}

func TestRemoveAccountForgetsAnImportedBinding(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, nil)

	imported, err := c.Import(context.Background(), []string{stub}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported = %+v, want one binding", imported)
	}
	id := imported[0].ID

	if err := c.RemoveAccount(context.Background(), id); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.ID == id {
			t.Fatalf("the binding is still listed: %+v", r)
		}
	}
	if _, err := os.Stat(stub); err != nil {
		t.Fatalf("RemoveAccount deleted the executable itself: %v", err)
	}
	if err := c.RemoveAccount(context.Background(), id); err == nil {
		t.Error("removing the same binding twice succeeded")
	}
}

// ---------------------------------------------------------------------------
// SetAccountEnabled
// ---------------------------------------------------------------------------

func TestSetAccountEnabledPersistsAndGatesChat(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	cfg := map[string]any{"binary": bin, "credential_files": []string{cred}}
	c, dir := newClient(t, cfg)

	if err := closeChat(t, c, "baseline"); err != nil {
		t.Fatalf("baseline Chat: %v", err)
	}

	if err := c.SetAccountEnabled(context.Background(), cliLoginID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	st := c.Status(context.Background())
	if st.Ready {
		t.Error("Ready = true after the operator disabled the login")
	}
	if !strings.Contains(st.Detail, "disabled") {
		t.Errorf("detail = %q, want it to explain the disable", st.Detail)
	}

	_, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("Chat error = %v, want it to name the disable", err)
	}

	// The choice survives a fresh client over the same DataDir.
	c2 := newClientIn(t, dir, cfg)
	recs, err := c2.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if findAccount(t, recs, cliLoginID).Enabled {
		t.Error("the disabled choice did not persist to " + accountsFileName)
	}
	if err := c2.SetAccountEnabled(context.Background(), cliLoginID, true); err != nil {
		t.Fatal(err)
	}
	if err := closeChat(t, c2, "after re-enable"); err != nil {
		t.Fatalf("Chat after re-enabling: %v", err)
	}
}

func TestSetAccountEnabledRejectsUnknownAccounts(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	if err := c.SetAccountEnabled(context.Background(), "not-a-thing", false); err == nil {
		t.Error("SetAccountEnabled accepted an unknown id")
	}
	if err := c.SetAccountEnabled(context.Background(), "", false); err == nil {
		t.Error("SetAccountEnabled accepted an empty id")
	}
}

func TestDisablingABindingStopsUsingIt(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, nil)

	imported, err := c.Import(context.Background(), []string{stub}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.run.binaryPath(); err != nil || got != stub {
		t.Fatalf("binaryPath = %q, %v; want the imported stub %q", got, err, stub)
	}

	if err := c.SetAccountEnabled(context.Background(), imported[0].ID, false); err != nil {
		t.Fatal(err)
	}
	if got, err := c.run.binaryPath(); err == nil {
		t.Fatalf("binaryPath = %q, want a miss: the only binding is disabled", got)
	}
}

// ---------------------------------------------------------------------------
// TestAccount
// ---------------------------------------------------------------------------

func TestTestAccountIsHonestWhenTheCLIIsMissing(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	res, err := c.TestAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Fatalf("ok = true with no CLI: %+v", res)
	}
	if res.AccountID != cliLoginID {
		t.Errorf("account_id = %q", res.AccountID)
	}
	if !strings.Contains(res.Error, "kimi CLI not found") {
		t.Errorf("error = %q, want it to say the CLI was not found", res.Error)
	}
	if !strings.Contains(res.Error, "kimi login") {
		t.Errorf("error = %q, want it to say what to do next", res.Error)
	}
}

func TestTestAccountIsHonestWithoutALogin(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin})

	res, err := c.TestAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Fatalf("ok = true without a login: %+v", res)
	}
	if !strings.Contains(res.Error, "kimi login") {
		t.Errorf("error = %q, want the login guidance", res.Error)
	}
}

func TestTestAccountSucceedsWithALoginAndSendsNoConversation(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), "sk-abcdefghijklmnopqrstuvwxyz")

	// The stub answers like the real CLI: TestAccount must drive one real
	// completion, not just stat the login files.
	bin := stubEmitting(t, cliFixture("pong"))
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}})

	res, err := c.TestAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok = false with a CLI and a credential: %+v", res)
	}
	if res.Model != c.cfg.DefaultModel {
		t.Errorf("model = %q, want %q", res.Model, c.cfg.DefaultModel)
	}
	if !strings.Contains(res.Reply, "pong") {
		t.Errorf("reply = %q, want the CLI's real answer", res.Reply)
	}
	if !strings.Contains(res.Reply, "probe:") {
		t.Errorf("reply = %q, want it to name the probed credential", res.Reply)
	}
}

func TestTestAccountReportsADisabledAccountAndUnknownIDs(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	if err := c.SetAccountEnabled(context.Background(), cliLoginID, false); err != nil {
		t.Fatal(err)
	}
	res, err := c.TestAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Fatalf("ok = true for a disabled account: %+v", res)
	}
	if !strings.Contains(res.Error, "disabled") {
		t.Errorf("error = %q, want it to name the disable", res.Error)
	}

	if _, err := c.TestAccount(context.Background(), "nope"); err == nil {
		t.Error("TestAccount accepted an unknown id")
	}
	if _, err := c.TestAccount(context.Background(), ""); err == nil {
		t.Error("TestAccount accepted an empty id")
	}
}

// ---------------------------------------------------------------------------
// RefreshAccount
// ---------------------------------------------------------------------------

func TestRefreshAccountIsHonest(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}})

	results, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("RefreshAccount returned nothing for a non-empty account list")
	}
	for _, r := range results {
		if r.OK {
			t.Errorf("result = %+v, want ok=false: kimi has no renewal to trigger", r)
		}
		if r.AccountID == "" {
			t.Error("result without an account id")
		}
		if !strings.Contains(r.Error, "refresh") {
			t.Errorf("error = %q, want it to explain why", r.Error)
		}
	}

	one, err := c.RefreshAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatalf("RefreshAccount(cli-login): %v", err)
	}
	if len(one) != 1 || one[0].AccountID != cliLoginID {
		t.Fatalf("results = %+v, want just cli-login", one)
	}
	if _, err := c.RefreshAccount(context.Background(), "nope"); err == nil {
		t.Error("RefreshAccount accepted an unknown id")
	}
}

// ---------------------------------------------------------------------------
// Discover
// ---------------------------------------------------------------------------

func TestDiscoverReportsWhatIsThereAndWhyItCannotBeImported(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)

	// A CLI-shaped stub in ~/.kimi-work/bin, plus the desktop toolchain file
	// that lives next to it and is NOT the CLI.
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	tool := writeScript(t, filepath.Join(home, ".kimi-work", "bin"), "kimi-slides", "#!/bin/sh\nexit 0\n")

	// The webbridge artefacts.
	wb := filepath.Join(home, ".kimi-webbridge")
	if err := os.MkdirAll(filepath.Join(wb, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "identity.json"), []byte(`{"device_id":"deadbeef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "daemon.pid"), []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wbExe := filepath.Join(wb, "bin", "kimi-webbridge"+exeSuffix())
	if err := os.WriteFile(wbExe, []byte("MZ"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The desktop app's encrypted store and its model cache.
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		appdata = home
	}
	desktop := filepath.Join(appdata, "kimi-desktop")
	if err := os.MkdirAll(filepath.Join(desktop, "bridge-store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(desktop, "kimi-agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(desktop, "bridge-store", "token-store.json")
	if err := os.WriteFile(store, []byte(`{"encrypted":"blob"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(desktop, "kimi-agent", "kimi-work-models-cache.json")
	if err := os.WriteFile(cache, []byte(`{"models":["k3-agent"]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _ := newClient(t, nil)
	got, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// The CLI stub is the only importable thing on this machine.
	cli := findDiscovered(t, got, stub)
	if !cli.Importable {
		t.Errorf("%s: importable = false, want true (%s)", stub, cli.Note)
	}
	if cli.Imported {
		t.Error("the stub is reported as already imported")
	}
	if cli.Kind != "cli-binary" {
		t.Errorf("kind = %q, want cli-binary", cli.Kind)
	}
	for _, d := range got {
		if d.Importable && bindingKey(d.Path) != bindingKey(stub) {
			t.Errorf("%s is importable but should not be: %s", d.Path, d.Note)
		}
	}

	// Everything else is reported, honestly, as not importable.
	for _, tc := range []struct{ path, want string }{
		{tool, "not the kimi Code CLI"},
		{filepath.Join(wb, "identity.json"), "device_id"},
		{filepath.Join(wb, "daemon.pid"), "pid file"},
		{wbExe, "webbridge daemon binary"},
		{store, "encrypt"},
		{cache, "no credential"},
	} {
		d := findDiscovered(t, got, tc.path)
		if d.Importable {
			t.Errorf("%s: importable = true, want false", tc.path)
		}
		if !strings.Contains(d.Note, tc.want) {
			t.Errorf("%s: note = %q, want it to mention %q", tc.path, d.Note, tc.want)
		}
	}
}

func TestDiscoverIsReadOnly(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, dir := newClient(t, nil)

	if _, err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, accountsFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Discover wrote state: %v", err)
	}
}

func TestDiscoverMarksAnImportedBinary(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, nil)

	if _, err := c.Import(context.Background(), []string{stub}, false); err != nil {
		t.Fatal(err)
	}
	got, err := c.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !findDiscovered(t, got, stub).Imported {
		t.Error("the imported binary is not marked as imported")
	}
}

// ---------------------------------------------------------------------------
// Import
// ---------------------------------------------------------------------------

func TestImportBindsTheCLIAndMakesItUsable(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	// A working stub the module cannot find on its own: it is not on PATH and
	// ~/.kimi-work/bin is not a location binaryPath() searches.
	stub := stubInWorkBin(t, `{"role":"assistant","content":"imported"}`+"\n")
	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	cfg := map[string]any{"credential_files": []string{cred}}
	c, dir := newClient(t, cfg)

	if got, err := c.run.binaryPath(); err == nil {
		t.Fatalf("binaryPath = %q before Import, want a miss", got)
	}
	if _, err := c.Chat(context.Background(), userReq("kimi", "hi")); err == nil {
		t.Fatal("Chat succeeded before the CLI was reachable")
	}

	imported, err := c.Import(context.Background(), []string{stub}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported = %+v, want one", imported)
	}
	if imported[0].State != "ready" || !imported[0].Enabled {
		t.Errorf("imported = %+v, want an enabled ready binding", imported[0])
	}
	if imported[0].Fields["path"] != stub {
		t.Errorf("fields = %+v, want the bound path", imported[0].Fields)
	}
	if !strings.HasPrefix(imported[0].ID, "binary:") {
		t.Errorf("id = %q, want a binary: binding id", imported[0].ID)
	}

	// The binding is now the executable a request runs, without a restart.
	if got, err := c.run.binaryPath(); err != nil || got != stub {
		t.Fatalf("binaryPath = %q, %v; want %q", got, err, stub)
	}
	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		t.Fatalf("Chat after Import: %v", err)
	}
	defer st.Close()
	events := recvAll(t, st)
	var text string
	for _, ev := range events {
		text += ev.Delta
	}
	if !strings.Contains(text, "imported") {
		t.Fatalf("stream = %q, want the stub's output", text)
	}

	// It persists, and the configured binary still outranks it.
	c2 := newClientIn(t, dir, cfg)
	if got, err := c2.run.binaryPath(); err != nil || got != stub {
		t.Fatalf("binaryPath after a restart = %q, %v; want %q", got, err, stub)
	}
	c3 := newClientIn(t, dir, map[string]any{"credential_files": []string{cred}, "binary": stub + "-configured"})
	if _, err := c3.run.binaryPath(); err == nil {
		t.Error("the imported binding outranked clients.kimi.binary")
	}
}

func TestImportRejectsNonImportablePaths(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(home, ".kimi-webbridge", "identity.json")
	if err := os.MkdirAll(filepath.Dir(identity), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identity, []byte(`{"device_id":"deadbeef"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, _ := newClient(t, nil)
	_, err = c.Import(context.Background(), []string{identity}, false)
	if err == nil {
		t.Fatal("Import accepted a device-identity file")
	}
	if !strings.Contains(err.Error(), "not importable") {
		t.Errorf("error = %v, want it to say why", err)
	}
	if _, err := c.Import(context.Background(), []string{filepath.Join(t.TempDir(), "ghost")}, false); err == nil {
		t.Error("Import accepted a path Discover never reported")
	}
}

func TestImportAllIsIdempotent(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, dir := newClient(t, nil)

	first, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import(all): %v", err)
	}
	if len(first) != 1 || first[0].Fields["path"] != stub {
		t.Fatalf("imported = %+v, want the one importable stub", first)
	}
	second, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("second Import(all): %v", err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("second import = %+v, want the same binding", second)
	}

	var state accountState
	if err := core.ReadJSON(filepath.Join(dir, accountsFileName), &state); err != nil {
		t.Fatalf("reading %s: %v", accountsFileName, err)
	}
	if len(state.Bindings) != 1 {
		t.Fatalf("bindings = %+v, want exactly one", state.Bindings)
	}
}

func TestImportWithNothingImportableIsNotAnError(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	got, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("imported = %+v, want none", got)
	}
}

// ---------------------------------------------------------------------------
// LoginProvider
// ---------------------------------------------------------------------------

func TestStartLoginWithoutTheCLIFailsWithGuidance(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	// The guided CLI flow is no longer the default (weblogin.go drives the
	// vendor's device flow instead), so a test of the CLI flow asks for it.
	c, _ := newClient(t, map[string]any{"login_mode": "cli"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed", state.State)
	}
	if !strings.HasPrefix(state.SessionID, loginSessionPrefix) {
		t.Errorf("session = %q, want the %s prefix", state.SessionID, loginSessionPrefix)
	}
	if !strings.Contains(state.Message, "install") || !strings.Contains(state.Message, "kimi login") {
		t.Errorf("message = %q, want install guidance", state.Message)
	}

	// The failed session stays failed, and can still be cancelled.
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginFailed {
		t.Errorf("state = %q, want failed", polled.State)
	}
	if err := c.CancelLogin(context.Background(), state.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	after, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != core.LoginCancelled {
		t.Errorf("state = %q, want cancelled", after.State)
	}
}

func TestStartLoginIsPendingThenSucceedsWhenTheCredentialAppears(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	cred := filepath.Join(t.TempDir(), "kimi-code.json")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}, "login_mode": "cli"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", state.State)
	}
	if !strings.Contains(state.Message, "kimi login") {
		t.Errorf("message = %q, want the command to run", state.Message)
	}
	if !strings.Contains(state.Message, "terminal") {
		t.Errorf("message = %q, want it to say the operator runs it in a terminal", state.Message)
	}
	if state.AccountID != "" {
		t.Errorf("account_id = %q, want empty while pending", state.AccountID)
	}

	// Still pending before anything happened.
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if polled.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", polled.State)
	}

	// The operator runs `kimi login` and the CLI writes its credential file.
	writeCredential(t, cred, "sk-abcdefghijklmnopqrstuvwxyz")

	done, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("state = %q, want success (%s)", done.State, done.Message)
	}
	if done.AccountID != cliLoginID {
		t.Errorf("account_id = %q, want %q", done.AccountID, cliLoginID)
	}
	if !strings.Contains(done.Message, "login detected") {
		t.Errorf("message = %q, want it to say what was detected", done.Message)
	}

	// The success sticks.
	again, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != core.LoginSuccess {
		t.Errorf("state = %q after a second poll, want success", again.State)
	}
}

func TestStartLoginSucceedsImmediatelyWhenAlreadyLoggedIn(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), "sk-abcdefghijklmnopqrstuvwxyz")
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "credential_files": []string{cred}, "login_mode": "cli"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state.State != core.LoginSuccess {
		t.Fatalf("state = %q, want success", state.State)
	}
	if state.AccountID != cliLoginID {
		t.Errorf("account_id = %q, want %q", state.AccountID, cliLoginID)
	}
	if !strings.Contains(state.Message, "already logged in") {
		t.Errorf("message = %q", state.Message)
	}
}

func TestPollLoginInfersSuccessFromTheIdentityFileAndSaysSo(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "login_mode": "cli"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", state.State)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	wb := filepath.Join(home, ".kimi-webbridge")
	if err := os.MkdirAll(wb, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wb, "identity.json"), []byte(`{"device_id":"deadbeef"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	done, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("state = %q, want success (%s)", done.State, done.Message)
	}
	if !strings.Contains(done.Message, "inferred") {
		t.Errorf("message = %q, want it to admit the success is inferred", done.Message)
	}
	if !strings.Contains(done.Message, "keyring") {
		t.Errorf("message = %q, want it to explain why it cannot verify", done.Message)
	}
}

func TestPollAndCancelRejectUnknownSessions(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)

	if _, err := c.PollLogin(context.Background(), "kimi-login-nope"); err == nil {
		t.Error("PollLogin accepted an unknown session")
	}
	if _, err := c.PollLogin(context.Background(), ""); err == nil {
		t.Error("PollLogin accepted an empty session")
	}
	if err := c.CancelLogin(context.Background(), "kimi-login-nope"); err == nil {
		t.Error("CancelLogin accepted an unknown session")
	}
	if err := c.CancelLogin(context.Background(), ""); err == nil {
		t.Error("CancelLogin accepted an empty session")
	}
}

func TestCancelLoginLeavesTheCLIsLoginAlone(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, _ := newClient(t, map[string]any{"binary": bin, "login_mode": "cli"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelLogin(context.Background(), state.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if polled.State != core.LoginCancelled {
		t.Fatalf("state = %q, want cancelled", polled.State)
	}
	if !strings.Contains(polled.Message, "was not touched") {
		t.Errorf("message = %q, want it to say the CLI's state is untouched", polled.Message)
	}
	// The module is still usable: cancelling a login is not a logout.
	if err := closeChat(t, c, "after cancel"); err == nil {
		t.Error("Chat succeeded without a login; it should still refuse")
	}
}

// ---------------------------------------------------------------------------
// State file hygiene
// ---------------------------------------------------------------------------

func TestAccountStateNeverHoldsASecret(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const token = "sk-abcdefghijklmnopqrstuvwxyz"

	cred := writeCredential(t, filepath.Join(t.TempDir(), "kimi-code.json"), token)
	stub := stubInWorkBin(t, `{"role":"assistant","content":"x"}`+"\n")
	c, dir := newClient(t, map[string]any{"credential_files": []string{cred}})

	if _, err := c.Import(context.Background(), []string{stub}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAccountEnabled(context.Background(), cliLoginID, false); err != nil {
		t.Fatal(err)
	}
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	results, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.TestAccount(context.Background(), cliLoginID)
	if err != nil {
		t.Fatal(err)
	}

	blob, err := os.ReadFile(filepath.Join(dir, accountsFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(blob), marshal(t, recs), marshal(t, results), marshal(t, res)} {
		if strings.Contains(s, token) {
			t.Fatalf("a credential leaked into panel output: %s", s)
		}
	}
}

func TestAccountsSurvivesACorruptStateFile(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	bin := stubEmitting(t, `{"role":"assistant","content":"x"}`+"\n")
	c, dir := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	if err := os.WriteFile(filepath.Join(dir, accountsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Accounts(context.Background()); err == nil {
		t.Error("Accounts hid a corrupt state file")
	}
	st := c.Status(context.Background())
	if !strings.Contains(st.Detail, "account state unreadable") {
		t.Errorf("detail = %q, want the corrupt file reported", st.Detail)
	}
	// A corrupt bookkeeping file must not take the module down with it.
	if err := closeChat(t, c, "still works"); err != nil {
		t.Fatalf("Chat after a corrupt state file: %v", err)
	}
}

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Live (opt-in)
// ---------------------------------------------------------------------------

// TestLivePanelAccountsAgainstTheRealMachine exercises the capabilities against
// whatever is actually installed on this host.  It is read-only: it never
// imports, never toggles and never starts a login.
func TestLivePanelAccountsAgainstTheRealMachine(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE") != "1" {
		t.Skip("set CLIENT2API_LIVE=1 to probe the real machine")
	}
	c, _ := newClient(t, nil)
	ctx := context.Background()

	caps := core.CapabilitiesOf(ctx, c)
	t.Logf("capabilities: manage=%v import=%v login=%v fields=%d", caps.Manage, caps.Import, caps.Login, len(caps.Fields))

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, r := range recs {
		t.Logf("account %-24s state=%-7s enabled=%v note=%s", r.ID, r.State, r.Enabled, r.Note)
	}
	ds, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range ds {
		t.Logf("found %-9s importable=%-5v %s", d.Kind, d.Importable, d.Path)
	}
}
