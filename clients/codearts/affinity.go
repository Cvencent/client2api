package codearts

import (
	"strings"
	"time"

	"client2api/internal/core"
)

// affinity.go opts Huawei CodeArts into core's conversation → account
// stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation is handed to a different credential than the first. CodeArts'
// SSE conversation is account-scoped, so pinning it keeps the session warm and
// avoids credential churn.
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
// CodeArts' health is per credential, not per model.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		e := c.pool.findLocked(strings.TrimSpace(accountID))
		return c.pool.availableLocked(e, time.Now())
	}
}

// pickAccount chooses the next candidate, preferring a live conversation
// binding. The binding is written by Chat only after this pick is accepted.
func (c *Client) pickAccount(req *core.ChatRequest, skip map[string]bool) *entry {
	key := conversationKey(req)
	if key != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(key, c.usableFor("")); ok {
			c.pool.mu.Lock()
			e := c.pool.findLocked(id)
			usable := e != nil && !skip[id] && c.pool.availableLocked(e, time.Now())
			c.pool.mu.Unlock()
			if usable {
				return e
			}
		}
	}
	return c.pool.pick(skip)
}

// bindServedConversation records the credential that actually received the
// request.
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
		c.deps.Log("codearts: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
