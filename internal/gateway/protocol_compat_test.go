package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestChatStreamOmitsUnsetRoleForStrictClients(t *testing.T) {
	for _, terminal := range []string{"stop", "eof"} {
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
