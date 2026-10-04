package workbuddy

import (
	"encoding/json"
	"os"
	"testing"
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
	if !cfg.browserHeadless() {
		t.Errorf("browser_headless = %v, want true", cfg.browserHeadless())
	}
	if got := cfg.autoLoginTimeout(); got != defaultAutoLoginTimeout {
		t.Errorf("auto_login_timeout = %v, want %v", got, defaultAutoLoginTimeout)
	}
	if got := cfg.smsPolls(); got != defaultSMSPolls {
		t.Errorf("sms_polls = %d, want %d", got, defaultSMSPolls)
	}
	if got := cfg.smsInterval(); got != defaultSMSInterval {
		t.Errorf("sms_interval = %v, want %v", got, defaultSMSInterval)
	}
	if got := cfg.dupRetries(); got != defaultDupRetries {
		t.Errorf("dup_retries = %d, want %d", got, defaultDupRetries)
	}
}
