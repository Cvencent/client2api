package loomy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseConfigEmptyIsNotAnError(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(``), json.RawMessage(`null`), json.RawMessage(`{}`)} {
		cfg, err := parseConfig(raw)
		if err != nil {
			t.Errorf("parseConfig(%q) returned %v, want no error", string(raw), err)
		}
		if !reflect.DeepEqual(cfg, config{}) {
			t.Errorf("parseConfig(%q) = %+v, want the zero config", string(raw), cfg)
		}
	}
}

// TestParseConfigFailureIsReturnedNotFatal proves a bad config is reported to
// the caller -- which logs it and carries on with defaults -- rather than
// panicking or being silently swallowed.
func TestParseConfigFailureIsReturnedNotFatal(t *testing.T) {
	// A duration field that is neither a string nor a number cannot be
	// coerced, so it is a decode failure.
	cfg, err := parseConfig(json.RawMessage(`{"request_timeout": {"nope": true}}`))
	if err == nil {
		t.Fatalf("expected an error for a duration field that is an object")
	}
	if !reflect.DeepEqual(cfg, config{}) {
		t.Errorf("a failed parse must return the zero config, got %+v", cfg)
	}
	if !strings.Contains(err.Error(), "neither a duration string nor a number of seconds") {
		t.Errorf("unexpected error text: %q", err.Error())
	}

	if _, err := parseConfig(json.RawMessage(`{"accounts": "not a list"}`)); err == nil {
		t.Errorf("expected an error for a wrongly typed accounts field")
	}
	if _, err := parseConfig(json.RawMessage(`{oops`)); err == nil {
		t.Errorf("expected an error for malformed JSON")
	}
}

// TestUnparseableDurationFallsBackInsteadOfFailing pins the forgiving half of
// the same design: a string that simply is not a duration is kept verbatim and
// resolved to the default at use time, so a typo in one timeout does not take
// the whole module down.
func TestUnparseableDurationFallsBackInsteadOfFailing(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"request_timeout": "banana", "chat_timeout": "-5m"}`))
	if err != nil {
		t.Fatalf("a non-duration string must not fail the decode: %v", err)
	}
	if got := cfg.requestTimeout(); got != defaultRequestTimeout {
		t.Errorf("request_timeout \"banana\" = %s, want the default %s", got, defaultRequestTimeout)
	}
	if got := cfg.chatTimeout(); got != defaultChatTimeout {
		t.Errorf("a negative chat_timeout = %s, want the default %s", got, defaultChatTimeout)
	}
}

// TestDurationFieldAcceptsStringsAndSeconds pins the two accepted spellings.
func TestDurationFieldAcceptsStringsAndSeconds(t *testing.T) {
	var cfg config
	raw := `{
		"request_timeout": "90s",
		"models_timeout": 45,
		"chat_timeout": "1h30m",
		"idle_timeout": "0s",
		"cooldown": "2m"
	}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.requestTimeout(); got != 90*time.Second {
		t.Errorf("request_timeout = %s, want 90s", got)
	}
	if got := cfg.modelsTimeout(); got != 45*time.Second {
		t.Errorf("models_timeout from a bare number = %s, want 45s", got)
	}
	if got := cfg.chatTimeout(); got != 90*time.Minute {
		t.Errorf("chat_timeout = %s, want 1h30m", got)
	}
	// A zero or negative duration falls back to the default rather than
	// disabling the deadline.
	if got := cfg.idleTimeout(); got != defaultIdleTimeout {
		t.Errorf("idle_timeout \"0s\" = %s, want the default %s", got, defaultIdleTimeout)
	}
	if got := cfg.cooldown(); got != 2*time.Minute {
		t.Errorf("cooldown = %s, want 2m", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	var cfg config
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"request_timeout", cfg.requestTimeout(), defaultRequestTimeout},
		{"models_timeout", cfg.modelsTimeout(), defaultModelsTimeout},
		{"chat_timeout", cfg.chatTimeout(), defaultChatTimeout},
		{"idle_timeout", cfg.idleTimeout(), defaultIdleTimeout},
		{"first_byte_timeout", cfg.firstByteTimeout(), defaultFirstByteTimeout},
		{"models_ttl", cfg.modelsTTL(), defaultModelsTTL},
		{"probe_timeout", cfg.probeTimeout(), defaultProbeTimeout},
		{"cooldown", cfg.cooldown(), defaultCooldown},
		{"auth_cooldown", cfg.authCooldown(), defaultAuthCooldown},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s default = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
	if got := cfg.maxAttempts(); got != defaultMaxAttempts {
		t.Errorf("max_attempts default = %d, want %d", got, defaultMaxAttempts)
	}
	if cfg.apiBase() != defaultAPIBase {
		t.Errorf("api_base = %q", cfg.apiBase())
	}
	if cfg.accountBase() != defaultAccountBase {
		t.Errorf("account_base = %q", cfg.accountBase())
	}
	if cfg.accessKeyID() != defaultAccessKeyID {
		t.Errorf("access_key_id = %q", cfg.accessKeyID())
	}
	if cfg.accessKeySecret() != defaultAccessKeySecret {
		t.Errorf("access_key_secret = %q", cfg.accessKeySecret())
	}
	if cfg.appID() != defaultAppID {
		t.Errorf("app_id = %q", cfg.appID())
	}
	if sessionTTLSeconds != 14*24*60*60 {
		t.Errorf("sessionTTLSeconds = %d, want 14 days in seconds", sessionTTLSeconds)
	}
}

func TestMaxAttemptsFloor(t *testing.T) {
	// A configured zero or negative retry budget must not disable the loop
	// entirely.
	zero := 0
	cfg := config{MaxAttempts: &zero}
	if got := cfg.maxAttempts(); got < 1 {
		t.Errorf("max_attempts 0 = %d, want at least 1", got)
	}
	seven := 7
	cfg = config{MaxAttempts: &seven}
	if got := cfg.maxAttempts(); got != 7 {
		t.Errorf("max_attempts 7 = %d", got)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	t.Setenv("CLIENT2API_LOOMY_API_BASE", "https://example.test/api")
	t.Setenv("CLIENT2API_LOOMY_ACCOUNT_BASE", "https://account.test")
	t.Setenv("CLIENT2API_LOOMY_ACCESS_KEY_ID", "env-ak")
	t.Setenv("CLIENT2API_LOOMY_ACCESS_KEY_SECRET", "env-sk")
	t.Setenv("CLIENT2API_LOOMY_APP_ID", "ENVAPP")

	cfg := config{APIBase: "https://configured.test"}
	applyEnv(&cfg)

	if cfg.APIBase != "https://example.test/api" {
		t.Errorf("api_base = %q, want the environment to win", cfg.APIBase)
	}
	if cfg.AccountBase != "https://account.test" {
		t.Errorf("account_base = %q", cfg.AccountBase)
	}
	if cfg.AccessKeyID != "env-ak" || cfg.AccessKeySecret != "env-sk" || cfg.AppID != "ENVAPP" {
		t.Errorf("key material not overridden: %+v", cfg)
	}
}

func TestApplyEnvLeavesUnsetValuesAlone(t *testing.T) {
	for _, name := range []string{
		"CLIENT2API_LOOMY_API_BASE", "CLIENT2API_LOOMY_ACCOUNT_BASE",
		"CLIENT2API_LOOMY_ACCESS_KEY_ID", "CLIENT2API_LOOMY_ACCESS_KEY_SECRET",
		"CLIENT2API_LOOMY_APP_ID",
	} {
		// t.Setenv to "" then unset, so the ambient machine environment cannot
		// leak into the assertion.
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	cfg := config{APIBase: "https://configured.test", AppID: "CONFIGURED"}
	applyEnv(&cfg)
	if cfg.APIBase != "https://configured.test" || cfg.AppID != "CONFIGURED" {
		t.Errorf("applyEnv clobbered configured values: %+v", cfg)
	}
}

func TestConfiguredAccountsOrderAndDedup(t *testing.T) {
	t.Setenv("CLIENT2API_LOOMY_ACCESS_TOKEN", "env-token")
	t.Setenv("CLIENT2API_LOOMY_USERID", "env-user")

	cfg := config{
		Accounts: []accountConfig{
			{ID: "first", AccessToken: "tok-a", UserID: "user-a"},
			{ID: "first", AccessToken: "tok-duplicate", UserID: "user-dup"},
			{ID: "", AccessToken: "tok-b", UserID: "user-b"},
			{ID: "blank", AccessToken: "   "},
		},
	}
	accounts := configuredAccounts(cfg)

	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3 (the duplicate id and the blank token dropped): %+v", len(accounts), accounts)
	}
	if accounts[0].ID != "first" || accounts[0].AccessToken != "tok-a" {
		t.Errorf("the first entry with an id must win: %+v", accounts[0])
	}
	if accounts[1].ID != "loomy-user-b" {
		t.Errorf("an entry with no id should get a derived one, got %q", accounts[1].ID)
	}
	if accounts[2].ID != "loomy-env-user" {
		t.Errorf("the environment credential should be last, got %q", accounts[2].ID)
	}
	if accounts[2].AccessToken != "env-token" {
		t.Errorf("the environment token was not picked up: %+v", accounts[2])
	}
	for _, a := range accounts {
		if a.origin != originConfig {
			t.Errorf("%s: origin = %q, want %q", a.ID, a.origin, originConfig)
		}
		if a.Label == "" {
			t.Errorf("%s: a blank label must be filled from the id", a.ID)
		}
	}
}

func TestConfiguredAccountsSingleShorthand(t *testing.T) {
	t.Setenv("CLIENT2API_LOOMY_ACCESS_TOKEN", "")
	os.Unsetenv("CLIENT2API_LOOMY_ACCESS_TOKEN")

	cfg := config{AccessToken: "shorthand", UserID: "u9", Phone: "13800000000"}
	accounts := configuredAccounts(cfg)
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1: %+v", len(accounts), accounts)
	}
	if accounts[0].AccessToken != "shorthand" || accounts[0].UserID != "u9" {
		t.Errorf("shorthand not honoured: %+v", accounts[0])
	}
	if accounts[0].ID != "loomy-u9" {
		t.Errorf("derived id = %q, want loomy-u9", accounts[0].ID)
	}
}

func TestConfiguredAccountsRespectsExplicitEnabledFalse(t *testing.T) {
	no := false
	cfg := config{Accounts: []accountConfig{{ID: "a", AccessToken: "tok", Enabled: &no}}}
	accounts := configuredAccounts(cfg)
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts", len(accounts))
	}
	if accounts[0].Enabled {
		t.Errorf("an explicitly disabled entry must stay disabled")
	}
}

func TestParseExpiryMS(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"   ", 0},
		{"nonsense", 0},
		{"1750000000000", 1750000000000}, // already milliseconds
		{"1750000000", 1750000000000},    // seconds
		{"2026-09-26T12:00:00Z", 1790424000000},
		{"2026-09-26T12:00:00", 1790424000000},
		{"2026-09-26 12:00:00", 1790424000000},
		{"2026-09-26", 1790380800000},
	}
	for _, tc := range cases {
		if got := parseExpiryMS(tc.in); got != tc.want {
			t.Errorf("parseExpiryMS(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseExpiryMSUnknownIsNotExpired(t *testing.T) {
	// A missing expiry must never be read as "already expired": trying a
	// possibly-stale credential and letting the vendor answer 100002 is better
	// than blocking the operator on a guess.
	a := account{storedAccount: storedAccount{Enabled: true, AccessToken: "tok"}}
	now := time.Now()
	if got := parseExpiryMS(""); got != 0 {
		t.Fatalf("parseExpiryMS(\"\") = %d", got)
	}
	a.ExpiresAtMS = parseExpiryMS("")
	if a.expired(now) {
		t.Fatalf("an account with no known expiry must not be expired")
	}
	if !a.selectable(now) {
		t.Fatalf("an account with no known expiry must stay selectable")
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "b", "c"); got != "b" {
		t.Errorf("firstNonEmpty = %q, want b", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty = %q, want empty", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Errorf("firstNonEmpty() = %q, want empty", got)
	}
}

// TestParseConfigFromFile exercises the real decode path a module sees, from
// bytes on disk rather than a literal, and confirms unknown keys are tolerated.
func TestParseConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	blob := []byte(`{
		"api_base": "https://loomyad.example/api/v1",
		"chat_timeout": "5m",
		"max_attempts": 2,
		"something_we_do_not_know": {"nested": true},
		"accounts": [{"id": "x", "access_token": "tok", "userid": "u", "expires_at": "2026-09-26T12:00:00Z"}]
	}`)
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.APIBase != "https://loomyad.example/api/v1" {
		t.Errorf("api_base = %q", cfg.APIBase)
	}
	if got := cfg.chatTimeout(); got != 5*time.Minute {
		t.Errorf("chat_timeout = %s", got)
	}
	if got := cfg.maxAttempts(); got != 2 {
		t.Errorf("max_attempts = %d", got)
	}
	accounts := configuredAccounts(cfg)
	if len(accounts) != 1 || accounts[0].ExpiresAtMS != 1790424000000 {
		t.Fatalf("accounts = %+v", accounts)
	}
}
