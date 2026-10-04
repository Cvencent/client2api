package trae

// payload.go — OpenAI request → SOLO llm_utils_chat body rewriting.
//
// Ported from the MIT reference client2api-lab/_upstream/trae2api-web
// (internal/upstream/payload.go).  The upstream Go structs differ from the
// OpenAI wire format in four ways that all cause a hard failure if ignored:
//
//  1. every message "content" must be an ARRAY of parts, never a bare string
//     (a string yields HTTP 400 with business code 4001);
//  2. the model must be written into BOTH "model" and "config_name";
//  3. tools[].function.parameters must be a JSON *string*, not an object;
//  4. "function" selects the upstream channel and is forced to solo_work_lite.
//
// The reference also normalises tool_choice and rewrites assistant tool_calls
// into the upstream "function_call" shape.

import (
	"encoding/json"
	"strings"

	"client2api/internal/core"
)

// payloadOptions carries the per-request rewrite parameters.
type payloadOptions struct {
	Function  string
	Model     string
	MaxTokens int
	Extra     map[string]any
}

// payloadOptions derives the rewrite parameters from the module config.
func (c *Config) payloadOptions() payloadOptions {
	return payloadOptions{
		Function:  c.functionName(),
		Model:     c.defaultModel(),
		MaxTokens: c.MaxTokens,
		Extra:     c.ExtraBody,
	}
}

// BuildBody renders a core.ChatRequest into the upstream request body.
func BuildBody(req *core.ChatRequest, opts payloadOptions) ([]byte, error) {
	obj := map[string]any{}

	msgs := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, buildMessage(m))
	}
	obj["messages"] = msgs

	if len(req.Tools) > 0 {
		obj["tools"] = buildTools(req.Tools)
	}
	if len(req.ToolChoice) > 0 {
		var tc any
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			obj["tool_choice"] = tc
		}
	}
	if req.Temperature != nil {
		obj["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		obj["top_p"] = *req.TopP
	}
	maxTokens := opts.MaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	if maxTokens > 0 {
		obj["max_tokens"] = maxTokens
	}
	if len(req.Stop) > 0 {
		obj["stop"] = req.Stop
	}
	if req.User != "" {
		obj["user"] = req.User
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = opts.Model
	}
	obj["model"] = model
	obj["stream"] = true

	PrepareBody(obj, opts)
	return json.Marshal(obj)
}

// buildMessage renders one core.Message as an OpenAI-shaped object.  Content is
// emitted as a string when the message has no structured parts; PrepareBody
// then converts it to the mandatory array form.
func buildMessage(m core.Message) map[string]any {
	msg := map[string]any{"role": m.Role}

	if len(m.Parts) > 0 {
		parts := make([]any, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case "image_url":
				img := map[string]any{"url": p.ImageURL}
				if p.Detail != "" {
					img["detail"] = p.Detail
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
			default:
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			}
		}
		msg["content"] = parts
	} else {
		msg["content"] = m.Content
	}

	if m.Name != "" {
		msg["name"] = m.Name
	}
	if m.ToolCallID != "" {
		msg["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		calls := make([]any, 0, len(m.ToolCalls))
		for _, tc := range m.ToolCalls {
			call := map[string]any{"type": firstNonEmpty(tc.Type, "function")}
			if tc.ID != "" {
				call["id"] = tc.ID
			}
			call["function"] = map[string]any{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			}
			calls = append(calls, call)
		}
		msg["tool_calls"] = calls
	}
	return msg
}

// buildTools renders core.Tool values in the OpenAI shape; PrepareBody turns
// the parameters object into the JSON string the upstream expects.
func buildTools(tools []core.Tool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			var params any
			if err := json.Unmarshal(t.Parameters, &params); err == nil && params != nil {
				fn["parameters"] = params
			}
		}
		out = append(out, map[string]any{
			"type":     firstNonEmpty(t.Type, "function"),
			"function": fn,
		})
	}
	return out
}

// PrepareBody applies the upstream rewriting rules in place and returns obj.
func PrepareBody(obj map[string]any, opts payloadOptions) map[string]any {
	if obj == nil {
		return obj
	}
	function := opts.Function
	if function == "" {
		function = defaultFunction
	}
	// Upstream always streams; a buffered mode does not exist server-side.
	obj["stream"] = true
	obj["function"] = function

	normalizeMessages(obj)
	normalizeTools(obj)
	normalizeToolChoice(obj)

	model, _ := obj["model"].(string)
	if model = strings.TrimSpace(model); model == "" {
		model = firstNonEmpty(opts.Model, defaultModel)
	}
	// The model goes into both fields: config_name selects the configuration,
	// model is what the upstream echoes back.
	obj["config_name"] = model
	obj["model"] = model

	for k, v := range opts.Extra {
		if _, exists := obj[k]; !exists {
			obj[k] = v
		}
	}
	return obj
}

// PrepareBodyBytes is the byte-slice convenience wrapper (used by tests and by
// anything holding an already-serialised OpenAI body).
func PrepareBodyBytes(src []byte, opts payloadOptions) ([]byte, error) {
	if len(src) == 0 {
		return src, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, err
	}
	PrepareBody(obj, opts)
	return json.Marshal(obj)
}

// normalizeMessages enforces the array-content rule and rewrites assistant
// tool_calls into the upstream function_call shape.
func normalizeMessages(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, item := range msgs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "assistant" {
			normalizeAssistantToolCalls(m)
		}
		content, present := m["content"]
		if !present || content == nil {
			continue
		}
		if s, isString := content.(string); isString {
			m["content"] = []any{map[string]any{"type": "text", "text": s}}
		}
		// An existing array (text / image_url parts) passes through untouched.
	}
}

// normalizeAssistantToolCalls converts function → function_call and drops calls
// with no function name, which the upstream rejects.
func normalizeAssistantToolCalls(m map[string]any) {
	calls, ok := m["tool_calls"].([]any)
	if !ok {
		return
	}
	kept := make([]any, 0, len(calls))
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			call["function_call"] = fn
			delete(call, "function")
		}
		if fc, ok := call["function_call"].(map[string]any); ok {
			if name, _ := fc["name"].(string); strings.TrimSpace(name) == "" {
				continue
			}
		}
		kept = append(kept, call)
	}
	if len(kept) == 0 {
		delete(m, "tool_calls")
		return
	}
	m["tool_calls"] = kept
}

// normalizeToolChoice rewrites the OpenAI tool_choice into the scalar form the
// upstream Go struct expects.
//
//	"none" / {"type":"none"}                     → drop tool_choice + tools
//	{"type":"auto"|"required"}                   → "auto" / "required"
//	{"type":"function","function":{"name":"x"}}  → "x"
//	anything else                                → drop tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	choice, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := choice.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools serialises each tool's parameters object into a JSON string
// and drops malformed entries.
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, tool)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
