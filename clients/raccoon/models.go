package raccoon

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// maxOutputCeiling bounds what we are willing to advertise as a per-model
// completion budget (core.ModelOutputLimit rejects anything above MaxInt32).
const maxOutputCeiling = 2147483647

// flexFloat decodes a JSON number or a numeric string into a float64. The
// vendor's billing multipliers arrive as numbers in the catalogue but as
// quoted strings in some exports.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	s = strings.Trim(s, `"`)
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// An unparseable multiplier is treated as absent rather than as an
		// error: one bad field must not blank the whole catalogue.
		return nil
	}
	*f = flexFloat(v)
	return nil
}

func (f *flexFloat) ptr() *float64 {
	if f == nil {
		return nil
	}
	v := float64(*f)
	return &v
}

// catalogModel is one entry of `data.categories[].models[]`.
type catalogModel struct {
	Name                       string      `json:"name"`
	Visible                    *bool       `json:"visible"`
	Description                string      `json:"description"`
	BillingEffectiveMultiplier *flexFloat  `json:"billing_effective_multiplier"`
	BillingMultiplier          *flexFloat  `json:"billing_multiplier"`
	BillingStatus              string      `json:"billing_status"`
	BillingStatusNote          string      `json:"billing_status_note"`
	ContextWindow              json.Number `json:"context_window"`
	Tags                       []string    `json:"tags"`
	Params                     struct {
		ContextWindow json.Number `json:"context_window"`
		MaxTokens     json.Number `json:"max_tokens"`
	} `json:"params"`
}

type catalogEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Categories []struct {
			Type   string         `json:"type"`
			Models []catalogModel `json:"models"`
		} `json:"categories"`
	} `json:"data"`
}

// formatMultiplier renders a multiplier the way the vendor's own UI does:
// at most four decimals, trailing zeros stripped (0.75 → "0.75", 1 → "1",
// 0.1 → "0.1").
func formatMultiplier(v float64) string {
	r := math.Round(v*10000) / 10000
	return strconv.FormatFloat(r, 'f', -1, 64)
}

// displayName builds the model label shown in the picker.
//
// The rules, in order:
//
//	a. a non-finite or negative effective multiplier appends NO suffix at all
//	   (never emit a dangling " · ");
//	b. an effective multiplier of 0 renders as "免费", never "x0";
//	c. when the base multiplier is finite, positive and strictly greater than
//	   the effective one, both are shown: "x<base>→x<effective>";
//	d. otherwise the effective multiplier is shown — INCLUDING 1×.
//
// Rule (d) is load-bearing: omitting the 1× case was a real user-reported
// defect, because the user cannot distinguish "it is 1×" from "we failed to
// read the multiplier".
func displayName(id, description string, effective, base *float64) string {
	name := strings.TrimSpace(description)
	if name == "" {
		name = strings.TrimSpace(id)
	}
	if effective == nil || math.IsNaN(*effective) || math.IsInf(*effective, 0) || *effective < 0 {
		return name
	}
	eff := *effective
	if eff == 0 {
		return name + " · 免费"
	}
	if base != nil && !math.IsNaN(*base) && !math.IsInf(*base, 0) && *base > 0 && *base > eff {
		return name + " · x" + formatMultiplier(*base) + "→x" + formatMultiplier(eff)
	}
	return name + " · x" + formatMultiplier(eff)
}

// stripMultiplier removes the " · …" billing suffix, so a caller may pass a
// display name straight back as a model id.
func stripMultiplier(name string) string {
	if i := strings.Index(name, " · "); i >= 0 {
		return strings.TrimSpace(name[:i])
	}
	return strings.TrimSpace(name)
}

func positiveInt(n json.Number) int {
	if n == "" {
		return 0
	}
	v, err := strconv.ParseFloat(string(n), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > maxOutputCeiling {
		return 0
	}
	return int(v)
}

func catalogVisible(m catalogModel) bool { return m.Visible == nil || *m.Visible }

func catalogTags(tags []string) (vision, reasoning bool) {
	for _, t := range tags {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "vision", "image", "image-understanding":
			vision = true
		case "reasoning", "thinking":
			reasoning = true
		}
	}
	return
}

// parseCatalog turns a `/model_catalog` response body into models. It never
// returns an error: a catalogue failure must degrade to the built-in table,
// not break the module.
func parseCatalog(body []byte) []core.Model {
	var env catalogEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil
	}
	if env.Code != 0 {
		return nil
	}
	var out []core.Model
	seen := map[string]bool{}
	for _, cat := range env.Data.Categories {
		if !strings.EqualFold(strings.TrimSpace(cat.Type), "chat") {
			continue
		}
		for _, m := range cat.Models {
			id := strings.TrimSpace(m.Name)
			if id == "" || !catalogVisible(m) || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, catalogModelToModel(m))
		}
	}
	return out
}

func catalogModelToModel(m catalogModel) core.Model {
	id := strings.TrimSpace(m.Name)
	ctx := positiveInt(m.Params.ContextWindow)
	if ctx == 0 {
		ctx = positiveInt(m.ContextWindow)
	}
	maxOut := positiveInt(m.Params.MaxTokens)
	vision, reasoning := catalogTags(m.Tags)
	eff := m.BillingEffectiveMultiplier.ptr()
	base := m.BillingMultiplier.ptr()

	extra := map[string]any{
		"display_name": displayName(id, m.Description, eff, base),
	}
	if ctx > 0 {
		extra["context_length"] = ctx
	}
	if maxOut > 0 {
		extra["max_output_tokens"] = maxOut
	}
	if vision {
		extra["vision"] = true
	}
	if reasoning {
		extra["reasoning"] = true
	}
	if eff != nil {
		extra["billing_effective_multiplier"] = *eff
	}
	if base != nil {
		extra["billing_multiplier"] = *base
	}
	if s := strings.TrimSpace(m.BillingStatus); s != "" {
		extra["billing_status"] = s
	}
	if s := strings.TrimSpace(m.BillingStatusNote); s != "" {
		extra["billing_status_note"] = s
	}
	return core.Model{ID: id, OwnedBy: "raccoon", Extra: extra}
}

// builtinModel is one row of the table measured from the vendor's catalogue
// (the six `visible:true` chat models, in remote order). It is the answer
// `Models()` gives before a live catalogue has ever been fetched, so a
// caller can always discover the ids.
type builtinModel struct {
	ID             string
	Description    string
	ContextWindow  int
	MaxTokens      int
	Effective      float64
	Base           float64
	SupportsImage  bool
	SupportsReason bool
}

var builtinModels = []builtinModel{
	{"sn-sensenova-6-8-flash", "SenseNova-6.8-Flash", 256000, 63999, 0, 0.5, true, true},
	{"sn-sensenova-6-8-flash-lite", "SenseNova-6.8-Flash-Lite", 256000, 63999, 0, 0.5, true, true},
	{"sn-glm-5-3", "GLM-5-3", 1000000, 100000, 0.75, 0.75, true, true},
	{"sn-kimi-k3", "Kimi-K3", 1000000, 100000, 1, 1, true, true},
	{"sn-glm-5-3-flash", "GLM-5-3-Flash", 1000000, 100000, 0.1, 0.2, false, false},
	{"sn-deepseek-v4-1-flash", "DeepSeek-V4.1-Flash", 1000000, 100000, 0.25, 0.25, false, false},
}

func fallbackModels() []core.Model {
	out := make([]core.Model, 0, len(builtinModels))
	for _, b := range builtinModels {
		eff, base := b.Effective, b.Base
		extra := map[string]any{
			"display_name":      displayName(b.ID, b.Description, &eff, &base),
			"context_length":    b.ContextWindow,
			"max_output_tokens": b.MaxTokens,
			"fallback":          true,
		}
		if b.SupportsImage {
			extra["vision"] = true
		}
		if b.SupportsReason {
			extra["reasoning"] = true
		}
		out = append(out, core.Model{ID: b.ID, OwnedBy: "raccoon", Extra: extra})
	}
	return out
}
