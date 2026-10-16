package codearts

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// freemodel_test.go pins the free-model rule: a model that bills against the
// vendor's free quota spends no credit, so a credential whose wallet is spent
// must still serve it.  Only a refusal of that exact (credential, model) pair
// takes the model away -- the credential keeps its other free models.

func TestBenefitModelsCarryTheFreeMarker(t *testing.T) {
	models := []core.Model{
		{ID: "glm-5.3-flash"},
		{ID: "GLM-5", Extra: map[string]any{"display_name": "GLM-5"}},
	}
	got := markBenefitModels(models, []string{" GLM-5.3-Flash ", ""})
	if got[0].Extra["free"] != true {
		t.Fatalf("benefit model Extra[free] = %v, want true", got[0].Extra["free"])
	}
	if _, ok := got[1].Extra["free"]; ok {
		t.Fatalf("a paid model must not carry the free marker: %v", got[1].Extra["free"])
	}
	if got[1].Extra["display_name"] != "GLM-5" {
		t.Error("marking wiped an unrelated extra field")
	}
}

func TestZeroWalletStillServesFreeModels(t *testing.T) {
	p := newPool()
	e := p.put(testAccount(), true)
	p.markFailure(e, kindQuota, "out of credit")

	if got := p.pick(nil); got != nil {
		t.Fatal("a spent wallet still served a paid request")
	}
	if got := p.pickFor(nil, true, "glm-5.3-flash"); got != e {
		t.Fatal("a spent wallet hid a free model")
	}
	if e.state != stateExhausted || e.until == 0 {
		t.Fatalf("the wallet park was retired by a free pick: state=%q until=%d", e.state, e.until)
	}
}

func TestOtherParksStillHoldFreeModels(t *testing.T) {
	p := newPool()
	e := p.put(testAccount(), true)
	p.markFailure(e, kindAuth, "expired token")
	if got := p.pickFor(nil, true, "glm-5.3-flash"); got != nil {
		t.Fatal("a dead session was stepped over for a free model")
	}
}

func TestFreePairParkOnlyHidesThatModel(t *testing.T) {
	p := newPool()
	e := p.put(testAccount(), true)
	p.ParkFreeModel(e, "glm-5.3-flash", time.Hour)

	if got := p.pickFor(nil, true, "glm-5.3-flash"); got != nil {
		t.Fatal("the refused free model was offered again")
	}
	if got := p.pickFor(nil, true, "deepseek-v4.1-flash"); got != e {
		t.Fatal("the pair park hid a different free model")
	}
	if got := p.pick(nil); got != e {
		t.Fatal("the pair park hid the paid models")
	}
}

func TestFreeSuccessKeepsTheCreditPark(t *testing.T) {
	p := newPool()
	e := p.put(testAccount(), true)
	p.markFailure(e, kindQuota, "out of credit")
	p.markUsedFor(e, true)

	if e.until == 0 || e.state != stateExhausted {
		t.Fatalf("a free success lifted the credit park: state=%q until=%d", e.state, e.until)
	}
	if got := p.pick(nil); got != nil {
		t.Fatal("the credit park no longer held after a free success")
	}
}

func TestChatReachesABenefitModelOnASpentCredential(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture has no credential")
	}
	c.pool.markFailure(e, kindQuota, "out of credit")

	st, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.3-flash", FreeModel: true})
	if err != nil {
		t.Fatalf("Chat for a free model = %v, want a stream", err)
	}
	defer st.Close()

	if got := len(f.chatRequests()); got != 1 {
		t.Fatalf("the fixture saw %d chat calls, want 1", got)
	}
	// The wallet park survives the free success: it is still true for the
	// models that would charge the wallet.
	if e.until == 0 {
		t.Error("the free success retired the wallet park")
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "deepseek-v4-flash"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat for a paid model = %v, want core.ErrNotConfigured", err)
	}
}

func TestAFreeCreditRefusalParksThePairNotTheAccount(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, r *http.Request, req recordedRequest, n int) {
		writeJSONBody(w, http.StatusPaymentRequired, map[string]any{"error": "insufficient credit"})
	}
	c := f.client(t, "")
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture has no credential")
	}

	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.3-flash", FreeModel: true}); err == nil {
		t.Fatal("a 402 was reported as success")
	}
	if e.until != 0 {
		t.Fatalf("a free-model refusal parked the whole credential until %d", e.until)
	}
	if _, ok := e.freeCool["glm-5.3-flash"]; !ok {
		t.Fatal("the refused (credential, model) pair was not remembered")
	}

	// A different free model is still routed to the same credential.
	f.onCha = nil
	st, err := c.Chat(context.Background(), &core.ChatRequest{Model: "deepseek-v4.1-flash", FreeModel: true})
	if err != nil {
		t.Fatalf("Chat for another free model = %v, want a stream", err)
	}
	defer st.Close()
}
