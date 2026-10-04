package opencode

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// readRequestBody drains the request body a fake transport was handed.
func readRequestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	if r.Body == nil {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Persistence.
// ---------------------------------------------------------------------------

func TestCredentialsPathHonoursDataDir(t *testing.T) {
	c := newTestClient(t, Config{})
	if got := c.credentialsPath(); got != "" {
		t.Fatalf("credentialsPath = %q, want empty without a DataDir", got)
	}
	c.deps.DataDir = t.TempDir()
	want := filepath.Join(c.deps.DataDir, credentialsFile)
	if got := c.credentialsPath(); got != want {
		t.Fatalf("credentialsPath = %q, want %q", got, want)
	}
	// Whitespace is not a data directory either.
	c.deps.DataDir = "   "
	if got := c.credentialsPath(); got != "" {
		t.Fatalf("credentialsPath = %q, want empty for a blank DataDir", got)
	}
}

// A key the operator keeps in the config or the environment must not be copied
// onto disk: the module would then be the second place the secret lives.
func TestPersistSkipsConfigAndEnvCredentials(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	c.pool.upsert(accountRecord{ID: "opencode:cfg", APIKey: "sk-cfg", Source: sourceConfig})
	c.pool.upsert(accountRecord{ID: "opencode:env", APIKey: "sk-env", Source: sourceEnv})
	c.pool.upsert(accountRecord{ID: "opencode:pan", APIKey: "sk-pan", Source: sourcePanel})
	c.persist()

	recs := loadCredentials(c.credentialsPath())
	if len(recs) != 1 || recs[0].ID != "opencode:pan" {
		t.Fatalf("persisted = %+v, want only the panel credential", recs)
	}
}

func TestPersistWithoutADataDirWritesNothing(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.pool.upsert(accountRecord{ID: "opencode:a", APIKey: "sk-a", Source: sourcePanel})
	c.persist() // must not panic or write into the working directory
	if c.credentialsPath() != "" {
		t.Fatal("credentialsPath changed")
	}
}

// A chat that already succeeded must not turn into an error because the data
// directory is unwritable: persist records the failure and returns.
func TestPersistFailureIsRecordedNotReturned(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	// A regular file where the data directory should be makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c.deps.DataDir = blocker
	c.pool.upsert(accountRecord{ID: "opencode:a", APIKey: "sk-a", Source: sourcePanel})

	c.persist() // returns nothing; the failure is recorded instead

	// The account is untouched and the write failure is recorded for Status,
	// not returned to a caller whose chat already succeeded.
	if _, ok := c.pool.byID("opencode:a"); !ok {
		t.Fatal("persist dropped the account")
	}
	note := c.lastErrorNote()
	if !strings.Contains(note, "saving credentials") {
		t.Fatalf("lastErrorNote = %q, want the write failure recorded", note)
	}
}

// The persisted state is what survives a restart, so it must be restored
// rather than reset to the defaults.
func TestLoadAccountsRestoresPersistedState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	id := accountID(keyFingerprint("sk-persisted"))
	until := testNow.Add(time.Hour).UTC().Format(time.RFC3339)
	if err := saveCredentials(c.credentialsPath(), []accountRecord{{
		ID:            id,
		Label:         "kept label",
		APIKey:        "sk-persisted",
		Enabled:       false,
		Source:        sourceImport,
		CooldownUntil: until,
		CooldownKind:  "rate",
		LastError:     "throttled",
		ErrCount:      3,
	}}); err != nil {
		t.Fatalf("saveCredentials = %v", err)
	}

	c.loadAccounts()

	rec, ok := c.pool.byID(id)
	if !ok {
		t.Fatal("loadAccounts dropped the persisted account")
	}
	if rec.Enabled {
		t.Fatal("Enabled was reset to true")
	}
	if rec.CooldownUntil != until || rec.CooldownKind != "rate" {
		t.Fatalf("cooldown = %q/%q, want it carried across", rec.CooldownUntil, rec.CooldownKind)
	}
	if rec.ErrCount != 3 || rec.LastError != "throttled" {
		t.Fatalf("failure state = %d/%q, want it carried across", rec.ErrCount, rec.LastError)
	}
	if rec.Label != "kept label" {
		t.Fatalf("Label = %q, want the persisted label", rec.Label)
	}
}

// A stored record whose key is no longer in any source file must still be
// listed: the panel is where accounts are deleted, not the filesystem.
func TestLoadAccountsKeepsAnAccountWhoseSourceVanished(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	id := accountID(keyFingerprint("sk-orphan"))
	if err := saveCredentials(c.credentialsPath(), []accountRecord{{
		ID: id, Label: "orphan", APIKey: "sk-orphan", Enabled: true, Source: sourceImport,
	}}); err != nil {
		t.Fatalf("saveCredentials = %v", err)
	}

	c.loadAccounts()
	if _, ok := c.pool.byID(id); !ok {
		t.Fatal("loadAccounts deleted an account because its source file vanished")
	}
}

func TestLoadAccountsDedupesByFingerprint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{APIKey: "sk-same"})
	c.deps.DataDir = t.TempDir()

	id := accountID(keyFingerprint("sk-same"))
	if err := saveCredentials(c.credentialsPath(), []accountRecord{{
		ID: id, APIKey: "sk-same", Enabled: false, Source: sourceImport, Label: "from file",
	}}); err != nil {
		t.Fatalf("saveCredentials = %v", err)
	}

	c.loadAccounts()

	recs := c.pool.snapshot()
	if len(recs) != 1 {
		t.Fatalf("pool = %+v, want one account: the config key and the stored key are the same key", recs)
	}
	if recs[0].Source != sourceConfig {
		t.Fatalf("Source = %q, want the config source to win", recs[0].Source)
	}
	if recs[0].Enabled {
		t.Fatal("the persisted enabled=false was not carried onto the config candidate")
	}
	if recs[0].Label != "from file" {
		t.Fatalf("Label = %q, want the persisted label kept", recs[0].Label)
	}
}

func TestLoadAccountsMergesConfigAndVendorSources(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"api","key":"sk-from-file"}}`)

	c := newTestClient(t, Config{APIKey: "sk-from-config"})
	c.deps.DataDir = t.TempDir()

	c.loadAccounts()

	ids := sortedAccountIDs(c.pool.snapshot())
	want := []string{
		accountID(keyFingerprint("sk-from-config")),
		accountID(keyFingerprint("sk-from-file")),
	}
	sort.Strings(want)
	if len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

// ---------------------------------------------------------------------------
// The panel's account table.
// ---------------------------------------------------------------------------

func TestAccountFields(t *testing.T) {
	c := newTestClient(t, Config{})
	fields := c.AccountFields(context.Background())
	if len(fields) != 2 {
		t.Fatalf("fields = %+v, want key and label", fields)
	}
	if fields[0].Key != "key" || fields[0].Type != "password" || !fields[0].Required {
		t.Fatalf("key field = %+v, want a required password", fields[0])
	}
	if fields[0].Help == "" {
		t.Fatal("the key field has no help text")
	}
	if fields[1].Key != "label" || fields[1].Required {
		t.Fatalf("label field = %+v, want an optional text field", fields[1])
	}
}

func TestAccountRecordsMaskTheKeyAndStayIdentityLess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:abc", "sk-secret-value")

	recs := c.accountRecords(testNow)
	if len(recs) != 1 {
		t.Fatalf("recs = %+v", recs)
	}
	r := recs[0]
	if r.Fields["key"] != core.MaskSecret("sk-secret-value") {
		t.Fatalf("Fields[key] = %v, want the masked key", r.Fields["key"])
	}
	if strings.Contains(anyString(r.Fields["key"]), "sk-secret-value") {
		t.Fatal("the raw key reached the panel view")
	}
	if r.Fields["source"] != "panel" {
		t.Fatalf("Fields[source] = %v, want the panel label", r.Fields["source"])
	}
	// Zen publishes no account identifier for a key, so guessing one would
	// merge unrelated keys.
	if r.Identity != "" {
		t.Fatalf("Identity = %q, want it empty", r.Identity)
	}
}

func TestAccountsListsThePool(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	addAccount(t, c, "opencode:b", "sk-b")

	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts = %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("Accounts = %+v, want both accounts", recs)
	}
}

func TestAddAccount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"key": "  sk-pasted  ", "label": " mine "},
	})
	if err != nil {
		t.Fatalf("AddAccount = %v", err)
	}
	wantID := accountID(keyFingerprint("sk-pasted"))
	if rec.ID != wantID {
		t.Fatalf("ID = %q, want %q (the key must be trimmed and fingerprinted)", rec.ID, wantID)
	}
	if rec.Label != "mine" {
		t.Fatalf("Label = %q, want it trimmed", rec.Label)
	}
	if rec.Fields["key"] != core.MaskSecret("sk-pasted") {
		t.Fatalf("Fields[key] = %v, want the masked key", rec.Fields["key"])
	}
	if !rec.Enabled {
		t.Fatal("a new account must default to enabled")
	}

	// It is persisted, so it survives a restart.
	recs := loadCredentials(c.credentialsPath())
	if len(recs) != 1 || recs[0].APIKey != "sk-pasted" || recs[0].Source != sourcePanel {
		t.Fatalf("persisted = %+v", recs)
	}
}

func TestAddAccountRejectsABlankKey(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	_, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"key": "   "}})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("AddAccount = %v, want core.ErrUnsupported", err)
	}
	if c.pool.size() != 0 {
		t.Fatal("a blank key created an account")
	}
}

func TestAddAccountFallsBackToTheSpecLabel(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label:  "from the spec",
		Fields: map[string]string{"key": "sk-labelled"},
	})
	if err != nil {
		t.Fatalf("AddAccount = %v", err)
	}
	if rec.Label != "from the spec" {
		t.Fatalf("Label = %q, want the spec label when the field is empty", rec.Label)
	}
}

func TestAddAccountHonoursAnExplicitDisabled(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	no := false
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Enabled: &no,
		Fields:  map[string]string{"key": "sk-off"},
	})
	if err != nil {
		t.Fatalf("AddAccount = %v", err)
	}
	if rec.Enabled {
		t.Fatal("Enabled = true, want the requested false")
	}
}

func TestRemoveAccount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	addAccount(t, c, "opencode:a", "sk-a")

	if err := c.RemoveAccount(context.Background(), "opencode:a"); err != nil {
		t.Fatalf("RemoveAccount = %v", err)
	}
	if c.pool.size() != 0 {
		t.Fatal("the account survived removal")
	}
	// An unknown id is an error, not a no-op: the panel's table is stale.
	if err := c.RemoveAccount(context.Background(), "opencode:a"); err == nil {
		t.Fatal("RemoveAccount accepted an unknown id")
	}
}

func TestSetAccountEnabled(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	addAccount(t, c, "opencode:a", "sk-a")

	if err := c.SetAccountEnabled(context.Background(), "opencode:a", false); err != nil {
		t.Fatalf("SetAccountEnabled = %v", err)
	}
	rec, _ := c.pool.byID("opencode:a")
	if rec.Enabled {
		t.Fatal("the account was not disabled")
	}
	if err := c.SetAccountEnabled(context.Background(), "opencode:missing", false); err == nil {
		t.Fatal("SetAccountEnabled accepted an unknown id")
	}
}

// Re-enabling must clear the failure state, or an account comes back already
// parked and fails its first request.
func TestSetAccountEnabledClearsTheFailureState(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	addAccount(t, c, "opencode:a", "sk-a")

	c.pool.setCooldown("opencode:a", testNow.Add(time.Hour), "throttled")
	c.pool.noteError("opencode:a", "boom", testNow, 1)
	rec, _ := c.pool.byID("opencode:a")
	if rec.CooldownUntil == "" || rec.ErrCount == 0 {
		t.Fatalf("setup failed: %+v", rec)
	}

	if err := c.SetAccountEnabled(context.Background(), "opencode:a", false); err != nil {
		t.Fatalf("disable = %v", err)
	}
	if err := c.SetAccountEnabled(context.Background(), "opencode:a", true); err != nil {
		t.Fatalf("enable = %v", err)
	}

	rec, _ = c.pool.byID("opencode:a")
	if rec.CooldownUntil != "" || rec.CooldownKind != "" || rec.LastError != "" || rec.ErrCount != 0 {
		t.Fatalf("record = %+v, want the failure state cleared on re-enable", rec)
	}
}

// A cooling account can already be enabled, so a re-enable has to be more than
// a no-op: the operator flips a switch that is already on to unpark it.
func TestSetAccountEnabledUnparksAnAlreadyEnabledAccount(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	addAccount(t, c, "opencode:a", "sk-a")

	c.pool.setCooldown("opencode:a", testNow.Add(time.Hour), "throttled")
	c.pool.noteError("opencode:a", "boom", testNow, 1)

	if err := c.SetAccountEnabled(context.Background(), "opencode:a", true); err != nil {
		t.Fatalf("re-enable = %v", err)
	}
	rec, _ := c.pool.byID("opencode:a")
	if rec.CooldownUntil != "" || rec.LastError != "" || rec.ErrCount != 0 {
		t.Fatalf("record = %+v, want the already-on switch to clear the parked state", rec)
	}
}

// ---------------------------------------------------------------------------
// TestAccount.
// ---------------------------------------------------------------------------

func TestTestAccountUnknownIsAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	if _, err := c.TestAccount(context.Background(), "opencode:missing"); err == nil {
		t.Fatal("TestAccount accepted an unknown id")
	}
}

func TestTestAccountSendsOneTinyRequest(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var gotPath, gotAuth, gotAccept string
	var gotBody []byte
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotBody = readRequestBody(t, r)
		return sseResponse(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
				"data: [DONE]\n\n"), nil
	})
	addAccount(t, c, "opencode:a", "sk-probe")

	res, err := c.TestAccount(context.Background(), "opencode:a")
	if err != nil {
		t.Fatalf("TestAccount = %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want OK", res)
	}
	if res.Reply != "pong" {
		t.Fatalf("Reply = %q", res.Reply)
	}
	if res.AccountID != "opencode:a" {
		t.Fatalf("AccountID = %q", res.AccountID)
	}
	if res.Model != defaultTestModel {
		t.Fatalf("Model = %q, want %q", res.Model, defaultTestModel)
	}
	if gotPath != "/zen/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotAuth != "Bearer sk-probe" {
		t.Fatalf("Authorization = %q, want the account's own key", gotAuth)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q", gotAccept)
	}
	if !strings.Contains(string(gotBody), `"model":"`+defaultTestModel+`"`) {
		t.Fatalf("body = %s, want the test model", gotBody)
	}
	// A successful probe must not leave the account parked.
	rec, _ := c.pool.byID("opencode:a")
	if rec.CooldownUntil != "" {
		t.Fatalf("record = %+v, want no cooldown after a success", rec)
	}
}

func TestTestAccountProbesTheFreeModelForAnonymousAccounts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var gotAPIKey, gotAuth string
	var gotBody []byte
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuth = r.Header.Get("Authorization")
		gotBody = readRequestBody(t, r)
		return sseResponse(
			"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"pong\"},\"finish_reason\":null}]}\n\n" +
				"data: [DONE]\n\n"), nil
	})
	if _, err := c.StartLoginRealm(context.Background(), "free"); err != nil {
		t.Fatalf("StartLoginRealm(free) = %v", err)
	}

	res, err := c.TestAccount(context.Background(), "opencode:anonymous")
	if err != nil {
		t.Fatalf("TestAccount = %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want OK", res)
	}
	if res.Model != "space-bunny-free" {
		t.Fatalf("Model = %q, want the free allowlist id", res.Model)
	}
	if gotAPIKey != "public" {
		t.Fatalf("x-api-key = %q, want public", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want none for an anonymous account", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"model":"space-bunny-free"`) {
		t.Fatalf("body = %s, want the free model", gotBody)
	}
}

func TestTestAccountReportsANon200(t *testing.T) {

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-rejected")

	res, err := c.TestAccount(context.Background(), "opencode:a")
	if err != nil {
		t.Fatalf("TestAccount = %v, want the verdict in the result", err)
	}
	if res.OK {
		t.Fatal("OK = true for a 401")
	}
	if !strings.Contains(res.Error, "HTTP 401") {
		t.Fatalf("Error = %q, want the status in the message", res.Error)
	}
	if strings.Contains(res.Error, "sk-rejected") {
		t.Fatalf("Error = %q, must not carry the key", res.Error)
	}
	// A key the vendor rejected is disabled, not merely noted.
	rec, _ := c.pool.byID("opencode:a")
	if rec.Enabled {
		t.Fatalf("record = %+v, want the account disabled after an auth failure", rec)
	}
}

func TestTestAccountReportsATransportError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused for sk-net")
	})
	addAccount(t, c, "opencode:a", "sk-net")

	res, err := c.TestAccount(context.Background(), "opencode:a")
	if err != nil {
		t.Fatalf("TestAccount = %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a transport error")
	}
	if !strings.Contains(res.Error, "connection refused") {
		t.Fatalf("Error = %q, want the transport message", res.Error)
	}
	if strings.Contains(res.Error, "sk-net") {
		t.Fatalf("Error = %q, must not carry the key", res.Error)
	}
}

// ---------------------------------------------------------------------------
// RefreshAccount.
// ---------------------------------------------------------------------------

func TestRefreshAccountUnknownIsAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	if _, err := c.RefreshAccount(context.Background(), "opencode:missing"); err == nil {
		t.Fatal("RefreshAccount accepted an unknown id")
	}
}

func TestRefreshAccountReimportsTheCredentialSources(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"api","key":"sk-refresh"}}`)

	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	c.loadAccounts()

	id := accountID(keyFingerprint("sk-refresh"))
	results, err := c.RefreshAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("RefreshAccount = %v", err)
	}
	if len(results) != 1 || !results[0].OK || results[0].AccountID != id {
		t.Fatalf("results = %+v", results)
	}
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

func TestFilepathGlobJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.json"), `{}`)
	writeFile(t, filepath.Join(dir, "b.txt"), `{}`)

	got, err := filepathGlobJSON(dir)
	if err != nil {
		t.Fatalf("filepathGlobJSON = %v", err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "a.json" {
		t.Fatalf("got = %v, want only the .json file", got)
	}
}

func TestPoolStats(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")

	stats := c.PoolStats()
	if stats.InFlight != 0 || stats.InFlightFull != 0 {
		t.Fatalf("PoolStats = %+v, want idle", stats)
	}
}

// anyString renders a panel field for an assertion without panicking on a
// value that is not a string.
func anyString(v any) string {
	s, _ := v.(string)
	return s
}
