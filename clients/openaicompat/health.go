package openaicompat

import (
	"fmt"

	"client2api/internal/core"
)

// health.go adds the two pool-facing optional capabilities: a servability
// verdict and the live in-flight counters.
//
// A provider row is a credential plus a base URL, so it has no cooldown state
// the picker consults: it is either enabled or disabled, and a request against
// it either succeeds or is rotated past.  The census therefore reports the rows
// Status() already publishes, and Servable is deliberately stricter than "a
// provider is enabled": a pool whose enabled providers are all at their
// in-flight ceiling is not servable, because a caller told otherwise would retry
// into the same wall.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	records := c.pool.snapshot()
	limit := c.cfg.maxInFlight()
	h := core.Health{Total: len(records)}
	ready, full := 0, 0
	for i := range records {
		a := &records[i]
		if a.Disabled {
			h.Disabled++
			continue
		}
		h.Ready++
		ready++
		if limit > 0 && a.inFlight >= limit {
			full++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "no provider is configured"
	case h.Ready == 0:
		h.Note = "every provider is disabled"
	case full == ready:
		h.Note = fmt.Sprintf("all %d enabled provider(s) are at their in-flight limit", ready)
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  InFlight is the exact number of
// open streams, and InFlightFull counts the enabled providers sitting at
// max_in_flight.  Runnable's provider pool has no conversation stickiness, so
// StickySessions stays zero.
func (c *Client) PoolStats() core.PoolStats {
	if c == nil || c.pool == nil {
		return core.PoolStats{}
	}
	inFlight, full := c.pool.stats(c.cfg.maxInFlight())
	return core.PoolStats{InFlight: inFlight, InFlightFull: full}
}
