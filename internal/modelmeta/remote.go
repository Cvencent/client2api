package modelmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ModelsDevURL is the public catalogue the reference panel consults. It is only
// ever requested when a caller explicitly opts in through RemoteConfig.Enabled.
const ModelsDevURL = "https://models.dev/api.json"

// Remote defaults, ported from the reference panel's modelsdev.go so the two
// implementations back off identically.
const (
	// DefaultRemoteTimeout bounds one fetch. A catalogue lookup must never hold a
	// request open.
	DefaultRemoteTimeout = 5 * time.Second
	// DefaultRemoteCooldown is the minimum gap between fetch attempts. It applies
	// to failures too — that is the whole point of a negative cooldown: an outage
	// must not turn into a request storm.
	DefaultRemoteCooldown = 5 * time.Minute
	// DefaultRemoteNegativeTTL is how long a model that models.dev does not know
	// about is remembered as unknown, so repeated lookups do not keep re-deciding.
	DefaultRemoteNegativeTTL = 24 * time.Hour

	remoteMaxBody      = 32 << 20
	remoteValueMax     = int64(1e9)
	remoteNegSoftCap   = 1024
	remoteNegHardCap   = 2048
	remoteVendorAccept = "application/json"
)

// modelsDevPreferredVendors lists the providers whose copy of a model we trust
// first when several vendors publish the same bare model id. Selection is
// deterministic: preferred vendor first, then lexicographically smallest provider
// name, so the same document always yields the same answer.
var modelsDevPreferredVendors = []string{
	"zai", "moonshotai", "moonshotai-cn", "openai", "google", "deepseek", "minimax",
}

// RemoteConfig configures the optional models.dev layer.
//
// The zero value is OFF: no client is built, no goroutine runs, no socket is
// opened, and Lookup behaves exactly as it does on a machine with no internet.
// That is deliberate — the module test suite and a user's laptop must never depend
// on outbound connectivity, so enabling this is always an explicit act.
type RemoteConfig struct {
	// Enabled is the opt-in. Everything else is ignored while it is false.
	Enabled bool
	// URL overrides ModelsDevURL; tests point it at an httptest server.
	URL string
	// Timeout bounds one fetch (default DefaultRemoteTimeout).
	Timeout time.Duration
	// Cooldown is the minimum gap between attempts (default
	// DefaultRemoteCooldown), shared by successes and failures.
	Cooldown time.Duration
	// NegativeTTL is how long a miss is remembered (default
	// DefaultRemoteNegativeTTL).
	NegativeTTL time.Duration
	// HTTPClient overrides the transport. When nil a private client with Timeout
	// is built; http.DefaultClient is never used, so this package cannot be given
	// a client it does not control by accident.
	HTTPClient *http.Client
	// OnRefreshed is called after a successful fetch with the number of indexed
	// models, so a panel can log or expose it.
	OnRefreshed func(models int)
}

// models.dev document shape: provider -> models -> id -> limit{context,output}.
type modelsDevDoc map[string]modelsDevProvider

type modelsDevProvider struct {
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	Limit *modelsDevLimit `json:"limit"`
}

type modelsDevLimit struct {
	Context int64 `json:"context"`
	Output  int64 `json:"output"`
}

// modelsDevEntry is one indexed bare model id.
type modelsDevEntry struct {
	Context   int64
	Output    int64
	Provider  string
	VendorKey string
}

// remoteCache owns the models.dev document and its fetch policy. A nil
// *remoteCache is the disabled state, and every method tolerates that receiver, so
// callers never branch on "is the remote layer on".
type remoteCache struct {
	url      string
	timeout  time.Duration
	cooldown time.Duration
	negTTL   time.Duration
	client   *http.Client
	onLoad   func(models int)

	mu          sync.Mutex
	loaded      bool
	index       map[string]modelsDevEntry
	inflight    bool
	lastAttempt time.Time
	done        chan struct{}
	negatives   map[string]time.Time
}

func newRemote(cfg *RemoteConfig) *remoteCache {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	rc := &remoteCache{
		url:       strings.TrimSpace(cfg.URL),
		timeout:   cfg.Timeout,
		cooldown:  cfg.Cooldown,
		negTTL:    cfg.NegativeTTL,
		onLoad:    cfg.OnRefreshed,
		negatives: map[string]time.Time{},
	}
	if rc.url == "" {
		rc.url = ModelsDevURL
	}
	if rc.timeout <= 0 {
		rc.timeout = DefaultRemoteTimeout
	}
	if rc.cooldown <= 0 {
		rc.cooldown = DefaultRemoteCooldown
	}
	if rc.negTTL <= 0 {
		rc.negTTL = DefaultRemoteNegativeTTL
	}
	if cfg.HTTPClient != nil {
		rc.client = cfg.HTTPClient
	} else {
		rc.client = &http.Client{Timeout: rc.timeout}
	}
	return rc
}

// enabled reports whether the layer is on. It is true only for a real remoteCache.
func (r *remoteCache) enabled() bool { return r != nil }

// lookup answers from the already-fetched document. It never triggers I/O: an
// unrefreshed document is simply a miss, which keeps every caller non-blocking.
func (r *remoteCache) lookup(model string) (Meta, bool) {
	if r == nil || model == "" {
		return Meta{}, false
	}
	key := bareModelID(model)
	r.mu.Lock()
	idx := r.index
	loaded := r.loaded
	var e modelsDevEntry
	var ok bool
	if loaded {
		e, ok = idx[key]
	}
	r.mu.Unlock()
	if !ok {
		return Meta{}, false
	}
	m := Meta{Source: SourceModelsDev, FieldSources: map[string]string{}}
	if e.Context > 0 {
		m.ContextLength = e.Context
		m.FieldSources[FieldContextLength] = SourceModelsDev
	}
	if e.Output > 0 {
		m.MaxOutputTokens = e.Output
		m.FieldSources[FieldMaxOutputTokens] = SourceModelsDev
	}
	if m.IsZero() {
		return Meta{}, false
	}
	return m, true
}

// alreadyNegated reports whether this model is inside its negative TTL. The
// timestamp is the FIRST miss, matching the reference: a later miss must not extend
// how long we distrust a model.
func (r *remoteCache) alreadyNegated(model string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.negatives[model]
	return ok && time.Since(at) < r.negTTL
}

// noteMiss records an unknown model without refreshing an existing entry.
func (r *remoteCache) noteMiss(model string) {
	if r == nil || model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.negatives == nil {
		r.negatives = map[string]time.Time{}
	}
	if _, exists := r.negatives[model]; exists {
		return
	}
	if len(r.negatives) >= remoteNegSoftCap {
		r.evictExpiredLocked()
	}
	if len(r.negatives) >= remoteNegHardCap {
		r.evictOldestHalfLocked()
	}
	r.negatives[model] = time.Now()
}

func (r *remoteCache) evictExpiredLocked() {
	for model, at := range r.negatives {
		if time.Since(at) >= r.negTTL {
			delete(r.negatives, model)
		}
	}
}

func (r *remoteCache) evictOldestHalfLocked() {
	type pair struct {
		model string
		at    time.Time
	}
	list := make([]pair, 0, len(r.negatives))
	for model, at := range r.negatives {
		list = append(list, pair{model: model, at: at})
	}
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].at.Before(list[j-1].at); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	for i := 0; i < len(list)/2; i++ {
		delete(r.negatives, list[i].model)
	}
}

// refreshAsync schedules a fetch if one is not already running and the cooldown has
// elapsed. It returns immediately: a request path must never wait on the network.
func (r *remoteCache) refreshAsync() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.inflight {
		r.mu.Unlock()
		return
	}
	if !r.lastAttempt.IsZero() && time.Since(r.lastAttempt) < r.cooldown {
		r.mu.Unlock()
		return
	}
	// The attempt is stamped before the request starts, so a failure also starts
	// the cooldown — the negative cooldown is what stops an outage from becoming a
	// retry storm.
	r.inflight = true
	r.lastAttempt = time.Now()
	ch := make(chan struct{})
	r.done = ch
	r.mu.Unlock()

	// GoSafe, not a bare "go": this fetch runs detached from every request, and
	// a panic in it would take the process down.  The defer is the other half:
	// a panic skips the normal bookkeeping, and leaving inflight set would block
	// every waiter on done and freeze the cache for the life of the process.
	core.GoSafe("modelmeta remote refresh", nil, func() {
		settled := false
		defer func() {
			if settled {
				return
			}
			r.mu.Lock()
			r.inflight = false
			close(ch)
			r.mu.Unlock()
		}()
		idx, err := r.fetch(context.Background())
		r.mu.Lock()
		if err == nil && len(idx) > 0 {
			r.index = idx
			r.loaded = true
		}
		r.inflight = false
		close(ch)
		r.mu.Unlock()
		settled = true
		if err == nil && len(idx) > 0 && r.onLoad != nil {
			r.onLoad(len(idx))
		}
	})
}

// fetch performs one HTTP request and indexes the document. Any failure leaves the
// previous state untouched.
func (r *remoteCache) fetch(ctx context.Context) (map[string]modelsDevEntry, error) {
	if r == nil || r.client == nil {
		return nil, fmt.Errorf("modelmeta: remote layer is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", remoteVendorAccept)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelmeta: %s returned %s", r.url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, remoteMaxBody))
	if err != nil {
		return nil, err
	}
	var doc modelsDevDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	return indexModelsDevDoc(doc), nil
}

// wait blocks until the in-flight attempt settles or the timeout expires, and
// reports whether a document is available. It exists for tests and for an explicit
// panel refresh; request paths never call it.
func (r *remoteCache) wait(timeout time.Duration) bool {
	if r == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for {
		r.mu.Lock()
		if r.loaded {
			r.mu.Unlock()
			return true
		}
		if !r.inflight {
			r.mu.Unlock()
			return false
		}
		ch := r.done
		r.mu.Unlock()
		remain := time.Until(deadline)
		if remain <= 0 {
			return false
		}
		timer := time.NewTimer(remain)
		select {
		case <-ch:
			timer.Stop()
		case <-timer.C:
			return false
		}
	}
}

// bareModelID reduces "vendor/model" to "model", which is how the reference keys
// its models.dev index.
func bareModelID(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		return model[i+1:]
	}
	return model
}

// indexModelsDevDoc flattens provider -> models into bare id -> entry, applying the
// reference's preference order and rejecting absurd values so a dirty upstream
// number cannot enter the resolution chain.
func indexModelsDevDoc(doc modelsDevDoc) map[string]modelsDevEntry {
	idx := make(map[string]modelsDevEntry)
	for provider, p := range doc {
		for rawID, model := range p.Models {
			if model.Limit == nil {
				continue
			}
			contextLen := cleanRemoteValue(model.Limit.Context)
			outputLen := cleanRemoteValue(model.Limit.Output)
			if contextLen == 0 && outputLen == 0 {
				continue
			}
			key := bareModelID(rawID)
			if key == "" {
				continue
			}
			candidate := modelsDevEntry{
				Context:   contextLen,
				Output:    outputLen,
				Provider:  provider,
				VendorKey: rawID,
			}
			existing, ok := idx[key]
			if !ok || preferCandidate(candidate, existing) {
				idx[key] = candidate
			}
		}
	}
	return idx
}

// preferCandidate picks the winner deterministically: a preferred vendor beats a
// non-preferred one, then the lexicographically smaller provider name wins, then
// the smaller raw id. No map iteration order can leak into the result.
func preferCandidate(candidate, existing modelsDevEntry) bool {
	cRank, cPref := vendorPreference(candidate.Provider)
	eRank, ePref := vendorPreference(existing.Provider)
	if cPref != ePref {
		return cPref
	}
	if cPref && ePref && cRank != eRank {
		return cRank < eRank
	}
	if candidate.Provider != existing.Provider {
		return candidate.Provider < existing.Provider
	}
	return candidate.VendorKey < existing.VendorKey
}

func vendorPreference(provider string) (int, bool) {
	for i, vendor := range modelsDevPreferredVendors {
		if strings.EqualFold(provider, vendor) {
			return i, true
		}
	}
	return 0, false
}

// cleanRemoteValue rejects zero, negative and implausibly large limits.
func cleanRemoteValue(v int64) int64 {
	if v <= 0 || v > remoteValueMax {
		return 0
	}
	return v
}
