package core

import (
	"context"
	"testing"
)

type backgroundProbeGateClient struct {
	*fakeClient
	allowed bool
	seen    []string
}

func (c *backgroundProbeGateClient) BackgroundProbeDue(_ context.Context, id string) bool {
	c.seen = append(c.seen, id)
	return c.allowed
}

func TestBackgroundProbeAllowedDefaultsToYesForModulesWithoutAGate(t *testing.T) {
	c := &fakeClient{name: "plain"}
	if !BackgroundProbeAllowed(context.Background(), c, "acct-1") {
		t.Fatal("a module without a probe gate was vetoed by the shared sweep")
	}
}

func TestBackgroundProbeAllowedHonoursTheModuleGate(t *testing.T) {
	c := &backgroundProbeGateClient{
		fakeClient: &fakeClient{name: "gated"},
		allowed:    false,
	}
	if BackgroundProbeAllowed(context.Background(), c, "acct-1") {
		t.Fatal("the shared sweep ignored the module's probe gate")
	}
	if len(c.seen) != 1 || c.seen[0] != "acct-1" {
		t.Fatalf("gate saw %v, want [acct-1]", c.seen)
	}
}

func TestIsRecoverableAccountState(t *testing.T) {
	for _, state := range []string{"cooling", "exhausted", "low_credit", "quota_exceeded", "rate_limited"} {
		if !IsRecoverableAccountState(state) {
			t.Errorf("IsRecoverableAccountState(%q) = false, want true", state)
		}
	}
	for _, state := range []string{"ready", "invalid", "unknown", ""} {
		if IsRecoverableAccountState(state) {
			t.Errorf("IsRecoverableAccountState(%q) = true, want false", state)
		}
	}
}
