package qwenwork

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// config.go holds this module's configuration schema and the credential
// sources it will look at.
//
// The module is usable with no configuration at all: credentials discovered
// from the environment or from its own store in Deps.DataDir are enough.  Every
// field here is an override for an operator who wants to pin something.
//
// See clients/qwenwork/README.md for the documented schema and
// clients/qwenwork/config.example.json for a copy-paste starting point.

// Files this module owns inside Deps.DataDir.  Nothing is ever written outside
// that directory.
const (
	accountsFile = "accounts.json" // credentials (the device flow writes here)
	stateFile    = "state.json"    // account health: cooldowns, last errors
	loginFile    = "login.json"    // a device-authorisation flow in progress
)

// Environment overrides.  These exist so a credential can be handed to the
// process without putting it in a config file; the config file still wins.
const (
	envAccessToken  = "CLIENT2API_QWENWORK_TOKEN"
	envRefreshToken = "CLIENT2API_QWENWORK_REFRESH_TOKEN"
	envUID          = "CLIENT2API_QWENWORK_UID"
	envNickname     = "CLIENT2API_QWENWORK_NICKNAME"
	envEmail        = "CLIENT2API_QWENWORK_EMAIL"
	envLogin        = "CLIENT2API_QWENWORK_LOGIN"
)

// Defaults.  The cooldown numbers are the reference policy (see README.md,
// "Account pool"): 15 s for a transient failure, 60 s for anything else, and a
// flat 24 h when the account is out of credit.
const (
	defaultMaxAttempts   = 3
	defaultModelsTTL     = 5 * time.Minute
	defaultModelsTimeout = 20 * time.Second
	defaultChatTimeout   = 10 * time.Minute
	defaultIdleTimeout   = 2 * time.Minute
	defaultCooldown      = 60 * time.Second
	defaultShortCooldown = 15 * time.Second
	defaultQuotaCooldown = 24 * time.Hour
	defaultRefreshMargin = 30 * time.Minute
	defaultPollInterval  = 3 * time.Second
	defaultLoginTimeout  = 10 * time.Minute
	defaultMaxTokens     = 32000
	maxOutputTokens      = 32768
	defaultContextLength = 180000
	defaultStoreFlush    = 5 * time.Second
)

// accountConfig is one credential as written by hand in the module config.
type accountConfig struct {
	Label        string `json:"label"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Email        string `json:"email"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	Disabled     bool   `json:"disabled"`
}

// config is the JSON object under "clients"."qwenwork".
type config struct {
	// Endpoint overrides.  Empty means the built-in vendor defaults.
	BaseURL   string `json:"base_url"`
	UserAgent string `json:"user_agent"`

	// Credentials.  Either a list, or the single-account shorthand below.
	Accounts     []accountConfig `json:"accounts"`
	AccessToken  string          `json:"access_token"`
	UID          string          `json:"uid"`
	Nickname     string          `json:"nickname"`
	Email        string          `json:"email"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    string          `json:"expires_at"`

	// Tuning.  All optional; see the constants above for the defaults.  Every
	// duration also accepts a bare number of seconds.
	MaxAttempts   *int          `json:"max_attempts"`
	ModelsTTL     durationField `json:"models_ttl"`
	ModelsTimeout durationField `json:"models_timeout"`
	ChatTimeout   durationField `json:"chat_timeout"`
	IdleTimeout   durationField `json:"idle_timeout"`
	Cooldown      durationField `json:"cooldown"`
	ShortCooldown durationField `json:"short_cooldown"`
	QuotaCooldown durationField `json:"quota_cooldown"`
	RefreshMargin durationField `json:"refresh_margin"`
	MaxTokens     *int          `json:"max_tokens"`

	// --- transport fingerprint ---------------------------------------------
	// TLSProfile selects the TLS ClientHello to imitate, so the handshake
	// carries the same cipher suites, extensions, GREASE values and ALPN list
	// as the browser the vendor expects instead of Go's crypto/tls.  Empty
	// keeps Go's handshake, which is why the default changes nothing.
	//
	// Only the TLS half is closed: the h2 frames are still written by
	// golang.org/x/net/http2.  See internal/fingerprint's package comment.
	TLSProfile string `json:"tls_profile"`
	// TLSProtocol is "h2" to require HTTP/2, "http/1.1" to offer http/1.1
	// alone, and empty to offer the profile's own ALPN list (h2 then
	// http/1.1) and downgrade when the server refuses h2.
	TLSProtocol string `json:"tls_protocol"`

	// Device authorisation.  Login=true (or CLIENT2API_QWENWORK_LOGIN=1) makes
	// the module start the PKCE flow at construction time and log the URL to
	// open, instead of waiting for a credential that cannot appear on its own.
	Login         bool          `json:"login"`
	ClientID      string        `json:"client_id"`
	RedirectURI   string        `json:"redirect_uri"`
	DeviceAuthURL string        `json:"device_auth_url"`
	PollInterval  durationField `json:"poll_interval"`
	LoginTimeout  durationField `json:"login_timeout"`
}

// durationField is a duration in a config file.  It accepts both a Go duration
// string ("90s", "2h") and a bare JSON number, which is read as seconds: an
// operator writing "cooldown": 300 means the same as "300" or "5m".
type durationField string

func (d *durationField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	switch {
	case s == "" || s == "null":
		return nil
	case s[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*d = durationField(v)
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n, ok := v.(float64)
	if !ok {
		return fmt.Errorf("qwenwork: %s is neither a duration string nor a number of seconds", s)
	}
	*d = durationField(strconv.FormatFloat(n, 'f', -1, 64))
	return nil
}

// ---------------------------------------------------------------------------
// accessors: every one of them answers with the default when the field is unset
// or unparseable, so a half-written config can never wedge the module
// ---------------------------------------------------------------------------

// tlsProfile is the ClientHello to imitate.  Empty means "look like Go", which
// is what every version before this key did.
func (c config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

// tlsProtocol is the application protocol to negotiate.  Empty offers the
// profile's own ALPN list and downgrades when the server refuses h2.
func (c config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

// baseURL is the configured origin, verbatim apart from surrounding
// whitespace: an operator who writes a trailing slash sees it echoed back by
// the configuration, and endpoint() is what removes it when a path is joined.
func (c config) baseURL() string {
	if v := strings.TrimSpace(c.BaseURL); v != "" {
		return v
	}
	return defaultBaseURL
}

// endpoint joins one of this module's fixed paths onto the configured origin
// without ever producing a double slash.
func (c config) endpoint(p string) string {
	return strings.TrimRight(c.baseURL(), "/") + p
}

func (c config) userAgent() string {
	if v := strings.TrimSpace(c.UserAgent); v != "" {
		return v
	}
	return defaultUserAgent
}

func (c config) maxAttempts() int {
	if c.MaxAttempts != nil && *c.MaxAttempts > 0 {
		return *c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c config) modelsTTL() time.Duration {
	return durationOr(string(c.ModelsTTL), defaultModelsTTL)
}
func (c config) modelsTimeout() time.Duration {
	return durationOr(string(c.ModelsTimeout), defaultModelsTimeout)
}
func (c config) chatTimeout() time.Duration {
	return durationOr(string(c.ChatTimeout), defaultChatTimeout)
}
func (c config) idleTimeout() time.Duration {
	return durationOr(string(c.IdleTimeout), defaultIdleTimeout)
}
func (c config) cooldown() time.Duration {
	return durationOr(string(c.Cooldown), defaultCooldown)
}
func (c config) shortCooldown() time.Duration {
	return durationOr(string(c.ShortCooldown), defaultShortCooldown)
}
func (c config) quotaCooldown() time.Duration {
	return durationOr(string(c.QuotaCooldown), defaultQuotaCooldown)
}
func (c config) refreshMargin() time.Duration {
	return durationOr(string(c.RefreshMargin), defaultRefreshMargin)
}
func (c config) pollInterval() time.Duration {
	return durationOr(string(c.PollInterval), defaultPollInterval)
}
func (c config) loginTimeout() time.Duration {
	return durationOr(string(c.LoginTimeout), defaultLoginTimeout)
}

func (c config) maxTokens() int {
	if c.MaxTokens != nil && *c.MaxTokens > 0 {
		return *c.MaxTokens
	}
	return defaultMaxTokens
}

func (c config) clientID() string {
	if v := strings.TrimSpace(c.ClientID); v != "" {
		return v
	}
	return defaultClientID
}

func (c config) redirectURI() string {
	if v := strings.TrimSpace(c.RedirectURI); v != "" {
		return v
	}
	return defaultRedirectURI
}

func (c config) deviceAuthURL() string {
	if v := strings.TrimSpace(c.DeviceAuthURL); v != "" {
		return v
	}
	return c.endpoint(deviceAuthPath)
}

func (c config) loginRequested() bool {
	if c.Login {
		return true
	}
	return truthy(os.Getenv(envLogin))
}

// durationOr parses a Go duration string, falling back to def.  A bare number
// is read as seconds, because "cooldown": 300 is what an operator will write.
func durationOr(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return def
		}
		return d
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// truthy reports whether an environment variable asks for something.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

// parseExpiry accepts RFC 3339, "2006-01-02 15:04:05" or a unix timestamp in
// seconds or milliseconds.  Zero means "unknown", never "expired".
func parseExpiry(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // milliseconds
			return n / 1000
		}
		return n
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// parseConfig decodes the raw client config.  A decode failure is reported to
// the caller so it can log it, but the caller carries on with defaults: a typo
// in the config must not take the module (or the process) down.
func parseConfig(raw json.RawMessage) (config, error) {
	var cfg config
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// configuredAccounts flattens the credentials the config and the environment
// supply.  Explicit config beats the environment, and the list form beats the
// single-account shorthand.  Order is preserved so the pool's LRU tie-break is
// deterministic.
func configuredAccounts(cfg config) []account {
	var out []account
	for _, a := range cfg.Accounts {
		out = append(out, account{
			UID:          strings.TrimSpace(a.UID),
			Nickname:     strings.TrimSpace(a.Nickname),
			Email:        strings.TrimSpace(a.Email),
			AccessToken:  strings.TrimSpace(a.AccessToken),
			RefreshToken: strings.TrimSpace(a.RefreshToken),
			ExpiresAt:    parseExpiry(a.ExpiresAt),
			Disabled:     a.Disabled,
			Note:         strings.TrimSpace(a.Label),
		})
	}
	if tok := strings.TrimSpace(cfg.AccessToken); tok != "" {
		out = append(out, account{
			UID:          strings.TrimSpace(cfg.UID),
			Nickname:     strings.TrimSpace(cfg.Nickname),
			Email:        strings.TrimSpace(cfg.Email),
			AccessToken:  tok,
			RefreshToken: strings.TrimSpace(cfg.RefreshToken),
			ExpiresAt:    parseExpiry(cfg.ExpiresAt),
		})
	}
	if tok := strings.TrimSpace(os.Getenv(envAccessToken)); tok != "" {
		out = append(out, account{
			UID:          strings.TrimSpace(os.Getenv(envUID)),
			Nickname:     strings.TrimSpace(os.Getenv(envNickname)),
			Email:        strings.TrimSpace(os.Getenv(envEmail)),
			AccessToken:  tok,
			RefreshToken: strings.TrimSpace(os.Getenv(envRefreshToken)),
		})
	}
	kept := out[:0]
	for _, a := range out {
		if a.AccessToken != "" {
			kept = append(kept, a)
		}
	}
	return kept
}
