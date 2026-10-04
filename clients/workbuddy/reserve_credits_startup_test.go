package workbuddy

import (
	"strings"
	"testing"
)

// A restart restores creditsKnown from pool.json before the platform policy is
// pushed.  Applying the default zero threshold must re-evaluate that staged
// state immediately; otherwise a known-zero account stays green until the next
// scheduled balance sweep.
func TestReserveGuardParksStoredZeroWhenPolicyIsInstalled(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u-zero"}
	writePoolFile(t, dir, persistedPool{Version: poolStateVersion, Accounts: []persistedPoolAccount{{
		ID:           a.ID(),
		Credits:      0,
		CreditsKnown: true,
	}}})

	p := restartPool(t, dir, a)
	if got := statusOf(t, p, a.ID()).State; got != stateReady {
		t.Fatalf("stored account before policy application = %q, want %q", got, stateReady)
	}

	// NewPool starts with the same zero value, so this is the call the startup
	// path makes when the config contains no reserve_credits override.
	p.SetReserveCredits(0)

	got := statusOf(t, p, a.ID())
	if got.State != stateExhausted || !strings.HasPrefix(got.Note, reserveCreditNote) {
		t.Fatalf("stored zero after policy application = %q/%q, want %q/%q",
			got.State, got.Note, stateExhausted, reserveCreditNote)
	}
}
