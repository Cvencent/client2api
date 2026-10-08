package qoder

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// config.go owns the module's schema: every field is optional, every field has
// a working default, and a malformed config is never a construction failure.
//
// Qoder CN (qoder.com.cn) is Alibaba's Qoder desktop coding agent, the mainland
// build of qoder.com.  Its desktop client keeps a device token in an Electron
// safeStorage blob under %APPDATA%\com.qodercn.app.stable, and the same bearer
// token drives every OpenAPI endpoint this module speaks.  Nothing here is
// derived from a user: the base URLs, the client type and the client version
// are the desktop client's own public constants.

// Filenames inside Deps.DataDir.  Credentials and runtime penalties live in
// separate files on purpose: restoring a credential backup must not silently
// pardon an account the vendor has already rejected.
const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
)

// Vendor constants.
const (
	// defaultOpenAPIBase serves userinfo, the quota ledger and the campaign
	// (check-in) endpoints.  It is the only host this module calls.
	defaultOpenAPIBase = "https://openapi.qoder.com.cn"
	// defaultClientType is the Cosy-ClientType the desktop client sends
	// (10 = the Qoder desktop build).  The endpoints reject a request without
	// it, so it is sent on every call.
	defaultClientType = 10
	// defaultCosyVersion is the Cosy-Version the desktop client reports.
	defaultCosyVersion = "0.4.3"
	// defaultUserAgent is the User-Agent the desktop client reports.
	defaultUserAgent = "Qoder"
	// desktopUserDataDir is the Electron user-data directory Qoder CN writes
	// its safeStorage key and auth blob into, relative to %APPDATA%.
	desktopUserDataDir = "com.qodercn.app.stable"
	// desktopAuthFile is the encrypted credential blob inside that directory.
	desktopAuthFile = "auth.v1.dat"
)

// Defaults for every tuning knob.
const (
	// defaultRequestTimeout bounds one non-streaming OpenAPI request.
	defaultRequestTimeout = 30 * time.Second
	// defaultProbeTimeout bounds one TestAccount / RefreshAccount round trip.
	// A panel button has to answer promptly.
	defaultProbeTimeout = 20 * time.Second
	// defaultCooldown parks an account after a rate limit or a transport
	// failure.  It is short because those conditions are temporary.
	defaultCooldown = 60 * time.Second
	// defaultAuthCooldown parks an account after the vendor rejects its token.
	// It is long because re-importing from the desktop client is the only
	// remedy this module offers.
	defaultAuthCooldown = 30 * time.Minute
)

// accountConfig is one entry of the `accounts` array.
type accountConfig struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Phone        string `json:"phone"`
	Name         string `json:"name"`
	ExpiresAt    string `json:"expires_at"`
	Enabled      *bool  `json:"enabled"`
}

// config is the decoded `clients.qoder` object.  An absent object and an empty
// object mean the same thing.
type config struct {
	OpenAPIBase string `json:"openapi_base"`
	ClientType  *int   `json:"client_type"`
	CosyVersion string `json:"cosy_version"`
	UserAgent   string `json:"user_agent"`

	Accounts []accountConfig `json:"accounts"`

	// Single-account shorthand, for an operator who has exactly one token.
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Phone        string `json:"phone"`
	Name         string `json:"name"`
	ExpiresAt    string `json:"expires_at"`

	RequestTimeout durationField `json:"request_timeout"`
	ProbeTimeout   durationField `json:"probe_timeout"`
	Cooldown       durationField `json:"cooldown"`
	AuthCooldown   durationField `json:"auth_cooldown"`

	// Models overrides the built-in catalogue.  It exists because the vendor's
	// own model list lives behind an endpoint this module cannot call (see
	// README), so the operator is the authority on the model ids the platform
	// serves.
	Models []string `json:"models"`
}

func (c config) openAPIBase() string {
	return strings.TrimRight(firstNonEmpty(c.OpenAPIBase, defaultOpenAPIBase), "/")
}

func (c config) clientType() int {
	if c.ClientType == nil || *c.ClientType <= 0 {
		return defaultClientType
	}
	return *c.ClientType
}

func (c config) cosyVersion() string {
	return firstNonEmpty(c.CosyVersion, defaultCosyVersion)
}

func (c config) userAgent() string {
	return firstNonEmpty(c.UserAgent, defaultUserAgent)
}

func (c config) requestTimeout() time.Duration {
	return durationOr(string(c.RequestTimeout), defaultRequestTimeout)
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

// modelIDs is the catalogue the operator configured, or the built-in list.
func (c config) modelIDs() []string {
	if len(c.Models) == 0 {
		return builtinModelIDs()
	}
	out := make([]string, 0, len(c.Models))
	seen := map[string]bool{}
	for _, id := range c.Models {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return builtinModelIDs()
	}
	return out
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
	return "qoder: " + e.raw + " is neither a duration string nor a number of seconds"
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
// itself is handled by configuredAccounts so config-file entries keep order.
func applyEnv(cfg *config) {
	envStr(&cfg.OpenAPIBase, "CLIENT2API_QODER_OPENAPI_BASE")
	envStr(&cfg.CosyVersion, "CLIENT2API_QODER_COSY_VERSION")
	envStr(&cfg.UserAgent, "CLIENT2API_QODER_USER_AGENT")
}

func envStr(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}

// configuredAccounts flattens every credential source into one ordered list:
// the `accounts` array first, then the single-account shorthand, then the
// environment.  Entries without a token are dropped, and the first entry with a
// given id wins, so the config file keeps authority over the environment.
func configuredAccounts(cfg config) []account {
	var out []account
	seen := map[string]bool{}

	add := func(a account) {
		if strings.TrimSpace(a.Token) == "" {
			return
		}
		a.origin = originConfig
		if a.ID == "" {
			a.ID = defaultAccountID(a)
		}
		if a.Label == "" {
			a.Label = a.ID
		}
		if seen[a.ID] {
			return
		}
		seen[a.ID] = true
		out = append(out, a)
	}

	for _, entry := range cfg.Accounts {
		add(account{storedAccount: storedAccount{
			ID:                 entry.ID,
			Label:              entry.Label,
			Token:              strings.TrimSpace(entry.Token),
			RefreshToken:       strings.TrimSpace(entry.RefreshToken),
			UserID:             strings.TrimSpace(entry.UserID),
			Phone:              strings.TrimSpace(entry.Phone),
			Name:               strings.TrimSpace(entry.Name),
			ExpiresAtMS:        parseExpiryMS(entry.ExpiresAt),
			RefreshExpiresAtMS: 0,
			Enabled:            entry.Enabled == nil || *entry.Enabled,
		}})
	}

	add(account{storedAccount: storedAccount{
		Token:        strings.TrimSpace(cfg.Token),
		RefreshToken: strings.TrimSpace(cfg.RefreshToken),
		UserID:       strings.TrimSpace(cfg.UserID),
		Phone:        strings.TrimSpace(cfg.Phone),
		Name:         strings.TrimSpace(cfg.Name),
		ExpiresAtMS:  parseExpiryMS(cfg.ExpiresAt),
		Enabled:      true,
	}})

	add(account{storedAccount: storedAccount{
		Token:        strings.TrimSpace(os.Getenv("CLIENT2API_QODER_TOKEN")),
		RefreshToken: strings.TrimSpace(os.Getenv("CLIENT2API_QODER_REFRESH_TOKEN")),
		UserID:       strings.TrimSpace(os.Getenv("CLIENT2API_QODER_USER_ID")),
		Phone:        strings.TrimSpace(os.Getenv("CLIENT2API_QODER_PHONE")),
		Name:         strings.TrimSpace(os.Getenv("CLIENT2API_QODER_NAME")),
		ExpiresAtMS:  parseExpiryMS(os.Getenv("CLIENT2API_QODER_EXPIRES_AT")),
		Enabled:      true,
	}})

	return out
}

// parseExpiryMS reads an expiry into a unix millisecond timestamp.  It accepts a
// bare millisecond timestamp, a bare second timestamp, and the date spellings an
// operator is likely to type.  0 means "unknown" and is deliberately NOT treated
// as expired: the vendor is the authority on whether a token still works.
func parseExpiryMS(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n >= 1e12:
			return n
		case n >= 1e9:
			return n * 1000
		default:
			return 0
		}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
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
