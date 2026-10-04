package zcode

// affinity.go — zcode's opt-in to core's conversation→account stickiness.
//
// zcode rotates credentials with a cursor (pool.next) and, unlike workbuddy, it
// does not salt a prompt-cache key per account, so stickiness is not a billing
// requirement here the way it is there.  It is still worth having, for reasons
// that do apply to this module:
//
//   - A zcode account that trips risk control (3012) is parked for the whole
//     cooldown window, and a fresh account means a fresh risk profile: every
//     rotation re-pays the warm-up on an endpoint that answers unusual activity
//     with a block rather than an error.  A conversation that keeps landing on a
//     different credential is a conversation that keeps re-rolling that dice.
//   - The key is free.  core.ConversationKey reads the request's options and
//     user, so a caller that supplies conversation_id (the panel's chat tab and
//     the gateway both do) gets stickiness with no protocol change, and a caller
//     that supplies nothing gets an empty key, which turns the feature off for
//     that request — see pickAccount.
//
// The mechanism lives in internal/core (affinity.go) and knows nothing about
// zcode.  What lives here is the module half.

import "client2api/internal/core"

// usableFor is the liveness predicate the affinity table consults before it
// hands back a binding.
//
// It delegates to the pool rather than deciding here, so that "usable" means
// exactly what the cursor picker means by it: disabled, invalid and exhausted
// accounts are out, a cooling account is out until its cooldown elapses, and a
// JWT account is out while no captcha solver is configured — because
// pool.selectableLocked refuses it and the request would fail with
// core.ErrNotConfigured.  Anything looser would let stickiness serve an account
// the picker would have refused.
//
// model is accepted for interface symmetry with the model-scoped modules and is
// deliberately unused: zcode's lifecycle state is per account, not per model, so
// there is no narrower question to ask.  A caller that passes "" is not asking a
// different question.
func (c *Client) usableFor(model string) func(accountID string) bool {
	return func(accountID string) bool {
		return c.pool.usableByID(accountID)
	}
}

// BindConversation implements core.ConversationBinder: how an operator or a test
// pins a conversation without going through a chat request.
func (c *Client) BindConversation(conversationKey, accountID string) {
	if c == nil {
		return
	}
	c.affinity.Bind(conversationKey, accountID)
}

// UnbindConversation implements core.ConversationBinder: the administrative
// "forget this session" hook.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil {
		return false
	}
	return c.affinity.Unbind(conversationKey)
}

// ConversationAccount implements core.ConversationBinder.  It answers with the
// bound account only while that account can still serve a request; a parked,
// cooling or exhausted account is reported absent, which the table treats as
// "drop this binding and pick normally".
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil {
		return "", false
	}
	return c.affinity.Resolve(conversationKey, c.usableFor(model))
}

// conversationKey derives the stickiness key for one request, or "" when the
// request is not conversation-scoped.
func conversationKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return core.ConversationKeyOf(req)
}

// pickAccount selects the account for one attempt.
//
// The order is the whole design:
//
//  1. If this conversation is bound to an account that is still usable, use it.
//     That is the point of the feature.  The account is taken from the pool by
//     id, and it is added to skip so the rotation loop cannot retry it.
//  2. Otherwise pick normally — the cursor rotation, exactly as before — and
//     bind the result.  The binding is written as soon as the account is chosen,
//     not after success, so two concurrent requests for the same new
//     conversation agree on one account instead of racing onto two.
//
// A binding that has gone bad is never served: core.Affinity.Resolve drops it
// the moment usableFor says no, and step 2 re-binds.  Stickiness is therefore an
// optimisation that can never turn into a failure and can never be honoured by
// serving a parked account.
//
// conversationKey may be empty, which means "this request is not
// conversation-scoped": the call then degenerates exactly into pool.next, with
// no table traffic at all.
//
// A bound account the caller has already skipped in this request is not
// resurrected: skip means "this credential already failed for this request", and
// honouring the binding would burn an attempt re-running a credential the loop
// deliberately excluded.  Falling through re-binds to the next pick, so the
// conversation moves on rather than sticking to a credential that is failing.
func (c *Client) pickAccount(skip map[string]bool, conversationKey, model string) *Account {
	if skip == nil {
		skip = map[string]bool{}
	}
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok && !skip[id] {
			if a := c.pool.find(id); a != nil {
				skip[id] = true
				return a
			}
		}
	}
	a := c.pool.next(skip)
	if a != nil && conversationKey != "" {
		c.affinity.Bind(conversationKey, a.ID)
	}
	return a
}

// ApplyLive implements core.LiveReloader.  zcode reads exactly two settings from
// the shared live config, both about stickiness: how long a binding lives and how
// often expired ones are swept.
//
// They arrive here rather than at construction because the operator can change
// them and reload without a restart, and because a zero value means "the file
// said nothing": the table then keeps the default it was built with instead of
// being pinned to a window of zero, which would quietly disable the feature.
func (c *Client) ApplyLive(s core.LiveSettings) {
	if c == nil {
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
	if s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0 {
		c.deps.Log("zcode: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}

// The live settings and the conversation binder are optional capabilities in
// core: a renamed method would otherwise surface only as the panel quietly
// reporting the module as incapable.
var (
	_ core.LiveReloader       = (*Client)(nil)
	_ core.ConversationBinder = (*Client)(nil)
)
