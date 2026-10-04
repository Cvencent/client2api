package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// statusClient is a fakeClient whose self-report a test can set, so the
// "prefer a platform that can actually serve the request" rule is testable.
type statusClient struct {
	fakeClient
	status Status
}

func (s *statusClient) Status(context.Context) Status { return s.status }

func readyStatus(name string) Status {
	return Status{Name: name, Ready: true, Accounts: []AccountStatus{{ID: "a1", Enabled: true, State: "ready"}}}
}

// A bare model name served by two platforms is no longer an error: the
// operator's priority decides, and the platform that asked for the lower
// number wins.
func TestResolveRoutesABareNameToTheHigherPriorityPlatform(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "deepseek/v4-flash"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "deepseek/v4-flash"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: 10},
	})

	gotClient, gotModel := resolveOK(t, r, "deepseek/v4-flash")
	if gotClient != "beta" || gotModel != "deepseek/v4-flash" {
		t.Fatalf("Resolve = (%q, %q), want (beta, deepseek/v4-flash)", gotClient, gotModel)
	}
}

// Equal priorities still need an answer: the name is the deterministic
// tie-break, so the same request cannot flip between processes.
func TestResolveTieBreaksEqualPrioritiesByName(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "openrouter", models: []Model{{ID: "vendor/model"}}},
		&fakeClient{name: "cline", models: []Model{{ID: "vendor/model"}}},
	)

	gotClient, _ := resolveOK(t, r, "vendor/model")
	if gotClient != "cline" {
		t.Fatalf("Resolve = %q, want the alphabetically first owner cline", gotClient)
	}
}

// The blacklist is per platform: a disabled model is skipped while looking for
// an owner, so the next platform that still allows it takes the request.
func TestResolveSkipsAPlatformThatDisabledTheModel(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "deepseek/v4-flash"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "deepseek/v4-flash"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 1, DisabledModels: []string{"deepseek/v4-flash"}},
		"beta":  {},
	})

	gotClient, _ := resolveOK(t, r, "deepseek/v4-flash")
	if gotClient != "beta" {
		t.Fatalf("Resolve = %q, want beta (alpha blacklisted it)", gotClient)
	}
}

// The blacklist matches ids case-insensitively, because the operator types
// them by hand and the upstream spelling drifts.
func TestResolveBlacklistIsCaseInsensitive(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}})
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {DisabledModels: []string{"  glm-5.3 "}},
	})

	if r.ModelAllowed("alpha", "GLM-5.3") {
		t.Fatal("ModelAllowed said a blacklisted model is allowed")
	}
}

// Qualifying the platform does not bypass the blacklist: an operator who
// disabled a model meant it.
func TestResolveRefusesAQualifiedModelDisabledOnItsPlatform(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}})
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {DisabledModels: []string{"GLM-5.3"}},
	})

	_, _, err := r.Resolve(context.Background(), "alpha/GLM-5.3")
	if err == nil {
		t.Fatal("Resolve accepted a qualified request for a disabled model")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("the refusal does not say the model is disabled: %v", err)
	}
}

// When every owner blacklisted the model the refusal has to say so, not claim
// that nobody serves it.
func TestResolveExplainsWhenEveryOwnerDisabledTheModel(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "GLM-5.3"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {DisabledModels: []string{"GLM-5.3"}},
		"beta":  {DisabledModels: []string{"GLM-5.3"}},
	})

	_, _, err := r.Resolve(context.Background(), "GLM-5.3")
	if err == nil {
		t.Fatal("Resolve accepted a model every platform disabled")
	}
	msg := err.Error()
	for _, want := range []string{"disabled", "alpha", "beta"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the refusal does not mention %q: %v", want, msg)
		}
	}
}

// Among allowed owners the one that can actually serve the request wins, even
// if a higher-priority platform is nominally first.
func TestResolvePrefersAnAllowedOwnerWithAReadyAccount(t *testing.T) {
	parked := &statusClient{
		fakeClient: fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}},
		status:     Status{Name: "alpha", Ready: true, Accounts: []AccountStatus{{ID: "a1", Enabled: true, State: "exhausted"}}},
	}
	usable := &statusClient{
		fakeClient: fakeClient{name: "beta", models: []Model{{ID: "GLM-5.3"}}},
		status:     readyStatus("beta"),
	}
	r := registryWith(t, parked, usable)
	r.SetPlatformConfigs(map[string]PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})

	gotClient, _ := resolveOK(t, r, "GLM-5.3")
	if gotClient != "beta" {
		t.Fatalf("Resolve = %q, want the usable platform beta", gotClient)
	}
}

// A single owner is still resolved without consulting Status at all, which is
// what keeps the cheap path cheap.
func TestResolveSingleOwnerDoesNotNeedStatus(t *testing.T) {
	boom := &boomStatusClient{fakeClient: fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}}}
	r := registryWith(t, boom)

	gotClient, _ := resolveOK(t, r, "GLM-5.3")
	if gotClient != "alpha" {
		t.Fatalf("Resolve = %q, want alpha", gotClient)
	}
	if boom.calls != 0 {
		t.Fatalf("Status was called %d times for an unambiguous request", boom.calls)
	}
}

type boomStatusClient struct {
	fakeClient
	calls int
}

func (b *boomStatusClient) Status(context.Context) Status {
	b.calls++
	return Status{Name: b.name}
}

func TestResolveCandidatesPrefersFreeAtEqualPriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "paid", models: []Model{{ID: "deepseek/v4-flash"}}},
		&fakeClient{name: "free", models: []Model{{ID: "deepseek/v4-flash", Extra: map[string]any{"free": true}}}},
	)

	candidates, err := r.ResolveCandidates(context.Background(), "deepseek/v4-flash")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2", len(candidates))
	}
	if got := candidates[0].Client.Name(); got != "free" || !candidates[0].Free {
		t.Fatalf("first candidate = %q free=%v, want free platform", got, candidates[0].Free)
	}
}

func TestResolveCandidatesDemotesSuppressedPlatform(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "m"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "m"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	now := time.Now()
	for i := 0; i < 3; i++ {
		r.NoteModelFailure("alpha", "m", now.Add(time.Duration(i)*time.Second))
	}

	candidates, err := r.ResolveCandidates(context.Background(), "m")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if got := candidates[0].Client.Name(); got != "beta" {
		t.Fatalf("first candidate = %q, want beta (alpha is suppressed)", got)
	}
}

func TestResolveCandidatesQualifiedModelHasOneCandidate(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "m"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "m"}}},
	)
	candidates, err := r.ResolveCandidates(context.Background(), "beta/m")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Client.Name() != "beta" || candidates[0].Model != "m" {
		t.Fatalf("qualified candidates = %+v, want only beta/m", candidates)
	}
}

// MaxInFlightPerAccountFor exposes the per-account ceiling the gateway turns
// into a counting semaphore.  A platform the operator did not configure, or one
// configured without the knob, means no ceiling (0).
func TestMaxInFlightPerAccountForReadsThePolicy(t *testing.T) {
	r := NewRegistry()
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"tabbit": {MaxInFlight: 5, MaxInFlightPerAccount: 3},
		"cline":  {MaxInFlight: 2},
	})

	if got := r.MaxInFlightPerAccountFor("tabbit"); got != 3 {
		t.Errorf("tabbit per-account ceiling = %d, want 3", got)
	}
	if got := r.MaxInFlightPerAccountFor("cline"); got != 0 {
		t.Errorf("cline per-account ceiling = %d, want 0 (unset)", got)
	}
	if got := r.MaxInFlightPerAccountFor("unknown"); got != 0 {
		t.Errorf("unknown per-account ceiling = %d, want 0", got)
	}
}
