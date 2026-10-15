package workbuddy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
)

// The main conversation adapter is the request shape the official WorkBuddy
// desktop/CLI uses for ordinary chat. These headers are load-bearing: the
// upstream uses them for conversation grouping, agent routing and tracing.
func TestWorkbuddyChatHeadersMatchDesktopConversationAdapter(t *testing.T) {
	u := &Upstream{}
	a := &Auth{
		AccessToken:  "at-cn-000000000000",
		UID:          "uid-123",
		EnterpriseID: "ent-456",
		Realm:        "cn",
		Domain:       "copilot.tencent.com",
	}
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	u.ChatHeaders(req, a, "", ChatMeta{
		ConversationID:        "conv-abc",
		ConversationRequestID: "0123456789abcdef0123456789abcdef",
	})

	if got := req.Header.Get("X-Agent-Intent"); got != "craft" {
		t.Fatalf("X-Agent-Intent = %q, want craft", got)
	}
	if got := req.Header.Get("X-Agent-Type"); got != "main" {
		t.Fatalf("X-Agent-Type = %q, want main", got)
	}
	if got := req.Header.Get("X-Private-Data"); got != "false" {
		t.Fatalf("X-Private-Data = %q, want false for the normal model-optimization path", got)
	}
	if got := req.Header.Get("X-Agent-Purpose"); got != "" {
		t.Fatalf("X-Agent-Purpose = %q, want omitted on a plain main chat", got)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != "" {
		t.Fatalf("X-Root-Request-ID = %q, want omitted without a rootRequestId", got)
	}

	messageID := req.Header.Get("X-Conversation-Message-ID")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(messageID) {
		t.Fatalf("X-Conversation-Message-ID = %q, want 32 lowercase hex chars", messageID)
	}
	if got := req.Header.Get("X-Request-ID"); got != messageID {
		t.Fatalf("X-Request-ID = %q, want the message id %q", got, messageID)
	}

	traceID := req.Header.Get("X-Trace-ID")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(traceID) {
		t.Fatalf("X-Trace-ID = %q, want a 32-hex trace id", traceID)
	}
	if got := req.Header.Get("X-B3-TraceId"); got != traceID {
		t.Fatalf("X-B3-TraceId = %q, want %q", got, traceID)
	}
	spanID := req.Header.Get("X-B3-SpanId")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(spanID) {
		t.Fatalf("X-B3-SpanId = %q, want 16 lowercase hex chars", spanID)
	}
	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Fatalf("X-B3-Sampled = %q, want 1", got)
	}
	if got := req.Header.Get("b3"); got != traceID+"-"+spanID+"-1" {
		t.Fatalf("b3 = %q, want %q", got, traceID+"-"+spanID+"-1")
	}
	if got := req.Header.Get("traceparent"); got != "" {
		t.Fatalf("traceparent = %q, want omitted on the main conversation adapter", got)
	}
}

func TestWorkbuddyChatHeadersIncludeIdentityHeadersWhenKnown(t *testing.T) {
	u := &Upstream{}
	a := &Auth{
		AccessToken:  "at-cn-000000000000",
		UID:          "uid-123",
		EnterpriseID: "ent-456",
		AuthMethod:   "external",
		IdSource:     "sso",
		Realm:        "cn",
		Domain:       "copilot.tencent.com",
	}
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	u.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Auth-Method"); got != "external" {
		t.Fatalf("X-Auth-Method = %q, want external", got)
	}
	if got := req.Header.Get("X-Id-Source"); got != "sso" {
		t.Fatalf("X-Id-Source = %q, want sso", got)
	}

	raw := req.Header.Get("X-Userinfo")
	if raw == "" {
		t.Fatal("X-Userinfo is missing")
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("X-Userinfo is not base64: %v", err)
	}
	var info map[string]string
	if err := json.Unmarshal(decoded, &info); err != nil {
		t.Fatalf("X-Userinfo is not a JSON object: %v", err)
	}
	if info["uin"] != "uid-123" || info["owner_uin"] != "ent-456" || info["id_source"] != "sso" || info["token_source"] != "external" {
		t.Fatalf("X-Userinfo decoded = %#v", info)
	}
}
