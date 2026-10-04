// Package livecfg holds the small set of settings that the management panel
// may change while the process is running.
//
// Ported from the reference internal/livecfg.  The startup configuration is
// read once into ordinary fields (read-many, write-never), but a few values
// are read deep in the request path and must be editable online.  An immutable
// snapshot behind an atomic pointer gives readers a consistent view with no
// lock and no data race.
//
// Only the fields with a deep read path and a real need to change live belong
// here.  Everything else keeps its own setter (a pool's SetBreaker, the
// scheduler's Reconfigure, and so on) and is not duplicated into this holder.
package livecfg

import (
	"sync/atomic"
	"time"
)

// Snapshot is one consistent read of the live-editable configuration.
type Snapshot struct {
	// APIKey is the shared gateway/panel bearer.  Empty disables auth, which
	// is why the panel refuses to clear it without an explicit acknowledgement.
	APIKey string `json:"api_key"`
	// AuthEnabled mirrors "an API key is set".  It is derived, not stored, so
	// that the panel cannot show a key as configured while enforcement is off.
	SoftCooldown time.Duration `json:"soft_cooldown"`
	// SanitizeFingerprints toggles the outbound body sanitiser.
	SanitizeFingerprints bool `json:"sanitize_fingerprints"`
	// PromptMode / PromptFile drive system-prompt replacement.
	PromptMode string `json:"prompt_mode"`
	PromptFile string `json:"prompt_file"`
	// MaxRotate and RotateBackoffBase let an operator tune failover without a
	// restart; <=0 means "use the built-in default".
	MaxRotate         int           `json:"max_rotate"`
	RotateBackoffBase time.Duration `json:"rotate_backoff_base"`
	// AffinityTTL is the session_sticky.ttl window.  The modules use it for
	// account stickiness; the gateway uses the same value for platform
	// stickiness, so a reload moves both tables together.
	// ExpiringSoon is the balance/expiry window shared by the panel and the
	// WorkBuddy pool. A reload must move both together.
	ExpiringSoon time.Duration `json:"expiring_soon"`
	AffinityTTL  time.Duration `json:"affinity_ttl"`
}

// AuthEnabled reports whether inbound auth is enforced.
func (s Snapshot) AuthEnabled() bool { return s.APIKey != "" }

// Holder atomically holds the current snapshot.
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New builds a Holder from an initial snapshot.
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load returns the current snapshot.  A nil Holder, or one that was never
// stored, yields the zero snapshot, so callers never have to nil-check.
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store replaces the snapshot wholesale.
func (h *Holder) Store(s Snapshot) {
	if h == nil {
		return
	}
	h.p.Store(&s)
}

// Update applies fn to a copy of the current snapshot and stores the result.
//
// It is a read-modify-write and therefore not atomic: two concurrent Updates can
// both load the same snapshot and the second Store overwrites the first one's
// change.  That is acceptable for the panel's config editor, where the fields
// being changed come from one form submission and one writer is the norm, and it
// is why the panel saves through a single request rather than one request per
// field.  A caller that needs real mutual exclusion should serialize its own
// writes; do not reach for a lock here, since the read path is lock-free by
// design and adding one would put it behind contention for every request.
func (h *Holder) Update(fn func(*Snapshot)) {
	if h == nil || fn == nil {
		return
	}
	s := h.Load()
	fn(&s)
	h.Store(s)
}
