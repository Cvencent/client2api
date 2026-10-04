package zcode

// Tests for the shared core.Failure contract.  Every assertion goes through
// core.AsFailure / core.FailureKindOf / core.Retryable rather than the error
// text, because those three functions are exactly what the gateway reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// accountsConfigJSON builds a config with n distinct API-key credentials
// (k1..kn) so a test can watch the pool rotate through them.
func accountsConfigJSON(n int) string {
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf(
			`{"id":"k%d","provider":"bigmodel","mode":"api_key","api_key":"%032d"}`, i, i))
	}
	return `{"auto_discover":false,"accounts":[` + strings.Join(parts, ",") + `]}`
}

// newFailureClient builds a client whose pool holds n accounts and whose Deps
// carries guard.  A nil guard is the path every ordinary unit test runs: the
// process simply never built one.
func newFailureClient(t *testing.T, guard *core.Guard, n int, transport *fakeTransport) *Client {
	t.Helper()
	isolateHome(t)
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(accountsConfigJSON(n)),
		HTTPClient: &http.Client{Transport: transport},
		Guard:      guard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}
	return client
}

func chatOnce(ctx context.Context, c *Client) (core.Stream, error) {
	return c.Chat(ctx, &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
}

// chatFailure runs one Chat call against a scripted upstream answer and returns
// the error it produced, which is what the gateway would classify.
func chatFailure(t *testing.T, status int, body string, accounts int) error {
	t.Helper()
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(status, body), nil
	}}
	c := newFailureClient(t, nil, accounts, transport)

	_, err := chatOnce(t.Context(), c)
	if err == nil {
		t.Fatal("Chat returned no error")
	}
	return err
}

// ---------------------------------------------------------------------------
// vendor signal -> core.FailureKind
// ---------------------------------------------------------------------------

const (
	riskBody  = `{"code":3012,"msg":"request has been blocked due to unusual activity."}`
	modelBody = `{"code":3006,"message":"model not allowed"}`
)

// TestUpstreamCodesMapToTheSharedFailureKinds drives the real Chat path for
// every signal this module names and asserts the shared classification the
// gateway acts on.
func TestUpstreamCodesMapToTheSharedFailureKinds(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   core.FailureKind
	}{
		{"3012 risk control", http.StatusMethodNotAllowed, riskBody, core.FailureWAF},
		{"bare 405", http.StatusMethodNotAllowed, `{"message":"blocked"}`, core.FailureWAF},
		{"402 balance exhausted", http.StatusPaymentRequired, `{"error":{"message":"insufficient balance"}}`, core.FailureQuota},
		{"quota keyword", http.StatusBadRequest, `{"message":"额度已用完"}`, core.FailureQuota},
		{"429 rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, core.FailureRateLimited},
		{"3009 concurrency in a 200", http.StatusOK, `{"code":3009,"message":"concurrency exceeded"}`, core.FailureRateLimited},
		{"401 rejected credential", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, core.FailureAuth},
		{"403 rejected credential", http.StatusForbidden, `{"error":{"message":"permission denied"}}`, core.FailureAuth},
		{"3006 model not allowed", http.StatusOK, modelBody, core.FailureOther},
		{"500 server error", http.StatusInternalServerError, `{"error":"internal"}`, core.FailureUpstream},
		{"404 unknown route", http.StatusNotFound, `{"error":{"message":"no such route"}}`, core.FailureUpstream},
		{"403 captcha challenge", http.StatusForbidden, `{"error":{"message":"captcha verify failed"}}`, core.FailureUpstream},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
				return jsonResponse(tc.status, tc.body), nil
			}}
			c := newFailureClient(t, nil, 1, transport)

			_, err := chatOnce(t.Context(), c)
			if err == nil {
				t.Fatal("Chat returned no error")
			}
			if got := transport.count(); got != 1 {
				t.Errorf("got %d attempts, want exactly 1", got)
			}

			f, ok := core.AsFailure(err)
			if !ok {
				t.Fatalf("error %v (%T) is not a *core.Failure; the gateway can only act on a classified error", err, err)
			}
			if f.Kind != tc.want {
				t.Errorf("kind = %q, want %q", f.Kind, tc.want)
			}
			if got := core.FailureKindOf(err); got != tc.want {
				t.Errorf("core.FailureKindOf = %q, want %q", got, tc.want)
			}
			if f.Client != "zcode" {
				t.Errorf("client = %q, want zcode", f.Client)
			}
			if f.Account != "k1" {
				t.Errorf("account = %q, want k1 (the account that produced the answer)", f.Account)
			}
			if f.Status != tc.status {
				t.Errorf("status = %d, want %d", f.Status, tc.status)
			}
		})
	}
}

// TestRiskControlYieldsFailureWAF is the deliverable that matters: the vendor
// decides risk control from the egress IP, so it has to arrive as a WAF block
// -- the only kind that feeds the gateway's IP-level gate.
func TestRiskControlYieldsFailureWAF(t *testing.T) {
	err := chatFailure(t, http.StatusMethodNotAllowed, riskBody, 1)

	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("3012 produced %v (%T), not a *core.Failure", err, err)
	}
	if f.Kind != core.FailureWAF {
		t.Fatalf("3012 classified as %q, want %q: only a WAF block feeds the IP gate", f.Kind, core.FailureWAF)
	}
	if !core.Retryable(f.Kind) {
		t.Errorf("core.Retryable(%q) = false, but the gateway must rotate a WAF block", f.Kind)
	}
	if f.Client != "zcode" || f.Account != "k1" {
		t.Errorf("attribution = client %q account %q, want zcode/k1", f.Client, f.Account)
	}
	if f.Status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", f.Status, http.StatusMethodNotAllowed)
	}
	// The account state machine must still cool the account it saw 3012 on.
	if !strings.Contains(f.Error(), "3012") && !strings.Contains(f.Error(), "unusual activity") {
		t.Errorf("the wrapped cause should still name the vendor code, got %q", f.Error())
	}
}

// TestRiskControlFeedsTheSharedIPGateAndStopsTheRotation proves the machinery
// actually fires: the second distinct account to hit risk control must stop the
// module's own rotation instead of spending the third one.
func TestRiskControlFeedsTheSharedIPGateAndStopsTheRotation(t *testing.T) {
	guard := core.NewGuard(nil)
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusMethodNotAllowed, riskBody), nil
	}}
	c := newFailureClient(t, guard, 3, transport)

	_, err := chatOnce(t.Context(), c)
	if f, ok := core.AsFailure(err); !ok || f.Kind != core.FailureWAF {
		t.Fatalf("err = %v, want a %q failure", err, core.FailureWAF)
	}
	if got := transport.count(); got != 2 {
		t.Errorf("got %d attempts, want 2: the IP gate trips on the second distinct account", got)
	}
	if !guard.IP.Active() {
		t.Error("the shared IP gate should be active after two accounts hit risk control")
	}
}

// TestNilGuardDoesNotPanic is the counterpart: with no Guard built (the unit
// test path) the report is a safe no-op, so the loop keeps walking the pool.
func TestNilGuardDoesNotPanic(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusMethodNotAllowed, riskBody), nil
	}}
	c := newFailureClient(t, nil, 2, transport)

	_, err := chatOnce(t.Context(), c)
	f, ok := core.AsFailure(err)
	if !ok || f.Kind != core.FailureWAF {
		t.Fatalf("err = %v, want a %q failure", err, core.FailureWAF)
	}
	if got := transport.count(); got != 2 {
		t.Errorf("got %d attempts, want 2: a nil Guard must not stop the rotation", got)
	}
	if f.Account != "k2" {
		t.Errorf("account = %q, want k2 (the last account tried)", f.Account)
	}
}

// TestRetryableAgreesWithTheKindsThisModuleEmits pins the module/gateway
// agreement: the gateway rotates exactly what core.Retryable allows.
func TestRetryableAgreesWithTheKindsThisModuleEmits(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"risk control rotates", http.StatusMethodNotAllowed, riskBody, true},
		{"quota rotates", http.StatusPaymentRequired, `{"error":{"message":"insufficient balance"}}`, true},
		{"rate limit rotates", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, true},
		{"bad credential rotates", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, true},
		{"transport error rotates", http.StatusInternalServerError, `{"error":"internal"}`, true},
		{"model rejection never rotates", http.StatusOK, modelBody, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := chatFailure(t, tc.status, tc.body, 1)
			kind := core.FailureKindOf(err)
			if kind == core.FailureOther {
				if _, ok := core.AsFailure(err); !ok {
					t.Fatalf("%v carries no *core.Failure", err)
				}
			}
			if got := core.Retryable(kind); got != tc.want {
				t.Errorf("kind %q: core.Retryable = %v, want %v", kind, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// pacing and cancellation
// ---------------------------------------------------------------------------

// TestChatPacesTheRotation pins that the wait is real: this loop used to fire up
// to five attempts back to back from one egress IP, which is exactly the density
// risk control measures.  BackoffFrom(base, 0) is 500ms +/- 25%, so anything
// under ~375ms proves no wait happened.
func TestChatPacesTheRotation(t *testing.T) {
	attempt := 0
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		attempt++
		if attempt == 1 {
			return jsonResponse(http.StatusMethodNotAllowed, riskBody), nil
		}
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := newFailureClient(t, nil, 2, transport)

	start := time.Now()
	stream, err := chatOnce(t.Context(), c)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat should have failed over, got %v", err)
	}
	_ = collect(t, stream)

	if got := transport.count(); got != 2 {
		t.Errorf("got %d attempts, want 2", got)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("two attempts took %s; the rotation should have paced them", elapsed)
	}
}

// TestCancelledCallerStopsTheLoopAndReturnsAClassifiedError cancels the call
// while the first attempt is in flight, so the assertion is deterministic and
// the test never sleeps for a backoff.
func TestCancelledCallerStopsTheLoopAndReturnsAClassifiedError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		// The caller gives up while this attempt is being answered.
		cancel()
		return jsonResponse(http.StatusMethodNotAllowed, riskBody), nil
	}}
	c := newFailureClient(t, nil, 5, transport)

	_, err := chatOnce(ctx, c)
	if err == nil {
		t.Fatal("Chat returned no error")
	}
	if got := transport.count(); got != 1 {
		t.Errorf("got %d attempts, want 1: a dead context must stop the loop", got)
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *core.Failure", err, err)
	}
	if f.Kind != core.FailureWAF {
		t.Errorf("kind = %q, want %q", f.Kind, core.FailureWAF)
	}
}
