// Package lobsterai implements the LobsterAI (有道龙虾, Youdao) upstream for the
// client2api gateway.
//
// LobsterAI is an Electron desktop agent whose backend is a thin OpenAI-shaped
// proxy in front of a mixed model catalogue (DeepSeek, Qwen, Kimi, Doubao, GLM,
// MiniMax).  Four things about it shape every file in this package:
//
//   - Credentials come from a browser hand-off.  There is no password grant:
//     the operator opens a portal URL, the portal redirects to a loopback
//     callback with a one-time authCode, and that code is exchanged for an
//     access/refresh pair.  The exchange also mints three fields the vendor
//     later demands back on every renewal (firstKeyfrom, latestKeyfrom, uuid),
//     so they are persisted with the tokens rather than recomputed.
//
//   - /api/proxy/v1/chat/completions is SSE-only.  A stream:false request makes
//     the upstream answer 500, so the bridge always asks for a stream and, when
//     the caller wanted one answer, re-aggregates the frames into a single
//     delta.  It is also the one endpoint that does NOT wrap its response in
//     the {code,msg,data} envelope every other endpoint uses.
//
//   - There is no image input.  A request that carries one is refused with
//     core.ErrUnsupported before anything touches the network.
//
//   - The vendor publishes no per-model output budget, so this module
//     deliberately does not implement core.ModelLimitsProvider.  See README.md.
package lobsterai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// Endpoint defaults.
//
// portal_url and base_url are two fields on purpose even though they resolve to
// the same host: the portal is the SPA the operator logs in through and the API
// base is the server the module talks to, and a deployment where one moves
// without the other must not need a code change.
const (
	defaultPortalURL  = "https://lobsterai.youdao.com"
	defaultBaseURL    = "https://lobsterai-server.youdao.com"
	defaultVersionURL = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"

	// loginPath is appended to portal_url.  The portal root already answers 200
	// and the login screen is a hash route under it, so the path is fixed.
	loginPath = "/portal#/login"

	// defaultCallbackPath is the loopback path the portal redirects to.  It is
	// the path the vendor's own desktop client uses.
	defaultCallbackPath = "/auth/callback"

	// defaultClientVersion is the fallback sent as the `version` field and as
	// the X-LobsterAI-Client-Version header when the version endpoint cannot be
	// reached.  The vendor tolerates a stale version; it does not tolerate a
	// missing one (check-in requires it as a query parameter).
	defaultClientVersion = "0.1.0"

	// defaultCapabilities is the capability token the vendor's own desktop
	// client sends.  Dropping it changes which models the backend will serve.
	defaultCapabilities = "kimi-k3-agentic-v1"

	// defaultUserAgent is the vendor's own desktop client's user agent.  The
	// "0.1.0" inside it is unrelated to the dynamic version header above and
	// must stay a constant.
	defaultUserAgent = "LobsterAI/0.1.0"

	// Check-in query parameters.  platform=win32 is sent on every OS: it is the
	// desktop client's disguise, not a statement about this machine.
	checkinPlacement           = "desktop_sidebar"
	checkinContainerAPIVersion = "2"
	checkinPlatform            = "win32"

	// clientName is the routing prefix and the name the panel shows.
	clientName = "lobsterai"

	// credentialsFile is the credential store, relative to Deps.DataDir.
	credentialsFile = "accounts.json"
)

// Timeout and policy defaults.
const (
	defaultLoginTimeout   = 10 * time.Minute
	defaultChatTimeout    = 10 * time.Minute
	defaultModelsTimeout  = 20 * time.Second
	defaultCheckinTimeout = 30 * time.Second
	defaultVersionTimeout = 10 * time.Second
	defaultModelsTTL      = time.Hour
	defaultVersionTTL     = 6 * time.Hour

	// defaultCooldown parks an account after a rate limit (or a 404) for a
	// minute; defaultQuotaCooldown parks it after an exhausted balance for
	// half a day, because a credit top-up is not something that fixes itself
	// within a request.
	defaultCooldown      = 60 * time.Second
	defaultQuotaCooldown = 12 * time.Hour

	// defaultRefreshMargin renews an access token this long before it expires,
	// so a request never races the expiry.
	defaultRefreshMargin = 30 * time.Minute

	// defaultMaxInFlight is the per-account concurrency ceiling.  The vendor's
	// risk control reacts to parallel streams from one account, so the default
	// is deliberately low.
	defaultMaxInFlight = 2
)

// versionPattern is the version spelling the vendor accepts: a dotted number
// with an optional pre-release suffix.  The live value is date-style
// ("2026.9.4"), which this pattern covers; a semver-looking "0.1.0" also
// covers it, which is what makes the fallback usable.
var versionPattern = regexp.MustCompile(`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`)

// AccountConfig is one credential, as it appears in the config file.
//
// It is embedded anonymously in Config so the same six fields can be written at
// the top level for a single-account deployment -- the spelling clients/tabbit
// and clients/qwenwork already use.
//
// first_keyfrom, latest_keyfrom and uuid are not optional decorations: renewal
// sends them back, and the vendor rejects a refresh that does not replay them.
// They are minted once at login and must be stored from then on.
type AccountConfig struct {
	Label         string `json:"label,omitempty"`
	UID           string `json:"uid,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	FirstKeyfrom  string `json:"first_keyfrom,omitempty"`
	LatestKeyfrom string `json:"latest_keyfrom,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

// Config is the clients.lobsterai config block.
//
// Every field is optional: a zero Config is a working configuration with the
// vendor's production endpoints, a fake fallback version and no credentials
// (which reports core.ErrNotConfigured until an account is added).
type Config struct {
	AccountConfig

	PortalURL     string `json:"portal_url,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	VersionURL    string `json:"version_url,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	Capabilities  string `json:"client_capabilities,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`

	// TLSProfile/TLSProtocol impersonate the desktop client's TLS
	// ClientHello.  The module drives a signed-in consumer account, so a
	// Go-shaped handshake is a detectable tell.  Empty keeps the stock
	// handshake.
	TLSProfile  string `json:"tls_profile,omitempty"`
	TLSProtocol string `json:"tls_protocol,omitempty"`

	// LoginPort is the loopback port the login callback listens on.  0 asks the
	// kernel for a free one, which is what the vendor's client does.
	LoginPort    int    `json:"login_port,omitempty"`
	CallbackPath string `json:"callback_path,omitempty"`

	// include_usage asks the upstream for a usage block on the SSE stream.  The
	// vendor's own client does not send stream_options, so this defaults to
	// false; a usage block that does arrive is parsed either way.
	IncludeUsage bool `json:"include_usage,omitempty"`

	// ExtraModels appends ids the dynamic catalogue may not list yet.
	ExtraModels []string `json:"extra_models,omitempty"`

	// ExtraHeaders are added to every upstream request.  A key that would
	// collide with an authentication header is ignored, so this cannot be used
	// to override the token.
	ExtraHeaders map[string]string `json:"extra_headers,omitempty"`

	// Duration knobs, as Go duration strings ("10m", "500ms").
	LoginTimeout   string `json:"login_timeout,omitempty"`
	ChatTimeout    string `json:"chat_timeout,omitempty"`
	ModelsTimeout  string `json:"models_timeout,omitempty"`
	CheckinTimeout string `json:"checkin_timeout,omitempty"`
	VersionTimeout string `json:"version_timeout,omitempty"`
	ModelsTTL      string `json:"models_ttl,omitempty"`
	VersionTTL     string `json:"version_ttl,omitempty"`
	Cooldown       string `json:"cooldown,omitempty"`
	QuotaCooldown  string `json:"quota_cooldown,omitempty"`
	RefreshMargin  string `json:"refresh_margin,omitempty"`

	MaxInFlight int `json:"max_in_flight,omitempty"`

	Accounts []AccountConfig `json:"accounts,omitempty"`
}

// parseConfig decodes the module's config block.  An absent block is not an
// error: it is a Config with defaults, which is what a deployment that only
// pastes a credential into the panel needs.
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
// returns an error: an unusable value falls back to the default and is reported
// by the caller, because a module that refuses to build cannot be repaired from
// the panel.
func (cfg Config) normalize() Config {
	out := cfg
	out.PortalURL = strings.TrimRight(strings.TrimSpace(out.PortalURL), "/")
	if out.PortalURL == "" {
		out.PortalURL = defaultPortalURL
	}
	out.BaseURL = strings.TrimRight(strings.TrimSpace(out.BaseURL), "/")
	if out.BaseURL == "" {
		out.BaseURL = defaultBaseURL
	}
	out.VersionURL = strings.TrimSpace(out.VersionURL)
	if out.VersionURL == "" {
		out.VersionURL = defaultVersionURL
	}
	out.ClientVersion = strings.TrimSpace(out.ClientVersion)
	if !versionPattern.MatchString(out.ClientVersion) {
		out.ClientVersion = defaultClientVersion
	}
	out.Capabilities = strings.TrimSpace(out.Capabilities)
	if out.Capabilities == "" {
		out.Capabilities = defaultCapabilities
	}
	out.UserAgent = strings.TrimSpace(out.UserAgent)
	if out.UserAgent == "" {
		out.UserAgent = defaultUserAgent
	}
	out.TLSProfile = strings.TrimSpace(out.TLSProfile)
	out.TLSProtocol = strings.TrimSpace(out.TLSProtocol)
	out.CallbackPath = strings.TrimSpace(out.CallbackPath)
	if out.CallbackPath == "" {
		out.CallbackPath = defaultCallbackPath
	}
	if !strings.HasPrefix(out.CallbackPath, "/") {
		out.CallbackPath = "/" + out.CallbackPath
	}
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

	out.ExtraModels = dedupeStrings(cfg.ExtraModels)
	return out
}

// normalizeAccount trims one credential and drops the ones with nothing in
// them, so an empty `accounts: [{}]` entry does not become an account.
func normalizeAccount(acct AccountConfig) AccountConfig {
	acct.Label = strings.TrimSpace(acct.Label)
	acct.UID = strings.TrimSpace(acct.UID)
	acct.UserID = strings.TrimSpace(acct.UserID)
	acct.UUID = strings.TrimSpace(acct.UUID)
	acct.AccessToken = strings.TrimSpace(acct.AccessToken)
	acct.RefreshToken = strings.TrimSpace(acct.RefreshToken)
	acct.ExpiresAt = strings.TrimSpace(acct.ExpiresAt)
	acct.FirstKeyfrom = strings.TrimSpace(acct.FirstKeyfrom)
	acct.LatestKeyfrom = strings.TrimSpace(acct.LatestKeyfrom)
	acct.Nickname = strings.TrimSpace(acct.Nickname)
	return acct
}

// configuredAccounts flattens the top-level credential and the accounts array
// into one list.
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
		"x-lobsterai-client-capabilities", "x-lobsterai-client-version":
		return true
	}
	return false
}

// --- duration accessors -----------------------------------------------------

// durationOr parses a Go duration string, falling back to def when it is empty
// or unparseable.  A negative value is treated as unset rather than as "no
// timeout", because a zero timeout silently disables the guard.
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

// tlsProfile / tlsProtocol expose the transport-fingerprint keys to New.
func (cfg Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(cfg.TLSProfile))
}

func (cfg Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(cfg.TLSProtocol))
}
func (cfg Config) modelsTimeout() time.Duration {
	return durationOr(cfg.ModelsTimeout, defaultModelsTimeout)
}
func (cfg Config) checkinTimeout() time.Duration {
	return durationOr(cfg.CheckinTimeout, defaultCheckinTimeout)
}
func (cfg Config) versionTimeout() time.Duration {
	return durationOr(cfg.VersionTimeout, defaultVersionTimeout)
}
func (cfg Config) modelsTTL() time.Duration  { return durationOr(cfg.ModelsTTL, defaultModelsTTL) }
func (cfg Config) versionTTL() time.Duration { return durationOr(cfg.VersionTTL, defaultVersionTTL) }
func (cfg Config) cooldown() time.Duration   { return durationOr(cfg.Cooldown, defaultCooldown) }
func (cfg Config) quotaCooldown() time.Duration {
	return durationOr(cfg.QuotaCooldown, defaultQuotaCooldown)
}
func (cfg Config) refreshMargin() time.Duration {
	return durationOr(cfg.RefreshMargin, defaultRefreshMargin)
}

func (cfg Config) maxInFlight() int {
	if cfg.MaxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return cfg.MaxInFlight
}

// --- URL builders -----------------------------------------------------------

func (cfg Config) chatURL() string { return cfg.BaseURL + "/api/proxy/v1/chat/completions" }

func (cfg Config) exchangeURL() string { return cfg.BaseURL + "/api/auth/exchange" }

func (cfg Config) refreshURL() string { return cfg.BaseURL + "/api/auth/refresh" }

func (cfg Config) profileSummaryURL() string { return cfg.BaseURL + "/api/user/profile-summary" }

// modelsURL carries the keyfrom body as a query string.  The vendor wants the
// same identity fields it wants in a renewal body, minus the refresh token.
func (cfg Config) modelsURL(acct AccountConfig, version string) string {
	q := url.Values{}
	q.Set("firstKeyfrom", acct.FirstKeyfrom)
	q.Set("latestKeyfrom", acct.LatestKeyfrom)
	q.Set("version", version)
	if acct.UUID != "" {
		q.Set("uuid", acct.UUID)
	}
	if acct.UserID != "" {
		q.Set("userId", acct.UserID)
	}
	return cfg.BaseURL + "/api/models/available?" + q.Encode()
}

func (cfg Config) activitySlotURL(version string) string {
	q := url.Values{}
	q.Set("placement", checkinPlacement)
	q.Set("clientVersion", version)
	q.Set("containerApiVersion", checkinContainerAPIVersion)
	q.Set("platform", checkinPlatform)
	return cfg.BaseURL + "/api/client-activities/slot?" + q.Encode()
}

func (cfg Config) activityContextURL(code, revision string) string {
	q := url.Values{}
	q.Set("configRevision", revision)
	return cfg.BaseURL + "/api/client-activities/" + url.PathEscape(code) + "/context?" + q.Encode()
}

func (cfg Config) activityCheckinURL(code string) string {
	return cfg.BaseURL + "/api/client-activities/" + url.PathEscape(code) + "/actions/check_in"
}

// loginURL builds the browser URL the operator opens.  redirect_uri is
// url-escaped because the portal hands it back verbatim.
func (cfg Config) loginURL(redirectURI, state string) string {
	q := url.Values{}
	q.Set("source", "electron")
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return cfg.PortalURL + loginPath + "?" + q.Encode()
}

func (cfg Config) callbackURL(port int) string {
	return "http://127.0.0.1:" + strconv.Itoa(port) + cfg.callbackPath()
}

// callbackPath is the loopback path the vendor redirects to.  It always starts
// with a slash: a hand-written "auth/callback" would otherwise build a URL with
// no path at all.
func (cfg Config) callbackPath() string {
	if p := strings.TrimSpace(cfg.CallbackPath); p != "" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return p
	}
	return defaultCallbackPath
}

// --- small helpers ----------------------------------------------------------

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
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

// validVersion reports whether a version string is one the vendor will accept.
func validVersion(v string) bool {
	return versionPattern.MatchString(strings.TrimSpace(v))
}
