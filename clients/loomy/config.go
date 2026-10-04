package loomy

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

// config.go owns the module's schema: every field optional, every field with a
// working default, and a malformed config never a construction failure.
//
// The reference plugin keeps Loomy's settings in its own settings namespace and
// derives almost everything from constants.  This module does the same: the
// vendor endpoints, the app id and the account-service access key are compiled
// in (they are the desktop client's own public constants, published in its
// resources/.env.prod), and the config object only exists to let an operator
// override them.

// Filenames inside Deps.DataDir.  Nothing outside that directory is ever
// written.
//
// Credentials and runtime penalties are kept in separate files on purpose.  A
// credential file that has been hand-edited, or an operator restoring a backup,
// must not be able to silently turn a session the vendor rejected back into a
// healthy-looking one.
const (
	accountsFile  = "accounts.json"
	stateFileName = "state.json"
)

// Vendor constants, taken from the reference's src/loomy.ts and
// src/loomy-product.ts (which read them out of the Loomy desktop client's
// resources/.env.prod).
const (
	// defaultAPIBase serves the OpenAI-compatible chat endpoint, the model
	// catalogue, the points ledger and the onboarding tasks.
	defaultAPIBase = "https://loomyad.xunfei.cn/api/v1"
	// defaultAccountBase is iFlytek's CAccount service, used only for the
	// SMS-code login that mints a session.  It is the only place the HMAC
	// signature is used.
	defaultAccountBase = "https://account.xfinfr.com"

	// defaultAccessKeyID / defaultAccessKeySecret sign requests to
	// defaultAccountBase.  They are the desktop client's own application
	// credentials, not a user's: the account service needs them to know which
	// application is asking, and every Loomy install ships with the same pair.
	// Nothing user-specific is derivable from them.
	defaultAccessKeyID     = "2thryby66wxi53sk"
	defaultAccessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"
	// defaultAppID is the application id the account service expects in the
	// request body's `base.appid`.
	defaultAppID = "GM3LOOMY"
)

// Defaults for every tuning knob.
const (
	// defaultRequestTimeout bounds one non-streaming request (the catalogue,
	// the points ledger, one login step).  The vendor's own client uses 60 s.
	defaultRequestTimeout = 60 * time.Second
	// defaultModelsTimeout bounds one catalogue fetch on its own, shorter than
	// the general request timeout because Models() is on a hot path.
	defaultModelsTimeout = 20 * time.Second
	// defaultChatTimeout bounds a whole streaming completion, including the
	// time the model spends thinking.  Loomy's reasoning models can be slow.
	defaultChatTimeout = 10 * time.Minute
	// defaultIdleTimeout is the gap between two SSE frames after which the
	// stream is declared dead.  The reference uses 120 s for the same knob.
	defaultIdleTimeout = 2 * time.Minute
	// defaultFirstByteTimeout bounds the wait for the response headers only.
	// It exists so that a wedged connection fails in two minutes rather than
	// in defaultChatTimeout.
	defaultFirstByteTimeout = 2 * time.Minute
	// defaultModelsTTL is how long a fetched catalogue is trusted before a
	// background refresh is kicked off.
	defaultModelsTTL = 30 * time.Minute
	// defaultProbeTimeout bounds one TestAccount / RefreshAccount round trip.
	// A panel button must answer promptly.
	defaultProbeTimeout = 30 * time.Second
	// defaultCooldown parks an account after a rate-limit or upstream failure.
	defaultCooldown = 60 * time.Second
	// defaultAuthCooldown parks an account after the vendor says the session is
	// dead.  It is long because there is nothing this module can do about it:
	// Loomy has no refresh endpoint, so the account stays parked until the
	// operator imports a new session.
	defaultAuthCooldown = 24 * time.Hour
	// defaultMaxAttempts is how many accounts one chat request may try before
	// giving up.
	defaultMaxAttempts = 3

	// sessionTTLSeconds is the session lifetime the login request asks for and
	// the only expiry information that exists: the account service never
	// returns an expiry, so the module stamps one locally at import time.
	// 1209600 s = 14 days, matching account-service.js's `expire: 14*24*3600`.
	sessionTTLSeconds = 1209600
	// smsCodeTTLSeconds is the lifetime of an SMS verification code.
	smsCodeTTLSeconds = 300
)

// accountConfig is one entry of the `accounts` array.
type accountConfig struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	AccessToken string `json:"access_token"`
	UserID      string `json:"userid"`
	Phone       string `json:"phone"`
	Nickname    string `json:"nickname"`
	ExpiresAt   string `json:"expires_at"`
	Enabled     *bool  `json:"enabled"`
}

// config is the decoded `clients.loomy` object.  An absent object and an empty
// object mean the same thing.
type config struct {
	APIBase         string `json:"api_base"`
	AccountBase     string `json:"account_base"`
	AccessKeyID     string `json:"access_key_id"`
	AccessKeySecret string `json:"access_key_secret"`
	AppID           string `json:"app_id"`

	Accounts []accountConfig `json:"accounts"`

	// Single-account shorthand, for an operator who has exactly one session.
	AccessToken string `json:"access_token"`
	UserID      string `json:"userid"`
	Phone       string `json:"phone"`
	Nickname    string `json:"nickname"`
	ExpiresAt   string `json:"expires_at"`

	RequestTimeout   durationField `json:"request_timeout"`
	ModelsTimeout    durationField `json:"models_timeout"`
	ChatTimeout      durationField `json:"chat_timeout"`
	IdleTimeout      durationField `json:"idle_timeout"`
	FirstByteTimeout durationField `json:"first_byte_timeout"`
	ModelsTTL        durationField `json:"models_ttl"`
	ProbeTimeout     durationField `json:"probe_timeout"`
	Cooldown         durationField `json:"cooldown"`
	AuthCooldown     durationField `json:"auth_cooldown"`

	MaxAttempts *int `json:"max_attempts"`

	// TLSProfile/TLSProtocol impersonate a browser's TLS ClientHello.  The
	// module drives a signed-in desktop session against the vendor's own
	// API, so a Go-shaped handshake is a detectable tell.  Empty keeps the
	// stock handshake.
	TLSProfile  string `json:"tls_profile"`
	TLSProtocol string `json:"tls_protocol"`

	// SMSToken is the one-time-SMS platform credential (eomsg).  Loomy's login
	// is a phone-code exchange, so the panel can rent a number and read the
	// code for it.  Empty means the panel reports "not configured" and the
	// auto-login flow refuses to start; the operator can also paste a token in
	// the panel, which overrides this per call without a restart.
	SMSToken string `json:"sms_token"`
	// SMSKeyword is the sender keyword the platform filters on.  Absent means
	// defaultSMSKeyword.  A wrong keyword reads as "no message has arrived".
	SMSKeyword string `json:"sms_keyword"`
	// SMSBase overrides the platform endpoint.  Absent means the built-in
	// eomsg URL, which is the only provider this module speaks.
	SMSBase string `json:"sms_base"`
	// SMSProvinces replaces the built-in rotation pool.  An empty list keeps
	// the built-in 31 provinces; "none" disables the rotation and lets the
	// platform choose.
	SMSProvinces []string `json:"sms_provinces"`
	// AutoLoginTimeoutSeconds bounds one auto-login run end to end.  Absent
	// means defaultAutoLoginTimeout.
	AutoLoginTimeoutSeconds *int `json:"auto_login_timeout_seconds"`
	// SMSPolls is how many times one rented number is checked for its SMS
	// before the run gives up on that number.  Absent means defaultSMSPolls.
	SMSPolls *int `json:"sms_polls"`
	// SMSIntervalSeconds is the pause between two SMS polls.  Absent means
	// defaultSMSInterval.
	SMSIntervalSeconds *int `json:"sms_interval_seconds"`
	// DupRetries is how many extra numbers to draw when the platform keeps
	// handing back one that is already in the pool.  Absent means
	// defaultDupRetries.
	DupRetries *int `json:"dup_retries"`
}

func (c config) apiBase() string {
	return firstNonEmpty(strings.TrimRight(c.APIBase, "/"), defaultAPIBase)
}

func (c config) accountBase() string {
	return firstNonEmpty(strings.TrimRight(c.AccountBase, "/"), defaultAccountBase)
}

func (c config) accessKeyID() string { return firstNonEmpty(c.AccessKeyID, defaultAccessKeyID) }
func (c config) accessKeySecret() string {
	return firstNonEmpty(c.AccessKeySecret, defaultAccessKeySecret)
}
func (c config) appID() string { return firstNonEmpty(c.AppID, defaultAppID) }

// tlsProfile / tlsProtocol expose the transport-fingerprint keys to New.
func (c config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

func (c config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

func (c config) requestTimeout() time.Duration {
	return durationOr(string(c.RequestTimeout), defaultRequestTimeout)
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
func (c config) firstByteTimeout() time.Duration {
	return durationOr(string(c.FirstByteTimeout), defaultFirstByteTimeout)
}
func (c config) modelsTTL() time.Duration {
	return durationOr(string(c.ModelsTTL), defaultModelsTTL)
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

func (c config) maxAttempts() int {
	if c.MaxAttempts == nil || *c.MaxAttempts <= 0 {
		return defaultMaxAttempts
	}
	return *c.MaxAttempts
}

// autoLoginTimeout resolves `auto_login_timeout_seconds`.
func (c config) autoLoginTimeout() time.Duration {
	if c.AutoLoginTimeoutSeconds != nil && *c.AutoLoginTimeoutSeconds > 0 {
		return time.Duration(*c.AutoLoginTimeoutSeconds) * time.Second
	}
	return defaultAutoLoginTimeout
}

// smsPolls resolves `sms_polls`: how many times one rented number is checked
// for its SMS before the run gives up on it.
func (c config) smsPolls() int {
	if c.SMSPolls != nil && *c.SMSPolls > 0 {
		return *c.SMSPolls
	}
	return defaultSMSPolls
}

// smsInterval resolves `sms_interval_seconds`: the pause between two SMS
// polls.
func (c config) smsInterval() time.Duration {
	if c.SMSIntervalSeconds != nil && *c.SMSIntervalSeconds > 0 {
		return time.Duration(*c.SMSIntervalSeconds) * time.Second
	}
	return defaultSMSInterval
}

// dupRetries resolves `dup_retries`: how many extra numbers to draw when the
// platform keeps handing back one that is already in the pool.
func (c config) dupRetries() int {
	if c.DupRetries != nil && *c.DupRetries > 0 {
		return *c.DupRetries
	}
	return defaultDupRetries
}

// durationField accepts a duration either as a Go duration string ("90s",
// "2m") or as a bare number of seconds, so an operator can write
// "chat_timeout": 600 without knowing the syntax.
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
	return "loomy: " + e.raw + " is neither a duration string nor a number of seconds"
}

// durationOr reads one duration field: empty falls back to the default, a Go
// duration string wins, otherwise the value is read as a number of seconds.  A
// non-positive or unparseable value falls back to the default rather than
// disabling a timeout -- a zero idle timeout would silently hang forever.
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

// parseConfig decodes the module's config object.
//
// An absent object is not an error: it yields the zero config, which is fully
// usable because every accessor has a default.  A malformed object IS an error,
// but the caller (New) logs it and carries on with defaults -- a typo in one
// timeout must never take the module off the panel.
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

// applyEnv lets the environment override the endpoints and the account-service
// application credentials.  The credential itself is handled by
// configuredAccounts so that config-file entries keep their order.
func applyEnv(cfg *config) {
	envStr(&cfg.APIBase, "CLIENT2API_LOOMY_API_BASE")
	envStr(&cfg.AccountBase, "CLIENT2API_LOOMY_ACCOUNT_BASE")
	envStr(&cfg.AccessKeyID, "CLIENT2API_LOOMY_ACCESS_KEY_ID")
	envStr(&cfg.AccessKeySecret, "CLIENT2API_LOOMY_ACCESS_KEY_SECRET")
	envStr(&cfg.AppID, "CLIENT2API_LOOMY_APP_ID")
	// EOMSG_TOKEN is shared with workbuddy: it is the same one-time-SMS
	// platform, so one environment variable can configure both modules.
	envStr(&cfg.SMSToken, "CLIENT2API_LOOMY_SMS_TOKEN")
	if strings.TrimSpace(cfg.SMSToken) == "" {
		envStr(&cfg.SMSToken, "EOMSG_TOKEN")
	}
	envStr(&cfg.SMSKeyword, "CLIENT2API_LOOMY_SMS_KEYWORD")
	envStr(&cfg.SMSBase, "CLIENT2API_LOOMY_SMS_BASE")
}

func envStr(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}

// configuredAccounts flattens every credential source into one ordered list:
// the `accounts` array first, then the single-account shorthand, then the
// environment.  Entries without a token are dropped, and a later entry never
// silently replaces an earlier one with the same account id -- the first one
// wins, so the config file keeps authority over the environment.
func configuredAccounts(cfg config) []account {
	var out []account
	seen := map[string]bool{}

	add := func(a account) {
		if strings.TrimSpace(a.AccessToken) == "" {
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
			ID:          entry.ID,
			Label:       entry.Label,
			AccessToken: strings.TrimSpace(entry.AccessToken),
			UserID:      strings.TrimSpace(entry.UserID),
			Phone:       strings.TrimSpace(entry.Phone),
			Nickname:    strings.TrimSpace(entry.Nickname),
			ExpiresAtMS: parseExpiryMS(entry.ExpiresAt),
			Enabled:     entry.Enabled == nil || *entry.Enabled,
		}})
	}

	add(account{storedAccount: storedAccount{
		AccessToken: strings.TrimSpace(cfg.AccessToken),
		UserID:      strings.TrimSpace(cfg.UserID),
		Phone:       strings.TrimSpace(cfg.Phone),
		Nickname:    strings.TrimSpace(cfg.Nickname),
		ExpiresAtMS: parseExpiryMS(cfg.ExpiresAt),
		Enabled:     true,
	}})

	add(account{storedAccount: storedAccount{
		AccessToken: strings.TrimSpace(os.Getenv("CLIENT2API_LOOMY_ACCESS_TOKEN")),
		UserID:      strings.TrimSpace(os.Getenv("CLIENT2API_LOOMY_USERID")),
		Phone:       strings.TrimSpace(os.Getenv("CLIENT2API_LOOMY_PHONE")),
		ExpiresAtMS: parseExpiryMS(os.Getenv("CLIENT2API_LOOMY_EXPIRES_AT")),
		Enabled:     true,
	}})

	return out
}

// parseExpiryMS reads an expiry into a unix millisecond timestamp, which is the
// unit the vendor's own credential uses (`expires_at` is a millisecond string).
//
// It accepts, in order: a bare millisecond timestamp, a bare second timestamp,
// and the date spellings an operator is likely to type.  0 means "unknown" and
// is deliberately NOT treated as expired -- the reference makes the same
// choice, because trying a possibly-stale session costs one request that
// answers 100002, while refusing to try blocks a working account on a guess.
func parseExpiryMS(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}

	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		switch {
		case n >= 1e12: // already milliseconds
			return n
		case n >= 1e9: // seconds
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
