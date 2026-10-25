package gateway

import (
	"context"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// preflightClient is a module that publishes a health verdict, an account
// census and optionally a per-model availability probe, so a test can drive the
// router's pre-flight with the same surfaces a real module exposes.
type preflightClient struct {
	name     string
	health   core.Health
	accounts []core.AccountStatus
	// available, when set, answers the per-model probe.
	available func(model string, free bool) error
	// free marks the catalogue entry so the router sees a free model.
	free bool
	// failWith, when set, is the refusal Chat returns.
	failWith error
	chatN    int
	served   string
}

func (c *preflightClient) Name() string { return c.name }

func (c *preflightClient) Models(context.Context) ([]core.Model, error) {
	m := core.Model{ID: "m1", OwnedBy: c.name}
	if c.free {
		m.Extra = map[string]any{"free": true}
	}
	return []core.Model{m}, nil
}

func (c *preflightClient) Chat(_ context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.chatN++
	if c.failWith != nil {
		return nil, c.failWith
	}
	core.NoteServedBy(req, c.served)
	return &fakeStream{events: usageEvents(2, 3)}, nil
}

func (c *preflightClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true, Accounts: c.accounts}
}

func (c *preflightClient) Health() core.Health { return c.health }

func (c *preflightClient) Available(model string, free bool) error {
	if c.available == nil {
		return nil
	}
	return c.available(model, free)
}

func newPreflightServer(t *testing.T, trackers *InFlightTrackers, clients ...core.Client) (*http.Server, *UsageStore) {
	t.Helper()
	reg := core.NewRegistry()
	usage := NewUsageStore(20)
	platforms := map[string]core.PlatformConfig{}
	for i, c := range clients {
		reg.Add(c)
		platforms[c.Name()] = core.PlatformConfig{Priority: i + 1}
	}
	reg.SetPlatformConfigs(platforms)
	srv := NewServer(Options{
		Registry:         reg,
		Version:          "test",
		Logger:           log.New(io.Discard, "", 0),
		Stats:            NewStats(),
		Usage:            usage,
		InFlightTrackers: trackers,
	})
	return srv, usage
}

func attemptRows(rows []UsageRecord) []UsageRecord {
	out := make([]UsageRecord, 0, len(rows))
	for _, r := range rows {
		if r.Attempt {
			out = append(out, r)
		}
	}
	return out
}

// TestPreflightSkipsAPlatformWithNoUsableAccount pins the pessimistic rule: a
// platform whose own health verdict says "no usable account" is not entered at
// all while a sibling can serve the model, and the skip files no failed
// candidate row because nothing was attempted.
func TestPreflightSkipsAPlatformWithNoUsableAccount(t *testing.T) {
	empty := &preflightClient{
		name:   "empty",
		health: core.Health{Total: 2, Cooling: 2, Note: "no account is usable right now"},
	}
	healthy := &preflightClient{
		name:   "healthy",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "healthy-acct",
	}
	srv, usage := newPreflightServer(t, nil, empty, healthy)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if empty.chatN != 0 {
		t.Fatalf("the unavailable platform was entered %d time(s); the pre-flight must skip it", empty.chatN)
	}
	if healthy.chatN != 1 {
		t.Fatalf("healthy platform calls = %d, want 1", healthy.chatN)
	}
	if rows := attemptRows(usage.Snapshot()); len(rows) != 0 {
		t.Fatalf("a skipped candidate must not file a failed-attempt row, got %+v", rows)
	}
	got := usage.Snapshot()
	final := got[len(got)-1]
	if final.Attempt || final.Failed || final.Client != "healthy" {
		t.Fatalf("final row = %+v, want the healthy platform's success", final)
	}
	if final.Candidate != 2 {
		t.Fatalf("candidate = %d, want 2: the skip must keep the priority position", final.Candidate)
	}
}

// TestPreflightSkipsAPlatformAtItsPerAccountCeiling pins the gateway-owned half
// of the check: every account of the platform sits at
// max_in_flight_per_account, which the gateway knows without asking the module.
func TestPreflightSkipsAPlatformAtItsPerAccountCeiling(t *testing.T) {
	trackers := NewInFlightTrackers()
	saturated := &preflightClient{
		name:     "saturated",
		health:   core.Health{Servable: true, Total: 1, Ready: 1},
		accounts: []core.AccountStatus{{ID: "acct-1", Enabled: true, State: "ready"}},
	}
	free := &preflightClient{
		name:     "free",
		health:   core.Health{Servable: true, Total: 1, Ready: 1},
		accounts: []core.AccountStatus{{ID: "acct-1", Enabled: true, State: "ready"}},
		served:   "free-acct",
	}
	reg := core.NewRegistry()
	reg.Add(saturated)
	reg.Add(free)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"saturated": {Priority: 1, MaxInFlightPerAccount: 1},
		"free":      {Priority: 2, MaxInFlightPerAccount: 1},
	})
	srv := NewServer(Options{
		Registry:         reg,
		Version:          "test",
		Logger:           log.New(io.Discard, "", 0),
		Stats:            NewStats(),
		Usage:            NewUsageStore(20),
		InFlightTrackers: trackers,
	})

	// Occupy the only slot of the platform's only account, the way a live
	// stream would.
	release, err := trackers.acquireAccount("saturated", "acct-1", 1)
	if err != nil {
		t.Fatalf("acquireAccount: %v", err)
	}
	defer release()

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if saturated.chatN != 0 {
		t.Fatalf("the saturated platform was entered %d time(s)", saturated.chatN)
	}
	if free.chatN != 1 {
		t.Fatalf("free platform calls = %d, want 1", free.chatN)
	}
}

// TestPreflightSkipsAPlatformAtItsPlatformCeiling pins the other gateway-owned
// brake: max_in_flight on the platform as a whole, which the gateway also knows
// without asking the module.
func TestPreflightSkipsAPlatformAtItsPlatformCeiling(t *testing.T) {
	trackers := NewInFlightTrackers()
	saturated := &preflightClient{
		name:   "saturated",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
	}
	sibling := &preflightClient{
		name:   "sibling",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "sibling-acct",
	}
	reg := core.NewRegistry()
	reg.Add(saturated)
	reg.Add(sibling)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"saturated": {Priority: 1, MaxInFlight: 1},
		"sibling":   {Priority: 2},
	})
	srv := NewServer(Options{
		Registry:         reg,
		Version:          "test",
		Logger:           log.New(io.Discard, "", 0),
		Stats:            NewStats(),
		Usage:            NewUsageStore(20),
		InFlightTrackers: trackers,
	})

	release, err := trackers.acquirePlatform("saturated", 1)
	if err != nil {
		t.Fatalf("acquirePlatform: %v", err)
	}
	defer release()

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if saturated.chatN != 0 {
		t.Fatalf("the platform-saturated candidate was entered %d time(s)", saturated.chatN)
	}
	if sibling.chatN != 1 {
		t.Fatalf("sibling calls = %d, want 1", sibling.chatN)
	}
}

// TestPreflightUsesTheModuleModelProbe pins the per-model hook: a park that is
// per (account, model) is invisible to the account census, so the module's own
// probe is what lets the router skip it.
func TestPreflightUsesTheModuleModelProbe(t *testing.T) {
	parked := &preflightClient{
		name:     "parked",
		health:   core.Health{Servable: true, Total: 1, Ready: 1},
		accounts: []core.AccountStatus{{ID: "acct-1", Enabled: true, State: "ready"}},
		available: func(model string, free bool) error {
			if model == "m1" && !free {
				return core.ErrPlatformExhausted
			}
			return nil
		},
	}
	sibling := &preflightClient{
		name:   "sibling",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "sibling-acct",
	}
	srv, usage := newPreflightServer(t, nil, parked, sibling)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if parked.chatN != 0 {
		t.Fatalf("the model-parked platform was entered %d time(s)", parked.chatN)
	}
	if sibling.chatN != 1 {
		t.Fatalf("sibling calls = %d, want 1", sibling.chatN)
	}
	if rows := attemptRows(usage.Snapshot()); len(rows) != 0 {
		t.Fatalf("a skipped candidate must not file a failed-attempt row, got %+v", rows)
	}
}

// TestPreflightNeverSkipsTheLastCandidate pins the safety valve: the pre-flight
// is a routing decision between siblings, never a verdict on the request.  The
// last candidate bypasses it entirely and is entered, so a stale local reading
// can reorder the chain but cannot turn a servable route into an outage.
func TestPreflightNeverSkipsTheLastCandidate(t *testing.T) {
	first := &preflightClient{name: "first", health: core.Health{Total: 1, Cooling: 1}}
	last := &preflightClient{
		name:   "last",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "last-acct",
		// This probe would refuse the candidate if it were consulted.  As the
		// last one it must not be, and the module must be entered.
		available: func(string, bool) error { return core.ErrPlatformExhausted },
	}
	srv, _ := newPreflightServer(t, nil, first, last)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if first.chatN != 0 {
		t.Fatalf("first candidate entered %d time(s), want 0", first.chatN)
	}
	if last.chatN != 1 {
		t.Fatalf("last candidate entered %d time(s), want 1", last.chatN)
	}
}

// TestPreflightDemotesTheSkippedPlatform pins that a skip teaches the ranking,
// so the next request does not even look at the platform first.
func TestPreflightDemotesTheSkippedPlatform(t *testing.T) {
	empty := &preflightClient{name: "empty", health: core.Health{Total: 1, Cooling: 1}}
	healthy := &preflightClient{
		name:   "healthy",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "healthy-acct",
	}
	srv, _ := newPreflightServer(t, nil, empty, healthy)

	for i := 0; i < 2; i++ {
		if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if empty.chatN != 0 || healthy.chatN != 2 {
		t.Fatalf("calls = empty:%d healthy:%d, want 0/2", empty.chatN, healthy.chatN)
	}
}

// TestPreflightKeepsTheFreeModelException pins that a free model still reaches
// a module whose account-level verdict is "unservable": a credit-parked account
// may serve it, and only the module may decide that.
func TestPreflightKeepsTheFreeModelException(t *testing.T) {
	parked := &preflightClient{
		name:   "freebie",
		health: core.Health{Total: 1, Cooling: 1, Note: "credit exhausted"},
		free:   true,
		served: "freebie-acct",
	}
	other := &preflightClient{name: "other", health: core.Health{Servable: true, Total: 1, Ready: 1}}
	srv, _ := newPreflightServer(t, nil, parked, other)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if parked.chatN != 1 {
		t.Fatalf("the free model's platform was entered %d time(s), want 1", parked.chatN)
	}
	if other.chatN != 0 {
		t.Fatalf("the free model must not be routed away from its free owner, other calls = %d", other.chatN)
	}
}

// TestPreflightKeepsPriorityPositionsAcrossFailover pins the candidate numbering
// the panel shows: skipping a platform must not renumber the ones behind it, so
// the row for the platform that finally serves still reports its real position
// in the operator's priority order.
func TestPreflightKeepsPriorityPositionsAcrossFailover(t *testing.T) {
	empty := &preflightClient{name: "empty", health: core.Health{Total: 1, Cooling: 1}}
	failing := &preflightClient{
		name:     "failing",
		health:   core.Health{Servable: true, Total: 1, Ready: 1},
		failWith: core.ErrPlatformExhausted,
	}
	serving := &preflightClient{
		name:   "serving",
		health: core.Health{Servable: true, Total: 1, Ready: 1},
		served: "serving-acct",
	}
	srv, usage := newPreflightServer(t, nil, empty, failing, serving)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if empty.chatN != 0 {
		t.Fatalf("the unavailable platform was entered %d time(s)", empty.chatN)
	}
	if failing.chatN != 1 || serving.chatN != 1 {
		t.Fatalf("calls = failing:%d serving:%d, want 1/1", failing.chatN, serving.chatN)
	}
	rows := usage.Snapshot()
	final := rows[len(rows)-1]
	if final.Attempt || final.Client != "serving" {
		t.Fatalf("final row = %+v, want the serving platform", final)
	}
	if final.Candidate != 3 {
		t.Fatalf("final candidate = %d, want 3: the skip must not renumber the chain", final.Candidate)
	}
}
