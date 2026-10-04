package trae

// sse.go — the SOLO SSE frame parser and the core.Stream implementation.
//
// The upstream always streams (even for stream:false) and it reports business
// failures *inside* an HTTP 200 body:
//
//	id:1
//	event:metadata
//	data:{"model":"","session_id":"...","prompt_completion_id":0}
//
//	id:2
//	event:timing_cost
//	data:{"name":"llm_raw_chat_v2"}
//
//	event:output                       ← repeated, the actual content
//	data:{"response":"<delta>","reasoning_content":"<think delta>","tool_calls":null}
//
//	event:token_usage
//	data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
//
//	event:done
//	data:{"finish_reason":"stop"}
//
//	event:error
//	data:{"code":1005,"message":"..."}
//
// Ported from the MIT reference client2api-lab/_upstream/trae2api-web
// (internal/upstream/solosse.go).

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	"client2api/internal/core"
)

// soloEvent is one normalised upstream SSE frame.
type soloEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	Notify       map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMessage string
}

// parseSOLOEvent decodes one event/data pair.  A malformed data line yields an
// error so the caller can decide to skip it.
func parseSOLOEvent(eventName, data string) (*soloEvent, error) {
	ev := &soloEvent{Event: strings.TrimSpace(eventName)}
	if strings.TrimSpace(data) == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "notify_usage":
		ev.Notify = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		ev.ErrorCode = numFromAny(raw["code"])
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
		if ev.ErrorMessage == "" {
			if v, ok := raw["msg"].(string); ok {
				ev.ErrorMessage = v
			}
		}
	}
	return ev, nil
}

// sseScanner turns an SSE byte stream into soloEvent values.  Event and data
// lines accumulate until a blank line closes the frame.
type sseScanner struct {
	br    *bufio.Reader
	event string
	data  strings.Builder
	// err latches a framing failure so feed, which cannot return one, can still
	// stop the scan.  Next reports it once and then stays failed.
	err error
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next parsed frame, or io.EOF at the end of the stream.
func (s *sseScanner) Next() (*soloEvent, error) {
	for {
		if s.err != nil {
			// Sticky: the scanner is poisoned, so every later call repeats the
			// verdict instead of falling through to another read.  A consumer
			// that keeps calling Next after an error therefore cannot resume a
			// stream we already declared untrustworthy.
			return nil, s.err
		}
		line, err := s.br.ReadString('\n')
		if line != "" {
			if ev := s.feed(strings.TrimRight(line, "\r\n")); ev != nil {
				return ev, nil
			}
		}
		if err != nil {
			if err == io.EOF {
				if ev := s.flush(); ev != nil {
					return ev, nil
				}
				return nil, io.EOF
			}
			return nil, err
		}
	}
}

// maxFrameBytes bounds the payload of one SSE frame.  Trae's deltas are short
// text or small JSON objects, so a megabyte is far above anything legitimate;
// without a ceiling a broken or hostile upstream could stream a single frame
// with no blank line forever and grow the builder until the process died.
const maxFrameBytes = 1 << 20

// errFrameTooLarge is returned when one frame exceeds maxFrameBytes.
var errFrameTooLarge = errors.New("trae: upstream SSE frame exceeded the size limit")

// feed consumes one line, returning a frame when the line closes one.
func (s *sseScanner) feed(line string) *soloEvent {
	switch {
	case line == "":
		return s.flush()
	case strings.HasPrefix(line, "event:"):
		s.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		s.data.WriteString(strings.TrimPrefix(line, "data:"))
		if s.data.Len() > maxFrameBytes && s.err == nil {
			// Latch instead of returning: feed's signature predates this check
			// and every caller treats a nil return as "no frame yet", so the
			// error has to surface from the next Next call.
			s.err = errFrameTooLarge
		}
	case strings.HasPrefix(line, ":"):
		// SSE comment.
	case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
		// Frame bookkeeping we do not need.
	}
	return nil
}

// flush emits the pending frame, if any.
func (s *sseScanner) flush() *soloEvent {
	event, data := s.event, s.data.String()
	s.event = ""
	s.data.Reset()
	if event == "" {
		return nil
	}
	ev, err := parseSOLOEvent(event, data)
	if err != nil {
		return nil
	}
	return ev
}

// ---- core.Stream ----------------------------------------------------------

// stream adapts one upstream SSE body to core.Stream.  Recv is single-goroutine
// by contract; Close is idempotent and safe from anywhere.
type stream struct {
	c    *Client
	auth *Auth
	body io.ReadCloser
	sc   *sseScanner

	mu       sync.Mutex
	closeOne sync.Once
	closed   bool
	ended    bool
	sentDone bool
	pending  []core.Event
}

func newStream(c *Client, a *Auth, body io.ReadCloser) *stream {
	return &stream{c: c, auth: a, body: body, sc: newSSEScanner(body)}
}

// Recv returns the next core.Event, io.EOF exactly once at the end.
func (s *stream) Recv() (core.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	if s.ended {
		return core.Event{}, io.EOF
	}

	for {
		ev, err := s.sc.Next()
		if err != nil {
			s.ended = true
			s.closeBodyLocked()
			if err == io.EOF {
				if s.sentDone {
					return core.Event{}, io.EOF
				}
				// Upstream ended without a done frame: close the stream cleanly.
				s.sentDone = true
				return core.Event{Type: core.EventDone, Finish: "stop"}, nil
			}
			return core.Event{}, err
		}

		switch ev.Event {
		case "output":
			calls := toolCallEvents(ev.ToolCalls)
			if ev.Response != "" || ev.Reasoning != "" {
				s.pending = append(s.pending, calls...)
				return core.Event{Type: core.EventDelta, Delta: ev.Response, Reasoning: ev.Reasoning}, nil
			}
			if len(calls) > 0 {
				s.pending = append(s.pending, calls[1:]...)
				return calls[0], nil
			}
		case "token_usage":
			if u := usageFromMap(ev.Usage); u != nil {
				return core.Event{Type: core.EventUsage, Usage: u}, nil
			}
		case "notify_usage":
			s.c.recordNotifyUsage(s.auth, ev.Notify)
		case "done":
			s.ended = true
			s.sentDone = true
			s.closeBodyLocked()
			finish := ev.FinishReason
			if finish == "" {
				finish = "stop"
			}
			return core.Event{Type: core.EventDone, Finish: finish}, nil
		case "error":
			// A business error delivered inside an HTTP 200 stream.
			s.ended = true
			s.closeBodyLocked()
			serr := &Error{
				Kind:   ClassifyCode(ev.ErrorCode),
				Code:   ev.ErrorCode,
				Status: 200,
				Msg:    ev.ErrorMessage,
			}
			if s.c != nil && s.c.pool != nil {
				s.c.pool.MarkFailure(s.auth, serr)
			}
			return core.Event{Type: core.EventError, Err: serr}, nil
		}
	}
}

// Close is idempotent.
func (s *stream) Close() error {
	s.closeOne.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.body != nil {
			_ = s.body.Close()
		}
	})
	return nil
}

func (s *stream) closeBodyLocked() {
	if s.body != nil {
		_ = s.body.Close()
	}
}

// ---- frame helpers --------------------------------------------------------

// toolCallEvents converts an output frame's tool_calls payload into
// EventToolCall values.  The upstream uses "function_call"; OpenAI uses
// "function" — both are accepted.
func toolCallEvents(raw json.RawMessage) []core.Event {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		arr = []map[string]any{one}
	}
	out := make([]core.Event, 0, len(arr))
	for i, call := range arr {
		if call == nil {
			continue
		}
		idx := i
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			fn, _ = call["function_call"].(map[string]any)
		}
		delta := &core.ToolCallDelta{Index: idx}
		if v, ok := call["id"].(string); ok {
			delta.ID = v
		}
		if fn != nil {
			if v, ok := fn["name"].(string); ok {
				delta.Name = v
			}
			if v, ok := fn["arguments"].(string); ok {
				delta.Arguments = v
			}
		}
		out = append(out, core.Event{Type: core.EventToolCall, ToolCall: delta})
	}
	return out
}

// usageFromMap converts a token_usage frame into core.Usage.
func usageFromMap(m map[string]any) *core.Usage {
	if m == nil {
		return nil
	}
	u := &core.Usage{
		PromptTokens:     int(numFromAny(m["prompt_tokens"])),
		CompletionTokens: int(numFromAny(m["completion_tokens"])),
		TotalTokens:      int(numFromAny(m["total_tokens"])),
		ReasoningTokens:  int(numFromAny(m["reasoning_tokens"])),
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return u
}

// numFromAny coerces a JSON number/string into an int64.
func numFromAny(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64); err == nil {
			return i
		}
	}
	return 0
}
