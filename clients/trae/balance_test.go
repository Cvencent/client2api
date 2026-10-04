package trae

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

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
