package gemini

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// AccountConfig is one credential as it appears in the config file.  The field
// names mirror the OAuth response verbatim (access_token / expiry …) because
// the whole JSON is round-tripped on refresh.
type AccountConfig struct {
	Label              string `json:"label,omitempty"`
	AccessToken        string `json:"access_token,omitempty"`
	RefreshToken       string `json:"refresh_token,omitempty"`
	TokenType          string `json:"token_type,omitempty"`
	ExpiresIn          int    `json:"expires_in,omitempty"`
	Scope              string `json:"scope,omitempty"`
	Expiry             string `json:"expiry,omitempty"`
	Sub                string `json:"sub,omitempty"`
	Email              string `json:"email,omitempty"`
	CloudAICodeProject string `json:"cloudaicompanionProject,omitempty"`
	Disabled           bool   `json:"disabled,omitempty"`
}

// Config is the clients.gemini config block.  Every field is optional: a zero
// Config is a working configuration pointed at the vendor's production
// endpoints with no credentials (which reports core.ErrNotConfigured until a
// Google account is signed in).
type Config struct {
	AccountConfig

	// ClientID / ClientSecret are the Cloud Code OAuth client the operator
	// registered with Google.  They are deliberately not embedded in this
	// repository: GitHub push protection rightly treats a pasted Google client
	// secret as a credential, and the value is deployment-specific anyway.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`

	// Project overrides the loadCodeAssist probe.  Empty means "probe, then
	// fall back to aicode-consumers".
	Project string `json:"project,omitempty"`

	// LoginPort is the loopback port the OAuth callback listens on.  0 asks the
	// kernel for a free one, which is what the reference does.
	LoginPort int `json:"login_port,omitempty"`

	// ExtraHeaders are added to every upstream request.  A key colliding with an
	// authentication or identity header is ignored.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`

	// Duration knobs, as Go duration strings ("10m", "500ms").
	LoginTimeout  string `json:"login_timeout,omitempty"`
	ChatTimeout   string `json:"chat_timeout,omitempty"`
	QuotaTimeout  string `json:"quota_timeout,omitempty"`
	Cooldown      string `json:"cooldown,omitempty"`
	QuotaCooldown string `json:"quota_cooldown,omitempty"`

	MaxInFlight int `json:"max_in_flight,omitempty"`

	Accounts []AccountConfig `json:"accounts,omitempty"`
}

// Defaults.
const (
	defaultLoginTimeout  = 6 * time.Minute
	defaultChatTimeout   = 10 * time.Minute
	defaultQuotaTimeout  = 30 * time.Second
	defaultCooldown      = 60 * time.Second
	defaultQuotaCooldown = 12 * time.Hour
	defaultMaxInFlight   = 2

	// Environment fallbacks keep the OAuth client out of the repository while
	// still allowing a deployment to inject it without editing a JSON file.
	envClientID     = "CLIENT2API_GEMINI_CLIENT_ID"
	envClientSecret = "CLIENT2API_GEMINI_CLIENT_SECRET"
)

// parseConfig decodes the module's config block.  An absent block is not an
// error: it is a Config with defaults.
func parseConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("clients.%s: %w", clientName, err)
	}
	return cfg, nil
}

// normalize trims every string and fills in the documented defaults.  It never
// returns an error: an unusable value falls back to the default.
func (cfg Config) normalize() Config {
	out := cfg
	out.ClientID = strings.TrimSpace(out.ClientID)
	out.ClientSecret = strings.TrimSpace(out.ClientSecret)
	out.Project = strings.TrimSpace(out.Project)
	if out.LoginPort < 0 || out.LoginPort > 65535 {
		out.LoginPort = 0
	}
	if out.MaxInFlight <= 0 {
		out.MaxInFlight = defaultMaxInFlight
	}
	out.AccountConfig = normalizeAccount(out.AccountConfig)
	accounts := make([]AccountConfig, 0, len(cfg.Accounts))
	for _, acct := range cfg.Accounts {
		acct = normalizeAccount(acct)
		if acct.AccessToken == "" && acct.RefreshToken == "" {
			continue
		}
		accounts = append(accounts, acct)
	}
	out.Accounts = accounts

	headers := make(map[string]string, len(cfg.ExtraHeaders))
	for k, v := range cfg.ExtraHeaders {
		k = strings.TrimSpace(k)
		if k == "" || reservedHeader(k) {
			continue
		}
		headers[k] = strings.TrimSpace(v)
	}
	out.ExtraHeaders = headers
	return out
}

// normalizeAccount trims one credential and drops nothing here (the caller
// skips empty ones).
func normalizeAccount(acct AccountConfig) AccountConfig {
	acct.Label = strings.TrimSpace(acct.Label)
	acct.AccessToken = strings.TrimSpace(acct.AccessToken)
	acct.RefreshToken = strings.TrimSpace(acct.RefreshToken)
	acct.TokenType = strings.TrimSpace(acct.TokenType)
	acct.Scope = strings.TrimSpace(acct.Scope)
	acct.Expiry = strings.TrimSpace(acct.Expiry)
	acct.Sub = strings.TrimSpace(acct.Sub)
	acct.Email = strings.TrimSpace(acct.Email)
	acct.CloudAICodeProject = strings.TrimSpace(acct.CloudAICodeProject)
	return acct
}

// configuredAccounts flattens the top-level credential and the accounts array.
func (cfg Config) configuredAccounts() []AccountConfig {
	var out []AccountConfig
	if cfg.AccessToken != "" || cfg.RefreshToken != "" {
		out = append(out, cfg.AccountConfig)
	}
	out = append(out, cfg.Accounts...)
	return out
}

// reservedHeader reports whether a configured extra header would collide with
// one the module owns.
func reservedHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization", "content-type", "accept", "user-agent",
		"x-client-name", "x-client-version", "x-machine-id", "x-vscode-sessionid",
		"x-goog-api-key", "x-goog-api-client":
		return true
	}
	return false
}

// clientID / clientSecret return the effective OAuth client.  Config wins over
// the environment; neither is allowed to pretend a missing deployment secret
// is a built-in default.
func (cfg Config) clientID() string {
	if cfg.ClientID != "" {
		return cfg.ClientID
	}
	return strings.TrimSpace(os.Getenv(envClientID))
}

func (cfg Config) clientSecret() string {
	if cfg.ClientSecret != "" {
		return cfg.ClientSecret
	}
	return strings.TrimSpace(os.Getenv(envClientSecret))
}

// requireOAuthClient reports a missing deployment-specific OAuth client
// before a login or refresh reaches Google and gets an opaque 400.
func (cfg Config) requireOAuthClient() error {
	id, secret := cfg.clientID(), cfg.clientSecret()
	switch {
	case id == "" && secret == "":
		return fmt.Errorf("%w: clients.gemini needs client_id and client_secret (or %s and %s) before Google sign-in", core.ErrNotConfigured, envClientID, envClientSecret)
	case id == "":
		return fmt.Errorf("%w: clients.gemini client_id is required (or %s)", core.ErrNotConfigured, envClientID)
	case secret == "":
		return fmt.Errorf("%w: clients.gemini client_secret is required (or %s)", core.ErrNotConfigured, envClientSecret)
	}
	return nil
}

// --- duration accessors -----------------------------------------------------

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

func (cfg Config) loginTimeout() time.Duration {
	return durationOr(cfg.LoginTimeout, defaultLoginTimeout)
}
func (cfg Config) chatTimeout() time.Duration { return durationOr(cfg.ChatTimeout, defaultChatTimeout) }
func (cfg Config) quotaTimeout() time.Duration {
	return durationOr(cfg.QuotaTimeout, defaultQuotaTimeout)
}
func (cfg Config) cooldown() time.Duration { return durationOr(cfg.Cooldown, defaultCooldown) }
func (cfg Config) quotaCooldown() time.Duration {
	return durationOr(cfg.QuotaCooldown, defaultQuotaCooldown)
}

func (cfg Config) maxInFlight() int {
	if cfg.MaxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return cfg.MaxInFlight
}
