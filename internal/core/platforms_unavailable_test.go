package core

import (
	"context"
	"testing"
	"time"
)

// A platform that is temporarily unable to serve (all accounts busy or
// cooling) must not be retried first on every request.  It is demoted like a
// suppressed platform but stays in the list as a last-resort candidate.
func TestResolveCandidatesDemotesUnavailablePlatform(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "m"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "m"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	r.NoteModelUnavailable("alpha", "m", time.Now())

	candidates, err := r.ResolveCandidates(context.Background(), "m")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 2 || candidates[0].Client.Name() != "beta" || candidates[1].Client.Name() != "alpha" {
		t.Fatalf("candidates = %+v, want beta before the demoted alpha", candidates)
	}
}
