package qwenwork

import (
	"client2api/internal/core"
)

// health.go adds the two pool-facing optional capabilities: a servability
// verdict and a live in-flight count.
//
// The census runs over the same snapshot Status() publishes, so the badge and
// the account table can never disagree.  In-flight is fed by the one success
// path in openChat, and sticky sessions come from the affinity table the picker
// reads.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	rows := c.pool.snapshot()
	h := core.Health{Total: len(rows)}
	for i := range rows {
		switch {
		case !rows[i].Enabled:
			h.Disabled++
		case rows[i].State == stateReady:
			h.Ready++
		case rows[i].State == stateExhausted:
			// Exhausted is a quota park: the credential is intact, so it
			// belongs with cooling rather than with a dead token.
			h.Cooling++
		default:
			if rows[i].State == stateCooling {
				h.Cooling++
			} else {
				h.Disabled++
			}
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加 qwenwork 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看账号冷却或登录状态"
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  qwenwork has no per-account
// in-flight ceiling of its own (the gateway enforces one above it), so
// InFlightFull is left at zero rather than invented, and InFlight is the real
// count the request path feeds.
func (c *Client) PoolStats() core.PoolStats {
	if c == nil {
		return core.PoolStats{}
	}
	stats := core.PoolStats{InFlight: int(c.inFlight.Load())}
	if c.affinity != nil {
		stats.StickySessions = c.affinity.Count()
	}
	return stats
}
