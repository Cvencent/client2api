package modelmeta

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

// Price is a per-million-token price in CNY.
//
// HasPrice distinguishes an explicitly free model from an unknown price. The
// latter must never be rendered as ¥0 because that would claim a free call.
type Price struct {
	InputPerMillion     float64 `json:"input_per_million,omitempty"`
	OutputPerMillion    float64 `json:"output_per_million,omitempty"`
	CacheReadPerMillion float64 `json:"cache_read_per_million,omitempty"`
	HasPrice            bool    `json:"has_price,omitempty"`
	HasCacheRead        bool    `json:"has_cache_read,omitempty"`
}

// IsZero reports whether no price decision is present.
func (p Price) IsZero() bool { return !p.HasPrice && !p.HasCacheRead }

//go:embed pricing.json
var pricingJSON []byte

type priceDoc struct {
	Meta struct {
		Currency       string  `json:"currency"`
		SourceCurrency string  `json:"source_currency"`
		Unit           string  `json:"unit"`
		Source         string  `json:"source"`
		SourceDate     string  `json:"source_date"`
		USDtoCNY       float64 `json:"usd_to_cny"`
	} `json:"_meta"`
	Prices map[string]struct {
		Provider  string   `json:"provider"`
		ID        string   `json:"id"`
		Input     *float64 `json:"input"`
		Output    *float64 `json:"output"`
		CacheRead *float64 `json:"cache_read"`
	} `json:"prices"`
}

var (
	pricingOnce sync.Once
	pricingTab  map[string]Price
	pricingInfo struct {
		Currency   string
		Unit       string
		Source     string
		SourceDate string
		USDtoCNY   float64
	}
)

func pricingTable() map[string]Price {
	pricingOnce.Do(func() {
		pricingTab = map[string]Price{}
		var doc priceDoc
		if err := json.Unmarshal(pricingJSON, &doc); err != nil {
			return
		}
		pricingInfo = struct {
			Currency   string
			Unit       string
			Source     string
			SourceDate string
			USDtoCNY   float64
		}{doc.Meta.Currency, doc.Meta.Unit, doc.Meta.Source, doc.Meta.SourceDate, doc.Meta.USDtoCNY}
		rate := doc.Meta.USDtoCNY
		if rate <= 0 {
			rate = 1
		}
		for id, e := range doc.Prices {
			if e.Input == nil || e.Output == nil {
				continue
			}
			p := Price{
				InputPerMillion:  *e.Input * rate,
				OutputPerMillion: *e.Output * rate,
				HasPrice:         true,
			}
			if e.CacheRead != nil {
				p.CacheReadPerMillion = *e.CacheRead * rate
				p.HasCacheRead = true
			}
			pricingTab[strings.ToLower(id)] = p
		}
	})
	return pricingTab
}

// DefaultPrice resolves an official default price for a client and model id.
func DefaultPrice(client, model string) (Price, bool) {
	if explicitFreePriceID(model) {
		return Price{HasPrice: true}, true
	}
	key := normalizePriceID(model)
	if key == "" {
		return Price{}, false
	}
	table := pricingTable()
	if p, ok := table[key]; ok {
		return p, true
	}
	if dashed := strings.ReplaceAll(key, ".", "-"); dashed != key {
		if p, ok := table[dashed]; ok {
			return p, true
		}
	}
	// A few reseller ids are opaque aliases. Only map an alias when the
	// client's catalogue makes the target unambiguous.
	aliases := clientPriceAliases[strings.ToLower(strings.TrimSpace(client))]
	if target, ok := aliases[key]; ok {
		if p, ok := table[target]; ok {
			return p, true
		}
		if dashed := strings.ReplaceAll(target, ".", "-"); dashed != target {
			if p, ok := table[dashed]; ok {
				return p, true
			}
		}
	}
	return Price{}, false
}

var clientPriceAliases = map[string]map[string]string{
	"qoder": {
		"qmodel_38max":  "qwen3.8-max",
		"qfmodel":       "qwen3.8-flash",
		"qmodel_latest": "qwen3.7-max",
		"qmodel":        "qwen3.7-plus",
		"q37fmodel":     "qwen3.7-flash",
		"dmodel":        "deepseek-v4-pro",
		"dfmodel":       "deepseek-v4-flash",
		"gmodel":        "glm-5.3",
		"gfmodel":       "glm-5.3-flash",
		"gm51model":     "glm-5.2",
		"kmodel_latest": "kimi-k3",
		"kmodel":        "kimi-k2.8-preview",
		"mmodel":        "minimax-m2.7",
	},
	"workbuddy": {
		"cn:glm-5.2":                 "glm-5.2",
		"cn:glm-5.1":                 "glm-5.1",
		"cn:hy3":                     "hy3",
		"cn:deepseek-v4-pro":         "deepseek-v4-pro",
		"cn:deepseek-v4-flash":       "deepseek-v4-flash",
		"cn:deepseek-v4.1-flash":     "deepseek-v4.1-flash",
		"cn:glm-5.3":                 "glm-5.3",
		"cn:glm-5.3-flash":           "glm-5.3-flash",
		"cn:minimax-m3":              "minimax-m3",
		"cn:glm-4.6v":                "glm-4.6v",
		"cn:glm-4.7":                 "glm-4.7",
		"cn:kimi-k2.6":               "kimi-k2.6",
		"cn:kimi-k2.5":               "kimi-k2.5",
		"cn:minimax-m2.5":            "minimax-m2.5",
		"cn:minimax-m2.7":            "minimax-m2.7",
		"cn:glm-4.6":                 "glm-4.6",
		"cn:glm-5v-turbo":            "glm-5v-turbo",
		"cn:hy4-preview":             "hy4-preview",
		"global:hy4-preview":         "hy4-preview",
		"global:hy3":                 "hy3",
		"global:deepseek-v4.1-flash": "deepseek-v4.1-flash",
		"global:gpt-5.5":             "gpt-5.5",
		"global:gpt-5.4":             "gpt-5.4",
		"global:gemini-3.5-flash":    "gemini-3.5-flash",
		"global:glm-5.3-flash":       "glm-5.3-flash",
		"global:glm-5.2":             "glm-5.2",
		"global:gpt-5.6-terra":       "gpt-5.6-terra",
		"global:kimi-k2.6":           "kimi-k2.6",
		"global:gpt-6-astra":         "gpt-6-astra",
		"global:kimi-k3":             "kimi-k3",
		"global:glm-5.3":             "glm-5.3",
		"global:gpt-5.6-luna":        "gpt-5.6-luna",
		"global:gpt-5.3-codex":       "gpt-5.3-codex",
	},
	"tabbit": {
		"mimo-v2.6-pro":         "mimo-v2.6-pro",
		"mimo-v2.6-flash":       "mimo-v2.6-flash",
		"longcat-2.0":           "longcat-2.0",
		"doubao-seed-2-1-pro":   "doubao-seed-2-1-pro-260628",
		"doubao-seed-2-1-turbo": "doubao-seed-2-1-turbo-260628",
		"doubao-seed-2.0-lite":  "doubao-seed-2.0-lite",
		"doubao-seed-2-0-lite":  "doubao-seed-2.0-lite",
	},
	"raccoon": {
		"sn-glm-5-3":             "glm-5.3",
		"sn-glm-5-3-flash":       "glm-5.3-flash",
		"sn-kimi-k3":             "kimi-k3",
		"sn-deepseek-v4-1-flash": "deepseek-v4.1-flash",
	},
	"loomy": {
		"qwen-3.8-max": "qwen3.8-max",
		"mimo-v2.5":    "mimo-v2.5",
	},
	"qwenwork": {
		"qwen3.8-max-preview": "qwen3.8-max",
	},
	"molly": {
		"gpt-6-luna-low":     "gpt-6-luna",
		"gpt-6-astra-fast":   "gpt-6-astra",
		"gpt-6.1-sol-fast":   "gpt-6.1-sol",
		"gpt-5.6-sol-fast":   "gpt-5.6-sol",
		"gpt-5.6-luna-low":   "gpt-5.6-luna",
		"gpt-5.6-terra-fast": "gpt-5.6-terra",
	},
	"lobsterai": {
		"doubao-seed-2-1-pro-260915": "doubao-seed-2-1-pro-260628",
		"qwen3.5-plus-2026-04-20":    "qwen3.5-plus",
		"minimax-m3.1-flash-preview": "minimax-m3.1-flash-preview",
	},
}

func normalizePriceID(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		model = model[i+1:]
	}
	model = strings.TrimPrefix(model, "~")
	for _, suffix := range []string{":batch", ":free"} {
		if strings.HasSuffix(model, suffix) {
			model = strings.TrimSuffix(model, suffix)
			break
		}
	}
	if i := strings.Index(model, ":"); i > 0 && i+1 < len(model) {
		model = model[i+1:]
	}
	return model
}

// explicitFreePriceID recognizes catalog ids that declare their price tier in
// the name. Free routes are a real zero-cost decision, not a missing price, so
// they still satisfy HasPrice without borrowing the paid model's price.
func explicitFreePriceID(model string) bool {
	raw := strings.ToLower(strings.TrimSpace(model))
	if raw == "" {
		return false
	}
	if strings.HasSuffix(raw, ":free") {
		return true
	}
	base := raw
	if i := strings.LastIndex(base, "/"); i >= 0 && i+1 < len(base) {
		base = base[i+1:]
	}
	return strings.HasPrefix(raw, "cline-free/") || strings.HasSuffix(base, "-free") || base == "free"
}

// mergePrice fills price fields when the resolved metadata has no manual or
// vendor price. It records the source field-by-field so the panel can explain
// where the number came from.
func mergePrice(dst Meta, p Price, source string) Meta {
	if p.IsZero() {
		return dst
	}
	if dst.FieldSources == nil {
		dst.FieldSources = map[string]string{}
	}
	if p.HasPrice && !dst.HasPrice {
		dst.InputPerMillion = p.InputPerMillion
		dst.OutputPerMillion = p.OutputPerMillion
		dst.HasPrice = true
		dst.FieldSources[FieldInputPrice] = source
		dst.FieldSources[FieldOutputPrice] = source
	}
	if p.HasCacheRead && !dst.HasCacheRead {
		dst.CacheReadPerMillion = p.CacheReadPerMillion
		dst.HasCacheRead = true
		dst.FieldSources[FieldCacheReadPrice] = source
	}
	dst.Source = strongestSource(dst.FieldSources)
	return dst
}
