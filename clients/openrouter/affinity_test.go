package openrouter

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// affinityTestClient builds a two-key pool whose LRU order is known.
func affinityTestClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, twoKeysCfg(), nil)
	c.pool.mu.Lock()
	if len(c.pool.accts) != 2 {
		c.pool.mu.Unlock()
		t.Fatalf("pool has %d credentials, want 2", len(c.pool.accts))
	}
	c.pool.accts[0].lastUsed = testBase
	c.pool.accts[1].lastUsed = testBase.Add(-time.Hour)
	c.pool.mu.Unlock()
	c.affinity = core.NewAffinity(0)
	return c
}

// idOfKey maps a raw key back to the pool's stable identifier.
func idOfKey(t *testing.T, c *Client, key string) string {
	t.Helper()
	for _, a := range c.pool.snapshot() {
		if a.APIKey == key {
			return a.ID
		}
	}
	t.Fatalf("key %q is not in the pool", key)
	return ""
}

func TestAcquireAccountKeepsConversationOnItsCredential(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	first, err := c.acquireAccount(req)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	c.pool.release(first.ID)
	c.bindServedConversation(req, first.ID)
	if got, ok := c.ConversationAccount("conv-1", "auto"); !ok || got != first.ID {
		t.Fatalf("binding = %q, %v; want %q", got, ok, first.ID)
	}

	// Make every credential equally idle; the binding must still win.
	c.pool.mu.Lock()
	for _, a := range c.pool.accts {
		a.lastUsed = testBase
	}
	c.pool.mu.Unlock()
	second, err := c.acquireAccount(req)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("second credential = %q, want pinned %q", second.ID, first.ID)
	}
	c.pool.release(second.ID)
}

func TestAcquireAccountRotatesWithoutAConversation(t *testing.T) {
	c := affinityTestClient(t)
	got, err := c.acquireAccount(&core.ChatRequest{})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	c.pool.release(got.ID)
	if c.affinity.Count() != 0 {
		t.Fatal("unscoped request wrote an affinity binding")
	}
}

func TestAcquireAccountDropsADeadBinding(t *testing.T) {
	c := affinityTestClient(t)
	disabledID := idOfKey(t, c, testKey)
	c.BindConversation("conv-2", disabledID)
	if !c.pool.setEnabled(disabledID, false) {
		t.Fatal("disabling the key reported no change")
	}

	req := &core.ChatRequest{ConversationID: "conv-2"}
	got, err := c.acquireAccount(req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got.ID == disabledID {
		t.Fatalf("acquire returned the disabled credential %q", got.ID)
	}
	c.pool.release(got.ID)
	c.bindServedConversation(req, got.ID)
	if id, ok := c.ConversationAccount("conv-2", "auto"); !ok || id != got.ID {
		t.Fatalf("binding = %q, %v; want %q after rebind", id, ok, got.ID)
	}
}

func TestUnbindConversationForgetsTheBinding(t *testing.T) {
	c := affinityTestClient(t)
	c.BindConversation("conv-3", testKey)
	if !c.UnbindConversation("conv-3") {
		t.Fatal("UnbindConversation reported no binding")
	}
	if _, ok := c.ConversationAccount("conv-3", "auto"); ok {
		t.Fatal("binding survived UnbindConversation")
	}
}
