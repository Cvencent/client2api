package codearts

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// affinityTestClient builds a two-account pool whose LRU order is known.
func affinityTestClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	now := time.Now().UTC()
	for i, a := range []account{
		{ID: "a1", AccessKeyID: "AK1", SecretAccessKey: "S1", SecurityToken: "T1"},
		{ID: "a2", AccessKeyID: "AK2", SecretAccessKey: "S2", SecurityToken: "T2"},
	} {
		used := now
		if i == 1 {
			used = now.Add(-time.Hour)
		}
		c.pool.mu.Lock()
		c.pool.upsertLocked(&entry{acct: a, state: stateReady, lastUsed: used.UnixMilli()})
		c.pool.mu.Unlock()
	}
	c.affinity = core.NewAffinity(0)
	return c
}

func TestPickAccountKeepsConversationOnItsAccount(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	first := c.pickAccount(req, nil)
	if first == nil || first.id() != "a2" {
		t.Fatalf("first pick = %#v, want a2", first)
	}
	c.bindServedConversation(req, first.id())
	if got, ok := c.ConversationAccount("conv-1", "auto"); !ok || got != "a2" {
		t.Fatalf("binding = %q, %v; want a2", got, ok)
	}

	c.pool.mu.Lock()
	c.pool.findLocked("a2").lastUsed = time.Now().UTC().UnixMilli()
	c.pool.mu.Unlock()
	second := c.pickAccount(req, nil)
	if second == nil || second.id() != "a2" {
		t.Fatalf("second pick = %#v, want a2 pinned", second)
	}
}

func TestPickAccountRotatesWithoutAConversation(t *testing.T) {
	c := affinityTestClient(t)
	got := c.pickAccount(&core.ChatRequest{}, nil)
	if got == nil || got.id() != "a2" {
		t.Fatalf("unscoped pick = %#v, want plain LRU a2", got)
	}
	if c.affinity.Count() != 0 {
		t.Fatal("unscoped request wrote an affinity binding")
	}
}

func TestPickAccountDropsADeadBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-2", "a1")
	if err := c.pool.setEnabled("a1", false); err != nil {
		t.Fatalf("disabling a1: %v", err)
	}

	got := c.pickAccount(&core.ChatRequest{ConversationID: "conv-2"}, nil)
	if got == nil || got.id() != "a2" {
		t.Fatalf("pick = %#v, want a2 after a1 was disabled", got)
	}
	req := &core.ChatRequest{ConversationID: "conv-2"}
	c.bindServedConversation(req, got.id())
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
