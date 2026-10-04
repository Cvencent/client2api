package core

import "time"

// LiveSettings are the configuration values that may change while the process
// is running, expressed in module-neutral terms.
//
// The gateway keeps them in a livecfg snapshot for its own request path; this
// struct is the *push* direction, used to tell the modules that a reload
// happened.  A module that wants hot behaviour implements LiveReloader;
// a module that does not is simply never called and keeps its startup values,
// which is why this is an optional capability and not part of Client.
type LiveSettings struct {
	// SanitizeFingerprints toggles outbound body rewriting.  A nil pointer
	// means "the config file said nothing", so the module keeps its default
	// (which is on) instead of being forced off.
	SanitizeFingerprints *bool
	// PromptMode is the system-prompt strategy: "" / "passthrough",
	// "custom" (replace with PromptFile) or "append" (add after the leading
	// system turns).  Modules that own a prompt pipeline act on it.
	PromptMode string
	// PromptFile is the replacement prompt's path for PromptMode == "custom".
	PromptFile string
	// MaxInFlight is a module pool's ceiling on concurrent upstream requests
	// per account.  0 means "no ceiling", which is the reference's documented
	// meaning of the same key; a nil pointer means "the config file said
	// nothing", so a partial reload keeps the ceiling it already had.
	MaxInFlight *int
	// MaxInFlightGlobal is the same ceiling for the realm whose risk control
	// is stricter (the reference's 403 fix).  0 means "not set": the global
	// tier then falls back to MaxInFlight rather than refusing every request,
	// which is why the zero value is not "limit nothing".
	MaxInFlightGlobal *int
	// AffinityEnabled is the operator's session_sticky.enabled: nil means "the
	// config file said nothing", so a partial reload keeps the current state.
	//
	// It is not the same knob as AffinityTTL.  The reference does not implement
	// "off" as a window of zero; it builds no session router at all, so every
	// request selects an account from scratch.  A module must therefore treat a
	// non-nil false as "stop pinning conversations", not as "pin them for zero
	// seconds".
	AffinityEnabled *bool
	// AffinityTTL is how long a conversation stays pinned to the account it
	// started on.  0 means "the config file said nothing", so a module keeps
	// the window it already had (core's DefaultAffinityTTL) instead of being
	// forced onto a window of zero, which would silently turn stickiness off.
	AffinityTTL time.Duration
	// AffinityGCInterval is how often expired bindings are swept.  0 means
	// "not mentioned"; the table's own default then stands.
	AffinityGCInterval time.Duration
	// Pool carries the account-pool policy.  A nil pointer means the config
	// file said nothing about that knob, so a partial reload never clears a
	// value the operator did not touch.
	Pool *PoolTuning
}

// PoolTuning is the account-pool policy the reference exposes as its pool.*
// configuration block.
//
// It lives here rather than in a module because the config file is
// process-wide: one deployment has one breaker threshold, and a module that
// owns no comparable pool simply never reads it.  Every field is a pointer so
// that "absent" and "zero" stay distinguishable -- for CostExploreInterval in
// particular, 0 means "stop exploring", which is not the same as silence.
type PoolTuning struct {
	// BreakerThreshold is how many authoritative failures in a row park an
	// account.  BreakerCooldown is the first park's length; each further trip
	// doubles it up to BreakerCooldownMax.
	BreakerThreshold   *int
	BreakerCooldown    *time.Duration
	BreakerCooldownMax *time.Duration
	// DegradeThreshold is how many unclassified failures in a row -- transport
	// errors and unknown 4xx, the failures no vendor code explains -- park an
	// account.  DegradeCooldown bounds the park, DegradeCooldownMax clamps it.
	DegradeThreshold   *int
	DegradeCooldown    *time.Duration
	DegradeCooldownMax *time.Duration
	// SoftRateMax bounds every soft (rate-limit) cooldown, so one vendor
	// message cannot park an account for the rest of the day.
	SoftRateMax *time.Duration
	// IdleWeightPerHour and IdleWeightMax are the idle-compensation curve: an
	// unused account gains weight each hour, up to the maximum.
	IdleWeightPerHour *float64
	IdleWeightMax     *float64
	// PreferExpiring routes to the account whose credits expire soonest, so
	// credit that is about to lapse is spent before credit that is not.
	PreferExpiring *bool
	// CostExploreInterval is how often the router may divert one request to an
	// account whose cost for the model is unknown, while an account known to
	// serve it free is available.  The diversion costs no extra upstream
	// request -- it only changes which account carries one the caller was
	// making anyway -- which is what makes probing affordable.  0 turns the
	// exploration off.
	// ExpiringSoon is the "expiring soon" window used to compute each
	// account's expiring-credit snapshot. Changing it invalidates any
	// snapshot computed under the previous window.
	ExpiringSoon        *time.Duration
	CostExploreInterval *time.Duration
}

// LiveReloader is an optional capability: a module that can apply
// LiveSettings without being reconstructed.
//
// ApplyLive must be safe to call concurrently with in-flight requests and must
// ignore (rather than reject) a zero-valued LiveSettings, so that a partial
// reload never clears a value the operator did not touch.
type LiveReloader interface {
	ApplyLive(LiveSettings)
}

// ApplyLive delivers settings to every module that accepts them and reports
// how many were updated.  A module that does not implement LiveReloader is
// skipped silently: that is the documented behaviour, not an error.
func ApplyLive(r *Registry, s LiveSettings) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, c := range r.All() {
		if lr, ok := c.(LiveReloader); ok {
			lr.ApplyLive(s)
			n++
		}
	}
	return n
}

// PlatformPolicyApplier is an optional capability: a module that wants its own
// per-platform policy pushed to it whenever the operator changes it.
//
// It exists next to LiveReloader because the two carry different scopes.
// LiveSettings is process-wide; a PlatformConfig is one platform's own routing
// policy.  The gateway reads the registry copy for its own in-flight ceilings,
// but a module that owns an account pool needs its own copy to decide which
// accounts are usable -- the low-balance guard lives in the pool, not in the
// shared request path.
type PlatformPolicyApplier interface {
	ApplyPlatformPolicy(PlatformConfig)
}

// ApplyPlatformPolicies delivers each module its own platform policy and
// reports how many modules were updated.  A module without the capability is
// skipped silently, exactly like LiveReloader.
//
// The zero PlatformConfig is the documented default, so a module with no entry
// in the map still receives it: "the operator removed the block" has to clear
// a policy the same way "the operator weakened it" does.
func ApplyPlatformPolicies(r *Registry, cfgs map[string]PlatformConfig) int {
	if r == nil {
		return 0
	}
	n := 0
	for _, c := range r.All() {
		p, ok := c.(PlatformPolicyApplier)
		if !ok {
			continue
		}
		p.ApplyPlatformPolicy(cfgs[c.Name()])
		n++
	}
	return n
}
