package workbuddy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"client2api/internal/core"
)

// Outbound body construction.  Ported from the reference implementation
// (internal/upstream/payload.go, cache_key.go, tool_pairing.go and the
// reasoning parts of thinking.go; all MIT).  The pipeline is applied in the
// same order as upstream, because each step depends on the previous one:
//
//	1. stream is forced to true (the upstream endpoint rejects non-streaming)
//	2. max_completion_tokens is translated to max_tokens
//	3. stream_options.include_usage is requested
//	4. tool_choice is flattened to the string form the upstream accepts
//	5. developer messages are relabelled system
//	6. string image_url parts are converted to the object form
//	7. broken tool_call / tool-result pairing is repaired
//	8. thinking / reasoning_effort is injected and normalised
//	9. reasoning_content is backfilled for multi-turn consistency
//	10. content-moderation fingerprint sanitisation (sanitize.go)
//
// Step 10 is a byte-exact port of the reference's sanitize.go, rewrite table
// included.  The upstream blocks requests by exact string match on the client's
// own identity sentences, so the fix is to alter those sentences by the smallest
// possible edit (one word, one hyphen) rather than to delete them: deleting a
// sentence changes what the model is told, while altering it does not.

// buildWireBody converts a core.ChatRequest into the OpenAI-shaped JSON the
// upstream expects.
func buildWireBody(req *core.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, errors.New("nil chat request")
	}
	obj := map[string]any{}

	// Caller-supplied extras first, so the typed fields below win.
	for k, v := range req.Options {
		if k == "" {
			continue
		}
		obj[k] = v
	}

	obj["model"] = req.Model
	obj["messages"] = wireMessages(req.Messages)
	if len(req.Tools) > 0 {
		obj["tools"] = wireTools(req.Tools)
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
	if req.MaxTokens != nil {
		obj["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		obj["stop"] = req.Stop
	}
	if req.User != "" {
		obj["user"] = req.User
	}
	return json.Marshal(obj)
}

func wireMessages(msgs []core.Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg := map[string]any{"role": m.Role}
		if len(m.Parts) > 0 {
			parts := make([]any, 0, len(m.Parts))
			for _, p := range m.Parts {
				switch strings.ToLower(strings.TrimSpace(p.Type)) {
				case "image_url":
					if p.ImageURL == "" {
						continue
					}
					iu := map[string]any{"url": p.ImageURL}
					if p.Detail != "" {
						iu["detail"] = p.Detail
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": iu})
				default:
					parts = append(parts, map[string]any{"type": "text", "text": p.Text})
				}
			}
			msg["content"] = parts
		} else {
			msg["content"] = m.Content
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]any, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				t := tc.Type
				if t == "" {
					t = "function"
				}
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": t,
					"function": map[string]any{
						"name":      tc.Name,
						"arguments": tc.Arguments,
					},
				})
			}
			msg["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			msg["tool_call_id"] = m.ToolCallID
		}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		if m.Reasoning != "" {
			msg["reasoning_content"] = m.Reasoning
		}
		out = append(out, msg)
	}
	return out
}

func wireTools(tools []core.Tool) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			var params any
			if err := json.Unmarshal(t.Parameters, &params); err == nil {
				fn["parameters"] = params
			}
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": typ, "function": fn})
	}
	return out
}

// prepareBody runs the full outbound rewrite pipeline.
func prepareBody(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil || obj == nil {
		return src
	}

	// 1. Upstream rejects non-streaming chat requests.
	obj["stream"] = true

	// 2. max_completion_tokens -> max_tokens
	translateMaxCompletionTokens(obj)

	// 3. usage in the final frame
	if _, ok := obj["stream_options"]; !ok {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4. tool_choice must be a string upstream
	normalizeToolChoice(obj)

	// 5. developer -> system
	normalizeRoles(obj)

	// 6. image_url string -> object
	normalizeImageURL(obj)

	// 7. repair tool_call / tool-result pairing
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		obj["messages"] = msgs
	}

	// 8. thinking / reasoning_effort
	modelName := strField(obj, "model")
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)

	// 9. multi-turn reasoning consistency
	backfillReasoningContent(obj)

	// 10. sanitise content-moderation fingerprints.  It runs last, exactly where
	// the reference runs it (payload.go:75-79), and only rewrites message text:
	// the request shape has already been settled by steps 1-9.
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens deletes max_completion_tokens and, when
// max_tokens is absent, translates a positive integral value into it.
func translateMaxCompletionTokens(obj map[string]any) {
	v, present := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !present {
		return
	}
	if _, ok := obj["max_tokens"]; ok {
		return
	}
	if n, ok := integralValue(v); ok {
		obj["max_tokens"] = n
	}
}

func integralValue(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n > 0 && n == float64(int64(n)) {
			return int64(n), true
		}
	case int64:
		if n > 0 {
			return n, true
		}
	case int:
		if n > 0 {
			return int64(n), true
		}
	}
	return 0, false
}

// normalizeToolChoice flattens tool_choice to the bare string form.  The
// upstream endpoint only accepts a string; an object produces HTTP 400 with
// business code 11101.
func normalizeToolChoice(obj map[string]any) {
	v, ok := obj["tool_choice"]
	if !ok {
		return
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if strings.EqualFold(s, "none") {
			delete(obj, "tool_choice")
			delete(obj, "tools")
			delete(obj, "functions")
			return
		}
		obj["tool_choice"] = s
		return
	}
	m, ok := v.(map[string]any)
	if !ok {
		delete(obj, "tool_choice")
		return
	}
	typ, _ := m["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "none":
		delete(obj, "tool_choice")
		delete(obj, "tools")
		delete(obj, "functions")
	case "auto", "required":
		obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
	case "function":
		if fn, ok := m["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); strings.TrimSpace(name) != "" {
				obj["tool_choice"] = strings.TrimSpace(name)
				return
			}
		}
		if name, _ := m["name"].(string); strings.TrimSpace(name) != "" {
			obj["tool_choice"] = strings.TrimSpace(name)
			return
		}
		obj["tool_choice"] = "auto"
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeRoles relabels "developer" messages as "system".
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); strings.EqualFold(strings.TrimSpace(role), "developer") {
			m["role"] = "system"
		}
	}
}

// normalizeImageURL converts a string image_url to the object form.
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, praw := range parts {
			part, ok := praw.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["type"].(string); t != "image_url" {
				continue
			}
			if s, ok := part["image_url"].(string); ok && strings.TrimSpace(s) != "" {
				part["image_url"] = map[string]any{"url": s}
			}
		}
	}
}

// --- tool-call / tool-result pairing ---------------------------------------

// repackToolResultBlocks moves tool results back next to their assistant
// message when the client interleaved other messages between them.  The
// upstream rejects such a sequence with HTTP 400.
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	for i := 0; i < len(messages); {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			out = append(out, messages[i])
			i++
			continue
		}
		role, _ := msg["role"].(string)
		tcs, _ := msg["tool_calls"].([]any)
		if role != "assistant" || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, traw := range tcs {
			tc, ok := traw.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); id != "" {
				want[id] = true
			}
		}
		out = append(out, msg)
		i++
		var results []any
		var between []any
		for i < len(messages) {
			next, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			nrole, _ := next["role"].(string)
			if nrole == "tool" {
				id, _ := next["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				i++
				continue
			}
			if len(results) == 0 {
				break
			}
			ntcs, _ := next["tool_calls"].([]any)
			if nrole == "assistant" && len(ntcs) > 0 {
				break
			}
			between = append(between, messages[i])
			changed = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls removes tool_calls with no matching result and tool
// results with no matching call.  Either half alone makes the upstream reject
// every subsequent message in the conversation.
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasTraffic = true
			for _, traw := range tcs {
				tc, ok := traw.(map[string]any)
				if !ok {
					continue
				}
				if id, _ := tc["id"].(string); id != "" {
					callIDs[id] = true
				}
			}
		}
		if role == "tool" {
			hasTraffic = true
			if id, _ := m["tool_call_id"].(string); id != "" {
				resultIDs[id] = true
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}
	changed := false
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := m["role"].(string)
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			kept := make([]any, 0, len(tcs))
			for _, traw := range tcs {
				tc, ok := traw.(map[string]any)
				if !ok {
					continue
				}
				id, _ := tc["id"].(string)
				if keep[id] {
					kept = append(kept, tc)
				}
			}
			if len(kept) != len(tcs) {
				changed = true
				if len(kept) == 0 {
					delete(m, "tool_calls")
				} else {
					m["tool_calls"] = kept
				}
			}
		}
		if role == "tool" {
			id, _ := m["tool_call_id"].(string)
			if !keep[id] {
				changed = true
				continue
			}
		}
		out = append(out, m)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// --- reasoning / thinking --------------------------------------------------

// defaultDeepSeekEffort is the effort used when the caller asked for thinking
// but did not name a level.
const defaultDeepSeekEffort = "high"

// effortRank orders the known reasoning levels.
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// lookupDefaultEffort returns the per-model default effort from the cached
// catalogue, or "".
func lookupDefaultEffort(defaultEfforts map[string]string, model string) string {
	if len(defaultEfforts) == 0 || model == "" {
		return ""
	}
	return defaultEfforts[model]
}

func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// injectThinking enables thinking for DeepSeek-family models.  An explicit
// "disabled" clears the reasoning effort; anything else enables thinking and
// ensures an effort level is present.
func injectThinking(obj map[string]any, defaultEffort string) {
	if obj == nil {
		return
	}
	model := strField(obj, "model")
	if !isDeepSeekModel(model) {
		return
	}
	if th, ok := obj["thinking"].(map[string]any); ok && th != nil {
		if t, _ := th["type"].(string); strings.TrimSpace(t) != "" {
			if strings.EqualFold(strings.TrimSpace(t), "disabled") {
				delete(obj, "reasoning_effort")
				delete(obj, "reasoningEffort")
				return
			}
			ensureDeepSeekEffort(obj, defaultEffort)
			return
		}
		th["type"] = "enabled"
		ensureDeepSeekEffort(obj, defaultEffort)
		return
	}
	obj["thinking"] = map[string]any{"type": "enabled"}
	ensureDeepSeekEffort(obj, defaultEffort)
}

func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	if _, ok := obj["reasoning_effort"]; ok {
		return
	}
	if _, ok := obj["reasoningEffort"]; ok {
		return
	}
	if strings.TrimSpace(defaultEffort) == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}

// normalizeReasoningEffort snaps the requested level onto one the model
// supports: exact match, else the highest supported level below it, else the
// lowest supported level.
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if obj == nil || len(efforts) == 0 {
		return
	}
	key := "reasoning_effort"
	raw, ok := obj[key]
	if !ok {
		key = "reasoningEffort"
		raw, ok = obj[key]
		if !ok {
			return
		}
	}
	requested, _ := raw.(string)
	requested = strings.ToLower(strings.TrimSpace(requested))
	if requested == "" {
		return
	}
	supported := efforts[strField(obj, "model")]
	if len(supported) == 0 {
		return
	}
	lower := make([]string, 0, len(supported))
	for _, s := range supported {
		lower = append(lower, strings.ToLower(strings.TrimSpace(s)))
	}
	for _, s := range lower {
		if s == requested {
			if key != "reasoning_effort" {
				obj["reasoning_effort"] = requested
				delete(obj, "reasoningEffort")
			}
			return
		}
	}
	wantRank, known := effortRank[requested]
	if !known {
		return
	}
	best := ""
	bestRank := -1
	lowest := ""
	lowestRank := 1 << 30
	for _, s := range lower {
		r, ok := effortRank[s]
		if !ok {
			continue
		}
		if r <= wantRank && r > bestRank {
			best, bestRank = s, r
		}
		if r < lowestRank {
			lowest, lowestRank = s, r
		}
	}
	chosen := best
	if chosen == "" {
		chosen = lowest
	}
	if chosen == "" {
		return
	}
	obj["reasoning_effort"] = chosen
	delete(obj, "reasoningEffort")
}

// backfillReasoningContent keeps DeepSeek multi-turn conversations consistent:
// once a reasoning trace is present, every assistant message must carry a
// reasoning_content field (possibly empty), or the upstream rejects the turn.
func backfillReasoningContent(obj map[string]any) {
	if obj == nil {
		return
	}
	if !isDeepSeekModel(strField(obj, "model")) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok && th != nil {
		if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "enabled") {
			thinkingEnabled = true
		}
	}
	hasTrace := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["reasoning"].(string); ok && s != "" {
			hasTrace = true
			break
		}
		if _, ok := m["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "assistant" {
			continue
		}
		rc, _ := m["reasoning_content"].(string)
		if rc == "" {
			rc, _ = m["reasoning"].(string)
		}
		m["reasoning_content"] = rc
		if existing, _ := m["reasoning"].(string); existing == "" {
			if rc != "" {
				m["reasoning"] = rc
			} else {
				m["reasoning"] = " "
			}
		}
	}
}

// --- console system prompt -------------------------------------------------

// ensureConsoleSystem prepends a system message when the conversation has none.
// The international ("console") realm rejects a conversation that starts with a
// user turn.
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok {
			if role, _ := first["role"].(string); role == "system" {
				return body
			}
		}
	}
	sys := map[string]any{"role": "system", "content": "You are a helpful assistant."}
	obj["messages"] = append([]any{sys}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// --- prompt cache ----------------------------------------------------------

// InjectPromptCacheKey sets prompt_cache_key so repeated prefixes hit the
// upstream prompt cache.  Measured effect in the reference project: without the
// key a repeated 8k-token prefix costs ~0.34 credit; with it the same prefix
// reports ~7808 cached tokens and costs ~0.02.
//
// The key is salted with the account uid: a shared key would let one account
// read another account's cached prefix, which is a data leak.
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return body
	}
	if existing, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(existing) != "" {
		return body
	}
	conv := strField(obj, "conversation_id")
	if conv == "" {
		conv = strField(obj, "conversationId")
	}
	if conv == "" {
		conv = strings.TrimSpace(conversationID)
	}
	obj["prompt_cache_key"] = buildCacheKey(uid, conv)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func buildCacheKey(uid, conversation string) string {
	uid8 := "-"
	if len(uid) >= 8 {
		uid8 = uid[:8]
	} else if uid != "" {
		uid8 = uid
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	return "wb2a-" + uid8 + "-" + hex.EncodeToString(sum[:16])
}

// strField returns a trimmed string field.
func strField(obj map[string]any, key string) string {
	s, _ := obj[key].(string)
	return strings.TrimSpace(s)
}
