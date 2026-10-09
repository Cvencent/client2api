package workbuddy

import (
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestNonChatSuccessDoesNotClearRiskPark(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-risk"}
	p := NewPool([]*Auth{a}, 0)

	if _, d := p.MarkFailure(a, &Error{Kind: ErrAccountFault, Status: 400, Msg: "request illegal"}); d <= 0 {
		t.Fatal("account-fault failure did not create a park")
	}
	if got := p.Snapshot()[0]; got.State != stateRisk {
		t.Fatalf("state after account fault = %q, want %q", got.State, stateRisk)
	}

	p.MarkNonChatSuccess(a)

	got := p.Snapshot()[0]
	if got.State != stateRisk {
		t.Fatalf("state after a chore success = %q, want %q; a check-in must not clear risk control", got.State, stateRisk)
	}
	if !strings.Contains(got.Note, ErrAccountFault.String()) {
		t.Fatalf("note after a chore success = %q, want the risk verdict to survive", got.Note)
	}
}

func TestSnapshotExplainsBreakerPark(t *testing.T) {
	a := &Auth{AccessToken: "at-222222222222", UID: "u-breaker"}
	p := NewPool([]*Auth{a}, 0)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	if !p.RestoreFaults(a, core.FaultSnapshot{
		BreakerUntil: now.Add(45 * time.Minute),
	}) {
		t.Fatal("RestoreFaults did not find the account")
	}

	got := p.Snapshot()[0]
	if got.State != stateCooling {
		t.Fatalf("state while breaker-blocked = %q, want %q", got.State, stateCooling)
	}
	if !strings.Contains(strings.ToLower(got.Note), "breaker") {
		t.Fatalf("note while breaker-blocked = %q, want a breaker explanation", got.Note)
	}
	if !strings.Contains(got.Note, "left") {
		t.Fatalf("note while breaker-blocked = %q, want a countdown", got.Note)
	}
}
