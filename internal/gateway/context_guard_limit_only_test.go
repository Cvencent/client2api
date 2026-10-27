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

// limitOnlyClient models a relay that names the limit but not the size it
// measured the request at.  The gateway must still use the named limit to
// trim and retry once on the same platform.
type limitOnlyClient struct {
	name   string
	limit  int64
	calls  int
	counts []int
	seens  []*core.ChatRequest
}

func (c *limitOnlyClient) Name() string { return c.name }

func (c *limitOnlyClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "deepseek-v4.1-flash-1017", OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(8000000),
	}}}, nil
}

func (c *limitOnlyClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.counts = append(c.counts, len(req.Messages))
	c.seens = append(c.seens, req)
	if int64(estimateRequestTokens(req)) > c.limit {
		return nil, core.Fail(c.name, "", core.FailureOther, http.StatusBadRequest,
			fmt.Errorf("openai-compat: chat (HTTP 400): Input exceeds the context limit (%d tokens). Please shorten the input.", c.limit))
	}
	return &fakeStream{events: []core.Event{{Type: core.EventDelta, Delta: "ok"}, {Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *limitOnlyClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

func TestLimitOnlyContextRefusalTrimsAndRetriesOnTheSamePlatform(t *testing.T) {
	c := &limitOnlyClient{name: "tokenrhythm", limit: 1048568}
	s := newGuardServer(t, c, "")
	body := limitOnlyBody(30, 700000)
	rr := chatHandler(t, s, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if c.calls != 2 {
		t.Fatalf("upstream calls = %d, want 2 (refusal + one trimmed retry)", c.calls)
	}
	if len(c.counts) != 2 {
		t.Fatalf("captured %d message counts, want 2", len(c.counts))
	}
	if c.counts[1] >= c.counts[0] {
		t.Fatalf("retry did not trim: counts = %v", c.counts)
	}
}

func limitOnlyBody(turns, size int) string {
	msgs := make([]map[string]string, 0, turns)
	for i := 0; i < turns; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, map[string]string{"role": role, "content": strings.Repeat("a", size)})
	}
	b, _ := json.Marshal(map[string]any{
		"model":    "tokenrhythm/deepseek-v4.1-flash-1017",
		"messages": msgs,
		"stream":   false,
	})
	return string(b)
}
