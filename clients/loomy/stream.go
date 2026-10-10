package loomy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"client2api/internal/core"
)

// stream.go is the module's own SSE reader.
//
// The repository has no shared SSE helper -- each client writes its own, because
// the vendors disagree about framing -- so this one is deliberately explicit
// about the two things that actually matter: a data frame is only complete at a
// blank line, and the connection must not be allowed to sit silent forever.

// chatStream reads one Loomy completion.
//
// Loomy's /chat/completions is plain OpenAI-compatible SSE: `data: {...}` frames
// terminated by a blank line, then a final `data: [DONE]`.  There is no
// envelope, no encryption and no format translation, so the only work here is
// turning chunks into core.Events and enforcing an idle deadline.
//
// A chatStream is not safe for concurrent use: Recv and Close belong to one
// goroutine.
type chatStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	idle   time.Duration
	timer  *time.Timer
	cancel context.CancelFunc

	// idleFired records that the idle timer, not the caller, ended the stream,
	// so the resulting read error can be reported as a stall rather than as a
	// cancellation.
	idleFired atomic.Bool

	// pending holds events decoded from a chunk that carries more than one
	// thing (a reasoning delta and a content delta, or several tool calls).
	pending []core.Event

	usage   *core.Usage
	finish  string
	done    bool
	closed  bool
	reached bool // a terminator frame was seen
}

func newChatStream(body io.ReadCloser, idle time.Duration, cancel context.CancelFunc) *chatStream {
	s := &chatStream{
		body:   body,
		reader: bufio.NewReaderSize(body, 64<<10),
		idle:   idle,
		cancel: cancel,
	}
	if idle > 0 {
		s.timer = time.AfterFunc(idle, func() {
			s.idleFired.Store(true)
			cancel()
		})
	}
	return s
}

func (s *chatStream) stopTimer() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

// touch pushes the idle deadline back after every byte the upstream sent.
func (s *chatStream) touch() {
	if s.timer != nil {
		s.timer.Reset(s.idle)
	}
}

// Recv returns the next event, or io.EOF exactly once at the clean end of the
// stream.
func (s *chatStream) Recv() (core.Event, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	if s.done || s.closed {
		return core.Event{}, io.EOF
	}

	for {
		payload, err := s.readFrame()
		if err != nil {
			s.stopTimer()
			s.done = true
			if errors.Is(err, io.EOF) {
				// A normal completion must carry an explicit terminal frame.
				// Treating a bare connection close as success turns truncated
				// answers into apparently valid responses for the caller.
				s.pending = append(s.pending, core.Event{
					Type: core.EventError,
					Err:  errors.New("loomy: upstream stream ended before a terminal frame"),
				})
				if len(s.pending) > 0 {
					ev := s.pending[0]
					s.pending = s.pending[1:]
					return ev, nil
				}
				return core.Event{}, io.EOF
			}
			if s.idleFired.Load() {
				return core.Event{}, fmt.Errorf("loomy: the stream was idle for %s", s.idle)
			}
			return core.Event{}, fmt.Errorf("loomy: reading the stream: %w", err)
		}

		// Two terminators are accepted: the standard `[DONE]` sentinel and the
		// empty `data:` frame the vendor also uses.
		if payload == "" || payload == "[DONE]" {
			// The terminator ends the stream for good: anything the upstream
			// sends afterwards is not part of this completion and must not be
			// handed to the caller as a late delta.
			s.stopTimer()
			s.done = true
			s.finishUp()
			if len(s.pending) > 0 {
				ev := s.pending[0]
				s.pending = s.pending[1:]
				return ev, nil
			}
			return core.Event{}, io.EOF
		}

		if err := s.decode(payload); err != nil {
			s.stopTimer()
			s.done = true
			return core.Event{}, err
		}
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
	}
}

// finishUp queues the trailing usage and done events once, when the stream ends.
func (s *chatStream) finishUp() {
	if s.reached {
		return
	}
	s.reached = true
	if s.usage != nil {
		s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: s.usage})
	}
	s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: s.finish})
}

// readFrame reads one SSE event and returns its data payload.
//
// Per the SSE specification a frame may carry several `data:` lines, which are
// joined with newlines; the frame ends at a blank line.  Comment lines (those
// starting with a colon) are keep-alives and are skipped.
func (s *chatStream) readFrame() (string, error) {
	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		s.touch()

		line = strings.TrimRight(line, "\r\n")
		if line != "" && !strings.HasPrefix(line, ":") {
			field, value, _ := strings.Cut(line, ":")
			if field == "data" {
				data = append(data, strings.TrimPrefix(value, " "))
			}
			// `event:`, `id:` and `retry:` are not used by this vendor and are
			// ignored rather than guessed at.
		}

		if errors.Is(err, io.EOF) {
			// The upstream closed.  A final frame that never received its blank
			// line is still a frame, so the last chunk must not be dropped.
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			return "", io.EOF
		}

		if line == "" {
			if len(data) == 0 {
				continue // a blank keep-alive between frames
			}
			return strings.Join(data, "\n"), nil
		}
	}
}

// chunk is one OpenAI-compatible streaming chunk.
type chunk struct {
	Code    string `json:"code"`
	Desc    string `json:"desc"`
	Message string `json:"message"`
	Error   *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		TotalTokens         int `json:"total_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// decode turns one frame payload into pending events.
//
// A business failure can also arrive inside the stream as HTTP 200, which is why
// a non-OK `code` or an `error` object is turned into a typed error here rather
// than being skipped as an unknown chunk.
func (s *chatStream) decode(payload string) error {
	var c chunk
	if err := json.Unmarshal([]byte(payload), &c); err != nil {
		return fmt.Errorf("loomy: the stream sent a frame that is not JSON: %w", err)
	}

	if c.Error != nil && c.Error.Message != "" {
		return &apiError{Message: c.Error.Message}
	}
	if c.Code != "" && c.Code != loomyOKCode {
		message := c.Desc
		if message == "" {
			message = c.Message
		}
		if message == "" {
			message = "business error " + c.Code
		}
		return &apiError{Code: c.Code, Message: message}
	}

	if c.Usage != nil {
		usage := &core.Usage{
			PromptTokens:     c.Usage.PromptTokens,
			CompletionTokens: c.Usage.CompletionTokens,
			TotalTokens:      c.Usage.TotalTokens,
			ReasoningTokens:  c.Usage.CompletionTokensDetails.ReasoningTokens,
			CachedTokens:     c.Usage.PromptTokensDetails.CachedTokens,
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
		s.usage = usage
	}

	for _, choice := range c.Choices {
		reasoning := choice.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = choice.Delta.Reasoning
		}
		if choice.Delta.Content != "" || reasoning != "" {
			s.pending = append(s.pending, core.Event{
				Type:      core.EventDelta,
				Delta:     choice.Delta.Content,
				Reasoning: reasoning,
			})
		}
		for _, call := range choice.Delta.ToolCalls {
			s.pending = append(s.pending, core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index:     call.Index,
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				},
			})
		}
		if choice.FinishReason != "" {
			s.finish = choice.FinishReason
		}
	}
	return nil
}

// Close releases the connection.  It is safe to call more than once.
func (s *chatStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.stopTimer()
	if s.cancel != nil {
		s.cancel()
	}
	return s.body.Close()
}
