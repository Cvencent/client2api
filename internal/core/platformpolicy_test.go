package core

import "testing"

// policyClient records the per-platform policy pushed to it, so the push path
// can be asserted without a real module.
type policyClient struct {
	plainClient
	got PlatformConfig
	n   int
}

func (c *policyClient) ApplyPlatformPolicy(cfg PlatformConfig) {
	c.got = cfg
	c.n++
}

func TestReserveCreditsForReadsThePolicy(t *testing.T) {
	r := NewRegistry()
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"workbuddy": {ReserveCredits: 10},
		"tabbit":    {ReserveCredits: -1},
	})

	if got := r.ReserveCreditsFor("workbuddy"); got != 10 {
		t.Errorf("ReserveCreditsFor(workbuddy) = %d, want 10", got)
	}
	if got := r.ReserveCreditsFor("tabbit"); got != -1 {
		t.Errorf("ReserveCreditsFor(tabbit) = %d, want -1", got)
	}
	// An absent entry is the documented default: park only a known zero.
	if got := r.ReserveCreditsFor("cline"); got != 0 {
		t.Errorf("ReserveCreditsFor(cline) = %d, want the 0 default", got)
	}
}

func TestApplyPlatformPoliciesPushesEachModuleItsOwnPolicy(t *testing.T) {
	r := NewRegistry()
	applier := &policyClient{plainClient: plainClient{name: "workbuddy"}}
	r.Add(applier)
	r.Add(&plainClient{name: "cline"})

	n := ApplyPlatformPolicies(r, map[string]PlatformConfig{
		"workbuddy": {Priority: 2, ReserveCredits: 10},
	})
	if n != 1 {
		t.Fatalf("ApplyPlatformPolicies reported %d modules, want 1", n)
	}
	if applier.n != 1 {
		t.Fatalf("ApplyPlatformPolicy calls = %d, want 1", applier.n)
	}
	if applier.got.Priority != 2 || applier.got.ReserveCredits != 10 {
		t.Fatalf("module received %+v, want priority 2 / reserve 10", applier.got)
	}

	// A module with no entry still gets the zero policy, so removing a block
	// clears the guard the same way weakening it does.
	n = ApplyPlatformPolicies(r, map[string]PlatformConfig{})
	if n != 1 || applier.n != 2 {
		t.Fatalf("second push: modules pushed = %d, calls = %d, want 1/2", n, applier.n)
	}
	if g := applier.got; g.Priority != 0 || g.ReserveCredits != 0 || len(g.DisabledModels) != 0 {
		t.Fatalf("module received %+v after the block was removed, want the zero policy", g)
	}

	// A nil registry and a nil policy map must not panic.
	if got := ApplyPlatformPolicies(nil, nil); got != 0 {
		t.Errorf("ApplyPlatformPolicies(nil, nil) = %d, want 0", got)
	}
	if got := ApplyPlatformPolicies(r, nil); got != 1 {
		t.Errorf("ApplyPlatformPolicies(r, nil) = %d, want the one capable module", got)
	}
}
