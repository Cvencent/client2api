package raccoon

import (
	"strings"

	"client2api/internal/core"
)

// affinity.go opts Raccoon into core's conversation → account stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation is handed to a different account than the first. This module's
// upstream session is account-scoped, and walking a conversation through the
// pool re-warms it on every turn. Pinning the conversation keeps the session
// warm and avoids that credential churn.
//
// The key is free: core.ConversationKeyOf reads the conversation id the
// gateway already resolved. An unscoped request keeps rotating exactly as
// before.
var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
)

func conversationKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(core.ConversationKeyOf(req))
}

// usableFor is the predicate the affinity table applies to a stored binding.
// It delegates to the pool so "usable" means exactly what pick() means by it.
// Raccoon's health is per credential, not per model.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		e := c.pool.find(strings.TrimSpace(accountID))
		if e == nil {
			return false
		}
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		return c.pool.usableLocked(e, c.pool.now())
	}
}

// pickAccount chooses the next candidate, preferring a live conversation
// binding. The binding is written only after the slot is genuinely acquired in
// openChat, so a skipped busy account cannot capture the conversation.
func (c *Client) pickAccount(skip map[string]bool, key string) *entry {
	if key != "" {
		if id, ok := c.affinity.Resolve(key, c.usableFor("")); ok {
			if e := c.pool.find(id); e != nil && (skip == nil || !skip[id]) {
				c.pool.mu.Lock()
				usable := c.pool.usableLocked(e, c.pool.now())
				c.pool.mu.Unlock()
				if usable {
					return e
				}
			}
		}
	}
	return c.pool.pick(skip)
}

// bindServedConversation records the account that actually received a slot.
func (c *Client) bindServedConversation(req *core.ChatRequest, accountID string) {
	if c == nil || c.affinity == nil {
		return
	}
	key := conversationKey(req)
	id := strings.TrimSpace(accountID)
	if key == "" || id == "" {
		return
	}
	c.affinity.Bind(key, id)
}

// BindConversation implements core.ConversationBinder.
func (c *Client) BindConversation(conversationKey, accountID string) {
	if c == nil || c.affinity == nil {
		return
	}
	key := strings.TrimSpace(conversationKey)
	id := strings.TrimSpace(accountID)
	if key == "" || id == "" {
		return
	}
	c.affinity.Bind(key, id)
}

// UnbindConversation implements core.ConversationBinder.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil || c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount implements core.ConversationBinder.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil || c.affinity == nil {
		return "", false
	}
	return c.affinity.Resolve(strings.TrimSpace(conversationKey), c.usableFor(model))
}

// ApplyLive implements core.LiveReloader.
func (c *Client) ApplyLive(s core.LiveSettings) {
	if c == nil || c.affinity == nil {
		return
	}
	if s.AffinityEnabled != nil {
		c.affinity.SetEnabled(*s.AffinityEnabled)
	}
	if s.AffinityTTL > 0 {
		c.affinity.SetTTL(s.AffinityTTL)
	}
	if s.AffinityGCInterval > 0 {
		c.affinity.SetGCInterval(s.AffinityGCInterval)
	}
	if c.deps.Logf != nil && (s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0) {
		c.deps.Log("raccoon: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
