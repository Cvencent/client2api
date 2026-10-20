package panel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

type fakeInviteClient struct {
	*fakeAccountClient
	mu       sync.Mutex
	defaultC string
	bind     []string
	redeem   []string
}

func (f *fakeInviteClient) InviteCodeDefault(context.Context) string { return f.defaultC }
func (f *fakeInviteClient) BindAccountInviteCode(_ context.Context, id, code string) (core.InviteCodeResult, error) {
	f.mu.Lock()
	f.bind = append(f.bind, id+":"+code)
	f.mu.Unlock()
	return core.InviteCodeResult{AccountID: id, Code: code, OK: true, AppliedCode: code, Reward: 10}, nil
}
func (f *fakeInviteClient) RedeemAccountCode(_ context.Context, id, code string) (core.RedemptionResult, error) {
	f.mu.Lock()
	f.redeem = append(f.redeem, id+":"+code)
	f.mu.Unlock()
	return core.RedemptionResult{AccountID: id, Code: code, OK: true, Points: 20, Balance: 120}, nil
}

func (f *fakeInviteClient) bound() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bind...)
}

func (f *fakeInviteClient) redeemed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.redeem...)
}

func invitePanel(t *testing.T, accounts ...core.AccountRecord) (*panel, *fakeInviteClient) {
	t.Helper()
	c := &fakeInviteClient{
		fakeAccountClient: &fakeAccountClient{
			fakeClient: &fakeClient{name: "loomy"},
			accounts:   accounts,
		},
		defaultC: "P276D2",
	}
	return taskPanel(t, c), c
}

func TestInviteCapabilitiesAndList(t *testing.T) {
	p, c := invitePanel(t,
		core.AccountRecord{ID: "a1", Label: "one", Enabled: true, Fields: map[string]any{"invite_code": "OLD", "invite_activated": true}},
		core.AccountRecord{ID: "a2", Label: "two", Enabled: false},
	)
	_, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/invites", "")
	if out["default_code"] != "P276D2" {
		t.Fatalf("default_code = %#v", out["default_code"])
	}
	rows := rowsOf(t, out["accounts"])
	if len(rows) != 2 || rows[0]["code"] != "OLD" || rows[0]["activated"] != true {
		t.Fatalf("rows = %#v, want the cached binding on a1", rows)
	}
	if len(c.bind) != 0 {
		t.Fatal("GET /invites touched the vendor; it must be a cache read")
	}

	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.InviteCodes || caps.InviteCodeDefault != "P276D2" {
		t.Fatalf("capabilities = %+v, want invite support with the default", caps)
	}
}

func TestBindInvitesAllSkipsDisabledAccounts(t *testing.T) {
	p, c := invitePanel(t,
		core.AccountRecord{ID: "a1", Enabled: true},
		core.AccountRecord{ID: "a2", Enabled: false},
		core.AccountRecord{ID: "a3", Enabled: true},
	)
	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/loomy/invites/bind", `{"code":"P276D2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if out["ok"] != float64(2) || out["failed"] != float64(0) {
		t.Fatalf("summary = %#v", out)
	}
	if got := strings.Join(c.bound(), ","); got != "a1:P276D2,a3:P276D2" {
		t.Fatalf("bind calls = %q", got)
	}
}

func TestBindInvitesOneAccountAndRedeem(t *testing.T) {
	p, c := invitePanel(t,
		core.AccountRecord{ID: "a1", Enabled: true},
		core.AccountRecord{ID: "a2", Enabled: true},
	)
	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/loomy/invites/bind", `{"account":"a2","code":"ALT"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(c.bound(), ","); got != "a2:ALT" {
		t.Fatalf("bind calls = %q", got)
	}

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/loomy/accounts/a1/redeem", `{"code":"V-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem status = %d: %s", rec.Code, rec.Body.String())
	}
	res, _ := out["result"].(map[string]any)
	if res["ok"] != true || res["points"] != float64(20) {
		t.Fatalf("redeem result = %#v", out["result"])
	}
	if got := strings.Join(c.redeemed(), ","); got != "a1:V-1" {
		t.Fatalf("redeem calls = %q", got)
	}
}

func TestBindInvitesRejectsAnUnknownAccount(t *testing.T) {
	p, c := invitePanel(t, core.AccountRecord{ID: "a1", Enabled: true})
	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/loomy/invites/bind", `{"account":"missing"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(c.bound()) != 0 {
		t.Fatal("an unknown account still reached the vendor")
	}
}
