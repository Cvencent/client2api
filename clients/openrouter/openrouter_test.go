package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- test doubles ---------------------------------------------------------

// reply is what the fake upstream answers with for one request.
type reply struct {
	status int
	body   string
	err    error
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeUpstream records every request it is handed and answers from the test's
// handler.  It is a RoundTripper rather than an httptest.Server so a test can
// assert on the exact headers and body without opening a socket.
type fakeUpstream struct {
	mu     sync.Mutex
	reqs   []*http.Request
	bodies []string
	handle func(req *http.Request, body string) reply
}

func (f *fakeUpstream) transport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var raw []byte
		if r.Body != nil {
			raw, _ = io.ReadAll(r.Body)
			_ = r.Body.Close()
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.bodies = append(f.bodies, string(raw))
		h := f.handle
		f.mu.Unlock()

		out := reply{status: http.StatusOK}
		if h != nil {
			out = h(r, string(raw))
		}
		if out.err != nil {
			return nil, out.err
		}
		if out.status == 0 {
			out.status = http.StatusOK
		}
		return &http.Response{
			StatusCode: out.status,
			Status:     fmt.Sprintf("%d %s", out.status, http.StatusText(out.status)),
			Proto:      "HTTP/1.1",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(out.body)),
			Request:    r,
		}, nil
	})
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeUpstream) requestAt(i int) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.reqs) {
		return nil
	}
	return f.reqs[i]
}

func (f *fakeUpstream) bodyAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.bodies) {
		return ""
	}
	return f.bodies[i]
}

func (f *fakeUpstream) jsonAt(t *testing.T, i int) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(f.bodyAt(i)), &out); err != nil {
		t.Fatalf("request %d body is not a JSON object: %v (%q)", i, err, f.bodyAt(i))
	}
	return out
}

// --- fixtures -------------------------------------------------------------

// testBase pins the clock so cooldown arithmetic is deterministic.
var testBase = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)

const (
	testKey  = "sk-or-v1-0123456789abcdef0123456789abcdef"
	otherKey = "sk-or-v1-fedcba9876543210fedcba9876543210"
)

// newTestClient builds a real Client over a fake transport.
//
// OPENROUTER_API_KEY is cleared first: the developer's own environment must
// never decide what a test sees.
func newTestClient(t *testing.T, cfg string, up *fakeUpstream) *Client {
	t.Helper()
	t.Setenv(envAPIKey, "")
	if up == nil {
		up = &fakeUpstream{}
	}
	cl, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(cfg),
		HTTPClient: &http.Client{Transport: up.transport()},
	})
	if err != nil {
		t.Fatalf("New() = %v, want a usable client", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New() returned %T, want *Client", cl)
	}
	c.now = func() time.Time { return testBase }
	return c
}

// freeOnlyOff is the fragment tests add when they exercise something other
// than the free-only policy.  `free_only` defaults to ON, so a test that
// chats with an arbitrary id has to opt out explicitly; the policy itself is
// covered by TestFreeOnly* in models_test.go.
const freeOnlyOff = `"free_only":false`

func cfgWithKey(key string) string {
	return fmt.Sprintf(`{"api_key":%q,%s}`, key, freeOnlyOff)
}

func twoKeysCfg() string {
	return fmt.Sprintf(`{"free_only":false,"accounts":[{"api_key":%q,"label":"one"},{"api_key":%q,"label":"two"}]}`, testKey, otherKey)
}

// mustByID is the test-side lookup: a missing id is a bug in the test.
func (c *Client) mustByID(t *testing.T, id string) accountRecord {
	t.Helper()
	rec, ok := c.pool.byID(id)
	if !ok {
		t.Fatalf("account %q is not in the pool", id)
	}
	return rec
}

// --- registration and capabilities ---------------------------------------

func TestClientNamesItself(t *testing.T) {
	c := newTestClient(t, "", nil)
	if got := c.Name(); got != clientName {
		t.Fatalf("Name() = %q, want %q", got, clientName)
	}
	if clientName != "openrouter" {
		t.Fatalf("the registered name must stay %q, it is the routing prefix", "openrouter")
	}
}

func TestCapabilitiesMatchImplementedInterfaces(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Manage || !caps.Import || !caps.Refresh || !caps.Balance {
		t.Fatalf("capabilities = %+v, want manage/import/refresh/balance", caps)
	}
	if len(caps.Fields) == 0 {
		t.Fatal("AccountFields returned nothing, so the panel hides the add form")
	}
	if caps.Fields[0].Key != "api_key" || !caps.Fields[0].Required {
		t.Fatalf("first field = %+v, want a required api_key", caps.Fields[0])
	}
	if !caps.Login {
		t.Fatalf("capabilities = %+v, want the PKCE login surface", caps)
	}
	if caps.Checkin || caps.Tasks || caps.Bundle || caps.Vouchers ||
		caps.Packages || caps.Degrade || caps.Revive || caps.Conversations ||
		caps.Captcha || caps.Batches {
		t.Fatalf("the module advertises a surface it does not implement: %+v", caps)
	}
}

// --- config ---------------------------------------------------------------

func TestParseConfigAcceptsAbsentAndNull(t *testing.T) {
	for _, raw := range []string{"", "null"} {
		cfg, err := parseConfig(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("parseConfig(%q) = %v, want no error", raw, err)
		}
		if cfg.base() != defaultBaseURL {
			t.Fatalf("base = %q, want %q", cfg.base(), defaultBaseURL)
		}
		if cfg.maxInFlight() != defaultInFlight {
			t.Fatalf("maxInFlight = %d, want %d", cfg.maxInFlight(), defaultInFlight)
		}
		if cfg.Referer != defaultReferer || cfg.Title != defaultTitle || cfg.UserAgent != defaultAgent {
			t.Fatalf("attribution defaults missing: %+v", cfg)
		}
		if cfg.usesLegacyMaxTokens() {
			t.Fatal("the deprecated max_tokens spelling must not be the default")
		}
		if cfg.testModel() != "openai/gpt-4o-mini" {
			t.Fatalf("testModel = %q", cfg.testModel())
		}
	}
}

func TestParseConfigRejectsBrokenJSON(t *testing.T) {
	_, err := parseConfig(json.RawMessage(`{"api_key":`))
	if err == nil {
		t.Fatal("a truncated object must be reported")
	}
	if !strings.Contains(err.Error(), "clients.openrouter") {
		t.Fatalf("error %q does not name the config key an operator must fix", err)
	}
	// A whitespace-only value is NOT the "absent" case: json.Unmarshal rejects it.
	if _, err := parseConfig(json.RawMessage("   ")); err == nil {
		t.Fatal("whitespace is not valid JSON and must be reported")
	}
}

func TestNewSurvivesABrokenConfig(t *testing.T) {
	t.Setenv(envAPIKey, "")
	cl, err := New(core.Deps{
		DataDir: t.TempDir(),
		Config:  json.RawMessage(`{"max_in_flight":"nope"}`),
	})
	if err != nil {
		t.Fatalf("New() = %v, want a client that still builds", err)
	}
	c := cl.(*Client)
	if c.cfgErr == nil {
		t.Fatal("the decode failure was not recorded")
	}
	if c.cfg.maxInFlight() != defaultInFlight {
		t.Fatalf("maxInFlight = %d, want the default after a bad config", c.cfg.maxInFlight())
	}
	st := c.Status(context.Background())
	if !strings.Contains(st.Detail, "config error") {
		t.Fatalf("Detail = %q, want the config error surfaced", st.Detail)
	}
}

func TestConfigNormalizeDefaultsAndDurations(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"base_url":" https://example.test/v1/ ","chat_timeout":"nonsense","stream_idle_timeout":"-5s","models_ttl":"0","cooldown":"45s","max_in_flight":-2}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.base() != "https://example.test/v1" {
		t.Fatalf("base = %q, want the trailing slash trimmed", cfg.base())
	}
	if cfg.chatURL() != "https://example.test/v1/chat/completions" {
		t.Fatalf("chatURL = %q", cfg.chatURL())
	}
	if cfg.chatTimeout() != 10*time.Minute {
		t.Fatalf("an unparseable duration must fall back, got %s", cfg.chatTimeout())
	}
	if cfg.streamIdle() != 90*time.Second {
		t.Fatalf("a negative duration must fall back, got %s", cfg.streamIdle())
	}
	if cfg.modelsTTL() != 10*time.Minute {
		t.Fatalf("a zero duration must fall back, got %s", cfg.modelsTTL())
	}
	if cfg.cooldown() != 45*time.Second {
		t.Fatalf("cooldown = %s, want the configured 45s", cfg.cooldown())
	}
	if cfg.MaxInFlight != 0 || cfg.maxInFlight() != defaultInFlight {
		t.Fatalf("MaxInFlight = %d / maxInFlight() = %d", cfg.MaxInFlight, cfg.maxInFlight())
	}
}

func TestDurationOrTreatsNonPositiveAsUnset(t *testing.T) {
	if got := durationOr("", time.Minute); got != time.Minute {
		t.Fatalf("empty = %s", got)
	}
	if got := durationOr("0", time.Minute); got != time.Minute {
		t.Fatalf("zero = %s, want the default (a zero timeout disables the guard)", got)
	}
	if got := durationOr("-1s", time.Minute); got != time.Minute {
		t.Fatalf("negative = %s", got)
	}
	if got := durationOr("250ms", time.Minute); got != 250*time.Millisecond {
		t.Fatalf("250ms = %s", got)
	}
}

func TestConfigNormalizeDropsReservedHeaders(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"extra_headers":{"Authorization":"Bearer evil","X-Title":"evil","user-agent":"evil","http-referer":"evil","Host":"evil","X-Custom":"ok","":"blank","X-Empty":"   "}}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.ExtraHeaders) != 1 {
		t.Fatalf("ExtraHeaders = %v, want only X-Custom to survive", cfg.ExtraHeaders)
	}
	if cfg.ExtraHeaders["X-Custom"] != "ok" {
		t.Fatalf("ExtraHeaders = %v", cfg.ExtraHeaders)
	}
}

func TestConfigNormalizeDedupesAccountsByKey(t *testing.T) {
	raw := fmt.Sprintf(`{"accounts":[{"api_key":%q,"label":"a"},{"api_key":"  %s  ","label":"b"},{"api_key":"   ","label":"c"},{"api_key":%q,"label":"d"}]}`, testKey, testKey, otherKey)
	cfg, err := parseConfig(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("Accounts = %+v, want the duplicate and the empty key dropped", cfg.Accounts)
	}
	if cfg.Accounts[0].Label != "a" || cfg.Accounts[0].APIKey != testKey {
		t.Fatalf("the first occurrence must win: %+v", cfg.Accounts[0])
	}
}

func TestConfigNormalizeMaxTokensField(t *testing.T) {
	for _, v := range []string{"", "max_completion_tokens", "max_token", "bogus"} {
		cfg, err := parseConfig(json.RawMessage(fmt.Sprintf(`{"max_tokens_field":%q}`, v)))
		if err != nil {
			t.Fatalf("parseConfig(%q): %v", v, err)
		}
		if cfg.usesLegacyMaxTokens() {
			t.Fatalf("%q switched the module to the deprecated field", v)
		}
	}
	cfg, _ := parseConfig(json.RawMessage(`{"max_tokens_field":"MAX_TOKENS"}`))
	if !cfg.usesLegacyMaxTokens() {
		t.Fatal("the spelling check must be case-insensitive")
	}
	cfg, _ = parseConfig(json.RawMessage(`{"max_tokens_field":"max_tokens"}`))
	if !cfg.usesLegacyMaxTokens() {
		t.Fatal("an explicit max_tokens must be honoured")
	}
}

func TestConfigURLBuilders(t *testing.T) {
	cfg, _ := parseConfig(nil)
	cases := map[string]string{
		cfg.chatURL():    "https://openrouter.ai/api/v1/chat/completions",
		cfg.modelsURL():  "https://openrouter.ai/api/v1/models",
		cfg.creditsURL(): "https://openrouter.ai/api/v1/credits",
		cfg.keyURL():     "https://openrouter.ai/api/v1/key",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("URL = %q, want %q", got, want)
		}
	}
}

func TestConfigApplyHeaders(t *testing.T) {
	cfg, _ := parseConfig(json.RawMessage(`{"http_referer":"https://me.test","x_title":"my-app","extra_headers":{"X-Custom":"1"}}`))
	req, _ := http.NewRequest(http.MethodPost, cfg.chatURL(), nil)
	cfg.applyHeaders(req, testKey)
	if got := req.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://me.test" {
		t.Fatalf("HTTP-Referer = %q", got)
	}
	if got := req.Header.Get("X-Title"); got != "my-app" {
		t.Fatalf("X-Title = %q", got)
	}
	if got := req.Header.Get("X-Custom"); got != "1" {
		t.Fatalf("X-Custom = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "application/json" {
		t.Fatalf("Accept = %q", got)
	}

	// The public catalogue call must stay anonymous: no empty bearer header.
	anon, _ := http.NewRequest(http.MethodGet, cfg.modelsURL(), nil)
	cfg.applyHeaders(anon, "")
	if got := anon.Header.Get("Authorization"); got != "" {
		t.Fatalf("an unauthenticated call carried Authorization: %q", got)
	}
}

// --- pool ----------------------------------------------------------------

func TestPoolAcquireWithoutAnyCredential(t *testing.T) {
	c := newTestClient(t, "", nil)
	_, err := c.pool.acquire(c.now(), c.limit())
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured (the gateway answers 503)", err)
	}
	if !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Fatalf("err = %v, want it to name the environment variable", err)
	}
}

func TestPoolAcquireWhenEveryCredentialIsDisabled(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"accounts":[{"api_key":%q,"disabled":true}]}`, testKey), nil)
	_, err := c.pool.acquire(c.now(), c.limit())
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("err = %v, want it to say the credential is disabled", err)
	}
}

func TestPoolAcquireReportsBusyWhileCoolingDown(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	c.pool.setCooldown(id, c.now().Add(time.Minute), "429 rate limited")
	_, err := c.pool.acquire(c.now(), c.limit())
	if !errors.Is(err, core.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy (the gateway answers 429)", err)
	}

	// The same account is selectable again the moment the park expires.
	later := c.now().Add(2 * time.Minute)
	if _, err := c.pool.acquire(later, c.limit()); err != nil {
		t.Fatalf("after the cooldown: %v", err)
	}
}

func TestPoolAcquireHonoursTheInFlightCeiling(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"max_in_flight":1}`, testKey), nil)
	if c.limit() != 1 {
		t.Fatalf("limit = %d, want 1", c.limit())
	}
	rec, err := c.pool.acquire(c.now(), c.limit())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second acquire = %v, want ErrBusy", err)
	}
	c.pool.release(rec.ID)
	if _, err := c.pool.acquire(c.now(), c.limit()); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestPoolAcquireRotatesLeastRecentlyUsed(t *testing.T) {
	c := newTestClient(t, twoKeysCfg(), nil)
	first, err := c.pool.acquire(c.now(), c.limit())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := c.pool.acquire(c.now(), c.limit())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("both requests went to %s; rotation is not happening", first.ID)
	}
	third, err := c.pool.acquire(c.now(), c.limit())
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	if third.ID != first.ID {
		t.Fatalf("third went to %s, want the least recently used %s", third.ID, first.ID)
	}
}

func TestPoolReleaseNeverGoesNegative(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	c.pool.release(id)
	c.pool.release("openrouter:does-not-exist")
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 0 {
		t.Fatalf("in-flight = %d, want 0", inFlight)
	}
}

func TestPoolNoteErrorParksAtTheThreshold(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	for i := 1; i < softErrorThreshold; i++ {
		c.pool.noteError(id, "boom", c.now(), softErrorThreshold, time.Minute)
		rec := c.mustByID(t, id)
		if got := c.pool.stateOf(&rec, c.now()); got != "ready" {
			t.Fatalf("after %d failure(s) state = %q, want ready", i, got)
		}
	}
	c.pool.noteError(id, "boom", c.now(), softErrorThreshold, time.Minute)
	parked := c.mustByID(t, id)
	if got := c.pool.stateOf(&parked, c.now()); got != "cooling" {
		t.Fatalf("after %d failures state = %q, want cooling", softErrorThreshold, got)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("a parked credential must not be selectable, got %v", err)
	}
}

func TestPoolReEnableClearsTheVerdict(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	c.pool.setCooldown(id, c.now().Add(time.Hour), "429")
	c.pool.markInvalid(id, true)
	if !c.pool.setEnabled(id, false) {
		t.Fatal("setEnabled returned false for a known account")
	}
	off := c.mustByID(t, id)
	if got := c.pool.stateOf(&off, c.now()); got != "invalid" {
		t.Fatalf("disabled state = %q, want invalid", got)
	}
	if !c.pool.setEnabled(id, true) {
		t.Fatal("re-enabling returned false")
	}
	rec := c.mustByID(t, id)
	if rec.CooldownUntil != "" || rec.Invalid || rec.ErrCount != 0 || rec.LastError != "" {
		t.Fatalf("re-enabling must clear the verdict, got %+v", rec)
	}
	if got := c.pool.stateOf(&rec, c.now()); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
}

func TestPoolStateOf(t *testing.T) {
	c := newTestClient(t, twoKeysCfg(), nil)
	ready := c.mustByID(t, accountIDFor(testKey))
	if got := c.pool.stateOf(&ready, c.now()); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
	parked := ready
	parked.CooldownUntil = c.now().Add(time.Minute).Format(time.RFC3339)
	if got := c.pool.stateOf(&parked, c.now()); got != "cooling" {
		t.Fatalf("state = %q, want cooling", got)
	}
	expired := ready
	expired.CooldownUntil = c.now().Add(-time.Minute).Format(time.RFC3339)
	if got := c.pool.stateOf(&expired, c.now()); got != "ready" {
		t.Fatalf("an expired park must read ready, got %q", got)
	}
	dead := ready
	dead.Invalid = true
	dead.CooldownUntil = c.now().Add(time.Minute).Format(time.RFC3339)
	if got := c.pool.stateOf(&dead, c.now()); got != "invalid" {
		t.Fatalf("state = %q, want invalid", got)
	}
	disabled := ready
	disabled.Disabled = true
	if got := c.pool.stateOf(&disabled, c.now()); got != "invalid" {
		t.Fatalf("state = %q, want invalid", got)
	}
	empty := ready
	empty.APIKey = ""
	if got := c.pool.stateOf(&empty, c.now()); got != "invalid" {
		t.Fatalf("a record with no key must read invalid, got %q", got)
	}
}

func TestPoolRecordsNeverRenderTheKey(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"accounts":[{"api_key":%q,"label":%q}]}`, testKey, testKey), nil)
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("len(Accounts) = %d, want 1", len(recs))
	}
	blob, err := json.Marshal(recs)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), testKey) {
		t.Fatalf("the account table leaks the key:\n%s", blob)
	}
	if recs[0].Label == testKey || recs[0].Label == "" {
		t.Fatalf("Label = %q, want a masked form", recs[0].Label)
	}
}

func TestPoolStatusesNeverRenderTheKey(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	id := accountIDFor(testKey)
	c.pool.setNote(id, "auth failed for "+testKey)
	st := c.Status(context.Background())
	blob, _ := json.Marshal(st)
	if strings.Contains(string(blob), testKey) {
		t.Fatalf("Status leaks the key:\n%s", blob)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "ready" {
		t.Fatalf("Accounts = %+v", st.Accounts)
	}
}

func TestSummaryLine(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"accounts":[{"api_key":%q},{"api_key":%q,"disabled":true}],"max_in_flight":3}`, testKey, otherKey), nil)
	got := c.pool.summary(c.now(), c.limit())
	want := "2 credential(s): 1 ready, 0 parked, 1 disabled; max_in_flight=3"
	if got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	empty := newTestClient(t, "", nil)
	if got := empty.pool.summary(empty.now(), empty.limit()); got != "no credential configured" {
		t.Fatalf("empty summary = %q", got)
	}
}

// --- credentials ----------------------------------------------------------

func TestStateFileHasNoKeyFieldAndConfigKeysAreNotCopied(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	c.pool.setNote(accountIDFor(testKey), "seen")
	c.persist()

	state, err := os.ReadFile(c.statePath())
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	if strings.Contains(string(state), testKey) {
		t.Fatalf("state.json leaks the key:\n%s", state)
	}
	if strings.Contains(string(state), `"api_key"`) {
		t.Fatalf("state.json has a key field at all:\n%s", state)
	}
	// A config-supplied key is already durable in the config file; copying it
	// into the data directory would duplicate a secret for no benefit.
	if _, err := os.Stat(c.credentialsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials.json exists for a config-only key (err = %v)", err)
	}
}

func TestCredentialsRoundTripThroughTheStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	in := []credentialRecord{{
		ID:      accountIDFor(testKey),
		Label:   "imported",
		APIKey:  testKey,
		Source:  sourceImported,
		AddedAt: testBase.Format(time.RFC3339),
	}}
	if err := saveCredentials(path, in); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}
	out := loadCredentials(path)
	if len(out) != 1 {
		t.Fatalf("loadCredentials = %+v", out)
	}
	if out[0].APIKey != testKey || out[0].Source != sourceImported || out[0].ID != in[0].ID {
		t.Fatalf("round trip = %+v", out[0])
	}
	// Saving an empty store must remove the file rather than leave a stub.
	if err := saveCredentials(path, nil); err != nil {
		t.Fatalf("saveCredentials(nil): %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an empty store must be removed, stat err = %v", err)
	}
}

func TestLoadCredentialsIgnoresAStraySource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	blob := fmt.Sprintf(`{"version":1,"accounts":[{"id":"x","label":"l","api_key":%q,"source":"config"},{"id":"y","label":"m","api_key":""}]}`, testKey)
	if err := os.WriteFile(path, []byte(blob), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out := loadCredentials(path)
	if len(out) != 1 {
		t.Fatalf("loadCredentials = %+v, want the empty-key row dropped", out)
	}
	if out[0].Source != sourceImported {
		t.Fatalf("source = %q, want a row the module does not own re-labelled imported", out[0].Source)
	}
}

func TestBuildCredentialsPrecedence(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	t.Setenv(envAPIKey, otherKey)

	thirdKey := "sk-or-v1-abcdefabcdefabcdefabcdefabcdefab"
	if err := saveCredentials(c.credentialsPath(), []credentialRecord{{
		ID: accountIDFor(thirdKey), Label: "imported", APIKey: thirdKey, Source: sourceImported,
	}}); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}
	c.pool.reload(c.buildCredentials())

	bySource := map[string]string{}
	for _, r := range c.pool.snapshot() {
		bySource[r.Source] = r.APIKey
	}
	if bySource[sourceConfig] != testKey {
		t.Fatalf("config source = %q", bySource[sourceConfig])
	}
	if bySource[sourceEnv] != otherKey {
		t.Fatalf("env source = %q", bySource[sourceEnv])
	}
	if bySource[sourceImported] != thirdKey {
		t.Fatalf("imported source = %q", bySource[sourceImported])
	}
	if c.pool.len() != 3 {
		t.Fatalf("pool = %d, want 3", c.pool.len())
	}

	// The same key in config AND environment is ONE account: it is the same
	// credential however it was supplied.
	c2 := newTestClient(t, cfgWithKey(testKey), nil)
	t.Setenv(envAPIKey, testKey)
	c2.pool.reload(c2.buildCredentials())
	if c2.pool.len() != 1 {
		t.Fatalf("pool = %d, want the duplicate collapsed", c2.pool.len())
	}
	if rec, _ := c2.pool.byID(accountIDFor(testKey)); rec.Source != sourceConfig {
		t.Fatalf("source = %q, want the highest precedence (config)", rec.Source)
	}
}

func TestKeyFingerprintAndAccountID(t *testing.T) {
	fp := keyFingerprint(testKey)
	if fp == "" || strings.Contains(fp, testKey) {
		t.Fatalf("keyFingerprint = %q, want a short opaque digest", fp)
	}
	if keyFingerprint(testKey) != fp {
		t.Fatal("keyFingerprint is not stable")
	}
	if keyFingerprint(testKey) == keyFingerprint(otherKey) {
		t.Fatal("two different keys share a fingerprint")
	}
	if got := accountIDFor(testKey); got != "openrouter:"+fp {
		t.Fatalf("accountIDFor = %q", got)
	}
	if strings.Contains(accountIDFor(testKey), testKey) {
		t.Fatal("the account id must not embed the key")
	}
}

func TestLooksLikeAPIKey(t *testing.T) {
	cases := map[string]bool{
		testKey:                      true,
		"sk-or-v1-short":             true, // the vendor prefix alone is enough to refuse to render it
		"":                           false,
		"configured key":             false,
		"sk-live-0123456789abcdef":   true, // 24 chars, no spaces: indistinguishable from a key, so mask it
		"abcdefghijklmnopqrstuvwxyz": true,
		"has a space in the middle":  false,
	}
	for in, want := range cases {
		if got := looksLikeAPIKey(in); got != want {
			t.Fatalf("looksLikeAPIKey(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestParseCredentialFileShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bare string", fmt.Sprintf("%q", testKey), 1},
		{"named field", fmt.Sprintf(`{"api_key":%q}`, testKey), 1},
		{"camel case field", fmt.Sprintf(`{"apiKey":%q}`, testKey), 1},
		{"nested data", fmt.Sprintf(`{"data":{"key":%q}}`, testKey), 1},
		{"list of objects", fmt.Sprintf(`{"accounts":[{"api_key":%q},{"api_key":%q}]}`, testKey, otherKey), 2},
		{"plain text", "OPENROUTER_API_KEY=" + testKey + "\n", 1},
		{"two plain keys", testKey + "\n" + otherKey + "\n", 2},
		{"nothing", `{"hello":"world"}`, 0},
		{"garbage", "not json at all", 0},
		{"repeated", fmt.Sprintf(`{"a":%q,"b":%q}`, testKey, testKey), 1},
	}
	for _, tc := range cases {
		got := parseCredentialFile([]byte(tc.body))
		if len(got) != tc.want {
			t.Fatalf("%s: parseCredentialFile = %+v, want %d key(s)", tc.name, got, tc.want)
		}
		for _, k := range got {
			if !looksLikeAPIKey(k.Key) {
				t.Fatalf("%s: accepted a non-key %q", tc.name, k.Key)
			}
		}
	}
}

func TestReadCredentialSourceFromEnv(t *testing.T) {
	t.Setenv("OPENROUTER_TEST_KEY", testKey)
	keys, err := readCredentialSource("env:OPENROUTER_TEST_KEY")
	if err != nil {
		t.Fatalf("readCredentialSource: %v", err)
	}
	if len(keys) != 1 || keys[0].Key != testKey {
		t.Fatalf("keys = %+v", keys)
	}
	if _, err := readCredentialSource("env:OPENROUTER_TEST_KEY_MISSING"); err == nil {
		t.Fatal("an unset environment variable must be reported")
	}
	if _, err := readCredentialSource(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("a missing file must be reported")
	}
}

func TestImportEmptyRequestIsANoOp(t *testing.T) {
	c := newTestClient(t, "", nil)
	out, err := c.Import(context.Background(), nil, false)
	if err != nil || out != nil {
		t.Fatalf("Import(nil,false) = (%v, %v), want (nil, nil)", out, err)
	}
	if _, err := os.Stat(c.credentialsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a no-op import must not write a file")
	}
}

func TestImportFromAFileThenDedupes(t *testing.T) {
	c := newTestClient(t, "", nil)
	path := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"api_key":%q,"label":"mine"}`, testKey)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := c.Import(context.Background(), []string{path}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(out) != 1 || out[0].Label != "mine" {
		t.Fatalf("Import = %+v", out)
	}
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), testKey) {
		t.Fatalf("Import returned the key itself:\n%s", blob)
	}
	if c.pool.len() != 1 {
		t.Fatalf("pool = %d, want 1", c.pool.len())
	}
	stored := loadCredentials(c.credentialsPath())
	if len(stored) != 1 || stored[0].APIKey != testKey || stored[0].Source != sourceImported {
		t.Fatalf("stored = %+v", stored)
	}

	// Importing the same file again reports the existing row, not a duplicate.
	again, err := c.Import(context.Background(), []string{path}, false)
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if len(again) != 1 || c.pool.len() != 1 {
		t.Fatalf("second Import = %+v, pool = %d", again, c.pool.len())
	}
}

func TestImportNamedPathWithNoKeyIsAnError(t *testing.T) {
	c := newTestClient(t, "", nil)
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte(`{"hello":"world"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := c.Import(context.Background(), []string{path}, false); err == nil {
		t.Fatal("an explicitly named path with no key must be an error, not a silent success")
	}
}

func TestDiscoverAlwaysSucceeds(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(found) < 2 {
		t.Fatalf("Discover = %+v, want the store and the environment reported", found)
	}
	var sawStore, sawEnv bool
	for _, d := range found {
		if strings.Contains(d.Note, testKey) {
			t.Fatalf("Discover leaked a key in Note: %+v", d)
		}
		if d.Path == c.credentialsPath() {
			sawStore = true
		}
		if d.Path == envCredentialPath {
			sawEnv = true
			if d.Importable {
				t.Fatal("an unset environment variable must not be importable")
			}
		}
	}
	if !sawStore || !sawEnv {
		t.Fatalf("Discover = %+v, want both the store and the env candidate", found)
	}
}

// --- status, health, live knobs ------------------------------------------

func TestStatusIsOfflineAndReady(t *testing.T) {
	up := &fakeUpstream{}
	c := newTestClient(t, cfgWithKey(testKey), up)
	st := c.Status(context.Background())
	if up.count() != 0 {
		t.Fatalf("Status made %d network call(s); the panel polls it every 10s", up.count())
	}
	if st.Name != clientName {
		t.Fatalf("Name = %q", st.Name)
	}
	if !st.Ready {
		t.Fatal("a configured key must make the module ready")
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "ready" || !st.Accounts[0].Enabled {
		t.Fatalf("Accounts = %+v", st.Accounts)
	}
	if len(st.Models) == 0 {
		t.Fatal("Status must still list the fallback catalogue")
	}
	if !strings.Contains(st.Detail, "1 credential(s)") {
		t.Fatalf("Detail = %q", st.Detail)
	}
}

func TestStatusWithoutCredentialIsNotReady(t *testing.T) {
	c := newTestClient(t, "", nil)
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("a module with no credential must not claim to be ready")
	}
	if len(st.Accounts) != 0 {
		t.Fatalf("Accounts = %+v", st.Accounts)
	}
	if !strings.Contains(st.Detail, "no credential configured") {
		t.Fatalf("Detail = %q", st.Detail)
	}
}

func TestHealthCountsEveryState(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"accounts":[{"api_key":%q},{"api_key":%q,"disabled":true}]}`, testKey, otherKey), nil)
	h := c.Health()
	if h.Total != 2 || h.Ready != 1 || h.Disabled != 1 || h.Cooling != 0 {
		t.Fatalf("health = %+v", h)
	}
	if !h.Servable {
		t.Fatal("one healthy credential makes the pool servable")
	}
	if !strings.Contains(h.Note, "1 of 2") {
		t.Fatalf("Note = %q", h.Note)
	}

	// A pool whose only healthy credential is at its ceiling is NOT servable:
	// that is the whole reason Health exists next to Status.
	c2 := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"max_in_flight":1}`, testKey), nil)
	if _, err := c2.pool.acquire(c2.now(), c2.limit()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h2 := c2.Health(); h2.Servable {
		t.Fatalf("a fully-saturated pool reported servable: %+v", h2)
	} else if h2.Ready != 1 {
		t.Fatalf("the credential itself is still healthy: %+v", h2)
	}

	c3 := newTestClient(t, "", nil)
	if h3 := c3.Health(); h3.Servable || h3.Note != "no OpenRouter credential configured" {
		t.Fatalf("empty health = %+v", h3)
	}
}

func TestPoolStatsReportInFlight(t *testing.T) {
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"max_in_flight":2}`, testKey), nil)
	if got := c.PoolStats(); got.InFlight != 0 || got.InFlightFull != 0 {
		t.Fatalf("PoolStats = %+v", got)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := c.PoolStats(); got.InFlight != 1 || got.InFlightFull != 0 {
		t.Fatalf("PoolStats = %+v", got)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := c.PoolStats(); got.InFlight != 2 || got.InFlightFull != 1 {
		t.Fatalf("PoolStats = %+v, want the account counted as full", got)
	}
}

func TestApplyLiveOverridesTheCeilings(t *testing.T) {
	c := newTestClient(t, `{"max_in_flight":2,"cooldown":"30s"}`, nil)
	if c.limit() != 2 || c.cooldown() != 30*time.Second {
		t.Fatalf("limit = %d, cooldown = %s", c.limit(), c.cooldown())
	}
	if c.parkThreshold() != softErrorThreshold {
		t.Fatalf("parkThreshold = %d", c.parkThreshold())
	}

	one := 1
	threshold := 7
	park := 5 * time.Minute
	c.ApplyLive(core.LiveSettings{
		MaxInFlight: &one,
		Pool:        &core.PoolTuning{BreakerThreshold: &threshold, BreakerCooldown: &park},
	})
	if c.limit() != 1 {
		t.Fatalf("limit = %d, want the live value", c.limit())
	}
	if c.parkThreshold() != 7 {
		t.Fatalf("parkThreshold = %d, want the live value", c.parkThreshold())
	}
	if c.cooldown() != 5*time.Minute {
		t.Fatalf("cooldown = %s, want the live value", c.cooldown())
	}

	// An explicit zero is a VALUE ("no ceiling"), not "the file said nothing".
	zero := 0
	c.ApplyLive(core.LiveSettings{MaxInFlight: &zero})
	if c.limit() != 0 {
		t.Fatalf("limit = %d, want 0 to mean no ceiling", c.limit())
	}
	// An unrelated live setting must not disturb what is already set.
	c.ApplyLive(core.LiveSettings{})
	if c.limit() != 0 || c.parkThreshold() != 7 {
		t.Fatalf("limit = %d, threshold = %d", c.limit(), c.parkThreshold())
	}
}

// --- helpers --------------------------------------------------------------

func TestTruncateIsRuneAware(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("héllo", 3); got != "hél…" {
		t.Fatalf("truncate = %q, want the cut marked", got)
	}
	if got := truncate("额度不足", 2); got != "额度…" {
		t.Fatalf("truncate = %q, want whole runes (vendor messages are Chinese)", got)
	}
	if got := truncate("abc", 0); got != "" {
		t.Fatalf("truncate = %q", got)
	}
}

func TestHelpersCoerceJSONValues(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", "  "); got != "" {
		t.Fatalf("firstNonEmpty = %q", got)
	}

	if v, ok := toInt(json.Number("7")); !ok || v != 7 {
		t.Fatalf("toInt(json.Number(7)) = (%d, %v)", v, ok)
	}
	if v, ok := toInt("12"); !ok || v != 12 {
		t.Fatalf("toInt(\"12\") = (%d, %v)", v, ok)
	}
	if v, ok := toInt(3.9); !ok || v != 3 {
		t.Fatalf("toInt(3.9) = (%d, %v)", v, ok)
	}
	if v, ok := toInt(true); !ok || v != 1 {
		t.Fatalf("toInt(true) = (%d, %v)", v, ok)
	}
	if _, ok := toInt(nil); ok {
		t.Fatal("toInt(nil) must report 'the vendor said nothing'")
	}
	if _, ok := toInt("abc"); ok {
		t.Fatal("toInt(\"abc\") must report 'no number'")
	}

	if got := asString(nil); got != "" {
		t.Fatalf("asString(nil) = %q", got)
	}
	if got := asString("x"); got != "x" {
		t.Fatalf("asString(\"x\") = %q", got)
	}
	if got := asString(json.Number("1.5")); got != "1.5" {
		t.Fatalf("asString(json.Number) = %q", got)
	}

	m := map[string]any{"b": "2", "a": "1"}
	if got := firstString(m, "a", "b"); got != "1" {
		t.Fatalf("firstString = %q", got)
	}
	if got := firstString(m, "zzz"); got != "" {
		t.Fatalf("firstString(missing) = %q", got)
	}
	if got := objectOf([]any{m}); len(got) != 2 {
		t.Fatalf("objectOf(list) = %v", got)
	}
	if got := objectOf("nope"); got != nil {
		t.Fatalf("objectOf(string) = %v", got)
	}
	if !containsString([]string{"a", "b"}, "b") || containsString([]string{"a"}, "b") {
		t.Fatal("containsString is wrong")
	}
	if got := readLimited(nil, 10); got != nil {
		t.Fatalf("readLimited(nil) = %q", got)
	}
	if got := readLimited(strings.NewReader("abcdef"), 3); string(got) != "abc" {
		t.Fatalf("readLimited = %q", got)
	}
}
