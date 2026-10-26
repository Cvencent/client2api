package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- body building -----------------------------------------------------------

// The Responses request must carry the same conversation as the chat wire:
// system text in instructions, tool calls as function_call items, tool results
// as function_call_output items, and a flat tool definition list.
func TestBuildResponsesBodyShape(t *testing.T) {
	max := 512
	cfg := Config{MaxTokensField: "max_tokens"}
	req := &core.ChatRequest{
		Model:     "gpt-5.6-sol",
		MaxTokens: &max,
		Messages: []core.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "weather in Paris?"},
			{Role: "assistant", Content: "checking", ToolCalls: []core.ToolCall{{
				ID: "call_1", Type: "function", Name: "get_weather", Arguments: `{"city":"Paris"}`,
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: "22C sunny"},
			{Role: "user", Content: "thanks"},
		},
		Tools: []core.Tool{{
			Type: "function", Name: "get_weather", Description: "Get current weather",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
		ToolChoice: json.RawMessage(`"auto"`),
	}
	raw, err := buildResponsesBody(cfg, ProviderConfig{}, "gpt-5.6-sol", req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		Stream          bool   `json:"stream"`
		MaxOutputTokens *int   `json:"max_output_tokens"`
		ToolChoice      string `json:"tool_choice"`
		Input           []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    string          `json:"output"`
		} `json:"input"`
		Tools []struct {
			Type       string          `json:"type"`
			Name       string          `json:"name"`
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model != "gpt-5.6-sol" || !body.Stream {
		t.Errorf("model=%q stream=%v", body.Model, body.Stream)
	}
	if body.Instructions != "be brief" {
		t.Errorf("instructions = %q, want the system text", body.Instructions)
	}
	if body.MaxOutputTokens == nil || *body.MaxOutputTokens != 512 {
		t.Errorf("max_output_tokens = %v, want 512", body.MaxOutputTokens)
	}
	if body.ToolChoice != "auto" {
		t.Errorf("tool_choice = %q, want auto", body.ToolChoice)
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Name != "get_weather" {
		t.Fatalf("tools = %+v, want one flat function tool", body.Tools)
	}
	if !strings.Contains(string(body.Tools[0].Parameters), `"type":"object"`) {
		t.Errorf("tool parameters = %s", body.Tools[0].Parameters)
	}
	// input: user, assistant text, function_call, function_call_output, user
	if len(body.Input) != 5 {
		t.Fatalf("input items = %d (%+v), want 5", len(body.Input), body.Input)
	}
	if body.Input[0].Role != "user" || !strings.Contains(string(body.Input[0].Content), "Paris") {
		t.Errorf("input[0] = %+v", body.Input[0])
	}
	if body.Input[1].Role != "assistant" || !strings.Contains(string(body.Input[1].Content), "checking") {
		t.Errorf("input[1] = %+v", body.Input[1])
	}
	if body.Input[2].Type != "function_call" || body.Input[2].CallID != "call_1" ||
		body.Input[2].Name != "get_weather" || body.Input[2].Arguments != `{"city":"Paris"}` {
		t.Errorf("input[2] = %+v", body.Input[2])
	}
	if body.Input[3].Type != "function_call_output" || body.Input[3].CallID != "call_1" ||
		body.Input[3].Output != "22C sunny" {
		t.Errorf("input[3] = %+v", body.Input[3])
	}
	if body.Input[4].Role != "user" {
		t.Errorf("input[4] = %+v", body.Input[4])
	}
}

// A user turn with an inline image becomes typed input parts.
func TestBuildResponsesBodyImage(t *testing.T) {
	req := &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{{
			Role: "user",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what is this"},
				{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
			},
		}},
	}
	raw, err := buildResponsesBody(Config{}, ProviderConfig{}, "m", req)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, `"input_text"`) || !strings.Contains(s, `"input_image"`) ||
		!strings.Contains(s, "data:image/png;base64,AAAA") {
		t.Errorf("image parts missing from the request: %s", s)
	}
}

// An empty tool-result payload must not be dropped: the vendor needs the
// function_call_output item to pair with its call.
func TestBuildResponsesBodyEmptyToolOutput(t *testing.T) {
	req := &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c1", Name: "t", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "c1"},
		},
	}
	raw, err := buildResponsesBody(Config{}, ProviderConfig{}, "m", req)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Type   string `json:"type"`
			Output string `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 3 || body.Input[2].Type != "function_call_output" {
		t.Fatalf("input = %+v", body.Input)
	}
}

// --- stream translation --------------------------------------------------------

// responsesSSEFeed turns frames into an HTTP response body the stream reader
// can consume.
func responsesSSEFeed(frames ...string) *http.Response {
	var b strings.Builder
	for _, f := range frames {
		if strings.HasPrefix(f, "event:") || strings.HasPrefix(f, "data:") || f == "" {
			b.WriteString(f + "\n")
			continue
		}
		b.WriteString("data: " + f + "\n\n")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(b.String())),
	}
}

func newResponsesTestStream(t *testing.T, frames ...string) *chatStream {
	t.Helper()
	c := &Client{cfg: Config{APIFormat: responsesFormat}, now: time.Now}
	return newChatStream(c, responsesSSEFeed(frames...), ProviderConfig{}, "acct", func() {})
}

func TestResponsesStreamTextAndUsage(t *testing.T) {
	s := newResponsesTestStream(t,
		`{"type":"response.created","response":{"id":"r1"}}`,
		`{"type":"response.output_item.added","item":{"type":"message","role":"assistant"},"output_index":0}`,
		`{"type":"response.output_text.delta","delta":"OK"}`,
		`{"type":"response.output_text.delta","delta":" there"}`,
		`{"type":"response.output_text.done","text":"OK there"}`,
		`{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":12,"output_tokens":34,"total_tokens":46,"output_tokens_details":{"reasoning_tokens":7},"input_tokens_details":{"cached_tokens":3}}}}`,
	)
	text, reasoning, calls, usage, finish, err := drainStream(s)
	if err != nil {
		t.Fatal(err)
	}
	if text != "OK there" || reasoning != "" || len(calls) != 0 {
		t.Errorf("text=%q reasoning=%q calls=%v", text, reasoning, calls)
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 34 || usage.TotalTokens != 46 {
		t.Errorf("usage = %+v", usage)
	}
	if usage != nil && (usage.ReasoningTokens != 7 || !usage.CachedTokensKnown || usage.CachedTokens != 3) {
		t.Errorf("usage details = %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}
}

func TestResponsesStreamToolCall(t *testing.T) {
	s := newResponsesTestStream(t,
		`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_9","name":"get_weather"},"output_index":1}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"Paris\"}"}`,
		`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},"output_index":1}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5}}}`,
	)
	_, _, calls, _, finish, err := drainStream(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one", calls)
	}
	call := calls[0]
	if call.Index != 1 || call.ID != "call_9" || call.Name != "get_weather" {
		t.Errorf("call = %+v", call)
	}
	// The arguments must be the complete object exactly once: deltas are
	// buffered, the done item emits the full payload.
	if call.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q", call.Arguments)
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}
}

func TestResponsesStreamReasoningAndIncomplete(t *testing.T) {
	s := newResponsesTestStream(t,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":8,"output_tokens":4}}}`,
	)
	text, reasoning, _, _, finish, err := drainStream(s)
	if err != nil {
		t.Fatal(err)
	}
	if text != "partial" || reasoning != "thinking" {
		t.Errorf("text=%q reasoning=%q", text, reasoning)
	}
	if finish != "length" {
		t.Errorf("finish = %q, want length for an incomplete response", finish)
	}
}

func TestResponsesStreamFailure(t *testing.T) {
	s := newResponsesTestStream(t,
		`{"type":"response.failed","response":{"error":{"code":"server_error","message":"boom upstream"}}}`,
	)
	_, _, _, _, _, err := drainStream(s)
	if err == nil || !strings.Contains(err.Error(), "boom upstream") {
		t.Fatalf("err = %v, want the vendor message", err)
	}
}

func TestResponsesStreamTopLevelError(t *testing.T) {
	s := newResponsesTestStream(t,
		`{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`,
	)
	_, _, _, _, _, err := drainStream(s)
	if err == nil || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("err = %v, want the vendor message", err)
	}
}

// --- source end-to-end ---------------------------------------------------------

// A source with api_format "responses" posts to /v1/responses, keeps the
// per-source User-Agent, and translates the vendor's event stream back.
func TestSourceResponsesWireEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var gotUA, gotAuth, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotUA, gotAuth = r.URL.Path, r.Header.Get("User-Agent"), r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"gpt-5.6-sol"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":"r1"}}`,
			``,
			`event: response.output_text.delta`,
			`data: {"type":"response.output_text.delta","delta":"pong"}`,
			``,
			`event: response.completed`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n"))
	}))
	defer srv.Close()

	c, err := newSource("agentrouter", core.SourceConfig{
		BaseURL:   srv.URL + "/v1",
		UserAgent: "codex_cli_rs/0.0.1 (client2api)",
		APIFormat: "responses",
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "sk-test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "gpt-5.6-sol",
		Messages: []core.Message{{Role: "user", Content: "say OK"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, _, calls, usage, finish, err := drainStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/responses" {
		t.Errorf("the chat went to %q, want /v1/responses", gotPath)
	}
	if gotUA != "codex_cli_rs/0.0.1 (client2api)" {
		t.Errorf("UA = %q, want the configured source UA", gotUA)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	var body struct {
		Model        string `json:"model"`
		Stream       bool   `json:"stream"`
		Input        []any  `json:"input"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("request body: %v (%s)", err, gotBody)
	}
	if body.Model != "gpt-5.6-sol" || !body.Stream || len(body.Input) != 1 {
		t.Errorf("request body = %s", gotBody)
	}
	if text != "pong" || len(calls) != 0 || finish != "stop" {
		t.Errorf("text=%q calls=%v finish=%q", text, calls, finish)
	}
	if usage == nil || usage.PromptTokens != 3 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
}

// A source without the format keeps posting to /chat/completions even when a
// responses-shaped server would also answer there.
func TestSourceDefaultWireUnchanged(t *testing.T) {
	var path string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path = r.URL.Path
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"demo"}]}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"delta":{"content":"pong"}}]}`)
		fmt.Fprint(w, "\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "k"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "demo",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, werr := drainStream(stream); werr != nil && !errors.Is(werr, io.EOF) {
		t.Fatalf("drain: %v", werr)
	}
	mu.Lock()
	defer mu.Unlock()
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", path)
	}
}
