package opencode

import (
	"client2api/internal/core"
)

// health.go adds the pool-facing servability verdict.  opencode already reports
// PoolStats; this is the other half of the pair.
//
// The census runs over the same statuses Status() renders, so the badge and the
// account table cannot disagree.  A cooling or out-of-credit account heals by
// time and counts as cooling; a disabled or credentialless row is a fault.

var _ core.HealthProvider = (*Client)(nil)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil {
		return core.Health{}
	}
	c.ensure()
	if c.pool == nil {
		return core.Health{}
	}
	now := c.now()
	accounts := c.pool.snapshot()
	h := core.Health{Total: len(accounts)}
	for i := range accounts {
		switch stateOf(&accounts[i], now) {
		case "ready":
			h.Ready++
		case "cooling", "exhausted":
			// A quota park leaves the credential intact, so it belongs with
			// cooling rather than with a dead token.
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "no OpenCode Zen account is configured"
	case h.Ready == 0:
		h.Note = "no OpenCode Zen account is usable right now"
	default:
		h.Servable = true
	}
	return h
}
