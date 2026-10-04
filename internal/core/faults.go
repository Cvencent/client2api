package core

import "time"

// FaultPolicy tunes the two account-level penalty mechanisms that sit beside a
// module's own cooldown state.
//
//   - The breaker parks an account for an exponentially growing (and capped)
//     window once BreakerThreshold *classified* failures pile up without an
//     intervening success. It answers "the upstream did tell us what went
//     wrong, but not for how long".
//   - The degrade counter parks an account for a fixed window once
//     DegradeThreshold *unclassified* failures pile up. It answers "something
//     keeps going wrong and we have no classification to reason from".
//
// The two counters are deliberately separate, and a module evaluates both as an
// OR against its own cooldown: the effective penalty is whichever deadline is
// furthest out, so the mechanisms coexist and never sum. Feeding one event to
// both counters would punish it twice, which is why the caller has to pick.
type FaultPolicy struct {
	BreakerThreshold   int
	BreakerCooldown    time.Duration
	BreakerCooldownMax time.Duration
	DegradeThreshold   int
	DegradeCooldown    time.Duration
	DegradeCooldownMax time.Duration
}

// DefaultFaultPolicy returns the reference pool's defaults: trip the breaker
// after 3 classified failures for 30 minutes, doubling per trip up to 6 hours;
// park an account after 5 unclassified failures for 10 minutes.
func DefaultFaultPolicy() FaultPolicy {
	return FaultPolicy{
		BreakerThreshold:   3,
		BreakerCooldown:    30 * time.Minute,
		BreakerCooldownMax: 6 * time.Hour,
		DegradeThreshold:   5,
		DegradeCooldown:    10 * time.Minute,
		DegradeCooldownMax: 2 * time.Hour,
	}
}

// Normalised replaces every unset field with its default, so a config file that
// set only one key still behaves like the reference everywhere else. The
// reference keeps the current value for a non-positive setter argument, which
// is the same contract expressed at the field level.
func (p FaultPolicy) Normalised() FaultPolicy {
	d := DefaultFaultPolicy()
	if p.BreakerThreshold <= 0 {
		p.BreakerThreshold = d.BreakerThreshold
	}
	if p.BreakerCooldown <= 0 {
		p.BreakerCooldown = d.BreakerCooldown
	}
	if p.BreakerCooldownMax <= 0 {
		p.BreakerCooldownMax = d.BreakerCooldownMax
	}
	if p.DegradeThreshold <= 0 {
		p.DegradeThreshold = d.DegradeThreshold
	}
	if p.DegradeCooldown <= 0 {
		p.DegradeCooldown = d.DegradeCooldown
	}
	if p.DegradeCooldownMax <= 0 {
		p.DegradeCooldownMax = d.DegradeCooldownMax
	}
	return p
}

// FaultSnapshot is a tracker's counters and deadlines, for a module that wants
// to carry them across a restart. The deadlines are absolute instants; a
// restored deadline that has already passed is dropped rather than revived.
type FaultSnapshot struct {
	BreakerUntil   time.Time
	BreakerRetries int
	DegradeUntil   time.Time
	DegradeFails   int
}

// FaultTracker carries one account's breaker and degrade state.
//
// It holds no lock of its own on purpose: a module embeds it in a pool entry
// and touches it under whatever lock the pool already holds, which is what
// keeps these deadlines consistent with the module's own cooldown fields. Every
// method is therefore safe only under that lock, and every method takes the
// caller's `now` so a whole selection pass sees one instant.
type FaultTracker struct {
	fails            int
	retries          int
	breakerUntil     time.Time
	consecutiveFails int
	degradeUntil     time.Time
}

// NoteFailure records one failure the upstream classified authoritatively — a
// 5xx, or a credential refresh the upstream rejected. Such failures already
// carry a penalty of their own, so a caller must never feed the same event to
// NoteUnpunishedFailure as well.
//
// It returns the new breaker deadline, and whether this call tripped it. The
// deadline is reported even when the call did not trip anything, so a caller
// can log the standing penalty without re-deriving it.
func (f *FaultTracker) NoteFailure(p FaultPolicy, now time.Time) (until time.Time, tripped bool) {
	p = p.Normalised()
	f.fails++
	if f.fails < p.BreakerThreshold {
		return f.breakerUntil, false
	}
	// Trip: reset the progress counter for the next round, and let the number
	// of trips so far set the backoff exponent.
	d := p.BreakerCooldown
	for i := 0; i < f.retries; i++ {
		d *= 2
		if d >= p.BreakerCooldownMax {
			d = p.BreakerCooldownMax
			break
		}
	}
	f.fails = 0
	f.retries++
	f.breakerUntil = now.Add(d)
	return f.breakerUntil, true
}

// NoteUnpunishedFailure records one failure the module could not classify: an
// unknown 4xx, or a transport-layer error. Those are exactly the failures with
// no authoritative "wait this long" instruction, which is why they get the
// degrade counter instead of the breaker.
//
// Reaching the threshold while already degraded does not extend or double the
// window: a user hammering retry must not be able to stack the penalty ever
// deeper. The progress counter still resets, so the next round of failures is
// measured from scratch.
func (f *FaultTracker) NoteUnpunishedFailure(p FaultPolicy, now time.Time) (until time.Time, tripped bool) {
	p = p.Normalised()
	f.consecutiveFails++
	if f.consecutiveFails < p.DegradeThreshold {
		return f.degradeUntil, false
	}
	f.consecutiveFails = 0
	if !f.degradeUntil.IsZero() && now.Before(f.degradeUntil) {
		return f.degradeUntil, false
	}
	d := p.DegradeCooldown
	if d > p.DegradeCooldownMax {
		d = p.DegradeCooldownMax
	}
	f.degradeUntil = now.Add(d)
	return f.degradeUntil, true
}

// NoteSuccess clears both counters and both deadlines. A success is the
// strongest recovery evidence an account can produce: it proves the chat path
// works, which is precisely what a balance refresh or a check-in does not
// prove. A module must still leave its model-scoped cooldowns alone — one
// model succeeding says nothing about another model's rate limit.
func (f *FaultTracker) NoteSuccess() {
	f.fails = 0
	f.retries = 0
	f.breakerUntil = time.Time{}
	f.consecutiveFails = 0
	f.degradeUntil = time.Time{}
}

// Revive is the operator override behind a panel's unfreeze action: it drops
// every penalty this tracker holds, on the assumption that a human has decided
// the account is usable again. It is the unconditional counterpart of
// NoteSuccess, which only fires when the upstream actually served a request.
func (f *FaultTracker) Revive() {
	f.NoteSuccess()
}

// Blocked reports whether either penalty is currently in force.
func (f *FaultTracker) Blocked(now time.Time) bool {
	return !f.BlockedUntil(now).IsZero()
}

// BlockedUntil returns the deadline the account is parked until, or the zero
// time when neither penalty is in force. When both are live it returns the
// later one, because that is the instant the account actually becomes eligible
// again — the OR gate a module applies, expressed as one value.
func (f *FaultTracker) BlockedUntil(now time.Time) time.Time {
	breaker := f.activeUntil(f.breakerUntil, now)
	degrade := f.activeUntil(f.degradeUntil, now)
	switch {
	case breaker.IsZero():
		return degrade
	case degrade.IsZero():
		return breaker
	case degrade.After(breaker):
		return degrade
	default:
		return breaker
	}
}

// NextRecovery returns the earliest of the live deadlines, or the zero time
// when neither penalty is in force. A module uses it to answer "when could this
// account come back at the soonest", which is what makes a fallback pick
// worthwhile when every account is parked.
func (f *FaultTracker) NextRecovery(now time.Time) time.Time {
	breaker := f.activeUntil(f.breakerUntil, now)
	degrade := f.activeUntil(f.degradeUntil, now)
	switch {
	case breaker.IsZero():
		return degrade
	case degrade.IsZero():
		return breaker
	case degrade.Before(breaker):
		return degrade
	default:
		return breaker
	}
}

// activeUntil reports t when it is still in the future, and the zero time
// otherwise, so an expired deadline reads as "not parked" everywhere.
func (f *FaultTracker) activeUntil(t time.Time, now time.Time) time.Time {
	if t.IsZero() || !now.Before(t) {
		return time.Time{}
	}
	return t
}

// BreakerUntil reports the raw breaker deadline, expired or not. It exists for
// status reporting, where "tripped at 12:04, already past" is worth showing.
func (f *FaultTracker) BreakerUntil() time.Time { return f.breakerUntil }

// DegradeUntil reports the raw degrade deadline, expired or not.
func (f *FaultTracker) DegradeUntil() time.Time { return f.degradeUntil }

// BreakerProgress reports the classified failures counted toward the next trip,
// and how many trips have happened so far (the backoff exponent).
func (f *FaultTracker) BreakerProgress() (fails, retries int) {
	return f.fails, f.retries
}

// DegradeProgress reports the unclassified failures counted toward the next
// park.
func (f *FaultTracker) DegradeProgress() int { return f.consecutiveFails }

// Snapshot returns the state worth persisting. A module without a state file
// simply never calls it, in which case the penalties live and die with the
// process — the same lifetime its existing cooldowns already have.
func (f *FaultTracker) Snapshot() FaultSnapshot {
	return FaultSnapshot{
		BreakerUntil:   f.breakerUntil,
		BreakerRetries: f.retries,
		DegradeUntil:   f.degradeUntil,
		DegradeFails:   f.consecutiveFails,
	}
}

// Restore seeds the tracker from a snapshot, dropping any deadline that has
// already passed so a restart cannot revive a penalty that would have expired
// while the process was down. The trip counter is restored with its deadline:
// it is only meaningful as the exponent of a live backoff, and keeping it after
// the deadline lapses would make the next trip jump straight to the ceiling.
func (f *FaultTracker) Restore(s FaultSnapshot, now time.Time) {
	f.fails = 0
	f.retries = 0
	f.breakerUntil = time.Time{}
	f.consecutiveFails = s.DegradeFails
	f.degradeUntil = time.Time{}
	if !s.BreakerUntil.IsZero() && now.Before(s.BreakerUntil) {
		f.breakerUntil = s.BreakerUntil
		f.retries = s.BreakerRetries
	}
	if !s.DegradeUntil.IsZero() && now.Before(s.DegradeUntil) {
		f.degradeUntil = s.DegradeUntil
	}
}
