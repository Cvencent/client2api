package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// chat_test.go drives the request path end to end against a fixture server:
// what goes on the wire, what comes back as core events, and what happens when
// the vendor says no.

// ---------------------------------------------------------------------------
// the fixture
// ---------------------------------------------------------------------------

// recordedRequest is one request the module made.
type recordedRequest struct {
	Path   string
	Query  string
	Header http.Header
	Auth   string
	Raw    []byte
	Body   map[string]any
}

// chatFixture answers as the vendor does: the chat endpoint, the queue-status
// endpoint, the catalogue and the free-quota config, all on one listener, so a
// test can never reach the real network.
type chatFixture struct {
	srv *httptest.Server

	mu    sync.Mutex
	got   []recordedRequest
	seen  map[string]int
	onCha func(w http.ResponseWriter, r *http.Request, req recordedRequest, n int)
	onQue func(w http.ResponseWriter, r *http.Request, n int)
}

func newChatFixture(t *testing.T) *chatFixture {
	t.Helper()
	f := &chatFixture{seen: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *chatFixture) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rec := recordedRequest{
		Path:   r.URL.Path,
		Query:  r.URL.RawQuery,
		Header: r.Header.Clone(),
		Auth:   r.Header.Get("Authorization"),
		Raw:    body,
	}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &rec.Body)
	}

	f.mu.Lock()
	f.got = append(f.got, rec)
	f.seen[r.URL.Path]++
	n := f.seen[r.URL.Path]
	f.mu.Unlock()

	switch r.URL.Path {
	case chatAPIPath:
		if f.onCha != nil {
			f.onCha(w, r, rec, n)
			return
		}
		writeSSE(w, happySSE)
	case queueStatusPath:
		if f.onQue != nil {
			f.onQue(w, r, n)
			return
		}
		writeJSONBody(w, http.StatusOK, map[string]any{"status": "working"})
	case builtinModelsPath:
		writeJSONBody(w, http.StatusOK, map[string]any{
			"builtinModels": []map[string]any{{"model_id": "deepseek-v4-flash", "model_name": "DeepSeek V4 Flash"}},
		})
	case "/gateway/config":
		writeJSONBody(w, http.StatusOK, map[string]any{
			"result": map[string]any{"models": []map[string]any{{"model_id": "glm-5.3-flash"}}},
		})
	default:
		http.NotFound(w, r)
	}
}

// requests returns everything the module sent, oldest first.
func (f *chatFixture) requests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.got...)
}

// chatRequests returns only the chat completions calls.
func (f *chatFixture) chatRequests() []recordedRequest {
	var out []recordedRequest
	for _, r := range f.requests() {
		if r.Path == chatAPIPath {
			out = append(out, r)
		}
	}
	return out
}

// client builds a module pointed entirely at this fixture, so nothing in the
// test can escape to the vendor.
func (f *chatFixture) client(t *testing.T, extra string) *Client {
	t.Helper()
	cfg := `{
		"base_url":` + jsonString(f.srv.URL) + `,
		"chat_url":` + jsonString(f.srv.URL+chatAPIPath) + `,
		"models_url":` + jsonString(f.srv.URL+builtinModelsPath) + `,
		"gateway_url":` + jsonString(f.srv.URL+"/gateway/config") + `,
		"models_ttl":"1h",
		"access_key_id":"AKIDTESTACCOUNT","secret_access_key":"SKTESTACCOUNT","security_token":"TOKTESTACCOUNT",
		"expires_at":"2030-01-02T03:04:05Z"` + extra + `}`
	return newTestClient(t, cfg)
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeSSE(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, body)
}

// mustJSON renders a value as a JSON string, for building SSE frames.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// sse joins payloads into a well-formed event stream.
func sse(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

// happySSE is a normal answer: a reasoning delta, two content deltas, a tool
// call, a finish reason, usage, and the terminator.
var happySSE = sse(
	mustJSON(map[string]any{
		"id": "c1", "model": "deepseek-v4-flash",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "let me think"}}},
	}),
	mustJSON(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Hello"}}},
	}),
	mustJSON(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": ", world"}}},
	}),
	mustJSON(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{"q":1}`}},
		}}}},
	}),
	mustJSON(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
	}),
	mustJSON(map[string]any{
		"usage": map[string]any{
			"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14,
			"prompt_tokens_details":     map[string]any{"cached_tokens": 6},
			"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
		},
	}),
	"[DONE]",
)

// ---------------------------------------------------------------------------
// the error paths that need no vendor
// ---------------------------------------------------------------------------

func TestChatWithoutAnAccountIsNotConfigured(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "deepseek-v4-flash"})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat = %v, want core.ErrNotConfigured", err)
	}
}

func TestChatRejectsAnEmptyModel(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")
	for _, model := range []string{"", "   "} {
		if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: model}); !errors.Is(err, core.ErrUnsupported) {
			t.Errorf("Chat(model=%q) = %v, want core.ErrUnsupported", model, err)
		}
	}
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("Chat(nil) = %v, want core.ErrUnsupported", err)
	}
	if n := len(f.chatRequests()); n != 0 {
		t.Fatalf("the fixture saw %d chat calls, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// the wire
// ---------------------------------------------------------------------------

func TestChatRequestShape(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")

	maxTok := 123
	req := &core.ChatRequest{
		Model:          "deepseek-v4-flash",
		Messages:       []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens:      &maxTok,
		ConversationID: "conv-abc",
	}
	req2 := *req
	st, err := c.Chat(context.Background(), &req2)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	got := f.chatRequests()
	if len(got) != 1 {
		t.Fatalf("the fixture saw %d chat calls, want 1", len(got))
	}
	r := got[0]

	if r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
	}
	// The harness's own field names must never appear upstream.
	if _, ok := r.Body["conversation_id"]; ok {
		t.Error("the request body carries conversation_id, a field only this side understands")
	}
	if _, ok := r.Body["served_by"]; ok {
		t.Error("the request body carries served_by, a field only this side understands")
	}
	if r.Body["stream"] != true {
		t.Errorf("stream = %v, want true: this module only streams", r.Body["stream"])
	}
	if r.Body["model"] != "deepseek-v4-flash" {
		t.Errorf("model = %v, want deepseek-v4-flash", r.Body["model"])
	}
	if r.Body["max_tokens"] != float64(123) {
		t.Errorf("max_tokens = %v, want the caller's 123", r.Body["max_tokens"])
	}
	// The conversation id is used as the prefix-cache key, which is a field the
	// vendor does understand.
	if r.Body["prompt_cache_key"] != "conv-abc" {
		t.Errorf("prompt_cache_key = %v, want the conversation id", r.Body["prompt_cache_key"])
	}
	if r.Body["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_summary = %v, want auto", r.Body["reasoning_summary"])
	}
	if r.Body["tool_stream"] != true {
		t.Errorf("tool_stream = %v, want true", r.Body["tool_stream"])
	}
	if _, ok := r.Body["thinking"]; ok {
		t.Error("thinking was sent even though reasoning is not disabled")
	}
	include, _ := r.Body["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want [reasoning.encrypted_content]", r.Body["include"])
	}
	// Session-Id and Chat-Id identify the stream to the backend, and the
	// vendor reads `lang` to pick its own answer language.
	if r.Header.Get("Session-Id") != "conv-abc" {
		t.Errorf("Session-Id = %q, want the conversation id", r.Header.Get("Session-Id"))
	}
	if r.Header.Get("Chat-Id") == "" {
		t.Error("Chat-Id was not sent")
	}
	if r.Header.Get("lang") != "en" {
		t.Errorf("lang = %q, want en", r.Header.Get("lang"))
	}
	// The security token travels in the signature, not as a bearer token.
	if r.Auth == "" || !strings.HasPrefix(r.Auth, "SDK-HMAC-SHA256 Access=") {
		t.Fatalf("Authorization = %q, want an SDK-HMAC-SHA256 signature", r.Auth)
	}
	if r.Header.Get("x-security-token") != "TOKTESTACCOUNT" {
		t.Errorf("x-security-token = %q, want the credential's token", r.Header.Get("x-security-token"))
	}
	if r.Header.Get("Host") != "" {
		t.Errorf("an explicit Host header was sent (%q); the runtime must set it", r.Header.Get("Host"))
	}
}

// THE HEADER-ORDERING TRAP.  maas_type must be signed; Agent-Type and
// X-Language must not be, and must not be sent on the chat path at all.
func TestChatSignsTheBenefitHeaderOnlyForBenefitModels(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")

	for _, tc := range []struct {
		model       string
		wantBenefit bool
		why         string
	}{
		{"glm-5.3-flash", true, "a built-in free-quota model"},
		{"GLM-5.3-FLASH", true, "the match is case-insensitive, as the vendor's is"},
		{"deepseek-v4-pro", false, "a paid model"},
	} {
		before := len(f.chatRequests())
		st, err := c.Chat(context.Background(), &core.ChatRequest{
			Model:    tc.model,
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("Chat(%s): %v", tc.model, err)
		}
		st.Close()

		got := f.chatRequests()
		if len(got) != before+1 {
			t.Fatalf("Chat(%s) made %d calls, want 1", tc.model, len(got)-before)
		}
		r := got[len(got)-1]
		signed := signedHeaderList(t, r.Auth)

		if got := r.Header.Get("maas_type"); (got != "") != tc.wantBenefit {
			t.Errorf("Chat(%s): maas_type = %q, want present=%v (%s)", tc.model, got, tc.wantBenefit, tc.why)
		}
		if contains(signed, "maas_type") != tc.wantBenefit {
			t.Errorf("Chat(%s): SignedHeaders = %v, want maas_type signed=%v (%s)",
				tc.model, signed, tc.wantBenefit, tc.why)
		}
		// The mandatory base headers are signed on every request.
		if !contains(signed, "host") || !contains(signed, "x-sdk-date") || !contains(signed, "x-sdk-content-sha256") {
			t.Errorf("Chat(%s): SignedHeaders = %v, want host, x-sdk-date and x-sdk-content-sha256", tc.model, signed)
		}
		// content-type IS part of the signature (the signer always adds it
		// for a non-GET), but the signer's copy is never carried onto the
		// request: signedRequestHeaders drops it and the transport's own
		// value is sent instead.
		if !contains(signed, "content-type") {
			t.Errorf("Chat(%s): SignedHeaders = %v, want content-type signed", tc.model, signed)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Chat(%s): Content-Type = %q, want application/json", tc.model, got)
		}
		for _, name := range []string{"chat-id", "session-id", "lang", "agent-type", "x-language"} {
			if contains(signed, name) {
				t.Errorf("Chat(%s): %s was signed, but it must be appended AFTER signing", tc.model, name)
			}
		}
		// The chat endpoint does not take the PromptCenter pair; sending it
		// would be a key this module invented.
		if r.Header.Get("Agent-Type") != "" {
			t.Errorf("Chat(%s): Agent-Type = %q, but the chat path does not use it", tc.model, r.Header.Get("Agent-Type"))
		}
		if r.Header.Get("X-Language") != "" {
			t.Errorf("Chat(%s): X-Language = %q, but the chat path uses `lang` instead", tc.model, r.Header.Get("X-Language"))
		}
	}
}

// The harness fields are set before every attempt, and an omitted max_tokens is
// filled from the configured default rather than being left out.
func TestChatFillsTheDefaultMaxTokens(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, `,"max_tokens":4096`)

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	st.Close()

	got := f.chatRequests()
	if len(got) != 1 {
		t.Fatalf("the fixture saw %d chat calls, want 1", len(got))
	}
	if got[0].Body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want the configured 4096", got[0].Body["max_tokens"])
	}
}

// An assistant message without reasoning_content is a hard 400 from
// deepseek-v4, so the field must always be present.
func TestChatAlwaysSendsReasoningContentOnAssistantMessages(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model: "deepseek-v4-pro",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	st.Close()

	msgs, _ := f.chatRequests()[0].Body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2", msgs)
	}
	assistant, _ := msgs[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; !ok {
		t.Fatalf("the assistant message has no reasoning_content field: %v", assistant)
	}
}

// ---------------------------------------------------------------------------
// the stream
// ---------------------------------------------------------------------------

func TestChatStreamsDeltasToolCallsUsageAndDone(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	var (
		content   strings.Builder
		reasoning strings.Builder
		toolCalls []core.ToolCallDelta
		usage     *core.Usage
		finish    string
		sawDone   bool
	)
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			content.WriteString(ev.Delta)
			reasoning.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				t.Fatal("an EventToolCall arrived with no ToolCall")
			}
			toolCalls = append(toolCalls, *ev.ToolCall)
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			sawDone = true
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("the stream reported an error event: %v", ev.Err)
		}
	}

	if got := content.String(); got != "Hello, world" {
		t.Errorf("content = %q, want %q", got, "Hello, world")
	}
	if got := reasoning.String(); got != "let me think" {
		t.Errorf("reasoning = %q, want %q", got, "let me think")
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool calls = %v, want 1", toolCalls)
	}
	tc := toolCalls[0]
	if tc.Index != 0 || tc.ID != "call_1" || tc.Name != "lookup" || tc.Arguments != `{"q":1}` {
		t.Errorf("tool call = %+v, want the vendor's own values", tc)
	}
	if !sawDone {
		t.Fatal("the stream never produced a done event")
	}
	if finish != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", finish)
	}
	if usage == nil {
		t.Fatal("the stream produced no usage event")
	}
	if usage.PromptTokens != 10 || usage.CompletionTokens != 4 || usage.TotalTokens != 14 {
		t.Errorf("usage = %+v, want 10/4/14", usage)
	}
	if usage.CachedTokens != 6 {
		t.Errorf("cached tokens = %d, want 6", usage.CachedTokens)
	}
	if usage.ReasoningTokens != 2 {
		t.Errorf("reasoning tokens = %d, want 2", usage.ReasoningTokens)
	}

	// Recv returns io.EOF once at the clean end and then keeps reading clean
	// rather than panicking or blocking.
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after the end = %v, want io.EOF", err)
	}
}

// A stream that ends without [DONE] still ends cleanly: the vendor closes the
// body after the final frame.
func TestChatStreamEndingWithoutDoneIsClean(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeSSE(w, sse(mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "bye"}}},
		})))
	}
	c := f.client(t, "")

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	var content strings.Builder
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventDelta {
			content.WriteString(ev.Delta)
		}
	}
	if got := content.String(); got != "bye" {
		t.Fatalf("content = %q, want %q", got, "bye")
	}
}

// An error embedded in the stream is terminal and carries the vendor's own
// code, so the gateway can classify it.
func TestChatStreamErrorFrameIsTerminal(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeSSE(w, sse(
			mustJSON(map[string]any{"error_code": "TM.00001041", "error_msg": "peak usage, try again after 10:00"}),
		))
	}
	c := f.client(t, "")

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	_, err = st.Recv()
	if err == nil {
		t.Fatal("an error frame produced no error")
	}
	if !strings.Contains(err.Error(), "TM.00001041") {
		t.Errorf("err = %q, want the vendor's code carried through", err.Error())
	}
	// The error is delivered once, then the stream reads clean.
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after a terminal error = %v, want io.EOF", err)
	}
}

// A backend that goes quiet mid-answer must not hang the caller forever.
func TestChatStreamIdleTimeout(t *testing.T) {
	f := newChatFixture(t)
	release := make(chan struct{})
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeSSE(w, sse(mustJSON(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}},
		})))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-release
	}
	defer close(release)

	c := f.client(t, `,"chunk_timeout":"60ms"`)

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	// The first frame arrives; the next read is what times out.
	var got string
	for {
		ev, err := st.Recv()
		if err != nil {
			if !errors.Is(err, errIdleTimeout) {
				t.Fatalf("Recv = %v, want the idle timeout", err)
			}
			break
		}
		if ev.Type == core.EventDelta {
			got += ev.Delta
		}
	}
	if got != "partial" {
		t.Fatalf("content = %q, want the frame that did arrive", got)
	}
}

// ---------------------------------------------------------------------------
// refusal handling
// ---------------------------------------------------------------------------

// A concurrency-queue rejection is polled, not surfaced: the operator never
// sees an error for a model that is merely busy.
func TestChatQueueRejectionIsPolledThenRetried(t *testing.T) {
	f := newChatFixture(t)
	var queues int
	f.onQue = func(w http.ResponseWriter, _ *http.Request, n int) {
		queues = n
		writeJSONBody(w, http.StatusOK, map[string]any{"status": "working"})
	}
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, n int) {
		if n == 1 {
			writeJSONBody(w, http.StatusBadRequest, map[string]any{
				"error_code": "TM.00001041", "error_msg": "peak usage, try again after 10:00",
			})
			return
		}
		writeSSE(w, sse(
			mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}}}),
			"[DONE]",
		))
	}
	c := f.client(t, `,"queue_poll_interval":"1ms"`)

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	var got string
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventDelta {
			got += ev.Delta
		}
	}
	if got != "ok" {
		t.Fatalf("content = %q, want the retried answer", got)
	}
	if queues == 0 {
		t.Fatal("the queue-status endpoint was never polled")
	}
	if n := len(f.chatRequests()); n != 2 {
		t.Fatalf("the chat endpoint was called %d times, want 2", n)
	}
	// The queue-status call carries the unsigned pair the endpoint expects.
	var q *recordedRequest
	for _, r := range f.requests() {
		if r.Path == queueStatusPath {
			rr := r
			q = &rr
			break
		}
	}
	if q == nil {
		t.Fatal("no queue-status request was recorded")
	}
	if q.Header.Get("Agent-Type") != "INFERHUB_AGENT" {
		t.Errorf("Agent-Type = %q, want INFERHUB_AGENT", q.Header.Get("Agent-Type"))
	}
	if q.Header.Get("x-snap-traceid") == "" {
		t.Error("the queue-status request carries no trace id")
	}
	if contains(signedHeaderList(t, q.Auth), "x-snap-traceid") {
		t.Error("the trace id was signed; it must be appended after signing")
	}
	// The task id and model travel as query parameters.
	if !strings.Contains(q.Query, "task_id=") || !strings.Contains(q.Query, "model=") {
		t.Errorf("queue query = %q, want model and task_id", q.Query)
	}
}

// makeRefreshable attaches the refresh material a credential only ever gets
// from the sign-in flow.
//
// The config file deliberately has no key for the DPoP private key or the PKCE
// verifier — they are produced by the login handshake and persisted with the
// credential — so a credential that came from the config is never refreshable.
// A test that wants to exercise the refresh path has to put the material there
// the way login does.
func makeRefreshable(t *testing.T, c *Client, refreshToken string, key *dpopKeyPair) {
	t.Helper()
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	if len(c.pool.entries) == 0 {
		t.Fatal("the pool has no entry to make refreshable")
	}
	c.pool.entries[0].acct.RefreshToken = refreshToken
	c.pool.entries[0].acct.DpopPrivateJwk = key.PrivateJwk
}

// An auth refusal mints a fresh credential once and retries on the same
// account, because the stated expiry cannot be trusted.
func TestChatAuthFailureRefreshesOnceThenRetries(t *testing.T) {
	sts, cap := newSTSServer(t, http.StatusOK, `{"credentials":{
		"access_key_id":"AKNEW","secret_access_key":"SKNEW","security_token":"TOKNEW",
		"expiration":"2031-01-02T03:04:05Z"},"refresh_token":"RTNEW"}`)

	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, req recordedRequest, n int) {
		if n == 1 {
			writeJSONBody(w, http.StatusUnauthorized, map[string]any{
				"error_code": "APIG.0301", "error_msg": "Invalid token",
			})
			return
		}
		if req.Header.Get("x-security-token") != "TOKNEW" {
			t.Errorf("the retry used security token %q, want the refreshed one", req.Header.Get("x-security-token"))
		}
		writeSSE(w, sse(
			mustJSON(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}}}),
			"[DONE]",
		))
	}

	key, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	c := f.client(t, `,"sts_url":`+jsonString(sts.URL))
	makeRefreshable(t, c, "RTOLD", key)

	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	var got string
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == core.EventDelta {
			got += ev.Delta
		}
	}
	if got != "ok" {
		t.Fatalf("content = %q, want the retried answer", got)
	}
	if cap.calls != 1 {
		t.Fatalf("the token endpoint was called %d times, want exactly 1", cap.calls)
	}
	if n := len(f.chatRequests()); n != 2 {
		t.Fatalf("the chat endpoint was called %d times, want 2", n)
	}
	// The refreshed credential is live, not just used once.
	if a := c.pool.all()[0].account(); a.SecurityToken != "TOKNEW" {
		t.Errorf("the stored credential still carries %q, want the refreshed token", a.SecurityToken)
	}
}

// A request the vendor considers malformed is this module's own bug: it must
// not be retried on another account, and it must not cool the account down.
func TestChatClientErrorIsTerminalAndDoesNotCoolTheAccount(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeJSONBody(w, http.StatusBadRequest, map[string]any{
			"error_code": "InferHub.002002009", "error_msg": "model is not registered",
		})
	}
	c := f.client(t, `,"max_attempts":3`)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "ghost-model",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a malformed request reported success")
	}
	// It is a typed failure so the gateway can classify it, and it names the
	// account that produced it.
	fl, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v (%T), want a *core.Failure", err, err)
	}
	if fl.Client != "codearts" {
		t.Errorf("Failure.Client = %q, want codearts", fl.Client)
	}
	if fl.Kind != core.FailureOther {
		t.Errorf("Failure.Kind = %q, want %q: a 400 is not retryable", fl.Kind, core.FailureOther)
	}
	if fl.Status != 400 {
		t.Errorf("Failure.Status = %d, want 400", fl.Status)
	}
	// One attempt only: repeating a malformed request on another account would
	// just repeat the mistake.
	if n := len(f.chatRequests()); n != 1 {
		t.Fatalf("the chat endpoint was called %d times, want 1", n)
	}
	if !c.pool.ready() {
		t.Error("the account was parked for a client error; a bad request says nothing about the credential")
	}
}

// A quota refusal is retryable and is attributed to the account.
func TestChatQuotaRefusalIsRetryable(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		writeJSONBody(w, http.StatusTooManyRequests, map[string]any{
			"error_code": "APIG.0308", "error_msg": "too many requests",
		})
	}
	c := f.client(t, `,"max_attempts":1`)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("a 429 reported success")
	}
	fl, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %v, want a *core.Failure", err)
	}
	if fl.Kind != core.FailureQuota {
		t.Errorf("Failure.Kind = %q, want %q", fl.Kind, core.FailureQuota)
	}
	if !core.Retryable(fl.Kind) {
		t.Error("a quota refusal must be retryable so the gateway can rotate accounts")
	}
	if fl.AccountID() == "" {
		t.Error("the failure does not name the account it came from")
	}
}

// The account is marked served on the request, which is how the gateway
// attributes usage.  The field must not reach the vendor.
func TestChatNotesTheServedByAccount(t *testing.T) {
	f := newChatFixture(t)
	c := f.client(t, "")

	var served string
	req := &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	}
	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	st.Close()

	if served == "" {
		t.Fatal("ServedBy was not set, so usage cannot be attributed")
	}
	if served != c.pool.all()[0].id() {
		t.Errorf("ServedBy = %q, want the account that served the request", served)
	}
	if _, ok := f.chatRequests()[0].Body["served_by"]; ok {
		t.Error("served_by was sent upstream")
	}
}

// Every attempt must set the attribution slot, not just the first.
//
// Two accounts are required: the retry loop marks an account as tried before
// its first attempt, so a single-account pool has nothing left to pick and the
// call ends in an error rather than a second attempt.
func TestChatNotesTheAccountOnEveryAttempt(t *testing.T) {
	f := newChatFixture(t)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, n int) {
		if n == 1 {
			writeJSONBody(w, http.StatusTooManyRequests, map[string]any{"error_msg": "too many requests"})
			return
		}
		writeSSE(w, sse("[DONE]"))
	}
	c := f.client(t, `,"max_attempts":2,"accounts":[
		{"label":"first","access_key_id":"AK1","secret_access_key":"SK1","security_token":"TOK1"},
		{"label":"second","access_key_id":"AK2","secret_access_key":"SK2","security_token":"TOK2"}
	]`)

	served := "stale"
	req := &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	}
	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	st.Close()

	got := f.chatRequests()
	if len(got) != 2 {
		t.Fatalf("the chat endpoint was called %d times, want 2", len(got))
	}
	// The two attempts really did come from two different credentials, which is
	// what makes the attribution assertion below meaningful.
	if got[0].Header.Get("x-security-token") == got[1].Header.Get("x-security-token") {
		t.Fatalf("both attempts used security token %q, so only one account was tried",
			got[0].Header.Get("x-security-token"))
	}
	ids := map[string]bool{}
	for _, e := range c.pool.all() {
		ids[e.id()] = true
	}
	if served == "stale" || served == "" {
		t.Fatalf("ServedBy = %q, want the retrying account's id", served)
	}
	if !ids[served] {
		t.Fatalf("ServedBy = %q, which is not one of this module's accounts (%v)", served, ids)
	}
}

// The whole point of the timeout is that a caller can bound a stream.
func TestChatHonoursTheCallerContext(t *testing.T) {
	f := newChatFixture(t)
	release := make(chan struct{})
	defer close(release)
	f.onCha = func(w http.ResponseWriter, _ *http.Request, _ recordedRequest, _ int) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-release
	}
	c := f.client(t, `,"first_token_timeout":"60ms"`)

	// Keep the caller deadline comfortably above the 60ms watchdog. A shared
	// CI runner can delay timer scheduling under load; a tight 2s deadline made
	// that scheduling delay look like a cancelled caller instead of idle timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer st.Close()

	// No frame ever arrives, so the first-token watchdog must fire.
	if _, err := st.Recv(); !errors.Is(err, errIdleTimeout) {
		t.Fatalf("Recv = %v, want the first-token idle timeout", err)
	}
}
