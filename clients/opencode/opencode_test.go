package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// testNow is the frozen clock every hermetic test uses.
var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Test harness.
// ---------------------------------------------------------------------------

// roundTripFunc lets a test stand in for the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// jsonResponse builds a JSON HTTP response.
func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// sseResponse builds a 200 text/event-stream response.
func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// newTestClient builds a hermetic client: the clock is frozen, the pool is
// empty, and ensureOnce is armed so a later ensure() cannot re-read the
// machine's real credentials (OPENCODE_API_KEY or ~/.local/share/opencode).
func newTestClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	t.Setenv(apiKeyEnv, "")
	c := &Client{cfg: cfg.normalize(), now: func() time.Time { return testNow }}
	c.ensureOnce.Do(func() { c.pool = newPool() })
	return c
}

// newFakeClient is a test client whose HTTP transport is fn.
func newFakeClient(t *testing.T, cfg Config, fn roundTripFunc) *Client {
	t.Helper()
	c := newTestClient(t, cfg)
	c.deps.HTTPClient = &http.Client{Transport: fn}
	return c
}

// addAccount puts one usable account in the pool.
func addAccount(t *testing.T, c *Client, id, key string) {
	t.Helper()
	if !c.pool.upsert(accountRecord{
		ID:      id,
		Label:   id,
		APIKey:  key,
		Enabled: true,
		Source:  sourcePanel,
	}) {
		t.Fatalf("upsert(%q) reported no change", id)
	}
}

// wantErrIs fails unless err wraps want.
func wantErrIs(t *testing.T, err error, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want it to wrap %v", err, want)
	}
}

// chatRequest builds a minimal request.
func chatRequest(model string) *core.ChatRequest {
	return &core.ChatRequest{
		Model:    model,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	}
}

// ---------------------------------------------------------------------------
// Construction and registration.
// ---------------------------------------------------------------------------

func TestRegistration(t *testing.T) {
	found := false
	for _, name := range core.Registered() {
		if name == clientName {
			found = true
		}
	}
	if !found {
		t.Fatalf("core.Registered() does not contain %q: %v", clientName, core.Registered())
	}
}

func TestNewWithNilConfig(t *testing.T) {
	c, err := New(core.Deps{})
	if err != nil {
		t.Fatalf("New(nil config) = %v, want nil error", err)
	}
	oc, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", c)
	}
	if oc.cfgErr != nil {
		t.Fatalf("cfgErr = %v, want nil for an absent config block", oc.cfgErr)
	}
	if got := oc.Name(); got != clientName {
		t.Fatalf("Name() = %q, want %q", got, clientName)
	}
	if got := oc.cfg.baseURL(); got != defaultBaseURL {
		t.Fatalf("baseURL() = %q, want %q", got, defaultBaseURL)
	}
}

// A config decode failure must never take the module down.
func TestNewWithMalformedConfigKeepsDefaults(t *testing.T) {
	var logged []string
	c, err := New(core.Deps{
		Config: json.RawMessage(`{"base_url": 42, "max_in_flight": "lots"`),
		Logf:   func(format string, args ...any) { logged = append(logged, format) },
	})
	if err != nil {
		t.Fatalf("New(bad config) = %v, want nil error", err)
	}
	oc := c.(*Client)
	if oc.cfgErr == nil {
		t.Fatal("cfgErr = nil, want the decode failure recorded")
	}
	if got := oc.cfg.baseURL(); got != defaultBaseURL {
		t.Fatalf("baseURL() = %q, want the default %q", got, defaultBaseURL)
	}
	if len(logged) == 0 {
		t.Fatal("the rejected config was not logged")
	}
}

func TestNewWithGoodConfig(t *testing.T) {
	c, err := New(core.Deps{Config: json.RawMessage(`{
		"base_url": "https://example.test/zen/v1/",
		"api_key": "  sk-abc  ",
		"max_in_flight": 7,
		"test_model": "gpt-5-nano",
		"chat_timeout": "30s"
	}`)})
	if err != nil {
		t.Fatalf("New = %v", err)
	}
	oc := c.(*Client)
	if oc.cfgErr != nil {
		t.Fatalf("cfgErr = %v, want nil", oc.cfgErr)
	}
	if got := oc.cfg.baseURL(); got != "https://example.test/zen/v1" {
		t.Fatalf("baseURL() = %q, want the trailing slash trimmed", got)
	}
	if got := oc.cfg.chatURL(); got != "https://example.test/zen/v1/chat/completions" {
		t.Fatalf("chatURL() = %q", got)
	}
	if got := oc.cfg.APIKey; got != "sk-abc" {
		t.Fatalf("APIKey = %q, want it trimmed", got)
	}
	if got := oc.cfg.maxInFlight(); got != 7 {
		t.Fatalf("maxInFlight() = %d, want 7", got)
	}
	if got := oc.cfg.chatTimeout(); got != 30*time.Second {
		t.Fatalf("chatTimeout() = %s, want 30s", got)
	}
}

// A config header may never override the ones the transport owns.
func TestConfigReservedHeadersAreDropped(t *testing.T) {
	cfg := Config{ExtraHeaders: map[string]string{
		"Authorization": "Bearer sneaky",
		"Content-Type":  "text/plain",
		"X-Trace":       "abc",
	}}.normalize()
	if _, ok := cfg.ExtraHeaders["Authorization"]; ok {
		t.Fatal("a config header overrode Authorization")
	}
	if _, ok := cfg.ExtraHeaders["Content-Type"]; ok {
		t.Fatal("a config header overrode Content-Type")
	}
	if got := cfg.ExtraHeaders["X-Trace"]; got != "abc" {
		t.Fatalf("X-Trace = %q, want it kept", got)
	}
}

// ---------------------------------------------------------------------------
// core.Client: request validation.
// ---------------------------------------------------------------------------

func TestChatRejectsNilRequest(t *testing.T) {
	c := newTestClient(t, Config{})
	_, err := c.Chat(context.Background(), nil)
	wantErrIs(t, err, core.ErrUnsupported)
}

func TestChatRejectsBlankModel(t *testing.T) {
	c := newTestClient(t, Config{})
	req := chatRequest("   ")
	_, err := c.Chat(context.Background(), req)
	wantErrIs(t, err, core.ErrUnsupported)
}

func TestChatRejectsEmptyMessages(t *testing.T) {
	c := newTestClient(t, Config{})
	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "gpt-5.1"})
	wantErrIs(t, err, core.ErrUnsupported)
}

// A request shape the module cannot express must be refused before an account
// is taken, so a bad request never consumes a pool slot.
func TestChatRejectsUnknownRoleWithoutTakingAnAccount(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	req := &core.ChatRequest{
		Model:    "gpt-5.1",
		Messages: []core.Message{{Role: "wizard", Content: "hi"}},
	}
	_, err := c.Chat(context.Background(), req)
	wantErrIs(t, err, core.ErrUnsupported)
	if inFlight, _ := c.pool.stats(c.cfg.maxInFlight()); inFlight != 0 {
		t.Fatalf("in-flight = %d, want 0: a rejected request must not hold a slot", inFlight)
	}
}

func TestChatWithoutAccountIsNotConfigured(t *testing.T) {
	c := newTestClient(t, Config{})
	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	wantErrIs(t, err, core.ErrNotConfigured)
}

// A saturated pool is 429, not 503: the caller should retry.
func TestChatWithSaturatedPoolIsBusy(t *testing.T) {
	c := newTestClient(t, Config{MaxInFlight: 1})
	addAccount(t, c, "opencode:a", "sk-a")
	if _, err := c.pool.acquire(testNow, 1); err != nil {
		t.Fatalf("acquire = %v", err)
	}
	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	wantErrIs(t, err, core.ErrBusy)
}

// ---------------------------------------------------------------------------
// core.Client: chat happy paths.
// ---------------------------------------------------------------------------

const happySSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"cost\":\"0.0001\"}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func TestChatStreamsDeltasInOrder(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	text, _, _, _, finish, err := drain(stream)
	if err != nil {
		t.Fatalf("drain = %v", err)
	}
	if text != "Hello" {
		t.Fatalf("text = %q, want %q", text, "Hello")
	}
	if finish != "stop" {
		t.Fatalf("finish = %q, want %q", finish, "stop")
	}
}

// A stream that ends without [DONE] is a normal end, not a failure.
func TestChatWithoutDoneMarkerStillEndsCleanly(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(body), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	text, _, _, _, _, err := drain(stream)
	if err != nil {
		t.Fatalf("drain = %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want %q", text, "ok")
	}
}

// The caller asked for Stream:false, so the module re-aggregates the upstream
// SSE into exactly one delta.
func TestChatNonStreamingAggregatesToOneDelta(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	req := chatRequest("gpt-5.1")
	req.Stream = false
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	var deltas int
	var text string
	var done int
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv = %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			deltas++
			text += ev.Delta
		case core.EventDone:
			done++
		}
	}
	if deltas != 1 {
		t.Fatalf("deltas = %d, want exactly 1 for a non-streaming caller", deltas)
	}
	if text != "Hello" {
		t.Fatalf("text = %q, want %q", text, "Hello")
	}
	if done != 1 {
		t.Fatalf("done events = %d, want exactly 1", done)
	}
}

// Tool-call argument fragments must be concatenated, never overwritten.
func TestChatConcatenatesToolCallArguments(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(body), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	_, _, calls, _, finish, err := drain(stream)
	if err != nil {
		t.Fatalf("drain = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1: %+v", len(calls), calls)
	}
	if calls[0].Name != "get_weather" {
		t.Fatalf("name = %q, want get_weather", calls[0].Name)
	}
	if calls[0].Arguments != `{"city":"SF"}` {
		t.Fatalf("arguments = %q, want the concatenation of both fragments", calls[0].Arguments)
	}
	if calls[0].ID != "call_1" {
		t.Fatalf("id = %q, want call_1", calls[0].ID)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q, want tool_calls", finish)
	}
}

func TestChatEmitsUsageOnce(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n" +
		"data: [DONE]\n\n"
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(body), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()

	var usage int
	var got core.Usage
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv = %v", err)
		}
		if ev.Type == core.EventUsage {
			usage++
			if ev.Usage != nil {
				got = *ev.Usage
			}
		}
	}
	if usage != 1 {
		t.Fatalf("usage events = %d, want exactly 1", usage)
	}
	if got.PromptTokens != 10 || got.CompletionTokens != 4 || got.TotalTokens != 14 {
		t.Fatalf("usage = %+v, want 10/4/14", got)
	}
}

// The account must be visible to the panel for the request it served.
func TestChatNotesServedBy(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	req := chatRequest("gpt-5.1")
	served := ""
	req.ServedBy = &served
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	defer stream.Close()
	if served != "opencode:a" {
		t.Fatalf("ServedBy = %q, want opencode:a", served)
	}
}

// A successful stream gives the pool slot back.
func TestChatReleasesTheSlotOnClose(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	if got := c.PoolStats().InFlight; got != 1 {
		t.Fatalf("in flight = %d, want 1 while the stream is open", got)
	}
	if _, _, _, _, _, err := drain(stream); err != nil {
		t.Fatalf("drain = %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in flight = %d, want 0 after Close", got)
	}
}

// Close must be idempotent: the gateway may close twice.
func TestStreamCloseIsIdempotent(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in flight = %d, want 0: a double Close must not double-release", got)
	}
}

// A cancelled context must end the stream promptly.
func TestChatHonoursContextCancellation(t *testing.T) {
	c := newFakeClient(t, Config{}, func(req *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			<-req.Context().Done()
			pw.CloseWithError(req.Context().Err())
		}()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Chat(ctx, chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v", err)
	}
	cancel()
	if err := stream.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// ---------------------------------------------------------------------------
// core.Client: chat failures.
// ---------------------------------------------------------------------------

func TestChatAuthErrorDisablesTheAccount(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err == nil {
		t.Fatal("Chat = nil error, want the vendor's AuthError surfaced")
	}
	if kind := core.FailureKindOf(err); kind != core.FailureAuth {
		t.Fatalf("failure kind = %q, want %q", kind, core.FailureAuth)
	}
	acct, _ := c.pool.byID("opencode:a")
	if acct.Enabled {
		t.Fatal("the account is still enabled after an AuthError")
	}
}

// The model check runs BEFORE the auth check and both answer 401, so a bogus
// model must not be mistaken for a dead account.
func TestChatUnknownModelIsUnsupportedAndLeavesTheAccountReady(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"type":"error","error":{"type":"ModelError","message":"Model bogus-model-xyz is not supported"}}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.Chat(context.Background(), chatRequest("bogus-model-xyz"))
	wantErrIs(t, err, core.ErrUnsupported)
	acct, _ := c.pool.byID("opencode:a")
	if !acct.Enabled {
		t.Fatal("a caller's typo disabled a healthy account")
	}
	if acct.CooldownUntil != "" {
		t.Fatalf("cooldown = %q, want none after a model error", acct.CooldownUntil)
	}
}

// A 429 is retryable, so Chat rotates to the second account and succeeds.
func TestChatRotatesToTheSecondAccountOn429(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{}, func(req *http.Request) (*http.Response, error) {
		calls++
		if strings.HasSuffix(req.Header.Get("Authorization"), "sk-a") {
			return jsonResponse(429, `{"type":"error","error":{"type":"RateLimitError","message":"rate limit exceeded"}}`), nil
		}
		return sseResponse(happySSE), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	addAccount(t, c, "opencode:b", "sk-b")

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat = %v, want it to rotate to the second account", err)
	}
	defer stream.Close()
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
}

// A 5xx on every account ends as the last real verdict, not a generic error.
func TestChatExhaustedRotationReturnsTheLastVerdict(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(503, `{"type":"error","error":{"type":"ServerError","message":"upstream on fire"}}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err == nil {
		t.Fatal("Chat = nil error, want a failure")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("error %v is not a *core.Failure", err)
	}
	if f.Status != 503 {
		t.Fatalf("status = %d, want the upstream 503 preserved", f.Status)
	}
}

// ---------------------------------------------------------------------------
// Transport.
// ---------------------------------------------------------------------------

func TestDoSendsBearerAndConfiguredHeaders(t *testing.T) {
	var gotAuth, gotUA, gotTrace, gotAccept string
	c := newFakeClient(t, Config{
		ExtraHeaders: map[string]string{"X-Trace": "abc"},
	}, func(req *http.Request) (*http.Response, error) {
		gotAuth = req.Header.Get("Authorization")
		gotUA = req.Header.Get("User-Agent")
		gotTrace = req.Header.Get("X-Trace")
		gotAccept = req.Header.Get("Accept")
		return jsonResponse(200, `{"object":"list","data":[]}`), nil
	})

	resp, err := c.do(context.Background(), requestSpec{
		method: http.MethodPost,
		url:    c.cfg.chatURL(),
		body:   []byte(`{}`),
		bearer: "sk-secret",
		accept: "text/event-stream",
	})
	if err != nil {
		t.Fatalf("do = %v", err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUA != userAgent {
		t.Fatalf("User-Agent = %q, want %q", gotUA, userAgent)
	}
	if gotTrace != "abc" {
		t.Fatalf("X-Trace = %q, want the configured header", gotTrace)
	}
	if gotAccept != "text/event-stream" {
		t.Fatalf("Accept = %q", gotAccept)
	}
}

// ---------------------------------------------------------------------------
// Status.
// ---------------------------------------------------------------------------

func TestStatusWithoutAccountIsNotReady(t *testing.T) {
	c := newTestClient(t, Config{})
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("Ready = true with no credential")
	}
	if st.Name != clientName {
		t.Fatalf("Name = %q, want %q", st.Name, clientName)
	}
	if !strings.Contains(st.Detail, apiKeyEnv) {
		t.Fatalf("Detail = %q, want it to name %s", st.Detail, apiKeyEnv)
	}
}

func TestStatusWithAccountIsReady(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Ready = false with one usable account; detail = %q", st.Detail)
	}
	if !strings.Contains(st.Detail, "1/1 accounts ready") {
		t.Fatalf("Detail = %q, want the pool summary", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("Accounts = %d, want 1", len(st.Accounts))
	}
}

func TestStatusReportsARejectedConfig(t *testing.T) {
	c := &Client{cfg: Config{}, cfgErr: errors.New("boom")}
	c.ensureOnce.Do(func() { c.pool = newPool() })
	c.now = func() time.Time { return testNow }
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("Ready = true although the config was rejected")
	}
	if !strings.Contains(st.Detail, "config rejected") {
		t.Fatalf("Detail = %q, want it to mention the rejected config", st.Detail)
	}
}

// Status must never expose a raw key.
func TestStatusMasksTheKey(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-verysecretvalue")
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Accounts = %d, want 1", len(st.Accounts))
	}
	blob, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "sk-verysecretvalue") {
		t.Fatalf("the raw key reached Status: %s", blob)
	}
}

// ---------------------------------------------------------------------------
// PoolStatsReporter.
// ---------------------------------------------------------------------------

func TestPoolStatsReportsInFlight(t *testing.T) {
	c := newTestClient(t, Config{MaxInFlight: 1})
	addAccount(t, c, "opencode:a", "sk-a")
	if _, err := c.pool.acquire(testNow, 1); err != nil {
		t.Fatalf("acquire = %v", err)
	}
	got := c.PoolStats()
	if got.InFlight != 1 {
		t.Fatalf("InFlight = %d, want 1", got.InFlight)
	}
	if got.InFlightFull != 1 {
		t.Fatalf("InFlightFull = %d, want 1", got.InFlightFull)
	}
}
