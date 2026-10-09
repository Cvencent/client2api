package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// gatedSweepClient exercises the shared command-line sweeps. It implements the
// module-side gate as well as both background capabilities they use.
type gatedSweepClient struct {
	*renewFakeClient

	mu           sync.Mutex
	allowed      map[string]bool
	gateCalls    []string
	balanceCalls []string
}

func (f *gatedSweepClient) BackgroundProbeDue(_ context.Context, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateCalls = append(f.gateCalls, id)
	return f.allowed[id]
}

func (f *gatedSweepClient) AccountBalance(_ context.Context, id string, _ time.Duration) (core.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceCalls = append(f.balanceCalls, id)
	return core.Balance{Credits: 1, Total: 10}, nil
}

func (f *gatedSweepClient) AccountBalanceCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.balanceCalls...)
}

func (f *gatedSweepClient) GateCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gateCalls...)
}

func TestRefreshBalancesHonoursBackgroundProbeGate(t *testing.T) {
	f := &gatedSweepClient{
		renewFakeClient: &renewFakeClient{
			name: "loomy",
			accounts: []core.AccountRecord{
				{ID: "quiet", Enabled: true},
				{ID: "due", Enabled: true},
			},
		},
		allowed: map[string]bool{"due": true},
	}
	reg := core.NewRegistry()
	reg.Add(f)

	refreshBalances(context.Background(), reg, 0, quietLogger())

	if got := f.AccountBalanceCalls(); len(got) != 1 || got[0] != "due" {
		t.Fatalf("balance sweep called %v, want only the due account", got)
	}
	if got := f.GateCalls(); len(got) != 2 {
		t.Fatalf("balance sweep consulted the gate %v, want both accounts", got)
	}
}

func TestRefreshBalancesSkipsRecoverableAccounts(t *testing.T) {
	f := &gatedSweepClient{
		renewFakeClient: &renewFakeClient{
			name: "loomy",
			accounts: []core.AccountRecord{
				{ID: "ready", Enabled: true, State: "ready"},
				{ID: "cooling", Enabled: true, State: "cooling"},
				{ID: "exhausted", Enabled: true, State: "exhausted"},
			},
		},
		allowed: map[string]bool{"ready": true, "cooling": true, "exhausted": true},
	}
	reg := core.NewRegistry()
	reg.Add(f)

	refreshBalances(context.Background(), reg, 0, quietLogger())

	if got := f.AccountBalanceCalls(); len(got) != 1 || got[0] != "ready" {
		t.Fatalf("background balance sweep called %v, want only ready", got)
	}
}

func TestRecoveryBalanceSweepIncludesRecoverableAndBypassesGate(t *testing.T) {
	f := &gatedSweepClient{
		renewFakeClient: &renewFakeClient{
			name: "loomy",
			accounts: []core.AccountRecord{
				{ID: "ready", Enabled: true, State: "ready"},
				{ID: "cooling", Enabled: true, State: "cooling"},
				{ID: "invalid", Enabled: true, State: "invalid"},
			},
		},
		allowed: map[string]bool{}, // every background gate vetoes
	}
	reg := core.NewRegistry()
	reg.Add(f)

	refreshBalancesForClient(context.Background(), reg, "loomy", 0, quietLogger(), true)

	if got := f.AccountBalanceCalls(); len(got) != 2 || got[0] != "ready" || got[1] != "cooling" {
		t.Fatalf("recovery balance sweep called %v, want ready+cooling without the gate", got)
	}
	if got := f.GateCalls(); len(got) != 0 {
		t.Fatalf("recovery balance sweep consulted the background gate %v", got)
	}
}

func TestRenewSweepHonoursBackgroundProbeGate(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(5 * time.Minute).UTC().Format(time.RFC3339)
	f := &gatedSweepClient{
		renewFakeClient: &renewFakeClient{
			name: "loomy",
			accounts: []core.AccountRecord{
				{ID: "quiet", Enabled: true, ExpiresAt: expiry, Fields: map[string]any{"refreshable": true}},
			},
		},
		allowed: map[string]bool{},
	}
	reg := core.NewRegistry()
	reg.Add(f)

	refreshExpiringAccounts(context.Background(), reg, now, quietLogger())

	if got := f.called(); len(got) != 1 || got[0] != "quiet" {
		t.Fatalf("renew sweep calls = %v, want the expiring account despite the recovery gate", got)
	}
	if got := f.GateCalls(); len(got) != 0 {
		t.Fatalf("renew sweep consulted the recovery probe gate: %v", got)
	}
}
