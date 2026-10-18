package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"client2api/internal/core"
)

// A relay that fronts DeepSeek answers a forced tool choice with
// MODEL_TOOL_CHOICE_NOT_SUPPORTED, whose message reads like the model cannot
// call tools at all.  Only "auto" (and "none", which disables tools) may be
// forwarded; everything else has to become an automatic choice.
func TestNormalizeToolChoiceDowngradesForcedChoices(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"absent", ``, ``},
		{"null", `null`, ``},
		{"empty string", `""`, ``},
		{"auto", `"auto"`, `"auto"`},
		{"auto mixed case", `"Auto"`, `"Auto"`},
		{"none", `"none"`, `"none"`},
		{"required", `"required"`, `"auto"`},
		{"named function string", `"my_tool"`, `"auto"`},
		{"named function object", `{"type":"function","function":{"name":"my_tool"}}`, `"auto"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolChoice(json.RawMessage(tc.in))
			if string(got) != tc.want {
				t.Fatalf("normalizeToolChoice(%s) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// A schema without "properties" -- most visibly the literal "{}" that a
// namespace or MCP declaration produces -- makes a DeepSeek-fronting relay
// report MODEL_TOOL_NOT_SUPPORTED.  The skeleton has to be filled in.
func TestNormalizeToolSchemaFillsTheObjectSkeleton(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"nil", ``},
		{"null", `null`},
		{"empty object", `{}`},
		{"type only", `{"type":"object"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeToolSchema(json.RawMessage(tc.in))
			var obj map[string]any
			if err := json.Unmarshal(got, &obj); err != nil {
				t.Fatalf("normalizeToolSchema(%s) = %s, not an object: %v", tc.in, got, err)
			}
			if obj["type"] != "object" {
				t.Errorf("type = %v, want object", obj["type"])
			}
			props, ok := obj["properties"].(map[string]any)
			if !ok || props == nil {
				t.Errorf("properties = %#v, want an object", obj["properties"])
			}
		})
	}

	// A real declaration keeps every key it came with.
	real := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)
	if got := normalizeToolSchema(real); string(got) != string(real) {
		t.Errorf("a complete schema was rewritten:\n got %s\nwant %s", got, real)
	}

	// A non-object passes through untouched rather than being invented.
	odd := json.RawMessage(`"not an object"`)
	if g := normalizeToolSchema(odd); string(g) != string(odd) {
		t.Errorf("non-object schema = %s, want it untouched", g)
	}
}

// The assistant turn is where the vendor demands reasoning_content, and the key
// must be present even when empty: omitting it is the 400, null is a different
// 400.  Other roles must not carry the key at all.
func TestWireMessageCarriesReasoningContentOnAssistantTurns(t *testing.T) {
	blank := wireMessage(core.Message{Role: "assistant", Reasoning: "   "})
	if blank.ReasoningContent == nil {
		t.Fatal("an assistant turn without a trace must still carry an empty reasoning_content")
	}
	if *blank.ReasoningContent != "" {
		t.Errorf("reasoning_content = %q, want an empty string", *blank.ReasoningContent)
	}
	raw, err := json.Marshal(blank)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"reasoning_content":""`) {
		t.Errorf("encoded assistant turn has no empty reasoning_content: %s", raw)
	}

	traced := wireMessage(core.Message{Role: "assistant", Reasoning: "why"})
	if traced.ReasoningContent == nil || *traced.ReasoningContent != "why" {
		t.Errorf("reasoning_content = %v, want the trace", traced.ReasoningContent)
	}
	// The OpenRouter spelling rides along for the relays that want it.
	if traced.Reasoning != "why" {
		t.Errorf("reasoning = %q, want the trace as well", traced.Reasoning)
	}

	user := wireMessage(core.Message{Role: "user", Content: "hi"})
	if user.ReasoningContent != nil {
		t.Errorf("a user turn must not carry reasoning_content: %v", *user.ReasoningContent)
	}
}

// wireTools must never emit a schema the relays reject, for any of the shapes
// an MCP/namespace declaration can produce.
func TestWireToolsNeverEmitsABareSchema(t *testing.T) {
	shapes := []core.Tool{
		{Name: "a"},
		{Name: "b", Parameters: json.RawMessage(`{}`)},
		{Name: "c", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "d", Parameters: json.RawMessage(`null`)},
	}
	out := wireTools(shapes)
	if len(out) != len(shapes) {
		t.Fatalf("wireTools returned %d tools, want %d", len(out), len(shapes))
	}
	for _, tool := range out {
		var obj map[string]any
		if err := json.Unmarshal(tool.Function.Parameters, &obj); err != nil {
			t.Errorf("tool %s: schema %s is not an object", tool.Function.Name, tool.Function.Parameters)
			continue
		}
		if _, ok := obj["properties"]; !ok {
			t.Errorf("tool %s: schema %s has no properties", tool.Function.Name, tool.Function.Parameters)
		}
	}
}

// End-to-end shape check: the exact body the module now puts on the wire for
// the three cases that used to 400 against a DeepSeek-fronting relay.
func TestFixedBodyShapeMatchesWhatTheUpstreamAccepts(t *testing.T) {
	mt := 64
	req := &core.ChatRequest{
		Model: "deepseek-v4.1-flash-1017",
		Messages: []core.Message{
			{Role: "user", Content: "echo hi"},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c1", Name: "probe_echo", Arguments: `{"text":"hi"}`}}},
			{Role: "tool", ToolCallID: "c1", Content: "hi"},
		},
		Stream:    true,
		MaxTokens: &mt,
		Tools: []core.Tool{{
			Name: "probe_echo",
			// The shape a namespace/MCP declaration produces.
			Parameters: json.RawMessage(`{}`),
		}},
		// The shape Codex sends to force a call.
		ToolChoice: json.RawMessage(`"required"`),
	}
	body, err := buildChatBody(Config{}, ProviderConfig{ID: "tokenrhythm"}, "deepseek-v4.1-flash-1017", req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	raw := string(body)

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %#v, want the downgraded auto", parsed["tool_choice"])
	}
	tools, _ := parsed["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", parsed["tools"])
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	if _, ok := params["properties"]; !ok {
		t.Errorf("tool schema still bare: %#v", fn["parameters"])
	}
	if !strings.Contains(raw, `"reasoning_content":""`) {
		t.Errorf("assistant turn is missing an explicit empty reasoning_content:\n%s", raw)
	}
}
