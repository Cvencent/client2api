package loomy

import (
	"strings"
	"time"

	"client2api/internal/core"
)

// affinity.go opts Loomy into core's conversation → account stickiness.
//
// The pool is least-recently-used, so without a binding the second turn of a
// conversation is handed to a different account than the first. Loomy's
// upstream is a signed session envelope, and account changes mid-conversation
// walk one conversation through the whole pool. Pinning it keeps the session
// warm and avoids that credential churn.
//
// The key is free: core.ConversationKeyOf reads the conversation id the
// gateway already resolved, so a caller that names a conversation gets
// stickiness with no protocol change, and an unscoped request keeps rotating
// exactly as before.
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
// It answers the same question the picker asks -- would this credential be
// handed a request right now -- so a binding can never keep a conversation on
// an account the pool would have refused. Loomy's health is per credential, not
// per model.
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
		return acc.selectable(time.Now().UTC())
	}
}

// orderedCandidates returns the pool's rotation with a live conversation
// binding moved to the front. The binding is not written here: a candidate can
// still be skipped because its in-flight slot is full, and pinning a
// conversation to an account that never served the request would make the next
// turn miss the account that did. Chat binds once a slot has actually been
// acquired.
func (c *Client) orderedCandidates(req *core.ChatRequest, model string) []account {
	candidates := c.store.candidates(time.Now().UTC())
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
		c.deps.Log("loomy: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
