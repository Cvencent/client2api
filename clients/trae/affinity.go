package trae

// affinity.go — trae's opt-in to core's conversation→account stickiness.
//
// trae's body carries no conversation id of its own and salts no prompt cache
// key per account, so stickiness here is not a cache-cost requirement the way it
// is for workbuddy.  It is still worth having for two reasons that do apply:
//
//   - Mid-conversation failover is expensive.  trae's accounts carry plan
//     (1005) and quota (4008) parks that last 12h and 6h; a conversation that
//     keeps getting handed a fresh account is a conversation that keeps paying
//     the warm-up and risks switching models of behaviour halfway through.
//   - The key is free.  core.ConversationKey reads the request's options and
//     user, so a caller that supplies conversation_id (the gateway does, for
//     every module that understands it) gets stickiness with no protocol
//     change, and a caller that supplies nothing gets an empty key, which turns
//     the whole feature off for that request -- see pickAccount.
//
// The mechanism lives in internal/core (affinity.go) and knows nothing about
// trae.  What lives here is the module half.

import "client2api/internal/core"

// usableFor is the liveness predicate the affinity table consults before it
// hands back a binding.
//
// It delegates to the pool rather than deciding here, so that "usable" means
// exactly what the scored picker means by it, per model: an account that is
// parked, or cooling down for the model this conversation is about to ask for,
// must not resolve as a sticky binding.  Anything looser would let stickiness
// serve an account the picker would have refused.
func (c *Client) usableFor(model string) func(accountID string) bool {
	return func(accountID string) bool {
		return c.pool.UsableForModel(accountID, model)
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
// bound account only while that account can still serve model; a parked or
// model-cooled account is reported absent, which the table treats as "drop this
// binding and pick normally".
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
//  1. If this conversation is bound to an account that is still usable for this
//     model, use it.  That is the point of the feature.
//  2. Otherwise pick normally -- model-aware and scored -- and bind the result.
//     The binding is written as soon as the account is chosen, not after
//     success, so two concurrent requests for the same new conversation agree on
//     one account instead of racing onto two.
//
// A binding that has gone bad is never served: core.Affinity.Resolve drops it
// the moment usableFor says no, and step 2 re-binds.  Stickiness is therefore an
// optimisation that can never turn into a failure and can never be honoured by
// serving a parked account.
//
// conversationKey may be empty, which means "this request is not
// conversation-scoped": the call then degenerates exactly into the scored pick,
// with no table traffic at all.
func (c *Client) pickAccount(skip map[string]bool, conversationKey, model string) (*Auth, bool) {
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok {
			if a, ok := c.pool.Find(skip, id); ok {
				return a, true
			}
		}
	}
	a, ok := c.pool.PickForModel(skip, model)
	if !ok {
		return nil, false
	}
	if conversationKey != "" {
		c.affinity.Bind(conversationKey, a.ID())
	}
	return a, true
}

// ApplyLive implements core.LiveReloader.  trae reads exactly two settings from
// the shared live config, both about stickiness: how long a binding lives and
// how often expired ones are swept.
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
	if c.log != nil && (s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0) {
		c.log("trae: conversation stickiness enabled=%t, window %s, swept every %s",
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
