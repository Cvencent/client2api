package panel

import "testing"

// TestRedactionTreatsPlatformNamesAsNames pins the difference between a config
// key that names a platform and a config key that names a credential field.
// `platforms.tokenrhythm` used to match the secret-key heuristic because the
// operator's platform name contains "token", so GET /panel/api/config replaced
// the whole policy with the <redacted> sentinel.  The platform page then read no
// priority (a number input cannot hold "<redacted>"), displayed it blank, and
// compared every save against that blank baseline -- so re-saving the default 0
// wrote nothing at all.
func TestRedactionTreatsPlatformNamesAsNames(t *testing.T) {
	cfg := map[string]any{
		"platforms": map[string]any{
			"tokenrhythm": map[string]any{
				"priority":           -1,
				"max_in_flight":      4,
				"account_priorities": map[string]any{"tokenrhythm:acct-1": -1},
				"account_notes":      map[string]any{"tokenrhythm:acct-1": "我的号"},
				"api_token":          "plainopaqueabc123",
				"disabled_models":    []any{"dead-model"},
			},
		},
	}
	platforms, ok := redactConfig(cfg).(map[string]any)["platforms"].(map[string]any)
	if !ok {
		t.Fatalf("platforms = %#v, want a map", redactConfig(cfg).(map[string]any)["platforms"])
	}
	tr, ok := platforms["tokenrhythm"].(map[string]any)
	if !ok {
		t.Fatalf("platforms.tokenrhythm = %#v; a platform name is not a credential field", platforms["tokenrhythm"])
	}
	if got := tr["priority"]; got != -1 {
		t.Errorf("tokenrhythm priority = %#v, want -1", got)
	}
	if got := tr["max_in_flight"]; got != 4 {
		t.Errorf("tokenrhythm max_in_flight = %#v, want 4", got)
	}
	ap, ok := tr["account_priorities"].(map[string]any)
	if !ok {
		t.Fatalf("account_priorities = %#v, want a map", tr["account_priorities"])
	}
	if got := ap["tokenrhythm:acct-1"]; got != -1 {
		t.Errorf("account priority = %#v, want -1", got)
	}
	if got := tr["api_token"]; got != "<redacted>" {
		t.Errorf("api_token = %#v, want the redaction sentinel", got)
	}
}

// TestRedactionStillMasksRealCredentialFields pins the other half of the
// contract: after the platform-name fix, actual credential fields, including
// ones inside a client entry whose own name looks secret-ish, stay masked.
func TestRedactionStillMasksRealCredentialFields(t *testing.T) {
	cfg := map[string]any{
		"api_key": "plainopaqueabc123",
		"clients": map[string]any{
			"tokenmetrics": map[string]any{
				"api_key": "plainopaqueabc123",
				"enabled": true,
			},
		},
	}
	out := redactConfig(cfg).(map[string]any)
	if got := out["api_key"]; got != "<redacted>" {
		t.Errorf("top-level api_key = %#v, want the redaction sentinel", got)
	}
	cm, ok := out["clients"].(map[string]any)["tokenmetrics"].(map[string]any)
	if !ok {
		t.Fatalf("clients.tokenmetrics = %#v; a client name is not a credential field", out["clients"])
	}
	if got := cm["api_key"]; got != "<redacted>" {
		t.Errorf("clients.tokenmetrics.api_key = %#v, want the redaction sentinel", got)
	}
	if got := cm["enabled"]; got != true {
		t.Errorf("clients.tokenmetrics.enabled = %#v, want true", got)
	}
}
