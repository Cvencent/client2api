package workbuddy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Upstream SSE normalisation.  Ported from the reference implementation
// (internal/upstream/sse.go + truncation.go, MIT), with the two
// http.ResponseWriter-coupled writers (Stream/StreamHint) replaced by
// ReadSSEFrames, which hands decoded chunks to a callback instead of writing
// them to a client.  The chunk semantics — envelope unwrapping, usage aliasing,
// tool-call fragment merging, frame whitelisting, truncation dropping — are
// unchanged.

// errEmptyStream is the text the reference puts in the client-visible error
// frame when the upstream stream carried nothing usable.  It is also what
// IsEmptyStreamError matches on, so the two never drift.
var errEmptyStream = errors.New("empty upstream stream")

// IsEmptyStreamError reports whether err means "the upstream stream carried no
// usable data event at all".
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// ParseSSEChunk decodes one SSE data payload.  It accepts the raw OpenAI-ish
// chunk as well as the `{"code":0,"msg":"","data":{…}}` envelope the WorkBuddy
// gateway wraps chunks in.  ok is false for empty input, "[DONE]", and
// anything that is not a JSON object.
func ParseSSEChunk(payload string) (map[string]any, bool) {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil || obj == nil {
		return nil, false
	}
	if inner, ok := unwrapEnvelope(obj); ok {
		return inner, true
	}
	return obj, true
}

// unwrapEnvelope unwraps the gateway envelope when the object looks like one.
func unwrapEnvelope(obj map[string]any) (map[string]any, bool) {
	inner, ok := obj["data"].(map[string]any)
	if !ok || inner == nil {
		return nil, false
	}
	_, hasCode := obj["code"]
	_, hasMsg := obj["msg"]
	if !hasCode && !hasMsg {
		return nil, false
	}
	return inner, true
}

// maxFrameBytes bounds the payload of one SSE frame, and errFrameTooLarge is
// what ReadSSEFrames returns once the ceiling is passed.  WorkBuddy chunks are
// small OpenAI-shaped objects, so a megabyte is far above anything real; without
// a bound an upstream that keeps sending data: lines without ever closing the
// frame grows the builder until the process runs out of memory.
const maxFrameBytes = 1 << 20

var errFrameTooLarge = errors.New("workbuddy: upstream SSE frame exceeded the size limit")

// ReadSSEFrames reads `data:` frames from r and calls onFrame for each decoded
// chunk.  done=true is delivered once, for the `[DONE]` terminator; it is not
// delivered when the stream simply ends without one.  Comment lines, blank
// lines and other SSE fields are ignored.  A bare JSON object without the
// `data: ` prefix is also accepted, which is a known upstream quirk.
func ReadSSEFrames(r io.Reader, onFrame func(obj map[string]any, done bool) error) error {
	if r == nil {
		return errors.New("nil stream reader")
	}
	if onFrame == nil {
		return errors.New("nil frame callback")
	}
	br := bufio.NewReaderSize(r, 64*1024)
	var pending strings.Builder
	sawAny := false

	flush := func() error {
		if pending.Len() == 0 {
			return nil
		}
		payload := pending.String()
		pending.Reset()
		obj, ok := ParseSSEChunk(payload)
		if !ok {
			return nil
		}
		sawAny = true
		return onFrame(obj, false)
	}

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case trimmed == "":
			// Frame boundary.
			if e := flush(); e != nil {
				return e
			}
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				if e := flush(); e != nil {
					return e
				}
				sawAny = true
				return onFrame(nil, true)
			}
			if pending.Len() > 0 {
				pending.WriteByte('\n')
			}
			pending.WriteString(payload)
			if pending.Len() > maxFrameBytes {
				return errFrameTooLarge
			}
			// Single-line JSON is the common case; flush as soon as the buffer
			// is complete so a missing blank line cannot merge two chunks.
			if json.Valid([]byte(pending.String())) {
				if e := flush(); e != nil {
					return e
				}
			}
		case strings.HasPrefix(trimmed, "{"):
			// Bare JSON chunk without the `data: ` prefix.
			if e := flush(); e != nil {
				return e
			}
			if obj, ok := ParseSSEChunk(trimmed); ok {
				sawAny = true
				if e := onFrame(obj, false); e != nil {
					return e
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if e := flush(); e != nil {
					return e
				}
				if !sawAny {
					return errEmptyStream
				}
				return nil
			}
			return err
		}
	}
}

// Aggregate consumes a whole (non-streaming) upstream response body and
// reassembles it into one chat.completion object.  It is used by the live test
// helper and by any caller that wants a single answer out of a stream.
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)

	var (
		id            string
		model         string
		created       float64
		content       strings.Builder
		reasoning     strings.Builder
		role          = "assistant"
		finishReason  = "stop"
		usage         map[string]any
		gotAnyContent bool
		validEvents   int
		sawDone       bool
		toolCalls     = map[int]map[string]any{}
		toolOrder     []int
		toolSeq       int
		idIndex       = map[string]int{}
	)

	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, raw := range tcs {
			call, ok := raw.(map[string]any)
			if !ok || call == nil {
				continue
			}
			idx := -1
			if f, ok := call["index"].(float64); ok {
				idx = int(f)
			} else if cid, _ := call["id"].(string); cid != "" {
				if seen, ok := idIndex[cid]; ok {
					idx = seen
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1]
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
		}
	}
	mergeMessageFields := func(msg map[string]any) {
		if r, _ := msg["role"].(string); r != "" {
			role = r
		}
		if c, _ := msg["content"].(string); c != "" {
			appendContent(c)
		}
		if rc, _ := msg["reasoning_content"].(string); rc != "" {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}

	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			// ParseSSEChunk unwraps the `{"code":0,...,"data":{...}}` envelope
			// upstream puts around every chunk.
			if chunk, ok := ParseSSEChunk(payload); ok && chunk != nil {
				validEvents++
				if id == "" {
					id, _ = chunk["id"].(string)
				}
				if model == "" {
					model, _ = chunk["model"].(string)
				}
				if created == 0 {
					if f, ok := chunk["created"].(float64); ok {
						created = f
					}
				}
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				if choices, ok := chunk["choices"].([]any); ok {
					for _, c := range choices {
						ch, ok := c.(map[string]any)
						if !ok {
							continue
						}
						if fr, _ := ch["finish_reason"].(string); fr != "" {
							finishReason = fr
						}
						if d, ok := ch["delta"].(map[string]any); ok {
							if r, _ := d["role"].(string); r != "" {
								role = r
							}
							if c, _ := d["content"].(string); c != "" {
								appendContent(c)
							}
							if rc, _ := d["reasoning_content"].(string); rc != "" {
								reasoning.WriteString(rc)
							}
							if tcs, ok := d["tool_calls"].([]any); ok {
								mergeToolCallsChunk(tcs)
							}
						}
						if !gotAnyContent {
							if m, ok := ch["message"].(map[string]any); ok {
								mergeMessageFields(m)
							}
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}

	if validEvents == 0 {
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{"role": role, "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		out["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(usage))
	}
	return out, nil
}

// ensureUsageTotal fills total_tokens from prompt+completion when the upstream
// omitted it.
func ensureUsageTotal(u map[string]any) map[string]any {
	if u == nil {
		return u
	}
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := cloneUsageMap(u)
	out["total_tokens"] = pt + ct
	return out
}

func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta merges one streamed tool-call fragment into the
// accumulated call.
func mergeToolCallDelta(merged, delta map[string]any) {
	if merged == nil || delta == nil {
		return
	}
	if id, _ := delta["id"].(string); id != "" {
		merged["id"] = id
	}
	if t, _ := delta["type"].(string); t != "" {
		merged["type"] = t
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if name, _ := df["name"].(string); name != "" {
		mf["name"] = name
	}
	if args, _ := df["arguments"].(string); args != "" {
		prev, _ := mf["arguments"].(string)
		if prev != "" {
			mf["arguments"] = prev + args
		} else {
			mf["arguments"] = args
		}
	}
}

// stripToolCallNames drops the function name from every tool-call fragment
// after the first one for a given index: upstream repeats it on every fragment,
// and an OpenAI client only accepts it once.
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return
	}
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := ch["delta"].(map[string]any)
		if !ok {
			continue
		}
		tcs, ok := delta["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, raw := range tcs {
			tc, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			idx := 0
			if f, ok := tc["index"].(float64); ok {
				idx = int(f)
			}
			if seen[idx] {
				if fn, ok := tc["function"].(map[string]any); ok && fn != nil {
					delete(fn, "name")
				}
				continue
			}
			seen[idx] = true
		}
	}
}

// normalizeFrame rebuilds a chunk from an explicit field whitelist so unknown
// upstream fields never leak to the client.
func normalizeFrame(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if s, _ := out["object"].(string); s == "" {
		out["object"] = "chat.completion.chunk"
	}
	if id, _ := out["id"].(string); id == "" {
		out["id"] = "chatcmpl-workbuddy"
	}
	choices, _ := obj["choices"].([]any)
	nchoices := make([]any, 0, len(choices))
	for _, c := range choices {
		ch, ok := c.(map[string]any)
		if !ok {
			continue
		}
		nc := map[string]any{}
		if idx, ok := ch["index"]; ok {
			nc["index"] = idx
		}
		delta := map[string]any{}
		if d, ok := ch["delta"].(map[string]any); ok {
			for _, k := range []string{"role", "content", "reasoning_content", "refusal"} {
				if s, ok := d[k].(string); ok && s != "" {
					delta[k] = s
				}
			}
			if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
				delta["tool_calls"] = tcs
			}
			if fc, ok := d["function_call"].(map[string]any); ok {
				name, _ := fc["name"].(string)
				args, _ := fc["arguments"].(string)
				if name != "" || args != "" {
					delta["function_call"] = fc
				}
			} else if fc, ok := d["function_call"]; ok && fc != nil {
				delta["function_call"] = fc
			}
		}
		nc["delta"] = delta
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			nc["finish_reason"] = fr
		} else {
			nc["finish_reason"] = nil
		}
		nchoices = append(nchoices, nc)
	}
	out["choices"] = nchoices
	if u, ok := obj["usage"]; ok {
		if um, ok := u.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(um)
		} else {
			out["usage"] = u
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// --- truncation ------------------------------------------------------------

// isTruncatedArguments reports whether a tool-call argument string is
// incomplete JSON, i.e. the upstream cut the call off mid-argument.
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls removes calls whose arguments did not parse.  A
// truncated call is unusable: the client cannot execute it and a retry would
// duplicate the work.
func dropTruncatedToolCalls(calls []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, ok := call["function"].(map[string]any)
		if !ok || fn == nil {
			out = append(out, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if isTruncatedArguments(args) {
			continue
		}
		out = append(out, call)
	}
	return out
}

// --- usage aliases ---------------------------------------------------------

// normalizeUsageCacheAliases copies the best available cache-hit token count
// onto every alias a strict client might read.  Some responses carry the real
// hit in prompt_tokens_details.cached_tokens while emitting zero-valued
// compatibility aliases that strict parsers prefer.
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	if usage == nil {
		return usage
	}
	hit, ok := bestUsageCacheHitTokens(usage)
	if !ok || hit <= 0 {
		return usage
	}
	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = hit
	out["cached_tokens"] = hit
	out["prompt_cache_hit_tokens"] = hit
	details := cloneUsageDetails(out["prompt_tokens_details"])
	details["cached_tokens"] = hit
	out["prompt_tokens_details"] = details
	if in, ok := out["input_tokens_details"].(map[string]any); ok {
		ind := cloneUsageDetails(in)
		ind["cached_tokens"] = hit
		out["input_tokens_details"] = ind
	}
	return out
}

// bestUsageCacheHitTokens probes the known spellings, first positive wins.
func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	if pt, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if n, ok := positiveUsageNumber(pt["cached_tokens"]); ok {
			return n, true
		}
	}
	for _, key := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if n, ok := positiveUsageNumber(usage[key]); ok {
			return n, true
		}
	}
	if it, ok := usage["input_tokens_details"].(map[string]any); ok {
		if n, ok := positiveUsageNumber(it["cached_tokens"]); ok {
			return n, true
		}
	}
	return 0, false
}

func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		if n > 0 {
			return n, true
		}
	case float32:
		if n > 0 {
			return float64(n), true
		}
	case int:
		if n > 0 {
			return float64(n), true
		}
	case int64:
		if n > 0 {
			return float64(n), true
		}
	case int32:
		if n > 0 {
			return float64(n), true
		}
	case uint:
		if n > 0 {
			return float64(n), true
		}
	case uint64:
		if n > 0 {
			return float64(n), true
		}
	case uint32:
		if n > 0 {
			return float64(n), true
		}
	}
	return 0, false
}

func cloneUsageMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneUsageDetails(v any) map[string]any {
	out := map[string]any{}
	if in, ok := v.(map[string]any); ok {
		for k, val := range in {
			out[k] = val
		}
	}
	return out
}
