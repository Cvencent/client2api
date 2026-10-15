package gateway

import (
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// dead-turn retry
// ---------------------------------------------------------------------------
//
// The shape under test: the model reasons for a long time, ends the turn with
// "stop", and hands back nothing the caller can act on -- no visible text at
// all, or a short fragment that says it is about to act.  Codex reads that as a
// finished turn and waits for the operator to type the next prompt, which is the
// interruption this retry removes.
//
// A turn that produced a readable answer is never retried: "Done." and a
// genuine short answer are indistinguishable from a truncated one, so the
// gateway leaves them alone rather than guess.

const responsesToolsBody = `{"model":"t/m1","stream":true,"input":"hi","tools":[{"type":"function","name":"exec","parameters":{"type":"object"}}]}`

const responsesPlainBody = `{"model":"t/m1","stream":true,"input":"hi"}`

// longReasoning is a delta that clears the reasoning floor, so a short visible
// answer is the only thing standing between the attempt and a retry.
func longReasoning() core.Event {
	return core.Event{Type: core.EventDelta, Reasoning: strings.Repeat("weighing the options. ", 600)}
}

func toolCallEvent(id, name, args string) core.Event {
	return core.Event{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, ID: id, Name: name, Arguments: args}}
}

func doneEvent(finish string) core.Event {
	return core.Event{Type: core.EventDone, Finish: finish}
}

// functionCallItem returns the function-call output item a Responses stream
// dispatched, or nil.
func functionCallItem(events []map[string]any) map[string]any {
	for _, e := range events {
		if typ, _ := e["type"].(string); typ != "response.output_item.done" {
			continue
		}
		item, ok := e["item"].(map[string]any)
		if !ok {
			continue
		}
		if item["type"] == "function_call" {
			return item
		}
	}
	return nil
}

func TestDeadTurnWithNoVisibleTextIsRetried(t *testing.T) {
	c := &testClient{name: "t", callEvents: [][]core.Event{
		{longReasoning(), doneEvent("stop")},
		{{Type: core.EventDelta, Delta: "on it"}, toolCallEvent("call_1", "exec", "{}"), doneEvent("tool_calls")},
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	frames := decodeSSE(t, rec.Body.String())
	if !hasType(frames, "response.completed") {
		t.Fatalf("the retry was not served: %s", rec.Body)
	}
	if item := functionCallItem(frames); item == nil || item["name"] != "exec" {
		t.Fatalf("the retry's tool call is missing: %s", rec.Body)
	}
	if c.chatCalls != 2 {
		t.Fatalf("upstream attempts = %d, want 2", c.chatCalls)
	}
}

func TestNarratedIntentWithNoToolCallIsRetried(t *testing.T) {
	c := &testClient{name: "t", callEvents: [][]core.Event{
		{longReasoning(), {Type: core.EventDelta, Delta: "Let me get the exact text first."}, doneEvent("stop")},
		{toolCallEvent("call_1", "exec", "{}"), doneEvent("tool_calls")},
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	frames := decodeSSE(t, rec.Body.String())
	if !hasType(frames, "response.completed") {
		t.Fatalf("the retry was not served: %s", rec.Body)
	}
	if item := functionCallItem(frames); item == nil || item["name"] != "exec" {
		t.Fatalf("the retry's tool call is missing: %s", rec.Body)
	}
	if c.chatCalls != 2 {
		t.Fatalf("upstream attempts = %d, want 2", c.chatCalls)
	}
}

func TestDeadTurnIsRetriedOnlyOnce(t *testing.T) {
	c := &testClient{name: "t", callEvents: [][]core.Event{
		{longReasoning(), doneEvent("stop")},
		{longReasoning(), doneEvent("stop")},
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	frames := decodeSSE(t, rec.Body.String())
	if hasType(frames, "response.completed") {
		t.Fatalf("a turn with no text and no tool call was reported as an answer: %s", rec.Body)
	}
	if !hasType(frames, "response.failed") {
		t.Fatalf("a dead turn must be reported as a failure: %s", rec.Body)
	}
	if c.chatCalls != 2 {
		t.Fatalf("upstream attempts = %d, want exactly 2", c.chatCalls)
	}
}

func TestShortAnswerIsNotRetried(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		{Type: core.EventDelta, Reasoning: "The tool call returned ok."},
		{Type: core.EventDelta, Delta: "Done."},
		doneEvent("stop"),
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	frames := decodeSSE(t, rec.Body.String())
	if !hasType(frames, "response.completed") {
		t.Fatalf("a short answer was not delivered: %s", rec.Body)
	}
	if c.chatCalls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", c.chatCalls)
	}
}

func TestNarratedIntentWithoutToolsIsNotRetried(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		longReasoning(),
		{Type: core.EventDelta, Delta: "Let me know if you want me to look."},
		doneEvent("stop"),
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesPlainBody)
	frames := decodeSSE(t, rec.Body.String())
	if !hasType(frames, "response.completed") {
		t.Fatalf("an answer to a request that offered no tools was not delivered: %s", rec.Body)
	}
	if c.chatCalls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", c.chatCalls)
	}
}

func TestToolCallIsNeverRetried(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		longReasoning(),
		toolCallEvent("call_1", "exec", "{}"),
		doneEvent("tool_calls"),
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	if !hasType(decodeSSE(t, rec.Body.String()), "response.completed") {
		t.Fatalf("a turn with a tool call was not delivered: %s", rec.Body)
	}
	if c.chatCalls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", c.chatCalls)
	}
}

func TestSubstantialAnswerIsNotRetried(t *testing.T) {
	c := &testClient{name: "t", events: []core.Event{
		longReasoning(),
		{Type: core.EventDelta, Delta: strings.Repeat("answer ", 300)},
		doneEvent("stop"),
	}}
	rec := responses(t, newTestServer(t, c, NewStats(), NewUsageStore(10)), responsesToolsBody)
	if !hasType(decodeSSE(t, rec.Body.String()), "response.completed") {
		t.Fatalf("a substantial answer was not delivered: %s", rec.Body)
	}
	if c.chatCalls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", c.chatCalls)
	}
}
