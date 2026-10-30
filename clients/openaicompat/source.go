package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// source is one operator-defined platform. Each immutable transport snapshot
// reuses the compat body/SSE implementation without sharing mutable config.
type source struct {
	name    string
	deps    core.Deps
	mu      sync.Mutex
	cfg     core.SourceConfig
	now     func() time.Time
	rows    []storedProvider
	pool    *providerPool
	cooling map[string]time.Time
	// invalid holds the vendor's own verdict about one key (it answered 401).
	// It is separate from Disabled, which is the operator's switch, and it is
	// runtime-only so a reload never resurrects a rejected credential.
	invalid map[string]bool
	// balanceProbe is the negative cache for the "auto" balance protocol: a
	// relay that answers none of the four billing probes is remembered here so
	// the next sweep skips it instead of repeating four failures.
	balanceProbe map[string]balanceProbeState
	// authExhausted is the wrapper's veto on that verdict: an auth-shaped
	// refusal that is really "this credit package is drained" (the OfficeAce
	// gateway answers a zero-credit account with a bare 401) is a cooldown,
	// because a refill revives it.  Nil on plain relays.
	authExhausted func(id string) bool
	lastUsed      map[string]uint64
	sequence      uint64
	limits        *sourceRequestLimiter
	lastErr       string
	active        bool
	generation    uint64
	affinity      *core.Affinity
	scanFilter    sourceModelFilter
	wireModel     func(string) string

	// httpMu guards the per-source HTTP client below.  It is a separate lock
	// from mu so a request path can fetch its client without touching the
	// pool/config state.
	httpMu sync.RWMutex
	// httpc is what every request this source makes goes through: the shared
	// gateway client when no per-source proxy is configured, otherwise a clone
	// of it pointed at the operator's proxy.  proxy records the value httpc
	// was built for, so a reload that does not touch it keeps the connections.
	httpc *http.Client
	proxy string
}

type sourceModelFilter func(context.Context, storedProvider, []modelCatalogueEntry) ([]modelCatalogueEntry, error)

func newSource(name string, cfg core.SourceConfig, deps core.Deps) (*source, error) {
	limits, err := newSourceRequestLimiter(filepath.Join(deps.DataDir, "request_limits.json"), cfg.EffectiveRequestLimits())
	if err != nil {
		return nil, err
	}
	s := &source{
		name:     name,
		deps:     deps,
		now:      time.Now,
		pool:     newProviderPool(),
		cooling:  map[string]time.Time{},
		invalid:  map[string]bool{},
		lastUsed: map[string]uint64{},
		limits:   limits,
		affinity: core.NewAffinity(0),
	}
	s.affinity.StartGC()
	if err := s.ConfigureSource(cfg); err != nil {
		return nil, err
	}
	return s, nil
}

// depsWithClient copies the module deps with this source's HTTP client in
// place.  Everything built from it -- chat, model scan, balance, account test
// -- therefore leaves through the source's proxy.
func (s *source) depsWithClient() core.Deps {
	d := s.deps
	d.HTTPClient = s.httpClient()
	return d
}

// httpClient returns the client every request for this source must use.
func (s *source) httpClient() *http.Client {
	s.httpMu.RLock()
	c := s.httpc
	s.httpMu.RUnlock()
	if c != nil {
		return c
	}
	if s.deps.HTTPClient != nil {
		return s.deps.HTTPClient
	}
	return defaultHTTPClient
}

// setProxy installs the client for the configured proxy.  An empty proxy keeps
// the gateway's shared client, which already honours the global proxy and the
// process environment; a non-empty one clones that transport and points it at
// the operator's proxy.  Validation has already rejected a malformed URL, so an
// unparsable hand-built value falls back to the shared client rather than
// breaking the platform.
func (s *source) setProxy(raw string) {
	want := strings.TrimSpace(raw)
	s.httpMu.Lock()
	defer s.httpMu.Unlock()
	if s.httpc != nil && s.proxy == want {
		return
	}
	base := s.deps.HTTPClient
	if base == nil {
		base = defaultHTTPClient
	}
	old := s.httpc
	if want == "" {
		s.httpc, s.proxy = base, ""
	} else if u, err := url.Parse(want); err == nil && u.Hostname() != "" {
		tr := cloneTransport(base.Transport)
		tr.Proxy = http.ProxyURL(u)
		s.httpc = &http.Client{Transport: tr, Timeout: base.Timeout, CheckRedirect: base.CheckRedirect, Jar: base.Jar}
		s.proxy = want
	} else {
		s.httpc, s.proxy = base, ""
	}
	if old != nil && old != s.httpc && old != base {
		closeIdleConnections(old)
	}
}

// cloneTransport copies the shared transport so the per-source proxy cannot
// alter the gateway client's settings.  A non-*http.Transport round tripper (a
// test stub, for instance) cannot be cloned, so the stock transport is used.
func cloneTransport(rt http.RoundTripper) *http.Transport {
	if t, ok := rt.(*http.Transport); ok {
		return t.Clone()
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{Proxy: nil, ForceAttemptHTTP2: true}
	}
	return base.Clone()
}

func closeIdleConnections(c *http.Client) {
	if c == nil {
		return
	}
	if tr, ok := c.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (s *source) Name() string   { return s.name }
func (s *source) store() *Client { return &Client{deps: s.deps} }
func (s *source) ConfigureSource(cfg core.SourceConfig) error {
	if err := core.ValidateSources(map[string]core.SourceConfig{s.name: cfg}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var store struct {
		Version   int              `json:"version"`
		Providers []storedProvider `json:"providers"`
	}
	err := core.ReadJSON(s.store().storePath(), &store)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading source accounts: %w", err)
	}
	if err == nil && store.Version != storeVersion {
		return fmt.Errorf("unsupported source account store version %d", store.Version)
	}
	rows := store.Providers
	if s.limits == nil {
		limits, err := newSourceRequestLimiter(filepath.Join(s.deps.DataDir, "request_limits.json"), cfg.EffectiveRequestLimits())
		if err != nil {
			return err
		}
		s.limits = limits
	} else if err := s.limits.Configure(cfg.EffectiveRequestLimits()); err != nil {
		return err
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	changed := false
	for i := range rows {
		rowBase := strings.TrimRight(rows[i].BaseURL, "/")
		// OfficeAce mints each account against the model gateway recorded in
		// that account's Huawei login.  Those per-account roots are data, not
		// a stale copy of the platform default, so a config reload must not
		// flatten them back to cfg.BaseURL.
		if officeAceEndpoint(s.name, cfg.BaseURL) && rowBase != "" {
			continue
		}
		if rowBase != base {
			rows[i].Models = nil
			rows[i].BaseURL = base
			changed = true
		}
	}
	if changed {
		if err := s.store().saveProviders(rows); err != nil {
			return err
		}
	}
	if !s.active || !reflect.DeepEqual(s.cfg, cfg) || !reflect.DeepEqual(s.rows, rows) {
		s.generation++
	}
	s.active = !cfg.Disabled
	s.cfg = cfg
	s.rows = rows
	ids := make(map[string]struct{}, len(rows))
	for _, p := range rows {
		ids[p.ID] = struct{}{}
	}
	s.limits.PruneAccounts(ids)
	s.publishLocked()
	s.setProxy(cfg.Proxy)
	return nil
}

func (s *source) DeactivateSource() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
	s.generation++
}

func (s *source) publishLocked() {
	records := make([]providerRecord, 0, len(s.rows))
	for _, p := range s.rows {
		records = append(records, providerRecord{ID: p.ID, Label: firstNonEmpty(p.Label, p.ID), APIKey: p.APIKey, AppSecret: p.AppSecret, BaseURL: s.accountBaseLocked(p), Models: p.Models, Disabled: p.Disabled, Source: sourcePanel})
	}
	s.pool.reload(records)
}
func (s *source) saveLocked(rows []storedProvider) error {
	if err := s.store().saveProviders(rows); err != nil {
		return err
	}
	s.rows = rows
	s.publishLocked()
	return nil
}
func (s *source) transportLocked() *Client {
	field := s.cfg.MaxTokensField
	if field == "" {
		field = "max_tokens"
	}
	providers := make([]ProviderConfig, 0, len(s.rows))
	for _, p := range s.rows {
		providers = append(providers, s.providerLocked(p))
	}
	return &Client{instanceName: s.name, deps: s.depsWithClient(), cfg: Config{MaxTokensField: field, APIFormat: normalizeAPIFormat(s.cfg.APIFormat), Providers: providers}, pool: s.pool, now: time.Now, wireModel: s.wireModel}
}
func (s *source) providerLocked(p storedProvider) ProviderConfig {
	ua := p.UserAgent
	if ua == "" {
		// A source-level UA is the operator default for every account on the
		// platform; an account-level value wins.
		ua = s.cfg.UserAgent
	}
	return ProviderConfig{ID: p.ID, APIKey: p.APIKey, AppSecret: p.AppSecret, BaseURL: s.accountBaseLocked(p), Models: append([]string(nil), p.Models...), ProbeModels: append([]string(nil), p.ProbeModels...), ExtraHeaders: p.Headers, UserAgent: ua, Label: p.Label, Disabled: p.Disabled}
}

// accountBaseLocked is the effective API root for one stored account.  An
// ordinary source still follows its platform-level URL; OfficeAce accounts
// keep the gateway root their own login returned.
func (s *source) accountBaseLocked(p storedProvider) string {
	if officeAceEndpoint(s.name, s.cfg.BaseURL) && strings.TrimSpace(p.BaseURL) != "" {
		return strings.TrimRight(p.BaseURL, "/")
	}
	return strings.TrimRight(s.cfg.BaseURL, "/")
}
func (s *source) Models(context.Context) ([]core.Model, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []core.Model{}
	seen := map[string]bool{}
	for _, p := range s.rows {
		if p.Disabled {
			continue
		}
		for _, id := range p.Models {
			if !seen[id] {
				seen[id] = true
				out = append(out, core.Model{ID: id, OwnedBy: s.name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *source) Status(ctx context.Context) core.Status {
	models, _ := s.Models(ctx)
	now := s.now()
	st := core.Status{Name: s.name, UpdatedAt: now, Accounts: []core.AccountStatus{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Label = firstNonEmpty(s.cfg.Label, s.name)
	st.Detail = s.lastErr
	sourceLimit := s.sourceLimitBlocked(now)
	if sourceLimit != nil {
		detail := "请求限额已满；恢复时间 " + sourceLimit.Until.UTC().Format(time.RFC3339)
		if st.Detail != "" {
			st.Detail += "; "
		}
		st.Detail += detail
	}
	for _, m := range models {
		st.Models = append(st.Models, m.ID)
	}
	for _, a := range s.pool.statuses(now) {
		var supported []string
		for _, p := range s.rows {
			if p.ID == a.ID {
				supported = p.Models
				break
			}
		}
		allowed := map[string]bool{}
		for _, m := range supported {
			allowed[m] = true
		}
		limitBlock := s.accountLimitBlocked(a.ID, now)
		parks := []map[string]any{}
		for _, m := range models {
			if !allowed[m.ID] {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "unsupported"})
			} else if until := s.cooling[a.ID]; now.Before(until) {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "rate_limit", "until": until.UTC().Format(time.RFC3339)})
			} else if limitBlock != nil {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "request_limit", "until": limitBlock.Until.UTC().Format(time.RFC3339)})
			}
		}
		a.Extra["model_cooldowns"] = parks
		switch {
		case !a.Enabled:
			a.State = "invalid"
		case s.invalid[a.ID]:
			// The vendor rejected this key; waiting does not bring it back.
			a.State = "invalid"
		case now.Before(s.cooling[a.ID]):
			a.State = "cooling"
			if limitBlock != nil {
				a.Note = "request limit reached"
				a.ExpiresAt = limitBlock.Until.UTC().Format(time.RFC3339)
			}
		case limitBlock != nil:
			a.State = "cooling"
			a.Note = "request limit reached"
			a.ExpiresAt = limitBlock.Until.UTC().Format(time.RFC3339)
		default:
			st.Ready = true
		}
		st.Accounts = append(st.Accounts, a)
	}
	if sourceLimit != nil {
		st.Ready = false
	}
	return st
}
func (s *source) Health() core.Health {
	st := s.Status(context.Background())
	h := core.Health{Total: len(st.Accounts), Servable: st.Ready}
	for _, a := range st.Accounts {
		if !a.Enabled {
			h.Disabled++
		} else if a.State == "cooling" {
			h.Cooling++
		} else {
			h.Ready++
		}
	}
	return h
}
func (s *source) PoolStats() core.PoolStats {
	n, _ := s.pool.stats(0)
	return core.PoolStats{InFlight: n, StickySessions: s.affinity.Count()}
}

func (s *source) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil || len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: chat messages are required", core.ErrUnsupported)
	}
	tried := map[string]bool{}
	var last error
	cooling := false
	var limitBlock *requestLimitBlock
	conversationKey := core.ConversationKeyOf(req)
	bound, _ := s.affinity.Resolve(conversationKey, s.usableFor(req.Model))
	// Account-slot contention does not spend a network retry; inspect all keys.
	attempts := 0
	for {
		s.mu.Lock()
		now := s.now()
		if !s.active {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: source %s is disabled or removed", core.ErrNotConfigured, s.name)
		}
		if block := s.sourceLimitBlocked(now); block != nil {
			limitBlock = block
			s.mu.Unlock()
			break
		}
		candidates := []storedProvider{}
		for _, p := range s.rows {
			if p.Disabled || tried[p.ID] || s.invalid[p.ID] {
				continue
			}
			for _, m := range p.Models {
				if m == req.Model {
					if now.Before(s.cooling[p.ID]) {
						cooling = true
						break
					}
					if block := s.accountLimitBlocked(p.ID, now); block != nil {
						if laterBlock(limitBlock, block) {
							limitBlock = block
						}
						break
					}
					candidates = append(candidates, p)
					break
				}
			}
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := core.AccountPriority(s.name, candidates[i].ID), core.AccountPriority(s.name, candidates[j].ID)
			if a != b {
				return a < b
			}
			return s.lastUsed[candidates[i].ID] < s.lastUsed[candidates[j].ID]
		})
		if len(candidates) == 0 {
			s.mu.Unlock()
			break
		}
		p := candidates[0]
		if bound != "" && !tried[bound] {
			for _, candidate := range candidates {
				if candidate.ID == bound {
					p = candidate
					break
				}
			}
		}
		tried[p.ID] = true
		s.sequence++
		s.lastUsed[p.ID] = s.sequence
		if conversationKey != "" {
			s.affinity.Bind(conversationKey, p.ID)
		}
		transport, prov := s.transportLocked(), s.providerLocked(p)
		s.mu.Unlock()

		body, err := buildChatBody(transport.cfg, prov, transport.wireModelID(req.Model), req)
		if err != nil {
			return nil, err
		}
		if err := req.AcquireAccountSlot(p.ID); err != nil {
			last = err
			continue
		}
		block, err := s.acquireRequestLimit(p.ID, s.now())
		if err != nil {
			req.ReleaseAccountSlot()
			return nil, err
		}
		if block != nil {
			req.ReleaseAccountSlot()
			last = core.ErrBusy
			if laterBlock(limitBlock, block) {
				limitBlock = block
			}
			if block.Scope == "source" {
				break
			}
			continue
		}
		core.NoteServedBy(req, p.ID)
		stream, err := transport.doChat(ctx, prov, p.ID, body)
		attempts++
		if err == nil {
			// A served request is direct evidence that the key works, so it
			// retires both the rate-limit park and the rejected-key verdict.
			s.mu.Lock()
			delete(s.cooling, p.ID)
			delete(s.invalid, p.ID)
			s.mu.Unlock()
			return &sourceStream{Stream: stream, source: s, id: p.ID, key: p.APIKey, base: prov.BaseURL}, nil
		}
		req.ReleaseAccountSlot()
		last = err
		s.noteFailure(p.ID, p.APIKey, prov.BaseURL, err)
		if attempts >= maxRotate || !retryableChatError(err) {
			break
		}
	}
	if last != nil {
		return nil, last
	}
	if limitBlock != nil {
		return nil, fmt.Errorf("%w: %s request limit reached; retry after %s", core.ErrBusy, s.name, limitBlock.Until.Format(time.RFC3339))
	}
	if cooling {
		// This is a cooldown, not the in-flight ceiling.  It still has to be an
		// ErrBusy: the gateway treats that sentinel as "skip this platform
		// without spending a retry", which is exactly right, and answering 429
		// rather than a hard failure is what lets the caller back off.  But
		// ErrBusy's own text says "client is at its in-flight limit", and that
		// sentence sent operators hunting for contention on a platform whose
		// only problem was a parked key.  coolingError keeps the sentinel and
		// replaces the wording.
		return nil, &coolingError{
			source: s.name,
			model:  req.Model,
			until:  s.coolingUntilFor(req.Model, s.now(), s.cooling),
		}
	}
	return nil, fmt.Errorf("%w: %s has no ready API key for model %q; scan models first", core.ErrNotConfigured, s.name, req.Model)
}

// coolingError says a platform has a key for the model but parked it, and when
// it becomes selectable again.
//
// It is an ErrBusy: the gateway's rule for that sentinel -- skip the platform,
// answer backpressure, do not spend a retry -- is the correct behaviour here.
// Only the wording differs, because "client is at its in-flight limit" describes
// contention and this is a cooldown.  The two states need different operator
// actions (raise a ceiling vs. wait for a park to lapse), so they must not read
// the same in a log.
type coolingError struct {
	source string
	model  string
	until  string
}

func (e *coolingError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s has a key for model %q but it is cooling down until %s",
		e.source, e.model, e.until)
}

// Is makes errors.Is(err, core.ErrBusy) true, which is what the gateway and the
// module's own callers match on.
func (e *coolingError) Is(target error) bool {
	return target == core.ErrBusy
}

func (s *source) accountLimitBlocked(id string, now time.Time) *requestLimitBlock {
	if s.limits == nil {
		return nil
	}
	return s.limits.AccountBlocked(id, now)
}

// coolingUntilFor reports when the earliest-parked key able to serve model
// becomes selectable again, for the operator-facing reason string.
//
// The caller has already established that every candidate was cooling; this
// answers "for how long", which is the only part of the sentence an operator
// can act on.  An unreadable state (nothing cooling, a zero timestamp) falls
// back to a wording that makes no promise about the time.
func (s *source) coolingUntilFor(model string, now time.Time, cooling map[string]time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var soonest time.Time
	for _, p := range s.rows {
		if p.Disabled || s.invalid[p.ID] {
			continue
		}
		serves := false
		for _, m := range p.Models {
			if m == model {
				serves = true
				break
			}
		}
		if !serves {
			continue
		}
		until, ok := cooling[p.ID]
		if !ok || !now.Before(until) {
			continue
		}
		if soonest.IsZero() || until.Before(soonest) {
			soonest = until
		}
	}
	if soonest.IsZero() {
		return "the cooldown lapses"
	}
	return soonest.Format(time.RFC3339)
}

func (s *source) sourceLimitBlocked(now time.Time) *requestLimitBlock {
	if s.limits == nil {
		return nil
	}
	return s.limits.SourceBlocked(now)
}

func (s *source) acquireRequestLimit(id string, now time.Time) (*requestLimitBlock, error) {
	if s.limits == nil {
		return nil, nil
	}
	return s.limits.Acquire(id, now)
}

func (s *source) noteFailure(id, key, base string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.rows {
		if p.ID == id && p.APIKey == key && s.accountBaseLocked(p) == strings.TrimRight(base, "/") {
			s.lastErr = describeError(err)
			kind := core.FailureKindOf(err)
			if kind == core.FailureAuth && s.authExhausted != nil && s.authExhausted(id) {
				// The wrapper knows this account is out of credits: the
				// auth-shaped refusal is the drained-package verdict, not a
				// dead key.  Park it on the quota path -- a refill (daily
				// check-in, a new trial, a top-up) revives the row without
				// operator action.
				kind = core.FailureQuota
			}
			switch kind {
			case core.FailureRateLimited:
				s.parkLocked(id, core.FailureRateLimited)
			case core.FailureAuth, core.FailureQuota:
				s.parkLocked(id, kind)
			case core.FailureUpstream:
				s.parkLocked(id, core.FailureUpstream)
			}
			// A rejected key is terminal for a static API key: there is
			// nothing to refresh, so cooling it down only re-selects it every
			// cooldown.  Marking it invalid is what turns the row into
			// 账号异常 and gives the operator the re-import path.  The
			// authExhausted veto above covers the one auth-shaped refusal a
			// cooldown does answer.
			if kind == core.FailureAuth {
				s.invalid[id] = true
			}
			break
		}
	}
}

func (s *source) parkLocked(id string, kind core.FailureKind) {
	cooldown := s.cfg.CooldownFor(kind, sourceDefaultCooldown(kind))
	if cooldown > 0 {
		s.cooling[id] = s.now().Add(cooldown)
		return
	}
	delete(s.cooling, id)
}

func sourceDefaultCooldown(kind core.FailureKind) time.Duration {
	switch kind {
	case core.FailureRateLimited:
		return 30 * time.Second
	case core.FailureAuth, core.FailureQuota:
		return 5 * time.Minute
	case core.FailureUpstream:
		return 10 * time.Second
	default:
		return 0
	}
}

type sourceStream struct {
	core.Stream
	source        *source
	id, key, base string
}

func (s *sourceStream) Recv() (core.Event, error) {
	ev, err := s.Stream.Recv()
	if err != nil {
		s.source.noteFailure(s.id, s.key, s.base, err)
	}
	if ev.Err != nil {
		s.source.noteFailure(s.id, s.key, s.base, ev.Err)
	}
	return ev, err
}
