package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/alerts"
	"client2api/internal/core"
	"client2api/internal/livecfg"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

type fakeStream struct {
	events []core.Event
	i      int
}

func (s *fakeStream) Recv() (core.Event, error) {
	if s.i >= len(s.events) {
		return core.Event{}, io.EOF
	}
	ev := s.events[s.i]
	s.i++
	return ev, nil
}

func (s *fakeStream) Close() error { return nil }

type testClient struct {
	name    string
	events  []core.Event
	chatErr error
	// catalogue overrides the default one-model answer, so a test can pin the
	// per-model Extra fields a module publishes.
	catalogue []core.Model
	// seen records the request the last Chat call received, so a test can
	// assert what the gateway translated the wire body into.
	seen *core.ChatRequest
	// servedBy makes this client behave like a real module: it names the
	// credential that served the request in the slot the gateway allocated.
	servedBy string
	// hint, when set, makes this client a core.HintProvider -- a module with its
	// own vendor error vocabulary.  Left empty the module stays silent, which is
	// how the shared table in internal/hint gets its turn.
	hint string
	// seenHint records the context the gateway resolved before asking, so a test
	// can pin what the error path actually handed the module.
	seenHint *core.HintContext
	// modelsCalls counts catalogue reads, so a test can prove that a request
	// which never fails does not pay for one.
	modelsCalls int
}

func (c *testClient) Name() string { return c.name }

func (c *testClient) Models(ctx context.Context) ([]core.Model, error) {
	c.modelsCalls++
	if c.catalogue != nil {
		return c.catalogue, nil
	}
	return []core.Model{{ID: "m1", OwnedBy: c.name}}, nil
}

func (c *testClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seen = req
	// A module names its account at the point it picks one, before it knows
	// whether the attempt will work -- exactly as the real ones do.
	core.NoteServedBy(req, c.servedBy)
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	return &fakeStream{events: c.events}, nil
}

func (c *testClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// Hint makes testClient a core.HintProvider.  The zero answer is "", so every
// existing test keeps its old behaviour: a silent module and a module without
// the interface are indistinguishable to the gateway.
func (c *testClient) Hint(kind core.FailureKind, message string, ctx core.HintContext) string {
	seen := ctx
	c.seenHint = &seen
	return c.hint
}

func newTestServer(t *testing.T, c *testClient, stats *Stats, usage *UsageStore) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	reg.Add(c)
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    stats,
		Usage:    usage,
	})
}

func chat(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func usageEvents(prompt, completion int) []core.Event {
	return []core.Event{
		{Type: core.EventDelta, Delta: "hello"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}},
		{Type: core.EventDone, Finish: "stop"},
	}
}

const bufferedBody = `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
const streamBody = `{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// ---------------------------------------------------------------------------
// usage recording
// ---------------------------------------------------------------------------

func TestChatRecordsBufferedUsage(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", events: usageEvents(10, 5)}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	r := got[0]
	if r.Client != "t" || r.Model != "t/m1" {
		t.Errorf("record = %+v, want client t and model t/m1", r)
	}
	if r.Failed {
		t.Errorf("record marked failed: %+v", r)
	}
	if r.PromptTokens != 10 || r.CompletionTokens != 5 || r.TotalTokens != 15 {
		t.Errorf("tokens = %d/%d/%d, want 10/5/15", r.PromptTokens, r.CompletionTokens, r.TotalTokens)
	}
	if stats.Requests() != 1 || stats.Failures() != 0 {
		t.Errorf("counters = %d/%d, want 1/0", stats.Requests(), stats.Failures())
	}
}

func TestChatRecordsStreamingUsage(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", events: usageEvents(200, 40)}, stats, usage)

	rec := chat(t, srv, streamBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("stream did not terminate normally: %s", rec.Body.String())
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	if got[0].PromptTokens != 200 || got[0].CompletionTokens != 40 {
		t.Errorf("streaming tokens not recorded: %+v", got[0])
	}
	if got[0].Failed {
		t.Errorf("streaming success marked failed: %+v", got[0])
	}
}

// TestChatAttributesASuccessToTheAccountTheModuleNames: a success used to be
// recorded with an empty account, because the gateway only ever learned the
// account from a failure's own error.  Every success therefore landed in the
// "(unrouted)" bucket of the usage report and showed a blank account on the
// console row.  A module that names its credential must be believed.
func TestChatAttributesASuccessToTheAccountTheModuleNames(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", servedBy: "acct-7", events: usageEvents(3, 4)}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	if got[0].Account != "acct-7" {
		t.Errorf("recorded account = %q, want acct-7", got[0].Account)
	}
	if got[0].Failed {
		t.Errorf("record marked failed: %+v", got[0])
	}

	// The account table is what the panel renders, so the attribution has to
	// survive the report aggregation too, not just the raw record.
	rep := usage.UsageSnapshot(0, nil)
	if len(rep.ByAccount) != 1 {
		t.Fatalf("ByAccount = %+v, want exactly one row", rep.ByAccount)
	}
	if rep.ByAccount[0].Name != "acct-7" {
		t.Errorf("ByAccount[0].Name = %q, want acct-7", rep.ByAccount[0].Name)
	}
}

// TestChatFallsBackToTheServedAccountOnFailure: a failure whose error names no
// account is still attributable, because the module said which credential it
// was about to use.  A failure that does name one keeps its own answer -- that
// is the more specific truth.
func TestChatFallsBackToTheServedAccountOnFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "anonymous failure falls back", err: errors.New("upstream said no"), want: "acct-7"},
		{name: "attributed failure wins", err: core.Fail("t", "acct-9", core.FailureUpstream, 502, errors.New("boom")), want: "acct-9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usage := NewUsageStore(10)
			srv := newTestServer(t, &testClient{name: "t", servedBy: "acct-7", chatErr: tc.err}, NewStats(), usage)

			rec := chat(t, srv, bufferedBody)
			if rec.Code == http.StatusOK {
				t.Fatalf("expected a failure, got %d", rec.Code)
			}

			got := usage.Snapshot()
			if len(got) != 1 {
				t.Fatalf("recorded %d usage records, want 1", len(got))
			}
			if got[0].Account != tc.want {
				t.Errorf("recorded account = %q, want %q", got[0].Account, tc.want)
			}
			if !got[0].Failed {
				t.Errorf("record not marked failed: %+v", got[0])
			}
		})
	}
}

// TestChatRecordsPreModuleFailure covers a request that dies before it ever
// reaches a module: it must still be one request and one failure, with zero
// tokens.
func TestChatRecordsPreModuleFailure(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", events: usageEvents(1, 1)}, stats, usage)

	rec := chat(t, srv, `{"model":"nobody-serves-this","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected a failure, got %d", rec.Code)
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	if !got[0].Failed {
		t.Errorf("record not marked failed: %+v", got[0])
	}
	if got[0].PromptTokens != 0 || got[0].TotalTokens != 0 {
		t.Errorf("tokens = %+v, want zero", got[0])
	}
	if stats.Requests() != 1 || stats.Failures() != 1 {
		t.Errorf("counters = %d/%d, want 1/1", stats.Requests(), stats.Failures())
	}
}

func TestChatRecordsInvalidBodyAsFailure(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t"}, stats, usage)

	rec := chat(t, srv, `{"model":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	got := usage.Snapshot()
	if len(got) != 1 || !got[0].Failed {
		t.Fatalf("invalid body was not recorded as a failure: %+v", got)
	}
}

func TestChatRecordsUpstreamErrorEvent(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	events := []core.Event{
		{Type: core.EventDelta, Delta: "partial"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 7, CompletionTokens: 2}},
		{Type: core.EventError, Err: errFake("upstream exploded")},
	}
	srv := newTestServer(t, &testClient{name: "t", events: events}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected an upstream failure, got %d", rec.Code)
	}

	got := usage.Snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d usage records, want 1", len(got))
	}
	if !got[0].Failed {
		t.Errorf("record not marked failed: %+v", got[0])
	}
	// Tokens reported before the error are still worth keeping.
	if got[0].PromptTokens != 7 || got[0].TotalTokens != 9 {
		t.Errorf("tokens = %+v, want 7/2/9", got[0])
	}
	if stats.Failures() != 1 {
		t.Errorf("failures = %d, want 1", stats.Failures())
	}
}

func TestChatRecordsChatFailure(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", chatErr: core.ErrNotConfigured}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	got := usage.Snapshot()
	if len(got) != 1 || !got[0].Failed {
		t.Fatalf("chat failure was not recorded: %+v", got)
	}
	if got[0].Client != "t" || got[0].Model != "t/m1" {
		t.Errorf("record should still name the routed client: %+v", got[0])
	}
}

// TestChatReportsBackpressureAs429 pins the wire shape of the in-flight
// ceiling.  A module that has nowhere to send the request right now is
// backpressure, not a broken upstream: the caller is told to come back, and
// told when, instead of being handed a 502 it would retry into the same wall.
func TestChatReportsBackpressureAs429(t *testing.T) {
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", chatErr: core.ErrBusy}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "rate_limit_error") {
		t.Errorf("body = %q, want it to name rate_limit_error", body)
	}
	if got := usage.Snapshot(); len(got) != 1 || !got[0].Failed {
		t.Fatalf("chat failure was not recorded: %+v", got)
	}
	if stats.Failures() != 1 {
		t.Errorf("failures = %d, want 1", stats.Failures())
	}
}

// TestErrorBodiesCarryTheReferenceCodeVocabulary pins `error.code`.  The
// reference panel always names a failure symbolically there and keeps the
// vendor's own numeric business code inside `message` verbatim
// (internal/server/handler.go:890-917); a caller that branches on error.code
// must see a word, not a missing field.
func TestErrorBodiesCarryTheReferenceCodeVocabulary(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"no usable account", core.ErrNotConfigured, http.StatusServiceUnavailable, "no_healthy_account"},
		{"in-flight ceiling is backpressure", core.ErrBusy, http.StatusTooManyRequests, "rate_limit_exceeded"},
		{"unsupported request", core.ErrUnsupported, http.StatusBadRequest, "invalid_request"},
		{"waf block", core.Fail("t", "acct", core.FailureWAF, 403, errFake("blocked by security policy")), 403, "waf_ip_blocked"},
		{"soft rate limit", core.Fail("t", "acct", core.FailureRateLimited, 429, errFake("too many requests")), 429, "rate_limit_exceeded"},
		{"quota", core.Fail("t", "acct", core.FailureQuota, 429, errFake("credits exhausted")), 429, "quota_exhausted"},
		{"rejected credential", core.Fail("t", "acct", core.FailureAuth, 401, errFake("bad token")), 401, "account_auth_failed"},
		{"content block", core.Fail("t", "acct", core.FailureContentBlocked, 400, errFake("content_blocked")), 400, "content_blocked"},
		{"unclassified", errFake("boom"), http.StatusBadGateway, "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, &testClient{name: "t", chatErr: tc.err}, NewStats(), NewUsageStore(10))
			rec := chat(t, srv, bufferedBody)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body.String())
			}
			var got apiErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
			if got.Error.Code != tc.code {
				t.Errorf("error.code = %v, want %q (body %s)", got.Error.Code, tc.code, rec.Body.String())
			}
			if got.Error.Message == "" {
				t.Errorf("error.message is empty: %s", rec.Body.String())
			}
		})
	}

	// The vendor's numeric business code is not a wire code: it travels inside
	// message, which is the only place the reference keeps it too.
	t.Run("vendor business code stays in message", func(t *testing.T) {
		err := core.Fail("trae", "acct", core.FailureUpstream, http.StatusBadGateway,
			errFake("trae: upstream returned 502 (code=4008 session expired)"))
		srv := newTestServer(t, &testClient{name: "t", chatErr: err}, NewStats(), NewUsageStore(10))
		rec := chat(t, srv, bufferedBody)
		var got apiErrorEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		if got.Error.Code != "upstream_error" {
			t.Errorf("error.code = %v, want upstream_error", got.Error.Code)
		}
		if !strings.Contains(got.Error.Message, "code=4008") {
			t.Errorf("message lost the vendor code: %s", got.Error.Message)
		}
	})
}

// TestMissingAPIKeyNamesItsCode covers the local auth rejection, which is the
// one place the reference has its own word for ("invalid_api_key").
func TestMissingAPIKeyNamesItsCode(t *testing.T) {
	srv := NewServer(Options{
		Registry: func() *core.Registry {
			reg := core.NewRegistry()
			reg.Add(&testClient{name: "t", events: usageEvents(1, 1)})
			return reg
		}(),
		Version: "test",
		Logger:  log.New(io.Discard, "", 0),
		APIKey:  "secret",
	})
	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	var got apiErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if got.Error.Code != "invalid_api_key" {
		t.Errorf("error.code = %v, want invalid_api_key", got.Error.Code)
	}
}

func TestStatusUsesSharedStats(t *testing.T) {
	stats := NewStats()
	stats.addRequest()
	stats.addFailure()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t"}, stats, usage)

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `"requests":1`) || !strings.Contains(body, `"failures":1`) {
		t.Errorf("/v1/status does not report the shared counters: %s", body)
	}
}

func TestNewServerDefaultsInstrumentation(t *testing.T) {
	srv := NewServer(Options{Registry: core.NewRegistry(), Version: "test", Logger: log.New(io.Discard, "", 0)})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
}

// /v1/models is the OpenAI-shaped catalogue, and the per-model facts that have
// no home in that shape — the credit multiplier, the campaign in force, the
// effort ladder — ride along in `extra`.  A module that publishes none must not
// grow the key, so a plain catalogue stays exactly the OpenAI one.
func TestModelsCarriesTheModuleExtraFields(t *testing.T) {
	c := &testClient{
		name: "wb",
		catalogue: []core.Model{
			{ID: "glm-5.2", OwnedBy: "workbuddy", Extra: map[string]any{
				"credits":       "x0.29",
				"promo_credits": "x0.15",
				"promo_factor":  0.5,
				"promo_label":   "夜间五折",
			}},
			{ID: "plain", OwnedBy: "workbuddy"},
		},
	}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var out struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if out.Object != "list" || len(out.Data) != 2 {
		t.Fatalf("envelope = %q with %d entries, want a two-entry list", out.Object, len(out.Data))
	}

	entry := map[string]map[string]any{}
	for _, e := range out.Data {
		id, _ := e["id"].(string)
		extra, _ := e["extra"].(map[string]any)
		entry[id] = extra
	}

	promo := entry["wb/glm-5.2"]
	if promo == nil {
		t.Fatalf("wb/glm-5.2 carries no extra: %s", rec.Body.String())
	}
	if promo["credits"] != "x0.29" || promo["promo_credits"] != "x0.15" || promo["promo_label"] != "夜间五折" {
		t.Errorf("extra = %v, want the list price and the effective campaign", promo)
	}
	if got, ok := promo["promo_factor"].(float64); !ok || got != 0.5 {
		t.Errorf("promo_factor = %v, want 0.5", promo["promo_factor"])
	}
	if _, ok := entry["wb/plain"]; !ok {
		t.Error("the plain model is missing from the catalogue")
	}
	if entry["wb/plain"] != nil {
		t.Errorf("a model with no Extra grew an `extra` key: %v", entry["wb/plain"])
	}
}

// An alias is part of what the gateway serves, so it belongs in the catalogue.
// The bug this pins: "glm-5.3" resolved and worked, but a client that lists
// models and picks from the list could never discover it.  The alias is
// advertised with owned_by=alias and its target in extra, so a UI can tell an
// alias from a real upstream id instead of guessing.
func TestModelsListsTheAliasTable(t *testing.T) {
	reg := core.NewRegistry()
	reg.Add(&testClient{name: "wb", catalogue: []core.Model{{ID: "GLM-5.3", OwnedBy: "workbuddy"}}})
	reg.AddAlias("glm-5.3", "wb/GLM-5.3")
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var out modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 2 {
		t.Fatalf("catalogue = %d entries, want the model plus its alias: %s", len(out.Data), rec.Body.String())
	}
	// Sorted by id: "glm-5.3" (lowercase g) sorts before "wb/GLM-5.3".
	if out.Data[0].ID != "glm-5.3" {
		t.Fatalf("first entry = %q, want the alias glm-5.3", out.Data[0].ID)
	}
	alias := out.Data[0]
	if alias.OwnedBy != "alias" {
		t.Errorf("alias owned_by = %q, want \"alias\" so a UI can tell it from a real id", alias.OwnedBy)
	}
	if alias.Extra["target"] != "wb/GLM-5.3" {
		t.Errorf("alias extra.target = %v, want wb/GLM-5.3", alias.Extra["target"])
	}
	// The alias must not shadow a real entry if the same string is both.
	if out.Data[1].ID != "wb/GLM-5.3" || out.Data[1].OwnedBy == "alias" {
		t.Errorf("second entry = %+v, want the untouched upstream model", out.Data[1])
	}
}

// A qualified alias target must survive the round trip: listing the alias and
// then requesting it has to reach the same module with the same upstream model
// name.  The response echoes the alias because that is what the caller asked
// for -- what matters is what the module was handed.
func TestAliasResolvesToItsTarget(t *testing.T) {
	c := &testClient{name: "wb", catalogue: []core.Model{{ID: "GLM-5.3"}}, events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(c)
	reg.AddAlias("glm-5.3", "wb/GLM-5.3")
	srv := NewServer(Options{
		Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0),
		Stats: NewStats(), Usage: NewUsageStore(10),
	})

	rec := chat(t, srv, `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("alias request failed: %d %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module never received the request")
	}
	if c.seen.Model != "GLM-5.3" {
		t.Errorf("module received model %q, want the upstream id GLM-5.3, not the alias", c.seen.Model)
	}
	if !strings.Contains(rec.Body.String(), `"model":"glm-5.3"`) {
		t.Errorf("response model = %s, want the alias echoed back to the caller", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// conversation identity
// ---------------------------------------------------------------------------

func TestResolveConversationIDPrefersMetadataOverTopLevel(t *testing.T) {
	cases := []struct {
		name string
		wire string
		want string
	}{
		{"metadata snake", `{"metadata":{"conversation_id":"conv-meta"}}`, "conv-meta"},
		{"metadata camel", `{"metadata":{"conversationId":"conv-meta-camel"}}`, "conv-meta-camel"},
		{"metadata beats the top level", `{"conversation_id":"conv-top","metadata":{"conversation_id":"conv-meta"}}`, "conv-meta"},
		{"snake beats camel inside metadata", `{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"},
		{"top level snake", `{"conversation_id":"conv-top"}`, "conv-top"},
		{"top level camel", `{"conversationId":"conv-top-camel"}`, "conv-top-camel"},
		{"top level snake beats camel", `{"conversation_id":"snake","conversationId":"camel"}`, "snake"},
		{"trimmed", `{"conversation_id":"  conv-trim  "}`, "conv-trim"},
		{"absent stays empty", `{}`, ""},
		{"blank stays empty", `{"conversation_id":"   "}`, ""},
		{"malformed metadata is not fatal", `{"conversation_id":"conv-top","metadata":"nope"}`, "conv-top"},
		{"user is not a conversation", `{"user":"u-1"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var w chatRequest
			if err := json.Unmarshal([]byte(tc.wire), &w); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.wire, err)
			}
			if got := resolveConversationID(&w); got != tc.want {
				t.Fatalf("resolveConversationID = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveConversationIDHandlesNil(t *testing.T) {
	if got := resolveConversationID(nil); got != "" {
		t.Fatalf("resolveConversationID(nil) = %q, want \"\"", got)
	}
}

func chatWithHeader(t *testing.T, srv *http.Server, body, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func TestChatCarriesTheCallersConversationIdentity(t *testing.T) {
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(4))

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-1"}}`
	rec := chatWithHeader(t, srv, body, "X-Conversation-Request-ID", "turn-7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module never saw the request")
	}
	if c.seen.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want conv-1", c.seen.ConversationID)
	}
	if c.seen.ConversationRequestID != "turn-7" {
		t.Errorf("ConversationRequestID = %q, want turn-7", c.seen.ConversationRequestID)
	}
}

func TestChatLeavesTheConversationEmptyWhenTheCallerSentNone(t *testing.T) {
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(4))

	// "user" is an end-user id, not a conversation.  Substituting it would make
	// the vendor aggregate unrelated chats under one conversation.
	body := `{"model":"m1","user":"u-1","messages":[{"role":"user","content":"hi"}]}`
	rec := chatWithHeader(t, srv, body, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen.ConversationID != "" {
		t.Errorf("ConversationID = %q, want \"\" — a user id is not a conversation", c.seen.ConversationID)
	}
	if c.seen.ConversationRequestID != "" {
		t.Errorf("ConversationRequestID = %q, want \"\"", c.seen.ConversationRequestID)
	}
}

func TestChatKeepsTheConversationIDOutOfTheOptions(t *testing.T) {
	// Modules copy Options verbatim into the body they send upstream, so a key
	// only this side understands must never be folded in there.
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(4))

	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],` +
		`"metadata":{"conversation_id":"conv-1"},"client2api":{"reasoning_effort":"high"}}`
	rec := chatWithHeader(t, srv, body, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil {
		t.Fatal("the module never saw the request")
	}
	if _, ok := c.seen.Options["conversation_id"]; ok {
		t.Error("the resolved conversation id was folded into Options; it would reach the vendor")
	}
	if _, ok := c.seen.Options["conversationId"]; ok {
		t.Error("the resolved conversation id was folded into Options; it would reach the vendor")
	}
	if got := c.seen.Options["reasoning_effort"]; got != "high" {
		t.Errorf("client2api options = %v, want reasoning_effort=high passed through untouched", c.seen.Options)
	}
}

func TestChatPublishesPlatformAlertAfterThreeFailedRounds(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	var got []alerts.Alert
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
		Live:     livecfg.New(livecfg.Snapshot{MaxRotate: 1, RotateBackoffBase: time.Nanosecond}),
		NotifyAlert: func(a alerts.Alert) {
			got = append(got, a)
		},
	})

	for i := 0; i < 3; i++ {
		if rec := chat(t, srv, bufferedBody); rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body %s", i+1, rec.Code, rec.Body.String())
		}
	}
	if len(got) != 1 {
		t.Fatalf("published %d alerts, want 1", len(got))
	}
	if got[0].Client != "alpha" || got[0].Model != "m1" || got[0].Count != core.PlatformFailureThreshold {
		t.Fatalf("alert = %+v, want alpha/m1 count %d", got[0], core.PlatformFailureThreshold)
	}
}

// errFake is a tiny error type so tests can build failures with odd payloads.
type errFake string

func (e errFake) Error() string { return string(e) }

func TestChatFailsOverToTheNextPlatform(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", servedBy: "b1", events: usageEvents(3, 4)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	usage := NewUsageStore(10)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    usage,
		Live: livecfg.New(livecfg.Snapshot{
			MaxRotate:         1,
			RotateBackoffBase: time.Nanosecond,
		}),
	})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if beta.seen == nil {
		t.Fatal("the second platform never received the request")
	}
	got := usage.Snapshot()
	var final *UsageRecord
	var sawAlphaAttempt bool
	for i := range got {
		if got[i].Attempt {
			sawAlphaAttempt = sawAlphaAttempt || (got[i].Client == "alpha" && got[i].Failed)
			continue
		}
		final = &got[i]
	}
	if !sawAlphaAttempt {
		t.Fatalf("usage = %+v, want the failed alpha candidate attempt", got)
	}
	if final == nil || final.Failed || final.Client != "beta" || final.Account != "b1" {
		t.Fatalf("final usage = %+v, want the successful beta/b1 record", final)
	}
}

func TestChatDoesNotFailOverAContentBlock(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureContentBlocked, http.StatusBadRequest, errors.New("blocked"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: 1}, "beta": {Priority: 2}})
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0), Stats: NewStats(), Usage: NewUsageStore(10)})

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if beta.seen != nil {
		t.Fatal("content policy refusal was retried on another platform")
	}
}
