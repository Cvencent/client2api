package panel

import (
	"context"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// background_probe_gate_test.go pins the shared half of the quiet-probe
// contract: a module may veto a background sweep for one account without
// failing the row, and an explicit operator refresh must ignore that veto.
// Without this split, either the sweeps keep hammering a penalised account or
// the operator's own button stops working.

type backgroundProbeGateClient struct {
	*fakeQuotaClient

	mu       sync.Mutex
	allowed  map[string]bool
	gateSeen []string
}

func (f *backgroundProbeGateClient) BackgroundProbeDue(ctx context.Context, id string) bool {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateSeen = append(f.gateSeen, id)
	return f.allowed[id]
}

func (f *backgroundProbeGateClient) seenBalanceIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.balID...)
}

func (f *backgroundProbeGateClient) gateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gateSeen...)
}

func TestBackgroundBalanceSweepSkipsAccountsTheModuleVetoes(t *testing.T) {
	c := &backgroundProbeGateClient{
		fakeQuotaClient: quotaClient("loomy",
			core.AccountRecord{ID: "quiet", Enabled: true},
			core.AccountRecord{ID: "due", Enabled: true},
		),
		allowed: map[string]bool{"due": true},
	}
	c.balances["quiet"] = core.Balance{Credits: 1, Total: 10}
	c.balances["due"] = core.Balance{Credits: 2, Total: 10}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	status, started := p.balanceCache.refreshClient(
		context.Background(), "loomy", balanceRefreshBatch, false, reviveTerminal,
	)
	if !started || !status.Running {
		t.Fatalf("background refresh did not start: status=%+v started=%v", status, started)
	}
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.balanceCache.statusForClient("loomy"); !st.Running && st.Done >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := c.seenBalanceIDs(); len(got) != 1 || got[0] != "due" {
		t.Fatalf("vendor balance reads = %v, want only the due account", got)
	}
	for _, id := range c.gateCalls() {
		if id == "" {
			t.Fatalf("the gate was called with an empty account id: %v", c.gateCalls())
		}
	}
}

func TestExplicitBalanceRouteIgnoresTheBackgroundGate(t *testing.T) {
	c := &backgroundProbeGateClient{
		fakeQuotaClient: quotaClient("loomy", core.AccountRecord{ID: "quiet", Enabled: true}),
		allowed:         map[string]bool{}, // veto every background probe
	}
	c.balances["quiet"] = core.Balance{Credits: 9, Total: 10}
	_, h := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	rec := post(t, h, "/panel/api/clients/loomy/accounts/quiet/balance")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := c.seenBalanceIDs(); len(got) != 1 || got[0] != "quiet" {
		t.Fatalf("explicit balance read = %v, want the vetoed account", got)
	}
}

func TestSoftBalanceSweepSkipsRecoverableAccounts(t *testing.T) {
	c := &backgroundProbeGateClient{
		fakeQuotaClient: quotaClient("loomy",
			core.AccountRecord{ID: "ready", Enabled: true, State: "ready"},
			core.AccountRecord{ID: "cooling", Enabled: true, State: "cooling"},
			core.AccountRecord{ID: "exhausted", Enabled: true, State: "exhausted"},
		),
		allowed: map[string]bool{"ready": true, "cooling": true, "exhausted": true},
	}
	c.balances["ready"] = core.Balance{Credits: 3, Total: 10}
	c.balances["cooling"] = core.Balance{Credits: 2, Total: 10}
	c.balances["exhausted"] = core.Balance{Credits: 1, Total: 10}
	p, _ := balanceHandler(Options{Registry: registryOf(c), Started: time.Now()})

	status, started := p.balanceCache.refreshClient(
		context.Background(), "loomy", balanceRefreshBatch, false, reviveTerminal,
	)
	if !started || !status.Running {
		t.Fatalf("soft refresh did not start: status=%+v started=%v", status, started)
	}
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.balanceCache.statusForClient("loomy"); !st.Running && st.Done >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := c.seenBalanceIDs(); len(got) != 1 || got[0] != "ready" {
		t.Fatalf("soft balance reads = %v, want only ready", got)
	}
}
