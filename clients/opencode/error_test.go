package opencode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The headline hazard.
// ---------------------------------------------------------------------------

// Zen checks the model BEFORE it checks the key, and answers 401 for both.
// Classifying on the status alone would disable a healthy account whenever a
// caller mistyped a model id.
func TestClassifyPrefersTheNamedTypeOverTheStatus(t *testing.T) {
	v := classify(401, []byte(`{"type":"error","error":{"type":"ModelError","message":"Model bogus is not supported"}}`))
	if v.kind != kindModel {
		t.Fatalf("kind = %q, want %q for a ModelError that arrives as HTTP 401", v.kind, kindModel)
	}
	if !strings.Contains(v.msg, "is not supported") {
		t.Fatalf("msg = %q, want the vendor's own sentence", v.msg)
	}

	v = classify(401, []byte(`{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`))
	if v.kind != kindAuth {
		t.Fatalf("kind = %q, want %q for an AuthError", v.kind, kindAuth)
	}
}

func TestClassifyTypeNames(t *testing.T) {
	cases := map[string]failureKind{
		"AuthError":           kindAuth,
		"AuthenticationError": kindAuth,
		"InvalidApiKeyError":  kindAuth,
		"ModelError":          kindModel,
		"ModelNotFoundError":  kindModel,
		"RateLimitError":      kindRate,
		"QuotaExceededError":  kindQuota,
		"BillingError":        kindQuota,
		"FreeTierError":       kindFreeTier,
		"":                    kindNone,
		"SomethingWeird":      kindNone,
	}
	for in, want := range cases {
		if got := classifyType(in); got != want {
			t.Fatalf("classifyType(%q) = %q, want %q", in, got, want)
		}
	}
}

// With no named type the module falls back to the vendor's wording, and only
// then to the status code.
func TestClassifyByTextMarkers(t *testing.T) {
	cases := []struct {
		body string
		want failureKind
	}{
		{`{"error":{"message":"Model claude-9 is not supported"}}`, kindModel},
		{`{"error":{"message":"Rate limit exceeded, slow down"}}`, kindRate},
		{`{"error":{"message":"Insufficient credit balance"}}`, kindQuota},
		{`{"error":{"message":"Invalid API key provided"}}`, kindAuth},
		// The free-tier gate answers 403 with no named type in some builds; the
		// wording alone must not fall through to the 403 -> auth fallback.
		{`{"error":{"message":"OpenCode's free tier can only be used from within OpenCode"}}`, kindFreeTier},
	}
	for _, tc := range cases {
		if got := classify(400, []byte(tc.body)).kind; got != tc.want {
			t.Fatalf("classify(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestClassifyByStatusFallback(t *testing.T) {
	cases := []struct {
		status int
		want   failureKind
	}{
		{402, kindQuota},
		{429, kindRate},
		{401, kindAuth},
		{403, kindAuth},
		{500, kindServer},
		{503, kindServer},
		{400, kindClient},
		{404, kindClient},
		{200, kindServer},
	}
	for _, tc := range cases {
		if got := classify(tc.status, []byte(`{}`)).kind; got != tc.want {
			t.Fatalf("classify(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestErrorTextOfPrefersTheNestedMessage(t *testing.T) {
	if got := errorTextOf([]byte(`{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`)); got != "Missing API key." {
		t.Fatalf("errorTextOf = %q, want the nested message", got)
	}
	if got := errorTextOf([]byte(`{"message":"top level"}`)); got != "top level" {
		t.Fatalf("errorTextOf = %q, want the top-level message", got)
	}
	got := errorTextOf([]byte(`not json at all`))
	if got != "not json at all" {
		t.Fatalf("errorTextOf = %q, want the raw body as a last resort", got)
	}
	// An empty body carries no message.  Returning "" is the correct answer:
	// inventing text here would put a made-up reason in front of the operator.
	if got := errorTextOf(nil); got != "" {
		t.Fatalf("errorTextOf(nil) = %q, want an empty string", got)
	}
	if got := errorTextOf([]byte("")); got != "" {
		t.Fatalf("errorTextOf(empty) = %q, want an empty string", got)
	}
}

func TestErrorTypeOf(t *testing.T) {
	if got := errorTypeOf([]byte(`{"error":{"type":"ModelError"}}`)); got != "ModelError" {
		t.Fatalf("errorTypeOf = %q", got)
	}
	if got := errorTypeOf([]byte(`{"type":"AuthError"}`)); got != "AuthError" {
		t.Fatalf("errorTypeOf = %q, want the top-level type", got)
	}
	if got := errorTypeOf([]byte(`nonsense`)); got != "" {
		t.Fatalf("errorTypeOf(garbage) = %q, want empty", got)
	}
}

func TestCoreKindForMapping(t *testing.T) {
	cases := map[failureKind]core.FailureKind{
		kindAuth:      core.FailureAuth,
		kindQuota:     core.FailureQuota,
		kindRate:      core.FailureRateLimited,
		kindFreeTier:  core.FailureRateLimited,
		kindServer:    core.FailureUpstream,
		kindTransport: core.FailureUpstream,
		kindModel:     core.FailureOther,
		kindClient:    core.FailureOther,
		kindNone:      core.FailureOther,
	}
	for in, want := range cases {
		if got := coreKindFor(in); got != want {
			t.Fatalf("coreKindFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// classifyHTTP: the recording side.
// ---------------------------------------------------------------------------

func TestClassifyHTTPUnknownModelIsUnsupportedAndDoesNotPark(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyHTTP("chat", acct, 401,
		[]byte(`{"type":"error","error":{"type":"ModelError","message":"Model bogus is not supported"}}`))
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("error = %v, want it to wrap core.ErrUnsupported so the gateway answers 400", err)
	}
	after, _ := c.pool.byID("opencode:a")
	if !after.Enabled {
		t.Fatal("a model error disabled the account")
	}
	if after.CooldownUntil != "" || after.ErrCount != 0 {
		t.Fatalf("a model error recorded state on the account: %+v", after)
	}
}

func TestClassifyHTTPAuthErrorDisablesTheAccount(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyHTTP("chat", acct, 401,
		[]byte(`{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`))
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("error %v is not a *core.Failure", err)
	}
	if f.Kind != core.FailureAuth {
		t.Fatalf("kind = %q, want %q", f.Kind, core.FailureAuth)
	}
	if f.Status != 401 {
		t.Fatalf("status = %d, want 401", f.Status)
	}
	if f.Account != "opencode:a" {
		t.Fatalf("account = %q, want the account that failed", f.Account)
	}
	after, _ := c.pool.byID("opencode:a")
	if after.Enabled {
		t.Fatal("an AuthError left the account enabled")
	}
}

func TestClassifyHTTPQuotaGetsTheQuotaCooldown(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyHTTP("chat", acct, 402,
		[]byte(`{"type":"error","error":{"type":"PaymentError","message":"Insufficient credit"}}`))
	if kind := core.FailureKindOf(err); kind != core.FailureQuota {
		t.Fatalf("kind = %q, want %q", kind, core.FailureQuota)
	}
	after, _ := c.pool.byID("opencode:a")
	if after.CooldownKind != "quota" {
		t.Fatalf("CooldownKind = %q, want quota", after.CooldownKind)
	}
	if !after.Enabled {
		t.Fatal("running out of credit disabled the account; it should only be parked")
	}
}

// A closed free-tier gate is not a bad key.  The vendor answers 403, which the
// status fallback would read as an auth failure and disable an account that is
// merely waiting for the gate to reopen.
func TestClassifyHTTPFreeTierErrorParksInsteadOfDisabling(t *testing.T) {
	c := newTestClient(t, Config{Cooldown: Duration(20 * time.Minute)})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	body := []byte(`{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	err := c.classifyHTTP("chat", acct, 403, body)
	if kind := core.FailureKindOf(err); kind != core.FailureRateLimited {
		t.Fatalf("kind = %q, want %q so the caller retries instead of giving up", kind, core.FailureRateLimited)
	}
	after, _ := c.pool.byID("opencode:a")
	if !after.Enabled {
		t.Fatal("the free-tier gate disabled the account; it must only be parked")
	}
	if after.CooldownKind != "rate" {
		t.Fatalf("CooldownKind = %q, want rate", after.CooldownKind)
	}
	if after.CooldownUntil == "" {
		t.Fatal("the free-tier refusal left no cooldown, so the account would be retried in a tight loop")
	}
}

// A transport failure is not the vendor refusing anything.
func TestClassifyErrForTransportIsUpstream(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyErrFor(acct, "chat", errors.New("dial tcp: connection reset"))
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("error %v is not a *core.Failure", err)
	}
	if f.Kind != core.FailureUpstream {
		t.Fatalf("kind = %q, want %q", f.Kind, core.FailureUpstream)
	}
	after, _ := c.pool.byID("opencode:a")
	if !after.Enabled {
		t.Fatal("a connection reset disabled the account")
	}
	if after.ErrCount != 1 {
		t.Fatalf("ErrCount = %d, want the soft error recorded", after.ErrCount)
	}
}

// A cancelled context is the caller's decision, not the vendor's verdict.
func TestClassifyErrForPassesContextErrorsThrough(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyErrFor(acct, "chat", context.Canceled)
	if _, ok := core.AsFailure(err); ok {
		t.Fatalf("a context error was turned into a *core.Failure: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled passed through", err)
	}
	after, _ := c.pool.byID("opencode:a")
	if after.ErrCount != 0 {
		t.Fatalf("ErrCount = %d, want a cancellation to record nothing", after.ErrCount)
	}
}

func TestNoteSuccessClearsTheCooldown(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	c.pool.setCooldown("opencode:a", testNow.Add(time.Hour), "rate limit")
	acct, _ := c.pool.byID("opencode:a")

	c.noteSuccess(acct)
	after, _ := c.pool.byID("opencode:a")
	if after.CooldownUntil != "" {
		t.Fatalf("CooldownUntil = %q, want the success to clear it", after.CooldownUntil)
	}
}

// ---------------------------------------------------------------------------
// Secret hygiene.
// ---------------------------------------------------------------------------

// core.Redact only recognises a secret when it is dressed as one (a key= pair,
// a bearer token, a JWT).  scrubSecret is the second line of defence: it
// removes the exact key we hold, wherever it turns up.
func TestScrubSecretReplacesTheLiteralKey(t *testing.T) {
	const key = "sk-verysecretvalue"

	// A bare path segment: nothing here looks like a credential to Redact, so
	// only the literal replacement can save it.
	msg := `Get "https://opencode.ai/zen/v1/` + key + `/models": dial tcp: timeout`
	got := scrubSecret(msg, key)
	if strings.Contains(got, key) {
		t.Fatalf("scrubSecret left the key in place: %q", got)
	}
	if !strings.Contains(got, core.MaskSecret(key)) {
		t.Fatalf("scrubSecret = %q, want the masked form %q", got, core.MaskSecret(key))
	}

	// The credential-shaped spelling is caught by Redact before the literal
	// replacement runs; either way the key must not survive.
	dressed := `Post "https://opencode.ai/zen/v1/chat/completions?key=` + key + `": dial tcp: timeout`
	if got := scrubSecret(dressed, key); strings.Contains(got, key) {
		t.Fatalf("scrubSecret left a query-string key in place: %q", got)
	}
}

func TestScrubSecretRedactsCredentialShapedText(t *testing.T) {
	got := scrubSecret("Authorization: Bearer abcdefghijklmnop", "")
	if strings.Contains(got, "abcdefghijklmnop") {
		t.Fatalf("scrubSecret did not redact a bearer token: %q", got)
	}
}

func TestDescribeErrorRedactsAndTruncates(t *testing.T) {
	long := "api_key=" + strings.Repeat("x", 600)
	got := describeError(errors.New(long))
	if strings.Contains(got, strings.Repeat("x", 600)) {
		t.Fatalf("describeError did not redact the key: %q", got)
	}
	if len([]rune(got)) > 320 {
		t.Fatalf("describeError returned %d runes, want it truncated", len([]rune(got)))
	}
}

func TestUpstreamErrorRendering(t *testing.T) {
	e := &upstreamError{Op: "chat", Status: 503, Type: "ServerError", Msg: "upstream on fire"}
	got := e.Error()
	for _, want := range []string{clientName, "chat", "503", "ServerError", "upstream on fire"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Error() = %q, want it to contain %q", got, want)
		}
	}
}

func TestVerdictTextFallsBackGracefully(t *testing.T) {
	cases := []struct {
		v    verdict
		want string
	}{
		{verdict{typ: "AuthError", msg: "nope", status: 401}, "AuthError: nope"},
		{verdict{msg: "nope", status: 401}, "nope"},
		{verdict{typ: "AuthError", status: 401}, "AuthError"},
		{verdict{status: 401}, "HTTP 401"},
	}
	for _, tc := range cases {
		if got := tc.v.text(); got != tc.want {
			t.Fatalf("text() = %q, want %q", got, tc.want)
		}
	}
}

func TestContainsMarkerIsCaseInsensitive(t *testing.T) {
	if !containsMarker("RATE LIMIT exceeded", rateMarkers) {
		t.Fatal("containsMarker missed an upper-case marker")
	}
	if containsMarker("all good", rateMarkers) {
		t.Fatal("containsMarker matched an absent marker")
	}
	if containsMarker("anything", nil) {
		t.Fatal("containsMarker matched against an empty marker list")
	}
}

func TestNoteFailureIsSilentForRequestLevelVerdicts(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	c.noteFailure(acct, kindClient, "bad request")
	after, _ := c.pool.byID("opencode:a")
	if after.ErrCount != 0 || after.CooldownUntil != "" || !after.Enabled {
		t.Fatalf("a 400-shaped verdict touched the account: %+v", after)
	}
}

func TestNoteFailureUsesTheQuotaCooldownForCreditVerdicts(t *testing.T) {
	cfg := Config{QuotaCooldown: Duration(30 * time.Minute)}
	c := newTestClient(t, cfg)
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	c.noteFailure(acct, kindQuota, "out of credit")
	after, _ := c.pool.byID("opencode:a")
	until, err := time.Parse(time.RFC3339, after.CooldownUntil)
	if err != nil {
		t.Fatalf("CooldownUntil = %q is not RFC3339: %v", after.CooldownUntil, err)
	}
	if got := until.Sub(testNow); got != 30*time.Minute {
		t.Fatalf("quota cooldown = %s, want the configured 30m", got)
	}
}
