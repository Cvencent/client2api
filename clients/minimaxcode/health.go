package minimaxcode

import (
	"fmt"

	"client2api/internal/core"
)

// health.go adds the pool-facing optional capabilities.  Both read the same
// state the picker uses, so the panel cannot report a healthy pool while every
// request would be refused.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	accounts := c.pool.accountsForStatus()
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
		h.Note = "还没有账号；请在账号页添加 MiniMax Code 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看冷却或凭据状态"
	default:
		h.Servable = true
		h.Note = fmt.Sprintf("%d/%d 个账号可用", h.Ready, h.Total)
	}
	return h
}

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
