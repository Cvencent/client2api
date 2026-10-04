package opencode

import (
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// firstNonEmpty returns the first value with non-blank content.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readLimited reads at most n bytes, so a hostile or broken upstream cannot
// make the module allocate without bound.
func readLimited(r io.Reader, n int64) []byte {
	if n <= 0 {
		n = maxErrorBody
	}
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return b
	}
	return b
}

// asString renders a decoded JSON scalar as a string.  Anything it does not
// recognise becomes "".
func asString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case json.Number:
		return t.String()
	case float64:
		return trimFloat(t)
	case float32:
		return trimFloat(float64(t))
	case int:
		return itoa(int64(t))
	case int64:
		return itoa(t)
	case int32:
		return itoa(int64(t))
	case json.RawMessage:
		return strings.Trim(string(t), `"`)
	default:
		return ""
	}
}

// toInt converts a decoded JSON value to an int.  Numeric strings are accepted
// because vendors are inconsistent about quoting counters.
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			f, ferr := t.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(n), true
	case float64:
		return int(t), true
	case float32:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case int32:
		return int(t), true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, false
		}
		var num json.Number = json.Number(s)
		return toInt(num)
	default:
		return 0, false
	}
}

// firstString returns the first non-blank value among keys in m.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := strings.TrimSpace(asString(v)); s != "" {
				return s
			}
		}
	}
	return ""
}

// truncate shortens s to n runes, marking the cut with an ellipsis.  It is
// rune-based so a multi-byte character is never split in half.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// objectOf returns v as a JSON object, or nil.
func objectOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// listOf returns v as a JSON array, or nil.
func listOf(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// stringsOf renders a JSON array of scalars as strings, dropping blanks.
func stringsOf(v any) []string {
	l := listOf(v)
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, item := range l {
		if s := strings.TrimSpace(asString(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// firstIntOK returns the first present, convertible value among keys.
func firstIntOK(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n, ok := toInt(v); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// firstPositive returns the first value greater than zero.  Vendors report the
// same counter under several names and leave the others at zero, so "the first
// one that is actually set" is the only reliable reading.
func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// containsString reports whether list holds want, case-insensitively.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

// copyModels deep-copies a model slice, including the Extra map, so a caller
// can never mutate the cache by writing through the returned value.
func copyModels(in []core.Model) []core.Model {
	if in == nil {
		return nil
	}
	out := make([]core.Model, 0, len(in))
	for _, m := range in {
		cp := m
		if m.Extra != nil {
			extra := make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				extra[k] = v
			}
			cp.Extra = extra
		}
		out = append(out, cp)
	}
	return out
}

// dedupeStrings trims, drops blanks and removes case-insensitive duplicates,
// preserving first-seen order.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		s := strings.TrimSpace(v)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sortedKeys returns a map's keys in sorted order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// trimFloat renders a float without a trailing ".0", which is what the vendor
// sends for token counters.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
