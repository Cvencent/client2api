package workbuddy

import (
	"testing"
	"time"

	"strings"

	"client2api/internal/core"
)

// The low-balance guard is what stops a spent account from staying in the
// rotation: an account whose last *known* balance is at or below the platform's
// reserve is parked until the balance rises again, which is how a daily
// check-in or a top-up brings it back on its own.  These tests pin the guard's
// decisions, because a silently broken guard looks exactly like a working one
// until a request lands on a dead account.

// reserveTestPool builds a pool with a fixed clock so the "next 04:00" park
// deadline is deterministic and easy to reason about.
func reserveTestPool(t *testing.T, accounts ...*Auth) *Pool {
	t.Helper()
	p := NewPool(accounts, 0)
	p.now = func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.Local) }
	return p
}

// statusOf returns the pool's published status for one account.
func statusOf(t *testing.T, p *Pool, id string) core.AccountStatus {
	t.Helper()
	for _, s := range p.Snapshot() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("pool has no status for %q", id)
	return core.AccountStatus{}
}

func TestReserveGuardParksAKnownZeroBalanceAndRevivesIt(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-zero"}
	b := &Auth{AccessToken: "at-222222222222", UID: "u-rich"}
	p := reserveTestPool(t, a, b)

	// Unknown balances never park: before any balance read there is nothing to
	// act on, and a fresh install must not park everything it owns.
	if got := statusOf(t, p, a.ID()).State; got != stateReady {
		t.Fatalf("state before any balance read = %q, want ready", got)
	}

	p.SetCreditsDetailed(b, 100, 100, 0, time.Time{}, 0)
	p.SetCreditsDetailed(a, 0, 5000, 0, time.Time{}, 0)

	got := statusOf(t, p, a.ID())
	// Snapshot appends the remaining cooldown to a live note, so match the prefix.
	if got.State != stateExhausted || !strings.HasPrefix(got.Note, reserveCreditNote) {
		t.Fatalf("zero-balance account = %q/%q, want %q/%q",
			got.State, got.Note, stateExhausted, reserveCreditNote)
	}
	if p.UsableForModel(a.ID(), "any-model") {
		t.Error("a parked zero-balance account must not be usable")
	}
	for i := 0; i < 4; i++ {
		auth, ok := p.Pick(nil)
		if !ok {
			t.Fatal("a funded account exists, so Pick must succeed")
		}
		if auth.ID() == a.ID() {
			t.Fatalf("pick %d returned the parked zero-balance account", i)
		}
	}

	// The vendor granting credit again is the evidence that lifts the park.
	p.SetCreditsDetailed(a, 6, 5000, 0, time.Time{}, 0)
	if got := statusOf(t, p, a.ID()); got.State != stateReady || got.Note != "" {
		t.Fatalf("account after a top-up = %q/%q, want ready with no note", got.State, got.Note)
	}
}

func TestReserveGuardThresholdAndOffSwitch(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := reserveTestPool(t, a)

	p.SetReserveCredits(10)
	p.SetCreditsDetailed(a, 10, 100, 0, time.Time{}, 0)
	if got := statusOf(t, p, a.ID()).State; got != stateExhausted {
		t.Fatalf("balance exactly at the reserve = %q, want %q", got, stateExhausted)
	}
	p.SetCreditsDetailed(a, 11, 100, 0, time.Time{}, 0)
	if got := statusOf(t, p, a.ID()).State; got != stateReady {
		t.Fatalf("balance above the reserve = %q, want %q", got, stateReady)
	}

	// Raising the threshold re-parks immediately, without waiting for the next
	// balance sweep: the operator's change must be visible at once.
	p.SetReserveCredits(50)
	if got := statusOf(t, p, a.ID()).State; got != stateExhausted {
		t.Fatalf("state after raising the reserve = %q, want %q", got, stateExhausted)
	}
	if got := p.ReserveCredits(); got != 50 {
		t.Fatalf("ReserveCredits() = %d, want 50", got)
	}

	// A negative reserve turns the guard off and releases its own park.
	p.SetReserveCredits(-1)
	if got := statusOf(t, p, a.ID()).State; got != stateReady {
		t.Fatalf("state after disabling the guard = %q, want %q", got, stateReady)
	}
}

func TestReserveGuardLeavesAnotherParkAlone(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := reserveTestPool(t, a)

	p.MarkFailure(a, &Error{Kind: ErrSoftRate, Status: 429, RetryAfter: 30 * time.Minute, Msg: "slow down"})
	before := statusOf(t, p, a.ID())
	if before.State != stateCooling {
		t.Fatalf("state after a rate limit = %q, want %q", before.State, stateCooling)
	}

	// A balance read must not overwrite a park another mechanism owns; the next
	// sweep re-applies the guard once that park lapses.
	p.SetCreditsDetailed(a, 0, 100, 0, time.Time{}, 0)
	after := statusOf(t, p, a.ID())
	if after.State != before.State || after.Note != before.Note {
		t.Fatalf("a balance read clobbered a rate-limit park: %q/%q became %q/%q",
			before.State, before.Note, after.State, after.Note)
	}
}

func TestSpendingTheLastCreditParksImmediately(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := reserveTestPool(t, a)
	p.SetCreditsDetailed(a, 3, 100, 0, time.Time{}, 0)
	if got := statusOf(t, p, a.ID()).State; got != stateReady {
		t.Fatalf("state with credit left = %q, want %q", got, stateReady)
	}

	// 3 credits over 1000 tokens: the ledger deducts the measured spend, which
	// here empties the balance, so the guard has to park without waiting for a
	// sweep or for the next request to fail.
	p.NoteModelCost(a, "some-model", 3, 1000)
	if got := statusOf(t, p, a.ID()); got.State != stateExhausted || !strings.HasPrefix(got.Note, reserveCreditNote) {
		t.Fatalf("state after spending the last credit = %q/%q, want %q/%q",
			got.State, got.Note, stateExhausted, reserveCreditNote)
	}
}

func TestApplyPlatformPolicyInstallsTheReserveThreshold(t *testing.T) {
	c := poolWithPolicy(t)

	c.ApplyPlatformPolicy(core.PlatformConfig{ReserveCredits: 7})
	if got := c.pool.ReserveCredits(); got != 7 {
		t.Fatalf("ReserveCredits() = %d after ApplyPlatformPolicy, want 7", got)
	}
	c.ApplyPlatformPolicy(core.PlatformConfig{ReserveCredits: -1})
	if got := c.pool.ReserveCredits(); got != -1 {
		t.Fatalf("ReserveCredits() = %d after disabling, want -1", got)
	}
}
