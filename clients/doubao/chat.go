package doubao

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

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
	Messages         []chatMessage `json:"messages"`
	CompletionOption completionOpt `json:"completion_option"`
	ConversationID   string        `json:"conversation_id,omitempty"`
	// LocalConversationID is what tells the vendor to create a conversation.
	// need_create_conversation is derived from it, exactly as the client does.
	LocalConversationID string `json:"local_conversation_id,omitempty"`
	LocalMessageID      string `json:"local_message_id,omitempty"`
	MessageID           string `json:"message_id,omitempty"`
	ReplyID             string `json:"reply_id,omitempty"`
	SectionID           string `json:"section_id,omitempty"`
}

type chatMessage struct {
	Role        string `json:"role"`
	Content     string `json:"content"`
	ContentType int    `json:"content_type,omitempty"`

	LocalMessageID string `json:"local_message_id,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	ReplyID        string `json:"reply_id,omitempty"`
	SectionID      string `json:"section_id,omitempty"`

	// Ext carries the model routing the client puts on the message.
	Ext *messageExt `json:"ext,omitempty"`
}

type completionOpt struct {
	NeedCreateConversation bool `json:"need_create_conversation"`
	IsRegen                bool `json:"is_regen"`
	WithSuggest            bool `json:"with_suggest"`
}

// messageExt is the routing metadata the client attaches to the message it
// sends.  mode_id + model_item_key + reply_mode select the model; the rest is
// what the client always includes alongside them.
type messageExt struct {
	ReplyMode       string `json:"reply_mode,omitempty"`
	ModeID          string `json:"mode_id,omitempty"`
	ModelItemKey    string `json:"model_item_key,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	AgentTaskParam  string `json:"agent_task_param,omitempty"`
	LocalMessageID  string `json:"local_message_id,omitempty"`
}

// buildChatBody folds a core.ChatRequest and the resolved model route into the
// vendor payload the desktop client sends.
//
// Two details are easy to get wrong and both matter:
//
//   - `content` is not plain text.  The client sends a JSON string such as
//     {"type":"text","text":"hi"}; a bare string is rejected upstream with an
//     "invalid character ... looking for beginning of value" business error.
//   - the model routing rides on the message's `ext`, not the top level.
//
// The identifiers are generated the same way the client does: a fresh
// conversation on the first turn, then the vendor's conversation id afterwards.
func (c *Client) buildChatBody(req *core.ChatRequest, route modelRoute) ([]byte, error) {
	if req == nil || len(req.Messages) == 0 {
		return nil, core.ErrUnsupported
	}
	now := time.Now().UnixMilli()
	localConv := ""
	conversation := req.ConversationID
	if conversation == "" {
		localConv = conversationID(now)
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		role := normaliseRole(m.Role)
		if role == "" {
			continue
		}
		text := messageText(m)
		if strings.TrimSpace(text) == "" {
			continue
		}
		msg := chatMessage{Role: role, Content: encodeContent(text), ContentType: contentTypeText}
		// The routing ext belongs on the message the user just sent; earlier
		// turns are replayed without it so the conversation keeps one mode.
		if i == len(req.Messages)-1 && role == "user" {
			mid := strconv.FormatInt(now, 10)
			msg.LocalMessageID = mid
			msg.MessageID = mid
			ext := &messageExt{
				ReplyMode:       route.ReplyMode,
				ModeID:          route.ModeID,
				ModelItemKey:    route.ModelItemKey,
				ReasoningEffort: route.ReasoningEffort,
				LocalMessageID:  mid,
			}
			if route.AgentMode != "" {
				ext.AgentTaskParam = `{"runtime_type":1}`
			}
			msg.Ext = ext
		}
		msgs = append(msgs, msg)
	}
	if len(msgs) == 0 {
		return nil, core.ErrUnsupported
	}
	body := chatBody{
		Messages:            msgs,
		ConversationID:      firstNonEmpty(conversation, "0"),
		LocalConversationID: localConv,
		LocalMessageID:      strconv.FormatInt(now, 10),
		MessageID:           strconv.FormatInt(now, 10),
		CompletionOption: completionOpt{
			NeedCreateConversation: localConv != "",
		},
	}
	return json.Marshal(body)
}

// contentJSON is the message body the vendor decodes after its own JSON pass.
type contentJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const contentTypeText = 2001

// encodeContent renders text as the nested JSON string the vendor expects.
func encodeContent(text string) string {
	raw, err := json.Marshal(contentJSON{Type: "text", Text: text})
	if err != nil {
		return text
	}
	return string(raw)
}

// conversationID is the local conversation id the client mints: "local_" plus
// a millisecond timestamp.
func conversationID(ms int64) string {
	return "local_" + strconv.FormatInt(ms, 10)
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

	body, err := c.buildChatBody(req, c.routeFor(acc, req.Model))
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
		c.store.noteFailure(acc.ID, isAuthError(err), c.failureCooldown(isAuthError(err)), err)
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

// vendorFrame is the envelope the vendor wraps each event in.  event_type is a
// string name on some builds ("CMPL") and a number on others (2001), so it is
// kept raw and normalised by eventKind.
type vendorFrame struct {
	EventType json.RawMessage `json:"event_type"`
	EventData json.RawMessage `json:"event_data"`
	Message   string          `json:"message"`
	Code      int             `json:"code"`
}

// The vendor's numeric EventType values, read out of the web bundle.
const (
	eventNumCMPL      = 2001
	eventNumFIN       = 2003
	eventNumERR       = 2005
	eventNumVUICMPL   = 2006
	eventNumOAPICMPL  = 2007
	eventNumBlocklist = 2022
)

// eventKind normalises event_type to one of CMPL, FIN, ERR, or "" for anything
// that is not answer text.  Both the string and numeric encodings are accepted.
func eventKind(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			switch strings.ToUpper(strings.TrimSpace(s)) {
			case "CMPL", "VUI_CMPL", "OAPI_CMPL":
				return "CMPL"
			case "FIN":
				return "FIN"
			case "ERR", "BLOCKLIST":
				return "ERR"
			}
		}
		return ""
	}
	switch int(parseDecimal(raw)) {
	case eventNumCMPL, eventNumVUICMPL, eventNumOAPICMPL:
		return "CMPL"
	case eventNumFIN:
		return "FIN"
	case eventNumERR, eventNumBlocklist:
		return "ERR"
	}
	return ""
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
	switch eventKind(vf.EventType) {
	case "CMPL":
		// The vendor also delivers business errors inside a CMPL frame's
		// event_data (a non-zero code with a message and no answer text), so
		// that case is surfaced as an error rather than dropped as empty text.
		text, reasoning := extractText(vf.EventData)
		if text == "" && reasoning == "" {
			if msg, code := inlineError(vf.EventData); msg != "" {
				s.pending = append(s.pending, core.Event{Type: core.EventError, Err: fmt.Errorf("doubao: %s (code %s)", core.Redact(msg), code)})
				return
			}
		}
		if text != "" || reasoning != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: text, Reasoning: reasoning})
		}
	case "FIN":
		s.rawDone = true
		s.pending = append(s.pending, s.endEvent())
	case "ERR":
		inlineMsg, inlineCode := inlineError(vf.EventData)
		msg := firstNonEmpty(vf.Message, inlineMsg, extractErrorMessage(vf.EventData), "the vendor reported an error on the stream")
		if inlineCode != "" {
			msg = msg + " (code " + inlineCode + ")"
		}
		s.pending = append(s.pending, core.Event{Type: core.EventError, Err: fmt.Errorf("doubao: %s", core.Redact(msg))})
	default:
		// CMD and anything else the vendor adds: not answer text.
	}
}

// inlineError pulls the business error out of a CMPL payload: a non-zero code
// with a message and no answer text.
func inlineError(raw json.RawMessage) (message, code string) {
	if len(raw) == 0 {
		return "", ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		trimmed := strings.TrimSpace(asString)
		if !strings.HasPrefix(trimmed, "{") {
			return "", ""
		}
		raw = json.RawMessage(trimmed)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", ""
	}
	if n := parseDecimal(obj["code"]); n == 0 {
		return "", ""
	}
	msg := firstJSONString(obj, "message", "msg")
	if msg == "" {
		if detail, ok := obj["error_detail"]; ok {
			msg = nestedStringField(detail, "message", "msg")
		}
	}
	if msg == "" {
		return "", ""
	}
	return msg, rawScalar(obj["code"])
}

// nestedStringField reads a string field from a nested JSON object that may
// itself arrive as a JSON string (the vendor double-encodes it).
func nestedStringField(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		trimmed := strings.TrimSpace(asString)
		if !strings.HasPrefix(trimmed, "{") {
			return ""
		}
		raw = json.RawMessage(trimmed)
	}
	return stringField(raw, keys...)
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
