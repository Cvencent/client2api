package minimaxcode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"client2api/internal/core"
)

// sseReader yields one SSE event payload at a time.  Only `data:` lines carry
// meaning here: the `event:` line duplicates the payload's own `type` field,
// and trusting one source rather than two is what keeps this reader honest when
// the vendor renames an event.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 32*1024)}
}

// maxFrameBytes bounds the payload of one SSE event, and errFrameTooLarge is
// what next returns once the ceiling is passed.  MiniMax frames are small JSON
// objects, so a megabyte is far above anything real; without a bound an
// upstream that never sends its closing blank line grows the accumulator until
// the process runs out of memory.
const maxFrameBytes = 1 << 20

var errFrameTooLarge = errors.New("minimaxcode: upstream SSE frame exceeded the size limit")

// next returns the next event's data.  It returns io.EOF when the body ends.
func (s *sseReader) next() ([]byte, error) {
	// A byte count alongside the builder keeps the ceiling check O(1) per line;
	// summing the lines each time would be O(n) per line.
	var b strings.Builder
	total := 0
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			if len(line) > 0 && !errors.Is(err, io.EOF) {
				return nil, err
			}
			if b.Len() == 0 {
				return nil, io.EOF
			}
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if b.Len() == 0 {
				continue // a blank line between events
			}
			break
		}
		if strings.HasPrefix(line, ":") {
			continue // a comment/keep-alive
		}
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			v := strings.TrimPrefix(rest, " ")
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
	if b.Len() == 0 {
		return nil, io.EOF
	}
	return []byte(b.String()), nil
}

// anthropicStream turns the vendor's SSE into core events.
type anthropicStream struct {
	body   io.ReadCloser
	sse    *sseReader
	cancel context.CancelFunc

	queue []core.Event

	// toolIndex maps the upstream's content-block index onto the compact
	// 0..n-1 index the OpenAI wire format uses for tool calls.
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

	closeOnce sync.Once
}

func newAnthropicStream(body io.ReadCloser, cancel context.CancelFunc) *anthropicStream {
	return &anthropicStream{
		body:      body,
		sse:       newSSEReader(body),
		cancel:    cancel,
		toolIndex: map[int]int{},
	}
}

// Recv returns the next event, io.EOF once the stream is spent.
func (s *anthropicStream) Recv() (core.Event, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, nil
		}
		if s.err != nil {
			err := s.err
			s.err = nil
			return core.Event{}, err
		}
		if s.finished {
			return core.Event{}, io.EOF
		}
		payload, err := s.sse.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.finishUp()
				continue
			}
			s.err = err
			s.finished = true
			return core.Event{}, err
		}
		if len(payload) == 0 {
			continue
		}
		if string(payload) == "[DONE]" {
			s.finishUp()
			continue
		}
		s.consume(payload)
	}
}

// Close releases the body and cancels the request context.  It is safe to call
// more than once and safe to call without ever reading the stream.
func (s *anthropicStream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.body != nil {
			err = s.body.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	return err
}

// finishUp appends the usage frame (when one arrived and was not already sent)
// and the terminating done frame, exactly once.
func (s *anthropicStream) finishUp() {
	if s.doneSent {
		s.finished = true
		return
	}
	s.doneSent = true
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
	s.finished = true
}

// consume dispatches one SSE payload.  Frames it does not understand are
// skipped: an unknown event kind is not a reason to lose a reply that is
// already streaming.
func (s *anthropicStream) consume(payload []byte) {
	var evt struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			Usage anthropicUsage `json:"usage"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage anthropicUsage `json:"usage"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &evt); err != nil {
		return
	}

	switch evt.Type {
	case "message_start":
		if evt.Message.Usage != (anthropicUsage{}) {
			s.mergeUsage(evt.Message.Usage)
		}

	case "content_block_start":
		if evt.ContentBlock.Type == "tool_use" {
			s.hasTool = true
			idx := s.toolIndexFor(evt.Index)
			s.queue = append(s.queue, core.Event{
				Type:     core.EventToolCall,
				ToolCall: &core.ToolCallDelta{Index: idx, ID: evt.ContentBlock.ID, Name: evt.ContentBlock.Name},
			})
		}

	case "content_block_delta":
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
				s.hasTool = true
				idx := s.toolIndexFor(evt.Index)
				s.queue = append(s.queue, core.Event{
					Type:     core.EventToolCall,
					ToolCall: &core.ToolCallDelta{Index: idx, Arguments: evt.Delta.PartialJSON},
				})
			}
		}

	case "message_delta":
		if evt.Delta.StopReason != "" {
			s.finish = finishReason(evt.Delta.StopReason)
		}
		if evt.Usage != (anthropicUsage{}) {
			s.mergeUsage(evt.Usage)
		}

	case "message_stop":
		s.finishUp()

	case "error":
		msg := strings.TrimSpace(evt.Error.Message)
		if msg == "" {
			msg = strings.TrimSpace(evt.Error.Type)
		}
		if msg == "" {
			msg = "the upstream reported an error mid-stream"
		}
		s.err = fmt.Errorf("minimaxcode upstream: %s", truncate(msg, 300))
		s.finished = true

	default:
		// ping, content_block_stop, and anything the vendor adds later.
	}
}

// toolIndexFor compacts the upstream's block indices into the 0..n-1 sequence
// the OpenAI wire format expects, remembering the mapping so a delta that
// arrives later lands on the same tool call.
func (s *anthropicStream) toolIndexFor(block int) int {
	if idx, ok := s.toolIndex[block]; ok {
		return idx
	}
	idx := s.nextTool
	s.toolIndex[block] = idx
	s.nextTool++
	return idx
}

// mergeUsage folds a partial usage report into the running total.  Anthropic
// splits input and output tokens across message_start and message_delta, so
// this has to be a merge rather than an assignment.
func (s *anthropicStream) mergeUsage(u anthropicUsage) {
	core := usageToCore(u)
	if core.CachedTokensKnown {
		s.usage.CachedTokensKnown = true
	}
	if core.PromptTokens > 0 {
		s.usage.PromptTokens = core.PromptTokens
		s.usage.CachedTokens = core.CachedTokens
	}
	if core.CompletionTokens > 0 {
		s.usage.CompletionTokens = core.CompletionTokens
	}
	s.usage.TotalTokens = s.usage.PromptTokens + s.usage.CompletionTokens
	s.hasUsage = true
}

// sliceStream replays a buffered event list as a Stream, so the non-streaming
// path and the streaming path present exactly one interface to the gateway.
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
