package workbuddy

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// accounts_test.go covers the optional panel surface offline.  Every test here
// builds its own data directory and, when it touches the vendor stores at all,
// points the environment at a throwaway root first — nothing in this file reads
// the developer's real credentials or reaches the network.

// --- fixtures ---------------------------------------------------------------

// enterpriseModelsFixture is the shape fetchEnterpriseModels accepts: a "cli"
// agent whose model list selects which entries survive.
const enterpriseModelsFixture = `{"code":0,"data":{
  "models":[
    {"id":"claude-sonnet-4","modelId":"claude-sonnet-4","maxOutputTokens":8192,"tags":[]},
    {"id":"glm-5","modelId":"glm-5","maxOutputTokens":8192,"tags":[]}
  ],
  "agents":[{"name":"cli","models":["claude-sonnet-4","glm-5"]}]
}}`

// panelClient builds a Client over a fresh temp data dir pre-seeded with the
// named credential files (name -> body).
func panelClient(t *testing.T, rt http.RoundTripper, files map[string]string) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	deps := core.Deps{DataDir: dir}
	if rt != nil {
		deps.HTTPClient = &http.Client{Transport: rt}
	}
	client, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := client.(*Client)
	if !ok {
		t.Fatalf("New returned %T", client)
	}
	return c, dir
}

// isolateVendorStores repoints the vendor lookup at a throwaway root so a test
// never walks the real machine's WorkBuddy directories.  It returns the root.
func isolateVendorStores(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "local"))
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))
	return root
}

// seedVendorStore writes a file into the isolated ~/.workbuddy store, which is
// one of the locations vendorCredentialStores reports.
func seedVendorStore(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, "home", ".workbuddy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir vendor store: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write vendor file: %v", err)
	}
	return p
}

// catalogueRT answers the enterprise model probe and fails everything else, so
// a test can drive one real catalogue round trip without a network.
func catalogueRT(status int, body string) *fakeRT {
	return &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, enterpriseModelsPth) {
			return jsonResponse(status, body), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"probe disabled"}`), nil
	}}
}

func addPanel(t *testing.T, c *Client, fields map[string]string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: fields})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec
}

// --- form -------------------------------------------------------------------

func TestWorkbuddyAccountFieldsSchema(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	fields := c.AccountFields(context.Background())
	if len(fields) != 9 {
		t.Fatalf("got %d fields, want 9", len(fields))
	}
	byKey := map[string]core.FieldSpec{}
	for _, f := range fields {
		if f.Key == "" {
			t.Fatalf("field %+v has no key", f)
		}
		if _, dup := byKey[f.Key]; dup {
			t.Fatalf("duplicate field key %q", f.Key)
		}
		if f.Label == "" {
			t.Fatalf("field %q has no label", f.Key)
		}
		switch f.Type {
		case "text", "password", "textarea", "number", "bool", "select":
		default:
			t.Fatalf("field %q has unsupported type %q", f.Key, f.Type)
		}
		byKey[f.Key] = f
	}
	for _, key := range []string{
		"access_token", "refresh_token", "nickname", "uid",
		"enterprise_id", "domain", "realm", "expires_at", "device_token",
	} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("missing field %q", key)
		}
	}
	if !byKey["access_token"].Required {
		t.Fatal("access_token must be required")
	}
	if byKey["access_token"].Type != "password" {
		t.Fatalf("access_token type = %q, want password", byKey["access_token"].Type)
	}
	realm := byKey["realm"]
	if realm.Type != "select" || len(realm.Options) != 3 {
		t.Fatalf("realm = %+v, want a 3-option select", realm)
	}
}

// --- listing ----------------------------------------------------------------

func TestWorkbuddyPanelAccountsListLiveAndParked(t *testing.T) {
	live := credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", "cn",
		"copilot.tencent.com", "uid-live-1", time.Now().Add(24*time.Hour).Unix())
	parked := credJSON("access-token-parked0001", "", "cn",
		"copilot.tencent.com", "uid-parked-1", 0)
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-live-1.json":                    live,
		"wb-uid-parked-1.json" + disabledSuffix: parked,
	})

	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2: %+v", len(records), records)
	}
	// Sorted by id, so parked sorts after live.
	if records[0].ID != "uid-live-1" || records[1].ID != "uid-parked-1" {
		t.Fatalf("unexpected order: %q, %q", records[0].ID, records[1].ID)
	}
	if !records[0].Enabled {
		t.Fatalf("live account reported disabled: %+v", records[0])
	}
	if records[1].Enabled {
		t.Fatalf("parked account reported enabled: %+v", records[1])
	}
	if records[1].State != disabledState {
		t.Fatalf("parked state = %q, want %q", records[1].State, disabledState)
	}
	if records[0].Fields["origin"] != originPanel {
		t.Fatalf("live origin = %v, want %q", records[0].Fields["origin"], originPanel)
	}
	if records[0].Fields["file"] != "wb-uid-live-1.json" {
		t.Fatalf("live file = %v", records[0].Fields["file"])
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-parked-1.json")); !os.IsNotExist(err) {
		t.Fatalf("parked file should not be loadable as *.json, stat err = %v", err)
	}
}

// --- adding -----------------------------------------------------------------

func TestWorkbuddyAddAccountWritesFile(t *testing.T) {
	c, dir := panelClient(t, nil, nil)
	rec := addPanel(t, c, map[string]string{
		"access_token": "panel-token-abcdefgh",
		"uid":          "uid-new-1",
		"nickname":     "alpha",
	})

	if rec.ID != "uid-new-1" {
		t.Fatalf("id = %q", rec.ID)
	}
	if rec.Label != "alpha" {
		t.Fatalf("label = %q, want alpha", rec.Label)
	}
	if !rec.Enabled {
		t.Fatalf("new account should be enabled: %+v", rec)
	}
	if rec.Fields["origin"] != originPanel {
		t.Fatalf("origin = %v, want %q", rec.Fields["origin"], originPanel)
	}

	p := filepath.Join(dir, "wb-uid-new-1.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("credential file not written: %v", err)
	}
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("written credential does not parse: %v", err)
	}
	if a.AccessTokenValue() != "panel-token-abcdefgh" {
		t.Fatalf("access token not persisted")
	}
	if a.NicknameValue() != "alpha" {
		t.Fatalf("nickname = %q", a.NicknameValue())
	}

	// And it is immediately usable, without a restart.
	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].ID != "uid-new-1" {
		t.Fatalf("added account not listed: %+v", records)
	}
}

func TestWorkbuddyAddAccountValidation(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"no token", map[string]string{"uid": "u"}, "access_token is required"},
		{"blank token", map[string]string{"access_token": "   "}, "access_token is required"},
		{"token with newline", map[string]string{"access_token": "abc\ndef"}, "must not contain whitespace"},
		{"token with tab", map[string]string{"access_token": "abc\tdef"}, "must not contain whitespace"},
		{"bad realm", map[string]string{"access_token": "tok-abcdefgh", "realm": "eu"}, "realm must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := panelClient(t, nil, nil)
			_, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: tc.fields})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "tok-abcdefgh") {
				t.Fatalf("error echoes the token: %q", err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatalf("a rejected add must not leave files: %v", entries)
			}
		})
	}
}

func TestWorkbuddyAddAccountUpdatesSameUID(t *testing.T) {
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-x.json": credJSON("old-token-aaaaaaaa", "", "cn", "copilot.tencent.com", "uid-x", 0),
	})
	addPanel(t, c, map[string]string{"access_token": "new-token-bbbbbbbb", "uid": "uid-x"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("same uid should update in place, got %d files", len(entries))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if a.AccessTokenValue() != "new-token-bbbbbbbb" {
		t.Fatalf("token not updated: %q", a.AccessTokenValue())
	}
}

func TestWorkbuddyAddAccountNeverClobbersExistingFile(t *testing.T) {
	other := credJSON("other-token-aaaaaaa", "", "cn", "copilot.tencent.com", "someone-else", 0)
	c, dir := panelClient(t, nil, map[string]string{"wb-uid-x.json": other})
	addPanel(t, c, map[string]string{"access_token": "fresh-token-ccccccc", "uid": "uid-x"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected the original to survive plus one new file, got %d", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(dir, "wb-uid-x.json"))
	if err != nil {
		t.Fatalf("the pre-existing file was moved or removed: %v", err)
	}
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if a.UIDValue() != "someone-else" {
		t.Fatalf("the pre-existing file was overwritten: uid = %q", a.UIDValue())
	}
}

// --- removing ---------------------------------------------------------------

func TestWorkbuddyRemoveAccount(t *testing.T) {
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
		"wb-uid-b.json": credJSON("access-token-bbbbbbbb", "", "cn", "copilot.tencent.com", "uid-b", 0),
	})
	if err := c.RemoveAccount(context.Background(), "uid-a"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-a.json")); !os.IsNotExist(err) {
		t.Fatalf("file still present, err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-b.json")); err != nil {
		t.Fatalf("removal touched the wrong file: %v", err)
	}
	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].ID != "uid-b" {
		t.Fatalf("after removal: %+v", records)
	}
}

func TestWorkbuddyRemoveParkedAccount(t *testing.T) {
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-p.json" + disabledSuffix: credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-p", 0),
	})
	if err := c.RemoveAccount(context.Background(), "uid-p"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-p.json"+disabledSuffix)); !os.IsNotExist(err) {
		t.Fatalf("parked file still present, err = %v", err)
	}
}

func TestWorkbuddyRemoveAccountRejectsUnknownAndBlank(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	if err := c.RemoveAccount(context.Background(), "  "); err == nil {
		t.Fatal("blank id should be an error")
	}
	err := c.RemoveAccount(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want not found", err)
	}
}

// --- enable / disable -------------------------------------------------------

func TestWorkbuddyParkAndReviveRoundTrip(t *testing.T) {
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", "cn",
			"copilot.tencent.com", "uid-a", 0),
	})
	ctx := context.Background()

	if err := c.SetAccountEnabled(ctx, "uid-a", false); err != nil {
		t.Fatalf("park: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-a.json")); !os.IsNotExist(err) {
		t.Fatalf("live file should be gone, err = %v", err)
	}
	parkedPath := filepath.Join(dir, "wb-uid-a.json"+disabledSuffix)
	if _, err := os.Stat(parkedPath); err != nil {
		t.Fatalf("parked file missing: %v", err)
	}
	records, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].Enabled || records[0].State != disabledState {
		t.Fatalf("after parking: %+v", records)
	}
	// The parked view must keep reporting where the credential came from: a
	// panel-created file is named with panelFilePrefix, so parking it does not
	// turn it into something the operator found on disk.
	if got := records[0].Fields["origin"]; got != originPanel {
		t.Fatalf("parked origin = %v, want %q (fields %+v)", got, originPanel, records[0].Fields)
	}
	// A parked account must not be offered to the pool.
	if c.pool.Len() != 0 {
		t.Fatalf("pool still holds %d accounts after parking", c.pool.Len())
	}

	if err := c.SetAccountEnabled(ctx, "uid-a", true); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-a.json")); err != nil {
		t.Fatalf("revived file missing: %v", err)
	}
	if c.pool.Len() != 1 {
		t.Fatalf("pool holds %d accounts after reviving, want 1", c.pool.Len())
	}
}

func TestWorkbuddySetAccountEnabledIsIdempotent(t *testing.T) {
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
	})
	ctx := context.Background()

	if err := c.SetAccountEnabled(ctx, "uid-a", true); err != nil {
		t.Fatalf("enabling a live account: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, "uid-a", false); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, "uid-a", false); err != nil {
		t.Fatalf("parking twice: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, "uid-a", true); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, "uid-a", true); err != nil {
		t.Fatalf("reviving twice: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "wb-uid-a.json" {
		t.Fatalf("unexpected directory after idempotent toggles: %v", entries)
	}
}

func TestWorkbuddySetAccountEnabledUnknown(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	if err := c.SetAccountEnabled(context.Background(), "", true); err == nil {
		t.Fatal("blank id should be an error")
	}
	if err := c.SetAccountEnabled(context.Background(), "ghost", true); err == nil {
		t.Fatal("unknown id should be an error")
	}
}

// --- testing a credential ---------------------------------------------------

func TestWorkbuddyTestAccountUnknownID(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	if _, err := c.TestAccount(context.Background(), "ghost"); err == nil {
		t.Fatal("unknown id should be a Go error, not a result")
	}
}

func TestWorkbuddyTestAccountParkedIsAResult(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-uid-p.json" + disabledSuffix: credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-p", 0),
	})
	res, err := c.TestAccount(context.Background(), "uid-p")
	if err != nil {
		t.Fatalf("a parked account is a result, not an error: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "parked") {
		t.Fatalf("result = %+v", res)
	}
}

func TestWorkbuddyTestAccountReachesUpstream(t *testing.T) {
	c, _ := panelClient(t, catalogueRT(http.StatusOK, enterpriseModelsFixture), map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
	})
	res, err := c.TestAccount(context.Background(), "uid-a")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want OK", res)
	}
	if res.Model == "" {
		t.Fatalf("no model reported: %+v", res)
	}
	if !strings.Contains(res.Reply, "model(s) reachable") {
		t.Fatalf("reply = %q", res.Reply)
	}
	if res.AccountID != "uid-a" {
		t.Fatalf("account id = %q", res.AccountID)
	}
}

func TestWorkbuddyTestAccountRefusalIsAResult(t *testing.T) {
	c, _ := panelClient(t, catalogueRT(http.StatusUnauthorized, `{"code":1,"msg":"token expired"}`),
		map[string]string{
			"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
		})
	res, err := c.TestAccount(context.Background(), "uid-a")
	if err != nil {
		t.Fatalf("a refusal must not be a Go error: %v", err)
	}
	if res.OK {
		t.Fatalf("result = %+v, want a failure", res)
	}
	if strings.TrimSpace(res.Error) == "" {
		t.Fatal("a failed probe must explain itself")
	}
	if strings.Contains(res.Error, "access-token-abcdefgh") {
		t.Fatalf("error leaks the token: %q", res.Error)
	}
}

// --- refreshing -------------------------------------------------------------

func TestWorkbuddyRefreshAccountWithoutRefreshToken(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
	})
	results, err := c.RefreshAccount(context.Background(), "uid-a")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Error, "no refresh token") {
		t.Fatalf("error = %q", results[0].Error)
	}
}

func TestWorkbuddyRefreshAccountUnknownAndParked(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-uid-p.json" + disabledSuffix: credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", "cn", "copilot.tencent.com", "uid-p", 0),
	})
	ctx := context.Background()
	if _, err := c.RefreshAccount(ctx, "ghost"); err == nil {
		t.Fatal("unknown id should be an error")
	}
	results, err := c.RefreshAccount(ctx, "uid-p")
	if err != nil {
		t.Fatalf("a parked account is a result, not an error: %v", err)
	}
	if len(results) != 1 || results[0].OK || !strings.Contains(results[0].Error, "parked") {
		t.Fatalf("results = %+v", results)
	}
}

func TestWorkbuddyRefreshAllAccountsSkipsBlankRefreshToken(t *testing.T) {
	c, _ := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
		"wb-uid-b.json": credJSON("access-token-bbbbbbbb", "", "cn", "copilot.tencent.com", "uid-b", 0),
	})
	results, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.OK {
			t.Fatalf("%s should not have refreshed: %+v", r.AccountID, r)
		}
	}
}

// --- discovery and import ---------------------------------------------------

func TestWorkbuddyDiscoverListsOwnDirectory(t *testing.T) {
	isolateVendorStores(t)
	c, dir := panelClient(t, nil, map[string]string{
		"wb-uid-a.json": credJSON("access-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-a", 0),
	})
	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(found), found)
	}
	d := found[0]
	if d.Importable || !d.Imported {
		t.Fatalf("an account already in the data dir is imported, not importable: %+v", d)
	}
	if filepath.Dir(d.Path) != dir {
		t.Fatalf("path = %q, want it under %q", d.Path, dir)
	}
}

func TestWorkbuddyDiscoverSeparatesPlaintextFromEncrypted(t *testing.T) {
	root := isolateVendorStores(t)
	plain := seedVendorStore(t, root, "plain.json",
		credJSON("vendor-token-abcdefgh", "vendor-refresh-abcdefgh", "cn", "copilot.tencent.com", "uid-vendor-1", 0))
	seedVendorStore(t, root, "junk.json", `{"$wbEncrypted":"AAAA","v":1}`)

	c, _ := panelClient(t, nil, nil)
	entries, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byPath := map[string]core.DiscoveredCredential{}
	for _, d := range entries {
		byPath[d.Path] = d
	}
	p, ok := byPath[plain]
	if !ok {
		t.Fatalf("the plaintext credential was not discovered: %+v", entries)
	}
	if !p.Importable || p.Imported {
		t.Fatalf("plaintext entry = %+v, want importable", p)
	}
	var unreadable int
	for _, d := range entries {
		if d.Kind == "unreadable" {
			unreadable++
			if d.Importable {
				t.Fatalf("an unreadable file must not be importable: %+v", d)
			}
			if strings.TrimSpace(d.Note) == "" {
				t.Fatalf("an unreadable file must say why: %+v", d)
			}
		}
	}
	if unreadable == 0 {
		t.Fatalf("the encrypted envelope was not reported: %+v", entries)
	}
}

func TestWorkbuddyImportRejectsEmptyAndBadPaths(t *testing.T) {
	isolateVendorStores(t)
	c, _ := panelClient(t, nil, nil)
	ctx := context.Background()

	if _, err := c.Import(ctx, nil, false); err == nil {
		t.Fatal("no paths and no all flag should be an error")
	}
	if _, err := c.Import(ctx, nil, true); err == nil {
		t.Fatal("nothing importable should be an error")
	}
	results, err := c.Import(ctx, []string{filepath.Join(t.TempDir(), "missing.json")}, false)
	if err != nil {
		t.Fatalf("a bad path is a per-entry result, not a Go error: %v", err)
	}
	if len(results) != 1 || results[0].State != "error" {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Note, "cannot read") {
		t.Fatalf("note = %q", results[0].Note)
	}
}

func TestWorkbuddyImportCopiesIntoAccountsDir(t *testing.T) {
	root := isolateVendorStores(t)
	src := seedVendorStore(t, root, "plain.json",
		credJSON("vendor-token-abcdefgh", "vendor-refresh-abcdefgh", "cn", "copilot.tencent.com", "uid-vendor-1", 0))

	c, dir := panelClient(t, nil, nil)
	results, err := c.Import(context.Background(), []string{src}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}
	if results[0].ID != "uid-vendor-1" {
		t.Fatalf("imported id = %q", results[0].ID)
	}
	target := filepath.Join(dir, "wb-uid-vendor-1.json")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the copy was not written: %v", err)
	}
	// The source is read-only material: importing must not move it.
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("import moved the source file: %v", err)
	}
	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].ID != "uid-vendor-1" {
		t.Fatalf("imported account not usable: %+v", records)
	}
}

func TestWorkbuddyImportAllUsesDiscovery(t *testing.T) {
	root := isolateVendorStores(t)
	seedVendorStore(t, root, "plain.json",
		credJSON("vendor-token-abcdefgh", "", "cn", "copilot.tencent.com", "uid-vendor-1", 0))
	seedVendorStore(t, root, "junk.json", `{"$wbEncrypted":"AAAA"}`)

	c, dir := panelClient(t, nil, nil)
	results, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import all: %v", err)
	}
	if len(results) != 1 || results[0].ID != "uid-vendor-1" {
		t.Fatalf("results = %+v", results)
	}
	if _, err := os.Stat(filepath.Join(dir, "wb-uid-vendor-1.json")); err != nil {
		t.Fatalf("the copy was not written: %v", err)
	}
}

// --- safety -----------------------------------------------------------------

func TestWorkbuddyPanelNeverLeaksToken(t *testing.T) {
	const token = "super-secret-token-abcdefgh"
	isolateVendorStores(t)
	c, _ := panelClient(t, nil, nil)
	addPanel(t, c, map[string]string{
		"access_token":  token,
		"refresh_token": "super-secret-refresh-abcdefgh",
		"uid":           "uid-secret-1",
		"nickname":      "secret holder",
	})

	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	blob, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, secret := range []string{token, "super-secret-refresh-abcdefgh"} {
		if strings.Contains(string(blob), secret) {
			t.Fatalf("panel payload leaks %q: %s", secret, blob)
		}
	}

	fields, err := json.Marshal(c.AccountFields(context.Background()))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(fields), token) {
		t.Fatalf("form payload leaks the token: %s", fields)
	}
}

func TestWorkbuddyCapabilitiesAdvertised(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Manage {
		t.Fatal("workbuddy should advertise account management")
	}
	if !caps.Import {
		t.Fatal("workbuddy should advertise credential import")
	}
	if !caps.Login {
		t.Fatal("workbuddy drives the vendor's browser login from Go and must claim it")
	}
	if len(caps.Fields) != 9 {
		t.Fatalf("advertised %d fields, want 9", len(caps.Fields))
	}
}

func TestWorkbuddyParseExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"nonsense", 0},
		{"2026-12-31T00:00:00Z", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC).Unix()},
		{now.Format("2006-01-02 15:04:05"), now.Unix()},
		{now.Format("2006-01-02T15:04:05"), now.Unix()},
		{"1798761600", 1798761600},
		{"1798761600000", 1798761600},
	}
	for _, tc := range cases {
		if got := parseExpiry(tc.in); got != tc.want {
			t.Fatalf("parseExpiry(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestWorkbuddySanitizeFileComponent(t *testing.T) {
	cases := map[string]string{
		"uid-1":            "uid-1",
		"a/b\\c":           "a-b-c",
		"../../etc/passwd": "etc-passwd",
		"  spaced  ":       "spaced",
		"":                 "",
		"...":              "",
	}
	for in, want := range cases {
		if got := sanitizeFileComponent(in); got != want {
			t.Fatalf("sanitizeFileComponent(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeFileComponent(strings.Repeat("x", 200)); len(got) != 48 {
		t.Fatalf("long input was not truncated: %d", len(got))
	}
}

// TestWorkbuddyPanelFileNameKeepsExtension pins the shape the pool depends on:
// LoadAccounts globs "*.json", so a disambiguating suffix appended after the
// extension would produce a file the module writes and then never loads.
func TestWorkbuddyPanelFileNameKeepsExtension(t *testing.T) {
	for _, name := range []string{
		panelFileName("uid-1"),
		panelFileName(""),
		panelFileNameSuffixed("uid-1", "abc123"),
		panelFileNameSuffixed("", "abc123"),
	} {
		if !strings.HasSuffix(name, ".json") {
			t.Fatalf("%q does not end in .json", name)
		}
		if strings.Count(name, ".json") != 1 {
			t.Fatalf("%q has more than one .json segment", name)
		}
		if strings.ContainsAny(name, `/\:*?"<>|`) {
			t.Fatalf("%q contains a path separator or reserved character", name)
		}
	}
	if got := panelFileNameSuffixed("uid-1", "abc123"); got != "wb-uid-1-abc123.json" {
		t.Fatalf("panelFileNameSuffixed = %q", got)
	}
	if got := panelFileName("uid-1"); got != "wb-uid-1.json" {
		t.Fatalf("panelFileName = %q", got)
	}
}

// TestWorkbuddyAddAccountCollisionLandsInLoadableFile guards the same property
// from the outside: when the natural file name is taken, the fallback name must
// still be one the pool picks up, otherwise the panel reports a write that the
// module then ignores.
func TestWorkbuddyAddAccountCollisionLandsInLoadableFile(t *testing.T) {
	other := credJSON("other-token-aaaaaaa", "", "cn", "copilot.tencent.com", "someone-else", 0)
	c, dir := panelClient(t, nil, map[string]string{"wb-uid-x.json": other})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": "fresh-token-ccccccc", "uid": "uid-x"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "uid-x" {
		t.Fatalf("id = %q, want uid-x", rec.ID)
	}
	if rec.State != stateReady {
		t.Fatalf("collision fallback did not load: state = %q note = %q", rec.State, rec.Note)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var loadable int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			loadable++
		}
	}
	if loadable != 2 {
		t.Fatalf("expected 2 loadable *.json files, got %d: %v", loadable, entries)
	}
}
