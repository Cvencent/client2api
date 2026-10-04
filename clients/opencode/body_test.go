package opencode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"client2api/internal/core"
)

func ptrFloat(v float64) *float64 { return &v }
func ptrInt(v int) *int           { return &v }

// decodeBody renders a request and decodes the wire body back into a loose map,
// so a test can assert on the presence and shape of individual keys.
func decodeBody(t *testing.T, cfg Config, req *core.ChatRequest) map[string]any {
	t.Helper()
	raw, err := buildChatBody(cfg, req)
	if err != nil {
		t.Fatalf("buildChatBody = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the body is not JSON: %v (%s)", err, raw)
	}
	return got
}

func TestBuildChatBodyRejectsNilAndEmpty(t *testing.T) {
	if _, err := buildChatBody(Config{}, nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("buildChatBody(nil) = %v, want core.ErrUnsupported", err)
	}
	if _, err := buildChatBody(Config{}, &core.ChatRequest{Model: "gpt-5.1"}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("buildChatBody(no messages) = %v, want core.ErrUnsupported", err)
	}
}

// The module always asks the vendor to stream: it re-aggregates for a
// non-streaming caller, so there is one code path and the usage frame is
// readable either way.
func TestBuildChatBodyAlwaysStreams(t *testing.T) {
	for _, stream := range []bool{true, false} {
		req := chatRequest("gpt-5.1")
		req.Stream = stream
		got := decodeBody(t, Config{}, req)
		if got["stream"] != true {
			t.Fatalf("stream = %#v with req.Stream=%v, want true", got["stream"], stream)
		}
	}
}

func TestBuildChatBodyOmitsAbsentOptionalFields(t *testing.T) {
	got := decodeBody(t, Config{}, chatRequest("gpt-5.1"))
	for _, key := range []string{"temperature", "top_p", "max_tokens", "stop", "user", "tools", "tool_choice", "stream_options"} {
		if _, ok := got[key]; ok {
			t.Fatalf("%q is present (%#v) when the caller set nothing", key, got[key])
		}
	}
	if got["model"] != "gpt-5.1" {
		t.Fatalf("model = %#v", got["model"])
	}
}

func TestBuildChatBodyCarriesOptionalFields(t *testing.T) {
	req := chatRequest("claude-opus-4-5")
	req.Temperature = ptrFloat(0.25)
	req.TopP = ptrFloat(0.9)
	req.MaxTokens = ptrInt(4096)
	req.Stop = []string{"\n\n", "END"}
	req.User = "user-42"
	got := decodeBody(t, Config{}, req)

	if got["temperature"] != 0.25 || got["top_p"] != 0.9 || got["max_tokens"] != float64(4096) {
		t.Fatalf("sampling fields = %#v / %#v / %#v", got["temperature"], got["top_p"], got["max_tokens"])
	}
	if got["user"] != "user-42" {
		t.Fatalf("user = %#v", got["user"])
	}
	stop, ok := got["stop"].([]any)
	if !ok || len(stop) != 2 || stop[0] != "\n\n" || stop[1] != "END" {
		t.Fatalf("stop = %#v", got["stop"])
	}
}

// include_usage is opt-in because Zen's neutral CommonRequest has no
// stream_options field and a converter may drop it.
func TestBuildChatBodyIncludeUsageOnlyWhenConfigured(t *testing.T) {
	if _, ok := decodeBody(t, Config{}, chatRequest("gpt-5.1"))["stream_options"]; ok {
		t.Fatal("stream_options was sent without include_usage configured")
	}
	got := decodeBody(t, Config{IncludeUsage: true}, chatRequest("gpt-5.1"))
	opts, ok := got["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options = %#v", got["stream_options"])
	}
	if opts["include_usage"] != true {
		t.Fatalf("include_usage = %#v, want true", opts["include_usage"])
	}
}

// Zen's CommonMessage role union is system|user|assistant|tool, so `developer`
// is folded rather than refused.
func TestBuildChatBodyFoldsDeveloperIntoSystem(t *testing.T) {
	req := chatRequest("gpt-5.1")
	req.Messages = []core.Message{
		{Role: "developer", Content: "be terse"},
		{Role: "user", Content: "hi"},
	}
	got := decodeBody(t, Config{}, req)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %#v", got["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be terse" {
		t.Fatalf("first message = %#v, want the developer turn folded to system", first)
	}
}

// An empty role defaults to user; anything else is refused, because silently
// relabelling a turn would change the meaning of the conversation.
func TestBuildChatBodyRoleHandling(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model:    "gpt-5.1",
		Messages: []core.Message{{Content: "hi"}},
	})
	msgs, _ := got["messages"].([]any)
	if first, _ := msgs[0].(map[string]any); first["role"] != "user" {
		t.Fatalf("role = %#v, want user for an empty role", first["role"])
	}

	for _, role := range []string{"system", "user", "assistant", "tool"} {
		if _, err := buildChatBody(Config{}, &core.ChatRequest{
			Model:    "gpt-5.1",
			Messages: []core.Message{{Role: role, Content: "x"}},
		}); err != nil {
			t.Fatalf("buildChatBody refused the accepted role %q: %v", role, err)
		}
	}

	_, err := buildChatBody(Config{}, &core.ChatRequest{
		Model:    "gpt-5.1",
		Messages: []core.Message{{Role: "robot", Content: "x"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("unknown role = %v, want core.ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), `unknown role "robot"`) {
		t.Fatalf("error = %v, want it to name the role", err)
	}
}

// An assistant turn that only asks for tools must not carry an empty content
// field: some converters reject `"content": ""` next to tool_calls.
func TestBuildChatBodyToolCallOnlyTurnOmitsContent(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model: "gpt-5.1",
		Messages: []core.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []core.ToolCall{
				{ID: "call_1", Type: "function", Name: "get_weather", Arguments: `{"city":"SF"}`},
			}},
		},
	})
	msgs, _ := got["messages"].([]any)
	second, _ := msgs[1].(map[string]any)
	if _, ok := second["content"]; ok {
		t.Fatalf("content = %#v, want it omitted on a tool_calls-only turn", second["content"])
	}
	calls, ok := second["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", second["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"SF"}` {
		t.Fatalf("tool call = %#v", call)
	}
}

// A tool result must name the call it answers, and a name is passed through.
func TestBuildChatBodyToolResultCarriesTheID(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model: "gpt-5.1",
		Messages: []core.Message{
			{Role: "tool", Content: "18C", ToolCallID: "call_1", Name: "get_weather"},
		},
	})
	msgs, _ := got["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	if m["tool_call_id"] != "call_1" || m["name"] != "get_weather" || m["content"] != "18C" {
		t.Fatalf("tool message = %#v", m)
	}
}

// A tool with no declared parameters still needs an object schema: an absent
// one is a 400.
func TestBuildChatBodyFillsDefaultToolParameters(t *testing.T) {
	req := chatRequest("gpt-5.1")
	req.Tools = []core.Tool{
		{Name: "no_params"},
		{Type: "function", Name: "with_params", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)},
	}
	got := decodeBody(t, Config{}, req)
	tools, ok := got["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["type"] != "function" {
		t.Fatalf("type = %#v, want function by default", first["type"])
	}
	fn0, _ := first["function"].(map[string]any)
	params, _ := fn0["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("default parameters = %#v, want an object schema", fn0["parameters"])
	}
	if _, ok := params["properties"]; !ok {
		t.Fatalf("default parameters = %#v, want an empty properties map", fn0["parameters"])
	}

	second, _ := tools[1].(map[string]any)
	fn1, _ := second["function"].(map[string]any)
	if fn1["description"] != "d" {
		t.Fatalf("description = %#v", fn1["description"])
	}
	if declared, _ := fn1["parameters"].(map[string]any); declared["type"] != "object" {
		t.Fatalf("declared parameters = %#v", fn1["parameters"])
	}
}

// tool_choice is meaningless without tools and is omitted, not sent as null.
func TestBuildChatBodyOnlySendsToolChoiceWithTools(t *testing.T) {
	req := chatRequest("gpt-5.1")
	req.ToolChoice = json.RawMessage(`"auto"`)
	got := decodeBody(t, Config{}, req)
	if _, ok := got["tool_choice"]; ok {
		t.Fatal("tool_choice was sent with no tools")
	}

	req.Tools = []core.Tool{{Name: "t"}}
	req.ToolChoice = json.RawMessage(`"required"`)
	got = decodeBody(t, Config{}, req)
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v, want required", got["tool_choice"])
	}
}

// Images are expressible because Zen's CommonContentPart has an image_url
// variant; the caller's Detail is dropped because the neutral shape has none.
func TestBuildChatBodyImageParts(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model: "gemini-3-flash",
		Messages: []core.Message{{
			Role: "user",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what is this?"},
				{Type: "image_url", ImageURL: "https://example.test/a.png", Detail: "high"},
			},
		}},
	})
	msgs, _ := got["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want two parts", m["content"])
	}
	p0, _ := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "what is this?" {
		t.Fatalf("part 0 = %#v", p0)
	}
	p1, _ := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Fatalf("part 1 = %#v", p1)
	}
	img, _ := p1["image_url"].(map[string]any)
	if img["url"] != "https://example.test/a.png" {
		t.Fatalf("image_url = %#v", p1["image_url"])
	}
	if _, ok := img["detail"]; ok {
		t.Fatal("detail was sent: Zen's neutral shape has no such field")
	}
	if _, ok := p1["detail"]; ok {
		t.Fatal("detail was sent at the part level")
	}
}

// Parts win over Content: a caller that supplied both means the structured one.
func TestBuildChatBodyPartsWinOverContent(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model: "gpt-5.1",
		Messages: []core.Message{{
			Role:    "user",
			Content: "ignored",
			Parts:   []core.ContentPart{{Type: "text", Text: "kept"}},
		}},
	})
	msgs, _ := got["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	parts, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v, want the structured parts", m["content"])
	}
	p0, _ := parts[0].(map[string]any)
	if p0["text"] != "kept" {
		t.Fatalf("part 0 = %#v", p0)
	}
}

func TestBuildChatBodyRejectsUnsupportedParts(t *testing.T) {
	_, err := buildChatBody(Config{}, &core.ChatRequest{
		Model:    "gpt-5.1",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{{Type: "image_url"}}}},
	})
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), "image part without a url") {
		t.Fatalf("image without url = %v", err)
	}

	_, err = buildChatBody(Config{}, &core.ChatRequest{
		Model:    "gpt-5.1",
		Messages: []core.Message{{Role: "user", Parts: []core.ContentPart{{Type: "audio"}}}},
	})
	if !errors.Is(err, core.ErrUnsupported) || !strings.Contains(err.Error(), `unknown content part "audio"`) {
		t.Fatalf("unknown part = %v", err)
	}
}

// A replayed reasoning turn is accepted outbound even though Zen's converters
// may drop it: refusing it would break a caller that replays its own history.
func TestBuildChatBodyCarriesReasoningOutbound(t *testing.T) {
	got := decodeBody(t, Config{}, &core.ChatRequest{
		Model: "gpt-5.1",
		Messages: []core.Message{
			{Role: "assistant", Content: "42", Reasoning: "because"},
		},
	})
	msgs, _ := got["messages"].([]any)
	m, _ := msgs[0].(map[string]any)
	if m["reasoning_content"] != "because" {
		t.Fatalf("reasoning_content = %#v", m["reasoning_content"])
	}
}

// Zen's CommonRequest.tool_choice union has no "none", so a caller asking for no
// tools gets the field omitted instead of a value the converter would mangle.
func TestNormalizeToolChoice(t *testing.T) {
	for _, raw := range []string{"", "null", `"none"`, "  ", "\n\t"} {
		if got := normalizeToolChoice(json.RawMessage(raw)); got != nil {
			t.Fatalf("normalizeToolChoice(%q) = %q, want nil", raw, got)
		}
	}
	for _, raw := range []string{`"auto"`, `"required"`, `{"type":"function","function":{"name":"t"}}`} {
		if got := normalizeToolChoice(json.RawMessage(raw)); string(got) != raw {
			t.Fatalf("normalizeToolChoice(%q) = %q, want it passed through", raw, got)
		}
	}
}

func TestTrimJSON(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"   ":          "",
		"null":         "null",
		"  null  ":     "null",
		"\t\"auto\"\n": `"auto"`,
		"  {}  ":       "{}",
	}
	for in, want := range cases {
		if got := trimJSON(json.RawMessage(in)); got != want {
			t.Fatalf("trimJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsSpace(t *testing.T) {
	for _, b := range []byte{' ', '\t', '\n', '\r'} {
		if !isSpace(b) {
			t.Fatalf("isSpace(%q) = false", b)
		}
	}
	for _, b := range []byte{'a', '0', '{'} {
		if isSpace(b) {
			t.Fatalf("isSpace(%q) = true", b)
		}
	}
}
