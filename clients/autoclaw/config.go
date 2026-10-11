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
