package openaicompat

import (
	"strings"
	"time"

	"client2api/internal/core"
)

// affinity.go gives both halves of openai-compat the same conversation
// stickiness contract as every other pool in the gateway.  The legacy client
// owns one provider per qualified model, while a dynamic source owns several
// API keys for one custom relay; in both cases a repeated conversation must
// stay on the credential that warmed the upstream prompt cache.

var (
	_ core.ConversationBinder = (*Client)(nil)
	_ core.LiveReloader       = (*Client)(nil)
	_ core.ConversationBinder = (*source)(nil)
	_ core.LiveReloader       = (*source)(nil)
)

// clientUsableFor is the liveness predicate for a legacy provider binding.
// A provider is usable only while it is present, enabled and able to serve the
// requested qualified model prefix.
func (c *Client) clientUsableFor(model string) func(string) bool {
	return func(accountID string) bool {
		if c == nil || c.pool == nil || strings.TrimSpace(accountID) == "" {
			return false
		}
		rec, ok := c.pool.byID(accountID)
		if !ok || rec.Disabled {
			return false
		}
		if strings.TrimSpace(model) == "" {
			return true
		}
		prov, _, ok := c.providerFor(model)
		return ok && strings.EqualFold(prov.ID, rec.ID)
	}
}

// BindConversation implements core.ConversationBinder for the legacy client.
func (c *Client) BindConversation(conversationKey, accountID string) {
	if c == nil {
		return
	}
	c.ensure()
	c.affinity.Bind(strings.TrimSpace(conversationKey), strings.TrimSpace(accountID))
}

// UnbindConversation implements core.ConversationBinder for the legacy client.
func (c *Client) UnbindConversation(conversationKey string) bool {
	if c == nil {
		return false
	}
	c.ensure()
	return c.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount implements core.ConversationBinder for the legacy client.
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.ensure()
	return c.affinity.Resolve(strings.TrimSpace(conversationKey), c.clientUsableFor(model))
}

// ApplyLive implements core.LiveReloader for the legacy client.
func (c *Client) ApplyLive(s core.LiveSettings) {
	if c == nil {
		return
	}
	c.ensure()
	applyAffinityLive(c.affinity, s)
}

// usableFor is the liveness predicate for a dynamic source binding.  It
// deliberately mirrors Chat's candidate filter exactly: source active, account
// enabled, account not cooling, and account advertising the requested model.
func (s *source) usableFor(model string) func(string) bool {
	return func(accountID string) bool {
		if s == nil {
			return false
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.active {
			return false
		}
		for _, p := range s.rows {
			if !strings.EqualFold(p.ID, accountID) || p.Disabled || time.Now().Before(s.cooling[p.ID]) {
				continue
			}
			for _, m := range p.Models {
				if m == model {
					return true
				}
			}
			return false
		}
		return false
	}
}

// BindConversation implements core.ConversationBinder for a custom source.
func (s *source) BindConversation(conversationKey, accountID string) {
	if s == nil {
		return
	}
	s.affinity.Bind(strings.TrimSpace(conversationKey), strings.TrimSpace(accountID))
}

// UnbindConversation implements core.ConversationBinder for a custom source.
func (s *source) UnbindConversation(conversationKey string) bool {
	if s == nil {
		return false
	}
	return s.affinity.Unbind(strings.TrimSpace(conversationKey))
}

// ConversationAccount implements core.ConversationBinder for a custom source.
func (s *source) ConversationAccount(conversationKey, model string) (string, bool) {
	if s == nil {
		return "", false
	}
	return s.affinity.Resolve(strings.TrimSpace(conversationKey), s.usableFor(model))
}

// ApplyLive implements core.LiveReloader for a custom source.
func (s *source) ApplyLive(settings core.LiveSettings) {
	if s == nil {
		return
	}
	applyAffinityLive(s.affinity, settings)
}

// applyAffinityLive is the shared live-settings half.  Zero values mean the
// config file said nothing, so a partial reload never clears a prior value.
func applyAffinityLive(a *core.Affinity, s core.LiveSettings) {
	if a == nil {
		return
	}
	if s.AffinityEnabled != nil {
		a.SetEnabled(*s.AffinityEnabled)
	}
	if s.AffinityTTL > 0 {
		a.SetTTL(s.AffinityTTL)
	}
	if s.AffinityGCInterval > 0 {
		a.SetGCInterval(s.AffinityGCInterval)
	}
}
