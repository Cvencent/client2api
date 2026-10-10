package modelmeta

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
	"time"
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
// DeepSeek's published rate varies by request time, so this compatibility
// entry point returns the off-peak rate used for model-list display.
func DefaultPrice(client, model string) (Price, bool) {
	return DefaultPriceAt(client, model, time.Time{})
}

// DefaultPriceAt resolves an official price applicable at at. An unset or zero
// time is treated as off-peak, matching DefaultPrice's display contract.
func DefaultPriceAt(client, model string, at time.Time) (Price, bool) {
	if explicitFreePriceID(model) {
		return Price{HasPrice: true}, true
	}
	rawModel := strings.ToLower(strings.TrimSpace(model))
	key := normalizePriceID(model)
	if key == "" {
		return Price{}, false
	}
	// Resolve a client-specific opaque alias before consulting either price
	// source. Some aliases point at an official id (Raccoon's sn-deepseek...),
	// and the official DeepSeek rate must win over the third-party aggregate
	// row that may share the normalized id.
	if officialKey, ok := deepSeekOfficialPriceKey(client, model); ok {
		p, _ := deepSeekOfficialPrice(officialKey, at)
		return p, true
	}
	aliases := clientPriceAliases[strings.ToLower(strings.TrimSpace(client))]
	target := aliases[rawModel]
	if target == "" {
		target = aliases[key]
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
	if target != "" {
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

// IsOfficialDeepSeekModel reports whether the client/model pair resolves to
// one of the built-in DeepSeek list-price rows. It is used by the one-time
// usage-history migration, which must not rewrite unrelated third-party
// prices just because they happen to share a DeepSeek-looking model name.
func IsOfficialDeepSeekModel(client, model string) bool {
	if explicitFreePriceID(model) {
		return false
	}
	_, ok := deepSeekOfficialPriceKey(client, model)
	return ok
}

// DeepSeekPreviousPrice returns the third-party aggregate row that the built-in
// table used before the official DeepSeek CNY override was added. It exists
// only for the one-time usage-history migration, which must distinguish an old
// automatic estimate from an operator's manual price.
func DeepSeekPreviousPrice(client, model string) (Price, bool) {
	if !IsOfficialDeepSeekModel(client, model) {
		return Price{}, false
	}
	rawModel := strings.ToLower(strings.TrimSpace(model))
	key := normalizePriceID(model)
	aliases := clientPriceAliases[strings.ToLower(strings.TrimSpace(client))]
	target := aliases[rawModel]
	if target == "" {
		target = aliases[key]
	}
	table := pricingTable()
	for _, candidate := range []string{target, key} {
		if candidate == "" {
			continue
		}
		if p, ok := table[candidate]; ok {
			return p, true
		}
		if dashed := strings.ReplaceAll(candidate, ".", "-"); dashed != candidate {
			if p, ok := table[dashed]; ok {
				return p, true
			}
		}
	}
	return Price{}, false
}

func deepSeekOfficialPriceKey(client, model string) (string, bool) {
	rawModel := strings.ToLower(strings.TrimSpace(model))
	key := normalizePriceID(model)
	if key == "" {
		return "", false
	}
	aliases := clientPriceAliases[strings.ToLower(strings.TrimSpace(client))]
	target := aliases[rawModel]
	if target == "" {
		target = aliases[key]
	}
	if target != "" {
		if _, ok := deepSeekOfficialPrices[target]; ok {
			return target, true
		}
	}
	if _, ok := deepSeekOfficialPrices[key]; ok {
		return key, true
	}
	return "", false
}

type deepSeekPrice struct {
	offPeak Price
	peak    Price
}

var deepSeekOfficialPrices = map[string]deepSeekPrice{
	"deepseek-flash": {
		offPeak: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		peak:    Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
	},
	"deepseek-v4-flash": {
		offPeak: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		peak:    Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
	},
	"deepseek-v4-flash-vision-exp": {
		offPeak: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		peak:    Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
	},
	"deepseek-v4.1-flash": {
		offPeak: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		peak:    Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
	},
	"deepseek-v4-pro": {
		offPeak: Price{InputPerMillion: 4.5, OutputPerMillion: 13.5, CacheReadPerMillion: 0.15, HasPrice: true, HasCacheRead: true},
		peak:    Price{InputPerMillion: 9, OutputPerMillion: 27, CacheReadPerMillion: 0.3, HasPrice: true, HasCacheRead: true},
	},
}

func deepSeekOfficialPrice(key string, at time.Time) (Price, bool) {
	p, ok := deepSeekOfficialPrices[key]
	if !ok {
		return Price{}, false
	}
	if at.IsZero() || !deepSeekPeak(at) {
		return p.offPeak, true
	}
	return p.peak, true
}

var deepSeekBeijing = time.FixedZone("Asia/Shanghai", 8*60*60)

// deepSeekPeak reports whether at is in DeepSeek's peak window. The published
// schedule is Beijing time, weekdays, excluding Chinese public holidays.
func deepSeekPeak(at time.Time) bool {
	t := at.In(deepSeekBeijing)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday || deepSeekHoliday(t) {
		return false
	}
	minute := t.Hour()*60 + t.Minute()
	return (minute >= 9*60 && minute < 12*60) || (minute >= 14*60 && minute < 18*60)
}

func deepSeekHoliday(t time.Time) bool {
	day := t.Format("2006-01-02")
	_, ok := deepSeekHolidays[day]
	return ok
}

// Public holiday dates are the mainland China State Council schedule. They are
// kept explicitly because Go's tz database has no concept of statutory
// holidays, and inferring them from weekends would misprice make-up workdays.
var deepSeekHolidays = map[string]struct{}{
	"2026-01-01": {}, "2026-01-02": {}, "2026-01-03": {},
	"2026-02-15": {}, "2026-02-16": {}, "2026-02-17": {}, "2026-02-18": {}, "2026-02-19": {}, "2026-02-20": {}, "2026-02-21": {}, "2026-02-22": {}, "2026-02-23": {},
	"2026-04-04": {}, "2026-04-05": {}, "2026-04-06": {},
	"2026-05-01": {}, "2026-05-02": {}, "2026-05-03": {}, "2026-05-04": {}, "2026-05-05": {},
	"2026-06-19": {}, "2026-06-20": {}, "2026-06-21": {},
	"2026-09-25": {}, "2026-09-26": {}, "2026-09-27": {},
	"2026-10-01": {}, "2026-10-02": {}, "2026-10-03": {}, "2026-10-04": {}, "2026-10-05": {}, "2026-10-06": {}, "2026-10-07": {},
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
