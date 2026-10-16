package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// freemodel_test.go pins the OpenRouter free-model rule: a zero-cost id spends
// no credit, so a key whose wallet is empty has to keep serving it.  Only a
// refusal of that exact (key, model) pair, or of the vendor's free tier as a
// whole, may take it away -- and even then the paid ids are untouched.

func TestModelFreeForUsesTheCataloguePricing(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	c.models.store([]core.Model{
		{ID: "vendor/paid", Extra: map[string]any{"vendor": clientName}},
		{ID: "vendor/zero", Extra: map[string]any{"vendor": clientName, "free": true}},
	}, c.now())

	if !c.modelFreeFor("vendor/zero") {
		t.Fatal("a catalogue entry priced at zero must route as a free model")
	}
	if c.modelFreeFor("vendor/paid") {
		t.Fatal("a priced catalogue entry must not route as a free model")
	}
	// A cold catalogue still has the vendor's own naming rule to fall back on.
	if !c.modelFreeFor("cohere/north-mini-code:free") || !c.modelFreeFor("openrouter/free") {
		t.Fatal("the vendor's :free ids must be recognised without a live catalogue")
	}
	if c.modelFreeFor("vendor/freeform-8b") {
		t.Fatal("a paid id that merely contains the letters free must not be swept in")
	}
}

func TestEmptyWalletStillServesFreeModels(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	paid := admission{model: "anthropic/claude-sonnet-4.5"}
	free := admission{model: "cohere/north-mini-code:free", free: true}

	c.classifyHTTP("chat", id, http.StatusPaymentRequired,
		[]byte(`{"error":{"message":"Insufficient credits","code":402}}`), paid)

	if _, err := c.pool.acquire(c.now(), c.limit(), paid); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("paid acquire = %v, want ErrBusy after a wallet verdict", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), free); err != nil {
		t.Fatalf("free acquire = %v, want the key to keep serving zero-cost ids", err)
	}
	// A free success must NOT lift the wallet park: nothing about a free call
	// proves the paid balance was topped up.
	c.pool.chatSuccess(id, free.model, free.free)
	if rec := c.mustByID(t, id); rec.CreditUntil == "" {
		t.Fatal("a free success lifted the wallet park")
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), paid); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("paid acquire after a free success = %v, want ErrBusy", err)
	}
}

func TestFreeCreditRefusalParksOnlyThatPair(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	adm := admission{model: "cohere/north-mini-code:free", free: true}

	c.classifyHTTP("chat", id, http.StatusPaymentRequired,
		[]byte(`{"error":{"message":"No credit for this endpoint","code":402}}`), adm)

	rec := c.mustByID(t, id)
	if rec.CooldownUntil != "" || rec.CreditUntil != "" {
		t.Fatalf("a free-model refusal benched the account: %+v", rec)
	}
	if _, ok := rec.ModelParks[adm.model]; !ok {
		t.Fatalf("the refused pair was not remembered: %+v", rec.ModelParks)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), adm); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("the refused pair acquire = %v, want ErrBusy", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), admission{model: "vendor/other:free", free: true}); err != nil {
		t.Fatalf("another free id acquire = %v, want the key to keep working", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), admission{model: "anthropic/claude-sonnet-4.5"}); err != nil {
		t.Fatalf("paid acquire = %v, want the pair park to leave paid ids alone", err)
	}
}

func TestFreeTierDailyCapParksTheFreeClassOnly(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	adm := admission{model: "cohere/north-mini-code:free", free: true}

	c.classifyHTTP("chat", id, http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Rate limit exceeded: free-models-per-day","code":429}}`), adm)

	rec := c.mustByID(t, id)
	if rec.FreeUntil == "" {
		t.Fatalf("the daily cap did not park the free class: %+v", rec)
	}
	if rec.CooldownUntil != "" || rec.CreditUntil != "" {
		t.Fatalf("the daily cap parked more than the free class: %+v", rec)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), adm); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("free acquire = %v, want ErrBusy while the cap holds", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), admission{model: "anthropic/claude-sonnet-4.5"}); err != nil {
		t.Fatalf("paid acquire = %v, want the paid ids to keep working", err)
	}
}

func TestThrottlingStillBenchesTheWholeKey(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	paid := admission{model: "anthropic/claude-sonnet-4.5"}

	// A 429 with no free-tier marker is a verdict on the credential, not on one
	// class of model, so it must hold the free ids too.
	c.classifyHTTP("chat", id, http.StatusTooManyRequests,
		[]byte(`{"error":{"message":"Too many requests","code":429}}`), paid)

	if _, err := c.pool.acquire(c.now(), c.limit(), paid); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("paid acquire = %v, want ErrBusy", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit(), admission{model: "cohere/north-mini-code:free", free: true}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("free acquire = %v, want the throttled key to stay out of rotation", err)
	}
}

func TestManualRefreshLiftsEveryScopedPark(t *testing.T) {
	dir := t.TempDir()
	up := &fakeUpstream{handle: func(r *http.Request, _ string) reply {
		if strings.HasSuffix(r.URL.Path, "/key") {
			return reply{body: `{"data":{"label":"one","limit":10,"limit_remaining":9,"usage":1}}`}
		}
		return reply{status: http.StatusForbidden, body: `{"error":{"message":"management key required"}}`}
	}}
	t.Setenv(envAPIKey, "")
	cl, err := New(core.Deps{DataDir: dir, Config: json.RawMessage(cfgWithKey(testKey)), HTTPClient: &http.Client{Transport: up.transport()}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := cl.(*Client)
	c.now = func() time.Time { return testBase }
	id := accountIDFor(testKey)

	c.pool.setCreditPark(id, testBase.Add(time.Hour), "out of credit")
	c.pool.setFreePark(id, testBase.Add(time.Hour), "daily cap")
	c.pool.parkModel(id, "vendor/refused:free", testBase.Add(time.Hour), "no endpoint")
	if _, err := c.RefreshAccount(context.Background(), id); err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	rec := c.mustByID(t, id)
	if rec.CreditUntil != "" || rec.FreeUntil != "" || len(rec.ModelParks) != 0 {
		t.Fatalf("a manual refresh left scoped parks behind: %+v", rec)
	}

	// The verdicts are on disk too: a restart must not resurrect them.
	reopened, err := New(core.Deps{DataDir: dir, Config: json.RawMessage(cfgWithKey(testKey)), HTTPClient: &http.Client{Transport: up.transport()}})
	if err != nil {
		t.Fatalf("New after a restart: %v", err)
	}
	next := reopened.(*Client)
	rec = next.mustByID(t, id)
	if rec.CreditUntil != "" || rec.FreeUntil != "" || len(rec.ModelParks) != 0 {
		t.Fatalf("the refresh was not persisted: %+v", rec)
	}
}

func TestScopedParksSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envAPIKey, "")
	cl, err := New(core.Deps{DataDir: dir, Config: json.RawMessage(cfgWithKey(testKey))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := cl.(*Client)
	c.now = func() time.Time { return testBase }
	id := accountIDFor(testKey)
	c.pool.setCreditPark(id, testBase.Add(time.Hour), "out of credit")
	c.pool.setFreePark(id, testBase.Add(2*time.Hour), "daily cap")
	c.pool.parkModel(id, "vendor/refused:free", testBase.Add(3*time.Hour), "no endpoint")
	c.persistState()

	reopened, err := New(core.Deps{DataDir: dir, Config: json.RawMessage(cfgWithKey(testKey))})
	if err != nil {
		t.Fatalf("New after a restart: %v", err)
	}
	next := reopened.(*Client)
	next.now = func() time.Time { return testBase }
	rec := next.mustByID(t, id)
	if rec.CreditUntil == "" || rec.FreeUntil == "" || rec.ModelParks["vendor/refused:free"] == "" {
		t.Fatalf("the scoped parks did not survive a restart: %+v", rec)
	}
	if got := next.pool.stateOf(&rec, testBase); got != "low_credit" {
		t.Fatalf("state = %q, want low_credit", got)
	}
}

func TestStateOfDistinguishesScopedParks(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)

	c.pool.setFreePark(id, testBase.Add(time.Hour), "daily cap")
	rec := c.mustByID(t, id)
	if got := c.pool.stateOf(&rec, testBase); got != "quota_exceeded" {
		t.Fatalf("free-tier cap state = %q, want quota_exceeded", got)
	}
	c.pool.clearParks(id)

	c.pool.parkModel(id, "vendor/refused:free", testBase.Add(time.Hour), "no endpoint")
	rec = c.mustByID(t, id)
	if got := c.pool.stateOf(&rec, testBase); got != "quota_exceeded" {
		t.Fatalf("pair park state = %q, want quota_exceeded", got)
	}
	c.pool.clearParks(id)

	c.pool.setCreditPark(id, testBase.Add(time.Hour), "out of credit")
	rec = c.mustByID(t, id)
	if got := c.pool.stateOf(&rec, testBase); got != "low_credit" {
		t.Fatalf("empty-wallet state = %q, want low_credit", got)
	}
}

func TestChatReachesAFreeModelOnASpentKey(t *testing.T) {
	var calls int
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		calls++
		if calls == 1 {
			return reply{status: http.StatusPaymentRequired, body: `{"error":{"message":"insufficient credits","code":402}}`}
		}
		return reply{body: sseBasic}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)
	id := accountIDFor(testKey)
	paid := "anthropic/claude-sonnet-4.5"
	free := "cohere/north-mini-code:free"

	// The wallet dies on the paid call...
	if _, err := c.Chat(context.Background(), simpleReq(paid)); err == nil {
		t.Fatal("the 402 paid call was reported as success")
	}
	// ...and the next free call still goes out, with no operator override.
	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model: free, Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat for a free model on a spent key = %v, want a stream", err)
	}
	text, _, _, _, _, derr := drainStream(st)
	if derr != nil || text != "Hello world" {
		t.Fatalf("drain = %q, %v", text, derr)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want the free model to actually reach the vendor", calls)
	}
	r := up.requestAt(1)
	if r == nil {
		t.Fatal("the free call never reached the vendor")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(up.bodyAt(1)), &body); err != nil {
		t.Fatalf("free call body: %v", err)
	}
	if body["model"] != free {
		t.Fatalf("free call model = %v, want %q", body["model"], free)
	}
	if rec := c.mustByID(t, id); rec.CreditUntil == "" {
		t.Fatal("the free call retired the wallet park")
	}
}

func TestChatRotatesPastAPairParkedFreeModel(t *testing.T) {
	var calls int
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		calls++
		if calls == 1 {
			return reply{status: http.StatusPaymentRequired, body: `{"error":{"message":"no credit for this endpoint","code":402}}`}
		}
		return reply{body: sseBasic}
	}}
	c := newTestClient(t, twoKeysCfg(), up)
	req := func() *core.ChatRequest {
		return &core.ChatRequest{Model: "cohere/north-mini-code:free", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	}
	// The first key refuses this pair; the request must rotate to the other key
	// instead of benching either account for every model.
	first, err := c.Chat(context.Background(), req())
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, _, _, _, _, err := drainStream(first); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want a refusal then a success", calls)
	}
	refused := accountIDFor(strings.TrimPrefix(up.requestAt(0).Header.Get("Authorization"), "Bearer "))
	rec := c.mustByID(t, refused)
	if rec.CooldownUntil != "" || rec.CreditUntil != "" {
		t.Fatalf("the refusing key was benched beyond the pair: %+v", rec)
	}
	if _, ok := rec.ModelParks[req().Model]; !ok {
		t.Fatalf("the refusal was not scoped to the (key, model) pair: %+v", rec.ModelParks)
	}
	// A second request for the same pair skips the parked key and lands on the
	// healthy one without any operator action.
	second, err := c.Chat(context.Background(), req())
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if _, _, _, _, _, err := drainStream(second); err != nil {
		t.Fatalf("second drain: %v", err)
	}
	used := accountIDFor(strings.TrimPrefix(up.requestAt(2).Header.Get("Authorization"), "Bearer "))
	if used == refused {
		t.Fatalf("the pair-parked key %q was offered again", refused)
	}
}
