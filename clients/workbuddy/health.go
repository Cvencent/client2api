package workbuddy

import (
	"fmt"

	"client2api/internal/core"
)

// The pool surfaces are optional capabilities in core, so a renamed method
// would otherwise show up only as the panel quietly answering 501.
var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider: it answers "could this module serve a
// request right now, and if not, why not".
//
// Servable is deliberately stricter than "an account is healthy": a pool whose
// healthy accounts are all at their in-flight ceiling is reported as
// unservable, which is the reference's rule and the useful answer.  A caller
// told "healthy" while every slot is taken retries into the same wall.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	n := c.pool.counts()
	h := core.Health{
		Ready:    n.ready,
		Cooling:  n.cooling,
		Disabled: n.disabled,
		Total:    n.total,
		Realms:   n.realms,
	}
	switch {
	case n.total == 0:
		h.Note = "no account has been discovered"
	case n.ready == 0:
		h.Note = "no account is usable right now"
	case n.full == n.ready:
		h.Note = fmt.Sprintf("all %d usable account(s) are at their in-flight limit", n.ready)
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.
//
// The in-flight numbers come from the same counter Acquire admits against, and
// the sticky count from the affinity table the request path uses, so the panel
// shows what the process is really doing rather than a second bookkeeping.
func (c *Client) PoolStats() core.PoolStats {
	if c == nil || c.pool == nil {
		return core.PoolStats{}
	}
	total, full := c.pool.InFlight()
	return core.PoolStats{
		InFlight:       total,
		InFlightFull:   full,
		StickySessions: c.affinity.Count(),
	}
}
