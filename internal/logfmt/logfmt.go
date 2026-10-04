// Package logfmt renders the fixed-width fields of the console lines the
// gateway writes for every request.
//
// The console is read by humans while a service is running, so the rules here
// exist to make that reading possible:
//
//   - account ids are always shortened the same way (eight characters), so the
//     same account is greppable across every line it appears on;
//   - a nick name is decoration next to that id, never a replacement for it;
//   - padding counts display columns, not bytes, because a Chinese nick name is
//     two columns per character and a byte-counted pad would misalign every
//     column after it;
//   - truncation never splits a rune, because error bodies are usually Chinese
//     and a half-cut rune renders as mojibake;
//   - padding only ever adds, never cuts: a value that is too long has already
//     been truncated by the caller, which knows what it is shortening.
//
// The package has no third-party dependencies on purpose.  Measuring display
// width properly means a width table, and the table below covers what the
// upstream can actually produce (CJK, fullwidth forms, emoji) without pulling a
// dependency into the hot path of the gateway.
package logfmt

import (
	"strings"
	"unicode/utf8"
)

// Truncate cuts s to at most n bytes, backing off to the nearest rune boundary
// so that the result is always valid UTF-8.
//
// It returns "" for a non-positive n, and trims surrounding space first so that
// a padded field never ends in a space that the caller cannot see.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// Back off until the cut lands on the start of a rune.  A UTF-8
	// continuation byte has the form 10xxxxxx, which is exactly what
	// utf8.RuneStart rejects.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// UID8 is the canonical short form of an account id: the first eight
// characters, or "-" when there is no id at all.
func UID8(uid string) string {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "-"
	}
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

// Label renders the account column: the nick name when there is one, with the
// short id in parentheses, and the bare short id otherwise.  The id is always
// present so that a line stays greppable even when the nick name changes.
func Label(uid, nick string) string {
	short := UID8(uid)
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return short
	}
	return nick + "(" + short + ")"
}

// DisplayWidth is the number of terminal columns s occupies.
func DisplayWidth(s string) int {
	width := 0
	for _, r := range s {
		width += runeWidth(r)
	}
	return width
}

// Pad appends spaces so that s occupies at least width display columns.  It
// never shortens s: a value that is too long must be truncated by the caller,
// which is the only place that knows how much of it matters.
func Pad(s string, width int) string {
	if width <= 0 {
		return s
	}
	if d := width - DisplayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// runeWidth reports the terminal columns a rune occupies: 0 for control
// characters, 2 for the wide ranges, 1 for everything else.
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		// C0 and C1 controls have no visible width.
		return 0
	case r < 0x1100:
		return 1
	case r <= 0x115f: // Hangul Jamo initial consonants
		return 2
	case r == 0x2329 || r == 0x232a: // angle brackets
		return 2
	case r >= 0x2e80 && r <= 0xa4cf && r != 0x303f: // CJK radicals .. Yi
		return 2
	case r >= 0xac00 && r <= 0xd7a3: // Hangul syllables
		return 2
	case r >= 0xf900 && r <= 0xfaff: // CJK compatibility ideographs
		return 2
	case r >= 0xfe30 && r <= 0xfe6f: // CJK compatibility forms
		return 2
	case r >= 0xff00 && r <= 0xff60: // fullwidth forms
		return 2
	case r >= 0xffe0 && r <= 0xffe6: // fullwidth signs
		return 2
	case r >= 0x1f300 && r <= 0x1f9ff: // emoji
		return 2
	case r >= 0x20000 && r <= 0x3fffd: // CJK extension B and beyond
		return 2
	}
	return 1
}
