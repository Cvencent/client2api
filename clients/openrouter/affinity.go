package openrouter

import (
	"strings"
	"time"

	"client2api/internal/core"
)

// affinity.go opts OpenRouter into core's conversation → account stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation is handed to a different credential than the first. OpenRouter
// routes each key to a different upstream account, so pinning a conversation
// keeps the upstream prompt cache warm and avoids credential churn.
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
// It delegates to the pool so "usable" means exactly what acquire() means by
// it. OpenRouter health is per key, not per model.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		a := c.pool.lookupLocked(strings.TrimSpace(accountID))
		return a != nil && c.pool.selectableLocked(a, c.now(), c.limit())
	}
}

// acquireAccount reserves the account for one attempt, preferring a live
// conversation binding. The binding itself is written by Chat only after the
// reservation succeeds, so a busy key that was skipped cannot capture the
// conversation.
func (c *Client) acquireAccount(req *core.ChatRequest) (accountRecord, error) {
	key := conversationKey(req)
	if key != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(key, c.usableFor("")); ok {
			if a, err := c.pool.acquireByID(id, c.now(), c.limit()); err == nil {
				return a, nil
			}
		}
	}
	return c.pool.acquire(c.now(), c.limit())
}

// bindServedConversation records the credential that actually received a slot.
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

// ApplyLive implements core.LiveReloader.  The affinity knobs now mean
// something here: they retune the conversation table without a restart.
func (c *Client) ApplyLive(s core.LiveSettings) {
	c.ensure()
	c.mu.Lock()
	if s.MaxInFlight != nil {
		v := *s.MaxInFlight
		c.liveLimit = &v
	}
	if s.Pool != nil {
		if s.Pool.BreakerThreshold != nil {
			v := *s.Pool.BreakerThreshold
			c.liveThreshold = &v
		}
		if s.Pool.BreakerCooldown != nil {
			c.liveCooldown = *s.Pool.BreakerCooldown
		}
	}
	c.mu.Unlock()

	if c.affinity == nil {
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
}

// acquireByID reserves one in-flight slot on a specific credential when it is
// selectable right now.  It applies the same admission policy as acquire().
func (p *pool) acquireByID(id string, now time.Time, limit int) (accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(strings.TrimSpace(id))
	if a == nil || !p.selectableLocked(a, now, limit) {
		return accountRecord{}, core.ErrBusy
	}
	a.inFlight++
	a.lastUsed = now
	return *a, nil
}
