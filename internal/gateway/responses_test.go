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

// A DeepSeek-family model can put its tool call in the visible content as
// DSML instead of returning it through tool_calls. Codex sees that text as an
// answer and never runs the tool, so the gateway protocol boundary must turn
// it into the same function_call event a native call produces.
func TestResponses_DSMLTextBecomesFunctionCallItem(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	closeMarker := strings.TrimPrefix(marker, "<")
	dsml := marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">{"cmd":"git status --short"}</` + closeMarker + " parameter>\n" +
		"</" + closeMarker + " invoke>\n" +
		"</" + closeMarker + " calls>"

	events := make([]core.Event, 0, len([]rune(dsml))+1)
	for _, r := range dsml {
		events = append(events, core.Event{Type: core.EventDelta, Delta: string(r)})
	}
	events = append(events, core.Event{Type: core.EventDone, Finish: "stop"})

	c := &testClient{name: "t", events: events}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"run it","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("raw DSML leaked to the caller: %s", rec.Body)
	}

	done := firstOfType(decodeSSE(t, rec.Body.String()), "response.output_item.done")
	if done == nil {
		t.Fatal("no output_item.done")
	}
	item := done["item"].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "exec" {
		t.Fatalf("done item = %#v, want exec function_call", item)
	}
	if item["arguments"] != `{"cmd":"git status --short"}` {
		t.Fatalf("arguments = %v", item["arguments"])
	}
}

// A truncated DSML block must fail the turn rather than leak markup as visible
// answer text. Otherwise Codex again receives protocol syntax it cannot act on.
func TestResponses_IncompleteDSMLFailsWithoutLeakingMarkup(t *testing.T) {
	marker := "<" + strings.Repeat("\uFF5C", 2) + "DSML" + strings.Repeat("\uFF5C", 2)
	dsml := marker + " calls>\n" +
		marker + ` invoke name="exec">` + "\n" +
		marker + ` parameter name="arguments" string="true">{"cmd":"pwd"}`

	events := make([]core.Event, 0, len([]rune(dsml))+1)
	for _, r := range dsml {
		events = append(events, core.Event{Type: core.EventDelta, Delta: string(r)})
	}
	events = append(events, core.Event{Type: core.EventDone, Finish: "stop"})

	c := &testClient{name: "t", events: events}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"run it","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "DSML") {
		t.Fatalf("incomplete DSML leaked to the caller: %s", rec.Body)
	}
	if !hasType(decodeSSE(t, rec.Body.String()), "response.failed") {
		t.Fatalf("incomplete DSML did not fail the response: %s", rec.Body)
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
	if toolTurn.Name != "exec_command" {
		t.Errorf("tool turn name = %q, want the matching call name", toolTurn.Name)
	}
	if toolTurn.Content != "build ok" {
		t.Errorf("tool turn content = %q", toolTurn.Content)
	}
}

func TestResponses_AssistantPreambleAndCallShareOneTurn(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	// Codex replays the model's visible text as a message item immediately
	// before the function_call it introduced.  Chat has one assistant turn for
	// the pair, so the translator must not splice them into two turns.
	body := `{
		"model": "t/m1",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"I should inspect."}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Found it. Let me confirm."}]},
			{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\":\"make\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var assistants []core.Message
	for _, m := range c.seen.Messages {
		if m.Role == "assistant" {
			assistants = append(assistants, m)
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("assistant turns = %d, want 1 (preamble and call must share one turn): %+v", len(assistants), c.seen.Messages)
	}
	a := assistants[0]
	if a.Content != "Found it. Let me confirm." {
		t.Errorf("preamble text = %q", a.Content)
	}
	if !strings.Contains(a.Reasoning, "I should inspect.") {
		t.Errorf("reasoning lost from the call turn: %q", a.Reasoning)
	}
	if len(a.ToolCalls) != 1 || a.ToolCalls[0].Name != "exec_command" {
		t.Errorf("tool calls on the merged turn = %+v", a.ToolCalls)
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

// TestResponses_NamespaceToolsAreFlattenedForTheVendor: Codex declares each MCP
// server's tools as one {"type":"namespace"} container.  A Chat vendor has no
// namespaces, so the children must be declared as ordinary functions -- and the
// container itself must never be.  It carries no schema, and a call to it lands
// as the name Codex refuses with "unsupported call".
func TestResponses_NamespaceToolsAreFlattenedForTheVendor(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"input": "hi",
		"tools": [
			{"type":"namespace","name":"mcp__codegraph","description":"CodeGraph tools.","tools":[
				{"type":"function","name":"codegraph_status","description":"status","parameters":{"type":"object","properties":{}}},
				{"type":"function","name":"codegraph_search","description":"search","parameters":{"type":"object"}}
			]},
			{"type":"function","name":"exec_command","parameters":{"type":"object"}}
		]
	}`
	if rec := responses(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	got := map[string]core.Tool{}
	for _, tool := range c.seen.Tools {
		got[tool.Name] = tool
	}
	for _, want := range []string{"mcp__codegraph__codegraph_status", "mcp__codegraph__codegraph_search", "exec_command"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("tool %q missing; module saw %+v", want, c.seen.Tools)
		}
	}
	if _, ok := got["mcp__codegraph"]; ok {
		t.Fatalf("the namespace container was declared as a tool: %+v", c.seen.Tools)
	}
	if d := got["mcp__codegraph__codegraph_status"].Description; d != "status" {
		t.Errorf("child description lost: %q", d)
	}
	if got["mcp__codegraph__codegraph_status"].Parameters == nil {
		t.Error("child schema lost")
	}
}

// TestResponses_NamespacedCallIsSplitBackIntoThePair: the vendor answers with the
// flattened name, but Codex dispatches on the (namespace, name) pair.  Sending
// the flat name alone is exactly the shape Codex rejects as an unsupported call.
func TestResponses_NamespacedCallIsSplitBackIntoThePair(t *testing.T) {
	tools := `"tools":[{"type":"namespace","name":"mcp__codegraph","tools":[{"type":"function","name":"codegraph_status","parameters":{"type":"object"}}]}]`

	for _, stream := range []bool{true, false} {
		label := "buffered"
		if stream {
			label = "stream"
		}
		t.Run(label, func(t *testing.T) {
			c := &testClient{name: "t", events: []core.Event{
				{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "call_1", Name: "mcp__codegraph__codegraph_status"}},
				{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, Arguments: "{}"}},
				{Type: core.EventDone, Finish: "tool_calls"},
			}}
			srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

			streamFlag := "false"
			if stream {
				streamFlag = "true"
			}
			rec := responses(t, srv, `{"model":"t/m1","input":"status please","stream":`+streamFlag+`,`+tools+`}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}

			var name, namespace any
			if stream {
				done := firstOfType(decodeSSE(t, rec.Body.String()), "response.output_item.done")
				if done == nil {
					t.Fatalf("no output_item.done; events = %v", sseType(decodeSSE(t, rec.Body.String())))
				}
				item := done["item"].(map[string]any)
				name, namespace = item["name"], item["namespace"]
			} else {
				var resp responseObject
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode: %v; body = %s", err, rec.Body)
				}
				var call *responseOutput
				for i := range resp.Output {
					if resp.Output[i].Type == "function_call" {
						call = &resp.Output[i]
					}
				}
				if call == nil {
					t.Fatalf("no function_call in %+v", resp.Output)
				}
				name, namespace = call.Name, call.Namespace
			}

			if name != "codegraph_status" {
				t.Errorf("name = %v, want the child name codegraph_status", name)
			}
			if namespace != "mcp__codegraph" {
				t.Errorf("namespace = %v, want mcp__codegraph", namespace)
			}
		})
	}
}

// TestResponses_ReplayedNamespacedCallKeepsTheVendorName: Codex replays the call
// it made as a (namespace, name) pair.  The Chat history has to spell it the way
// the vendor was offered it, or the assistant turn cites a tool the model never
// saw and the tool result no longer lines up.
func TestResponses_ReplayedNamespacedCallKeepsTheVendorName(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"input": [
			{"type":"message","role":"user","content":"status?"},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"codegraph_status","namespace":"mcp__codegraph","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"{\"ok\":true}"}
		]
	}`
	if rec := responses(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var callName, resultName string
	for _, m := range c.seen.Messages {
		if len(m.ToolCalls) > 0 {
			callName = m.ToolCalls[0].Name
		}
		if m.Role == "tool" {
			resultName = m.Name
		}
	}
	if want := "mcp__codegraph__codegraph_status"; callName != want {
		t.Errorf("replayed call name = %q, want %q", callName, want)
	}
	if resultName != callName {
		t.Errorf("tool result name = %q, want it to match the call name %q", resultName, callName)
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
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
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

func TestResponses_ContextWindowErrorIsActionable(t *testing.T) {
	err := core.Fail("t", "acct", core.FailureContextWindow, http.StatusBadRequest,
		io.ErrUnexpectedEOF)
	srv := newTestServer(t, &testClient{name: "t", chatErr: err}, NewStats(), NewUsageStore(10))

	rec := responses(t, srv, `{"model":"t/m1","input":"hi"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
	var env apiErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body = %s", err, rec.Body)
	}
	if env.Error.Code != "context_window_exceeded" {
		t.Errorf("error.code = %v, want context_window_exceeded", env.Error.Code)
	}
	if env.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", env.Error.Type)
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

func TestAppendTailKeepsTheEnd(t *testing.T) {
	var b strings.Builder
	appendTail(&b, "hello ", 6)
	appendTail(&b, "world", 6)
	if b.String() != " world" {
		t.Fatalf("tail = %q, want the last six bytes", b.String())
	}
	appendTail(&b, "", 6)
	if b.String() != " world" {
		t.Fatalf("empty append changed the tail: %q", b.String())
	}
}

// TestResponses_CustomToolCallKeepsItsFreeformInput pins the translation of a
// replayed custom tool call.  Responses carries those as "custom_tool_call"
// items whose payload is freeform text in "input" -- there is no "arguments"
// field at all.  Reading it as a plain function call produced the empty string
// in the assistant history, and an upstream that validates arguments refused
// the whole request with "Assistant tool call function.arguments must be valid
// JSON".  The item has to reach the vendor as a call with a JSON object.
func TestResponses_CustomToolCallKeepsItsFreeformInput(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"input": [
			{"type":"custom_tool_call","id":"ctc_1","call_id":"call_exec","name":"exec","input":"const x = 1;\ntext(String(x));"},
			{"type":"custom_tool_call_output","call_id":"call_exec","output":"1"}
		]
	}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	call := firstToolCall(t, c.seen)
	if call.Name != "exec" {
		t.Errorf("tool name = %q, want exec", call.Name)
	}
	if !json.Valid([]byte(call.Arguments)) {
		t.Fatalf("custom tool call arguments = %q, want valid JSON", call.Arguments)
	}
	if !strings.Contains(call.Arguments, "const x = 1;") {
		t.Errorf("the freeform input was dropped: %q", call.Arguments)
	}
}

// TestResponses_MalformedReplayedArgumentsAreRepaired covers the dirty history
// a strict vendor refuses: the assistant turn is replayed with a tool-call
// arguments string that is not JSON at all.
func TestResponses_MalformedReplayedArgumentsAreRepaired(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))

	body := `{
		"model": "t/m1",
		"input": [
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"exec_command","arguments":"{\"cmd\": \"echo hi\", \"prefix_rule\": pwsh, \"tty\": false}"},
			{"type":"function_call_output","call_id":"call_1","output":"hi"}
		]
	}`
	rec := responses(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	call := firstToolCall(t, c.seen)
	if !json.Valid([]byte(call.Arguments)) {
		t.Fatalf("replayed arguments = %q, want valid JSON", call.Arguments)
	}
}

// firstToolCall returns the only tool call the module saw, or fails.
func firstToolCall(t *testing.T, req *core.ChatRequest) core.ToolCall {
	t.Helper()
	if req == nil {
		t.Fatal("module never saw a request")
	}
	for i := range req.Messages {
		if len(req.Messages[i].ToolCalls) > 0 {
			return req.Messages[i].ToolCalls[0]
		}
	}
	t.Fatalf("no tool call reached the module: %+v", req.Messages)
	return core.ToolCall{}
}
