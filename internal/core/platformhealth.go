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
const (
	PlatformFailureWindow    = 10 * time.Minute
	PlatformFailureThreshold = 3
	PlatformCooldown         = 10 * time.Minute
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
	until    time.Time
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

// NoteSuccess clears failures and suppression for a pair.
func (h *PlatformHealth) NoteSuccess(client, model string, now time.Time) {
	if h == nil {
		return
	}
	key := healthKey(client, model)
	h.mu.Lock()
	delete(h.entries, key)
	h.mu.Unlock()
}

// Suppressed reports whether a pair is currently in cooldown.  Expired
// entries are removed on observation so the map cannot grow forever.
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
	if !e.until.IsZero() && !now.Before(e.until) {
		delete(h.entries, key)
		return false
	}
	return now.Before(e.until)
}
