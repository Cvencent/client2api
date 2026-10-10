package cline

import (
	"strings"

	"client2api/internal/core"
)

// affinity.go is the panel-facing half of Cline's conversation stickiness.
//
// The pick path has consulted the binding table since the chat path was
// written (see pickAccount in cline.go), which keeps one conversation on the
// credential that warmed it.  What was missing was the administrative half:
// without core.ConversationBinder the panel hides the per-account conversation
// test, so an operator could not deliberately pin a conversation to an account
// or read the pin back.  This file adds that contract and the live knobs that
// retune it, and nothing else.

var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
)

// BindConversation implements core.ConversationBinder: pin a conversation to an
// account.  It never fails; a module that cannot record a binding must still
// serve the request that follows.
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

// UnbindConversation implements core.ConversationBinder: forget a conversation's
// pin, reporting whether one existed.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil || c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount implements core.ConversationBinder.  It reports the bound
// account only while that account could still serve a request, so a parked or
// disabled credential is reported absent rather than kept as a broken pin.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil || c.affinity == nil {
		return "", false
	}
	_ = model // Cline's liveness is per credential, not per model.
	return c.affinity.Resolve(strings.TrimSpace(conversationKey), c.usableFor)
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
		c.deps.Log("cline: conversation stickiness enabled=%t, window %s, swept every %s",
			c.affinity.Enabled(), c.affinity.TTL(), c.affinity.GCInterval())
	}
}
