package codearts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// config_test.go covers the config surface: the defaults an operator gets
// without writing anything, the duration flexibility, and the credential
// flattening.  A typo'd config must not take the module down, so the decode
// failure path is asserted explicitly.

// TestParseConfigEmptyIsNotAnError checks that a module with no config at all
// still works: the gateway passes an empty RawMessage when nothing is set.
func TestParseConfigEmptyIsNotAnError(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("null"), json.RawMessage("{}")} {
		cfg, err := parseConfig(raw)
		if err != nil {
			t.Fatalf("parseConfig(%q): %v", raw, err)
		}
		if got := cfg.maxAttempts(); got != defaultMaxAttempts {
			t.Errorf("parseConfig(%q).maxAttempts() = %d, want %d", raw, got, defaultMaxAttempts)
		}
	}
}

// TestParseConfigDecodeFailureIsReportedNotFatal checks the contract the
// constructor relies on: a broken config produces an error the caller logs, and
// the caller then carries on with defaults.  Nothing here may panic.
func TestParseConfigDecodeFailureIsReportedNotFatal(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{`),
		json.RawMessage(`"a string, not an object"`),
		json.RawMessage(`{"max_attempts": "not a number"}`),
		json.RawMessage(`{"models_ttl": {}}`),
	} {
		if _, err := parseConfig(raw); err == nil {
			t.Errorf("parseConfig(%s) accepted invalid config", raw)
		}
	}
	// The defaults are still usable after the failure, which is what makes
	// carrying on safe.
	var zero config
	if zero.chatTimeout() != defaultChatTimeout || zero.maxTokens() != defaultMaxTokens {
		t.Error("the zero config does not answer with defaults")
	}
}

// TestParseConfigReadsEveryKey checks each documented key actually lands,
// because a key that silently does nothing is worse than a missing one.
func TestParseConfigReadsEveryKey(t *testing.T) {
	raw := json.RawMessage(`{
	  "base_url": "https://mirror.example.com",
	  "chat_url": "https://mirror.example.com/custom/chat",
	  "models_url": "https://mirror.example.com/custom/models",
	  "sts_url": "https://mirror.example.com/custom/sts",
	  "gateway_url": "https://mirror.example.com/custom/gateway",
	  "ticket_url": "https://mirror.example.com/custom/ticket",
	  "user_agent": "custom-agent/1.0",
	  "max_attempts": 7,
	  "models_ttl": "90m",
	  "models_timeout": "11s",
	  "chat_timeout": "17m",
	  "idle_timeout": "45s",
	  "first_token_timeout": "61s",
	  "chunk_timeout": "62s",
	  "cooldown": "13s",
	  "short_cooldown": "3s",
	  "quota_cooldown": "5h",
	  "refresh_margin": "9m",
	  "login_timeout": "21m",
	  "queue_poll_interval": "4s",
	  "queue_max_wait": "6m",
	  "max_tokens": 1234,
	  "reasoning_effort": "off",
	  "models": ["a-model", "b-model"],
	  "login": true,
	  "access_key_id": "AKSINGLE",
	  "secret_access_key": "SKSINGLE",
	  "security_token": "TOKSINGLE",
	  "refresh_token": "RTSINGLE",
	  "expires_at": "2030-01-02T03:04:05Z",
	  "domain_id": "DOMSINGLE",
	  "user_id": "USERSINGLE",
	  "user_name": "single user"
	}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	for name, got := range map[string]any{
		"baseURL":          cfg.baseURL(),
		"chatURL":          cfg.chatURL(),
		"modelsURL":        cfg.modelsURL(),
		"stsURL":           cfg.stsURL(),
		"gatewayConfigURL": cfg.gatewayConfigURL(),
		"ticketURL":        cfg.ticketURL(),
		"userAgent":        cfg.userAgent(),
		"maxAttempts":      cfg.maxAttempts(),
		"modelsTTL":        cfg.modelsTTL(),
		"modelsTimeout":    cfg.modelsTimeout(),
		"chatTimeout":      cfg.chatTimeout(),
		"idleTimeout":      cfg.idleTimeout(),
		"firstTokenWait":   cfg.firstTokenWait(),
		"chunkWait":        cfg.chunkWait(),
		"cooldown":         cfg.cooldown(),
		"shortCooldown":    cfg.shortCooldown(),
		"quotaCooldown":    cfg.quotaCooldown(),
		"refreshMargin":    cfg.refreshMargin(),
		"loginTimeout":     cfg.loginTimeout(),
		"queuePollEvery":   cfg.queuePollEvery(),
		"queueMaxWait":     cfg.queueMaxWait(),
		"maxTokens":        cfg.maxTokens(),
	} {
		switch name {
		case "baseURL":
			if got != "https://mirror.example.com" {
				t.Errorf("%s = %v", name, got)
			}
		case "chatURL":
			if got != "https://mirror.example.com/custom/chat" {
				t.Errorf("%s = %v", name, got)
			}
		case "modelsURL":
			if got != "https://mirror.example.com/custom/models" {
				t.Errorf("%s = %v", name, got)
			}
		case "stsURL":
			if got != "https://mirror.example.com/custom/sts" {
				t.Errorf("%s = %v", name, got)
			}
		case "gatewayConfigURL":
			if got != "https://mirror.example.com/custom/gateway" {
				t.Errorf("%s = %v", name, got)
			}
		case "ticketURL":
			if got != "https://mirror.example.com/custom/ticket" {
				t.Errorf("%s = %v", name, got)
			}
		case "userAgent":
			if got != "custom-agent/1.0" {
				t.Errorf("%s = %v", name, got)
			}
		case "maxAttempts":
			if got != 7 {
				t.Errorf("%s = %v", name, got)
			}
		case "maxTokens":
			if got != 1234 {
				t.Errorf("%s = %v", name, got)
			}
		default:
			if got == time.Duration(0) || got == nil {
				t.Errorf("%s was not set", name)
			}
		}
	}
	for key, want := range map[string]time.Duration{
		"modelsTTL":      90 * time.Minute,
		"modelsTimeout":  11 * time.Second,
		"chatTimeout":    17 * time.Minute,
		"idleTimeout":    45 * time.Second,
		"firstTokenWait": 61 * time.Second,
		"chunkWait":      62 * time.Second,
		"cooldown":       13 * time.Second,
		"shortCooldown":  3 * time.Second,
		"quotaCooldown":  5 * time.Hour,
		"refreshMargin":  9 * time.Minute,
		"loginTimeout":   21 * time.Minute,
		"queuePollEvery": 4 * time.Second,
		"queueMaxWait":   6 * time.Minute,
	} {
		var got time.Duration
		switch key {
		case "modelsTTL":
			got = cfg.modelsTTL()
		case "modelsTimeout":
			got = cfg.modelsTimeout()
		case "chatTimeout":
			got = cfg.chatTimeout()
		case "idleTimeout":
			got = cfg.idleTimeout()
		case "firstTokenWait":
			got = cfg.firstTokenWait()
		case "chunkWait":
			got = cfg.chunkWait()
		case "cooldown":
			got = cfg.cooldown()
		case "shortCooldown":
			got = cfg.shortCooldown()
		case "quotaCooldown":
			got = cfg.quotaCooldown()
		case "refreshMargin":
			got = cfg.refreshMargin()
		case "loginTimeout":
			got = cfg.loginTimeout()
		case "queuePollEvery":
			got = cfg.queuePollEvery()
		case "queueMaxWait":
			got = cfg.queueMaxWait()
		}
		if got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
	if !cfg.reasoningOff() {
		t.Error("reasoning_effort=off did not register")
	}
	if !cfg.loginRequested() {
		t.Error("login=true did not register")
	}
	if got := cfg.fallbackModels(); len(got) != 2 || got[0] != "a-model" || got[1] != "b-model" {
		t.Errorf("fallbackModels() = %v", got)
	}
	accts := configuredAccounts(cfg)
	if len(accts) != 1 {
		t.Fatalf("configuredAccounts() returned %d accounts, want 1", len(accts))
	}
	if accts[0].AccessKeyID != "AKSINGLE" || accts[0].SecretAccessKey != "SKSINGLE" {
		t.Errorf("the single-account shorthand was not read: %+v", accts[0])
	}
	if accts[0].UserName != "single user" || accts[0].DomainID != "DOMSINGLE" {
		t.Errorf("the optional credential fields were not read: %+v", accts[0])
	}
}

// TestDefaultsAreTheDocumentedOnes pins every default, so a change to one is a
// deliberate change to a test rather than an accident.
func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	var c config
	for name, got := range map[string]time.Duration{
		"modelsTTL":      c.modelsTTL(),
		"modelsTimeout":  c.modelsTimeout(),
		"chatTimeout":    c.chatTimeout(),
		"idleTimeout":    c.idleTimeout(),
		"firstTokenWait": c.firstTokenWait(),
		"chunkWait":      c.chunkWait(),
		"cooldown":       c.cooldown(),
		"shortCooldown":  c.shortCooldown(),
		"quotaCooldown":  c.quotaCooldown(),
		"refreshMargin":  c.refreshMargin(),
		"loginTimeout":   c.loginTimeout(),
		"queuePollEvery": c.queuePollEvery(),
		"queueMaxWait":   c.queueMaxWait(),
	} {
		var want time.Duration
		switch name {
		case "modelsTTL":
			want = defaultModelsTTL
		case "modelsTimeout":
			want = defaultModelsTimeout
		case "chatTimeout":
			want = defaultChatTimeout
		case "idleTimeout":
			want = defaultIdleTimeout
		case "firstTokenWait":
			want = defaultFirstTokenWait
		case "chunkWait":
			want = defaultChunkWait
		case "cooldown":
			want = defaultCooldown
		case "shortCooldown":
			want = defaultShortCooldown
		case "quotaCooldown":
			want = defaultQuotaCooldown
		case "refreshMargin":
			want = defaultRefreshMargin
		case "loginTimeout":
			want = defaultLoginTimeout
		case "queuePollEvery":
			want = defaultQueuePollEvery
		case "queueMaxWait":
			want = defaultQueueMaxWait
		}
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	if c.maxAttempts() != defaultMaxAttempts {
		t.Errorf("maxAttempts = %d", c.maxAttempts())
	}
	if c.maxTokens() != defaultMaxTokens {
		t.Errorf("maxTokens = %d", c.maxTokens())
	}
	if c.maxTokens() != 65536 {
		t.Errorf("the documented default max_tokens is 65536, got %d", c.maxTokens())
	}
	if c.reasoningOff() {
		t.Error("reasoning must be on by default")
	}
	if c.loginRequested() {
		t.Error("login must not be requested by default")
	}
	if got := c.baseURL(); got != defaultBaseURL {
		t.Errorf("baseURL = %q", got)
	}
	if got := c.chatURL(); got != defaultBaseURL+"/api/v2/chat/completions" {
		t.Errorf("chatURL = %q", got)
	}
	if got := c.modelsURL(); got != defaultBaseURL+"/v1/model/builtin" {
		t.Errorf("modelsURL = %q", got)
	}
	if got := c.queueStatusURL(); got != defaultBaseURL+"/api/v1/queue/status" {
		t.Errorf("queueStatusURL = %q", got)
	}
	if got := c.gatewayConfigURL(); got != defaultGatewayConfigURL {
		t.Errorf("gatewayConfigURL = %q", got)
	}
	if got := c.stsURL(); got != stsTokenEndpoint {
		t.Errorf("stsURL = %q", got)
	}
	if got := c.userAgent(); got != defaultUserAgent {
		t.Errorf("userAgent = %q", got)
	}
	// The fallback catalogue is the built-in list, non-empty and deduplicated.
	fallback := c.fallbackModels()
	if len(fallback) == 0 {
		t.Fatal("the default fallback catalogue is empty")
	}
	seen := map[string]bool{}
	for _, id := range fallback {
		if seen[id] {
			t.Errorf("the fallback catalogue repeats %q", id)
		}
		seen[id] = true
	}
}

// TestMaxAttemptsAndMaxTokensRejectNonPositive checks that a zero or negative
// value falls back to the default rather than disabling retries or capping the
// answer at nothing.
func TestMaxAttemptsAndMaxTokensRejectNonPositive(t *testing.T) {
	for _, n := range []int{0, -1} {
		attempts, tokens := n, n
		c := config{MaxAttempts: &attempts, MaxTokens: &tokens}
		if c.maxAttempts() != defaultMaxAttempts {
			t.Errorf("max_attempts=%d gave %d", n, c.maxAttempts())
		}
		if c.maxTokens() != defaultMaxTokens {
			t.Errorf("max_tokens=%d gave %d", n, c.maxTokens())
		}
	}
}

// TestDurationFieldAcceptsStringsAndSeconds is the flexibility the brief asks
// for: `"90s"`, `"2h"`, `90` and `"90"` must all work, and an unparseable value
// must fall back to the default rather than to zero.
func TestDurationFieldAcceptsStringsAndSeconds(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{`{"chat_timeout": "90s"}`, 90 * time.Second},
		{`{"chat_timeout": "2h"}`, 2 * time.Hour},
		{`{"chat_timeout": 90}`, 90 * time.Second},
		{`{"chat_timeout": "90"}`, 90 * time.Second},
		{`{"chat_timeout": "1m30s"}`, 90 * time.Second},
		{`{"chat_timeout": 0}`, defaultChatTimeout},
		{`{"chat_timeout": null}`, defaultChatTimeout},
		{`{"chat_timeout": ""}`, defaultChatTimeout},
		{`{"chat_timeout": "not a duration"}`, defaultChatTimeout},
		{`{"chat_timeout": "0s"}`, defaultChatTimeout},
		{`{"chat_timeout": "-5s"}`, defaultChatTimeout},
	} {
		cfg, err := parseConfig(json.RawMessage(tc.raw))
		if err != nil {
			t.Fatalf("parseConfig(%s): %v", tc.raw, err)
		}
		if got := cfg.chatTimeout(); got != tc.want {
			t.Errorf("parseConfig(%s).chatTimeout() = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestDurationOr checks the free function directly, including that an empty
// string and an unparseable one both answer the default.
func TestDurationOr(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"", 7 * time.Second},
		{"   ", 7 * time.Second},
		{"3s", 3 * time.Second},
		{"1500ms", 1500 * time.Millisecond},
		{"12", 12 * time.Second},
		{"-4", 7 * time.Second},
		{"0", 7 * time.Second},
		{"nonsense", 7 * time.Second},
	} {
		if got := durationOr(tc.in, 7*time.Second); got != tc.want {
			t.Errorf("durationOr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestTruthy checks the env-var spelling of a boolean.
func TestTruthy(t *testing.T) {
	for _, in := range []string{"1", "true", "TRUE", "yes", "Yes", "on", "y", " tRuE ", " on "} {
		if !truthy(in) {
			t.Errorf("truthy(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "0", "false", "no", "off", "n", "2", "maybe"} {
		if truthy(in) {
			t.Errorf("truthy(%q) = true, want false", in)
		}
	}
}

// TestParseExpiry covers every shape an operator or the login flow may write,
// including the two that mean "unknown" and must never be read as expired.
func TestParseExpiry(t *testing.T) {
	want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	for _, in := range []string{
		"2030-01-02T03:04:05Z",
		"2030-01-02T03:04:05.000Z",
		"2030-01-02T11:04:05+08:00",
		"2030-01-02 03:04:05",
	} {
		if got := parseExpiry(in); got != want {
			t.Errorf("parseExpiry(%q) = %d, want %d", in, got, want)
		}
	}
	if got := parseExpiry("1893456000"); got != 1893456000 {
		t.Errorf("unix seconds: parseExpiry = %d", got)
	}
	if got := parseExpiry("1893456000000"); got != 1893456000 {
		t.Errorf("unix milliseconds: parseExpiry = %d", got)
	}
	for _, in := range []string{"", "   ", "not a date", "yesterday", "0", "-1"} {
		if got := parseExpiry(in); got != 0 {
			t.Errorf("parseExpiry(%q) = %d, want 0 (unknown)", in, got)
		}
	}
	// A zero expiry must not be read as expired.
	a := account{AccessKeyID: "a", SecretAccessKey: "b", ExpiresAt: 0}
	if a.expired(time.Now()) {
		t.Error("an unknown expiry was treated as expired")
	}
}

// TestFirstTokenWaitEnvOverride checks the reference implementation's
// environment names win, in milliseconds, so a machine already tuned for that
// project keeps its numbers.
func TestFirstTokenWaitEnvOverride(t *testing.T) {
	t.Setenv(envLegacyFirstTokenWait, "1234")
	t.Setenv(envLegacyChunkWait, "5678")
	var c config
	if got := c.firstTokenWait(); got != 1234*time.Millisecond {
		t.Errorf("firstTokenWait = %v, want 1234ms", got)
	}
	if got := c.chunkWait(); got != 5678*time.Millisecond {
		t.Errorf("chunkWait = %v, want 5678ms", got)
	}
	// A garbage value falls back to the config, then to the default.
	t.Setenv(envLegacyFirstTokenWait, "not a number")
	if got := c.firstTokenWait(); got != defaultFirstTokenWait {
		t.Errorf("firstTokenWait with garbage env = %v, want the default", got)
	}
	t.Setenv(envLegacyFirstTokenWait, "-1")
	if got := c.firstTokenWait(); got != defaultFirstTokenWait {
		t.Errorf("firstTokenWait with a negative env = %v, want the default", got)
	}
}

// TestIdleTimeoutDrivesBothWatchdogs checks that `idle_timeout` is a real key
// and not documentation: it is the coarse knob for both stream watchdogs, and
// the two specific keys win over it.
//
// It also pins the deliberate absence of an effect by default.  The two
// watchdog defaults are different numbers, so a generic `idle_timeout` default
// must not silently replace the long first-token budget the vendor's queue
// needs.
func TestIdleTimeoutDrivesBothWatchdogs(t *testing.T) {
	// Unset: the two reference defaults, and idle_timeout changes nothing.
	var empty config
	if got := empty.firstTokenWait(); got != defaultFirstTokenWait {
		t.Errorf("unset firstTokenWait = %v, want %v", got, defaultFirstTokenWait)
	}
	if got := empty.chunkWait(); got != defaultChunkWait {
		t.Errorf("unset chunkWait = %v, want %v", got, defaultChunkWait)
	}

	// Only the coarse knob set: it drives both.
	var coarse config
	coarse.IdleTimeout = "45s"
	if got := coarse.firstTokenWait(); got != 45*time.Second {
		t.Errorf("firstTokenWait with idle_timeout = %v, want 45s", got)
	}
	if got := coarse.chunkWait(); got != 45*time.Second {
		t.Errorf("chunkWait with idle_timeout = %v, want 45s", got)
	}

	// The specific keys win over the coarse one.
	var specific config
	specific.IdleTimeout = "45s"
	specific.FirstTokenWait = "61s"
	specific.ChunkWait = "62s"
	if got := specific.firstTokenWait(); got != 61*time.Second {
		t.Errorf("firstTokenWait = %v, want the specific 61s", got)
	}
	if got := specific.chunkWait(); got != 62*time.Second {
		t.Errorf("chunkWait = %v, want the specific 62s", got)
	}

	// The environment beats both, as it does everywhere else in this file.
	t.Setenv(envLegacyFirstTokenWait, "1234")
	if got := specific.firstTokenWait(); got != 1234*time.Millisecond {
		t.Errorf("firstTokenWait with the env set = %v, want 1234ms", got)
	}
}

// TestConfiguredAccountsPrecedence checks the documented order: the list form
// first, then the single-account shorthand, then the environment, with
// incomplete entries dropped.
func TestConfiguredAccountsPrecedence(t *testing.T) {
	t.Setenv(envAccessKeyID, "AKENV")
	t.Setenv(envSecretAccessKey, "SKENV")
	t.Setenv(envSecurityToken, "TOKENV")
	t.Setenv(envRefreshToken, "RTENV")
	t.Setenv(envExpiresAt, "2030-01-02T03:04:05Z")

	cfg := config{
		Accounts: []accountConfig{
			{AccessKeyID: "AKLIST1", SecretAccessKey: "SKLIST1", Label: "first"},
			{AccessKeyID: "AKLIST2", SecretAccessKey: "SKLIST2", Disabled: true},
			{AccessKeyID: "AKINCOMPLETE"},   // no secret: dropped
			{SecretAccessKey: "SKNOSECRET"}, // no key: dropped
		},
		AccessKeyID:     "AKSHORTHAND",
		SecretAccessKey: "SKSHORTHAND",
		ExpiresAt:       "2031-05-06T07:08:09Z",
	}
	got := configuredAccounts(cfg)
	if len(got) != 4 {
		t.Fatalf("configuredAccounts returned %d accounts, want 4: %+v", len(got), got)
	}
	if got[0].AccessKeyID != "AKLIST1" || got[1].AccessKeyID != "AKLIST2" {
		t.Errorf("the list form did not come first: %s, %s", got[0].AccessKeyID, got[1].AccessKeyID)
	}
	if got[0].Note != "first" {
		t.Errorf("the list entry's label was not kept: %q", got[0].Note)
	}
	if !got[1].Disabled {
		t.Error("a disabled list entry lost its park switch")
	}
	if got[2].AccessKeyID != "AKSHORTHAND" {
		t.Errorf("the single-account shorthand was not read: %s", got[2].AccessKeyID)
	}
	if got[2].ExpiresAt != time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC).Unix() {
		t.Errorf("the shorthand expiry was not parsed: %d", got[2].ExpiresAt)
	}
	if got[3].AccessKeyID != "AKENV" || got[3].SecretAccessKey != "SKENV" {
		t.Errorf("the environment credential was not read: %+v", got[3])
	}
	if got[3].SecurityToken != "TOKENV" || got[3].RefreshToken != "RTENV" {
		t.Errorf("the environment's optional fields were not read: %+v", got[3])
	}
}

// TestConfiguredAccountsEnvOnlyNeedsBothHalves checks that half an environment
// credential is ignored rather than producing an account that cannot sign.
func TestConfiguredAccountsEnvOnlyNeedsBothHalves(t *testing.T) {
	t.Setenv(envAccessKeyID, "AKENV")
	t.Setenv(envSecretAccessKey, "")
	if got := configuredAccounts(config{}); len(got) != 0 {
		t.Errorf("a half-supplied environment credential produced %d accounts", len(got))
	}
	t.Setenv(envAccessKeyID, "")
	t.Setenv(envSecretAccessKey, "SKENV")
	if got := configuredAccounts(config{}); len(got) != 0 {
		t.Errorf("a half-supplied environment credential produced %d accounts", len(got))
	}
}

// TestBaseURLOverridesTheEnvironment checks that config beats the environment
// and the environment beats the built-in default.
func TestBaseURLOverridesTheEnvironment(t *testing.T) {
	t.Setenv(envBaseURL, "https://env.example.com")
	var c config
	if got := c.baseURL(); got != "https://env.example.com" {
		t.Errorf("baseURL = %q, want the environment value", got)
	}
	c.BaseURL = "https://config.example.com/"
	if got := c.baseURL(); got != "https://config.example.com/" {
		t.Errorf("baseURL = %q, want the config value", got)
	}
	// endpoint() never produces a double slash.
	if got := c.endpoint("/v1/x"); got != "https://config.example.com/v1/x" {
		t.Errorf("endpoint = %q", got)
	}
	// A trailing slash on a URL override is trimmed for chat/models.
	c.ChatURL = "https://chat.example.com/v2/"
	if got := c.chatURL(); got != "https://chat.example.com/v2" {
		t.Errorf("chatURL = %q", got)
	}
	c.ModelsURL = "https://models.example.com/v1/"
	if got := c.modelsURL(); got != "https://models.example.com/v1" {
		t.Errorf("modelsURL = %q", got)
	}
}

// TestReasoningEffortSpellings checks every spelling the module accepts for
// turning the hidden chain of thought off.
func TestReasoningEffortSpellings(t *testing.T) {
	for _, in := range []string{"off", "OFF", " off ", "none", "disabled", "false"} {
		c := config{ReasoningEffort: in}
		if !c.reasoningOff() {
			t.Errorf("reasoning_effort=%q did not turn reasoning off", in)
		}
	}
	for _, in := range []string{"", "auto", "on", "high", "true"} {
		c := config{ReasoningEffort: in}
		if c.reasoningOff() {
			t.Errorf("reasoning_effort=%q turned reasoning off", in)
		}
	}
}

// TestLoginRequestedFromEnv checks CLIENT2API_CODEARTS_LOGIN.
func TestLoginRequestedFromEnv(t *testing.T) {
	t.Setenv(envLogin, "1")
	var c config
	if !c.loginRequested() {
		t.Error("CLIENT2API_CODEARTS_LOGIN=1 did not request a login")
	}
	t.Setenv(envLogin, "0")
	if c.loginRequested() {
		t.Error("CLIENT2API_CODEARTS_LOGIN=0 requested a login")
	}
}

// TestFallbackModelsTrimsAndDedupes checks a hand-written model list is
// normalised before it reaches the picker.
func TestFallbackModelsTrimsAndDedupes(t *testing.T) {
	c := config{ModelList: []string{" a ", "", "a", "  ", "b"}}
	got := c.fallbackModels()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("fallbackModels() = %v, want [a b]", got)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Errorf("fallbackModels() = %v", got)
	}
}
