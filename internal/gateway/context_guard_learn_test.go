package gateway

import (
	"context"
	"fmt"
	"testing"

	"client2api/internal/core"
)

// The learn hook runs on every FailureContextWindow, whatever the module or the
// vendor said.  These tests pin the cases where reading a number out of the
// message would be wrong, because a wrong window is worse than no window: it
// makes every later request for that model get trimmed against a fiction.
func TestLearnIgnoresNumbersThatAreNotALimit(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			// The real 11115 shape, but the only number that matches is inside a
			// request id or a status code.  Learning 11115 or 400 would shrink a
			// model to nothing.
			"code and request id only",
			fmt.Errorf("upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long\",\"requestId\":\"b15aeebef2a4f830ddc58866a5288569\"}"),
		},
		{
			"no size vocabulary at all",
			core.Fail("workbuddy", "", core.FailureContextWindow, 400,
				fmt.Errorf("request refused by upstream")),
		},
		{
			// A number far below any plausible serving window is a stray value,
			// not a limit.
			"implausibly small window",
			core.Fail("workbuddy", "", core.FailureContextWindow, 400,
				fmt.Errorf("prompt is too long: 500 tokens > 400 maximum")),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newGuardServer(t, &bunnyClient{name: "workbuddy", realWindow: 100000}, "")
			if got := s.learnContextWindowFromFailure(context.Background(), &bunnyClient{name: "workbuddy"}, "cn:space-bunny", tc.err); got != 0 {
				t.Fatalf("must not learn a limit from %q, got %d", tc.err, got)
			}
			if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 0 {
				t.Fatalf("nothing may be recorded, got %d", got)
			}
		})
	}
}

// A window is only ever tightened.  If the vendor's own catalogue already says a
// model is at least this large, there is nothing to correct, and writing the
// file would be churn.
func TestLearnDoesNotShrinkAnAlreadyCorrectWindow(t *testing.T) {
	// The catalogue publishes 100000, which matches the vendor.
	c := &catalogueClient{name: "workbuddy", window: 100000}
	s := newGuardServer(t, c, "")
	refusal := core.Fail("workbuddy", "", core.FailureContextWindow, 400,
		fmt.Errorf("prompt is too long: 100001 tokens > 100000 maximum"))
	if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("nothing should be recorded, got %d", got)
	}
	_ = s.learnContextWindowFromFailure(context.Background(), c, "cn:space-bunny", refusal)
	if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("a matching limit must not be recorded, got %d", got)
	}
}

// catalogueClient publishes a window of its own.
type catalogueClient struct {
	name   string
	window int64
}

func (c *catalogueClient) Name() string { return c.name }

func (c *catalogueClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:space-bunny", OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.window,
	}}}, nil
}

func (c *catalogueClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return &fakeStream{events: []core.Event{{Type: core.EventDone, Finish: "stop"}}}, nil
}

func (c *catalogueClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}
