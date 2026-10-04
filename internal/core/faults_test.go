package core

import (
	"testing"
	"time"
)

var faultNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func TestFaultPolicyNormalisesUnsetFields(t *testing.T) {
	got := FaultPolicy{BreakerThreshold: 7}.Normalised()
	def := DefaultFaultPolicy()
	if got.BreakerThreshold != 7 {
		t.Fatalf("BreakerThreshold = %d, want the value that was set (7)", got.BreakerThreshold)
	}
	if got.BreakerCooldown != def.BreakerCooldown ||
		got.BreakerCooldownMax != def.BreakerCooldownMax ||
		got.DegradeThreshold != def.DegradeThreshold ||
		got.DegradeCooldown != def.DegradeCooldown ||
		got.DegradeCooldownMax != def.DegradeCooldownMax {
		t.Fatalf("unset fields were not filled from the defaults: %+v", got)
	}
	// A zero policy normalises to exactly the defaults.
	if zero := (FaultPolicy{}).Normalised(); zero != def {
		t.Fatalf("zero policy = %+v, want %+v", zero, def)
	}
}

func TestFaultTrackerBreakerTripsOnlyAtThreshold(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 1; i < p.BreakerThreshold; i++ {
		until, tripped := f.NoteFailure(p, faultNow)
		if tripped {
			t.Fatalf("failure %d of %d tripped the breaker", i, p.BreakerThreshold)
		}
		if !until.IsZero() {
			t.Fatalf("failure %d reported deadline %s before any trip", i, until)
		}
		if fails, retries := f.BreakerProgress(); fails != i || retries != 0 {
			t.Fatalf("after failure %d: fails=%d retries=%d, want %d/0", i, fails, retries, i)
		}
	}
	until, tripped := f.NoteFailure(p, faultNow)
	if !tripped {
		t.Fatalf("failure %d did not trip the breaker", p.BreakerThreshold)
	}
	if want := faultNow.Add(p.BreakerCooldown); !until.Equal(want) {
		t.Fatalf("first trip deadline = %s, want %s", until, want)
	}
	// The progress counter restarts so the next round is measured from scratch.
	if fails, retries := f.BreakerProgress(); fails != 0 || retries != 1 {
		t.Fatalf("after the first trip: fails=%d retries=%d, want 0/1", fails, retries)
	}
	if !f.Blocked(faultNow) {
		t.Fatal("a freshly tripped breaker must park the account")
	}
}

func TestFaultTrackerBreakerBacksOffExponentiallyThenCaps(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	want := []time.Duration{
		30 * time.Minute, // 1st trip: the base cooldown
		1 * time.Hour,    // 2nd trip: doubled
		2 * time.Hour,    // 3rd
		4 * time.Hour,    // 4th
		6 * time.Hour,    // 5th: 8h would exceed the ceiling, so it clamps
		6 * time.Hour,    // 6th: stays at the ceiling
	}
	for trip, expected := range want {
		var until time.Time
		var tripped bool
		for i := 0; i < p.BreakerThreshold; i++ {
			until, tripped = f.NoteFailure(p, faultNow)
		}
		if !tripped {
			t.Fatalf("trip %d never fired after %d failures", trip+1, p.BreakerThreshold)
		}
		if got := until.Sub(faultNow); got != expected {
			t.Fatalf("trip %d backoff = %s, want %s", trip+1, got, expected)
		}
	}
	if _, retries := f.BreakerProgress(); retries != len(want) {
		t.Fatalf("retries = %d, want %d", retries, len(want))
	}
}

func TestFaultTrackerSuccessClearsBothPenalties(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 0; i < p.BreakerThreshold; i++ {
		f.NoteFailure(p, faultNow)
	}
	for i := 0; i < p.DegradeThreshold; i++ {
		f.NoteUnpunishedFailure(p, faultNow)
	}
	if !f.Blocked(faultNow) {
		t.Fatal("setup: the account should be parked by both penalties")
	}
	f.NoteSuccess()
	if f.Blocked(faultNow) {
		t.Fatal("a success must clear the breaker and the degrade window")
	}
	if fails, retries := f.BreakerProgress(); fails != 0 || retries != 0 {
		t.Fatalf("after success: fails=%d retries=%d, want 0/0", fails, retries)
	}
	if n := f.DegradeProgress(); n != 0 {
		t.Fatalf("after success: consecutiveFails = %d, want 0", n)
	}
	// The backoff exponent is gone, so the next trip starts at the base again.
	var until time.Time
	for i := 0; i < p.BreakerThreshold; i++ {
		until, _ = f.NoteFailure(p, faultNow)
	}
	if got := until.Sub(faultNow); got != p.BreakerCooldown {
		t.Fatalf("post-success trip backoff = %s, want the base %s", got, p.BreakerCooldown)
	}
}

func TestFaultTrackerCountersAreIndependent(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	// Classified failures alone must never move the degrade counter.
	for i := 0; i < p.BreakerThreshold; i++ {
		f.NoteFailure(p, faultNow)
	}
	if n := f.DegradeProgress(); n != 0 {
		t.Fatalf("classified failures advanced the degrade counter to %d", n)
	}
	if !f.DegradeUntil().IsZero() {
		t.Fatal("classified failures opened a degrade window")
	}
	// Unclassified failures alone must never move the breaker counter.
	var g FaultTracker
	for i := 0; i < p.DegradeThreshold; i++ {
		g.NoteUnpunishedFailure(p, faultNow)
	}
	if fails, retries := g.BreakerProgress(); fails != 0 || retries != 0 {
		t.Fatalf("unclassified failures advanced the breaker to fails=%d retries=%d", fails, retries)
	}
	if !g.BreakerUntil().IsZero() {
		t.Fatal("unclassified failures opened a breaker window")
	}
}

func TestFaultTrackerDegradeTripsOnlyAtThreshold(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 1; i < p.DegradeThreshold; i++ {
		until, tripped := f.NoteUnpunishedFailure(p, faultNow)
		if tripped || !until.IsZero() {
			t.Fatalf("failure %d of %d parked the account early", i, p.DegradeThreshold)
		}
	}
	until, tripped := f.NoteUnpunishedFailure(p, faultNow)
	if !tripped {
		t.Fatalf("failure %d did not park the account", p.DegradeThreshold)
	}
	if want := faultNow.Add(p.DegradeCooldown); !until.Equal(want) {
		t.Fatalf("degrade deadline = %s, want %s", until, want)
	}
	if n := f.DegradeProgress(); n != 0 {
		t.Fatalf("degrade progress = %d, want 0 after the trip", n)
	}
}

func TestFaultTrackerDegradeDoesNotStackWhileParked(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 0; i < p.DegradeThreshold; i++ {
		f.NoteUnpunishedFailure(p, faultNow)
	}
	first := f.DegradeUntil()
	if first.IsZero() {
		t.Fatal("setup: the degrade window never opened")
	}
	// A user hammering retry must not be able to push the window further out.
	later := faultNow.Add(time.Minute)
	for i := 0; i < p.DegradeThreshold*3; i++ {
		if _, tripped := f.NoteUnpunishedFailure(p, later); tripped {
			t.Fatalf("failure %d re-tripped the degrade window while it was still in force", i+1)
		}
	}
	if got := f.DegradeUntil(); !got.Equal(first) {
		t.Fatalf("degrade deadline moved from %s to %s while parked", first, got)
	}
	// Once the window lapses, the next round of failures parks it again.
	after := first.Add(time.Second)
	var tripped bool
	for i := 0; i < p.DegradeThreshold; i++ {
		_, tripped = f.NoteUnpunishedFailure(p, after)
	}
	if !tripped {
		t.Fatal("the degrade window never re-armed after it expired")
	}
	if want := after.Add(p.DegradeCooldown); !f.DegradeUntil().Equal(want) {
		t.Fatalf("re-armed deadline = %s, want %s", f.DegradeUntil(), want)
	}
}

func TestFaultTrackerDegradeClampsToItsCeiling(t *testing.T) {
	p := FaultPolicy{DegradeThreshold: 1, DegradeCooldown: 3 * time.Hour, DegradeCooldownMax: 2 * time.Hour}
	until, tripped := (&FaultTracker{}).NoteUnpunishedFailure(p, faultNow)
	if !tripped {
		t.Fatal("threshold 1 should trip on the first failure")
	}
	if want := faultNow.Add(2 * time.Hour); !until.Equal(want) {
		t.Fatalf("degrade deadline = %s, want the ceiling %s", until, want)
	}
}

func TestFaultTrackerBlockedUntilIsTheLaterDeadline(t *testing.T) {
	p := FaultPolicy{BreakerThreshold: 1, BreakerCooldown: 10 * time.Minute, DegradeThreshold: 1, DegradeCooldown: 2 * time.Hour}
	var f FaultTracker
	f.NoteFailure(p, faultNow)
	f.NoteUnpunishedFailure(p, faultNow)
	if got, want := f.BlockedUntil(faultNow), faultNow.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("BlockedUntil = %s, want the later deadline %s", got, want)
	}
	if got, want := f.NextRecovery(faultNow), faultNow.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("NextRecovery = %s, want the earlier deadline %s", got, want)
	}
	// Only one penalty live: both accessors agree on it.
	var g FaultTracker
	g.NoteFailure(p, faultNow)
	if got, want := g.BlockedUntil(faultNow), faultNow.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("BlockedUntil with one penalty = %s, want %s", got, want)
	}
	if got, want := g.NextRecovery(faultNow), faultNow.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("NextRecovery with one penalty = %s, want %s", got, want)
	}
}

func TestFaultTrackerExpiredDeadlinesStopBlocking(t *testing.T) {
	p := FaultPolicy{BreakerThreshold: 1, BreakerCooldown: time.Minute, DegradeThreshold: 1, DegradeCooldown: time.Minute}
	var f FaultTracker
	f.NoteFailure(p, faultNow)
	f.NoteUnpunishedFailure(p, faultNow)
	if !f.Blocked(faultNow.Add(30 * time.Second)) {
		t.Fatal("the account should still be parked before the deadline")
	}
	after := faultNow.Add(2 * time.Minute)
	if f.Blocked(after) {
		t.Fatalf("the account is still parked at %s, past both deadlines", after)
	}
	if !f.BlockedUntil(after).IsZero() {
		t.Fatalf("BlockedUntil past the deadline = %s, want the zero time", f.BlockedUntil(after))
	}
	if !f.NextRecovery(after).IsZero() {
		t.Fatalf("NextRecovery past the deadline = %s, want the zero time", f.NextRecovery(after))
	}
	// The raw deadlines survive for status reporting.
	if f.BreakerUntil().IsZero() || f.DegradeUntil().IsZero() {
		t.Fatal("raw deadlines should still be readable after they expire")
	}
}

func TestFaultTrackerReviveDropsEveryPenalty(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 0; i < p.BreakerThreshold; i++ {
		f.NoteFailure(p, faultNow)
	}
	for i := 0; i < p.DegradeThreshold; i++ {
		f.NoteUnpunishedFailure(p, faultNow)
	}
	f.Revive()
	if f.Blocked(faultNow) {
		t.Fatal("Revive must clear both penalties")
	}
	if fails, retries := f.BreakerProgress(); fails != 0 || retries != 0 {
		t.Fatalf("after Revive: fails=%d retries=%d, want 0/0", fails, retries)
	}
	if n := f.DegradeProgress(); n != 0 {
		t.Fatalf("after Revive: consecutiveFails = %d, want 0", n)
	}
}

func TestFaultTrackerSnapshotRoundTrip(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 0; i < p.BreakerThreshold; i++ {
		f.NoteFailure(p, faultNow)
	}
	for i := 0; i < 2; i++ {
		f.NoteUnpunishedFailure(p, faultNow)
	}
	snap := f.Snapshot()

	var g FaultTracker
	g.Restore(snap, faultNow.Add(time.Minute))
	if got, want := g.BreakerUntil(), f.BreakerUntil(); !got.Equal(want) {
		t.Fatalf("restored breaker deadline = %s, want %s", got, want)
	}
	if got, want := g.DegradeUntil(), f.DegradeUntil(); !got.Equal(want) {
		t.Fatalf("restored degrade deadline = %s, want %s", got, want)
	}
	if got := g.DegradeProgress(); got != 2 {
		t.Fatalf("restored degrade progress = %d, want 2 (progress survives a restart)", got)
	}
	if _, retries := g.BreakerProgress(); retries != 1 {
		t.Fatalf("restored retries = %d, want 1 so the backoff exponent survives", retries)
	}
}

func TestFaultTrackerRestoreDropsExpiredDeadlines(t *testing.T) {
	p := DefaultFaultPolicy()
	var f FaultTracker
	for i := 0; i < p.BreakerThreshold; i++ {
		f.NoteFailure(p, faultNow)
	}
	for i := 0; i < p.DegradeThreshold; i++ {
		f.NoteUnpunishedFailure(p, faultNow)
	}
	snap := f.Snapshot()

	var g FaultTracker
	// Restore far past both deadlines: a penalty that would have lapsed while
	// the process was down must not come back.
	g.Restore(snap, faultNow.Add(24*time.Hour))
	if g.Blocked(faultNow.Add(24 * time.Hour)) {
		t.Fatal("a deadline that already passed was revived by Restore")
	}
	if !g.BreakerUntil().IsZero() {
		t.Fatalf("expired breaker deadline was restored as %s", g.BreakerUntil())
	}
	if _, retries := g.BreakerProgress(); retries != 0 {
		t.Fatalf("retries = %d, want 0: an exponent without a live backoff would jump to the ceiling", retries)
	}
	if !g.DegradeUntil().IsZero() {
		t.Fatalf("expired degrade deadline was restored as %s", g.DegradeUntil())
	}
}
