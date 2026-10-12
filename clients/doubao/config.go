package doubao

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// config.go owns the module's schema: every field is optional, every field has
// a working default, and a malformed config object never takes the module off
// the panel -- New logs it and falls back to the defaults.
//
// Doubao (Doubao) is ByteDance's mainland consumer assistant.  The desktop
// client is a Chromium shell around the www.doubao.com web app, and this module
// speaks that web API directly: the same host, the same routes, the same cookie
// session.  Nothing here is a secret; the credential is the cookie jar, which
// lives only in the account store.

// Filenames inside Deps.DataDir.  Credentials and runtime penalties are kept in
// separate files on purpose: restoring a credential backup must not silently
// pardon an account the vendor already rejected.
const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
)

// Vendor constants.  These are the platform identifiers the web app sends on
// every request; they were read out of the desktop client's own bundle.
const (
	defaultWebBase = "https://www.doubao.com"

	// The public product ids the web app reports.  They are not secrets.
	paramAid     = "582478"
	paramRealAid = "10022"
	paramPkgType = "1"
	paramVersion = "2.31.4"
	paramRegion  = "cn"
	agentJSConv  = "str"
	// defaultBotID is the assistant id the desktop client stamps on the action
	// bar and chat requests.  It is the public default bot, not a secret.
	defaultBotID = "7338286299411103781"
)

// Endpoints the module speaks.  Every one of them was confirmed in the desktop
// client's bundle.
const (
	pathAccountInfo  = "/alice/user/get_account_info"
	pathProfileBrief = "/alice/profile/self_brief"
	pathChat         = "/samantha/chat/completion"
	// pathUserSettingGet is the endpoint whose action_bar_menu_config carries
	// the v1 (menu_item_list) model menu the desktop client renders.
	pathUserSettingGet = "/samantha/user/setting/get"
	// pathActionBarBriefList is the v2 menu endpoint.  Its entry_list carries
	// the modeConfV2 (mode_list/model_list) the current client prefers; when
	// use_menu_conf_v2 is absent the v1 menu above is the one to use.
	pathActionBarBriefList = "/alice/slot/action_bar_v3/brief_list"
	// pathLaunchCore is where the client learns whether the v2 menu is on.
	pathLaunchCore         = "/alice/user/launch/core"
	pathConversationUpdate = "/alice/conversation/update"
	pathCreditNum          = "/commerce/benefit_supply/credit/get_credit_num_optional_tasks"
	pathCreditUsage        = "/commerce/benefit_supply/credit/get_credit_usage_history"
	// The membership quota endpoints expose the rolling usage window shown in
	// the desktop client under the credit balance. The summary response is the
	// smallest payload; overview is a compatibility fallback with the same
	// window_limit_section shape.
	pathSubscriptionQuotaSummary = "/alice/commerce/sale/subscription/quota/summary/"
	pathSubscriptionOverview     = "/alice/commerce/sale/subscription/overview/"
)

// Defaults for every tuning knob.
const (
	defaultRequestTimeout = 30 * time.Second
	defaultChatTimeout    = 5 * time.Minute
	defaultProbeTimeout   = 20 * time.Second
	defaultModelsTTL      = 30 * time.Minute
	defaultCooldown       = 60 * time.Second
	defaultAuthCooldown   = 30 * time.Minute

	// defaultLoginTimeout bounds one web-login hand-off end to end.  The
	// operator has to scan a QR code or receive an SMS, so it is generous.
	defaultLoginTimeout = 10 * time.Minute
)

// config is the decoded `clients.doubao` object.  An absent object and an empty
// object mean the same thing.
type config struct {
	// WebBase is the origin the API and the login page live on.  It is
	// configurable because the vendor has served the app from more than one
	// host, and a wrong one fails in confusing ways.
	WebBase string `json:"web_base"`

	// Accounts is the hand-managed credential list.  Normally empty: the web
	// login and the panel's account form own the store.
	Accounts []accountConfig `json:"accounts"`

	// Single-account shorthand, for an operator with exactly one cookie.
	Cookie string `json:"cookie"`
	Label  string `json:"label"`

	// Models overrides the built-in catalogue.
	Models []string `json:"models"`

	// ChatBotID is the `bot_id` the chat body carries.  Empty means the
	// vendor's default bot, which is what the web app itself sends.
	ChatBotID string `json:"chat_bot_id"`

	RequestTimeout durationField `json:"request_timeout"`
	ChatTimeout    durationField `json:"chat_timeout"`
	ProbeTimeout   durationField `json:"probe_timeout"`
	Cooldown       durationField `json:"cooldown"`
	AuthCooldown   durationField `json:"auth_cooldown"`
	ModelsTTL      durationField `json:"models_ttl"`

	// BrowserPath pins the Chromium-family executable the web login drives.
	// Empty means "find an installed Edge or Chrome".
	BrowserPath string `json:"browser_path"`
	// BrowserHeadless runs that browser without a window.  Absent means false:
	// a login needs a visible page for the operator to scan.
	BrowserHeadless *bool `json:"browser_headless"`
	// LoginTimeoutSeconds bounds one login hand-off end to end.  Absent means
	// 600s.
	LoginTimeoutSeconds *int `json:"login_timeout_seconds"`

	// ClientUserDataDir points at the Doubao desktop client's Chromium user-data
	// directory (the one holding `Local State`).  Empty means the default
	// location under %LOCALAPPDATA%.  It only matters for the `client_import`
	// path, which reads the already-signed-in client instead of a browser
	// sign-in.
	ClientUserDataDir string `json:"client_user_data_dir"`
	// ClientProfile pins which profile directory inside the user-data dir holds
	// the session.  Empty means the one that has a live Doubao session cookie.
	ClientProfile string `json:"client_profile"`

	// DefaultModeID and DefaultModelItemKey are the routing fields a chat
	// request carries when the caller asked for a plain model id the client has
	// not shown.  They match the web client's own defaults.
	DefaultModeID       string `json:"default_mode_id"`
	DefaultModelItemKey string `json:"default_model_item_key"`
	DefaultReplyMode    string `json:"default_reply_mode"`
	DefaultReasoning    string `json:"default_reasoning_effort"`
}

// accountConfig is one entry of the `accounts` array.
type accountConfig struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Cookie  string `json:"cookie"`
	Enabled *bool  `json:"enabled"`
}

func (c config) webBase() string {
	return strings.TrimRight(firstNonEmpty(c.WebBase, defaultWebBase), "/")
}

func (c config) requestTimeout() time.Duration {
	return durationOr(string(c.RequestTimeout), defaultRequestTimeout)
}

func (c config) chatTimeout() time.Duration {
	return durationOr(string(c.ChatTimeout), defaultChatTimeout)
}

func (c config) probeTimeout() time.Duration {
	return durationOr(string(c.ProbeTimeout), defaultProbeTimeout)
}

func (c config) cooldown() time.Duration {
	return durationOr(string(c.Cooldown), defaultCooldown)
}

func (c config) authCooldown() time.Duration {
	return durationOr(string(c.AuthCooldown), defaultAuthCooldown)
}

func (c config) modelsTTL() time.Duration {
	return durationOr(string(c.ModelsTTL), defaultModelsTTL)
}

// clientUserDataDir resolves the desktop client's Chromium user-data directory:
// an explicit config wins, then the environment, then the default per-user
// location.  An empty result means the client import is unavailable.
func (c config) clientUserDataDir() string {
	if v := strings.TrimSpace(c.ClientUserDataDir); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CLIENT2API_DOUBAO_USER_DATA_DIR")); v != "" {
		return v
	}
	local := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if local == "" {
		return ""
	}
	return filepath.Join(local, "Doubao", "User Data")
}

// clientProfileDir pins the profile directory, or returns "" for auto-detect.
func (c config) clientProfileDir() string {
	return strings.TrimSpace(c.ClientProfile)
}

// chatDefaults are the routing fields a chat request carries when the caller's
// model resolved to nothing more specific.  They mirror the web client's own
// defaults: mode 3 (an Agent/mode-list selection) and model item "0".
func (c config) chatDefaults() modelRoute {
	return modelRoute{
		ModeID:          firstNonEmpty(strings.TrimSpace(c.DefaultModeID), "3"),
		ModelItemKey:    firstNonEmpty(strings.TrimSpace(c.DefaultModelItemKey), "0"),
		ReplyMode:       firstNonEmpty(strings.TrimSpace(c.DefaultReplyMode), strings.TrimSpace(c.DefaultModelItemKey), "0"),
		ReasoningEffort: strings.TrimSpace(c.DefaultReasoning),
	}
}

// defaultRoute is the routing the module sends when a caller asked for a model
// the client has not advertised.  Operator overrides win; otherwise it is the
// client's own default (mode 3 / model item "0").
func defaultRoute(cfg config) modelRoute {
	r := cfg.chatDefaults()
	if r.ModeID == "" {
		r.ModeID = defaultModeID
	}
	if r.ModelItemKey == "" {
		r.ModelItemKey = defaultModelItemKey
	}
	if r.ReplyMode == "" {
		r.ReplyMode = firstNonEmpty(r.ModelItemKey, r.ModeID)
	}
	return r
}

// browserPath resolves the executable the login drives.
func (c config) browserPath() string {
	return strings.TrimSpace(c.BrowserPath)
}

// browserHeadless resolves `browser_headless`.  Absent means false: a login
// flow needs a window the operator can see.
func (c config) browserHeadless() bool {
	if c.BrowserHeadless != nil {
		return *c.BrowserHeadless
	}
	return false
}

// loginTimeout resolves `login_timeout_seconds`.
func (c config) loginTimeout() time.Duration {
	if c.LoginTimeoutSeconds != nil && *c.LoginTimeoutSeconds > 0 {
		return time.Duration(*c.LoginTimeoutSeconds) * time.Second
	}
	return defaultLoginTimeout
}

// durationField accepts a duration either as a Go duration string ("90s",
// "2m") or as a bare number of seconds.
type durationField string

func (d *durationField) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*d = durationField(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		*d = durationField(n.String())
		return nil
	}
	return &durationError{raw: string(raw)}
}

type durationError struct{ raw string }

func (e *durationError) Error() string {
	return "doubao: " + e.raw + " is neither a duration string nor a number of seconds"
}

// durationOr reads one duration field: empty falls back to the default, a Go
// duration string wins, otherwise the value is read as a number of seconds.  A
// non-positive or unparseable value falls back to the default rather than
// disabling a timeout.
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
	if secs, err := strconv.ParseFloat(s, 64); err == nil {
		d := time.Duration(secs * float64(time.Second))
		if d <= 0 {
			return def
		}
		return d
	}
	return def
}

// parseConfig decodes the module's config object.  An absent object yields the
// zero config, which is fully usable because every accessor has a default.
func parseConfig(raw json.RawMessage) (config, error) {
	var cfg config
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// applyEnv lets the environment override the endpoint knobs.  The credential
// itself is handled by configuredAccounts.
func applyEnv(cfg *config) {
	envStr(&cfg.WebBase, "CLIENT2API_DOUBAO_WEB_BASE")
	envStr(&cfg.BrowserPath, "CLIENT2API_DOUBAO_BROWSER_PATH")
	envStr(&cfg.ChatBotID, "CLIENT2API_DOUBAO_CHAT_BOT_ID")
}

func envStr(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}

// firstNonEmpty returns the first non-blank string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
