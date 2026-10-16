package workbuddy

import (
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// The free tier is the one pricing fact with a routing consequence: a model the
// vendor charges nothing for does not depend on the account's balance, so the
// pool must keep a broke-but-working account in play for it.  These tests pin
// both halves of that rule -- the catalogue marker that tells the router a model
// is free, and the pool's willingness to serve it from a credit park.

// --- catalogue: which models are free ---------------------------------------

func TestWorkbuddyModelRateParsesTheVendorSpellings(t *testing.T) {
	tests := []struct {
		raw   string
		want  float64
		wantK bool
	}{
		{"x0.15 credits", 0.15, true},
		{"x0.29", 0.29, true},
		{"  x1 credits  ", 1, true},
		{"0.5", 0.5, true},
		{"x0.00 credits", 0, true},
		{"免费", 0, true},
		{"free", 0, true},
		{"FREE_UNLIMITED", 0, true},
		{"", 0, false},
		{"credits", 0, false},
		{"高", 0, false},
		{"-1", 0, false},
	}
	for _, tc := range tests {
		got, ok := modelRate(tc.raw)
		if ok != tc.wantK || (ok && got != tc.want) {
			t.Errorf("modelRate(%q) = %v/%v, want %v/%v", tc.raw, got, ok, tc.want, tc.wantK)
		}
	}
}

func TestWorkbuddyModelFreeFollowsTheEffectivePrice(t *testing.T) {
	half := 0.5
	tests := []struct {
		name string
		mi   ModelInfo
		want bool
	}{
		{"list price is zero", ModelInfo{Credits: "x0.00 credits"}, true},
		{"a free campaign is in force", ModelInfo{Credits: "x1", PromoCredits: "x0.00 credits", PromoFactor: &half}, true},
		{"a paid campaign outranks a free list price", ModelInfo{Credits: "x0.00", PromoCredits: "x0.15", PromoFactor: &half}, false},
		{"a badge-only campaign leaves the list price", ModelInfo{Credits: "x0.00", PromoLabel: "错峰优惠"}, true},
		{"a paid list price", ModelInfo{Credits: "x0.29"}, false},
		{"no price at all is not free", ModelInfo{}, false},
		{"an unreadable price is not free", ModelInfo{Credits: "按量计费"}, false},
	}
	for _, tc := range tests {
		if got := modelIsFree(tc.mi); got != tc.want {
			t.Errorf("%s: modelIsFree(%q/%q) = %v, want %v", tc.name, tc.mi.Credits, tc.mi.PromoCredits, got, tc.want)
		}
	}
}

func TestWorkbuddyModelExtraPublishesTheFreeMarker(t *testing.T) {
	half := 0.5
	free := modelExtra(ModelInfo{ID: "wb-free", Credits: "x0.00 credits"})
	if free["free"] != true {
		t.Fatalf("free marker = %v, want true for a zero multiplier", free["free"])
	}
	promo := modelExtra(ModelInfo{ID: "wb-promo", Credits: "x1", PromoCredits: "x0.00 credits", PromoFactor: &half})
	if promo["free"] != true {
		t.Fatalf("free marker = %v, want true while the free campaign runs", promo["free"])
	}
	paid := modelExtra(ModelInfo{ID: "wb-paid", Credits: "x0.29"})
	if _, ok := paid["free"]; ok {
		t.Fatalf("a paid model must not carry the free marker: %v", paid["free"])
	}
	unknown := modelExtra(ModelInfo{ID: "wb-unknown"})
	if _, ok := unknown["free"]; ok {
		t.Fatalf("an unpriced model must not carry the free marker: %v", unknown["free"])
	}
}

// --- pool: a credit park does not gate a free model -------------------------

// reserveParkedPool builds a single-account pool whose only credential has a
// known zero balance, which is the state the low-balance guard parks.
func reserveParkedPool(t *testing.T, a *Auth) (*Pool, *pickClock) {
	t.Helper()
	p, clk := newPickPool([]*Auth{a})
	p.SetCreditsDetailed(a, 0, 5000, 0, time.Time{}, 0)
	if got := statusOf(t, p, a.ID()).State; got != stateExhausted {
		t.Fatalf("account state after reporting a zero balance = %q, want %q", got, stateExhausted)
	}
	return p, clk
}

func TestWorkbuddyFreeModelIsServedFromAZeroBalanceAccount(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-broke"}
	p, _ := reserveParkedPool(t, a)

	if _, ok := p.PickForModelFor(nil, "paid-model", false); ok {
		t.Fatal("a zero-balance account must not serve a paid model")
	}
	got, ok := p.PickForModelFor(nil, "free-model", true)
	if !ok || got == nil || got.ID() != a.ID() {
		t.Fatalf("free pick = %v/%v, want the zero-balance account", got, ok)
	}
	// The park is still the truth about the wallet: the free request used the
	// account, it did not fix it.
	if st := statusOf(t, p, a.ID()); st.State != stateExhausted {
		t.Fatalf("state after serving a free model = %q, want the credit park to stand", st.State)
	}
}

func TestWorkbuddyFreeModelIsServedFromAHardCreditPark(t *testing.T) {
	a, b := pickAuths()
	p, _ := newPickPool([]*Auth{a, b})
	if _, d := p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"}); d <= 0 {
		t.Fatal("a hard-credit answer must park the account")
	}

	if !p.UsableForModelInRealmFor(a.ID(), "free-model", "", true) {
		t.Fatal("a hard-credit park must not exclude the account from a free model")
	}
	if p.UsableForModelInRealmFor(a.ID(), "free-model", "", false) {
		t.Fatal("a hard-credit park must still exclude the account from a paid model")
	}
	// With the healthy sibling skipped, the free request lands on the parked
	// one instead of reporting the platform exhausted.
	got, ok := p.PickForModelFor(map[string]bool{b.ID(): true}, "free-model", true)
	if !ok || got == nil || got.ID() != a.ID() {
		t.Fatalf("free pick = %v/%v, want the hard-credit account %s", got, ok, a.ID())
	}
}

func TestWorkbuddyFreeModelStillHonoursEveryOtherPark(t *testing.T) {
	// One pool per park, because each case parks the same credential.
	tests := []struct {
		name string
		park func(p *Pool, a *Auth)
	}{
		{"a rate limit", func(p *Pool, a *Auth) {
			p.MarkFailure(a, &Error{Kind: ErrSoftRate, Status: 429, RetryAfter: time.Hour, Msg: "slow down"})
		}},
		{"a WAF block", func(p *Pool, a *Auth) {
			p.MarkFailure(a, &Error{Kind: ErrWafBlock, Status: 403, Msg: "blocked"})
		}},
		{"a vendor risk flag", func(p *Pool, a *Auth) {
			p.MarkFailure(a, &Error{Kind: ErrAccountFault, Status: 403, Msg: "request illegal"})
		}},
		{"a model park", func(p *Pool, a *Auth) {
			p.MarkModelBlocked(a, "free-model", ModelBlockReason)
		}},
		{"a free-model refusal", func(p *Pool, a *Auth) {
			p.MarkFreeModelUnavailable(a, "free-model")
		}},
	}
	for _, tc := range tests {
		a, _ := pickAuths()
		p, _ := newPickPool([]*Auth{a})
		tc.park(p, a)
		if p.UsableForModelInRealmFor(a.ID(), "free-model", "", true) {
			t.Errorf("%s must exclude the account from a free model too", tc.name)
		}
	}
}

func TestWorkbuddyFreeModelRefusalParksOnlyThatPair(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-broke"}
	p, clk := reserveParkedPool(t, a)

	if d := p.MarkFreeModelUnavailable(a, "free-one"); d <= 0 {
		t.Fatal("a refused free model must be parked")
	}
	if p.UsableForModelInRealmFor(a.ID(), "free-one", "", true) {
		t.Error("the refused model must not be offered again")
	}
	if !p.UsableForModelInRealmFor(a.ID(), "free-two", "", true) {
		t.Error("another free model must stay reachable on the same account")
	}
	// The account's own state is untouched: the refusal said nothing new about
	// the credential, and the paid models were already parked by the wallet.
	if st := statusOf(t, p, a.ID()); st.State != stateExhausted || !strings.HasPrefix(st.Note, reserveCreditNote) {
		t.Fatalf("account state after a free refusal = %q/%q, want the low-balance park", st.State, st.Note)
	}
	// The park lapses with the quota day, so the pair is probed again once a
	// balance could plausibly have appeared.
	clk.advance(17 * time.Hour)
	if !p.UsableForModelInRealmFor(a.ID(), "free-one", "", true) {
		t.Error("the free-model park must lapse with the quota day")
	}
}

func TestWorkbuddyFreeSuccessKeepsTheCreditPark(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-broke"}
	p, _ := reserveParkedPool(t, a)

	p.MarkSuccessForModelFor(a, "free-model", true)
	st := statusOf(t, p, a.ID())
	if st.State != stateExhausted || !strings.HasPrefix(st.Note, reserveCreditNote) {
		t.Fatalf("a free success left the account %q/%q, want the credit park kept", st.State, st.Note)
	}
	if _, ok := p.PickForModelFor(nil, "paid-model", false); ok {
		t.Fatal("a free success must not re-open the account for paid models")
	}
	// A paid success does retire the park, which is the pre-existing rule: the
	// request itself is evidence the account can pay.
	p.MarkSuccessForModel(a, "paid-model")
	if st := statusOf(t, p, a.ID()); st.State != stateReady {
		t.Fatalf("state after a paid success = %q, want ready", st.State)
	}
}

func TestWorkbuddyFreeModelStaysStickyOnACreditParkedAccount(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-broke"}
	p, clk := reserveParkedPool(t, a)
	c := &Client{pool: p, affinity: core.NewAffinity(0)}
	key := "conv-free"

	if _, ok := c.pickAccount(map[string]bool{}, key, routeModel("free-model"), true); !ok {
		t.Fatal("a free request must bind to the credit-parked account")
	}
	// The binding is honoured for the next free turn and dropped for a paid one.
	if !p.UsableForModelInRealmFor(a.ID(), "free-model", "", true) {
		t.Fatal("the bound account must still be usable for the free model")
	}
	if p.UsableForModelInRealmFor(a.ID(), "free-model", "", false) {
		t.Fatal("the bound account must not be usable for a paid model")
	}
	clk.advance(time.Second)
}
