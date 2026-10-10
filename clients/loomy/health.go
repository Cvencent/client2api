package loomy

import (
	"time"

	"client2api/internal/core"
)

// health.go adds the two pool-facing optional capabilities: a servability
// verdict and a live in-flight count.
//
// The census runs over the same snapshot Status() renders and applies the same
// selectable() verdict the picker uses, so the badge and the account table can
// never disagree.  In-flight is fed by the one success path in Chat, and sticky
// sessions come from the affinity table orderedCandidates reads.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
//
// A dead session or a disabled row is a fault the operator has to fix; a
// cooled-down account heals by time, so it counts as cooling.  A declared
// expiry is deliberately not treated as dead here: the picker still offers
// that row because the vendor, not the local stamp, decides whether it works.
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
		case a.dead || !a.Enabled:
			h.Disabled++
		case a.selectable(now):
			h.Ready++
		default:
			h.Cooling++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "还没有账号；请先在账号页添加 Loomy 账号"
	case h.Ready == 0:
		h.Note = "当前没有可用账号；请查看账号冷却或会话是否过期"
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  Loomy's ceiling is enforced by
// the gateway's per-account gate, so InFlightFull is left at zero rather than
// invented; InFlight is the real count Chat feeds.
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
