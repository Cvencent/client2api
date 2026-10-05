package main

import (
	"encoding/json"
	"testing"
)

// TestPlatformConfigsProjectsTheFileBlock pins the one translation between the
// config file's "platforms" object and the router's policy type.  A blank model
// id can never match an upstream id, so it must be dropped rather than kept as
// a dead blacklist entry; a priority of any integer (including a negative one)
// must survive verbatim.
func TestPlatformConfigsProjectsTheFileBlock(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"cline": {Priority: 5, DisabledModels: []string{"gpt-9", "  ", "GLM-5.3"}},
		"zcode": {Priority: -1},
	}}

	got := cfg.platformConfigs()
	if len(got) != 2 {
		t.Fatalf("platformConfigs = %v, want two platforms", got)
	}
	if got["cline"].Priority != 5 {
		t.Errorf("cline priority = %d, want 5", got["cline"].Priority)
	}
	if len(got["cline"].DisabledModels) != 2 {
		t.Errorf("cline disabled = %v, want the two non-blank ids", got["cline"].DisabledModels)
	}
	if got["zcode"].Priority != -1 {
		t.Errorf("zcode priority = %d, want -1", got["zcode"].Priority)
	}

	if empty := (&fileConfig{}).platformConfigs(); len(empty) != 0 {
		t.Errorf("a file with no platforms block should project nothing, got %v", empty)
	}
}

// The operator's per-account identity note (the phone number or e-mail a
// credential signs in with) rides the platforms block too.  The panel serves it
// verbatim, so it has to survive the projection; a blank id or a blank note is
// dropped there so the registry never carries a meaningless entry.
func TestPlatformConfigsProjectsAccountNotes(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"trae": {AccountNotes: map[string]string{
			"3595881099822378": " 13800138000 ",
			"":                 "drop me",
			"blank":            "   ",
		}},
	}}
	got := cfg.platformConfigs()
	notes := got["trae"].AccountNotes
	if len(notes) != 1 || notes["3595881099822378"] != "13800138000" {
		t.Fatalf("account notes = %v, want only the trimmed 3595881099822378 entry", notes)
	}

	// The JSON tag is the contract with both the config file and the panel PATCH.
	var parsed fileConfig
	if err := json.Unmarshal([]byte(`{"platforms":{"trae":{"account_notes":{"a":"ops@example.com"}}}}`), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Platforms["trae"].AccountNotes["a"] != "ops@example.com" {
		t.Errorf("account_notes did not round-trip: %+v", parsed.Platforms["trae"])
	}
}

// The config file carries two ceilings per platform: the platform-wide one
// and the per-account one.  Both have to survive the projection onto the
// router's policy type.  A missing key takes the documented default of 2; an
// explicit 0 still means "no ceiling".
func TestPlatformConfigsProjectsBothInFlightCeilings(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"tabbit": {MaxInFlight: intPtr(5), MaxInFlightPerAccount: intPtr(3)},
		"cline":  {MaxInFlight: intPtr(2)},
	}}

	got := cfg.platformConfigs()
	if got["tabbit"].MaxInFlight != 5 || got["tabbit"].MaxInFlightPerAccount != 3 {
		t.Errorf("tabbit ceilings = (%d, %d), want (5, 3)",
			got["tabbit"].MaxInFlight, got["tabbit"].MaxInFlightPerAccount)
	}
	if got["cline"].MaxInFlightPerAccount != 2 {
		t.Errorf("cline per-account ceiling = %d, want the default 2", got["cline"].MaxInFlightPerAccount)
	}
}
