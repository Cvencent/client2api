package openaicompat

import (
	"net/http"
	"testing"
	"time"
)

// The fixed values below were produced by an independent implementation of
// Huawei's documented SDK-HMAC-SHA256 canonicalisation.  Keeping the whole
// Authorization line pinned catches changes to path/query encoding or header
// ordering that a round-trip test would happily accept.
func TestOfficeAceSignRequestMatchesHuaweiCanonicalForm(t *testing.T) {
	got, err := signOfficeAceRequest(
		http.MethodPost,
		"https://example.test/v1/demo?b=two&a=one%20x&a=three",
		map[string]string{"Content-Type": "application/json", "X-Test": "  spaced  "},
		`{"b":2,"a":1}`,
		"test-access",
		"test-secret",
		time.Date(2026, 10, 16, 12, 34, 56, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	want := "SDK-HMAC-SHA256 Access=test-access, SignedHeaders=content-type;host;x-sdk-date;x-test, Signature=1060e70f54524abb3fe4a4101cb655b4057624f868e0a70d43a9339936fd664d"
	if got["Authorization"] != want {
		t.Fatalf("Authorization = %q, want %q", got["Authorization"], want)
	}
	if got["host"] != "example.test" || got["X-Sdk-Date"] != "20261016T123456Z" {
		t.Fatalf("signed headers = %#v", got)
	}
}

// The V11 values below were produced by an independent transcription of the
// client's own signer (V11-HMAC-SHA256 with the region-scoped derived key).
// Pinning the whole Authorization line catches scope or key-derivation drift
// that a round-trip test would happily accept.
func TestOfficeAceV11SignRequestMatchesClientSigner(t *testing.T) {
	got, err := signOfficeAceV11Request(
		http.MethodPost,
		"https://modelgw.example/v1/subscription/bonus/claim",
		map[string]string{"Content-Type": "application/json;charset=utf8"},
		"",
		"AK",
		"SK",
		"ST",
		"",
		"cn-southwest-2",
		time.Date(2026, 10, 16, 12, 34, 56, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	want := "V11-HMAC-SHA256 Credential=AK/20261016/cn-southwest-2/apic, " +
		"SignedHeaders=content-type;host;x-sdk-date;x-security-token, " +
		"Signature=7fc05be755be80bcc5ee88e8f032816bfba30f9ee2591206040a4608ffa246b6"
	if got["Authorization"] != want {
		t.Fatalf("Authorization =\n%q\nwant\n%q", got["Authorization"], want)
	}
	if got["X-Security-Token"] != "ST" || got["host"] != "modelgw.example" {
		t.Fatalf("signed headers = %#v", got)
	}
	if _, err := signOfficeAceV11Request(http.MethodGet, "https://modelgw.example/x", nil, "", "AK", "SK", "", "", "cn-southwest-2", time.Now()); err == nil {
		t.Fatal("V11 signing must reject a missing security token")
	}
}

// The URL encoding the signers use is stricter than encodeURIComponent: it also
// escapes ! * ' ( ).  Huawei's documented canonicalisation does the same.
func TestOfficeAceSignEncodingEscapesReservedPunctuation(t *testing.T) {
	got := officeAceEncodeComponent("a!b*c'd(e)f~g-h_i.j")
	want := "a%21b%2Ac%27d%28e%29f~g-h_i.j"
	if got != want {
		t.Fatalf("encode = %q, want %q", got, want)
	}
}
