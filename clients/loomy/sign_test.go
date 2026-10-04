package loomy

import (
	"strings"
	"testing"
)

// The vectors below are the module's proof that the signature is right.
//
// They were computed by an INDEPENDENT oracle -- PowerShell driving .NET's
// System.Security.Cryptography.HMACSHA1 and MD5 -- and not by this module's own
// code, so they fail if the canonical string, the escaping, the content hash or
// the key ever drifts.  Every input is frozen: a fixed secret, a fixed Date
// header and a fixed Nonce, so the expected signature is a constant rather than
// "no error".
const (
	vectorSecret = "test-sk"
	vectorDate   = "Mon, 02 Jan 2006 15:04:05 GMT"
	vectorNonce  = "00000000-0000-4000-8000-000000000000"
)

func TestSignatureGoldenVectors(t *testing.T) {
	cases := []struct {
		name    string
		options signOptions
		wantMD5 string
		wantSig string
		// wantLen is the exact length of the canonical string, which catches a
		// dropped or duplicated newline even if a coincidence kept the HMAC
		// stable.  Zero means "do not check".
		wantLen int
	}{
		{
			name: "post login body",
			options: signOptions{
				AccessKeyID:     "2thryby66wxi53sk",
				AccessKeySecret: vectorSecret,
				Method:          "POST",
				Path:            "/login/phone/sendMsgCode",
				Body:            `{"base":{"appid":"GM3LOOMY"},"param":{"ccode":"86","phone":"13800000000","expire":300}}`,
				ContentType:     "application/json",
				Date:            vectorDate,
				Nonce:           vectorNonce,
			},
			wantMD5: "+xa6TF0VZvejraiUoknykA==",
			wantSig: "SwMWjtMd4yHBTY1stX8QOLXH3Ds=",
			wantLen: 141,
		},
		{
			name: "get ledger with query",
			options: signOptions{
				AccessKeyID:     "2thryby66wxi53sk",
				AccessKeySecret: vectorSecret,
				Method:          "GET",
				Path:            "/points/records",
				QueryParams:     pointsRecordsQuery(),
				ContentType:     "application/json",
				Date:            vectorDate,
				Nonce:           vectorNonce,
			},
			wantMD5: "",
			wantSig: "fJXZ9Nln9hfZRsteTGXLt0cDr/M=",
		},
		{
			name: "escaping in path and query",
			options: signOptions{
				AccessKeyID:     "2thryby66wxi53sk",
				AccessKeySecret: vectorSecret,
				Method:          "POST",
				Path:            "/a b/ü",
				QueryParams:     []queryParam{{Key: "q", Value: "x y&z"}},
				ContentType:     "application/json",
				Date:            vectorDate,
				Nonce:           vectorNonce,
			},
			wantMD5: "",
			wantSig: "dmTuhlzGMuUDBhRdh9KZ83ZcKS0=",
		},
		{
			name: "real vendor secret and utf-8 body",
			options: signOptions{
				AccessKeyID:     "2thryby66wxi53sk",
				AccessKeySecret: defaultAccessKeySecret,
				Method:          "POST",
				Path:            "/x",
				Body:            `{"hello":"世界"}`,
				ContentType:     "application/json",
				Date:            vectorDate,
				Nonce:           vectorNonce,
			},
			wantMD5: "Aulw/A6cvXyhaQxbZTIGCQ==",
			wantSig: "X/ZlEzGIIMV7JXcIrinThITBF7I=",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contentMD5(tc.options.Body); got != tc.wantMD5 {
				t.Errorf("contentMD5(%q) = %q, want %q", tc.options.Body, got, tc.wantMD5)
			}
			canonical := canonicalString(tc.options)
			if tc.wantLen != 0 && len(canonical) != tc.wantLen {
				t.Errorf("canonical string is %d bytes, want %d:\n%q",
					len(canonical), tc.wantLen, canonical)
			}
			if got := signature(tc.options); got != tc.wantSig {
				t.Errorf("signature = %q, want %q\ncanonical string was:\n%q",
					got, tc.wantSig, canonical)
			}
		})
	}
}

// TestCanonicalStringEndsWithTwoNewlines pins the detail the reference calls out:
// the two trailing empty segments are part of the signed bytes.  Trimming them
// is the single easiest way to break every request, so it is asserted directly
// rather than only through a signature.
func TestCanonicalStringEndsWithTwoNewlines(t *testing.T) {
	o := signOptions{
		Method:      "POST",
		Path:        "/x",
		ContentType: "application/json",
		Date:        vectorDate,
		Nonce:       vectorNonce,
	}
	canonical := canonicalString(o)
	if !strings.HasSuffix(canonical, "\n\n") {
		t.Fatalf("canonical string must end with two newlines, got %q", canonical)
	}
	if strings.HasSuffix(canonical, "\n\n\n") {
		t.Fatalf("canonical string has a third trailing newline: %q", canonical)
	}
	if want := 9; len(strings.Split(canonical, "\n")) != want {
		t.Fatalf("canonical string has %d segments, want %d: %q",
			len(strings.Split(canonical, "\n")), want, canonical)
	}
}

// TestCanonicalStringOrderIsLoadBearing proves the method is upper-cased and the
// segments are in the reference's order, by rebuilding the string by hand.
func TestCanonicalStringOrderIsLoadBearing(t *testing.T) {
	o := signOptions{
		Method:      "post",
		Path:        "login/phone/sendMsgCode",
		Body:        "{}",
		ContentType: "application/json",
		Date:        vectorDate,
		Nonce:       vectorNonce,
	}
	got := canonicalString(o)
	want := strings.Join([]string{
		"POST",
		"/login/phone/sendMsgCode",
		"",
		contentMD5("{}"),
		"application/json",
		vectorDate,
		vectorNonce,
		"",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("canonical string\n got %q\nwant %q", got, want)
	}
}

func TestContentMD5EmptyBodyIsEmptyString(t *testing.T) {
	// The vendor's own sign.js returns "" for a bodyless request rather than
	// the MD5 of zero bytes.  Getting this wrong changes the fourth canonical
	// segment and every bodyless request fails.
	if got := contentMD5(""); got != "" {
		t.Fatalf("contentMD5(\"\") = %q, want the empty string", got)
	}
	if got := contentMD5(""); got == "1B2M2Y8AsgTpgAmY7PhCfg==" {
		t.Fatalf("contentMD5(\"\") returned the MD5 of zero bytes")
	}
}

func TestBuildEscapedPath(t *testing.T) {
	cases := map[string]string{
		"/":                        "/",
		"":                         "/",
		"/a/b":                     "/a/b",
		"/a/b/":                    "/a/b",
		"a/b":                      "/a/b",
		"/a b/ü":                   "/a%20b/%C3%BC",
		"/a/b?c=d":                 "/a/b%3Fc%3Dd",
		"/sp ace/one two/":         "/sp%20ace/one%20two",
		"/~tilde/-dash/._/":        "/~tilde/-dash/._",
		"/already%20escaped/":      "/already%2520escaped",
		"/points/records":          "/points/records",
		"/chat/completions":        "/chat/completions",
		"/login/phone/sendMsgCode": "/login/phone/sendMsgCode",
	}
	for in, want := range cases {
		if got := buildEscapedPath(in); got != want {
			t.Errorf("buildEscapedPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildEscapedQuery(t *testing.T) {
	if got := buildEscapedQuery(nil); got != "" {
		t.Errorf("buildEscapedQuery(nil) = %q, want empty", got)
	}
	// Order is the caller's, never sorted: the signature depends on it.
	got := buildEscapedQuery([]queryParam{
		{Key: "pageNo", Value: "1"},
		{Key: "pageSize", Value: "1"},
		{Key: "recordType", Value: "all"},
	})
	if want := "pageNo=1&pageSize=1&recordType=all"; got != want {
		t.Errorf("buildEscapedQuery = %q, want %q", got, want)
	}
	if got := buildEscapedQuery([]queryParam{{Key: "q", Value: "x y&z"}}); got != "q=x%20y%26z" {
		t.Errorf("buildEscapedQuery escaping = %q, want %q", got, "q=x%20y%26z")
	}
}

func TestEscapeRFC3986(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"abcXYZ019":   "abcXYZ019",
		"-._~":        "-._~",
		"x y&z":       "x%20y%26z",
		"!'()*":       "%21%27%28%29%2A",
		"a+b":         "a%2Bb",
		"a/b":         "a%2Fb",
		"世界":          "%E4%B8%96%E7%95%8C",
		"a=b":         "a%3Db",
		"100%":        "100%25",
		"trailing%20": "trailing%2520",
	}
	for in, want := range cases {
		if got := escapeRFC3986(in); got != want {
			t.Errorf("escapeRFC3986(%q) = %q, want %q", in, got, want)
		}
	}
	// Upper-case hex only: a lower-case escape would be a different signature.
	if got := escapeRFC3986("\x00\xff"); got != "%00%FF" {
		t.Errorf("escapeRFC3986 high bytes = %q, want %q", got, "%00%FF")
	}
}

func TestAuthHeadersFillsDefaults(t *testing.T) {
	o := signOptions{
		AccessKeyID:     "2thryby66wxi53sk",
		AccessKeySecret: vectorSecret,
		Method:          "GET",
		Path:            "/points/records",
	}
	headers := authHeaders(o)

	if headers["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", headers["Content-Type"])
	}
	if headers["Date"] == "" || headers["Nonce"] == "" {
		t.Fatalf("Date and Nonce must be filled in, got %q / %q", headers["Date"], headers["Nonce"])
	}
	if _, ok := headers["Content-MD5"]; ok {
		t.Errorf("Content-MD5 must be absent for a bodyless request, got %q", headers["Content-MD5"])
	}
	want := "account 2thryby66wxi53sk:" + signature(signOptions{
		AccessKeyID:     "2thryby66wxi53sk",
		AccessKeySecret: vectorSecret,
		Method:          "GET",
		Path:            "/points/records",
		ContentType:     "application/json",
		Date:            headers["Date"],
		Nonce:           headers["Nonce"],
	})
	if headers["Authorization"] != want {
		t.Errorf("Authorization = %q, want %q", headers["Authorization"], want)
	}
}

func TestAuthHeadersIncludesContentMD5ForBody(t *testing.T) {
	body := `{"a":1}`
	headers := authHeaders(signOptions{
		AccessKeyID:     "ak",
		AccessKeySecret: vectorSecret,
		Method:          "POST",
		Path:            "/x",
		Body:            body,
	})
	if got, want := headers["Content-MD5"], contentMD5(body); got != want {
		t.Fatalf("Content-MD5 = %q, want %q", got, want)
	}
}

func TestNewNonceIsUUIDv4(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		nonce := newNonce()
		if len(nonce) != 36 {
			t.Fatalf("nonce %q is %d characters, want 36", nonce, len(nonce))
		}
		if nonce[14] != '4' {
			t.Fatalf("nonce %q is not version 4", nonce)
		}
		switch nonce[19] {
		case '8', '9', 'a', 'b':
		default:
			t.Fatalf("nonce %q does not carry the RFC 4122 variant", nonce)
		}
		if seen[nonce] {
			t.Fatalf("nonce %q was minted twice", nonce)
		}
		seen[nonce] = true
	}
}
