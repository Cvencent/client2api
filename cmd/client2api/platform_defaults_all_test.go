package main

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// platformDefaultsAllClients pins the difference between "the platforms block
// lists a platform" and "the platform exists".  The operator asked for a
// default of two everywhere, so a client with no platforms entry must still
// receive the ceiling.
func TestPlatformDefaultsApplyToEveryRegisteredClient(t *testing.T) {
	cfg := &fileConfig{Platforms: map[string]platformConfig{
		"zcode": {Priority: 10},
	}}
	got := cfg.platformConfigsFor([]core.Client{stubClient("zcode"), stubClient("cline")})
	if got["zcode"].MaxInFlight != 2 || got["zcode"].MaxInFlightPerAccount != 2 {
		t.Errorf("zcode = (%d,%d), want (2,2)", got["zcode"].MaxInFlight, got["zcode"].MaxInFlightPerAccount)
	}
	if got["cline"].MaxInFlight != 2 || got["cline"].MaxInFlightPerAccount != 2 {
		t.Errorf("unlisted cline = (%d,%d), want the default (2,2)",
			got["cline"].MaxInFlight, got["cline"].MaxInFlightPerAccount)
	}
}

type stubClient string

func (s stubClient) Name() string { return string(s) }

func (s stubClient) Models(context.Context) ([]core.Model, error) { return nil, nil }

func (s stubClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, nil
}

func (s stubClient) Status(context.Context) core.Status { return core.Status{} }
