package core

import "regexp"

// Redact removes credential-shaped substrings from a string that is about to be
// shown to a human (the panel) or written to a log.
//
// It exists because the panel renders whatever a module puts in Status verbatim:
// a module whose account has no stable user id may fall back to a token prefix
// as its identifier, and that must not reach the dashboard.  Redaction here is
// defence in depth -- modules are still expected not to put credentials in
// Status in the first place.
//
// Ordinary identifiers are left alone: a numeric uid such as 3595881099822378 or
// a label such as "cli-login" passes through unchanged.
func Redact(s string) string {
	if s == "" {
		return s
	}
	// Order matters.  The bearer and marker rules consume the whole credential,
	// including any JWT sitting inside it, so the bare-JWT sweep runs last and
	// only catches credentials that carry no marker at all.
	s = reBearer.ReplaceAllString(s, "Bearer <redacted>")
	s = reCred.ReplaceAllString(s, "${1}=<redacted>")
	s = reJWT.ReplaceAllString(s, "<redacted-jwt>")
	return s
}

// RedactAny walks a JSON-ish value and redacts every string it contains.  Maps
// and slices are rebuilt; other values are returned as-is.
func RedactAny(v any) any {
	switch t := v.(type) {
	case string:
		return Redact(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = RedactAny(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = RedactAny(e)
		}
		return out
	default:
		return v
	}
}

// RedactStatus returns a copy of st with every credential-shaped string masked.
//
// Both exits that render a Status to a human -- GET /v1/status and the panel --
// run through here, so a module cannot leak a token prefix merely because one
// endpoint was overlooked.  Modules are still expected not to put credentials in
// Status at all; this is defence in depth.
func RedactStatus(st Status) Status {
	st.Detail = Redact(st.Detail)
	if len(st.Accounts) == 0 {
		return st
	}
	accounts := make([]AccountStatus, len(st.Accounts))
	for i, a := range st.Accounts {
		a.ID = Redact(a.ID)
		a.Label = Redact(a.Label)
		a.Note = Redact(a.Note)
		if a.Extra != nil {
			if red, ok := RedactAny(a.Extra).(map[string]any); ok {
				a.Extra = red
			}
		}
		accounts[i] = a
	}
	st.Accounts = accounts
	return st
}

var (
	// A JWT: three dot-separated base64url runs.  The `eyJ` prefix is required
	// because it is base64url for `{"`, i.e. the start of a JOSE header -- and
	// because without it this pattern also swallows ordinary dotted identifiers
	// such as `clients.qwenwork.access_token`, which is exactly the kind of
	// config-path text these messages need to keep readable.
	reJWT = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}`)
	// An Authorization-style bearer value.
	reBearer = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	// A credential introduced by an obvious marker: tok:xxx, "api_key": "xxx",
	// secret=xxx, cosy-key: xxx.  Quotes may sit on either side of the colon.
	reCred = regexp.MustCompile(`(?i)\b(tok|token|access[_-]?token|refresh[_-]?token|api[_-]?key|apikey|key|secret|password|passwd|cosy-key)\b\s*"?\s*[:=]\s*"?[A-Za-z0-9._~+/=-]{6,}`)
)
