package panel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeModelPolicyClient is an account manager that also implements
// core.AccountModelPolicyManager.  It records every call so a test can tell
// "the route ran" apart from "the route answered 200" -- for a write endpoint
// that is the whole difference between a working control and a button that
// lies.
type fakeModelPolicyClient struct {
	*fakeAccountClient
	mu    sync.Mutex
	calls []modelPolicyCall
	err   error
}

type modelPolicyCall struct {
	id       string
	disabled []string
	force    bool
}

func (f *fakeModelPolicyClient) SetAccountModelPolicy(_ context.Context, id string, disabled []string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, modelPolicyCall{id: id, disabled: append([]string(nil), disabled...), force: force})
	return nil
}

func (f *fakeModelPolicyClient) recorded() []modelPolicyCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]modelPolicyCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func modelPolicyFor(name string, accounts ...core.AccountRecord) *fakeModelPolicyClient {
	return &fakeModelPolicyClient{fakeAccountClient: &fakeAccountClient{
		fakeClient: &fakeClient{name: name},
		accounts:   accounts,
	}}
}

// TestModelPolicyRouteAppliesAndRelists pins the whole contract of the
// per-account model policy endpoint: the module is actually called with the
// operator's blacklist and force flag, the account table comes back so the
// browser can repaint without a second round trip, and the capability is
// advertised so the panel knows to render the control at all.
func TestModelPolicyRouteAppliesAndRelists(t *testing.T) {
	c := modelPolicyFor("wb", core.AccountRecord{ID: "uid-1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/accounts/uid-1/model-policy",
		`{"disabled_models":["expensive-model"],"force_enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	got := c.recorded()
	if len(got) != 1 {
		t.Fatalf("module calls = %d, want 1", len(got))
	}
	if got[0].id != "uid-1" || !got[0].force {
		t.Fatalf("call = %+v, want id uid-1 force true", got[0])
	}
	if len(got[0].disabled) != 1 || got[0].disabled[0] != "expensive-model" {
		t.Fatalf("disabled = %v, want [expensive-model]", got[0].disabled)
	}
	if _, has := decodeMap(t, rec)["accounts"]; !has {
		t.Errorf("body has no accounts list: %s", rec.Body.String())
	}
}

func TestModelPolicyWithoutTheCapability(t *testing.T) {
	// An account manager with no per-model routing.  This must be a 501, not a
	// silent 200: the operator has to learn the control cannot mean anything
	// for this module.
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/kimi/accounts/a1/model-policy",
		`{"disabled_models":[],"force_enabled":false}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
	if msg := msgString(decodeMap(t, rec)["error"]); !strings.Contains(msg, "model") {
		t.Errorf("error = %q, want it to name the missing capability", msg)
	}
}

func TestModelPolicyRejectsTheWrongMethod(t *testing.T) {
	c := modelPolicyFor("wb", core.AccountRecord{ID: "uid-1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/wb/accounts/uid-1/model-policy")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got := c.recorded(); len(got) != 0 {
		t.Fatalf("a rejected method still wrote the policy: %+v", got)
	}
}

func TestModelPolicyReportsAGoneAccountAsNotFound(t *testing.T) {
	c := modelPolicyFor("wb", core.AccountRecord{ID: "other"})
	c.err = errString(`account "uid-9" not found`)
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/accounts/uid-9/model-policy",
		`{"disabled_models":[],"force_enabled":false}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if msg := msgString(decodeMap(t, rec)["error"]); !strings.Contains(msg, "account not found") {
		t.Errorf("error = %q, want the 404 wording", msg)
	}
}

func TestModelPolicyRedactsTheFailure(t *testing.T) {
	c := modelPolicyFor("wb", core.AccountRecord{ID: "uid-1"})
	c.err = errString("cannot write uid-1: access_token=abcdef123456 rejected")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := hitRoute(t, h, http.MethodPost, "/panel/api/clients/wb/accounts/uid-1/model-policy",
		`{"disabled_models":[],"force_enabled":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "abcdef123456") {
		t.Errorf("the token reached the browser: %s", body)
	}
	if !strings.Contains(body, "access_token=<redacted>") {
		t.Errorf("the credential was not redacted in place: %s", body)
	}
}
