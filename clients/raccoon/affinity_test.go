package raccoon

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
	c := newTestClient(t, t.TempDir(), "{}", nil)
	for _, a := range []account{
		{ID: "a1", Label: "a1", AccessToken: testToken(t), UserID: "u1", Origin: originStored},
		{ID: "a2", Label: "a2", AccessToken: testToken(t), UserID: "u2", Origin: originStored},
	} {
		c.pool.put(a)
	}
	c.pool.markUsed(c.pool.find("a1"))
	c.pool.find("a1").acct.LastUsed = now.UnixMilli()
	c.pool.find("a2").acct.LastUsed = now.Add(-time.Hour).UnixMilli()
	c.affinity = core.NewAffinity(0)
	return c
}

func testToken(t *testing.T) string {
	t.Helper()
	return jwtWith(t, map[string]any{"exp": time.Now().Add(3 * time.Hour).Unix()})
}

func TestPickAccountKeepsConversationOnItsAccount(t *testing.T) {
	c := affinityTestClient(t)
	req := &core.ChatRequest{ConversationID: "conv-1"}

	first := c.pickAccount(nil, conversationKey(req))
	if first == nil || first.acct.id() != "a2" {
		t.Fatalf("first pick = %#v, want a2", first)
	}
	c.bindServedConversation(req, first.acct.id())
	if got, ok := c.ConversationAccount("conv-1", "auto"); !ok || got != "a2" {
		t.Fatalf("binding = %q, %v; want a2", got, ok)
	}

	c.pool.markUsed(first)
	second := c.pickAccount(nil, conversationKey(req))
	if second == nil || second.acct.id() != "a2" {
		t.Fatalf("second pick = %#v, want a2 pinned", second)
	}
}

func TestPickAccountRotatesWithoutAConversation(t *testing.T) {
	c := affinityTestClient(t)
	got := c.pickAccount(nil, "")
	if got == nil || got.acct.id() != "a2" {
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

	got := c.pickAccount(nil, "conv-2")
	if got == nil || got.acct.id() != "a2" {
		t.Fatalf("pick = %#v, want a2 after a1 was disabled", got)
	}
	req := &core.ChatRequest{ConversationID: "conv-2"}
	c.bindServedConversation(req, got.acct.id())
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
