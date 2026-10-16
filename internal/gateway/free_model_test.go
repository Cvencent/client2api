package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// The router prices a model before the module sees it, and the module's pool is
// where a credit park is enforced.  These tests pin the one field that carries
// the verdict across that boundary, and pin that it comes from the catalogue
// rather than from the caller.

func TestChatMarksAFreeModelCandidateForTheModule(t *testing.T) {
	c := &testClient{
		name: "wb",
		catalogue: []core.Model{
			{ID: "m1", OwnedBy: "wb", Extra: map[string]any{"free": true}},
		},
	}
	srv := newTestServer(t, c, NewStats(), nil)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called")
	}
	if !c.seen.FreeModel {
		t.Fatal("a model the catalogue marks free must reach the module marked free")
	}
}

func TestChatLeavesAPaidModelUnmarked(t *testing.T) {
	c := &testClient{
		name: "wb",
		catalogue: []core.Model{
			{ID: "m1", OwnedBy: "wb", Extra: map[string]any{"free": false}},
		},
	}
	srv := newTestServer(t, c, NewStats(), nil)

	if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called")
	}
	if c.seen.FreeModel {
		t.Fatal("a paid candidate must not be marked free")
	}
}

func TestChatPinsTheFreeFlagPerCandidate(t *testing.T) {
	// The first platform refuses the request, so the gateway fails over to a
	// sibling that serves the same id for free.  The flag has to travel with the
	// candidate, not with the request: the second attempt is the free one.
	first := &testClient{
		name: "paid",
		catalogue: []core.Model{
			{ID: "m1", OwnedBy: "paid"},
		},
		chatErr: core.Fail("paid", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom")),
	}
	second := &testClient{
		name: "wb",
		catalogue: []core.Model{
			{ID: "m1", OwnedBy: "wb", Extra: map[string]any{"free": true}},
		},
	}

	reg := core.NewRegistry()
	reg.Add(first)
	reg.Add(second)
	// The paid platform is the operator's first choice; the free sibling is the
	// failover.  Without the explicit priorities the router's free-first tie-break
	// would put the free one first and the test would prove nothing.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"paid": {Priority: 1},
		"wb":   {Priority: 2},
	})
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		// The failover waits out the rotation backoff; the test only cares about
		// the flag the second attempt carries.
		Live: livecfg.New(livecfg.Snapshot{MaxRotate: 1, RotateBackoffBase: time.Nanosecond}),
	})

	if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if first.seen == nil || first.seen.FreeModel {
		t.Fatal("the paid platform must not be told the model is free")
	}
	if second.seen == nil {
		t.Fatal("the free sibling was never tried")
	}
	if !second.seen.FreeModel {
		t.Fatal("the failover candidate's own price must decide the flag")
	}
}

func TestCallerCannotClaimAFreeModel(t *testing.T) {
	c := &testClient{name: "wb"}
	srv := newTestServer(t, c, NewStats(), nil)

	// The wire body is pure caller input: a key the router owns must be ignored,
	// and a module must never be handed a caller-supplied verdict about pricing.
	rec := chat(t, srv, `{"model":"m1","messages":[{"role":"user","content":"hi"}],"free_model":true,"freeModel":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module was never called")
	}
	if c.seen.FreeModel {
		t.Fatal("a caller must not be able to mark its own request free")
	}
}
