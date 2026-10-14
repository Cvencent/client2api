package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelpolicy"
)

// modelpolicy_test.go pins the gateway half of the operator's per-account
// model blacklist: the shared account hook refuses an account for a model the
// operator switched off, and it reports that refusal as ErrBusy so the module
// rotates to the next account exactly as it does for a full in-flight slot.
// Without this, the model dialog's checkboxes would render for every platform
// but only WorkBuddy (which keeps its own blacklist in its pool) would honour
// them.

// rotatingClient behaves like a real module: it walks its accounts, takes the
// gateway's per-account slot before each attempt, and reports busy when no
// account is left.  It names the credential it used, so the test can see which
// account actually served.
type rotatingClient struct {
	name     string
	accounts []string
	// attempted records every account it asked for, in order.
	attempted []string
	// served is the account that passed the gate and answered.
	served string
}

func (c *rotatingClient) Name() string { return c.name }

func (c *rotatingClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1"}, {ID: "m2"}}, nil
}

func (c *rotatingClient) Chat(_ context.Context, req *core.ChatRequest) (core.Stream, error) {
	for _, id := range c.accounts {
		c.attempted = append(c.attempted, id)
		if err := req.AcquireAccountSlot(id); err != nil {
			continue
		}
		c.served = id
		core.NoteServedBy(req, id)
		return &fakeStream{events: usageEvents(1, 1)}, nil
	}
	return nil, fmt.Errorf("%w: no account is able to serve this model", core.ErrBusy)
}

func (c *rotatingClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func newPolicyServer(t *testing.T, c *rotatingClient, policies *modelpolicy.Store) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry:      reg,
		Version:       "test",
		Logger:        log.New(io.Discard, "", 0),
		ModelPolicies: policies,
	})
}

func TestAccountSlotRefusesAModelTheOperatorSwitchedOff(t *testing.T) {
	policies, err := modelpolicy.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := policies.Set("rot", "acct-a", []string{"m1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	srv := &server{opts: Options{ModelPolicies: policies}}

	if _, err := srv.acquireAccountSlot("rot", "acct-a", "m1"); !errors.Is(err, core.ErrBusy) || !IsModelPolicyRefusal(err) {
		t.Fatalf("switched-off model error = %v, want a policy refusal wrapping ErrBusy", err)
	}
	release, err := srv.acquireAccountSlot("rot", "acct-a", "m2")
	if err != nil {
		t.Fatalf("allowed model was refused: %v", err)
	}
	release()
	// An account the module never named, or a request with no model, is not
	// gated by policy: the store cannot be matched against either.
	if _, err := srv.acquireAccountSlot("rot", "acct-b", "m1"); err != nil {
		t.Fatalf("other account was gated: %v", err)
	}
	if _, err := srv.acquireAccountSlot("rot", "acct-a", ""); err != nil {
		t.Fatalf("empty model was gated: %v", err)
	}
	if _, err := srv.acquireAccountSlot("rot", "", "m1"); err != nil {
		t.Fatalf("empty account was gated: %v", err)
	}
	// The refusal must not consume the in-flight slot it never took.
	release, err = srv.acquireAccountSlot("rot", "acct-a", "m2")
	if err != nil {
		t.Fatalf("second acquire after refusal: %v", err)
	}
	release()
}

func TestModelPolicyMakesTheModuleRotateToAnotherAccount(t *testing.T) {
	policies, err := modelpolicy.Open("")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := &rotatingClient{name: "rot", accounts: []string{"acct-a", "acct-b"}}
	srv := newPolicyServer(t, c, policies)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(c.attempted) != 1 || c.attempted[0] != "acct-a" || c.served != "acct-a" {
		t.Fatalf("attempted = %v served = %q, want acct-a only", c.attempted, c.served)
	}

	if err := policies.Set("rot", "acct-a", []string{"m1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c.attempted, c.served = nil, ""
	rec = chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("with policy: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(c.attempted) != 2 || c.attempted[0] != "acct-a" || c.attempted[1] != "acct-b" {
		t.Fatalf("attempted = %v, want acct-a then acct-b", c.attempted)
	}
	if c.served != "acct-b" {
		t.Fatalf("served = %q, want acct-b to take the request", c.served)
	}

	// A provider-prefixed request must match the bare id the account record
	// lists (openaicompat stores bare ids but asks upstream for "<provider>/<model>").
	c2 := &rotatingClient{name: "oc", accounts: []string{"groq"}}
	srv2 := newPolicyServer(t, c2, policies)
	if err := policies.Set("oc", "groq", []string{"llama-3.3-70b"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := (&server{opts: Options{ModelPolicies: policies}}).acquireAccountSlot("oc", "groq", "groq/llama-3.3-70b"); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("provider-prefixed model error = %v, want ErrBusy", err)
	}
	_ = srv2
	if err := policies.Set("oc", "groq", nil); err != nil {
		t.Fatalf("clear: %v", err)
	}

	// A policy for another model must not touch this one.
	if err := policies.Set("rot", "acct-a", []string{"m2"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c.attempted, c.served = nil, ""
	if rec = chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
		t.Fatalf("policy on m2 blocked m1: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(c.attempted) != 1 || c.served != "acct-a" {
		t.Fatalf("attempted = %v served = %q, want acct-a to serve m1", c.attempted, c.served)
	}

	// Every account switched off: the module reports busy and the request is
	// answered as backpressure instead of reaching the vendor.
	if err := policies.Set("rot", "acct-a", []string{"m1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := policies.Set("rot", "acct-b", []string{"m1"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	c.attempted, c.served = nil, ""
	if rec = chat(t, srv, bufferedBody); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("all accounts off: status = %d, want 429, body %s", rec.Code, rec.Body.String())
	}
	if len(c.attempted) != 2 || c.served != "" {
		t.Fatalf("attempted = %v served = %q, want both accounts tried and none served", c.attempted, c.served)
	}
}
