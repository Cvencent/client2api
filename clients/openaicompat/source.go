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
	rows       []storedProvider
	pool       *providerPool
	cooling    map[string]time.Time
	lastUsed   map[string]uint64
	sequence   uint64
	lastErr    string
	active     bool
	generation uint64
	affinity   *core.Affinity
}

func newSource(name string, cfg core.SourceConfig, deps core.Deps) (*source, error) {
	s := &source{
		name:     name,
		deps:     deps,
		pool:     newProviderPool(),
		cooling:  map[string]time.Time{},
		lastUsed: map[string]uint64{},
		affinity: core.NewAffinity(0),
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
		if strings.TrimRight(rows[i].BaseURL, "/") != base {
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
	if !s.active || s.cfg != cfg || !reflect.DeepEqual(s.rows, rows) {
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
		records = append(records, providerRecord{ID: p.ID, Label: firstNonEmpty(p.Label, p.ID), APIKey: p.APIKey, BaseURL: s.cfg.BaseURL, Models: p.Models, Disabled: p.Disabled, Source: sourcePanel})
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
	return ProviderConfig{ID: p.ID, APIKey: p.APIKey, BaseURL: s.cfg.BaseURL, Models: append([]string(nil), p.Models...), Label: p.Label, Disabled: p.Disabled}
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
	st := core.Status{Name: s.name, UpdatedAt: time.Now(), Accounts: []core.AccountStatus{}}
	s.mu.Lock()
	defer s.mu.Unlock()
	st.Label = firstNonEmpty(s.cfg.Label, s.name)
	st.Detail = s.lastErr
	for _, m := range models {
		st.Models = append(st.Models, m.ID)
	}
	for _, a := range s.pool.statuses(time.Now()) {
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
			} else if until := s.cooling[a.ID]; time.Now().Before(until) {
				parks = append(parks, map[string]any{"model": m.ID, "kind": "rate_limit", "until": until.UTC().Format(time.RFC3339)})
			}
		}
		a.Extra["model_cooldowns"] = parks
		if !a.Enabled {
			a.State = "invalid"
		} else if time.Now().Before(s.cooling[a.ID]) {
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
	conversationKey := core.ConversationKeyOf(req)
	bound, _ := s.affinity.Resolve(conversationKey, s.usableFor(req.Model))
	// Account-slot contention does not spend a network retry; inspect all keys.
	attempts := 0
	for {
		s.mu.Lock()
		if !s.active {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: source %s is disabled or removed", core.ErrNotConfigured, s.name)
		}
		candidates := []storedProvider{}
		for _, p := range s.rows {
			if p.Disabled || tried[p.ID] || time.Now().Before(s.cooling[p.ID]) {
				continue
			}
			for _, m := range p.Models {
				if m == req.Model {
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
	return nil, fmt.Errorf("%w: %s has no ready API key for model %q; scan models first", core.ErrNotConfigured, s.name, req.Model)
}

func (s *source) noteFailure(id, key, base string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimRight(s.cfg.BaseURL, "/") != strings.TrimRight(base, "/") {
		return
	}
	for _, p := range s.rows {
		if p.ID == id && p.APIKey == key {
			s.lastErr = describeError(err)
			switch core.FailureKindOf(err) {
			case core.FailureRateLimited:
				s.cooling[id] = time.Now().Add(30 * time.Second)
			case core.FailureAuth, core.FailureQuota:
				s.cooling[id] = time.Now().Add(5 * time.Minute)
			case core.FailureUpstream:
				s.cooling[id] = time.Now().Add(10 * time.Second)
			}
			break
		}
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
