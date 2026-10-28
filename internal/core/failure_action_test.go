package core

import (
	"testing"
)

// The decision table is the whole point of this file: one place that answers
// "try another account here?" and "move to the next platform?" for every kind
// of refusal.  Before it, those two questions were answered in two files --
// Retryable here and modelRefusal in internal/gateway -- so a kind added to one
// and not the other changed routing in a way neither file described.  The same
// defect (11158) had to be fixed twice because of it.
//
// These tests keep the table honest: every kind in the vocabulary has a row,
// the rows say what the incidents taught us, and the two questions stay
// independent.

// TestEveryFailureKindHasAnAction is the guard the roadmap asked for: adding a
// kind to the vocabulary without deciding what to do with it fails here rather
// than silently falling back to "unclassified".
func TestEveryFailureKindHasAnAction(t *testing.T) {
	for _, k := range FailureKinds() {
		if !KnownFailureKind(k) {
			t.Errorf("FailureKind %q has no row in failureActions: decide whether it rotates, "+
				"moves platforms, or parks the account, and add the row with its reasoning", k)
			continue
		}
		if FailureActionReason(k) == "" {
			t.Errorf("FailureKind %q has a row but no documented reason", k)
		}
	}
}

// TestFailureKindsCoversTheVocabulary pins the list FailureKinds returns to the
// constants the package declares.  Without it, someone can add a constant and a
// row, and still leave it out of the completeness walk above.
func TestFailureKindsCoversTheVocabulary(t *testing.T) {
	declared := []FailureKind{
		FailureWAF, FailureRateLimited, FailureQuota, FailureAuth, FailureSessionDead,
		FailureUpstream, FailureContentBlocked, FailureContextWindow, FailureOther,
		FailureModelRefused,
	}
	listed := FailureKinds()
	if len(listed) != len(declared) {
		t.Fatalf("FailureKinds() has %d entries, the package declares %d", len(listed), len(declared))
	}
	seen := map[FailureKind]bool{}
	for _, k := range listed {
		if seen[k] {
			t.Errorf("FailureKinds() lists %q twice", k)
		}
		seen[k] = true
	}
	for _, k := range declared {
		if !seen[k] {
			t.Errorf("FailureKinds() is missing %q", k)
		}
	}
}

// TestRetryAndNextPlatformAreIndependentQuestions is the regression test for the
// confusion that caused the double fix.  A refusal can answer yes to one and no
// to the other, and the table has to allow both.
func TestRetryAndNextPlatformAreIndependentQuestions(t *testing.T) {
	cases := []struct {
		kind         FailureKind
		rotate       bool
		nextPlatform bool
		why          string
	}{
		// Throttling follows the credential: another account on the same
		// platform clears it, but another platform is not needed.
		{FailureRateLimited, true, false, "throttling is account-scoped"},
		// A model-scoped refusal is the mirror image: the account is healthy,
		// but every account here says the same thing.
		{FailureModelRefused, false, true, "the platform cannot serve this id at all"},
		// An unclassified error must never license a retry on a guess.
		{FailureOther, false, false, "retrying on a guess is worse than failing"},
		// A content block is the vendor objecting to the text.
		{FailureContentBlocked, false, false, "the text is what was refused"},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := RotateOn(tc.kind); got != tc.rotate {
				t.Errorf("RotateOn(%v) = %v, want %v (%s)", tc.kind, got, tc.rotate, tc.why)
			}
			if got := NextPlatformOn(tc.kind); got != tc.nextPlatform {
				t.Errorf("NextPlatformOn(%v) = %v, want %v (%s)", tc.kind, got, tc.nextPlatform, tc.why)
			}
		})
	}
}

// TestRetryableMatchesTheTable keeps the public helper and the table from
// drifting apart; Retryable is what modules call, the table is what the gateway
// reads.
func TestRetryableMatchesTheTable(t *testing.T) {
	for _, k := range FailureKinds() {
		if Retryable(k) != RotateOn(k) {
			t.Errorf("Retryable(%v) and RotateOn(%v) disagree", k, k)
		}
	}
}

// TestUnknownKindFallsBackToUnclassified guards the zero value: a kind the
// table has never seen must behave like FailureOther (no rotation), never like
// a licence to retry.
func TestUnknownKindFallsBackToUnclassified(t *testing.T) {
	unknown := FailureKind("some_future_kind")
	if KnownFailureKind(unknown) {
		t.Fatal("the test's own premise is wrong: that kind is in the table")
	}
	if RotateOn(unknown) || NextPlatformOn(unknown) || ParksAccount(unknown) {
		t.Error("an unknown kind must not rotate, move platforms, or park an account")
	}
}

// TestParksAccountMarksTheVerdictsWeRemember documents which refusals are worth
// recording against an account.  Rotation and parking are also independent: a
// 429 rotates without parking (the pool's own cooldown handles it), while an
// exhausted balance does both.
func TestParksAccountMarksTheVerdictsWeRemember(t *testing.T) {
	if !ParksAccount(FailureQuota) {
		t.Error("an exhausted balance must be remembered against the account")
	}
	if !ParksAccount(FailureAuth) {
		t.Error("a rejected credential must be remembered against the account")
	}
	if ParksAccount(FailureModelRefused) {
		t.Error("a model-scoped refusal says nothing about the account and must not park it")
	}
	if ParksAccount(FailureRateLimited) {
		t.Error("throttling is transient; the pool's cooldown owns it, not a parked verdict")
	}
}
