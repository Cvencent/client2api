package doubao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestSSEReaderToleratesMissingSpaceAndNoTrailingBlankLine pins the two
// upstream quirks that silently drop answers on a strict reader.
func TestSSEReaderToleratesMissingSpaceAndNoTrailingBlankLine(t *testing.T) {
	input := "data:{\"event_type\":\"CMPL\",\"event_data\":\"hi\"}\n\ndata: {\"event_type\":\"FIN\"}"
	r := newSSEReader(strings.NewReader(input))
	f1, err := r.next()
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if !strings.Contains(f1.Data, "CMPL") {
		t.Fatalf("first frame data = %q", f1.Data)
	}
	f2, err := r.next()
	if err != nil {
		t.Fatalf("second frame (no trailing blank line): %v", err)
	}
	if !strings.Contains(f2.Data, "FIN") {
		t.Fatalf("second frame data = %q", f2.Data)
	}
	if _, err := r.next(); err != io.EOF {
		t.Fatalf("end = %v, want io.EOF", err)
	}
}

// TestBuildChatBodyMapsRolesAndOptions pins the request body the vendor gets.
func TestBuildChatBodyMapsRolesAndOptions(t *testing.T) {
	c := &Client{cfg: config{}}
	req := &core.ChatRequest{
		ConversationID: "conv-1",
		Messages: []core.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hello"},
		},
	}
	raw, err := c.buildChatBody(req, defaultRoute(config{}))
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body chatBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("got %d messages, want 2", len(body.Messages))
	}
	if body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("roles = %+v", body.Messages)
	}
	// A follow-up turn carries the existing conversation id and does NOT ask to
	// create one; that is the client's own rule (need_create tracks a fresh
	// local_conversation_id).
	if body.ConversationID != "conv-1" {
		t.Fatalf("conversation_id = %q, want conv-1", body.ConversationID)
	}
	if body.CompletionOption.NeedCreateConversation {
		t.Fatalf("need_create_conversation = true for an existing conversation, want false")
	}
	// The routing trio rides on the last user message's ext.
	last := body.Messages[len(body.Messages)-1]
	if last.Ext == nil || last.Ext.ModeID != defaultModeID || last.Ext.ModelItemKey != defaultModelItemKey {
		t.Fatalf("last message ext = %+v, want mode %s model %s", last.Ext, defaultModeID, defaultModelItemKey)
	}
	if strings.TrimSpace(last.Content) == "" || last.Content[0] != '{' {
		t.Fatalf("content = %q, want a JSON object string", last.Content)
	}
}

// TestBuildChatBodyStartsANewConversation pins the first-turn shape: no vendor
// conversation id yet, so the body carries a local_conversation_id and asks the
// server to create the conversation.
func TestBuildChatBodyStartsANewConversation(t *testing.T) {
	c := &Client{cfg: config{}}
	req := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}}
	raw, err := c.buildChatBody(req, defaultRoute(config{}))
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body chatBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.CompletionOption.NeedCreateConversation {
		t.Fatalf("need_create_conversation = false on the first turn, want true")
	}
	if !strings.HasPrefix(body.LocalConversationID, "local_") {
		t.Fatalf("local_conversation_id = %q, want local_ prefix", body.LocalConversationID)
	}
}

// TestExtractTextFindsTextThroughTheUnion pins the tolerant extractor: the
// answer text sits behind different keys across builds.
func TestExtractTextFindsTextThroughTheUnion(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"plain"`, "plain"},
		{`{"content":"a"}`, "a"},
		{`{"message":{"content":"b"}}`, "b"},
		{`{"text":{"value":"c"}}`, "c"},
	}
	for _, tc := range cases {
		got, _ := extractText(json.RawMessage(tc.raw))
		if got != tc.want {
			t.Errorf("extractText(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestChatStreamMapsFramesToEvents pins the CMPL/FIN mapping end to end over a
// scripted SSE body, and that a clean end emits exactly one done.
func TestChatStreamMapsFramesToEvents(t *testing.T) {
	body := strings.Join([]string{
		"data: {\"event_type\":\"CMPL\",\"event_data\":\"{\\\"content\\\":\\\"Hel\\\"}\"}",
		"",
		"data:{\"event_type\":\"CMPL\",\"event_data\":\"{\\\"content\\\":\\\"lo\\\"}\"}",
		"",
		"data: {\"event_type\":\"FIN\"}",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var text string
	sawDone := 0
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text += ev.Delta
		case core.EventDone:
			sawDone++
		}
	}
	if text != "Hello" {
		t.Fatalf("assembled text = %q, want Hello", text)
	}
	if sawDone != 1 {
		t.Fatalf("saw %d done events, want exactly 1", sawDone)
	}
}

// TestChatWithoutAccountIsErrNotConfigured pins the documented seam.
func TestChatWithoutAccountIsErrNotConfigured(t *testing.T) {
	c := newWebClient(t, "http://127.0.0.1:1")
	_, err := c.Chat(context.Background(), &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != core.ErrNotConfigured {
		t.Fatalf("Chat with no account = %v, want ErrNotConfigured", err)
	}
}
