package autoclaw

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"client2api/internal/fingerprint"
)

const (
	accountsFile = "accounts.json"
	stateFile    = "state.json"
)

const (
	envAccessToken  = "CLIENT2API_AUTOCLAW_ACCESS_TOKEN"
	envRefreshToken = "CLIENT2API_AUTOCLAW_REFRESH_TOKEN"
	envUserID       = "CLIENT2API_AUTOCLAW_USER_ID"
	envNickname     = "CLIENT2API_AUTOCLAW_NICKNAME"
	envPhone        = "CLIENT2API_AUTOCLAW_PHONE"
	envDeviceID     = "CLIENT2API_AUTOCLAW_DEVICE_ID"
	envAccountID    = "CLIENT2API_AUTOCLAW_ACCOUNT_ID"
	envUserData     = "CLIENT2API_AUTOCLAW_USER_DATA_DIR"
	envSMSToken     = "CLIENT2API_AUTOCLAW_SMS_TOKEN"
	envSMSKeyword   = "CLIENT2API_AUTOCLAW_SMS_KEYWORD"
	envSMSBase      = "CLIENT2API_AUTOCLAW_SMS_BASE"
)

const (
	defaultMaxAttempts       = 3
	defaultModelsTTL         = 5 * time.Minute
	defaultModelsTimeout     = 20 * time.Second
	defaultChatTimeout       = 10 * time.Minute
	defaultIdleTimeout       = 2 * time.Minute
	defaultFirstTokenTimeout = 120 * time.Second
	defaultCooldown          = 60 * time.Second
	defaultShortCooldown     = 15 * time.Second
	defaultQuotaCooldown     = 24 * time.Hour
	defaultRefreshMargin     = tokenRefreshWindow
	defaultStoreFlush        = 5 * time.Second
	defaultLoginTimeout      = 5 * time.Minute
)

type durationField string

func (d durationField) String() string { return string(d) }

func (d *durationField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" || s == `""` {
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*d = durationField(v)
		return nil
	}
	*d = durationField(s)
	return nil
}

type accountConfig struct {
	Label         string `json:"label,omitempty"`
	AccountID     string `json:"account_id,omitempty"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	NumericUserID string `json:"numeric_user_id,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	Phone         string `json:"phone,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

type config struct {
	BaseURL       string `json:"base_url,omitempty"`
	UserAgent     string `json:"user_agent,omitempty"`
	ClientVersion string `json:"client_version,omitempty"`
	TLSProfile    string `json:"tls_profile,omitempty"`
	TLSProtocol   string `json:"tls_protocol,omitempty"`

	Accounts []accountConfig `json:"accounts,omitempty"`

	AccountID     string `json:"account_id,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	NumericUserID string `json:"numeric_user_id,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	Phone         string `json:"phone,omitempty"`
	DeviceID      string `json:"device_id,omitempty"`
	Label         string `json:"label,omitempty"`

	CredentialPaths []string `json:"credential_paths,omitempty"`
	AutoDiscover    *bool    `json:"auto_discover,omitempty"`

	MaxAttempts       *int          `json:"max_attempts,omitempty"`
	ModelsTTL         durationField `json:"models_ttl,omitempty"`
	ModelsTimeout     durationField `json:"models_timeout,omitempty"`
	ChatTimeout       durationField `json:"chat_timeout,omitempty"`
	IdleTimeout       durationField `json:"idle_timeout,omitempty"`
	FirstTokenTimeout durationField `json:"first_token_timeout,omitempty"`
	Cooldown          durationField `json:"cooldown,omitempty"`
	ShortCooldown     durationField `json:"short_cooldown,omitempty"`
	QuotaCooldown     durationField `json:"quota_cooldown,omitempty"`
	RefreshMargin     durationField `json:"refresh_margin,omitempty"`
	StoreFlush        durationField `json:"store_flush,omitempty"`
	LoginTimeout      durationField `json:"login_timeout,omitempty"`

	// SMSToken is the one-time-SMS platform credential (eomsg).  AutoClaw's
	// login is a phone-code exchange, so the panel can rent a number and read
	// the code for it.  Empty means the panel reports "not configured" and the
	// auto-login flow refuses to start; the operator can also paste a token in
	// the panel, which overrides this per call without a restart.
	SMSToken string `json:"sms_token,omitempty"`
	// SMSKeyword is the sender keyword the platform filters on.  Absent means
	// defaultSMSKeyword (智谱华章).  A wrong keyword reads as "no message has
	// arrived".
	SMSKeyword string `json:"sms_keyword,omitempty"`
	// SMSBase overrides the platform endpoint.  Absent means the built-in
	// eomsg URL, which is the only provider this module speaks.
	SMSBase string `json:"sms_base,omitempty"`
	// SMSProvinces replaces the built-in rotation pool.  An empty list keeps
	// the built-in 31 provinces; "none" disables the rotation and lets the
	// platform choose.
	SMSProvinces []string `json:"sms_provinces,omitempty"`
	// SMSCardType is the card class the platform hands out.  Absent means
	// "全部".
	SMSCardType string `json:"sms_card_type,omitempty"`
	// AutoLoginTimeoutSeconds bounds one auto-login run end to end.  Absent
	// means defaultAutoLoginTimeout.
	AutoLoginTimeoutSeconds *int `json:"auto_login_timeout_seconds,omitempty"`
	// SMSPolls is how many times one rented number is checked for its SMS
	// before the run gives up on that number.  Absent means defaultSMSPolls.
	SMSPolls *int `json:"sms_polls,omitempty"`
	// SMSIntervalSeconds is the pause between two SMS polls.  Absent means
	// defaultSMSInterval.
	SMSIntervalSeconds *int `json:"sms_interval_seconds,omitempty"`
	// DupRetries is how many extra numbers to draw when the platform keeps
	// handing back one that is already in the pool.  Absent means
	// defaultDupRetries.
	DupRetries *int `json:"dup_retries,omitempty"`
}

func (c config) baseURL() string {
	if v := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/"); v != "" {
		return v
	}
	return autoclawAPIBase
}
func (c config) endpoint(p string) string { return c.baseURL() + p }

func (c config) userAgent() string {
	if v := strings.TrimSpace(c.UserAgent); v != "" {
		return v
	}
	return "AutoClaw/2.0.5 (Windows)"
}
func (c config) version() string {
	if v := strings.TrimSpace(c.ClientVersion); v != "" {
		return v
	}
	return autoclawVersion
}
func (c config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}
func (c config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}
func (c config) autoDiscover() bool {
	return c.AutoDiscover == nil || *c.AutoDiscover
}
func (c config) maxAttempts() int {
	if c.MaxAttempts != nil && *c.MaxAttempts > 0 {
		return *c.MaxAttempts
	}
	return defaultMaxAttempts
}
func (c config) modelsTTL() time.Duration { return durationOr(c.ModelsTTL.String(), defaultModelsTTL) }
func (c config) modelsTimeout() time.Duration {
	return durationOr(c.ModelsTimeout.String(), defaultModelsTimeout)
}
func (c config) chatTimeout() time.Duration {
	return durationOr(c.ChatTimeout.String(), defaultChatTimeout)
}
func (c config) idleTimeout() time.Duration {
	return durationOr(c.IdleTimeout.String(), defaultIdleTimeout)
}
func (c config) firstTokenTimeout() time.Duration {
	return durationOr(c.FirstTokenTimeout.String(), defaultFirstTokenTimeout)
}
func (c config) cooldown() time.Duration { return durationOr(c.Cooldown.String(), defaultCooldown) }
func (c config) shortCooldown() time.Duration {
	return durationOr(c.ShortCooldown.String(), defaultShortCooldown)
}
func (c config) quotaCooldown() time.Duration {
	return durationOr(c.QuotaCooldown.String(), defaultQuotaCooldown)
}
func (c config) refreshMargin() time.Duration {
	return durationOr(c.RefreshMargin.String(), defaultRefreshMargin)
}
func (c config) storeFlush() time.Duration {
	return durationOr(c.StoreFlush.String(), defaultStoreFlush)
}
func (c config) loginTimeout() time.Duration {
	return durationOr(c.LoginTimeout.String(), defaultLoginTimeout)
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

func (c config) credentialPaths() []string {
	out := make([]string, 0, len(c.CredentialPaths))
	for _, p := range c.CredentialPaths {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func durationOr(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

func parseConfig(raw []byte) (config, error) {
	var cfg config
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

// applyEnv lets the environment supply the one-time-SMS platform settings, so
// a deployment can keep the token out of the config file.  EOMSG_TOKEN is the
// same variable workbuddy and loomy use: it is one platform account shared by
// every module that rents numbers from it.
func applyEnv(cfg *config) {
	envStr(&cfg.SMSToken, envSMSToken)
	if strings.TrimSpace(cfg.SMSToken) == "" {
		envStr(&cfg.SMSToken, "EOMSG_TOKEN")
	}
	envStr(&cfg.SMSKeyword, envSMSKeyword)
	envStr(&cfg.SMSBase, envSMSBase)
}

func envStr(dst *string, name string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		*dst = v
	}
}

func configuredAccounts(cfg config) []account {
	var out []account
	add := func(a accountConfig, origin string) {
		tok := strings.TrimSpace(a.AccessToken)
		if tok == "" {
			return
		}
		out = append(out, account{
			credential: credential{
				AccountID:     strings.TrimSpace(a.AccountID),
				AccessToken:   tok,
				RefreshToken:  strings.TrimSpace(a.RefreshToken),
				ExpiresAt:     flexString(strings.TrimSpace(a.ExpiresAt)),
				UserID:        strings.TrimSpace(a.UserID),
				NumericUserID: strings.TrimSpace(a.NumericUserID),
				Nickname:      strings.TrimSpace(a.Nickname),
				Phone:         strings.TrimSpace(a.Phone),
				DeviceID:      strings.TrimSpace(a.DeviceID),
			},
			Label:    strings.TrimSpace(a.Label),
			Disabled: a.Disabled,
			Origin:   origin,
		})
	}
	for _, a := range cfg.Accounts {
		add(a, originConfig)
	}
	add(accountConfig{
		Label: cfg.Label, AccountID: cfg.AccountID, AccessToken: cfg.AccessToken,
		RefreshToken: cfg.RefreshToken, ExpiresAt: cfg.ExpiresAt, UserID: cfg.UserID,
		NumericUserID: cfg.NumericUserID, Nickname: cfg.Nickname, Phone: cfg.Phone,
		DeviceID: cfg.DeviceID,
	}, originConfig)
	if len(out) == 0 {
		add(accountConfig{
			AccountID: os.Getenv(envAccountID), AccessToken: os.Getenv(envAccessToken),
			RefreshToken: os.Getenv(envRefreshToken), UserID: os.Getenv(envUserID),
			Nickname: os.Getenv(envNickname), Phone: os.Getenv(envPhone),
			DeviceID: os.Getenv(envDeviceID),
		}, originEnv)
	}
	return out
}
