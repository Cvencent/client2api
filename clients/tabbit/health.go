package tabbit

import (
	"time"

	"strings"

	"client2api/internal/core"
)

// health.go adds the two pool-facing optional capabilities: a servability
// verdict and the live in-flight count.
//
// tabbit has no single vendor credential: the panel store (acct) holds sidecar
// endpoints and web-token accounts side by side, and the web transport keeps
// its own verdict cache (webChecks) for the cookies it calls.  Health folds the
// two together so the badge reflects the same rows Accounts() renders, across
// both transports, rather than a synthetic third view.
//
// A sidecar row that is enabled and named is reported ready: whether it is
// actually listening is a question Status() answers with a probe, and a row
// that is merely unreachable is not a dead credential.  A web row uses the
// verdict the request path already recorded, so a rate-limited session counts
// as cooling and a rejected or expired one as disabled.

var (
	_ core.HealthProvider    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)

// Health implements core.HealthProvider.
func (c *Client) Health() core.Health {
	if c == nil {
		return core.Health{}
	}
	endpoints := c.endpointsSnapshot()
	h := core.Health{Total: len(endpoints)}
	now := time.Now()
	for _, ep := range endpoints {
		switch c.healthOfEndpoint(ep, now) {
		case epStateReady:
			h.Ready++
		case epStateCooling:
			h.Cooling++
		default:
			h.Disabled++
		}
	}
	switch {
	case h.Total == 0:
		h.Note = "no endpoint has been added yet"
	case h.Ready == 0:
		h.Note = "no endpoint is usable right now"
	default:
		h.Servable = true
	}
	return h
}

// healthOfEndpoint resolves one stored row to the state vocabulary the panel
// uses.  It applies the same rules Accounts() does so the two cannot disagree.
func (c *Client) healthOfEndpoint(ep storedEndpoint, now time.Time) string {
	if !ep.Enabled {
		return epStateInvalid
	}
	if epKind(ep) == kindWebToken {
		token := strings.TrimSpace(ep.Token)
		if token == "" {
			return epStateInvalid
		}
		claims, _ := parseWebToken(token)
		if exp := webTokenExpiry(claims); !exp.IsZero() && now.After(exp) {
			return epStateInvalid
		}
		if _, held := c.webRateLimited(ep.ID, now); held {
			return epStateCooling
		}
		if v, seen := c.webVerdictFor(ep.ID); seen && v.err != "" && !v.transient {
			return epStateInvalid
		}
	}
	return epStateReady
}

// PoolStats implements core.PoolStatsReporter.  InFlight is the real count of
// open streams across both transports, and StickySessions is the affinity table
// the request path reads.  The gateway owns the per-account ceiling, so
// InFlightFull stays zero rather than inventing a second limit.
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
