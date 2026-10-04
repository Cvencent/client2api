package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sign.go implements the iFlytek CAccount request signature.
//
// It is a Go port of the reference's src/loomy-sign.ts, which is itself a
// byte-for-byte copy of the Loomy desktop client's electron/xfyun/sign.js.  The
// signature is an HMAC-SHA1 over a nine-line canonical string, base64-encoded,
// and carried as `Authorization: account <ak>:<sig>`.
//
// The signing string is, in order, each segment on its own line:
//
//	{METHOD}                 upper-cased
//	{ESCAPED_PATH}           each segment RFC 3986 escaped
//	{ESCAPED_QUERY}          escaped, in the caller's order (never sorted)
//	{Content-MD5}            base64 MD5 of the body, or empty for no body
//	{Content-Type}           e.g. application/json
//	{Date}                   RFC 1123 in UTC, i.e. JS toUTCString()
//	{Nonce}                  a fresh UUID per request
//	{SignedHeaders}          always empty here -- we sign no x-* headers
//	{CanonicalizedHeaders}   always empty here
//
// The last two segments are always empty, so the canonical string ENDS WITH TWO
// NEWLINES.  That is not cosmetic: trimming the trailing newline changes the
// bytes that are hashed and every request then fails with a signature error.
//
// Only the account endpoint (SMS-code login) uses this.  The business and
// reasoning endpoints authenticate with the user's own `session` as a plain
// header, so nothing here touches user data.

// queryParam is one query-string entry.  Order matters: the reference escapes
// and joins the caller's entries verbatim and never sorts them, so the signing
// string depends on insertion order.
type queryParam struct {
	Key   string
	Value string
}

// signOptions is the input to the signature, mirroring the reference's
// LoomySignOptions.  Date and Nonce are supplied by the caller so the whole
// computation is deterministic and can be unit-tested against a frozen vector;
// authHeaders fills them in when they are empty.
type signOptions struct {
	AccessKeyID     string
	AccessKeySecret string
	Method          string
	Path            string
	QueryParams     []queryParam
	Body            string
	ContentType     string
	Date            string
	Nonce           string
}

// canonicalString builds the nine-line string that gets signed.
func canonicalString(o signOptions) string {
	return strings.Join([]string{
		strings.ToUpper(o.Method),
		buildEscapedPath(o.Path),
		buildEscapedQuery(o.QueryParams),
		contentMD5(o.Body),
		o.ContentType,
		o.Date,
		o.Nonce,
		"", // SignedHeaders -- no x-* header is ever signed
		"", // CanonicalizedHeaders
	}, "\n")
}

// signature is the base64 HMAC-SHA1 of canonicalString keyed by the access key
// secret.
func signature(o signOptions) string {
	mac := hmac.New(sha1.New, []byte(o.AccessKeySecret))
	mac.Write([]byte(canonicalString(o)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// authHeaders signs one request and returns the headers to send with it.
//
// The caller MUST send the exact same body string that was passed in here.  A
// second serialisation of the same object can reorder keys or change spacing,
// and any byte difference invalidates the signature.
func authHeaders(o signOptions) map[string]string {
	if o.ContentType == "" {
		o.ContentType = "application/json"
	}
	if o.Date == "" {
		o.Date = time.Now().UTC().Format(http.TimeFormat)
	}
	if o.Nonce == "" {
		o.Nonce = newNonce()
	}

	headers := map[string]string{
		"Authorization": "account " + o.AccessKeyID + ":" + signature(o),
		"Date":          o.Date,
		"Nonce":         o.Nonce,
		"Content-Type":  o.ContentType,
	}
	// Content-MD5 is omitted entirely when there is no body, exactly as the
	// reference does: an empty body contributes an empty segment to the
	// canonical string and no header at all.
	if sum := contentMD5(o.Body); sum != "" {
		headers["Content-MD5"] = sum
	}
	return headers
}

// contentMD5 is the base64 MD5 of the request body.
//
// An EMPTY body yields the empty string, not the MD5 of zero bytes.  That is a
// quirk of the vendor's own sign.js and it is load-bearing: the canonical
// string's fourth segment must be blank for a bodyless request.
func contentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// buildEscapedPath escapes a raw path for the canonical string.
//
// A leading slash is added when missing and a trailing slash is dropped (unless
// the path is just "/"); every non-empty segment is then escaped individually
// and the segments are rejoined with "/", so an escaped segment can never
// introduce a path separator.
func buildEscapedPath(rawPath string) string {
	path := rawPath
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		segments[i] = escapeRFC3986(segment)
	}
	return strings.Join(segments, "/")
}

// buildEscapedQuery escapes query parameters in the caller's order.  An absent
// parameter set is the empty string; values are never sorted.
func buildEscapedQuery(params []queryParam) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, escapeRFC3986(p.Key)+"="+escapeRFC3986(p.Value))
	}
	return strings.Join(parts, "&")
}

// escapeRFC3986 is the reference's escapeRfc3986(): encodeURIComponent() with
// ! ' ( ) * escaped as well.
//
// encodeURIComponent keeps ALPHA / DIGIT / "-" / "." / "_" / "~" plus the five
// sub-delimiters, and the reference then escapes those five by hand.  The
// surviving set is therefore exactly the RFC 3986 unreserved set, which is what
// this encodes directly -- so it agrees with the reference without needing a
// second pass.
func escapeRFC3986(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if isUnreserved(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0f])
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

// isUnreserved reports whether b survives escaping unencoded.
func isUnreserved(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '.', b == '_', b == '~':
		return true
	}
	return false
}

// newNonce mints the per-request nonce.  The reference uses randomUUID(), so
// this produces a version 4 UUID in the same textual form.
func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A failing entropy source is not survivable in any useful sense, but
		// a timestamp nonce is still unique enough to sign one request and is
		// far better than panicking inside a chat request.
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
