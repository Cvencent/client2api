package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func ParseSources(raw []byte) (map[string]SourceConfig, error) {
	var sources map[string]SourceConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&sources); err != nil {
		return nil, fmt.Errorf("sources: %w", err)
	}
	if err := ValidateSources(sources); err != nil {
		return nil, err
	}
	return sources, nil
}

// SourceConfig defines a hot-editable OpenAI-compatible routing platform.
// Credentials belong to its account store, never this configuration.
type SourceConfig struct {
	Label          string `json:"label,omitempty"`
	BaseURL        string `json:"base_url"`
	Disabled       bool   `json:"disabled,omitempty"`
	MaxTokensField string `json:"max_tokens_field,omitempty"`
}

type SourceClient interface {
	Client
	ConfigureSource(SourceConfig) error
}
type SourceFactory func(string, SourceConfig, Deps) (SourceClient, error)

var sourceFactory SourceFactory
var sourceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func RegisterSourceFactory(factory SourceFactory) {
	regMu.Lock()
	defer regMu.Unlock()
	if factory == nil || sourceFactory != nil {
		panic("core.RegisterSourceFactory: nil or duplicate factory")
	}
	sourceFactory = factory
}

func ValidateSources(sources map[string]SourceConfig) error {
	reserved := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true, "clock": true, "auto": true}
	for i := 1; i <= 9; i++ {
		reserved[fmt.Sprintf("com%d", i)] = true
		reserved[fmt.Sprintf("lpt%d", i)] = true
	}
	for _, name := range Registered() {
		reserved[strings.ToLower(name)] = true
	}
	// These are shared process data folders rather than client stores.
	for _, name := range []string{"backups", "backup", "logs", "configs", "sources"} {
		reserved[name] = true
	}
	for id, cfg := range sources {
		if !sourceIDPattern.MatchString(id) || reserved[id] {
			return fmt.Errorf("source %q: use a unique lowercase ID (letters, digits, hyphens); reserved names are not allowed", id)
		}
		u, err := url.Parse(cfg.BaseURL)
		if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(cfg.BaseURL, "\r\n\t ") {
			return fmt.Errorf("source %q: base_url must be an HTTP(S) API root without credentials, query or fragment", id)
		}
		if len(cfg.Label) > 240 {
			return fmt.Errorf("source %q: label is too long", id)
		}
		if cfg.MaxTokensField != "" && cfg.MaxTokensField != "max_tokens" && cfg.MaxTokensField != "max_completion_tokens" {
			return fmt.Errorf("source %q: max_tokens_field must be max_tokens or max_completion_tokens", id)
		}
	}
	return nil
}

func (r *Registry) ReconcileSources(sources map[string]SourceConfig, deps Deps) error {
	regMu.RLock()
	factory := sourceFactory
	regMu.RUnlock()
	return r.reconcileSources(sources, deps, factory)
}

// Reconciliation preserves source instances, so existing streams and account
// counters survive edits. Removed sources remain alive for their current users.
func (r *Registry) reconcileSources(sources map[string]SourceConfig, deps Deps, factory SourceFactory) error {
	r.sourceMu.Lock()
	defer r.sourceMu.Unlock()
	if err := ValidateSources(sources); err != nil {
		return err
	}
	if len(sources) > 0 && factory == nil {
		return fmt.Errorf("no compatible source factory is registered")
	}
	names := make([]string, 0, len(sources))
	for name, cfg := range sources {
		if !cfg.Disabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	added := map[string]SourceClient{}
	for _, name := range names {
		if existing, ok := r.Get(name); ok {
			if _, owned := r.sources[name]; !owned {
				return fmt.Errorf("source %q collides with a registered client", name)
			}
			if _, ok := existing.(SourceClient); !ok {
				return fmt.Errorf("source %q does not support updates", name)
			}
			continue
		}
		scoped := deps
		scoped.DataDir = filepath.Join(deps.DataDir, name)
		if err := EnsureDir(scoped.DataDir); err != nil {
			return fmt.Errorf("source %s: %w", name, err)
		}
		c, err := factory(name, sources[name], scoped)
		if err != nil {
			return fmt.Errorf("source %s: %w", name, err)
		}
		if c == nil || c.Name() != name {
			return fmt.Errorf("source %s: invalid factory result", name)
		}
		added[name] = c
	}
	for _, name := range names {
		if _, ok := added[name]; ok {
			continue
		}
		c, _ := r.Get(name)
		if err := c.(SourceClient).ConfigureSource(sources[name]); err != nil {
			return fmt.Errorf("source %s: %w", name, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.sources {
		if cfg, ok := sources[name]; !ok || cfg.Disabled {
			if lifecycle, ok := r.clients[name].(interface{ DeactivateSource() }); ok {
				lifecycle.DeactivateSource()
			}
			delete(r.clients, name)
		}
	}
	order := r.order[:0]
	for _, name := range r.order {
		if _, ok := r.clients[name]; ok {
			order = append(order, name)
		}
	}
	r.order = order
	next := map[string]SourceConfig{}
	for _, name := range names {
		if c, ok := added[name]; ok {
			r.clients[name] = c
			r.order = append(r.order, name)
		}
		next[name] = sources[name]
	}
	r.sources = next
	return nil
}
