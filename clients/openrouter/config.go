package openrouter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Defaults.  Every one of these is overridable from `clients.openrouter`.
const (
	defaultBaseURL  = "https://openrouter.ai/api/v1"
	defaultReferer  = "https://github.com/deepseek-harness/client2api"
	defaultTitle    = "client2api"
	defaultAgent    = "client2api/1.0 (+openrouter)"
	defaultInFlight = 4
)

// defaultAuthURL is the browser entry point of the PKCE login flow.
const defaultAuthURL = "https://openrouter.ai/auth"

// AccountConfig is one credential written directly into the module config.
//
// It is the primary credential path: an operator can drop a key here and the
// module has an account without any panel interaction.
type AccountConfig struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Disabled bool   `json:"disabled"`
}

// Config is the `clients.openrouter` object.
//
// Every field is optional; normalize() fills documented defaults and never
// fails, because a module that refuses to build disappears from the panel and
// cannot be repaired from it.
type Config struct {
	// BaseURL is the API root.  It is a knob rather than a constant so a test
	// (or an operator behind a gateway) can point the module elsewhere.
	BaseURL string `json:"base_url"`
	// APIKey is the convenience form of a single credential.  Accounts is the
	// multi-key form; both may be present.
	APIKey   string          `json:"api_key"`
	Accounts []AccountConfig `json:"accounts"`
	// Referer and Title are the two OPTIONAL attribution headers OpenRouter
	// documents (HTTP-Referer / X-Title).  They are not credentials and the
	// vendor does not require them; they exist so a deployment can identify
	// itself on the vendor's leaderboard.
	Referer      string            `json:"http_referer"`
	Title        string            `json:"x_title"`
	UserAgent    string            `json:"user_agent"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	// ExtraModels are ids appended to the built-in fallback catalogue.  They
	// never override a live model, they only make a cold cache richer.
	ExtraModels []string `json:"extra_models"`

	// FreeOnly narrows the served catalogue, and the models a chat may
	// target, to the zero-cost ids.  It is a tri-state pointer so an absent
	// field keeps the default (on): this deployment exists to spend
	// OpenRouter's free quota, so a paid model must be opted into with
	// `"free_only": false` rather than reached by accident.
	FreeOnly *bool `json:"free_only"`

	ChatTimeout       string `json:"chat_timeout"`
	ModelsTimeout     string `json:"models_timeout"`
	StreamIdleTimeout string `json:"stream_idle_timeout"`
	ModelsTTL         string `json:"models_ttl"`
	Cooldown          string `json:"cooldown"`
	RateCooldown      string `json:"rate_cooldown"`
	QuotaCooldown     string `json:"quota_cooldown"`
	AuthCooldown      string `json:"auth_cooldown"`

	MaxInFlight int `json:"max_in_flight"`

	// MaxTokensField selects which spelling of the output cap is sent.
	// OpenRouter documents `max_tokens` as deprecated in favour of
	// `max_completion_tokens`, so that is the default; an upstream provider
	// that still rejects the newer name can be accommodated by setting this to
	// "max_tokens".
	MaxTokensField string `json:"max_tokens_field"`

	// TestModel is the model the panel's Test button runs one tiny completion
	// against. It has to be a cheap, widely available id; point it somewhere
	// else if the configured key cannot reach the default.
	TestModel string `json:"test_model"`

	// AuthURL is the browser page the PKCE login sends the operator to.  It
	// is a knob rather than a constant so a mirror or a test can point the
	// flow elsewhere; the callback is always a local loopback listener.
	AuthURL string `json:"auth_url"`
	// LoginTimeout bounds how long a started PKCE login stays pending before
	// it is reported as failed.  Zero or unparseable means the default.
	LoginTimeout string `json:"login_timeout"`
}

// parseConfig decodes the raw `clients.openrouter` object.
//
// An absent or `null` object is not an error: it means "no config", and the
// module then runs on its environment/imported credentials and its defaults.
func parseConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) == 0 || string(raw) == "null" {
		return cfg.normalize(), nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("clients.%s: %w", clientName, err)
	}
	return cfg.normalize(), nil
}

// normalize trims, defaults and de-duplicates.  It never returns an error.
func (cfg Config) normalize() Config {
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")

	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	cfg.Referer = strings.TrimSpace(cfg.Referer)
	if cfg.Referer == "" {
		cfg.Referer = defaultReferer
	}
	cfg.Title = strings.TrimSpace(cfg.Title)
	if cfg.Title == "" {
		cfg.Title = defaultTitle
	}
	cfg.UserAgent = strings.TrimSpace(cfg.UserAgent)
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultAgent
	}

	cfg.AuthURL = strings.TrimSpace(cfg.AuthURL)
	cfg.LoginTimeout = strings.TrimSpace(cfg.LoginTimeout)

	headers := make(map[string]string, len(cfg.ExtraHeaders))
	for k, v := range cfg.ExtraHeaders {
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" || reservedHeader(k) {
			continue
		}
		headers[k] = v
	}
	if len(headers) == 0 {
		headers = nil
	}
	cfg.ExtraHeaders = headers

	accounts := make([]AccountConfig, 0, len(cfg.Accounts))
	seen := make(map[string]bool, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		a.ID = strings.TrimSpace(a.ID)
		a.Label = strings.TrimSpace(a.Label)
		a.APIKey = strings.TrimSpace(a.APIKey)
		if a.APIKey == "" {
			// A record with no key cannot serve a request, so it is not an
			// account; keeping it would put a permanently dead row in the
			// panel's table.
			continue
		}
		fp := keyFingerprint(a.APIKey)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		accounts = append(accounts, a)
	}
	cfg.Accounts = accounts

	cfg.ExtraModels = dedupeStrings(cfg.ExtraModels)

	// Anything other than the exact legacy spelling means "use the documented
	// modern name", so a typo cannot silently switch the module to a field the
	// vendor calls deprecated.
	cfg.MaxTokensField = strings.TrimSpace(cfg.MaxTokensField)
	if !strings.EqualFold(cfg.MaxTokensField, "max_tokens") {
		cfg.MaxTokensField = "max_completion_tokens"
	}

	if cfg.MaxInFlight < 0 {
		cfg.MaxInFlight = 0
	}
	return cfg
}

// --- duration knobs -------------------------------------------------------
//
// Every duration is a Go duration string ("10m", "500ms").  An empty,
// unparseable or non-positive value falls back to the default: a zero timeout
// would silently disable a guard rather than mean "no timeout".

func (cfg Config) chatTimeout() time.Duration {
	return durationOr(cfg.ChatTimeout, 10*time.Minute)
}

func (cfg Config) modelsTimeout() time.Duration {
	return durationOr(cfg.ModelsTimeout, 30*time.Second)
}

// streamIdle bounds the gap BETWEEN two SSE frames, not the whole stream: a
// long generation is fine, a stalled connection is not.
func (cfg Config) streamIdle() time.Duration {
	return durationOr(cfg.StreamIdleTimeout, 90*time.Second)
}

func (cfg Config) modelsTTL() time.Duration {
	return durationOr(cfg.ModelsTTL, 10*time.Minute)
}

func (cfg Config) cooldown() time.Duration {
	return durationOr(cfg.Cooldown, 60*time.Second)
}

func (cfg Config) rateCooldown() time.Duration {
	return durationOr(cfg.RateCooldown, 2*time.Minute)
}

func (cfg Config) quotaCooldown() time.Duration {
	return durationOr(cfg.QuotaCooldown, 30*time.Minute)
}

func (cfg Config) authCooldown() time.Duration {
	return durationOr(cfg.AuthCooldown, 30*time.Minute)
}

// maxInFlight is the STARTUP ceiling; ApplyLive may replace it at runtime.
func (cfg Config) maxInFlight() int {
	if cfg.MaxInFlight > 0 {
		return cfg.MaxInFlight
	}
	return defaultInFlight
}

// usesLegacyMaxTokens reports whether the deprecated `max_tokens` spelling was
// explicitly requested.
func (cfg Config) usesLegacyMaxTokens() bool {
	return strings.EqualFold(strings.TrimSpace(cfg.MaxTokensField), "max_tokens")
}

// freeOnly reports whether the deployment is restricted to zero-cost models.
// It defaults to true: an explicit `"free_only": false` is the only way to
// reach the vendor's paid catalogue.
func (cfg Config) freeOnly() bool {
	if cfg.FreeOnly == nil {
		return true
	}
	return *cfg.FreeOnly
}

// testModel is the id the panel's Test button probes with.
func (cfg Config) testModel() string {
	return firstNonEmpty(strings.TrimSpace(cfg.TestModel), "openai/gpt-4o-mini")
}

func durationOr(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// --- URL builders ---------------------------------------------------------

func (cfg Config) base() string {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return defaultBaseURL
	}
	return base
}

func (cfg Config) chatURL() string    { return cfg.base() + "/chat/completions" }
func (cfg Config) modelsURL() string  { return cfg.base() + "/models" }
func (cfg Config) creditsURL() string { return cfg.base() + "/credits" }
func (cfg Config) keyURL() string     { return cfg.base() + "/key" }

// keysURL is the PKCE code-exchange endpoint.  It sits under the API base,
// not the browser auth page.
func (cfg Config) keysURL() string { return cfg.base() + "/auth/keys" }

// authURL is the browser page the PKCE login opens.
func (cfg Config) authURL() string {
	base := strings.TrimRight(strings.TrimSpace(cfg.AuthURL), "/")
	if base == "" {
		return defaultAuthURL
	}
	return base
}

// loginTimeout bounds how long a PKCE login stays pending.
func (cfg Config) loginTimeout() time.Duration {
	return durationOr(cfg.LoginTimeout, 5*time.Minute)
}

// --- small helpers --------------------------------------------------------

// reservedHeader reports whether a header may NOT be overridden through
// extra_headers.  The list is the set of headers this module builds itself
// plus the hop-by-hop ones: letting a config file replace Authorization would
// silently detach the credential from the request.
func reservedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "accept-encoding",
		"user-agent", "host", "content-length", "transfer-encoding",
		"connection", "http-referer", "x-title":
		return true
	}
	return false
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyHeaders installs the module's own headers on a request.  `key` may be
// empty (the catalogue and discovery paths are unauthenticated).
func (cfg Config) applyHeaders(req *http.Request, key string) {
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if cfg.Referer != "" {
		req.Header.Set("HTTP-Referer", cfg.Referer)
	}
	if cfg.Title != "" {
		req.Header.Set("X-Title", cfg.Title)
	}
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}
	for k, v := range cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
}
