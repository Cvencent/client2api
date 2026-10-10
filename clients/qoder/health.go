package qoder

import (
	"fmt"
	"time"

	"client2api/internal/core"
)

// health.go implements the pool-facing optional capabilities.  They are
// deliberately computed from the same account snapshot the request path uses,
// so the panel cannot show a healthy pool while every chat request would be
// refused.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.  Readiness is an account-level
// verdict: an account is ready when it is enabled, has not been rejected, has
// not expired, and is not serving a cooldown.
func (c *Client) Health() core.Health {
	if c == nil || c.store == nil {
		return core.Health{}
	}

	now := time.Now().UTC()
	accounts := c.store.snapshot()
	h := core.Health{Total: len(accounts)}
	for i := range accounts {
		a := &accounts[i]
		switch {
		case !a.Enabled, a.dead, a.expired(now):
			h.Disabled++
		case !a.cooldownTill.IsZero() && now.Before(a.cooldownTill):
			h.Cooling++
		default:
			h.Ready++
		}
	}

	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加 Qoder CN 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看账号异常或冷却时间"
	default:
		h.Servable = true
		h.Note = fmt.Sprintf("%d/%d 个账号可用", h.Ready, h.Total)
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  Both numbers are real: the
// in-flight count is fed by Chat, and the sticky count is the affinity table
// the same request path reads.
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
