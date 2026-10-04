package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"
)

// sign_test.go pins down Huawei's SDK-HMAC-SHA256, including the header
// ordering that decides whether a request is accepted.
//
// The signature test does not call signRequest and compare it with itself: it
// rebuilds the canonical request from the specification, by hand, and checks
// the HMAC of that.  A test that re-derived the canonical request with the
// same helper the production code uses would pass no matter how wrong the
// helper was.

const (
	testAK  = "AKIDEXAMPLE"
	testSK  = "SKEXAMPLE"
	testTok = "TOKENEXAMPLE"
)

// testNow is a fixed instant, so every date stamp and signature below is a
// constant.
var testNow = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

// testChatURL is the endpoint the module really calls.  It deliberately has no
// trailing slash: the signer has to add one.
const testChatURL = "https://snap-access.cn-north-4.myhuaweicloud.com/api/v2/chat/completions"

// hmacHex is the independent HMAC-SHA256 the expected signatures are built
// with.
func hmacHex(key, msg string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func sha256HexTest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestSignRequestCanonicalRequest checks every element of the canonical
// request, including the two that are easiest to get wrong: the forced
// trailing slash on the URI, and the empty line between the header block and
// the signed-header list.
func TestSignRequestCanonicalRequest(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash"}`)
	signed, err := signRequest(testAK, testSK, testTok, http.MethodPost, testChatURL, body,
		map[string]string{"maas_type": "benefit"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}

	payloadHash := sha256HexTest(string(body))
	dateStamp := "20260918T120000Z"
	signedHeaders := "content-type;host;maas_type;x-sdk-content-sha256;x-sdk-date;x-security-token"

	// The canonical request, written out from the specification rather than
	// assembled by the production code.
	canonical := "POST" + "\n" +
		"/api/v2/chat/completions/" + "\n" + // forced trailing slash
		"" + "\n" + // empty query
		"content-type:application/json" + "\n" +
		"host:snap-access.cn-north-4.myhuaweicloud.com" + "\n" +
		"maas_type:benefit" + "\n" +
		"x-sdk-content-sha256:" + payloadHash + "\n" +
		"x-sdk-date:" + dateStamp + "\n" +
		"x-security-token:" + testTok + "\n" +
		"" + "\n" + // the separator line
		signedHeaders + "\n" +
		payloadHash

	stringToSign := "SDK-HMAC-SHA256" + "\n" + dateStamp + "\n" + sha256HexTest(canonical)
	want := "SDK-HMAC-SHA256 Access=" + testAK +
		",SignedHeaders=" + signedHeaders +
		",Signature=" + hmacHex(testSK, stringToSign)

	if got := signed["Authorization"]; got != want {
		t.Errorf("Authorization mismatch\n got: %s\nwant: %s", got, want)
	}
	if got := signed["x-sdk-date"]; got != dateStamp {
		t.Errorf("x-sdk-date = %q, want %q", got, dateStamp)
	}
	if got := signed["x-sdk-content-sha256"]; got != payloadHash {
		t.Errorf("x-sdk-content-sha256 = %q, want %q", got, payloadHash)
	}
	if got := signed["host"]; got != "snap-access.cn-north-4.myhuaweicloud.com" {
		t.Errorf("host = %q", got)
	}
}

// TestSignRequestTrailingSlashIsForced checks the rule on its own, because it
// is invisible in the outgoing request: the URL that is sent has no trailing
// slash, but the URL that is signed always does.
//
// The rule is stated positively — the two spellings must sign identically —
// and then checked for vacuity: a genuinely different path must not.
func TestSignRequestTrailingSlashIsForced(t *testing.T) {
	for _, base := range []string{
		"https://h.example.com/api/v2/chat/completions",
		"https://h.example.com/v1/model/builtin",
		"https://h.example.com",
	} {
		withSlash, err := signRequest(testAK, testSK, "", http.MethodGet, base+"/", nil, nil, testNow)
		if err != nil {
			t.Fatalf("signRequest(%q/): %v", base, err)
		}
		without, err := signRequest(testAK, testSK, "", http.MethodGet, base, nil, nil, testNow)
		if err != nil {
			t.Fatalf("signRequest(%q): %v", base, err)
		}
		if withSlash["Authorization"] != without["Authorization"] {
			t.Errorf("the trailing slash is not forced: %q and %q signed differently", base, base+"/")
		}
		// A different path must change the signature, or the assertion above
		// would hold for a signer that ignored the path entirely.
		other, err := signRequest(testAK, testSK, "", http.MethodGet, base+"/deeper", nil, nil, testNow)
		if err != nil {
			t.Fatalf("signRequest(%q/deeper): %v", base, err)
		}
		if other["Authorization"] == without["Authorization"] {
			t.Errorf("the signed path is not covered by the signature for %q", base)
		}
	}
}

// TestSignRequestGetHasNoContentType checks that a bodyless GET is not signed
// with a content-type.  The vendor's catalogue endpoint is a GET, and signing
// a content-type it never sends produces a signature mismatch.
func TestSignRequestGetHasNoContentType(t *testing.T) {
	signed, err := signRequest(testAK, testSK, "", http.MethodGet, "https://h.example.com/v1/model/builtin", nil, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if _, ok := signed["content-type"]; ok {
		t.Fatalf("a GET was signed with a content-type: %v", signed)
	}
	if !strings.Contains(signed["Authorization"], "SignedHeaders=host;x-sdk-content-sha256;x-sdk-date") {
		t.Errorf("GET signed headers = %s", signed["Authorization"])
	}

	// A POST on the same URL does get one.
	post, err := signRequest(testAK, testSK, "", http.MethodPost, "https://h.example.com/v1/model/builtin", []byte("{}"), nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if got := post["content-type"]; got != "application/json" {
		t.Errorf("POST content-type = %q, want application/json", got)
	}
}

// TestSignRequestSecurityTokenOnlyWhenPresent checks that a credential without
// a security token is not signed with an empty one.
func TestSignRequestSecurityTokenOnlyWhenPresent(t *testing.T) {
	withTok, err := signRequest(testAK, testSK, testTok, http.MethodGet, "https://h.example.com/x", nil, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	without, err := signRequest(testAK, testSK, "", http.MethodGet, "https://h.example.com/x", nil, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if withTok["x-security-token"] != testTok {
		t.Errorf("x-security-token = %q", withTok["x-security-token"])
	}
	if _, ok := without["x-security-token"]; ok {
		t.Errorf("a credential with no security token was signed with one")
	}
	if !strings.Contains(withTok["Authorization"], "x-security-token") {
		t.Errorf("the security token is not in the signed-header list: %s", withTok["Authorization"])
	}
	if strings.Contains(without["Authorization"], "x-security-token") {
		t.Errorf("the signed-header list names a header that is not sent: %s", without["Authorization"])
	}
}

// TestSignRequestRejectsEmptyCredential checks the two failure modes that must
// be errors rather than a signature nobody can verify.
func TestSignRequestRejectsEmptyCredential(t *testing.T) {
	if _, err := signRequest("", testSK, "", http.MethodGet, "https://h.example.com/x", nil, nil, testNow); err == nil {
		t.Error("an empty access key was accepted")
	}
	if _, err := signRequest(testAK, "", "", http.MethodGet, "https://h.example.com/x", nil, nil, testNow); err == nil {
		t.Error("an empty secret key was accepted")
	}
	if _, err := signRequest(testAK, testSK, "", http.MethodGet, "not-a-url", nil, nil, testNow); err == nil {
		t.Error("a URL with no host was accepted")
	}
}

// TestSignRequestHostIsNeverTakenFromExtra checks that a caller cannot sign one
// host and connect to another.
func TestSignRequestHostIsNeverTakenFromExtra(t *testing.T) {
	signed, err := signRequest(testAK, testSK, "", http.MethodGet, "https://real.example.com/x", nil,
		map[string]string{"host": "evil.example.com"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if got := signed["host"]; got != "real.example.com" {
		t.Errorf("host = %q, want the URL's host", got)
	}
}

// TestSignHuaweiHeaderOrdering is the guard for the trap this module exists to
// get right.
//
// `maas_type: benefit` must be inside the signature, and `Agent-Type` /
// `X-Language` must be outside it.  Both halves are asserted, so a change that
// starts signing the second pair — or stops signing the first — fails here
// rather than as a 401 or a 404 in production.
func TestSignHuaweiHeaderOrdering(t *testing.T) {
	body := []byte(`{"model":"glm-5.3-flash"}`)

	// The benefit model: maas_type participates in the signature.
	signed, err := signRequest(testAK, testSK, testTok, http.MethodPost, testChatURL, body,
		map[string]string{"maas_type": "benefit"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	auth := signed["Authorization"]
	lowerAuth := strings.ToLower(auth)

	if !strings.Contains(lowerAuth, "signedheaders=content-type;host;maas_type;") {
		t.Fatalf("maas_type is not in the signed-header list: %s", auth)
	}
	if got := signed["maas_type"]; got != "benefit" {
		t.Fatalf("maas_type = %q, want benefit", got)
	}

	// Agent-Type and X-Language must not be in the signature.  The signer must
	// not add them on its own, and the caller must not pass them in `extra`.
	for _, forbidden := range []string{"agent-type", "x-language"} {
		if strings.Contains(lowerAuth, forbidden) {
			t.Fatalf("%s must never be signed, but it is in the signature: %s", forbidden, auth)
		}
		if _, ok := signed[forbidden]; ok {
			t.Fatalf("%s must not be added by the signer, but it is in the signed set", forbidden)
		}
	}

	// The outgoing header set is the signed set minus host and content-type,
	// with the unsigned pair appended afterwards.
	out := signedRequestHeaders(signed)
	appendUnsignedHeaders(out, "PromptCenter", "zh-cn")

	if _, ok := out["host"]; ok {
		t.Error("host must not be copied into the request; net/http sets it")
	}
	if _, ok := out["content-type"]; ok {
		t.Error("content-type must not be copied; the caller sets it explicitly")
	}
	if out["maas_type"] != "benefit" {
		t.Errorf("maas_type must survive the copy, got %q", out["maas_type"])
	}
	if out["Authorization"] != auth {
		t.Errorf("the Authorization header was altered by the copy")
	}
	if out["Agent-Type"] != "PromptCenter" {
		t.Errorf("Agent-Type = %q, want PromptCenter", out["Agent-Type"])
	}
	if out["X-Language"] != "zh-cn" {
		t.Errorf("X-Language = %q, want zh-cn", out["X-Language"])
	}

	// Signing the unsigned pair changes the signature.  This is the trap
	// stated positively: if it did not change anything, the assertion above
	// would be vacuous.
	trapped, err := signRequest(testAK, testSK, testTok, http.MethodPost, testChatURL, body,
		map[string]string{"maas_type": "benefit", "Agent-Type": "PromptCenter", "X-Language": "zh-cn"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if trapped["Authorization"] == auth {
		t.Fatal("signing Agent-Type/X-Language produced the same signature, so the ordering check proves nothing")
	}
	if !strings.Contains(strings.ToLower(trapped["Authorization"]), "agent-type") {
		t.Fatalf("the deliberately-wrong signature does not name agent-type: %s", trapped["Authorization"])
	}
}

// TestSignRequestBenefitHeaderIsSignedNotAppended is the narrower statement of
// the same rule: maas_type belongs in `extra`, so dropping it changes the
// signature.  If it did not, the header would be travelling unsigned.
func TestSignRequestBenefitHeaderIsSignedNotAppended(t *testing.T) {
	body := []byte(`{}`)
	withBenefit, err := signRequest(testAK, testSK, testTok, http.MethodPost, testChatURL, body,
		map[string]string{"maas_type": "benefit"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	without, err := signRequest(testAK, testSK, testTok, http.MethodPost, testChatURL, body, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if withBenefit["Authorization"] == without["Authorization"] {
		t.Fatal("the benefit header is not covered by the signature")
	}
}

// TestSignRequestExtraHeadersAreLowercased checks that a caller's spelling does
// not change the signature, because the vendor compares header names
// case-insensitively and a mismatch would be a silent 401.
func TestSignRequestExtraHeadersAreLowercased(t *testing.T) {
	body := []byte(`{}`)
	lower, err := signRequest(testAK, testSK, "", http.MethodPost, testChatURL, body,
		map[string]string{"maas_type": "benefit"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	upper, err := signRequest(testAK, testSK, "", http.MethodPost, testChatURL, body,
		map[string]string{"MaaS_Type": "benefit"}, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if lower["Authorization"] != upper["Authorization"] {
		t.Errorf("header-name case changed the signature\nlower: %s\nupper: %s", lower["Authorization"], upper["Authorization"])
	}
}

// TestSignRequestQueryStringIsSigned checks that the queue-status query is part
// of the signature, since that request is signed too.
func TestSignRequestQueryStringIsSigned(t *testing.T) {
	base := "https://h.example.com/api/v1/queue/status?model=glm-5.3-flash&task_id=abc"
	other := "https://h.example.com/api/v1/queue/status?model=glm-5.3-flash&task_id=def"
	a, err := signRequest(testAK, testSK, "", http.MethodGet, base, nil, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	b, err := signRequest(testAK, testSK, "", http.MethodGet, other, nil, nil, testNow)
	if err != nil {
		t.Fatalf("signRequest: %v", err)
	}
	if a["Authorization"] == b["Authorization"] {
		t.Error("the query string is not part of the signature")
	}
}

// TestSignedRequestHeadersKeepsEverythingElse checks that the copy is a
// subtraction, not an allow-list: a header added to the signer later must reach
// the request without anyone remembering to update this function.
func TestSignedRequestHeadersKeepsEverythingElse(t *testing.T) {
	in := map[string]string{
		"host":                 "h.example.com",
		"content-type":         "application/json",
		"x-sdk-date":           "20260918T120000Z",
		"x-sdk-content-sha256": "abc",
		"x-security-token":     "tok",
		"maas_type":            "benefit",
		"Authorization":        "SDK-HMAC-SHA256 ...",
	}
	out := signedRequestHeaders(in)
	want := []string{"x-sdk-date", "x-sdk-content-sha256", "x-security-token", "maas_type", "Authorization"}
	names := make([]string, 0, len(out))
	for k := range out {
		names = append(names, k)
	}
	sort.Strings(names)
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("copied headers = %v, want %v", names, want)
	}
}

// TestAppendUnsignedHeadersIgnoresEmpty checks that an empty agent type or
// language does not put an empty header on the wire.
func TestAppendUnsignedHeadersIgnoresEmpty(t *testing.T) {
	out := map[string]string{}
	appendUnsignedHeaders(out, "  ", "")
	if len(out) != 0 {
		t.Errorf("empty values produced headers: %v", out)
	}
	appendUnsignedHeaders(out, "INFERHUB_AGENT", "en")
	if out["Agent-Type"] != "INFERHUB_AGENT" || out["X-Language"] != "en" {
		t.Errorf("headers = %v", out)
	}
	// A nil map must not panic.
	appendUnsignedHeaders(nil, "x", "y")
}
