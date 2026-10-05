package panel

import (
	"net/http"
	"strings"
	"testing"
)

// TestConfigPatchCarriesPlatformRoutingPolicy pins the write path for the
// per-platform routing policy.  The "平台配置" page saves a top-level
// "platforms" object; configwrite must accept it, keep it in canonical order
// next to the other routing keys, and never silently drop it.
func TestConfigPatchCarriesPlatformRoutingPolicy(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"cline":{"priority":5,"disabled_models":["gpt-9"]},"zcode":{"priority":10}}}`)
	// (the max_in_flight case lives in TestPlatformConfigAcceptsMaxInFlight)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}

	got := onDisk(t, path)
	pl, ok := got["platforms"].(map[string]any)
	if !ok {
		t.Fatalf("platforms was not saved as an object: %v", got["platforms"])
	}
	cline, _ := pl["cline"].(map[string]any)
	if cline["priority"] != float64(5) {
		t.Errorf("cline priority = %v, want 5", cline["priority"])
	}
	dm, _ := cline["disabled_models"].([]any)
	if len(dm) != 1 || dm[0] != "gpt-9" {
		t.Errorf("cline disabled_models = %v, want [gpt-9]", cline["disabled_models"])
	}
	if cline["disabled_models"] == nil {
		t.Errorf("disabled_models did not round-trip: %v", cline)
	}

	text := readFileString(t, path)
	if strings.Index(text, "\"platforms\"") > strings.Index(text, "\"clients\"") {
		t.Fatalf("platforms should be written before clients:\n%s", text)
	}
}

// A typo in the routing policy must be rejected at save time, not only surface
// as a failed startup or, worse, as a silently dead platform entry.
func TestConfigPatchRejectsMalformedPlatformPolicy(t *testing.T) {
	cases := []struct{ name, patch string }{
		{"platforms not an object", `{"platforms":[]}`},
		{"entry not an object", `{"platforms":{"cline":"off"}}`},
		{"priority not a number", `{"platforms":{"cline":{"priority":"high"}}}`},
		{"priority not whole", `{"platforms":{"cline":{"priority":1.5}}}`},
		{"disabled_models not an array", `{"platforms":{"cline":{"disabled_models":"gpt-9"}}}`},
		{"disabled_models not strings", `{"platforms":{"cline":{"disabled_models":[9]}}}`},
		{"max_in_flight not a number", `{"platforms":{"cline":{"max_in_flight":"lots"}}}`},
		{"max_in_flight not whole", `{"platforms":{"cline":{"max_in_flight":1.5}}}`},
		{"max_in_flight negative", `{"platforms":{"cline":{"max_in_flight":-1}}}`},
		{"max_in_flight_per_account not a number", `{"platforms":{"cline":{"max_in_flight_per_account":"lots"}}}`},
		{"max_in_flight_per_account not whole", `{"platforms":{"cline":{"max_in_flight_per_account":1.5}}}`},
		{"max_in_flight_per_account negative", `{"platforms":{"cline":{"max_in_flight_per_account":-1}}}`},
		{"reserve_credits not a number", `{"platforms":{"cline":{"reserve_credits":"lots"}}}`},
		{"reserve_credits not whole", `{"platforms":{"cline":{"reserve_credits":1.5}}}`},
		{"reserve_credits below -1", `{"platforms":{"cline":{"reserve_credits":-2}}}`},
		{"account_priorities not an object", `{"platforms":{"cline":{"account_priorities":[]}}}`},
		{"account priority not a number", `{"platforms":{"cline":{"account_priorities":{"a":"high"}}}}`},
		{"account priority not whole", `{"platforms":{"cline":{"account_priorities":{"a":1.5}}}}`},
		{"account_notes not an object", `{"platforms":{"cline":{"account_notes":[]}}}`},
		{"account note not a string", `{"platforms":{"cline":{"account_notes":{"a":5}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, baseConfig)
			w, _ := doConfig(t, configPanel(path), http.MethodPatch, tc.patch)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("HTTP %d, want 400: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestConfigPatchAcceptsAccountPriorities(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"workbuddy":{"account_priorities":{"acct-1":-2,"acct-2":5}}}}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}
	got := onDisk(t, path)
	pl, _ := got["platforms"].(map[string]any)
	wb, _ := pl["workbuddy"].(map[string]any)
	aps, _ := wb["account_priorities"].(map[string]any)
	if aps["acct-1"] != float64(-2) || aps["acct-2"] != float64(5) {
		t.Fatalf("account_priorities = %v, want acct-1=-2 acct-2=5", aps)
	}
}

// The operator's own per-account label (the phone number or e-mail a credential
// signs in with) rides the same config object as the routing policy, so a PATCH
// has to store it verbatim.
func TestConfigPatchAcceptsAccountNotes(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"trae":{"account_notes":{"acct-1":"13800138000","acct-2":"ops@example.com"}}}}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}
	got := onDisk(t, path)
	pl, _ := got["platforms"].(map[string]any)
	tr, _ := pl["trae"].(map[string]any)
	notes, _ := tr["account_notes"].(map[string]any)
	if notes["acct-1"] != "13800138000" || notes["acct-2"] != "ops@example.com" {
		t.Fatalf("account_notes = %v, want acct-1=13800138000 acct-2=ops@example.com", notes)
	}
}

// The note is rendered verbatim in the account table, so an unbounded string
// must be refused at save time rather than bloating the config and the layout.
func TestConfigPatchRejectsOverlongAccountNote(t *testing.T) {
	path := configFile(t, baseConfig)
	long := strings.Repeat("x", maxAccountNoteRunes+1)
	w, _ := doConfig(t, configPanel(path), http.MethodPatch,
		`{"platforms":{"trae":{"account_notes":{"a":"`+long+`"}}}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("HTTP %d, want 400: %s", w.Code, w.Body.String())
	}
}

// Both in-flight ceilings belong on the platform configuration page, so both
// have to be accepted and stored verbatim by the config endpoint.
func TestConfigPatchAcceptsPerAccountMaxInFlight(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"tabbit":{"max_in_flight":5,"max_in_flight_per_account":3}}}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}
	got := onDisk(t, path)
	pl, _ := got["platforms"].(map[string]any)
	tb, _ := pl["tabbit"].(map[string]any)
	if tb["max_in_flight"] != float64(5) || tb["max_in_flight_per_account"] != float64(3) {
		t.Errorf("tabbit ceilings = (%v, %v), want (5, 3)",
			tb["max_in_flight"], tb["max_in_flight_per_account"])
	}
}

// The low-balance guard is configured per platform on the same page, and -1
// is a legal value because it turns the guard off.
func TestConfigPatchAcceptsReserveCredits(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path),
		`{"platforms":{"workbuddy":{"reserve_credits":10},"tabbit":{"reserve_credits":-1}}}`)

	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "platforms" {
		t.Fatalf("changed = %v, want [platforms]", out["changed"])
	}
	got := onDisk(t, path)
	pl, _ := got["platforms"].(map[string]any)
	wb, _ := pl["workbuddy"].(map[string]any)
	tb, _ := pl["tabbit"].(map[string]any)
	if wb["reserve_credits"] != float64(10) {
		t.Errorf("workbuddy reserve_credits = %v, want 10", wb["reserve_credits"])
	}
	if tb["reserve_credits"] != float64(-1) {
		t.Errorf("tabbit reserve_credits = %v, want -1 (guard off)", tb["reserve_credits"])
	}
}
