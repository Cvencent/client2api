package qwenwork

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// freemodel_test.go pins the free-model rule: a model whose credits multiplier
// is zero spends no credit, so a credential whose wallet is spent must still
// serve it.  Only a refusal of that exact (credential, model) pair takes it
// away.

func TestCreditsAreFree(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"0", true},
		{"0.0", true},
		{"x0", true},
		{"0x", true},
		{"免费", true},
		{"free", true},
		{"free_unlimited", true},
		{"1", false},
		{"x0.15", false},
		{"-1", false},
		{"", false},
		{"high", false},
	}
	for _, tc := range cases {
		if got := creditsAreFree(tc.raw); got != tc.want {
			t.Errorf("creditsAreFree(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestZeroWalletStillServesFreeModels(t *testing.T) {
	p := newTestPool(t, account{UID: "1", AccessToken: "tok-1"})
	e := p.pick(nil)
	if e == nil {
		t.Fatal("the pool has no usable account")
	}
	p.markFailure(e, kindQuota, "out of credit")

	if got := p.pick(nil); got != nil {
		t.Fatal("a spent wallet still served a paid request")
	}
	if got := p.pickFor(nil, true, "pro"); got != e {
		t.Fatal("a spent wallet hid a free model")
	}
	if e.state != stateExhausted || e.until.IsZero() {
		t.Fatalf("the wallet park was retired by a free pick: state=%q until=%v", e.state, e.until)
	}
}

func TestOtherParksStillHoldFreeModels(t *testing.T) {
	p := newTestPool(t, account{UID: "1", AccessToken: "tok-1"})
	e := p.pick(nil)
	p.markFailure(e, kindAuth, "expired token")
	if got := p.pickFor(nil, true, "pro"); got != nil {
		t.Fatal("a dead session was stepped over for a free model")
	}
}

func TestFreePairParkOnlyHidesThatModel(t *testing.T) {
	p := newTestPool(t, account{UID: "1", AccessToken: "tok-1"})
	e := p.pick(nil)
	p.ParkFreeModel(e, "pro", time.Hour)

	if got := p.pickFor(nil, true, "pro"); got != nil {
		t.Fatal("the refused free model was offered again")
	}
	if got := p.pickFor(nil, true, "flash"); got != e {
		t.Fatal("the pair park hid a different free model")
	}
	if got := p.pick(nil); got != e {
		t.Fatal("the pair park hid the paid models")
	}
}

func TestFreeSuccessKeepsTheCreditPark(t *testing.T) {
	p := newTestPool(t, account{UID: "1", AccessToken: "tok-1"})
	e := p.pick(nil)
	p.markFailure(e, kindQuota, "out of credit")
	p.markUsedFor(e, true)

	if e.until.IsZero() || e.state != stateExhausted {
		t.Fatalf("a free success lifted the credit park: state=%q until=%v", e.state, e.until)
	}
	if got := p.pick(nil); got != nil {
		t.Fatal("the credit park no longer held after a free success")
	}
}

func TestChatReachesAFreeModelOnASpentCredential(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, chatSSEFrames), nil
	}
	c := panelClient(t, twoAccountConfig, rt)
	for _, e := range c.pool.entries {
		c.pool.markFailure(e, kindQuota, "out of credit")
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "pro", FreeModel: true})
	if err != nil {
		t.Fatalf("Chat for a free model = %v, want a stream", err)
	}
	stream.Close()

	for _, e := range c.pool.entries {
		if e.until.IsZero() {
			t.Errorf("account %s: the free success retired the wallet park", e.acct.id())
		}
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "pro"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat for a paid model = %v, want core.ErrNotConfigured", err)
	}
}
