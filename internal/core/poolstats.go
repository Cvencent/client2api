package core

// PoolStats is the account-pool surface a module with a pool reports to the
// panel.
//
// It is deliberately small and deliberately optional.  A module without a pool
// does not implement PoolStatsReporter, so the panel renders "not implemented"
// rather than a row of zeroes: "this client has no pool" and "this client's
// pool is idle" are different answers, and only the module can tell them
// apart.
type PoolStats struct {
	// InFlight is how many upstream requests this module is running right
	// now, across every account.  A request counts from the moment it is
	// admitted until its response body is closed, which is what makes the
	// number match what the vendor's risk control sees.
	InFlight int `json:"in_flight,omitempty"`
	// InFlightFull counts the accounts that are healthy but at their ceiling.
	// Without it, "why is this slow" has no visible answer.
	InFlightFull int `json:"in_flight_full,omitempty"`
	// StickySessions is the number of live conversation-to-account bindings.
	StickySessions int `json:"sticky_sessions,omitempty"`
}

// PoolStatsReporter is an optional capability.
//
// PoolStats must be cheap and must not block: the panel calls it on every
// status poll, including while the pool is saturated, which is exactly when an
// operator is looking at it.
type PoolStatsReporter interface {
	Client
	PoolStats() PoolStats
}

// PoolStatsOf reports a module's pool statistics.  ok is false when the module
// keeps no pool.
func PoolStatsOf(c Client) (PoolStats, bool) {
	if r, ok := c.(PoolStatsReporter); ok {
		return r.PoolStats(), true
	}
	return PoolStats{}, false
}
