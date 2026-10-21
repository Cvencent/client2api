package gateway

import (
	"errors"
	"os"
	"strings"
	"sync"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Per-model estimator calibration.
//
// token_estimate.go is a heuristic: it counts CJK as one token per character
// and divides everything else by a fixed characters-per-token figure.  Every
// vendor tokenizes differently, so the heuristic is closer to some platforms
// than to others -- and the production ledger shows the error does not even
// point the same way:
//
//	autoclaw    vendor 861713, estimate 931735  -> the heuristic OVER-counts ~8%
//	workbuddy   vendor 1477004, estimate 628884 -> the heuristic UNDER-counts ~2.3x
//
// A single global constant therefore cannot be right: tuning it for autoclaw
// makes workbuddy worse by the same amount.  What the gateway does have is the
// vendor's own count on every successful call -- prompt_tokens -- which is
// ground truth for exactly one thing: how many vendor tokens the bytes it just
// measured were worth.  Dividing that by this gateway's estimate for the same
// bytes gives a per-model factor.
//
// The factor is applied to the budget the guard trims against, never to the
// estimate itself.  The estimate is also what the usage row and the
// /v1/messages/count_tokens endpoint report, and those should stay a statement
// about the request, not about the model that happens to be serving it.
//
// This matters because promptBudgetSafety already holds back 8%.  On autoclaw
// the heuristic over-reads by almost exactly that 8%, so the safety margin was
// entirely consumed by estimator error: requests were trimmed at ~82% of the
// real window while the vendor would have accepted them whole.
// ---------------------------------------------------------------------------

const (
	// estimateCalibrationFloor and Ceiling bound the factor a real tokenizer
	// could plausibly be off by.  A ratio outside this range is not a tokenizer
	// difference -- it is a different request (a module that rewrites the body),
	// a partial response, or a garbage usage block -- and acting on it would
	// size the guard from a fiction.
	estimateCalibrationFloor   = 0.5
	estimateCalibrationCeiling = 4.0
	// estimateCalibrationAlpha is the weight a new measurement carries.  One
	// observation is noisy (cache boundaries, a truncated response, a module
	// that adds a turn), so the stored factor moves towards each new value
	// instead of jumping to it.
	estimateCalibrationAlpha = 0.5
	// estimateCalibrationVersion is the on-disk format.
	estimateCalibrationVersion = 1
)

type estimateCalibrationKey struct {
	client string
	model  string
}

type estimateCalibrationDoc struct {
	Version int                           `json:"version"`
	Factors map[string]map[string]float64 `json:"factors"`
}

// estimateCalibrationStore holds one factor per (client, model).
//
// A nil or zero store is usable and always reports the identity factor, which
// is the correct behaviour when no data directory is configured: the guard
// falls back to exactly the heuristics it used before.
type estimateCalibrationStore struct {
	path  string
	warnf func(string, ...any)

	mu      sync.RWMutex
	factors map[estimateCalibrationKey]float64
}

// newEstimateCalibrationStore loads the store from path.  Every failure mode --
// missing, unreadable, corrupt -- collapses to an empty store: this is a cache
// of measurements and must never be able to break a request.
func newEstimateCalibrationStore(path string, warnf func(string, ...any)) *estimateCalibrationStore {
	s := &estimateCalibrationStore{
		path:    strings.TrimSpace(path),
		warnf:   warnf,
		factors: map[estimateCalibrationKey]float64{},
	}
	if s.path == "" {
		return s
	}
	var doc estimateCalibrationDoc
	if err := core.ReadJSON(s.path, &doc); err != nil {
		if warnf != nil && !errors.Is(err, os.ErrNotExist) {
			warnf("estimate: ignoring unreadable calibration %s: %v", s.path, err)
		}
		return s
	}
	if doc.Version != 0 && doc.Version != estimateCalibrationVersion {
		if warnf != nil {
			warnf("estimate: ignoring calibration %s with version %d", s.path, doc.Version)
		}
		return s
	}
	for client, models := range doc.Factors {
		for model, f := range models {
			if f >= estimateCalibrationFloor && f <= estimateCalibrationCeiling {
				s.factors[estimateCalibrationKey{client: client, model: model}] = f
			}
		}
	}
	return s
}

// factor returns the multiplier to apply to an estimate-space budget, or 1 when
// this model has never been measured.
func (s *estimateCalibrationStore) factor(client, model string) float64 {
	if s == nil {
		return 1
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	f, ok := s.factors[estimateCalibrationKey{client: client, model: model}]
	if !ok || f <= 0 {
		return 1
	}
	return f
}

// observe folds one vendor measurement into the stored factor and persists it.
//
// estimated is this gateway's own estimate of the exact request the vendor
// just measured, so the ratio is a statement about the two tokenizers and not
// about the conversation.  Anything that is not a plausible ratio is ignored
// rather than stored.
func (s *estimateCalibrationStore) observe(client, model string, vendorTokens, estimatedTokens int64) {
	if s == nil || client == "" || model == "" {
		return
	}
	if vendorTokens <= 0 || estimatedTokens <= 0 {
		return
	}
	raw := float64(vendorTokens) / float64(estimatedTokens)
	if raw < estimateCalibrationFloor || raw > estimateCalibrationCeiling {
		return
	}
	s.mu.Lock()
	key := estimateCalibrationKey{client: client, model: model}
	next := raw
	if prev, ok := s.factors[key]; ok && prev > 0 {
		next = prev*(1-estimateCalibrationAlpha) + raw*estimateCalibrationAlpha
	}
	if next < estimateCalibrationFloor {
		next = estimateCalibrationFloor
	}
	if next > estimateCalibrationCeiling {
		next = estimateCalibrationCeiling
	}
	changed := true
	if prev, ok := s.factors[key]; ok && prev == next {
		changed = false
	}
	s.factors[key] = next
	err := s.saveLocked()
	s.mu.Unlock()
	if changed && err != nil && s.warnf != nil {
		s.warnf("estimate: cannot persist the calibration for %s/%s: %v", client, model, err)
	}
}

// saveLocked writes the document atomically.  Callers hold s.mu.
func (s *estimateCalibrationStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := estimateCalibrationDoc{Version: estimateCalibrationVersion, Factors: map[string]map[string]float64{}}
	for key, f := range s.factors {
		models := doc.Factors[key.client]
		if models == nil {
			models = map[string]float64{}
			doc.Factors[key.client] = models
		}
		models[key.model] = f
	}
	return core.WriteJSONAtomic(s.path, doc)
}
