package main

import (
	"encoding/json"
	"testing"
)

// TestProbeBeforeTrimDefaultsOn pins that the probe is opt-out, not opt-in: an
// existing config with no mention of it gets the vendor-first behaviour.
func TestProbeBeforeTrimDefaultsOn(t *testing.T) {
	var cfg fileConfig
	if err := json.Unmarshal([]byte("{}"), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.liveSnapshot().ProbeBeforeTrim; got != true {
		t.Fatalf("default ProbeBeforeTrim = %v, want true", got)
	}
}

// TestProbeBeforeTrimCanBeDisabled keeps the escape hatch honest: a deployment
// that wants the old trim-first behaviour can say so in the config file.
func TestProbeBeforeTrimCanBeDisabled(t *testing.T) {
	var cfg fileConfig
	if err := json.Unmarshal([]byte(`{"features":{"probe_before_trim":false}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.liveSnapshot().ProbeBeforeTrim; got != false {
		t.Fatalf("explicit false = %v, want false", got)
	}
}

func TestProbeBeforeTrimExplicitOn(t *testing.T) {
	var cfg fileConfig
	if err := json.Unmarshal([]byte(`{"features":{"probe_before_trim":true}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.liveSnapshot().ProbeBeforeTrim; got != true {
		t.Fatalf("explicit true = %v, want true", got)
	}
}
