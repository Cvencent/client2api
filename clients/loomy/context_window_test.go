package loomy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// dashscopeRefusal is the exact body the backend behind DeepSeek-V4.1-Flash
// answers with when the assembled prompt is over its limit.  It carries no
// token count, so no context window can be learned from it, but it is still a
// request-shape refusal rather than a credential problem.
const dashscopeRefusal = `{"error":{"message":"内容太长了，请精简输入或开启新对话",` +
	`"type":"invalid_request_error","param":null,"code":400,` +
	`"metadata":{"provider_name":"alibaba-dashscope"}}}`

// A prompt-size refusal must become core.FailureContextWindow.  Landing on
// FailureUpstream made the gateway answer 502 for what the vendor had already
// called a 400, and made the pool rotate the request across every account --
// each of which refuses the same oversized body.
func TestFailureKindRecognisesTheDashScopePromptTooLongRefusal(t *testing.T) {
	err := &apiError{Status: http.StatusBadRequest, Message: dashscopeRefusal}
	if got := failureKind(err); got != core.FailureContextWindow {
		t.Fatalf("failureKind = %q, want %q", got, core.FailureContextWindow)
	}
}

// The same refusal can arrive with its Chinese escaped on the wire, because it
// is carried inside a JSON error body.
func TestFailureKindRecognisesEscapedChinesePromptTooLong(t *testing.T) {
	escaped := `{"error":{"message":"\u5185\u5bb9\u592a\u957f\u4e86\uff0c\u8bf7\u7cbe\u7b80\u8f93\u5165\u6216\u5f00\u542f\u65b0\u5bf9\u8bdd"}}`
	err := &apiError{Status: http.StatusBadRequest, Message: escaped}
	if got := failureKind(err); got != core.FailureContextWindow {
		t.Fatalf("failureKind = %q, want %q", got, core.FailureContextWindow)
	}
}

// The English spelling the same backend family uses when the account's locale
// is not Chinese.
func TestFailureKindRecognisesTheEnglishPromptTooLongRefusal(t *testing.T) {
	err := &apiError{Status: http.StatusBadRequest, Message: "prompt is too long"}
	if got := failureKind(err); got != core.FailureContextWindow {
		t.Fatalf("failureKind = %q, want %q", got, core.FailureContextWindow)
	}
}

// A refusal this module cannot name keeps its previous classification: only the
// prompt-size wording is recognised as a context-window refusal, so an unrelated
// 400 is not turned into a compaction trigger.
func TestFailureKindLeavesUnrelatedStatusesAlone(t *testing.T) {
	cases := []struct {
		name string
		err  *apiError
		want core.FailureKind
	}{
		{"500", &apiError{Status: http.StatusInternalServerError, Message: "boom"}, core.FailureUpstream},
		{"bad request without markers", &apiError{Status: http.StatusBadRequest, Message: "no such field"}, core.FailureUpstream},
		{"rate limited", &apiError{Status: http.StatusTooManyRequests}, core.FailureRateLimited},
		{"out of credit", &apiError{Status: http.StatusPaymentRequired}, core.FailureQuota},
	}
	for _, tc := range cases {
		if got := failureKind(tc.err); got != tc.want {
			t.Errorf("%s: failureKind = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Chat must hand the gateway a classified refusal with the vendor's status, and
// must not spend the account's 30s/1m/2m cooldown ladder on a request-shaped
// refusal.  Before this, ten healthy accounts in the live deployment had been
// walked to `abnormal` by one oversized conversation.
func TestChatDoesNotPenaliseAnAccountForAPromptTooLongRefusal(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	calls := 0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		return jsonResponse(http.StatusBadRequest, dashscopeRefusal)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: strings.Repeat("x", 100)}},
	})
	if err == nil {
		t.Fatal("Chat reported success on a prompt-too-long refusal")
	}

	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %T %v, want a *core.Failure so the gateway can preserve HTTP 400", err, err)
	}
	if f.Kind != core.FailureContextWindow {
		t.Fatalf("kind = %q, want %q", f.Kind, core.FailureContextWindow)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", f.Status, http.StatusBadRequest)
	}
	if f.Account != "loomy-1" {
		t.Fatalf("account = %q, want the credential that was refused", f.Account)
	}
	if !strings.Contains(err.Error(), "内容太长了") {
		t.Fatalf("error %q lost the vendor's own message", err)
	}

	// The refusal says nothing about the credential: retrying the same body on
	// the same account would just be refused again.
	if calls != 1 {
		t.Fatalf("chat calls = %d, want 1: a prompt-size refusal must not be retried", calls)
	}

	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished from the store")
	}
	if acc.failures != 0 || acc.cooldownStage != 0 {
		t.Fatalf("the refusal was charged to the account: %+v", acc)
	}
	if !acc.cooldownTill.IsZero() || acc.dead || acc.abnormal || acc.lowCredit {
		t.Fatalf("the refusal parked the account: %+v", acc)
	}
	if !acc.selectable(time.Now().UTC()) {
		t.Fatalf("the account left rotation after a request-shaped refusal: %+v", acc)
	}
}

// The wallet verdict and the transient ladder must keep working exactly as they
// did: only the prompt-size refusal is exempt.
func TestChatStillCoolsDownOnATransientRefusal(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	calls := 0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		return jsonResponse(http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat reported success on a 500")
	}
	if calls != 2 {
		t.Fatalf("chat calls = %d, want the same-account retry before the ladder", calls)
	}
	acc, _ := c.store.lookup("loomy-1")
	if acc.cooldownStage != 1 || acc.cooldownTill.IsZero() {
		t.Fatalf("a transient failure did not advance the cooldown ladder: %+v", acc)
	}
}
