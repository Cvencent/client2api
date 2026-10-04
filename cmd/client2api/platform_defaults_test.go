package main

import "testing"

// A platform with no explicit ceiling must now default to two on both axes.
// An explicit zero still means "no ceiling", so the projection has to tell
// "absent" apart from "zero".
func TestPlatformConfigsDefaultsBothInFlightCeilingsToTwo(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"zcode": {},
		"cline": {MaxInFlight: intPtr(0), MaxInFlightPerAccount: intPtr(0)},
	}}

	got := cfg.platformConfigs()
	if got["zcode"].MaxInFlight != 2 || got["zcode"].MaxInFlightPerAccount != 2 {
		t.Errorf("zcode defaults = (%d, %d), want (2, 2)",
			got["zcode"].MaxInFlight, got["zcode"].MaxInFlightPerAccount)
	}
	if got["cline"].MaxInFlight != 0 || got["cline"].MaxInFlightPerAccount != 0 {
		t.Errorf("cline explicit zero = (%d, %d), want (0, 0)",
			got["cline"].MaxInFlight, got["cline"].MaxInFlightPerAccount)
	}
}

func intPtr(v int) *int { return &v }
