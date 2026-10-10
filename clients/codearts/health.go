package codearts

import (
	"time"

	"client2api/internal/core"
)

// health.go adds the two pool-facing optional capabilities: a servability
// verdict and the live in-flight count.
//
// The census runs over the same pool entries Status() renders and applies the
// same availableLocked() verdict the picker uses, so the badge and the account
// table can never disagree.  In-flight is fed by the success path in Chat, and
// sticky sessions come from the affinity table the picker reads.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	p := c.pool
	p.mu.Lock()
	now := time.Now()
	h := core.Health{Total: len(p.entries)}
	for _, e := range p.entries {
		if p.availableLocked(e, now) {
			h.Ready++
			continue
		}
		switch e.state {
		case stateCooling, stateExhausted:
			// Exhausted is a quota park: the credential is intact, so it
			// belongs with cooling rather than with a dead token.
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	p.mu.Unlock()
	switch {
	case h.Total == 0:
		h.Note = "no credential has been added yet"
	case h.Ready == 0:
		h.Note = "no credential is usable right now"
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.  The picker has no per-account
// in-flight ceiling of its own (the gateway enforces one above it), so
// InFlightFull is left at zero rather than invented; InFlight is the real count
// the success path in Chat feeds.
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
