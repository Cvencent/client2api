package workbuddy

// affinity.go — WorkBuddy's opt-in to core's conversation→account stickiness.
//
// Why WorkBuddy needs it more than most: this module salts the vendor's
// prompt_cache_key with the account uid (see InjectPromptCacheKey in prompt.go,
// called from Upstream.ChatStream).  The same conversation served by a second
// account therefore produces a second cache key, and the vendor re-bills the
// whole prefix.  Account rotation is a cost decision here, not just a latency
// one, so a conversation must stay on the account that warmed its cache.
//
// The mechanism itself lives in internal/core (affinity.go) and knows nothing
// about WorkBuddy.  What lives here is the module half: the liveness predicate
// the table is fed, the pick path that consults it, and the three methods that
// make Client a core.ConversationBinder.

import "client2api/internal/core"

// usableFor is the liveness predicate the affinity table consults before it
// hands back a binding.
//
// It delegates to the pool rather than deciding here, because "usable" has to
// mean exactly what the pick path means by it, per model and per realm: an
// account that is parked, that is cooling down for the model this conversation
// is about to ask for, or that belongs to a realm the model cannot be served
// from, must not be resolved as a sticky binding.  Anything looser would let
// stickiness serve an account the picker would have refused.
func (c *Client) usableFor(model, realm string) func(accountID string) bool {
	return func(accountID string) bool {
		return c.pool.UsableForModelInRealm(accountID, model, realm)
	}
}

// BindConversation implements core.ConversationBinder.  It is how an operator or
// a test pins a conversation without going through a chat request.
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
	r := routeModel(model)
	return c.affinity.Resolve(conversationKey, c.usableFor(r.bare, r.affinityRealm))
}

// pickAccount selects the account for one attempt.
//
// The order is the whole design:
//
//  1. If this conversation is bound to an account that is still usable for this
//     model and realm, use it.  That is the point of the feature.
//  2. Otherwise pick normally -- model-aware, realm-aware and scored -- and bind
//     the result.  The binding is written as soon as the account is chosen, not
//     after success, so two concurrent requests for the same new conversation
//     agree on one account instead of racing onto two and splitting the cache.
//
// The realm preference list is walked in order, which is where the bare-name
// softness lives: "cn:glm-5.2" has exactly one entry and fails honestly when no
// domestic account can serve it, while a bare "glm-5.2" prefers the domestic
// realm and then accepts whatever is left.
//
// A binding that has gone bad is never served: core.Affinity.Resolve drops it
// the moment usableFor says no, and step 2 re-binds.  Stickiness is therefore an
// optimisation that can never turn into a failure and can never be honoured by
// serving a parked account.
//
// conversationKey may be empty, which means "this request is not
// conversation-scoped": the call then degenerates exactly into the model-aware
// pick, with no table traffic at all.
func (c *Client) pickAccount(skip map[string]bool, conversationKey string, r modelRoute) (*Auth, bool) {
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(r.bare, r.affinityRealm)); ok {
			if a, ok := c.pool.Find(skip, id); ok {
				return a, true
			}
		}
	}
	var a *Auth
	ok := false
	for _, realm := range r.realms {
		if a, ok = c.pool.PickForModelInRealm(skip, r.bare, realm); ok {
			break
		}
	}
	if !ok {
		return nil, false
	}
	if conversationKey != "" {
		c.affinity.Bind(conversationKey, a.ID())
	}
	return a, true
}

// conversationKey derives the stickiness key for one request.
//
// core.ConversationKey is the same precedence the reference uses (explicit
// conversation ids first, then the vendor's prompt_cache_key), and it falls back
// to the request's user.  The meta the upstream call already carries is the last
// resort so that the key used for stickiness and the key used for the cache salt
// can never disagree.
//
// When none of those name a conversation, the reference derives one from the
// message content instead (its README's 粘性会话内容回退): without it a client
// that sends no conversation id at all rotates accounts every turn, and every
// turn re-bills the whole prefix because the cache the previous account had
// already warmed is unreachable.  The derived key carries the "d-" namespace so
// it can never be mistaken for an id a client really sent, and it is the last
// resort — every explicit spelling still wins.
func conversationKey(req *core.ChatRequest, meta ChatMeta) string {
	if req == nil {
		return meta.ConversationID
	}
	if k := core.ConversationKeyOf(req); k != "" {
		return k
	}
	if meta.ConversationID != "" {
		return meta.ConversationID
	}
	return core.DeriveConversationKey(req.Messages)
}
