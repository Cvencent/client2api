package kimi

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"client2api/internal/core"
)

// maxRecordBytes bounds one NDJSON line.  A record larger than this is
// diagnostics, not content; dropping it keeps memory bounded.
const maxRecordBytes = 8 << 20

// ---------------------------------------------------------------------------
// NDJSON records
//
// The CLI's stream-json format is NOT documented anywhere we could reach (the
// CLI is not installed on the machine this module was written for, and the
// vendor publishes no flag or output reference).  What is known is what the MIT
// reference relied on:
//
//	{"role":"assistant","content":"..."}
//
// The struct below therefore decodes that shape and the obvious variants a
// streaming CLI tends to use, and nothing more.  Every field is optional; a
// record that matches nothing is simply ignored.  This is the one place where
// guessing is unavoidable, so it is isolated here and called out in README.md
// under "Known gaps".
// ---------------------------------------------------------------------------

type record struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Text    string          `json:"text"`
	Delta   json.RawMessage `json:"delta"`

	// Streaming variants nest the payload under "message".
	Message *record `json:"message"`

	Reasoning        string `json:"reasoning"`
	ReasoningContent string `json:"reasoning_content"`

	Usage *usageRecord `json:"usage"`

	ToolCalls []toolCallRecord `json:"tool_calls"`

	Error   json.RawMessage `json:"error"`
	IsError bool            `json:"is_error"`
}

type usageRecord struct {
	PromptTokens         *int `json:"prompt_tokens"`
	InputTokens          *int `json:"input_tokens"`
	CompletionTokens     *int `json:"completion_tokens"`
	OutputTokens         *int `json:"output_tokens"`
	TotalTokens          *int `json:"total_tokens"`
	ReasoningTokens      *int `json:"reasoning_tokens"`
	CachedTokens         *int `json:"cached_tokens"`
	CacheReadInputTokens *int `json:"cache_read_input_tokens"`
}

type toolCallRecord struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Index    *int            `json:"index"`
	Function *functionRecord `json:"function"`
	// Flat variants.
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
}

type functionRecord struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// textNonCarriers are record types that never carry assistant prose.  Skipping
// them by name keeps the CLI's own progress/diagnostic records out of the
// completion.
var textNonCarriers = map[string]bool{
	"tool": true, "tool_call": true, "tool_use": true, "tool_result": true,
	"result": true, "error": true, "system": true, "init": true, "start": true,
	"usage": true, "summary": true, "compact": true, "log": true, "debug": true,
	"info": true, "warning": true, "metric": true, "metrics": true,
}

// handleRecord dispatches one decoded record.
func (s *stream) handleRecord(rec *record) {
	if rec == nil {
		return
	}

	// 1. Errors first: an error record ends the turn regardless of anything
	//    else it carries.
	if msg := decodeError(rec); msg != "" {
		s.emit(core.Event{Type: core.EventError, Err: fmt.Errorf("kimi: %s", msg)})
	}

	// 2. Usage.
	if u := decodeUsage(rec.Usage); u != nil && !s.usageSent {
		s.usage = u
	}

	// 3. Native tool calls, decoded structurally from their own field.
	if len(rec.ToolCalls) > 0 {
		s.emitToolCalls(rec.ToolCalls)
	}

	// 4. Reasoning text.
	if r := firstNonEmpty(rec.Reasoning, rec.ReasoningContent); r != "" {
		s.emit(core.Event{Type: core.EventDelta, Reasoning: r})
	}

	// 5. Assistant prose.
	if text := s.recordText(rec); text != "" {
		s.emitText(text)
	}
}

// recordText extracts the assistant prose from a record, or "" when the record
// carries none.
func (s *stream) recordText(rec *record) string {
	if rec.Message != nil {
		// A wrapper record: recurse, but only into the nested payload.
		if inner := s.recordText(rec.Message); inner != "" {
			return inner
		}
	}

	typ := strings.ToLower(strings.TrimSpace(rec.Type))
	if textNonCarriers[typ] {
		return ""
	}
	if rec.IsError {
		return ""
	}

	role := strings.ToLower(strings.TrimSpace(rec.Role))
	if role != "" && role != "assistant" {
		// "user" records echo our own prompt back; "system"/"tool" are not
		// prose either.
		return ""
	}

	// The reference's contract: role == "assistant" with a string content.
	if role == "assistant" {
		if text, ok := jsonString(rec.Content); ok {
			return text
		}
	}
	// Streaming variants: a bare string content, a delta payload, or text.
	if text, ok := jsonString(rec.Content); ok {
		return text
	}
	if text := deltaText(rec.Delta); text != "" {
		return text
	}
	if role == "" && rec.Text != "" && !textNonCarriers[typ] {
		return rec.Text
	}
	if role == "assistant" {
		return rec.Text
	}
	return ""
}

// jsonString decodes a JSON value that is a string, tolerating null.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// deltaText extracts text from a "delta" value, which is either a string or an
// object with a text/content field.
func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if s, ok := jsonString(raw); ok {
		return s
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return firstNonEmpty(obj.Text, obj.Content)
}

// decodeError renders an error field, which may be a string or an object with a
// message.  It returns "" when the record carries no error.
func decodeError(rec *record) string {
	if len(rec.Error) > 0 && !isJSONNull(rec.Error) {
		if s, ok := jsonString(rec.Error); ok {
			return redactSecrets(s)
		}
		var obj struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(rec.Error, &obj); err == nil && obj.Message != "" {
			return redactSecrets(obj.Message)
		}
		return redactSecrets(strings.TrimSpace(string(rec.Error)))
	}
	if rec.IsError {
		if s, ok := jsonString(rec.Content); ok && s != "" {
			return redactSecrets(s)
		}
		return "the CLI reported an error"
	}
	return ""
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// decodeUsage converts a usage record, or returns nil when the CLI did not
// report one.  The reference emitted a hard-coded {0,0,0}; emitting nothing is
// honest, a fake zero is not.
func decodeUsage(u *usageRecord) *core.Usage {
	if u == nil {
		return nil
	}
	out := core.Usage{}
	any := false
	if v := firstInt(u.PromptTokens, u.InputTokens); v != nil {
		out.PromptTokens = *v
		any = true
	}
	if v := firstInt(u.CompletionTokens, u.OutputTokens); v != nil {
		out.CompletionTokens = *v
		any = true
	}
	if v := firstInt(u.TotalTokens); v != nil {
		out.TotalTokens = *v
		any = true
	} else if any {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if v := firstInt(u.ReasoningTokens); v != nil {
		out.ReasoningTokens = *v
	}
	if v := firstInt(u.CachedTokens, u.CacheReadInputTokens); v != nil {
		out.CachedTokens = *v
	}
	if !any {
		return nil
	}
	return &out
}

func firstInt(vals ...*int) *int {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Tool calls
// ---------------------------------------------------------------------------

// emitToolCalls renders structurally-decoded tool calls.  The CLI cannot stream
// partial tool arguments (it emits whole records), so each call is emitted as a
// single complete fragment, indexed by its position.
func (s *stream) emitToolCalls(calls []toolCallRecord) {
	for i, tc := range calls {
		index := i
		if tc.Index != nil && *tc.Index >= 0 {
			index = *tc.Index
		}
		name, args := toolCallParts(tc)
		if name == "" && args == "" {
			continue
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = "call_" + randHex(12)
		}
		s.calls++
		s.output = true
		s.emit(core.Event{
			Type: core.EventToolCall,
			ToolCall: &core.ToolCallDelta{
				Index:     index,
				ID:        id,
				Name:      name,
				Arguments: args,
			},
		})
	}
}

func toolCallParts(tc toolCallRecord) (name, args string) {
	if tc.Function != nil {
		name = strings.TrimSpace(tc.Function.Name)
		args = rawToArguments(tc.Function.Arguments)
	}
	if name == "" {
		name = strings.TrimSpace(tc.Name)
	}
	if args == "" {
		args = rawToArguments(tc.Arguments)
	}
	if args == "" {
		args = rawToArguments(tc.Input)
	}
	if args == "" {
		args = "{}"
	}
	return name, args
}

// rawToArguments normalises an arguments value to the JSON-encoded string the
// OpenAI wire format uses.  It accepts both an already-encoded string (what the
// prompt asks for) and a bare object (what a native tool call would carry).
func rawToArguments(raw json.RawMessage) string {
	if len(raw) == 0 || isJSONNull(raw) {
		return ""
	}
	if s, ok := jsonString(raw); ok {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// toolEnvelope is the shape the prompt asks the model to emit when it wants to
// call tools (see toolInstructions).
type toolEnvelope struct {
	ToolCalls []toolCallRecord `json:"tool_calls"`
}

// parseToolEnvelope recognises an assistant turn that is really a tool call.
//
// This is the structural replacement for the reference's defect: it regexed the
// entire output for a top-level {"tool_calls":[...]} and re-parsed it, which
// both mangled legitimate answers that happened to contain that text and missed
// calls embedded in anything else.  Here the whole buffered assistant text must
// decode as exactly one JSON object carrying a non-empty tool_calls array; a
// bare object without calls, or any surrounding prose, is not an envelope and
// is emitted as ordinary content.
func parseToolEnvelope(text string) ([]toolCallRecord, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()

	var env toolEnvelope
	if err := dec.Decode(&env); err != nil {
		return nil, false
	}
	// Reject trailing garbage: the object must be the entire message.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, false
	}
	if len(env.ToolCalls) == 0 {
		return nil, false
	}
	for _, tc := range env.ToolCalls {
		name, _ := toolCallParts(tc)
		if name == "" {
			return nil, false
		}
	}
	return env.ToolCalls, true
}

// emitEnvelopeCalls emits a parsed prompt-injected tool call.  Arguments are
// already complete, so each call goes out as one fragment.
func (s *stream) emitEnvelopeCalls(calls []toolCallRecord) {
	s.emitToolCalls(calls)
}
