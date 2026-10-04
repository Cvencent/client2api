package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeReviveClient adds core.Reviver on top of the account-manager fake.  It
// records the ids it was asked to revive so a test can tell "the route ran"
// apart from "the route answered 200", which for this endpoint is the whole
// difference between a working escape hatch and a button that lies.
type fakeReviveClient struct {
	*fakeAccountClient
	mu      sync.Mutex
	revived []string
	err     error
}

func (f *fakeReviveClient) ReviveAccount(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.revived = append(f.revived, id)
	return nil
}

func (f *fakeReviveClient) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.revived))
	copy(out, f.revived)
	return out
}

func reviverFor(name string, accounts ...core.AccountRecord) *fakeReviveClient {
	return &fakeReviveClient{fakeAccountClient: &fakeAccountClient{
		fakeClient: &fakeClient{name: name},
		accounts:   accounts,
	}}
}

func TestReviveClearsThePenalties(t *testing.T) {
	c := reviverFor("wb", core.AccountRecord{ID: "uid-1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-1/revive")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := c.calls(); len(got) != 1 || got[0] != "uid-1" {
		t.Fatalf("revived = %v, want exactly [uid-1]", got)
	}
	body := decodeMap(t, rec)
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	// The account table comes back with the answer, so the browser repaints a
	// revived account without a second round trip.
	if _, has := body["accounts"]; !has {
		t.Errorf("body has no accounts list: %v", body)
	}
}

func TestReviveWithoutTheCapability(t *testing.T) {
	// An account manager that holds no runtime penalty at all.  This must be a
	// 501, not a silent 200: the operator has to learn that the button cannot
	// mean anything for this module.
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	h := New(Options{Registry: registryOf(plain), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/kimi/accounts/a1/revive")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", rec.Code, rec.Body.String())
	}
	if msg := decodeMap(t, rec)["error"]; !strings.Contains(msgString(msg), "no runtime penalties") {
		t.Errorf("error = %v, want it to name the missing capability", msg)
	}
}

func TestReviveRejectsTheWrongMethod(t *testing.T) {
	c := reviverFor("trae", core.AccountRecord{ID: "a1"})
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/clients/trae/accounts/a1/revive")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
	}
	if got := c.calls(); len(got) != 0 {
		t.Fatalf("a rejected method still revived %v", got)
	}
}

func TestReviveReportsAGoneAccountAsNotFound(t *testing.T) {
	// The module refuses because it does not know the id, and the account list
	// agrees.  A stale tab must not be told it fixed something that is gone.
	c := reviverFor("wb", core.AccountRecord{ID: "other"})
	c.err = errors.New(`account "uid-9" not found`)
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-9/revive")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if msg := msgString(decodeMap(t, rec)["error"]); !strings.Contains(msg, "account not found") {
		t.Errorf("error = %q, want the 404 wording", msg)
	}
}

func TestReviveReportsAFailureForAnAccountThatIsStillThere(t *testing.T) {
	// Same refusal, but the account is listed: the revive itself failed (an
	// unwritable credential file, say), so this is a 400 carrying the module's
	// own message rather than a 404 sending the operator hunting for the wrong
	// problem.
	c := reviverFor("wb", core.AccountRecord{ID: "uid-1"})
	c.err = errors.New("cannot rename the credential file: permission denied")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-1/revive")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if msg := msgString(decodeMap(t, rec)["error"]); !strings.Contains(msg, "permission denied") {
		t.Errorf("error = %q, want the module's own message", msg)
	}
}

func TestReviveRedactsTheFailure(t *testing.T) {
	// A module that leaks a credential into its error is still a module the
	// panel must not echo verbatim.
	c := reviverFor("wb", core.AccountRecord{ID: "uid-1"})
	c.err = errors.New("cannot revive uid-1: access_token=abcdef123456 rejected")
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/uid-1/revive")
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

func TestCapabilitiesReportRevive(t *testing.T) {
	// The panel hides the button when this flag is false, so the flag has to be
	// true for exactly the modules that implemented the interface -- and a
	// module that did not must keep reporting everything else it does.
	ctx := context.Background()
	if caps := core.CapabilitiesOf(ctx, reviverFor("wb")); !caps.Revive {
		t.Error("a Reviver did not report the revive capability")
	}
	plain := &fakeAccountClient{fakeClient: &fakeClient{name: "kimi"}}
	caps := core.CapabilitiesOf(ctx, plain)
	if caps.Revive {
		t.Error("a module without Reviver reported the revive capability")
	}
	if !caps.Manage {
		t.Error("the fixtures stopped being account managers; the check above proves nothing")
	}
}

func msgString(v any) string {
	s, _ := v.(string)
	return s
}
