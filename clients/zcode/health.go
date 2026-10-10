package zcode

import (
	"fmt"
	"time"

	"client2api/internal/core"
)

// health.go adds the pool-facing optional capabilities.  The census applies
// selectableLocked itself rather than reading the rendered rows, because a
// cooling JWT whose captcha solver is missing is not actually servable even
// though its row still renders as ready.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	p := c.pool
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	h := core.Health{Total: len(p.accounts)}
	for _, a := range p.accounts {
		switch {
		case p.selectableLocked(a, now):
			h.Ready++
		case !a.Enabled || a.State == stateInvalid || a.State == stateExhausted:
			h.Disabled++
		default:
			h.Cooling++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加 ZCode 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看冷却、凭据或验证码配置"
	default:
		h.Servable = true
		h.Note = fmt.Sprintf("%d/%d 个凭据可用", h.Ready, h.Total)
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
