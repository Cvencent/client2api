package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"regexp"
	"strings"
	"testing"
	"time"
)

// crypto_test.go checks the hand-written cryptography against published
// vectors and against independent implementations, rather than against itself.
//
// The DPoP proof in particular is verified by pulling the public key back out
// of the JWS header and running ecdsa.Verify over the signing input, which is
// what the STS endpoint does.  A test that only checked the proof's shape would
// pass for a proof signed over the wrong bytes.

// TestPKCEChallengeRFC7636Vector uses the worked example from RFC 7636
// appendix B.  It is the only way to be sure the S256 transform is the one the
// specification names, and not, say, base64 of the raw digest without the
// unpadded encoding.
func TestPKCEChallengeRFC7636Vector(t *testing.T) {
	const (
		verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
		challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	)
	if got := pkceChallengeS256(verifier); got != challenge {
		t.Errorf("pkceChallengeS256 = %q, want %q", got, challenge)
	}
}

// TestNewPKCEPair checks the pair is internally consistent, long enough for
// RFC 7636, and different every time.
func TestNewPKCEPair(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		p, err := newPKCEPair()
		if err != nil {
			t.Fatalf("newPKCEPair: %v", err)
		}
		if n := len(p.Verifier); n < 43 || n > 128 {
			t.Fatalf("verifier length %d is outside RFC 7636's 43..128 window", n)
		}
		if got := pkceChallengeS256(p.Verifier); got != p.Challenge {
			t.Fatalf("challenge %q does not match the verifier", p.Challenge)
		}
		if seen[p.Verifier] {
			t.Fatal("newPKCEPair returned a repeated verifier")
		}
		seen[p.Verifier] = true
	}
}

// TestB64URLDecodeAcceptsPadding checks that a value copied out of a web tool
// with padding still decodes, because that is a realistic operator paste.
func TestB64URLDecodeAcceptsPadding(t *testing.T) {
	raw := []byte("the quick brown fox")
	padded := base64.URLEncoding.EncodeToString(raw)
	bare := base64.RawURLEncoding.EncodeToString(raw)

	for _, in := range []string{padded, bare, "  " + bare + "  "} {
		got, err := b64urlDecode(in)
		if err != nil {
			t.Fatalf("b64urlDecode(%q): %v", in, err)
		}
		if string(got) != string(raw) {
			t.Errorf("b64urlDecode(%q) = %q, want %q", in, got, raw)
		}
	}
	if _, err := b64urlDecode(""); err == nil {
		t.Error("an empty value decoded without error")
	}
	if _, err := b64urlDecode("!!not base64!!"); err == nil {
		t.Error("garbage decoded without error")
	}
}

// TestB64URLIsUnpadded checks the encoding the JWK and JWS require.
func TestB64URLIsUnpadded(t *testing.T) {
	// 1 byte encodes to 2 characters with no padding.
	if got := b64url([]byte{0xff}); got != "_w" {
		t.Errorf("b64url = %q, want _w", got)
	}
	if strings.Contains(b64url([]byte("a")), "=") {
		t.Error("b64url produced padding")
	}
}

// TestPadTo32 checks the left-padding that keeps a coordinate with a leading
// zero byte from encoding short.
func TestPadTo32(t *testing.T) {
	one := padTo32(big.NewInt(1))
	if len(one) != 32 {
		t.Fatalf("len = %d, want 32", len(one))
	}
	if one[31] != 1 || one[0] != 0 {
		t.Errorf("padTo32(1) = %x, want 31 zero bytes then 01", one)
	}
	if got := padTo32(nil); len(got) != 32 {
		t.Errorf("padTo32(nil) has length %d, want 32", len(got))
	}
	big33 := make([]byte, 33)
	big33[0] = 0xaa
	big33[32] = 0xbb
	if got := padTo32(new(big.Int).SetBytes(big33)); len(got) != 32 || got[0] != 0x00 || got[31] != 0xbb {
		t.Errorf("padTo32 truncated from the wrong end: %x", got)
	}
}

// TestDpopKeyPairRoundTrip checks that the private JWK persisted in
// accounts.json rebuilds into the same public key and the same thumbprint.
// The credential is unusable for a refresh if this round-trip loses anything.
func TestDpopKeyPairRoundTrip(t *testing.T) {
	original, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	restored, err := keyPairFromStoredJwk(original.PrivateJwk)
	if err != nil {
		t.Fatalf("keyPairFromStoredJwk: %v", err)
	}
	if restored.PublicJwk != original.PublicJwk {
		t.Errorf("public JWK changed across the round-trip\n got: %s\nwant: %s", restored.PublicJwk, original.PublicJwk)
	}
	if restored.thumbprint() != original.thumbprint() {
		t.Errorf("thumbprint changed across the round-trip")
	}
	if restored.Private.D.Cmp(original.Private.D) != 0 {
		t.Errorf("the private scalar changed across the round-trip")
	}
	if !restored.Private.PublicKey.Equal(&original.Private.PublicKey) {
		t.Errorf("the public key changed across the round-trip")
	}
}

// TestDpopPrivateJwkShape checks the persisted JWK has the members the
// reference implementation writes, with 32-byte coordinates.
func TestDpopPrivateJwkShape(t *testing.T) {
	k, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	var jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		D   string `json:"d"`
	}
	if err := json.Unmarshal([]byte(k.PrivateJwk), &jwk); err != nil {
		t.Fatalf("the private JWK is not JSON: %v", err)
	}
	if jwk.Kty != "EC" || jwk.Crv != "P-256" {
		t.Errorf("kty/crv = %s/%s, want EC/P-256", jwk.Kty, jwk.Crv)
	}
	for name, v := range map[string]string{"x": jwk.X, "y": jwk.Y, "d": jwk.D} {
		raw, err := b64urlDecode(v)
		if err != nil {
			t.Fatalf("%s does not decode: %v", name, err)
		}
		if len(raw) != 32 {
			t.Errorf("%s is %d bytes, want 32", name, len(raw))
		}
	}

	// The public JWK must be the private one without `d`, and must not leak it.
	if strings.Contains(k.PublicJwk, jwk.D) {
		t.Error("the public JWK carries the private scalar")
	}
	if strings.Contains(k.PublicJwk, `"d"`) {
		t.Error("the public JWK has a d member")
	}
}

// TestDpopThumbprintMatchesRFC7638 computes the thumbprint independently: the
// canonical JSON of exactly crv, kty, x and y, in lexicographic order, hashed
// with SHA-256 and base64url-encoded.
func TestDpopThumbprintMatchesRFC7638(t *testing.T) {
	k, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	x, _ := k.publicJwk["x"].(string)
	y, _ := k.publicJwk["y"].(string)
	canonical := `{"crv":"P-256","kty":"EC","x":"` + x + `","y":"` + y + `"}`
	sum := sha256.Sum256([]byte(canonical))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got := k.thumbprint(); got != want {
		t.Errorf("thumbprint = %q, want %q", got, want)
	}
}

// TestSignDpopProofIsAVerifiableJWS is the core check on the DPoP proof: the
// signature must verify against the key named in the proof's own header, over
// the exact `header.payload` signing input.  It also asserts the signature is
// the raw R||S pair JWS requires, not the ASN.1 DER form — mixing those up is
// what produces an opaque InvalidDPoPHeader from the server.
func TestSignDpopProofIsAVerifiableJWS(t *testing.T) {
	k, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	const (
		htm = "POST"
		htu = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	)
	at := time.Unix(1_800_000_000, 0)
	proof, err := k.signDpopProof(htm, htu, at)
	if err != nil {
		t.Fatalf("signDpopProof: %v", err)
	}

	parts := strings.Split(proof.Compact, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWS has three segments, got %d", len(parts))
	}

	headerJSON, err := b64urlDecode(parts[0])
	if err != nil {
		t.Fatalf("decoding the protected header: %v", err)
	}
	var header struct {
		Alg string         `json:"alg"`
		Typ string         `json:"typ"`
		Jwk map[string]any `json:"jwk"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatalf("the protected header is not JSON: %v", err)
	}
	if header.Alg != "ES256" {
		t.Errorf("alg = %q, want ES256", header.Alg)
	}
	if header.Typ != "dpop+jwt" {
		t.Errorf("typ = %q, want dpop+jwt", header.Typ)
	}
	if header.Jwk == nil {
		t.Fatal("the protected header carries no jwk")
	}
	if _, ok := header.Jwk["d"]; ok {
		t.Error("the protected header leaked the private key")
	}

	payloadJSON, err := b64urlDecode(parts[1])
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	var payload struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		t.Fatalf("the payload is not JSON: %v", err)
	}
	if payload.Htm != htm || payload.Htu != htu {
		t.Errorf("htm/htu = %q/%q, want %q/%q", payload.Htm, payload.Htu, htm, htu)
	}
	if payload.Iat != at.Unix() {
		t.Errorf("iat = %d, want %d", payload.Iat, at.Unix())
	}
	if len(payload.Jti) != 64 {
		t.Errorf("jti = %q, want 32 random bytes as hex", payload.Jti)
	}

	sig, err := b64urlDecode(parts[2])
	if err != nil {
		t.Fatalf("decoding the signature: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("the signature is %d bytes; JWS wants the raw 64-byte R||S pair, not DER", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&k.Private.PublicKey, digest[:], r, s) {
		t.Fatal("the DPoP signature does not verify against the key in its own header")
	}
}

// TestSignDpopProofBindsMethodAndURL checks that a proof cannot be replayed
// against a different request: changing either the method or the URL must
// change the signature.
func TestSignDpopProofBindsMethodAndURL(t *testing.T) {
	k, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	at := time.Unix(1_800_000_000, 0)
	a, err := k.signDpopProof("POST", "https://sts.example.com/v1/oauth2/tokens", at)
	if err != nil {
		t.Fatalf("signDpopProof: %v", err)
	}
	b, err := k.signDpopProof("POST", "https://sts.example.com/v1/oauth2/other", at)
	if err != nil {
		t.Fatalf("signDpopProof: %v", err)
	}
	c, err := k.signDpopProof("GET", "https://sts.example.com/v1/oauth2/tokens", at)
	if err != nil {
		t.Fatalf("signDpopProof: %v", err)
	}
	if a.Compact == b.Compact {
		t.Error("the URL is not covered by the proof")
	}
	if a.Compact == c.Compact {
		t.Error("the method is not covered by the proof")
	}
	// The jti is random, so even two proofs for the same request differ.
	d, err := k.signDpopProof("POST", "https://sts.example.com/v1/oauth2/tokens", at)
	if err != nil {
		t.Fatalf("signDpopProof: %v", err)
	}
	if a.Compact == d.Compact {
		t.Error("the jti is not random: two identical proofs were produced")
	}
	if a.Jkt != d.Jkt {
		t.Error("the thumbprint of one key changed between proofs")
	}
}

// TestSignDpopProofHasNoKey checks the failure mode that must be an error
// rather than a panic.
func TestSignDpopProofHasNoKey(t *testing.T) {
	var k *dpopKeyPair
	if _, err := k.signDpopProof("POST", "https://example.com", time.Now()); err == nil {
		t.Error("a nil key pair signed a proof")
	}
	if _, err := (&dpopKeyPair{}).signDpopProof("POST", "https://example.com", time.Now()); err == nil {
		t.Error("a key pair with no private key signed a proof")
	}
}

// TestKeyPairFromStoredJwkRejectsBadInput checks every way a stored key can be
// wrong, because a credential whose key is unusable must be reported rather
// than produce an unexplainable signature failure later.
func TestKeyPairFromStoredJwkRejectsBadInput(t *testing.T) {
	good, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	var full map[string]any
	if err := json.Unmarshal([]byte(good.PrivateJwk), &full); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	with := func(mutate func(map[string]any)) string {
		clone := map[string]any{}
		for k, v := range full {
			clone[k] = v
		}
		mutate(clone)
		b, _ := json.Marshal(clone)
		return string(b)
	}

	for name, jwk := range map[string]string{
		"empty":          "",
		"not json":       "{",
		"wrong kty":      with(func(m map[string]any) { m["kty"] = "RSA" }),
		"wrong curve":    with(func(m map[string]any) { m["crv"] = "P-384" }),
		"no d":           with(func(m map[string]any) { delete(m, "d") }),
		"garbage x":      with(func(m map[string]any) { m["x"] = "!!!" }),
		"x not on curve": with(func(m map[string]any) { m["x"] = b64url(make([]byte, 32)) }),
		// x/y valid points but belonging to a different scalar than d.
		"x/y disagree with d": with(func(m map[string]any) {
			other, err := generateDpopKeyPair()
			if err != nil {
				t.Fatalf("generateDpopKeyPair: %v", err)
			}
			var o map[string]any
			if err := json.Unmarshal([]byte(other.PrivateJwk), &o); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			m["x"] = o["x"]
			m["y"] = o["y"]
		}),
	} {
		if _, err := keyPairFromStoredJwk(jwk); err == nil {
			t.Errorf("keyPairFromStoredJwk accepted a %s key", name)
		}
	}

	// And the good one still works.
	if _, err := keyPairFromStoredJwk(good.PrivateJwk); err != nil {
		t.Errorf("keyPairFromStoredJwk rejected a valid key: %v", err)
	}
}

// TestGenerateDpopKeyPairIsUnique checks two key pairs never collide, which is
// what makes the thumbprint a usable account identifier.
func TestGenerateDpopKeyPairIsUnique(t *testing.T) {
	a, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	b, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	if a.thumbprint() == b.thumbprint() {
		t.Error("two generated key pairs share a thumbprint")
	}
	if a.Private.D.Cmp(b.Private.D) == 0 {
		t.Error("two generated key pairs share a private scalar")
	}
}

// TestNewDpopKeyPairRejectsWrongCurve checks that a non-P-256 key is refused
// rather than signed with, since the STS endpoint accepts only ES256.
func TestNewDpopKeyPairRejectsWrongCurve(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a P-384 key for the test: %v", err)
	}
	if _, err := newDpopKeyPair(priv); err == nil {
		t.Error("a P-384 key was accepted")
	}
	if _, err := newDpopKeyPair(nil); err == nil {
		t.Error("a nil key was accepted")
	}
}

// TestNewUUIDShape checks the identifier the chat API wants is a version-4
// UUID with the right variant bits.
func TestNewUUIDShape(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := newUUID()
		if !re.MatchString(id) {
			t.Fatalf("newUUID = %q, which is not a version-4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("newUUID repeated %q", id)
		}
		seen[id] = true
	}
}

// TestRandomHexLength checks the jti helper produces exactly the requested
// number of bytes.
func TestRandomHexLength(t *testing.T) {
	for _, n := range []int{1, 8, 32} {
		got := randomHex(n)
		if len(got) != n*2 {
			t.Errorf("randomHex(%d) has length %d, want %d", n, len(got), n*2)
		}
	}
	if randomHex(32) == randomHex(32) {
		t.Error("randomHex repeated")
	}
}
