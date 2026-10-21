package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// busyOnceClient reports local backpressure for its first N chat calls, the way
// a platform with max_in_flight_per_account=1 does when a concurrent request
// already holds the only slot.
type busyOnceClient struct {
	name     string
	model    string
	busyLeft int
	calls    int
}

func (c *busyOnceClient) Name() string { return c.name }

func (c *busyOnceClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: c.model, OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(200000),
	}}}, nil
}

func (c *busyOnceClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls++
	if c.busyLeft > 0 {
		c.busyLeft--
		return nil, core.ErrBusy
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "SUMMARY"},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *busyOnceClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// Local backpressure is transient by definition: the slot is held by another
// request that will finish.  Treating it as "cannot compact" degrades straight
// back into the silent trim the whole mechanism exists to avoid, so the
// summariser waits for the slot instead of giving up on the first refusal.
func TestAutoCompactRetriesThroughTransientBackpressure(t *testing.T) {
	client := &busyOnceClient{name: "opencode", model: "deepseek-v4.1-flash-free", busyLeft: 1}

	reg := core.NewRegistry()
	reg.Add(client)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"deepseek-v4.1-flash": {
			Members: []core.ModelGroupMember{
				{Client: "opencode", Model: "deepseek-v4.1-flash-free"},
			},
		},
	})
	s := newGuardServer2(t, reg, func(o *Options) {
		o.CompactionKeyPath = filepath.Join(t.TempDir(), "compaction.key")
	})

	req := &core.ChatRequest{
		Model: "deepseek-v4.1-flash",
		Messages: []core.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: strings.Repeat("b", 400)},
			{Role: "user", Content: "the current question"},
		},
	}

	item := s.autoCompact(context.Background(), req, &UsageRecord{})
	if item == nil {
		t.Fatalf("compaction must survive one transient busy refusal; calls=%d", client.calls)
	}
	if client.calls < 2 {
		t.Fatalf("compaction should have retried after ErrBusy, calls=%d", client.calls)
	}
}
