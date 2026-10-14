package gemini

import (
	"strings"

	"client2api/internal/core"
)

// affinity.go opts Gemini into core's conversation → account stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation lands on a different account than the first.  Gemini's
// server-side session state (and its prompt cache) is account-scoped, so
// pinning the conversation keeps the session warm.
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

// usableFor is the affinity predicate; it defers to the pool so "usable" means
// exactly what acquire means by it.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		a := c.pool.find(strings.TrimSpace(accountID))
		if a == nil {
			return false
		}
		return c.pool.selectable(a, c.now(), c.cfg.maxInFlight())
	}
}

// acquireAccount reserves an account for one attempt, preferring a live
// binding.
func (c *Client) acquireAccount(req *core.ChatRequest) (*accountRecord, error) {
	key := conversationKey(req)
	limit := c.cfg.maxInFlight()
	if key != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(key, c.usableFor(req.Model)); ok {
			if acct, err := c.pool.acquireByID(id, c.now(), limit); err == nil {
				return acct, nil
			}
		}
	}
	return c.pool.acquire(c.now(), limit)
}

// bindServedConversation records the account that actually got the slot.
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

// ConversationBindings reports the live bindings.
func (c *Client) ConversationBindings() []core.ConversationBinding {
	if c == nil || c.affinity == nil {
		return nil
	}
	return c.affinity.Bindings()
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
		c.deps.Log("gemini: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
