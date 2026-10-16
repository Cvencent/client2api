package autoclaw

import (
	"os"
	"testing"
)

// config.example.json is a deliverable: it must parse as this module's own
// config and stay usable as a copy-paste starting point.
func TestConfigExampleParses(t *testing.T) {
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("reading config.example.json: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig(config.example.json): %v", err)
	}
	if !cfg.autoDiscover() {
		t.Error("auto_discover should default to true in the example")
	}
	if got := cfg.version(); got != autoclawVersion {
		t.Errorf("client_version = %q, want %q", got, autoclawVersion)
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(cfg.Accounts))
	}
	if cfg.SMSKeyword != defaultSMSKeyword {
		t.Errorf("sms_keyword = %q, want %q", cfg.SMSKeyword, defaultSMSKeyword)
	}
	if got := cfg.autoLoginTimeout(); got != defaultAutoLoginTimeout {
		t.Errorf("auto_login_timeout_seconds = %v, want %v", got, defaultAutoLoginTimeout)
	}
}
