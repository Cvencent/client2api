package lobsterai

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// This file holds the small shared helpers.  They are deliberately local to the
// package rather than exported from internal/core: each module's copy is a few
// dozen lines, and a shared one would have to guess which vendor's quirks it is
// accommodating.

// firstNonEmpty returns the first non-blank value.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readLimited reads at most n bytes.  Error bodies are the only unbounded thing
// a misbehaving upstream can hand us, so every read of a non-stream body goes
// through it.
func readLimited(r io.Reader, n int64) []byte {
	if r == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}

// asString renders a decoded JSON value as a string.
//
// The vendor is inconsistent about whether an id is a number or a string
// (user.id arrives as either depending on the account), so every id goes
// through here.  json.Number is handled explicitly because the envelope decoder
// turns on UseNumber to keep a large configRevision from losing digits.
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
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case json.RawMessage:
		return strings.Trim(string(t), `"`)
	default:
		return ""
	}
}

// toInt renders a decoded JSON value as an int.
func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case int:
		return t, true
	case int64:
		return int(t), true
	case int32:
		return int(t), true
	case float64:
		return int(t), true
	case float32:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
		if f, err := t.Float64(); err == nil {
			return int(f), true
		}
		return 0, false
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(t))
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// firstString returns the first key present in m, rendered as a string.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := asString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// truncate shortens s to at most n runes, appending an ellipsis when it cut
// something.  Rune-based, because the vendor's error messages are Chinese and a
// byte-wise cut would split a character.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// objectOf narrows a decoded JSON value to an object.
func objectOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// listOf narrows a decoded JSON value to a list.
func listOf(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

// stringsOf flattens a list of strings, or of objects carrying one of the named
// keys, into a plain string set.  The vendor sends activity actions as bare
// strings in one revision and as {"action": "..."} objects in another.
func stringsOf(v any, keys ...string) []string {
	out := []string{}
	for _, item := range listOf(v) {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		default:
			if m := objectOf(item); m != nil {
				if s := firstString(m, keys...); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// firstIntOK reads the first key that carries an integer, tolerating the
// numeric strings a few endpoints use.  It reports whether any key was present
// so a caller can tell "the vendor said zero" from "the vendor said nothing".
func firstIntOK(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		if n, ok := toInt(v); ok {
			return n, true
		}
	}
	return 0, false
}

// firstInt is firstIntOK with a zero default.
func firstInt(m map[string]any, keys ...string) int {
	n, _ := firstIntOK(m, keys...)
	return n
}

// containsString reports whether want is in list.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// boolOf reads a JSON boolean, tolerating the "true"/"false" strings a few
// endpoints use.
func boolOf(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		return err == nil && b
	default:
		return false
	}
}

// copyModels deep-copies a model list, including its Extra map, so a caller
// cannot mutate the cache through the slice it was handed.
func copyModels(in []core.Model) []core.Model {
	out := make([]core.Model, 0, len(in))
	for _, m := range in {
		copied := m
		if m.Extra != nil {
			copied.Extra = make(map[string]any, len(m.Extra))
			for k, v := range m.Extra {
				copied.Extra[k] = v
			}
		}
		out = append(out, copied)
	}
	return out
}
