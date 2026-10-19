package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Trimming must never leave an orphaned tool result behind.
//
// The production failure: the catalogue advertised 1M for GLM-5.3-Flash while
// the vendor served 100k.  The guard trimmed the oldest turns, the boundary
// landed in the middle of a tool round trip, and the request that reached the
// vendor began with a tool result whose assistant call had been dropped.
// WorkBuddy answered that with code 11133 ("the request parameters were
// rejected by the model provider") rather than 11115 ("prompt is too long"),
// because it is a malformed-history refusal, not a size one -- and no amount of
// retrying a malformed body can fix it.
// ---------------------------------------------------------------------------

// toolTurn builds the shape Codex replays: an assistant turn that issued one or
// more calls, followed by one tool result per call.
func toolTurn(payload string, callIDs ...string) []core.Message {
	assistant := core.Message{Role: "assistant"}
	for _, call := range callIDs {
		assistant.ToolCalls = append(assistant.ToolCalls, core.ToolCall{
			ID:        call,
			Type:      "function",
			Name:      "exec_command",
			Arguments: `{"cmd":"echo hi"}`,
		})
	}
	out := []core.Message{assistant}
	for _, call := range callIDs {
		out = append(out, core.Message{
			Role:       "tool",
			ToolCallID: call,
			Name:       "exec_command",
			Content:    payload,
		})
	}
	return out
}

// firstDroppablePosition reports the index of the first kept message that is not
// a pinned system prompt, which is what a vendor validates when it re-pairs the
// history.
func firstNonSystem(req *core.ChatRequest) (int, core.Message) {
	for i, m := range req.Messages {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "system") {
			return i, m
		}
	}
	return -1, core.Message{}
}

// TestApplyContextBudgetNeverLeavesAnOrphanToolResult is the regression test
// for the 11133 refusal.  Whatever boundary the budget lands on, the kept
// window must not begin with a tool result whose assistant call was dropped.
func TestApplyContextBudgetNeverLeavesAnOrphanToolResult(t *testing.T) {
	// A conversation whose tail is a tool round trip, sized so the trim has to
	// land inside it: the final tool result alone fits, but the assistant call
	// that issued it does not fit alongside the older turns.
	req := &core.ChatRequest{}
	req.Messages = append(req.Messages, core.Message{Role: "user", Content: strings.Repeat("a", 2000)})
	req.Messages = append(req.Messages, core.Message{Role: "assistant", Content: strings.Repeat("b", 2000)})
	req.Messages = append(req.Messages, toolTurn(strings.Repeat("c", 2000), "call_1")...)

	if _, trimmed := applyContextBudget(req, 1200); !trimmed {
		t.Fatal("expected a trim for a request this far over budget")
	}
	if _, first := firstNonSystem(req); strings.EqualFold(strings.TrimSpace(first.Role), "tool") {
		t.Fatalf("the trimmed history begins with a tool result (%d messages left), which the vendor reads as a malformed request", len(req.Messages))
	}
}

// TestApplyContextBudgetKeepsAWholeToolRoundTrip pins the other half: when the
// budget allows the round trip, both the call and its result survive together,
// so the trim is not merely avoiding the bug by discarding the newest work.
func TestApplyContextBudgetKeepsAWholeToolRoundTrip(t *testing.T) {
	req := &core.ChatRequest{}
	req.Messages = append(req.Messages, core.Message{Role: "user", Content: strings.Repeat("a", 4000)})
	req.Messages = append(req.Messages, toolTurn(strings.Repeat("b", 400), "call_1")...)

	if _, trimmed := applyContextBudget(req, 20000); trimmed {
		t.Fatal("a request that fits must not be trimmed")
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want the whole round trip kept", len(req.Messages))
	}
	if !strings.EqualFold(req.Messages[1].Role, "assistant") || len(req.Messages[1].ToolCalls) != 1 {
		t.Fatal("the assistant call must still be there")
	}
	if !strings.EqualFold(req.Messages[2].Role, "tool") || req.Messages[2].ToolCallID != "call_1" {
		t.Fatal("the matching tool result must still be there")
	}
}

// TestApplyContextBudgetTrimsBeforeAMultiResultRoundTrip covers the parallel
// call case: several results answer one assistant turn, so the boundary has to
// step back over all of them, not just the last one.
func TestApplyContextBudgetTrimsBeforeAMultiResultRoundTrip(t *testing.T) {
	req := &core.ChatRequest{}
	req.Messages = append(req.Messages, core.Message{Role: "user", Content: strings.Repeat("a", 3000)})
	req.Messages = append(req.Messages, core.Message{Role: "assistant", Content: strings.Repeat("z", 3000)})
	req.Messages = append(req.Messages, toolTurn(strings.Repeat("c", 300), "call_1", "call_2", "call_3")...)

	if _, trimmed := applyContextBudget(req, 2000); !trimmed {
		t.Fatal("expected a trim")
	}
	// Every surviving tool result must still be preceded by the assistant turn
	// that issued its call.  The check walks the kept window and tracks the call
	// ids the most recent assistant turn opened, which is exactly the pairing the
	// vendor validates.
	issued := map[string]bool{}
	for _, m := range req.Messages {
		switch {
		case strings.EqualFold(strings.TrimSpace(m.Role), "assistant"):
			issued = map[string]bool{}
			for _, tc := range m.ToolCalls {
				issued[tc.ID] = true
			}
		case strings.EqualFold(strings.TrimSpace(m.Role), "tool"):
			if !issued[m.ToolCallID] {
				t.Fatalf("tool result %q survived without its assistant call: %+v", m.ToolCallID, m)
			}
			delete(issued, m.ToolCallID)
		}
	}
}

// ---------------------------------------------------------------------------
// The estimator must price a base64 screenshot as an image, not as prose.
//
// view_image returns the picture inline as a data URL.  One 466x466 PNG is
// ~60k base64 characters, which the plain text divisor read as ~18k tokens
// while the vendor bills a few tiles' worth.  That single overcount is what
// put the guard's estimate an order of magnitude ahead of the vendor's own
// measurement on any conversation that had just looked at a screenshot.
// ---------------------------------------------------------------------------

func TestEstimateContentTokensPricesAnInlineImageAsAnImage(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 3000)
	got := estimateContentTokens(image)
	if got != inlineImageTokens {
		t.Fatalf("a pure image body = %d tokens, want the flat image cost %d", got, inlineImageTokens)
	}
	if naive := estimateTextTokens(image); got >= naive {
		t.Fatalf("the image-aware estimate %d must be far below the text estimate %d", got, naive)
	}
}

func TestEstimateContentTokensCountsTextAroundTheImage(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("QUJDRA", 2000)
	before, after := "here is the screenshot you asked for\n", "\nthat is all"
	got := estimateContentTokens(before + image + after)
	want := inlineImageTokens + estimateTextTokens(before) + estimateTextTokens(after)
	if got != want {
		t.Fatalf("image with surrounding text = %d tokens, want %d", got, want)
	}
}

func TestEstimateContentTokensHandlesSeveralImages(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("QUJDRA", 1000)
	if got, want := estimateContentTokens(image+image+image), 3*inlineImageTokens; got != want {
		t.Fatalf("three images = %d tokens, want %d", got, want)
	}
}

func TestEstimateContentTokensLeavesPlainTextAlone(t *testing.T) {
	for _, body := range []string{
		"",
		"just some ordinary prose",
		"data:image/png;base64,not!!valid!!base64",
		"https://example.com/data:image/png;base64,QUJD",
	} {
		if got, want := estimateContentTokens(body), estimateTextTokens(body); got != want {
			t.Fatalf("estimateContentTokens(%q) = %d, want the text estimate %d", body, got, want)
		}
	}
}

// TestEstimateMessageTokensPricesAScreenshotToolResultAsAnImage is the
// end-to-end shape: the tool result carries the data URL in Content, which is
// where rawToText puts a view_image output.
func TestEstimateMessageTokensPricesAScreenshotToolResultAsAnImage(t *testing.T) {
	image := "data:image/png;base64," + strings.Repeat("iVBORw0KGgoAAAANSUhEUg", 4000)
	msg := core.Message{Role: "tool", Name: "view_image", Content: image}
	got := estimateMessageTokens(msg)
	if got > 4*inlineImageTokens {
		t.Fatalf("a one-screenshot tool result = %d tokens, want roughly one image's cost", got)
	}
	if naive := messageOverheadTokens + estimateTextTokens(image); got >= naive {
		t.Fatalf("the screenshot result %d must be far below the text estimate %d", got, naive)
	}
}

// ---------------------------------------------------------------------------
// A refusal that names no numbers still gets one conservative retry.
//
// Loomy answers an oversized prompt with "...内容太长了，请精简输入或开启新对话"
// and names neither the size it measured nor the limit it compared against, so
// there is no proportion to keep and nothing to learn.  The gateway still knows
// what it advertised and the refusal says that figure was optimistic.
// ---------------------------------------------------------------------------

func TestRetryBudgetForAnUnmeasuredRefusalUsesAFractionOfTheWindow(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: strings.Repeat("a", 4000000)}}}
	estimated := int64(estimateRequestTokens(req))
	window := int64(1000000)

	budget := retryBudgetForUnmeasuredRefusal(req, window, 0)
	if budget <= 0 || budget >= estimated {
		t.Fatalf("budget = %d, want a positive value below the estimate %d", budget, estimated)
	}
	if want := window / unmeasuredRefusalRetryDivisor; budget > want {
		t.Fatalf("budget = %d, want it inside a fraction of the advertised %d", budget, window)
	}
	// An unknown window gives the guard nothing to size a retry against.
	if got := retryBudgetForUnmeasuredRefusal(req, 0, 0); got != 0 {
		t.Fatalf("budget without a window = %d, want 0", got)
	}
	// A request that already fits needs no retry.
	small := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}}
	if got := retryBudgetForUnmeasuredRefusal(small, window, 0); got != 0 {
		t.Fatalf("budget for a fitting request = %d, want 0", got)
	}
}

// unmeasuredRefusalClient behaves like Loomy's backing vendor: it advertises one
// window and enforces a smaller one, refusing oversized prompts with Chinese
// prose that carries no numbers at all.
type unmeasuredRefusalClient struct {
	name       string
	advertised int64
	enforced   int64
	calls      int
	lastSeen   *core.ChatRequest
}

func (c *unmeasuredRefusalClient) Name() string { return c.name }

func (c *unmeasuredRefusalClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:glm-5.3-flash", OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.advertised,
	}}}, nil
}

func (c *unmeasuredRefusalClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.lastSeen = req
	if int64(estimateRequestTokens(req)) > c.enforced {
		return nil, core.Fail(c.name, "", core.FailureContextWindow, 400,
			fmt.Errorf(`upstream HTTP 400: {"error":{"message":"内容太长了，请精简输入或开启新对话","type":"invalid_request_error","code":400,"metadata":{"provider_name":"baidu-qianfan"}}}`))
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "ok"},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *unmeasuredRefusalClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// TestUnmeasuredRefusalIsRetriedWithAConservativeTrim pins the repair: a
// refusal that names no numbers is still retried once against a fraction of the
// advertised window, so a conversation the guard under-measured is served
// instead of dead-ending.
func TestUnmeasuredRefusalIsRetriedWithAConservativeTrim(t *testing.T) {
	// The catalogue advertises 100k, so the proactive guard forwards a ~70k
	// request; the vendor enforces 50k and says so without numbers.  Halving the
	// advertised window gives the retry a ~45k budget, which fits.
	c := &unmeasuredRefusalClient{name: "loomy", advertised: 100000, enforced: 50000}
	s := newGuardServer(t, c, "")

	body := strings.Replace(oversizedBody(4, 60000), "bunny/cn:space-bunny", "loomy/cn:glm-5.3-flash", 1)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected the unmeasured refusal to be retried and served, got %d: %s", rr.Code, rr.Body.String())
	}
	if c.calls < 2 {
		t.Fatalf("upstream calls = %d, want the refusal plus at least one retry", c.calls)
	}
	if got := int64(estimateRequestTokens(c.lastSeen)); got > c.enforced {
		t.Fatalf("the retry must fit the vendor's window, got %d > %d", got, c.enforced)
	}
}

// TestUnmeasuredRefusalReturnsTheVendorErrorWhenTheRetryAlsoFails pins that the
// retry is an attempt, not a promise: when even the conservative trim cannot
// fit, the caller gets a refusal rather than a silent rewrite that answers a
// different question.
func TestUnmeasuredRefusalReturnsTheVendorErrorWhenTheRetryAlsoFails(t *testing.T) {
	// Advertised 100k, enforced 40k: the newest turn alone is ~45k, so neither
	// the first attempt nor the halved retry can fit.
	c := &unmeasuredRefusalClient{name: "loomy", advertised: 100000, enforced: 40000}
	s := newGuardServer(t, c, "")

	body := strings.Replace(oversizedBody(4, 153000), "bunny/cn:space-bunny", "loomy/cn:glm-5.3-flash", 1)
	rr := chatHandler(t, s, body)
	if rr.Code == http.StatusOK {
		t.Fatalf("an unfittable request must not be reported as served: %s", rr.Body.String())
	}
	if c.calls > 2 {
		t.Fatalf("upstream calls = %d, want at most the refusal plus one retry", c.calls)
	}
}

// TestPrepareCandidateAttemptRepairsToolPairingForEveryPlatform pins the shared
// history repair: a module is never handed a result before its call, a result
// with no call anywhere, or a call whose result was lost.
func TestPrepareCandidateAttemptRepairsToolPairingForEveryPlatform(t *testing.T) {
	c := &testClient{name: "any", catalogue: []core.Model{{ID: "m1", Extra: map[string]any{"context_length": 1000000}}}}
	s := newGuardServer(t, c, "")
	base := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: "run"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_keep", Name: "exec_command", Arguments: `{}`}}},
		{Role: "user", Content: "interjection"},
		{Role: "tool", ToolCallID: "call_keep", Name: "exec_command", Content: "ok"},
		{Role: "tool", ToolCallID: "call_missing", Name: "exec_command", Content: "orphan"},
	}}
	got, err := s.prepareCandidateAttempt(context.Background(), core.Candidate{Client: c, Model: "m1"}, "m1", base, nil, false)
	if err != nil {
		t.Fatalf("prepareCandidateAttempt: %v", err)
	}
	var sawCall, sawResult bool
	for i, m := range got.Messages {
		if len(m.ToolCalls) > 0 && m.ToolCalls[0].ID == "call_keep" {
			sawCall = true
			if i+1 >= len(got.Messages) || got.Messages[i+1].Role != "tool" || got.Messages[i+1].ToolCallID != "call_keep" {
				t.Fatalf("tool result is not adjacent to its call: %+v", got.Messages)
			}
		}
		if m.Role == "tool" {
			if m.ToolCallID == "call_missing" {
				t.Fatalf("orphan tool result survived: %+v", got.Messages)
			}
			if m.ToolCallID == "call_keep" {
				sawResult = true
			}
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("matched round trip was lost: %+v", got.Messages)
	}
}

func TestPrepareCandidateAttemptDropsAnOrphanToolCall(t *testing.T) {
	c := &testClient{name: "any", catalogue: []core.Model{{ID: "m1", Extra: map[string]any{"context_length": 1000000}}}}
	s := newGuardServer(t, c, "")
	base := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: "run"},
		{Role: "assistant", Content: "starting", ToolCalls: []core.ToolCall{{ID: "call_missing", Name: "exec_command", Arguments: `{}`}}},
		{Role: "user", Content: "continue without the result"},
	}}
	got, err := s.prepareCandidateAttempt(context.Background(), core.Candidate{Client: c, Model: "m1"}, "m1", base, nil, false)
	if err != nil {
		t.Fatalf("prepareCandidateAttempt: %v", err)
	}
	for _, m := range got.Messages {
		if len(m.ToolCalls) != 0 {
			t.Fatalf("orphan tool call survived: %+v", got.Messages)
		}
	}
	foundText := false
	for _, m := range got.Messages {
		if m.Role == "assistant" && m.Content == "starting" {
			foundText = true
		}
	}
	if !foundText {
		t.Fatalf("assistant text was lost with the orphan call: %+v", got.Messages)
	}
}
