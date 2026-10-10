package qoder

import (
	"regexp"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// text.go holds the small text helpers the stream decoder shares with the rest
// of the module: error rendering that can never echo a credential, and the
// numeric coercions the vendor's JSON needs.

// maxErrorText bounds anything copied out of an upstream body before it can
// reach a log line or a Status detail.
const maxErrorText = 200

var (
	reScriptTag = regexp.MustCompile(`(?is)<script[\s\S]*?</script>`)
	reStyleTag  = regexp.MustCompile(`(?is)<style[\s\S]*?</style>`)
	reHTMLTag   = regexp.MustCompile(`<[^>]*>`)
	reSpace     = regexp.MustCompile(`\s+`)
	// reSecret scrubs anything that looks like a labelled credential before a
	// string is logged.
	reSecret = regexp.MustCompile(`(?i)(bearer[ ]+|"?(?:access_token|refresh_token|device_token|security_oauth_token|cosy-key|authorization|token)"?[ ]*[:=][ ]*"?)([A-Za-z0-9._~+/=-]{8,})`)
	// reJWT matches a bare JSON Web Token: three base64url segments joined by
	// dots.  An upstream body can echo one back with no "token=" label in
	// front of it, which reSecret would miss.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b`)
	// reCOSY matches a COSY bearer credential, which is the one shape this
	// module itself puts on the wire.
	reCOSY = regexp.MustCompile(`\bCOSY\.[A-Za-z0-9+/=_-]+\.[A-Za-z0-9+/=_-]+`)
)

// cleanErrorText flattens an upstream body into one survivable line with every
// credential shape masked.
func cleanErrorText(s string) string {
	if s == "" {
		return ""
	}
	s = reScriptTag.ReplaceAllString(s, " ")
	s = reStyleTag.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = reSecret.ReplaceAllStringFunc(s, func(match string) string {
		loc := reSecret.FindStringSubmatchIndex(match)
		if len(loc) < 6 {
			return "***"
		}
		return match[:loc[2]] + core.MaskSecret(match[loc[4]:loc[5]])
	})
	s = reJWT.ReplaceAllStringFunc(s, core.MaskSecret)
	s = reCOSY.ReplaceAllStringFunc(s, core.MaskSecret)
	if len(s) > maxErrorText {
		s = s[:maxErrorText]
	}
	return s
}

// itoa is strconv.Itoa under a shorter name, matching the sibling modules.
func itoa(n int) string { return strconv.Itoa(n) }

// asFloat coerces a JSON value to a float.  A numeric string is accepted too:
// the vendor quotes some counters ("prompt_tokens": "12").
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}
