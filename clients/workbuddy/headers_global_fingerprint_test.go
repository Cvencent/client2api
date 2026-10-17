package workbuddy

import (
	"net/http"
	"regexp"
	"testing"
)

// The international realm answers a request whose fingerprint still looks like
// the retired CN desktop with business code 11-128 ("request illegal"), the
// same body the gateway used to surface as an account-authentication failure.
// These tests pin the international fingerprint to the one the official
// WorkBuddy AI 5.8.4 desktop client sends (observed in its CloudProductManager
// request log and its bundled CLI):
//
//	user-agent: WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0
//	x-domain:   www.workbuddy.ai
//	x-product:  SaaS
//
// The CN realm keeps its own pair (5.7.7 / 2.137.1) and stays without
// X-Product / X-Agent-Purpose, exactly as the domestic desktop adapter does.
func TestWorkbuddyGlobalFingerprintMatchesOfficialClient(t *testing.T) {
	u := &Upstream{}
	a := &Auth{
		AccessToken: "at-global-000000000000",
		UID:         "7cd135a3-799e-4056-9c12-8c65240eea07",
		Realm:       realmGlobal,
		Domain:      "www.workbuddy.ai",
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	u.ChatHeaders(req, a, "", ChatMeta{})

	wantUA := "WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("X-IDE-Version"); got != "5.8.4" {
		t.Fatalf("X-IDE-Version = %q, want the 5.8.4 international desktop version", got)
	}
	if got := req.Header.Get("X-IDE-Name"); got != "WorkBuddy" {
		t.Fatalf("X-IDE-Name = %q, want WorkBuddy", got)
	}
	if got := req.Header.Get("X-IDE-Type"); got != "WorkBuddy" {
		t.Fatalf("X-IDE-Type = %q, want WorkBuddy", got)
	}
	if got := req.Header.Get("X-Agent-Purpose"); got != "conversation" {
		t.Fatalf("X-Agent-Purpose = %q, want conversation on the international adapter", got)
	}
	if got := req.Header.Get("X-Product"); got != "SaaS" {
		t.Fatalf("X-Product = %q, want SaaS on the international adapter", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Fatalf("X-Domain = %q, want www.workbuddy.ai", got)
	}
	if got := req.Header.Get("Origin"); got != "" {
		t.Fatalf("Origin = %q, want omitted on the international chat adapter", got)
	}
	if got := req.Header.Get("Referer"); got != "" {
		t.Fatalf("Referer = %q, want omitted on the international chat adapter", got)
	}
	if got := req.Header.Get("X-Machine-ID"); got != "" {
		t.Fatalf("X-Machine-ID = %q, want omitted on the international chat adapter", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != "" {
		t.Fatalf("X-Session-ID = %q, want omitted on the international chat adapter", got)
	}
	if got := req.Header.Get("X-Agent-Type"); got != "" {
		t.Fatalf("X-Agent-Type = %q, want omitted on the international chat adapter", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "en-US" {
		t.Fatalf("Accept-Language = %q, want en-US", got)
	}
	if got := req.Header.Get("X-Private-Data"); got != "true" {
		t.Fatalf("X-Private-Data = %q, want true on the international chat adapter", got)
	}
	acpConnectionID := req.Header.Get("acp-connection-id")
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(acpConnectionID) {
		t.Fatalf("acp-connection-id = %q, want a lowercase UUIDv4", acpConnectionID)
	}
	traceparent := req.Header.Get("traceparent")
	match := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`).FindStringSubmatch(traceparent)
	if match == nil {
		t.Fatalf("traceparent = %q, want W3C trace context", traceparent)
	}
	traceID, spanID := match[1], match[2]
	parentSpanID := req.Header.Get("X-B3-ParentSpanId")
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(parentSpanID) {
		t.Fatalf("X-B3-ParentSpanId = %q, want 16 lowercase hex chars", parentSpanID)
	}
	if got := req.Header.Get("X-B3-TraceId"); got != traceID {
		t.Fatalf("X-B3-TraceId = %q, want %q", got, traceID)
	}
	if got := req.Header.Get("X-B3-ParentSpanId"); got != parentSpanID {
		t.Fatalf("X-B3-ParentSpanId = %q, want %q", got, parentSpanID)
	}
	if got := req.Header.Get("X-B3-SpanId"); got != spanID {
		t.Fatalf("X-B3-SpanId = %q, want %q", got, spanID)
	}
	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Fatalf("X-B3-Sampled = %q, want 1", got)
	}
	if got := req.Header.Get("X-Trace-ID"); got != traceID {
		t.Fatalf("X-Trace-ID = %q, want %q", got, traceID)
	}
	if got := req.Header.Get("b3"); got != traceID+"-"+spanID+"-1-"+parentSpanID {
		t.Fatalf("b3 = %q, want trace-span-sampled-parent", got)
	}
}

// A global chat with no caller-supplied conversation id must still carry a
// non-empty X-Conversation-ID. The international channel verifier answers a
// request without it with business code 11-128 even when every other header
// matches the official client.
func TestWorkbuddyGlobalAlwaysSendsConversationID(t *testing.T) {
	u := &Upstream{}
	a := &Auth{
		AccessToken: "at-global-000000000000",
		UID:         "7cd135a3-799e-4056-9c12-8c65240eea07",
		Realm:       realmGlobal,
		Domain:      "www.workbuddy.ai",
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	u.ChatHeaders(req, a, "", ChatMeta{})

	conversationID := req.Header.Get("X-Conversation-ID")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(conversationID) {
		t.Fatalf("X-Conversation-ID = %q, want a 32-hex id for an international chat", conversationID)
	}
}
func TestWorkbuddyCNFingerprintStaysOnTheDomesticPair(t *testing.T) {
	u := &Upstream{}
	a := &Auth{
		AccessToken: "at-cn-000000000000",
		UID:         "uid-123",
		Realm:       realmCN,
		Domain:      "copilot.tencent.com",
	}
	req, err := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	u.ChatHeaders(req, a, "", ChatMeta{})

	wantUA := "WorkBuddy/5.7.7 WorkBuddy/5.7.7 CLI/2.137.1"
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if got := req.Header.Get("X-IDE-Version"); got != "5.7.7" {
		t.Fatalf("X-IDE-Version = %q, want the domestic 5.7.7 version", got)
	}
	if got := req.Header.Get("X-Agent-Purpose"); got != "" {
		t.Fatalf("X-Agent-Purpose = %q, want omitted on the domestic adapter", got)
	}
	if got := req.Header.Get("X-Product"); got != "" {
		t.Fatalf("X-Product = %q, want omitted on the domestic adapter", got)
	}
}

// An explicit client_version / cli_version still wins over both realm defaults:
// operators who pinned a version on purpose must not be silently moved.
func TestWorkbuddyExplicitVersionsOverrideRealmDefaults(t *testing.T) {
	u := &Upstream{ClientVersion: "9.9.9", CliVersion: "0.0.1"}
	cases := []struct {
		realm string
		want  string
	}{
		{realmGlobal, "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/0.0.1"},
		{realmCN, "WorkBuddy/9.9.9 WorkBuddy/9.9.9 CLI/0.0.1"},
	}
	for _, tc := range cases {
		a := &Auth{Realm: tc.realm}
		if got := u.userAgent(a); got != tc.want {
			t.Fatalf("realm %s: User-Agent = %q, want %q", tc.realm, got, tc.want)
		}
		if got := u.clientVersionFor(a); got != "9.9.9" {
			t.Fatalf("realm %s: client version = %q, want the configured 9.9.9", tc.realm, got)
		}
	}
}

// The billing/check-in endpoints carry the same version the chat path uses, so
// an international account must not announce the retired CN version there.
func TestWorkbuddyBillingUAFollowsRealm(t *testing.T) {
	u := &Upstream{}
	if got := u.billingUA(&Auth{Realm: realmGlobal}); got != "WorkBuddy/5.8.4" {
		t.Fatalf("global billing UA = %q, want WorkBuddy/5.8.4", got)
	}
	if got := u.billingUA(&Auth{Realm: realmCN}); got != "WorkBuddy/5.7.7" {
		t.Fatalf("cn billing UA = %q, want WorkBuddy/5.7.7", got)
	}
	if got := u.billingUA(nil); got != "WorkBuddy/5.7.7" {
		t.Fatalf("default billing UA = %q, want the domestic version", got)
	}
}
