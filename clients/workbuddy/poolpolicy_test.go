package workbuddy

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// applyPoolTuning is the only path from a live-settings reload into the pool's
// fault policy, soft rate cap, idle-weight curve, expiry routing preference and
// cost-exploration window.  It had no test of its own: ApplyLive is covered,
// but always with a nil Pool, so every branch below was dead as far as the
// suite was concerned.  A knob that quietly stopped reaching the pool would
// have looked exactly like a knob that worked.

// poolWithPolicy builds the client every test here needs, and refuses to
// continue if New ever stops attaching a pool -- otherwise the assertions below
// would pass by inspecting nothing.
func poolWithPolicy(t *testing.T) *Client {
	t.Helper()
	c, _ := newTestClient(t, nil)
	if c.pool == nil {
		t.Fatal("the test client has no pool; nothing below would prove anything")
	}
	return c
}

func TestApplyPoolTuningReachesEveryKnob(t *testing.T) {
	c := poolWithPolicy(t)

	breakerThreshold := 7
	breakerCooldown := 11 * time.Minute
	breakerCooldownMax := 3 * time.Hour
	degradeThreshold := 9
	degradeCooldown := 4 * time.Minute
	degradeCooldownMax := 90 * time.Minute
	softRateMax := 45 * time.Minute
	idlePerHour := 1.5
	idleMax := 12.0
	preferExpiring := true
	costExplore := 20 * time.Minute

	c.applyPoolTuning(&core.PoolTuning{
		BreakerThreshold:    &breakerThreshold,
		BreakerCooldown:     &breakerCooldown,
		BreakerCooldownMax:  &breakerCooldownMax,
		DegradeThreshold:    &degradeThreshold,
		DegradeCooldown:     &degradeCooldown,
		DegradeCooldownMax:  &degradeCooldownMax,
		SoftRateMax:         &softRateMax,
		IdleWeightPerHour:   &idlePerHour,
		IdleWeightMax:       &idleMax,
		PreferExpiring:      &preferExpiring,
		CostExploreInterval: &costExplore,
	})

	got := c.pool.Policy()
	if got.BreakerThreshold != breakerThreshold {
		t.Errorf("BreakerThreshold = %d, want %d", got.BreakerThreshold, breakerThreshold)
	}
	if got.BreakerCooldown != breakerCooldown {
		t.Errorf("BreakerCooldown = %s, want %s", got.BreakerCooldown, breakerCooldown)
	}
	if got.BreakerCooldownMax != breakerCooldownMax {
		t.Errorf("BreakerCooldownMax = %s, want %s", got.BreakerCooldownMax, breakerCooldownMax)
	}
	if got.DegradeThreshold != degradeThreshold {
		t.Errorf("DegradeThreshold = %d, want %d", got.DegradeThreshold, degradeThreshold)
	}
	if got.DegradeCooldown != degradeCooldown {
		t.Errorf("DegradeCooldown = %s, want %s", got.DegradeCooldown, degradeCooldown)
	}
	if got.DegradeCooldownMax != degradeCooldownMax {
		t.Errorf("DegradeCooldownMax = %s, want %s", got.DegradeCooldownMax, degradeCooldownMax)
	}

	// softRateMax has no exported getter -- it is only ever read inside the
	// pool's own cooldown arithmetic -- so the field is inspected directly.
	if c.pool.softRateMax != softRateMax {
		t.Errorf("softRateMax = %s, want %s", c.pool.softRateMax, softRateMax)
	}

	if perHour, max := c.pool.Weights(); perHour != idlePerHour || max != idleMax {
		t.Errorf("Weights() = %v/%v, want %v/%v", perHour, max, idlePerHour, idleMax)
	}
	if !c.pool.PreferExpiring() {
		t.Error("PreferExpiring() = false, want the true that was just set")
	}
	if got := c.pool.CostExploreInterval(); got != costExplore {
		t.Errorf("CostExploreInterval() = %s, want %s", got, costExplore)
	}
}

// A reload that mentions one knob must not reset the others.  The assign
// helpers only write when the source pointer is non-nil, and this is what
// proves it: a naive "copy every field" would blank the rest back to zero and
// then Normalised would silently restore the defaults.
func TestApplyPoolTuningLeavesUnsetKnobsAlone(t *testing.T) {
	c := poolWithPolicy(t)

	// Establish a non-default baseline so a reset would be visible.
	threshold := 8
	cooldown := 7 * time.Minute
	softRateMax := 25 * time.Minute
	idlePerHour := 2.0
	idleMax := 9.0
	c.applyPoolTuning(&core.PoolTuning{
		BreakerThreshold:  &threshold,
		BreakerCooldown:   &cooldown,
		SoftRateMax:       &softRateMax,
		IdleWeightPerHour: &idlePerHour,
		IdleWeightMax:     &idleMax,
	})
	c.pool.SetPreferExpiring(true)
	c.pool.SetCostExploreInterval(15 * time.Minute)

	before := c.pool.Policy()
	beforePerHour, beforeMax := c.pool.Weights()
	beforeSoft := c.pool.softRateMax
	beforeExplore := c.pool.CostExploreInterval()

	// One knob only.
	newCooldown := 90 * time.Minute
	c.applyPoolTuning(&core.PoolTuning{BreakerCooldown: &newCooldown})

	after := c.pool.Policy()
	if after.BreakerCooldown != newCooldown {
		t.Errorf("BreakerCooldown = %s, want the %s that was just set", after.BreakerCooldown, newCooldown)
	}
	if after != (core.FaultPolicy{
		BreakerThreshold:   before.BreakerThreshold,
		BreakerCooldown:    newCooldown,
		BreakerCooldownMax: before.BreakerCooldownMax,
		DegradeThreshold:   before.DegradeThreshold,
		DegradeCooldown:    before.DegradeCooldown,
		DegradeCooldownMax: before.DegradeCooldownMax,
	}) {
		t.Errorf("a one-knob reload changed more than that knob:\n got %+v\nwant %+v with only BreakerCooldown at %s",
			after, before, newCooldown)
	}
	if perHour, max := c.pool.Weights(); perHour != beforePerHour || max != beforeMax {
		t.Errorf("Weights() moved from %v/%v to %v/%v", beforePerHour, beforeMax, perHour, max)
	}
	if c.pool.softRateMax != beforeSoft {
		t.Errorf("softRateMax moved from %s to %s", beforeSoft, c.pool.softRateMax)
	}
	if !c.pool.PreferExpiring() {
		t.Error("PreferExpiring() was reset to false by an unrelated reload")
	}
	if got := c.pool.CostExploreInterval(); got != beforeExplore {
		t.Errorf("CostExploreInterval() moved from %s to %s", beforeExplore, got)
	}
}

// SetPolicy normalises, so a threshold of zero comes back as the default.  That
// read-back is the reason applyPoolTuning re-reads the policy instead of logging
// what it was handed; this pins the behaviour so a future edit cannot start
// reporting a number the pool is not using.
func TestApplyPoolTuningNormalisesAnUnusableThreshold(t *testing.T) {
	c := poolWithPolicy(t)

	zero := 0
	c.applyPoolTuning(&core.PoolTuning{BreakerThreshold: &zero})

	want := core.DefaultFaultPolicy().BreakerThreshold
	if got := c.pool.Policy().BreakerThreshold; got != want {
		t.Errorf("BreakerThreshold = %d after being set to 0, want the normalised default %d", got, want)
	}
}

// Zero is a legal cost-exploration interval and means "never explore" -- the
// one setter that does not read a non-positive value as "keep the current
// setting".  If that exception ever regressed, the detour could not be turned
// off from the panel at all.
func TestApplyPoolTuningCanTurnCostExplorationOff(t *testing.T) {
	c := poolWithPolicy(t)

	on := 30 * time.Minute
	c.applyPoolTuning(&core.PoolTuning{CostExploreInterval: &on})
	if got := c.pool.CostExploreInterval(); got != on {
		t.Fatalf("CostExploreInterval() = %s, want %s", got, on)
	}

	off := time.Duration(0)
	c.applyPoolTuning(&core.PoolTuning{CostExploreInterval: &off})
	if got := c.pool.CostExploreInterval(); got != 0 {
		t.Errorf("CostExploreInterval() = %s after being set to 0, want 0 (exploration off)", got)
	}
}

// Every caller goes through a client that may have no pool (a credential file
// that never loaded) and a settings snapshot whose tuning group may be absent.
// None of those may panic, and none may touch anything.
func TestApplyPoolTuningToleratesMissingInputs(t *testing.T) {
	var nilClient *Client
	nilClient.applyPoolTuning(&core.PoolTuning{}) // must not panic

	(&Client{}).applyPoolTuning(&core.PoolTuning{}) // pool is nil

	withPool := poolWithPolicy(t)
	before := withPool.pool.Policy()
	withPool.applyPoolTuning(nil)
	if after := withPool.pool.Policy(); after != before {
		t.Errorf("a nil tuning group changed the policy from %+v to %+v", before, after)
	}

	// An empty group is not nil, but every field inside it is, so it is a
	// no-op too.
	withPool.applyPoolTuning(&core.PoolTuning{})
	if after := withPool.pool.Policy(); after != before {
		t.Errorf("an empty tuning group changed the policy from %+v to %+v", before, after)
	}
}
