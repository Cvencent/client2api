package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sign.go implements Huawei's SDK-HMAC-SHA256 request signing, which is what
// authenticates every CodeArts request.
//
// The algorithm, in the order the reference implementation performs it:
//
//  1. Take the request path.  **If it does not already end in `/`, append
//     one.**  The signature is computed over the forced-trailing-slash path,
//     so a request to `/api/v2/chat/completions` is signed as
//     `/api/v2/chat/completions/`.  The request itself still goes to the
//     original URL.
//  2. The query string is the raw `?a=b` text without the `?`.
//  3. The payload hash is the hex SHA-256 of the body (the hex SHA-256 of the
//     empty string for a bodyless request).
//  4. The base headers are `host`, `x-sdk-date`, `x-sdk-content-sha256` and,
//     when the credential has one, `x-security-token`.  Any extra headers are
//     merged in *before* the canonical request is built, so they participate
//     in both the canonical headers and the signed-header list.
//  5. A GET request gets no `content-type`; anything else gets
//     `content-type: application/json`.
//  6. The canonical request is
//     method \n uri \n query \n <sorted `name:value` lines joined by \n> \n
//     <empty line> \n <sorted names joined by `;`> \n payloadHash
//     — note the empty line, which is the header-list/header-block separator
//     of the HTTP canonicalisation rules and is easy to drop by accident.
//  7. stringToSign = "SDK-HMAC-SHA256" \n dateStamp \n sha256hex(canonical)
//  8. signature = hex HMAC-SHA256(secretAccessKey, stringToSign)
//
// The result also carries an `Authorization` header of the form
// `SDK-HMAC-SHA256 Access=<ak>,SignedHeaders=<h1;h2;…>,Signature=<sig>`.
//
// ---------------------------------------------------------------------------
// The header-ordering trap
// ---------------------------------------------------------------------------
// Two groups of headers look alike and behave nothing alike:
//
//   - `maas_type: benefit` MUST be passed in as an extra header so that it is
//     signed.  It is what routes the request to the free-quota backend; left
//     out, the gateway answers `InferHub.002002009.404 model is not
//     registered`.
//   - `Agent-Type: PromptCenter` and `X-Language: zh-cn` MUST be appended
//     after signing and must never be signed.  Signing them makes the gateway
//     answer `401 APIG.0301 … verify ak sk signature fail`.
//
// The signer below therefore never adds those two on its own; the caller adds
// them to the outgoing request only.  TestSignHuaweiHeaderOrdering pins this
// down.

// sdkDateLayout is the `x-sdk-date` format: `20060102T150405Z`.
const sdkDateLayout = "20060102T150405Z"

// errEmptyCredential is returned when a signing attempt has no key pair.
var errEmptyCredential = errors.New("codearts: credential has no access key")

// signRequest produces the complete signed header set for one request.
//
// `body` may be nil.  `extra` carries headers that must participate in the
// signature; it is the caller's job to decide which those are (see the
// header-ordering note above).
//
// The returned map contains `host`, which the caller must NOT copy into the
// outgoing request — net/http sets it from the URL, and setting it by hand
// produces a duplicate.
func signRequest(ak, sk, securityToken, method, rawURL string, body []byte, extra map[string]string, now time.Time) (map[string]string, error) {
	ak = strings.TrimSpace(ak)
	sk = strings.TrimSpace(sk)
	if ak == "" {
		return nil, errEmptyCredential
	}
	if sk == "" {
		return nil, errors.New("codearts: credential has no secret key")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("codearts: signing an unparseable URL %q: %w", rawURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("codearts: signing a URL with no host: %q", rawURL)
	}

	// 1. forced trailing slash
	uri := u.EscapedPath()
	if uri == "" {
		uri = "/"
	}
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	// 2. raw query, no leading '?'
	query := u.RawQuery

	// 3. payload hash
	payloadHash := sha256Hex(body)

	dateStamp := now.UTC().Format(sdkDateLayout)

	headers := map[string]string{
		"host":                 u.Host,
		"x-sdk-date":           dateStamp,
		"x-sdk-content-sha256": payloadHash,
	}
	if tok := strings.TrimSpace(securityToken); tok != "" {
		headers["x-security-token"] = tok
	}

	// 4. extra signed headers, normalised to lowercase so a caller writing
	//    `MaaS_Type` and one writing `maas_type` produce the same signature.
	for k, v := range extra {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if k == "host" {
			// The host is derived from the URL; accepting a caller's value
			// here would sign one host and connect to another.
			continue
		}
		headers[k] = v
	}

	// 5. content type only for a body-carrying method
	if !strings.EqualFold(method, "GET") {
		if _, ok := headers["content-type"]; !ok {
			headers["content-type"] = "application/json"
		}
	}

	// 6. canonical request
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var lines []string
	for _, k := range names {
		lines = append(lines, k+":"+strings.TrimSpace(headers[k]))
	}
	signedHeaders := strings.Join(names, ";")
	canonicalRequest := strings.Join([]string{
		method,
		uri,
		query,
		strings.Join(lines, "\n"),
		"", // the empty line between the header block and the signed-header list
		signedHeaders,
		payloadHash,
	}, "\n")

	// 7. string to sign
	stringToSign := "SDK-HMAC-SHA256\n" + dateStamp + "\n" + sha256Hex([]byte(canonicalRequest))

	// 8. signature
	mac := hmac.New(sha256.New, []byte(sk))
	mac.Write([]byte(stringToSign))
	signature := hex.EncodeToString(mac.Sum(nil))

	headers["Authorization"] = "SDK-HMAC-SHA256 Access=" + ak +
		",SignedHeaders=" + signedHeaders +
		",Signature=" + signature
	return headers, nil
}

// signedRequestHeaders turns a signature into the header set an http.Request
// carries.
//
// Two rules from the reference implementation are enforced here, in one place,
// so no caller can get them wrong:
//
//   - `host` is dropped.  It is part of the signature but not of the request;
//     net/http derives it from the URL, and copying it produces a duplicate.
//   - `content-type` is dropped too.  It is signed, but the chat endpoint
//     wants a specific value and the caller sets it explicitly afterwards, so
//     carrying the signer's copy would be redundant.  Every other signed
//     header is copied verbatim.
func signedRequestHeaders(signed map[string]string) map[string]string {
	out := make(map[string]string, len(signed))
	for k, v := range signed {
		switch strings.ToLower(k) {
		case "host", "content-type":
			continue
		}
		out[k] = v
	}
	return out
}

// appendUnsignedHeaders adds the headers that must NOT participate in the
// signature.  They are the mirror image of the extra signed headers: the
// gateway rejects a request that signs them.
func appendUnsignedHeaders(h map[string]string, agentType, language string) {
	if h == nil {
		return
	}
	if v := strings.TrimSpace(agentType); v != "" {
		h["Agent-Type"] = v
	}
	if v := strings.TrimSpace(language); v != "" {
		h["X-Language"] = v
	}
}
