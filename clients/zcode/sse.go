package zcode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// SSE framing
// ---------------------------------------------------------------------------

// sseReader extracts `data:` payloads from a server-sent-event body.  Only the
// data lines matter to us; `event:` names are redundant with the `type` field
// inside the JSON payload.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 32*1024)}
}

// maxFrameBytes bounds the payload of one SSE event, and errFrameTooLarge is
// what next returns when the ceiling is passed.  ZCode frames are small JSON
// objects, so a megabyte is far above anything real; without a bound an
// upstream that never sends its closing blank line would grow the accumulator
// until the process ran out of memory.
const maxFrameBytes = 1 << 20

var errFrameTooLarge = errors.New("zcode: upstream SSE frame exceeded the size limit")

// next returns the payload of the next event.  io.EOF marks the clean end.
func (s *sseReader) next() ([]byte, error) {
	// Accumulated with a byte count rather than a []string so the ceiling can be
	// checked on every line without an O(n) length sum each time.
	var b strings.Builder
	total := 0
	for {
		line, err := s.r.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if b.Len() > 0 {
					return []byte(b.String()), nil
				}
			case strings.HasPrefix(trimmed, ":"):
				// comment / keep-alive
			case strings.HasPrefix(trimmed, "data:"):
				v := strings.TrimPrefix(strings.TrimPrefix(trimmed, "data:"), " ")
				if b.Len() > 0 {
					b.WriteByte('\n')
					total++
				}
				b.WriteString(v)
				total += len(v)
				if total > maxFrameBytes {
					return nil, errFrameTooLarge
				}
			}
		}
		if err != nil {
			if b.Len() > 0 {
				return []byte(b.String()), nil
			}
			return nil, err
		}
	}
}

// ---------------------------------------------------------------------------
// Streaming translation
// ---------------------------------------------------------------------------

// anthropicStream translates Anthropic SSE frames into core.Events.
type anthropicStream struct {
	body   io.ReadCloser
	sse    *sseReader
	cancel context.CancelFunc

	queue []core.Event

	// Anthropic indexes every content block; OpenAI indexes only tool calls.
	toolIndex map[int]int
	nextTool  int
	hasTool   bool

	usage     core.Usage
	hasUsage  bool
	usageSent bool
	finish    string

	doneSent bool
	finished bool
	err      error
	closeOne sync.Once
}

func newAnthropicStream(body io.ReadCloser, model string) *anthropicStream {
	_ = model
	return &anthropicStream{
		body:      body,
		sse:       newSSEReader(body),
		toolIndex: make(map[int]int),
	}
}

// toolIndexFor maps an Anthropic block index onto a compact tool-call index.
func (s *anthropicStream) toolIndexFor(block int) int {
	if idx, ok := s.toolIndex[block]; ok {
		return idx
	}
	idx := s.nextTool
	s.nextTool++
	s.toolIndex[block] = idx
	return idx
}

// Recv implements core.Stream.
func (s *anthropicStream) Recv() (core.Event, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.finished {
			return core.Event{}, io.EOF
		}
		if s.err != nil {
			err := s.err
			s.err = nil
			s.finished = true
			return core.Event{}, err
		}

		payload, err := s.sse.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.finishUp()
				continue
			}
			s.err = err
			continue
		}
		s.consume(payload)
	}
}

// Close implements core.Stream and is safe to call more than once.
func (s *anthropicStream) Close() error {
	var err error
	s.closeOne.Do(func() {
		s.finished = true
		if s.body != nil {
			err = s.body.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	return err
}

// finishUp appends the terminal usage + done events exactly once.
func (s *anthropicStream) finishUp() {
	if !s.doneSent {
		if s.hasUsage && !s.usageSent {
			u := s.usage
			s.queue = append(s.queue, core.Event{Type: core.EventUsage, Usage: &u})
			s.usageSent = true
		}
		finish := s.finish
		if finish == "" {
			if s.hasTool {
				finish = "tool_calls"
			} else {
				finish = "stop"
			}
		}
		s.queue = append(s.queue, core.Event{Type: core.EventDone, Finish: finish})
		s.doneSent = true
	}
	s.finished = true
}

// consume translates one SSE payload into zero or more queued events.
func (s *anthropicStream) consume(payload []byte) {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return
	}
	if text == "[DONE]" {
		s.finishUp()
		return
	}

	var evt struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message *struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock *anthropicContentBlock `json:"content_block"`
		Delta        *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthropicUsage `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &evt); err != nil {
		return // an unparseable frame is skipped, never fatal
	}

	switch evt.Type {
	case "message_start":
		if evt.Message != nil {
			s.usage = usageToCore(evt.Message.Usage)
			s.hasUsage = true
		}

	case "content_block_start":
		if evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
			idx := s.toolIndexFor(evt.Index)
			s.hasTool = true
			s.queue = append(s.queue, core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index: idx,
					ID:    evt.ContentBlock.ID,
					Name:  evt.ContentBlock.Name,
				},
			})
		}

	case "content_block_delta":
		if evt.Delta == nil {
			return
		}
		switch evt.Delta.Type {
		case "text_delta":
			if evt.Delta.Text != "" {
				s.queue = append(s.queue, core.Event{Type: core.EventDelta, Delta: evt.Delta.Text})
			}
		case "thinking_delta":
			if evt.Delta.Thinking != "" {
				s.queue = append(s.queue, core.Event{Type: core.EventDelta, Reasoning: evt.Delta.Thinking})
			}
		case "input_json_delta":
			if evt.Delta.PartialJSON != "" {
				idx := s.toolIndexFor(evt.Index)
				s.queue = append(s.queue, core.Event{
					Type: core.EventToolCall,
					ToolCall: &core.ToolCallDelta{
						Index:     idx,
						Arguments: evt.Delta.PartialJSON,
					},
				})
			}
		}
		// signature_delta carries the thinking signature, which core.Message
		// has nowhere to round-trip; it is intentionally dropped.

	case "message_delta":
		if evt.Delta != nil && evt.Delta.StopReason != "" {
			s.finish = finishReason(evt.Delta.StopReason)
		}
		if evt.Usage != nil {
			s.mergeUsage(*evt.Usage)
		}

	case "message_stop":
		s.finishUp()

	case "error":
		msg := "stream error"
		if evt.Error != nil && strings.TrimSpace(evt.Error.Message) != "" {
			msg = evt.Error.Message
		}
		s.queue = append(s.queue, core.Event{Type: core.EventError, Err: errors.New("zcode upstream: " + msg)})
		s.finished = true

	default:
		// ping, content_block_stop and unknown frames carry nothing we need.
	}
}

// mergeUsage folds an incremental usage report into the running totals.
// message_delta usually carries only output_tokens.
func (s *anthropicStream) mergeUsage(u anthropicUsage) {
	if u.InputTokens > 0 || u.CacheReadInputTokens > 0 || u.CacheCreationInputTokens > 0 {
		s.usage.PromptTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
		s.usage.CachedTokens = u.CacheReadInputTokens
	}
	if u.OutputTokens > 0 {
		s.usage.CompletionTokens = u.OutputTokens
	}
	s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
	s.hasUsage = true
}

// ---------------------------------------------------------------------------
// Non-streaming translation
// ---------------------------------------------------------------------------

// sliceStream replays a pre-computed event sequence.
type sliceStream struct {
	events []core.Event
	pos    int
	body   io.Closer
	cancel context.CancelFunc
	closed bool
}

func newSliceStream(events []core.Event, body io.Closer, cancel context.CancelFunc) *sliceStream {
	return &sliceStream{events: events, body: body, cancel: cancel}
}

func (s *sliceStream) Recv() (core.Event, error) {
	if s.pos >= len(s.events) {
		return core.Event{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *sliceStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.body != nil {
		err = s.body.Close()
	}
	if s.cancel != nil {
		s.cancel()
	}
	return err
}
