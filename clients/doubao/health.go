package doubao

import (
	"fmt"
	"time"

	"client2api/internal/core"
)

// health.go adds the pool-facing optional capabilities.  Both read the same
// state the picker uses, so the panel cannot report a healthy pool while every
// request would be refused.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil || c.store == nil {
		return core.Health{}
	}
	accounts := c.store.snapshot()
	now := time.Now()
	h := core.Health{Total: len(accounts)}
	for _, a := range accounts {
		switch {
		case !a.Enabled:
			h.Disabled++
		case a.State == stateInvalid || a.State == stateExhausted:
			h.Disabled++
		case a.State == stateCooling && now.Before(a.CooldownUntil):
			h.Cooling++
		default:
			h.Ready++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有豆包账号；请在账号页用网页登录添加"
	case h.Ready == 0:
		h.Note = "当前没有可用的豆包账号；请查看冷却或凭据状态"
	default:
		h.Servable = true
		h.Note = fmt.Sprintf("%d/%d 个豆包账号可用", h.Ready, h.Total)
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.
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
