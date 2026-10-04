package loomy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// models.go owns the model catalogue: the built-in fallback table, the parsing
// of the vendor's own list, and the normalisation of the rate multiplier that
// Loomy hides inside the model's display name.

// loomyEfforts is the reasoning-effort ladder every chat model declares,
// measured from the live catalogue by the reference (2026-09-28).
var loomyEfforts = []string{"none", "low", "medium", "high", "xhigh"}

// loomyDefaultEffort is the effort this module declares as the default.
//
// The reference deliberately does NOT use the vendor's own
// `default_reasoning_effort` (which is "low"): DSH sends whatever default the
// adapter declares when the user picks nothing, so declaring "high" makes the
// out-of-the-box behaviour match the reference's intent.  This module has no
// effort picker of its own, so the value is informational.
const loomyDefaultEffort = "high"

// fallbackModel is one entry of the built-in catalogue.  Names are already
// normalised and therefore already carry the rate multiplier.
type fallbackModel struct {
	ID            string
	Name          string
	ContextWindow int
}

// fallbackModels is the catalogue measured from a live GET /models on
// 2026-09-26 by the reference, in the order the vendor returned it.
//
// It is what Models() answers before -- or without -- a live account, so a
// caller can always discover the ids even with no credential at all.
//
// `spark-x` is a known inconsistency: the vendor's list declares a 1048576
// context window, while the Loomy desktop client forces 262144 locally through
// its own MODEL_CONTEXT_OVERRIDES.  This table keeps the vendor's number,
// exactly as the reference does; see the README.
var fallbackModels = []fallbackModel{
	{"deepseek-v4-flash-0731", "DeepSeek V4 Flash 0731 · x3.0", 1048576},
	{"MiniMax-M3", "MiniMax M3 · x4.0", 1048576},
	{"Kimi-k2.6", "Kimi k2.6 · x6.5", 262144},
	{"qwen-3.8-max", "Qwen 3.8 Max · x12.0", 1000000},
	{"GLM-5.3-Flash", "GLM 5.3 Flash · x0.8", 1048576},
	{"qwen3.8-flash", "qwen 3.8 flash · x0.8", 1000000},
	{"spark-x", "Spark X2.5 · x0.1", 1048576},
	{"mimo-v2.5", "MiMo V2.5 · x3.3", 1048576},
}

// remoteModel is one entry of the vendor's catalogue after normalisation.
type remoteModel struct {
	ID               string
	Name             string // display name, multiplier included
	ContextWindow    int
	MaxOutputTokens  int // the vendor's advertised output budget, 0 when it publishes none
	SupportsImage    bool
	SupportsThinking bool
	Efforts          []string
	DefaultEffort    string
}

// rawModelEntry is one entry of the vendor's JSON list, read defensively: the
// vendor has been seen to type `id` as a string and `context_length` as either
// a number or a string, so both are decoded as raw JSON and coerced.
// `max_output_tokens` is decoded the same way for the same reason.
type rawModelEntry struct {
	ID                     json.RawMessage `json:"id"`
	Name                   string          `json:"name"`
	Type                   string          `json:"type"`
	ContextLength          json.RawMessage `json:"context_length"`
	MaxOutputTokens        json.RawMessage `json:"max_output_tokens"`
	ReasoningEfforts       []any           `json:"reasoning_efforts"`
	DefaultReasoningEffort string          `json:"default_reasoning_effort"`
	Capabilities           *struct {
		Reasoning       bool     `json:"reasoning"`
		InputModalities []string `json:"input_modalities"`
	} `json:"capabilities"`
}

// isChat reports whether an entry is a chat model.  It is judged on `type`
// alone and deliberately NOT on `output_modalities`: five of Loomy's chat
// models accept `image` as an INPUT modality (vision input), which says nothing
// about whether the model generates images.
func (row rawModelEntry) isChat() bool {
	return scalarString(row.ID) != "" && row.Type == "chat"
}

func (row rawModelEntry) model() (remoteModel, bool) {
	if !row.isChat() {
		return remoteModel{}, false
	}
	id := scalarString(row.ID)
	rawName := row.Name
	if rawName == "" {
		rawName = id
	}

	m := remoteModel{
		ID:   id,
		Name: displayName(rawName),
	}
	if n, ok := numberValue(row.ContextLength); ok && n > 0 {
		m.ContextWindow = int(n)
	}
	if n, ok := numberValue(row.MaxOutputTokens); ok && n > 0 {
		m.MaxOutputTokens = int(n)
	}
	if row.Capabilities != nil {
		m.SupportsThinking = row.Capabilities.Reasoning
		for _, modality := range row.Capabilities.InputModalities {
			if strings.EqualFold(strings.TrimSpace(modality), "image") {
				m.SupportsImage = true
			}
		}
	}
	// Efforts are de-duplicated in first-seen order; a non-string element is
	// dropped rather than stringified into a bogus choice.
	for _, entry := range row.ReasoningEfforts {
		s, ok := entry.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" || containsString(m.Efforts, s) {
			continue
		}
		m.Efforts = append(m.Efforts, s)
	}
	if d := strings.TrimSpace(row.DefaultReasoningEffort); d != "" {
		m.DefaultEffort = d
	}
	return m, true
}

// parseRemoteModels reads the vendor's catalogue out of a decoded envelope.
//
// It accepts both shapes the reference accepts: a bare JSON array, and an
// object wrapping the array in `data`.  Entries that are not chat models are
// skipped silently -- the vendor lists image generators in the same array.
func parseRemoteModels(raw json.RawMessage) ([]remoteModel, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, fmt.Errorf("loomy: the model list is empty")
	}

	entries := trimmed
	if trimmed[0] == '{' {
		var wrapper struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(trimmed, &wrapper); err != nil {
			return nil, fmt.Errorf("loomy: model list: %w", err)
		}
		if len(wrapper.Data) == 0 {
			return nil, fmt.Errorf("loomy: the model list has no data field")
		}
		entries = wrapper.Data
	}

	var rows []rawModelEntry
	if err := json.Unmarshal(entries, &rows); err != nil {
		return nil, fmt.Errorf("loomy: model list: %w", err)
	}

	out := make([]remoteModel, 0, len(rows))
	for _, row := range rows {
		if m, ok := row.model(); ok {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("loomy: the model list carries no chat model")
	}
	return out, nil
}

// coreModels projects a normalised catalogue into the contract's shape.
//
// `ID` is the vendor's own model id -- that is what a caller puts after the
// `loomy/` route prefix and what goes upstream.  `Extra["display_name"]` is the
// display name, multiplier included, which is what a picker renders: Loomy
// encodes the credit multiplier inside the name rather than in a field of its
// own, and hiding it would hide the price.
func coreModels(models []remoteModel, fallback bool) []core.Model {
	out := make([]core.Model, 0, len(models))
	for _, m := range models {
		extra := map[string]any{
			"display_name":   m.Name,
			"context_length": m.ContextWindow,
			"vision":         m.SupportsImage,
			"reasoning":      m.SupportsThinking,
		}
		// The budget is published only when the vendor published one.  Writing a
		// zero here would be worse than omitting the key: core.ModelOutputLimit
		// reads a missing key as "this catalogue does not say", and the gateway
		// then sends no cap, whereas an invented number is sent as max_tokens.
		if m.MaxOutputTokens > 0 {
			extra["max_output_tokens"] = m.MaxOutputTokens
		}
		if fallback {
			extra["fallback"] = true
		}
		if len(m.Efforts) > 0 {
			extra["reasoning_efforts"] = append([]string(nil), m.Efforts...)
		}
		if m.DefaultEffort != "" {
			extra["default_effort"] = m.DefaultEffort
		}
		out = append(out, core.Model{ID: m.ID, OwnedBy: "loomy", Extra: extra})
	}
	return out
}

// fallbackCatalogue converts the built-in table into the contract's shape.
func fallbackCatalogue() []core.Model {
	models := make([]remoteModel, 0, len(fallbackModels))
	for _, m := range fallbackModels {
		models = append(models, remoteModel{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.ContextWindow,
			// The reference under-reports vision on a fallback entry on
			// purpose: advertising a modality the server may reject is worse
			// than a caller not offering an image until a live catalogue
			// arrives and says otherwise.
			SupportsImage:    false,
			SupportsThinking: true,
			Efforts:          append([]string(nil), loomyEfforts...),
			DefaultEffort:    loomyDefaultEffort,
		})
	}
	return coreModels(models, true)
}

// ---------------------------------------------------------------------------
// The rate multiplier buried in the display name
// ---------------------------------------------------------------------------

// ratePatterns match the two spellings of the multiplier, both anchored to the
// end of the string.  The first is the vendor's own, in either bracket width
// and with optional spaces; the second is this project's already-normalised
// form, so splitting a normalised name is a no-op.
var ratePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(.*?)\s*[（(]\s*(x\s*[\d.]+)\s*[)）]\s*$`),
	regexp.MustCompile(`(?i)^(.*?)\s*·\s*(x\s*[\d.]+)\s*$`),
}

// splitRate separates the multiplier from a model name.
//
// The returned rate is lower-cased and stripped of all whitespace ("x 1.0" ->
// "x1.0"), so the two spellings compare equal.  An empty rate means the model
// has no multiplier at all -- some entries in the vendor's list, image
// generators among them, carry none.
//
// The function is idempotent: splitRate(displayName(x)) == splitRate(x).  That
// property is what makes it safe to normalise a name that may already be
// normalised, and it is asserted by the tests.
func splitRate(rawName string) (name, rate string) {
	for _, pattern := range ratePatterns {
		match := pattern.FindStringSubmatch(rawName)
		if match == nil {
			continue
		}
		body := strings.TrimSpace(match[1])
		if body == "" {
			// A name that is nothing but a multiplier is malformed.  Keeping
			// the original string is the reference's behaviour: inventing a
			// name would be worse than showing an odd one.
			return rawName, ""
		}
		return body, strings.ToLower(strings.Join(strings.Fields(match[2]), ""))
	}
	return rawName, ""
}

// displayName renders a model name with its multiplier, or bare when there is
// none -- never with a dangling separator.
func displayName(rawName string) string {
	name, rate := splitRate(rawName)
	if rate == "" {
		return name
	}
	return name + " · " + rate
}

// modelNameWithoutRate is what a human would call the model, without the price.
func modelNameWithoutRate(rawName string) string {
	name, _ := splitRate(rawName)
	return name
}

// ---------------------------------------------------------------------------
// Small coercions
// ---------------------------------------------------------------------------

// scalarString renders a JSON scalar as text.  Numbers are formatted without an
// exponent so an 18-digit user id survives the round trip.
func scalarString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return strings.TrimSpace(s)
		}
		return ""
	}
	if n, ok := numberValue(trimmed); ok {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	return ""
}

// numberValue reads a JSON number, or a JSON string that holds one.
func numberValue(raw json.RawMessage) (float64, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return 0, false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return 0, false
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	f, err := strconv.ParseFloat(string(trimmed), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
