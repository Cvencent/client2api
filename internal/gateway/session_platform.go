package gateway

import (
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
type sessionPlatforms struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]sessionPlatformEntry
	now     func() time.Time
}

type sessionPlatformEntry struct {
	platform string
	lastSeen time.Time
}

// sessionPlatformTTL is the default idle window for a platform binding.  It
// mirrors core.DefaultAffinityTTL so the gateway and the modules age their
// conversation state on the same clock.
const sessionPlatformTTL = 30 * time.Minute

func newSessionPlatforms() *sessionPlatforms {
	return &sessionPlatforms{
		ttl:     sessionPlatformTTL,
		entries: map[string]sessionPlatformEntry{},
		now:     time.Now,
	}
}

// bind records that platform served conversationKey.  An empty key or platform
// is a no-op: unscoped requests keep the old rotation behaviour.
func (s *sessionPlatforms) bind(conversationKey, platform string) {
	if s == nil || conversationKey == "" || platform == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]sessionPlatformEntry{}
	}
	s.entries[conversationKey] = sessionPlatformEntry{platform: platform, lastSeen: s.clock()}
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
	if usable != nil && !usable(e.platform) {
		delete(s.entries, conversationKey)
		return "", false
	}
	return e.platform, true
}

// forget drops a binding, used when the pinned platform is known to be bad.
func (s *sessionPlatforms) forget(conversationKey string) {
	if s == nil || conversationKey == "" {
		return
	}
	s.mu.Lock()
	delete(s.entries, conversationKey)
	s.mu.Unlock()
}

// setTTL retunes the idle window on a live reload.
func (s *sessionPlatforms) setTTL(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.mu.Lock()
	s.ttl = d
	s.mu.Unlock()
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
