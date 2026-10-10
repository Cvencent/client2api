// Package opencode implements the `opencode` client2api client module.
//
// It exposes OpenCode Zen — the paid model gateway run by the opencode project
// — as an OpenAI-compatible chat endpoint under the route prefix
// `opencode/<model>`.
//
// Zen speaks OpenAI chat completions natively and translates to each model's
// own protocol (Anthropic Messages, Google generateContent, the Responses API,
// Jev's /systemone) inside its own server, so this module needs exactly one
// inference endpoint: POST /chat/completions.
package opencode

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// clientName is the registered name, the routing prefix, and the value
// Name() returns.  The gateway routes `<client>/<model>` by splitting on the
// FIRST slash (internal/core/registry.go:169), so `opencode/gpt-5.1` reaches
// this module with Model already stripped to `gpt-5.1`.
const clientName = "opencode"

const (
	// defaultBaseURL is the documented Zen API origin.  GET /models is public;
	// everything else needs the key.
	defaultBaseURL = "https://opencode.ai/zen/v1"

	// defaultAuthBaseURL is the OpenCode Console origin the device-code login
	// talks to.
	defaultAuthBaseURL = "https://opencode.ai/console"

	defaultChatTimeout   = 10 * time.Minute
	defaultModelsTimeout = 20 * time.Second
	defaultModelsTTL     = time.Hour
	defaultIdleTimeout   = 2 * time.Minute
	defaultCooldown      = 60 * time.Second
	defaultQuotaCooldown = 12 * time.Hour
	defaultMaxInFlight   = 4
	defaultQueueTimeout  = time.Minute

	// credentialsFile is where imported/added credentials are persisted under
	// Deps.DataDir.  It is this module's own file; no other module reads it.
	credentialsFile = "accounts.json"
	// importDirName is the sub-directory of DataDir scanned by Discover
	// alongside the vendor's own auth.json.
	importDirName = "import"

	// maxErrorBody caps how much of an error response is read into memory
	// before it is classified.
	maxErrorBody = 32 << 10

	// credentialsVersion is bumped when the persisted shape changes.
	credentialsVersion = 1

	// defaultTestModel is what the panel's "test" button asks.  It is a cheap
	// model, and it is configurable because Zen retires models: if this id ever
	// disappears, the test would report a model error rather than the account's
	// health, so the operator needs a way to point it elsewhere without a
	// rebuild.
	defaultTestModel = "gpt-5-nano"
)

// Duration is a JSON duration that accepts a Go duration string ("90s"), a
// bare JSON number read as seconds (90) or null.
//
// A value it cannot parse becomes zero, and every accessor reads zero as "use
// the documented default".  That is deliberate: the contract requires that a
// config decode failure never takes the module down, and one mistyped duration
// must not discard the rest of the block.
type Duration time.Duration

// UnmarshalJSON never returns an error.  A malformed duration is recorded as
// zero, which the accessors turn into the documented default.
func (d *Duration) UnmarshalJSON(b []byte) error {
	*d = 0
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return nil
		}
		if parsed, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && parsed > 0 {
			*d = Duration(parsed)
		}
		return nil
	}
	var secs float64
	if err := json.Unmarshal(b, &secs); err != nil {
		return nil
	}
	// A month is the ceiling: anything larger is a unit mistake, not a
	// deliberate deadline.
	if secs > 0 && secs <= 30*24*60*60 {
		*d = Duration(time.Duration(secs * float64(time.Second)))
	}
	return nil
}

// MarshalJSON writes the canonical Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// or returns the parsed duration, or def when it is unset or non-positive (a
// zero timeout would silently disable the guard it configures).
func (d Duration) or(def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return time.Duration(d)
}

// AccountConfig is one credential supplied inline in the host config.
type AccountConfig struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
	Key      string `json:"key"` // alias for api_key
	Disabled bool   `json:"disabled"`
}

// Config is the schema of the JSON object under `clients.opencode`.
//
// Every key is optional: a deployment that only exports OPENCODE_API_KEY, or
// that imports the credential from opencode's own auth.json, needs no config
// block at all.
type Config struct {
	// BaseURL overrides the Zen origin.  It exists so a mirror or a local
	// recorder can be pointed at without a rebuild.
	BaseURL string `json:"base_url"`
	// APIKey is the single-credential shorthand.  It becomes one pool account.
	APIKey string `json:"api_key"`
	// Accounts is the multi-credential form.
	Accounts []AccountConfig `json:"accounts"`
	// ExtraModels appends ids to the built-in catalogue.  It never removes one:
	// the live GET /models list is the authority on which ids exist.
	ExtraModels []string `json:"extra_models"`
	// ExtraHeaders are added to every request.  Reserved names are dropped.
	ExtraHeaders map[string]string `json:"extra_headers"`

	ChatTimeout   Duration `json:"chat_timeout"`
	QueueTimeout  Duration `json:"queue_timeout"`
	ModelsTimeout Duration `json:"models_timeout"`
	ModelsTTL     Duration `json:"models_ttl"`
	IdleTimeout   Duration `json:"idle_timeout"`
	Cooldown      Duration `json:"cooldown"`
	QuotaCooldown Duration `json:"quota_cooldown"`

	// MaxInFlight is the per-account concurrent-request ceiling before
	// core.ErrBusy.
	MaxInFlight int `json:"max_in_flight"`
	// IncludeUsage asks for a usage frame via stream_options.  See the README:
	// Zen's own protocol converters are built on a neutral request shape that
	// does not list stream_options, so this is best-effort and off by default.
	IncludeUsage bool `json:"include_usage"`
	// DisableAuthJSONDiscovery turns off the on-machine auth.json scan.
	DisableAuthJSONDiscovery bool `json:"disable_auth_json_discovery"`
	// TestModel is the model the panel's account test asks.  Empty means
	// defaultTestModel.
	TestModel string `json:"test_model"`
	// AuthBaseURL overrides the OpenCode Console origin the device-code login
	// talks to.  Empty means the documented console origin.
	AuthBaseURL string `json:"auth_base_url"`
	// DefaultRealm is the login realm StartLogin targets when the panel sends
	// none: "free" or "oauth".  Empty means "free".
	DefaultRealm string `json:"default_realm"`
	// FreeModels is the allowlist of zero-cost model ids served to anonymous
	// accounts.  Empty means the built-in list.  The vendor gates most of the
	// *-free ids behind a paid plan, so this is an allowlist, not a pattern.
	FreeModels []string `json:"free_models"`
}

// parseConfig decodes the raw JSON of `clients.opencode`.
//
// An absent or null object is a zero Config with a nil error — that is a valid
// deployment, not a mistake.  Only a real JSON syntax or type error is
// reported, and New() turns even that into a logged warning plus defaults.
func parseConfig(raw json.RawMessage) (Config, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return Config{}, nil
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("clients.%s: %w", clientName, err)
	}
	return cfg, nil
}

// reservedHeaders are the names a config-supplied header may never override:
// the module owns authentication, content negotiation and framing.
var reservedHeaders = map[string]bool{
	"authorization":         true,
	"x-api-key":             true,
	"x-opencode-org-id":     true,
	"x-opencode-session":    true,
	"x-opencode-session-id": true,
	"content-type":          true,
	"content-length":        true,
	"accept":                true,
	"accept-encoding":       true,
	"user-agent":            true,
	"host":                  true,
	"transfer-encoding":     true,
}

// normalize fills defaults and drops entries that cannot be used.  It never
// returns an error.
func (cfg Config) normalize() Config {
	out := cfg

	out.BaseURL = strings.TrimRight(strings.TrimSpace(out.BaseURL), "/")
	if !validBaseURL(out.BaseURL) {
		out.BaseURL = defaultBaseURL
	}

	out.APIKey = strings.TrimSpace(out.APIKey)
	out.ExtraModels = dedupeStrings(out.ExtraModels)
	out.ExtraHeaders = filterHeaders(out.ExtraHeaders)

	out.AuthBaseURL = strings.TrimRight(strings.TrimSpace(out.AuthBaseURL), "/")
	if !validBaseURL(out.AuthBaseURL) {
		out.AuthBaseURL = defaultAuthBaseURL
	}
	out.DefaultRealm = normalizeLoginRealm(out.DefaultRealm)
	out.FreeModels = dedupeStrings(out.FreeModels)

	accts := make([]AccountConfig, 0, len(out.Accounts))
	for _, a := range out.Accounts {
		a.ID = strings.TrimSpace(a.ID)
		a.Label = strings.TrimSpace(a.Label)
		a.APIKey = strings.TrimSpace(firstNonEmpty(a.APIKey, a.Key))
		a.Key = ""
		if a.APIKey == "" {
			// An entry with no key is not an account; keeping it would put a
			// permanently-dead row in the panel.
			continue
		}
		accts = append(accts, a)
	}
	out.Accounts = accts
	return out
}

// validBaseURL accepts an absolute http(s) URL with a host.
func validBaseURL(raw string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// filterHeaders drops reserved names and blanks, and trims the rest.
func filterHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		name := strings.TrimSpace(k)
		if name == "" || reservedHeaders[strings.ToLower(name)] {
			continue
		}
		if strings.TrimSpace(v) == "" {
			continue
		}
		out[name] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (cfg Config) baseURL() string {
	if !validBaseURL(cfg.BaseURL) {
		return defaultBaseURL
	}
	return strings.TrimRight(cfg.BaseURL, "/")
}

// chatURL is the one inference endpoint this module uses.
func (cfg Config) chatURL() string { return cfg.baseURL() + "/chat/completions" }

// modelsURL is public and needs no credential.
func (cfg Config) modelsURL() string { return cfg.baseURL() + "/models" }

func (cfg Config) chatTimeout() time.Duration   { return cfg.ChatTimeout.or(defaultChatTimeout) }
func (cfg Config) queueTimeout() time.Duration  { return cfg.QueueTimeout.or(defaultQueueTimeout) }
func (cfg Config) modelsTimeout() time.Duration { return cfg.ModelsTimeout.or(defaultModelsTimeout) }
func (cfg Config) modelsTTL() time.Duration     { return cfg.ModelsTTL.or(defaultModelsTTL) }
func (cfg Config) idleTimeout() time.Duration   { return cfg.IdleTimeout.or(defaultIdleTimeout) }
func (cfg Config) cooldown() time.Duration      { return cfg.Cooldown.or(defaultCooldown) }

func (cfg Config) quotaCooldown() time.Duration {
	return cfg.QuotaCooldown.or(defaultQuotaCooldown)
}

func (cfg Config) maxInFlight() int {
	if cfg.MaxInFlight <= 0 {
		return defaultMaxInFlight
	}
	return cfg.MaxInFlight
}

// testModel is the model the panel's account test asks.
func (cfg Config) testModel() string {
	return firstNonEmpty(strings.TrimSpace(cfg.TestModel), defaultTestModel)
}

// authBaseURL is the OpenCode Console origin the device-code login uses.
func (cfg Config) authBaseURL() string {
	if !validBaseURL(cfg.AuthBaseURL) {
		return defaultAuthBaseURL
	}
	return strings.TrimRight(cfg.AuthBaseURL, "/")
}

// resolveAuthURL makes a console URL absolute.  The device-code endpoint
// returns a relative path (for example "/console/device?user_code=…"), which
// a browser cannot open on its own; the vendor's own client resolves it against
// the console origin, so this does the same.  An already-absolute URL is
// returned unchanged.
func (cfg Config) resolveAuthURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	ref, err := url.Parse(raw)
	if err != nil || ref.IsAbs() {
		return raw
	}
	base, err := url.Parse(cfg.authBaseURL() + "/")
	if err != nil {
		return raw
	}
	return base.ResolveReference(ref).String()
}

// defaultRealm is the login realm StartLogin targets with no argument.
func (cfg Config) defaultRealm() string { return normalizeLoginRealm(cfg.DefaultRealm) }

// freeModels is the allowlist of model ids an anonymous account may ask for.
func (cfg Config) freeModels() []string {
	if len(cfg.FreeModels) == 0 {
		return append([]string(nil), freeModelAllowlist...)
	}
	return cfg.FreeModels
}
