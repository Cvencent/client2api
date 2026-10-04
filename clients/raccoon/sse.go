package raccoon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// maxFrameBytes bounds one SSE frame, so a hostile or broken upstream cannot
// grow a frame without limit.
const maxFrameBytes = 1 << 20

var (
	// errStreamDone is returned by a frame handler for `data: [DONE]`;
	// readFrames turns it into a clean nil.
	errStreamDone    = errors.New("raccoon: stream done")
	errFrameTooLarge = errors.New("raccoon: sse frame too large")
	errEmptyStream   = errors.New("raccoon: upstream returned an empty stream")
)

// readFrames is the SSE framing layer. It understands `data:` lines, blank-line
// event delimiters, `:` comments/heartbeats and multi-line data (joined with
// "\n"). A bare `{`-prefixed line is also accepted, for gateways that omit the
// `data:` prefix.
func readFrames(ctx context.Context, r io.Reader, onPayload func(string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data strings.Builder

	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := data.String()
		data.Reset()
		return onPayload(payload)
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, rerr := br.ReadString('\n')
		if line != "" {
			if data.Len() > maxFrameBytes {
				return errFrameTooLarge
			}
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if ferr := flush(); ferr != nil {
					if errors.Is(ferr, errStreamDone) {
						return nil
					}
					return ferr
				}
			case strings.HasPrefix(trimmed, ":"):
				// comment / heartbeat
			case strings.HasPrefix(trimmed, "data:"):
				v := strings.TrimPrefix(trimmed, "data:")
				v = strings.TrimPrefix(v, " ")
				if data.Len() > 0 {
					data.WriteString("\n")
				}
				data.WriteString(v)
			case strings.HasPrefix(trimmed, "event:"),
				strings.HasPrefix(trimmed, "id:"),
				strings.HasPrefix(trimmed, "retry:"):
				// ignored framing fields
			case strings.HasPrefix(trimmed, "{"):
				if data.Len() == 0 {
					data.WriteString(trimmed)
				}
			}
		}
		if rerr != nil {
			if ferr := flush(); ferr != nil && !errors.Is(ferr, errStreamDone) {
				return ferr
			}
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}

// chunk is one parsed SSE payload.
type chunk struct {
	Delta     string
	Reasoning string
	ToolCalls []core.ToolCallDelta
	Finish    string
	Usage     *core.Usage
	Err       string
}

func (c chunk) empty() bool {
	return c.Delta == "" && c.Reasoning == "" && len(c.ToolCalls) == 0 &&
		c.Finish == "" && c.Usage == nil && c.Err == ""
}

// parseChunk decodes one OpenAI-shaped streaming chunk. It reports ok=false
// for a payload that carries nothing we understand.
func parseChunk(raw string) (chunk, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[DONE]" || raw == "{}" || raw == "null" {
		return chunk{}, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return chunk{}, false
	}
	var out chunk

	if e, ok := m["error"].(map[string]any); ok {
		out.Err = firstStringField(e, "message", "msg", "detail")
		if out.Err == "" {
			out.Err = "upstream stream error"
		}
		return out, true
	}
	if code, ok := numberField(m, "code"); ok && code != 0 {
		msg := firstStringField(m, "message", "msg", "details")
		if msg == "" {
			msg = "upstream code " + strconv.FormatFloat(code, 'f', -1, 64)
		}
		out.Err = msg
		return out, true
	}

	if u, ok := m["usage"]; ok {
		out.Usage = usageFromAny(u)
	}

	choices, _ := m["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		if choice != nil {
			if s := firstStringField(choice, "finish_reason"); s != "" && s != "null" {
				out.Finish = s
			}
			if d, ok := choice["delta"].(map[string]any); ok {
				fillFromMessage(&out, d)
			} else if msg, ok := choice["message"].(map[string]any); ok {
				// Some gateways answer a "stream" with one whole message.
				fillFromMessage(&out, msg)
			}
		}
	}
	if out.empty() {
		return chunk{}, false
	}
	return out, true
}

func fillFromMessage(out *chunk, m map[string]any) {
	out.Delta += stringField(m, "content")
	out.Reasoning += firstStringField(m, "reasoning_content", "reasoning")
	out.ToolCalls = append(out.ToolCalls, toolCallDeltas(m["tool_calls"])...)
}

// toolCallDeltas normalises a `tool_calls` array. The index defaults to the
// slice position when the vendor omits it.
func toolCallDeltas(v any) []core.ToolCallDelta {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]core.ToolCallDelta, 0, len(arr))
	for i, raw := range arr {
		obj, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		d := core.ToolCallDelta{
			Index: i,
			ID:    stringField(obj, "id"),
		}
		if n, ok := numberField(obj, "index"); ok {
			d.Index = int(n)
		}
		if fn, ok := obj["function"].(map[string]any); ok {
			d.Name = stringField(fn, "name")
			d.Arguments = stringField(fn, "arguments")
		}
		if d.ID == "" && d.Name == "" && d.Arguments == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

// usageFromAny extracts token counters from a `usage` object. It returns nil
// when every counter is zero, so a chunk with no real usage does not emit a
// bogus EventUsage.
func usageFromAny(v any) *core.Usage {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			if f, ok := numberField(m, k); ok && f > 0 {
				return int(f)
			}
		}
		return 0
	}
	u := core.Usage{
		PromptTokens:     num("prompt_tokens", "input_tokens"),
		CompletionTokens: num("completion_tokens", "output_tokens"),
		TotalTokens:      num("total_tokens"),
		ReasoningTokens:  num("reasoning_tokens"),
		CachedTokens:     num("cached_tokens", "prompt_cache_hit_tokens", "cache_read_input_tokens"),
	}
	if d, ok := m["completion_tokens_details"].(map[string]any); ok {
		if f, ok := numberField(d, "reasoning_tokens"); ok && f > 0 {
			u.ReasoningTokens = int(f)
		}
	}
	if d, ok := m["prompt_tokens_details"].(map[string]any); ok {
		if f, ok := numberField(d, "cached_tokens"); ok && f > 0 {
			u.CachedTokens = int(f)
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.ReasoningTokens == 0 && u.CachedTokens == 0 {
		return nil
	}
	return &u
}

// normalizeFinish maps the vendor's finish reason onto the four values the
// core understands.
func normalizeFinish(finish string, sawToolCall bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "stop", "end_turn", "eos":
		return "stop"
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	case "tool_calls", "function_call", "tool_use":
		return "tool_calls"
	case "content_filter", "safety":
		return "content_filter"
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringField(m, k); s != "" && s != "null" {
			return s
		}
	}
	return ""
}

// numberField accepts both JSON numbers and numeric strings (the vendor
// quotes some counters).
func numberField(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case float64:
			return t, true
		case int:
			return float64(t), true
		case json.Number:
			if f, err := t.Float64(); err == nil {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// ---- stream ----------------------------------------------------------

// toolAccumulator merges the fragments of ONE tool call. Fragments are keyed
// by `index`; `arguments` are concatenated. Emitting each raw fragment
// instead would leak a single call as several calls.
type toolAccumulator struct {
	id   string
	name string
	args strings.Builder
}

type raccoonStream struct {
	ctx          context.Context
	cancel       context.CancelFunc
	body         io.ReadCloser
	ch           chan core.Event
	release      context.CancelFunc
	once         sync.Once
	firstTimeout time.Duration
	idleTimeout  time.Duration
	closeErr     error
}

func newRaccoonStream(parent context.Context, release context.CancelFunc, body io.ReadCloser, first, idle time.Duration) *raccoonStream {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	if first <= 0 {
		first = defaultFirstTokenTimeout
	}
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	s := &raccoonStream{
		ctx:          ctx,
		cancel:       cancel,
		body:         body,
		ch:           make(chan core.Event, 32),
		release:      release,
		firstTimeout: first,
		idleTimeout:  idle,
	}
	// GoSafe already spawns the goroutine; wrapping it in a bare "go" as well
	// only adds a goroutine that does nothing but call GoSafe.
	core.GoSafe("raccoon stream", nil, s.run)
	return s
}

// Recv returns io.EOF exactly once, at the clean end of the stream.
func (s *raccoonStream) Recv() (core.Event, error) {
	ev, ok := <-s.ch
	if !ok {
		return core.Event{}, io.EOF
	}
	return ev, nil
}

// Close is safe to call repeatedly.
func (s *raccoonStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.release != nil {
			s.release()
		}
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
	})
	return s.closeErr
}

func (s *raccoonStream) emit(ev core.Event) {
	select {
	case s.ch <- ev:
	case <-s.ctx.Done():
	}
}

func (s *raccoonStream) run() {
	defer close(s.ch)
	defer func() {
		if s.release != nil {
			s.release()
		}
	}()
	// Registered last, so it runs FIRST: a panic becomes an error event
	// instead of killing the process.
	defer func() {
		if r := recover(); r != nil {
			s.emit(core.Event{Type: core.EventError, Err: fmt.Errorf("raccoon: stream panic: %v", r)})
		}
	}()

	frames := make(chan string, 16)
	readErr := make(chan error, 1)
	// GoSafe, not a bare "go": s.run's own recover above covers only this
	// goroutine.  A panic in the reader would close frames but never fill
	// readErr, and the consumer's blocking <-readErr would hang the stream for
	// good -- so the report becomes the read error.
	core.GoSafe("raccoon frame reader", func(msg string) {
		readErr <- errors.New(msg)
	}, func() {
		defer close(frames)
		readErr <- readFrames(s.ctx, s.body, func(p string) error {
			select {
			case frames <- p:
				return nil
			case <-s.ctx.Done():
				return s.ctx.Err()
			}
		})
	})

	var (
		acc     = map[int]*toolAccumulator{}
		order   []int
		sawTool bool
		sawData bool
		finish  string
		usage   *core.Usage
	)

	flushTools := func() {
		for _, idx := range order {
			a := acc[idx]
			if a == nil {
				continue
			}
			sawTool = true
			s.emit(core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index:     idx,
					ID:        a.id,
					Name:      a.name,
					Arguments: a.args.String(),
				},
			})
		}
		acc = map[int]*toolAccumulator{}
		order = order[:0]
	}

	timeout := s.firstTimeout
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	first := true

loop:
	for {
		select {
		case <-s.ctx.Done():
			return
		case p, ok := <-frames:
			if !ok {
				rerr := <-readErr
				if rerr != nil && s.ctx.Err() == nil && !errors.Is(rerr, context.Canceled) {
					s.emit(core.Event{Type: core.EventError, Err: rerr})
					return
				}
				break loop
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if first {
				first = false
				timeout = s.idleTimeout
			}
			timer.Reset(timeout)

			raw := strings.TrimSpace(p)
			if raw == "" {
				continue
			}
			if raw == "[DONE]" {
				break loop
			}
			ch, okc := parseChunk(raw)
			if !okc {
				continue
			}
			sawData = true
			if ch.Err != "" {
				s.emit(core.Event{Type: core.EventError, Err: errors.New("raccoon: " + ch.Err)})
				s.cancel()
				return
			}
			if ch.Usage != nil {
				usage = ch.Usage
			}
			if ch.Finish != "" {
				finish = ch.Finish
			}
			for _, d := range ch.ToolCalls {
				a := acc[d.Index]
				if a == nil {
					a = &toolAccumulator{}
					acc[d.Index] = a
					order = append(order, d.Index)
				}
				if a.id == "" {
					a.id = d.ID
				}
				if a.name == "" {
					a.name = d.Name
				}
				a.args.WriteString(d.Arguments)
			}
			if ch.Reasoning != "" {
				s.emit(core.Event{Type: core.EventDelta, Reasoning: ch.Reasoning})
			}
			if ch.Delta != "" {
				s.emit(core.Event{Type: core.EventDelta, Delta: ch.Delta})
			}
			if ch.Finish != "" {
				flushTools()
			}
		case <-timer.C:
			s.emit(core.Event{
				Type: core.EventError,
				Err:  fmt.Errorf("raccoon: upstream stopped sending data (idle timeout)"),
			})
			s.cancel()
			return
		}
	}

	flushTools()
	if !sawData && finish == "" {
		s.emit(core.Event{Type: core.EventError, Err: errEmptyStream})
		return
	}
	if usage != nil {
		s.emit(core.Event{Type: core.EventUsage, Usage: usage})
	}
	s.emit(core.Event{Type: core.EventDone, Finish: normalizeFinish(finish, sawTool)})
}
