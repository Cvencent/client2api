package core

import (
	"context"
	"testing"
	"time"
)

// A group member's qualified id names one platform, so it is a hard pin: the
// request stays on that platform.  The group itself -- the bare name -- is what
// carries the sibling chain, and a request for it still falls back in order.
func TestQualifiedGroupMemberStaysOnItsPlatform(t *testing.T) {
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

	// Naming the lower-priority member keeps the request on alpha, even though
	// the group would have preferred beta: the caller picked a platform.
	got, err := r.ResolveCandidates(context.Background(), "alpha/alpha-model")
	if err != nil {
		t.Fatalf("ResolveCandidates(alpha/alpha-model) returned an error: %v", err)
	}
	if len(got) != 1 || got[0].Client.Name() != "alpha" {
		t.Fatalf("candidates = %+v, want only the named alpha member", got)
	}

	// The bare group name is the failover chain and keeps both members, in
	// priority order.
	got, err = r.ResolveCandidates(context.Background(), "shared")
	if err != nil {
		t.Fatalf("ResolveCandidates(shared) returned an error: %v", err)
	}
	if len(got) != 2 || got[0].Client.Name() != "beta" || got[1].Client.Name() != "alpha" {
		t.Fatalf("group candidates = %+v, want beta then alpha by priority", got)
	}
}

// A group whose member the router recently found unreachable is demoted behind
// a healthy sibling on the next request, so a failed round is not repeated
// while the demotion is in force.
func TestGroupCachedUnavailableMemberFallsBackToSibling(t *testing.T) {
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

	got, err := r.ResolveCandidates(context.Background(), "shared")
	if err != nil {
		t.Fatalf("ResolveCandidates(shared) returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want both group members", got)
	}
	if got[0].Client.Name() != "beta" {
		t.Fatalf("first candidate = %q, want the healthy beta sibling after the alpha demotion", got[0].Client.Name())
	}

	// An explicit platform choice is not softened by the demotion: the caller
	// named alpha, so alpha is what it gets.
	got, err = r.ResolveCandidates(context.Background(), "alpha/alpha-model")
	if err != nil {
		t.Fatalf("ResolveCandidates(alpha/alpha-model) returned an error: %v", err)
	}
	if len(got) != 1 || got[0].Client.Name() != "alpha" {
		t.Fatalf("candidates = %+v, want only alpha for the explicit request", got)
	}
}
