package opencode

import (
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// affinity.go opts OpenCode Zen into core's conversation → account stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation is handed to a different account than the first. The upstream
// session is account-scoped, so pinning the conversation keeps it warm and
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
// It delegates to the pool so "usable" means exactly what acquire() means by
// it. OpenCode's health is per credential, not per model.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		c.pool.mu.Lock()
		defer c.pool.mu.Unlock()
		a := c.pool.find(strings.TrimSpace(accountID))
		return selectable(a, c.now(), c.cfg.maxInFlight())
	}
}

// acquireAccount reserves the account for one attempt, preferring a live
// conversation binding. The binding itself is written by openChat only after
// this reservation succeeds, so a busy account that was skipped cannot capture
// the conversation.
func (c *Client) acquireAccount(req *core.ChatRequest) (*accountRecord, error) {
	c.ensure()
	key := conversationKey(req)
	limit := c.cfg.maxInFlight()
	if key != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(key, c.usableFor("")); ok {
			if a, err := c.pool.acquireByID(id, c.now(), limit); err == nil {
				return a, nil
			}
		}
	}
	return c.pool.acquire(c.now(), limit)
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
		c.deps.Log("opencode: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}

// acquireByID reserves one in-flight slot on a specific account when it is
// selectable right now. It is the affinity-aware twin of acquire: the caller
// has already chosen the account, but the pool still enforces every admission
// rule before handing out the slot.
func (p *pool) acquireByID(id string, now time.Time, limit int) (*accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(strings.TrimSpace(id))
	if !selectable(a, now, limit) {
		return nil, fmt.Errorf("%w: account %s is not selectable", core.ErrBusy, id)
	}
	a.inFlight++
	a.lastUsed = now
	cp := *a
	return &cp, nil
}
