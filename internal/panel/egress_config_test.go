package panel

import "testing"

func TestValidateConfigAcceptsEgressModes(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"egress": map[string]any{"enabled": true, "mode": "client", "url": "socks5h://127.0.0.1:1080"}},
		{"egress": map[string]any{"enabled": true, "mode": "subscription", "url": "https://subscription.example/token"}},
		{"egress": map[string]any{"enabled": false, "mode": "client"}},
	} {
		if err := validateConfig(cfg); err != nil {
			t.Fatalf("validateConfig rejected %v: %v", cfg["egress"], err)
		}
	}
}

func TestValidateConfigRejectsBadEgressValues(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"egress": map[string]any{"enabled": true, "mode": "client", "url": "ftp://proxy.example"}},
		{"egress": map[string]any{"enabled": true, "mode": "subscription"}},
		{"egress": map[string]any{"enabled": "yes", "mode": "client", "url": "http://127.0.0.1:1"}},
	} {
		if err := validateConfig(cfg); err == nil {
			t.Fatalf("validateConfig accepted invalid egress block %v", cfg["egress"])
		}
	}
}

func TestRedactConfigMasksEgressSubscription(t *testing.T) {
	got := redactConfig(map[string]any{
		"egress": map[string]any{
			"enabled": true,
			"mode":    "subscription",
			"url":     "https://subscription.example/secret-token",
		},
	}).(map[string]any)
	egress, _ := got["egress"].(map[string]any)
	if egress["url"] != redactedPlaceholder {
		t.Fatalf("egress.url = %v, want %q", egress["url"], redactedPlaceholder)
	}
}

func TestEgressIsPartOfColdConfigKeys(t *testing.T) {
	if !coldConfigKeys["egress"] {
		t.Fatal("egress must be a cold config key")
	}
	found := false
	for _, k := range configKeyOrder {
		if k == "egress" {
			found = true
		}
	}
	if !found {
		t.Fatal("configKeyOrder must place egress among the known top-level keys")
	}
}
