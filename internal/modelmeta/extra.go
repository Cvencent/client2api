package modelmeta

import "strings"

// ExtraKeys names the map keys a client module uses when it projects model
// metadata into core.Model.Extra. The same number has a different spelling in
// different modules (workbuddy emits "context_window", kimi and tabbit emit
// "context_length"), so this package takes the spelling as data instead of
// assuming one. Nothing here invents a key that a module does not already emit:
// wiring only fills fields the module would have emitted anyway.
type ExtraKeys struct {
	ContextLength   string
	MaxOutputTokens string
	Efforts         string
	DefaultEffort   string
}

// Key spellings in use in this repository.
var (
	// KeysCanonical is the OpenAI-compatible spelling emitted by clients\kimi
	// (models.go) and clients\tabbit (sidecar.go).
	KeysCanonical = ExtraKeys{
		ContextLength:   FieldContextLength,
		MaxOutputTokens: FieldMaxOutputTokens,
		Efforts:         FieldEfforts,
		DefaultEffort:   FieldDefaultEffort,
	}
	// KeysWorkbuddy is the spelling emitted by clients\workbuddy
	// (workbuddy.go fetchModelsBounded).
	KeysWorkbuddy = ExtraKeys{
		ContextLength:   "context_window",
		MaxOutputTokens: "max_tokens",
		Efforts:         "reasoning_efforts",
		DefaultEffort:   "default_effort",
	}
)

// FromExtra reads back the metadata a module already projected into an Extra map,
// so a live value can be handed to Provider.Merge as the vendor value without the
// module hand-rolling the type switches.
//
// source records where the values came from; empty means SourceVendor, because an
// Extra map on a model a module just observed is by definition what it observed.
func FromExtra(extra map[string]any, keys ExtraKeys, source string) Meta {
	if len(extra) == 0 {
		return Meta{}
	}
	if source == "" {
		source = SourceVendor
	}
	m := Meta{Source: source, FieldSources: map[string]string{}}
	if keys.ContextLength != "" {
		if v := extraInt(extra[keys.ContextLength]); v > 0 {
			m.ContextLength = v
			m.FieldSources[FieldContextLength] = source
		}
	}
	if keys.MaxOutputTokens != "" {
		if v := extraInt(extra[keys.MaxOutputTokens]); v > 0 {
			m.MaxOutputTokens = v
			m.FieldSources[FieldMaxOutputTokens] = source
		}
	}
	if keys.Efforts != "" {
		if v := extraStrings(extra[keys.Efforts]); len(v) > 0 {
			m.Efforts = dedupeStrings(v)
			m.FieldSources[FieldEfforts] = source
		}
	}
	if keys.DefaultEffort != "" {
		if v, ok := extra[keys.DefaultEffort].(string); ok && strings.TrimSpace(v) != "" {
			m.DefaultEffort = strings.TrimSpace(v)
			m.FieldSources[FieldDefaultEffort] = source
		}
	}
	if m.IsZero() {
		return Meta{}
	}
	m.Source = strongestSource(m.FieldSources)
	// An advertised default that the vocabulary does not contain is dropped, the
	// same rule Merge applies.
	if m.DefaultEffort != "" && !containsString(m.Efforts, m.DefaultEffort) {
		m.DefaultEffort = ""
		delete(m.FieldSources, FieldDefaultEffort)
	}
	return m
}

// FillExtra writes m into extra for the fields that are currently missing, and
// reports which canonical fields it wrote so the caller can log or surface the
// provenance.
//
// It never overwrites a value the caller already has: a key that already holds a
// positive integer, or a non-empty effort list, is left exactly as it is. A key
// holding something that is not a usable value (for example a non-positive number)
// counts as missing, which is what makes gap-filling safe to call unconditionally.
func FillExtra(extra map[string]any, m Meta, keys ExtraKeys) []string {
	if extra == nil {
		return nil
	}
	var wrote []string
	if keys.ContextLength != "" && m.ContextLength > 0 && extraInt(extra[keys.ContextLength]) <= 0 {
		extra[keys.ContextLength] = m.ContextLength
		wrote = append(wrote, FieldContextLength)
	}
	if keys.MaxOutputTokens != "" && m.MaxOutputTokens > 0 && extraInt(extra[keys.MaxOutputTokens]) <= 0 {
		extra[keys.MaxOutputTokens] = m.MaxOutputTokens
		wrote = append(wrote, FieldMaxOutputTokens)
	}
	if keys.Efforts != "" && len(m.Efforts) > 0 && len(extraStrings(extra[keys.Efforts])) == 0 {
		extra[keys.Efforts] = append([]string(nil), m.Efforts...)
		wrote = append(wrote, FieldEfforts)
	}
	if keys.DefaultEffort != "" && m.DefaultEffort != "" {
		if cur, _ := extra[keys.DefaultEffort].(string); strings.TrimSpace(cur) == "" {
			extra[keys.DefaultEffort] = m.DefaultEffort
			wrote = append(wrote, FieldDefaultEffort)
		}
	}
	return wrote
}

// extraInt reads a positive integer out of an Extra value. JSON-decoded numbers
// arrive as float64 and generated values as int/int64, so both are accepted;
// anything else is treated as absent.
func extraInt(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case uint:
		return int64(n)
	case uint32:
		return int64(n)
	case uint64:
		if n > uint64(remoteValueMax) {
			return 0
		}
		return int64(n)
	case float32:
		return int64(n)
	case float64:
		if n != float64(int64(n)) {
			return 0
		}
		return int64(n)
	default:
		return 0
	}
}

// extraStrings reads a string list out of an Extra value, accepting both a
// []string built in Go and a []any produced by JSON decoding. Anything else, or a
// list holding a non-string, yields nothing rather than a partial vocabulary.
func extraStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}
