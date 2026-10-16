package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
)

// freemodel_test.go pins the free-model rule: a model whose multiplier is zero
// spends no credit, so a credential whose wallet is spent must still serve it.
// Only a refusal of that exact (credential, model) pair takes it away.

func TestZeroMultiplierModelsCarryTheFreeMarker(t *testing.T) {
	byID := map[string]core.Model{}
	for _, m := range fallbackModels() {
		byID[m.ID] = m
	}
	if got := byID["sn-sensenova-6-8-flash"].Extra["free"]; got != true {
		t.Fatalf("sn-sensenova-6-8-flash Extra[free] = %v, want true", got)
	}
	if got := byID["sn-glm-5-3"].Extra["free"]; got == true {
		t.Fatalf("a paid model carries the free marker: %v", got)
	}
}

func TestZeroWalletStillServesFreeModels(t *testing.T) {
	c := affinityTestClient(t)
	e := c.pool.find("a2")
	for _, other := range c.pool.entries {
		c.pool.markFailure(other, kindQuota, "out of credit")
	}

	if got := c.pool.pick(nil); got != nil {
		t.Fatal("a spent wallet still served a paid request")
	}
	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash"); got != e {
		t.Fatal("a spent wallet hid a free model")
	}
	if e.state != stateExhausted || e.until.IsZero() {
		t.Fatalf("the wallet park was retired by a free pick: state=%q until=%v", e.state, e.until)
	}
}

func TestOtherParksStillHoldFreeModels(t *testing.T) {
	c := affinityTestClient(t)
	for _, e := range c.pool.entries {
		c.pool.markFailure(e, kindAuth, "expired token")
	}
	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash"); got != nil {
		t.Fatal("a dead session was stepped over for a free model")
	}
}

func TestFreePairParkOnlyHidesThatModel(t *testing.T) {
	c := affinityTestClient(t)
	if err := c.pool.setEnabled("a1", false); err != nil {
		t.Fatalf("disabling a1: %v", err)
	}
	e := c.pool.find("a2")
	c.pool.ParkFreeModel(e, "sn-sensenova-6-8-flash", time.Hour)

	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash"); got != nil {
		t.Fatal("the refused free model was offered again")
	}
	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash-lite"); got != e {
		t.Fatal("the pair park hid a different free model")
	}
	if got := c.pool.pick(nil); got != e {
		t.Fatal("the pair park hid the paid models")
	}
}

func TestFreeSuccessKeepsTheCreditPark(t *testing.T) {
	c := affinityTestClient(t)
	if err := c.pool.setEnabled("a1", false); err != nil {
		t.Fatalf("disabling a1: %v", err)
	}
	e := c.pool.find("a2")
	c.pool.markFailure(e, kindQuota, "out of credit")
	c.pool.markUsedFor(e, true)

	if e.until.IsZero() || e.state != stateExhausted {
		t.Fatalf("a free success lifted the credit park: state=%q until=%v", e.state, e.until)
	}
	if got := c.pool.pick(nil); got != nil {
		t.Fatal("the credit park no longer held after a free success")
	}
}

func TestChatReachesAFreeModelOnASpentCredential(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			`{"choices":[{"delta":{"content":"hi"}}]}`,
			`{"choices":[{"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture has no credential")
	}
	c.pool.markFailure(e, kindQuota, "out of credit")

	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-sensenova-6-8-flash", FreeModel: true})
	if err != nil {
		t.Fatalf("Chat for a free model = %v, want a stream", err)
	}
	defer s.Close()

	if e.until.IsZero() {
		t.Error("the free success retired the wallet park")
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-glm-5-3"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat for a paid model = %v, want core.ErrNotConfigured", err)
	}
}

func TestAFreeCreditRefusalParksThePairNotTheAccount(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient credit"}}`))
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q,"access_token":"tok"}`, ts.URL), ts.Client())
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture has no credential")
	}

	// The vendor refuses the free model on credit grounds.  The account must
	// keep serving its other free models.
	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "sn-sensenova-6-8-flash", FreeModel: true})
	if err == nil {
		t.Fatal("a 402 was reported as success")
	}
	if !e.until.IsZero() {
		t.Fatalf("a free-model refusal parked the whole credential until %v", e.until)
	}
	if _, ok := e.freeCool["sn-sensenova-6-8-flash"]; !ok {
		t.Fatal("the refused (credential, model) pair was not remembered")
	}
	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash"); got != nil {
		t.Fatal("the refused pair was offered again")
	}
	if got := c.pool.pickFor(nil, true, "sn-sensenova-6-8-flash-lite"); got != e {
		t.Fatal("the pair park hid a different free model")
	}
}
