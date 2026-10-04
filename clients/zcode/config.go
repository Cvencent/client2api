package zcode

import (
	"encoding/json"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// Defaults.  Everything here is overridable through the module config; nothing
// in this file is a hardcoded *region* or credential.
const (
	// defaultAppVersion is the baseline client build this module claims to be.
	// It must stay current: the vendor gates the daily Start Plan promotion on
	// the reported version (3.11.2 silently answers with an empty plan list
	// where 3.14.4 answers with a claimable plan).  version.go adopts whatever
	// the official release manifest advertises at runtime, newer than this.
	defaultAppVersion = "3.14.4"
	defaultAgent      = "glm"
	defaultPlatform   = "win32-x64"
	defaultOSVersion  = "10.0.22631"
	defaultReleaseCh  = "stable"
	defaultLanguage   = "zh-CN"
	defaultTimezone   = "Asia/Shanghai"
	defaultTitle      = "Z Code@electron"
	defaultReferer    = "https://zcode.z.ai/"
	defaultUserAgent  = "ZCode/" + defaultAppVersion

	defaultMaxTokens      = 4096
	defaultAttempts       = 5
	defaultCooldownSec    = 300
	defaultConcurrCoolSec = 15
	defaultTimeoutSec     = 600

	// configHost serves the unauthenticated client configuration document from
	// which the Aliyun captcha region/scene/prefix are read at runtime.
	configHost = "https://zcode.z.ai"

	// ZCode plan endpoints.  These are hosts, not regions.
	baseZaiPlan     = "https://zcode.z.ai/api/v1/zcode-plan/anthropic"
	baseZaiAPIKey   = "https://api.z.ai/api/anthropic"
	baseBigmodelKey = "https://open.bigmodel.cn/api/anthropic"

	messagesPath = "/v1/messages"

	// Panel login (the Z.AI "OAuth CLI" flow).  These mirror the reference
	// implementation's app/constants.py and app/settings.py:
	// OAUTH_API_BASE = ZCODE_ORIGIN + "/api/v1",
	// ZAI_EXCHANGE_ORIGIN = ZAI_API_ORIGIN.
	defaultOAuthAPIBase      = "https://zcode.z.ai/api/v1"
	defaultOAuthExchangeBase = "https://api.z.ai"
)

// defaultModels are the model ids the upstream accepted at recon time.  The
// upstream is the authority on this list; 3006 ("model not allowed") is the
// only signal we get for a wrong id, so the list stays small and configurable.
var defaultModels = []string{"GLM-5.3", "GLM-5.3-Flash"}

// accountConfig is one entry of the "accounts" array in the module config.
type accountConfig struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"` // zai | bigmodel
	Mode     string `json:"mode"`     // api_key | jwt
	APIKey   string `json:"api_key"`
	JWT      string `json:"jwt"`
	BaseURL  string `json:"base_url"`
	Enabled  *bool  `json:"enabled"`
}

// identityConfig mirrors the companion headers the official client sends.
type identityConfig struct {
	AppVersion     string `json:"app_version"`
	Agent          string `json:"agent"`
	Platform       string `json:"platform"`
	OSCategory     string `json:"os_category"`
	OSVersion      string `json:"os_version"`
	ReleaseChannel string `json:"release_channel"`
	Language       string `json:"language"`
	Timezone       string `json:"timezone"`
	Title          string `json:"title"`
	Referer        string `json:"referer"`
	DeviceMid      string `json:"device_mid"`
}

// Config is the JSON object found under "clients"."zcode" in the host config.
type Config struct {
	Accounts []accountConfig `json:"accounts"`

	// UpstreamBase is an explicit, off-by-default override that routes every
	// request to a single base URL (e.g. a locally running translating
	// gateway) and disables credential selection entirely.  It is never used
	// as a silent fallback.
	UpstreamBase string `json:"upstream_base"`

	AutoDiscover *bool    `json:"auto_discover"`
	Models       []string `json:"models"`

	MaxTokensDefault   int   `json:"max_tokens_default"`
	MaxAccountAttempts int   `json:"max_account_attempts"`
	CooldownSeconds    int   `json:"cooldown_seconds"`
	TimeoutSeconds     int   `json:"timeout_seconds"`
	InjectSystemBlocks *bool `json:"inject_system_blocks"`
	InjectCacheControl *bool `json:"inject_cache_control"`

	// Captcha: the JWT ("start-plan") channel needs a fresh Aliyun traceless
	// verification parameter on every request.  We do not ship a solver; an
	// operator may point at one.  Empty command == JWT accounts are unusable
	// and are reported as such.
	CaptchaCommand string   `json:"captcha_command"`
	CaptchaArgs    []string `json:"captcha_args"`
	CaptchaRegion  string   `json:"captcha_region"`

	// Panel login (Z.AI "OAuth CLI" flow).  The three endpoints below are the
	// only knobs; the flow itself is fixed by the vendor.
	//
	// OAuthAPIBase serves /oauth/cli/init and /oauth/cli/poll/{flow_id}.
	OAuthAPIBase string `json:"oauth_api_base"`
	// OAuthExchangeBase serves the /api/auth/z/login -> organisation/project ->
	// API key walk that turns an OAuth access token into a usable credential.
	OAuthExchangeBase string `json:"oauth_exchange_base"`
	// OAuthClientID is sent as "client_id" on init *only when set*.  The
	// reference flow does not use one (it sends provider:"zai" alone), so the
	// default is empty and the field exists purely as an escape hatch for a
	// vendor change.
	OAuthClientID string `json:"oauth_client_id"`
	// LoginTTLSeconds caps how long a started flow stays pollable.  The
	// reference keeps 300s, which is also what the vendor returns as
	// "expires_in" on init.
	LoginTTLSeconds int `json:"login_ttl_seconds"`

	Identity identityConfig `json:"identity"`

	// --- transport fingerprint ---------------------------------------------
	// TLSProfile selects the TLS ClientHello to imitate, so the handshake
	// carries the same cipher suites, extensions, GREASE values and ALPN list
	// as the official client instead of Go's crypto/tls.  Empty keeps Go's
	// handshake, which is why the default changes nothing.
	//
	// Only the TLS half is closed: the h2 frames are still written by
	// golang.org/x/net/http2.  See internal/fingerprint's package comment.
	TLSProfile string `json:"tls_profile"`
	// TLSProtocol is "h2" to require HTTP/2, "http/1.1" to offer http/1.1
	// alone, and empty to offer the profile's own ALPN list (h2 then
	// http/1.1) and downgrade when the server refuses h2.
	TLSProtocol string `json:"tls_protocol"`
}

func (c *Config) autoDiscover() bool {
	if c.AutoDiscover == nil {
		return true
	}
	return *c.AutoDiscover
}

func (c *Config) injectSystemBlocks() bool {
	if c.InjectSystemBlocks == nil {
		return true
	}
	return *c.InjectSystemBlocks
}

func (c *Config) injectCacheControl() bool {
	if c.InjectCacheControl == nil {
		return true
	}
	return *c.InjectCacheControl
}

// tlsProfile is the ClientHello to imitate.  Empty means "look like Go", which
// is what every version before this key did.
func (c *Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

// tlsProtocol is the application protocol to negotiate.  Empty offers the
// profile's own ALPN list and downgrades when the server refuses h2.
func (c *Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

func (c *Config) cooldown() time.Duration {
	if c.CooldownSeconds > 0 {
		return time.Duration(c.CooldownSeconds) * time.Second
	}
	return defaultCooldownSec * time.Second
}

func (c *Config) timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	if c.TimeoutSeconds < 0 {
		return 0
	}
	return defaultTimeoutSec * time.Second
}

func (c *Config) attempts() int {
	if c.MaxAccountAttempts > 0 {
		return c.MaxAccountAttempts
	}
	return defaultAttempts
}

func (c *Config) modelIDs() []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	return defaultModels
}

// oauthAPIBase is the origin+prefix that serves the OAuth CLI endpoints.
func (c *Config) oauthAPIBase() string {
	if v := strings.TrimSpace(c.OAuthAPIBase); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultOAuthAPIBase
}

// oauthExchangeBase is the origin that serves the business-token exchange.
func (c *Config) oauthExchangeBase() string {
	if v := strings.TrimSpace(c.OAuthExchangeBase); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultOAuthExchangeBase
}

// loginTTL is how long a started flow may be polled before it is abandoned.
// Unlike timeout() there is no "disabled" value: a login session always
// expires, so an abandoned flow can never be polled forever.
func (c *Config) loginTTL() time.Duration {
	if c.LoginTTLSeconds > 0 {
		return time.Duration(c.LoginTTLSeconds) * time.Second
	}
	return defaultLoginTTL
}

// loadConfig parses the module config, filling in every default.  A malformed
// config is reported through logf and degraded to defaults rather than turning
// into a factory error (a skipped module is worse than a degraded one).
func loadConfig(raw json.RawMessage, logf func(string, ...any)) *Config {
	cfg := &Config{}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, cfg); err != nil {
			if logf != nil {
				logf("zcode: config is not valid JSON (%v); using defaults", err)
			}
			cfg = &Config{}
		}
	}
	cfg.UpstreamBase = strings.TrimSpace(cfg.UpstreamBase)
	cfg.Identity = withIdentityDefaults(cfg.Identity)
	return cfg
}

func withIdentityDefaults(id identityConfig) identityConfig {
	set := func(dst *string, def string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = def
		}
	}
	set(&id.AppVersion, defaultAppVersion)
	set(&id.Agent, defaultAgent)
	set(&id.Platform, defaultPlatform)
	set(&id.OSCategory, osCategoryFor(id.Platform))
	set(&id.OSVersion, defaultOSVersion)
	set(&id.ReleaseChannel, defaultReleaseCh)
	set(&id.Language, defaultLanguage)
	set(&id.Timezone, defaultTimezone)
	set(&id.Title, defaultTitle)
	set(&id.Referer, defaultReferer)
	return id
}

// osCategoryFor mirrors the official client's platform -> category mapping.
func osCategoryFor(platform string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	switch {
	case strings.HasPrefix(p, "win"), strings.HasPrefix(p, "windows"):
		return "windows"
	case strings.HasPrefix(p, "darwin"), strings.HasPrefix(p, "mac"):
		return "macos"
	default:
		return "linux"
	}
}

// joinMessages builds the chat endpoint URL for a base that may or may not
// already carry the path.
func joinMessages(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, messagesPath) {
		return base
	}
	return base + messagesPath
}

// defaultBaseURL picks the endpoint family for a credential.
func defaultBaseURL(provider, mode string) string {
	if mode == modeJWT {
		return baseZaiPlan
	}
	if strings.EqualFold(provider, providerBigmodel) {
		return baseBigmodelKey
	}
	return baseZaiAPIKey
}
