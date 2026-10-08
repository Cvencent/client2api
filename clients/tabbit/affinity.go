package tabbit

// affinity.go — tabbit's opt-in to core's conversation→account stickiness.
//
// WHAT AN ACCOUNT IS HERE.  tabbit has no vendor token to manage: the thing an
// operator adds is an ENDPOINT, and the panel store (accounts.go) is the account
// list.  Two kinds share that store and one id namespace:
//
//   - a "sidecar" account is a local tabbit2api bridge, and its id is the
//     canonical base URL, because for an endpoint the URL *is* the identity
//     (normalizeBaseURL, accounts.go:205).
//   - a "web-token" account is a web.tabbit.com browser session, and its id is
//     "tabbit-web:" + the session cookie's uid (webAccountID, web.go:145).
//
// So an "account id" is a storedEndpoint.ID, and a conversation can be pinned to
// one of them.
//
// WHY STICKINESS IS WORTH HAVING EVEN THOUGH THIS PICKER IS NOT SCORED.  The
// request path picks the FIRST enabled account of the transport it routes to --
// firstEnabledWeb (web.go:218) or firstEnabledStored (accounts.go:186), the
// latter reached through locate()'s precedence chain (config.go:282).  That is a
// deterministic pick, but it is not a single account: the store holds as many
// sidecar endpoints and as many browser sessions as the operator adds, and only
// the first of each is ever used.  Without a binding the conversation's account
// is therefore "whatever is first in the store right now", and it changes the
// moment the operator adds, disables or reorders an account.  The binding is
// what makes the panel's "which account serves this conversation" answer stable
// instead of incidental.
//
// The mechanism lives in internal/core (affinity.go) and knows nothing about
// tabbit.  What lives here is the module half.

import (
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// usableFor is the liveness predicate the affinity table consults before it
// hands back a binding.
//
// It answers the same question the request path answers when it is about to hand
// an account a request: is this account, as it stands right now, one the module
// would accept?  That is: the id names a stored account, the account is enabled,
// it carries the credential its kind needs, its cookie has not expired locally,
// and it has not been proven dead by an earlier call -- the same non-transient
// verdict webRoute (web.go:260) consults before it abandons the web transport.
//
// Anything looser would let stickiness serve an account the picker would have
// refused: a disabled endpoint, an empty cookie, an expired session, a session
// the vendor already rejected.
//
// model is accepted because the binder interface is per-model and the table is
// fed one predicate per call, but tabbit deliberately ignores it.  This module
// has no per-model account state to consult: no scored pool, no model-scoped
// cooldown, no per-model entitlement.  Every account here serves every model the
// vendor lists, or none of them; the model a request names is resolved against
// the catalogue later (webModelEntry, web.go:462) and never decides which
// account serves it.  Making usableFor model-aware here would mean inventing a
// rule the picker does not have.
func (c *Client) usableFor(model string) func(accountID string) bool {
	return func(accountID string) bool {
		return c.usableAccount(accountID)
	}
}

// usableAccount reports whether accountID names an account the module could hand
// a request to right now, on the transport that account belongs to.
func (c *Client) usableAccount(accountID string) bool {
	if c == nil || strings.TrimSpace(accountID) == "" {
		return false
	}
	ep, ok := c.storedByID(accountID)
	if !ok {
		return false
	}
	return c.endpointUsable(ep, epKind(ep) == kindWebToken)
}

// storedByID finds an account in the panel store.
//
// It deliberately does not fall back to the implicitly resolved endpoint the way
// lookupEndpoint (accounts.go:744) does: an endpoint that exists only in the
// config, the environment, the state file or the documented default is not an
// account.  It has no id an operator could have pinned a conversation to, and
// honouring a binding against it would let a stale table row outrank
// clients.tabbit.base_url.
func (c *Client) storedByID(id string) (storedEndpoint, bool) {
	if c == nil || strings.TrimSpace(id) == "" {
		return storedEndpoint{}, false
	}
	for _, ep := range c.endpointsSnapshot() {
		if ep.ID == id {
			return ep, true
		}
	}
	return storedEndpoint{}, false
}

// endpointUsable is the module's own "may this account serve right now" test,
// for one transport.  web says which transport the request is about to take, so
// a sidecar account cannot be served by a web call and a browser session cannot
// be served by a sidecar call -- exactly the split webRoute (web.go:248) makes.
func (c *Client) endpointUsable(ep storedEndpoint, web bool) bool {
	if c == nil || !ep.Enabled {
		return false
	}
	if web {
		if epKind(ep) != kindWebToken || strings.TrimSpace(ep.Token) == "" {
			return false
		}
		if c.webCooling(ep.ID, time.Now()) {
			return false
		}
		claims, _ := parseWebToken(ep.Token)
		if exp := webTokenExpiry(claims); !exp.IsZero() && time.Now().After(exp) {
			// The cookie is locally known to be dead.  webStatus (webchat.go:560)
			// reports the same session as "invalid", so it must not resolve as
			// sticky either.
			return false
		}
		// A non-transient verdict is a verdict on the credential itself: the
		// vendor rejected it.  webRoute refuses to route to such a session, and
		// this predicate refuses to pin a conversation to it.
		if v, seen := c.webVerdictFor(ep.ID); seen && v.err != "" && !v.transient {
			return false
		}
		return true
	}
	return epKind(ep) == kindSidecar && strings.TrimSpace(ep.BaseURL) != ""
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
// bound account only while that account can still serve: a disabled endpoint, an
// expired cookie or a session the vendor already rejected is reported absent,
// which the table treats as "drop this binding and pick normally".
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

// pickWebSession chooses the browser session for one web request.
//
// The order is the whole design:
//
//  1. If this conversation is bound to a stored session that can still serve,
//     use it.  That is the point of the feature: it is how a conversation stays
//     on the session it started with instead of following store order.
//  2. Otherwise pick normally -- firstEnabledWeb, exactly as before -- and bind
//     the result.  The binding is written as soon as the session is CHOSEN, not
//     after success, so two concurrent requests for the same new conversation
//     agree on one session instead of racing onto two.
//
// A binding that has gone bad is never served: core.Affinity.Resolve drops it
// the moment usableFor says no, and endpointUsable refuses anything that is not
// a live web session, so stickiness can never turn into a failure and can never
// be honoured by serving a dead cookie.  Step 2 then re-binds.
//
// conversationKey may be empty, which means "this request is not
// conversation-scoped": the call then degenerates into the existing pick with no
// table traffic at all.
func (c *Client) pickWebSession(conversationKey, model string) (storedEndpoint, bool) {
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok {
			if ep, ok := c.storedByID(id); ok && c.endpointUsable(ep, true) {
				return ep, true
			}
		}
	}
	ep, ok := c.firstAvailableWeb()
	if !ok {
		// Every session is cooling or otherwise unusable.  Do not fall back
		// to store order here: that would hand the gateway the very session
		// whose 429 it is trying to rest, and every retry would create another
		// room and deepen the vendor's penalty.  resolveWebAuthFor turns this
		// into backpressure instead.
		return storedEndpoint{}, false
	}
	if !ok {
		return storedEndpoint{}, false
	}
	if conversationKey != "" {
		c.affinity.Bind(conversationKey, ep.ID)
	}
	return ep, true
}

// pickSidecar chooses the sidecar account for one request, or reports that the
// endpoint is not the module's to choose.
//
// locate() gives the config, the environment and the state file precedence over
// the panel store.  When one of those named the endpoint there is no account
// choice to make and nothing to pin, so this reports false and the caller keeps
// locate()'s answer unchanged.  When the panel store is what decided the
// endpoint (source == epOriginPanel) the store holds the choice -- possibly
// several sidecar accounts -- and a binding is honoured among them.
func (c *Client) pickSidecar(loc location, conversationKey, model string) (storedEndpoint, bool) {
	if loc.source != epOriginPanel {
		return storedEndpoint{}, false
	}
	if conversationKey != "" {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok {
			if ep, ok := c.storedByID(id); ok && c.endpointUsable(ep, false) {
				return ep, true
			}
		}
	}
	ep, ok := c.firstEnabledStored()
	if !ok {
		return storedEndpoint{}, false
	}
	if conversationKey != "" {
		c.affinity.Bind(conversationKey, ep.ID)
	}
	return ep, true
}

// locationFor points loc at a chosen stored account, keeping everything about
// the process (command, args, workdir, manage, web host) that locate() resolved.
// Only the endpoint and the key that belongs to it are replaced: for this
// request the pinned account outranks the config's own base_url, the same way
// locate() already lets the first stored endpoint outrank the state file and the
// documented default.
func (c *Client) locationFor(ep storedEndpoint, loc location) location {
	loc.baseURL = ep.BaseURL
	loc.source = epOriginPanel
	if k := strings.TrimSpace(ep.APIKey); k != "" {
		loc.apiKey, loc.keySource = k, epOriginPanel
	}
	return loc
}

// resolveWebAuthFor is resolveWebAuth (web.go:268) for one chat request: it lets
// a conversation binding choose among the stored sessions, then falls back to
// exactly what resolveWebAuth does -- the first stored session, or the static
// config/environment token when the panel store holds none.
func (c *Client) resolveWebAuthFor(conversationKey, model string) (webAuth, error) {
	if ep, ok := c.pickWebSession(conversationKey, model); ok {
		return c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL), nil
	}
	// A panel account that is merely cooling must not fall through to the
	// "first enabled" fallback: that path ignores the cooldown, so the
	// gateway's retry walks straight back into the same 429 and every extra
	// attempt makes the vendor's penalty worse.  Report backpressure instead,
	// which the gateway returns to the caller without rotating into it again.
	if c.anyWebAccount() && !c.anyAvailableWebAccount() {
		return webAuth{}, fmt.Errorf("%w: every Tabbit session is cooling after a rate limit", core.ErrBusy)
	}
	return c.resolveWebAuth()
}

// ApplyLive implements core.LiveReloader.  tabbit reads exactly two settings
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
	if s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0 {
		c.deps.Log("tabbit: conversation stickiness enabled=%t, window %s, swept every %s",
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
