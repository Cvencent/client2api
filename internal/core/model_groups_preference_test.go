package core

import (
	"context"
	"testing"
	"time"
)

// A group member's qualified id is an equivalence entry, not a hard platform
// pin: the named member is tried first, then its siblings in priority order.
func TestModelGroupMemberNamePrefersThatMemberThenFallsBack(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 50},
		"beta":  {Priority: 10},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
		},
	})

	// The plain group name keeps the priority order: beta wins.
	if gotClient, _ := resolveOK(t, r, "shared"); gotClient != "beta" {
		t.Fatalf("Resolve(shared) = %q, want beta by priority", gotClient)
	}

	// Naming the lower-priority member moves it to the front, but the sibling
	// stays in the candidate list for failover.
	got, err := r.ResolveCandidates(context.Background(), "alpha/alpha-model")
	if err != nil {
		t.Fatalf("ResolveCandidates(alpha/alpha-model) returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want both group members", got)
	}
	if got[0].Client.Name() != "alpha" || got[1].Client.Name() != "beta" {
		t.Fatalf("candidate order = (%q, %q), want (alpha, beta)", got[0].Client.Name(), got[1].Client.Name())
	}
}

// A member the router recently found unreachable is cached and demoted behind
// a healthy sibling on the next request, so a failed round is not repeated
// while the demotion is in force.
func TestModelGroupPreferredMemberCachedUnavailableFallsBack(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
		},
	})

	r.NoteModelUnavailable("alpha", "alpha-model", time.Now())

	got, err := r.ResolveCandidates(context.Background(), "alpha/alpha-model")
	if err != nil {
		t.Fatalf("ResolveCandidates(alpha/alpha-model) returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want both group members", got)
	}
	if got[0].Client.Name() != "beta" {
		t.Fatalf("first candidate = %q, want the healthy beta sibling after the alpha demotion", got[0].Client.Name())
	}
}
