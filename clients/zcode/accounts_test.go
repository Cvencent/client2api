package zcode

// Offline tests for the panel account-management surface.
//
// Nothing here touches the network: every upstream call goes through
// fakeTransport, every credential store is a temporary directory, and the
// discovery paths are pointed at a temporary home and %APPDATA%.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"client2api/internal/core"
)

// Distinctive credentials: their prefixes can never collide with a hex
// fingerprint or a random account id, so a prefix check is meaningful.
const (
	testAPIKey   = "sk-zzz-primary-0123456789abcdef.ABCDEFGHIJKLMNOP"
	testOtherKey = "sk-zzz-secondary-fedcba9876543210.ZYXWVUTSRQPONMLK"
)

// panelEnv is a hermetic environment: a temporary home, a temporary %APPDATA%
// and a temporary data dir that survives reopening the client.
type panelEnv struct {
	home    string
	appdata string
	dataDir string
	cfg     string
}

func newPanelEnv(t *testing.T, configJSON string) *panelEnv {
	t.Helper()
	home := isolateHome(t)
	appdata := t.TempDir()
	t.Setenv("APPDATA", appdata)
	return &panelEnv{home: home, appdata: appdata, dataDir: t.TempDir(), cfg: testConfigWithoutBrowser(configJSON)}
}

// testConfigWithoutBrowser turns the built-in browser solver off unless a test
// asks for it explicitly.  Most tests predate the solver and assume a JWT
// credential is unusable; leaving the machine's Edge installation visible would
// make those tests launch a real browser and become machine-dependent.
func testConfigWithoutBrowser(configJSON string) string {
	if strings.Contains(configJSON, `"captcha_browser"`) {
		return configJSON
	}
	trimmed := strings.TrimSpace(configJSON)
	if trimmed == "" {
		return `{"captcha_browser":false}`
	}
	if !strings.HasPrefix(trimmed, "{") {
		return configJSON
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "{"))
	if rest == "" || rest == "}" {
		return `{"captcha_browser":false}`
	}
	return `{"captcha_browser":false,` + rest
}

func (e *panelEnv) client(t *testing.T, transport *fakeTransport) *Client {
	t.Helper()
	c, err := New(core.Deps{
		DataDir:    e.dataDir,
		Config:     json.RawMessage(e.cfg),
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}
	return client
}

func (e *panelEnv) v2Path(name string) string {
	return filepath.Join(e.home, ".zcode", "v2", name)
}

func (e *panelEnv) storePath() string {
	return filepath.Join(e.dataDir, managedFile)
}

func (e *panelEnv) statePath() string {
	return filepath.Join(e.dataDir, accountsFile)
}

func configWithAPIKey(id, key string) string {
	return `{"auto_discover":false,"accounts":[{"id":"` + id + `","label":"test key",` +
		`"provider":"zai","mode":"api_key","api_key":"` + key + `"}]}`
}

func configWithJWT(id, token string) string {
	return `{"auto_discover":false,"accounts":[{"id":"` + id + `","label":"plan",` +
		`"provider":"zai","mode":"jwt","jwt":"` + token + `"}]}`
}

// recordsByID indexes the panel view.
func recordsByID(t *testing.T, c *Client) map[string]core.AccountRecord {
	t.Helper()
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	out := make(map[string]core.AccountRecord, len(recs))
	for _, r := range recs {
		out[r.ID] = r
	}
	return out
}

// assertNoSecret fails if the rendered record contains the credential, or any
// prefix long enough to be useful.  The published fingerprint is a hash, not a
// slice, of the secret, so it cannot trip this.
func assertNoSecret(t *testing.T, rec core.AccountRecord, secret string) {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("account record leaks the credential: %s", raw)
	}
	for _, n := range []int{4, 8, 12, 16} {
		if len(secret) > n && strings.Contains(string(raw), secret[:n]) {
			t.Fatalf("account record leaks a %d-character prefix of the credential: %s", n, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// capabilities and schema
// ---------------------------------------------------------------------------

func TestPanelCapabilitiesAreAdvertised(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if _, ok := any(c).(core.AccountManager); !ok {
		t.Error("zcode must implement core.AccountManager")
	}
	if _, ok := any(c).(core.CredentialImporter); !ok {
		t.Error("zcode must implement core.CredentialImporter")
	}
	if _, ok := any(c).(core.LoginProvider); !ok {
		t.Error("zcode must advertise the panel sign-in flow it drives")
	}

	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Manage || !caps.Import || !caps.Login {
		t.Errorf("capabilities = %+v", caps)
	}
	if len(caps.Fields) == 0 {
		t.Error("the panel needs the field schema")
	}
}

func TestAccountFieldsSchema(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	fields := c.AccountFields(context.Background())
	byKey := make(map[string]core.FieldSpec, len(fields))
	for _, f := range fields {
		if f.Key == "" || f.Label == "" || f.Type == "" {
			t.Errorf("every field needs a key, a label and a type: %+v", f)
		}
		byKey[f.Key] = f
	}

	kind, ok := byKey[fieldKind]
	if !ok {
		t.Fatalf("no %q field in %+v", fieldKind, fields)
	}
	if kind.Type != "select" || !kind.Required || kind.Default != kindAPIKey {
		t.Errorf("%s = %+v", fieldKind, kind)
	}
	if !reflect.DeepEqual(kind.Options, []string{kindAPIKey, kindJWT}) {
		t.Errorf("%s options = %v", fieldKind, kind.Options)
	}

	if key := byKey[fieldAPIKey]; key.Type != "password" || key.Required {
		t.Errorf("%s = %+v", fieldAPIKey, key)
	}
	if jwt := byKey[fieldJWT]; jwt.Type != "textarea" || jwt.Required {
		t.Errorf("%s = %+v", fieldJWT, jwt)
	}
	if _, ok := byKey[fieldLabel]; !ok {
		t.Errorf("no %q field", fieldLabel)
	}
	region, ok := byKey[fieldRegion]
	if !ok {
		t.Fatalf("no %q field", fieldRegion)
	}
	if region.Type != "select" || region.Default != regionZai {
		t.Errorf("%s = %+v", fieldRegion, region)
	}
	if !reflect.DeepEqual(region.Options, []string{regionZai, regionBigmodel}) {
		t.Errorf("%s options = %v", fieldRegion, region.Options)
	}
	if _, ok := byKey[fieldBaseURL]; !ok {
		t.Errorf("no %q field", fieldBaseURL)
	}

	// The schema is static: it can never carry a credential.
	for _, f := range fields {
		assertNoSecret(t, core.AccountRecord{Fields: map[string]any{
			"key": f.Key, "label": f.Label, "help": f.Help, "placeholder": f.Placeholder,
		}}, testAPIKey)
	}
}

// ---------------------------------------------------------------------------
// AddAccount
// ---------------------------------------------------------------------------

func TestAddAccountRejectsBadInput(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	cases := []struct {
		name string
		spec core.AccountSpec
		// every one of these must appear in the error, so the panel can point
		// the operator at the offending field
		mustName []string
	}{
		{
			name:     "unknown kind",
			spec:     core.AccountSpec{Fields: map[string]string{fieldKind: "oauth"}},
			mustName: []string{fieldKind},
		},
		{
			name:     "api-key without a key",
			spec:     core.AccountSpec{Fields: map[string]string{fieldKind: kindAPIKey}},
			mustName: []string{fieldAPIKey},
		},
		{
			name:     "jwt without a token",
			spec:     core.AccountSpec{Fields: map[string]string{fieldKind: kindJWT}},
			mustName: []string{fieldJWT},
		},
		{
			name: "jwt submitted as an api key",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldKind: kindAPIKey, fieldAPIKey: makeJWT(`{"sub":"u-1"}`),
			}},
			mustName: []string{fieldAPIKey, kindJWT},
		},
		{
			name: "unknown region",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldAPIKey: testAPIKey, fieldRegion: "mars",
			}},
			mustName: []string{fieldRegion},
		},
		{
			name: "non-http base url",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldAPIKey: testAPIKey, fieldBaseURL: "ftp://example.com/anthropic",
			}},
			mustName: []string{fieldBaseURL},
		},
		{
			name: "base url without a host",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldAPIKey: testAPIKey, fieldBaseURL: "https://",
			}},
			mustName: []string{fieldBaseURL},
		},
		{
			name: "key with whitespace",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldAPIKey: "abc def ghi",
			}},
			mustName: []string{fieldAPIKey},
		},
		{
			name: "key with non-ascii",
			spec: core.AccountSpec{Fields: map[string]string{
				fieldAPIKey: "ключ",
			}},
			mustName: []string{fieldAPIKey},
		},
		{
			name: "account id with spaces",
			spec: core.AccountSpec{ID: "has space", Fields: map[string]string{
				fieldAPIKey: testAPIKey,
			}},
			mustName: []string{"id"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.AddAccount(context.Background(), tc.spec)
			if err == nil {
				t.Fatal("AddAccount accepted invalid input")
			}
			for _, want := range tc.mustName {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err.Error(), want)
				}
			}
			assertNoSecret(t, core.AccountRecord{
				ID: "error text", Label: err.Error(), Note: err.Error(),
			}, testAPIKey)
		})
	}

	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Fatalf("a rejected account must not be persisted: %+v", recs)
	}
	if _, err := os.Stat(env.storePath()); err == nil {
		t.Error("a rejected account must not create the store")
	}
}

func TestAddAccountPersistsAndNeverReturnsTheSecret(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldKind:   kindAPIKey,
		fieldAPIKey: testAPIKey,
		fieldLabel:  "my key",
		fieldRegion: regionBigmodel,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	if !strings.HasPrefix(rec.ID, managedIDPrefix) {
		t.Errorf("a hand-added account gets a %q id, got %q", managedIDPrefix, rec.ID)
	}
	if rec.Label != "my key" || !rec.Enabled || rec.State != stateReady {
		t.Errorf("record = %+v", rec)
	}
	if rec.Fields["kind"] != kindAPIKey {
		t.Errorf("kind = %v", rec.Fields["kind"])
	}
	if rec.Fields["provider"] != providerBigmodel {
		t.Errorf("provider = %v", rec.Fields["provider"])
	}
	if rec.Fields["managed"] != true {
		t.Errorf("managed = %v", rec.Fields["managed"])
	}
	if rec.Fields["fingerprint"] != credentialTag(testAPIKey) {
		t.Errorf("fingerprint = %v", rec.Fields["fingerprint"])
	}
	if rec.Fields["fingerprint"] == "" {
		t.Error("the fingerprint must be published so two keys can be told apart")
	}
	assertNoSecret(t, rec, testAPIKey)

	// The store is the one place the credential lives at rest.
	raw, err := os.ReadFile(env.storePath())
	if err != nil {
		t.Fatalf("read %s: %v", managedFile, err)
	}
	if !strings.Contains(string(raw), testAPIKey) {
		t.Fatalf("%s must hold the credential, got %s", managedFile, raw)
	}
	info, err := os.Stat(env.storePath())
	if err != nil {
		t.Fatalf("stat %s: %v", managedFile, err)
	}
	// Windows synthesises permission bits from the read-only attribute, so the
	// 0600 we ask for is not observable there; only assert where it means
	// something.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("%s mode = %o, want 600", managedFile, perm)
	}

	for _, r := range recordsByID(t, c) {
		assertNoSecret(t, r, testAPIKey)
	}
}

func TestAddAccountDefaultsAndLabelSanitising(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
		fieldLabel:  "  noisy\x00\x07label  ",
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.Label != "noisylabel" {
		t.Errorf("label = %q, want the control characters stripped", rec.Label)
	}
	if rec.Fields["kind"] != kindAPIKey {
		t.Errorf("kind must default to %q, got %v", kindAPIKey, rec.Fields["kind"])
	}
	if rec.Fields["provider"] != providerZai {
		t.Errorf("region must default to %q, got %v", regionZai, rec.Fields["provider"])
	}

	rec, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldKind:   kindJWT,
		fieldJWT:    makeJWT(`{"user_id":"u-1"}`),
		fieldRegion: regionBigmodel,
	}})
	if err != nil {
		t.Fatalf("AddAccount jwt: %v", err)
	}
	if rec.Label != regionBigmodel+" "+kindJWT {
		t.Errorf("default label = %q", rec.Label)
	}
	if rec.Fields["kind"] != kindJWT {
		t.Errorf("kind = %v", rec.Fields["kind"])
	}
}

// The panel's account form sends the label inside Fields, but a caller driving
// the panel API directly sets the top-level AccountSpec.Label.  Both must reach
// the row, and the form field wins when both are present.
func TestAddAccountAcceptsTheLabelOnTheSpec(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label:  "  from the spec  ",
		Fields: map[string]string{fieldAPIKey: testAPIKey},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.Label != "from the spec" {
		t.Errorf("label = %q, want the spec label to reach the row", rec.Label)
	}

	rec, err = c.AddAccount(context.Background(), core.AccountSpec{
		Label: "from the spec",
		Fields: map[string]string{
			fieldAPIKey: testAPIKey + "-second",
			fieldLabel:  "from the form",
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.Label != "from the form" {
		t.Errorf("label = %q, want the form field to win", rec.Label)
	}
}

func TestAddAccountRejectsADuplicateCredential(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	first, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	_, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey, fieldLabel: "the same key again",
	}})
	if err == nil {
		t.Fatal("the same credential must not be added twice")
	}
	if !strings.Contains(err.Error(), first.ID) {
		t.Errorf("the error must name the account that already holds it: %v", err)
	}

	// A credential the desktop client already provides is rejected too.
	writeZcodeV2(t, env.home, `{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic",`+
		`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`, "")
	env.cfg = `{"auto_discover":true}`
	c2 := env.client(t, &fakeTransport{})
	_, err = c2.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testOtherKey,
	}})
	if err == nil {
		t.Fatal("a credential the machine already provides must not be added twice")
	}
	if !strings.Contains(err.Error(), "zcode-config:builtin:bigmodel") {
		t.Errorf("the error must name the discovered account: %v", err)
	}
}

func TestAddAccountHonoursAnExplicitID(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		ID:     "team-key-1",
		Fields: map[string]string{fieldAPIKey: testAPIKey},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "team-key-1" {
		t.Errorf("id = %q", rec.ID)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{
		ID:     "team-key-1",
		Fields: map[string]string{fieldAPIKey: testOtherKey},
	}); err == nil {
		t.Fatal("a duplicate id must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

func TestAccountsListsEverySourceWithoutLeaking(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	jwt := makeJWT(`{"user_id":"u-1","sub":"u-1"}`)
	writeZcodeV2(t, env.home,
		`{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic","name":"BigModel",`+
			`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"zcodejwttoken":"`+jwt+`"}`)
	c := env.client(t, &fakeTransport{})

	hand, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey, fieldLabel: "hand added",
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	byID := recordsByID(t, c)
	if _, ok := byID["zcode-config:builtin:bigmodel"]; !ok {
		t.Errorf("the desktop config account must be listed: %v", keysOf(byID))
	}
	if _, ok := byID["zcode-credentials:zcodejwttoken"]; !ok {
		t.Errorf("the desktop jwt must be listed: %v", keysOf(byID))
	}
	if got, ok := byID[hand.ID]; !ok || got.Label != "hand added" {
		t.Errorf("the hand-added account must be listed: %v", keysOf(byID))
	}

	for _, r := range byID {
		assertNoSecret(t, r, testAPIKey)
		assertNoSecret(t, r, testOtherKey)
		assertNoSecret(t, r, jwt)
	}
}

func keysOf(m map[string]core.AccountRecord) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// RemoveAccount
// ---------------------------------------------------------------------------

func TestRemoveManagedAccountIsFinal(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := c.RemoveAccount(context.Background(), rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Fatalf("the account is still listed: %v", keysOf(recs))
	}

	raw, err := os.ReadFile(env.storePath())
	if err != nil {
		t.Fatalf("read %s: %v", managedFile, err)
	}
	if strings.Contains(string(raw), testAPIKey) {
		t.Fatalf("the credential is still in %s: %s", managedFile, raw)
	}
	if strings.Contains(string(raw), rec.ID) {
		t.Fatalf("the account id is still in %s: %s", managedFile, raw)
	}

	// A managed account needs no tombstone: nothing would bring it back.
	if raw, err := os.ReadFile(env.statePath()); err == nil && strings.Contains(string(raw), rec.ID) {
		t.Errorf("a managed account must not be tombstoned: %s", raw)
	}

	if recs := recordsByID(t, env.client(t, &fakeTransport{})); len(recs) != 0 {
		t.Fatalf("the deletion did not survive a restart: %v", keysOf(recs))
	}
}

func TestRemoveDiscoveredAccountLeavesATombstone(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	writeZcodeV2(t, env.home, `{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic",`+
		`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`, "")
	c := env.client(t, &fakeTransport{})

	const id = "zcode-config:builtin:bigmodel"
	if _, ok := recordsByID(t, c)[id]; !ok {
		t.Fatalf("the discovered account must be listed: %v", keysOf(recordsByID(t, c)))
	}
	if err := c.RemoveAccount(context.Background(), id); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if _, ok := recordsByID(t, c)[id]; ok {
		t.Fatal("the account is still listed after removal")
	}

	raw, err := os.ReadFile(env.statePath())
	if err != nil {
		t.Fatalf("read %s: %v", accountsFile, err)
	}
	if !strings.Contains(string(raw), "removed") || !strings.Contains(string(raw), id) {
		t.Fatalf("%s must remember the deletion: %s", accountsFile, raw)
	}
	if strings.Contains(string(raw), testOtherKey) {
		t.Fatalf("%s must never hold a credential: %s", accountsFile, raw)
	}

	// Discovery must not resurrect it.
	if _, ok := recordsByID(t, env.client(t, &fakeTransport{}))[id]; ok {
		t.Fatal("the deletion did not survive a restart")
	}
}

func TestRemoveAndEnableUnknownAccountFail(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if err := c.RemoveAccount(context.Background(), "nope"); err == nil {
		t.Error("removing an unknown account must fail")
	}
	if err := c.RemoveAccount(context.Background(), ""); err == nil {
		t.Error("removing with an empty id must fail")
	}
	if err := c.SetAccountEnabled(context.Background(), "nope", true); err == nil {
		t.Error("enabling an unknown account must fail")
	}
	if err := c.SetAccountEnabled(context.Background(), "", true); err == nil {
		t.Error("enabling with an empty id must fail")
	}
}

// ---------------------------------------------------------------------------
// SetAccountEnabled
// ---------------------------------------------------------------------------

func TestSetAccountEnabledTouchesStoreAndLivePool(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if got := c.pool.usableCount(); got != 1 {
		t.Fatalf("usableCount = %d, want 1", got)
	}

	if err := c.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := recordsByID(t, c)[rec.ID]; got.Enabled {
		t.Error("the record still reports the account as enabled")
	}
	if got := c.pool.usableCount(); got != 0 {
		t.Errorf("a disabled account must not be selectable: usableCount = %d", got)
	}
	if raw, err := os.ReadFile(env.storePath()); err != nil || !strings.Contains(string(raw), `"enabled": false`) {
		t.Errorf("the store must remember the toggle: %v %s", err, raw)
	}
	if got := recordsByID(t, env.client(t, &fakeTransport{}))[rec.ID]; got.Enabled {
		t.Error("the toggle did not survive a restart")
	}

	c2 := env.client(t, &fakeTransport{})
	if err := c2.SetAccountEnabled(context.Background(), rec.ID, true); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := recordsByID(t, c2)[rec.ID]; !got.Enabled {
		t.Error("re-enabling did not take")
	}
	if got := c2.pool.usableCount(); got != 1 {
		t.Errorf("usableCount after re-enabling = %d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// TestAccount
// ---------------------------------------------------------------------------

func TestTestAccountSucceeds(t *testing.T) {
	env := newPanelEnv(t, configWithAPIKey("cfg-1", testAPIKey))
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := env.client(t, transport)

	res, err := c.TestAccount(context.Background(), "cfg-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v", res)
	}
	if res.AccountID != "cfg-1" {
		t.Errorf("account id = %q", res.AccountID)
	}
	if res.Model != "GLM-5.3" {
		t.Errorf("model = %q, want the first configured model", res.Model)
	}
	if !strings.Contains(res.Reply, "Hello from mock upstream") {
		t.Errorf("reply = %q", res.Reply)
	}
	if transport.count() != 1 {
		t.Errorf("the probe must send exactly one request, sent %d", transport.count())
	}
	assertNoSecret(t, core.AccountRecord{Note: res.Reply, Fields: map[string]any{"error": res.Error}}, testAPIKey)
}

func TestTestAccountExpiredKeyIsAResultNotAnError(t *testing.T) {
	env := newPanelEnv(t, configWithAPIKey("cfg-1", testAPIKey))
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"code":1000,"msg":"令牌已过期或验证不正确"}`), nil
	}}
	c := env.client(t, transport)

	res, err := c.TestAccount(context.Background(), "cfg-1")
	if err != nil {
		t.Fatalf("an upstream refusal must be a result, not a Go error: %v", err)
	}
	if res.OK {
		t.Error("a 401 must not be reported as OK")
	}
	if res.Error == "" {
		t.Error("the result must explain the refusal")
	}
	if !strings.Contains(res.Error, "401") {
		t.Errorf("the result should carry the upstream status, got %q", res.Error)
	}
	assertNoSecret(t, core.AccountRecord{Note: res.Error}, testAPIKey)

	// A rejected probe is a real observation: the account is now invalid.
	if got := recordsByID(t, c)["cfg-1"]; got.State != stateInvalid {
		t.Errorf("state after a 401 = %q, want %q", got.State, stateInvalid)
	}
	if got := c.pool.usableCount(); got != 0 {
		t.Errorf("an invalid account must not be selectable: usableCount = %d", got)
	}
}

// A credential can be short enough that the module-wide redact() heuristic
// leaves it alone, so the panel result masks unconditionally.
func TestTestAccountMasksEvenAShortCredential(t *testing.T) {
	const short = "k3y1"
	env := newPanelEnv(t, configWithAPIKey("cfg-1", short))
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized,
			`{"error":{"message":"invalid api key k3y1 supplied"}}`), nil
	}}
	c := env.client(t, transport)

	res, err := c.TestAccount(context.Background(), "cfg-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Error("a 401 must not be reported as OK")
	}
	if strings.Contains(res.Error, short) {
		t.Fatalf("a short credential leaked into the result: %q", res.Error)
	}
	if !strings.Contains(res.Error, "***len=4") {
		t.Errorf("the credential must be masked in place, got %q", res.Error)
	}
}

func TestTestAccountRateLimitIsAResultNotAnError(t *testing.T) {
	env := newPanelEnv(t, configWithAPIKey("cfg-1", testAPIKey))
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`), nil
	}}
	c := env.client(t, transport)

	res, err := c.TestAccount(context.Background(), "cfg-1")
	if err != nil {
		t.Fatalf("a rate limit must be a result, not a Go error: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Errorf("result = %+v", res)
	}
	if got := recordsByID(t, c)["cfg-1"]; got.State != stateCooling {
		t.Errorf("state after a 429 = %q, want %q", got.State, stateCooling)
	}
}

func TestTestAccountUnknownIDFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := &fakeTransport{}
	c := env.client(t, transport)

	if _, err := c.TestAccount(context.Background(), "nope"); err == nil {
		t.Error("testing an unknown account must be a real error")
	}
	if _, err := c.TestAccount(context.Background(), ""); err == nil {
		t.Error("testing with an empty id must be a real error")
	}
	if transport.count() != 0 {
		t.Error("an unknown account must not reach the network")
	}
}

func TestTestAccountJWTWithoutASolverDoesNotCallOut(t *testing.T) {
	env := newPanelEnv(t, configWithJWT("jwt-1", makeJWT(`{"user_id":"u-1"}`)))
	transport := &fakeTransport{}
	c := env.client(t, transport)

	res, err := c.TestAccount(context.Background(), "jwt-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if res.OK {
		t.Error("a jwt account cannot be tested without a captcha solver")
	}
	if !strings.Contains(res.Error, "captcha_command") {
		t.Errorf("the result must name the missing setting, got %q", res.Error)
	}
	if transport.count() != 0 {
		t.Error("the module must not call out when the channel is disabled")
	}
}

// ---------------------------------------------------------------------------
// RefreshAccount
// ---------------------------------------------------------------------------

func TestRefreshAccountAPIKeyIsHonest(t *testing.T) {
	env := newPanelEnv(t, configWithAPIKey("cfg-1", testAPIKey))
	c := env.client(t, &fakeTransport{})

	results, err := c.RefreshAccount(context.Background(), "cfg-1")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].OK {
		t.Error("an api key has no renewal concept; OK must be false")
	}
	if results[0].AccountID != "cfg-1" || results[0].Error == "" {
		t.Errorf("result = %+v", results[0])
	}
	if !strings.Contains(results[0].Error, "api-key") {
		t.Errorf("the result must explain itself, got %q", results[0].Error)
	}
	assertNoSecret(t, core.AccountRecord{Note: results[0].Error}, testAPIKey)
}

func TestRefreshAccountUnknownIDFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if _, err := c.RefreshAccount(context.Background(), "nope"); err == nil {
		t.Error("refreshing an unknown account must be a real error")
	}
}

func TestRefreshAccountJWTPicksUpARenewedToken(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	const id = "zcode-credentials:zcodejwttoken"
	first := makeJWT(`{"user_id":"u-1","sub":"u-1"}`)
	second := makeJWT(`{"user_id":"u-1","sub":"u-1","renewed":2}`)

	writeZcodeV2(t, env.home, "", `{"zcodejwttoken":"`+first+`"}`)
	c := env.client(t, &fakeTransport{})
	if _, ok := recordsByID(t, c)[id]; !ok {
		t.Fatalf("the discovered jwt must be listed: %v", keysOf(recordsByID(t, c)))
	}

	results, err := c.RefreshAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || results[0].OK {
		t.Fatalf("nothing has been renewed yet, so this must be an honest failure: %+v", results)
	}
	if results[0].Error == "" {
		t.Error("the result must explain itself")
	}

	// The desktop client renews the token in its own store.
	writeZcodeV2(t, env.home, "", `{"zcodejwttoken":"`+second+`"}`)

	results, err = c.RefreshAccount(context.Background(), id)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("the renewed token must be picked up: %+v", results)
	}

	live := c.pool.accountByID(id)
	if live == nil {
		t.Fatal("the account vanished")
	}
	if live.jwt != second {
		t.Error("the live pool still holds the stale token")
	}
	if live.UserID != "u-1" {
		t.Errorf("user id = %q", live.UserID)
	}
}

func TestRefreshAccountWithoutAnIDCoversEveryAccount(t *testing.T) {
	env := newPanelEnv(t, configWithAPIKey("cfg-1", testAPIKey))
	writeZcodeV2(t, env.home, "", "")
	c := env.client(t, &fakeTransport{})

	results, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || results[0].AccountID != "cfg-1" {
		t.Fatalf("results = %+v", results)
	}

	empty := newPanelEnv(t, `{"auto_discover":false}`).client(t, &fakeTransport{})
	results, err = empty.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount on an empty pool: %v", err)
	}
	if results == nil || len(results) != 0 {
		t.Errorf("an empty pool must give an empty, non-nil list: %+v", results)
	}
}

// ---------------------------------------------------------------------------
// Discover and Import
// ---------------------------------------------------------------------------

func TestDiscoverReportsEveryLocationItLookedAt(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	jwt := makeJWT(`{"user_id":"u-1"}`)
	writeZcodeV2(t, env.home,
		`{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic","name":"BigModel",`+
			`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"zcodejwttoken":"`+jwt+`"}`)
	c := env.client(t, &fakeTransport{})

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	byPath := map[string]core.DiscoveredCredential{}
	for _, d := range found {
		byPath[d.Path] = d
		assertNoSecret(t, core.AccountRecord{ID: d.Path, Label: d.Label, Note: d.Note}, testOtherKey)
		assertNoSecret(t, core.AccountRecord{ID: d.Path, Label: d.Label, Note: d.Note}, jwt)
	}

	keyPath := env.v2Path("config.json") + "#provider.builtin:bigmodel"
	key, ok := byPath[keyPath]
	if !ok {
		t.Fatalf("the desktop config credential must be reported at %q, got %v", keyPath, keysOfDisc(found))
	}
	if !key.Importable || key.Imported || key.Kind != kindAPIKey {
		t.Errorf("config credential = %+v", key)
	}

	jwtPath := env.v2Path("credentials.json") + "#zcodejwttoken"
	plan, ok := byPath[jwtPath]
	if !ok {
		t.Fatalf("the desktop jwt must be reported at %q, got %v", jwtPath, keysOfDisc(found))
	}
	if !plan.Importable || plan.Imported || plan.Kind != kindJWT {
		t.Errorf("jwt credential = %+v", plan)
	}
}

func keysOfDisc(in []core.DiscoveredCredential) []string {
	out := make([]string, 0, len(in))
	for _, d := range in {
		out = append(out, d.Path)
	}
	return out
}

func TestDiscoverAndImportTheDesktopProfileIsNotImportable(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	profile := filepath.Join(env.appdata, "ZCode")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	c := env.client(t, &fakeTransport{})

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var seen *core.DiscoveredCredential
	for i := range found {
		if found[i].Path == profile {
			seen = &found[i]
		}
	}
	if seen == nil {
		t.Fatalf("the desktop profile at %q must be reported, got %v", profile, keysOfDisc(found))
	}
	if seen.Importable {
		t.Error("the Electron profile holds nothing this module can read, so it must not be importable")
	}
	if seen.Imported {
		t.Error("a non-importable location can never be imported")
	}
	if !strings.Contains(seen.Note, "leveldb") {
		t.Errorf("the note must say why: %q", seen.Note)
	}

	if _, err := c.Import(context.Background(), []string{profile}, false); err == nil {
		t.Error("importing the profile must fail")
	}
}

func TestImportAllThenAgainIsANoOp(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	jwt := makeJWT(`{"user_id":"u-1"}`)
	writeZcodeV2(t, env.home,
		`{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic",`+
			`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"zcodejwttoken":"`+jwt+`"}`)
	c := env.client(t, &fakeTransport{})

	imported, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 2 {
		t.Fatalf("imported = %+v", imported)
	}
	ids := map[string]bool{}
	for _, r := range imported {
		ids[r.ID] = true
		assertNoSecret(t, r, testOtherKey)
		assertNoSecret(t, r, jwt)
	}
	if !ids["zcode-config:builtin:bigmodel"] || !ids["zcode-credentials:zcodejwttoken"] {
		t.Errorf("an import must preserve the source ids, got %v", keysOf(recordsByID(t, c)))
	}

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range found {
		if d.Importable && !d.Imported {
			t.Errorf("%s should now report as imported", d.Path)
		}
	}

	again, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("importing twice must be a no-op, got %+v", again)
	}
}

func TestImportByPath(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	jwt := makeJWT(`{"user_id":"u-1"}`)
	writeZcodeV2(t, env.home,
		`{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic",`+
			`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"zcodejwttoken":"`+jwt+`"}`)
	c := env.client(t, &fakeTransport{})

	// A bare file path addresses every credential in that file.
	imported, err := c.Import(context.Background(), []string{env.v2Path("config.json")}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 || imported[0].ID != "zcode-config:builtin:bigmodel" {
		t.Fatalf("imported = %+v", imported)
	}

	// The fragment form addresses one credential inside a file.
	imported, err = c.Import(context.Background(), []string{env.v2Path("credentials.json") + "#zcodejwttoken"}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 1 || imported[0].ID != "zcode-credentials:zcodejwttoken" {
		t.Fatalf("imported = %+v", imported)
	}

	// Re-importing the same path is a no-op, not an error.
	imported, err = c.Import(context.Background(), []string{env.v2Path("config.json")}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 0 {
		t.Errorf("imported = %+v", imported)
	}
}

func TestImportWithNothingSelected(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	imported, err := c.Import(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if imported == nil || len(imported) != 0 {
		t.Errorf("an empty selection must give an empty, non-nil list: %+v", imported)
	}
}

func TestImportUnknownPathFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	missing := filepath.Join(env.home, "not-a-store.json")
	_, err := c.Import(context.Background(), []string{missing}, false)
	if err == nil {
		t.Fatal("importing a path that holds nothing must fail")
	}
	if !strings.Contains(err.Error(), filepath.Base(missing)) {
		t.Errorf("the error must name the path, got %v", err)
	}
}

func TestImportSkipsACredentialThePoolAlreadyHas(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	writeZcodeV2(t, env.home, `{"provider":{"builtin:bigmodel":{"enabled":true,"kind":"anthropic",`+
		`"options":{"apiKey":"`+testOtherKey+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`, "")
	c := env.client(t, &fakeTransport{})

	// Discovery already put this credential in the pool, so importing it is a
	// no-op rather than a duplicate-account error.
	imported, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(imported) != 0 {
		t.Errorf("imported = %+v", imported)
	}
	if got := len(recordsByID(t, c)); got != 1 {
		t.Errorf("the pool must still hold exactly one account, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Degenerate pools
// ---------------------------------------------------------------------------

func TestPanelSurfaceOnAnEmptyPool(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})
	ctx := context.Background()

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if recs == nil || len(recs) != 0 {
		t.Errorf("an empty pool must give an empty, non-nil list: %+v", recs)
	}

	results, err := c.RefreshAccount(ctx, "")
	if err != nil || len(results) != 0 {
		t.Errorf("RefreshAccount on an empty pool = %+v, %v", results, err)
	}

	found, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range found {
		if d.Imported {
			t.Errorf("nothing can be imported into an empty store: %+v", d)
		}
	}

	if _, err := c.Import(ctx, []string{filepath.Join(env.home, "nope.json")}, false); err == nil {
		t.Error("importing a missing path must fail")
	}
	if len(c.AccountFields(ctx)) == 0 {
		t.Error("the schema must be available even with no accounts")
	}
}

func TestPanelSurfaceSurvivesAnUnwritableDataDir(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	env.dataDir = ""
	c := env.client(t, &fakeTransport{})
	ctx := context.Background()

	rec, err := c.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{fieldAPIKey: testAPIKey}})
	if err != nil {
		t.Fatalf("AddAccount without a data dir: %v", err)
	}
	if rec.ID == "" {
		t.Error("the account must still be usable in memory")
	}
	if _, err := c.Accounts(ctx); err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if err := c.RemoveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if got := len(recordsByID(t, c)); got != 0 {
		t.Errorf("the account must be gone, got %d", got)
	}
}
