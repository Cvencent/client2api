package trae

// config.go — the JSON object under "clients"."trae" in the main config.
//
// Every header value the upstream cares about is configurable on purpose: the
// two MIT reference clients disagree about the IDE version the server expects
// (trae2api-web hardcodes 0.1.52 / 20260811, while trae2api derives 3.3.67 CN /
// 3.5.51 SG from the IDE manifest).  docs/upstream/trae.md leaves that
// unsettled and explicitly asks for every version to be configurable, so the
// constants below are only defaults and Config can override each one.
//
// The defaults follow the Go port target (trae2api-web), not the Node spec
// client.  See README.md "Header versions".

import (
	"encoding/json"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// Upstream defaults.
const (
	defaultChatHost       = "https://trae-api-cn.mchost.guru"
	defaultAuthHost       = "https://api.trae.cn"
	defaultOAuthHost      = "https://api.trae.com.cn"
	defaultConsoleHost    = "https://www.trae.cn"
	defaultModel          = "glm-5.2"
	defaultFunction       = "solo_work_lite"
	defaultAppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	defaultClientID       = "en1oxy7wnw8j9n"
	defaultAppVersion     = "default"
	defaultIdeVersion     = "0.1.52"
	defaultIdeVersionCode = "20260811"
	defaultIdeVersionType = "stable"
	defaultDeviceType     = "windows"
	defaultOSVersion      = "Windows 11 Pro"
	defaultDeviceBrand    = "83DG"
	defaultDeviceCPU      = "Intel"
	defaultTrafficType    = "prod"
)

// Panel-login defaults, all taken from the trae2api-web reference
// (internal/upstream/constants.go, internal/server/callback.go) except the
// callback port: see loginCallbackPort().
const (
	// defaultLoginCallbackHost keeps the callback listener on loopback.  The
	// reference hardcodes 127.0.0.1 and this module does not offer a way to
	// widen that — the login page hands the operator's browser a credential.
	defaultLoginCallbackHost = "127.0.0.1"
	// defaultLoginPluginVersion is the login page's plugin_version parameter
	// (callback.go:33).
	defaultLoginPluginVersion = "2.3.62834"
	// defaultLoginSessionTTLSec mirrors the reference's 10-minute pending TTL
	// (login.go:288).
	defaultLoginSessionTTLSec = 600
)

// Behaviour defaults.
const (
	defaultTimeoutSec     = 120 // per-attempt budget for short JSON calls
	defaultIdleSec        = 300 // no-data watchdog on a streaming chat
	defaultCooldownSec    = 60  // generic short cooldown
	defaultRefreshSkewSec = 300 // refresh an access token this long before expiry
	defaultAttempts       = 3   // account failovers per request
	defaultModelsTTL      = time.Hour
)

// Config is the raw `clients.trae` object.  All fields are optional; zero values
// fall back to the defaults above (or to credential auto-discovery).
type Config struct {
	// --- credentials -------------------------------------------------------
	// StoragePath pins a single storage.json.  StoragePaths pins several.
	// AutoDiscover (default true) scans the known Trae product directories.
	StoragePath  string   `json:"storage_path"`
	StoragePaths []string `json:"storage_paths"`
	AutoDiscover *bool    `json:"auto_discover"`

	AccessToken       string `json:"access_token"`
	RefreshToken      string `json:"refresh_token"`
	UserID            string `json:"user_id"`
	AccessTokenExpiry string `json:"access_token_expiry"`
	MachineID         string `json:"machine_id"`
	DeviceID          string `json:"device_id"`
	AccountHost       string `json:"account_host"`
	Region            string `json:"region"`

	// --- panel login (web OAuth) -------------------------------------------
	// LoginEnabled (default true) lets an operator turn off the loopback
	// listener entirely.  OAuthHost/ConsoleHost are the CN web-login hosts from
	// the reference; the international realm has no documented console host, so
	// these stay CN-only (see README "Panel login").
	LoginEnabled       *bool  `json:"login_enabled"`
	OAuthHost          string `json:"oauth_host"`
	ConsoleHost        string `json:"console_host"`
	LoginCallbackHost  string `json:"login_callback_host"`
	LoginCallbackPort  int    `json:"login_callback_port"`
	LoginSessionTTLSec int    `json:"login_session_ttl_sec"`
	LoginAppVersion    string `json:"login_app_version"`
	LoginPluginVersion string `json:"login_plugin_version"`

	// --- channel / model ---------------------------------------------------
	ChatHost  string   `json:"chat_host"`
	Function  string   `json:"function"`
	Model     string   `json:"model"`
	Models    []string `json:"models"`
	MaxTokens int      `json:"max_tokens"`

	// --- header identity (all configurable; see README) --------------------
	UserAgent      string            `json:"user_agent"`
	AppID          string            `json:"app_id"`
	AppVersion     string            `json:"app_version"`
	IdeVersion     string            `json:"ide_version"`
	IdeVersionCode string            `json:"ide_version_code"`
	IdeVersionType string            `json:"ide_version_type"`
	DeviceType     string            `json:"device_type"`
	OSVersion      string            `json:"os_version"`
	DeviceBrand    string            `json:"device_brand"`
	DeviceCPU      string            `json:"device_cpu"`
	TrafficType    string            `json:"request_traffic_type"`
	ClientID       string            `json:"client_id"`
	ExtraHeaders   map[string]string `json:"extra_headers"`

	// --- transport fingerprint ---------------------------------------------
	// TLSProfile selects the TLS ClientHello to imitate, so the handshake
	// carries the same cipher suites, extensions, GREASE values and ALPN list
	// as the real Electron client instead of Go's crypto/tls.  Empty keeps
	// Go's handshake, which is why the default changes nothing.
	//
	// Only the TLS half is closed: the h2 frames are still written by
	// golang.org/x/net/http2.  See internal/fingerprint's package comment.
	TLSProfile string `json:"tls_profile"`
	// TLSProtocol is "h2" to require HTTP/2, "http/1.1" to offer http/1.1
	// alone, and empty to offer the profile's own ALPN list (h2 then
	// http/1.1) and downgrade when the server refuses h2.
	TLSProtocol string `json:"tls_protocol"`

	// --- behaviour ---------------------------------------------------------
	// SelfRenew is OFF by default: ExchangeToken rotates the refresh-token
	// family server-side, which can log the desktop app out (trae2api ships
	// TRAE_POOL_SELF_RENEW=off for the same reason).
	SelfRenew      *bool          `json:"self_renew"`
	TimeoutSec     int            `json:"timeout_sec"`
	IdleSec        int            `json:"idle_timeout_sec"`
	CooldownSec    int            `json:"cooldown_sec"`
	RefreshSkewSec int            `json:"refresh_skew_sec"`
	Attempts       int            `json:"attempts"`
	ExtraBody      map[string]any `json:"extra_body"`
}

// loadConfig decodes the module config.  Empty, null or malformed input degrades
// to the defaults: a module that is skipped entirely is worse than a module that
// runs with sane defaults.
func loadConfig(raw json.RawMessage, logf func(string, ...any)) *Config {
	cfg := &Config{}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal(raw, cfg); err != nil {
			if logf != nil {
				logf("invalid config, using defaults: %v", err)
			}
			cfg = &Config{}
		}
	}
	cfg.normalize()
	return cfg
}

// normalize trims strings and drops blank list entries.
func (c *Config) normalize() {
	trim := func(p *string) { *p = strings.TrimSpace(*p) }
	trim(&c.StoragePath)
	trim(&c.AccessToken)
	trim(&c.RefreshToken)
	trim(&c.UserID)
	trim(&c.AccessTokenExpiry)
	trim(&c.MachineID)
	trim(&c.DeviceID)
	trim(&c.AccountHost)
	trim(&c.Region)
	trim(&c.OAuthHost)
	trim(&c.ConsoleHost)
	trim(&c.LoginCallbackHost)
	trim(&c.LoginAppVersion)
	trim(&c.LoginPluginVersion)
	trim(&c.ChatHost)
	trim(&c.Function)
	trim(&c.Model)
	trim(&c.UserAgent)
	trim(&c.AppID)
	trim(&c.AppVersion)
	trim(&c.IdeVersion)
	trim(&c.IdeVersionCode)
	trim(&c.IdeVersionType)
	trim(&c.DeviceType)
	trim(&c.OSVersion)
	trim(&c.DeviceBrand)
	trim(&c.DeviceCPU)
	trim(&c.TrafficType)
	trim(&c.ClientID)
	trim(&c.TLSProfile)
	trim(&c.TLSProtocol)

	paths := make([]string, 0, len(c.StoragePaths))
	for _, p := range c.StoragePaths {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	c.StoragePaths = paths

	models := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	c.Models = models
}

// ---- accessors applying defaults ------------------------------------------

// tlsProfile is the ClientHello to imitate.  Empty means "look like Go", which
// is what every version before this key did.
func (c *Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(c.TLSProfile)
}

// tlsProtocol is the application protocol to negotiate.  Empty offers the
// profile's own ALPN list and downgrades when the server refuses h2.
func (c *Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(c.TLSProtocol)
}

func (c *Config) autoDiscover() bool {
	if c.AutoDiscover == nil {
		return true
	}
	return *c.AutoDiscover
}

func (c *Config) selfRenew() bool {
	if c.SelfRenew == nil {
		return false
	}
	return *c.SelfRenew
}

func (c *Config) chatHost() string {
	if c.ChatHost != "" {
		return strings.TrimRight(c.ChatHost, "/")
	}
	return defaultChatHost
}

func (c *Config) authHost() string {
	if c.AccountHost != "" {
		return strings.TrimRight(c.AccountHost, "/")
	}
	return defaultAuthHost
}

// ---- panel login accessors ------------------------------------------------

// loginEnabled reports whether the panel may open a loopback callback listener.
func (c *Config) loginEnabled() bool {
	if c.LoginEnabled == nil {
		return true
	}
	return *c.LoginEnabled
}

// oauthHost is the host serving ExchangeToken and GetUserInfo.  The reference
// hardcodes this as upstream.OAuthHost (constants.go:8).
func (c *Config) oauthHost() string {
	if c.OAuthHost != "" {
		return strings.TrimRight(c.OAuthHost, "/")
	}
	return defaultOAuthHost
}

// consoleHost serves the /authorization page the operator opens (constants.go:9).
func (c *Config) consoleHost() string {
	if c.ConsoleHost != "" {
		return strings.TrimRight(c.ConsoleHost, "/")
	}
	return defaultConsoleHost
}

func (c *Config) loginCallbackHost() string {
	if c.LoginCallbackHost != "" {
		return c.LoginCallbackHost
	}
	return defaultLoginCallbackHost
}

// loginCallbackPort is the loopback port for the OAuth callback.  0 — the
// default — asks the OS for a free port, which is what StartLogin needs so two
// concurrent sessions cannot collide.  The reference instead pins 18080
// (login.go:60-65); set login_callback_port to 18080 to reproduce that.
func (c *Config) loginCallbackPort() int {
	if c.LoginCallbackPort < 0 || c.LoginCallbackPort > 65535 {
		return 0
	}
	return c.LoginCallbackPort
}

// loginSessionTTL bounds how long a pending session may wait for the browser
// redirect.  The reference uses 10 minutes (login.go:288).
func (c *Config) loginSessionTTL() time.Duration {
	if c.LoginSessionTTLSec <= 0 {
		return defaultLoginSessionTTLSec * time.Second
	}
	return time.Duration(c.LoginSessionTTLSec) * time.Second
}

// loginAppVersion is the x_app_version sent to the login page.  The reference
// ties it to the IDE version (callback.go:20), so the IDE version wins unless
// login_app_version overrides it.
func (c *Config) loginAppVersion() string {
	if c.LoginAppVersion != "" {
		return c.LoginAppVersion
	}
	return c.ideVersion()
}

func (c *Config) loginPluginVersion() string {
	if c.LoginPluginVersion != "" {
		return c.LoginPluginVersion
	}
	return defaultLoginPluginVersion
}

func (c *Config) functionName() string {
	if c.Function != "" {
		return c.Function
	}
	return defaultFunction
}

func (c *Config) defaultModel() string {
	if c.Model != "" {
		return c.Model
	}
	return defaultModel
}

func (c *Config) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return "Trae/" + c.ideVersion()
}

func (c *Config) appID() string {
	if c.AppID != "" {
		return c.AppID
	}
	return defaultAppID
}

func (c *Config) clientID() string {
	if c.ClientID != "" {
		return c.ClientID
	}
	return defaultClientID
}

func (c *Config) appVersion() string {
	if c.AppVersion != "" {
		return c.AppVersion
	}
	return defaultAppVersion
}

func (c *Config) ideVersion() string {
	if c.IdeVersion != "" {
		return c.IdeVersion
	}
	return defaultIdeVersion
}

func (c *Config) ideVersionCode() string {
	if c.IdeVersionCode != "" {
		return c.IdeVersionCode
	}
	return defaultIdeVersionCode
}

func (c *Config) ideVersionType() string {
	if c.IdeVersionType != "" {
		return c.IdeVersionType
	}
	return defaultIdeVersionType
}

func (c *Config) deviceType() string {
	if c.DeviceType != "" {
		return c.DeviceType
	}
	return defaultDeviceType
}

func (c *Config) osVersion() string {
	if c.OSVersion != "" {
		return c.OSVersion
	}
	return defaultOSVersion
}

func (c *Config) deviceBrand() string {
	if c.DeviceBrand != "" {
		return c.DeviceBrand
	}
	return defaultDeviceBrand
}

func (c *Config) deviceCPU() string {
	if c.DeviceCPU != "" {
		return c.DeviceCPU
	}
	return defaultDeviceCPU
}

func (c *Config) trafficType() string {
	if c.TrafficType != "" {
		return c.TrafficType
	}
	return defaultTrafficType
}

func (c *Config) timeout() time.Duration {
	if c.TimeoutSec <= 0 {
		return defaultTimeoutSec * time.Second
	}
	return time.Duration(c.TimeoutSec) * time.Second
}

func (c *Config) idle() time.Duration {
	if c.IdleSec <= 0 {
		return defaultIdleSec * time.Second
	}
	return time.Duration(c.IdleSec) * time.Second
}

func (c *Config) cooldown() time.Duration {
	if c.CooldownSec <= 0 {
		return defaultCooldownSec * time.Second
	}
	return time.Duration(c.CooldownSec) * time.Second
}

func (c *Config) refreshSkew() time.Duration {
	if c.RefreshSkewSec <= 0 {
		return defaultRefreshSkewSec * time.Second
	}
	return time.Duration(c.RefreshSkewSec) * time.Second
}

func (c *Config) attempts() int {
	if c.Attempts <= 0 {
		return defaultAttempts
	}
	return c.Attempts
}

// modelIDs is the offline fallback catalogue.
func (c *Config) modelIDs() []string {
	if len(c.Models) > 0 {
		return c.Models
	}
	return defaultModelIDs
}

// defaultModelIDs is the static SOLO catalogue taken from the reference
// (trae2api-web internal/server/handler.go, SPEC P3).  It is only a fallback for
// when the live get_detail_param call is unavailable.
var defaultModelIDs = []string{
	"Doubao-Seed-2.1-Pro",
	"seed-code-pro-0430",
	"Doubao-Seed-2.1-Turbo",
	"Doubao-Seed-2.0-Code",
	"DeepSeek-V4-Flash-Official",
	"browser_use_subagent",
	"glm-5.2",
	"glm-5-turbo",
	"glm-5",
	"DeepSeek-V4-Pro",
	"DeepSeek-V4-Flash",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"minimax-m3",
	"qwen-3.7-plus",
	"sagitta",
	"aquila",
	"custom_model_gemini",
	"custom_model_placeholder",
	"custom_model_1M_text",
	"custom_model_1M",
	"custom_model_kimi",
	"custom_model_claude",
	"custom_model_gpt-5",
	"custom_model_no-fc",
	"custom_model_deepseek_chat",
	"custom_model_deepseek_reasoner",
	"custom_model_deepseek_v4",
	"explore_sub_agent_v13",
	"explore_sub_agent_v2",
	"summary",
}
