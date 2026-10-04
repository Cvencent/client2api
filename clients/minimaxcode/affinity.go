package minimaxcode

import (
	"client2api/internal/core"
)

// Conversation stickiness for minimaxcode.
//
// A chat is normally a sequence: the caller sends a prompt, then a tool result,
// then a follow-up.  The gateway rotates accounts between requests, which is
// exactly what an operator wants for throughput and exactly what a conversation
// does not want -- MiniMax Code's route keeps a per-credential prompt cache and
// a per-credential rate window, so bouncing a single conversation across two
// accounts pays for the same prefix twice and can trip a limit that one
// credential would have absorbed.
//
// This file binds a conversation to the account that served it first.  The
// binding is an OPTIMISATION, never a constraint:
//
//   - It is only honoured while the bound account is still selectable by this
//     module's own pool.  core.Affinity.Resolve is given c.usableFor, which asks
//     the pool's own selectableLocked -- the same predicate next() applies -- so
//     stickiness can never resolve to an account the picker would refuse, and a
//     binding that has gone cold is dropped and re-made rather than served.
//   - It is written as soon as an account is CHOSEN, not after it succeeds, so
//     two concurrent first turns of one conversation agree on a single account
//     instead of each binding to whichever credential it happened to rotate to.
//   - A request with no conversation key touches the table not at all, and
//     behaves exactly as it did before this file existed.
//
// The table itself (Bind/Unbind/Resolve, idle TTL, sweeping) lives in
// internal/core/affinity.go and is shared with the other client modules; only
// the notion of "usable" is module-specific.

// Compile-time proof that the gateway's conversation contract is met.  The
// capability is advertised by type assertion in core.CapabilitiesOf, so a method
// that drifts from the interface would silently drop the feature from the panel
// rather than fail a build -- hence this line.
var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
)

// BindConversation pins a conversation to an account.  It is called by the
// picker below and exposed for the panel's "use this account" affordance.
//
// nil-safe: a Client built without New has no table, and pinning nothing is the
// correct answer rather than a panic.
func (c *Client) BindConversation(conversationKey, accountID string) {
	if c == nil || c.affinity == nil {
		return
	}
	c.affinity.Bind(conversationKey, accountID)
}

// UnbindConversation forgets a conversation's account and reports whether there
// was anything to forget.  nil-safe.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil || c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(conversationKey)
}

// ConversationAccount reports which account a conversation is pinned to, and
// whether it still is.  A binding whose account has since cooled, been parked or
// been removed is dropped here and reported absent -- that is the resolve path
// doing its job, not an error.
//
// The model argument is accepted because the gateway's interface passes it (a
// module whose health IS model-scoped needs it); minimaxcode's is not, so it is
// ignored.  See usableFor.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil || c.affinity == nil {
		return "", false
	}
	return c.affinity.Resolve(conversationKey, c.usableFor(model))
}

// usableFor returns the liveness predicate the affinity table consults before
// honouring a binding.
//
// It is deliberately a thin wrapper over the pool: an account is usable here
// exactly when selectableLocked says so -- enabled, not invalid, not exhausted,
// and either ready or cooled down past its CooldownUntil.  That is the same test
// next() applies when it picks.  Anything looser would let a binding serve an
// account the picker would have skipped; anything stricter would discard
// bindings that would have worked.
//
// The model parameter is accepted and ignored ON PURPOSE.  minimaxcode's health
// is not model-scoped: noteFailure parks a credential for the whole module
// whatever model triggered it, and every account in the pool speaks every model
// in the catalogue (they are the same endpoint with the same token).  So "usable
// for model M" and "usable" are one question, and this returns the answer to it.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model
	return func(accountID string) bool {
		if c == nil || c.pool == nil {
			return false
		}
		return c.pool.usable(accountID)
	}
}

// conversationKey extracts the stickiness key for a request.  "" means the
// request is not conversation-scoped: no key was supplied and there is no user
// identity to fall back on, so the table is not consulted and no binding is
// written.  See core.ConversationKeyOf for the accepted spellings.
func conversationKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return core.ConversationKeyOf(req)
}

// pickAccount chooses the account for one attempt.
//
// The order is the whole design:
//
//  1. If this conversation is bound to an account that can still serve right
//     now, serve that one.  It is read as a SNAPSHOT (pool.current), never as
//     the pool's live pointer: the credential fields are rewritten under the
//     pool lock by a concurrent refresh, and this request is about to read them
//     (see the README's data-race rule).  usable() is re-checked after Resolve
//     because the account can have cooled in the moment since.
//  2. Otherwise pick normally, through the pool's existing rotation -- next()
//     is called unchanged, so an unbound conversation behaves exactly as before.
//  3. If the request is conversation-scoped, bind the account that was just
//     CHOSEN.  Binding before the attempt rather than after it is what makes two
//     concurrent first turns agree: both resolve to nothing, one wins the
//     bind, and the loser's own bind writes the same account id.
//
// A dead binding is never served: Resolve drops it the instant usableFor says
// no, and step 3 immediately re-binds to whatever the rotation chose.
//
// exclude is the caller's already-tried set and is mutated here, exactly as
// next() would have mutated it.
func (c *Client) pickAccount(exclude map[string]bool, conversationKey, model string) (Account, bool) {
	if conversationKey != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(conversationKey, c.usableFor(model)); ok {
			if acct, found := c.pool.current(id); found && c.pool.usable(id) {
				if exclude != nil {
					exclude[id] = true
				}
				return acct, true
			}
		}
	}

	chosen := c.pool.next(exclude)
	if chosen == nil {
		return Account{}, false
	}
	if conversationKey != "" && c.affinity != nil {
		c.affinity.Bind(conversationKey, chosen.ID)
	}

	// next() hands back a live pointer; snapshot it under the pool lock before
	// anything reads a credential field out of it.
	if acct, ok := c.pool.current(chosen.ID); ok {
		return acct, true
	}
	return Account{}, false
}

// ApplyLive implements core.LiveReloader.
//
// minimaxcode takes exactly two settings from the shared live config, and both
// are about stickiness: how long a binding lives, and how often expired ones are
// swept.  They arrive here rather than at construction so an operator can move
// them and reload without restarting the gateway.
//
// A zero value means "the config file said nothing" -- not "use zero" -- so the
// table keeps whatever it had instead of being pinned to a window of nothing,
// which would quietly disable the feature.  The log line is emitted only when
// something actually moved.
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
	if c.logf != nil && (s.AffinityEnabled != nil || s.AffinityTTL > 0 || s.AffinityGCInterval > 0) {
		c.logf("minimaxcode: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
