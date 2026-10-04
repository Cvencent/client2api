package qwenwork

// affinity.go — qwenwork's opt-in to core's conversation→account stickiness.
//
// Why it is worth having here.  qwenwork is the most rotation-hostile module in
// the tree: the upstream protocol is a signed "agent envelope" and every account
// carries its own COSY session (see cosy.go and sessionFor), so an account
// change mid-conversation is not a transparent hand-off.  The pool is strictly
// least-recently-used, which means that without this file the *second* turn of a
// conversation is almost guaranteed to leave the account that served the first:
// markUsed moves the account just used to the back of the queue.
//
//   - A conversation that stays put keeps its warmed COSY session and its
//     upstream context instead of rebuilding both on every turn.
//   - The vendor sees one credential per conversation rather than a walk
//     through the whole pool, which is the pattern the README's account-ban
//     warning is about.
//
// The key is free.  core.ConversationKey reads the request's options and user,
// so a caller that supplies conversation_id -- the gateway does, for every
// module that understands it -- gets stickiness with no protocol change, and a
// caller that supplies nothing gets an empty key, which turns the feature off
// for that request.
//
// The mechanism lives in internal/core (affinity.go) and knows nothing about
// qwenwork.  What lives here is the module half: the liveness predicate the
// table is fed, the pick path that consults it, and the three methods that make
// Client a core.ConversationBinder.

import "client2api/internal/core"

// usableEntry returns the live entry for one id when the pool could hand it out
// right now, and nil otherwise.
//
// It is the id-addressed twin of pick(): the same availableLocked verdict, the
// same skip set, taken under the same lock.  That is deliberate -- it is what
// guarantees stickiness can never serve an account the picker would have
// refused, and it is why the predicate below delegates rather than re-deriving
// "healthy" here.
func (p *pool) usableEntry(id string, skip map[string]bool) *entry {
	if p == nil || id == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if skip != nil && skip[id] {
		return nil
	}
	now := p.now()
	for _, e := range p.entries {
		if e.acct.id() != id {
			continue
		}
		if p.availableLocked(e, now) {
			return e
		}
		return nil
	}
	return nil
}

// usableFor is the liveness predicate the affinity table consults before it
// hands back a binding.
//
// It delegates to the pool rather than deciding here, so that "usable" means
// exactly what pick() means by it: an account that is disabled, cooling down,
// out of credit, expired, or inside the 60 s expiry margin must not resolve as
// a sticky binding.  Anything looser would let stickiness serve an account the
// picker would have refused.
//
// qwenwork's health is NOT model-scoped: availableLocked looks only at the
// credential and its own cooldown, and pick() ignores the model too.  model is
// therefore accepted for interface parity with the other modules and
// deliberately ignored -- that is the picker's own notion of usable, not a
// looser one.  It stays correct if the pool ever grows a per-model cooldown,
// because the closure is re-evaluated on every Resolve rather than baked in.
func (c *Client) usableFor(model string) func(accountID string) bool {
	return func(accountID string) bool {
		if c == nil {
			return false
		}
		return c.pool.usableEntry(accountID, nil) != nil
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
// disabled account is reported absent, which the table treats as "drop this
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

// pickAccount selects the account for one attempt.  It is the single pick point
// openChat uses, which is what makes the stickiness real rather than decorative.
//
// The order is the whole design:
//
//  1. If this conversation is bound to an account that is still usable for this
//     model and has not already failed earlier in this request, use it.  That is
//     the point of the feature.
//  2. Otherwise pick normally -- least-recently-used first, cooldown aware --
//     and bind the result.  The binding is written as soon as the account is
//     chosen, not after success, so two concurrent requests for the same new
//     conversation agree on one account instead of racing onto two.
//
// A binding that has gone bad is never served: core.Affinity.Resolve drops it
// the moment usableFor says no, and step 2 re-binds.  Stickiness is therefore an
// optimisation that can never turn into a failure and can never be honoured by
// serving a parked account.
//
// conversationKey may be empty, which means "this request is not
// conversation-scoped": the call then degenerates exactly into the pool's own
// pick, with no table traffic at all.
func (c *Client) pickAccount(skip map[string]bool, conversationKey, model string) *entry {
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok {
			if e := c.pool.usableEntry(id, skip); e != nil {
				return e
			}
		}
	}
	e := c.pool.pick(skip)
	if e == nil {
		return nil
	}
	if conversationKey != "" {
		c.affinity.Bind(conversationKey, e.acct.id())
	}
	return e
}

// ApplyLive implements core.LiveReloader.  qwenwork reads exactly two settings
// from the shared live config, both about stickiness: how long a binding lives
// and how often expired ones are swept.
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
	if c.deps.Logf != nil && (s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0) {
		c.deps.Log("qwenwork: conversation stickiness enabled=%t, window %s, swept every %s",
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
