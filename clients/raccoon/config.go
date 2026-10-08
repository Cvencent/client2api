package raccoon

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// Files the module owns inside Deps.DataDir.
const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
)

// Environment overrides. A value set in the config object always wins over
// the environment, so a deployment can pin a credential in the config and
// still keep the environment variable as a fallback for other machines.
const (
	envAccessToken  = "CLIENT2API_RACCOON_ACCESS_TOKEN"
	envRefreshToken = "CLIENT2API_RACCOON_REFRESH_TOKEN"
	envUserID       = "CLIENT2API_RACCOON_USER_ID"
	envNickname     = "CLIENT2API_RACCOON_NICKNAME"
	envPhone        = "CLIENT2API_RACCOON_PHONE"
	envDeviceID     = "CLIENT2API_RACCOON_DEVICE_ID"
	envOffice       = "CLIENT2API_RACCOON_OFFICE_IDENTITY"
)

// Defaults. Every one of these is what an unset (or unparseable) config field
// resolves to.
const (
	defaultMaxAttempts       = 3
	defaultModelsTTL         = 5 * time.Minute
	defaultModelsTimeout     = 20 * time.Second
	defaultChatTimeout       = 10 * time.Minute
	defaultIdleTimeout       = 2 * time.Minute
	defaultFirstTokenTimeout = 120 * time.Second
	defaultCooldown          = 60 * time.Second
	defaultShortCooldown     = 15 * time.Second
	defaultQuotaCooldown     = 24 * time.Hour
	defaultRefreshMargin     = tokenRefreshWindow
	defaultStoreFlush        = 5 * time.Second
	defaultModelsRetry       = 30 * time.Second
	defaultLoginTimeout      = 5 * time.Minute
)

// durationField accepts either a Go duration string ("90s", "5m") or a bare
// JSON number read as SECONDS. An empty string or null is a no-op.
type durationField string

func (d durationField) String() string { return string(d) }

func (d *durationField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" || s == `""` {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*d = durationField(v)
		return nil
	}
	*d = durationField(s)
	return nil
}

// accountConfig is one entry of the `accounts` array (or the single-account
// shorthand). It is a credential plus the two operator-facing knobs.
type accountConfig struct {
	Label        string `json:"label,omitempty"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Office       string `json:"office_identity,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Phone        string `json:"phone,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Disabled     bool   `json:"disabled,omitempty"`
}

// config mirrors the JSON object under `clients.raccoon`.
type config struct {
	BaseURL        string `json:"base_url,omitempty"`
	UserAgent      string `json:"user_agent,omitempty"`
	ClientPlatform string `json:"client_platform,omitempty"`
	ClientVersion  string `json:"client_version,omitempty"`

	// TLSProfile/TLSProtocol impersonate the desktop client's TLS
	// ClientHello.  The module drives a signed-in account against the
	// vendor's own API, so a Go-shaped handshake is a detectable tell.
	// Empty keeps the stock handshake.
	TLSProfile  string `json:"tls_profile,omitempty"`
	TLSProtocol string `json:"tls_protocol,omitempty"`

	Accounts []accountConfig `json:"accounts,omitempty"`

	// Single-account shorthand.
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	Office       string `json:"office_identity,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Phone        string `json:"phone,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	Label        string `json:"label,omitempty"`

	// CredentialPaths are operator-declared JSON files that are offered as
	// importable credentials. This is the ONLY discovery input: the module
	// does not guess at vendor client storage locations (see README).
	CredentialPaths []string `json:"credential_paths,omitempty"`

	MaxAttempts       *int          `json:"max_attempts,omitempty"`
	ModelsTTL         durationField `json:"models_ttl,omitempty"`
	ModelsTimeout     durationField `json:"models_timeout,omitempty"`
	ChatTimeout       durationField `json:"chat_timeout,omitempty"`
	IdleTimeout       durationField `json:"idle_timeout,omitempty"`
	FirstTokenTimeout durationField `json:"first_token_timeout,omitempty"`
	Cooldown          durationField `json:"cooldown,omitempty"`
	ShortCooldown     durationField `json:"short_cooldown,omitempty"`
	QuotaCooldown     durationField `json:"quota_cooldown,omitempty"`
	RefreshMargin     durationField `json:"refresh_margin,omitempty"`
	StoreFlush        durationField `json:"store_flush,omitempty"`

	// LoginTimeout bounds how long an interactive login stays pending.
	LoginTimeout durationField `json:"login_timeout,omitempty"`
}

func (c config) baseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); v != "" {
		return v
	}
	return raccoonAPIBase
}

func (c config) endpoint(p string) string { return c.baseURL() + p }

func (c config) userAgent() string {
	if v := strings.TrimSpace(c.UserAgent); v != "" {
		return v
	}
	return userAgent
}

func (c config) platform() string {
	if v := strings.TrimSpace(c.ClientPlatform); v != "" {
		return v
	}
	return clientPlatform
}

func (c config) version() string {
	if v := strings.TrimSpace(c.ClientVersion); v != "" {
		return v
	}
	return clientVersion
}

// tlsProfile / tlsProtocol expose the transport-fingerprint keys to New.
func (c config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

func (c config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

func (c config) maxAttempts() int {
	if c.MaxAttempts != nil && *c.MaxAttempts > 0 {
		return *c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c config) modelsTTL() time.Duration {
	return durationOr(c.ModelsTTL.String(), defaultModelsTTL)
}
func (c config) modelsTimeout() time.Duration {
	return durationOr(c.ModelsTimeout.String(), defaultModelsTimeout)
}
func (c config) chatTimeout() time.Duration {
	return durationOr(c.ChatTimeout.String(), defaultChatTimeout)
}
func (c config) idleTimeout() time.Duration {
	return durationOr(c.IdleTimeout.String(), defaultIdleTimeout)
}
func (c config) firstTokenTimeout() time.Duration {
	return durationOr(c.FirstTokenTimeout.String(), defaultFirstTokenTimeout)
}
func (c config) cooldown() time.Duration {
	return durationOr(c.Cooldown.String(), defaultCooldown)
}
func (c config) shortCooldown() time.Duration {
	return durationOr(c.ShortCooldown.String(), defaultShortCooldown)
}
func (c config) quotaCooldown() time.Duration {
	return durationOr(c.QuotaCooldown.String(), defaultQuotaCooldown)
}
func (c config) refreshMargin() time.Duration {
	return durationOr(c.RefreshMargin.String(), defaultRefreshMargin)
}
func (c config) storeFlush() time.Duration {
	return durationOr(c.StoreFlush.String(), defaultStoreFlush)
}

func (c config) loginTimeout() time.Duration {
	return durationOr(c.LoginTimeout.String(), defaultLoginTimeout)
}

func (c config) credentialPaths() []string {
	out := make([]string, 0, len(c.CredentialPaths))
	for _, p := range c.CredentialPaths {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// durationOr parses a Go duration, then a bare number of seconds, and falls
// back to def. A non-positive result also falls back to def, so "0s" can
// never accidentally disable a timeout.
func durationOr(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d > 0 {
			return d
		}
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// parseConfig decodes the module's config object. An absent object is the
// same as `{}`. A MALFORMED object returns an error that the factory logs
// before carrying on with defaults: a config typo must never take the module
// down.
func parseConfig(raw []byte) (config, error) {
	var cfg config
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// configuredAccounts merges the `accounts` array, the single-account
// shorthand and the environment overrides, in that priority order. Entries
// without an access token are dropped. Order is preserved so LRU ties are
// deterministic.
func configuredAccounts(cfg config) []account {
	var out []account
	add := func(a accountConfig, origin string) {
		tok := strings.TrimSpace(a.AccessToken)
		if tok == "" {
			return
		}
		office := strings.TrimSpace(a.Office)
		// A personal account carries no org code: the vendor's own user_info
		// reports office_identity as "", and inventing the sentinel
		// "personal" makes the chat endpoint answer 200022 org_not_found.
		out = append(out, account{
			credential: credential{
				AccessToken:    tok,
				RefreshToken:   strings.TrimSpace(a.RefreshToken),
				ExpiresAt:      flexString(strings.TrimSpace(a.ExpiresAt)),
				OfficeIdentity: office,
				UserID:         strings.TrimSpace(a.UserID),
				Nickname:       strings.TrimSpace(a.Nickname),
				Phone:          strings.TrimSpace(a.Phone),
				DeviceID:       strings.TrimSpace(a.DeviceID),
			},
			Label:    strings.TrimSpace(a.Label),
			Disabled: a.Disabled,
			Origin:   origin,
		})
	}
	for _, a := range cfg.Accounts {
		add(a, originConfig)
	}
	add(accountConfig{
		Label:        cfg.Label,
		AccessToken:  cfg.AccessToken,
		RefreshToken: cfg.RefreshToken,
		ExpiresAt:    cfg.ExpiresAt,
		Office:       cfg.Office,
		UserID:       cfg.UserID,
		Nickname:     cfg.Nickname,
		Phone:        cfg.Phone,
		DeviceID:     cfg.DeviceID,
	}, originConfig)

	// Environment fallback: only consulted when the config named nothing, so
	// a config-pinned credential always wins.
	if len(out) == 0 {
		add(accountConfig{
			AccessToken:  os.Getenv(envAccessToken),
			RefreshToken: os.Getenv(envRefreshToken),
			UserID:       os.Getenv(envUserID),
			Nickname:     os.Getenv(envNickname),
			Phone:        os.Getenv(envPhone),
			DeviceID:     os.Getenv(envDeviceID),
			Office:       os.Getenv(envOffice),
		}, originEnv)
	}
	return out
}
