package main

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// selfHealFakeClient is gatedSweepClient with the marker loomy carries
// (core.SelfHealableInvalid): its invalid rows include self-healable marks, so
// the sweep's invalid door opens for it -- paced, as always, by the module's
// own BackgroundProbeDue (issues #168/#169).
type selfHealFakeClient struct {
	gatedSweepClient
}

func (f *selfHealFakeClient) InvalidSelfHeals() {}

// TestSweepReachesInvalidRowsOfASelfHealableModule pins the other half of the
// invalid door: TestRefreshBalancesSkipsInvalidAccounts pins that a module
// WITHOUT the marker keeps its invalid rows closed (issue #52); this pins that
// one WITH the marker gets them probed -- exactly once per sweep, still subject
// to the module's own quiet-probe gate.
func TestSweepReachesInvalidRowsOfASelfHealableModule(t *testing.T) {
	f := &selfHealFakeClient{gatedSweepClient{
		renewFakeClient: &renewFakeClient{
			name: "loomy",
			accounts: []core.AccountRecord{
				{ID: "invalid-abnormal", Enabled: true, State: "invalid"},
				{ID: "invalid-expired", Enabled: true, State: "INVALID"},
			},
		},
		allowed: map[string]bool{"invalid-abnormal": true}, // expired one is inside its quiet window
	}}
	reg := core.NewRegistry()
	reg.Add(f)

	refreshBalances(context.Background(), reg, 0, quietLogger(), nil)

	if got := f.AccountBalanceCalls(); len(got) != 1 || got[0] != "invalid-abnormal" {
		t.Fatalf("balance sweep called %v, want only the gate-approved invalid row (issues #168/#169)", got)
	}
	for _, asked := range f.GateCalls() {
		if asked != "invalid-abnormal" && asked != "invalid-expired" {
			t.Errorf("gate was consulted for %q; only invalid rows of a self-healable module reach the gate", asked)
		}
	}
}
