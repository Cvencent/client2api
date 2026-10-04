package main

import "testing"

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

// The config file carries two ceilings per platform: the platform-wide one
// and the per-account one.  Both have to survive the projection onto the
// router's policy type, and a platform that omits the per-account knob keeps
// the documented default of no ceiling (0).
func TestPlatformConfigsProjectsBothInFlightCeilings(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"tabbit": {MaxInFlight: 5, MaxInFlightPerAccount: 3},
		"cline":  {MaxInFlight: 2},
	}}

	got := cfg.platformConfigs()
	if got["tabbit"].MaxInFlight != 5 || got["tabbit"].MaxInFlightPerAccount != 3 {
		t.Errorf("tabbit ceilings = (%d, %d), want (5, 3)",
			got["tabbit"].MaxInFlight, got["tabbit"].MaxInFlightPerAccount)
	}
	if got["cline"].MaxInFlightPerAccount != 0 {
		t.Errorf("cline per-account ceiling = %d, want 0 (unset)", got["cline"].MaxInFlightPerAccount)
	}
}
