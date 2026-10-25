package gateway

import (
	"testing"

	"client2api/internal/core"
)

// The per-account counter must keep counting on platforms the operator left
// unlimited.  The refusal is the only thing that needs a ceiling; the panel
// reads the same counter to fill the account pool's 在途 column, and a
// platform with no ceiling would otherwise always report 0 there.
func TestAccountInFlightCountsEvenWithoutACeiling(t *testing.T) {
	st := NewInFlightTrackers()

	if got := st.AccountInFlight("p", "acct-1"); got != 0 {
		t.Fatalf("unknown account in-flight = %d, want 0", got)
	}

	release1, err := st.acquireAccount("p", "acct-1", 0)
	if err != nil {
		t.Fatalf("acquire with no ceiling: %v", err)
	}
	if got := st.AccountInFlight("p", "acct-1"); got != 1 {
		t.Fatalf("in-flight after first acquire = %d, want 1", got)
	}
	release2, err := st.acquireAccount("p", "acct-1", 0)
	if err != nil {
		t.Fatalf("second acquire with no ceiling: %v", err)
	}
	if got := st.AccountInFlight("p", "acct-1"); got != 2 {
		t.Fatalf("in-flight after second acquire = %d, want 2", got)
	}
	// The counter is per (platform, account): another account and another
	// platform must not share the budget.
	if got := st.AccountInFlight("p", "acct-2"); got != 0 {
		t.Fatalf("other account in-flight = %d, want 0", got)
	}
	if got := st.AccountInFlight("other", "acct-1"); got != 0 {
		t.Fatalf("other platform in-flight = %d, want 0", got)
	}

	release1()
	if got := st.AccountInFlight("p", "acct-1"); got != 1 {
		t.Fatalf("in-flight after first release = %d, want 1", got)
	}
	release2()
	release2() // must be idempotent
	if got := st.AccountInFlight("p", "acct-1"); got != 0 {
		t.Fatalf("in-flight after both releases = %d, want 0", got)
	}
}

// A configured ceiling still refuses the extra slot even though the counter
// now always runs.
func TestAccountInFlightStillRefusesAtTheCeiling(t *testing.T) {
	st := NewInFlightTrackers()
	release, err := st.acquireAccount("p", "acct-1", 1)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := st.acquireAccount("p", "acct-1", 1); err != core.ErrBusy {
		t.Fatalf("second acquire error = %v, want ErrBusy", err)
	}
	release()
	if _, err := st.acquireAccount("p", "acct-1", 1); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
