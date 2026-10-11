package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"client2api/internal/core"
)

// chat.go turns the gateway's normalised request into the vendor's
// /samantha/chat/completion call and maps the reply stream back to core.Events.
//
// The vendor frames are JSON objects of the shape
//
//	{"event_type":"CMPL|FIN|ERR","event_data": <message-or-string>}
//
// where event_data is itself often a JSON string.  This module extracts the
// answer text from the common places the vendor puts it rather than pinning a
// single schema, because the message union grows with every product surface and
// a strict decoder would start dropping good answers the day the vendor adds a
// field.

// chatBody is the request the vendor expects.
type chatBody struct {
	Messages         []chatMessage  `json:"messages"`
	CompletionOption completionOpt  `json:"completion_option"`
}

type chatMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ContentType int   `json:"content_type,omitempty"`
}

type completionOpt struct {
	NeedCreateConversation bool `json:"need_create_conversation"`
	IsRegen                bool `json:"is_regen"`
	WithSuggest            bool `json:"with_suggest"`
}

// buildChatBody folds a core.ChatRequest into the vendor payload.
func (c *Client) buildChatBody(req *core.ChatRequest) ([]byte, error) {
	if req == nil || len(req.Messages) == 0 {
		return nil, core.ErrUnsupported
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		role := normaliseRole(m.Role)
		if role == "" {
			continue
		}
		text := messageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		msgs = append(msgs, chatMessage{Role: role, Content: text})
	}
	if len(msgs) == 0 {
		return nil, core.ErrUnsupported
	}
	body := chatBody{
		Messages: msgs,
		CompletionOption: completionOpt{
			NeedCreateConversation: req.ConversationID != "",
		},
	}
	return json.Marshal(body)
}

// normaliseRole maps the gateway's role names to the vendor's.
func normaliseRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system":
		return "system"
	case "assistant":
		return "assistant"
	case "user":
		return "user"
	case "tool":
		return "user"
	default:
		return ""
	}
}

// messageText flattens a message's text parts.
func messageText(m core.Message) string {
	if m.Content != "" && len(m.Parts) == 0 {
		return m.Content
	}
	var b strings.Builder
	if m.Content != "" {
		b.WriteString(m.Content)
	}
	for _, p := range m.Parts {
		if p.Type == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Chat implements core.Client.  It returns core.ErrNotConfigured when the pool
// is empty so the gateway maps that to HTTP 503.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil {
		return nil, core.ErrUnsupported
	}
	acc, err := c.pickAccount()
	if err != nil {
		return nil, err
	}
	if err := req.AcquireAccountSlot(acc.ID); err != nil {
		return nil, err
	}
	core.NoteServedBy(req, acc.ID)

	body, err := c.buildChatBody(req)
	if err != nil {
		req.ReleaseAccountSlot()
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	resp, err := c.up.rawPost(cctx, acc, pathChat, json.RawMessage(body))
	if err != nil {
		cancel()
		req.ReleaseAccountSlot()
		c.noteFailure(err)
		c.store.noteFailure(acc.ID, isAuthError(err), err)
		if isAuthError(err) {
			return nil, err
		}
		if isRateLimitError(err) {
			return nil, err
		}
		return nil, err
	}

	st := &chatStream{
		client:    c,
		req:       req,
		resp:      resp,
		reader:    newSSEReader(resp.Body),
		accountID: acc.ID,
		cancel:    cancel,
	}
	c.inFlight.Add(1)
	return st, nil
}

// pickAccount is the pool hook Chat uses.
func (c *Client) pickAccount() (*account, error) {
	return c.store.pick()
}

// chatStream is the core.Stream over one upstream SSE response.
type chatStream struct {
	client    *Client
	req       *core.ChatRequest
	resp      *http.Response
	reader    *sseReader
	accountID string
	cancel    context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	rawDone   bool
	succeeded bool
	pending   []core.Event
	finish    string
}

// Recv returns the next event, io.EOF exactly once at clean end.
func (s *chatStream) Recv() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.rawDone {
			return core.Event{}, io.EOF
		}
		frame, err := s.reader.next()
		if err != nil {
			s.rawDone = true
			if err == io.EOF {
				return s.endEvent(), nil
			}
			return core.Event{}, err
		}
		s.absorb(frame)
	}
}

// Close closes the body, releases the slot and cancels the context.  Idempotent.
func (s *chatStream) Close() error {
	s.closeOnce.Do(func() {
		if s.resp != nil && s.resp.Body != nil {
			s.closeErr = s.resp.Body.Close()
		}
		if s.client != nil {
			s.client.inFlight.Add(-1)
		}
		if s.req != nil {
			s.req.ReleaseAccountSlot()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	return s.closeErr
}

// endEvent emits the terminal event and teaches the pool the account worked.
func (s *chatStream) endEvent() core.Event {
	if !s.succeeded {
		s.succeeded = true
		if s.client != nil {
			s.client.store.noteSuccess(s.accountID)
		}
	}
	return core.Event{Type: core.EventDone, Finish: firstNonEmpty(s.finish, "stop")}
}

// vendorFrame is the envelope the vendor wraps each event in.
type vendorFrame struct {
	EventType string          `json:"event_type"`
	EventData json.RawMessage `json:"event_data"`
	Message   string          `json:"message"`
	Code      int             `json:"code"`
}

// absorb turns one SSE frame into zero or more queued events.
func (s *chatStream) absorb(frame sseFrame) {
	data := strings.TrimSpace(frame.Data)
	if data == "" || data == "[DONE]" {
		if data == "[DONE]" {
			s.rawDone = true
			s.pending = append(s.pending, s.endEvent())
		}
		return
	}
	var vf vendorFrame
	if err := json.Unmarshal([]byte(data), &vf); err != nil {
		// Not JSON: a keep-alive.  Dropping it keeps one stray line from
		// killing a good answer.
		return
	}
	switch strings.ToUpper(strings.TrimSpace(vf.EventType)) {
	case "CMPL":
		if text, reasoning := extractText(vf.EventData); text != "" || reasoning != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: text, Reasoning: reasoning})
		}
	case "FIN":
		s.rawDone = true
		s.pending = append(s.pending, s.endEvent())
	case "ERR":
		msg := firstNonEmpty(vf.Message, extractErrorMessage(vf.EventData), "the vendor reported an error on the stream")
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: fmt.Errorf("doubao: %s", core.Redact(msg))})
	default:
		// CMD and anything else the vendor adds: not answer text.
	}
}

// extractText pulls the visible answer and any reasoning out of one event_data
// payload.  It accepts an object or a JSON string and looks in the places the
// vendor's message union stores text.
func extractText(raw json.RawMessage) (text, reasoning string) {
	if len(raw) == 0 {
		return "", ""
	}
	// event_data is frequently a JSON string holding a JSON object.  Only
	// re-parse when the string looks like an object; a plain string is itself
	// the answer text.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if trimmed := strings.TrimSpace(asString); strings.HasPrefix(trimmed, "{") {
			raw = json.RawMessage(trimmed)
		} else {
			return asString, ""
		}
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		// A bare string with no object wrapper is itself the text.
		var plain string
		if json.Unmarshal(raw, &plain) == nil {
			return plain, ""
		}
		return "", ""
	}
	text = firstJSONString(obj, "content", "text", "answer", "delta", "reply")
	if text == "" {
		if msg, ok := obj["message"]; ok {
			text = stringField(msg, "content", "text")
		}
	}
	reasoning = firstJSONString(obj, "reasoning", "reasoning_content", "think", "thought")
	if reasoning == "" {
		if msg, ok := obj["message"]; ok {
			reasoning = stringField(msg, "reasoning_content", "reasoning")
		}
	}
	return text, reasoning
}

// firstJSONString reads the first present string field.
func firstJSONString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if raw, ok := obj[k]; ok {
			if s := decodeStringOrObject(raw); s != "" {
				return s
			}
		}
	}
	return ""
}

// stringField reads the first present string field of a nested object.
func stringField(raw json.RawMessage, keys ...string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return firstJSONString(obj, keys...)
}

// decodeStringOrObject reads a JSON string, or the text of an object that nests
// it under one of the common keys.
func decodeStringOrObject(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, k := range []string{"text", "content", "value"} {
		if inner, ok := obj[k]; ok {
			var v string
			if json.Unmarshal(inner, &v) == nil && v != "" {
				return v
			}
		}
	}
	return ""
}

var _ = errors.Is

