package tabbit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Transport: every upstream call this module makes goes through here.
// ---------------------------------------------------------------------------

func joinURL(base, path string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("no sidecar base_url")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid sidecar base_url %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid sidecar base_url %q: need scheme and host", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (c *Client) newRequest(ctx context.Context, loc location, method, path string, body io.Reader) (*http.Request, error) {
	target, err := joinURL(loc.baseURL, path)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+loc.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "client2api/tabbit")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return req, nil
}

func (c *Client) doGet(ctx context.Context, loc location, path string) (*http.Response, error) {
	req, err := c.newRequest(ctx, loc, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	return c.httpClient().Do(req)
}

// openStream performs the upstream chat call and re-frames the answer into a
// core.Stream.  The sidecar is always asked to stream, because the whole point
// of this module is the SSE translation; a non-streaming caller is buffered by
// the gateway.
func (c *Client) openStream(ctx context.Context, loc location, body []byte) (core.Stream, error) {
	base := ctx
	var cancelTimeout context.CancelFunc
	if d := c.cfg.Timeouts.requestBudget(); d > 0 {
		base, cancelTimeout = context.WithTimeout(ctx, d)
	}
	rctx, cancel := context.WithCancel(base)
	cleanup := func() {
		cancel()
		if cancelTimeout != nil {
			cancelTimeout()
		}
	}

	req, err := c.newRequest(rctx, loc, http.MethodPost, c.cfg.ChatPath, bytes.NewReader(body))
	if err != nil {
		cleanup()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		cleanup()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("%w: sidecar %s is not reachable: %v", core.ErrNotConfigured, loc.baseURL, err)
	}

	if resp.StatusCode != http.StatusOK {
		raw := readLimited(resp.Body, 8<<10)
		resp.Body.Close()
		cleanup()
		msg := upstreamErrorMessage(raw)
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			err := fmt.Errorf("%w: sidecar %s rejected the local api key (HTTP %d): %s",
				core.ErrNotConfigured, loc.baseURL, resp.StatusCode, msg)
			return nil, core.Fail("tabbit", loc.baseURL, core.FailureAuth, resp.StatusCode, err)
		case http.StatusTooManyRequests:
			return nil, core.Fail("tabbit", loc.baseURL, core.FailureRateLimited, resp.StatusCode,
				fmt.Errorf("sidecar %s is rate limiting (HTTP 429): %s", loc.baseURL, msg))
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return nil, core.Fail("tabbit", loc.baseURL, core.FailureUpstream, resp.StatusCode,
				fmt.Errorf("sidecar %s returned HTTP %d for %s: %s",
					loc.baseURL, resp.StatusCode, c.cfg.ChatPath, msg))
		default:
			return nil, core.Fail("tabbit", loc.baseURL, core.FailureOther, resp.StatusCode,
				fmt.Errorf("sidecar %s returned HTTP %d for %s: %s",
					loc.baseURL, resp.StatusCode, c.cfg.ChatPath, msg))
		}
	}

	// Verify the frame shape rather than assuming it: if the sidecar answered
	// with a whole JSON completion (or an error envelope) instead of SSE,
	// re-frame that instead of failing.
	if !isEventStream(resp.Header.Get("Content-Type")) {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		cleanup()
		if err != nil {
			return nil, fmt.Errorf("reading sidecar response: %w", err)
		}
		return streamFromJSON(raw, c.deps.Log)
	}

	return newChatStream(resp.Body, cancel, c.deps.Log), nil
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// upstreamErrorMessage pulls a human message out of an upstream error body.
func upstreamErrorMessage(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "(empty body)"
	}
	var env struct {
		Error *oaiError `json:"error"`
	}
	if json.Unmarshal(trimmed, &env) == nil && env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
		return env.Error.Message
	}
	var generic struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(trimmed, &generic) == nil && strings.TrimSpace(generic.Message) != "" {
		return generic.Message
	}
	s := string(trimmed)
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// The re-framing stream
// ---------------------------------------------------------------------------

type chatStream struct {
	body   io.ReadCloser
	reader *sseReader
	cancel context.CancelFunc
	log    func(format string, args ...any)

	pending   []core.Event
	finish    string
	usageSent bool
	doneSent  bool
	eofSent   bool
	closed    bool
	stopRead  bool // a terminal frame was seen; drain pending then end
}

func newChatStream(body io.ReadCloser, cancel context.CancelFunc, log func(string, ...any)) *chatStream {
	return &chatStream{
		body:   body,
		reader: newSSEReader(body),
		cancel: cancel,
		log:    log,
	}
}

// Recv implements core.Stream.  It emits at most one usage and one done event
// and returns io.EOF exactly once, at the clean end.
func (s *chatStream) Recv() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.eofSent || s.closed {
			s.eofSent = true
			return core.Event{}, io.EOF
		}
		if s.stopRead || s.reader == nil {
			return s.end()
		}
		fr, err := s.reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return s.end()
			}
			s.eofSent = true
			return core.Event{}, fmt.Errorf("reading sidecar stream: %w", err)
		}
		s.consume(fr)
	}
}

// end emits the single done event and then io.EOF.
func (s *chatStream) end() (core.Event, error) {
	if !s.doneSent {
		s.doneSent = true
		reason := s.finish
		if reason == "" {
			reason = "stop"
		}
		return core.Event{Type: core.EventDone, Finish: reason}, nil
	}
	s.eofSent = true
	return core.Event{}, io.EOF
}

// Close implements core.Stream and is idempotent.
func (s *chatStream) Close() error {
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

func (s *chatStream) consume(fr sseFrame) {
	data := strings.TrimSpace(fr.Data)
	if data == "" {
		if strings.EqualFold(fr.Event, "error") {
			s.terminalError("sidecar sent an empty error event")
		}
		return
	}
	if isDonePayload(data) {
		s.stopRead = true
		return
	}

	var chunk oaiChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		if strings.EqualFold(fr.Event, "error") {
			s.terminalError(data)
			return
		}
		s.logf("ignoring unparseable sidecar SSE frame (%d bytes): %v", len(data), err)
		return
	}
	if chunk.Error != nil && (strings.TrimSpace(chunk.Error.Message) != "" || chunk.Error.Type != "") {
		s.terminalError(chunk.Error.Message)
		return
	}

	for _, ch := range chunk.Choices {
		d := ch.Delta
		if d == nil {
			d = ch.Message
		}
		if d != nil {
			if txt := flattenContent(d.Content); txt != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: txt})
			}
			if r := firstNonEmpty(d.ReasoningContent, d.Reasoning); r != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: r})
			}
			for _, tc := range d.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				s.pending = append(s.pending, core.Event{
					Type: core.EventToolCall,
					ToolCall: &core.ToolCallDelta{
						Index:     idx,
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		if ch.Text != "" { // legacy text-completion shape
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: ch.Text})
		}
		if ch.FinishReason != nil && strings.TrimSpace(*ch.FinishReason) != "" {
			s.finish = normalizeFinish(*ch.FinishReason)
		}
	}
	if chunk.Usage != nil {
		s.pushUsage(chunk.Usage)
	}
}

func (s *chatStream) pushUsage(u *oaiUsage) {
	if s.usageSent || u == nil {
		return
	}
	s.usageSent = true
	usage := u.toCore()
	s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: &usage})
}

// terminalError emits one error event and ends the stream after it.
func (s *chatStream) terminalError(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "sidecar reported an error"
	}
	s.pending = append(s.pending, core.Event{Type: core.EventError, Err: errors.New(msg)})
	s.stopRead = true
	s.doneSent = true // no done event after an error event
}

func (s *chatStream) logf(format string, args ...any) {
	if s.log != nil {
		s.log(format, args...)
	}
}

// streamFromJSON re-frames a whole (non-SSE) JSON completion.
func streamFromJSON(raw []byte, log func(string, ...any)) (core.Stream, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("sidecar returned an empty response")
	}
	var chunk oaiChunk
	if err := json.Unmarshal(trimmed, &chunk); err != nil {
		return nil, fmt.Errorf("sidecar returned a non-JSON response: %s", upstreamErrorMessage(trimmed))
	}
	if chunk.Error != nil {
		msg := strings.TrimSpace(chunk.Error.Message)
		if msg == "" {
			msg = "sidecar reported an error"
		}
		return nil, errors.New(msg)
	}
	s := &chatStream{log: log, stopRead: true}
	for _, ch := range chunk.Choices {
		d := ch.Message
		if d == nil {
			d = ch.Delta
		}
		if d != nil {
			if txt := flattenContent(d.Content); txt != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: txt})
			}
			if r := firstNonEmpty(d.ReasoningContent, d.Reasoning); r != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: r})
			}
			for _, tc := range d.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				s.pending = append(s.pending, core.Event{
					Type: core.EventToolCall,
					ToolCall: &core.ToolCallDelta{
						Index:     idx,
						ID:        tc.ID,
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		if ch.Text != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: ch.Text})
		}
		if ch.FinishReason != nil {
			s.finish = normalizeFinish(*ch.FinishReason)
		}
	}
	if chunk.Usage != nil {
		s.pushUsage(chunk.Usage)
	}
	return s, nil
}
