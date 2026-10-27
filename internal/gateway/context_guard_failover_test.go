package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// fallbackWindowClient refuses the first attempt with a limit-only context
// refusal, then serves the request once it has been trimmed.  It records how
// many messages each attempt carried so the test can prove the retry stayed on
// the same platform and did not simply fail over to a sibling.
type fallbackWindowClient struct {
	name         string
	limit        int64
	calls        int
	counts       []int
	alwaysRefuse bool
}

func (c *fallbackWindowClient) Name() string { return c.name }

func (c *fallbackWindowClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "deepseek-v4.1-flash-1017", OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(8000000),
	}}}, nil
}

func (c *fallbackWindowClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.counts = append(c.counts, len(req.Messages))
	if c.alwaysRefuse || int64(estimateRequestTokens(req)) > c.limit {
		return nil, core.Fail(c.name, "", core.FailureOther, http.StatusBadRequest,
			fmt.Errorf("openai-compat: chat (HTTP 400): Input exceeds the context limit (%d tokens). Please shorten the input.", c.limit))
	}
	return &fakeStream{events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *fallbackWindowClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// TestContextRefusalStaysOnPlatformUntilTrimRetryFails pins the operator's
// chosen order: when a context refusal names the limit, the gateway first
// trims and retries on the same platform; only if that retry is refused again
// may the router move to the next platform.
func TestContextRefusalStaysOnPlatformUntilTrimRetryFails(t *testing.T) {
	first := &fallbackWindowClient{name: "tokenrhythm", limit: 1048568}
	second := &fallbackWindowClient{name: "sibling", limit: 8000000}

	reg := core.NewRegistry()
	reg.Add(first)
	reg.Add(second)
	reg.Add(&alwaysOKClient{name: "fallback"})

	// Give the first member a head start so the router enters it before the
	// healthy sibling.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"tokenrhythm": {Priority: 1},
		"sibling":     {Priority: 2},
		"fallback":    {Priority: 3},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-v4.1-flash": {Members: []core.ModelGroupMember{
			{Client: "tokenrhythm", Model: "deepseek-v4.1-flash-1017"},
			{Client: "sibling", Model: "deepseek-v4.1-flash-1017"},
			{Client: "fallback", Model: "deepseek-v4.1-flash-1017"},
		}},
	})

	s := newGuardServer2(t, reg, func(o *Options) {
		o.ProbeBeforeTrim = false
	})
	body := contextFailoverBody(30, 700000, "deepseek-v4.1-flash")
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if first.calls != 2 {
		t.Fatalf("first platform calls = %d, want 2 (refusal + trimmed retry)", first.calls)
	}
	if second.calls != 0 {
		t.Fatalf("sibling calls = %d, want 0: the trim retry must be attempted first", second.calls)
	}
	if first.counts[1] >= first.counts[0] {
		t.Fatalf("same-platform retry was not trimmed: counts = %v", first.counts)
	}
}

// TestContextRefusalFailsOverAfterTrimRetryAlsoFails pins the second half:
// once the same-platform trim retry has already been spent, a second context
// refusal may move on to the next platform in the model group.
func TestContextRefusalFailsOverAfterTrimRetryAlsoFails(t *testing.T) {
	first := &fallbackWindowClient{name: "tokenrhythm", limit: 1048568, alwaysRefuse: true}
	second := &fallbackWindowClient{name: "sibling", limit: 8000000}

	reg := core.NewRegistry()
	reg.Add(first)
	reg.Add(second)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"tokenrhythm": {Priority: 1},
		"sibling":     {Priority: 2},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-v4.1-flash": {Members: []core.ModelGroupMember{
			{Client: "tokenrhythm", Model: "deepseek-v4.1-flash-1017"},
			{Client: "sibling", Model: "deepseek-v4.1-flash-1017"},
		}},
	})

	s := newGuardServer2(t, reg, func(o *Options) { o.ProbeBeforeTrim = false })
	rr := chatHandler(t, s, contextFailoverBody(30, 700000, "deepseek-v4.1-flash"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if first.calls != 2 {
		t.Fatalf("first platform calls = %d, want 2 (refusal + trim retry)", first.calls)
	}
	if second.calls != 1 {
		t.Fatalf("sibling calls = %d, want 1 after the same-platform trim retry failed", second.calls)
	}
}

type alwaysOKClient struct{ name string }

func (c *alwaysOKClient) Name() string { return c.name }

func (c *alwaysOKClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "deepseek-v4.1-flash-1017", OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(8000000),
	}}}, nil
}

func (c *alwaysOKClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return &fakeStream{events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *alwaysOKClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func contextFailoverBody(turns, size int, model string) string {
	msgs := make([]map[string]string, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": strings.Repeat("a", size)})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": msgs,
		"stream":   false,
	})
	return string(b)
}
