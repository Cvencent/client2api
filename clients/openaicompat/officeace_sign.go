package openaicompat

// officeace_sign.go implements the signatures Huawei's cloud SDK puts on API
// requests.  The OfficeAce client signs two families of calls:
//
//   - SDK-HMAC-SHA256, the project-scoped signature, used for the claw
//     permission/subscription calls and for the cloud IAM endpoints;
//   - V11-HMAC-SHA256, the region-scoped signature the AgentArts model
//     gateway wants for /v1/subscription and the other premium routes.  Its
//     signing key is derived from the secret and a scope of
//     "<yyyymmdd>/<region>/apic" instead of being used directly.
//
// Both share one canonical request, so the encoding lives in one place and the
// signers differ only in how the string-to-sign and the Authorization line are
// assembled.  The canonicalisation matches the client's own signer, including
// its rule that ! * ' ( ) are percent-encoded.
//
// CanonicalRequest =
//   HTTPRequestMethod + '\n' +
//   CanonicalURI + '\n' +
//   CanonicalQueryString + '\n' +
//   CanonicalHeaders + '\n' +
//   SignedHeaders + '\n' +
//   HexEncode(Hash(RequestPayload))
//
// SDK-HMAC-SHA256:
//   StringToSign = "SDK-HMAC-SHA256" '\n' X-Sdk-Date '\n' HexEncode(Hash(CanonicalRequest))
//   Signature    = HexEncode(HMAC-SHA256(StringToSign, SecretAccessKey))
//
// V11-HMAC-SHA256:
//   Scope        = yyyymmdd '/' region '/' "apic"
//   StringToSign = "V11-HMAC-SHA256" '\n' X-Sdk-Date '\n' Scope '\n' HexEncode(Hash(CanonicalRequest))
//   SigningKey   = HexEncode(HMAC-SHA256(HMAC-SHA256(AccessKey, SecretAccessKey), Scope + 0x01))
//   Signature    = HexEncode(HMAC-SHA256(StringToSign, SigningKey))

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	officeAceSignAlgorithm    = "SDK-HMAC-SHA256"
	officeAceV11SignAlgorithm = "V11-HMAC-SHA256"
	// officeAceSignScopeService is the service suffix the V11 scope carries.
	officeAceSignScopeService = "apic"
)

// officeAceSDKDate is the timestamp format the signature carries.
const officeAceSDKDateFormat = "20060102T150405Z"

// signOfficeAceRequest returns the headers to send, including the host, the
// SDK date and the Authorization line.  The caller keeps the request body
// unchanged: a body is hashed as-is, and any method other than POST/PUT/PATCH
// is signed with an empty body, exactly like the SDK.
func signOfficeAceRequest(method, rawURL string, headers map[string]string, body, accessKey, secretKey string, now time.Time) (map[string]string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil || u.Host == "" {
		return nil, fmt.Errorf("officeace: cannot sign %q", rawURL)
	}
	if strings.TrimSpace(accessKey) == "" || strings.TrimSpace(secretKey) == "" {
		return nil, fmt.Errorf("officeace: signing needs an access key and secret")
	}
	out, signedHeaders, canonicalRequest := officeAceCanonicalise(method, u, headers, body, now)
	stringToSign := strings.Join([]string{
		officeAceSignAlgorithm,
		out["X-Sdk-Date"],
		officeAceSHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	mac := hmac.New(sha256.New, []byte(secretKey))
	if _, err := mac.Write([]byte(stringToSign)); err != nil {
		return nil, err
	}
	signature := hex.EncodeToString(mac.Sum(nil))
	out["Authorization"] = fmt.Sprintf("%s Access=%s, SignedHeaders=%s, Signature=%s",
		officeAceSignAlgorithm, accessKey, signedHeaders, signature)
	return out, nil
}

// signOfficeAceV11Request signs one request with the region-scoped
// V11-HMAC-SHA256 variant the AgentArts premium routes require.  A non-empty
// securityToken is added as X-Security-Token, exactly like the client's signer
// does for a temporary credential.
func signOfficeAceV11Request(method, rawURL string, headers map[string]string, body, accessKey, secretKey, securityToken, projectID, region string, now time.Time) (map[string]string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil || u.Host == "" {
		return nil, fmt.Errorf("officeace: cannot sign %q", rawURL)
	}
	accessKey = strings.TrimSpace(accessKey)
	secretKey = strings.TrimSpace(secretKey)
	securityToken = strings.TrimSpace(securityToken)
	region = strings.TrimSpace(region)
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("officeace: signing needs an access key and secret")
	}
	if securityToken == "" {
		return nil, fmt.Errorf("officeace: V11 signing needs a security token")
	}
	if region == "" {
		return nil, fmt.Errorf("officeace: V11 signing needs a region")
	}
	merged := officeAceCopyHeaders(headers)
	merged["X-Security-Token"] = securityToken
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		merged["X-Project-ID"] = projectID
	}
	out, signedHeaders, canonicalRequest := officeAceCanonicalise(method, u, merged, body, now)

	date := out["X-Sdk-Date"]
	scope := officeAceV11Scope(date, region)
	stringToSign := strings.Join([]string{
		officeAceV11SignAlgorithm,
		date,
		scope,
		officeAceSHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	signingKey := officeAceV11SigningKey(accessKey, secretKey, scope)
	signature := officeAceHMACSHA256Hex([]byte(signingKey), []byte(stringToSign))
	out["Authorization"] = fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		officeAceV11SignAlgorithm, accessKey, scope, signedHeaders, signature)
	return out, nil
}

// officeAceCanonicalise produces the signed header set and the canonical
// request both algorithms hash.  It adds Host and X-Sdk-Date, collapses header
// values the way the client does, and signs an empty body for methods that
// cannot carry one.
func officeAceCanonicalise(method string, u *url.URL, headers map[string]string, body string, now time.Time) (map[string]string, string, string) {
	out := officeAceCopyHeaders(headers)
	out["host"] = u.Host
	out["X-Sdk-Date"] = now.UTC().Format(officeAceSDKDateFormat)

	signedBody := body
	switch method {
	case http.MethodGet, http.MethodDelete, http.MethodHead, http.MethodOptions:
		signedBody = ""
	}

	lower := make(map[string]string, len(out))
	names := make([]string, 0, len(out))
	for key, value := range out {
		name := strings.ToLower(strings.TrimSpace(key))
		if name == "" {
			continue
		}
		if _, seen := lower[name]; !seen {
			names = append(names, name)
		}
		lower[name] = officeAceCollapseWhitespace(strings.TrimSpace(value))
	}
	sort.Strings(names)
	var canonicalHeaders strings.Builder
	for _, name := range names {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteString(":")
		canonicalHeaders.WriteString(lower[name])
		canonicalHeaders.WriteString("\n")
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		strings.ToUpper(method),
		officeAceCanonicalURI(u),
		officeAceCanonicalQuery(u),
		canonicalHeaders.String(),
		signedHeaders,
		officeAceSHA256Hex([]byte(signedBody)),
	}, "\n")
	return out, signedHeaders, canonicalRequest
}

// officeAceCopyHeaders clones a header map, dropping a caller-supplied Host so
// the canonicalisation owns it.
func officeAceCopyHeaders(headers map[string]string) map[string]string {
	out := make(map[string]string, len(headers)+4)
	for key, value := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "host") {
			continue
		}
		out[key] = value
	}
	return out
}

// officeAceCollapseWhitespace replaces every run of whitespace with one space,
// which is what the client's canonicaliser does to a header value.
func officeAceCollapseWhitespace(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(raw))
	space := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteByte(c)
	}
	return b.String()
}

// officeAceV11Scope is "<yyyymmdd>/<region>/apic" for the request's date.
func officeAceV11Scope(date, region string) string {
	day := date
	if len(day) > 8 {
		day = day[:8]
	}
	return day + "/" + region + "/" + officeAceSignScopeService
}

// officeAceV11SigningKey derives the region-scoped key V11 uses.  It follows
// the client's own scheme rather than the SDK one: the seed is
// HMAC(AccessKey, SecretAccessKey), and the final key is
// HMAC(seed, Scope + 0x01).
func officeAceV11SigningKey(accessKey, secretKey, scope string) string {
	mac := hmac.New(sha256.New, []byte(accessKey))
	_, _ = mac.Write([]byte(secretKey))
	seed := mac.Sum(nil)
	mac = hmac.New(sha256.New, seed)
	_, _ = mac.Write(append([]byte(scope), 0x01))
	return hex.EncodeToString(mac.Sum(nil))
}

func officeAceHMACSHA256Hex(key, message []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(message)
	return hex.EncodeToString(mac.Sum(nil))
}

// officeAceCanonicalURI percent-encodes each path segment and keeps the
// trailing slash the SDK requires.
func officeAceCanonicalURI(u *url.URL) string {
	if u == nil {
		return "/"
	}
	segments := strings.Split(u.Path, "/")
	for i, segment := range segments {
		segments[i] = officeAceEncodeComponent(segment)
	}
	uri := strings.Join(segments, "/")
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	if uri == "" {
		return "/"
	}
	return uri
}

// officeAceCanonicalQuery rebuilds the query string with sorted keys and
// values, each percent-encoded.  A URL without a query contributes nothing.
func officeAceCanonicalQuery(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	values := u.Query()
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		list := append([]string(nil), values[key]...)
		sort.Strings(list)
		for _, value := range list {
			parts = append(parts, officeAceEncodeComponent(key)+"="+officeAceEncodeComponent(value))
		}
	}
	return strings.Join(parts, "&")
}

// officeAceEncodeComponent is the URL encoding both Huawei signers use: the
// unreserved set is A-Za-z0-9-_.~ and every other byte is escaped with
// uppercase hex.  That is stricter than JavaScript's encodeURIComponent, which
// leaves ! * ' ( ) alone, and matches Huawei's documented canonicalisation.
func officeAceEncodeComponent(raw string) string {
	if raw == "" {
		return ""
	}
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

func officeAceSHA256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
