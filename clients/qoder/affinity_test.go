package qoder

import (
	"testing"
	"time"

	"client2api/internal/core"
)

func affinityTestClient(t *testing.T) *Client {
	t.Helper()
	now := time.Now().UTC()
	c := testClient(t, defaultOpenAPIBase,
		account{
			storedAccount: storedAccount{ID: "a1", Token: "t1", Enabled: true},
			lastUsed:      now,
		},
		account{
			storedAccount: storedAccount{ID: "a2", Token: "t2", Enabled: true},
			lastUsed:      now.Add(-time.Hour),
		},
	)
	c.affinity = core.NewAffinity(0)
	return c
}

func TestOrderedCandidatesKeepsConversationOnItsAccount(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	// The second account is least-recently-used, so a plain rotation would
	// start there; the first call binds the conversation to it.
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
	req := &core.ChatRequest{}
	got := c.orderedCandidates(req, "auto")
	if len(got) == 0 || got[0].ID != "a2" {
		t.Fatalf("unscoped pick = %#v, want plain LRU a2", got)
	}
	if c.affinity.Count() != 0 {
		t.Fatalf("unscoped request wrote an affinity binding")
	}
}

func TestOrderedCandidatesDropsADeadBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-2", "a1")
	c.store.setEnabled("a1", false)

	got := c.orderedCandidates(&core.ChatRequest{ConversationID: "conv-2"}, "auto")
	if len(got) == 0 || got[0].ID != "a2" {
		t.Fatalf("pick = %#v, want a2 after a1 was disabled", got)
	}
	c.bindServedConversation(&core.ChatRequest{ConversationID: "conv-2"}, got[0].ID)
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
