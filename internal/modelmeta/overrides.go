package modelmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Override is an operator-supplied model metadata override. A zero field means
// "no manual value for this field", so context and output can be restored
// independently.
type Override struct {
	ContextLength   int64 `json:"context_length,omitempty"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	// SupportedEfforts is an optional manual reasoning-effort vocabulary.
	// Nil or empty means "no manual value", so the resolved list continues to
	// come from the module cache or the embedded table.
	SupportedEfforts []string `json:"supported_efforts,omitempty"`
	// DefaultEffort is an optional manual default. It is only authoritative
	// when the resolved vocabulary contains it; Merge drops it otherwise.
	DefaultEffort string `json:"default_effort,omitempty"`
	// ElevateTop opts this model into cap alignment: a caller's top tier
	// (xhigh) may be promoted to the model's actual highest tier (for example
	// max). The zero value is deliberately off, so existing installs are
	// unchanged until an operator enables it in the panel.
	ElevateTop bool `json:"elevate_top,omitempty"`
	// ElevateTopExplicit records that the operator made a deliberate
	// cap-alignment choice. It is what lets an explicit "off" survive a model
	// whose embedded table defaults the policy on: without it, "off" and "never
	// configured" would be indistinguishable and the default would win.
	ElevateTopExplicit bool `json:"elevate_top_explicit,omitempty"`
	// Prices are in CNY per million tokens. Has flags keep an explicit free
	// model (0/0) distinct from a field that was never configured.
	InputPerMillion     float64 `json:"input_per_million,omitempty"`
	OutputPerMillion    float64 `json:"output_per_million,omitempty"`
	CacheReadPerMillion float64 `json:"cache_read_per_million,omitempty"`
	HasPrice            bool    `json:"has_price,omitempty"`
	HasCacheRead        bool    `json:"has_cache_read,omitempty"`
}

// IsZero reports whether the override carries no manual value.
func (o Override) IsZero() bool {
	return o.ContextLength <= 0 && o.MaxOutputTokens <= 0 && len(o.SupportedEfforts) == 0 &&
		o.DefaultEffort == "" && !o.ElevateTop && !o.ElevateTopExplicit &&
		!o.HasPrice && !o.HasCacheRead
}

// ExplicitElevateTop reports the operator's cap-alignment choice and whether
// they made one at all. The second result distinguishes a deliberate "off" from
// "never configured", which lets an embedded default apply only in the latter.
func (o Override) ExplicitElevateTop() (on bool, explicit bool) {
	// Files written before ElevateTopExplicit existed carry only
	// elevate_top:true. That was still a deliberate operator choice, so it must
	// keep overriding an embedded default instead of being read as "unset".
	return o.ElevateTop, o.ElevateTopExplicit || o.ElevateTop
}

type overrideDoc struct {
	Clients map[string]map[string]Override `json:"clients"`
}

// OverrideStore persists operator-supplied model metadata overrides. It is safe
// for concurrent use by the panel and the gateway.
type OverrideStore struct {
	path string

	mu  sync.RWMutex
	doc overrideDoc
}

// OpenOverrideStore opens or creates an override store. An empty path creates
// an in-memory-only store, which keeps tests and callers without a data
// directory simple. A missing file is an empty store; a corrupt file is treated
// as empty and reported through the optional warning function.
func OpenOverrideStore(path string, warnf ...func(string, ...any)) (*OverrideStore, error) {
	s := &OverrideStore{path: strings.TrimSpace(path)}
	s.doc = overrideDoc{Clients: map[string]map[string]Override{}}
	if s.path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return s, nil
	}
	var doc overrideDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		if len(warnf) > 0 && warnf[0] != nil {
			warnf[0]("modelmeta: ignoring corrupt override file %s: %v", s.path, err)
		}
		return s, nil
	}
	if doc.Clients == nil {
		doc.Clients = map[string]map[string]Override{}
	}
	s.doc = doc
	return s, nil
}

// Get returns the override for one client/model pair.
func (s *OverrideStore) Get(client, model string) (Override, bool) {
	if s == nil {
		return Override{}, false
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return Override{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := s.doc.Clients[client]
	if models == nil {
		return Override{}, false
	}
	o, ok := models[model]
	if !ok || o.IsZero() {
		return Override{}, false
	}
	return o, true
}

// Set replaces the context/output manual values for one client/model pair.
// Price fields that were already configured are preserved.
func (s *OverrideStore) Set(client, model string, contextLength, maxOutputTokens int64) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	if contextLength < 0 || maxOutputTokens < 0 {
		return errors.New("modelmeta: overrides must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.ContextLength = contextLength
	o.MaxOutputTokens = maxOutputTokens
	return s.putLocked(client, model, o)
}

// SetEfforts replaces the manual reasoning-effort vocabulary and default while
// preserving every other override. Empty values clear those manual fields.
// Levels are normalised to the lower-case vocabulary modules send upstream.
func (s *OverrideStore) SetEfforts(client, model string, efforts []string, defaultEffort string) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	efforts = normalizeEfforts(efforts)
	defaultEffort = strings.ToLower(strings.TrimSpace(defaultEffort))
	if defaultEffort != "" && len(efforts) > 0 && !containsString(efforts, defaultEffort) {
		defaultEffort = ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.SupportedEfforts = efforts
	o.DefaultEffort = defaultEffort
	return s.putLocked(client, model, o)
}

// SetElevateTop enables or disables cap alignment for one model. The flag is
// kept independently of the manual effort vocabulary so an operator can use
// the module's live facts while opting into the policy.
func (s *OverrideStore) SetElevateTop(client, model string, on bool) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.ElevateTop = on
	o.ElevateTopExplicit = true
	return s.putLocked(client, model, o)
}

// ClearElevateTop drops the operator's cap-alignment choice so the model falls
// back to the embedded table's per-model default. It backs the panel's
// "restore default" action.
func (s *OverrideStore) ClearElevateTop(client, model string) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.ElevateTop = false
	o.ElevateTopExplicit = false
	return s.putLocked(client, model, o)
}

// SetPrice replaces the manual per-million-token price for one client/model
// pair while preserving context and output overrides. A price with both input
// and output zero is still retained when HasPrice is true, representing a
// deliberately free model rather than an unknown one.
func (s *OverrideStore) SetPrice(client, model string, inputPerMillion, outputPerMillion, cacheReadPerMillion float64, hasCacheRead bool) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	if inputPerMillion < 0 || outputPerMillion < 0 || cacheReadPerMillion < 0 {
		return errors.New("modelmeta: prices must not be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.InputPerMillion = inputPerMillion
	o.OutputPerMillion = outputPerMillion
	o.CacheReadPerMillion = cacheReadPerMillion
	o.HasPrice = true
	o.HasCacheRead = hasCacheRead
	if !hasCacheRead {
		o.CacheReadPerMillion = 0
	}
	return s.putLocked(client, model, o)
}

// DeletePrice removes only the manual price, leaving context/output intact.
func (s *OverrideStore) DeletePrice(client, model string) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.getLocked(client, model)
	o.InputPerMillion = 0
	o.OutputPerMillion = 0
	o.CacheReadPerMillion = 0
	o.HasPrice = false
	o.HasCacheRead = false
	return s.putLocked(client, model, o)
}

// Delete removes all manual values for one client/model pair.
func (s *OverrideStore) Delete(client, model string) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	client, model = strings.TrimSpace(client), strings.TrimSpace(model)
	if client == "" || model == "" {
		return errors.New("modelmeta: client and model are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if models := s.doc.Clients[client]; models != nil {
		delete(models, model)
		if len(models) == 0 {
			delete(s.doc.Clients, client)
		}
	}
	return s.writeLocked()
}

func (s *OverrideStore) getLocked(client, model string) Override {
	if models := s.doc.Clients[client]; models != nil {
		return models[model]
	}
	return Override{}
}

func (s *OverrideStore) putLocked(client, model string, o Override) error {
	if s.doc.Clients == nil {
		s.doc.Clients = map[string]map[string]Override{}
	}
	if o.IsZero() {
		if models := s.doc.Clients[client]; models != nil {
			delete(models, model)
			if len(models) == 0 {
				delete(s.doc.Clients, client)
			}
		}
		return s.writeLocked()
	}
	models := s.doc.Clients[client]
	if models == nil {
		models = map[string]Override{}
		s.doc.Clients[client] = models
	}
	models[model] = o
	return s.writeLocked()
}

func normalizeEfforts(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, effort := range in {
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" {
			continue
		}
		out = append(out, effort)
	}
	return dedupeStrings(out)
}

// Snapshot returns a deep copy of all overrides.
func (s *OverrideStore) Snapshot() map[string]map[string]Override {
	if s == nil {
		return map[string]map[string]Override{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]map[string]Override, len(s.doc.Clients))
	for client, models := range s.doc.Clients {
		if len(models) == 0 {
			continue
		}
		cp := make(map[string]Override, len(models))
		for model, o := range models {
			if !o.IsZero() {
				cp[model] = o
			}
		}
		if len(cp) > 0 {
			out[client] = cp
		}
	}
	return out
}

// Replace swaps the whole override document for the supplied snapshot and
// persists it. It is the restore path: unlike Set it also drops entries the
// incoming document does not carry, so a bundle is authoritative for what the
// target had configured.
func (s *OverrideStore) Replace(snapshot map[string]map[string]Override) error {
	if s == nil {
		return errors.New("modelmeta: nil override store")
	}
	next := overrideDoc{Clients: map[string]map[string]Override{}}
	for client, models := range snapshot {
		client = strings.TrimSpace(client)
		if client == "" {
			continue
		}
		out := map[string]Override{}
		for model, o := range models {
			model = strings.TrimSpace(model)
			if model == "" || o.IsZero() {
				continue
			}
			out[model] = o
		}
		if len(out) > 0 {
			next.Clients[client] = out
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = next
	return s.writeLocked()
}

func (s *OverrideStore) writeLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".model_context-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace override file: %w", err)
	}
	return nil
}
