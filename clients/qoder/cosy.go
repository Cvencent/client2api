package qoder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cosy.go implements the COSY request signature the Qoder CN gateway
// (gateway.qoder.com.cn) requires on its /algo edge.
//
// The scheme is the one the desktop client ships (verified against the CN
// build's own bundle):
//
//  1. build a compact JSON identity object holding the account uid, name,
//     e-mail and oauth token -- `security_oauth_token` is the device token the
//     client itself stores, see credential.go;
//  2. AES-128-CBC encrypt it under a random 16-character "temp key" used as
//     BOTH key and IV, then base64 the ciphertext -- this is `info`;
//  3. RSA-encrypt the temp key with PKCS#1 v1.5 under the vendor's 1024-bit
//     public key, then base64 the ciphertext -- this is the Cosy-Key;
//  4. build {version, requestId, info, cosyVersion, ideVersion} in exactly that
//     key order and base64 it -- this is the payload segment;
//  5. md5(payload + "\n" + cosyKey + "\n" + unixDate + "\n" + body + "\n" +
//     path) -- this is the signature segment, where path is the request path
//     with a leading /algo removed;
//  6. send `Authorization: Bearer COSY.<payload>.<signature>` next to
//     Cosy-User / Cosy-Key / Cosy-Date and the client-identity headers.
//
// Everything uses crypto/* and encoding/base64 rather than any hand-rolled
// primitive.  The only deliberate non-standard choices are the protocol's own:
// key == IV for the CBC step, and MD5 as the signature digest (a wire-format
// requirement, not a security decision).

// pathAlgoPrefix is the edge mount point the vendor signs away.
const pathAlgoPrefix = "/algo"

// cosyOAuthFieldName is the identity key the desktop client writes for the
// token.  The server re-serialises the decrypted identity, so the exact
// spelling matters.
const cosyOAuthFieldName = "security_oauth_token"

// cosyExponent is the RSA public exponent of the vendor's key (65537).
const cosyExponent = 65537

// cosyModulusHex is the vendor's 1024-bit RSA modulus, exactly as the CN
// desktop client ships it.  TestCosyPublicKeyMatchesVendorKey re-derives it
// from the vendor's SubjectPublicKeyInfo so a typo here cannot go unnoticed.
const cosyModulusHex = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4" +
	"a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a" +
	"1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24b" +
	"e1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"

// cosyVendorPublicKeyPEM is the same key in the spelling the desktop client
// embeds.  It exists so a test can prove the two spellings agree.
const cosyVendorPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

var (
	cosyKeyOnce sync.Once
	cosyKeyPub  *rsa.PublicKey
	cosyKeyErr  error
)

// cosyPublicKey returns the vendor's RSA public key, parsed once.  A malformed
// constant yields an error rather than a panic: this module must never take the
// process down.
func cosyPublicKey() (*rsa.PublicKey, error) {
	cosyKeyOnce.Do(func() {
		n, ok := new(big.Int).SetString(cosyModulusHex, 16)
		if !ok || n.Sign() <= 0 {
			cosyKeyErr = errors.New("qoder: built-in COSY RSA modulus is not valid hex")
			return
		}
		cosyKeyPub = &rsa.PublicKey{N: n, E: cosyExponent}
	})
	return cosyKeyPub, cosyKeyErr
}

// ---------------------------------------------------------------------------
// encoding helpers
// ---------------------------------------------------------------------------

// jsonValue renders v the way JavaScript's JSON.stringify does, which matters
// because the server re-serialises the same objects to verify them.
// Encoding/json's HTML escaping (which would turn "<" into "\u003c") is off.
func jsonValue(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return strings.TrimRight(buf.String(), "\n")
}

// orderedCompact renders obj as compact JSON in the order the keys are given.
// The COSY payload is signed byte-for-byte, and the desktop client builds it
// with object-literal key order rather than alphabetically, so the order is
// part of the wire format.
func orderedCompact(keys []string, obj map[string]any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(jsonValue(k))
		b.WriteByte(':')
		b.WriteString(jsonValue(obj[k]))
	}
	b.WriteByte('}')
	return b.String()
}

// randomHex returns n random hexadecimal characters.  n is 16 for the COSY temp
// key, which must be exactly 16 ASCII bytes because it is used as an AES-128
// key.
func randomHex(n int) (string, error) {
	if n <= 0 {
		return "", nil
	}
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf)[:n], nil
}

// cosyRequestID returns the request-id spelling the client uses: a random
// RFC 4122 UUID with every dash removed.
func cosyRequestID() string {
	return strings.ReplaceAll(newUUID(), "-", "")
}

// ---------------------------------------------------------------------------
// primitives
// ---------------------------------------------------------------------------

// aesCBCEncryptPKCS7 encrypts plain under a 16-byte key with AES-128-CBC, using
// the key as the IV as well (the protocol derives both from the same temp key)
// and PKCS#7 padding.
func aesCBCEncryptPKCS7(plain, key []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("qoder: COSY temp key must be %d bytes, got %d", aes.BlockSize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	buf := make([]byte, len(plain)+pad)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(pad)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, buf)
	return out, nil
}

// rsaEncryptPKCS1v15 encrypts msg with the RSAES-PKCS1-v1_5 scheme, whose
// random non-zero padding is exactly what the protocol expects.
func rsaEncryptPKCS1v15(pub *rsa.PublicKey, msg []byte) ([]byte, error) {
	if pub == nil {
		return nil, errors.New("qoder: nil COSY public key")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, pub, msg)
}

// md5Hex is the signature digest.  MD5 is required by the protocol; it is not
// used here as a security primitive.
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// session
// ---------------------------------------------------------------------------

// cosySession is the per-credential signing material.  It is derived once per
// token and reused: `info` and `cosyKey` are stable for a session, and
// re-deriving them per request would rotate the identity the server tracks.
type cosySession struct {
	uid     string
	tempKey string
	cosyKey string // base64(RSAES-PKCS1-v1_5(tempKey))
	info    string // base64(AES-128-CBC(identity, tempKey))
}

// newCosySession builds a fresh signing session for one account.
func newCosySession(uid, name, email, token string) (cosySession, error) {
	pub, err := cosyPublicKey()
	if err != nil {
		return cosySession{}, err
	}
	tempKey, err := randomHex(aes.BlockSize)
	if err != nil {
		return cosySession{}, fmt.Errorf("qoder: generating COSY temp key: %w", err)
	}
	// Key names and order are the desktop client's, not ours: it writes
	// uid/aid/name/email/security_oauth_token in that order.
	identity := orderedCompact([]string{"uid", "aid", "name", "email", cosyOAuthFieldName}, map[string]any{
		"uid":              uid,
		"aid":              "",
		"name":             name,
		"email":            email,
		cosyOAuthFieldName: token,
	})
	encrypted, err := aesCBCEncryptPKCS7([]byte(identity), []byte(tempKey))
	if err != nil {
		return cosySession{}, fmt.Errorf("qoder: sealing COSY identity: %w", err)
	}
	wrapped, err := rsaEncryptPKCS1v15(pub, []byte(tempKey))
	if err != nil {
		return cosySession{}, fmt.Errorf("qoder: wrapping COSY temp key: %w", err)
	}
	return cosySession{
		uid:     uid,
		tempKey: tempKey,
		cosyKey: base64.StdEncoding.EncodeToString(wrapped),
		info:    base64.StdEncoding.EncodeToString(encrypted),
	}, nil
}

// cosyPayloadB64 renders the signed payload segment of the bearer token.  The
// key order is the client's literal order, not a sorted one.
func cosyPayloadB64(info, requestID, cosyVersion, ideVersion string) string {
	if cosyVersion == "" {
		cosyVersion = defaultCosyVersion
	}
	return base64.StdEncoding.EncodeToString([]byte(orderedCompact(
		[]string{"version", "requestId", "info", "cosyVersion", "ideVersion"},
		map[string]any{
			"version":     "v1",
			"requestId":   requestID,
			"info":        info,
			"cosyVersion": cosyVersion,
			"ideVersion":  ideVersion,
		},
	)))
}

// cosySignature is the md5 over the five newline-joined fields the server
// re-derives.  The body is the exact byte string that goes on the wire, so it
// must be marshalled once and reused for both the signature and the request.
func cosySignature(payloadB64, cosyKey string, unixDate int64, body, path string) string {
	return md5Hex(payloadB64 + "\n" + cosyKey + "\n" + strconv.FormatInt(unixDate, 10) + "\n" + body + "\n" + path)
}

// pathForSignature returns the request path with a leading "/algo" segment
// removed: the endpoint is mounted under /algo on the edge but signed without
// it.  Only an exact path segment counts, so "/algoish/api" keeps its prefix.
func pathForSignature(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("qoder: parsing request URL: %w", err)
	}
	p := u.Path
	switch {
	case p == pathAlgoPrefix:
		p = "/"
	case strings.HasPrefix(p, pathAlgoPrefix+"/"):
		p = strings.TrimPrefix(p, pathAlgoPrefix)
	}
	return p, nil
}

// cosyRequest carries everything one signed request needs.  The request id and
// the clock are fields rather than being read inside so that a test can pin them
// and get a deterministic signature.
type cosyRequest struct {
	UID         string // Cosy-User: the account uid
	Body        string // the exact bytes that go on the wire
	RawURL      string // full request URL
	Accept      string
	UserAgent   string
	RequestID   string // uuid inside the signed payload
	XRequestID  string // X-Request-Id header
	UnixDate    int64
	CosyVersion string
	IDVersion   string
	// ClientType / BusinessProduct are the desktop client's own identity
	// headers.  They are part of the request, not the signature.
	ClientType      string
	BusinessProduct string
	MachineOS       string
}

// headers renders the complete COSY header set for one request.
func (s cosySession) headers(r cosyRequest) (map[string]string, error) {
	path, err := pathForSignature(r.RawURL)
	if err != nil {
		return nil, err
	}
	accept := r.Accept
	if accept == "" {
		accept = "text/event-stream"
	}
	userAgent := r.UserAgent
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	clientType := r.ClientType
	if clientType == "" {
		clientType = strconv.Itoa(defaultClientType)
	}
	product := r.BusinessProduct
	if product == "" {
		product = defaultBusinessProduct
	}
	machineOS := r.MachineOS
	if machineOS == "" {
		machineOS = defaultMachineOS
	}
	ideVersion := r.IDVersion
	if ideVersion == "" {
		ideVersion = defaultCosyVersion
	}
	payload := cosyPayloadB64(s.info, r.RequestID, r.CosyVersion, ideVersion)
	date := strconv.FormatInt(r.UnixDate, 10)
	return map[string]string{
		"accept":                accept,
		"content-type":          "application/json",
		"user-agent":            userAgent,
		"x-request-id":          r.XRequestID,
		"x-ide-platform":        product,
		"x-version":             ideVersion,
		"x-machine-os":          machineOS,
		"cosy-clienttype":       clientType,
		"cosy-business-product": product,
		"cosy-user":             r.UID,
		"cosy-key":              s.cosyKey,
		"cosy-date":             date,
		"authorization":         "Bearer COSY." + payload + "." + cosySignature(payload, s.cosyKey, r.UnixDate, r.Body, path),
		"accept-encoding":       "identity",
	}, nil
}

// jsonMarshalled renders v as the exact wire body once, so the caller can sign
// the same bytes it sends.
func jsonMarshalled(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// nowUnix is a seam for tests that pin the signature clock.
var nowUnix = func() int64 { return time.Now().Unix() }
