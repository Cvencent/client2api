package opencode

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// affinityTestClient builds a two-account pool whose LRU order is known: a2 was
// used an hour ago and a1 a moment ago, so plain rotation starts at a2.
func affinityTestClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, Config{})
	addAccount(t, c, "a1", "sk-a1")
	addAccount(t, c, "a2", "sk-a2")
	c.pool.mu.Lock()
	c.pool.find("a1").lastUsed = testNow
	c.pool.find("a2").lastUsed = testNow.Add(-time.Hour)
	c.pool.mu.Unlock()
	// The cursor starts at a2 so an unscoped request proves plain rotation.
	c.pool.mu.Lock()
	c.pool.rr = 1
	c.pool.mu.Unlock()
	c.affinity = core.NewAffinity(0)
	return c
}

func TestAcquireAccountKeepsConversationOnItsAccount(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	first, err := c.acquireAccount(req)
	if err != nil || first == nil || first.ID != "a2" {
		t.Fatalf("first acquire = %#v, %v; want a2", first, err)
	}
	c.pool.release(first.ID)
	c.bindServedConversation(req, first.ID)
	if got, ok := c.ConversationAccount("conv-1", "auto"); !ok || got != "a2" {
		t.Fatalf("binding = %q, %v; want a2", got, ok)
	}

	// Simulate the LRU moving a2 to the back: the binding must still win.
	c.pool.mu.Lock()
	c.pool.find("a2").lastUsed = testNow
	c.pool.mu.Unlock()
	// Simulate the round-robin cursor moving on: the binding must still win.
	c.pool.mu.Lock()
	c.pool.rr = 0
	c.pool.mu.Unlock()
	second, err := c.acquireAccount(req)
	if err != nil || second == nil || second.ID != "a2" {
		t.Fatalf("second acquire = %#v, %v; want a2 pinned", second, err)
	}
	c.pool.release(second.ID)
}

func TestAcquireAccountRotatesWithoutAConversation(t *testing.T) {
	c := affinityTestClient(t)
	got, err := c.acquireAccount(&core.ChatRequest{})
	if err != nil || got == nil || got.ID != "a2" {
		t.Fatalf("unscoped acquire = %#v, %v; want plain LRU a2", got, err)
	}
	c.pool.release(got.ID)
	if c.affinity.Count() != 0 {
		t.Fatal("unscoped request wrote an affinity binding")
	}
}

func TestAcquireAccountDropsADeadBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-2", "a1")
	if !c.pool.setEnabled("a1", false) {
		t.Fatal("disabling a1 reported no change")
	}

	req := &core.ChatRequest{ConversationID: "conv-2"}
	got, err := c.acquireAccount(req)
	if err != nil || got == nil || got.ID != "a2" {
		t.Fatalf("acquire = %#v, %v; want a2 after a1 was disabled", got, err)
	}
	c.pool.release(got.ID)
	c.bindServedConversation(req, got.ID)
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
