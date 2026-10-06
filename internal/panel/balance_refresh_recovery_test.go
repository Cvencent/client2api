package panel

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// revivableQuotaClient adds core.Reviver to the quota fixture. It records the
// operator override so a test can prove a successful balance read actually
// clears a funded cooling account instead of only changing the number column.
type revivableQuotaClient struct {
	*fakeQuotaClient

	mu      sync.Mutex
	revived []string
}

func (f *revivableQuotaClient) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.AccountRecord(nil), f.accounts...), nil
}

func (f *revivableQuotaClient) ReviveAccount(ctx context.Context, id string) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revived = append(f.revived, id)
	for i := range f.accounts {
		if f.accounts[i].ID == id {
			f.accounts[i].State = "ready"
			f.accounts[i].Note = ""
		}
	}
	return nil
}

func (f *revivableQuotaClient) reviveCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revived...)
}

func TestBalancesRefreshIsPerClientAndReportsProgress(t *testing.T) {
	wb := quotaClient("wb", core.AccountRecord{ID: "w1"})
	wb.balances["w1"] = core.Balance{Credits: 8, Total: 10}
	other := quotaClient("other", core.AccountRecord{ID: "o1"})
	other.balances["o1"] = core.Balance{Credits: 9, Total: 10}
	p, _ := balanceHandler(Options{Registry: registryOf(wb, other), Started: time.Now()})

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/balances/refresh", "")
	if rec.Code != http.StatusOK || out["started"] != true {
		t.Fatalf("refresh response = %d %v, want 200 started:true", rec.Code, out)
	}
	refresh, ok := out["refresh"].(map[string]any)
	if !ok {
		t.Fatalf("refresh status missing: %v", out)
	}
	if refresh["total"] != float64(1) || refresh["running"] != true {
		t.Fatalf("refresh status = %v, want one running account", refresh)
	}
	if got := other.seenSoon(); len(got) != 0 {
		t.Fatalf("refresh of wb touched another client: %v", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(wb.seenSoon()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := wb.seenSoon(); len(got) != 1 {
		t.Fatalf("wb refresh asked the vendor %d time(s), want 1", len(got))
	}
}

func TestBalancesRefreshReportsThrottleInsteadOfSilentNoop(t *testing.T) {
	c := quotaClient("wb", core.AccountRecord{ID: "a1"})
	c.balances["a1"] = core.Balance{Credits: 3, Total: 10}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	if _, first := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/balances/refresh", ""); first["started"] != true {
		t.Fatalf("first refresh did not start: %v", first)
	}
	_, second := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/balances/refresh", "")
	if second["started"] != false {
		t.Fatalf("second refresh = %v, want started:false", second)
	}
	status, _ := second["refresh"].(map[string]any)
	if status == nil || status["retry_after_ms"] == float64(0) {
		t.Fatalf("second refresh did not explain the throttle: %v", second)
	}
}

func TestAccountBalanceRevivesAFundedCoolingAccount(t *testing.T) {
	c := &revivableQuotaClient{fakeQuotaClient: quotaClient("wb", core.AccountRecord{
		ID: "a1", State: "cooling", Enabled: true,
	})}
	c.balances["a1"] = core.Balance{Credits: 12, Total: 100}
	_, h := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/wb/accounts/a1/balance")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := c.reviveCalls(); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("revive calls = %v, want funded cooling account revived", got)
	}
	rows := rowsOf(t, decodeMap(t, rec)["accounts"])
	if len(rows) != 1 || rows[0]["state"] != "ready" {
		t.Fatalf("account row after balance = %v, want ready", rows)
	}
}

func TestBalancesRefreshRevivesFundedCoolingAccounts(t *testing.T) {
	c := &revivableQuotaClient{fakeQuotaClient: quotaClient("wb", core.AccountRecord{
		ID: "a1", State: "cooling", Enabled: true,
	})}
	c.balances["a1"] = core.Balance{Credits: 7, Total: 100}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	if _, out := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/balances/refresh", ""); out["started"] != true {
		t.Fatalf("refresh did not start: %v", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.reviveCalls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.reviveCalls(); len(got) != 1 || got[0] != "a1" {
		t.Fatalf("revive calls = %v, want the refreshed funded account revived", got)
	}
}

// The background sweep (the pass a plain GET /balances starts) must clear a
// verdict that has no clock of its own.  A plan that came back after being
// parked "exhausted" is usable again the moment a funded balance is read, so
// the operator does not have to press 刷新余额 for the state to catch up.
func TestBalancesSoftRefreshClearsATerminalQuotaVerdict(t *testing.T) {
	c := &revivableQuotaClient{fakeQuotaClient: quotaClient("zcode", core.AccountRecord{
		ID: "jwt1", State: "exhausted", Enabled: true,
	})}
	c.balances["jwt1"] = core.Balance{Credits: 100_000_000, Total: 100_000_000}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	doTask(t, p, http.MethodGet, "/panel/api/clients/zcode/balances", "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.reviveCalls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.reviveCalls(); len(got) != 1 || got[0] != "jwt1" {
		t.Fatalf("background refresh revive calls = %v, want the funded exhausted account cleared", got)
	}
}

// A live rate-limit cooldown clears itself, so the background sweep must leave
// it alone: clearing it on every page open would defeat the back-off the vendor
// asked for.  Only an explicit operator refresh forces it.
func TestBalancesSoftRefreshLeavesALiveCooldownAlone(t *testing.T) {
	c := &revivableQuotaClient{fakeQuotaClient: quotaClient("wb", core.AccountRecord{
		ID: "a1", State: "cooling", Enabled: true,
	})}
	c.balances["a1"] = core.Balance{Credits: 5, Total: 10}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	doTask(t, p, http.MethodGet, "/panel/api/clients/wb/balances", "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.seenSoon()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := c.reviveCalls(); len(got) != 0 {
		t.Fatalf("background refresh revived a live cooldown: %v", got)
	}
}
