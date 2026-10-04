package core

// Health is a module's own answer to "could I serve a request right now?".
//
// The reference's /healthz returns 503 while its pool reports no servable
// account, so that a load balancer stops routing to an instance that would
// only fail.  A module opts in by implementing HealthProvider; a module that
// does not implement it is treated as always servable, which keeps the
// contract backwards compatible.
type Health struct {
	Servable bool `json:"servable"`
	// Ready/Cooling/Disabled summarise the account pool when the module has
	// one.  They are informational.
	Ready    int `json:"ready,omitempty"`
	Cooling  int `json:"cooling,omitempty"`
	Disabled int `json:"disabled,omitempty"`
	Total    int `json:"total,omitempty"`
	// Realms maps a realm name (e.g. "cn", "global") to whether that realm can
	// serve right now.  Modules whose vendors have separate realms report
	// them so the status page can show why a region is down.
	Realms map[string]bool `json:"realms,omitempty"`
	// Note is a short human-readable reason, usually why Servable is false.
	Note string `json:"note,omitempty"`
}

// HealthProvider is the optional capability a module implements when it can
// tell whether it has a usable account.
type HealthProvider interface {
	Health() Health
}

// HealthOf reports the module's health and whether it implements the
// capability at all.  ok is false for a module that has not opted in.
func HealthOf(c Client) (Health, bool) {
	if c == nil {
		return Health{}, false
	}
	hp, ok := c.(HealthProvider)
	if !ok {
		return Health{}, false
	}
	return hp.Health(), true
}
