package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// A hand-written SSE reader.
//
// There is no shared helper in this repo, and the vendor's stream is not plain
// OpenAI: the same connection carries
//
//	data: {"choices":[{...}]}                 normal chunk
//	data: {"choices":[],"cost":"0.0001"}      a cost frame (oa-compat)
//	event: ping                               a keep-alive (anthropic/openai)
//	data: {"type":"ping",...}
//	data: [DONE]
//
// so an unknown frame must be ignored rather than treated as a failure, and a
// chunk with an empty choices array is normal.  The reader also has to survive
// a server that never sends the trailing blank line, and one that stalls
// forever — hence the idle timeout, which the reader cannot provide on its own
// (a blocking Read cannot be interrupted without a deadline on the socket), so
// the frames are pumped through a goroutine and Recv selects on a timer.
// ---------------------------------------------------------------------------

type sseFrame struct {
	Event string
	Data  string
}

type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// readLine returns one line without its terminator.
func (s *sseReader) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		if len(line) > 0 {
			// A final line without a newline is still a line.
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// next returns the next frame.  io.EOF means the stream ended cleanly; a frame
// that was cut off by the end of the stream is still returned.
func (s *sseReader) next() (sseFrame, error) {
	var frame sseFrame
	var saw bool
	for {
		line, err := s.readLine()
		if err != nil {
			if saw {
				return frame, nil
			}
			return sseFrame{}, err
		}
		if line == "" {
			if saw {
				return frame, nil
			}
			// A blank line between frames is a separator, not a frame.
			continue
		}
		if strings.HasPrefix(line, ":") {
			// A comment, which is how several vendors keep a connection alive.
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			// A bare field name with no colon: an event with empty data.
			saw = true
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if frame.Data != "" {
				frame.Data += "\n"
			}
			frame.Data += value
			saw = true
		case "event":
			frame.Event = value
			saw = true
		default:
			// id, retry and anything else are not used here.
		}
	}
}

// frameResult is one frame or the terminal error.
type frameResult struct {
	frame sseFrame
	err   error
}

// framePump reads frames on its own goroutine so Recv can enforce an idle
// timeout and honour context cancellation while a Read is blocked.
type framePump struct {
	ch   chan frameResult
	done chan struct{}
	once sync.Once
}

func newFramePump(r io.Reader, report func(string)) *framePump {
	p := &framePump{
		ch:   make(chan frameResult, 1),
		done: make(chan struct{}),
	}
	reader := newSSEReader(r)
	// core.GoSafe, not a bare goroutine: a panic while parsing a hostile frame
	// must not take the process down.
	core.GoSafe("opencode sse reader", report, func() {
		for {
			frame, err := reader.next()
			select {
			case p.ch <- frameResult{frame: frame, err: err}:
			case <-p.done:
				return
			}
			if err != nil {
				return
			}
		}
	})
	return p
}

// stop asks the pump to finish.  It does not unblock a pending Read; closing
// the response body does that.
func (p *framePump) stop() {
	p.once.Do(func() { close(p.done) })
}

// next waits for a frame, the idle timeout, or the context.
func (p *framePump) next(ctx context.Context, idle time.Duration) (sseFrame, error) {
	var timeout <-chan time.Time
	if idle > 0 {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case res := <-p.ch:
		return res.frame, res.err
	case <-timeout:
		return sseFrame{}, fmt.Errorf("%s: stream went idle for %s", clientName, idle)
	case <-ctx.Done():
		return sseFrame{}, ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// The response wire shape (CommonChunk).
// ---------------------------------------------------------------------------

type oaiPromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type oaiCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// oaiUsage mirrors CommonUsage: the neutral shape carries both the OpenAI
// spellings and the Anthropic ones, and a converter may fill either.
type oaiUsage struct {
	PromptTokens            int                   `json:"prompt_tokens"`
	CompletionTokens        int                   `json:"completion_tokens"`
	TotalTokens             int                   `json:"total_tokens"`
	InputTokens             int                   `json:"input_tokens"`
	OutputTokens            int                   `json:"output_tokens"`
	CacheReadInputTokens    *int                  `json:"cache_read_input_tokens"`
	CacheCreationInputToken int                   `json:"cache_creation_input_tokens"`
	PromptTokensDetails     *oaiPromptDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *oaiCompletionDetails `json:"completion_tokens_details"`
	InputTokensDetails      *oaiPromptDetails     `json:"input_tokens_details"`
	OutputTokensDetails     *oaiCompletionDetails `json:"output_tokens_details"`
}

func (u *oaiUsage) toCore() core.Usage {
	prompt := firstPositive(u.PromptTokens, u.InputTokens)
	completion := firstPositive(u.CompletionTokens, u.OutputTokens)
	total := firstPositive(u.TotalTokens, prompt+completion)
	out := core.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}
	switch {
	case u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0:
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	case u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0:
		out.CachedTokens = u.InputTokensDetails.CachedTokens
	case u.CacheReadInputTokens != nil && *u.CacheReadInputTokens > 0:
		out.CachedTokens = *u.CacheReadInputTokens
	}
	out.CachedTokensKnown = u.PromptTokensDetails != nil || u.InputTokensDetails != nil || u.CacheReadInputTokens != nil
	if u.CompletionTokensDetails != nil && u.CompletionTokensDetails.ReasoningTokens > 0 {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	} else if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// oaiDelta is both the streaming delta and the non-streaming message: the
// vendor uses the same shape for both, and the reader prefers Delta but falls
// back to Message so a converter that answers with a whole message still works.
type oaiDelta struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ToolCalls        []oaiToolCall   `json:"tool_calls"`
}

func (d *oaiDelta) textOf() string      { return flattenContent(d.Content) }
func (d *oaiDelta) reasoningOf() string { return firstNonEmpty(d.ReasoningContent, d.Reasoning) }

type oaiChoice struct {
	Index        int       `json:"index"`
	Delta        *oaiDelta `json:"delta"`
	Message      *oaiDelta `json:"message"`
	Text         string    `json:"text"`
	FinishReason *string   `json:"finish_reason"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type oaiChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
	Error   *oaiError   `json:"error"`
}

// flattenContent renders a content field that may be a string, an array of
// parts, or absent.
func flattenContent(raw json.RawMessage) string {
	trimmed := trimJSON(raw)
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err != nil {
			return ""
		}
		return s
	case '[':
		var parts []oaiContentPart
		if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
			return ""
		}
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	default:
		return ""
	}
}

// normalizeFinish maps a vendor finish reason onto the gateway's vocabulary.
func normalizeFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "stop", "end_turn", "eos", "stop_sequence":
		return "stop"
	case "length", "max_tokens", "max_output_tokens", "max_completion_tokens":
		return "length"
	case "tool_calls", "function_call", "tool_use":
		return "tool_calls"
	case "content_filter", "content_filtered", "refusal":
		return "content_filter"
	default:
		// An unrecognised reason still means the turn ended.
		return "stop"
	}
}

func isDonePayload(data string) bool {
	return strings.EqualFold(strings.TrimSpace(data), "[DONE]")
}

// ---------------------------------------------------------------------------
// chatStream.
// ---------------------------------------------------------------------------

type chatStream struct {
	client *Client
	body   io.ReadCloser
	pump   *framePump
	ctx    context.Context
	cancel context.CancelFunc
	idle   time.Duration

	accountID string
	account   *accountRecord

	closeOnce sync.Once
	closeErr  error

	rawDone   bool
	doneSent  bool
	usageSent bool
	succeeded bool
	failed    bool
	// emittedContent records that some delta has already gone out, so a
	// converter that answers with one whole message cannot make the answer
	// appear twice.
	emittedContent bool

	aggregate bool
	aggDone   bool

	pending []core.Event

	finish string

	// aggregate accumulation
	aggText   strings.Builder
	aggReason strings.Builder
	aggCalls  []core.ToolCallDelta
	aggUsage  *core.Usage
	toolOrder []int
	toolCalls map[int]*core.ToolCallDelta
}

// newChatStream wraps a live response body.
func newChatStream(c *Client, body io.ReadCloser, acct *accountRecord, ctx context.Context,
	cancel context.CancelFunc, aggregate bool) *chatStream {

	accountID := ""
	if acct != nil {
		accountID = acct.ID
	}
	return &chatStream{
		client:    c,
		body:      body,
		pump:      newFramePump(body, func(msg string) { c.noteError(msg) }),
		ctx:       ctx,
		cancel:    cancel,
		idle:      c.cfg.idleTimeout(),
		accountID: accountID,
		account:   acct,
		aggregate: aggregate,
		toolCalls: map[int]*core.ToolCallDelta{},
	}
}

// Recv implements core.Stream.  It is single-goroutine by contract.
func (s *chatStream) Recv() (core.Event, error) {
	if s.aggregate {
		return s.recvAggregated()
	}
	return s.recvRaw()
}

// Close is idempotent.  Closing the body is what unblocks the reader goroutine,
// so it happens before the pump is told to stop.
func (s *chatStream) Close() error {
	s.closeOnce.Do(func() {
		if s.body != nil {
			s.closeErr = s.body.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
		s.pump.stop()
		if s.accountID != "" {
			s.client.pool.release(s.accountID)
		}
	})
	return s.closeErr
}

// recvRaw streams the vendor's frames straight through.
func (s *chatStream) recvRaw() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.rawDone {
			return core.Event{}, io.EOF
		}
		frame, err := s.pump.next(s.ctx, s.idle)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// An upstream that closes without [DONE] has ended normally.
				s.rawDone = true
				if !s.doneSent {
					s.queue(s.endEvent())
				}
				continue
			}
			s.rawDone = true
			return core.Event{}, s.fail(err)
		}
		s.absorb(frame)
	}
}

// recvAggregated drains the upstream, then replays the answer as a small,
// ordered sequence so a caller that asked for a non-streaming response gets
// exactly one delta, one usage and one done.
func (s *chatStream) recvAggregated() (core.Event, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]
		return ev, nil
	}
	if s.aggDone {
		return core.Event{}, io.EOF
	}
	for {
		ev, err := s.recvRaw()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.aggDone = true
			return core.Event{}, err
		}
		switch ev.Type {
		case core.EventDelta:
			s.aggText.WriteString(ev.Delta)
			s.aggReason.WriteString(ev.Reasoning)
		case core.EventToolCall:
			// Nothing to do here: absorbChoice already folded this fragment
			// into s.toolCalls (mergeToolCallFrom -> mergeAt) and
			// buildAggregate replays that map.  Merging again would append
			// every argument slice twice.
		case core.EventUsage:
			s.aggUsage = ev.Usage
		case core.EventDone:
			s.finish = ev.Finish
		case core.EventError:
			s.aggDone = true
			return ev, nil
		}
	}
	s.aggDone = true
	s.buildAggregate()
	if len(s.pending) == 0 {
		return core.Event{}, io.EOF
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	return ev, nil
}

// buildAggregate queues the replayed answer.
func (s *chatStream) buildAggregate() {
	text := s.aggText.String()
	reason := s.aggReason.String()
	if text != "" || reason != "" {
		s.pending = append(s.pending, core.Event{
			Type:      core.EventDelta,
			Delta:     text,
			Reasoning: reason,
		})
	}
	for _, idx := range s.toolOrder {
		if call, ok := s.toolCalls[idx]; ok && call != nil {
			cp := *call
			s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: &cp})
		}
	}
	if s.aggUsage != nil {
		u := *s.aggUsage
		s.usageSent = true
		s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: &u})
	}
	s.doneSent = true
	s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: s.finish})
}

// queue appends an event for the next Recv.
func (s *chatStream) queue(ev core.Event) { s.pending = append(s.pending, ev) }

// endEvent marks the turn a success exactly once and returns the done event.
func (s *chatStream) endEvent() core.Event {
	if !s.succeeded {
		s.succeeded = true
		s.client.noteSuccess(s.account)
	}
	s.doneSent = true
	return core.Event{Type: core.EventDone, Finish: s.finish}
}

// fail records a mid-stream transport failure against the account and returns a
// typed error.  A cancelled context is not the vendor's fault and is passed
// through untouched.
func (s *chatStream) fail(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if !s.failed {
		s.failed = true
		s.client.noteFailure(s.account, kindTransport, describeError(err))
	}
	return core.Fail(clientName, s.accountID, core.FailureUpstream, 0,
		&upstreamError{Op: "chat stream", Msg: scrubAccount(describeError(err), s.account)})
}

// absorb parses one frame and queues whatever it carries.
func (s *chatStream) absorb(frame sseFrame) {
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return
	}
	if isDonePayload(data) {
		s.rawDone = true
		if !s.doneSent {
			s.queue(s.endEvent())
		}
		return
	}

	var chunk oaiChunk
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&chunk); err != nil {
		// Not a chat chunk: a keep-alive, a cost frame with no body we use, or
		// a comment the vendor invented.  Dropping it is correct.
		return
	}
	if chunk.Error != nil {
		msg := firstNonEmpty(chunk.Error.Message, asString(chunk.Error.Code), "upstream error")
		typ := firstNonEmpty(chunk.Error.Type, "Error")
		s.rawDone = true
		s.queue(core.Event{
			Type: core.EventError,
			Err:  fmt.Errorf("%s: %s: %s", clientName, scrubAccount(typ, s.account), scrubAccount(msg, s.account)),
		})
		return
	}

	for i := range chunk.Choices {
		s.absorbChoice(&chunk.Choices[i])
	}
	if chunk.Usage != nil && !s.usageSent {
		u := chunk.Usage.toCore()
		s.usageSent = true
		s.queue(core.Event{Type: core.EventUsage, Usage: &u})
	}
}

// absorbChoice converts one choice into events.
func (s *chatStream) absorbChoice(choice *oaiChoice) {
	var text, reasoning string
	var calls []oaiToolCall

	if choice.Delta != nil {
		text = choice.Delta.textOf()
		reasoning = choice.Delta.reasoningOf()
		calls = choice.Delta.ToolCalls
	}
	// Some converters answer a stream request with a single whole message.
	// Taking it only when nothing has been seen keeps the answer from being
	// emitted twice.
	if text == "" && reasoning == "" && len(calls) == 0 &&
		choice.Message != nil && !s.sawAnything() {
		text = choice.Message.textOf()
		reasoning = choice.Message.reasoningOf()
		calls = choice.Message.ToolCalls
	}
	if text == "" && choice.Text != "" {
		text = choice.Text
	}

	if text != "" || reasoning != "" {
		s.emittedContent = true
		s.queue(core.Event{Type: core.EventDelta, Delta: text, Reasoning: reasoning})
	}
	for i := range calls {
		frag := s.mergeToolCallFrom(calls[i])
		s.emittedContent = true
		s.queue(core.Event{Type: core.EventToolCall, ToolCall: &frag})
	}
	if choice.FinishReason != nil {
		if f := normalizeFinish(*choice.FinishReason); f != "" {
			s.finish = f
		}
	}
}

// sawAnything reports whether any content has already been emitted.
func (s *chatStream) sawAnything() bool {
	return s.aggText.Len() > 0 || s.aggReason.Len() > 0 || len(s.toolCalls) > 0 || s.emittedContent
}

// mergeToolCallFrom folds a fragment into the accumulating tool call and
// returns the RAW fragment with its index filled in.
//
// The accumulation is keyed by the index the vendor sends and arguments are
// concatenated, never replaced: the vendor streams them a slice at a time.
//
// It must not return the accumulated value: both consumers of Recv concatenate
// the fragments themselves (the streaming relay at internal/gateway/server.go:893
// passes Arguments straight through, and the non-streaming aggregator at
// internal/gateway/server.go:1000 does tc.Arguments += ...), so emitting the
// running total here would repeat every earlier argument slice.
func (s *chatStream) mergeToolCallFrom(frag oaiToolCall) core.ToolCallDelta {
	idx := 0
	if frag.Index != nil {
		idx = *frag.Index
	} else if len(s.toolOrder) > 0 {
		idx = s.toolOrder[len(s.toolOrder)-1]
	}
	s.mergeAt(idx, frag)
	return core.ToolCallDelta{
		Index:     idx,
		ID:        frag.ID,
		Name:      frag.Function.Name,
		Arguments: frag.Function.Arguments,
	}
}

func (s *chatStream) mergeAt(idx int, frag oaiToolCall) core.ToolCallDelta {
	merged, ok := s.toolCalls[idx]
	if !ok {
		merged = &core.ToolCallDelta{Index: idx}
		s.toolCalls[idx] = merged
		s.toolOrder = append(s.toolOrder, idx)
	}
	if frag.ID != "" {
		merged.ID = frag.ID
	}
	if frag.Function.Name != "" {
		merged.Name = frag.Function.Name
	}
	merged.Arguments += frag.Function.Arguments
	return *merged
}

// drain consumes a stream to completion.  It is used by TestAccount and by the
// tests; the gateway drives Recv directly.
func drain(s core.Stream) (text, reasoning string, calls []core.ToolCallDelta,
	usage *core.Usage, finish string, err error) {

	defer s.Close()
	byIndex := map[int]*core.ToolCallDelta{}
	var order []int
	for {
		ev, rerr := s.Recv()
		if errors.Is(rerr, io.EOF) {
			// Fragments were merged by index as they arrived; the caller wants
			// them in first-seen order.
			for _, idx := range order {
				if c, ok := byIndex[idx]; ok && c != nil {
					calls = append(calls, *c)
				}
			}
			return text, reasoning, calls, usage, finish, nil
		}
		if rerr != nil {
			return text, reasoning, calls, usage, finish, rerr
		}
		switch ev.Type {
		case core.EventDelta:
			text += ev.Delta
			reasoning += ev.Reasoning
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			idx := ev.ToolCall.Index
			cur, ok := byIndex[idx]
			if !ok {
				cp := *ev.ToolCall
				byIndex[idx] = &cp
				order = append(order, idx)
				continue
			}
			if ev.ToolCall.ID != "" {
				cur.ID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				cur.Name = ev.ToolCall.Name
			}
			cur.Arguments += ev.ToolCall.Arguments
		case core.EventUsage:
			if ev.Usage != nil {
				u := *ev.Usage
				usage = &u
			}
		case core.EventDone:
			finish = ev.Finish
		case core.EventError:
			return text, reasoning, calls, usage, finish, ev.Err
		}
	}
}
