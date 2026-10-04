package qwenwork

// Offline unit tests.  Nothing here touches the network: the transport is a
// fake http.RoundTripper and every credential lives in a t.TempDir().  There is
// deliberately no live test file, because no live credential exists for this
// client yet and the device-authorisation flow needs a human to click a URL.
//
// The crypto fixtures are pinned against values computed by an independent
// implementation (.NET's Aes / MD5 / SHA256), never by this package: a test that
// asserts "the code still does what the code did" proves nothing about the
// protocol.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// testIdentityJSON is the COSY identity object the desktop client sends, byte
// for byte.  Its length matters: 111 bytes pads to 112.
const testIdentityJSON = `{"aid":"a1","email":"u@example.com","name":"tester","security_oauth_token":"jwt.token.here","uid":"1234567890"}`

// testPayloadB64 is base64({"cosyVersion":"1.1.18","ideVersion":"1.0.5",
// "info":"QUVTSU5GTw==","requestId":"11111111-2222-4333-8444-555555555555",
// "version":"v1"}) in sorted-key order.
const testPayloadB64 = "eyJjb3N5VmVyc2lvbiI6IjEuMS4xOCIsImlkZVZlcnNpb24iOiIxLjAuNSIsImluZm8iOiJRVVZUU1U1R1R3PT0iLCJyZXF1ZXN0SWQiOiIxMTExMTExMS0yMjIyLTQzMzMtODQ0NC01NTU1NTU1NTU1NTUiLCJ2ZXJzaW9uIjoidjEifQ=="

// testSignature is md5(payloadB64 \n cosyKey \n date \n body \n path).
const testSignature = "61e4dd320359fa4794a52b326754d3b5"

const testRequestID = "11111111-2222-4333-8444-555555555555"
const testXRequestID = "22222222-3333-4444-8555-666666666666"
const testUnixDate = int64(1790601000)
const testCosyKey = "RkFLLUNPU1ktS0VZ"

// vendorPublicKeyPEM is the vendor's SubjectPublicKeyInfo, published by the
// reference project.  cosyModulusHex must equal the modulus inside it.
const vendorPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// ---------------------------------------------------------------------------
// COSY: crypto primitives
// ---------------------------------------------------------------------------

func TestAES128CBCPKCS7Vectors(t *testing.T) {
	key := []byte("0123456789abcdef")
	tests := []struct {
		name  string
		plain string
		want  string
	}{
		{
			name:  "identity: 111 bytes gains exactly one padding byte",
			plain: testIdentityJSON,
			want:  "DyULn5qXNKf1Oc636S2OYs20ET2OPaonwY94ICqXcba1nUO/k/lg2da6n3kWR/d1edEs68qc2fP2zSPeFrnykcz/kUXH9QphZjm9hp8JmYSs9o15WQCbYxBGFTwTdAaFxBwbzrWDOK9ScYLbqIpcdg==",
		},
		{
			name:  "block-aligned input gains a whole extra block",
			plain: "0123456789abcdef",
			want:  "C5sV2ktEoPUVHc/EwB811b8xuRnjiS3cO1khLWp7EeY=",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.plain == testIdentityJSON && len(testIdentityJSON) != 111 {
				t.Fatalf("fixture drifted: identity is %d bytes, the vector was computed for 111", len(testIdentityJSON))
			}
			got, err := aesCBCEncryptPKCS7([]byte(tc.plain), key)
			if err != nil {
				t.Fatalf("aesCBCEncryptPKCS7: %v", err)
			}
			if len(got)%aes.BlockSize != 0 || len(got) <= len(tc.plain) {
				t.Errorf("PKCS#7 must pad up to a larger multiple of %d: %d -> %d", aes.BlockSize, len(tc.plain), len(got))
			}
			if b64 := base64.StdEncoding.EncodeToString(got); b64 != tc.want {
				t.Errorf("ciphertext mismatch\n got %s\nwant %s", b64, tc.want)
			}
		})
	}

	if _, err := aesCBCEncryptPKCS7([]byte("x"), []byte("not-sixteen")); err == nil {
		t.Error("a key that is not 16 bytes must be rejected, not silently used")
	}
	if _, err := aesCBCEncryptPKCS7(nil, key); err != nil {
		t.Errorf("an empty plaintext must still encrypt to one block: %v", err)
	}
}

func TestCosyPublicKeyMatchesVendorPEM(t *testing.T) {
	block, _ := pem.Decode([]byte(vendorPublicKeyPEM))
	if block == nil {
		t.Fatal("fixture PEM did not decode")
	}
	var spki struct {
		Algorithm pkix.AlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(block.Bytes, &spki); err != nil {
		t.Fatalf("unmarshalling SubjectPublicKeyInfo: %v", err)
	}
	var inner struct {
		N *big.Int
		E int
	}
	if _, err := asn1.Unmarshal(spki.PublicKey.Bytes, &inner); err != nil {
		t.Fatalf("unmarshalling RSAPublicKey: %v", err)
	}
	if inner.N.BitLen() != 1024 {
		t.Fatalf("fixture is not RSA-1024: %d bits", inner.N.BitLen())
	}
	if inner.E != cosyExponent {
		t.Fatalf("fixture exponent = %d, want %d", inner.E, cosyExponent)
	}

	pub, err := cosyPublicKey()
	if err != nil {
		t.Fatalf("cosyPublicKey: %v", err)
	}
	if pub.N.Cmp(inner.N) != 0 {
		t.Error("cosyModulusHex does not match the modulus inside the vendor public key")
	}
	if pub.E != inner.E {
		t.Errorf("cosyPublicKey exponent = %d, want %d", pub.E, inner.E)
	}
	if pub.N.BitLen() != 1024 {
		t.Errorf("modulus is %d bits, want 1024", pub.N.BitLen())
	}
}

func TestRSAEncryptPKCS1v15RoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generating a throwaway key: %v", err)
	}
	ct, err := rsaEncryptPKCS1v15(&priv.PublicKey, []byte("temp-key"))
	if err != nil {
		t.Fatalf("rsaEncryptPKCS1v15: %v", err)
	}
	if len(ct) != 128 {
		t.Errorf("ciphertext is %d bytes, want one 1024-bit block", len(ct))
	}
	pt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, ct)
	if err != nil {
		t.Fatalf("the padding is not PKCS#1 v1.5: %v", err)
	}
	if string(pt) != "temp-key" {
		t.Errorf("round trip = %q, want %q", pt, "temp-key")
	}
}

func TestSortedCompactAndJSONValue(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"keys are sorted", map[string]any{"b": 1, "a": 2, "c": 3}, `{"a":2,"b":1,"c":3}`},
		{"HTML is not escaped", map[string]any{"url": "a&b<c>"}, `{"url":"a&b<c>"}`},
		{"nested objects survive", map[string]any{"z": map[string]any{"y": "x"}}, `{"z":{"y":"x"}}`},
		{"empty", map[string]any{}, `{}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedCompact(tc.in); got != tc.want {
				t.Errorf("sortedCompact = %s, want %s", got, tc.want)
			}
		})
	}

	// The identity the session seals must come out byte-for-byte identical to
	// the fixture the AES vector was computed from.
	got := sortedCompact(map[string]any{
		"uid":                  "1234567890",
		"aid":                  "a1",
		"name":                 "tester",
		"email":                "u@example.com",
		"security_oauth_token": "jwt.token.here",
	})
	if got != testIdentityJSON {
		t.Errorf("identity mismatch\n got %s\nwant %s", got, testIdentityJSON)
	}

	if v := jsonValue("a<b>&c"); v != `"a<b>&c"` {
		t.Errorf("jsonValue = %s, want %s", v, `"a<b>&c"`)
	}
}

func TestNewCosySessionSealsTheIdentity(t *testing.T) {
	sess, err := newCosySession("1234567890", "tester", "u@example.com", "jwt.token.here")
	if err != nil {
		t.Fatalf("newCosySession: %v", err)
	}
	if len(sess.tempKey) != aes.BlockSize {
		t.Fatalf("temp key is %d bytes, want %d", len(sess.tempKey), aes.BlockSize)
	}
	if sess.machineID == "" || sess.cosyKey == "" || sess.info == "" {
		t.Fatal("session fields must all be populated")
	}
	if id := newUUID(); id == "" {
		t.Fatal("newUUID returned an empty id")
	}
	if len(sess.machineID) != 36 {
		t.Errorf("machine id %q is not a uuid", sess.machineID)
	}

	// cosyKey is base64(RSAES-PKCS1-v1_5(tempKey)): one 1024-bit block.
	wrapped, err := base64.StdEncoding.DecodeString(sess.cosyKey)
	if err != nil {
		t.Fatalf("cosyKey is not base64: %v", err)
	}
	if len(wrapped) != 128 {
		t.Errorf("cosyKey is %d bytes, want 128", len(wrapped))
	}

	// info is base64(AES-128-CBC(identity, tempKey)) with the temp key as IV.
	ct, err := base64.StdEncoding.DecodeString(sess.info)
	if err != nil {
		t.Fatalf("info is not base64: %v", err)
	}
	blk, err := aes.NewCipher([]byte(sess.tempKey))
	if err != nil {
		t.Fatalf("the temp key is not a valid AES key: %v", err)
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(blk, []byte(sess.tempKey)).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(pt) {
		t.Fatalf("bad PKCS#7 padding byte %d", pad)
	}
	pt = pt[:len(pt)-pad]
	if !json.Valid(pt) {
		t.Fatalf("the sealed identity is not JSON: %q", pt)
	}
	var id map[string]string
	if err := json.Unmarshal(pt, &id); err != nil {
		t.Fatalf("sealed identity: %v", err)
	}
	want := map[string]string{
		"uid":                  "1234567890",
		"aid":                  "1234567890",
		"name":                 "tester",
		"email":                "u@example.com",
		"security_oauth_token": "jwt.token.here",
	}
	for k, v := range want {
		if id[k] != v {
			t.Errorf("identity[%q] = %q, want %q", k, id[k], v)
		}
	}
	if len(id) != len(want) {
		t.Errorf("identity has %d keys, want %d: %v", len(id), len(want), id)
	}
}

func TestNewUUIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		u := newUUID()
		if len(u) != 36 {
			t.Fatalf("uuid %q is %d chars, want 36", u, len(u))
		}
		for _, i := range []int{8, 13, 18, 23} {
			if u[i] != '-' {
				t.Errorf("uuid %q has no dash at %d", u, i)
			}
		}
		if u[14] != '4' {
			t.Errorf("uuid %q is not version 4", u)
		}
		if !strings.ContainsRune("89ab", rune(u[19])) {
			t.Errorf("uuid %q has a bad variant nibble %q", u, u[19])
		}
		if strings.ToLower(u) != u {
			t.Errorf("uuid %q must be lower case", u)
		}
		if seen[u] {
			t.Errorf("uuid %q repeated", u)
		}
		seen[u] = true
	}
}

func TestRandomHex(t *testing.T) {
	got, err := randomHex(16)
	if err != nil {
		t.Fatalf("randomHex: %v", err)
	}
	if len(got) != 16 {
		t.Errorf("randomHex(16) is %d chars, want 16", len(got))
	}
	for _, r := range got {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("randomHex produced a non-hex character %q", r)
		}
	}
	other, _ := randomHex(16)
	if got == other {
		t.Error("two random hex strings matched; the source is not random")
	}
}

// ---------------------------------------------------------------------------
// COSY: signing
// ---------------------------------------------------------------------------

func TestCosyPayloadB64Vector(t *testing.T) {
	if got := cosyPayloadB64("QUVTSU5GTw==", testRequestID); got != testPayloadB64 {
		t.Errorf("payload mismatch\n got %s\nwant %s", got, testPayloadB64)
	}
}

func TestCosySignatureVector(t *testing.T) {
	got := cosySignature(testPayloadB64, testCosyKey, testUnixDate, `{"a":1}`, "/api/v1/userinfo")
	if got != testSignature {
		t.Fatalf("signature = %s, want %s", got, testSignature)
	}

	// Every input must actually feed the digest: a signing function that
	// ignores one of them would still pass the fixed vector by luck.
	tests := []struct {
		name       string
		payload    string
		key        string
		date       int64
		body       string
		path       string
		wantChange bool
	}{
		{"payload", "x" + testPayloadB64, testCosyKey, testUnixDate, `{"a":1}`, "/api/v1/userinfo", true},
		{"cosy key", testPayloadB64, testCosyKey + "x", testUnixDate, `{"a":1}`, "/api/v1/userinfo", true},
		{"date", testPayloadB64, testCosyKey, testUnixDate + 1, `{"a":1}`, "/api/v1/userinfo", true},
		{"body", testPayloadB64, testCosyKey, testUnixDate, `{"a":2}`, "/api/v1/userinfo", true},
		{"path", testPayloadB64, testCosyKey, testUnixDate, `{"a":1}`, "/api/v1/userinfo2", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cosySignature(tc.payload, tc.key, tc.date, tc.body, tc.path)
			if (got != testSignature) != tc.wantChange {
				t.Errorf("changing %s did not change the signature (%s)", tc.name, got)
			}
		})
	}
}

func TestPathForSignature(t *testing.T) {
	tests := []struct {
		name   string
		rawURL string
		want   string
	}{
		{"the /algo mount point is stripped", "https://gateway.qwenwork.cn/algo/api/v1/userinfo", "/api/v1/userinfo"},
		{"the query is dropped", "https://gateway.qwenwork.cn/algo/api/v2/model/list?x=1", "/api/v2/model/list"},
		{"no /algo prefix", "https://gateway.qwenwork.cn/api/v1/userinfo", "/api/v1/userinfo"},
		{"only an exact /algo prefix", "https://gateway.qwenwork.cn/algoish/api", "/algoish/api"},
		{"nested path", "https://gateway.qwenwork.cn/algo/api/v2/service/pro/sse/agent_chat_generation", "/api/v2/service/pro/sse/agent_chat_generation"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pathForSignature(tc.rawURL)
			if err != nil {
				t.Fatalf("pathForSignature(%q): %v", tc.rawURL, err)
			}
			if got != tc.want {
				t.Errorf("pathForSignature(%q) = %q, want %q", tc.rawURL, got, tc.want)
			}
		})
	}
	if _, err := pathForSignature("://nope"); err == nil {
		t.Error("an unparseable URL must be an error, not a silent empty path")
	}
}

func TestCosyHeaders(t *testing.T) {
	sess := cosySession{
		machineID:   "machine-1",
		tempKey:     "0123456789abcdef",
		cosyKey:     testCosyKey,
		info:        "QUVTSU5GTw==",
		accessToken: "super-secret-access-token",
	}
	hdrs, err := sess.headers(cosyRequest{
		UID:        "u1",
		Body:       `{"a":1}`,
		RawURL:     "https://gateway.qwenwork.cn/algo/api/v1/userinfo",
		RequestID:  testRequestID,
		XRequestID: testXRequestID,
		UnixDate:   testUnixDate,
	})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}

	wantAuth := "Bearer COSY." + testPayloadB64 + "." + testSignature
	checks := map[string]string{
		"authorization":              wantAuth,
		"cosy-key":                   testCosyKey,
		"cosy-user":                  "u1",
		"cosy-date":                  "1790601000",
		"cosy-machineid":             "machine-1",
		"cosy-version":               cosyVersion,
		"cosy-clienttype":            cosyClientType,
		"cosy-business-product":      cosyProduct,
		"cosy-business-type":         cosyBusinessType,
		"cosy-scene":                 cosyScene,
		"cosy-machineos":             cosyMachineOS,
		"login-version":              cosyLoginVersion,
		"x-request-id":               testXRequestID,
		"x-qwenwork-version":         ideVersion,
		"x-qwenwork-release-version": releaseVersion,
		"x-qwenwork-build":           buildNumber,
		"x-qwenwork-platform":        cosyPlatform,
		"x-qwenwork-arch":            cosyArch,
		"x-qwenwork-channel":         cosyChannel,
		"accept":                     "text/event-stream",
		"content-type":               "application/json",
		"user-agent":                 defaultUserAgent,
	}
	for k, want := range checks {
		if got := hdrs[k]; got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := hdrs["x-model-key"]; ok {
		t.Error("x-model-key must be omitted when the call is not model-scoped")
	}

	// A model-scoped call adds the two model headers.
	scoped, err := sess.headers(cosyRequest{UID: "u1", Body: `{"a":1}`, RawURL: "https://x/algo/api/v1/userinfo", ModelKey: "pro"})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if scoped["x-model-key"] != "pro" || scoped["x-model-source"] != "system" {
		t.Errorf("model headers = %q/%q, want pro/system", scoped["x-model-key"], scoped["x-model-source"])
	}

	// A custom user agent is honoured.
	custom, err := sess.headers(cosyRequest{UID: "u1", Body: "", RawURL: "https://x/algo/api/v1/userinfo", UserAgent: "qoderwork/9.9.9"})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	if custom["user-agent"] != "qoderwork/9.9.9" {
		t.Errorf("user-agent = %q, want the configured one", custom["user-agent"])
	}

	// No header may leak the access token: the identity is sealed inside the
	// signature, never sent in the clear.
	for k, v := range hdrs {
		if strings.Contains(v, "super-secret-access-token") {
			t.Errorf("header %s leaks the access token", k)
		}
	}
}

// ---------------------------------------------------------------------------
// PKCE and the device flow
// ---------------------------------------------------------------------------

func TestNewPKCEVerifier(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		v, err := newPKCEVerifier()
		if err != nil {
			t.Fatalf("newPKCEVerifier: %v", err)
		}
		if len(v) != pkceVerifierLen {
			t.Fatalf("verifier is %d chars, want %d", len(v), pkceVerifierLen)
		}
		if len(v) < 43 || len(v) > 128 {
			t.Errorf("verifier is %d chars, outside RFC 7636's 43..128", len(v))
		}
		for _, r := range v {
			if !strings.ContainsRune(pkceAlphabet, r) {
				t.Errorf("verifier contains %q, which is outside the unreserved alphabet", r)
			}
		}
		if seen[v] {
			t.Errorf("verifier %q repeated", v)
		}
		seen[v] = true
	}
}

func TestPKCEChallengeS256(t *testing.T) {
	// RFC 7636, Appendix B.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	got := pkceChallengeS256(verifier)
	if got != want {
		t.Errorf("challenge = %s, want %s", got, want)
	}
	if strings.ContainsAny(got, "+/=") {
		t.Errorf("challenge %s must be base64url without padding", got)
	}
	if pkceChallengeS256(verifier+"x") == got {
		t.Error("the challenge does not depend on the verifier")
	}
}

func TestDeviceAuthURL(t *testing.T) {
	got := deviceAuthURL("https://gateway.qwenwork.cn", "CHAL", "NONCE", "MACHINE", "CLIENT", "qwenwork-cn://")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("deviceAuthURL produced %q: %v", got, err)
	}
	if u.Scheme != "https" || u.Host != "gateway.qwenwork.cn" {
		t.Errorf("URL points at %s://%s", u.Scheme, u.Host)
	}
	if u.Path != deviceAuthPath {
		t.Errorf("path = %q, want %q", u.Path, deviceAuthPath)
	}
	q := u.Query()
	checks := map[string]string{
		"challenge":        "CHAL",
		"challenge_method": "S256",
		"nonce":            "NONCE",
		"machine_id":       "MACHINE",
		"client_id":        "CLIENT",
		"redirect_uri":     "qwenwork-cn://",
	}
	for k, want := range checks {
		if q.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestUpsertStoredAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, accountsFile)

	if err := upsertStoredAccount("", account{UID: "x", AccessToken: "y"}); err != nil {
		t.Errorf("an empty path must be a no-op, got %v", err)
	}

	if err := upsertStoredAccount(path, account{UID: "1", Nickname: "first", AccessToken: "tok-1"}); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := upsertStoredAccount(path, account{UID: "2", AccessToken: "tok-2"}); err != nil {
		t.Fatalf("second insert: %v", err)
	}
	// A re-login of the same uid updates in place and clears the old verdict.
	if err := upsertStoredAccount(path, account{
		UID:           "1",
		Nickname:      "renamed",
		AccessToken:   "tok-1b",
		CooldownUntil: time.Now().Add(time.Hour).UnixMilli(),
		LastError:     "credits exhausted",
		Disabled:      true,
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", accountsFile, err)
	}
	var got []account
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("accounts.json is not an array of accounts: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d accounts, want 2: %s", len(got), raw)
	}
	if got[0].Nickname != "renamed" || got[0].AccessToken != "tok-1b" {
		t.Errorf("update did not win: %+v", got[0])
	}
	if got[0].CooldownUntil != 0 || got[0].LastError != "" || got[0].Disabled {
		t.Errorf("a fresh authorisation must revive the account: %+v", got[0])
	}
	if got[1].UID != "2" || got[1].AccessToken != "tok-2" {
		t.Errorf("the second account was disturbed: %+v", got[1])
	}
}

// ---------------------------------------------------------------------------
// the agent envelope
// ---------------------------------------------------------------------------

func TestMapModel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "pro"},
		{"pro", "pro"},
		{"auto", "pro"},
		{"advanced", "pro"},
		{"PRO", "pro"},
		{" Pro ", "pro"},
		{"lite", "flash"},
		{"flash", "flash"},
		{"max", "qwen3.8-max-preview"},
		{"qwen3.8-max", "qwen3.8-max-preview"},
		{"qwen3.8-max-preview", "qwen3.8-max-preview"},
		{"gpt-4o", "gpt-4o"},
	}
	for _, tc := range tests {
		if got := mapModel(tc.in); got != tc.want {
			t.Errorf("mapModel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildBodyEnvelope(t *testing.T) {
	req := &core.ChatRequest{
		Model: "pro",
		Messages: []core.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "and now?"},
		},
	}
	raw, err := buildBody(req, "pro", defaultMaxTokens)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}

	str := func(key string) string {
		v, _ := env[key].(string)
		return v
	}
	if str("request_id") == "" || str("session_id") == "" {
		t.Fatal("request_id and session_id must be populated")
	}
	if str("request_id") != str("request_set_id") || str("request_id") != str("chat_record_id") {
		t.Error("the three record ids must share one fresh uuid")
	}
	if str("session_id") == str("request_id") {
		t.Error("session_id must be a different uuid")
	}
	if env["stream"] != true {
		t.Error("stream must be true")
	}
	if str("chat_task") != "FREE_INPUT" {
		t.Errorf("chat_task = %q, want FREE_INPUT", str("chat_task"))
	}
	if str("agent_id") != "agent_common" || str("task_id") != "common" || str("session_type") != "qoder_work" {
		t.Errorf("agent envelope identity = %q/%q/%q", str("agent_id"), str("task_id"), str("session_type"))
	}
	if str("version") != "3" {
		t.Errorf("version = %q, want 3", str("version"))
	}
	if env["source"] != float64(1) {
		t.Errorf("source = %v, want 1", env["source"])
	}
	if env["is_reply"] != false {
		t.Error("is_reply must be false: this client never replies into a thread")
	}

	ctx, ok := env["chat_context"].(map[string]any)
	if !ok {
		t.Fatal("chat_context is missing")
	}
	if ctx["text"] != "and now?" {
		t.Errorf("chat_context.text = %v, want the last user turn", ctx["text"])
	}
	extra, _ := ctx["extra"].(map[string]any)
	if extra["originalContent"] != "and now?" {
		t.Errorf("originalContent = %v, want the last user turn", extra["originalContent"])
	}
	mc, _ := extra["modelConfig"].(map[string]any)
	if mc["key"] != "pro" || mc["is_reasoning"] != false {
		t.Errorf("chat_context.modelConfig = %v", mc)
	}

	model, _ := env["model_config"].(map[string]any)
	if model["key"] != "pro" || model["format"] != "openai" || model["source"] != "system" {
		t.Errorf("model_config = %v", model)
	}
	if model["is_vl"] != true {
		t.Error("is_vl must be true: the vendor catalogue is multimodal")
	}
	if model["max_input_tokens"] != float64(defaultContextLength) {
		t.Errorf("max_input_tokens = %v, want %d", model["max_input_tokens"], defaultContextLength)
	}

	biz, _ := env["business"].(map[string]any)
	if biz["product"] != cosyProduct || biz["type"] != cosyBusinessType || biz["version"] != "1" {
		t.Errorf("business = %v", biz)
	}

	if env["system"] != "be brief" {
		t.Errorf("system = %v, want the joined system prompt", env["system"])
	}
	msgs, _ := env["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (the system turn is hoisted out)", len(msgs))
	}
	for i, wantRole := range []string{"user", "assistant", "user"} {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != wantRole {
			t.Errorf("messages[%d].role = %v, want %s", i, m["role"], wantRole)
		}
	}
	if _, ok := env["tools"]; ok {
		t.Error("tools must be omitted when the request carries none")
	}
}

func TestBuildBodySystemJoiningAndPromptFallback(t *testing.T) {
	tests := []struct {
		name       string
		messages   []core.Message
		wantSystem string
		wantText   string
		wantMsgs   int
	}{
		{
			name:       "system and developer are joined",
			messages:   []core.Message{{Role: "system", Content: "a"}, {Role: "developer", Content: "b"}, {Role: "user", Content: "q"}},
			wantSystem: "a\n\nb",
			wantText:   "q",
			wantMsgs:   1,
		},
		{
			name:       "a tool-result-only turn still needs a prompt",
			messages:   []core.Message{{Role: "tool", Content: "42", ToolCallID: "call_1"}},
			wantSystem: "",
			wantText:   "ping",
			wantMsgs:   1,
		},
		{
			name:       "the first user turn is the fallback when the last is empty",
			messages:   []core.Message{{Role: "user", Content: "first"}, {Role: "assistant", Content: "reply"}, {Role: "user", Content: ""}},
			wantSystem: "",
			wantText:   "first",
			wantMsgs:   3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := buildBody(&core.ChatRequest{Model: "pro", Messages: tc.messages}, "pro", defaultMaxTokens)
			if err != nil {
				t.Fatalf("buildBody: %v", err)
			}
			var env map[string]any
			if err := json.Unmarshal(raw, &env); err != nil {
				t.Fatalf("envelope is not JSON: %v", err)
			}
			if env["system"] != tc.wantSystem {
				t.Errorf("system = %v, want %q", env["system"], tc.wantSystem)
			}
			ctx, _ := env["chat_context"].(map[string]any)
			if ctx["text"] != tc.wantText {
				t.Errorf("chat_context.text = %v, want %q", ctx["text"], tc.wantText)
			}
			msgs, _ := env["messages"].([]any)
			if len(msgs) != tc.wantMsgs {
				t.Errorf("messages = %d, want %d", len(msgs), tc.wantMsgs)
			}
		})
	}
}

func TestBuildBodyCarriesToolCalls(t *testing.T) {
	req := &core.ChatRequest{
		Model: "pro",
		Messages: []core.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"sf"}`}}},
			{Role: "tool", Content: "sunny", ToolCallID: "call_1", Name: "get_weather"},
		},
	}
	raw, err := buildBody(req, "pro", defaultMaxTokens)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	msgs, _ := env["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", asst["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("tool call = %v, want id call_1 and type function", call)
	}
	fn, _ := call["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"sf"}` {
		t.Errorf("tool call function = %v", fn)
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "call_1" || tool["name"] != "get_weather" {
		t.Errorf("tool result = %v", tool)
	}
}

func TestBuildToolsAndToolChoice(t *testing.T) {
	tools := []core.Tool{
		{Type: "function", Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Type: "web_search", Name: "ignored"},
		{Type: "function", Name: ""},
	}
	raw, err := buildBody(&core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "q"}}, Tools: tools}, "pro", defaultMaxTokens)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	list, _ := env["tools"].([]any)
	if len(list) != 1 {
		t.Fatalf("tools = %v, want only the one function tool", env["tools"])
	}
	tool, _ := list[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tool type = %v, want function", tool["type"])
	}
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["description"] != "weather" {
		t.Errorf("tool function = %v", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("tool parameters must survive as an object, got %v", fn["parameters"])
	}

	// tool_choice "none" suppresses the whole block.
	raw, err = buildBody(&core.ChatRequest{
		Model:      "pro",
		Messages:   []core.Message{{Role: "user", Content: "q"}},
		Tools:      tools,
		ToolChoice: json.RawMessage(`"none"`),
	}, "pro", defaultMaxTokens)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	env = map[string]any{}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("envelope is not JSON: %v", err)
	}
	if _, ok := env["tools"]; ok {
		t.Error("tool_choice \"none\" must drop the tools block")
	}
}

func TestToolsRequested(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"unset means the default is allowed", "", true},
		{"null", "null", true},
		{"none", `"none"`, false},
		{"auto", `"auto"`, true},
		{"a named function", `{"type":"function","function":{"name":"f"}}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolsRequested(json.RawMessage(tc.in)); got != tc.want {
				t.Errorf("toolsRequested(%s) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildParameters(t *testing.T) {
	temp, topP := 0.3, 0.9
	maxTokens := 1000

	got := buildParameters(&core.ChatRequest{
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTokens,
		Options:     map[string]any{"presence_penalty": 0.5, "frequency_penalty": json.Number("0.25")},
	}, defaultMaxTokens)
	if got["temperature"] != 0.3 || got["top_p"] != 0.9 {
		t.Errorf("sampling parameters = %v", got)
	}
	if got["max_tokens"] != 1000 {
		t.Errorf("max_tokens = %v, want the caller's 1000", got["max_tokens"])
	}
	if got["presence_penalty"] != 0.5 {
		t.Errorf("presence_penalty = %v", got["presence_penalty"])
	}
	if got["frequency_penalty"] != 0.25 {
		t.Errorf("frequency_penalty = %v, want the value behind a json.Number", got["frequency_penalty"])
	}

	// A caller asking for more than the vendor allows is clamped.
	huge := maxOutputTokens + 1
	got = buildParameters(&core.ChatRequest{MaxTokens: &huge}, defaultMaxTokens)
	if got["max_tokens"] != maxOutputTokens {
		t.Errorf("max_tokens = %v, want it clamped to %d", got["max_tokens"], maxOutputTokens)
	}

	// No limit asked for: the module default applies.
	got = buildParameters(&core.ChatRequest{}, defaultMaxTokens)
	if got["max_tokens"] != defaultMaxTokens {
		t.Errorf("max_tokens = %v, want the default %d", got["max_tokens"], defaultMaxTokens)
	}
	if _, ok := got["temperature"]; ok {
		t.Error("an unset temperature must not be sent at all")
	}
}

func TestOptionFloat(t *testing.T) {
	opts := map[string]any{
		"a":   float64(1.5),
		"b":   json.Number("2.5"),
		"c":   "3.5",
		"d":   4,
		"bad": "not a number",
	}
	tests := []struct {
		name string
		keys []string
		want float64
		ok   bool
	}{
		{"float64", []string{"a"}, 1.5, true},
		{"json.Number", []string{"b"}, 2.5, true},
		{"string", []string{"c"}, 3.5, true},
		{"int", []string{"d"}, 4, true},
		{"first alias wins", []string{"missing", "a"}, 1.5, true},
		{"unparseable", []string{"bad"}, 0, false},
		{"absent", []string{"nope"}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := optionFloat(opts, tc.keys...)
			if ok != tc.ok || got != tc.want {
				t.Errorf("optionFloat(%v) = (%v, %v), want (%v, %v)", tc.keys, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestBuildBodyRejectsImages(t *testing.T) {
	req := &core.ChatRequest{
		Model: "pro",
		Messages: []core.Message{{
			Role:    "user",
			Content: "what is this?",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what is this?"},
				{Type: "image_url", ImageURL: "https://example.com/cat.png"},
			},
		}},
	}
	if _, err := buildBody(req, "pro", defaultMaxTokens); !errors.Is(err, errImageUnsupported) {
		t.Fatalf("buildBody error = %v, want errImageUnsupported", err)
	}
	if _, err := buildBody(nil, "pro", defaultMaxTokens); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("a nil request must be ErrUnsupported, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// SSE framing and chunk decoding
// ---------------------------------------------------------------------------

func TestReadFrames(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name: "data lines, comments and blank separators",
			input: ": keepalive\n\n" +
				"data: {\"a\":1}\n\n" +
				"\n" +
				"data: {\"b\":2}\n\n",
			want: []string{`{"a":1}`, `{"b":2}`},
		},
		{
			name:  "a bare JSON line is accepted",
			input: "{\"a\":1}\n\n",
			want:  []string{`{"a":1}`},
		},
		{
			name:  "multi-line data is joined with newlines",
			input: "data: {\"a\":\ndata: 1}\n\n",
			want:  []string{"{\"a\":\n1}"},
		},
		{
			name:  "no trailing newline is still flushed",
			input: "data: {\"a\":1}",
			want:  []string{`{"a":1}`},
		},
		{
			name:  "an empty stream yields nothing",
			input: "",
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			err := readFrames(context.Background(), strings.NewReader(tc.input), func(payload string) error {
				got = append(got, payload)
				return nil
			})
			if err != nil {
				t.Fatalf("readFrames: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d payloads %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("payload %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	// "[DONE]" stops the loop cleanly: the handler returns errStreamDone and
	// readFrames turns that back into nil.
	var seen []string
	err := readFrames(context.Background(), strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]\n\ndata: {\"never\":true}\n\n"), func(payload string) error {
		if payload == "[DONE]" {
			return errStreamDone
		}
		seen = append(seen, payload)
		return nil
	})
	if err != nil {
		t.Fatalf("readFrames: %v", err)
	}
	if len(seen) != 1 || seen[0] != `{"a":1}` {
		t.Errorf("payloads after [DONE] must not be delivered, got %q", seen)
	}

	// A handler error that is not errStreamDone is propagated.
	boom := errors.New("boom")
	if err := readFrames(context.Background(), strings.NewReader("data: {\"a\":1}\n\n"), func(string) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("handler error = %v, want boom", err)
	}

	// Cancellation is noticed between lines.  The handler cancels from inside the
	// reader goroutine, so the loop is guaranteed to reach its between-lines check
	// with a dead context.  A cancel issued from the test goroutine instead races
	// the next ReadString: either it lands before the loop starts, or the loop is
	// already parked inside a read that a stalled pipe will never feed, and the
	// contract never promised that a read in flight is interruptible.
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- readFrames(ctx, pr, func(string) error {
			cancel()
			return nil
		})
	}()
	if _, err := pw.Write([]byte("data: {\"a\":1}\n\n")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("readFrames after cancellation = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readFrames did not return after the context was cancelled")
	}
}

func TestUnwrapFrame(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantOK     bool
		wantRaw    string
		wantErrSub string
	}{
		{"a bare OpenAI chunk", `{"choices":[{"delta":{"content":"hi"}}]}`, true, `{"choices":[{"delta":{"content":"hi"}}]}`, ""},
		{"an object envelope", `{"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}","statusCodeValue":200}`, true, `{"choices":[{"delta":{"content":"hi"}}]}`, ""},
		{"an empty object", `{}`, false, "", ""},
		{"done", `[DONE]`, false, "", ""},
		{"blank", `   `, false, "", ""},
		{"non-JSON becomes the message", `upstream exploded`, true, "upstream exploded", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, ok, errMsg := unwrapFrame(tc.payload)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (errMsg %q)", ok, tc.wantOK, errMsg)
			}
			if ok && raw != tc.wantRaw {
				t.Errorf("raw = %q, want %q", raw, tc.wantRaw)
			}
			if tc.wantErrSub != "" && !strings.Contains(errMsg, tc.wantErrSub) {
				t.Errorf("errMsg = %q, want it to contain %q", errMsg, tc.wantErrSub)
			}
		})
	}

	// A frame that carries a >= 400 statusCodeValue is an error, not a chunk.
	raw, ok, errMsg := unwrapFrame(`{"body":"{\"code\":14018,\"message\":\"credits exhausted\"}","statusCodeValue":429}`)
	if ok {
		t.Errorf("a 429 envelope must not decode as a chunk (raw %q)", raw)
	}
	if errMsg == "" {
		t.Error("a 429 envelope must carry a message")
	}
}

func TestParseChunk(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		check func(*testing.T, sseChunk)
	}{
		{
			name: "content, reasoning and finish",
			raw:  `{"choices":[{"delta":{"content":"hi","reasoning_content":"why"},"finish_reason":"stop"}]}`,
			check: func(t *testing.T, c sseChunk) {
				if c.Delta != "hi" || c.Reasoning != "why" || c.Finish != "stop" {
					t.Errorf("chunk = %+v", c)
				}
			},
		},
		{
			name: "a whole message instead of a delta",
			raw:  `{"choices":[{"message":{"content":"whole"}}]}`,
			check: func(t *testing.T, c sseChunk) {
				if c.Delta != "whole" {
					t.Errorf("delta = %q, want whole", c.Delta)
				}
			},
		},
		{
			name: "tool call deltas",
			raw:  `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
			check: func(t *testing.T, c sseChunk) {
				if len(c.ToolCalls) != 1 {
					t.Fatalf("tool calls = %v", c.ToolCalls)
				}
				tc := c.ToolCalls[0]
				if tc.Index != 0 || tc.ID != "call_1" || tc.Name != "f" || tc.Arguments != "{}" {
					t.Errorf("tool call = %+v", tc)
				}
			},
		},
		{
			name: "usage with a derived total",
			raw:  `{"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
			check: func(t *testing.T, c sseChunk) {
				if c.Usage == nil || c.Usage.PromptTokens != 3 || c.Usage.CompletionTokens != 2 || c.Usage.TotalTokens != 5 {
					t.Errorf("usage = %+v", c.Usage)
				}
			},
		},
		{
			name: "an error frame",
			raw:  `{"error":{"message":"boom","type":"upstream_error"}}`,
			check: func(t *testing.T, c sseChunk) {
				if c.Err != "boom" {
					t.Errorf("err = %q, want boom", c.Err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chunk, ok := parseChunk(tc.raw)
			if !ok {
				t.Fatalf("parseChunk(%s) reported a decode failure", tc.raw)
			}
			tc.check(t, chunk)
		})
	}

	if _, ok := parseChunk("not json"); ok {
		t.Error("a non-JSON chunk must report a decode failure")
	}
	empty, ok := parseChunk("{}")
	if !ok || !empty.empty() {
		t.Errorf("an empty object decodes to an empty chunk: %+v ok=%v", empty, ok)
	}
}

func TestToolCallDeltas(t *testing.T) {
	if got := toolCallDeltas(nil); got != nil {
		t.Errorf("toolCallDeltas(nil) = %v, want nil", got)
	}
	if got := toolCallDeltas([]any{}); got != nil {
		t.Errorf("toolCallDeltas(empty) = %v, want nil", got)
	}
	got := toolCallDeltas([]any{
		map[string]any{"index": float64(0), "id": "call_1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		map[string]any{"function": map[string]any{"arguments": "x"}},
		map[string]any{"index": float64(2)},
	})
	if len(got) != 2 {
		t.Fatalf("got %d deltas, want 2 (an empty entry is dropped): %+v", len(got), got)
	}
	if got[0].Index != 0 || got[0].ID != "call_1" || got[0].Name != "f" || got[0].Arguments != "{}" {
		t.Errorf("delta 0 = %+v", got[0])
	}
	if got[1].Index != 1 || got[1].Arguments != "x" {
		t.Errorf("delta 1 = %+v, want the positional index and the argument fragment", got[1])
	}
}

func TestUsageFromAny(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want *core.Usage
	}{
		{"nil", nil, nil},
		{"not an object", "usage", nil},
		{"empty", map[string]any{}, nil},
		{"all zero is not a usage event", map[string]any{"prompt_tokens": 0, "completion_tokens": 0}, nil},
		{"plain", map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}, &core.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
		{"total is derived", map[string]any{"prompt_tokens": 3, "completion_tokens": 2}, &core.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}},
		{"input/output aliases", map[string]any{"input_tokens": 5, "output_tokens": 7}, &core.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12}},
		{"cache aliases", map[string]any{"prompt_tokens": 10, "completion_tokens": 1, "prompt_cache_hit_tokens": 4}, &core.Usage{PromptTokens: 10, CompletionTokens: 1, TotalTokens: 11, CachedTokens: 4}},
		{"cached in details", map[string]any{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 9}}, &core.Usage{PromptTokens: 10, TotalTokens: 10, CachedTokens: 9}},
		{"reasoning in details", map[string]any{"prompt_tokens": 1, "completion_tokens_details": map[string]any{"reasoning_tokens": 4}}, &core.Usage{PromptTokens: 1, TotalTokens: 1, ReasoningTokens: 4}},
		{"numbers as strings", map[string]any{"prompt_tokens": "12", "completion_tokens": json.Number("1")}, &core.Usage{PromptTokens: 12, CompletionTokens: 1, TotalTokens: 13}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := usageFromAny(tc.in)
			if (got == nil) != (tc.want == nil) {
				t.Fatalf("usage = %+v, want %+v", got, tc.want)
			}
			if got != nil && *got != *tc.want {
				t.Errorf("usage = %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func TestNormalizeFinish(t *testing.T) {
	tests := []struct {
		finish string
		tool   bool
		want   string
	}{
		{"stop", false, "stop"},
		{"end_turn", false, "stop"},
		{"eos", false, "stop"},
		{"STOP", false, "stop"},
		{" length ", false, "length"},
		{"max_tokens", false, "length"},
		{"max_output_tokens", false, "length"},
		{"tool_calls", false, "tool_calls"},
		{"function_call", false, "tool_calls"},
		{"content_filter", false, "content_filter"},
		{"", false, "stop"},
		{"", true, "tool_calls"},
		{"something_new", true, "tool_calls"},
		{"something_new", false, "stop"},
	}
	for _, tc := range tests {
		if got := normalizeFinish(tc.finish, tc.tool); got != tc.want {
			t.Errorf("normalizeFinish(%q, %v) = %q, want %q", tc.finish, tc.tool, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// the stream
// ---------------------------------------------------------------------------

const chatSSEFrames = "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"lo\",\"reasoning_content\":\"why\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

const toolSSEFrames = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"sf\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
	"data: [DONE]\n\n"

// drain reads a stream to its end.
func drain(t *testing.T, s core.Stream) []core.Event {
	t.Helper()
	var out []core.Event
	for i := 0; i < 64; i++ {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv returned %v after %d events", err, len(out))
		}
		out = append(out, ev)
	}
	t.Fatalf("the stream produced 64 events without ending")
	return nil
}

func TestQwenStreamEmitsNormalisedEvents(t *testing.T) {
	s := newQwenStream(context.Background(), nil, io.NopCloser(strings.NewReader(chatSSEFrames)))
	defer s.Close()

	got := drain(t, s)
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	if got[0].Type != core.EventDelta || got[0].Delta != "Hel" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].Type != core.EventDelta || got[1].Delta != "lo" || got[1].Reasoning != "why" {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[2].Type != core.EventUsage || got[2].Usage == nil || got[2].Usage.TotalTokens != 5 {
		t.Errorf("event 2 = %+v", got[2])
	}
	if got[3].Type != core.EventDone || got[3].Finish != "stop" {
		t.Errorf("event 3 = %+v", got[3])
	}

	// Recv keeps returning io.EOF once the stream has ended.
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after the end = %v, want io.EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close must be idempotent and quiet, got %v", err)
	}
}

func TestQwenStreamToolCalls(t *testing.T) {
	s := newQwenStream(context.Background(), nil, io.NopCloser(strings.NewReader(toolSSEFrames)))
	defer s.Close()

	got := drain(t, s)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[0].Type != core.EventToolCall || got[0].ToolCall == nil {
		t.Fatalf("event 0 = %+v", got[0])
	}
	if got[0].ToolCall.Index != 0 || got[0].ToolCall.ID != "call_1" || got[0].ToolCall.Name != "get_weather" || got[0].ToolCall.Arguments != `{"city"` {
		t.Errorf("first tool call delta = %+v", got[0].ToolCall)
	}
	if got[1].Type != core.EventToolCall || got[1].ToolCall == nil {
		t.Fatalf("event 1 = %+v", got[1])
	}
	if got[1].ToolCall.Index != got[0].ToolCall.Index {
		t.Errorf("the argument fragment landed on index %d, want the same index as the opening delta (%d)", got[1].ToolCall.Index, got[0].ToolCall.Index)
	}
	if got[1].ToolCall.Arguments != `:"sf"}` {
		t.Errorf("argument fragment = %q", got[1].ToolCall.Arguments)
	}
	// The two fragments are halves of one argument object: the vendor streams
	// the JSON in pieces, so reassembling them must yield valid JSON.
	if joined := got[0].ToolCall.Arguments + got[1].ToolCall.Arguments; !json.Valid([]byte(joined)) {
		t.Errorf("the reassembled arguments are not valid JSON: %q", joined)
	}
	if got[2].Type != core.EventDone || got[2].Finish != "tool_calls" {
		t.Errorf("event 2 = %+v", got[2])
	}
}

func TestQwenStreamErrorFrame(t *testing.T) {
	s := newQwenStream(context.Background(), nil, io.NopCloser(strings.NewReader("data: {\"error\":{\"message\":\"boom\",\"type\":\"upstream_error\"}}\n\n")))
	defer s.Close()

	got := drain(t, s)
	if len(got) != 1 {
		t.Fatalf("got %d events, want exactly one error: %+v", len(got), got)
	}
	if got[0].Type != core.EventError || got[0].Err == nil {
		t.Fatalf("event 0 = %+v", got[0])
	}
	if !strings.Contains(got[0].Err.Error(), "boom") {
		t.Errorf("error = %v, want it to mention the upstream message", got[0].Err)
	}
	for _, ev := range got {
		if ev.Type == core.EventDone {
			t.Error("a stream that ended in an error must not also report done")
		}
	}
}

func TestQwenStreamEmptyBody(t *testing.T) {
	s := newQwenStream(context.Background(), nil, io.NopCloser(strings.NewReader("")))
	defer s.Close()

	got := drain(t, s)
	if len(got) != 1 || got[0].Type != core.EventError {
		t.Fatalf("an empty body must yield one error event, got %+v", got)
	}
	if !errors.Is(got[0].Err, errEmptyStream) {
		t.Errorf("error = %v, want errEmptyStream", got[0].Err)
	}
}

func TestWatchdogBodyIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	w := newWatchdogBody(context.Background(), pr, 50*time.Millisecond)
	defer w.Close()

	start := time.Now()
	buf := make([]byte, 8)
	_, err := w.Read(buf) // no writer will ever send: only the watchdog can end this
	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("Read = %v, want errIdleTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the watchdog took %s to fire", elapsed)
	}

	// A body that keeps producing data is never cut off, and Close is quiet.
	pr2, pw2 := io.Pipe()
	w2 := newWatchdogBody(context.Background(), pr2, 250*time.Millisecond)
	go func() {
		for i := 0; i < 3; i++ {
			time.Sleep(60 * time.Millisecond)
			_, _ = pw2.Write([]byte("x"))
		}
		pw2.Close()
	}()
	total := 0
	for {
		n, err := w2.Read(buf)
		total += n
		if err != nil {
			break
		}
	}
	if total != 3 {
		t.Errorf("read %d bytes, want 3 (the idle timer must reset on every read)", total)
	}
	if err := w2.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := w2.Close(); err != nil {
		t.Errorf("the second Close = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// classification and the cooldown policy
// ---------------------------------------------------------------------------

func TestCreditExhausted(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"the vendor's 14018 code", `{"code":14018,"message":"failed"}`, true},
		{"the code with spaces", `{"code" : 14018 }`, true},
		{"a different code", `{"code":14019}`, true},
		{"english marker", `{"message":"Credits Exhausted"}`, true},
		{"chinese marker", `{"message":"积分不足"}`, true},
		{"chinese quota marker", `{"message":"额度用尽"}`, true},
		{"a plain rate limit", `{"message":"too many requests"}`, false},
		{"empty", ``, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := creditExhausted(tc.body); got != tc.want {
				t.Errorf("creditExhausted(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   errKind
	}{
		{"200 is not a failure", 200, "", kindNone},
		{"401", 401, "", kindAuth},
		{"403", 403, "", kindAuth},
		{"402 is always quota", 402, "", kindQuota},
		{"429 with a credit body is quota", 429, `{"code":14018}`, kindQuota},
		{"429 with credits exhausted is quota", 429, `{"message":"credits exhausted"}`, kindQuota},
		{"429 alone is transient", 429, `{"message":"slow down"}`, kindTransient},
		{"500", 500, "", kindTransient},
		{"503", 503, "", kindTransient},
		{"400 is the caller's fault", 400, "", kindClient},
		{"404 is the caller's fault", 404, "", kindClient},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.status, tc.body); got != tc.want {
				t.Errorf("classify(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestCooldownPolicy(t *testing.T) {
	cd := cooldownConfig{def: defaultCooldown, short: defaultShortCooldown, quota: defaultQuotaCooldown}
	tests := []struct {
		name      string
		kind      errKind
		want      time.Duration
		wantState string
	}{
		{"a quota exhaustion parks the account for a day", kindQuota, 24 * time.Hour, stateExhausted},
		{"a transient failure cools briefly", kindTransient, defaultShortCooldown, stateCooling},
		{"a network failure cools briefly", kindNetwork, defaultShortCooldown, stateCooling},
		{"an auth failure cools for the default", kindAuth, defaultCooldown, stateCooling},
		{"a bad request never parks a healthy account", kindClient, 0, stateUnknown},
		{"an unclassified failure earns no cooldown", kindNone, 0, stateUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, state := cooldownFor(tc.kind, cd)
			if d != tc.want {
				t.Errorf("cooldown = %s, want %s", d, tc.want)
			}
			if state != tc.wantState {
				t.Errorf("state = %q, want %q", state, tc.wantState)
			}
		})
	}
	if d, _ := cooldownFor(kindQuota, cd); d != 24*time.Hour {
		t.Errorf("the 14018 quota cooldown must be 24h, got %s", d)
	}

	// The policy honours the configured durations rather than hard-coded ones.
	custom := cooldownConfig{def: 5 * time.Second, short: time.Second, quota: 30 * time.Minute}
	if d, _ := cooldownFor(kindQuota, custom); d != 30*time.Minute {
		t.Errorf("configured quota cooldown = %s, want 30m", d)
	}
	if d, _ := cooldownFor(kindTransient, custom); d != time.Second {
		t.Errorf("configured short cooldown = %s, want 1s", d)
	}
}

// TestAClientErrorNeverParksAHealthyAccount pins the pool-level consequence of
// the policy above.  A live run found the defect in cline and qwenwork shared
// it: a request-side error (a 400 the vendor rejected for a model it does not
// serve) parked a perfectly good credential, so the NEXT request — for a model
// that does work — was answered "no healthy account" for the whole cooldown.
// retryable() already refuses to rotate on a client error; the account that
// sent it is still healthy and must stay selectable.
func TestAClientErrorNeverParksAHealthyAccount(t *testing.T) {
	dir := t.TempDir()
	cd := cooldownConfig{def: defaultCooldown, short: defaultShortCooldown, quota: defaultQuotaCooldown}
	p := newPool(filepath.Join(dir, "accounts.json"), filepath.Join(dir, "state.json"), time.Minute, cd, nil)
	p.put(account{
		ID:          "a1",
		UID:         "u1",
		AccessToken: "tok-live",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	e := p.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a live account")
	}
	p.markFailureWith(e, kindClient, "model does not exist", 0)

	if got := p.usable(); len(got) != 1 {
		t.Fatalf("usable() = %d entries after a client error, want the account still selectable", len(got))
	}
	snap := p.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot() = %d rows, want 1", len(snap))
	}
	if snap[0].State != stateReady {
		t.Fatalf("state = %q after a client error, want %q", snap[0].State, stateReady)
	}
	if !p.ready() {
		t.Fatal("pool reports not ready after a client error on a healthy account")
	}
}

// TestAnAuthErrorStillParksTheAccount is the other half: removing kindClient
// from the park arm must not weaken the arm that IS the credential's fault.
func TestAnAuthErrorStillParksTheAccount(t *testing.T) {
	dir := t.TempDir()
	cd := cooldownConfig{def: defaultCooldown, short: defaultShortCooldown, quota: defaultQuotaCooldown}
	p := newPool(filepath.Join(dir, "accounts.json"), filepath.Join(dir, "state.json"), time.Minute, cd, nil)
	p.put(account{
		ID:          "a1",
		UID:         "u1",
		AccessToken: "tok-live",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	e := p.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a live account")
	}
	p.markFailureWith(e, kindAuth, "401 unauthorized", 0)

	if got := p.usable(); len(got) != 0 {
		t.Fatalf("usable() = %d entries after an auth error, want 0", len(got))
	}
	if snap := p.snapshot(); len(snap) != 1 || snap[0].State != stateCooling {
		t.Fatalf("snapshot() = %#v after an auth error, want one cooling row", snap)
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		kind errKind
		want bool
	}{
		{kindNone, false},
		{kindQuota, true},
		{kindTransient, true},
		{kindNetwork, true},
		{kindAuth, true},
		{kindClient, false},
	}
	for _, tc := range tests {
		if got := retryable(tc.kind); got != tc.want {
			t.Errorf("retryable(%v) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestCleanErrorText(t *testing.T) {
	got := cleanErrorText("<html><head><style>p{}</style></head><body><p>Credits\n  exhausted</p></body></html>")
	if strings.Contains(got, "<") || strings.Contains(got, "\n") {
		t.Errorf("cleanErrorText left markup or newlines: %q", got)
	}
	if !strings.Contains(got, "Credits exhausted") {
		t.Errorf("cleanErrorText = %q, want the collapsed message", got)
	}

	if got := cleanErrorText(strings.Repeat("x", 500)); len(got) != 200 {
		t.Errorf("cleanErrorText length = %d, want it truncated to 200", len(got))
	}

	// A credential in an error body must never survive into a log line.
	long := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij"
	if got := cleanErrorText("failed for token " + long); strings.Contains(got, long) {
		t.Errorf("cleanErrorText leaked a credential: %q", got)
	}
	if cleanErrorText("") != "" {
		t.Error("an empty body must stay empty")
	}
}

func TestNewUpstreamErrorAndRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "90")
	h.Set("Retry-After-Ms", "10")

	ue := newUpstreamError(429, `{"code":14018,"message":"积分不足"}`, h)
	if ue.Status != 429 || ue.Kind != kindQuota {
		t.Errorf("upstream error = %+v", ue)
	}
	if !strings.Contains(ue.Message, "upstream 429") {
		t.Errorf("message = %q, want it to name the status", ue.Message)
	}
	if ue.RetryAfter != 90*time.Second {
		t.Errorf("RetryAfter = %s, want 90s (seconds win over milliseconds)", ue.RetryAfter)
	}
	if err := ue.Error(); err != "" && err == "upstream " {
		t.Errorf("Error() = %q", err)
	}

	var asErr error = ue
	if back, ok := asUpstreamError(asErr); !ok || back != ue {
		t.Error("asUpstreamError did not recover the error")
	}
	if _, ok := asUpstreamError(errors.New("plain")); ok {
		t.Error("asUpstreamError must not invent an upstream error")
	}

	tests := []struct {
		name string
		hdr  http.Header
		want time.Duration
	}{
		{"nil header", nil, 0},
		{"empty", http.Header{}, 0},
		{"seconds", http.Header{"Retry-After": []string{"30"}}, 30 * time.Second},
		{"milliseconds", http.Header{"Retry-After-Ms": []string{"1500"}}, 1500 * time.Millisecond},
		{"x-ratelimit-reset", http.Header{"X-Ratelimit-Reset": []string{"12"}}, 12 * time.Second},
		{"nonsense is ignored", http.Header{"Retry-After": []string{"soon"}}, 0},
		{"zero is ignored", http.Header{"Retry-After": []string{"0"}}, 0},
		{"a hostile value is capped", http.Header{"Retry-After": []string{"604800"}}, 2 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.hdr); got != tc.want {
				t.Errorf("parseRetryAfter = %s, want %s", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the account pool
// ---------------------------------------------------------------------------

func testCooldownConfig() cooldownConfig {
	return cooldownConfig{def: defaultCooldown, short: defaultShortCooldown, quota: defaultQuotaCooldown}
}

func newTestPool(t *testing.T, accts ...account) *pool {
	t.Helper()
	dir := t.TempDir()
	p := newPool(
		filepath.Join(dir, accountsFile),
		filepath.Join(dir, stateFile),
		defaultStoreFlush,
		testCooldownConfig(),
		func(string, ...any) {},
	)
	p.load(accts)
	return p
}

func TestAccountUsability(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		acct account
		want bool
	}{
		{"a plain token", account{AccessToken: "t"}, true},
		{"no token", account{}, false},
		{"disabled", account{AccessToken: "t", Disabled: true}, false},
		{"cooling", account{AccessToken: "t", CooldownUntil: now.Add(time.Hour).UnixMilli()}, false},
		{"a cooldown that has passed", account{AccessToken: "t", CooldownUntil: now.Add(-time.Hour).UnixMilli()}, true},
		{"an unknown expiry never expires", account{AccessToken: "t", ExpiresAt: 0}, true},
		{"a far-future expiry", account{AccessToken: "t", ExpiresAt: now.Add(time.Hour).Unix()}, true},
		{"already expired", account{AccessToken: "t", ExpiresAt: now.Add(-time.Hour).Unix()}, false},
		{"expiring within the refresh margin is unusable", account{AccessToken: "t", ExpiresAt: now.Add(30 * time.Second).Unix()}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acct.usable(now); got != tc.want {
				t.Errorf("usable = %v, want %v", got, tc.want)
			}
		})
	}

	refreshable := account{AccessToken: "t", RefreshToken: "r", ExpiresAt: now.Add(5 * time.Minute).Unix()}
	if !refreshable.needsRefresh(now, defaultRefreshMargin) {
		t.Error("a token inside the refresh margin needs a refresh")
	}
	if (account{AccessToken: "t", ExpiresAt: now.Add(5 * time.Minute).Unix()}).needsRefresh(now, defaultRefreshMargin) {
		t.Error("an account with no refresh token must never be refreshed")
	}
}

func TestAccountIdentity(t *testing.T) {
	if got := (account{ID: "abc"}).id(); got != "abc" {
		t.Errorf("id = %q, want the explicit id", got)
	}
	if got := (account{UID: "42"}).id(); got != "uid:42" {
		t.Errorf("id = %q, want uid:42", got)
	}
	long := account{AccessToken: "0123456789abcdefghij"}
	if got := long.id(); got != "tok:0123456789abcdef" {
		t.Errorf("id = %q, want the first 16 token characters", got)
	}
	if got := (account{AccessToken: "short"}).id(); got != "tok:short" {
		t.Errorf("id = %q, want the whole short token", got)
	}
	if got := (account{}).id(); got != "tok:" {
		t.Errorf("id = %q", got)
	}

	labels := []struct {
		acct account
		want string
	}{
		{account{Nickname: "nick", UID: "42"}, "nick (42)"},
		{account{Nickname: "nick"}, "nick"},
		{account{UID: "42"}, "42"},
		{account{Email: "u@example.com"}, "u@example.com"},
		{account{}, "account"},
	}
	for _, tc := range labels {
		if got := tc.acct.label(); got != tc.want {
			t.Errorf("label = %q, want %q", got, tc.want)
		}
	}
}

func TestPoolPicksLeastRecentlyUsed(t *testing.T) {
	p := newTestPool(t,
		account{UID: "1", AccessToken: "t1", LastUsed: 100},
		account{UID: "2", AccessToken: "t2", LastUsed: 50},
		account{UID: "3", AccessToken: "t3", LastUsed: 200},
	)
	if p.len() != 3 {
		t.Fatalf("pool holds %d accounts, want 3", p.len())
	}
	if !p.ready() {
		t.Fatal("a pool of fresh accounts must be ready")
	}

	e := p.pick(nil)
	if e == nil {
		t.Fatal("pick returned nil")
	}
	if e.acct.UID != "2" {
		t.Errorf("picked uid %s, want the least-recently-used 2", e.acct.UID)
	}

	// A successful call moves that account to the back of the queue.
	p.markUsed(e)
	if next := p.pick(nil); next == nil || next.acct.UID != "1" {
		t.Errorf("after markUsed the next pick should be uid 1, got %v", next)
	}

	// skip excludes an account for the rest of one request.
	skip := map[string]bool{"uid:1": true}
	if got := p.pick(skip); got == nil || got.acct.UID != "3" {
		t.Errorf("pick with skip = %v, want uid 3", got)
	}
	if got := p.pick(map[string]bool{"uid:1": true, "uid:2": true, "uid:3": true}); got != nil {
		t.Errorf("pick with everything skipped = %v, want nil", got)
	}
}

func TestPoolSkipsUnusableAccounts(t *testing.T) {
	now := time.Now()
	p := newTestPool(t,
		account{UID: "cooling", AccessToken: "t", CooldownUntil: now.Add(time.Hour).UnixMilli()},
		account{UID: "disabled", AccessToken: "t", Disabled: true},
		account{UID: "expired", AccessToken: "t", ExpiresAt: now.Add(-time.Hour).Unix()},
		account{UID: "fine", AccessToken: "t"},
	)
	e := p.pick(nil)
	if e == nil || e.acct.UID != "fine" {
		t.Fatalf("pick = %v, want the only usable account", e)
	}
	snap := p.snapshot()
	states := map[string]string{}
	for _, s := range snap {
		states[s.Extra["uid"].(string)] = s.State
	}
	want := map[string]string{
		"cooling":  stateCooling,
		"disabled": stateInvalid,
		"expired":  stateInvalid,
		"fine":     stateReady,
	}
	for uid, wantState := range want {
		if states[uid] != wantState {
			t.Errorf("account %s state = %q, want %q", uid, states[uid], wantState)
		}
	}
	if !strings.Contains(p.summary(), "ready") {
		t.Errorf("summary = %q, want it to count the ready account", p.summary())
	}
}

func TestPoolQuotaCooldownIsADay(t *testing.T) {
	base := time.Now()
	p := newTestPool(t, account{UID: "1", AccessToken: "t1"})
	p.now = func() time.Time { return base }

	e := p.pick(nil)
	if e == nil {
		t.Fatal("pick returned nil")
	}
	// The vendor's 14018 answer: 429 + a credits-exhausted body.
	ue := newUpstreamError(429, `{"code":14018,"message":"积分不足"}`, http.Header{})
	p.markFailureWith(e, ue.Kind, ue.Message, ue.RetryAfter)

	snap := p.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d accounts", len(snap))
	}
	if snap[0].State != stateExhausted {
		t.Errorf("state = %q, want %q", snap[0].State, stateExhausted)
	}
	if p.ready() {
		t.Error("an exhausted-only pool must not be ready")
	}
	if p.pick(nil) != nil {
		t.Error("an exhausted account must not be picked")
	}

	// 23 hours later it is still parked...
	p.now = func() time.Time { return base.Add(23 * time.Hour) }
	if p.ready() {
		t.Error("the account must still be cooling after 23h")
	}
	// ...and after a full day it is usable again.
	p.now = func() time.Time { return base.Add(24*time.Hour + time.Minute) }
	if !p.ready() {
		t.Error("a 24h quota cooldown must expire after a day")
	}
}

func TestPoolTransientCooldownAndRetryAfter(t *testing.T) {
	base := time.Now()
	p := newTestPool(t, account{UID: "1", AccessToken: "t1"}, account{UID: "2", AccessToken: "t2"})
	p.now = func() time.Time { return base }

	first := p.pick(nil)
	p.markFailure(first, kindTransient, "boom")
	if got := p.pick(nil); got == nil || got.acct.id() == first.acct.id() {
		t.Fatalf("a cooling account must be skipped, got %v", got)
	}

	// A Retry-After may only lengthen the cooldown, never shorten it.
	second := p.pick(nil)
	p.markFailureWith(second, kindTransient, "slow down", 10*time.Minute)
	until := map[string]time.Time{}
	for _, e := range p.entries {
		until[e.acct.id()] = e.until
	}
	if left := until[second.acct.id()].Sub(base); left != 10*time.Minute {
		t.Errorf("Retry-After cooldown = %s, want 10m", left)
	}
	if left := until[first.acct.id()].Sub(base); left != defaultShortCooldown {
		t.Errorf("plain transient cooldown = %s, want %s", left, defaultShortCooldown)
	}

	// A shorter Retry-After does not shorten the policy's own cooldown.
	third := newTestPool(t, account{UID: "3", AccessToken: "t3"})
	third.now = func() time.Time { return base }
	e := third.pick(nil)
	third.markFailureWith(e, kindTransient, "boom", time.Second)
	if left := third.entries[0].until.Sub(base); left != defaultShortCooldown {
		t.Errorf("a short Retry-After shortened the cooldown to %s", left)
	}
}

func TestPoolMarkDeadAndRevive(t *testing.T) {
	p := newTestPool(t, account{UID: "1", AccessToken: "t1"})
	e := p.pick(nil)
	if e == nil {
		t.Fatal("pick returned nil")
	}
	p.markDead(e, "refresh token rejected")
	if p.ready() {
		t.Error("a disabled account must not be ready")
	}
	snap := p.snapshot()
	if snap[0].State != stateInvalid || snap[0].Enabled {
		t.Errorf("snapshot = %+v, want an invalid, disabled account", snap[0])
	}
	if !strings.Contains(snap[0].Note, "refresh token rejected") {
		t.Errorf("note = %q, want the reason", snap[0].Note)
	}

	p.revive(e)
	if !p.ready() {
		t.Error("revive must make the account usable again")
	}
	if got := p.snapshot()[0]; got.State != stateReady || !got.Enabled {
		t.Errorf("after revive = %+v", got)
	}
}

func TestPoolPersistsAccountsAndHealth(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, accountsFile)
	statePath := filepath.Join(dir, stateFile)
	p := newPool(storePath, statePath, defaultStoreFlush, testCooldownConfig(), func(string, ...any) {})
	p.load([]account{{UID: "1", AccessToken: "tok-1", Nickname: "first"}})

	// The store is written once the pool has loaded configured credentials.
	raw, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("accounts.json was not written: %v", err)
	}
	if !strings.Contains(string(raw), "tok-1") {
		t.Errorf("accounts.json = %s", raw)
	}

	e := p.pick(nil)
	p.markDead(e, "dead")
	p.flushState(true)

	health, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state.json was not written: %v", err)
	}
	if !strings.Contains(string(health), "invalid") {
		t.Errorf("state.json = %s, want it to record the invalid state", health)
	}
	if strings.Contains(string(health), "tok-1") {
		t.Error("state.json must never contain a credential")
	}

	// A fresh pool over the same store keeps the health verdicts.
	p2 := newPool(storePath, statePath, defaultStoreFlush, testCooldownConfig(), func(string, ...any) {})
	p2.load([]account{{UID: "1", AccessToken: "tok-1"}})
	if p2.ready() {
		t.Error("the disabled verdict must survive a restart")
	}

	// An account that vanishes from the config is dropped, not resurrected.
	p2.load(nil)
	if p2.len() != 0 {
		t.Errorf("pool holds %d accounts after the config was emptied", p2.len())
	}
}

func TestPoolSummaryAndSnapshotExtra(t *testing.T) {
	if got := newTestPool(t).summary(); got != "no accounts" {
		t.Errorf("empty summary = %q", got)
	}
	p := newTestPool(t,
		account{UID: "1", AccessToken: "t1", Nickname: "n1", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()},
		account{UID: "2", AccessToken: "t2", Disabled: true},
	)
	summary := p.summary()
	if !strings.Contains(summary, "1 ready") || !strings.Contains(summary, "1 invalid") {
		t.Errorf("summary = %q, want it to count both states", summary)
	}
	snap := p.snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot has %d entries", len(snap))
	}
	if snap[0].ID != "uid:1" || snap[0].Label != "n1 (1)" {
		t.Errorf("snapshot[0] = %+v", snap[0])
	}
	if snap[0].ExpiresAt == "" {
		t.Error("a known expiry must be published as RFC 3339")
	}
	if _, ok := snap[0].Extra["uid"]; !ok {
		t.Error("the uid must be published in Extra")
	}
	for _, s := range snap {
		if s.Note != "" && strings.Contains(s.Note, "t1") {
			t.Errorf("a note leaked a credential: %q", s.Note)
		}
	}
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("a nil config must not fail: %v", err)
	}
	if cfg.baseURL() != defaultBaseURL || cfg.userAgent() != defaultUserAgent {
		t.Errorf("defaults = %q/%q", cfg.baseURL(), cfg.userAgent())
	}
	if cfg.maxAttempts() != defaultMaxAttempts || cfg.maxTokens() != defaultMaxTokens {
		t.Errorf("defaults = %d attempts / %d tokens", cfg.maxAttempts(), cfg.maxTokens())
	}
	if cfg.chatTimeout() != defaultChatTimeout || cfg.modelsTTL() != defaultModelsTTL {
		t.Errorf("defaults = %s / %s", cfg.chatTimeout(), cfg.modelsTTL())
	}
	if cfg.quotaCooldown() != defaultQuotaCooldown || cfg.cooldown() != defaultCooldown || cfg.shortCooldown() != defaultShortCooldown {
		t.Error("the cooldown defaults are wrong")
	}
	if cfg.loginRequested() {
		t.Error("the device flow must not start without being asked for")
	}

	raw := json.RawMessage(`{
		"base_url": "https://example.test/",
		"user_agent": "ua/1",
		"max_attempts": 7,
		"models_ttl": "90s",
		"cooldown": 300,
		"quota_cooldown": "2h",
		"max_tokens": 1234,
		"login": true
	}`)
	cfg, err = parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.baseURL() != "https://example.test/" || cfg.userAgent() != "ua/1" {
		t.Errorf("override = %q/%q", cfg.baseURL(), cfg.userAgent())
	}
	if cfg.maxAttempts() != 7 || cfg.modelsTTL() != 90*time.Second || cfg.maxTokens() != 1234 {
		t.Errorf("override = %d / %s / %d", cfg.maxAttempts(), cfg.modelsTTL(), cfg.maxTokens())
	}
	if cfg.cooldown() != 300*time.Second {
		t.Errorf("a bare number must be read as seconds, got %s", cfg.cooldown())
	}
	if cfg.quotaCooldown() != 2*time.Hour {
		t.Errorf("quota cooldown = %s", cfg.quotaCooldown())
	}
	if !cfg.loginRequested() {
		t.Error("login: true must request the device flow")
	}

	if _, err := parseConfig(json.RawMessage(`{"max_attempts":`)); err == nil {
		t.Error("malformed JSON must be reported so New can log it")
	}
}

func TestDurationOrAndTruthyAndParseExpiry(t *testing.T) {
	durations := []struct {
		in   string
		def  time.Duration
		want time.Duration
	}{
		{"", time.Minute, time.Minute},
		{"30s", time.Minute, 30 * time.Second},
		{"2m", time.Minute, 2 * time.Minute},
		{"300", time.Minute, 300 * time.Second},
		{"0", time.Minute, time.Minute},
		{"-5s", time.Minute, time.Minute},
		{"nonsense", time.Minute, time.Minute},
	}
	for _, tc := range durations {
		if got := durationOr(tc.in, tc.def); got != tc.want {
			t.Errorf("durationOr(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	for in, want := range map[string]bool{"1": true, "true": true, "YES": true, "on": true, "y": true, "": false, "0": false, "no": false, "maybe": false} {
		if got := truthy(in); got != want {
			t.Errorf("truthy(%q) = %v, want %v", in, got, want)
		}
	}

	now := time.Now()
	expiries := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"nonsense", 0},
		{"1790601000", 1790601000},
		{"1790601000000", 1790601000},
		{now.Format(time.RFC3339), now.Unix()},
		{now.UTC().Format("2006-01-02 15:04:05"), now.UTC().Unix()},
	}
	for _, tc := range expiries {
		if got := parseExpiry(tc.in); got != tc.want {
			t.Errorf("parseExpiry(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestConfiguredAccounts(t *testing.T) {
	clearCredentialEnv(t)

	cfg := config{
		Accounts: []accountConfig{
			{Label: "labelled", UID: "1", AccessToken: " tok-1 ", ExpiresAt: "1790601000"},
			{UID: "2"}, // dropped: no token
		},
	}
	got := configuredAccounts(cfg)
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].UID != "1" || got[0].AccessToken != "tok-1" || got[0].Note != "labelled" {
		t.Errorf("account = %+v, want trimmed fields and the label as the note", got[0])
	}
	if got[0].ExpiresAt != 1790601000 {
		t.Errorf("ExpiresAt = %d", got[0].ExpiresAt)
	}

	// The single-account shorthand and the environment are additive.
	cfg.AccessToken = "tok-single"
	t.Setenv(envAccessToken, "tok-env")
	t.Setenv(envUID, "9")
	got = configuredAccounts(cfg)
	if len(got) != 3 {
		t.Fatalf("got %d accounts, want the list entry, the shorthand and the environment", len(got))
	}
	if got[1].AccessToken != "tok-single" || got[2].AccessToken != "tok-env" || got[2].UID != "9" {
		t.Errorf("accounts = %+v", got)
	}
}

// ---------------------------------------------------------------------------
// the client
// ---------------------------------------------------------------------------

// clearCredentialEnv makes a test independent of the ambient environment.
func clearCredentialEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{envAccessToken, envRefreshToken, envUID, envNickname, envEmail, envLogin} {
		t.Setenv(k, "")
	}
}

func TestNewWithoutCredential(t *testing.T) {
	clearCredentialEnv(t)

	deps := core.Deps{DataDir: t.TempDir(), Logf: func(string, ...any) {}}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New must not fail without a credential: %v", err)
	}
	if c.Name() != "qwenwork" {
		t.Errorf("Name = %q, want qwenwork", c.Name())
	}

	ctx := context.Background()
	st := c.Status(ctx)
	if st.Ready {
		t.Error("Ready must be false without a credential")
	}
	if st.Name != "qwenwork" || st.UpdatedAt.IsZero() {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(st.Detail, "no credential") {
		t.Errorf("Detail = %q, want it to explain the missing credential", st.Detail)
	}
	if len(st.Models) != len(builtinModels) {
		t.Errorf("Models = %v, want the builtin catalogue", st.Models)
	}

	// Chat reports the runtime condition; Models still answers offline.
	if _, err := c.Chat(ctx, &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, core.ErrNotConfigured) {
		t.Errorf("Chat = %v, want core.ErrNotConfigured", err)
	}
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models must never fail: %v", err)
	}
	if len(models) != len(builtinModels) {
		t.Fatalf("Models = %d entries, want %d", len(models), len(builtinModels))
	}
	for _, m := range models {
		if m.OwnedBy != "qwenwork" {
			t.Errorf("model %s is owned by %q", m.ID, m.OwnedBy)
		}
	}

	// The catalogue is cheap: a second call must not block on anything.
	done := make(chan struct{})
	go func() {
		_, _ = c.Models(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Models blocked")
	}
}

func TestNewRejectsNothingAndDegradesOnBadConfig(t *testing.T) {
	clearCredentialEnv(t)

	logged := make(chan string, 4)
	deps := core.Deps{
		DataDir: t.TempDir(),
		Config:  json.RawMessage(`{"max_attempts":`),
		Logf:    func(format string, args ...any) { logged <- format },
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("a broken config must not stop construction: %v", err)
	}
	select {
	case msg := <-logged:
		if !strings.Contains(msg, "invalid config") {
			t.Errorf("log = %q, want it to mention the invalid config", msg)
		}
	case <-time.After(time.Second):
		t.Error("a broken config must be logged")
	}
	if c.Status(context.Background()).Ready {
		t.Error("a client built from a broken config cannot be ready")
	}
}

func TestChatRejectsUnsupportedRequests(t *testing.T) {
	clearCredentialEnv(t)

	dir := t.TempDir()
	if err := core.WriteJSONAtomic(filepath.Join(dir, accountsFile), []account{{UID: "1", AccessToken: "tok-1"}}); err != nil {
		t.Fatalf("seeding the account store: %v", err)
	}
	deps := core.Deps{DataDir: dir, Logf: func(string, ...any) {}}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	if _, err := c.Chat(ctx, nil); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("a nil request = %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(ctx, &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("an empty model = %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(ctx, &core.ChatRequest{Model: "   "}); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("a blank model = %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(ctx, &core.ChatRequest{
		Model:    "pro",
		Messages: []core.Message{{Role: "user", Content: "look", Parts: []core.ContentPart{{Type: "image_url", ImageURL: "https://x/y.png"}}}},
	}); !errors.Is(err, errImageUnsupported) {
		t.Errorf("an image request = %v, want errImageUnsupported", err)
	}

	// A nil context is tolerated rather than panicking.
	ctx2, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Chat(ctx2, &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Error("a cancelled context must surface as an error, not a stream")
	}
}

// ---------------------------------------------------------------------------
// the transport: retry, cooldown and the stream, end to end
// ---------------------------------------------------------------------------

type recordedRequest struct {
	method string
	url    string
	header http.Header
	body   string
}

// fakeTransport is a RoundTripper that records what the client sent and answers
// from a script.  It is what keeps these tests offline.
type fakeTransport struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(n int, req *http.Request, body string) (*http.Response, error)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		body = string(raw)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{
		method: req.Method,
		url:    req.URL.String(),
		header: req.Header.Clone(),
		body:   body,
	})
	n := len(f.requests)
	handler := f.handler
	f.mu.Unlock()

	if handler == nil {
		return nil, errors.New("fakeTransport: no handler")
	}
	return handler(n, req, body)
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeTransport) at(i int) recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func fakeResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestChatRetriesOnATransientFailureAndStreams(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		if n == 1 {
			return fakeResponse(req, http.StatusInternalServerError, `{"code":500,"message":"boom"}`), nil
		}
		return fakeResponse(req, http.StatusOK, chatSSEFrames), nil
	}

	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"accounts":[{"uid":"1","nickname":"first","access_token":"tok-first"},{"uid":"2","nickname":"second","access_token":"tok-second"}]}`),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc, ok := c.(*Client)
	if !ok {
		t.Fatal("New did not return a *Client")
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "pro",
		Messages: []core.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if rt.count() != 2 {
		t.Fatalf("made %d requests, want a retry on the second account", rt.count())
	}

	first, second := rt.at(0), rt.at(1)
	if first.method != http.MethodPost {
		t.Errorf("method = %s, want POST", first.method)
	}
	if !strings.HasSuffix(first.url, chatPath) {
		t.Errorf("url = %s, want it to end with %s", first.url, chatPath)
	}
	if auth := first.header.Get("authorization"); !strings.HasPrefix(auth, "Bearer COSY.") {
		t.Errorf("authorization = %q, want a COSY bearer token", auth)
	}
	if first.header.Get("cosy-user") != "1" {
		t.Errorf("the first attempt used cosy-user %q, want 1", first.header.Get("cosy-user"))
	}
	if second.header.Get("cosy-user") != "2" {
		t.Errorf("the retry used cosy-user %q, want the second account", second.header.Get("cosy-user"))
	}
	if first.header.Get("authorization") == second.header.Get("authorization") {
		t.Error("each account must sign with its own session")
	}
	if !strings.Contains(first.body, `"chat_task":"FREE_INPUT"`) {
		t.Errorf("the body is not the agent envelope: %s", first.body)
	}
	if !strings.Contains(first.body, `"system":"be brief"`) {
		t.Errorf("the system prompt was not hoisted out: %s", first.body)
	}
	if first.header.Get("x-model-key") != "pro" {
		t.Errorf("x-model-key = %q, want pro", first.header.Get("x-model-key"))
	}

	// The failing account is cooling; the healthy one is ready.
	states := map[string]string{}
	for _, s := range qc.pool.snapshot() {
		states[s.Extra["uid"].(string)] = s.State
	}
	if states["1"] != stateCooling {
		t.Errorf("account 1 state = %q, want %q", states["1"], stateCooling)
	}
	if states["2"] != stateReady {
		t.Errorf("account 2 state = %q, want %q", states["2"], stateReady)
	}

	st := c.Status(context.Background())
	if !st.Ready {
		t.Errorf("Status = %+v, want ready after a successful call", st)
	}
	if !strings.Contains(st.Detail, "ready") {
		t.Errorf("Detail = %q", st.Detail)
	}

	events := drain(t, stream)
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Delta != "Hel" || events[1].Delta != "lo" {
		t.Errorf("deltas = %q/%q", events[0].Delta, events[1].Delta)
	}
	if events[2].Type != core.EventUsage || events[2].Usage.TotalTokens != 5 {
		t.Errorf("usage event = %+v", events[2])
	}
	if events[3].Type != core.EventDone || events[3].Finish != "stop" {
		t.Errorf("done event = %+v", events[3])
	}
}

func TestChatStopsOnANonRetryableFailure(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return fakeResponse(req, http.StatusBadRequest, `{"message":"model does not exist"}`), nil
	}
	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"accounts":[{"uid":"1","access_token":"t1"},{"uid":"2","access_token":"t2"},{"uid":"3","access_token":"t3"}]}`),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.Chat(context.Background(), &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a 400 must fail the request")
	}
	if !strings.Contains(err.Error(), "model does not exist") {
		t.Errorf("error = %v, want the upstream message", err)
	}
	if rt.count() != 1 {
		t.Errorf("made %d requests, want 1: a bad request is bad on every account", rt.count())
	}
}

func TestChatExhaustsThePoolAndReportsNotConfigured(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return fakeResponse(req, http.StatusTooManyRequests, `{"code":14018,"message":"credits exhausted"}`), nil
	}
	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"accounts":[{"uid":"1","access_token":"t1"},{"uid":"2","access_token":"t2"}]}`),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc := c.(*Client)

	_, err = c.Chat(context.Background(), &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a quota exhaustion must fail the request")
	}
	if rt.count() != 2 {
		t.Errorf("made %d requests, want one per account", rt.count())
	}
	for _, s := range qc.pool.snapshot() {
		if s.State != stateExhausted {
			t.Errorf("account %s state = %q, want %q", s.ID, s.State, stateExhausted)
		}
	}

	// With every account parked for a day, the next call cannot start.
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, core.ErrNotConfigured) {
		t.Errorf("the second call = %v, want core.ErrNotConfigured", err)
	}
	st := c.Status(context.Background())
	if st.Ready {
		t.Error("Status must not claim readiness while every account is exhausted")
	}
	if !strings.Contains(st.Detail, "unavailable") {
		t.Errorf("Detail = %q, want it to explain the pool state", st.Detail)
	}
}

func TestChatMarksAnAccountDeadWhenTheRefreshFails(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "deviceToken") {
			return fakeResponse(req, http.StatusUnauthorized, `{"message":"refresh token rejected"}`), nil
		}
		return fakeResponse(req, http.StatusUnauthorized, `{"message":"token expired"}`), nil
	}
	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"accounts":[{"uid":"1","access_token":"t1","refresh_token":"r1"}]}`),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc := c.(*Client)

	_, err = c.Chat(context.Background(), &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("a 401 must fail the request")
	}
	snap := qc.pool.snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].State != stateInvalid || snap[0].Enabled {
		t.Errorf("account = %+v, want it disabled after a failed refresh", snap[0])
	}
	if !strings.Contains(snap[0].Note, "refresh token rejected") {
		t.Errorf("note = %q, want the refresh failure", snap[0].Note)
	}
	if rt.count() < 2 {
		t.Errorf("made %d requests, want the original call plus the refresh attempt", rt.count())
	}
}

func TestStatusReportsAnUnreachableUpstream(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(n int, req *http.Request, body string) (*http.Response, error) {
		return nil, errors.New("dial tcp: connection refused")
	}
	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"accounts":[{"uid":"1","access_token":"t1"}]}`),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "pro", Messages: []core.Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("a dead transport must fail the request")
	}

	// The account itself is fine, so it is only cooling; Status must say so
	// without making a network call of its own.
	done := make(chan core.Status, 1)
	go func() { done <- c.Status(context.Background()) }()
	select {
	case st := <-done:
		if st.Ready {
			t.Errorf("Status = %+v, want not ready while the account cools", st)
		}
		if !strings.Contains(st.Detail, "unavailable") && !strings.Contains(st.Detail, "unreachable") {
			t.Errorf("Detail = %q", st.Detail)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Status blocked")
	}
}

func TestStatusReportsAnIncompleteDeviceFlow(t *testing.T) {
	clearCredentialEnv(t)

	dir := t.TempDir()
	rec := loginRecord{
		URL:       "https://gateway.qwenwork.cn/device/selectAccounts?challenge=x",
		Nonce:     "n",
		Verifier:  "v",
		StartedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}
	if err := core.WriteJSONAtomic(filepath.Join(dir, loginFile), rec); err != nil {
		t.Fatalf("seeding %s: %v", loginFile, err)
	}

	c, err := New(core.Deps{DataDir: dir, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	st := c.Status(context.Background())
	if st.Ready {
		t.Error("an incomplete device flow cannot be ready")
	}
	if !strings.Contains(st.Detail, "device flow") {
		t.Errorf("Detail = %q, want it to point at the pending authorisation", st.Detail)
	}
	if !strings.Contains(st.Detail, "https://gateway.qwenwork.cn") {
		t.Errorf("Detail = %q, want the URL a human has to open", st.Detail)
	}
}

func TestSessionCacheReuseAndInvalidation(t *testing.T) {
	clearCredentialEnv(t)

	c, err := New(core.Deps{DataDir: t.TempDir(), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc := c.(*Client)
	acct := account{UID: "1", AccessToken: "tok-1"}

	first, err := qc.sessionFor(acct)
	if err != nil {
		t.Fatalf("sessionFor: %v", err)
	}
	again, err := qc.sessionFor(acct)
	if err != nil {
		t.Fatalf("sessionFor: %v", err)
	}
	if first.info != again.info || first.cosyKey != again.cosyKey {
		t.Error("the session must be reused for the same token")
	}

	// A new token for the same account must not reuse the old session.
	rotated := acct
	rotated.AccessToken = "tok-2"
	third, err := qc.sessionFor(rotated)
	if err != nil {
		t.Fatalf("sessionFor: %v", err)
	}
	if third.info == first.info {
		t.Error("a rotated token must get a fresh session")
	}

	qc.invalidateSession(rotated)
	if _, ok := qc.sessions[rotated.id()]; ok {
		t.Error("invalidateSession must drop the cached session")
	}
	if _, err := qc.sessionFor(rotated); err != nil {
		t.Fatalf("sessionFor after invalidation: %v", err)
	}
}

// TestQwenworkChatNamesTheServedAccount pins the gateway-facing attribution.
// The credential that served a turn is known only inside Chat, and before this
// slot existed every success was filed under "(unrouted)".  account.id() falls
// back to "uid:"+UID for a hand-written config entry, so the slot must carry
// uid:1 for the single account configured here.
func TestQwenworkChatNamesTheServedAccount(t *testing.T) {
	clearCredentialEnv(t)

	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusOK, chatSSEFrames), nil
	}
	c := panelClient(t, `{"accounts":[{"uid":"1","nickname":"first","access_token":"tok-1"}]}`, rt)

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "pro",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream.Close()

	if served != "uid:1" {
		t.Errorf("ServedBy = %q, want %q", served, "uid:1")
	}
}
