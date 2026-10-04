package loomy

import (
	"encoding/json"
	"testing"

	"client2api/internal/core"
)

// body_test.go pins the wire body.  /chat/completions is OpenAI-shaped, so the
// value here is not cleverness but the handful of decisions that are easy to get
// silently wrong: that the request always streams, that an empty string is never
// sent where the vendor wants an absent field, and that a tool without a name is
// dropped rather than sent as an unusable entry.

func buildBodyMap(t *testing.T, req *core.ChatRequest) map[string]any {
	t.Helper()
	raw, err := buildBody(req)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("buildBody produced invalid JSON (%v): %s", err, raw)
	}
	return out
}

func messageAt(t *testing.T, body map[string]any, index int) map[string]any {
	t.Helper()
	messages, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v, want an array", body["messages"])
	}
	if index >= len(messages) {
		t.Fatalf("messages has %d entries, want at least %d", len(messages), index+1)
	}
	msg, ok := messages[index].(map[string]any)
	if !ok {
		t.Fatalf("messages[%d] = %#v, want an object", index, messages[index])
	}
	return msg
}

// This module only ever opens a streamed completion, so `stream` is not a
// caller-controlled field: it is a constant of the transport.
func TestBuildBodyAlwaysAsksForAStream(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if got := body["stream"]; got != true {
		t.Errorf("stream = %#v, want true", got)
	}
	if got := body["model"]; got != "deepseek-v4-flash-0731" {
		t.Errorf("model = %#v, want the resolved id echoed back", got)
	}
}

// An assistant turn that carries only tool calls has no text.  Sending
// `"content": ""` is rejected by some OpenAI-compatible servers, so the field
// must be absent or null -- never an empty string.
func TestBuildBodyNeverSendsAnEmptyContentString(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{{
			Role:      "assistant",
			ToolCalls: []core.ToolCall{{ID: "call_1", Name: "get_time", Arguments: `{}`}},
		}},
	})
	msg := messageAt(t, body, 0)
	if value, present := msg["content"]; present && value != nil {
		t.Errorf("content = %#v, want it absent or null for a tool-only turn", value)
	}
	if value, present := msg["content"]; present && value == "" {
		t.Error("content was sent as an empty string")
	}
}

func TestBuildBodyRendersStructuredPartsAsAContentArray(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{{
			Role: "user",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what is this"},
				{Type: "image_url", ImageURL: "https://example.test/a.png", Detail: "high"},
			},
		}},
	})
	msg := messageAt(t, body, 0)
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want an array when the message has parts", msg["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("content has %d parts, want 2", len(parts))
	}
	text, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("parts[0] = %#v", parts[0])
	}
	if text["type"] != "text" || text["text"] != "what is this" {
		t.Errorf("parts[0] = %#v, want a text part", text)
	}
	image, ok := parts[1].(map[string]any)
	if !ok {
		t.Fatalf("parts[1] = %#v", parts[1])
	}
	if image["type"] != "image_url" {
		t.Errorf("parts[1].type = %#v, want image_url", image["type"])
	}
	url, ok := image["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url = %#v, want an object", image["image_url"])
	}
	if url["url"] != "https://example.test/a.png" || url["detail"] != "high" {
		t.Errorf("image_url = %#v, want the url and detail preserved", url)
	}
}

func TestBuildBodyDropsANamelessToolAndKeepsToolChoice(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools: []core.Tool{
			{Name: "get_time", Description: "reads the clock", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Description: "a tool with no name is unusable and must be dropped"},
		},
		ToolChoice: json.RawMessage(`{"type":"function","function":{"name":"get_time"}}`),
	})

	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly the one named tool", body["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok {
		t.Fatalf("tools[0] = %#v", tools[0])
	}
	if tool["type"] != "function" {
		t.Errorf("tool.type = %#v, want the default \"function\"", tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool.function = %#v", tool["function"])
	}
	if fn["name"] != "get_time" || fn["description"] != "reads the clock" {
		t.Errorf("function = %#v", fn)
	}
	if _, present := fn["parameters"]; !present {
		t.Error("the tool's parameters schema was dropped")
	}
	if _, present := body["tool_choice"]; !present {
		t.Error("tool_choice was dropped even though a tool was sent")
	}
}

func TestBuildBodyDropsToolChoiceWhenThereAreNoTools(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model:      "m",
		Messages:   []core.Message{{Role: "user", Content: "hi"}},
		ToolChoice: json.RawMessage(`{"type":"function"}`),
	})
	if value, present := body["tool_choice"]; present {
		t.Errorf("tool_choice = %#v, want it dropped when no tools were sent", value)
	}
}

// The vendor rejects a bare JSON null, so a caller that spells "no preference"
// as the literal `null` must not have it forwarded.
func TestBuildBodyDropsABareNullToolChoice(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model:      "m",
		Messages:   []core.Message{{Role: "user", Content: "hi"}},
		Tools:      []core.Tool{{Name: "get_time"}},
		ToolChoice: json.RawMessage(`null`),
	})
	if value, present := body["tool_choice"]; present {
		t.Errorf("tool_choice = %#v, want a literal null dropped", value)
	}
}

// A pointer field distinguishes "the caller asked for zero" from "the caller
// said nothing", which is exactly why these fields are pointers.
func TestBuildBodyKeepsZeroTheCallerSetAndOmitsWhatTheyDidNot(t *testing.T) {
	zero := 0.0
	tokens := 4096
	body := buildBodyMap(t, &core.ChatRequest{
		Model:       "m",
		Messages:    []core.Message{{Role: "user", Content: "hi"}},
		Temperature: &zero,
		MaxTokens:   &tokens,
	})
	got, present := body["temperature"]
	if !present || got != 0.0 {
		t.Errorf("temperature = %#v (present %v), want an explicit 0", got, present)
	}
	if got := body["max_tokens"]; got != float64(4096) {
		t.Errorf("max_tokens = %#v, want 4096", got)
	}
	for _, absent := range []string{"top_p", "stop"} {
		if value, present := body[absent]; present {
			t.Errorf("%s = %#v, want it omitted when the caller never set it", absent, value)
		}
	}
}

func TestBuildBodyForwardsTheReasoningEffortFromOptions(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
		want    string
	}{
		{"snake case", map[string]any{"reasoning_effort": "xhigh"}, "xhigh"},
		{"camel case", map[string]any{"reasoningEffort": "low"}, "low"},
		{"reasoning", map[string]any{"reasoning": "medium"}, "medium"},
		{"effort", map[string]any{"effort": "high"}, "high"},
		{"the documented key wins over the aliases", map[string]any{"effort": "high", "reasoning_effort": "none"}, "none"},
		// The vendor ignores an effort it does not know rather than failing, so
		// the value is forwarded verbatim instead of being validated here.
		{"an unknown tier is forwarded verbatim", map[string]any{"reasoning_effort": "turbo"}, "turbo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildBodyMap(t, &core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
				Options:  tc.options,
			})
			if got := body["reasoning_effort"]; got != tc.want {
				t.Errorf("reasoning_effort = %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildBodyIgnoresAnUnusableReasoningEffort(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
	}{
		{"empty string", map[string]any{"reasoning_effort": ""}},
		{"not a string", map[string]any{"reasoning_effort": 3}},
		{"nothing at all", map[string]any{}},
		{"a nil options bag", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildBodyMap(t, &core.ChatRequest{
				Model:    "m",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
				Options:  tc.options,
			})
			if value, present := body["reasoning_effort"]; present {
				t.Errorf("reasoning_effort = %#v, want the field omitted so the vendor's own default applies", value)
			}
		})
	}
}

func TestBuildBodyDefaultsTheToolCallType(t *testing.T) {
	body := buildBodyMap(t, &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{{
			Role:      "assistant",
			ToolCalls: []core.ToolCall{{ID: "call_1", Name: "get_time", Arguments: `{"zone":"UTC"}`}},
		}},
	})
	msg := messageAt(t, body, 0)
	calls, ok := msg["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v, want one entry", msg["tool_calls"])
	}
	call, ok := calls[0].(map[string]any)
	if !ok {
		t.Fatalf("tool_calls[0] = %#v", calls[0])
	}
	if call["type"] != "function" {
		t.Errorf("tool_call.type = %#v, want the default \"function\"", call["type"])
	}
	if call["id"] != "call_1" {
		t.Errorf("tool_call.id = %#v", call["id"])
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool_call.function = %#v", call["function"])
	}
	if fn["name"] != "get_time" || fn["arguments"] != `{"zone":"UTC"}` {
		t.Errorf("function = %#v", fn)
	}
}
