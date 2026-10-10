package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"

	"client2api/internal/core"
)

func TestResponsesReplayPreservesAssistantText(t *testing.T) {
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	first := responses(t, srv, `{"model":"t/m1","input":"hi"}`)
	var reply responseObject
	if err := json.Unmarshal(first.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	history := []any{map[string]any{"role": "user", "content": "hi"}}
	for _, item := range reply.Output {
		history = append(history, item)
	}
	history = append(history, map[string]any{"role": "user", "content": "continue"})
	body, _ := json.Marshal(map[string]any{"model": "t/m1", "input": history})
	second := responses(t, srv, string(body))
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", second.Code, second.Body)
	}
	if len(c.seen.Messages) != 3 || c.seen.Messages[1].Role != "assistant" || c.seen.Messages[1].Content != "hello" {
		t.Fatalf("replayed messages = %+v", c.seen.Messages)
	}
}

func TestResponsesImageInputPreservesURLAndDetail(t *testing.T) {
	c := &testClient{name: "t", events: usageEvents(1, 1)}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := responses(t, srv, `{"model":"t/m1","input":[{"role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	want := []core.ContentPart{{Type: "text", Text: "inspect"}, {Type: "image_url", ImageURL: "data:image/png;base64,AA==", Detail: "high"}}
	if !reflect.DeepEqual(c.seen.Messages[0].Parts, want) {
		t.Fatalf("image parts = %+v", c.seen.Messages[0].Parts)
	}
}

func TestResponsesStreamItemLifecycle(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Reasoning: "consider"},
		{Type: core.EventDelta, Delta: "answer"},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 7, Arguments: `{"x":`}},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 2, ID: "call_b", Name: "second", Arguments: `{}`}},
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 7, ID: "call_a", Name: "first", Arguments: `1}`}},
		{Type: core.EventDelta, Delta: "!"},
		{Type: core.EventDone, Finish: "tool_calls"},
	}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
	rec := responses(t, srv, `{"model":"t/m1","stream":true,"input":"run"}`)
	events := decodeSSE(t, rec.Body.String())
	added := map[string]int{}
	done := map[int]map[string]any{}
	var order []string
	for _, ev := range events {
		kind := ev["type"]
		switch kind {
		case "response.output_item.added":
			item := ev["item"].(map[string]any)
			id := item["id"].(string)
			ix := int(ev["output_index"].(float64))
			if _, exists := added[id]; exists || ix != len(order) {
				t.Fatalf("duplicate or inconsistent item: %v", ev)
			}
			added[id] = ix
			order = append(order, item["type"].(string))
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
			id := ev["item_id"].(string)
			ix, exists := added[id]
			if !exists || float64(ix) != ev["output_index"] {
				t.Errorf("delta without matching earlier added item: %v", ev)
			}
		case "response.output_item.done":
			item := ev["item"].(map[string]any)
			ix := int(ev["output_index"].(float64))
			if known, exists := added[item["id"].(string)]; !exists || known != ix {
				t.Errorf("done identity changed: %v", ev)
			}
			done[ix] = item
		}
	}
	if !reflect.DeepEqual(order, []string{"reasoning", "message", "function_call", "function_call"}) {
		t.Errorf("item appearance order = %v", order)
	}
	completed := firstOfType(events, "response.completed")["response"].(map[string]any)
	output := completed["output"].([]any)
	if len(output) != 4 || len(done) != 4 {
		t.Fatalf("output = %v, done = %v", output, done)
	}
	for ix, item := range output {
		if !reflect.DeepEqual(item, done[ix]) {
			t.Errorf("final output[%d] differs from done: %v / %v", ix, item, done[ix])
		}
	}
	if output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"] != "consider" {
		t.Error("final reasoning summary lost")
	}
	if output[2].(map[string]any)["call_id"] != "call_a" {
		t.Error("late tool call ID lost")
	}
}

func TestResponsesTokenLimitIsIncomplete(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			c := &testClient{name: "t", events: []core.Event{{Type: core.EventDelta, Delta: "partial"}, {Type: core.EventDone, Finish: "length"}}}
			srv := newTestServer(t, c, NewStats(), NewUsageStore(10))
			body, _ := json.Marshal(map[string]any{"model": "t/m1", "input": "hi", "stream": streaming})
			rec := responses(t, srv, string(body))
			var resp map[string]any
			if streaming {
				ev := firstOfType(decodeSSE(t, rec.Body.String()), "response.incomplete")
				if ev == nil {
					t.Fatalf("no response.incomplete: %s", rec.Body)
				}
				resp = ev["response"].(map[string]any)
			} else if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if resp["status"] != "incomplete" {
				t.Fatalf("status = %v", resp["status"])
			}
			details, _ := resp["incomplete_details"].(map[string]any)
			if details["reason"] != "max_output_tokens" {
				t.Fatalf("incomplete details = %v", details)
			}
		})
	}
}

func TestResponsesStopsAtTerminalEvent(t *testing.T) {
	t.Run("buffered done", func(t *testing.T) {
		c := &testClient{name: "t", events: []core.Event{
			{Type: core.EventDelta, Delta: "ok"},
			{Type: core.EventDone, Finish: "stop"},
			{Type: core.EventError, Err: io.ErrUnexpectedEOF},
		}}
		rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), `{"model":"t/m1","input":"hi"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("continued reading past done: %d %s", rec.Code, rec.Body)
		}
	})
	t.Run("stream error", func(t *testing.T) {
		c := &testClient{name: "t", events: []core.Event{
			{Type: core.EventDelta, Delta: "partial"},
			{Type: core.EventError, Err: io.ErrUnexpectedEOF},
			{Type: core.EventDelta, Delta: "after error"},
		}}
		rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), `{"model":"t/m1","stream":true,"input":"hi"}`)
		for _, ev := range decodeSSE(t, rec.Body.String()) {
			if ev["delta"] == "after error" {
				t.Fatal("continued reading past terminal error")
			}
		}
	})
}
