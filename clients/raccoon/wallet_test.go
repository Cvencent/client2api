package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// wallet_test.go pins issue #51: the vendor bills an empty wallet as HTTP 400
// ("积分余额不足，请充值后重试"), and that refusal used to land in kindClient.
// kindClient does not rotate and does not park, so the walk stopped at the
// first (broke) account, the funded siblings were never asked, and the empty
// wallet was picked first again on the next request. A wallet verdict must
// rotate to a funded sibling and park the broke one, exactly like the 402
// path next to which it lives.

// walletTestClient builds a two-account client pointed at ts, in a known LRU
// order (a2 is picked first), mirroring affinityTestClient.
func walletTestClient(t *testing.T, ts *httptest.Server) *Client {
	t.Helper()
	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), ts.Client())
	for _, a := range []account{
		{ID: "a1", Label: "a1", AccessToken: testToken(t), UserID: "u1", Origin: originStored},
		{ID: "a2", Label: "a2", AccessToken: testToken(t), UserID: "u2", Origin: originStored},
	} {
		c.pool.put(a)
	}
	now := time.Now().UTC()
	c.pool.markUsed(c.pool.find("a1"))
	c.pool.find("a1").acct.LastUsed = now.UnixMilli()
	c.pool.find("a2").acct.LastUsed = now.Add(-time.Hour).UnixMilli()
	c.affinity = core.NewAffinity(0)
	return c
}

func TestWalletRefusalOn400IsQuota(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		want    errKind
	}{
		{"the vendor's own wording", 400, "积分余额不足，请充值后重试", kindQuota},
		{"the other spelling", 400, "请充值后重试", kindQuota},
		{"the english wording", 400, "insufficient credit balance", kindQuota},
		{"a real request-shape refusal stays client", 400, "invalid parameter: tools", kindClient},
		{"a bare bad request stays client", 400, "Bad Request", kindClient},
		{"the same wording on 402 was already quota", 402, "积分余额不足", kindQuota},
	}
	for _, tc := range cases {
		err := &apiError{Status: tc.status, Message: tc.message}
		if got := classifyErr(err); got != tc.want {
			t.Errorf("%s: classifyErr = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The walk-level contract: the broke account parks as exhausted (so it stops
// being picked first), the walk moves on to the funded sibling, and the
// request is served.
func TestChatRotatesPastAnEmptyWalletToAFundedAccount(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// The least-recently-used account (the one picked first) has no
		// credit; the second account in the walk is funded and answers.
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":1000,"message":"积分余额不足，请充值后重试"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer ts.Close()

	c := walletTestClient(t, ts)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-glm-5-3-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat = %v, want the funded sibling to serve the request", err)
	}
	_ = stream.Close()

	// The broke account must be parked as exhausted, not left "ready".
	broke := c.pool.find("a2")
	if broke.state != stateExhausted || broke.until.IsZero() {
		t.Fatalf("the empty wallet was not parked: state=%q until=%v", broke.state, broke.until)
	}
	if !strings.Contains(broke.note, "余额不足") {
		t.Fatalf("park note = %q, want the vendor's own refusal recorded", broke.note)
	}
	if calls < 2 {
		t.Fatalf("upstream got %d calls, want at least 2 (broke account + funded sibling)", calls)
	}
}

// The terminal error of a fully-broke pool must classify as quota so the
// gateway moves to the next platform, and must NOT carry the "not
// account-specific" sentence: a wallet verdict is exactly account-specific.
func TestChatOnAllBrokeReportsQuotaWithoutTheSiblingFutileSentence(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":1000,"message":"积分余额不足，请充值后重试"}`))
	}))
	defer ts.Close()

	c := walletTestClient(t, ts)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-glm-5-3-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success against an all-broke pool")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("err = %v, want ErrPlatformExhausted so the gateway moves to the next platform", err)
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureQuota {
		t.Fatalf("kind = %q, want %q for an empty wallet", f.Kind, core.FailureQuota)
	}
	if !core.Retryable(f.Kind) {
		t.Fatal("a quota verdict must let the gateway rotate")
	}
	if strings.Contains(err.Error(), "not account-specific") {
		t.Fatalf("err = %v; a wallet verdict IS account-specific, the futile-sibling sentence must not appear", err)
	}
	// Both accounts should now be parked, so the next request does not knock
	// on them again immediately.
	for _, id := range []string{"a1", "a2"} {
		e := c.pool.find(id)
		if e.state != stateExhausted || e.until.IsZero() {
			t.Fatalf("account %s was not parked after the wallet refusal: state=%q until=%v", id, e.state, e.until)
		}
	}
}

// A genuine request-shape 400 keeps its old contract: no rotation, no park,
// the futile-sibling sentence, FailureOther for the caller.
func TestChatKeepsARealRequestRefusalOutOfTheWalletPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":0,"message":"invalid parameter: tools"}`))
	}))
	defer ts.Close()

	c := walletTestClient(t, ts)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "sn-glm-5-3-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success on a 400")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a classified *core.Failure", err)
	}
	if f.Kind != core.FailureOther {
		t.Fatalf("kind = %q, want %q for a request-shape refusal", f.Kind, core.FailureOther)
	}
	if core.Retryable(f.Kind) {
		t.Fatal("a request-shape refusal must not rotate")
	}
	if !strings.Contains(err.Error(), "not account-specific") {
		t.Fatalf("err = %v, want the futile-sibling sentence for a real request refusal", err)
	}
	e := c.pool.find("a2")
	if e.state == stateExhausted {
		t.Fatal("a request-shape refusal parked the account; the credential is healthy")
	}
}

// The free-model branch keeps its own semantics: a free-model quota refusal
// parks the (credential, model) pair, never the wallet -- a paid model must
// still reach the account.
func TestFreeModelWalletRefusalStillParksOnlyThePair(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":1000,"message":"积分余额不足，请充值后重试"}`))
	}))
	defer ts.Close()

	c := walletTestClient(t, ts)
	model := "sn-sensenova-6-8-flash"

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:     model,
		FreeModel: true,
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success on a refused free model")
	}
	e := c.pool.find("a2")
	if e.state == stateExhausted && !e.until.IsZero() {
		t.Fatalf("the free-model refusal parked the wallet: state=%q until=%v -- the pair park must not hide paid models", e.state, e.until)
	}
	if got := c.pool.pickFor(nil, false, ""); got == nil {
		t.Fatal("the pair park hid the account from PAID requests; the wallet verdict leaked into the free-model path")
	}
}
