package codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// crypto.go hand-writes the three pieces of cryptography CodeArts needs.
//
// Nothing here uses a third-party library: the module may only depend on the
// standard library plus the two packages the repository already requires, so
// the ES256 JWS that backs the DPoP proof, the PKCE pair, and the JWK
// round-trip are built out of crypto/ecdsa, crypto/sha256, encoding/base64 and
// encoding/json.
//
// The three pieces, and where each one is used:
//
//	PKCE (S256)      the browser login flow: proves the process that redeems
//	                 the authorisation code is the one that started it.
//	DPoP proof       every call to the STS token endpoint: a short-lived ES256
//	                 JWS whose protected header carries the public key, which
//	                 binds the issued credential to this key pair.
//	JWK round-trip   the private key is persisted in accounts.json, so it has
//	                 to survive a JSON write/read with its curve parameters
//	                 intact.

// ---------------------------------------------------------------------------
// base64url
// ---------------------------------------------------------------------------

// b64url encodes without padding, which is what JWS, JWK and PKCE all require.
// encoding/base64's URLEncoding has the padding built in, so it is stripped
// rather than configured.
func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// b64urlDecode accepts both padded and unpadded input: an operator pasting a
// value out of a web tool may well have padding on it.
func b64urlDecode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty base64url value")
	}
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// ---------------------------------------------------------------------------
// PKCE
// ---------------------------------------------------------------------------

// pkcePair is a code_verifier and the S256 challenge derived from it.
type pkcePair struct {
	Verifier  string
	Challenge string
}

// newPKCEPair mints a verifier and its S256 challenge.
//
// The verifier is 48 random bytes base64url-encoded, which is the shape the
// reference implementation uses (and comfortably inside RFC 7636's 43..128
// character window).  The challenge is SHA-256(verifier) base64url-encoded.
func newPKCEPair() (pkcePair, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return pkcePair{}, fmt.Errorf("codearts: generating a PKCE verifier: %w", err)
	}
	v := b64url(buf)
	return pkcePair{Verifier: v, Challenge: pkceChallengeS256(v)}, nil
}

// pkceChallengeS256 is the S256 transform on its own, so the derivation can be
// tested against a known vector without generating a random verifier.
func pkceChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64url(sum[:])
}

// ---------------------------------------------------------------------------
// ES256 keys and JWK
// ---------------------------------------------------------------------------

// dpopKeyPair is a P-256 key pair plus the JWK forms of both halves.
type dpopKeyPair struct {
	Private *ecdsa.PrivateKey
	// PrivateJwk is the JSON of the full private JWK, which is what gets
	// persisted; PublicJwk is the JSON of the same key without `d`, which is
	// what goes into the DPoP proof's protected header.
	PrivateJwk string
	PublicJwk  string
	// publicJwk is the decoded form, reused for every proof this key signs.
	publicJwk map[string]any
}

// generateDpopKeyPair mints a fresh ES256 key pair.  CodeArts' DPoP proof is
// ES256 over P-256, and the STS endpoint refuses anything else.
func generateDpopKeyPair() (*dpopKeyPair, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("codearts: generating a DPoP key: %w", err)
	}
	return newDpopKeyPair(priv)
}

// newDpopKeyPair wraps an existing private key in its JWK encodings.
func newDpopKeyPair(priv *ecdsa.PrivateKey) (*dpopKeyPair, error) {
	if priv == nil {
		return nil, errors.New("codearts: nil DPoP key")
	}
	if priv.Curve != elliptic.P256() {
		return nil, errors.New("codearts: the DPoP key must be on P-256")
	}
	x := padTo32(priv.PublicKey.X)
	y := padTo32(priv.PublicKey.Y)
	d := padTo32(priv.D)

	pub := map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   b64url(x),
		"y":   b64url(y),
	}
	full := map[string]any{
		"kty": "EC",
		"crv": "P-256",
		"x":   b64url(x),
		"y":   b64url(y),
		"d":   b64url(d),
	}
	privJSON, err := json.Marshal(full)
	if err != nil {
		return nil, fmt.Errorf("codearts: encoding the DPoP private JWK: %w", err)
	}
	pubJSON, err := json.Marshal(pub)
	if err != nil {
		return nil, fmt.Errorf("codearts: encoding the DPoP public JWK: %w", err)
	}
	return &dpopKeyPair{
		Private:    priv,
		PrivateJwk: string(privJSON),
		PublicJwk:  string(pubJSON),
		publicJwk:  pub,
	}, nil
}

// keyPairFromStoredJwk rebuilds a key pair out of the private JWK that was
// persisted with the credential.  A key that cannot be rebuilt is an error the
// caller reports: a credential whose key is gone can never be refreshed, so
// pretending otherwise would just produce a confusing signature failure
// later.
func keyPairFromStoredJwk(jwk string) (*dpopKeyPair, error) {
	jwk = strings.TrimSpace(jwk)
	if jwk == "" {
		return nil, errors.New("codearts: no DPoP private key is stored with this credential")
	}
	var raw struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		X   string `json:"x"`
		Y   string `json:"y"`
		D   string `json:"d"`
	}
	if err := json.Unmarshal([]byte(jwk), &raw); err != nil {
		return nil, fmt.Errorf("codearts: parsing the stored DPoP key: %w", err)
	}
	if !strings.EqualFold(raw.Kty, "EC") || raw.Crv != "P-256" {
		return nil, fmt.Errorf("codearts: the stored DPoP key is %s/%s, expected EC/P-256", raw.Kty, raw.Crv)
	}
	xb, err := b64urlDecode(raw.X)
	if err != nil {
		return nil, fmt.Errorf("codearts: the stored DPoP key has no usable x: %w", err)
	}
	yb, err := b64urlDecode(raw.Y)
	if err != nil {
		return nil, fmt.Errorf("codearts: the stored DPoP key has no usable y: %w", err)
	}
	db, err := b64urlDecode(raw.D)
	if err != nil {
		return nil, fmt.Errorf("codearts: the stored DPoP key has no usable d: %w", err)
	}
	curve := elliptic.P256()
	priv := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: curve,
			X:     new(big.Int).SetBytes(xb),
			Y:     new(big.Int).SetBytes(yb),
		},
		D: new(big.Int).SetBytes(db),
	}
	if !curve.IsOnCurve(priv.PublicKey.X, priv.PublicKey.Y) {
		return nil, errors.New("codearts: the stored DPoP key is not a point on P-256")
	}
	// Re-derive the public half from the private scalar and check it agrees.
	// A JWK whose x/y do not match d would produce proofs the server rejects
	// with no hint as to why, so it is caught here instead.
	dx, dy := curve.ScalarBaseMult(db)
	if dx.Cmp(priv.PublicKey.X) != 0 || dy.Cmp(priv.PublicKey.Y) != 0 {
		return nil, errors.New("codearts: the stored DPoP key is inconsistent (x/y do not match d)")
	}
	return newDpopKeyPair(priv)
}

// padTo32 left-pads a big integer to the 32-byte width P-256 coordinates and
// scalars use in a JWK.  Without it a coordinate whose leading byte is zero
// would encode short and the server would reject the key.
func padTo32(n *big.Int) []byte {
	out := make([]byte, 32)
	if n == nil {
		return out
	}
	b := n.Bytes()
	if len(b) > 32 {
		b = b[len(b)-32:]
	}
	copy(out[32-len(b):], b)
	return out
}

// ---------------------------------------------------------------------------
// DPoP proof
// ---------------------------------------------------------------------------

// dpopProof is a signed DPoP proof, ready for the `DPoP` header.
type dpopProof struct {
	// Compact is the JWS in `header.payload.signature` form.
	Compact string
	// Jkt is the JWK thumbprint (RFC 7638) of the key that signed it, which
	// the server echoes back in the credential's `cnf.jkt`.  It is exposed so
	// a test can assert the proof is bound to the key the caller holds.
	Jkt string
}

// signDpopProof builds the DPoP proof for one HTTP request.
//
// The proof is an ES256 JWS whose protected header carries the public key
// inline (`alg`, `typ`, `jwk`) and whose payload names the HTTP method and the
// full request URL (`htm`, `htu`), a timestamp (`iat`) and a random
// identifier (`jti`).  The signature is the raw R||S pair, 32 bytes each —
// JWS wants the concatenation, not the ASN.1 DER encoding that
// ecdsa.SignASN1 produces, and getting that wrong yields an opaque
// `InvalidDPoPHeader` from the server.
func (k *dpopKeyPair) signDpopProof(htm, htu string, now time.Time) (dpopProof, error) {
	if k == nil || k.Private == nil {
		return dpopProof{}, errors.New("codearts: no DPoP key to sign with")
	}
	header := map[string]any{
		"alg": "ES256",
		"typ": "dpop+jwt",
		"jwk": k.publicJwk,
	}
	jti := make([]byte, 32)
	if _, err := rand.Read(jti); err != nil {
		return dpopProof{}, fmt.Errorf("codearts: generating a DPoP jti: %w", err)
	}
	payload := map[string]any{
		"htm": htm,
		"htu": htu,
		"iat": now.Unix(),
		"jti": hex.EncodeToString(jti),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return dpopProof{}, fmt.Errorf("codearts: encoding the DPoP header: %w", err)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return dpopProof{}, fmt.Errorf("codearts: encoding the DPoP payload: %w", err)
	}
	signingInput := b64url(headerJSON) + "." + b64url(payloadJSON)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, k.Private, digest[:])
	if err != nil {
		return dpopProof{}, fmt.Errorf("codearts: signing the DPoP proof: %w", err)
	}
	sig := append(padTo32(r), padTo32(s)...)
	return dpopProof{
		Compact: signingInput + "." + b64url(sig),
		Jkt:     k.thumbprint(),
	}, nil
}

// thumbprint is the RFC 7638 JWK thumbprint of the public key: SHA-256 over
// the canonical JSON of exactly the required members, in lexicographic order.
// CodeArts' STS binds the issued credential to this value.
func (k *dpopKeyPair) thumbprint() string {
	x, _ := k.publicJwk["x"].(string)
	y, _ := k.publicJwk["y"].(string)
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, x, y)
	sum := sha256.Sum256([]byte(canonical))
	return b64url(sum[:])
}

// ---------------------------------------------------------------------------
// misc identifiers
// ---------------------------------------------------------------------------

// newUUID returns a random RFC 4122 version 4 UUID.  It is used for the
// identifiers the chat API wants (Chat-Id, Session-Id, prompt_cache_key, the
// queue trace id) and for local login sessions.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; if it ever does, a
		// timestamp-derived value keeps the caller working rather than
		// panicking inside a request path.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randomHex returns n random bytes as lowercase hex.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%0*x", n*2, time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// sha256Hex is the hex SHA-256 used by the SDK-HMAC-SHA256 canonical request
// and its payload hash.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
