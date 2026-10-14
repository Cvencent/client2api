package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// upstream response shapes
// ---------------------------------------------------------------------------

type geminiUsageMetadata struct {
	PromptTokenCount        *int `json:"promptTokenCount"`
	CandidatesTokenCount    *int `json:"candidatesTokenCount"`
	ThoughtsTokenCount      *int `json:"thoughtsTokenCount"`
	TotalTokenCount         *int `json:"totalTokenCount"`
	CachedContentTokenCount *int `json:"cachedContentTokenCount"`
}

type geminiCandidate struct {
	Content      *geminiCandidateContent `json:"content"`
	FinishReason string                  `json:"finishReason"`
}

type geminiCandidateContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate    `json:"candidates"`
	UsageMetadata *geminiUsageMetadata `json:"usageMetadata"`
	ModelVersion  string               `json:"modelVersion"`
	ResponseID    string               `json:"responseId"`
}

// ---------------------------------------------------------------------------
// stream
// ---------------------------------------------------------------------------

// chatStream reads the upstream SSE (or a buffered JSON body) and re-emits
// core.Events.  Recv is single-goroutine; Close is idempotent.
//
// The translation is a small producer/queue: each Recv drains pending, and
// when it runs dry the next frame (SSE) or the single parsed body (buffered)
// is turned into more events.  Terminal usage+done are appended exactly once,
// when the source is exhausted.
type chatStream struct {
	ctx     context.Context
	cancel  context.CancelFunc
	body    io.ReadCloser
	scanner *bufio.Scanner
	sig     *sigStore

	// buffered, when non-nil, is a whole JSON body to parse once.
	buffered []byte
	parsed   bool

	pending []core.Event

	// open tool call bookkeeping across frames.
	toolIndex int
	toolCalls int

	finish    string
	usage     *core.Usage
	usageSeen bool
	sawText   bool

	exhausted bool
	done      bool

	mu     sync.Mutex
	closed bool
}

func newSSEStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser, sig *sigStore) *chatStream {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	return &chatStream{ctx: ctx, cancel: cancel, body: body, scanner: sc, sig: sig}
}

func newBufferedStream(ctx context.Context, cancel context.CancelFunc, body io.ReadCloser, sig *sigStore) (*chatStream, []byte, error) {
	defer body.Close()
	buf, err := io.ReadAll(body)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return &chatStream{ctx: ctx, cancel: cancel, buffered: buf, sig: sig}, buf, nil
}

// Recv returns the next event.
func (s *chatStream) Recv() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			return core.Event{}, io.EOF
		}
		if err := s.fill(); err != nil {
			return core.Event{}, err
		}
	}
}

// fill appends the next batch of events, or marks the stream done.
func (s *chatStream) fill() error {
	if err := s.ctx.Err(); err != nil {
		s.done = true
		return err
	}
	if s.buffered != nil {
		s.fillFromBuffer()
		return nil
	}
	return s.fillFromSSE()
}

// fillFromBuffer parses the whole body once.
func (s *chatStream) fillFromBuffer() {
	if s.parsed {
		s.complete()
		return
	}
	s.parsed = true
	inner := unwrapResponse(s.buffered)
	var resp geminiResponse
	if err := json.Unmarshal(inner, &resp); err != nil {
		// The body was not a single response.  Try SSE, which is what the
		// upstream returns when the caller asked for a stream but the module
		// took the buffered path only for an error probe.
		if events, ok := parseSSEBody(s.buffered, s); ok {
			s.pending = append(s.pending, events...)
			s.complete()
			return
		}
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: errors.New("gemini: upstream body was neither JSON nor SSE")})
		s.complete()
		return
	}
	s.noteUsage(resp.UsageMetadata)
	if len(resp.Candidates) > 0 {
		s.emitCandidate(&resp.Candidates[0])
	}
	s.complete()
}

// fillFromSSE reads frames until one produces an event or the stream ends.
func (s *chatStream) fillFromSSE() error {
	for {
		if !s.scanner.Scan() {
			if err := s.scanner.Err(); err != nil {
				s.pending = append(s.pending, core.Event{Type: core.EventError, Err: err})
				s.complete()
				return nil
			}
			s.complete()
			return nil
		}
		line := strings.TrimRight(s.scanner.Text(), "\r")
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			s.complete()
			return nil
		}
		var resp geminiResponse
		if err := json.Unmarshal(unwrapResponse([]byte(payload)), &resp); err != nil {
			continue // an unparseable frame is skipped, not fatal
		}
		s.noteUsage(resp.UsageMetadata)
		if len(resp.Candidates) > 0 {
			s.emitCandidate(&resp.Candidates[0])
		}
		if len(s.pending) > 0 {
			return nil
		}
	}
}

// emitCandidate converts one candidate's parts into pending events.
func (s *chatStream) emitCandidate(cand *geminiCandidate) {
	if cand == nil {
		return
	}
	if cand.FinishReason != "" {
		s.finish = cand.FinishReason
	}
	if cand.Content == nil {
		return
	}
	for _, part := range cand.Content.Parts {
		if part.FunctionCall != nil {
			args := canonicalArgs(part.FunctionCall.Args)
			if s.sig != nil && part.ThoughtSignature != "" {
				s.sig.Put(part.FunctionCall.Name, args, part.ThoughtSignature)
			}
			s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
				Index: s.toolIndex, ID: newToolCallID(s.toolIndex), Name: part.FunctionCall.Name, Arguments: args,
			}})
			s.toolIndex++
			s.toolCalls++
			continue
		}
		if part.Text == "" {
			continue
		}
		s.sawText = true
		if part.Thought {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: part.Text})
			continue
		}
		s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: part.Text})
	}
}

// noteUsage keeps the frame with the largest totalTokenCount.
func (s *chatStream) noteUsage(raw *geminiUsageMetadata) {
	if raw == nil {
		return
	}
	u := usageFromMetadata(raw)
	if u == nil {
		return
	}
	if !s.usageSeen || u.TotalTokens >= s.usage.TotalTokens {
		s.usage = u
		s.usageSeen = true
	}
}

// complete appends the terminal usage + done events exactly once.
func (s *chatStream) complete() {
	if s.exhausted {
		return
	}
	s.exhausted = true
	if !s.sawText && s.toolCalls == 0 {
		// A turn with no text, no reasoning and no tool call is not an answer.
		// The reference throws the same way; the gateway would otherwise file a
		// silent half-turn as a success.
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: errors.New("gemini: the model returned no content")})
		s.done = true
		return
	}
	if s.usage != nil {
		s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: s.usage})
	}
	s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: mapFinish(s.finish, s.toolCalls)})
	// Mark done so that once the queue drains, Recv reports io.EOF.  It is set
	// after a successful drain in Recv, but marking it now is harmless: Recv
	// drains pending before consulting done.
	s.done = true
}

// Close releases the upstream body and cancels the request context.
func (s *chatStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

// unwrapResponse peels the `{"response":{…}}` envelope when present.
func unwrapResponse(raw []byte) []byte {
	var probe struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.Response) > 0 {
		trimmed := strings.TrimSpace(string(probe.Response))
		if strings.HasPrefix(trimmed, "{") {
			return probe.Response
		}
	}
	return raw
}

// parseSSEBody is the fallback used when a "buffered" body turns out to be an
// SSE transcript (the vendor answers with SSE regardless of the request).
func parseSSEBody(raw []byte, s *chatStream) ([]core.Event, bool) {
	var events []core.Event
	sawFrame := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var resp geminiResponse
		if err := json.Unmarshal(unwrapResponse([]byte(payload)), &resp); err != nil {
			continue
		}
		sawFrame = true
		s.noteUsage(resp.UsageMetadata)
		if len(resp.Candidates) > 0 {
			s.emitCandidate(&resp.Candidates[0])
		}
	}
	if !sawFrame {
		return nil, false
	}
	events = append(events, s.pending...)
	s.pending = nil
	return events, true
}

// usageFromMetadata maps the vendor's usage onto core.Usage.  promptTokenCount
// INCLUDES the cached tokens, so the cached portion is subtracted to keep the
// two protocols' ledgers comparable.
func usageFromMetadata(raw *geminiUsageMetadata) *core.Usage {
	if raw == nil {
		return nil
	}
	u := &core.Usage{}
	if raw.PromptTokenCount != nil {
		cached := 0
		if raw.CachedContentTokenCount != nil {
			cached = *raw.CachedContentTokenCount
			u.CachedTokens = cached
			u.CachedTokensKnown = true
		}
		u.PromptTokens = max0(*raw.PromptTokenCount - cached)
	}
	if raw.CandidatesTokenCount != nil {
		u.CompletionTokens = *raw.CandidatesTokenCount
	}
	if raw.TotalTokenCount != nil {
		u.TotalTokens = *raw.TotalTokenCount
	}
	if raw.ThoughtsTokenCount != nil {
		u.ReasoningTokens = *raw.ThoughtsTokenCount
	}
	return u
}

// mapFinish maps the vendor's finishReason onto an OpenAI finish reason.
func mapFinish(reason string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_calls"
	}
	if strings.EqualFold(strings.TrimSpace(reason), "MAX_TOKENS") {
		return "length"
	}
	return "stop"
}

// newToolCallID mints a stable, readable id (the vendor supplies none).
func newToolCallID(index int) string {
	return "gemini_tool_" + strconv.FormatInt(time.Now().UnixNano(), 36) + "_" + strconv.Itoa(index)
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

var _ core.Stream = (*chatStream)(nil)
