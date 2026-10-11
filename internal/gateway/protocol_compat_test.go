package gateway

import (
	"bytes"
	"encoding/json"

	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestChatStreamOmitsUnsetRoleForStrictClients(t *testing.T) {
	for _, terminal := range []string{"stop"} {
		t.Run(terminal, func(t *testing.T) {
			events := []core.Event{
				{Type: core.EventDelta, Reasoning: "think"},
				{Type: core.EventDelta, Delta: "answer"},
				{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "call_1", Name: "exec", Arguments: `{}`}},
				{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}},
			}
			if terminal != "eof" {
				events = append(events, core.Event{Type: core.EventDone, Finish: terminal})
			}
			c := &testClient{name: "t", events: events}
			rec := chat(t, newTestServer(t, c, NewStats(), NewUsageStore(10)),
				`{"model":"t/m1","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			chunks := decodeSSE(t, strings.ReplaceAll(rec.Body.String(), "data: [DONE]\n\n", ""))
			var sawText, sawReasoning, sawTool, sawFinish, sawUsage bool
			for i, chunk := range chunks {
				choices := chunk["choices"].([]any)
				for _, value := range choices {
					choice := value.(map[string]any)
					delta := choice["delta"].(map[string]any)
					if role, present := delta["role"]; present && role != "assistant" {
						t.Errorf("chunk %d: invalid delta.role = %v; strict clients require assistant or an absent key", i, role)
					}
					if i == 0 && delta["role"] != "assistant" {
						t.Error("first chunk does not identify the assistant")
					}
					sawText = sawText || delta["content"] == "answer"
					sawReasoning = sawReasoning || delta["reasoning_content"] == "think"
					sawTool = sawTool || delta["tool_calls"] != nil
					sawFinish = sawFinish || choice["finish_reason"] != nil
				}
				sawUsage = sawUsage || chunk["usage"] != nil
			}
			if !sawText || !sawReasoning || !sawTool || !sawFinish || !sawUsage {
				t.Fatalf("missing stream content: text=%v reasoning=%v tool=%v finish=%v usage=%v",
					sawText, sawReasoning, sawTool, sawFinish, sawUsage)
			}
		})
	}
}

func TestUnterminatedStreamsFailAcrossProtocols(t *testing.T) {
	for _, protocol := range []string{"chat-stream", "chat-buffered", "responses-stream", "responses-buffered"} {
		t.Run(protocol, func(t *testing.T) {
			c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "partial"}}}
			srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
			switch protocol {
			case "chat-stream":
				rec := chat(t, srv, `{"model":"t/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
				if strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) || strings.Contains(rec.Body.String(), "data: [DONE]") || !strings.Contains(rec.Body.String(), "upstream stream ended before a terminal frame") {
					t.Fatalf("truncated stream terminal markers are wrong: %s", rec.Body)
				}
			case "chat-buffered":
				rec := chat(t, srv, `{"model":"t/m1","messages":[{"role":"user","content":"hi"}]}`)
				if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "unexpected EOF") {
					t.Fatalf("truncated buffered response was not reported: %d %s", rec.Code, rec.Body)
				}
			case "responses-stream":
				rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"hi"}`)
				frames := decodeSSE(t, rec.Body.String())
				if hasType(frames, "response.completed") || !hasType(frames, "response.failed") {
					t.Fatalf("truncated Responses stream terminal events: %s", rec.Body)
				}
			case "responses-buffered":
				rec := responses(t, srv, `{"model":"t/m1","input":"hi"}`)
				if rec.Code == http.StatusOK || !strings.Contains(rec.Body.String(), "unexpected EOF") {
					t.Fatalf("truncated buffered response was not reported: %d %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
func TestResponsesPreservesRequiredEmptyOutputFields(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, text := range []string{"answer", ""} {
			name := "buffered"
			if streaming {
				name = "streaming"
			}
			if text == "" {
				name += " empty text"
			}
			t.Run(name, func(t *testing.T) {
				c := &testClient{name: "t", events: []core.Event{
					{Type: core.EventDelta, Delta: text},
					{Type: core.EventDone, Finish: "stop"},
				}}
				body, _ := json.Marshal(map[string]any{"model": "t/m1", "input": "hi", "stream": streaming})
				rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), string(body))
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
				}
				checkContent := func(part map[string]any) {
					t.Helper()
					if got, ok := part["text"].(string); !ok || got != text {
						t.Errorf("output_text requires a text string, including when empty: %v", part)
					}
					if annotations, ok := part["annotations"].([]any); !ok || len(annotations) != 0 {
						t.Errorf("output_text requires an annotations array, including when empty: %v", part)
					}
				}
				var reply map[string]any
				if streaming {
					events := decodeSSE(t, rec.Body.String())
					for _, ev := range events {
						switch ev["type"] {
						case "response.content_part.done":
							checkContent(ev["part"].(map[string]any))
						case "response.output_item.done":
							item := ev["item"].(map[string]any)
							checkContent(item["content"].([]any)[0].(map[string]any))
						}
					}
					reply = firstOfType(events, "response.completed")["response"].(map[string]any)
				} else if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
					t.Fatal(err)
				}
				item := reply["output"].([]any)[0].(map[string]any)
				checkContent(item["content"].([]any)[0].(map[string]any))
			})
		}
	}
}

func TestResponsesPreservesEmptyFunctionArguments(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			c := &testClient{name: "t", events: []core.Event{
				{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: "call_1", Name: "exec"}},
				{Type: core.EventDone, Finish: "tool_calls"},
			}}
			body, _ := json.Marshal(map[string]any{"model": "t/m1", "input": "hi", "stream": streaming})
			rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), string(body))
			var reply map[string]any
			if streaming {
				events := decodeSSE(t, rec.Body.String())
				reply = firstOfType(events, "response.completed")["response"].(map[string]any)
				item := firstOfType(events, "response.output_item.done")["item"].(map[string]any)
				if got, ok := item["arguments"].(string); !ok || got != "" {
					t.Errorf("function_call.done omitted its empty arguments string: %v", item)
				}
			} else if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, value := range reply["output"].([]any) {
				item := value.(map[string]any)
				if item["type"] != "function_call" {
					continue
				}
				found = true
				if got, ok := item["arguments"].(string); !ok || got != "" {
					t.Errorf("function_call omitted its empty arguments string: %v", item)
				}
			}
			if !found {
				t.Fatal("function call missing from output")
			}
		})
	}
}

func TestChatStreamErrorDoesNotEmitSuccessfulTerminal(t *testing.T) {
	usage := NewUsageStore(10)
	stats := NewStats()
	logger := &bytes.Buffer{}
	reg := core.NewRegistry()
	reg.Add(&testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "partial"},
		{Type: core.EventError, Err: io.ErrUnexpectedEOF},
	}})
	srv := NewServer(Options{Registry: reg, Version: "test", Logger: log.New(logger, "", 0), Stats: stats, Usage: usage})
	rec := chat(t, srv, `{"model":"t/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec.Body.String()
	if strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "data: [DONE]") {
		t.Fatalf("failed stream contains a successful terminal marker: %s", body)
	}
	if !strings.Contains(body, "upstream_error") {
		t.Fatalf("failed stream omitted its error event: %s", body)
	}
	rows := usage.Snapshot()
	if len(rows) != 1 || !rows[0].Failed {
		t.Fatalf("failed stream not recorded as failed: %+v", rows)
	}
	if stats.Failures() != 1 {
		t.Fatalf("failures = %d, want exactly one", stats.Failures())
	}
	if !strings.Contains(logger.String(), "protocol=chat") || !strings.Contains(logger.String(), "terminal=upstream_unexpected_eof") || !strings.Contains(logger.String(), "events=2") {
		t.Fatalf("stream termination diagnostics missing: %s", logger.String())
	}
	if strings.Contains(logger.String(), "partial") {
		t.Fatalf("diagnostics must not contain response content: %s", logger.String())
	}
}

func TestResponsesStreamIncludesSequenceNumbers(t *testing.T) {
	for _, terminal := range []string{"stop", "length", "error"} {
		t.Run(terminal, func(t *testing.T) {
			events := []core.Event{{Type: core.EventDelta, Delta: "partial"}}
			if terminal == "error" {
				events = append(events, core.Event{Type: core.EventError, Err: io.ErrUnexpectedEOF})
			} else {
				events = append(events, core.Event{Type: core.EventDone, Finish: terminal})
			}
			c := &testClient{name: "t", events: events}
			rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)),
				`{"model":"t/m1","input":"hi","stream":true}`)
			frames := decodeSSE(t, rec.Body.String())
			for i, frame := range frames {
				if seq, ok := frame["sequence_number"].(float64); !ok || seq != float64(i) {
					t.Errorf("frame %d (%v): sequence_number = %v", i, frame["type"], frame["sequence_number"])
				}
			}
			want := map[string]string{"stop": "response.completed", "length": "response.incomplete", "error": "response.failed"}[terminal]
			if !hasType(frames, want) {
				t.Fatalf("missing terminal event %s: %s", want, rec.Body)
			}
		})
	}
}

func TestResponsesStreamDiagnosticsDistinguishLengthFromStop(t *testing.T) {
	var logger strings.Builder
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Delta: "partial"},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, Name: "f", Arguments: "{}"}},
		{Type: core.EventUsage, Usage: &core.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}},
		{Type: core.EventDone, Finish: "length"},
	}}
	reg := core.NewRegistry()
	reg.Add(c)
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(&logger, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := logger.String()
	for _, want := range []string{
		"protocol=responses",
		"terminal=incomplete",
		"finish=length",
		"delta_bytes=7",
		"tool_calls=1",
		"completion_tokens=4",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostics missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "partial") {
		t.Fatalf("diagnostics must not contain response content: %s", got)
	}
}
