package loomy

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// affinityTestClient builds a two-account pool whose LRU order is known: a2 was
// used an hour ago and a1 a moment ago, so plain rotation starts at a2.
func affinityTestClient(t *testing.T) *Client {
	t.Helper()
	now := time.Now().UTC()
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "a1", testToken, 0)
	seedAccount(t, c, "a2", testTokenTwo, 0)
	c.store.reset("a1", now)
	c.store.reset("a2", now.Add(-time.Hour))
	c.affinity = core.NewAffinity(0)
	return c
}

func TestOrderedCandidatesKeepsConversationOnItsAccount(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	first := c.orderedCandidates(req, "auto")
	if len(first) == 0 || first[0].ID != "a2" {
		t.Fatalf("first pick = %#v, want a2 at the front", first)
	}
	c.bindServedConversation(req, first[0].ID)
	if got, ok := c.ConversationAccount("conv-1", "auto"); !ok || got != "a2" {
		t.Fatalf("binding = %q, %v; want a2", got, ok)
	}

	// Simulate the LRU moving a2 to the back: the binding must still win.
	c.store.reset("a2", time.Now().UTC())
	second := c.orderedCandidates(req, "auto")
	if len(second) == 0 || second[0].ID != "a2" {
		t.Fatalf("second pick = %#v, want a2 pinned", second)
	}
}

func TestOrderedCandidatesRotatesWithoutAConversation(t *testing.T) {
	c := affinityTestClient(t)
	got := c.orderedCandidates(&core.ChatRequest{}, "auto")
	if len(got) == 0 || got[0].ID != "a2" {
		t.Fatalf("unscoped pick = %#v, want plain LRU a2", got)
	}
	if c.affinity.Count() != 0 {
		t.Fatal("unscoped request wrote an affinity binding")
	}
}

func TestOrderedCandidatesDropsADeadBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-2", "a1")
	if err := c.store.setEnabled("a1", false); err != nil {
		t.Fatalf("disabling a1: %v", err)
	}

	got := c.orderedCandidates(&core.ChatRequest{ConversationID: "conv-2"}, "auto")
	if len(got) == 0 || got[0].ID != "a2" {
		t.Fatalf("pick = %#v, want a2 after a1 was disabled", got)
	}
	req := &core.ChatRequest{ConversationID: "conv-2"}
	c.bindServedConversation(req, got[0].ID)
	if id, ok := c.ConversationAccount("conv-2", "auto"); !ok || id != "a2" {
		t.Fatalf("binding = %q, %v; want a2 after rebind", id, ok)
	}
}

func TestUnbindConversationForgetsTheBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-3", "a1")
	if !c.UnbindConversation("conv-3") {
		t.Fatal("UnbindConversation reported no binding")
	}
	if _, ok := c.ConversationAccount("conv-3", "auto"); ok {
		t.Fatal("binding survived UnbindConversation")
	}
}
