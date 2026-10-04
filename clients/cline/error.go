package cline

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// errKind classifies an upstream failure.  The names are deliberately local to
// this module; failureKind maps them onto the shared core.FailureKind contract.
type errKind int

const (
	kindNone errKind = iota
	// kindNetwork is a transport failure: no HTTP response was received.
	kindNetwork
	// kindAuth is 401/403: the credential is not accepted.
	kindAuth
	// kindQuota is 402, or 429 with a credit signal: out of money or credit.
	kindQuota
	// kindTransient is 429 without a credit signal, and any 5xx.
	kindTransient
	// kindClient is any other 4xx: our request was wrong.
	kindClient
)

// String renders a kind for a log line.
func (k errKind) String() string {
	switch k {
	case kindNone:
		return "none"
	case kindNetwork:
		return "network"
	case kindAuth:
		return "auth"
	case kindQuota:
		return "quota"
	case kindTransient:
		return "transient"
	case kindClient:
		return "client"
	}
	return "unknown"
}

// maxErrorText bounds anything that can reach a log line or a Status detail.
const maxErrorText = 200

var (
	reScriptTag = regexp.MustCompile(`(?is)<script.*?</script>`)
	reStyleTag  = regexp.MustCompile(`(?is)<style.*?</style>`)
	reHTMLTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpace     = regexp.MustCompile(`\s+`)
)

// creditMarkers are the wordings the vendor uses when an account is out of
// credit.  They are matched case-insensitively against the response body.
var creditMarkers = []string{
	"insufficient credit",
	"insufficient_credit",
	"insufficient balance",
	"credit balance",
	"out of credit",
	"quota exceeded",
	"exceeded your quota",
	"no credit",
	"payment required",
	"billing",
	"credits remaining",
	"余额不足",
	"额度不足",
	"积分不足",
	"余额",
}

// creditExhausted reports whether a body carries a credit signal.
func creditExhausted(body string) bool {
	low := strings.ToLower(body)
	for _, m := range creditMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// modelBlocked are the wordings the vendor uses when it refuses one *model*
// rather than the credential.  They matter because the vendor answers 403 for
// both: a regional block on a single model looks exactly like a rejected
// token until the body is read.
var modelBlocked = []string{
	"not available in your region",
	"model is not available",
	"unsupported model",
	"model not found",
	"no access to this model",
}

// modelRefused reports whether a body refuses the model rather than the account.
func modelRefused(body string) bool {
	low := strings.ToLower(body)
	for _, m := range modelBlocked {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// classify turns an HTTP status and body into a kind.  Order matters: 402 is
// always quota, 429 is quota only when the body says so, and 5xx is checked
// before the generic 4xx branch.
func classify(status int, body string) errKind {
	switch {
	case status == http.StatusPaymentRequired:
		return kindQuota
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// A 403 usually means the credential, but the vendor also uses 403 to
		// refuse a single model (a regional block, a retired id).  That verdict
		// is about the model, not the account, and parking the account takes
		// every other model down with it: the live repro was
		// `cline-free/muse-spark-1.3-contributor` answering
		// "access forbidden: … is not available in your region", which cooled
		// the only account and made the *next* good model fail with 503.
		if status == http.StatusForbidden && modelRefused(body) {
			return kindClient
		}
		return kindAuth
	case status == http.StatusTooManyRequests:
		if creditExhausted(body) {
			return kindQuota
		}
		return kindTransient
	case status >= 500:
		return kindTransient
	case status >= 400:
		return kindClient
	}
	return kindNone
}

// failureKind maps a local kind onto the shared failure contract.
func failureKind(k errKind) core.FailureKind {
	switch k {
	case kindNetwork:
		return core.FailureUpstream
	case kindAuth:
		return core.FailureAuth
	case kindQuota:
		return core.FailureQuota
	case kindTransient:
		return core.FailureRateLimited
	case kindClient:
		return core.FailureOther
	}
	return core.FailureOther
}

// retryable reports whether another account is worth trying after this kind.
// A client error is not: our request was wrong and will be wrong everywhere.
func retryable(k errKind) bool {
	switch k {
	case kindNetwork, kindAuth, kindQuota, kindTransient:
		return true
	}
	return false
}

// upstreamError is one failed HTTP exchange.
type upstreamError struct {
	Status     int
	Kind       errKind
	Message    string
	RetryAfter time.Duration
}

// Error implements error.
func (e *upstreamError) Error() string {
	if e == nil {
		return "upstream error"
	}
	if e.Status > 0 {
		return "upstream returned HTTP " + strconv.Itoa(e.Status) + ": " + e.Message
	}
	return e.Message
}

// asUpstreamError extracts an upstreamError from an error chain.
func asUpstreamError(err error) (*upstreamError, bool) {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// kindOfErr classifies any error for the pool: an upstreamError keeps its own
// verdict, and anything else is a client-side failure that must not park the
// account (a context deadline or a local decode error is not the credential's
// fault).
func kindOfErr(err error) errKind {
	if ue, ok := asUpstreamError(err); ok {
		return ue.Kind
	}
	return kindClient
}

// newUpstreamError builds the classified error for one response.
func newUpstreamError(status int, body string, header http.Header) *upstreamError {
	msg := cleanErrorText(body)
	if msg == "" {
		msg = http.StatusText(status)
	}
	return &upstreamError{
		Status:     status,
		Kind:       classify(status, body),
		Message:    msg,
		RetryAfter: parseRetryAfter(header),
	}
}

// errorMessage digs the most useful human string out of a JSON error body,
// falling back to the raw text.
func errorMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err == nil {
		for _, key := range []string{"error", "message", "msg", "detail"} {
			switch v := m[key].(type) {
			case string:
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			case map[string]any:
				if s := firstStringField(v, "message", "msg", "detail"); s != "" {
					return s
				}
			}
		}
	}
	return trimmed
}

// cleanErrorText strips HTML, collapses whitespace, redacts anything that looks
// like a secret and truncates.  Everything that can reach a log line, a Status
// detail or a returned error goes through here.
func cleanErrorText(s string) string {
	if s == "" {
		return ""
	}
	s = reScriptTag.ReplaceAllString(s, " ")
	s = reStyleTag.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = core.Redact(s)
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len(s) > maxErrorText {
		s = strings.TrimSpace(s[:maxErrorText]) + "…"
	}
	return s
}

// parseRetryAfter honours the reset headers, capped at two hours so a broken or
// hostile value cannot park an account for a week.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	const cap = 2 * time.Hour
	for _, key := range []string{"Retry-After", "X-Ratelimit-Reset", "Retry-After-Ms"} {
		v := strings.TrimSpace(h.Get(key))
		if v == "" {
			continue
		}
		if key == "Retry-After-Ms" {
			if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
				d := time.Duration(ms) * time.Millisecond
				if d > cap {
					d = cap
				}
				return d
			}
			continue
		}
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > 0 {
			d := time.Duration(secs) * time.Second
			if d > cap {
				d = cap
			}
			return d
		}
		if t, err := http.ParseTime(v); err == nil {
			d := time.Until(t)
			if d <= 0 {
				continue
			}
			if d > cap {
				d = cap
			}
			return d
		}
	}
	return 0
}

// firstStringField returns the first present non-empty string value.
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok {
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// asString renders any decoded JSON scalar as a string.
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
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	}
	return ""
}

// pickString returns the first present non-empty string field under any of the
// given spellings.  The WorkOS endpoints are documented with snake_case while
// the Cline endpoints answer in camelCase, so both spellings are accepted.
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asString(m[k]); strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// asInt reads a decoded JSON number as an int.
func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case int64:
		return int(t), true
	case float64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}

// readLimited reads at most n bytes of a body, so a hostile or broken response
// cannot exhaust memory.
func readLimited(r io.Reader, n int64) []byte {
	if r == nil || n <= 0 {
		return nil
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	var total int64
	for total < n {
		want := int64(len(tmp))
		if remaining := n - total; remaining < want {
			want = remaining
		}
		nr, err := r.Read(tmp[:want])
		if nr > 0 {
			buf = append(buf, tmp[:nr]...)
			total += int64(nr)
		}
		if err != nil {
			break
		}
	}
	return buf
}
