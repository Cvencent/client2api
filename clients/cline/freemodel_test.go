package cline

import (
	"testing"
	"time"
)

// A quota park is about money.  A model the vendor charges nothing for spends
// none, so the pool must keep a broke-but-working account in play for it -- and
// must remember the pair the vendor actually refused, so the account is not
// walked into a second time for the same model.

// freeTestPool builds a one-account pool with a clock the test moves by hand.
func freeTestPool(t *testing.T) (*pool, *entry, *time.Time) {
	t.Helper()
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p := newPool("", "", time.Minute, cooldownConfig{def: time.Hour, short: time.Minute, quota: 6 * time.Hour}, nil)
	p.now = func() time.Time { return at }
	e := p.put(account{ID: "acct-1", AccessToken: "tok"})
	if e == nil {
		t.Fatal("pool refused to hold the account")
	}
	return p, e, &at
}

func TestClineFreeModelSurvivesAQuotaPark(t *testing.T) {
	p, e, _ := freeTestPool(t)
	p.markFailure(e, kindQuota, "out of credit")

	if p.availableLockedFor(e, p.now(), false, "cline-free/model") {
		t.Fatal("a quota-parked account must not serve a paid model")
	}
	if !p.availableLockedFor(e, p.now(), true, "cline-free/model") {
		t.Fatal("a quota park must not exclude the account from a free model")
	}
	if got := p.pickFor(nil, true, "cline-free/model"); got != e {
		t.Fatalf("free pick = %v, want the quota-parked account", got)
	}
	if got := p.pickFor(nil, false, "cline-free/model"); got != nil {
		t.Fatalf("paid pick = %v, want no candidate", got)
	}
}

func TestClineFreeModelParkIsScopedToThePair(t *testing.T) {
	p, e, at := freeTestPool(t)
	p.markFailure(e, kindQuota, "out of credit")
	p.ParkFreeModel(e, "cline-free/one")

	if p.availableLockedFor(e, p.now(), true, "cline-free/one") {
		t.Fatal("the refused free model must not be offered again")
	}
	if !p.availableLockedFor(e, p.now(), true, "cline-free/two") {
		t.Fatal("another free model must stay reachable on the same account")
	}
	// The park is tied to the account's own quota window: once the vendor could
	// plausibly have refilled the wallet, the pair is worth probing again.
	*at = at.Add(6*time.Hour + time.Minute)
	if !p.availableLockedFor(e, p.now(), true, "cline-free/one") {
		t.Fatal("the pair park must lapse with the quota window")
	}
}

func TestClineNonQuotaParksStillGateFreeModels(t *testing.T) {
	tests := []struct {
		name string
		park func(p *pool, e *entry)
	}{
		{"a rate limit", func(p *pool, e *entry) { p.markFailure(e, kindTransient, "slow down") }},
		{"an auth park", func(p *pool, e *entry) { p.markFailure(e, kindAuth, "token rejected") }},
		{"a disabled account", func(p *pool, e *entry) { e.acct.Disabled = true }},
		{"a dead credential", func(p *pool, e *entry) { p.markDead(e, "revoked") }},
	}
	for _, tc := range tests {
		p, e, _ := freeTestPool(t)
		tc.park(p, e)
		if p.availableLockedFor(e, p.now(), true, "cline-free/model") {
			t.Errorf("%s must exclude the account from a free model too", tc.name)
		}
	}
}

func TestClineFreeSuccessKeepsTheQuotaVerdict(t *testing.T) {
	p, e, _ := freeTestPool(t)
	p.markFailure(e, kindQuota, "out of credit")

	p.markUsedFor(e, true)
	if e.state != stateExhausted {
		t.Fatalf("a free success left the account in state %q, want the quota verdict kept", e.state)
	}
	// A paid success is evidence the account can pay again, which is what the
	// pre-existing rule says -- pin it so the free path cannot quietly change it.
	p.markUsedFor(e, false)
	if e.state != stateReady {
		t.Fatalf("a paid success left the account in state %q, want ready", e.state)
	}
}
