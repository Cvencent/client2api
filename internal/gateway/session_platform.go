package gateway

import (
	"sort"
	"sync"
	"time"

	"client2api/internal/core"
)

// sessionPlatforms remembers which platform last served a conversation.
//
// The account pools already pin a conversation to one credential inside a
// module.  A bare model id, however, can be served by several modules, and that
// binding is invisible to every pool: the gateway chooses the module, so only
// the gateway can keep the conversation on the platform that last worked.
//
// The table is deliberately in-memory.  A restart is a reasonable moment to
// re-rank platforms, and the binding is an optimisation, never a correctness
// requirement: a missing or unusable entry means "rank normally".
type SessionPlatforms struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]sessionPlatformEntry
	now     func() time.Time

	// gcMu guards only the GC goroutine lifecycle, so StartGC/StopGC never
	// contend with the request path.
	gcMu    sync.Mutex
	stopGC  chan struct{}
	gcEvery time.Duration
}

// sessionPlatforms keeps the old internal spelling used by the package's
// request path while the exported type lets the panel share the same table.
type sessionPlatforms = SessionPlatforms

type sessionPlatformEntry struct {
	platform string
	model    string
	manual   bool
	lastSeen time.Time
}

// sessionPlatformTTL is the default idle window for a platform binding.  It
// mirrors core.DefaultAffinityTTL so the gateway and the modules age their
// conversation state on the same clock.
const sessionPlatformTTL = 30 * time.Minute

const sessionPlatformGCInterval = 5 * time.Minute

func newSessionPlatforms() *sessionPlatforms {
	return NewSessionPlatforms()
}

// NewSessionPlatforms returns an empty platform-stickiness table.  The caller
// can inject one into both the gateway and the panel so an operator action sees
// the same routes the request path uses.
func NewSessionPlatforms() *SessionPlatforms {
	return &sessionPlatforms{
		ttl:     sessionPlatformTTL,
		entries: map[string]sessionPlatformEntry{},
		now:     time.Now,
	}
}

// bind records that platform served conversationKey.  An empty key or platform
// is a no-op: unscoped requests keep the old rotation behaviour.
func (s *sessionPlatforms) bind(conversationKey, platform string) {
	s.bindModel(conversationKey, platform, "")
}

// bindModel records the platform and resolved upstream model that served
// conversationKey. The model is display and targeting metadata; routing still
// keys off the platform.
func (s *sessionPlatforms) bindModel(conversationKey, platform, model string) {
	if s == nil || conversationKey == "" || platform == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]sessionPlatformEntry{}
	}
	if current, ok := s.entries[conversationKey]; ok && current.manual {
		return
	}
	s.entries[conversationKey] = sessionPlatformEntry{platform: platform, model: model, lastSeen: s.clock()}
}

// resolve reports the platform bound to conversationKey, and only when that
// platform is still usable.  An expired or unusable binding is forgotten, so
// the caller falls back to normal candidate ranking.
func (s *sessionPlatforms) resolve(conversationKey string, usable func(string) bool) (string, bool) {
	if s == nil || conversationKey == "" {
		return "", false
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[conversationKey]
	if !ok {
		return "", false
	}
	if s.ttl > 0 && now.Sub(e.lastSeen) > s.ttl {
		delete(s.entries, conversationKey)
		return "", false
	}
	if usable != nil && !usable(e.platform) && !e.manual {
		delete(s.entries, conversationKey)
		return "", false
	}
	return e.platform, true
}

// forget drops a binding, used when the pinned platform is known to be bad.
func (s *sessionPlatforms) forget(conversationKey string) {
	s.Forget(conversationKey)
}

// StickyRoute is one live conversation-to-platform binding.
type StickyRoute struct {
	Key       string    `json:"key"`
	Platform  string    `json:"platform"`
	Model     string    `json:"model,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// List returns the live platform routes in stable key order.
func (s *SessionPlatforms) List() []StickyRoute {
	if s == nil {
		return nil
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]StickyRoute, 0, len(s.entries))
	for key, e := range s.entries {
		if s.ttl > 0 && now.Sub(e.lastSeen) > s.ttl {
			continue
		}
		row := StickyRoute{Key: key, Platform: e.platform, Model: e.model, LastSeen: e.lastSeen}
		if s.ttl > 0 {
			row.ExpiresAt = e.lastSeen.Add(s.ttl)
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Pin rewrites one platform route without touching the account layer.  It is
// the targeted half of the panel's force-switch action.
func (s *SessionPlatforms) Pin(conversationKey, platform, model string) bool {
	if s == nil || conversationKey == "" || platform == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]sessionPlatformEntry{}
	}
	s.entries[conversationKey] = sessionPlatformEntry{
		platform: platform,
		model:    model,
		manual:   true,
		lastSeen: s.clock(),
	}
	return true
}

// Forget drops one platform route and reports whether it existed.
func (s *SessionPlatforms) Forget(conversationKey string) bool {
	if s == nil || conversationKey == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[conversationKey]; !ok {
		return false
	}
	delete(s.entries, conversationKey)
	return true
}

// setTTL retunes the idle window on a live reload.
func (s *sessionPlatforms) setTTL(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.mu.Lock()
	s.ttl = d
	s.mu.Unlock()
	s.setGCInterval(d)
}

func (s *sessionPlatforms) setGCInterval(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.gcMu.Lock()
	s.gcEvery = d
	s.gcMu.Unlock()
}

// GC drops every expired, non-manual binding and reports how many it removed.
// Manual pins are operator state, not idle heuristics, so the sweep leaves them
// alone just as resolve does.
func (s *SessionPlatforms) GC() int {
	if s == nil {
		return 0
	}
	now := s.clock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ttl <= 0 {
		return 0
	}
	removed := 0
	for key, e := range s.entries {
		if e.manual {
			continue
		}
		if now.Sub(e.lastSeen) > s.ttl {
			delete(s.entries, key)
			removed++
		}
	}
	return removed
}

// StartGC launches the idle sweep. It is idempotent and nil-safe.
func (s *SessionPlatforms) StartGC() {
	if s == nil {
		return
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.stopGC != nil {
		return
	}
	stop := make(chan struct{})
	s.stopGC = stop
	every := s.gcEvery
	if every <= 0 {
		every = sessionPlatformGCInterval
	}
	core.GoSafe("session platform sweep", nil, func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.GC()
			}
		}
	})
}

// StopGC stops the idle sweep. Calling it without StartGC, or twice, is
// harmless, and it is nil-safe.
func (s *SessionPlatforms) StopGC() {
	if s == nil {
		return
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	if s.stopGC == nil {
		return
	}
	close(s.stopGC)
	s.stopGC = nil
}

func (s *sessionPlatforms) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// prioritize moves the bound platform to the front of a candidate list.  The
// binding never removes a candidate: if the pinned platform fails, the rest of
// the list remains available as failover.
func prioritizeStickyPlatform(candidates []core.Candidate, platform string) []core.Candidate {
	if platform == "" || len(candidates) < 2 {
		return candidates
	}
	idx := -1
	for i, c := range candidates {
		if c.Client != nil && c.Client.Name() == platform {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return candidates
	}
	out := make([]core.Candidate, 0, len(candidates))
	out = append(out, candidates[idx])
	out = append(out, candidates[:idx]...)
	out = append(out, candidates[idx+1:]...)
	return out
}
