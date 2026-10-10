package qoder

import (
	"strings"

	"client2api/internal/core"
)

// affinity.go opts Qoder into core's conversation → account stickiness.
//
// Why it matters here.  chatContext sends the caller's conversation id to the
// vendor as `session_id`, and the gateway uses it to group one conversation's
// turns.  The pool is least-recently-used, so without a binding the second turn
// of a conversation is handed to the account that was used *least* recently --
// almost never the one that served the first.  That breaks the vendor's own
// session grouping, re-warms a cold COSY session on every turn, and walks one
// conversation through the whole pool, which is exactly the credential-churn
// pattern the module's cooldown logic exists to avoid.
//
// The key is free: core.ConversationKeyOf reads the conversation id the gateway
// already resolved, so a caller that names a conversation gets stickiness with
// no protocol change, and an unscoped request keeps rotating exactly as before.
//
// The table lives in internal/core; what lives here is the liveness predicate,
// the ordering hook Chat uses, and the three methods that make Client a
// core.ConversationBinder.

var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
)

// conversationKey derives the stickiness key for one request, or "" when the
// request is not conversation-scoped.
func conversationKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(core.ConversationKeyOf(req))
}

// usableFor is the predicate the affinity table applies to a stored binding.
//
// It answers the same question the picker asks -- would this credential be
// handed a request right now -- so a binding can never keep a conversation on
// an account the pool would have refused.  The model is accepted for interface
// parity; Qoder's health is per credential, not per model.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.store == nil {
			return false
		}
		acc, ok := c.store.lookup(strings.TrimSpace(accountID))
		if !ok {
			return false
		}
		return acc.selectable(nowUTC())
	}
}

// orderedCandidates returns the pool's rotation with a live conversation
// binding moved to the front.
//
// The binding is not written here: a candidate can still be skipped because its
// in-flight slot is full, and pinning a conversation to an account that never
// served the request would make the next turn miss the account that did.  Chat
// binds once a slot has actually been acquired.
//
// The fallback is the pool's own order, so a request with no conversation key
// degenerates exactly into the behaviour that existed before this file.
func (c *Client) orderedCandidates(req *core.ChatRequest, model string) []account {
	candidates := c.store.candidates(nowUTC())
	if len(candidates) == 0 {
		return candidates
	}
	key := conversationKey(req)
	if key == "" || c.affinity == nil {
		return candidates
	}

	if id, ok := c.affinity.Resolve(key, c.usableFor(model)); ok {
		for i := range candidates {
			if candidates[i].ID != id {
				continue
			}
			bound := candidates[i]
			copy(candidates[1:i+1], candidates[:i])
			candidates[0] = bound
			break
		}
	}
	return candidates
}

// bindServedConversation records the account that actually received a slot.
// It is called from Chat, after AcquireAccountSlot succeeds, so the binding can
// never name an account the request did not reach.  An empty key means the
// request was not conversation-scoped and nothing is written.
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

// BindConversation implements core.ConversationBinder: how an operator or a test
// pins a conversation without going through a chat request.
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

// UnbindConversation implements core.ConversationBinder: the administrative
// "forget this conversation" hook.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil || c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount implements core.ConversationBinder.  It answers the bound
// account only while that credential could still serve, so a parked or disabled
// account is reported absent and the caller picks normally.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil || c.affinity == nil {
		return "", false
	}
	return c.affinity.Resolve(strings.TrimSpace(conversationKey), c.usableFor(model))
}

// ApplyLive implements core.LiveReloader: the operator's session_sticky settings
// retune the binding window without a restart.  A zero value means the file said
// nothing, so what is in force is kept.
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
		c.logf("qoder: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
