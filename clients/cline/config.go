package cline

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// Product constants.  Everything here was measured against the official client;
// see README.md for the provenance of each value.
const (
	// defaultAPIBase serves inference and the account endpoints.
	defaultAPIBase = "https://api.cline.bot"
	// defaultAppBase is the human-facing console; no request is ever sent to it,
	// but it is where the operator opens the browser.
	defaultAppBase = "https://app.cline.bot"
	// defaultWorkOSBase serves the device-authorisation flow.
	defaultWorkOSBase = "https://api.workos.com"

	// defaultWorkOSClientID is the official client's public OAuth client id.
	defaultWorkOSClientID = "client_01K3A541FN8TA3EPPHTD2325AR"

	// tokenPrefix is prepended to every WorkOS access token before it is sent.
	//
	// This is a load-bearing detail, not cosmetics: `Bearer workos:eyJ…` is
	// accepted by GET /api/v1/users/me while `Bearer eyJ…` is rejected with 401
	// and a body that blames an outdated Cline build.  The prefix is stripped
	// only when a JWT payload is decoded, never in header construction.
	tokenPrefix = "workos:"

	// clientTypeHeader / referer / title identify the official client on every
	// request, inference and account alike.
	headerReferer    = "https://cline.bot"
	headerTitle      = "Cline"
	headerClientType = "cline-sdk"
	headerMultiRoot  = "false"
)

// File names inside Deps.DataDir.
const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
	loginFile    = "login.json"
)

// Environment variables.  A credential can be handed to the process instead of
// being written to a config file.
const (
	envAccessToken  = "CLIENT2API_CLINE_TOKEN"
	envRefreshToken = "CLIENT2API_CLINE_REFRESH_TOKEN"
	envAccountID    = "CLIENT2API_CLINE_ACCOUNT_ID"
	envEmail        = "CLIENT2API_CLINE_EMAIL"
	envNickname     = "CLIENT2API_CLINE_NICKNAME"
	envLogin        = "CLIENT2API_CLINE_LOGIN"
)

// Defaults.  Every one of these is a documented, overridable value.
const (
	defaultMaxAttempts    = 3
	defaultModelsTTL      = 5 * time.Minute
	defaultModelsTimeout  = 20 * time.Second
	defaultChatTimeout    = 10 * time.Minute
	defaultIdleTimeout    = 2 * time.Minute
	defaultCooldown       = 60 * time.Second
	defaultShortCooldown  = 15 * time.Second
	defaultQuotaCooldown  = 24 * time.Hour
	defaultRefreshMargin  = 30 * time.Minute
	defaultMaxTokens      = 32000
	defaultStoreFlush     = 5 * time.Second
	defaultPollInterval   = 5 * time.Second
	defaultLoginTimeout   = 10 * time.Minute
	defaultMaxConcurrency = 4

	// defaultContextLength is the context window advertised by the fallback
	// catalogue; the live catalogue carries a per-model value.
	defaultContextLength = 200000
)

// durationField accepts either a Go duration string ("90s", "2h") or a bare
// JSON number, which is read as seconds.  Anything else is treated as unset, so
// a nonsense value falls back to the default instead of failing the module.
type durationField time.Duration

// UnmarshalJSON implements json.Unmarshaler.
func (d *durationField) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return err
		}
		if v, ok := parseDuration(str); ok {
			*d = durationField(v)
		}
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		*d = durationField(time.Duration(n * float64(time.Second)))
	}
	return nil
}

// parseDuration reads a Go duration string, or a bare number of seconds.
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if v, err := time.ParseDuration(s); err == nil {
		if v <= 0 {
			return 0, false
		}
		return v, true
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Second)), true
	}
	return 0, false
}

// accountConfig is one explicit credential.
type accountConfig struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Email        string `json:"email"`
	Nickname     string `json:"nickname"`
	ExpiresAt    string `json:"expires_at"`
	Disabled     bool   `json:"disabled"`
}

// config is the JSON object under `clients.cline`.  Every key is optional.
type config struct {
	APIBase      string          `json:"api_base"`
	AppBase      string          `json:"app_base"`
	WorkOSBase   string          `json:"workos_base"`
	WorkOSID     string          `json:"workos_client_id"`
	Accounts     []accountConfig `json:"accounts"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	AccountID    string          `json:"account_id"`
	Email        string          `json:"email"`
	Nickname     string          `json:"nickname"`
	ExpiresAt    string          `json:"expires_at"`

	MaxAttempts   *int          `json:"max_attempts"`
	ModelsTTL     durationField `json:"models_ttl"`
	ModelsTimeout durationField `json:"models_timeout"`
	ChatTimeout   durationField `json:"chat_timeout"`
	IdleTimeout   durationField `json:"idle_timeout"`
	Cooldown      durationField `json:"cooldown"`
	ShortCooldown durationField `json:"short_cooldown"`
	QuotaCooldown durationField `json:"quota_cooldown"`
	RefreshMargin durationField `json:"refresh_margin"`

	MaxTokens       *int          `json:"max_tokens"`
	MaxConcurrency  *int          `json:"max_concurrency"`
	ReasoningEffort string        `json:"reasoning_effort"`
	Login           bool          `json:"login"`
	PollInterval    durationField `json:"poll_interval"`
	LoginTimeout    durationField `json:"login_timeout"`

	// TLSProfile/TLSProtocol impersonate the official client's TLS
	// ClientHello.  The module drives a signed-in consumer account, so a
	// Go-shaped handshake is a detectable tell.  Empty keeps the stock
	// handshake, which is the historical behaviour.
	TLSProfile  string `json:"tls_profile"`
	TLSProtocol string `json:"tls_protocol"`
}

// parseConfig decodes the module's slice of the host config.  An absent object
// is the same as {}; a malformed one is returned as an error so the caller can
// log it and carry on with defaults.
func parseConfig(raw json.RawMessage) (config, error) {
	var cfg config
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// --- accessors: every one is default-safe ---------------------------------

func (c config) apiBase() string    { return firstNonEmpty(c.APIBase, defaultAPIBase) }
func (c config) appBase() string    { return firstNonEmpty(c.AppBase, defaultAppBase) }
func (c config) workOSBase() string { return firstNonEmpty(c.WorkOSBase, defaultWorkOSBase) }
func (c config) workOSClientID() string {
	return firstNonEmpty(c.WorkOSID, defaultWorkOSClientID)
}

// endpoint joins a path onto the API origin without doubling the separator.
func (c config) endpoint(p string) string {
	return strings.TrimRight(c.apiBase(), "/") + p
}

// workOSEndpoint joins a path onto the WorkOS origin.
func (c config) workOSEndpoint(p string) string {
	return strings.TrimRight(c.workOSBase(), "/") + p
}

// durOr returns the configured duration, or def when unset.
func durOr(d durationField, def time.Duration) time.Duration {
	if v := time.Duration(d); v > 0 {
		return v
	}
	return def
}

func (c config) maxAttempts() int {
	if c.MaxAttempts != nil && *c.MaxAttempts > 0 {
		return *c.MaxAttempts
	}
	return defaultMaxAttempts
}

func (c config) modelsTTL() time.Duration     { return durOr(c.ModelsTTL, defaultModelsTTL) }
func (c config) modelsTimeout() time.Duration { return durOr(c.ModelsTimeout, defaultModelsTimeout) }
func (c config) chatTimeout() time.Duration   { return durOr(c.ChatTimeout, defaultChatTimeout) }
func (c config) idleTimeout() time.Duration   { return durOr(c.IdleTimeout, defaultIdleTimeout) }
func (c config) cooldown() time.Duration      { return durOr(c.Cooldown, defaultCooldown) }
func (c config) shortCooldown() time.Duration { return durOr(c.ShortCooldown, defaultShortCooldown) }
func (c config) quotaCooldown() time.Duration { return durOr(c.QuotaCooldown, defaultQuotaCooldown) }
func (c config) refreshMargin() time.Duration { return durOr(c.RefreshMargin, defaultRefreshMargin) }
func (c config) pollInterval() time.Duration  { return durOr(c.PollInterval, defaultPollInterval) }
func (c config) loginTimeout() time.Duration  { return durOr(c.LoginTimeout, defaultLoginTimeout) }

// tlsProfile / tlsProtocol expose the transport-fingerprint keys to New.
func (c config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

func (c config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

func (c config) maxTokens() int {
	if c.MaxTokens != nil && *c.MaxTokens > 0 {
		return *c.MaxTokens
	}
	return defaultMaxTokens
}

func (c config) maxConcurrency() int {
	if c.MaxConcurrency != nil && *c.MaxConcurrency > 0 {
		return *c.MaxConcurrency
	}
	return defaultMaxConcurrency
}

// reasoningEffort is the configured default tier, or "" when unset.  It is not
// validated here: the upstream silently ignores an unknown tier, and echoing
// the operator's value back in Status is more useful than silently rewriting it.
func (c config) reasoningEffort() string { return strings.TrimSpace(c.ReasoningEffort) }

// loginRequested reports whether the device flow should start at construction.
func (c config) loginRequested() bool {
	return c.Login || truthy(os.Getenv(envLogin))
}

// configuredAccounts is the credential roster, in precedence order: the
// accounts[] list, then the single-account shorthand, then the environment.
// Entries without an access token are dropped.
func configuredAccounts(cfg config) []account {
	var out []account
	for _, a := range cfg.Accounts {
		if strings.TrimSpace(a.AccessToken) == "" {
			continue
		}
		out = append(out, account{
			ID:           a.ID,
			Label:        a.Label,
			AccessToken:  normalizeToken(a.AccessToken),
			RefreshToken: strings.TrimSpace(a.RefreshToken),
			AccountID:    strings.TrimSpace(a.AccountID),
			Email:        strings.TrimSpace(a.Email),
			Nickname:     strings.TrimSpace(a.Nickname),
			ExpiresAt:    parseExpiry(a.ExpiresAt),
			Disabled:     a.Disabled,
		})
	}
	shorthand := account{
		ID:           strings.TrimSpace(os.Getenv("CLIENT2API_CLINE_ID")),
		AccessToken:  normalizeToken(firstNonEmpty(cfg.AccessToken, os.Getenv(envAccessToken))),
		RefreshToken: strings.TrimSpace(firstNonEmpty(cfg.RefreshToken, os.Getenv(envRefreshToken))),
		AccountID:    strings.TrimSpace(firstNonEmpty(cfg.AccountID, os.Getenv(envAccountID))),
		Email:        strings.TrimSpace(firstNonEmpty(cfg.Email, os.Getenv(envEmail))),
		Nickname:     strings.TrimSpace(firstNonEmpty(cfg.Nickname, os.Getenv(envNickname))),
		ExpiresAt:    parseExpiry(cfg.ExpiresAt),
	}
	if shorthand.AccessToken != "" {
		out = append(out, shorthand)
	}
	return out
}

// normalizeToken makes sure an access token carries the mandatory workos:
// prefix.  A value that already has it is returned unchanged; one that does not
// (a hand-pasted JWT, or a fixture) gets it, because the header without it is
// rejected with a misleading 401.
func normalizeToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return ""
	}
	if strings.HasPrefix(tok, tokenPrefix) {
		return tok
	}
	return tokenPrefix + tok
}

// bareToken strips the workos: prefix.  It is used ONLY when decoding a JWT
// payload -- never when building an Authorization header.
func bareToken(tok string) string {
	return strings.TrimPrefix(strings.TrimSpace(tok), tokenPrefix)
}

// parseExpiry reads an expiry into unix seconds.  0 means "unknown", which is
// treated conservatively as "not expired" everywhere.
//
// Accepted: unix seconds, unix milliseconds (>1e12), RFC3339(Nano), and
// "2006-01-02 15:04:05".  ISO 8601 with fractional seconds and a Z suffix --
// the shape the vendor actually returns -- is RFC3339Nano.
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

// truthy reads the boolean-ish environment spellings the other modules accept.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "y":
		return true
	}
	return false
}

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
