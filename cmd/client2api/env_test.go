package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// clearEnv blanks every spelling of the named knobs, so a variable exported on
// the machine running the tests cannot make an assertion pass or fail by
// accident.  An empty value counts as unset, which is exactly the semantic
// under test elsewhere in this file.
func clearEnv(t *testing.T, shorts ...string) {
	t.Helper()
	for _, s := range shorts {
		for _, name := range envNames(s) {
			t.Setenv(name, "")
		}
	}
}

// configFile writes a config file with the given JSON and returns its path.
func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client2api.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestEnvironmentOverridesTheFile pins the precedence: defaults < file <
// environment, for every top-level knob the reference exposed.
func TestEnvironmentOverridesTheFile(t *testing.T) {
	clearEnv(t, "LISTEN", "API_KEY", "DATA_DIR", "PROXY", "SOFT_RATE", "SOFT_RATE_MAX",
		"PROMPT_MODE", "PROMPT_FILE", "EXPIRING_SOON", "PREFER_EXPIRING", "SANITIZE_FINGERPRINTS")

	path := configFile(t, `{
		"listen": "127.0.0.1:1111",
		"api_key": "file-key",
		"data_dir": "file-data",
		"proxy": "http://file.invalid:1",
		"prompt": {"mode": "custom", "file": "file.md"},
		"pool": {"expiring_soon": "1h", "prefer_expiring": false},
		"cooldown": {"soft_rate": "1s", "soft_rate_max": "2s"},
		"features": {"sanitize_blacklist_fingerprints": true}
	}`)

	t.Setenv("CLIENT2API_LISTEN", "127.0.0.1:2222")
	t.Setenv("CLIENT2API_API_KEY", "env-key")
	t.Setenv("CLIENT2API_DATA_DIR", "env-data")
	t.Setenv("CLIENT2API_PROXY", "http://env.invalid:2")
	t.Setenv("CLIENT2API_PROMPT_MODE", "passthrough")
	t.Setenv("CLIENT2API_PROMPT_FILE", "env.md")
	t.Setenv("CLIENT2API_EXPIRING_SOON", "9h")
	t.Setenv("CLIENT2API_PREFER_EXPIRING", "true")
	t.Setenv("CLIENT2API_SOFT_RATE", "3s")
	t.Setenv("CLIENT2API_SOFT_RATE_MAX", "4s")
	t.Setenv("CLIENT2API_SANITIZE_FINGERPRINTS", "false")

	cfg, created, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if created {
		t.Fatal("loadConfig claimed to create a file that already existed")
	}

	for _, tc := range []struct {
		knob string
		got  string
		want string
	}{
		{"listen", cfg.Listen, "127.0.0.1:2222"},
		{"api_key", cfg.APIKey, "env-key"},
		{"data_dir", cfg.DataDir, "env-data"},
		{"proxy", cfg.Proxy, "http://env.invalid:2"},
		{"prompt.mode", cfg.Prompt.Mode, "passthrough"},
		{"prompt.file", cfg.Prompt.File, "env.md"},
		{"pool.expiring_soon", cfg.Pool.ExpiringSoon, "9h"},
		{"cooldown.soft_rate", cfg.Cooldown.SoftRate, "3s"},
		{"cooldown.soft_rate_max", cfg.Cooldown.SoftRateMax, "4s"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want the environment's %q", tc.knob, tc.got, tc.want)
		}
	}
	if cfg.Pool.PreferExpiring == nil || !*cfg.Pool.PreferExpiring {
		t.Errorf("pool.prefer_expiring = %v, want the environment's true", cfg.Pool.PreferExpiring)
	}
	if cfg.Features.SanitizeBlacklistFingerprints == nil || *cfg.Features.SanitizeBlacklistFingerprints {
		t.Errorf("features.sanitize_blacklist_fingerprints = %v, want the environment's false",
			cfg.Features.SanitizeBlacklistFingerprints)
	}
}

// TestEnvironmentFillsWhatTheFileOmitted is the case a migration actually hits:
// the knob is not in the file at all, so the environment is the only thing that
// can set it, and the defaulting pass must not erase it afterwards.
func TestEnvironmentFillsWhatTheFileOmitted(t *testing.T) {
	clearEnv(t, "LISTEN", "PREFER_EXPIRING")
	path := configFile(t, `{"api_key": "file-key"}`)

	t.Setenv("CLIENT2API_LISTEN", "0.0.0.0:9999")
	t.Setenv("CLIENT2API_PREFER_EXPIRING", "false")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	cfg.applyDefaults()

	if cfg.Listen != "0.0.0.0:9999" {
		t.Errorf("listen = %q, want the environment's 0.0.0.0:9999 (applyDefaults must not overwrite it)", cfg.Listen)
	}
	if cfg.Pool.PreferExpiring == nil || *cfg.Pool.PreferExpiring {
		t.Errorf("pool.prefer_expiring = %v, want the environment's false to survive defaulting", cfg.Pool.PreferExpiring)
	}
	// The rest of the defaults are still filled in.
	if cfg.Panel.PackageDetailLimit != defaultPackageDetailLimit {
		t.Errorf("package_detail_limit = %d, want the default %d", cfg.Panel.PackageDetailLimit, defaultPackageDetailLimit)
	}
}

// TestTheReferenceVariableNamesStillWork: an operator moving off
// workbuddy2api-panel keeps the environment they already deploy.
func TestTheReferenceVariableNamesStillWork(t *testing.T) {
	clearEnv(t, "LISTEN", "PROMPT_MODE", "PREFER_EXPIRING", "USER_AGENT")
	path := configFile(t, `{}`)

	t.Setenv("WB2A_LISTEN", "127.0.0.1:3333")
	t.Setenv("WB2A_PROMPT_MODE", "custom")
	t.Setenv("WB2A_PREFER_EXPIRING", "true")
	t.Setenv("WB2A_USER_AGENT", "Reference/9")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:3333" {
		t.Errorf("listen = %q, want the WB2A_ spelling to apply", cfg.Listen)
	}
	if cfg.Prompt.Mode != "custom" {
		t.Errorf("prompt.mode = %q, want the WB2A_ spelling to apply", cfg.Prompt.Mode)
	}
	if cfg.Pool.PreferExpiring == nil || !*cfg.Pool.PreferExpiring {
		t.Errorf("pool.prefer_expiring = %v, want the WB2A_ spelling to apply", cfg.Pool.PreferExpiring)
	}
	if got := clientKey(t, cfg, "workbuddy", "user_agent"); got != "Reference/9" {
		t.Errorf("clients.workbuddy.user_agent = %v, want the WB2A_ spelling to apply", got)
	}
}

// TestTheSpecificPrefixWinsWhenBothAreSet: with both spellings present, the one
// that names this program decides.
func TestTheSpecificPrefixWinsWhenBothAreSet(t *testing.T) {
	clearEnv(t, "LISTEN")
	path := configFile(t, `{}`)

	t.Setenv("WB2A_LISTEN", "127.0.0.1:3333")
	t.Setenv("CLIENT2API_LISTEN", "127.0.0.1:4444")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:4444" {
		t.Errorf("listen = %q, want the CLIENT2API_ spelling to win", cfg.Listen)
	}
}

// TestAnEmptyVariableDoesNotOverride: a container that inherits a blank variable
// must not blank the file's setting.
func TestAnEmptyVariableDoesNotOverride(t *testing.T) {
	clearEnv(t, "LISTEN", "USER_AGENT")
	path := configFile(t, `{"listen": "127.0.0.1:1111", "clients": {"workbuddy": {"user_agent": "File/1"}}}`)

	t.Setenv("CLIENT2API_LISTEN", "")
	t.Setenv("WB2A_LISTEN", "")
	t.Setenv("CLIENT2API_USER_AGENT", "")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != "127.0.0.1:1111" {
		t.Errorf("listen = %q, want the file's value kept", cfg.Listen)
	}
	if got := clientKey(t, cfg, "workbuddy", "user_agent"); got != "File/1" {
		t.Errorf("clients.workbuddy.user_agent = %v, want the file's value kept", got)
	}
}

// TestAnUnparseableValueKeepsTheFileValue: a typo must not silently change a
// setting -- least of all a safety switch.
func TestAnUnparseableValueKeepsTheFileValue(t *testing.T) {
	clearEnv(t, "PREFER_EXPIRING", "TIMEOUT_SECONDS", "SANITIZE_FINGERPRINTS")
	path := configFile(t, `{
		"pool": {"prefer_expiring": false},
		"features": {"sanitize_blacklist_fingerprints": true},
		"clients": {"workbuddy": {"timeout_seconds": 11}}
	}`)

	t.Setenv("CLIENT2API_PREFER_EXPIRING", "maybe")
	t.Setenv("CLIENT2API_SANITIZE_FINGERPRINTS", "yes-please")
	t.Setenv("CLIENT2API_TIMEOUT_SECONDS", "soon")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Pool.PreferExpiring == nil || *cfg.Pool.PreferExpiring {
		t.Errorf("pool.prefer_expiring = %v, want the file's false kept", cfg.Pool.PreferExpiring)
	}
	if cfg.Features.SanitizeBlacklistFingerprints == nil || !*cfg.Features.SanitizeBlacklistFingerprints {
		t.Errorf("features.sanitize_blacklist_fingerprints = %v, want the file's true kept",
			cfg.Features.SanitizeBlacklistFingerprints)
	}
	if got := clientKey(t, cfg, "workbuddy", "timeout_seconds"); got != float64(11) {
		t.Errorf("clients.workbuddy.timeout_seconds = %v, want the file's 11 kept", got)
	}
}

// TestUpstreamIdentityLandsInTheWorkbuddyBlock covers the reference's upstream.*
// knobs, which belong to a client here.  The block is opaque JSON, so the
// override has to merge into it rather than replace it.
func TestUpstreamIdentityLandsInTheWorkbuddyBlock(t *testing.T) {
	clearEnv(t, "AUTH_DIR", "USER_AGENT", "CLIENT_VERSION", "CLI_VERSION", "CLIENT_NAME",
		"DEVICE_TOKEN", "DEVICE_TOKEN_FILE", "TIMEOUT_SECONDS", "HEADER_TIMEOUT_SECONDS",
		"IDLE_TIMEOUT_SECONDS", "PASSTHROUGH_IP")

	path := configFile(t, `{"clients": {
		"workbuddy": {"models": ["kept"], "client_name": "file-name"},
		"kimi": {"models": ["untouched"]}
	}}`)

	t.Setenv("CLIENT2API_AUTH_DIR", "creds")
	t.Setenv("CLIENT2API_USER_AGENT", "Agent/1")
	t.Setenv("CLIENT2API_CLIENT_VERSION", "9.9.9")
	t.Setenv("CLIENT2API_CLI_VERSION", "cli-9")
	t.Setenv("CLIENT2API_DEVICE_TOKEN", "dt-secret")
	t.Setenv("CLIENT2API_DEVICE_TOKEN_FILE", "dt.json")
	t.Setenv("CLIENT2API_CLIENT_NAME", "env-name")
	t.Setenv("CLIENT2API_TIMEOUT_SECONDS", "77")
	t.Setenv("CLIENT2API_HEADER_TIMEOUT_SECONDS", "8")
	t.Setenv("CLIENT2API_IDLE_TIMEOUT_SECONDS", "90")
	t.Setenv("CLIENT2API_PASSTHROUGH_IP", "true")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	for key, want := range map[string]any{
		"accounts_dir":           "creds",
		"user_agent":             "Agent/1",
		"client_version":         "9.9.9",
		"cli_version":            "cli-9",
		"device_token":           "dt-secret",
		"device_token_file":      "dt.json",
		"client_name":            "env-name",
		"timeout_seconds":        float64(77),
		"header_timeout_seconds": float64(8),
		"idle_timeout_seconds":   float64(90),
		"passthrough_ip":         true,
	} {
		if got := clientKey(t, cfg, "workbuddy", key); got != want {
			t.Errorf("clients.workbuddy.%s = %v, want %v", key, got, want)
		}
	}
	// A key the environment did not name survives the merge.
	if got := clientKey(t, cfg, "workbuddy", "models"); got == nil {
		t.Error("clients.workbuddy.models was dropped while patching the block")
	}
	// Another client's block is byte-identical: patching one module's settings
	// must not touch a sibling's.
	var kimi map[string]any
	if err := json.Unmarshal(cfg.Clients["kimi"], &kimi); err != nil {
		t.Fatalf("clients.kimi is no longer an object: %v", err)
	}
	if len(kimi) != 1 {
		t.Errorf("clients.kimi = %v, want it untouched", kimi)
	}
}

// TestAnUnparseableClientBlockIsLeftAlone: a block that is not an object is the
// operator's to fix.  Replacing it with a fragment of itself would hide the
// mistake behind a config that looks valid.
func TestAnUnparseableClientBlockIsLeftAlone(t *testing.T) {
	clearEnv(t, "USER_AGENT")
	path := configFile(t, `{"clients": {"workbuddy": "not an object"}}`)

	t.Setenv("CLIENT2API_USER_AGENT", "Agent/1")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := string(cfg.Clients["workbuddy"]); got != `"not an object"` {
		t.Errorf("clients.workbuddy = %s, want the operator's value left alone", got)
	}
}

// TestANullClientBlockIsFilledIn: a literal null decodes to a nil map, and
// assigning into that would panic.
func TestANullClientBlockIsFilledIn(t *testing.T) {
	clearEnv(t, "USER_AGENT")
	path := configFile(t, `{"clients": {"workbuddy": null}}`)

	t.Setenv("CLIENT2API_USER_AGENT", "Agent/1")

	cfg, _, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got := clientKey(t, cfg, "workbuddy", "user_agent"); got != "Agent/1" {
		t.Errorf("clients.workbuddy.user_agent = %v, want the override to apply to a null block", got)
	}
}

// TestAnInjectedKeyIsNotCopiedOntoDisk: a fresh install writes a file so the
// deployment is self-describing, but the environment's key is a secret the
// operator chose to keep out of the file.  The two must not be conflated.
func TestAnInjectedKeyIsNotCopiedOntoDisk(t *testing.T) {
	clearEnv(t, "LISTEN", "API_KEY", "DATA_DIR")
	path := filepath.Join(t.TempDir(), "client2api.json")

	t.Setenv("CLIENT2API_API_KEY", "env-key")
	t.Setenv("CLIENT2API_LISTEN", "0.0.0.0:5555")

	cfg, created, err := loadConfig(path, "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !created {
		t.Fatal("a fresh install should report creating the file")
	}
	if cfg.APIKey != "env-key" {
		t.Errorf("api_key = %q, want the environment's key", cfg.APIKey)
	}
	if cfg.Listen != "0.0.0.0:5555" {
		t.Errorf("listen = %q, want the environment's address", cfg.Listen)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back the generated config: %v", err)
	}
	var onDisk fileConfig
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("the generated config is not valid JSON: %v", err)
	}
	if onDisk.APIKey == "env-key" {
		t.Error("the environment's api_key was written to disk")
	}
	if len(onDisk.APIKey) != 32 {
		t.Errorf("the generated api_key is %d characters, want 32", len(onDisk.APIKey))
	}
}

// TestSessionStickyIsOnUnlessTheFileSaysOtherwise pins the default the
// reference applies (cmd/server/config.go:234): session_sticky.enabled is true
// when the file is silent.  It is a pointer precisely so "absent" and
// "explicitly false" stay distinguishable — if applyDefaults wrote true over an
// explicit false, an operator could never turn stickiness off from the file.
func TestSessionStickyIsOnUnlessTheFileSaysOtherwise(t *testing.T) {
	clearEnv(t, "LISTEN", "API_KEY", "DATA_DIR")

	// A file that says nothing about stickiness must come out enabled.
	silent, _, err := loadConfig(configFile(t, `{"listen":"127.0.0.1:1"}`), "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	silent.applyDefaults()
	if silent.SessionSticky.Enabled == nil {
		t.Fatal("applyDefaults left session_sticky.enabled unset: nil is not a usable default")
	}
	if !*silent.SessionSticky.Enabled {
		t.Error("session_sticky.enabled defaulted to false, want the reference's true")
	}
	// The window keeps its own defaults, and it is not the switch.
	if silent.SessionSticky.TTL != "30m" || silent.SessionSticky.GCInterval != "5m" {
		t.Errorf("session sticky window defaults = %q/%q, want 30m/5m",
			silent.SessionSticky.TTL, silent.SessionSticky.GCInterval)
	}
	// liveSettings must forward the switch, or no module ever hears about it.
	if live := silent.liveSettings(); live.AffinityEnabled == nil || !*live.AffinityEnabled {
		t.Error("liveSettings did not forward session_sticky.enabled=true")
	}

	// An explicit false has to survive applyDefaults untouched.
	off, _, err := loadConfig(configFile(t, `{"session_sticky":{"enabled":false}}`), "test")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	off.applyDefaults()
	if off.SessionSticky.Enabled == nil || *off.SessionSticky.Enabled {
		t.Fatal("an explicit session_sticky.enabled=false was overwritten by the default")
	}
	if live := off.liveSettings(); live.AffinityEnabled == nil || *live.AffinityEnabled {
		t.Error("liveSettings did not forward an explicit session_sticky.enabled=false")
	}
	// Turning the switch off must not silently zero the window: the two knobs
	// are independent, and a zeroed window would read as "expire immediately".
	if off.SessionSticky.TTL != "30m" {
		t.Errorf("ttl = %q after disabling stickiness, want the untouched default 30m",
			off.SessionSticky.TTL)
	}
}

// TestLoadConfigAtRefusesToInventAConfig: reload and an explicitly named
// -config both call this with create=false.  A missing file there must be an
// error, not a freshly generated api_key -- minting one is how a "reload"
// silently changed the credential the gateway accepts, and how a mistyped
// -config started a second installation in the working directory.
func TestLoadConfigAtRefusesToInventAConfig(t *testing.T) {
	clearEnv(t, "LISTEN", "API_KEY", "DATA_DIR")
	path := filepath.Join(t.TempDir(), "client2api.json")

	cfg, created, err := loadConfigAt(path, "test", false)
	if err == nil {
		t.Fatalf("loadConfigAt(create=false) returned %+v, want an error", cfg)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist so the caller can explain the typo", err)
	}
	if created {
		t.Error("created = true for a file that was never written")
	}
	if _, serr := os.Stat(path); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("the config file exists after a refused load (stat: %v)", serr)
	}
}

// TestLoadConfigAtStillCreatesWhenAsked: the boot path keeps the fresh-install
// behaviour, including creating the parent directory.
func TestLoadConfigAtStillCreatesWhenAsked(t *testing.T) {
	clearEnv(t, "LISTEN", "API_KEY", "DATA_DIR")
	path := filepath.Join(t.TempDir(), "nested", "client2api.json")

	cfg, created, err := loadConfigAt(path, "test", true)
	if err != nil {
		t.Fatalf("loadConfigAt(create=true): %v", err)
	}
	if !created {
		t.Fatal("created = false, want true for a missing file")
	}
	if len(cfg.APIKey) != 32 {
		t.Errorf("generated api_key has %d characters, want 32", len(cfg.APIKey))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the generated config was not written: %v", err)
	}
}

// clientKey decodes one key out of a client's opaque configuration block.
func clientKey(t *testing.T, cfg *fileConfig, client, key string) any {
	t.Helper()
	raw, ok := cfg.Clients[client]
	if !ok {
		t.Fatalf("clients.%s is absent", client)
	}
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatalf("clients.%s is not an object: %v", client, err)
	}
	return block[key]
}
