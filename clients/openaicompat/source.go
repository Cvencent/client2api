package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
	name       string
	deps       core.Deps
	mu         sync.Mutex
	cfg        core.SourceConfig
	now        func() time.Time
	rows       []storedProvider
	pool       *providerPool
	cooling    map[string]time.Time
	lastUsed   map[string]uint64
	sequence   uint64
	rpmWindow  time.Time
	rpmUsed    int
	accountRPM map[string]*sourceRateWindow
	lastErr    string
	active     bool
	generation uint64
	affinity   *core.Affinity
}

type sourceRateWindow struct {
	start time.Time
	used  int
}

func newSource(name string, cfg core.SourceConfig, deps core.Deps) (*source, error) {
	s := &source{
		name:       name,
		deps:       deps,
		now:        time.Now,
		pool:       newProviderPool(),
		cooling:    map[string]time.Time{},
		lastUsed:   map[string]uint64{},
		accountRPM: map[string]*sourceRateWindow{},
		affinity:   core.NewAffinity(0),
	}
	s.affinity.StartGC()
	if err := s.ConfigureSource(cfg); err != nil {
		return nil, err
	}
	return s, nil
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
	s.publishLocked()
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
	return &Client{instanceName: s.name, deps: s.deps, cfg: Config{MaxTokensField: field, Providers: providers}, pool: s.pool, now: time.Now}
}
func (s *source) providerLocked(p storedProvider) ProviderConfig {
	return ProviderConfig{ID: p.ID, APIKey: p.APIKey, AppSecret: p.AppSecret, BaseURL: s.accountBaseLocked(p), Models: append([]string(nil), p.Models...), Label: p.Label, Disabled: p.Disabled}
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
		parks := []map[string]any{}
		for _, m := range models {
			if !allowed[m.ID] {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "unsupported"})
			} else if until := s.cooling[a.ID]; now.Before(until) {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "rate_limit", "until": until.UTC().Format(time.RFC3339)})
			}
		}
		a.Extra["model_cooldowns"] = parks
		if !a.Enabled {
			a.State = "invalid"
		} else if now.Before(s.cooling[a.ID]) {
			a.State = "cooling"
		} else {
			st.Ready = true
		}
		st.Accounts = append(st.Accounts, a)
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
	rpmBlocked := false
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
		candidates := []storedProvider{}
		for _, p := range s.rows {
			if p.Disabled || tried[p.ID] {
				continue
			}
			for _, m := range p.Models {
				if m == req.Model {
					if now.Before(s.cooling[p.ID]) {
						cooling = true
						break
					}
					if !s.accountRPMAllowedLocked(p.ID, now) {
						rpmBlocked = true
						continue
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
		if err := req.AcquireAccountSlot(p.ID); err != nil {
			last = err
			continue
		}
		// RPM is charged only after the local account slot is secured: a slot
		// refusal never reached the vendor and must not consume the next
		// caller's minute.
		s.mu.Lock()
		if !s.acquireRPMsLocked(p.ID, s.now()) {
			s.mu.Unlock()
			req.ReleaseAccountSlot()
			last = core.ErrBusy
			cooling = true
			break
		}
		s.mu.Unlock()
		body, err := buildChatBody(transport.cfg, prov, req.Model, req)
		if err != nil {
			req.ReleaseAccountSlot()
			return nil, err
		}
		core.NoteServedBy(req, p.ID)
		stream, err := transport.doChat(ctx, prov, p.ID, body)
		attempts++
		if err == nil {
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
	if cooling || rpmBlocked {
		return nil, fmt.Errorf("%w: %s has a key for model %q but it is cooling down", core.ErrBusy, s.name, req.Model)
	}
	return nil, fmt.Errorf("%w: %s has no ready API key for model %q; scan models first", core.ErrNotConfigured, s.name, req.Model)
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
			switch core.FailureKindOf(err) {
			case core.FailureRateLimited:
				s.parkLocked(id, core.FailureRateLimited)
			case core.FailureAuth, core.FailureQuota:
				s.parkLocked(id, core.FailureKindOf(err))
			case core.FailureUpstream:
				s.parkLocked(id, core.FailureUpstream)
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

func (s *source) accountRPMAllowedLocked(id string, now time.Time) bool {
	limit := s.cfg.RequestsPerMinutePerAccount
	if limit <= 0 {
		return true
	}
	window := s.accountRPM[id]
	if window == nil || now.Sub(window.start) >= time.Minute {
		return true
	}
	return window.used < limit
}

func (s *source) acquireRPMsLocked(id string, now time.Time) bool {
	if s.cfg.RequestsPerMinute > 0 {
		if s.rpmWindow.IsZero() || now.Sub(s.rpmWindow) >= time.Minute {
			s.rpmWindow = now
			s.rpmUsed = 0
		}
		if s.rpmUsed >= s.cfg.RequestsPerMinute {
			return false
		}
	}
	if s.cfg.RequestsPerMinutePerAccount > 0 {
		window := s.accountRPM[id]
		if window == nil || now.Sub(window.start) >= time.Minute {
			window = &sourceRateWindow{start: now}
			s.accountRPM[id] = window
		}
		if window.used >= s.cfg.RequestsPerMinutePerAccount {
			return false
		}
	}
	if s.cfg.RequestsPerMinute > 0 {
		s.rpmUsed++
	}
	if s.cfg.RequestsPerMinutePerAccount > 0 {
		s.accountRPM[id].used++
	}
	return true
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
