package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// messages drives the Anthropic inbound endpoint the way Claude Code does.
func messages(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func countTokens(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// translateAnthropic feeds a raw request body through the inbound decoder and
// the translator, which is what the unit tests below assert on.  It bypasses
// routing so a test about translation never depends on a module's stream.
func translateAnthropic(t *testing.T, body string) (*core.ChatRequest, error) {
	t.Helper()
	var wire anthropicRequest
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return toCoreRequestFromAnthropic(&wire)
}

// ---------------------------------------------------------------------------
// request translation
// ---------------------------------------------------------------------------

func TestAnthropicSystemStringAndBlocksBothBecomeOneSystemMessage(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"string": {`{"model":"m1","max_tokens":16,"system":"be terse","messages":[{"role":"user","content":"hi"}]}`, "be terse"},
		"blocks": {`{"model":"m1","max_tokens":16,"system":[{"type":"text","text":"be "},{"type":"text","text":"terse"}],"messages":[{"role":"user","content":"hi"}]}`, "be \nterse"},
	} {
		t.Run(name, func(t *testing.T) {
			req, err := translateAnthropic(t, tc.body)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != tc.want {
				t.Fatalf("messages = %+v, want a system turn with %q", req.Messages, tc.want)
			}
		})
	}
}

func TestAnthropicUserToolResultBecomesToolRoleMessage(t *testing.T) {
	body := `{"model":"m1","max_tokens":16,"messages":[
		{"role":"assistant","content":[{"type":"text","text":"running"},{"type":"tool_use","id":"toolu_1","name":"exec","input":{"cmd":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"a.go"}]},{"type":"text","text":"thanks"}]}
	]}`
	req, err := translateAnthropic(t, body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	msgs := req.Messages
	// assistant, tool, user
	if len(msgs) != 3 {
		t.Fatalf("messages = %+v", msgs)
	}
	if msgs[0].Role != "assistant" || len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].Name != "exec" || msgs[0].ToolCalls[0].Arguments != `{"cmd":"ls"}` {
		t.Errorf("assistant message = %+v", msgs[0])
	}
	if msgs[1].Role != "tool" || msgs[1].ToolCallID != "toolu_1" || msgs[1].Content != "a.go" {
		t.Errorf("tool message = %+v", msgs[1])
	}
	if msgs[2].Role != "user" || msgs[2].Content != "thanks" {
		t.Errorf("user message = %+v", msgs[2])
	}
}

func TestAnthropicImageBecomesInlineDataURL(t *testing.T) {
	body := `{"model":"m1","max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"text","text":"what"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
	]}]}`
	req, err := translateAnthropic(t, body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	parts := req.Messages[len(req.Messages)-1].Parts
	if len(parts) != 2 || parts[1].Type != "image_url" || parts[1].ImageURL != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("parts = %+v", parts)
	}
}

func TestAnthropicThinkingBudgetMapsToNamedEffort(t *testing.T) {
	body := `{"model":"m1","max_tokens":16,"thinking":{"type":"enabled","budget_tokens":20000},"messages":[{"role":"user","content":"hi"}]}`
	req, err := translateAnthropic(t, body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := req.Options["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
}

func TestAnthropicToolChoiceAnyBecomesRequired(t *testing.T) {
	body := `{"model":"m1","max_tokens":16,"tool_choice":{"type":"any"},
		"tools":[{"name":"exec","description":"run","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"go"}]}`
	req, err := translateAnthropic(t, body)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if string(req.ToolChoice) != `"required"` {
		t.Fatalf("tool_choice = %s, want \"required\"", req.ToolChoice)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "exec" {
		t.Fatalf("tools = %+v", req.Tools)
	}
}

func TestAnthropicRejectsUnknownRole(t *testing.T) {
	if _, err := translateAnthropic(t, `{"model":"m1","max_tokens":16,"messages":[{"role":"robot","content":"hi"}]}`); err == nil {
		t.Fatal("an unknown role was accepted")
	}
}

// ---------------------------------------------------------------------------
// streaming emitter
// ---------------------------------------------------------------------------

func TestAnthropicStreamsEventsClaudeCodeExpects(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Reasoning: "hmm"},
		{Type: core.EventDelta, Delta: "hello "},
		{Type: core.EventDelta, Delta: "world"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}
	events := decodeSSE(t, rec.Body.String())
	types := sseType(events)
	for _, want := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !hasType(events, want) {
			t.Errorf("missing event %s; got %v", want, types)
		}
	}
	if types[0] != "message_start" {
		t.Errorf("first event = %s, want message_start", types[0])
	}
	if types[len(types)-1] != "message_stop" {
		t.Errorf("last event = %s, want message_stop", types[len(types)-1])
	}
	// Thinking and text must land on separate blocks, and the text deltas must
	// reassemble to the answer.
	var text strings.Builder
	for _, e := range events {
		if e["type"] != "content_block_delta" {
			continue
		}
		delta, _ := e["delta"].(map[string]any)
		if delta["type"] == "text_delta" {
			text.WriteString(delta["text"].(string))
		}
	}
	if text.String() != "hello world" {
		t.Errorf("streamed text = %q, want %q", text.String(), "hello world")
	}
	if !hasType(events, "content_block_delta") {
		t.Fatal("no deltas")
	}
	md := firstOfType(events, "message_delta")
	delta := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", delta["stop_reason"])
	}
	usage := md["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 12 || usage["output_tokens"].(float64) != 3 {
		t.Errorf("usage = %v", usage)
	}
}

func TestAnthropicStreamsToolUseAsBlock(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "toolu_9", Name: "exec"}},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, Arguments: `{"cmd":"ls"}`}},
		{Type: core.EventDone, Finish: "tool_calls"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"go"}]}`)
	events := decodeSSE(t, rec.Body.String())
	var started bool
	var args strings.Builder
	for _, e := range events {
		switch e["type"] {
		case "content_block_start":
			block, _ := e["content_block"].(map[string]any)
			if block["type"] == "tool_use" && block["name"] == "exec" && block["id"] == "toolu_9" {
				started = true
			}
		case "content_block_delta":
			delta, _ := e["delta"].(map[string]any)
			if delta["type"] == "input_json_delta" {
				args.WriteString(delta["partial_json"].(string))
			}
		}
	}
	if !started {
		t.Fatalf("no tool_use content_block_start; got %v", sseType(events))
	}
	if args.String() != `{"cmd":"ls"}` {
		t.Errorf("partial_json = %q", args.String())
	}
	md := firstOfType(events, "message_delta")
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", md["delta"].(map[string]any)["stop_reason"])
	}
}

// ---------------------------------------------------------------------------
// buffered emitter
// ---------------------------------------------------------------------------

func TestAnthropicBufferedReturnsMessageObject(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "answer"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":16,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var msg anthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if msg.Type != "message" || msg.Role != "assistant" || !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("envelope = %+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "answer" {
		t.Errorf("content = %+v", msg.Content)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", msg.StopReason)
	}
	if msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", msg.Usage)
	}
}

func TestAnthropicBufferedToolUse(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "toolu_2", Name: "exec", Arguments: `{"a":1}`}},
		{Type: core.EventDone, Finish: "tool_calls"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":16,"messages":[{"role":"user","content":"go"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var msg anthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" || msg.Content[0].Name != "exec" {
		t.Fatalf("content = %+v", msg.Content)
	}
	if string(msg.Content[0].Input) != `{"a":1}` {
		t.Errorf("input = %s", msg.Content[0].Input)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", msg.StopReason)
	}
}

// TestAnthropicUnterminatedStreamFails mirrors the cross-protocol truncation
// contract: a stream that ends without a terminal frame is an error, not a
// silent half-turn.
func TestAnthropicUnterminatedStreamFails(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "partial"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{"model":"t/m1","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "unexpected EOF") {
		t.Fatalf("truncated buffered response was not reported: %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// count_tokens
// ---------------------------------------------------------------------------

func TestAnthropicCountTokensReturnsPositiveEstimate(t *testing.T) {
	c := &testClient{name: "t"}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := countTokens(t, srv, `{"model":"t/m1","messages":[{"role":"user","content":"hello there, describe the sea"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if out.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want > 0", out.InputTokens)
	}
}

// ---------------------------------------------------------------------------
// protocol errors
// ---------------------------------------------------------------------------

func TestAnthropicErrorEnvelopeShape(t *testing.T) {
	c := &testClient{name: "t"}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := messages(t, srv, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if env.Type != "error" || env.Error.Type != "invalid_request_error" {
		t.Fatalf("envelope = %+v; body = %s", env, rec.Body)
	}
}
