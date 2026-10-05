package trae

// failure_test.go — trae's opt-in to the shared error-classification contract
// plus the inter-attempt pacing.
//
// These tests run against a local httptest.Server, so what gets classified is
// the real HTTP path: status codes, response bodies and Authorization headers.
// Nothing here touches the network (127.0.0.1 only), and nothing waits out a
// real backoff: the pacing tests inject a tiny base or an already-dead context.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const okSSE = "event:output\ndata:{\"response\":\"ok\"}\n\nevent:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func chatReq() *core.ChatRequest {
	return &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}}
}

// upstream is a local stand-in for the vendor chat endpoint.  It answers every
// request through reply, and records how many attempts it saw and which
// credentials they carried.
type upstream struct {
	*httptest.Server
	calls  atomic.Int64
	mu     sync.Mutex
	tokens []string
}

func newUpstream(t *testing.T, reply func(attempt int, auth string) (int, string)) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		n := int(u.calls.Add(1))
		u.mu.Lock()
		u.tokens = append(u.tokens, auth)
		u.mu.Unlock()

		status, body := reply(n, auth)
		ct := "application/json"
		if status == 200 {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(u.Close)
	return u
}

func fixedReply(status int, body string) func(int, string) (int, string) {
	return func(int, string) (int, string) { return status, body }
}

// tokensSeen returns a copy of the Authorization headers offered so far.
func (u *upstream) tokensSeen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.tokens...)
}

// chatClient points a Client at the local upstream and is the only place the
// attempt count is set, so every case here is explicit about how many times the
// vendor may be hit.
func chatClient(t *testing.T, u *upstream, attempts int, auths []*Auth) *Client {
	t.Helper()
	cfg := loadConfig(nil, nil)
	cfg.ChatHost = u.URL
	cfg.Attempts = attempts
	return testClient(t, cfg, auths, nil)
}

// assertFailure checks the error is a *core.Failure of the wanted kind,
// attributed to the wanted account, and that the original *trae.Error is still
// reachable through it.
func assertFailure(t *testing.T, err error, want core.FailureKind, wantAccount string) *core.Failure {
	t.Helper()
	if err == nil {
		t.Fatal("expected a classified error, got nil")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("error is not classified: %T: %v", err, err)
	}
	if f.Kind != want {
		t.Errorf("Kind = %v, want %v (err: %v)", f.Kind, want, err)
	}
	if got := core.FailureKindOf(err); got != want {
		t.Errorf("core.FailureKindOf = %v, want %v", got, want)
	}
	if f.Client != clientName {
		t.Errorf("Client = %q, want %q", f.Client, clientName)
	}
	if wantAccount != "" && f.Account != wantAccount {
		t.Errorf("Account = %q, want %q (err: %v)", f.Account, wantAccount, err)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Errorf("the trae *Error must stay reachable through the wrapper: %v", err)
	}
	return f
}

// ---- vendor code -> core.FailureKind ---------------------------------------

func TestTerminalErrorIsClassified(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		want       core.FailureKind
		wantErr    ErrKind
		wantStatus int
		wantCode   int64
	}{
		// Quota: the vendor says the plan or the balance is spent.
		{"1005 plan limit", 403, `{"code":1005,"message":"plan_limit"}`, core.FailureQuota, ErrPlanLimit, 403, 1005},
		{"4008 quota exhausted", 402, `{"code":4008,"message":"quota exhausted"}`, core.FailureQuota, ErrQuota, 402, 4008},
		// Auth: the credential itself is no good.
		// A 401 rejects the credential at the transport-auth layer.  The body
		// still carries the vendor's 1001/4010 and that code is kept for the
		// operator, but the kind is session_dead: the token cannot serve until
		// it is replaced, so it must be parked rather than 60s-cooled.
		{"1001 auth failed", 401, `{"code":1001,"message":"auth failed"}`, core.FailureSessionDead, ErrSessionDead, 401, 1001},
		{"4010 credential rejected", 401, `{"code":4010,"message":"token rejected"}`, core.FailureSessionDead, ErrSessionDead, 401, 4010},
		// A dead vendor session survives the trip to the gateway so the pool
		// can park it for an hour.  The body carries no code, so there is none
		// to report: 0 means "the body had none", not "HTTP-level".
		{"401 session dead", 401, `{"message":"session expired"}`, core.FailureSessionDead, ErrSessionDead, 401, 0},
		// Pacing kinds.
		{"429 soft rate", 429, `{"message":"too many requests"}`, core.FailureRateLimited, ErrSoftRate, 429, 0},
		{"4011 rate limit", 400, `{"code":4011,"message":"rate limited"}`, core.FailureRateLimited, ErrSoftRate, 400, 4011},
		{"9074 retry later", 400, `{"code":9074,"message":"contention"}`, core.FailureRateLimited, ErrRetryLater, 400, 9074},
		// Transport-ish.
		{"503 server", 503, `upstream unavailable`, core.FailureUpstream, ErrServer, 503, 0},
		{"599 server", 599, `boom`, core.FailureUpstream, ErrServer, 599, 0},
		// Request-level: deliberately FailureOther, i.e. never retried by the
		// gateway, because trae's own rules never rotate them either.
		{"4001 param", 400, `{"code":4001,"message":"cannot unmarshal"}`, core.FailureOther, ErrParam, 400, 4001},
		{"404 not found", 404, `no such route`, core.FailureOther, ErrNotFound, 404, 0},
		{"400 other client error", 400, `{"message":"bad request"}`, core.FailureOther, ErrClient, 400, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, fixedReply(tc.status, tc.body))
			account := "u-" + strings.ReplaceAll(tc.name, " ", "-")
			c := chatClient(t, u, 1, []*Auth{testAuth(account, "token-"+account)})

			_, err := c.Chat(context.Background(), chatReq())
			f := assertFailure(t, err, tc.want, account)

			if f.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", f.Status, tc.wantStatus)
			}
			// The trae kind behind the core kind must be the expected one.
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("the trae *Error must stay reachable through the wrapper: %v", err)
			}
			if e.Kind != tc.wantErr {
				t.Errorf("trae kind = %v, want %v", e.Kind, tc.wantErr)
			}
			// A vendor code the body carried must survive onto the *Error.  It
			// is parsed to classify the failure anyway, and it is the number an
			// operator greps for; throwing it away also made the HTTP path
			// disagree with the SSE path about the same upstream refusal.
			if e.Code != tc.wantCode {
				t.Errorf("trae code = %d, want %d (err: %v)", e.Code, tc.wantCode, err)
			}
			if got := u.calls.Load(); got != 1 {
				t.Errorf("attempts = %d, want 1", got)
			}
		})
	}
}

// A network failure never reaches HTTP, but it is what most retries are for.
func TestTransportFailureIsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening: connection refused, not an HTTP answer

	cfg := loadConfig(nil, nil)
	cfg.ChatHost = url
	cfg.Attempts = 1
	c := testClient(t, cfg, []*Auth{testAuth("u-net", "token-net")}, nil)

	_, err := c.Chat(context.Background(), chatReq())
	f := assertFailure(t, err, core.FailureUpstream, "u-net")
	if f.Err == nil {
		t.Error("a transport failure must keep its cause")
	}
}

// A 1005 is a plan limit: the module must not spend another account on it, and
// the account that hit it is parked on the 12h plan-limit cooldown.
func TestPlanLimitNeverRotatesToTheNextAccount(t *testing.T) {
	u := newUpstream(t, fixedReply(403, `{"code":1005,"message":"plan_limit"}`))
	c := chatClient(t, u, 3, []*Auth{testAuth("A", "token-A"), testAuth("B", "token-B")})

	_, err := c.Chat(context.Background(), chatReq())
	f := assertFailure(t, err, core.FailureQuota, "A")
	if f.Status != 403 {
		t.Errorf("Status = %d, want 403", f.Status)
	}
	if got := u.calls.Load(); got != 1 {
		t.Errorf("a plan limit must not rotate: %d upstream attempts", got)
	}
	seen := u.tokensSeen()
	if len(seen) != 1 || !strings.Contains(seen[0], "token-A") {
		t.Errorf("credentials offered = %v, want only token-A", seen)
	}

	states := map[string]string{}
	for _, s := range c.pool.Snapshot() {
		states[s.ID] = s.State
	}
	if states["A"] != stateExhausted {
		t.Errorf("account A state = %q, want %q", states["A"], stateExhausted)
	}
	if states["B"] != stateReady {
		t.Errorf("account B state = %q, want %q (it must be untouched)", states["B"], stateReady)
	}
}

// ---- retryability ---------------------------------------------------------

// The classification must agree with trae's own retry rules, with exactly one
// documented divergence: a plan limit is FailureQuota, which core.Retryable
// allows and trae.retryableKind forbids.
func TestRetryableAgreesWithClassification(t *testing.T) {
	cases := []struct {
		kind ErrKind
		want core.FailureKind
	}{
		{ErrQuota, core.FailureQuota},
		{ErrPlanLimit, core.FailureQuota}, // divergence, asserted below
		{ErrAuth, core.FailureAuth},
		{ErrSessionDead, core.FailureSessionDead},
		{ErrSoftRate, core.FailureRateLimited},
		{ErrRetryLater, core.FailureRateLimited},
		{ErrServer, core.FailureUpstream},
		{ErrTransport, core.FailureUpstream},
		{ErrParam, core.FailureOther},
		{ErrNotFound, core.FailureOther},
		{ErrClient, core.FailureOther},
	}

	// ErrNone is not a failure and cannot reach classifyTerminal, but the
	// default arm still has to answer something; FailureOther is the safe one.
	if got := failureKindFor(ErrNone); got != core.FailureOther {
		t.Errorf("failureKindFor(ok) = %v, want %v", got, core.FailureOther)
	}

	for _, tc := range cases {
		t.Run(tc.kind.String(), func(t *testing.T) {
			got := failureKindFor(tc.kind)
			if got != tc.want {
				t.Fatalf("failureKindFor(%v) = %v, want %v", tc.kind, got, tc.want)
			}
			if tc.kind == ErrPlanLimit {
				if retryableKind(tc.kind) {
					t.Error("trae must keep refusing to rotate a plan limit")
				}
				if !core.Retryable(got) {
					t.Error("the shared contract classifies a quota failure as retryable; the divergence is real and must stay visible")
				}
				return
			}
			if core.Retryable(got) != retryableKind(tc.kind) {
				t.Errorf("core.Retryable(%v) = %v but trae.retryableKind(%v) = %v",
					got, core.Retryable(got), tc.kind, retryableKind(tc.kind))
			}
		})
	}
}

// A non-*trae.Error cannot be classified and must survive untouched, so the
// gateway keeps treating it as unclassified rather than mislabelling it.
func TestUnclassifiableErrorIsPassedThrough(t *testing.T) {
	boom := errors.New("boom")
	if got := classifyTerminal("A", boom); !errors.Is(got, boom) {
		t.Errorf("classifyTerminal changed an unclassifiable error: %v", got)
	}
	if _, ok := core.AsFailure(classifyTerminal("A", boom)); ok {
		t.Error("a non-trae error must stay unclassified")
	}
	if got := classifyTerminal("A", nil); got != nil {
		t.Errorf("classifyTerminal(nil) = %v, want nil", got)
	}
}

// ---- pacing ---------------------------------------------------------------

func TestChatPacesBetweenAttempts(t *testing.T) {
	u := newUpstream(t, func(_ int, auth string) (int, string) {
		if strings.Contains(auth, "token-A") {
			return 401, `{"code":1001,"message":"auth failed"}`
		}
		return 200, okSSE
	})
	c := chatClient(t, u, 2, []*Auth{testAuth("A", "token-A"), testAuth("B", "token-B")})
	// BackoffFrom jitters by +/-25%, so the floor for one wait -- the first
	// pause, which is BackoffFrom(base, 0) -- is three quarters of the base.
	// Anything at or above that proves the loop really waited; without a pause
	// this is two loopback round trips, a millisecond or two.
	c.backoffBase = 30 * time.Millisecond
	floor := (c.backoffBase * 3) / 4

	start := time.Now()
	st, err := c.Chat(context.Background(), chatReq())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat should have failed over to B, got %v", err)
	}
	if got := u.calls.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2", got)
	}
	if elapsed < floor {
		t.Errorf("two attempts took %v with a %v backoff base: the loop did not pace itself", elapsed, c.backoffBase)
	}
	ev, err := st.Recv()
	if err != nil || ev.Type != core.EventDelta || ev.Delta != "ok" {
		t.Fatalf("first event = %#v, %v", ev, err)
	}
}

// A dead caller context must abort the loop at the sleep instead of waiting the
// backoff out; the error still names the account the attempt failed on.
func TestCancelledContextStopsTheRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int64
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		cancel() // the caller went away while this attempt was in flight
		return jsonResponse(401, `{"code":1001,"message":"auth failed"}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", "token-A"), testAuth("B", "token-B")}, rt)
	// If the sleep were not abortable, this backoff would dominate the test.
	c.backoffBase = 30 * time.Second

	start := time.Now()
	_, err := c.Chat(ctx, chatReq())
	elapsed := time.Since(start)

	if got := calls.Load(); got != 1 {
		t.Errorf("a cancelled context must stop the loop: %d attempts", got)
	}
	assertFailure(t, err, core.FailureSessionDead, "A")
	if elapsed > time.Second {
		t.Errorf("a cancelled context waited %v: the backoff must be abortable", elapsed)
	}
}

// ---- guard ------------------------------------------------------------------

// deps.Guard is nil in every unit test and may be nil in production wiring; the
// client must not reach for it.
func TestNilGuardIsSafe(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(403, `{"code":1005,"message":"plan_limit"}`), nil
	})}
	cl, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"access_token":"tok-nil-guard","user_id":"u-nil","auto_discover":false,"attempts":1}`),
		HTTPClient: hc,
		Guard:      nil,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", cl)
	}

	_, err = c.Chat(context.Background(), chatReq())
	f := assertFailure(t, err, core.FailureQuota, "u-nil")
	if f.Status != 403 {
		t.Errorf("Status = %d, want 403", f.Status)
	}
}
