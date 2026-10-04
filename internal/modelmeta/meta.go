// Package modelmeta resolves model metadata — context window, max output tokens,
// reasoning efforts — and remembers where every number came from.
//
// It exists because every client module in this repository reports whatever the
// vendor happens to return and nothing at all when the vendor is unreachable. On a
// cold start or an upstream outage /v1/models loses context_length /
// max_output_tokens / supported_efforts, so callers of this API cannot size their
// requests. This package is the static, offline-safe fallback for that gap.
//
// Resolution order mirrors the reference panel (workbuddy2api-panel/internal/
// upstream), from strongest to weakest:
//
//	vendor-supplied value -> static table -> models.dev -> local cache
//
// Two rules from the reference are load-bearing and are enforced here:
//
//   - an unknown context window may fall back to a policy value (1M), because
//     over-estimating is the safe direction: the downstream client keeps its
//     context and an oversized request is rejected retriably upstream. The policy
//     value is opt-in per Provider (Options.ContextCap); with no cap configured an
//     unknown model honestly reports not-found instead of a zero-value lie.
//   - an unknown output limit stays empty. There is no safe direction for it, so
//     guessing one would be a bug.
//
// The models.dev layer is OFF by default and is trivially disableable: a Provider
// built without an enabled RemoteConfig never touches the network, so the test
// suite and an offline laptop behave exactly like a machine with no upstream.
//
// This package is standard library only.
package modelmeta

import (
	"sort"
	"strings"
)

// Source values recorded in Meta.Source and Meta.FieldSources. Provenance matters
// as much as the number: an operator must be able to tell where a value came from.
const (
	// SourceVendor means the value came from the live vendor response.
	SourceVendor = "vendor"
	// SourceStatic means the value came from this package's embedded table.
	SourceStatic = "static"
	// SourceModelsDev means the value came from the models.dev document.
	SourceModelsDev = "models-dev"
	// SourceCache means the value came from the local on-disk cache.
	SourceCache = "cache"
	// SourceFallback means the value is a policy default applied because nothing
	// was known, not a measured capability. It is deliberately ranked weakest.
	SourceFallback = "fallback"
)

// Field names used as keys in Meta.FieldSources and by Meta.Has.
const (
	FieldContextLength   = "context_length"
	FieldMaxOutputTokens = "max_output_tokens"
	FieldEfforts         = "supported_efforts"
	FieldDefaultEffort   = "default_effort"
)

// DefaultContextWindow is the reference panel's context policy value: an unknown
// context window is over-estimated to 1M rather than under-estimated, see the
// package comment. It is never applied by Lookup unless Options.ContextCap asks
// for it.
const DefaultContextWindow int64 = 1000000

// Fields lists every metadata field this package resolves, in precedence order.
func Fields() []string {
	return []string{FieldContextLength, FieldMaxOutputTokens, FieldEfforts, FieldDefaultEffort}
}

// Meta is a resolved set of model metadata plus its provenance.
//
// The zero value means "nothing known", which is the honest answer for a model
// that appears in no source. Callers can test it with IsZero.
type Meta struct {
	// ContextLength is the model's input context window in tokens, 0 if unknown.
	ContextLength int64 `json:"context_length,omitempty"`
	// MaxOutputTokens is the per-response output cap in tokens, 0 if unknown.
	// Zero means "unknown, omit the field", never "zero tokens allowed".
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	// Efforts is the accepted reasoning-effort vocabulary, nil if unknown.
	Efforts []string `json:"supported_efforts,omitempty"`
	// DefaultEffort is the advertised default. It is only ever set to a member of
	// Efforts; an unsupported default is dropped rather than advertised.
	DefaultEffort string `json:"default_effort,omitempty"`
	// Source is the strongest provenance among the fields that are set.
	Source string `json:"source,omitempty"`
	// FieldSources maps a Field* constant to the source of that single field, so a
	// merged result can mix layers and still explain itself.
	FieldSources map[string]string `json:"field_sources,omitempty"`
}

// IsZero reports whether the Meta carries no usable metadata at all.
func (m Meta) IsZero() bool {
	return m.ContextLength <= 0 && m.MaxOutputTokens <= 0 && len(m.Efforts) == 0 && m.DefaultEffort == ""
}

// Has reports whether a single Field* is set to a usable value.
func (m Meta) Has(field string) bool {
	switch field {
	case FieldContextLength:
		return m.ContextLength > 0
	case FieldMaxOutputTokens:
		return m.MaxOutputTokens > 0
	case FieldEfforts:
		return len(m.Efforts) > 0
	case FieldDefaultEffort:
		return m.DefaultEffort != ""
	}
	return false
}

// SourceOf returns the provenance of one field, falling back to the whole-value
// Source when the per-field map has nothing to say.
func (m Meta) SourceOf(field string) string {
	if v := m.FieldSources[field]; v != "" {
		return v
	}
	if m.Has(field) {
		return m.Source
	}
	return ""
}

// Clone returns a deep copy, so a returned Meta never aliases the static table.
func (m Meta) Clone() Meta {
	out := m
	if m.Efforts != nil {
		out.Efforts = append([]string(nil), m.Efforts...)
	}
	if m.FieldSources != nil {
		out.FieldSources = make(map[string]string, len(m.FieldSources))
		for k, v := range m.FieldSources {
			out.FieldSources[k] = v
		}
	}
	return out
}

// Equal reports whether two Metas carry the same values and the same provenance.
func (m Meta) Equal(o Meta) bool {
	if m.ContextLength != o.ContextLength || m.MaxOutputTokens != o.MaxOutputTokens ||
		m.DefaultEffort != o.DefaultEffort || m.Source != o.Source {
		return false
	}
	if len(m.Efforts) != len(o.Efforts) {
		return false
	}
	for i := range m.Efforts {
		if m.Efforts[i] != o.Efforts[i] {
			return false
		}
	}
	if len(m.FieldSources) != len(o.FieldSources) {
		return false
	}
	for k, v := range m.FieldSources {
		if o.FieldSources[k] != v {
			return false
		}
	}
	return true
}

// Merge fills the fields that vendor left empty from fallback and returns the
// result. It never overwrites a value that is already present, so a live vendor
// number always wins. A DefaultEffort that ends up outside the effective Efforts
// list is dropped — this package never advertises an unsupported default.
//
// Merge is the one API a client module needs: hand it the vendor's partially
// filled metadata and it applies the whole precedence chain, so no module has to
// re-implement the rules.
func Merge(vendor, fallback Meta) Meta {
	out := vendor.Clone()
	if out.FieldSources == nil {
		out.FieldSources = make(map[string]string, len(Fields()))
	}
	seedProvenance(out.FieldSources, vendor)
	for _, field := range Fields() {
		if out.Has(field) || !fallback.Has(field) {
			continue
		}
		copyField(&out, fallback, field)
	}
	out.Efforts = dedupeStrings(out.Efforts)
	if out.DefaultEffort != "" && !containsString(out.Efforts, out.DefaultEffort) {
		out.DefaultEffort = ""
		delete(out.FieldSources, FieldDefaultEffort)
	}
	out.Source = strongestSource(out.FieldSources)
	return out
}

// strongestSource returns the most authoritative source present in a per-field
// provenance map, or "" when nothing is known.
func strongestSource(sources map[string]string) string {
	best, bestRank := "", 0
	for _, field := range Fields() {
		src := sources[field]
		if src == "" {
			continue
		}
		rank := sourceRank(src)
		if best == "" || rank < bestRank {
			best, bestRank = src, rank
		}
	}
	return best
}

// sourceRank orders sources from most to least authoritative. Unknown sources are
// ranked last but still reported verbatim, so an unfamiliar label is visible
// rather than silently rewritten.
func sourceRank(src string) int {
	switch src {
	case SourceVendor:
		return 0
	case SourceStatic:
		return 1
	case SourceModelsDev:
		return 2
	case SourceCache:
		return 3
	case SourceFallback:
		return 4
	}
	return 50
}

// seedProvenance records which fields a Meta already carries, so a value that
// arrived without per-field provenance still reports its whole-value Source.
func seedProvenance(dst map[string]string, m Meta) {
	for _, field := range Fields() {
		if !m.Has(field) {
			continue
		}
		if dst[field] != "" {
			continue
		}
		if src := m.SourceOf(field); src != "" {
			dst[field] = src
		}
	}
}

func copyField(dst *Meta, src Meta, field string) {
	switch field {
	case FieldContextLength:
		dst.ContextLength = src.ContextLength
	case FieldMaxOutputTokens:
		dst.MaxOutputTokens = src.MaxOutputTokens
	case FieldEfforts:
		dst.Efforts = append([]string(nil), src.Efforts...)
	case FieldDefaultEffort:
		dst.DefaultEffort = src.DefaultEffort
	default:
		return
	}
	if dst.FieldSources == nil {
		dst.FieldSources = make(map[string]string, len(Fields()))
	}
	switch src.FieldSources[field] {
	case "":
		dst.FieldSources[field] = src.Source
	default:
		dst.FieldSources[field] = src.FieldSources[field]
	}
	if dst.FieldSources[field] == "" {
		delete(dst.FieldSources, field)
	}
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// dedupeStrings removes duplicates and empty entries while preserving order.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortedCopy returns a sorted copy; the static table's ids are exposed through it
// so callers can never mutate package state.
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
