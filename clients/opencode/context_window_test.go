package opencode

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// zenContextRefusal is the exact 400 OpenCode Zen answers with when the
// assembled prompt is over the model's serving window.  It names no token count
// at all, so no window can be learned from it -- but it is a request-shape
// refusal, not a credential problem, and it is the sentence that must reach the
// caller as context_window_exceeded so its own compaction can fire.
const zenContextRefusal = `{"type":"error","error":{"type":"InvalidRequestError",` +
	`"message":"Your request resulted in more tokens than this model's maximum ` +
	`context length allows. Please reduce the length of the messages or run compaction."}}`

// A prompt-size refusal must become core.FailureContextWindow.  Landing on
// FailureOther made the gateway answer invalid_request, which is not a code any
// client compacts on, so Codex re-sent the same oversized body forever.
func TestClassifyRecognisesTheZenContextRefusal(t *testing.T) {
	if got := classify(http.StatusBadRequest, []byte(zenContextRefusal)).kind; got != kindContext {
		t.Fatalf("kind = %q, want %q", got, kindContext)
	}
}

// The wordings other vendors use for the same verdict, so the recogniser is not
// pinned to one sentence.
func TestClassifyRecognisesContextRefusalWordings(t *testing.T) {
	cases := []string{
		"maximum context length exceeded",
		"prompt is too long",
		"input is too long for this model",
		"this model's maximum context length allows fewer tokens",
		"reduce the length of the messages",
		"the context window is 200000 tokens",
		"too many tokens",
	}
	for _, msg := range cases {
		body := []byte(`{"error":{"message":"` + msg + `"}}`)
		if got := classify(http.StatusBadRequest, body).kind; got != kindContext {
			t.Errorf("classify(%q) = %q, want %q", msg, got, kindContext)
		}
	}
}

// The recogniser only sees wording, so it must stay conservative: an unrelated
// 400 that merely mentions a context field, or a refusal that is really about
// the account, keeps its previous verdict.
func TestClassifyLeavesUnrelatedRefusalsAlone(t *testing.T) {
	cases := []struct {
		body string
		want failureKind
	}{
		{`{"error":{"message":"no such field: context"}}`, kindClient},
		{`{"error":{"message":"Invalid API key provided"}}`, kindAuth},
		{`{"error":{"message":"Rate limit exceeded"}}`, kindRate},
		{`{"error":{"message":"Insufficient credit balance"}}`, kindQuota},
		{`{"error":{"message":"Model claude-9 is not supported"}}`, kindModel},
	}
	for _, tc := range cases {
		if got := classify(http.StatusBadRequest, []byte(tc.body)).kind; got != tc.want {
			t.Errorf("classify(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// The mapping that makes the gateway answer the code clients compact on.
func TestCoreKindForMapsContextToTheWindowVerdict(t *testing.T) {
	if got := coreKindFor(kindContext); got != core.FailureContextWindow {
		t.Fatalf("coreKindFor(kindContext) = %q, want %q", got, core.FailureContextWindow)
	}
}

// classifyHTTP must preserve the vendor's own 400 and must not charge the
// refusal to the account: an oversized body says nothing about the credential,
// and charging it here walks a healthy pool to abnormal on one long conversation.
func TestClassifyHTTPContextRefusalDoesNotPenaliseTheAccount(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.byID("opencode:a")

	err := c.classifyHTTP("chat", acct, http.StatusBadRequest, []byte(zenContextRefusal))
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %T %v, want a *core.Failure", err, err)
	}
	if f.Kind != core.FailureContextWindow {
		t.Fatalf("kind = %q, want %q", f.Kind, core.FailureContextWindow)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want the vendor's own %d", f.Status, http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "run compaction") {
		t.Fatalf("error %q lost the vendor's own sentence", err)
	}
	after, _ := c.pool.byID("opencode:a")
	if !after.Enabled || after.CooldownUntil != "" || after.ErrCount != 0 {
		t.Fatalf("a context-window refusal recorded state on the account: %+v", after)
	}
}

// One upstream call.  Retrying the identical oversized body on the same account
// is guaranteed to be refused again, and rotating accounts is worse: every
// account refuses the same bytes.
func TestChatDoesNotRetryAContextRefusal(t *testing.T) {
	calls := 0
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			return jsonResponse(http.StatusOK, `{"object":"list","data":[]}`), nil
		}
		calls++
		return jsonResponse(http.StatusBadRequest, zenContextRefusal), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err == nil {
		t.Fatal("Chat reported success on a context-window refusal")
	}
	if got := core.FailureKindOf(err); got != core.FailureContextWindow {
		t.Fatalf("kind = %q, want %q", got, core.FailureContextWindow)
	}
	if calls != 1 {
		t.Fatalf("chat calls = %d, want exactly 1", calls)
	}
}
