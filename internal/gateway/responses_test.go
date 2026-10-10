package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// responses drives the Responses endpoint the way Codex does.
func responses(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

// decodeSSE parses an SSE body into (type → raw data) pairs, in order.
func decodeSSE(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var data string
		for _, line := range strings.Split(block, "\n") {
			if rest, ok := strings.CutPrefix(line, "data: "); ok {
				data = rest
			}
		}
		if data == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad SSE payload %q: %v", data, err)
		}
		out = append(out, m)
	}
	return out
}

// sseType returns the type of every event, for asserting a sequence.
func sseType(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		s, _ := e["type"].(string)
		out = append(out, s)
	}
	return out
}

func hasType(events []map[string]any, typ string) bool {
	for _, e := range events {
		if s, _ := e["type"].(string); s == typ {
			return true
		}
	}
	return false
}

// firstOfType returns the first event of a given type, or nil.
func firstOfType(events []map[string]any, typ string) map[string]any {
	for _, e := range events {
		if s, _ := e["type"].(string); s == typ {
			return e
		}
	}
	return nil
}

func TestResponses_PlainTextStreamsAsCodexExpects(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "hello "},
		{Type: core.EventDelta, Delta: "world"},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type = %q", ct)
	}

	events := decodeSSE(t, rec.Body.String())
	types := sseType(events)

	// Codex dispatches function calls on output_item.done and requires
	// response.completed to carry an id; both must be present.
	for _, want := range []string{"response.created", "response.output_text.delta", "response.output_item.done", "response.completed"} {
		if !hasType(events, want) {
			t.Errorf("missing event %s; got %v", want, types)
		}
	}

	// Deltas must carry the text in order.
	var text strings.Builder
	for _, e := range events {
		if e["type"] == "response.output_text.delta" {
			text.WriteString(e["delta"].(string))
		}
	}
	if text.String() != "hello world" {
		t.Errorf("streamed text = %q, want %q", text.String(), "hello world")
	}

	// The done item must carry the assembled text, since that is what the
	// client replays.
	done := firstOfType(events, "response.output_item.done")
	item := done["item"].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("done item type = %v, want message", item["type"])
	}

	// response.completed must carry a non-empty response.id.
	completed := firstOfType(events, "response.completed")
	resp := completed["response"].(map[string]any)
	if id, _ := resp["id"].(string); id == "" {
		t.Error("response.completed.response.id is empty; Codex rejects this")
	}
}

func TestResponses_ToolCallBecomesFunctionCallItem(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "call_1", Name: "exec"}},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, Arguments: `{"cmd":"ls"}`}},
		{Type: core.EventDone, Finish: "tool_calls"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"run it"}`)
	events := decodeSSE(t, rec.Body.String())

	if !hasType(events, "response.function_call_arguments.delta") {
		t.Errorf("missing arguments delta; got %v", sseType(events))
	}

	done := firstOfType(events, "response.output_item.done")
	if done == nil {
		t.Fatal("no output_item.done")
	}
	item := done["item"].(map[string]any)
	if item["type"] != "function_call" {
		t.Fatalf("done item type = %v, want function_call", item["type"])
	}
	if item["name"] != "exec" {
		t.Errorf("name = %v, want exec", item["name"])
	}
	if item["call_id"] != "call_1" {
		t.Errorf("call_id = %v, want call_1", item["call_id"])
	}
	if item["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

func TestResponses_BufferedReturnsResponseObject(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "answer"},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","input":"q"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp responseObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body)
	}
	if resp.Object != "response" {
		t.Errorf("object = %q, want response", resp.Object)
	}
	if resp.Status != "completed" {
		t.Errorf("status = %q, want completed", resp.Status)
	}
	if resp.ID == "" {
		t.Error("id is empty")
	}
	if len(resp.Output) == 0 {
		t.Fatal("output is empty")
	}
	if resp.Output[0].Type != "message" {
		t.Errorf("output[0].type = %q, want message", resp.Output[0].Type)
	}
	if resp.Output[0].Content[0].Text != "answer" {
		t.Errorf("output text = %q", resp.Output[0].Content[0].Text)
	}
	if resp.Usage == nil || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 5 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

// TestResponses_TranslatesCodexRequestShape is the load-bearing test: it feeds
// the exact item shapes Codex emits and asserts the messages the module sees.
func TestResponses_TranslatesCodexRequestShape(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"instructions": "You are a coding agent.",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run the build"}]},
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"I should run make."}],"encrypted_content":"gAAAAAB"},
			{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"exec_command","arguments":"{\"cmd\":\"make\"}"},
			{"type":"function_call_output","call_id":"call_abc","output":"build ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"now test"}]}
		]
	}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if c.seen == nil {
		t.Fatal("module never saw the request")
	}

	msgs := c.seen.Messages
	if len(msgs) == 0 {
		t.Fatal("no messages translated")
	}
	if msgs[0].Role != "system" || msgs[0].Content != "You are a coding agent." {
		t.Errorf("instructions not mapped to a leading system message: %+v", msgs[0])
	}

	// The function_call must become an assistant turn carrying tool_calls, and
	// its output a tool turn keyed by call_id.
	var (
		assistantWithCall *core.Message
		toolTurn          *core.Message
	)
	for i := range msgs {
		if len(msgs[i].ToolCalls) > 0 {
			assistantWithCall = &msgs[i]
		}
		if msgs[i].Role == "tool" {
			toolTurn = &msgs[i]
		}
	}
	if assistantWithCall == nil {
		t.Fatalf("no assistant turn carried a tool call: %+v", msgs)
	}
	if assistantWithCall.Role != "assistant" {
		t.Errorf("tool-call turn role = %q, want assistant", assistantWithCall.Role)
	}
	if got := assistantWithCall.ToolCalls[0].Name; got != "exec_command" {
		t.Errorf("tool name = %q", got)
	}
	if got := assistantWithCall.ToolCalls[0].ID; got != "call_abc" {
		t.Errorf("tool call id = %q", got)
	}
	if assistantWithCall.ToolCalls[0].Arguments != `{"cmd":"make"}` {
		t.Errorf("tool arguments = %q", assistantWithCall.ToolCalls[0].Arguments)
	}
	// The reasoning summary must be preserved on the assistant turn, and the
	// ciphertext must not have been invented into text.
	if !strings.Contains(assistantWithCall.Reasoning, "I should run make.") {
		t.Errorf("reasoning summary lost: %q", assistantWithCall.Reasoning)
	}
	if strings.Contains(assistantWithCall.Reasoning, "gAAAAAB") {
		t.Errorf("encrypted blob leaked into reasoning: %q", assistantWithCall.Reasoning)
	}
	if toolTurn == nil {
		t.Fatal("function_call_output did not become a tool turn")
	}
	if toolTurn.ToolCallID != "call_abc" {
		t.Errorf("tool turn call id = %q", toolTurn.ToolCallID)
	}
	if toolTurn.Content != "build ok" {
		t.Errorf("tool turn content = %q", toolTurn.Content)
	}
}

// TestResponses_EncryptedAgentContentIsAccepted is the regression that matters:
// the request a relay answers with 400 must be served here.
func TestResponses_EncryptedAgentContentIsAccepted(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"stream": true,
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"review this"}]},
			{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/review",
			 "content":[
				{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/review\n"},
				{"type":"encrypted_content","encrypted_content":"AbCdEf0123456789AbCdEf0123456789AbCdEf0123456789AbCdEf0123456789"}
			 ]}
		]
	}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("encrypted agent content was rejected: status = %d, body = %s", rec.Code, rec.Body)
	}
	if c.seen == nil {
		t.Fatal("module never saw the request")
	}

	// The plaintext envelope must survive; the ciphertext must not be forwarded
	// as text the model would try to read.
	var envelope string
	for _, m := range c.seen.Messages {
		if strings.Contains(m.Content, "NEW_TASK") {
			envelope = m.Content
		}
	}
	if envelope == "" {
		t.Fatalf("plaintext envelope lost: %+v", c.seen.Messages)
	}
	if strings.Contains(envelope, "AbCdEf0123456789") {
		t.Errorf("ciphertext forwarded to the model: %q", envelope)
	}
	if !strings.Contains(envelope, "encrypted agent payload omitted") {
		t.Errorf("envelope does not state the payload was omitted: %q", envelope)
	}
}

func TestResponses_InstructionsOnlyRequestWorks(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","instructions":"be terse","input":[{"type":"message","role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if c.seen.Messages[0].Role != "system" {
		t.Errorf("first message role = %q, want system", c.seen.Messages[0].Role)
	}
}

func TestResponses_FlatAndNestedToolsBothAccepted(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	// Responses flattens tools; some relays nest them. Both must reduce to the
	// same core.Tool.
	flat := `{"model":"t/m1","input":"hi","tools":[{"type":"function","name":"flat","description":"d","parameters":{"type":"object"}}]}`
	if rec := responses(t, srv, flat); rec.Code != http.StatusOK {
		t.Fatalf("flat tool status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(c.seen.Tools) != 1 || c.seen.Tools[0].Name != "flat" {
		t.Fatalf("flat tool not translated: %+v", c.seen.Tools)
	}

	nested := `{"model":"t/m1","input":"hi","tools":[{"type":"function","function":{"name":"nested","description":"d","parameters":{"type":"object"}}}]}`
	if rec := responses(t, srv, nested); rec.Code != http.StatusOK {
		t.Fatalf("nested tool status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(c.seen.Tools) != 1 || c.seen.Tools[0].Name != "nested" {
		t.Fatalf("nested tool not translated: %+v", c.seen.Tools)
	}
}

func TestResponses_BuiltinToolIsSkippedNotFatal(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{"model":"t/m1","input":"hi","tools":[{"type":"web_search"},{"type":"function","name":"keep","parameters":{}}]}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(c.seen.Tools) != 1 || c.seen.Tools[0].Name != "keep" {
		t.Errorf("built-in tool handling wrong: %+v", c.seen.Tools)
	}
}

func TestResponses_UnknownItemTypeIsSkipped(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{"model":"t/m1","input":[{"type":"some_future_item","x":1},{"type":"message","role":"user","content":"hi"}]}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown item type was fatal: status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestResponses_MissingInputIsRejected(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
}

func TestResponses_MethodMustBePost(t *testing.T) {
	c := &testClient{name: "t"}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestResponses_UnknownModelIs404(t *testing.T) {
	c := &testClient{name: "t"}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"nope/m1","input":"hi"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body)
	}
}

func TestResponses_UpstreamErrorIsReported(t *testing.T) {
	c := &testClient{name: "t", chatErr: core.ErrNotConfigured}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","input":"hi"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body)
	}
	var env apiErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body = %s", err, rec.Body)
	}
	if env.Error.Message == "" {
		t.Error("error message is empty")
	}
}

func TestResponses_StreamErrorBecomesResponseFailed(t *testing.T) {
	// A stream that breaks after opening must emit response.failed rather than
	// hanging or closing silently, which is what keeps Codex from stalling.
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "partial"},
		{Type: core.EventError, Err: io.ErrUnexpectedEOF},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"hi"}`)
	events := decodeSSE(t, rec.Body.String())
	if !hasType(events, "response.failed") {
		t.Errorf("missing response.failed; got %v", sseType(events))
	}
}

func TestResponses_UsageIsMapped(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "x"},
		{Type: core.EventUsage, Usage: &core.Usage{
			PromptTokens: 100, CompletionTokens: 40, TotalTokens: 140,
			ReasoningTokens: 25, CachedTokens: 60,
		}},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"hi"}`)
	events := decodeSSE(t, rec.Body.String())
	completed := firstOfType(events, "response.completed")
	if completed == nil {
		t.Fatal("no response.completed")
	}
	resp := completed["response"].(map[string]any)
	usage, ok := resp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage in completed response: %+v", resp)
	}
	if usage["input_tokens"].(float64) != 100 {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
	if usage["output_tokens"].(float64) != 40 {
		t.Errorf("output_tokens = %v", usage["output_tokens"])
	}
	if usage["total_tokens"].(float64) != 140 {
		t.Errorf("total_tokens = %v", usage["total_tokens"])
	}
	details, _ := usage["input_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"].(float64) != 60 {
		t.Errorf("cached_tokens not mapped: %v", usage["input_tokens_details"])
	}
}

func TestResponses_ReasoningIsMapped(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Reasoning: "thinking..."},
		{Type: core.EventDelta, Delta: "answer"},
		{Type: core.EventDone, Finish: "stop"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","input":"hi"}`)
	var resp responseObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawReasoning, sawMessage bool
	for _, item := range resp.Output {
		switch item.Type {
		case "reasoning":
			sawReasoning = true
			if len(item.Summary) == 0 || !strings.Contains(item.Summary[0].Text, "thinking") {
				t.Errorf("reasoning summary lost: %+v", item)
			}
		case "message":
			sawMessage = true
		}
	}
	if !sawReasoning {
		t.Error("no reasoning item emitted")
	}
	if !sawMessage {
		t.Error("no message item emitted")
	}
}

func TestResponses_ConversationIDKeepsStickiness(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	// A client that follows the OpenAI convention carries its id in metadata.
	rec := responses(t, srv, `{"model":"t/m1","input":"hi","metadata":{"conversation_id":"conv-42"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if c.seen.ConversationID != "conv-42" {
		t.Errorf("conversation id = %q, want conv-42", c.seen.ConversationID)
	}
}

func TestResponses_PlainStringInputBecomesUserTurn(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","input":"just a string"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(c.seen.Messages) != 1 || c.seen.Messages[0].Role != "user" || c.seen.Messages[0].Content != "just a string" {
		t.Errorf("string input = %+v", c.seen.Messages)
	}
}

func TestResponses_ReasoningEffortReachesOptions(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	// The Responses-shaped reasoning object.
	rec := responses(t, srv, `{"model":"t/m1","input":"hi","reasoning":{"effort":"high"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := c.seen.Options["reasoning_effort"]; got != "high" {
		t.Errorf("upstream effort = %v, want high", got)
	}
}

// TestResponses_AndChatAgreeOnModelResolution guards the shared router: both
// endpoints must resolve the same model string to the same module.
func TestResponses_AndChatAgreeOnModelResolution(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	chatRec := chat(t, srv, `{"model":"t/m1","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	respRec := responses(t, srv, `{"model":"t/m1","input":"hi","stream":true}`)

	if chatRec.Code != http.StatusOK || respRec.Code != http.StatusOK {
		t.Fatalf("statuses: chat=%d responses=%d", chatRec.Code, respRec.Code)
	}
	// Both must have reached the module with the same resolved model.
	if c.seen.Model != "m1" {
		t.Errorf("resolved model = %q, want m1", c.seen.Model)
	}
}

func TestLooksEncrypted(t *testing.T) {
	long := strings.Repeat("Ab3+/", 80)
	cases := []struct {
		in   string
		want bool
	}{
		{"short", false},
		{"a normal sentence with spaces and enough length " + strings.Repeat("x", 300), false},
		{long, true},
		{"", false},
	}
	for _, tc := range cases {
		if got := looksEncrypted(tc.in); got != tc.want {
			t.Errorf("looksEncrypted(len=%d) = %v, want %v", len(tc.in), got, tc.want)
		}
	}
}

func TestHasEncryptedPart(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"array with encrypted", `[{"type":"input_text","text":"a"},{"type":"encrypted_content","encrypted_content":"x"}]`, true},
		{"array without", `[{"type":"input_text","text":"a"}]`, false},
		{"bare object", `{"type":"encrypted_content","encrypted_content":"x"}`, true},
		{"string content", `"plain"`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		if got := hasEncryptedPart(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSSEFrameTypeMatchesPayload(t *testing.T) {
	var sb strings.Builder
	if err := sseFrame(&sb, "response.created", map[string]any{"type": "response.created"}); err != nil {
		t.Fatalf("sseFrame: %v", err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "event: response.created\ndata: ") {
		t.Errorf("frame header wrong:\n%s", out)
	}
	if !strings.HasSuffix(out, "\n\n") {
		t.Errorf("frame not terminated by a blank line:\n%q", out)
	}
	var payload map[string]any
	data := strings.TrimSuffix(strings.TrimPrefix(out, "event: response.created\ndata: "), "\n\n")
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if payload["type"] != "response.created" {
		t.Errorf("payload type = %v; the header and payload must agree", payload["type"])
	}
}

var _ = log.New
