package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// measuredClient models the production case the proactive guard cannot see:
// the vendor's tokenizer reads a conversation far denser than the gateway's
// estimate, so a request the guard budgets as fitting is refused anyway -- and
// the refusal names both numbers, which is what makes the retry possible.
type measuredClient struct {
	name          string
	window        int64
	density       int64
	calls         int
	seen          []*core.ChatRequest
	messageCounts []int
}

func (c *measuredClient) Name() string { return c.name }

func (c *measuredClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:deepseek-v4.1-flash", OwnedBy: c.name, Extra: map[string]any{
		"context_length":    int64(1000000),
		"max_output_tokens": int64(32768),
	}}}, nil
}

func (c *measuredClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.seen = append(c.seen, req)
	c.messageCounts = append(c.messageCounts, len(req.Messages))
	measured := int64(estimateRequestTokens(req)) * c.density
	if measured > c.window {
		// WorkBuddy's real body escapes ">" as \u003e; the parser has to read
		// both numbers through that escaping for the retry to be possible.
		return nil, core.Fail(c.name, "", core.FailureContextWindow, 400,
			fmt.Errorf("upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long: %d tokens \\u003e %d maximum\",\"requestId\":\"b15aeebef2a4f830ddc58866a5288569\"}",
				measured, c.window))
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "ok"},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *measuredClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// TestMeasuredRefusalIsTrimmedAndServedOnTheRetry pins the production repair:
// a conversation the heuristic reads as fitting, but the vendor refuses with
// its own measurement, is trimmed to the measured proportion and retried once
// instead of failing the caller forever.
func TestMeasuredRefusalIsTrimmedAndServedOnTheRetry(t *testing.T) {
	c := &measuredClient{name: "workbuddy", window: 1048576, density: 2}
	usage := NewUsageStore(0)
	s := newGuardServer(t, c, "", func(o *Options) { o.Usage = usage })

	// Roughly 700k estimated tokens: under the published 1M window, so the
	// proactive guard forwards the request whole, exactly as it did in
	// production.
	body := strings.Replace(oversizedBody(60, 40000), "bunny/cn:space-bunny", "workbuddy/cn:deepseek-v4.1-flash", 1)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected the measured refusal to be trimmed and served, got %d: %s", rr.Code, rr.Body.String())
	}
	if c.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2: the refusal plus one trimmed retry", c.calls)
	}
	if len(c.seen) != 2 {
		t.Fatalf("seen %d requests, want 2", len(c.seen))
	}
	// The retry reuses the same request object and trims it in place, so the
	// counts have to be captured at call time rather than read back from seen.
	if len(c.messageCounts) != 2 {
		t.Fatalf("captured %d message counts, want 2", len(c.messageCounts))
	}
	if c.messageCounts[1] >= c.messageCounts[0] {
		t.Fatalf("the retry must carry less history: %d -> %d messages",
			c.messageCounts[0], c.messageCounts[1])
	}
	if got := int64(estimateRequestTokens(c.seen[1])) * c.density; got > c.window {
		t.Fatalf("the retried request still does not fit the measured window: %d > %d", got, c.window)
	}

	rows := usage.Snapshot()
	var trimmed *UsageRecord
	for i := range rows {
		if rows[i].Trimmed {
			trimmed = &rows[i]
			break
		}
	}
	if trimmed == nil {
		t.Fatal("the measured trim must be recorded on the usage row")
	}
	if trimmed.ContextWindow != c.window {
		t.Fatalf("usage row window = %d, want the vendor's measured %d", trimmed.ContextWindow, c.window)
	}
	if trimmed.TrimDropped <= 0 || trimmed.TrimmedFromTokens <= trimmed.TrimmedToTokens {
		t.Fatalf("usage row must show what was dropped, got %+v", *trimmed)
	}
}

func TestRetryBudgetFollowsTheVendorMeasurement(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: strings.Repeat("a", 2720000)}}}
	estimated := int64(estimateRequestTokens(req))
	budget := retryBudgetForMeasuredRefusal(req, 1427021, 1048576, 0)
	if budget <= 0 || budget >= estimated {
		t.Fatalf("budget = %d, want a positive value below the estimate %d", budget, estimated)
	}
	// Without the vendor's measured size there is nothing to keep in proportion.
	if got := retryBudgetForMeasuredRefusal(req, 0, 1048576, 0); got != 0 {
		t.Fatalf("budget without a measurement = %d, want 0", got)
	}
	// A measurement implausibly far past the window is not a conversation that
	// should be cut down to a sliver on its word alone.
	if got := retryBudgetForMeasuredRefusal(req, 1048576*measuredRefusalMaxOvershoot+1, 1048576, 0); got != 0 {
		t.Fatalf("budget beyond the overshoot cap = %d, want 0", got)
	}
}
