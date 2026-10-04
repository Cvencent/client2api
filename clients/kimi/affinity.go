package kimi

import (
	"strings"

	"client2api/internal/core"
)

// kimi has no account pool: a request is served either by the panel's own login
// (webLoginID, over HTTPS -- see direct.go) or by the CLI the runner resolves
// (cliLoginID, or an executable binding the operator imported).  Stickiness for
// this module therefore means pinning a conversation to one of those serving
// identities, and that is what makes the panel's per-account chat test mean
// something: without a pin, a request aimed at the panel login that fails is
// silently served by the CLI, and the operator ends up testing an account they
// did not choose.
//
// A pin is honoured in Chat as a *path* decision, not as a hint: a conversation
// pinned to the web login is not retried on the CLI, because retrying would
// answer from the wrong account.  The escape hatch is the table itself --
// usableFor stops admitting an account as soon as this module parks it, so a
// dead pin is dropped by Resolve and the next request picks normally.

var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
)

// conversationKey names the conversation a request belongs to, or "" when the
// caller did not scope it.  An empty key means "no stickiness", never an error.
func conversationKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	return strings.TrimSpace(core.ConversationKeyOf(req))
}

// usableFor is the predicate the affinity table applies to a stored binding.
// It answers "would this account be the one that actually serves right now?",
// which is deliberately the same question the picker asks: a binding kept for an
// account Chat would refuse is worse than no binding at all, because it turns
// stickiness into a source of failures rather than a way to avoid them.
//
// The CLI side is checked through binaryPathFrom because that is what decides
// which account a CLI request belongs to.  A binding whose path is no longer
// the resolved one cannot serve, and must not keep a conversation.
func (c *Client) usableFor(model string) func(accountID string) bool {
	_ = model // kimi's liveness is per account; no path here consults the model
	return func(accountID string) bool {
		id := strings.TrimSpace(accountID)
		if id == "" {
			return false
		}
		if id == webLoginID {
			return c.cfg.preferHTTP() && !c.accountExplicitlyDisabled(webLoginID) && c.selectable(webLoginID)
		}
		if !c.selectable(id) {
			return false
		}
		bin, account, err := c.run.binaryPathFrom()
		return err == nil && bin != "" && account == id
	}
}

// BindConversation pins conversationKey to accountID.  It never fails: the panel
// route rejects a malformed request before reaching here, and a module that
// cannot record a binding must still serve the request.
func (c *Client) BindConversation(conversationKey, accountID string) {
	c.bindConversation(conversationKey, accountID)
}

// bindConversation is the internal, no-questions form used by Chat, which has
// already established that both halves are non-empty.
func (c *Client) bindConversation(conversationKey, accountID string) {
	if c.affinity == nil {
		return
	}
	key := strings.TrimSpace(conversationKey)
	id := strings.TrimSpace(accountID)
	if key == "" || id == "" {
		return
	}
	c.affinity.Bind(key, id)
}

// UnbindConversation forgets conversationKey, reporting whether it was bound.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount reports the account conversationKey is pinned to, and only
// while that account can still serve.  An unusable binding is reported as
// absent, never as a failure.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c.affinity == nil {
		return "", false
	}
	return c.affinity.Resolve(strings.TrimSpace(conversationKey), c.usableFor(model))
}

// affinityPin resolves a conversation to the account that must serve it.  An
// ok == false result means the caller should behave exactly as it did before
// this feature existed.
func (c *Client) affinityPin(conversationKey, model string) (string, bool) {
	return c.ConversationAccount(conversationKey, model)
}

// ApplyLive retunes the stickiness window from the live configuration.  A zero
// value means the file said nothing about it, so what is in force is kept.
func (c *Client) ApplyLive(s core.LiveSettings) {
	if c.affinity == nil {
		return
	}
	changed := false
	if s.AffinityEnabled != nil {
		c.affinity.SetEnabled(*s.AffinityEnabled)
		changed = true
	}
	if s.AffinityTTL > 0 {
		c.affinity.SetTTL(s.AffinityTTL)
		changed = true
	}
	if s.AffinityGCInterval > 0 {
		c.affinity.SetGCInterval(s.AffinityGCInterval)
		changed = true
	}
	if changed && c.deps.Logf != nil {
		c.deps.Logf("kimi: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
