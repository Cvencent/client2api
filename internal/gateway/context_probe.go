package gateway

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Sticky context probes.
//
// The context guard sizes a request with a heuristic and, until now, trusted
// that heuristic enough to drop the caller's oldest turns before the vendor
// ever saw them.  When the heuristic runs high -- and on a platform like
// raccoon it does -- a request the vendor would have served whole gets
// shortened anyway, and the caller is told nothing.
//
// A probe inverts the order of authority.  The first time a request is judged
// too big for a (platform, model) this gateway has never tested, the request is
// forwarded unchanged and the vendor decides.  The outcome is recorded:
//
//   - accepted -- the estimate the gateway doubted was demonstrably servable.
//     That number becomes a floor for this model, so the same size is never
//     trimmed again.
//   - refused -- the vendor has spoken, and the existing learn-the-window +
//     trim + retry path handles it from here on.
//
// A model's window does not change between requests, so one probe settles the
// question for that (platform, model) until an operator clears it.
const (
	probeOutcomeAccepted = "accepted"
	probeOutcomeRefused  = "refused"
	// probeOutcomeCleared is a tombstone: the operator asked to forget this
	// model's verdict.  It is stored rather than deleted because a model whose
	// context length is known otherwise counts as settled by default, and a
	// deletion would immediately flip it back to "settled".
	probeOutcomeCleared = "cleared"
)

// probeRecord is one model's probe verdict.
//
// EstimateTokens is in the gateway's own estimate space, not the vendor's
// token count, because the guard trims against estimates.  Recording a vendor
// figure would force a conversion through the rolling calibration factor --
// the very lag that caused the over-eager trim this feature exists to fix.
type probeRecord struct {
	Probed         bool      `json:"probed"`
	Outcome        string    `json:"outcome"`
	EstimateTokens int64     `json:"estimate_tokens,omitempty"`
	VendorTokens   int64     `json:"vendor_tokens,omitempty"`
	At             time.Time `json:"at"`
}

type probeKey struct {
	client string
	model  string
}

// probeDoc is the on-disk shape.
type probeDoc struct {
	Version int                               `json:"version"`
	Probes  map[string]map[string]probeRecord `json:"probes"`
}

// probeDocVersion is the current on-disk format.
const probeDocVersion = 1

// probeStore remembers which (platform, model) pairs have been probed and what
// the vendor said.
type probeStore struct {
	mu      sync.RWMutex
	path    string
	warnf   func(string, ...any)
	entries map[probeKey]probeRecord
}

// openProbeStore loads the store from path.  Every failure mode collapses to an
// empty store: this is a cache of measurements and must never be able to break
// a request.
func openProbeStore(path string, warnf func(string, ...any)) *probeStore {
	s := &probeStore{
		path:    strings.TrimSpace(path),
		warnf:   warnf,
		entries: map[probeKey]probeRecord{},
	}
	if s.path == "" {
		return s
	}
	var doc probeDoc
	if err := core.ReadJSON(s.path, &doc); err != nil {
		if s.warnf != nil && !errors.Is(err, os.ErrNotExist) {
			s.warnf("context: ignoring unreadable probe records %s: %v", s.path, err)
		}
		return s
	}
	if doc.Version != 0 && doc.Version != probeDocVersion {
		if s.warnf != nil {
			s.warnf("context: ignoring probe records %s with version %d", s.path, doc.Version)
		}
		return s
	}
	for client, models := range doc.Probes {
		for model, rec := range models {
			if rec.Probed {
				s.entries[probeKey{client: client, model: model}] = rec
			}
		}
	}
	return s
}

// get returns a model's probe record.
func (s *probeStore) get(client, model string) (probeRecord, bool) {
	if s == nil {
		return probeRecord{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.entries[probeKey{client: client, model: model}]
	return rec, ok
}

// recordAccepted notes that the vendor served a request of this estimated size.
// It raises the stored floor and never lowers it.
func (s *probeStore) recordAccepted(client, model string, estimate, vendor int64) bool {
	if s == nil || client == "" || model == "" || estimate <= 0 {
		return false
	}
	s.mu.Lock()
	key := probeKey{client: client, model: model}
	prev, existed := s.entries[key]
	next := probeRecord{
		Probed:         true,
		Outcome:        probeOutcomeAccepted,
		EstimateTokens: estimate,
		VendorTokens:   vendor,
		At:             time.Now(),
	}
	if existed && prev.EstimateTokens > next.EstimateTokens {
		next.EstimateTokens = prev.EstimateTokens
	}
	if existed && prev.Outcome == next.Outcome && prev.EstimateTokens == next.EstimateTokens &&
		prev.VendorTokens == next.VendorTokens {
		s.mu.Unlock()
		return false
	}
	s.entries[key] = next
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil && s.warnf != nil {
		s.warnf("context: cannot persist the probe record for %s/%s: %v", client, model, err)
	}
	return true
}

// recordRefused notes that the vendor rejected a probe as oversized.
func (s *probeStore) recordRefused(client, model string, estimate int64) bool {
	if s == nil || client == "" || model == "" {
		return false
	}
	s.mu.Lock()
	key := probeKey{client: client, model: model}
	prev, existed := s.entries[key]
	if existed && prev.Outcome == probeOutcomeRefused {
		s.mu.Unlock()
		return false
	}
	s.entries[key] = probeRecord{
		Probed:         true,
		Outcome:        probeOutcomeRefused,
		EstimateTokens: 0,
		At:             time.Now(),
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil && s.warnf != nil {
		s.warnf("context: cannot persist the probe record for %s/%s: %v", client, model, err)
	}
	return true
}

// clear forgets one model's probe so the next oversized request probes again.
func (s *probeStore) clear(client, model string) bool {
	if s == nil || client == "" || model == "" {
		return false
	}
	s.mu.Lock()
	key := probeKey{client: client, model: model}
	if prev, ok := s.entries[key]; ok && prev.Outcome == probeOutcomeCleared {
		s.mu.Unlock()
		return false
	}
	s.entries[key] = probeRecord{Probed: false, Outcome: probeOutcomeCleared, At: time.Now()}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil && s.warnf != nil {
		s.warnf("context: cannot persist the cleared probe for %s/%s: %v", client, model, err)
	}
	return true
}

// ProbeRecord is the exported view of one model's probe verdict, so the panel
// can render it without the gateway leaking its internal key type.
type ProbeRecord = probeRecord

// Snapshot exposes every probe verdict for the panel.
func (s *probeStore) Snapshot() map[string]map[string]ProbeRecord {
	return s.snapshot()
}

// Clear forgets one model's probe so the next oversized request probes again.
func (s *probeStore) Clear(client, model string) bool {
	return s.clear(client, model)
}

// OpenProbeStore loads the persisted probe verdicts from path.
//
// It is exported because the process that owns the data directory builds the
// store, while the gateway consumes it: the wiring in cmd/client2api cannot
// reach an unexported constructor.  A nil store and an empty path are both
// valid and mean "nobody is probing", so the guard keeps its old behaviour.
func OpenProbeStore(path string, warnf func(string, ...any)) *probeStore {
	return openProbeStore(path, warnf)
}

// snapshot returns a copy of every probe record, for the panel.
func (s *probeStore) snapshot() map[string]map[string]probeRecord {
	if s == nil {
		return map[string]map[string]probeRecord{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *probeStore) snapshotLocked() map[string]map[string]probeRecord {
	out := map[string]map[string]probeRecord{}
	for key, rec := range s.entries {
		models := out[key.client]
		if models == nil {
			models = map[string]probeRecord{}
			out[key.client] = models
		}
		models[key.model] = rec
	}
	return out
}

func (s *probeStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := probeDoc{Version: probeDocVersion, Probes: s.snapshotLocked()}
	return core.WriteJSONAtomic(s.path, doc)
}
