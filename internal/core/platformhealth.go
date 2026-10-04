package core

import (
	"strings"
	"sync"
	"time"
)

// Platform health policy.  A "failure" is one complete gateway request round
// against a (platform, model) pair after all of that platform's account
// rotations have been exhausted.  Three rounds inside the window trip a
// cooldown and one alert; traffic then tries the other platforms first, but the
// suppressed platform stays in the candidate list so the gateway can still use
// it if every healthy platform fails.
//
// A platform can also be "unavailable" without failing: every account may be
// busy or cooling, or the module may have no account to offer.  That is
// backpressure rather than a broken upstream, so it gets a much shorter
// demotion.  The candidate stays in the list and is retried once the window
// expires, which keeps a transient saturation from turning into a permanent
// ban.
const (
	PlatformFailureWindow    = 10 * time.Minute
	PlatformFailureThreshold = 3
	PlatformCooldown         = 10 * time.Minute

	// PlatformUnavailableCooldown is how long a platform that cannot serve
	// right now is moved behind healthy candidates.  It is deliberately much
	// shorter than PlatformCooldown: the platform is saturated or temporarily
	// empty, not broken.
	PlatformUnavailableCooldown = 30 * time.Second
)

// HealthEvent is what a failure observation changed.
type HealthEvent struct {
	Failures   int
	Alert      bool
	Suppressed bool
}

type platformHealthKey struct {
	client string
	model  string
}

type platformHealthEntry struct {
	failures int
	last     time.Time
	// until is the long failure cooldown.
	until time.Time
	// degradedUntil is the short "cannot serve right now" demotion.
	degradedUntil time.Time
}

// PlatformHealth tracks consecutive failure rounds per platform and model.
// It is deliberately in-memory: a restart is a reasonable point to trust the
// platforms again, and operators can restart to clear a mistaken suppression.
type PlatformHealth struct {
	mu      sync.Mutex
	entries map[platformHealthKey]platformHealthEntry
}

// NewPlatformHealth returns an empty health tracker.
func NewPlatformHealth() *PlatformHealth {
	return &PlatformHealth{entries: map[platformHealthKey]platformHealthEntry{}}
}

func healthKey(client, model string) platformHealthKey {
	return platformHealthKey{
		client: strings.TrimSpace(client),
		model:  strings.ToLower(strings.TrimSpace(model)),
	}
}

// NoteFailure records one failed round and reports whether it tripped the
// alert threshold.  While a pair is already in cooldown, observations do not
// extend or restart that cooldown.
func (h *PlatformHealth) NoteFailure(client, model string, now time.Time) HealthEvent {
	if h == nil {
		return HealthEvent{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	key := healthKey(client, model)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries == nil {
		h.entries = map[platformHealthKey]platformHealthEntry{}
	}
	e := h.entries[key]
	if now.Before(e.until) {
		return HealthEvent{Suppressed: true}
	}
	if e.last.IsZero() || now.Sub(e.last) > PlatformFailureWindow {
		e.failures = 0
	}
	e.failures++
	e.last = now
	if e.failures >= PlatformFailureThreshold {
		e.failures = 0
		e.until = now.Add(PlatformCooldown)
		h.entries[key] = e
		return HealthEvent{Failures: PlatformFailureThreshold, Alert: true, Suppressed: true}
	}
	h.entries[key] = e
	return HealthEvent{Failures: e.failures}
}

// NoteUnavailable records that a platform cannot serve the request right now
// without treating that as an upstream failure.  It demotes the pair for a
// short window and never raises an alert.
func (h *PlatformHealth) NoteUnavailable(client, model string, now time.Time) HealthEvent {
	if h == nil {
		return HealthEvent{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	key := healthKey(client, model)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries == nil {
		h.entries = map[platformHealthKey]platformHealthEntry{}
	}
	e := h.entries[key]
	if now.Before(e.until) {
		return HealthEvent{Suppressed: true}
	}
	e.degradedUntil = now.Add(PlatformUnavailableCooldown)
	h.entries[key] = e
	return HealthEvent{}
}

// NoteSuccess clears failures and any demotion for a pair.
func (h *PlatformHealth) NoteSuccess(client, model string, now time.Time) {
	if h == nil {
		return
	}
	key := healthKey(client, model)
	h.mu.Lock()
	delete(h.entries, key)
	h.mu.Unlock()
}

// Suppressed reports whether a pair is in the long failure cooldown.  The
// short "cannot serve right now" demotion is reported separately by Degraded,
// so callers can tell a broken upstream from backpressure.  Expired entries
// are removed on observation so the map cannot grow forever.
func (h *PlatformHealth) Suppressed(client, model string, now time.Time) bool {
	if h == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	key := healthKey(client, model)
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	if !ok {
		return false
	}
	if !now.Before(e.until) {
		e.until = time.Time{}
		// An entry may still be accumulating failures without an active
		// cooldown; keep it so the next round can reach the threshold.
		if e.degradedUntil.IsZero() || !now.Before(e.degradedUntil) {
			e.degradedUntil = time.Time{}
			if e.failures == 0 {
				delete(h.entries, key)
				return false
			}
		}
		h.entries[key] = e
		return false
	}
	return true
}

// Degraded reports whether a pair is in the short "cannot serve right now"
// demotion, as opposed to the long failure cooldown.
func (h *PlatformHealth) Degraded(client, model string, now time.Time) bool {
	if h == nil {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}
	key := healthKey(client, model)
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.entries[key]
	if !ok {
		return false
	}
	if !e.degradedUntil.IsZero() && !now.Before(e.degradedUntil) {
		e.degradedUntil = time.Time{}
		if (e.until.IsZero() || !now.Before(e.until)) && e.failures == 0 {
			delete(h.entries, key)
			return false
		}
		h.entries[key] = e
		return false
	}
	return now.Before(e.degradedUntil)
}
