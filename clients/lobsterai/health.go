package lobsterai

import (
	"client2api/internal/core"
)

// health.go adds the pool-facing servability verdict.  lobsterai already
// reports PoolStats; this is the other half of the pair.
//
// The census reads the same snapshot Status() renders and applies the same
// stateOf() rule the panel uses for each row, so the badge and the account
// table cannot disagree.  A cooling account heals by time and counts as
// cooling; a disabled or tokenless row is a fault.

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
		case "cooling":
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "no LobsterAI account is configured"
	case h.Ready == 0:
		h.Note = "no LobsterAI account is usable right now"
	default:
		h.Servable = true
	}
	return h
}
