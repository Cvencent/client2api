package loomy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

// The operator asked for a quieter recovery cadence: after a failure, a Loomy
// account may still be selected by a real request once its ordinary cooldown
// lapses, but the shared background sweeps must leave it alone for 3..6 hours.
// These tests pin both halves: the gate defers only background probes, and a
// manual test that succeeds clears the penalty outright.
func TestPenalisedAccountDefersBackgroundProbesForThreeToSixHours(t *testing.T) {
	rt := alwaysJSON(200, okEnvelope(`{"balance":5}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	now := time.Now().UTC()
	c.store.penalise("loomy-1", &apiError{Status: 500, Message: "boom"}, now, time.Minute)

	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the penalised account vanished")
	}
	first := acc.probeAfter.Sub(now)
	if first < 3*time.Hour || first > 6*time.Hour {
		t.Fatalf("first background probe in %s, want between 3h and 6h", first)
	}
	if c.store.backgroundProbeDue("loomy-1", now, backgroundProbeDelay) {
		t.Fatal("a background probe was allowed inside the quiet window")
	}
	if !c.store.backgroundProbeDue("loomy-1", now.Add(6*time.Hour+time.Minute), backgroundProbeDelay) {
		t.Fatal("a background probe was still blocked after the quiet window")
	}
	after, _ := c.store.lookup("loomy-1")
	next := after.probeAfter.Sub(now.Add(6*time.Hour + time.Minute))
	if next < 3*time.Hour || next > 6*time.Hour {
		t.Fatalf("next background probe in %s, want another 3h..6h window", next)
	}
	if c.store.backgroundProbeDue("loomy-1", now.Add(6*time.Hour+time.Minute), backgroundProbeDelay) {
		t.Fatal("the gate allowed two background probes at once")
	}
	if rt.count() != 0 {
		t.Fatalf("the gate itself touched the vendor %d time(s)", rt.count())
	}
}

func TestExplicitBalanceReadIgnoresTheQuietWindow(t *testing.T) {
	rt := alwaysJSON(200, okEnvelope(`{"balance":7}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", &apiError{Status: 500, Message: "boom"}, time.Now().UTC(), time.Minute)

	bal, err := c.AccountBalance(context.Background(), "loomy-1", 0)
	if err != nil {
		t.Fatalf("explicit balance read: %v", err)
	}
	if bal.Credits != 7 {
		t.Fatalf("credits = %d, want 7", bal.Credits)
	}
	if rt.count() != 1 {
		t.Fatalf("explicit balance read asked the vendor %d time(s), want 1", rt.count())
	}
}

func TestSuccessfulAccountTestClearsEveryPenalty(t *testing.T) {
	c := newTestClient(t, "{}", alwaysSSE("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", &apiError{Status: 500, Message: "boom"}, time.Now().UTC(), time.Minute)

	result, err := c.TestAccount(context.Background(), "loomy-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !result.OK {
		t.Fatalf("TestAccount OK = false: %+v", result)
	}
	acc, _ := c.store.lookup("loomy-1")
	if acc.failures != 0 || !acc.cooldownTill.IsZero() || !acc.probeAfter.IsZero() || acc.lastError != "" {
		t.Fatalf("successful test left penalties behind: %+v", acc)
	}
}

func TestSessionDeadAccountIsNeverBackgroundProbed(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, failureEnvelope("100002", "dead")))
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now().UTC(), time.Minute)

	if c.store.backgroundProbeDue("loomy-1", time.Now().UTC().Add(7*time.Hour), backgroundProbeDelay) {
		t.Fatal("a dead session was handed to a background probe; only a new import can fix it")
	}
}

func TestDisabledAccountIsNeverBackgroundProbed(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)
	if err := c.store.setEnabled("loomy-1", false); err != nil {
		t.Fatalf("setEnabled: %v", err)
	}
	if c.store.backgroundProbeDue("loomy-1", time.Now().UTC().Add(7*time.Hour), backgroundProbeDelay) {
		t.Fatal("an operator-parked account was handed to a background probe")
	}
}

func TestLegacyPersistedCooldownGetsAQuietProbeWindow(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)
	now := time.Now().UTC()
	state := stateFile{Accounts: map[string]accountState{
		"loomy-1": {CooldownTill: now.Add(time.Minute).Format(time.RFC3339)},
	}}
	if err := core.WriteJSONAtomic(c.store.statePath, state); err != nil {
		t.Fatalf("write legacy state: %v", err)
	}

	reloaded := newTestClientInDir(t, filepath.Dir(c.store.path), "{}", nil)
	acc, ok := reloaded.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished across the state migration")
	}
	quiet := acc.probeAfter.Sub(now)
	if quiet < 3*time.Hour || quiet > 6*time.Hour+time.Minute {
		t.Fatalf("legacy cooldown got a %s probe delay, want 3h..6h", quiet)
	}
	if reloaded.store.backgroundProbeDue("loomy-1", now.Add(2*time.Hour), backgroundProbeDelay) {
		t.Fatal("a migrated penalty was probed inside its quiet window")
	}
	again := newTestClientInDir(t, filepath.Dir(c.store.path), "{}", nil)
	reloadedAgain, _ := again.store.lookup("loomy-1")
	if !reloadedAgain.probeAfter.Equal(acc.probeAfter) {
		if !reloadedAgain.probeAfter.Truncate(time.Second).Equal(acc.probeAfter.Truncate(time.Second)) {
			t.Fatalf("the migrated probe window changed across a restart: %s -> %s", acc.probeAfter, reloadedAgain.probeAfter)
		}
	}
}
