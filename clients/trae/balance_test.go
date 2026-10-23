package trae

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

// balance_test.go drives the Trae entitlement read entirely offline.

func TestTraeAccountBalanceUsesVendorUsageSummary(t *testing.T) {
	var rec rtRecorder
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec.record(req)
		if req.URL.String() != "https://api.trae.cn"+ugEntUsagePath {
			return jsonResponse(http.StatusNotFound, `{"code":404,"msg":"wrong path"}`), nil
		}
		return jsonResponse(http.StatusOK, `{
			"is_credits_billing": true,
			"usage_summary": {"consumed_amount": 1859.74, "total_amount": 4500},
			"user_entitlement_pack_list": [
				{"entitlement_base_info":{"quota":{"credits_limit":4000}},"usage":{"credits_amount":1359.7396}},
				{"entitlement_base_info":{"quota":{"enable_solo_lite":true}},"usage":{}},
				{"entitlement_base_info":{"quota":{"credits_limit":500,"enable_solo_lite":true}},"usage":{"credits_amount":500}}
			]
		}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	bal, err := c.AccountBalance(context.Background(), "u1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 2640 || bal.Total != 4500 || bal.Used != 1859.74 {
		t.Fatalf("balance = %+v, want 2640/4500 used 1859.74", bal)
	}
	if bal.Unit != traeBalanceUnit {
		t.Fatalf("unit = %q, want %q", bal.Unit, traeBalanceUnit)
	}

	urls, methods, bodies, headers := rec.snapshot()
	if len(urls) != 1 || methods[0] != http.MethodPost || bodies[0] != "{}" {
		t.Fatalf("request = %v %v %q", urls, methods, bodies)
	}
	h := headers[0]
	if got := h.Get("Authorization"); got != "Cloud-IDE-JWT TOK_1" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("X-User-Region"); got != ugRegion {
		t.Errorf("X-User-Region = %q, want %q", got, ugRegion)
	}
	if got := h.Get("X-Device-Id"); got != "2235771921399404" {
		t.Errorf("X-Device-Id = %q", got)
	}
	for _, k := range []string{"X-App-Id", "X-Ide-Version", "X-Cloudide-Token", "X-Uid"} {
		if h.Get(k) != "" {
			t.Errorf("balance request leaked SOLO header %s = %q", k, h.Get(k))
		}
	}
}

func TestTraeAccountBalanceReportsUpstreamFailure(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusUnauthorized, `{"code":1001,"msg":"invalid token"}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	if _, err := c.AccountBalance(context.Background(), "u1", 0); err == nil {
		t.Fatal("expected an upstream error")
	}
}

func TestTraeAccountBalanceRejectsUnknownAccount(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	if _, err := c.AccountBalance(context.Background(), "nobody", 0); err == nil {
		t.Fatal("expected an unknown-account error")
	}
}

func TestTraeAccountBalanceFallsBackToPackTotals(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{
			"user_entitlement_pack_list": [
				{"entitlement_base_info":{"quota":{"credits_limit":2000}},"usage":{"credits_amount":125.5}},
				{"entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":0}}
			]
		}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	bal, err := c.AccountBalance(context.Background(), "u1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 2374 || bal.Total != 2500 || bal.Used != 125.5 {
		t.Fatalf("balance = %+v, want 2374/2500 used 125.5", bal)
	}
}

func TestTraeAccountBalanceReportsUnlimited(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{
			"usage_summary": {"consumed_amount": 123.25, "total_amount": -1},
			"user_entitlement_pack_list": [
				{"entitlement_base_info":{"quota":{"credits_limit":-1}},"usage":{"credits_amount":123.25}}
			]
		}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	bal, err := c.AccountBalance(context.Background(), "u1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if !bal.Unlimited || bal.Credits != 0 || bal.Total != 0 || bal.Used != 123.25 {
		t.Fatalf("balance = %+v, want unlimited with used 123.25", bal)
	}
}

func TestTraeAccountBalanceRejectsBlankID(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	if _, err := c.AccountBalance(context.Background(), "   ", 0); err == nil {
		t.Fatal("expected a blank-id error")
	}
}

func TestTraeAccountBalanceSkipsForeignRegion(t *testing.T) {
	a := testAuth("u1", "TOK_1")
	a.Region = "US"
	c := testClient(t, nil, []*Auth{a}, nil)

	_, err := c.AccountBalance(context.Background(), "u1", 0)
	if err == nil || !strings.Contains(err.Error(), "CN") {
		t.Fatalf("foreign-region error = %v, want a CN-only refusal", err)
	}
}

func TestTraeAccountBalanceClampsNegativeRemaining(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{
			"user_entitlement_pack_list": [
				{"entitlement_base_info":{"quota":{"credits_limit":10,"enable_solo_lite":true}},"usage":{"credits_amount":25}}
			]
		}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	bal, err := c.AccountBalance(context.Background(), "u1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 0 || bal.Total != 10 || bal.Used != 25 {
		t.Fatalf("balance = %+v, want remaining clamped to 0", bal)
	}
}

func TestTraeCapabilitiesAdvertiseBalance(t *testing.T) {
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Balance {
		t.Fatal("caps.Balance = false for the trae client")
	}
}

func TestTraeAccountBalanceHonoursContextCancellation(t *testing.T) {
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("transport should not be reached")
	})
	c := testClient(t, nil, []*Auth{testAuth("u1", "TOK_1")}, rt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.AccountBalance(ctx, "u1", 0); err == nil {
		t.Fatal("expected a cancellation error")
	}
}

// A token that crossed the refresh skew used to make the balance read fail with
// the vendor's "user not signed in" reply until some unrelated test or chat
// renewed it as a side effect.  With self_renew on the read must renew it
// itself, before the entitlement call, and then succeed on the fresh token.
func TestTraeAccountBalanceRefreshesExpiredToken(t *testing.T) {
	var rec rtRecorder
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec.record(req)
		switch req.URL.Path {
		case epExchange:
			return jsonResponse(http.StatusOK, `{
				"Token": "NEW_TOK",
				"RefreshToken": "RT2",
				"TokenExpireDuration": 3600
			}`), nil
		case ugEntUsagePath:
			if got := req.Header.Get("Authorization"); got != "Cloud-IDE-JWT NEW_TOK" {
				return jsonResponse(http.StatusUnauthorized, `{"code":1001,"msg":"user not signed in"}`), nil
			}
			return jsonResponse(http.StatusOK, `{
				"usage_summary": {"consumed_amount": 1, "total_amount": 10}
			}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{"code":404,"msg":"wrong path"}`), nil
	})

	cfg := loadConfig(json.RawMessage(`{"self_renew":true}`), nil)
	a := testAuth("u1", "OLD_TOK")
	a.ExpiresAt = time.Now().Add(-time.Minute)
	a.RefreshToken = "RT1"
	c := testClient(t, cfg, []*Auth{a}, rt)

	bal, err := c.AccountBalance(context.Background(), "u1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 9 || bal.Total != 10 {
		t.Fatalf("balance = %+v, want 9/10", bal)
	}

	urls, _, bodies, headers := rec.snapshot()
	if len(urls) != 2 {
		t.Fatalf("upstream calls = %v, want the exchange then the entitlement read", urls)
	}
	if urls[0] != "https://api.trae.cn"+epExchange || urls[1] != "https://api.trae.cn"+ugEntUsagePath {
		t.Fatalf("upstream calls = %v, want [%s %s]", urls, epExchange, ugEntUsagePath)
	}
	if !strings.Contains(bodies[0], `"RefreshToken":"RT1"`) {
		t.Errorf("exchange body = %q, want the stored refresh token", bodies[0])
	}
	if got := headers[0].Get("Authorization"); got != "" {
		t.Errorf("exchange Authorization = %q, want none", got)
	}
	if got := headers[1].Get("Authorization"); got != "Cloud-IDE-JWT NEW_TOK" {
		t.Errorf("entitlement Authorization = %q, want the refreshed token", got)
	}
	if a.Token() != "NEW_TOK" {
		t.Errorf("stored token = %q, want the refreshed one", a.Token())
	}
}

// With self_renew off the module must not reach for the network to mint a
// token.  An already expired credential has to fail fast with an auth error and
// leave the entitlement endpoint untouched.
func TestTraeAccountBalanceRefusesExpiredTokenWhenSelfRenewOff(t *testing.T) {
	var calls int
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(http.StatusOK, `{}`), nil
	})

	a := testAuth("u1", "OLD_TOK")
	a.ExpiresAt = time.Now().Add(-time.Minute)
	a.RefreshToken = "RT1"
	c := testClient(t, nil, []*Auth{a}, rt)

	_, err := c.AccountBalance(context.Background(), "u1", 0)
	if err == nil {
		t.Fatal("expected an auth error for the expired token")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != ErrAuth {
		t.Fatalf("error = %v, want a trae auth error", err)
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want none when self_renew is off", calls)
	}
}
