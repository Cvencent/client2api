package autoclaw

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"client2api/internal/core"
)

const maxOutputCeiling = 2147483647

type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), "\"")
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
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

// catalogEnvelope is the AutoClaw model-config response.  The live document
// is `data.models[]`; each entry follows the desktop client's normalized
// catalogue shape.
type catalogEnvelope struct {
	Code int `json:"code"`
	Data struct {
		Models []catalogModel `json:"models"`
	} `json:"data"`
}

type catalogModel struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	API               string          `json:"api"`
	Reasoning         bool            `json:"reasoning"`
	Input             []string        `json:"input"`
	ContextWindow     json.Number     `json:"contextWindow"`
	MaxTokens         json.Number     `json:"maxTokens"`
	Price             *flexFloat      `json:"price"`
	Tooltip           string          `json:"tooltip"`
	IconURL           string          `json:"iconURL"`
	CreditConsumption string          `json:"creditConsumptionLevel"`
	ReasoningLevel    []string        `json:"reasoningLevel"`
	ReasoningLevels   []string        `json:"reasoningLevels"`
	Metadata          json.RawMessage `json:"metadata"`
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

func displayName(id, name, tooltip string) string {
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	if s := strings.TrimSpace(tooltip); s != "" {
		return s
	}
	return strings.TrimSpace(id)
}

func stripMultiplier(name string) string {
	if i := strings.Index(name, " · "); i >= 0 {
		return strings.TrimSpace(name[:i])
	}
	return strings.TrimSpace(name)
}

func catalogModelToModel(m catalogModel) core.Model {
	id := strings.TrimSpace(m.ID)
	ctx := positiveInt(m.ContextWindow)
	maxOut := positiveInt(m.MaxTokens)
	extra := map[string]any{
		"display_name": displayName(id, m.Name, m.Tooltip),
	}
	if s := strings.TrimSpace(m.API); s != "" {
		extra["api"] = s
	}
	if ctx > 0 {
		extra["context_length"] = ctx
	}
	if maxOut > 0 {
		extra["max_output_tokens"] = maxOut
	}
	if m.Reasoning {
		extra["reasoning"] = true
	}
	for _, s := range m.Input {
		if strings.EqualFold(strings.TrimSpace(s), "image") {
			extra["vision"] = true
			break
		}
	}
	if m.Price != nil {
		extra["price"] = float64(*m.Price)
	}
	if s := strings.TrimSpace(m.Tooltip); s != "" {
		extra["tooltip"] = s
	}
	if s := strings.TrimSpace(m.IconURL); s != "" {
		extra["icon_url"] = s
	}
	if s := strings.TrimSpace(m.CreditConsumption); s != "" {
		extra["credit_consumption_level"] = s
	}
	return core.Model{ID: id, OwnedBy: "autoclaw", Extra: extra}
}

// parseCatalog is tolerant about the outer envelope because the desktop client
// has shipped both bare `{models:[...]}` and `{code:0,data:{models:[...]}}`
// documents.  An unparseable document returns nil so the caller keeps its last
// good catalogue.
func parseCatalog(body []byte) []core.Model {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return nil
	}
	var env catalogEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Data.Models) > 0 {
		return modelsFromCatalog(env.Data.Models)
	}
	var bare struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal(body, &bare); err == nil && len(bare.Models) > 0 {
		return modelsFromCatalog(bare.Models)
	}
	return nil
}

func modelsFromCatalog(entries []catalogModel) []core.Model {
	out := make([]core.Model, 0, len(entries))
	seen := map[string]bool{}
	for _, m := range entries {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		switch strings.TrimSpace(m.API) {
		case "", "openai-completions", "anthropic-messages":
		default:
			continue
		}
		seen[id] = true
		out = append(out, catalogModelToModel(m))
	}
	return out
}

// builtinModels is the fallback table captured from the AutoClaw 2.0.5 model
// config.  It exists only so a cold or temporarily unreachable catalogue never
// leaves the picker empty; a live fetch replaces it.
type builtinModel struct {
	ID            string
	Name          string
	ContextWindow int
	MaxTokens     int
	Reasoning     bool
	Vision        bool
}

var builtinModels = []builtinModel{
	{"zai_auto", "Auto", 1048576, 393216, true, true},
	{"zai_auto-fast", "Auto-Fast", 1048576, 131072, false, true},
	{"zaicoding_glm-5.3", "GLM-5.3", 1048576, 307200, true, true},
	{"zai_glm-5.3-flash", "GLM-5.3-Flash", 1048576, 131072, false, true},
}

func fallbackModels() []core.Model {
	out := make([]core.Model, 0, len(builtinModels))
	for _, b := range builtinModels {
		extra := map[string]any{
			"display_name":      b.Name,
			"context_length":    b.ContextWindow,
			"max_output_tokens": b.MaxTokens,
			"fallback":          true,
		}
		if b.Reasoning {
			extra["reasoning"] = true
		}
		if b.Vision {
			extra["vision"] = true
		}
		out = append(out, core.Model{ID: b.ID, OwnedBy: "autoclaw", Extra: extra})
	}
	return out
}
