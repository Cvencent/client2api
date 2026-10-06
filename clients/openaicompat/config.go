// Package openaicompat implements a generic OpenAI-compatible client module
// for client2api.  One module serves many providers (Groq, Cerebras,
// SiliconFlow, ...): each provider is a row in the module's config, carrying
// its own base URL, API key and optional model list, and each account maps to
// exactly one provider.  This mirrors OmniRoute's registry pattern, where a
// lightweight source is pure configuration, not bespoke code.
package openaicompat

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const clientName = "openai-compat"

// Default durations, overridable from the module config.
const (
	defaultChatTimeout   = 10 * time.Minute
	defaultModelsTimeout = 30 * time.Second
	defaultStreamIdle    = 90 * time.Second
	defaultModelsTTL     = 10 * time.Minute
	defaultCooldown      = 60 * time.Second
	defaultRateCooldown  = 2 * time.Minute
	defaultQuotaCooldown = 30 * time.Minute
	defaultAuthCooldown  = 30 * time.Minute
	defaultInFlight      = 4
)

// ProviderConfig is one upstream source: a base URL, a credential and the
// models that credential can reach.  It is the unit of configuration: adding
// a new free source is a config edit, not a code change.
type ProviderConfig struct {
	// ID is the routing prefix under this module ("openai-compat/groq/...").
	// It also selects the built-in defaults for unknown fields.
	ID string `json:"id"`
	// BaseURL is the provider's OpenAI-compatible API root, without a
	// trailing slash.  An empty value resolves through the built-in table.
	BaseURL string `json:"base_url"`
	// APIKey is the bearer credential sent on every request.
	APIKey string `json:"api_key"`
	// Label is an optional display name for the panel.
	Label string `json:"label"`
	// Disabled parks the provider without deleting it.
	Disabled bool `json:"disabled"`
	// Models narrows the served catalogue when non-empty; an empty list
	// serves the built-in defaults for the id (or nothing for an unknown id
	// unless extra_models fills it).
	Models []string `json:"models"`
	// ExtraHeaders are appended to every request to this provider.
	ExtraHeaders map[string]string `json:"extra_headers"`

	// Source records whether this row came from the config file or from the
	// panel (accounts.json).  It is set at merge time and never serialised,
	// so a config file cannot forge it.
	Source string `json:"-"`

	// dedupeKey is computed by normalize and used to collapse duplicate rows
	// (same id and same key).
	dedupeKey string
}

// source reports where a row came from.  An empty Source is treated as a
// config-file row, which is the only other possibility.
func (p ProviderConfig) source() string {
	if p.Source == sourcePanel {
		return sourcePanel
	}
	return sourceConfig
}

// Config is the `clients.openai-compat` object.
type Config struct {
	Providers []ProviderConfig `json:"providers"`

	ChatTimeout       string `json:"chat_timeout"`
	ModelsTimeout     string `json:"models_timeout"`
	StreamIdleTimeout string `json:"stream_idle_timeout"`
	ModelsTTL         string `json:"models_ttl"`
	Cooldown          string `json:"cooldown"`
	RateCooldown      string `json:"rate_cooldown"`
	QuotaCooldown     string `json:"quota_cooldown"`
	AuthCooldown      string `json:"auth_cooldown"`

	MaxInFlight int `json:"max_in_flight"`

	// MaxTokensField selects which spelling of the output cap is sent.  The
	// default is `max_tokens`, which every mainstream provider accepts;
	// "max_completion_tokens" is the OpenRouter spelling.
	MaxTokensField string `json:"max_tokens_field"`

	// ExtraModels are appended to the built-in catalogue for every provider.
	ExtraModels []string `json:"extra_models"`
}

// parseConfig decodes the raw `clients.openai-compat` object.
func parseConfig(raw json.RawMessage) (Config, error) {
	var cfg Config
	if len(raw) == 0 || string(raw) == "null" {
		return cfg.normalize(), nil
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("clients.%s: %w", clientName, err)
	}
	return cfg.normalize(), nil
}

// normalize trims, defaults and de-duplicates.  It never returns an error.
func (cfg Config) normalize() Config {
	providers := make([]ProviderConfig, 0, len(cfg.Providers))
	seen := make(map[string]bool, len(cfg.Providers))
	for _, p := range cfg.Providers {
		p.ID = strings.TrimSpace(p.ID)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Label = strings.TrimSpace(p.Label)
		if p.BaseURL == "" {
			p.BaseURL = builtinBaseURL(p.ID)
		}
		if p.BaseURL == "" {
			continue
		}
		if p.ID == "" {
			continue
		}
		// A credential is required unless the upstream is on this machine.
		// That is the one case where an empty key describes the service
		// (OmniRoute answers keyless on loopback) instead of a mistake.
		if p.APIKey == "" && !isLoopbackBase(p.BaseURL) {
			continue
		}
		models := make([]string, 0, len(p.Models))
		seenModel := make(map[string]bool, len(p.Models))
		for _, m := range p.Models {
			m = strings.TrimSpace(m)
			if m == "" || seenModel[m] {
				continue
			}
			seenModel[m] = true
			models = append(models, m)
		}
		p.Models = models

		headers := make(map[string]string, len(p.ExtraHeaders))
		for k, v := range p.ExtraHeaders {
			k = strings.TrimSpace(k)
			v = strings.TrimSpace(v)
			if k == "" || v == "" || reservedHeader(k) {
				continue
			}
			headers[k] = v
		}
		if len(headers) == 0 {
			headers = nil
		}
		p.ExtraHeaders = headers

		key := p.ID + "\x00" + p.APIKey
		if seen[key] {
			continue
		}
		seen[key] = true
		p.dedupeKey = key
		providers = append(providers, p)
	}
	cfg.Providers = providers

	cfg.ExtraModels = dedupeStrings(cfg.ExtraModels)
	cfg.MaxTokensField = strings.TrimSpace(cfg.MaxTokensField)
	if cfg.MaxTokensField == "" {
		cfg.MaxTokensField = "max_tokens"
	}
	if cfg.MaxInFlight < 0 {
		cfg.MaxInFlight = 0
	}
	return cfg
}

// --- built-in provider table ----------------------------------------------

// builtinProviders maps a provider id to its documented OpenAI-compatible
// base URL.  An id not in this table must carry its own base_url in config.
var builtinProviders = map[string]string{
	"groq":        "https://api.groq.com/openai/v1",
	"cerebras":    "https://api.cerebras.ai/v1",
	"siliconflow": "https://api.siliconflow.com/v1",
	"mistral":     "https://api.mistral.ai/v1",
	"nvidia":      "https://integrate.api.nvidia.com/v1",
	"together":    "https://api.together.xyz/v1",
	"fireworks":   "https://api.fireworks.ai/inference/v1",
	"deepinfra":   "https://api.deepinfra.com/v1/openai",
	"chutes":      "https://llm.chutes.ai/v1",
	"huggingface": "https://router.huggingface.co/v1",
}

// builtinBaseURL resolves a provider id to its known API root.
func builtinBaseURL(id string) string {
	if u, ok := builtinProviders[strings.ToLower(strings.TrimSpace(id))]; ok {
		return u
	}
	return ""
}

// keyPages maps a provider id to the vendor page where an API key is created.
// It is advisory metadata for the panel: a provider missing from this table
// simply renders no "get a key" link.  Keep it next to builtinProviders so
// adding a source is still a one-file change.
var keyPages = map[string]string{
	"groq":        "https://console.groq.com/keys",
	"cerebras":    "https://cloud.cerebras.ai/",
	"siliconflow": "https://cloud.siliconflow.cn/account/ak",
	"mistral":     "https://console.mistral.ai/api-keys/",
	"nvidia":      "https://build.nvidia.com/",
	"together":    "https://api.together.ai/settings/api-keys",
	"fireworks":   "https://fireworks.ai/account/api-keys",
	"deepinfra":   "https://deepinfra.com/dash/api_keys",
	"chutes":      "https://chutes.ai/app/api-keys",
	"huggingface": "https://huggingface.co/settings/tokens",
}

// builtinKeyPage resolves a provider id to its key-creation page.
func builtinKeyPage(id string) string {
	return keyPages[strings.ToLower(strings.TrimSpace(id))]
}

// builtinModels are the cold-start model tables per provider.  They are the
// models a fresh process can serve before any live listing succeeds; they are
// deliberately modest lists of the ids the vendor documents, not exhaustive.
var builtinModels = map[string][]string{
	"groq": {
		"llama-3.3-70b-versatile",
		"meta-llama/llama-4-scout-17b-16e-instruct",
		"openai/gpt-oss-120b",
		"openai/gpt-oss-20b",
		"qwen/qwen3-32b",
		"groq/compound",
	},
	"cerebras": {
		"zai-glm-4.7",
		"gpt-oss-120b",
		"gemma-4-31b",
	},
	"siliconflow": {
		"deepseek-ai/DeepSeek-V3",
		"deepseek-ai/DeepSeek-R1",
		"Qwen/Qwen3-32B",
		"Qwen/Qwen3-Coder-30B-A3B-Instruct",
		"Qwen/Qwen2.5-72B-Instruct",
	},
	"mistral": {
		"mistral-large-latest",
		"mistral-small-latest",
		"codestral-latest",
		"ministral-8b-latest",
	},
	"nvidia": {
		"meta/llama-3.3-70b-instruct",
		"nvidia/llama-3.1-nemotron-70b-instruct",
		"deepseek-ai/deepseek-r1",
	},
	"together": {
		"meta-llama/Llama-3.3-70B-Instruct-Turbo",
		"deepseek-ai/DeepSeek-V3",
		"Qwen/Qwen2.5-72B-Instruct-Turbo",
	},
	"fireworks": {
		"accounts/fireworks/models/llama-v3p3-70b-instruct",
		"accounts/fireworks/models/deepseek-v3",
	},
	"deepinfra": {
		"meta-llama/Meta-Llama-3.3-70B-Instruct",
		"deepseek-ai/DeepSeek-V3",
		"Qwen/Qwen2.5-72B-Instruct",
	},
	"chutes": {
		"Qwen2.5-72B-Instruct",
		"deepseek-ai/DeepSeek-V3",
	},
	"huggingface": {
		"meta-llama/Llama-3.3-70B-Instruct",
		"Qwen/Qwen2.5-72B-Instruct",
	},
}

// modelsFor resolves the served list for one provider: an explicit config
// list wins; otherwise the built-in table for the id is used; otherwise the
// module-level extra_models are offered.
func (p ProviderConfig) modelsFor(cfgExtraModels []string) []string {
	if len(p.Models) > 0 {
		return p.Models
	}
	if table, ok := builtinModels[strings.ToLower(strings.TrimSpace(p.ID))]; ok {
		return table
	}
	return cfgExtraModels
}

// --- duration knobs ---------------------------------------------------------

func (cfg Config) chatTimeout() time.Duration {
	return durationOr(cfg.ChatTimeout, defaultChatTimeout)
}

func (cfg Config) modelsTimeout() time.Duration {
	return durationOr(cfg.ModelsTimeout, defaultModelsTimeout)
}

func (cfg Config) streamIdle() time.Duration {
	return durationOr(cfg.StreamIdleTimeout, defaultStreamIdle)
}

func (cfg Config) modelsTTL() time.Duration {
	return durationOr(cfg.ModelsTTL, defaultModelsTTL)
}

func (cfg Config) cooldown() time.Duration {
	return durationOr(cfg.Cooldown, defaultCooldown)
}

func (cfg Config) rateCooldown() time.Duration {
	return durationOr(cfg.RateCooldown, defaultRateCooldown)
}

func (cfg Config) quotaCooldown() time.Duration {
	return durationOr(cfg.QuotaCooldown, defaultQuotaCooldown)
}

func (cfg Config) authCooldown() time.Duration {
	return durationOr(cfg.AuthCooldown, defaultAuthCooldown)
}

// maxInFlight is the STARTUP ceiling; a live override can replace it.
func (cfg Config) maxInFlight() int {
	if cfg.MaxInFlight > 0 {
		return cfg.MaxInFlight
	}
	return defaultInFlight
}

// usesLegacyMaxTokens reports whether the caller asked for the OpenRouter
// spelling of the output cap.  The default ("max_tokens") is what every
// mainstream OpenAI-compatible provider accepts.
func (cfg Config) usesLegacyMaxTokens() bool {
	return strings.EqualFold(strings.TrimSpace(cfg.MaxTokensField), "max_tokens")
}

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

// --- per-provider URL and headers -------------------------------------------

// chatURL is the endpoint a completion is POSTed to.
func (p ProviderConfig) chatURL() string { return p.base() + "/chat/completions" }

// base trims and falls back to the built-in table.
func (p ProviderConfig) base() string {
	base := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if base == "" {
		return builtinBaseURL(p.ID)
	}
	return base
}

// reservedHeader reports whether a header may NOT be overridden through
// extra_headers: letting a config file replace Authorization would silently
// detach the credential from the request.
func reservedHeader(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "content-type", "accept", "accept-encoding",
		"user-agent", "host", "content-length", "transfer-encoding", "connection":
		return true
	}
	return false
}

// applyHeaders installs the shared and per-provider headers on a request.
func (p ProviderConfig) applyHeaders(req *request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// A keyless local upstream must not be sent a bare "Bearer " header:
	// some servers reject an empty credential outright instead of treating
	// it as an anonymous request.
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	req.Header.Set("User-Agent", "client2api/1.0 (+openai-compat)")
	for k, v := range p.ExtraHeaders {
		req.Header.Set(k, v)
	}
}

// request is a tiny alias so config.go does not import net/http (the wire
// layer lives in the chat/model files); it keeps the config surface pure.
type request = httpRequest

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
