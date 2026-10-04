package raccoon

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---- helpers -----------------------------------------------------------

// jwtWith builds an UNSIGNED JWT carrying the given claims. The module never
// verifies the signature, so a fixed dummy one is fine.
func jwtWith(t *testing.T, claims map[string]any) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	pl, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return head + "." + base64.RawURLEncoding.EncodeToString(pl) + ".sig"
}

func tokenExpiringIn(t *testing.T, d time.Duration) string {
	t.Helper()
	return jwtWith(t, map[string]any{"exp": time.Now().Add(d).Unix()})
}

func newTestClient(t *testing.T, dir, cfg string, hc *http.Client) *Client {
	t.Helper()
	var raw json.RawMessage
	if strings.TrimSpace(cfg) != "" {
		raw = json.RawMessage(cfg)
	}
	cl, err := New(core.Deps{DataDir: dir, Config: raw, HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", cl)
	}
	return c
}

func f64(v float64) *float64 { return &v }

// ---- expiry ------------------------------------------------------------

func TestExpiryFallsBackToJWTExp(t *testing.T) {
	now := time.Now()
	exp := now.Add(3 * time.Hour).Truncate(time.Second)
	c := credential{AccessToken: jwtWith(t, map[string]any{"exp": exp.Unix()})}

	ms, ok := c.expiresAtMs()
	if !ok {
		t.Fatal("expiresAtMs: a credential with no expires_at but a JWT exp must still report an expiry")
	}
	if got := time.UnixMilli(ms).Unix(); got != exp.Unix() {
		t.Fatalf("expiresAtMs = %d (%s), want the JWT exp %d", ms, time.UnixMilli(ms), exp.Unix())
	}
	if c.expired(now) {
		t.Fatal("a token three hours in the future must not be expired")
	}
	if c.needsRefresh(now, tokenRefreshWindow) {
		t.Fatal("a token three hours out is well outside the 300s renewal window")
	}
	if !c.needsRefresh(exp.Add(-60*time.Second), tokenRefreshWindow) {
		t.Fatal("60s before expiry must be inside the 300s renewal window")
	}
}

func TestExpiresAtWinsOverJWTExp(t *testing.T) {
	now := time.Now()
	// The token itself is already stale; the stored expires_at is the truth.
	c := credential{
		AccessToken: jwtWith(t, map[string]any{"exp": now.Add(-2 * time.Hour).Unix()}),
		ExpiresAt:   flexString(strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10)),
	}
	ms, ok := c.expiresAtMs()
	if !ok {
		t.Fatal("expiresAtMs: expected the stored expires_at to be used")
	}
	if got := time.UnixMilli(ms); got.Sub(now.Add(time.Hour)).Abs() > 2*time.Second {
		t.Fatalf("expiresAtMs = %s, want about %s", got, now.Add(time.Hour))
	}
	if c.expired(now) {
		t.Fatal("expires_at says an hour left, so the credential is not expired")
	}
}

func TestUnparseableExpiresAtFallsBackToJWTExp(t *testing.T) {
	now := time.Now()
	exp := now.Add(2 * time.Hour).Truncate(time.Second)
	c := credential{
		AccessToken: jwtWith(t, map[string]any{"exp": exp.Unix()}),
		ExpiresAt:   "not-a-number",
	}
	ms, ok := c.expiresAtMs()
	if !ok {
		t.Fatal("a garbage expires_at must fall through to the JWT exp, not disable expiry resolution")
	}
	if got := time.UnixMilli(ms).Unix(); got != exp.Unix() {
		t.Fatalf("expiresAtMs = %d, want the JWT exp %d", got, exp.Unix())
	}
}

func TestNoExpiryInformationIsNotExpired(t *testing.T) {
	now := time.Now()
	for _, c := range []credential{
		{},
		{AccessToken: "not-a-jwt"},
		{AccessToken: "a.b.c"},
		{AccessToken: jwtWith(t, map[string]any{"sub": "u1"})},
		{AccessToken: jwtWith(t, map[string]any{"exp": "later"})},
		{AccessToken: jwtWith(t, map[string]any{"exp": 0})},
		{AccessToken: jwtWith(t, map[string]any{"exp": -5})},
	} {
		if _, ok := c.expiresAtMs(); ok {
			t.Fatalf("expiresAtMs(%q) reported an expiry out of thin air", c.AccessToken)
		}
		if c.expired(now) {
			t.Fatalf("credential %q has no expiry information at all, so it must NOT be reported expired "+
				"(the server's 401 is the authority)", c.AccessToken)
		}
	}
}

func TestExpiredUsesJWTExp(t *testing.T) {
	now := time.Now()
	c := credential{AccessToken: jwtWith(t, map[string]any{"exp": now.Add(-time.Minute).Unix()})}
	if !c.expired(now) {
		t.Fatal("a token whose JWT exp has passed must be reported expired")
	}
}

func TestRefreshable(t *testing.T) {
	if (credential{}).refreshable() {
		t.Fatal("a credential with no refresh token must not be refreshable")
	}
	if (credential{RefreshToken: "   "}).refreshable() {
		t.Fatal("a whitespace-only refresh token must not count")
	}
	if !(credential{RefreshToken: "rt-1"}).refreshable() {
		t.Fatal("raccoon HAS a refresh endpoint, so a non-empty refresh token must be refreshable")
	}
}

func TestIdentityPrefersUserIDThenJWT(t *testing.T) {
	fromJWT := jwtWith(t, map[string]any{"user_id": "jwt-user", "exp": time.Now().Add(time.Hour).Unix()})
	if got := (credential{AccessToken: fromJWT}).identity(); got != "jwt-user" {
		t.Fatalf("identity = %q, want the JWT user_id", got)
	}
	if got := (credential{AccessToken: fromJWT, UserID: "explicit"}).identity(); got != "explicit" {
		t.Fatalf("identity = %q, want the explicit user_id to win", got)
	}
	if got := (credential{AccessToken: "garbage"}).identity(); got != "" {
		t.Fatalf("identity = %q, want empty", got)
	}
}

// ---- headers -----------------------------------------------------------

func TestRaccoonHeaders(t *testing.T) {
	h := raccoonHeaders(credential{
		AccessToken:    "AT",
		OfficeIdentity: "personal",
		DeviceID:       "deadbeef",
	}, "desktop-windows", "v1.0.35")

	if got := h.Get("Authorization"); got != "Bearer AT" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := h.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := h.Get("X-Raccoon-Language"); got != "zh" {
		t.Fatalf("X-Raccoon-Language = %q", got)
	}
	if got := h.Get("X-Org-Code"); got != "personal" {
		t.Fatalf("X-Org-Code = %q", got)
	}
	if got := h.Get("X-Client-Platform"); got != "desktop-windows" {
		t.Fatalf("X-Client-Platform = %q", got)
	}
	if got := h.Get("X-Client-Version"); got != "v1.0.35" {
		t.Fatalf("X-Client-Version = %q", got)
	}
	if got := h.Get("X-Client-Device-ID"); got != "deadbeef" {
		t.Fatalf("X-Client-Device-ID = %q", got)
	}

	// X-Org-Code is ALWAYS sent, empty for a personal account; the optional
	// client headers are omitted entirely when unknown.
	lean := raccoonHeaders(credential{AccessToken: "AT"}, "", "")
	if _, ok := lean["X-Org-Code"]; !ok {
		t.Fatal("X-Org-Code must always be present, even when empty")
	}
	if got := lean.Get("X-Org-Code"); got != "" {
		t.Fatalf("X-Org-Code = %q, want empty", got)
	}
	for _, k := range []string{"X-Client-Platform", "X-Client-Version", "X-Client-Device-ID"} {
		if _, ok := lean[http.CanonicalHeaderKey(k)]; ok {
			t.Fatalf("%s must be omitted when empty", k)
		}
	}
}

// ---- phone encryption --------------------------------------------------

// fixedIV is a deterministic IV so the ciphertext can be pinned.
var fixedIV = []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f}

// TestEncryptPhonePinnedVector pins the wire format exactly: AES-128 (a
// 16-byte key), CFB mode, no padding, output = Base64(iv || ciphertext).
// AES-256 or a different mode changes this string.
func TestEncryptPhonePinnedVector(t *testing.T) {
	const want = "AAECAwQFBgcICQoLDA0OD37WL6pKybd1FsS1"
	got, err := encryptPhone("13800138000", fixedIV)
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	if got != want {
		t.Fatalf("encryptPhone(13800138000, fixedIV) = %q, want %q", got, want)
	}
}

func TestEncryptPhoneRoundTrip(t *testing.T) {
	out, err := encryptPhone("13800138000", fixedIV)
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		t.Fatalf("output is not standard base64: %v", err)
	}
	if len(raw) < aes.BlockSize {
		t.Fatalf("output is %d bytes, too short to carry the IV", len(raw))
	}
	if !bytes.Equal(raw[:aes.BlockSize], fixedIV) {
		t.Fatal("the IV must be the first 16 bytes of the output")
	}
	block, err := aes.NewCipher([]byte(phoneCipherSecret))
	if err != nil {
		t.Fatalf("AES-128 key: %v", err)
	}
	plain := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCFBDecrypter(block, raw[:aes.BlockSize]).XORKeyStream(plain, raw[aes.BlockSize:])
	if string(plain) != "13800138000" {
		t.Fatalf("round trip produced %q", plain)
	}
}

func TestEncryptPhoneRejectsBadIV(t *testing.T) {
	if _, err := encryptPhone("13800138000", []byte("short")); err == nil {
		t.Fatal("a non-16-byte IV must be rejected")
	}
}

func TestEncryptPhoneRandomIsUnique(t *testing.T) {
	a, err := encryptPhoneRandom("13800138000")
	if err != nil {
		t.Fatalf("encryptPhoneRandom: %v", err)
	}
	b, err := encryptPhoneRandom("13800138000")
	if err != nil {
		t.Fatalf("encryptPhoneRandom: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same phone must not produce the same ciphertext (random IV)")
	}
}

// ---- config ------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"baseURL", cfg.baseURL(), raccoonAPIBase},
		{"userAgent", cfg.userAgent(), userAgent},
		{"platform", cfg.platform(), clientPlatform},
		{"version", cfg.version(), clientVersion},
		{"maxAttempts", cfg.maxAttempts(), defaultMaxAttempts},
		{"modelsTTL", cfg.modelsTTL(), defaultModelsTTL},
		{"modelsTimeout", cfg.modelsTimeout(), defaultModelsTimeout},
		{"chatTimeout", cfg.chatTimeout(), defaultChatTimeout},
		{"idleTimeout", cfg.idleTimeout(), defaultIdleTimeout},
		{"firstTokenTimeout", cfg.firstTokenTimeout(), defaultFirstTokenTimeout},
		{"cooldown", cfg.cooldown(), defaultCooldown},
		{"shortCooldown", cfg.shortCooldown(), defaultShortCooldown},
		{"quotaCooldown", cfg.quotaCooldown(), defaultQuotaCooldown},
		{"storeFlush", cfg.storeFlush(), defaultStoreFlush},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("default %s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if cfg.refreshMargin() != tokenRefreshWindow {
		t.Errorf("default refresh margin = %v, want %v", cfg.refreshMargin(), tokenRefreshWindow)
	}
	if n := len(cfg.credentialPaths()); n != 0 {
		t.Errorf("default credential paths = %d, want 0", n)
	}
}

func TestConfigOverridesAndUnits(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
		"base_url": "https://example.test/",
		"max_attempts": 7,
		"chat_timeout": "90s",
		"idle_timeout": 30,
		"models_ttl": "1m",
		"refresh_margin": 60,
		"credential_paths": ["a.json", "b.json"]
	}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := cfg.baseURL(); got != "https://example.test" {
		t.Errorf("baseURL = %q, want the trailing slash trimmed", got)
	}
	if got := cfg.maxAttempts(); got != 7 {
		t.Errorf("maxAttempts = %d, want 7", got)
	}
	if got := cfg.chatTimeout(); got != 90*time.Second {
		t.Errorf("chatTimeout = %v, want 90s", got)
	}
	if got := cfg.idleTimeout(); got != 30*time.Second {
		t.Errorf("idleTimeout = %v, want a bare number to mean SECONDS", got)
	}
	if got := cfg.modelsTTL(); got != time.Minute {
		t.Errorf("modelsTTL = %v, want 1m", got)
	}
	if got := cfg.refreshMargin(); got != time.Minute {
		t.Errorf("refreshMargin = %v, want 60s", got)
	}
	if got := cfg.credentialPaths(); len(got) != 2 || got[1] != "b.json" {
		t.Errorf("credentialPaths = %v", got)
	}
}

func TestConfigMalformedIsNotFatal(t *testing.T) {
	if _, err := parseConfig([]byte(`{"max_attempts":`)); err == nil {
		t.Fatal("a malformed config must be reported so the factory can log it")
	}
	// ...and the factory must still come up with defaults.
	c := newTestClient(t, t.TempDir(), `{"max_attempts":`, nil)
	if got := c.cfg.maxAttempts(); got != defaultMaxAttempts {
		t.Fatalf("after a malformed config maxAttempts = %d, want the default %d", got, defaultMaxAttempts)
	}
	if got := c.Status(context.Background()); got.Ready {
		t.Fatal("a module with no credential must not report Ready")
	}
}

func TestConfiguredAccountsSources(t *testing.T) {
	cfg, err := parseConfig([]byte(`{
		"accounts": [
			{"access_token": "tok-a", "label": "A"},
			{"access_token": "", "label": "empty"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	got := configuredAccounts(cfg)
	if len(got) != 1 {
		t.Fatalf("configuredAccounts = %d, want 1 (the empty access_token must be dropped)", len(got))
	}
	if got[0].Origin != originConfig || got[0].AccessToken != "tok-a" || got[0].Label != "A" {
		t.Fatalf("configuredAccounts[0] = %+v", got[0])
	}
	if got[0].OfficeIdentity != "personal" {
		t.Fatalf("office_identity default = %q, want personal", got[0].OfficeIdentity)
	}

	// The single-account shorthand.
	cfg2, _ := parseConfig([]byte(`{"access_token": "tok-b", "refresh_token": "rt-b"}`))
	got2 := configuredAccounts(cfg2)
	if len(got2) != 1 || got2[0].RefreshToken != "rt-b" {
		t.Fatalf("shorthand not honoured: %+v", got2)
	}

	// Environment only when nothing at all is configured.
	t.Setenv(envAccessToken, "tok-env")
	cfg3, _ := parseConfig(nil)
	got3 := configuredAccounts(cfg3)
	if len(got3) != 1 || got3[0].Origin != originEnv || got3[0].AccessToken != "tok-env" {
		t.Fatalf("env fallback not honoured: %+v", got3)
	}
	// ...and never when the config already names an account.
	got4 := configuredAccounts(cfg)
	if len(got4) != 1 || got4[0].AccessToken != "tok-a" {
		t.Fatalf("the env token must not be added when the config has accounts: %+v", got4)
	}
}

// ---- persistence -------------------------------------------------------

func TestCredentialPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	tok := tokenExpiringIn(t, 3*time.Hour)
	cfg := `{"access_token": "` + tok + `", "refresh_token": "RT-1", "user_id": "u-9", "nickname": "nick"}`
	c1 := newTestClient(t, dir, cfg, nil)

	ctx := context.Background()
	recs, err := c1.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Accounts = %d records, want 1", len(recs))
	}
	if recs[0].Identity != "u-9" {
		t.Fatalf("Identity = %q, want the credential user_id", recs[0].Identity)
	}

	store := filepath.Join(dir, accountsFile)
	data, err := os.ReadFile(store)
	if err != nil {
		t.Fatalf("the credential store must be written under the data dir: %v", err)
	}
	for _, want := range []string{"RT-1", tok, "u-9"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("the store does not contain %q: %s", want, data)
		}
	}

	// A fresh module over the same data dir, with NO config, must see the
	// persisted credential again.
	c2 := newTestClient(t, dir, "", nil)
	recs2, err := c2.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts (reload): %v", err)
	}
	if len(recs2) != 1 {
		t.Fatalf("reloaded Accounts = %d records, want the persisted 1", len(recs2))
	}
	if recs2[0].ID != recs[0].ID || recs2[0].Identity != "u-9" {
		t.Fatalf("reloaded record = %+v, want %+v", recs2[0], recs[0])
	}
}

func TestCredentialStoreIsUnderDataDirOnly(t *testing.T) {
	c := newTestClient(t, "", `{"access_token": "tok"}`, nil)
	if c.accountsPath != "" || c.statePath != "" {
		t.Fatalf("with no DataDir the module must not pick paths: %q %q", c.accountsPath, c.statePath)
	}
	if got := c.pool.len(); got != 1 {
		t.Fatalf("pool len = %d, want the configured account even without a data dir", got)
	}
}

func TestAccountIDIsStableAcrossSources(t *testing.T) {
	a := account{credential: credential{UserID: "u-1", AccessToken: "tok"}}
	if got := a.id(); got != "uid:u-1" {
		t.Fatalf("id = %q, want uid:u-1", got)
	}
	b := account{credential: credential{AccessToken: "0123456789abcdef"}}
	if got := b.id(); got != "tok:0123456789abcdef" {
		t.Fatalf("id = %q, want a token-derived id", got)
	}
	if a.id() != accountIDFor(credential{UserID: "u-1", AccessToken: "other"}) {
		t.Fatal("the same user id must produce the same account id regardless of token")
	}
}
