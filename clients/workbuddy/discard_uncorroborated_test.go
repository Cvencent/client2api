package workbuddy

import (
	"strings"
	"testing"
	"time"
)

// DiscardUncorroboratedCredit is the pool half of the fix for WorkBuddy's
// self-contradicting trial package: a balance the vendor's own reply
// contradicts must not keep an account out of rotation, while a park the
// vendor earned with a real refusal must survive it untouched.

func TestDiscardUncorroboratedCreditLiftsOnlyTheLowCreditGuard(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-zero"}
	p := reserveTestPool(t, a)

	// A known-zero balance parks the account through the low-credit guard.
	p.SetCreditsDetailed(a, 0, 5000, 0, time.Time{}, 0)
	got := statusOf(t, p, a.ID())
	if got.State != stateExhausted || !strings.HasPrefix(got.Note, reserveCreditNote) {
		t.Fatalf("precondition: state/note = %q/%q, want %q/%q",
			got.State, got.Note, stateExhausted, reserveCreditNote)
	}

	// The very reading that parked it is now known to be untrustworthy, so the
	// guard's own park loses its evidence and the account returns to rotation.
	if !p.DiscardUncorroboratedCredit(a) {
		t.Fatal("DiscardUncorroboratedCredit returned false for a guard-created park")
	}
	after := statusOf(t, p, a.ID())
	if after.State != stateReady || after.Note != "" {
		t.Fatalf("state/note = %q/%q, want %q with no note", after.State, after.Note, stateReady)
	}
	if !p.UsableForModel(a.ID(), "any-model") {
		t.Error("the account is still unusable after its park was described as untrustworthy")
	}

	// Nothing to discard a second time.
	if p.DiscardUncorroboratedCredit(a) {
		t.Error("DiscardUncorroboratedCredit lifted a park that was already gone")
	}
}

func TestDiscardUncorroboratedCreditLeavesTheVendorParkAlone(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-hard"}
	p := reserveTestPool(t, a)

	// The vendor itself refused the account for lack of credit: that park has
	// its own evidence and its own deadline.
	p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 429, Msg: "额度已用尽"})
	before := statusOf(t, p, a.ID())
	if before.State != stateExhausted || !strings.HasPrefix(before.Note, ErrHardCredit.String()) {
		t.Fatalf("precondition: state/note = %q/%q, want %q/%q",
			before.State, before.Note, stateExhausted, ErrHardCredit.String())
	}

	if p.DiscardUncorroboratedCredit(a) {
		t.Fatal("DiscardUncorroboratedCredit lifted a park the vendor earned")
	}
	after := statusOf(t, p, a.ID())
	if after.State != before.State || after.Note != before.Note {
		t.Fatalf("vendor park changed: %q/%q became %q/%q", before.State, before.Note, after.State, after.Note)
	}
}

func TestDiscardUncorroboratedCreditIgnoresAnUnparkedAccount(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-ready"}
	p := reserveTestPool(t, a)

	if p.DiscardUncorroboratedCredit(a) {
		t.Error("DiscardUncorroboratedCredit reported a change for a ready account")
	}
	if p.DiscardUncorroboratedCredit(nil) {
		t.Error("DiscardUncorroboratedCredit accepted a nil auth")
	}
}
