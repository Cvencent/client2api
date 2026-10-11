package doubao

import (
	"time"

	"client2api/internal/core"
)

// affinity.go exposes the conversation->account stickiness this module keeps.
// The binding itself lives in core.Affinity; this file only adapts it to the
// ConversationBinder contract the panel discovers by assertion.

var _ core.ConversationBinder = (*Client)(nil)

// BindConversation implements core.ConversationBinder.
func (c *Client) BindConversation(conversationKey, accountID string) {
	if c.affinity != nil {
		c.affinity.Bind(conversationKey, accountID)
	}
}

// UnbindConversation implements core.ConversationBinder.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c.affinity == nil {
		return false
	}
	return c.affinity.Unbind(conversationKey)
}

// ConversationBindings implements core.ConversationBinder.
func (c *Client) ConversationBindings() []core.ConversationBinding {
	if c.affinity == nil {
		return nil
	}
	return c.affinity.Bindings()
}

// ConversationAccount reports the account bound to a key when it can still
// serve.  A disabled or invalid account reports absent, so the caller picks
// normally instead of failing.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c.affinity == nil {
		return "", false
	}
	return c.affinity.Resolve(conversationKey, func(id string) bool {
		acc, ok := c.store.byID(id)
		return ok && acc.selectable(time.Now())
	})
}
