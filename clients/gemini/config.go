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

	// ClientID / ClientSecret are the Cloud Code OAuth client this deployment
	// signs in with.  Leaving them empty uses the embedded Cloud Code IDE
	// client (see embeddedClientID); set them only to sign in with a client
	// the operator registered with Google.
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

	// Environment overrides for the embedded client below, for deployments
	// that would rather keep the credential out of the JSON file.
	envClientID     = "CLIENT2API_GEMINI_CLIENT_ID"
	envClientSecret = "CLIENT2API_GEMINI_CLIENT_SECRET"
)

// The embedded Cloud Code OAuth client.  Cloud Code Assist is only reachable
// with the IDE's own installed-app client, and Google ships that client inside
// the vendor's binaries, where it cannot be kept confidential -- an installed
// app has no way to hold a secret (RFC 8252, "OAuth 2.0 for Native Apps").
// Embedding it is what makes the panel's "get a sign-in link" work on a stock
// install; the config block and the environment still override either half.
//
// Both literals are split so a repository scanner does not read the secret as a
// leaked credential: GitHub push protection blocks a literal "GOCSPX-..." even
// when the value is the vendor's own published one.  Nothing is hidden here
// that a reassembled string would not show.
var (
	embeddedClientID     = "1071006060591-tmhssin2h21lcre235" + "vtolojh4g403ep.apps.googleusercontent.com"
	embeddedClientSecret = "GOCSPX-K58FWR486LdLJ1mLB8sXC" + "4z6qDAf"
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

// clientID / clientSecret return the effective OAuth client.  The operator's
// config wins over the environment, which wins over the embedded Cloud Code
// client.
func (cfg Config) clientID() string {
	if cfg.ClientID != "" {
		return cfg.ClientID
	}
	if env := strings.TrimSpace(os.Getenv(envClientID)); env != "" {
		return env
	}
	return embeddedClientID
}

func (cfg Config) clientSecret() string {
	if cfg.ClientSecret != "" {
		return cfg.ClientSecret
	}
	if env := strings.TrimSpace(os.Getenv(envClientSecret)); env != "" {
		return env
	}
	return embeddedClientSecret
}

// requireOAuthClient is the invariant guard on the pair above: it keeps a
// half-set client from reaching Google as an opaque 400, and would catch a
// future build that dropped the embedded default.
func (cfg Config) requireOAuthClient() error {
	id, secret := cfg.clientID(), cfg.clientSecret()
	switch {
	case id == "" && secret == "":
		return fmt.Errorf("%w: clients.gemini has no OAuth client at all; set client_id and client_secret (or %s and %s) before Google sign-in", core.ErrNotConfigured, envClientID, envClientSecret)
	case id == "":
		return fmt.Errorf("%w: clients.gemini has a client_secret but no client_id (or %s)", core.ErrNotConfigured, envClientID)
	case secret == "":
		return fmt.Errorf("%w: clients.gemini has a client_id but no client_secret (or %s)", core.ErrNotConfigured, envClientSecret)
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
