package cline

import (
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
// Cline's ceiling is per process, not per account (see enter/leave), so a pool
// whose accounts are all healthy is still reported as servable here: the
// module-level ceiling is backpressure, not a broken credential, and it is
// already visible as the in-flight number PoolStats reports.
func (c *Client) Health() core.Health {
	if c == nil || c.pool == nil {
		return core.Health{}
	}
	accounts := c.pool.snapshot()
	h := core.Health{Total: len(accounts)}
	for i := range accounts {
		switch accounts[i].State {
		case stateReady:
			h.Ready++
		case stateExhausted:
			// Exhausted is a quota park: the credential is intact, so it
			// belongs with cooling rather than with a dead token.
			h.Cooling++
		case stateCooling:
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "no account has been discovered"
	case h.Ready == 0:
		h.Note = "no account is usable right now"
	default:
		h.Servable = true
	}
	return h
}

// PoolStats implements core.PoolStatsReporter.
//
// InFlight is the same counter enter/leave admits against, so the panel shows
// what the process is really doing.  Cline has no per-account ceiling to count,
// so InFlightFull is deliberately left at zero rather than invented from the
// process-wide limit, and the sticky count comes from the affinity table the
// request path reads.
func (c *Client) PoolStats() core.PoolStats {
	if c == nil {
		return core.PoolStats{}
	}
	c.inflightMu.Lock()
	inFlight := c.inflight
	c.inflightMu.Unlock()
	stats := core.PoolStats{InFlight: inFlight}
	if c.affinity != nil {
		stats.StickySessions = c.affinity.Count()
	}
	return stats
}
