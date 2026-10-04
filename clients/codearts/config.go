package codearts

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// config.go holds this module's configuration schema and the credential
// sources it will look at.
//
// The module is usable with no configuration at all: credentials discovered
// from the environment or from its own store in Deps.DataDir are enough.  Every
// field here is an override for an operator who wants to pin something.
//
// See clients/codearts/README.md for the documented schema and
// clients/codearts/config.example.json for a copy-paste starting point.

// Files this module owns inside Deps.DataDir.  Nothing is ever written outside
// that directory.
const (
	accountsFile = "accounts.json"       // credentials (the login flow writes here)
	stateFile    = "state.json"          // account health: cooldowns, last errors
	loginFile    = "login.json"          // a login flow in progress
	modelsFile   = "models.json"         // the last model catalogue seen upstream
	benefitFile  = "benefit_models.json" // the ids that bill against free quota
)

// Environment overrides.  These exist so a credential can be handed to the
// process without putting it in a config file; the config file still wins.
const (
	envAccessKeyID     = "CLIENT2API_CODEARTS_AK"
	envSecretAccessKey = "CLIENT2API_CODEARTS_SK"
	envSecurityToken   = "CLIENT2API_CODEARTS_SECURITY_TOKEN"
	envRefreshToken    = "CLIENT2API_CODEARTS_REFRESH_TOKEN"
	envExpiresAt       = "CLIENT2API_CODEARTS_EXPIRES_AT"
	envBaseURL         = "CLIENT2API_CODEARTS_BASE_URL"
	envLogin           = "CLIENT2API_CODEARTS_LOGIN"
	// These two mirror the reference implementation's own environment names.
	// They are read first, then the CLIENT2API_* names above, so an operator
	// migrating from that project does not have to rename anything.
	envLegacyCacheDir       = "DSH_CODEARTS_CACHE_DIR"
	envLegacyFirstTokenWait = "DSH_CODEARTS_SSE_FIRST_TOKEN_TIMEOUT_MS"
	envLegacyChunkWait      = "DSH_CODEARTS_SSE_CHUNK_TIMEOUT_MS"
)

// Defaults.  The cooldown numbers are this module's policy (see README.md,
// "Account pool"): 15 s for a transient failure, 60 s for anything else, and a
// flat 24 h when the account is out of quota.
const (
	defaultMaxAttempts    = 3
	defaultModelsTTL      = 2 * time.Hour // matches the reference's 2 h catalogue refresh
	defaultModelsTimeout  = 20 * time.Second
	defaultChatTimeout    = 30 * time.Minute // covers the reference's 30 min queue wait
	defaultIdleTimeout    = 2 * time.Minute
	defaultFirstTokenWait = 300 * time.Second // reference default
	defaultChunkWait      = 600 * time.Second // reference default
	defaultCooldown       = 60 * time.Second
	defaultShortCooldown  = 15 * time.Second
	defaultQuotaCooldown  = 24 * time.Hour
	defaultRefreshMargin  = 30 * time.Minute
	defaultLoginTimeout   = 10 * time.Minute
	defaultMaxTokens      = 65536 // the reference's proven working cap; 131072 yields an empty stream
	maxOutputTokens       = 65536
	defaultContextLength  = 1000000
	defaultStoreFlush     = 5 * time.Second
	defaultQueuePollEvery = 10 * time.Second
	defaultQueueMaxWait   = 30 * time.Minute
)

// accountConfig is one credential as written by hand in the module config.
//
// CodeArts has two shapes of credential.  The long-lived one is the OAuth
// result: access_key_id / secret_access_key / security_token plus the
// refresh_token that renews it.  The shorthand one is a plain AK/SK pair with
// no security token, which only works against the plain SDK-HMAC-SHA256
// endpoints (the model catalogue) and NOT against the chat API, which wants a
// security token in the signature.
type accountConfig struct {
	Label           string `json:"label"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	RefreshToken    string `json:"refresh_token"`
	ExpiresAt       string `json:"expires_at"`
	DomainID        string `json:"domain_id"`
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	Disabled        bool   `json:"disabled"`
}

// config is the JSON object under "clients"."codearts".
type config struct {
	// Endpoint overrides.  Empty means the built-in vendor defaults.
	BaseURL   string `json:"base_url"`   // snap-access origin
	ChatURL   string `json:"chat_url"`   // chat completions base
	ModelsURL string `json:"models_url"` // model catalogue endpoint
	// STSURL is the IAM token endpoint the OAuth grants are posted to, and
	// GatewayURL is where the free-quota model ids are read from.  Both are
	// overridable so a test (or an operator behind a mirror) never has to
	// reach the real host.
	STSURL     string `json:"sts_url"`
	GatewayURL string `json:"gateway_url"`
	TicketURL  string `json:"ticket_url"`
	UserAgent  string `json:"user_agent"`

	// Credentials.  Either a list, or the single-account shorthand below.
	Accounts        []accountConfig `json:"accounts"`
	AccessKeyID     string          `json:"access_key_id"`
	SecretAccessKey string          `json:"secret_access_key"`
	SecurityToken   string          `json:"security_token"`
	RefreshToken    string          `json:"refresh_token"`
	ExpiresAt       string          `json:"expires_at"`
	DomainID        string          `json:"domain_id"`
	UserID          string          `json:"user_id"`
	UserName        string          `json:"user_name"`

	// Tuning.  All optional; see the constants above for the defaults.  Every
	// duration also accepts a bare number of seconds.
	MaxAttempts    *int          `json:"max_attempts"`
	ModelsTTL      durationField `json:"models_ttl"`
	ModelsTimeout  durationField `json:"models_timeout"`
	ChatTimeout    durationField `json:"chat_timeout"`
	IdleTimeout    durationField `json:"idle_timeout"`
	FirstTokenWait durationField `json:"first_token_timeout"`
	ChunkWait      durationField `json:"chunk_timeout"`
	Cooldown       durationField `json:"cooldown"`
	ShortCooldown  durationField `json:"short_cooldown"`
	QuotaCooldown  durationField `json:"quota_cooldown"`
	RefreshMargin  durationField `json:"refresh_margin"`
	LoginTimeout   durationField `json:"login_timeout"`
	QueuePollEvery durationField `json:"queue_poll_interval"`
	QueueMaxWait   durationField `json:"queue_max_wait"`
	MaxTokens      *int          `json:"max_tokens"`

	// ReasoningEffort is "off" to send thinking:{type:"disabled"}, which is what
	// the reference does for models that would otherwise spend the whole budget
	// on a hidden chain of thought.  Empty means the vendor default.
	ReasoningEffort string `json:"reasoning_effort"`

	// ModelList is the fallback catalogue used before the first successful
	// refresh and whenever the catalogue endpoint is unreachable.  Empty keeps
	// the built-in list.
	ModelList []string `json:"models"`

	// Login=true (or CLIENT2API_CODEARTS_LOGIN=1) makes the module prepare an
	// authorisation URL at construction time and log it, instead of waiting for
	// a credential that cannot appear on its own.
	Login bool `json:"login"`
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
		return fmt.Errorf("codearts: %s is neither a duration string nor a number of seconds", s)
	}
	*d = durationField(strconv.FormatFloat(n, 'f', -1, 64))
	return nil
}

// ---------------------------------------------------------------------------
// accessors: every one of them answers with the default when the field is unset
// or unparseable, so a half-written config can never wedge the module
// ---------------------------------------------------------------------------

// baseURL is the snap-access origin, verbatim apart from surrounding
// whitespace.  endpoint() is what removes a trailing slash when a path is
// joined.
func (c config) baseURL() string {
	if v := strings.TrimSpace(c.BaseURL); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(envBaseURL)); v != "" {
		return v
	}
	return defaultBaseURL
}

// endpoint joins one of this module's fixed paths onto the configured origin
// without ever producing a double slash.
func (c config) endpoint(p string) string {
	return strings.TrimRight(c.baseURL(), "/") + p
}

// chatURL is the base of the chat completions API.
func (c config) chatURL() string {
	if v := strings.TrimSpace(c.ChatURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	return c.endpoint(chatAPIPath)
}

// modelsURL is the built-in model catalogue endpoint.
func (c config) modelsURL() string {
	if v := strings.TrimSpace(c.ModelsURL); v != "" {
		return strings.TrimRight(v, "/")
	}
	return c.endpoint(builtinModelsPath)
}

// gatewayConfigURL is where the free-quota ("benefit") model ids come from.
func (c config) gatewayConfigURL() string {
	if v := strings.TrimSpace(c.GatewayURL); v != "" {
		return v
	}
	return defaultGatewayConfigURL
}

// stsURL is the IAM token endpoint.
func (c config) stsURL() string {
	if v := strings.TrimSpace(c.STSURL); v != "" {
		return v
	}
	return stsTokenEndpoint
}

// ticketURL is the legacy login ticket endpoint.
func (c config) ticketURL() string {
	if v := strings.TrimSpace(c.TicketURL); v != "" {
		return v
	}
	return c.endpoint(ticketPath)
}

// queueStatusURL is the concurrency-queue endpoint a queued chat request is
// polled against.
func (c config) queueStatusURL() string {
	return c.endpoint(queueStatusPath)
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

// idleTimeout reports the configured `idle_timeout`, or the reference value
// when it is unset.  It is the raw field, not what the stream watchdogs use:
// firstTokenWait and chunkWait consult the field directly so that an unset
// `idle_timeout` leaves their own (different) defaults in force.  Nothing in
// the request path reads this accessor.
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
func (c config) loginTimeout() time.Duration {
	return durationOr(string(c.LoginTimeout), defaultLoginTimeout)
}
func (c config) queuePollEvery() time.Duration {
	return durationOr(string(c.QueuePollEvery), defaultQueuePollEvery)
}
func (c config) queueMaxWait() time.Duration {
	return durationOr(string(c.QueueMaxWait), defaultQueueMaxWait)
}

// firstTokenWait is how long the stream may stay silent before its first
// byte.  The DSH_CODEARTS_* name from the reference implementation wins when it
// is set, so a machine already tuned for that project keeps its numbers.
//
// `idle_timeout` is the coarse knob: it is consulted only when the specific
// first_token_timeout is unset, and it drives both watchdogs.  The two
// defaults are not the same number, so `idle_timeout` deliberately has no
// effect until an operator sets it — replacing a 300 s first-token budget with
// a shorter generic one would break exactly the queued requests the long
// budget exists for.
func (c config) firstTokenWait() time.Duration {
	if d := msFromEnv(envLegacyFirstTokenWait); d > 0 {
		return d
	}
	if d := durationOr(string(c.FirstTokenWait), 0); d > 0 {
		return d
	}
	if d := durationOr(string(c.IdleTimeout), 0); d > 0 {
		return d
	}
	return defaultFirstTokenWait
}

// chunkWait is how long the stream may stay silent between two bytes.
func (c config) chunkWait() time.Duration {
	if d := msFromEnv(envLegacyChunkWait); d > 0 {
		return d
	}
	if d := durationOr(string(c.ChunkWait), 0); d > 0 {
		return d
	}
	if d := durationOr(string(c.IdleTimeout), 0); d > 0 {
		return d
	}
	return defaultChunkWait
}

func (c config) maxTokens() int {
	if c.MaxTokens != nil && *c.MaxTokens > 0 {
		return *c.MaxTokens
	}
	return defaultMaxTokens
}

// reasoningOff reports whether the request should pin thinking to disabled.
func (c config) reasoningOff() bool {
	switch strings.ToLower(strings.TrimSpace(c.ReasoningEffort)) {
	case "off", "none", "disabled", "false":
		return true
	default:
		return false
	}
}

// fallbackModels is the catalogue to show before the first successful refresh.
func (c config) fallbackModels() []string {
	var out []string
	seen := make(map[string]bool)
	for _, m := range c.ModelList {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return append([]string(nil), builtinModels...)
	}
	return out
}

// loginRequested reports whether the module should prepare a login URL at
// construction time.
func (c config) loginRequested() bool {
	if c.Login {
		return true
	}
	return truthy(os.Getenv(envLogin))
}

// cacheDir is where the reference implementation kept its own catalogue cache.
// This module keeps its state inside Deps.DataDir instead, and only reads this
// value to stay compatible with an operator who set it for that project.
func (c config) cacheDir() string {
	return strings.TrimSpace(os.Getenv(envLegacyCacheDir))
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// msFromEnv reads a duration given in milliseconds, which is the unit the
// reference implementation's timeout variables use.  A non-positive or
// unparseable value means "not set".
func msFromEnv(name string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
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
		if n <= 0 {
			// Zero and negative both mean "unknown".  A negative value is a
			// corrupt field, not a credential that expired before the epoch.
			return 0
		}
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
			AccessKeyID:     strings.TrimSpace(a.AccessKeyID),
			SecretAccessKey: strings.TrimSpace(a.SecretAccessKey),
			SecurityToken:   strings.TrimSpace(a.SecurityToken),
			RefreshToken:    strings.TrimSpace(a.RefreshToken),
			ExpiresAt:       parseExpiry(a.ExpiresAt),
			DomainID:        strings.TrimSpace(a.DomainID),
			UserID:          strings.TrimSpace(a.UserID),
			UserName:        strings.TrimSpace(a.UserName),
			Disabled:        a.Disabled,
			Note:            strings.TrimSpace(a.Label),
		})
	}
	if ak := strings.TrimSpace(cfg.AccessKeyID); ak != "" {
		out = append(out, account{
			AccessKeyID:     ak,
			SecretAccessKey: strings.TrimSpace(cfg.SecretAccessKey),
			SecurityToken:   strings.TrimSpace(cfg.SecurityToken),
			RefreshToken:    strings.TrimSpace(cfg.RefreshToken),
			ExpiresAt:       parseExpiry(cfg.ExpiresAt),
			DomainID:        strings.TrimSpace(cfg.DomainID),
			UserID:          strings.TrimSpace(cfg.UserID),
			UserName:        strings.TrimSpace(cfg.UserName),
		})
	}
	if ak := strings.TrimSpace(os.Getenv(envAccessKeyID)); ak != "" {
		out = append(out, account{
			AccessKeyID:     ak,
			SecretAccessKey: strings.TrimSpace(os.Getenv(envSecretAccessKey)),
			SecurityToken:   strings.TrimSpace(os.Getenv(envSecurityToken)),
			RefreshToken:    strings.TrimSpace(os.Getenv(envRefreshToken)),
			ExpiresAt:       parseExpiry(os.Getenv(envExpiresAt)),
		})
	}
	kept := out[:0]
	for _, a := range out {
		if a.AccessKeyID != "" && a.SecretAccessKey != "" {
			kept = append(kept, a)
		}
	}
	return kept
}
