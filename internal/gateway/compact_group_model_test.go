package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// freeOnlyClient behaves like the real opencode module on an anonymous free
// account: its catalogue carries only the -free id, and it refuses anything
// else before spending an upstream round trip.
type freeOnlyClient struct {
	name      string
	allowed   string
	seenModel []string
}

func (c *freeOnlyClient) Name() string { return c.name }

func (c *freeOnlyClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: c.allowed, OwnedBy: c.name, Extra: map[string]any{
		"context_length": int64(200000),
	}}}, nil
}

func (c *freeOnlyClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.seenModel = append(c.seenModel, req.Model)
	if req.Model != c.allowed {
		return nil, core.Fail(c.name, "", core.FailureOther, 400,
			fmt.Errorf("%w: model %q is not available to the anonymous free account", core.ErrUnsupported, req.Model))
	}
	return &fakeStream{events: []core.Event{
		{Type: core.EventDelta, Delta: "SUMMARY"},
		{Type: core.EventDone, Finish: "stop"},
	}}, nil
}

func (c *freeOnlyClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// The production bug: a caller asks for a model GROUP name, routing resolves it
// to a concrete platform model, but auto-compaction summarised with the group
// name instead.  On a platform whose catalogue only carries the resolved id
// (opencode's anonymous free account), that summarisation is refused every
// single turn, so compaction never happens and the guard silently trims
// instead.
func TestAutoCompactSummarisesWithTheResolvedModel(t *testing.T) {
	client := &freeOnlyClient{name: "opencode", allowed: "deepseek-v4.1-flash-free"}

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
		Model: "deepseek-v4.1-flash", // the GROUP name the caller wrote
		Messages: []core.Message{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: "second question"},
			{Role: "assistant", Content: strings.Repeat("b", 400)},
			{Role: "user", Content: "the current question"},
		},
	}

	item := s.autoCompact(context.Background(), req, &UsageRecord{})
	if item == nil {
		t.Fatalf("autoCompact returned nil; summariser saw models %v", client.seenModel)
	}
	if len(client.seenModel) == 0 {
		t.Fatal("the summariser was never called")
	}
	for _, m := range client.seenModel {
		if m != "deepseek-v4.1-flash-free" {
			t.Fatalf("summariser was asked for %q, want the resolved model %q (group name must never reach a module)",
				m, "deepseek-v4.1-flash-free")
		}
	}
	// The caller's own model name must survive: routing still keys off the
	// group, and the wire echoes what the caller sent.
	if req.Model != "deepseek-v4.1-flash" {
		t.Fatalf("autoCompact must not rewrite the caller's model, got %q", req.Model)
	}
}

// newGuardServer2 builds a server around an already-populated registry.
func newGuardServer2(t *testing.T, reg *core.Registry, opts ...func(*Options)) *server {
	t.Helper()
	o := Options{Registry: reg, Version: "test"}
	for _, fn := range opts {
		fn(&o)
	}
	if o.Stats == nil {
		o.Stats = NewStats()
	}
	if o.Usage == nil {
		o.Usage = NewUsageStore(0)
	}
	if o.SessionPlatforms == nil {
		o.SessionPlatforms = NewSessionPlatforms()
	}
	if o.Logger == nil {
		o.Logger = testDiscardLogger()
	}
	s := &server{
		opts:             o,
		stats:            o.Stats,
		sessionPlatforms: o.SessionPlatforms,
		estimates:        newEstimateCalibrationStore(o.EstimateCalibrationPath, nil),
	}
	s.learnedWindows = openLearnedWindowStore(o.LearnedWindowsPath, o.Logger.Printf)
	s.compaction = newCompactionSigner(o.CompactionKeyPath, o.Logger.Printf)
	s.handler = s.routes()
	return s
}

func testDiscardLogger() *log.Logger { return log.New(io.Discard, "", 0) }
