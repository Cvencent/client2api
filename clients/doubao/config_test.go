package doubao

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// config.example.json is a deliverable: it has to parse, and it has to agree
// with the documented defaults, or a copy-paste start would be a lie.
func TestExampleConfigParsesAndMatchesTheDocumentedDefaults(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("reading config.example.json: %v", err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config.example.json is not valid JSON for the module config: %v", err)
	}
	if got := cfg.webBase(); got != defaultWebBase {
		t.Errorf("web_base = %q, want %q", got, defaultWebBase)
	}
	if cfg.browserHeadless() {
		t.Errorf("browser_headless = true, want false (a login needs a window)")
	}
	if got := cfg.loginTimeout(); got != defaultLoginTimeout {
		t.Errorf("login_timeout = %v, want %v", got, defaultLoginTimeout)
	}
	if got := cfg.cooldown(); got != defaultCooldown {
		t.Errorf("cooldown = %v, want %v", got, defaultCooldown)
	}
	if got := cfg.authCooldown(); got != defaultAuthCooldown {
		t.Errorf("auth_cooldown = %v, want %v", got, defaultAuthCooldown)
	}
	if len(cfg.Models) != 0 {
		t.Errorf("models = %v, want empty", cfg.Models)
	}
}

func TestEmptyConfigUsesDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if got := cfg.webBase(); got != defaultWebBase {
		t.Errorf("webBase() = %q, want %q", got, defaultWebBase)
	}
	if got := cfg.requestTimeout(); got != defaultRequestTimeout {
		t.Errorf("requestTimeout() = %v, want %v", got, defaultRequestTimeout)
	}
	if got := cfg.chatTimeout(); got != defaultChatTimeout {
		t.Errorf("chatTimeout() = %v, want %v", got, defaultChatTimeout)
	}
	if got := cfg.probeTimeout(); got != defaultProbeTimeout {
		t.Errorf("probeTimeout() = %v, want %v", got, defaultProbeTimeout)
	}
	if got := cfg.modelsTTL(); got != defaultModelsTTL {
		t.Errorf("modelsTTL() = %v, want %v", got, defaultModelsTTL)
	}
}

// TestDurationFieldAcceptsStringAndNumber pins the two spellings the config
// accepts so a bare "30" is not silently a zero timeout.
func TestDurationFieldAcceptsStringAndNumber(t *testing.T) {
	var cfg config
	if err := json.Unmarshal([]byte(`{"request_timeout":"45s","chat_timeout":90}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := cfg.requestTimeout(); got != 45*time.Second {
		t.Errorf("request_timeout = %v, want 45s", got)
	}
	if got := cfg.chatTimeout(); got != 90*time.Second {
		t.Errorf("chat_timeout = %v, want 90s", got)
	}
}
