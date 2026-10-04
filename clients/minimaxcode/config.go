// Package minimaxcode relays the MiniMax Code desktop client.
//
// MiniMax Code is an Electron app that talks to a native Anthropic Messages
// endpoint:
//
//	POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages
//	Authorization: Bearer mmoat_...
//	anthropic-version: 2023-06-01
//
// so this module is shaped like clients/zcode: an OpenAI-shaped request is
// rewritten into an Anthropic body, and the Anthropic SSE that comes back is
// translated into core events.  The two modules share a shape and nothing else
// -- no file here imports another client module.
//
// Credentials are plaintext, unlike zcode's secretbox: the desktop client
// stores them in ~/.minimax/auth/<buildEnv>/<region>/<clientId>/auth.json.
package minimaxcode

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

const (
	// name is the registry key, the model prefix the gateway qualifies with,
	// and the client name every core.Failure carries.
	name = "minimaxcode"

	// defaultBaseURL is the client's own baseURL with the trailing /v1 folded
	// into messagesPath, so both "…/llm" and "…/llm/v1" spellings in a
	// config.yaml or a base_url override resolve to the same endpoint.
	defaultBaseURL = "https://agent.minimax.cn/mavis/api/v1/llm"
	messagesPath   = "/v1/messages"

	// The desktop client keeps both of these under the user's home directory.
	defaultAuthDir    = ".minimax/auth"
	defaultConfigYAML = ".minimax/config.yaml"

	// The OAuth refresh endpoint and the client id the CLI uses.  The desktop
	// store records clientId "mcode-public"; the CLI refreshes with
	// "mcode_tool".  Both are tried, recorded in the README.
	defaultOAuthTokenURL = "https://account.minimax.cn/oauth2/token"
	// defaultOAuthDeviceURL is the RFC 8628 device authorization endpoint the
	// desktop client starts a sign-in at.  It is always the token endpoint's
	// origin plus /oauth2/device/code, so a region change moves both together.
	defaultOAuthDeviceURL = "https://account.minimax.cn/oauth2/device/code"
	oauthClientIDDesktop  = "mcode-public"
	oauthClientIDCLI      = "mcode_tool"

	defaultTimeoutSeconds  = 120
	defaultMaxTokens       = 8192
	defaultMaxAccountTries = 3
	defaultCooldownSeconds = 120

	// refreshLeeway is how far ahead of expiresAtMs a token counts as expired.
	// Refreshing rotates the refresh token, so this must not be eager.
	refreshLeeway = 60 * time.Second

	maxTokenBytes = 4096
	maxLabelRunes = 120
	maxURLBytes   = 512

	probeReplyLimit = 400
	probeEventLimit = 5000
)

// modelInfo is one row of the local model catalogue.
type modelInfo struct {
	ID      string
	Name    string
	Context int
	Output  int
	Vision  bool
	Reason  bool
}

// fallbackModels is the compiled-in catalogue.  It exists because MiniMax Code
// exposes no model-list endpoint (see README, "Known gaps"), so a machine with
// no readable config.yaml must still answer Models() with something true.
// These four rows are the vendor's own whitelist, verbatim.
var fallbackModels = []modelInfo{
	{ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview", Context: 512000, Output: 128000, Vision: true, Reason: true},
	{ID: "MiniMax-M3", Name: "M3", Context: 512000, Output: 128000, Vision: true, Reason: true},
	{ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed", Context: 200000, Output: 128000, Reason: true},
	{ID: "MiniMax-M2.7", Name: "M2.7", Context: 200000, Output: 128000, Reason: true},
}

// accountConfig is one credential declared inline in client2api.json.
type accountConfig struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Token   string `json:"access_token"`
	BaseURL string `json:"base_url"`
	Enabled *bool  `json:"enabled"`
}

// Config is this module's slice of the client2api.json "clients" object.
type Config struct {
	Accounts []accountConfig `json:"accounts"`

	BaseURL    string `json:"base_url"`
	AuthDir    string `json:"auth_dir"`
	ConfigYAML string `json:"config_yaml"`

	Models       []string `json:"models"`
	AutoDiscover *bool    `json:"auto_discover"`

	MaxTokensDefault   int `json:"max_tokens_default"`
	MaxAccountAttempts int `json:"max_account_attempts"`
	CooldownSeconds    int `json:"cooldown_seconds"`
	TimeoutSeconds     int `json:"timeout_seconds"`

	// Timezone is the IANA zone sent to the vendor's sign-in API.  It is
	// optional and normally inferred from the machine; set it when the account
	// belongs to a different zone than the host running client2api, because the
	// vendor decides which day is "today" from this value alone.
	Timezone string `json:"timezone"`

	// RefreshEnabled gates the OAuth refresh path.  It defaults to on: the
	// desktop client and this module share one token file, and an expired
	// token would otherwise need a manual re-login in the GUI.
	RefreshEnabled *bool  `json:"refresh_enabled"`
	OAuthTokenURL  string `json:"oauth_token_url"`
	OAuthClientID  string `json:"oauth_client_id"`
	// OAuthDeviceURL overrides where the panel's device-code sign-in starts.
	// Empty means the token URL's own origin + /oauth2/device/code, which is
	// what the desktop client builds; set it only for a non-standard host.
	OAuthDeviceURL string `json:"oauth_device_url"`

	TLSProfile  string `json:"tls_profile"`
	TLSProtocol string `json:"tls_protocol"`
}

// loadConfig never returns an error.  A module that still starts and reports
// why it is degraded is more useful than one that refuses to exist because a
// field was misspelled.
func loadConfig(raw json.RawMessage, logf func(string, ...any)) *Config {
	cfg := &Config{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, cfg); err != nil {
			logf("minimaxcode: config is not valid JSON (%v); using defaults", err)
			cfg = &Config{}
		}
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.MaxTokensDefault <= 0 {
		cfg.MaxTokensDefault = defaultMaxTokens
	}
	if cfg.MaxAccountAttempts <= 0 {
		cfg.MaxAccountAttempts = defaultMaxAccountTries
	}
	if cfg.CooldownSeconds <= 0 {
		cfg.CooldownSeconds = defaultCooldownSeconds
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = defaultTimeoutSeconds
	}
	if strings.TrimSpace(cfg.OAuthTokenURL) == "" {
		cfg.OAuthTokenURL = defaultOAuthTokenURL
	}
	if strings.TrimSpace(cfg.OAuthClientID) == "" {
		cfg.OAuthClientID = oauthClientIDDesktop
	}
	return cfg
}

func (c *Config) autoDiscover() bool {
	if c.AutoDiscover == nil {
		return true
	}
	return *c.AutoDiscover
}

func (c *Config) refreshEnabled() bool {
	if c.RefreshEnabled == nil {
		return true
	}
	return *c.RefreshEnabled
}

func (c *Config) timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return time.Duration(defaultTimeoutSeconds) * time.Second
}

// oauthTokenURL is the endpoint that trades a refresh token for a fresh access
// token.  The mainland build uses account.minimax.cn and the overseas build
// account.minimax.io; both were observed, so the host stays configurable
// instead of being welded to one region.
func (c *Config) oauthTokenURL() string {
	if p := strings.TrimSpace(c.OAuthTokenURL); p != "" {
		return p
	}
	return defaultOAuthTokenURL
}

// oauthDeviceURL is where the panel's device-code sign-in starts.  An explicit
// override wins; otherwise it is derived from the token endpoint so that
// pointing this module at the international build moves both endpoints at
// once.  Deriving rather than hard-coding matters because the two builds sit
// on different hosts (account.minimax.cn and account.minimax.io) and a
// credential minted against one is not accepted by the other.
func (c *Config) oauthDeviceURL() string {
	if p := strings.TrimSpace(c.OAuthDeviceURL); p != "" {
		return p
	}
	if u, err := url.Parse(c.oauthTokenURL()); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/oauth2/device/code"
	}
	return defaultOAuthDeviceURL
}

// oauthRegion names which of the two account services a credential belongs
// to.  The region is not a preference -- it is part of the account id, and a
// row labelled with the wrong one would be compared against the wrong realm
// during discovery -- so it is read off the host this module actually talks
// to rather than from a separate setting that could disagree with it.
func (c *Config) oauthRegion() string {
	for _, endpoint := range []string{c.oauthTokenURL(), c.oauthDeviceURL()} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" {
			continue
		}
		host := strings.ToLower(u.Host)
		if strings.Contains(host, "minimax.io") || strings.Contains(host, "-overseas") {
			return "en"
		}
		if strings.Contains(host, "minimax") || strings.Contains(host, "xaminim") {
			return "cn"
		}
	}
	return "cn"
}

// maxAccountAttempts bounds how many accounts one Chat may try.  It falls back
// to the built-in default when unset, so a sparse config file cannot turn a
// single request into an unbounded sweep of the pool.
func (c *Config) maxAccountAttempts() int {
	if c.MaxAccountAttempts > 0 {
		return c.MaxAccountAttempts
	}
	return defaultMaxAccountTries
}

func (c *Config) tlsProfile() fingerprint.Profile {
	return fingerprint.Profile(strings.TrimSpace(c.TLSProfile))
}

func (c *Config) tlsProtocol() fingerprint.Protocol {
	return fingerprint.Protocol(strings.TrimSpace(c.TLSProtocol))
}

// signinTimezone is the IANA zone reported to the vendor's sign-in API.
//
// An explicit setting always wins.  Otherwise the machine's own zone is used,
// because the vendor computes "today" in the zone it is handed: probing a
// +08:00 account with Europe/London came back with no day marked today and day
// 1 reading Disabled, so the wrong zone is not a cosmetic problem.
func (c *Config) signinTimezone() string {
	if tz := strings.TrimSpace(c.Timezone); tz != "" {
		return tz
	}
	return localSigninTimezone()
}

// authDir resolves the directory that holds <buildEnv>/<region>/<clientId>.
func (c *Config) authDir() string {
	if p := strings.TrimSpace(c.AuthDir); p != "" {
		return expandHome(p)
	}
	return filepath.Join(homeDir(), filepath.FromSlash(defaultAuthDir))
}

// configYAMLPath resolves the desktop client's own configuration file.
func (c *Config) configYAMLPath() string {
	if p := strings.TrimSpace(c.ConfigYAML); p != "" {
		return expandHome(p)
	}
	return filepath.Join(homeDir(), filepath.FromSlash(defaultConfigYAML))
}

// messagesURL joins a base URL with the Messages path.  It tolerates a base
// that already carries the /v1 suffix, which is how the vendor's own
// config.yaml spells it.
func messagesURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = defaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	switch {
	case strings.HasSuffix(base, messagesPath):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/messages"
	default:
		return base + messagesPath
	}
}

// homeDir is the user's home directory, or "" when it cannot be determined.
// Callers treat "" as "no discovered credentials" rather than an error.
func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

// expandHome turns a leading "~" into the user's home directory.
func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

// parseModelCatalogue reads the subset of the desktop client's config.yaml
// that describes models.
//
// It is a deliberately tiny line scanner, not a YAML parser: this project does
// not take a YAML dependency for one file, and the only things needed are the
// whitelist order, each model's display name, and its two limit numbers.
// Anything it cannot understand is skipped; an empty result makes the caller
// fall back to the compiled-in table.  It never panics and never errors.
func parseModelCatalogue(raw string) []modelInfo {
	var (
		modelsIndent     = -1
		cur              *modelInfo
		limitIndent      = -1
		seen             []*modelInfo
		index            = map[string]*modelInfo{}
		whitelistIndent  = -1
		modelOrderIndent = -1
		whitelist        []string
		modelOrder       []string
	)

	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := lineIndent(line)

		// List items belong to the most recently opened list key that is
		// shallower than them.
		if item, ok := strings.CutPrefix(trimmed, "- "); ok {
			item = unquoteYAML(strings.TrimSpace(item))
			switch {
			case whitelistIndent >= 0 && indent > whitelistIndent:
				whitelist = append(whitelist, item)
			case modelOrderIndent >= 0 && indent > modelOrderIndent:
				modelOrder = append(modelOrder, item)
			}
			continue
		}

		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}

		// A key at or above a section's own indent closes that section.
		if modelsIndent >= 0 && indent <= modelsIndent && key != "models" {
			modelsIndent, cur, limitIndent = -1, nil, -1
		}
		if whitelistIndent >= 0 && indent <= whitelistIndent && key != "whitelist" {
			whitelistIndent = -1
		}
		if modelOrderIndent >= 0 && indent <= modelOrderIndent && key != "model_order" {
			modelOrderIndent = -1
		}

		switch {
		case key == "models" && value == "":
			modelsIndent, cur, limitIndent = indent, nil, -1
		case key == "whitelist" && value == "":
			whitelistIndent = indent
		case key == "model_order" && value == "":
			modelOrderIndent = indent
		case modelsIndent >= 0 && indent == modelsIndent+2 && value == "":
			// A model id.  Its block is everything indented further.
			m := &modelInfo{ID: unquoteYAML(key)}
			cur, limitIndent = m, -1
			seen = append(seen, m)
			index[m.ID] = m
		case cur != nil && key == "limit" && value == "":
			limitIndent = indent
		case cur != nil && limitIndent >= 0 && indent > limitIndent:
			switch key {
			case "context":
				cur.Context = atoiOr(value, 0)
			case "output":
				cur.Output = atoiOr(value, 0)
			}
		case cur != nil && indent > modelsIndent:
			switch key {
			case "name":
				cur.Name = unquoteYAML(value)
			case "reasoning":
				cur.Reason = value == "true"
			case "attachment":
				cur.Vision = value == "true"
			}
		}
	}

	order := whitelist
	if len(order) == 0 {
		order = modelOrder
	}
	if len(order) == 0 {
		out := make([]modelInfo, 0, len(seen))
		for _, m := range seen {
			out = append(out, *m)
		}
		return out
	}

	out := make([]modelInfo, 0, len(order))
	for _, id := range order {
		if m, ok := index[id]; ok {
			out = append(out, *m)
			continue
		}
		// A whitelisted id with no parsable block still belongs in the
		// catalogue; borrow the compiled-in numbers when we know them.
		if fb, ok := fallbackFor(id); ok {
			out = append(out, fb)
			continue
		}
		out = append(out, modelInfo{ID: id})
	}
	return out
}

func fallbackFor(id string) (modelInfo, bool) {
	for _, m := range fallbackModels {
		if m.ID == id {
			return m, true
		}
	}
	return modelInfo{}, false
}

func lineIndent(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' && r != '\t' {
			break
		}
		n++
	}
	return n
}

func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// catalogueRows reads the local catalogue, falling back to the compiled-in
// table and then applying the operator's own id filter.
func (c *Config) catalogueRows(logf func(string, ...any)) []modelInfo {
	rows := c.readCatalogue(logf)
	if len(rows) == 0 {
		rows = append([]modelInfo(nil), fallbackModels...)
	}
	if len(c.Models) == 0 {
		return rows
	}
	allowed := make(map[string]bool, len(c.Models))
	for _, id := range c.Models {
		allowed[strings.TrimSpace(id)] = true
	}
	filtered := make([]modelInfo, 0, len(rows))
	for _, m := range rows {
		if allowed[m.ID] {
			filtered = append(filtered, m)
		}
	}
	return filtered
}

func (c *Config) readCatalogue(logf func(string, ...any)) []modelInfo {
	path := c.configYAMLPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logf("minimaxcode: cannot read %s (%v); using the compiled-in model table", path, err)
		}
		return nil
	}
	return parseModelCatalogue(string(raw))
}

// coreModels converts the catalogue to the gateway's shape.
func coreModels(rows []modelInfo) []core.Model {
	out := make([]core.Model, 0, len(rows))
	for _, m := range rows {
		extra := map[string]any{"source": "local-config"}
		if m.Name != "" {
			extra["display_name"] = m.Name
		}
		if m.Context > 0 {
			extra["context_length"] = m.Context
		}
		if m.Output > 0 {
			extra["max_completion_tokens"] = m.Output
		}
		extra["vision"] = m.Vision
		extra["reasoning"] = m.Reason
		out = append(out, core.Model{ID: m.ID, OwnedBy: name, Extra: extra})
	}
	return out
}
