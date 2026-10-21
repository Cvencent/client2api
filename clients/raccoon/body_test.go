package raccoon

import (
	"encoding/json"
	"testing"

	"client2api/internal/core"
)

func TestBuildChatBodyStripsNullEscapeFromToolPatterns(t *testing.T) {
	req := &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools: []core.Tool{{
			Type:       "function",
			Name:       "capture_process_scenario",
			Parameters: json.RawMessage(`{"type":"object","properties":{"environment":{"type":"object","propertyNames":{"type":"string","pattern":"^[^=\\0]+$"}}}}`),
		}},
	}

	raw, err := json.Marshal(buildChatBody(req, "sn-deepseek-v4-1-flash-1017", true))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var top struct {
		Tools []struct {
			Function struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(top.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(top.Tools))
	}
	properties := top.Tools[0].Function.Parameters["properties"].(map[string]any)
	environment := properties["environment"].(map[string]any)
	propertyNames := environment["propertyNames"].(map[string]any)
	if got := propertyNames["pattern"]; got != "^[^=]+$" {
		t.Fatalf("pattern = %q, want null escape removed", got)
	}
}

// SenseNova's validator rejects a function whose `parameters` is absent or an
// empty object ("is not valid under any of the schemas listed in the 'anyOf'
// keyword"). Codex's dynamic/MCP tools can legitimately carry no schema, so the
// adapter must substitute the canonical empty-object schema instead of
// forwarding nothing and turning the whole request into a 400.
func TestBuildChatBodyFillsAMissingToolSchema(t *testing.T) {
	for _, tc := range []struct {
		name  string
		param json.RawMessage
	}{
		{name: "absent"},
		{name: "empty object", param: json.RawMessage(`{}`)},
		{name: "null", param: json.RawMessage(`null`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &core.ChatRequest{
				Messages: []core.Message{{Role: "user", Content: "hi"}},
				Tools:    []core.Tool{{Type: "function", Name: "schema_less_tool", Parameters: tc.param}},
			}
			raw, err := json.Marshal(buildChatBody(req, "sn-deepseek-v4-1-flash-1017", true))
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			var top struct {
				Tools []struct {
					Function struct {
						Parameters map[string]any `json:"parameters"`
					} `json:"function"`
				} `json:"tools"`
			}
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatalf("unmarshal body: %v", err)
			}
			if len(top.Tools) != 1 {
				t.Fatalf("tools = %d, want 1", len(top.Tools))
			}
			params := top.Tools[0].Function.Parameters
			if params == nil {
				t.Fatal("parameters must be present so the upstream validator accepts the tool")
			}
			if params["type"] != "object" {
				t.Fatalf("parameters = %#v, want the canonical empty-object schema", params)
			}
		})
	}
}
