package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"client2api/internal/core"
)

// pairingIsValid reports whether every tool message answers an assistant call
// present in the same slice.  That is the invariant WorkBuddy enforces with its
// "tool calls and tool results do not match" refusal (code 11148), and the whole
// reason a summarisation chunk may not be cut through a round trip.
func pairingIsValid(msgs []core.Message) bool {
	owner := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if id := strings.TrimSpace(tc.ID); id != "" {
				owner[id] = true
			}
		}
	}
	for _, m := range msgs {
		if !strings.EqualFold(strings.TrimSpace(m.Role), "tool") {
			continue
		}
		if !owner[strings.TrimSpace(m.ToolCallID)] {
			return false
		}
	}
	return true
}

// The production bug: splitForSummarisation packed messages greedily by size,
// so a chunk boundary could fall between an assistant turn that declared a tool
// call and the tool result answering it.  The summarisation call then carried an
// orphaned result, which WorkBuddy refused with code 11148 -- every fold failed,
// and the guard silently trimmed the history instead.
func TestSummarisationChunksNeverSplitAToolRoundTrip(t *testing.T) {
	msgs := []core.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Name: "exec", Arguments: `{"cmd":"ls"}`}}},
		{Role: "tool", ToolCallID: "call_1", Name: "exec", Content: strings.Repeat("x", 200)},
		{Role: "user", Content: "q2"},
	}

	resultsIn := 0
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "tool") {
			resultsIn++
		}
	}

	chunks := splitForSummarisation(msgs, 30)
	resultsOut := 0
	for _, chunk := range chunks {
		if !pairingIsValid(chunk) {
			t.Fatalf("a summarisation chunk split a tool round trip: %+v", chunk)
		}
		for _, m := range chunk {
			if strings.EqualFold(m.Role, "tool") {
				resultsOut++
			}
		}
	}
	if resultsOut != resultsIn {
		t.Fatalf("tool results were dropped rather than kept with their call: %d in, %d out", resultsIn, resultsOut)
	}
}

// A caller can replay a history whose tool result lost its call -- Codex does
// exactly that when it trims on its own side.  The summarisation request is built
// from that history, so it must repair the pairing before it is sent: otherwise
// the vendor refuses the fold with "tool calls and tool results do not match" and
// the guard falls back to a silent trim.
func TestSummarisationRepairsAnOrphanedToolResult(t *testing.T) {
	c := &testClient{name: "stub", events: summaryEvents("folded state")}
	srv := newGuardServer2(t, regWith(c))

	// The first result has no matching call anywhere in the history.
	orphan := core.Message{Role: "tool", ToolCallID: "call_missing", Name: "exec", Content: "stale output"}
	req := &core.ChatRequest{
		Model: "m1",
		Messages: []core.Message{
			orphan,
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: "the current question"},
		},
	}

	if _, _, _, err := compactConversation(context.Background(), srv, c, "m1", req, 1); err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if c.seen == nil {
		t.Fatal("the summariser was never called")
	}
	if !pairingIsValid(c.seen.Messages) {
		t.Fatalf("the summarisation request carried an orphaned tool result: %+v", c.seen.Messages)
	}
}

// parkedModelClient reproduces the shape that actually broke compaction in
// production: the platform still has a ready account, so its own health surface
// reports "servable", but every account is parked for THIS model.  Health cannot
// express that, so routing keeps the platform first and only the upstream call
// discovers the pool is exhausted for the model.
type parkedModelClient struct {
	name     string
	model    string
	window   int64
	servable bool
	chatSeen int
}

func (c *parkedModelClient) Name() string { return c.name }

func (c *parkedModelClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: c.model, OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.window,
	}}}, nil
}

func (c *parkedModelClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.chatSeen++
	// Exactly what workbuddy's pool produces when no account is allowed to serve
	// this model: the exhausted marker joined onto the classified refusal.
	return nil, errors.Join(core.ErrPlatformExhausted,
		core.Fail(c.name, "", core.FailureRateLimited, 429, fmt.Errorf("no account can serve model %s", req.Model)))
}

func (c *parkedModelClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: c.servable}
}

func (c *parkedModelClient) Health() core.Health {
	if c.servable {
		return core.Health{Ready: 1, Total: 1, Servable: true}
	}
	return core.Health{Ready: 0, Cooling: 1, Total: 1, Note: "no account is usable right now"}
}

// groupOf builds the operator-declared equivalence chain the caller reaches by
// naming the group, which is how a real bare-model request resolves to several
// platforms.
func groupOf(name string, members ...core.ModelGroupMember) map[string]core.ModelGroup {
	return map[string]core.ModelGroup{name: {Members: members}}
}

// The production bug: auto-compaction summarised with candidates[0] regardless
// of whether that platform could serve.  When the first platform was exhausted
// the caller-visible turn failed over to a sibling and succeeded, while the fold
// failed on the exhausted platform and degraded straight back into a silent
// trim -- so the caller never received the compaction item and re-sent the same
// oversized history forever.
func TestAutoCompactFailsOverToASiblingThatCanServe(t *testing.T) {
	primary := &parkedModelClient{name: "primary", model: "model-a", window: 1000000, servable: true}
	secondary := &testClient{
		name: "secondary",
		catalogue: []core.Model{{ID: "model-b", OwnedBy: "secondary", Extra: map[string]any{
			"context_length": int64(1000000),
		}}},
		events: summaryEvents("folded state"),
	}

	reg := core.NewRegistry()
	reg.Add(primary)
	reg.Add(secondary)
	reg.SetModelGroups(groupOf("shared-model",
		core.ModelGroupMember{Client: "primary", Model: "model-a"},
		core.ModelGroupMember{Client: "secondary", Model: "model-b"},
	))

	s := newGuardServer2(t, reg)

	// Precondition: the exhausted platform really is the first candidate.  If
	// routing ever stops ordering it first this test must fail here rather than
	// silently asserting nothing.
	candidates, err := reg.ResolveCandidates(context.Background(), "shared-model")
	if err != nil || len(candidates) < 2 {
		t.Fatalf("precondition: want at least two candidates, got %d (%v)", len(candidates), err)
	}
	if candidates[0].Client.Name() != "primary" {
		t.Fatalf("precondition: first candidate = %s, want primary", candidates[0].Client.Name())
	}
	if _, unavailable := moduleUnavailable(primary); unavailable != nil {
		t.Fatalf("precondition: the parked platform must still look servable to health, got %v", unavailable)
	}

	req := &core.ChatRequest{
		Model: "shared-model",
		Messages: []core.Message{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: "the current question"},
		},
	}

	item := s.autoCompact(context.Background(), req, &UsageRecord{})
	if item == nil {
		t.Fatalf("autoCompact degraded to a trim because it summarised with the exhausted platform; primary attempts=%d secondary calls=%d",
			primary.chatSeen, secondary.chatCalls)
	}
	// The parked platform cannot be identified before the call -- its own health
	// surface reports servable -- so trying it once is correct.  What matters is
	// that the fold did not stop there.
	if primary.chatSeen == 0 {
		t.Fatal("the first candidate was never tried; the failover path was not exercised")
	}
	if secondary.chatCalls == 0 {
		t.Fatal("the healthy sibling was never asked to summarise, so the fold degraded to a trim")
	}
}

// When no candidate can serve, the turn itself is about to fail.  Spending a
// summarisation round trip first only adds latency and a misleading log line.
func TestAutoCompactSkipsWhenNoCandidateCanServe(t *testing.T) {
	primary := &parkedModelClient{name: "primary", model: "model-a", window: 1000000}

	reg := core.NewRegistry()
	reg.Add(primary)
	reg.SetModelGroups(groupOf("shared-model",
		core.ModelGroupMember{Client: "primary", Model: "model-a"},
	))

	s := newGuardServer2(t, reg)
	req := &core.ChatRequest{
		Model: "shared-model",
		Messages: []core.Message{
			{Role: "user", Content: "first question"},
			{Role: "assistant", Content: strings.Repeat("a", 400)},
			{Role: "user", Content: "the current question"},
		},
	}

	if item := s.autoCompact(context.Background(), req, &UsageRecord{}); item != nil {
		t.Fatalf("compaction ran even though no candidate could serve: %+v", item)
	}
	if primary.chatSeen != 0 {
		t.Fatalf("the exhausted platform was asked to summarise %d time(s)", primary.chatSeen)
	}
}
