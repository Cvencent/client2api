package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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
	Label              string  `json:"label,omitempty"`
	BaseURL            string  `json:"base_url"`
	Disabled           bool    `json:"disabled,omitempty"`
	MaxTokensField     string  `json:"max_tokens_field,omitempty"`
	BalanceProtocol    string  `json:"balance_protocol,omitempty"`
	BalanceQuotaPerUSD float64 `json:"balance_quota_per_usd,omitempty"`

	// RequestsPerMinute caps every upstream attempt for this source.  Zero
	// means no per-source RPM ceiling.
	RequestsPerMinute int `json:"requests_per_minute,omitempty"`
	// RequestsPerMinutePerAccount applies the same rule to one credential.
	// When one key is full the source tries the next key; only when every
	// eligible key is full does it report busy.
	RequestsPerMinutePerAccount int `json:"requests_per_minute_per_account,omitempty"`
	// MaxInFlight and MaxInFlightPerAccount feed the gateway's existing
	// platform brakes.  A pointer keeps "absent" distinct from an explicit
	// zero, which means no ceiling.
	MaxInFlight           *int `json:"max_in_flight,omitempty"`
	MaxInFlightPerAccount *int `json:"max_in_flight_per_account,omitempty"`

	// Cooldown is the shared fallback for every failure type.  The more
	// specific durations win when they are set, and an explicit "0s" disables
	// that failure's cooldown.
	Cooldown          string `json:"cooldown,omitempty"`
	RateLimitCooldown string `json:"rate_limit_cooldown,omitempty"`
	QuotaCooldown     string `json:"quota_cooldown,omitempty"`
	AuthCooldown      string `json:"auth_cooldown,omitempty"`
	UpstreamCooldown  string `json:"upstream_cooldown,omitempty"`
}

// CooldownFor resolves one failure's cooldown.  The specific field wins over
// the shared cooldown, an explicit zero disables the park, and def is used
// only when neither field is set.  Invalid values are rejected by
// ValidateSources, so the parse fallback here is only for hand-built configs
// and tests.
func (cfg SourceConfig) CooldownFor(kind FailureKind, def time.Duration) time.Duration {
	raw := cfg.Cooldown
	switch kind {
	case FailureRateLimited:
		if cfg.RateLimitCooldown != "" {
			raw = cfg.RateLimitCooldown
		}
	case FailureQuota:
		if cfg.QuotaCooldown != "" {
			raw = cfg.QuotaCooldown
		}
	case FailureAuth:
		if cfg.AuthCooldown != "" {
			raw = cfg.AuthCooldown
		}
	case FailureUpstream:
		if cfg.UpstreamCooldown != "" {
			raw = cfg.UpstreamCooldown
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		return def
	}
	return d
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

// --- shipped platforms ------------------------------------------------------

// builtinSources are the routing platforms this binary ships with.  A module
// declares its own default from init(), and the config loader, the source
// validator and the panel all read the same table -- so a shipped platform
// behaves exactly like one the operator wrote into the config file, except
// that it exists without an entry.
var builtinSources = map[string]SourceConfig{}

// RegisterBuiltinSource declares one shipped platform.  The operator's config
// always wins over this definition, including an entry that only disables the
// platform; a missing entry means "use the shipped default".  A duplicate or
// malformed registration is a programming error, exactly like Register.
func RegisterBuiltinSource(id string, cfg SourceConfig) {
	id = strings.ToLower(strings.TrimSpace(id))
	if !sourceIDPattern.MatchString(id) {
		panic("core.RegisterBuiltinSource: invalid platform id " + id)
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		panic("core.RegisterBuiltinSource: " + id + " has no base_url")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := builtinSources[id]; dup {
		panic("core.RegisterBuiltinSource: duplicate platform " + id)
	}
	builtinSources[id] = cfg
}

// BuiltinSourceDefaults copies the shipped platform definitions, keyed by id.
func BuiltinSourceDefaults() map[string]SourceConfig {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make(map[string]SourceConfig, len(builtinSources))
	for id, cfg := range builtinSources {
		out[id] = cfg
	}
	return out
}

// BuiltinSource reports one shipped platform definition.
func BuiltinSource(id string) (SourceConfig, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	cfg, ok := builtinSources[strings.ToLower(strings.TrimSpace(id))]
	return cfg, ok
}

// WithBuiltinSources returns the effective source table: every shipped
// platform the operator has not defined, plus the operator's own entries
// unchanged.  The loader calls it on every read, so a shipped platform needs
// no config entry while an operator's edit -- including one that only flips
// "disabled" -- always wins.
func WithBuiltinSources(cfg map[string]SourceConfig) map[string]SourceConfig {
	defaults := BuiltinSourceDefaults()
	out := make(map[string]SourceConfig, len(cfg)+len(defaults))
	for id, c := range defaults {
		out[id] = c
	}
	for id, c := range cfg {
		out[id] = c
	}
	return out
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
		switch strings.ToLower(strings.TrimSpace(cfg.BalanceProtocol)) {
		case "", "auto", "newapi", "sub2api", "openai_billing", "none":
		default:
			return fmt.Errorf("source %q: balance_protocol must be auto, newapi, sub2api, openai_billing or none", id)
		}
		if math.IsNaN(cfg.BalanceQuotaPerUSD) || math.IsInf(cfg.BalanceQuotaPerUSD, 0) || cfg.BalanceQuotaPerUSD < 0 {
			return fmt.Errorf("source %q: balance_quota_per_usd must be positive when set", id)
		}
		if cfg.RequestsPerMinute < 0 {
			return fmt.Errorf("source %q: requests_per_minute must be >= 0", id)
		}
		if cfg.RequestsPerMinutePerAccount < 0 {
			return fmt.Errorf("source %q: requests_per_minute_per_account must be >= 0", id)
		}
		if cfg.MaxInFlight != nil && *cfg.MaxInFlight < 0 {
			return fmt.Errorf("source %q: max_in_flight must be >= 0", id)
		}
		if cfg.MaxInFlightPerAccount != nil && *cfg.MaxInFlightPerAccount < 0 {
			return fmt.Errorf("source %q: max_in_flight_per_account must be >= 0", id)
		}
		for name, raw := range map[string]string{
			"cooldown":            cfg.Cooldown,
			"rate_limit_cooldown": cfg.RateLimitCooldown,
			"quota_cooldown":      cfg.QuotaCooldown,
			"auth_cooldown":       cfg.AuthCooldown,
			"upstream_cooldown":   cfg.UpstreamCooldown,
		} {
			if err := validateSourceCooldown(raw); err != nil {
				return fmt.Errorf("source %q: %s %v", id, name, err)
			}
		}
	}
	return nil
}

func validateSourceCooldown(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("must be a duration such as 30s or 5m")
	}
	if d < 0 {
		return fmt.Errorf("must not be negative")
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
