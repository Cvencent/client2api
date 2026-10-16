package core

import "strings"

// freemodel.go decides what the router treats as a zero-cost model.
//
// Two kinds of evidence count:
//
//   - `extra.free`, which each module sets when it can read the vendor's own
//     pricing (a zero multiplier, a zero price, the vendor's free-quota list);
//   - a vendor label that names the free tier -- the display name, the group
//     the model is filed under (`owned_by`), or the id itself when it carries
//     the group as a prefix (`cline-free/...`) or a suffix (`model:free`).
//
// Both mean the same thing to a wallet: the request spends no credit, so an
// account whose balance is gone must still be routable for it.  The label
// match is on whole words, so a paid model that merely happens to contain the
// letters (`freeform-8b`) is not swept in.

// ModelIsFree reports whether a catalogue entry is a zero-cost model.
func ModelIsFree(m Model) bool {
	if free, _ := m.Extra["free"].(bool); free {
		return true
	}
	if freeLabel(m.OwnedBy) {
		return true
	}
	for _, key := range []string{"display_name", "name"} {
		if text, _ := m.Extra[key].(string); freeLabel(text) {
			return true
		}
	}
	return freeLabel(m.ID)
}

// freeLabel reports whether a vendor-supplied label names the free tier.
func freeLabel(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false
	}
	if strings.Contains(s, "免费") {
		return true
	}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if field == "free" {
			return true
		}
	}
	return false
}
