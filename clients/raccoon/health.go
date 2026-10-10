package raccoon

import (
	"fmt"

	"client2api/internal/core"
)

// health.go adds the pool-facing optional capabilities.  They are computed
// from the same snapshot Status renders, so the panel cannot show a healthy
// pool while every chat request would be refused.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.  Raccoon's snapshot already resolves
// each entry to exactly one state, so the census is a straight count of those
// states rather than a second opinion about the credential.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	accounts := c.pool.snapshot()
	h := core.Health{Total: len(accounts)}
	for _, a := range accounts {
		switch a.State {
		case stateReady:
			h.Ready++
		case stateCooling, stateExhausted:
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加小浣熊账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看冷却或凭据状态"
	default:
		h.Servable = true
		h.Note = fmt.Sprintf("%d/%d 个账号可用", h.Ready, h.Total)
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  InFlight is the real count of
// open chat streams, and StickySessions is the affinity table the request path
// reads.  The gateway owns the per-account ceiling, so InFlightFull stays zero
// rather than inventing a second limit.
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
