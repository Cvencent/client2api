// Package modelpark remembers short-lived, model-scoped upstream rate-limit
// windows the gateway observed.
//
// The panel already renders a module's own per-model parks when it publishes
// Status().Accounts[].Extra["model_cooldowns"].  Modules that keep only an
// account-level cooldown never publish that key, so a 429 that really was
// scoped to one model looked "available" in the model dialog.  This store is
// the gateway's platform-independent fallback: every FailureRateLimited the
// gateway sees is parked here for the (platform, account, model) triple, the
// account acquirer refuses that triple until the window lapses, and the panel
// merges the parks into the same key it already knows how to render.
//
// Parks are intentionally runtime-only.  A restart loses at most one window
// and re-learns it with a single failed attempt; persisting them would make
// the gateway's data files part of the routing contract for no real gain.
package modelpark

import (
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// BasePark is the first park after a rate limit that carried no
	// Retry-After hint.
	BasePark = 60 * time.Second
	// MaxPark caps any single park, including one the vendor asked to be
	// longer: a bad or hostile header must not remove an account for a day.
	MaxPark = 30 * time.Minute
	// maxShift bounds the doubling so the shift itself cannot overflow.
	maxShift = 8
)

type parkKey struct {
	platform string
	account  string
	model    string
}

type parkRecord struct {
	until  time.Time
	reason string
	hits   int
}

// Store holds the live parks.  It is safe for concurrent use by the gateway
// (which writes and reads on the request path) and the panel (which reads
// while rendering status).
type Store struct {
	mu    sync.Mutex
	now   func() time.Time
	parks map[parkKey]parkRecord
}

// New returns an empty store.
func New() *Store {
	return &Store{now: time.Now, parks: map[parkKey]parkRecord{}}
}

// Park records one rate-limit window and returns how long it will last.
//
// A park that is still live when the same triple fails again doubles: 60s,
// 120s, 240s ... capped at MaxPark.  A positive retryAfter overrides the
// backoff, capped the same way.  Empty identifiers are ignored: a module that
// could not name the account or the model must not create a park the panel
// cannot attribute.
func (s *Store) Park(platform, account, model, reason string, retryAfter time.Duration) time.Duration {
	if s == nil {
		return 0
	}
	k, ok := newParkKey(platform, account, model)
	if !ok {
		return 0
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	hits := 1
	if prev, ok := s.parks[k]; ok && now.Before(prev.until) {
		hits = prev.hits + 1
	}
	d := parkDuration(hits, retryAfter)
	s.parks[k] = parkRecord{
		until:  now.Add(d),
		reason: strings.TrimSpace(reason),
		hits:   hits,
	}
	return d
}

// Active reports the park for one triple when it has not lapsed.
func (s *Store) Active(platform, account, model string) (core.ModelPark, bool) {
	if s == nil {
		return core.ModelPark{}, false
	}
	k, ok := newParkKey(platform, account, model)
	if !ok {
		return core.ModelPark{}, false
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	p, ok := s.parks[k]
	if !ok {
		return core.ModelPark{}, false
	}
	return core.NewModelPark(k.model, core.ModelParkRateLimit, p.reason, p.until, time.Time{}), true
}

// ForAccount returns every live park for one account, sorted by model.  The
// panel merges this into the account's model_cooldowns list.
func (s *Store) ForAccount(platform, account string) []core.ModelPark {
	if s == nil {
		return nil
	}
	platform, account = strings.TrimSpace(platform), strings.TrimSpace(account)
	if platform == "" || account == "" {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	out := make([]core.ModelPark, 0, len(s.parks))
	for k, p := range s.parks {
		if k.platform != platform || k.account != account {
			continue
		}
		out = append(out, core.NewModelPark(k.model, core.ModelParkRateLimit, p.reason, p.until, time.Time{}))
	}
	return core.SortModelParks(out)
}

// pruneLocked drops lapsed records so the map stays bounded by the number of
// live windows rather than the number of failures since boot.  Callers must
// hold s.mu.
func (s *Store) pruneLocked(now time.Time) {
	for k, p := range s.parks {
		if !now.Before(p.until) {
			delete(s.parks, k)
		}
	}
}

func newParkKey(platform, account, model string) (parkKey, bool) {
	k := parkKey{
		platform: strings.TrimSpace(platform),
		account:  strings.TrimSpace(account),
		model:    strings.TrimSpace(model),
	}
	if k.platform == "" || k.account == "" || k.model == "" {
		return parkKey{}, false
	}
	return k, true
}

func parkDuration(hits int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > MaxPark {
			return MaxPark
		}
		return retryAfter
	}
	shift := hits - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxShift {
		shift = maxShift
	}
	d := BasePark << shift
	if d <= 0 || d > MaxPark {
		return MaxPark
	}
	return d
}