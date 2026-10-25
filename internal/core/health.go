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

// ModelAvailabilityProvider is the optional capability a module implements
// when it can answer "could you serve THIS model right now" from state it
// already holds, without going upstream.
//
// Health answers an account-level question, but a park is often per
// (account, model): WorkBuddy's vendor parks one model on a credential while
// the credential stays perfectly healthy for every other model, so its account
// census says "ready" while the picker has nothing to hand out for that id.
// The router asks this before entering Chat, so such a platform is skipped for
// a sibling instead of paying a round trip that only re-learns what the module
// already knew.
//
// Available returns nil when at least one account could take the request, and
// otherwise the refusal Chat itself would have produced for the same state, so
// the router and the caller see the same answer either way.  A module that
// keeps no per-model state simply does not implement this capability and keeps
// the optimistic path.
type ModelAvailabilityProvider interface {
	// Available reports whether any account could serve one model right now.
	// free marks a model the vendor charges nothing for, because a pool that
	// parks accounts on an empty wallet may still serve it.
	Available(model string, free bool) error
}

// AsModelAvailability reports the module's per-model availability probe, when
// it implements one.  ok is false for a module that has not opted in.
func AsModelAvailability(c Client) (ModelAvailabilityProvider, bool) {
	if c == nil {
		return nil, false
	}
	p, ok := c.(ModelAvailabilityProvider)
	return p, ok
}
