package trae

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// panelClient builds a Client with a private DataDir and automatic discovery
// switched off, so no test here can ever read the developer's real Trae
// credential off disk.
func panelClient(t *testing.T, cfg *Config, rt http.RoundTripper) *Client {
	t.Helper()
	if cfg == nil {
		cfg = loadConfig(nil, nil)
	}
	off := false
	cfg.AutoDiscover = &off
	c := testClient(t, cfg, nil, rt)
	c.dataDir = t.TempDir()
	c.store = loadAccountStore(c.dataDir)
	c.pool = NewPool(nil)
	return c
}

// fakeJWT mints an unsigned JWT whose payload carries the given expiry. Only
// the payload segment is ever read, and only for pre-filling the panel.
func fakeJWT(t *testing.T, exp time.Time) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"exp": exp.Unix(), "sub": "u1"})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(claims)
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func addTestAccount(t *testing.T, c *Client, fields map[string]string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Label: "panel", Fields: fields})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec
}

func TestAccountFieldsSchema(t *testing.T) {
	c := panelClient(t, nil, nil)
	specs := c.AccountFields(context.Background())
	if len(specs) == 0 {
		t.Fatal("AccountFields returned nothing")
	}
	byKey := map[string]core.FieldSpec{}
	for _, s := range specs {
		if s.Key == "" || s.Label == "" {
			t.Errorf("incomplete field spec %#v", s)
		}
		if _, dup := byKey[s.Key]; dup {
			t.Errorf("duplicate field key %q", s.Key)
		}
		switch s.Type {
		case "text", "password", "textarea", "number", "bool", "select":
		default:
			t.Errorf("field %q has unsupported type %q", s.Key, s.Type)
		}
		if s.Type == "select" && len(s.Options) == 0 {
			t.Errorf("select field %q has no options", s.Key)
		}
		byKey[s.Key] = s
	}
	tok, ok := byKey["access_token"]
	if !ok {
		t.Fatalf("no access_token field; got %v", byKey)
	}
	if !tok.Required {
		t.Error("access_token should be required")
	}
	if tok.Type != "password" {
		t.Errorf("access_token type = %q, want password", tok.Type)
	}
}

func TestAddAccountRequiresToken(t *testing.T) {
	c := panelClient(t, nil, nil)
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{}); err == nil {
		t.Fatal("expected an error for an empty spec")
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"access_token": "   "}}); err == nil {
		t.Fatal("expected an error for a blank access_token")
	}
}

func TestAddAccountPersistsAndLists(t *testing.T) {
	ctx := context.Background()
	c := panelClient(t, nil, nil)
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0001", "user_id": "u-42"})

	if rec.ID == "" {
		t.Fatal("AddAccount returned an empty id")
	}
	if !rec.Enabled {
		t.Error("a freshly added account should be enabled")
	}
	if rec.Label != "panel" {
		t.Errorf("label = %q, want panel", rec.Label)
	}

	// The store file must exist and hold the token, since that is the only
	// place a hand-entered credential can live.
	raw, err := os.ReadFile(filepath.Join(c.dataDir, accountsFileName))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !strings.Contains(string(raw), "PANEL_TOKEN_0001") {
		t.Error("token was not persisted to the store file")
	}

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var found *core.AccountRecord
	for i := range list {
		if list[i].ID == rec.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatalf("added account %q missing from Accounts", rec.ID)
	}
	if !found.Enabled {
		t.Error("listed account should be enabled")
	}
	if found.Fields["managed"] != true {
		t.Errorf("managed flag = %#v, want true", found.Fields["managed"])
	}

	// The pool must have picked it up so chat actually uses it.
	if c.pool.Len() == 0 {
		t.Error("pool is empty after AddAccount")
	}
}

func TestAddAccountDerivesJWTExpiry(t *testing.T) {
	want := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	c := panelClient(t, nil, nil)
	rec := addTestAccount(t, c, map[string]string{"access_token": fakeJWT(t, want)})
	if rec.ExpiresAt == "" {
		t.Fatal("ExpiresAt not derived from the JWT")
	}
	got, err := time.Parse(time.RFC3339, rec.ExpiresAt)
	if err != nil {
		t.Fatalf("ExpiresAt %q is not RFC3339: %v", rec.ExpiresAt, err)
	}
	if !got.UTC().Truncate(time.Second).Equal(want) {
		t.Errorf("ExpiresAt = %s, want %s", got.UTC(), want)
	}
}

func TestSetAccountEnabledKeepsStoredListed(t *testing.T) {
	ctx := context.Background()
	c := panelClient(t, nil, nil)
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0002"})

	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}
	if c.pool.Len() != 0 {
		t.Errorf("pool still holds %d account(s) after disabling", c.pool.Len())
	}

	// A disabled account must remain visible, otherwise it can never be
	// switched back on from the panel.
	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var found *core.AccountRecord
	for i := range list {
		if list[i].ID == rec.ID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("disabled account vanished from Accounts")
	}
	if found.Enabled {
		t.Error("disabled account is still reported as enabled")
	}

	if err := c.SetAccountEnabled(ctx, rec.ID, true); err != nil {
		t.Fatalf("SetAccountEnabled(true): %v", err)
	}
	if c.pool.Len() == 0 {
		t.Error("pool empty after re-enabling")
	}

	if err := c.SetAccountEnabled(ctx, "nope", false); err == nil {
		t.Error("expected an error for an unknown id")
	}
}

// A discovered credential has no stored record to flip, so switching it off
// leaves nothing behind but a suppression entry.  Switching it back on has to
// work from the panel: otherwise one click on "disable" bricks the module.
func TestSetAccountEnabledRevivesASuppressedDiscoveredAccount(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	fixture := map[string]any{
		storageKeyAuth:        tcFixture,
		"telemetry.machineId": strings.Repeat("a", 64),
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg := loadConfig(nil, nil)
	cfg.StoragePath = path
	c := panelClient(t, cfg, nil)
	// panelClient pins discovery off so no test reads the developer's real
	// credential; this fixture is a temp file, so switching it back on is safe.
	on := true
	c.cfg.AutoDiscover = &on
	c.rebuildPool()

	if c.pool.Len() != 1 {
		t.Fatalf("pool holds %d account(s), want the one discovered credential", c.pool.Len())
	}
	id := c.pool.Accounts()[0].ID()
	if id == "" {
		t.Fatal("the discovered account has no id")
	}
	if c.store.index(id) >= 0 {
		t.Fatalf("fixture account %q must be discovered, not stored", id)
	}

	if err := c.SetAccountEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}
	if c.pool.Len() != 0 {
		t.Errorf("pool still holds %d account(s) after disabling", c.pool.Len())
	}

	if err := c.SetAccountEnabled(ctx, id, true); err != nil {
		t.Fatalf("SetAccountEnabled(true) on a suppressed discovered account: %v", err)
	}
	if c.pool.Len() != 1 {
		t.Errorf("pool holds %d account(s) after re-enabling, want 1", c.pool.Len())
	}
}

func TestRemoveAccount(t *testing.T) {
	ctx := context.Background()
	c := panelClient(t, nil, nil)
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0003"})

	if err := c.RemoveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if err := c.RemoveAccount(ctx, rec.ID); err == nil {
		t.Error("expected an error removing an unknown id")
	}
	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	for _, a := range list {
		if a.ID == rec.ID {
			t.Fatalf("removed account %q still listed", rec.ID)
		}
	}
}

func TestTestAccountSuccess(t *testing.T) {
	ctx := context.Background()
	body := "event:output\ndata:{\"response\":\"pong\"}\n\n"
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return sseResponse(body), nil
	})
	c := panelClient(t, nil, rt)
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0004"})

	res, err := c.TestAccount(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error = %q", res.Error)
	}
	if res.Reply != "pong" {
		t.Errorf("Reply = %q, want pong", res.Reply)
	}
	if res.AccountID != rec.ID {
		t.Errorf("AccountID = %q, want %q", res.AccountID, rec.ID)
	}
	if res.Model == "" {
		t.Error("Model is empty")
	}
}

func TestTestAccountUpstreamFailure(t *testing.T) {
	ctx := context.Background()
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"code":1001,"message":"token expired"}`), nil
	})
	c := panelClient(t, nil, rt)
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0005"})

	res, err := c.TestAccount(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TestAccount returned a hard error: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true despite a 401 upstream")
	}
	if strings.TrimSpace(res.Error) == "" {
		t.Error("Error is empty")
	}
}

func TestTestAccountUnknownID(t *testing.T) {
	c := panelClient(t, nil, nil)
	if _, err := c.TestAccount(context.Background(), "ghost"); err == nil {
		t.Error("expected an error for an unknown id")
	}
}

func TestRefreshAccountReportsPerAccountError(t *testing.T) {
	ctx := context.Background()
	c := panelClient(t, nil, nil)
	// No refresh token: the account cannot be renewed, but that is a
	// per-account result, not a failure of the whole call.
	rec := addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0006"})

	results, err := c.RefreshAccount(ctx, rec.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].OK {
		t.Error("OK = true for an account with no refresh token")
	}
	if strings.TrimSpace(results[0].Error) == "" {
		t.Error("Error is empty")
	}
}

func TestDiscoverAndImportStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	fixture := map[string]any{
		storageKeyAuth:                              tcFixture,
		"telemetry.machineId":                       strings.Repeat("a", 64),
		"telemetry.devDeviceId":                     strings.Repeat("b", 64),
		"iCubeAuthInfo://icube-dc:2235771921399404": "e30=",
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg := loadConfig(nil, nil)
	cfg.StoragePath = path
	c := panelClient(t, cfg, nil)

	found, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var target *core.DiscoveredCredential
	for i := range found {
		if found[i].Path == path {
			target = &found[i]
		}
	}
	if target == nil {
		t.Fatalf("Discover did not report %s; got %#v", path, found)
	}
	if !target.Importable {
		t.Errorf("fixture is not importable: %s", target.Note)
	}
	if target.Imported {
		t.Error("Imported should be false before Import runs")
	}

	imported, err := c.Import(ctx, []string{path}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 {
		t.Fatalf("imported %d accounts, want 1", len(imported))
	}
	if imported[0].ID == "" {
		t.Error("imported account has no id")
	}

	again, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	for _, d := range again {
		if d.Path == path && !d.Imported {
			t.Error("Imported is still false after a successful import")
		}
	}
}

func TestAccountsNeverLeakToken(t *testing.T) {
	ctx := context.Background()
	c := panelClient(t, nil, nil)
	addTestAccount(t, c, map[string]string{"access_token": "SUPER_SECRET_TOKEN_XYZ"})

	list, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	blob, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal accounts: %v", err)
	}
	if strings.Contains(string(blob), "SUPER_SECRET_TOKEN_XYZ") {
		t.Fatalf("Accounts leaked the raw token: %s", blob)
	}
}

func TestAccountStoreFileIsPrivate(t *testing.T) {
	c := panelClient(t, nil, nil)
	addTestAccount(t, c, map[string]string{"access_token": "PANEL_TOKEN_0007"})
	info, err := os.Stat(filepath.Join(c.dataDir, accountsFileName))
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if info.Size() == 0 {
		t.Error("store file is empty")
	}
	// Windows has no POSIX mode bits; Go reports 0666 for any writable file, so
	// the 0600 intent can only be checked where the bits are real.
	if runtime.GOOS == "windows" {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Errorf("store mode = %v, want no group/other access", info.Mode().Perm())
	}
}
