package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- SSE fixtures ---------------------------------------------------------

const sseBasic = `data: {"id":"gen-1","object":"chat.completion.chunk","model":"anthropic/claude-sonnet-4.5","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"completion_tokens_details":{"reasoning_tokens":3},"prompt_tokens_details":{"cached_tokens":5}}}

data: [DONE]

`

// sseReasoning uses the vendor's own field name for thinking text.  OpenRouter
// sends `reasoning`, NOT the `reasoning_content` other vendors use.
const sseReasoning = `data: {"id":"gen-1","choices":[{"index":0,"delta":{"reasoning":"let me think"},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":"42"},"finish_reason":"length"}]}

data: [DONE]

`

// sseTools streams one tool call in three fragments, the way a real provider
// does: the name arrives first, the arguments JSON dribbles in afterwards.
const sseTools = `data: {"id":"gen-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"ci"}}]},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"gen-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// sseNoDone just stops: a clean upstream close with no [DONE] sentinel is a
// normal end of stream, not a failure.
const sseNoDone = `data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":"bye"},"finish_reason":"stop"}]}

`

const sseError = `data: {"id":"gen-1","error":{"code":429,"message":"Rate limit exceeded: free-models-per-day","metadata":{"error_type":"rate_limit_exceeded"}}}

`

// --- transport helpers ----------------------------------------------------

// newClientWithTransport builds a client over an arbitrary transport, for the
// tests that need a body which does not come from a string.
func newClientWithTransport(t *testing.T, cfg string, rt http.RoundTripper) *Client {
	t.Helper()
	t.Setenv(envAPIKey, "")
	cl, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(cfg),
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	c := cl.(*Client)
	c.now = func() time.Time { return testBase }
	return c
}

// stallBody blocks until it is closed (what net/http does when a read is
// interrupted), or until the request context is cancelled.
type stallBody struct {
	ctx     context.Context
	once    sync.Once
	release chan struct{}
}

func newStallBody(ctx context.Context) *stallBody {
	return &stallBody{ctx: ctx, release: make(chan struct{})}
}

func (b *stallBody) Read(p []byte) (int, error) {
	if b.ctx != nil {
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-b.release:
			return 0, errors.New("http: read on closed response body")
		}
	}
	<-b.release
	return 0, errors.New("http: read on closed response body")
}

func (b *stallBody) Close() error {
	b.once.Do(func() { close(b.release) })
	return nil
}

// stallTransport hands out a body that never produces a frame.
func stallTransport() http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Proto:      "HTTP/1.1",
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newStallBody(r.Context()),
			Request:    r,
		}, nil
	})
}

func simpleReq(model string) *core.ChatRequest {
	return &core.ChatRequest{
		Model:    model,
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
}

// --- request body ---------------------------------------------------------

func TestBuildChatBodyRejectsWhatItCannotExpress(t *testing.T) {
	if _, err := buildChatBody(Config{}, nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("nil request = %v, want ErrUnsupported", err)
	}
	if _, err := buildChatBody(Config{}, &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "x"}}}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("empty model = %v, want ErrUnsupported", err)
	}
	_, err := buildChatBody(Config{}, &core.ChatRequest{Model: "   "})
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "anthropic/claude-sonnet-4.5") {
		t.Fatalf("no messages = %v, want an ErrUnsupported naming an example id", err)
	}
	if _, err := buildChatBody(Config{}, &core.ChatRequest{Model: "m"}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("no messages = %v, want ErrUnsupported", err)
	}
}

func TestBuildChatBodyAlwaysStreams(t *testing.T) {
	raw, err := buildChatBody(Config{}, simpleReq("anthropic/claude-sonnet-4.5"))
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["stream"] != true {
		t.Fatalf("stream = %v, want true (the upstream is always streamed)", body["stream"])
	}
	if body["model"] != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("model = %v", body["model"])
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatal("max_tokens must not be sent when the caller did not ask for one")
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatal("max_completion_tokens must not be sent when the caller did not ask for one")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", body["messages"])
	}
}

func TestBuildChatBodyMaxTokensField(t *testing.T) {
	limit := 4096
	req := simpleReq("m")
	req.MaxTokens = &limit

	raw, err := buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	body := map[string]any{}
	_ = json.Unmarshal(raw, &body)
	if body["max_completion_tokens"] != float64(4096) {
		t.Fatalf("body = %v, want max_completion_tokens (max_tokens is deprecated)", body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatal("both spellings were sent at once")
	}

	cfg, _ := parseConfig(json.RawMessage(`{"max_tokens_field":"max_tokens"}`))
	raw, err = buildChatBody(cfg, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	body = map[string]any{}
	_ = json.Unmarshal(raw, &body)
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("body = %v, want the legacy max_tokens field when asked", body)
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatal("both spellings were sent at once")
	}

	// A non-positive limit means "the caller did not ask", not "send zero".
	zero := 0
	req.MaxTokens = &zero
	raw, _ = buildChatBody(Config{}, req)
	body = map[string]any{}
	_ = json.Unmarshal(raw, &body)
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("body = %v, want no limit field for a zero limit", body)
	}
}

func TestBuildChatBodyMessageShapes(t *testing.T) {
	req := &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{
			{Content: "no role"},
			{Role: " assistant ", Content: "  hi  "},
			{Role: "tool", ToolCallID: " call_1 ", Name: " get_weather ", Content: "{}"},
			{Role: "assistant", ToolCalls: []core.ToolCall{
				{Name: "", Arguments: "{}"},
				{ID: "call_2", Name: "lookup", Arguments: `{"q":1}`},
			}},
		},
	}
	raw, err := buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    any    `json:"content"`
			Name       string `json:"name"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 4 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	if body.Messages[0].Role != "user" {
		t.Fatalf("an empty role must default to user, got %q", body.Messages[0].Role)
	}
	// The role is trimmed, but a plain string content travels verbatim:
	// rewriting the caller's own text is not this module's business.  (Part
	// texts ARE trimmed, because they are re-joined into one string.)
	if body.Messages[1].Role != "assistant" || body.Messages[1].Content != "  hi  " {
		t.Fatalf("message 1 = %+v, want a trimmed role and untouched content", body.Messages[1])
	}
	if body.Messages[2].Name != "get_weather" || body.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("message 2 = %+v", body.Messages[2])
	}
	calls := body.Messages[3].ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool calls = %+v, want the nameless one dropped", calls)
	}
	if calls[0].ID != "call_2" || calls[0].Type != "function" || calls[0].Function.Name != "lookup" {
		t.Fatalf("call = %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"q":1}` {
		t.Fatalf("arguments = %q, want them passed through verbatim", calls[0].Function.Arguments)
	}
}

func TestBuildChatBodyContentParts(t *testing.T) {
	// Text-only parts are flattened to a string: every provider accepts one.
	req := &core.ChatRequest{Model: "m", Messages: []core.Message{{
		Role:  "user",
		Parts: []core.ContentPart{{Type: "text", Text: " one "}, {Type: "text", Text: "two"}},
	}}}
	raw, err := buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &body)
	if got, ok := body.Messages[0].Content.(string); !ok || got != "one\ntwo" {
		t.Fatalf("content = %#v, want the joined string", body.Messages[0].Content)
	}

	// An image switches the message to the multimodal array form.
	req = &core.ChatRequest{Model: "m", Messages: []core.Message{{
		Role: "user",
		Parts: []core.ContentPart{
			{Type: "text", Text: "what is this"},
			{Type: "image_url", ImageURL: "https://example.test/a.png", Detail: "high"},
		},
	}}}
	raw, err = buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var partsBody struct {
		Messages []struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL *struct {
					URL    string `json:"url"`
					Detail string `json:"detail"`
				} `json:"image_url"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &partsBody); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parts := partsBody.Messages[0].Content
	if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "image_url" {
		t.Fatalf("content = %+v", parts)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.test/a.png" || parts[1].ImageURL.Detail != "high" {
		t.Fatalf("image part = %+v", parts[1].ImageURL)
	}

	// An empty image_url is not an image: it must not trigger the array form.
	req = &core.ChatRequest{Model: "m", Messages: []core.Message{{
		Role:  "user",
		Parts: []core.ContentPart{{Type: "image_url"}, {Type: "text", Text: "plain"}},
	}}}
	raw, _ = buildChatBody(Config{}, req)
	_ = json.Unmarshal(raw, &body)
	if got, ok := body.Messages[0].Content.(string); !ok || got != "plain" {
		t.Fatalf("content = %#v, want the plain string form", body.Messages[0].Content)
	}
}

func TestBuildChatBodyToolsAndToolChoice(t *testing.T) {
	req := simpleReq("m")
	req.Tools = []core.Tool{
		{Name: "", Description: "nameless", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "lookup", Description: "d"},
		{Name: "other", Type: "function", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)},
	}
	req.ToolChoice = json.RawMessage(`"auto"`)
	raw, err := buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Tools) != 2 {
		t.Fatalf("tools = %+v, want the nameless one dropped", body.Tools)
	}
	if body.Tools[0].Function.Name != "lookup" || body.Tools[0].Type != "function" {
		t.Fatalf("tool 0 = %+v", body.Tools[0])
	}
	// A tool with no schema still needs a valid one: several providers reject
	// an empty parameters object outright.
	if string(body.Tools[0].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("parameters = %s", body.Tools[0].Function.Parameters)
	}
	if string(body.Tools[1].Function.Parameters) != `{"type":"object","properties":{"q":{"type":"string"}}}` {
		t.Fatalf("parameters = %s, want them passed through", body.Tools[1].Function.Parameters)
	}
	if string(body.ToolChoice) != `"auto"` {
		t.Fatalf("tool_choice = %s", body.ToolChoice)
	}

	// With no tools there is no tool_choice either: the vendor rejects one.
	req = simpleReq("m")
	req.ToolChoice = json.RawMessage(`"auto"`)
	raw, _ = buildChatBody(Config{}, req)
	if strings.Contains(string(raw), "tool_choice") {
		t.Fatalf("body = %s, want no tool_choice without tools", raw)
	}
}

func TestBuildChatBodyOptionAllowlist(t *testing.T) {
	req := simpleReq("m")
	req.Options = map[string]any{
		"presence_penalty":    0.5,
		"frequency_penalty":   "0.25",
		"top_k":               40,
		"seed":                7,
		"parallel_tool_calls": false,
		"reasoning_effort":    "high",
		"provider":            map[string]any{"sort": "price"},
		"models":              []string{"a/b"},
	}
	raw, err := buildChatBody(Config{}, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["presence_penalty"] != 0.5 || body["frequency_penalty"] != 0.25 {
		t.Fatalf("penalties = %v / %v", body["presence_penalty"], body["frequency_penalty"])
	}
	if body["top_k"] != float64(40) || body["seed"] != float64(7) {
		t.Fatalf("top_k/seed = %v / %v", body["top_k"], body["seed"])
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v", body["parallel_tool_calls"])
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", body["reasoning_effort"])
	}
	// A caller must not be able to smuggle arbitrary vendor fields through.
	if _, ok := body["provider"]; ok {
		t.Fatal("provider was forwarded; the option allowlist is not enforced")
	}
	if _, ok := body["models"]; ok {
		t.Fatal("models was forwarded; the option allowlist is not enforced")
	}
}

func TestNormalizeToolChoiceDropsEmptyForms(t *testing.T) {
	for _, raw := range []string{"", "null", `""`, `"   "`} {
		if got := normalizeToolChoice(json.RawMessage(raw)); got != nil {
			t.Fatalf("normalizeToolChoice(%q) = %s, want nil", raw, got)
		}
	}
	forced := `{"type":"function","function":{"name":"lookup"}}`
	if got := normalizeToolChoice(json.RawMessage(forced)); string(got) != forced {
		t.Fatalf("normalizeToolChoice = %s, want a forced tool passed through", got)
	}
}

func TestNormalizeFinishReasons(t *testing.T) {
	cases := map[string]string{
		"stop":           "stop",
		"end_turn":       "stop",
		"":               "stop",
		"error":          "stop", // the failure itself travels as an EventError
		"tool_calls":     "tool_calls",
		"tool_call":      "tool_calls",
		"function_call":  "tool_calls",
		"length":         "length",
		"max_tokens":     "length",
		"content_filter": "content_filter",
		"something-new":  "stop",
	}
	for in, want := range cases {
		if got := normalizeFinish(in); got != want {
			t.Fatalf("normalizeFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlattenContentShapes(t *testing.T) {
	cases := map[string]string{
		`""`:      "",
		`null`:    "",
		`"hello"`: "hello",
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`: "ab",
		`[{"type":"text","text":"a"}]`:                            "a",
		`{"unexpected":true}`:                                     "",
	}
	for in, want := range cases {
		if got := flattenContent(json.RawMessage(in)); got != want {
			t.Fatalf("flattenContent(%s) = %q, want %q", in, got, want)
		}
	}
	if got := flattenContent(nil); got != "" {
		t.Fatalf("flattenContent(nil) = %q", got)
	}
}

func TestUsageToCoreFillsTotals(t *testing.T) {
	var nilUsage *oaiUsage
	if got := nilUsage.toCore(); got != nil {
		t.Fatalf("a nil usage must stay nil, got %+v", got)
	}
	raw := `{"prompt_tokens":10,"completion_tokens":4,"completion_tokens_details":{"reasoning_tokens":2},"prompt_tokens_details":{"cached_tokens":3}}`
	var u oaiUsage
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := u.toCore()
	if got.PromptTokens != 10 || got.CompletionTokens != 4 || got.TotalTokens != 14 {
		t.Fatalf("usage = %+v, want the total derived when the vendor omits it", got)
	}
	if got.ReasoningTokens != 2 || got.CachedTokens != 3 {
		t.Fatalf("usage = %+v, want the detail blocks read", got)
	}

	// The vendor's explicit total wins over the derived one.
	var u2 oaiUsage
	_ = json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":4,"total_tokens":99}`), &u2)
	if got := u2.toCore(); got.TotalTokens != 99 {
		t.Fatalf("total = %d, want the vendor's own number", got.TotalTokens)
	}
}

func TestOptionCoercion(t *testing.T) {
	opts := map[string]any{
		"f": 1.5, "f2": json.Number("2.5"), "f3": "3.5", "f4": 4, "f5": "abc",
		"i": 12, "i2": "13", "i3": 1.9, "i4": "x",
		"b": true, "b2": "yes", "b3": "0", "b4": "maybe", "b5": false,
		"s": "high", "s2": "  ", "s3": 5,
	}
	if v, ok := optionFloat(opts, "f"); !ok || v != 1.5 {
		t.Fatalf("optionFloat(f) = (%v, %v)", v, ok)
	}
	if v, ok := optionFloat(opts, "f2"); !ok || v != 2.5 {
		t.Fatalf("optionFloat(f2) = (%v, %v)", v, ok)
	}
	if v, ok := optionFloat(opts, "f3"); !ok || v != 3.5 {
		t.Fatalf("optionFloat(f3) = (%v, %v)", v, ok)
	}
	if v, ok := optionFloat(opts, "f4"); !ok || v != 4 {
		t.Fatalf("optionFloat(f4) = (%v, %v)", v, ok)
	}
	if _, ok := optionFloat(opts, "f5"); ok {
		t.Fatal("optionFloat accepted a non-number")
	}
	if _, ok := optionFloat(nil, "f"); ok {
		t.Fatal("optionFloat accepted a nil map")
	}
	if v, ok := optionInt(opts, "i"); !ok || v != 12 {
		t.Fatalf("optionInt(i) = (%v, %v)", v, ok)
	}
	if v, ok := optionInt(opts, "i2"); !ok || v != 13 {
		t.Fatalf("optionInt(i2) = (%v, %v)", v, ok)
	}
	if v, ok := optionInt(opts, "i3"); !ok || v != 1 {
		t.Fatalf("optionInt(i3) = (%v, %v), want it truncated", v, ok)
	}
	if _, ok := optionInt(opts, "i4"); ok {
		t.Fatal("optionInt accepted a non-number")
	}
	if v, ok := optionBool(opts, "b"); !ok || !v {
		t.Fatalf("optionBool(b) = (%v, %v)", v, ok)
	}
	if v, ok := optionBool(opts, "b2"); !ok || !v {
		t.Fatalf("optionBool(b2) = (%v, %v)", v, ok)
	}
	if v, ok := optionBool(opts, "b3"); !ok || v {
		t.Fatalf("optionBool(b3) = (%v, %v)", v, ok)
	}
	if v, ok := optionBool(opts, "b5"); !ok || v {
		t.Fatalf("optionBool(b5) = (%v, %v), want an explicit false to be a value", v, ok)
	}
	if _, ok := optionBool(opts, "b4"); ok {
		t.Fatal("optionBool accepted \"maybe\"")
	}
	if v, ok := optionString(opts, "s"); !ok || v != "high" {
		t.Fatalf("optionString(s) = (%q, %v)", v, ok)
	}
	if _, ok := optionString(opts, "s2"); ok {
		t.Fatal("optionString accepted a blank string")
	}
	if _, ok := optionString(opts, "s3"); ok {
		t.Fatal("optionString accepted a number")
	}
}

// --- the SSE reader -------------------------------------------------------

func TestSSEReaderToleratesVendorFraming(t *testing.T) {
	// A comment line, a `data:` with no space, CRLF endings, an unknown field
	// and a frame with no terminating blank line.
	in := ": keep-alive\r\n" +
		"\r\n" +
		"data:one\r\n" +
		"\r\n" +
		"event: ping\n" +
		"id: 7\n" +
		"retry: 100\n" +
		"data: two\n" +
		"\n" +
		"data: last"
	r := newSSEReader(strings.NewReader(in))

	f1, err := r.next()
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if f1.Data != "one" {
		t.Fatalf("frame 1 = %+v, want a data line with no space after the colon", f1)
	}
	f2, err := r.next()
	if err != nil {
		t.Fatalf("frame 2: %v", err)
	}
	if f2.Data != "two" || f2.Event != "ping" {
		t.Fatalf("frame 2 = %+v", f2)
	}
	f3, err := r.next()
	if err != nil {
		t.Fatalf("frame 3: %v, want the unterminated last frame", err)
	}
	if f3.Data != "last" {
		t.Fatalf("frame 3 = %+v", f3)
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("after the last frame = %v, want io.EOF", err)
	}
}

func TestSSEReaderJoinsMultiLineData(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: a\ndata: b\n\ndata: c\n\n"))
	f1, err := r.next()
	if err != nil {
		t.Fatalf("frame 1: %v", err)
	}
	if f1.Data != "a\nb" {
		t.Fatalf("frame 1 = %q, want the data lines joined with a newline", f1.Data)
	}
	f2, _ := r.next()
	if f2.Data != "c" {
		t.Fatalf("frame 2 = %q", f2.Data)
	}
}

func TestSSEReaderEOFWhenExhausted(t *testing.T) {
	for _, in := range []string{"", "\n\n", ": only a comment\n\n"} {
		r := newSSEReader(strings.NewReader(in))
		f, err := r.next()
		if err != io.EOF {
			t.Fatalf("next() on %q = (%+v, %v), want io.EOF", in, f, err)
		}
	}
}

func TestSSEReaderTreatsBareJSONAsData(t *testing.T) {
	// A vendor that ignored `stream:true` answers with one whole JSON object.
	r := newSSEReader(strings.NewReader(`{"choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n"))
	f, err := r.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if !strings.HasPrefix(f.Data, `{"choices"`) {
		t.Fatalf("frame = %+v, want the bare JSON treated as data", f)
	}
}

func TestIsDonePayload(t *testing.T) {
	for _, in := range []string{"[DONE]", " [done] ", "\t[Done]\n"} {
		if !isDonePayload(in) {
			t.Fatalf("isDonePayload(%q) = false", in)
		}
	}
	for _, in := range []string{"", "done", `"[DONE]"`, "{}"} {
		if isDonePayload(in) {
			t.Fatalf("isDonePayload(%q) = true", in)
		}
	}
}

// --- the stream -----------------------------------------------------------

func TestChatStreamEmitsDeltasUsageAndDone(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("anthropic/claude-sonnet-4.5"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, reasoning, calls, usage, finish, err := drainStream(st)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q", text)
	}
	if reasoning != "" || len(calls) != 0 {
		t.Fatalf("reasoning = %q, calls = %+v", reasoning, calls)
	}
	if usage == nil || usage.PromptTokens != 11 || usage.CompletionTokens != 7 || usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.ReasoningTokens != 3 || usage.CachedTokens != 5 {
		t.Fatalf("usage = %+v, want the detail blocks", usage)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
	// A stream that ended with [DONE] and no error must have cleared the park.
	if rec := c.mustByID(t, accountIDFor(testKey)); rec.LastError != "" {
		t.Fatalf("a clean stream left an error note: %q", rec.LastError)
	}
}

func TestChatStreamUsesTheVendorReasoningField(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseReasoning} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, reasoning, _, _, finish, err := drainStream(st)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if reasoning != "let me think" {
		t.Fatalf("reasoning = %q, want the vendor's own `reasoning` field", reasoning)
	}
	if text != "42" {
		t.Fatalf("text = %q", text)
	}
	if finish != "length" {
		t.Fatalf("finish = %q, want the length mapping", finish)
	}
}

func TestChatStreamMergesToolCallFragments(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseTools} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, calls, _, finish, err := drainStream(st)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one merged call", calls)
	}
	if calls[0].Name != "get_weather" || calls[0].ID != "call_1" {
		t.Fatalf("call = %+v", calls[0])
	}
	// The arguments arrive in three fragments and must be CONCATENATED, never
	// overwritten: taking the last fragment alone would drop most of the JSON.
	if calls[0].Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q, want the fragments joined", calls[0].Arguments)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatStreamCleanCloseWithoutDoneIsNormal(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseNoDone} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, finish, err := drainStream(st)
	if err != nil {
		t.Fatalf("a missing [DONE] must not be an error, got %v", err)
	}
	if text != "bye" || finish != "stop" {
		t.Fatalf("text = %q, finish = %q", text, finish)
	}
}

func TestChatStreamErrorChunkBecomesAnEventError(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseError} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, _, _, _, err = drainStream(st)
	if err == nil {
		t.Fatal("an error chunk inside the stream must surface as an error")
	}
	// "free-models-per-day" is the free-tier DAILY cap, which the classifier
	// deliberately ranks above the plain rate-limit markers: retrying in two
	// minutes would only be refused again, so it parks until the cap resets.
	if core.FailureKindOf(err) != core.FailureQuota {
		t.Fatalf("kind = %q, want quota for the free-tier daily cap", core.FailureKindOf(err))
	}
	if !strings.Contains(err.Error(), "Rate limit exceeded") {
		t.Fatalf("err = %v, want the vendor's message", err)
	}
	// A 429 inside the stream is capacity evidence: the credential is parked.
	rec := c.mustByID(t, accountIDFor(testKey))
	if rec.CooldownUntil == "" {
		t.Fatalf("the credential was not parked after a rate-limit chunk: %+v", rec)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("acquire = %v, want ErrBusy while parked", err)
	}
}

func TestChatStreamPlainRateLimitIsNotADailyQuota(t *testing.T) {
	// The precedence that matters: a 429 with no free-tier marker is a plain
	// rate limit (parked for rate_cooldown), NOT the daily cap (parked until
	// midnight).  Getting this backwards would bench a healthy key for hours.
	payload := `data: {"id":"gen-1","choices":[],"error":{"code":429,"message":"Too many requests"}}` + "\n\n"
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: payload} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"rate_cooldown":"3m","free_only":false}`, testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, _, _, _, err = drainStream(st)
	if err == nil {
		t.Fatal("expected an error event")
	}
	if core.FailureKindOf(err) != core.FailureRateLimited {
		t.Fatalf("kind = %q, want rate_limited", core.FailureKindOf(err))
	}
	rec := c.mustByID(t, accountIDFor(testKey))
	until, perr := time.Parse(time.RFC3339, rec.CooldownUntil)
	if perr != nil {
		t.Fatalf("cooldown = %q, want a timestamp: %v", rec.CooldownUntil, perr)
	}
	if got := until.Sub(c.now()); got != 3*time.Minute {
		t.Fatalf("cooldown = %s, want the configured 3m rate_cooldown, not a midnight park", got)
	}
}

func TestChatStreamIdleTimeoutIsReported(t *testing.T) {
	c := newClientWithTransport(t, fmt.Sprintf(`{"api_key":%q,"stream_idle_timeout":"25ms","free_only":false}`, testKey), stallTransport())
	if got := c.cfg.streamIdle(); got != 25*time.Millisecond {
		t.Fatalf("streamIdle = %s", got)
	}
	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	start := time.Now()
	_, err = st.Recv()
	if err == nil {
		t.Fatal("a stalled stream must fail rather than hang forever")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the idle watchdog took %s to fire", elapsed)
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v, want it to say the stream stalled", err)
	}
	if core.FailureKindOf(err) != core.FailureUpstream {
		t.Fatalf("kind = %q, want upstream", core.FailureKindOf(err))
	}
	// A stalled connection says nothing about the key, so the credential must
	// NOT be parked: only the note records it.
	rec := c.mustByID(t, accountIDFor(testKey))
	if rec.CooldownUntil != "" {
		t.Fatalf("a stalled stream parked the credential: %+v", rec)
	}
	if !strings.Contains(rec.Note, "stalled") {
		t.Fatalf("note = %q, want the stall recorded", rec.Note)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 0 {
		t.Fatalf("in-flight = %d, want the slot returned after a failure", inFlight)
	}
}

func TestChatStreamHonoursContextCancellation(t *testing.T) {
	c := newClientWithTransport(t, fmt.Sprintf(`{"api_key":%q,"stream_idle_timeout":"10m","free_only":false}`, testKey), stallTransport())
	ctx, cancel := context.WithCancel(context.Background())
	st, err := c.Chat(ctx, simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := st.Recv()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Recv = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv did not return promptly after the caller cancelled")
	}
}

func TestChatStreamCloseIsIdempotentAndReleases(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"max_in_flight":1,"free_only":false}`, testKey), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 1 {
		t.Fatalf("in-flight = %d, want the slot held while the stream is open", inFlight)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v, want an idempotent close", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 0 {
		t.Fatalf("in-flight = %d, want the slot returned exactly once", inFlight)
	}
	if _, err := st.Recv(); err != io.EOF {
		t.Fatalf("Recv after Close = %v, want io.EOF", err)
	}
	// The slot really is free: the same credential can be taken again.
	if _, err := c.pool.acquire(c.now(), c.limit()); err != nil {
		t.Fatalf("acquire after Close: %v", err)
	}
}

func TestChatStreamKeepsASlotItNeverTook(t *testing.T) {
	// The panel's Test button probes a credential WITHOUT taking a pool slot.
	// If the probe's Close() returned one anyway, a busy gateway would leak
	// slots until every credential looked full.
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"max_in_flight":1,"free_only":false}`, testKey), up)
	rec, err := c.pool.acquire(c.now(), c.limit())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	body, err := buildChatBody(c.cfg, simpleReq("m"))
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}

	st, err := c.doChat(context.Background(), rec, body, false)
	if err != nil {
		t.Fatalf("doChat(holds=false): %v", err)
	}
	if _, _, _, _, _, err := drainStream(st); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 1 {
		t.Fatalf("in-flight = %d, want the caller's slot untouched by a probe", inFlight)
	}

	// The same call on the real path holds a slot and gives it back on Close.
	// doChat never ACQUIRES: the rotate loop in Chat does that, and the stream
	// only remembers whether it may release.
	body, _ = buildChatBody(c.cfg, simpleReq("m"))
	st, err = c.doChat(context.Background(), rec, body, true)
	if err != nil {
		t.Fatalf("doChat(holds=true): %v", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 1 {
		t.Fatalf("in-flight = %d, want the caller's slot still held", inFlight)
	}
	if _, _, _, _, _, err := drainStream(st); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 0 {
		t.Fatalf("in-flight = %d, want the real path's slot returned", inFlight)
	}
}

func TestChatStreamReadsASingleObjectResponse(t *testing.T) {
	// The vendor streamed anyway, but a non-SSE answer must not hang the
	// reader: sseReader treats a bare JSON line as data.
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{body: `{"id":"gen-1","choices":[{"index":0,"message":{"role":"assistant","content":"one shot"},"finish_reason":"stop"}]}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)
	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, finish, err := drainStream(st)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if text != "one shot" {
		t.Fatalf("text = %q, want the non-streaming message read", text)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
}

// --- Chat -----------------------------------------------------------------

func TestChatRejectsAnUnsupportedRequestWithoutTakingAnAccount(t *testing.T) {
	up := &fakeUpstream{}
	c := newTestClient(t, cfgWithKey(testKey), up)
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "m"}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported (the gateway answers 400)", err)
	}
	if up.count() != 0 {
		t.Fatal("an unexpressible request reached the vendor")
	}
	if inFlight, _ := c.pool.stats(c.limit()); inFlight != 0 {
		t.Fatal("an unexpressible request took a pool slot")
	}
}

func TestChatWithoutACredentialIsNotConfigured(t *testing.T) {
	up := &fakeUpstream{}
	c := newTestClient(t, `{"free_only":false}`, up)
	if _, err := c.Chat(context.Background(), simpleReq("m")); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured (the gateway answers 503)", err)
	}
	if up.count() != 0 {
		t.Fatal("a credential-less module still called the vendor")
	}
}

func TestChatReportsBusyWhenEveryCredentialIsParked(t *testing.T) {
	up := &fakeUpstream{}
	c := newTestClient(t, cfgWithKey(testKey), up)
	c.pool.setCooldown(accountIDFor(testKey), c.now().Add(time.Hour), "429")
	if _, err := c.Chat(context.Background(), simpleReq("m")); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy (the gateway answers 429 + Retry-After)", err)
	}
	if up.count() != 0 {
		t.Fatal("a parked credential was used anyway")
	}
}

func TestChatSendsTheVendorRequestShape(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"http_referer":"https://me.test","x_title":"my-app","free_only":false}`, testKey), up)

	if _, err := c.Chat(context.Background(), simpleReq("anthropic/claude-sonnet-4.5")); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	req := up.requestAt(0)
	if req == nil {
		t.Fatal("no request reached the vendor")
	}
	if req.Method != http.MethodPost || req.URL.String() != "https://openrouter.ai/api/v1/chat/completions" {
		t.Fatalf("%s %s, want POST /api/v1/chat/completions", req.Method, req.URL)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := req.Header.Get("HTTP-Referer"); got != "https://me.test" {
		t.Fatalf("HTTP-Referer = %q", got)
	}
	if got := req.Header.Get("X-Title"); got != "my-app" {
		t.Fatalf("X-Title = %q", got)
	}
	body := up.jsonAt(t, 0)
	if body["model"] != "anthropic/claude-sonnet-4.5" || body["stream"] != true {
		t.Fatalf("body = %v", body)
	}
}

func TestChatRotatesAwayFromAFailingCredential(t *testing.T) {
	var calls int32
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		if atomic.AddInt32(&calls, 1) == 1 {
			return reply{status: http.StatusTooManyRequests, body: `{"error":{"message":"Rate limit exceeded","code":429}}`}
		}
		return reply{body: sseBasic}
	}}
	c := newTestClient(t, twoKeysCfg(), up)

	st, err := c.Chat(context.Background(), simpleReq("m"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, _, err := drainStream(st)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if text != "Hello world" {
		t.Fatalf("text = %q", text)
	}
	if up.count() != 2 {
		t.Fatalf("requests = %d, want one failure then one success", up.count())
	}
	first := up.requestAt(0).Header.Get("Authorization")
	second := up.requestAt(1).Header.Get("Authorization")
	if first == second {
		t.Fatalf("both attempts used %q; a rate-limited credential must be rotated away from", first)
	}
	// The credential that answered 429 is parked, whichever of the two the
	// pool happened to pick first.
	failedKey := strings.TrimPrefix(first, "Bearer ")
	rec := c.mustByID(t, accountIDFor(failedKey))
	if rec.CooldownUntil == "" {
		t.Fatalf("the rate-limited credential was not parked: %+v", rec)
	}
}

func TestChatGivesUpWithTheLastFailureAfterRotating(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{status: http.StatusTooManyRequests, body: `{"error":{"message":"Rate limit exceeded","code":429}}`}
	}}
	c := newTestClient(t, twoKeysCfg(), up)

	_, err := c.Chat(context.Background(), simpleReq("m"))
	// Once an attempt has actually been made, the LAST classified failure is
	// more useful than a bare ErrBusy: it names the reason (so the gateway can
	// answer 429/401 instead of a generic 429), and it carries the account.
	if errors.Is(err, core.ErrBusy) {
		t.Fatalf("err = %v, want the vendor's failure rather than a generic ErrBusy", err)
	}
	if core.FailureKindOf(err) != core.FailureRateLimited {
		t.Fatalf("err = %v (kind %q), want the rate-limit failure", err, core.FailureKindOf(err))
	}
	if up.count() > maxRotate {
		t.Fatalf("requests = %d, want at most %d attempts", up.count(), maxRotate)
	}
}

func TestChatMapsTransportFailureToUpstream(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{err: errors.New("dial tcp: connection refused")} }}
	c := newTestClient(t, cfgWithKey(testKey), up)
	_, err := c.Chat(context.Background(), simpleReq("m"))
	if err == nil {
		t.Fatal("a transport failure must be reported")
	}
	if core.FailureKindOf(err) != core.FailureUpstream {
		t.Fatalf("kind = %q, want upstream", core.FailureKindOf(err))
	}
}

func TestChatMapsUnauthorizedToAuthFailure(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{status: http.StatusUnauthorized, body: `{"error":{"message":"No cookie auth credentials found","code":401}}`}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)
	_, err := c.Chat(context.Background(), simpleReq("m"))
	if core.FailureKindOf(err) != core.FailureAuth {
		t.Fatalf("err = %v (kind %q), want an auth failure", err, core.FailureKindOf(err))
	}
	if got := core.ErrorAccountID(err); got != accountIDFor(testKey) {
		t.Fatalf("account = %q, want the refused credential named", got)
	}
	// A key the vendor refuses is not merely cooled down: it is marked invalid
	// so the panel shows it as such.
	if rec := c.mustByID(t, accountIDFor(testKey)); !rec.Invalid {
		t.Fatalf("the refused credential was not marked invalid: %+v", rec)
	}
}

func TestChatNeverLeaksTheKeyIntoTheError(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		// The vendor echoes the credential back inside a sentence.
		return reply{status: http.StatusUnauthorized, body: fmt.Sprintf(`{"error":{"message":"invalid key %s","code":401}}`, testKey)}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)
	_, err := c.Chat(context.Background(), simpleReq("m"))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("the error leaks the credential: %v", err)
	}
	rec := c.mustByID(t, accountIDFor(testKey))
	if strings.Contains(rec.Note, testKey) || strings.Contains(rec.LastError, testKey) {
		t.Fatalf("the pool note leaks the credential: %+v", rec)
	}
	st := c.Status(context.Background())
	blob, _ := json.Marshal(st)
	if strings.Contains(string(blob), testKey) {
		t.Fatalf("Status leaks the credential:\n%s", blob)
	}
}

// --- the model catalogue --------------------------------------------------

const modelsBody = `{"data":[
  {"id":"anthropic/claude-sonnet-4.5","canonical_slug":"anthropic/claude-4.5-sonnet-20250929","name":"Anthropic: Claude Sonnet 4.5","created":1759161676,"context_length":1000000,
   "architecture":{"modality":"text+image+file->text","input_modalities":["text","image","file"],"output_modalities":["text"],"tokenizer":"Claude"},
   "pricing":{"prompt":"0.000003","completion":"0.000015"},
   "top_provider":{"context_length":1000000,"max_completion_tokens":64000,"is_moderated":true},
   "supported_parameters":["max_tokens","tools"],"knowledge_cutoff":"2025-01-31"},
  {"id":"openrouter/auto","name":"Auto Router","context_length":2000000,"pricing":{"prompt":"-1","completion":"-1"},"top_provider":{"context_length":2000000,"is_moderated":false}},
  {"id":"","name":"nameless"},
  {"id":"anthropic/claude-sonnet-4.5","name":"duplicate"}
]}`

func TestParseModelsBodyReadsStringPricingAndLimits(t *testing.T) {
	list, err := parseModelsBody([]byte(modelsBody))
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want the id-less row and the duplicate dropped", list)
	}
	claude := list[0]
	if claude.ID != "anthropic/claude-sonnet-4.5" || claude.OwnedBy != "anthropic" {
		t.Fatalf("model = %+v", claude)
	}
	if claude.Extra["context_length"] != 1000000 {
		t.Fatalf("context_length = %#v", claude.Extra["context_length"])
	}
	if limit, ok := core.OutputLimitFor(list, "anthropic/claude-sonnet-4.5"); !ok || limit != 64000 {
		t.Fatalf("max output = (%d, %v), want 64000", limit, ok)
	}
	// pricing.prompt is a STRING in the live response even though the vendor's
	// schema calls it a number; it must survive decoding.
	if claude.Extra["pricing_prompt"] != "0.000003" {
		t.Fatalf("pricing_prompt = %#v", claude.Extra["pricing_prompt"])
	}
	// The router pseudo-model publishes no output budget: say nothing.
	if _, ok := core.OutputLimitFor(list, "openrouter/auto"); ok {
		t.Fatal("a router model must not publish a guessed output limit")
	}
}

func TestParseModelsBodyRejectsGarbage(t *testing.T) {
	if _, err := parseModelsBody([]byte("not json")); err == nil {
		t.Fatal("garbage must be an error")
	}
	if list, err := parseModelsBody([]byte(`{"data":[]}`)); err != nil || len(list) != 0 {
		t.Fatalf("empty catalogue = (%+v, %v)", list, err)
	}
}

func TestFallbackModelsIncludeCuratedAndExtra(t *testing.T) {
	cfg, _ := parseConfig(json.RawMessage(`{"free_only":false,"extra_models":["my/custom","anthropic/claude-sonnet-4.5",""]}`))
	list := fallbackModels(cfg)
	if len(list) == 0 {
		t.Fatal("the cold-start catalogue must not be empty")
	}
	seen := map[string]int{}
	for _, m := range list {
		seen[m.ID]++
		if m.Extra["vendor"] != clientName {
			t.Fatalf("model %s has no vendor tag", m.ID)
		}
	}
	if seen["my/custom"] != 1 {
		t.Fatalf("extra_models was not merged: %v", seen["my/custom"])
	}
	if seen["anthropic/claude-sonnet-4.5"] != 1 {
		t.Fatalf("a curated id added through extra_models was duplicated: %d", seen["anthropic/claude-sonnet-4.5"])
	}
	if limit, ok := core.OutputLimitFor(list, "anthropic/claude-sonnet-4.5"); !ok || limit != 64000 {
		t.Fatalf("curated limit = (%d, %v)", limit, ok)
	}
	// A model only named in extra_models has no published budget.
	if _, ok := core.OutputLimitFor(list, "my/custom"); ok {
		t.Fatal("an extra model must not invent an output limit")
	}
}

func TestAuthorOf(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-sonnet-4.5": "anthropic",
		"openai/gpt-oss-120b:free":    "openai",
		"openrouter/auto":             "openrouter",
		"noslash":                     "",
		"  spaced/model  ":            "spaced",
		"":                            "",
	}
	for in, want := range cases {
		if got := authorOf(in); got != want {
			t.Fatalf("authorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestModelsServesTheFallbackWithoutBlocking(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: modelsBody} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("a cold cache must still answer from the built-in catalogue")
	}
}

func TestModelsWithoutACredentialMakesNoRequest(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: modelsBody} }}
	c := newTestClient(t, `{"free_only":false}`, up)

	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("the fallback catalogue must be served with no credential")
	}
	if up.count() != 0 {
		t.Fatalf("requests = %d, want none: the contract forbids fetching without a credential", up.count())
	}
}

func TestRefreshModelsStoresTheLiveCatalogue(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: modelsBody} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
	if up.count() != 1 {
		t.Fatalf("requests = %d, want 1", up.count())
	}
	req := up.requestAt(0)
	if req.Method != http.MethodGet || req.URL.String() != "https://openrouter.ai/api/v1/models" {
		t.Fatalf("%s %s, want GET /api/v1/models", req.Method, req.URL)
	}
	// /models is public, but the module still sends the key when it has one:
	// an authenticated caller gets their own catalogue back.
	if got := req.Header.Get("Authorization"); got != "Bearer "+testKey {
		t.Fatalf("Authorization = %q", got)
	}
	// The live list is now the catalogue, so a fresh cache is served and the
	// panel sees the ids the vendor actually publishes.
	again, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(again) != 2 || again[0].ID != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("cached list = %+v", again)
	}
	if up.count() != 1 {
		t.Fatalf("requests = %d, want the TTL cache to serve the second call", up.count())
	}
}

func TestRefreshModelsKeepsTheLastGoodListOnFailure(t *testing.T) {
	var fail int32
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		if atomic.LoadInt32(&fail) == 1 {
			return reply{status: http.StatusInternalServerError, body: `{"error":{"message":"upstream is having a moment","code":500}}`}
		}
		return reply{body: modelsBody}
	}}
	c := newTestClient(t, cfgWithKey(testKey), up)

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	atomic.StoreInt32(&fail, 1)
	list, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failing refresh must report the error")
	}
	if len(list) != 2 {
		t.Fatalf("list = %+v, want the last known-good catalogue alongside the error", list)
	}
	// And the catalogue is still served: an outage must not empty the picker.
	served, err := c.Models(context.Background())
	if err != nil || len(served) != 2 {
		t.Fatalf("Models after a failed refresh = (%d models, %v)", len(served), err)
	}
}

func TestRefreshModelsWithoutACredentialIsNotConfigured(t *testing.T) {
	up := &fakeUpstream{}
	c := newTestClient(t, `{"free_only":false}`, up)
	list, err := c.RefreshModels(context.Background())
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if len(list) == 0 {
		t.Fatal("the fallback catalogue must still be returned alongside the error")
	}
	if up.count() != 0 {
		t.Fatal("a credential-less refresh called the vendor")
	}
}

func TestModelMaxOutputTokensAnswersFromTheCacheOnly(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: modelsBody} }}
	c := newTestClient(t, cfgWithKey(testKey), up)

	// Cold cache: the curated table answers, and NOTHING is fetched.  This runs
	// inside a chat request, so a metadata round trip here would stall a turn.
	limit, ok := c.ModelMaxOutputTokens(context.Background(), "anthropic/claude-sonnet-4.5")
	if !ok || limit != 64000 {
		t.Fatalf("limit = (%d, %v), want the curated 64000", limit, ok)
	}
	if up.count() != 0 {
		t.Fatalf("requests = %d, want 0: this path must never fetch", up.count())
	}
	// A model nobody published a budget for is "cannot say", not a failure.
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "openrouter/auto"); ok {
		t.Fatal("a router model must not publish a guessed budget")
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "not/a-real-model"); ok {
		t.Fatal("an unknown model must answer ok=false")
	}
	if up.count() != 0 {
		t.Fatalf("requests = %d, want 0", up.count())
	}

	// Once a live catalogue exists, it is the source.
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if limit, ok := c.ModelMaxOutputTokens(context.Background(), "anthropic/claude-sonnet-4.5"); !ok || limit != 64000 {
		t.Fatalf("limit = (%d, %v) after a refresh", limit, ok)
	}
	if up.count() != 1 {
		t.Fatalf("requests = %d, want the refresh to be the only call", up.count())
	}
}

func TestStatusListsTheCatalogueWithoutFetching(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: modelsBody} }}
	c := newTestClient(t, cfgWithKey(testKey), up)
	st := c.Status(context.Background())
	if len(st.Models) == 0 {
		t.Fatal("Status must list a catalogue")
	}
	for _, id := range st.Models {
		// The routing prefix is stripped by the gateway BEFORE this module sees
		// the model, so the catalogue must publish the unqualified vendor id.
		// "openrouter/auto" is a legitimate VENDOR id (a router pseudo-model);
		// "openrouter/openrouter/auto" would mean the prefix was re-added.
		if strings.HasPrefix(id, clientName+"/"+clientName+"/") {
			t.Fatalf("catalogue id %q looks doubly qualified", id)
		}
	}
	if !containsString(st.Models, "anthropic/claude-sonnet-4.5") {
		t.Fatalf("catalogue = %v, want the curated vendor ids", st.Models)
	}
	if up.count() != 0 {
		t.Fatal("Status fetched the catalogue; it must never touch the network")
	}
}

func TestCloneModelsDoesNotShareExtra(t *testing.T) {
	orig := []core.Model{{ID: "a/b", Extra: map[string]any{"k": "v"}}}
	cp := cloneModels(orig)
	cp[0].Extra["k"] = "changed"
	if orig[0].Extra["k"] != "v" {
		t.Fatal("a caller could mutate the cache through the returned slice")
	}
	if cloneModels(nil) != nil {
		t.Fatal("cloning nothing must return nothing")
	}
}
