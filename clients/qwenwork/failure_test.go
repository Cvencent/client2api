package qwenwork

// Terminal-error classification tests.  These cover the contract between this
// module's own errKind vocabulary and internal/core's shared core.FailureKind,
// which is what lets internal/gateway rotate accounts, back off, and turn a
// refusal into a status code and an error type.
//
// Everything here is offline (fakeTransport) and nothing sleeps for the shared
// 500 ms rotation base: the pace is shrunk through paceBaseOverride, which is
// the only reason that field exists.  The two tests that assert pacing did NOT
// happen go the other way and INFLATE the base, so that a single unwanted sleep
// would blow a generous wall-clock bound instead of hiding inside it — a tight
// bound against a 1 ms base proves nothing and is flaky under -race load.

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

const (
	cfgOneAccount  = `{"accounts":[{"uid":"1","nickname":"first","access_token":"tok-first"}]}`
	cfgTwoAccounts = `{"accounts":[{"uid":"1","nickname":"first","access_token":"tok-first"},` +
		`{"uid":"2","nickname":"second","access_token":"tok-second"}]}`
	cfgThreeAccounts = `{"accounts":[{"uid":"1","access_token":"tok-one"},` +
		`{"uid":"2","access_token":"tok-two"},{"uid":"3","access_token":"tok-three"}]}`
)

// terminalTestClient builds a client around rt.  clearCredentialEnv keeps the
// pool down to exactly the accounts the config names, so the attempt count in
// these tests is deterministic.
func terminalTestClient(t *testing.T, rt *fakeTransport, accounts string) (core.Client, *Client) {
	t.Helper()
	clearCredentialEnv(t)
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(accounts),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc, ok := c.(*Client)
	if !ok {
		t.Fatal("New did not return a *Client")
	}
	return c, qc
}

func terminalChat(t *testing.T, c core.Client, ctx context.Context) error {
	t.Helper()
	_, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "pro",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	return err
}

// wantFailure insists that err is a classified *core.Failure, which is the only
// shape the gateway can rotate on.
func wantFailure(t *testing.T, err error) *core.Failure {
	t.Helper()
	if err == nil {
		t.Fatal("Chat succeeded, want a failure")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("Chat error %v (%T) is not a *core.Failure: the gateway can only rotate on one", err, err)
	}
	if f.Client != "qwenwork" {
		t.Errorf("failure client = %q, want qwenwork", f.Client)
	}
	return f
}

// TestTerminalFailureKinds walks every terminal condition the retry loop can
// end on and asserts the shared classification, the gateway's retry decision,
// the status, and the attribution.
func TestTerminalFailureKinds(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		transportErr  error
		want          core.FailureKind
		wantRetryable bool
	}{
		{"a 402 is quota", http.StatusPaymentRequired, `{"message":"payment required"}`, nil, core.FailureQuota, true},
		{"14018 on a 429 is quota", http.StatusTooManyRequests, `{"code":14018,"message":"insufficient credits"}`, nil, core.FailureQuota, true},
		{"a plain 429 is rate limited", http.StatusTooManyRequests, `{"message":"slow down"}`, nil, core.FailureRateLimited, true},
		{"a 503 is rate limited", http.StatusServiceUnavailable, `{"message":"upstream busy"}`, nil, core.FailureRateLimited, true},
		{"a 401 is auth", http.StatusUnauthorized, `{"message":"token expired"}`, nil, core.FailureAuth, true},
		{"a 403 is auth", http.StatusForbidden, `{"message":"forbidden"}`, nil, core.FailureAuth, true},
		{"a 400 is the caller's fault", http.StatusBadRequest, `{"message":"model does not exist"}`, nil, core.FailureOther, false},
		{"a dead connection is upstream", 0, "", errors.New("dial tcp: connect: connection refused"), core.FailureUpstream, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeTransport{}
			rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return fakeResponse(req, tc.status, tc.body), nil
			}
			c, qc := terminalTestClient(t, rt, cfgOneAccount)
			// Inflated, not shrunk: this test asserts the loop does NOT pace.
			// With a 1 ms base a stray sleep would be invisible; with 10 s it
			// cannot fit inside the bound below.
			qc.paceBaseOverride = 10 * time.Second

			start := time.Now()
			err := terminalChat(t, c, context.Background())
			elapsed := time.Since(start)

			f := wantFailure(t, err)
			if f.Kind != tc.want {
				t.Errorf("kind = %s, want %s (status %d, body %s)", f.Kind, tc.want, tc.status, tc.body)
			}
			if got := core.FailureKindOf(err); got != tc.want {
				t.Errorf("core.FailureKindOf = %s, want %s", got, tc.want)
			}
			if got := core.Retryable(f.Kind); got != tc.wantRetryable {
				t.Errorf("core.Retryable(%s) = %v, want %v", f.Kind, got, tc.wantRetryable)
			}
			if f.Status != tc.status {
				t.Errorf("failure status = %d, want %d", f.Status, tc.status)
			}
			if f.Account != "uid:1" {
				t.Errorf("failure account = %q, want the account that answered (uid:1)", f.Account)
			}
			if rt.count() != 1 {
				t.Errorf("made %d requests, want 1: only one account is configured", rt.count())
			}
			// The loop stops when pick finds nothing, before it reaches the
			// pacing call, so a one-account failure must not wait at all. The
			// bound is generous because this runs under -race alongside other
			// packages; the 10 s base above is what makes it decisive.
			if elapsed > 2*time.Second {
				t.Errorf("Chat took %s: the loop must not wait after its last attempt", elapsed)
			}
		})
	}
}

// TestClientFailureDoesNotRotateAcrossThePool is the reason kindClient maps to
// FailureOther rather than to a retryable kind: a request the vendor calls
// malformed is malformed on every account, so rotating only spends the pool.
func TestClientFailureDoesNotRotateAcrossThePool(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return fakeResponse(req, http.StatusBadRequest, `{"message":"model does not exist"}`), nil
	}
	c, qc := terminalTestClient(t, rt, cfgThreeAccounts)
	qc.paceBaseOverride = time.Millisecond

	err := terminalChat(t, c, context.Background())
	f := wantFailure(t, err)
	if f.Kind != core.FailureOther {
		t.Fatalf("kind = %s, want %s", f.Kind, core.FailureOther)
	}
	if core.Retryable(f.Kind) {
		t.Errorf("core.Retryable(%s) = true: a malformed request would be replayed on every account", f.Kind)
	}
	if rt.count() != 1 {
		t.Errorf("made %d requests, want 1: three accounts were available, so a rotating classification would have used all three", rt.count())
	}
}

// TestCoreRetryableAgreesWithModuleClassification pins the two decision
// functions to each other.  If they ever disagree, the pool parks an account
// for a failure the gateway then refuses to rotate on (or the reverse).
func TestCoreRetryableAgreesWithModuleClassification(t *testing.T) {
	tests := []struct {
		kind errKind
		want core.FailureKind
	}{
		{kindQuota, core.FailureQuota},
		{kindTransient, core.FailureRateLimited},
		{kindNetwork, core.FailureUpstream},
		{kindAuth, core.FailureAuth},
		{kindClient, core.FailureOther},
	}
	for _, tc := range tests {
		got := failureKind(tc.kind)
		if got != tc.want {
			t.Errorf("failureKind(%s) = %s, want %s", tc.kind, got, tc.want)
		}
		if core.Retryable(got) != retryable(tc.kind) {
			t.Errorf("core.Retryable(%s) = %v but retryable(%s) = %v: the pool and the gateway must agree about rotating",
				got, core.Retryable(got), tc.kind, retryable(tc.kind))
		}
	}
	// kindNone never reaches the terminal classification -- classifyTerminal
	// answers ErrNotConfigured for a nil error instead -- but the default must
	// stay on the conservative "try somewhere else" side if it ever does.
	if got := failureKind(kindNone); got != core.FailureUpstream {
		t.Errorf("failureKind(kindNone) = %s, want %s", got, core.FailureUpstream)
	}
}

// TestClassifyTerminalTreatsALocalFailureAsOther covers the path with no vendor
// status at all, where the only safe answer is the non-rotating one.
func TestClassifyTerminalTreatsALocalFailureAsOther(t *testing.T) {
	local := errors.New("cosy: key exchange failed")
	err := classifyTerminal(local, account{UID: "7"})

	f := wantFailure(t, err)
	if f.Kind != core.FailureOther {
		t.Errorf("kind = %s, want %s: a local failure has no vendor verdict to rotate on", f.Kind, core.FailureOther)
	}
	if f.Status != 0 {
		t.Errorf("status = %d, want 0", f.Status)
	}
	if !errors.Is(err, local) {
		t.Errorf("the classified failure does not wrap the original error: %v", err)
	}
	if core.Retryable(f.Kind) {
		t.Errorf("core.Retryable(%s) = true: a local failure must not rotate", f.Kind)
	}
	if f.Account != "uid:7" {
		t.Errorf("account = %q, want uid:7", f.Account)
	}
	// A nil error means nothing failed, which must keep the module's existing
	// answer rather than inventing a failure.
	if got := classifyTerminal(nil, account{}); !errors.Is(got, core.ErrNotConfigured) {
		t.Errorf("classifyTerminal(nil) = %v, want ErrNotConfigured", got)
	}
}

// TestChatStopsWhenTheContextDiesBetweenAttempts is the pacing contract: the
// loop notices a dead context at the sleep, stops immediately, and still
// returns a classified failure rather than a bare context error.
func TestChatStopsWhenTheContextDiesBetweenAttempts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rt := &fakeTransport{}
	// The caller walks away while the first attempt is in flight.  The 503 is
	// already on its way back, so the loop holds a classified failure and one
	// more account to try -- which is exactly when the pace runs.
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		cancel()
		return fakeResponse(req, http.StatusServiceUnavailable, `{"message":"upstream busy"}`), nil
	}
	c, qc := terminalTestClient(t, rt, cfgTwoAccounts)
	// Inflated, not shrunk: the point is that the dead context makes the pace
	// return at once. A 1 ms base would be indistinguishable from sleeping.
	qc.paceBaseOverride = 5 * time.Second

	start := time.Now()
	err := terminalChat(t, c, ctx)
	elapsed := time.Since(start)

	if rt.count() != 1 {
		t.Errorf("made %d requests, want 1: a cancelled context must stop the loop before the second account", rt.count())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Chat took %s: it waited out the backoff on a dead context", elapsed)
	}
	f := wantFailure(t, err)
	if f.Kind != core.FailureRateLimited {
		t.Errorf("kind = %s, want %s: the 503 already in flight is what the loop must report", f.Kind, core.FailureRateLimited)
	}
	if f.Account != "uid:1" {
		t.Errorf("account = %q, want uid:1, the account that produced the terminal failure", f.Account)
	}
}

// TestNilGuardDoesNotPanic: unit tests build core.Deps by hand and leave Guard
// nil, and every core.Guard method is documented nil-safe, so nothing here may
// require one.
func TestNilGuardDoesNotPanic(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return fakeResponse(req, http.StatusInternalServerError, `{"message":"boom"}`), nil
	}
	clearCredentialEnv(t)
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(cfgTwoAccounts),
		HTTPClient: &http.Client{Transport: rt},
		Guard:      nil,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc := c.(*Client)
	qc.paceBaseOverride = time.Millisecond

	chatErr := terminalChat(t, c, context.Background())
	if chatErr == nil {
		t.Fatal("a 500 on every account must fail the request")
	}
	wantFailure(t, chatErr)
	if rt.count() != 2 {
		t.Errorf("made %d requests, want 2 (one per account)", rt.count())
	}

	st := c.Status(context.Background())
	if len(st.Accounts) != 2 {
		t.Errorf("Status listed %d accounts, want 2", len(st.Accounts))
	}
}

// TestStatusExtraHoldsOnlyDocumentedHealthKeys closes the loop on the
// account-context decision: because no quota or plan fields are surfaced,
// Extra must carry the documented health keys and nothing credential-shaped.
func TestStatusExtraHoldsOnlyDocumentedHealthKeys(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return fakeResponse(req, http.StatusInternalServerError, `{"message":"boom"}`), nil
	}
	c, qc := terminalTestClient(t, rt, cfgTwoAccounts)
	qc.paceBaseOverride = time.Millisecond
	if err := terminalChat(t, c, context.Background()); err == nil {
		t.Fatal("a 500 on every account must fail the request")
	}

	allowed := map[string]bool{"failures": true, "uid": true, "expires_in": true, "cooldown_until": true}
	st := c.Status(context.Background())
	for _, acct := range st.Accounts {
		for k, v := range acct.Extra {
			if !allowed[k] {
				t.Errorf("account %s has an undocumented Extra key %q = %v", acct.ID, k, v)
			}
		}
		blob, err := json.Marshal(acct)
		if err != nil {
			t.Fatalf("marshalling the account status: %v", err)
		}
		for _, secret := range []string{"tok-first", "tok-second"} {
			if strings.Contains(string(blob), secret) {
				t.Errorf("a credential leaked into Status: %s", blob)
			}
		}
		if strings.Contains(acct.Note, "tok-") {
			t.Errorf("a credential leaked into the note: %q", acct.Note)
		}
	}
}
