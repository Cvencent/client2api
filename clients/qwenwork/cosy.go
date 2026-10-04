package qwenwork

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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cosy.go implements the COSY request signature the QwenWork / QoderWork CN
// desktop agent backend requires on every call.
//
// The scheme, learned from the reference implementation's wire behaviour (see
// README.md, "Provenance"):
//
//  1. build a compact JSON identity object, keys sorted, holding the account
//     uid, nickname, e-mail and access token;
//  2. AES-128-CBC encrypt it under a random 16-character "temp key" that is
//     used as BOTH the key and the IV, then base64 the ciphertext — this is
//     `info`;
//  3. RSA-encrypt the temp key with PKCS#1 v1.5 under a hard-coded 1024-bit
//     public key, then base64 the ciphertext — this is `cosyKey`;
//  4. build {cosyVersion, ideVersion, info, requestId, version} the same
//     compact/sorted way and base64 it — this is the payload segment;
//  5. md5(payload + "\n" + cosyKey + "\n" + unixDate + "\n" + body + "\n" +
//     path) — this is the signature segment;
//  6. send `Authorization: Bearer COSY.<payload>.<signature>` plus the cosy-*
//     and x-qwenwork-* header set.
//
// Everything here uses crypto/* and encoding/base64 rather than any hand-rolled
// primitive.  The only deliberate non-standard choices are the protocol's own:
// key == IV for the CBC step, and MD5 as the signature digest (which is a
// protocol requirement, not a security decision).

// Protocol constants.  The server validates these, so they are not knobs: a
// different value is a different client and will be rejected (or, worse,
// flagged).
const (
	cosyVersion      = "1.1.18"
	ideVersion       = "1.0.5"
	releaseVersion   = "1.0.5-26090901"
	buildNumber      = "26090901"
	cosyClientType   = "6"
	cosyProduct      = "qoder_work"
	cosyBusinessType = "agent"
	cosyScene        = "qwork"
	cosyMachineOS    = "x86_64_win32"
	cosyPlatform     = "win32"
	cosyArch         = "x64"
	cosyChannel      = "stable"
	cosyLoginVersion = "v2"

	defaultUserAgent = "qoderwork/1.0.5"
	defaultBaseURL   = "https://gateway.qwenwork.cn"

	// pathAlgoPrefix is the edge mount point the vendor signs away.
	pathAlgoPrefix = "/algo"
)

// cosyExponent is the RSA public exponent of the vendor's key (65537).
const cosyExponent = 65537

// cosyModulusHex is the vendor's 1024-bit RSA modulus, as shipped inside the
// desktop client.  TestCosyPublicKeyMatchesVendorPEM re-derives it from the
// vendor's SubjectPublicKeyInfo so a typo here cannot go unnoticed.
const cosyModulusHex = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4" +
	"a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a" +
	"1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24b" +
	"e1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"

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
			cosyKeyErr = errors.New("qwenwork: built-in COSY RSA modulus is not valid hex")
			return
		}
		cosyKeyPub = &rsa.PublicKey{N: n, E: cosyExponent}
	})
	return cosyKeyPub, cosyKeyErr
}

// ---------------------------------------------------------------------------
// encoding helpers
// ---------------------------------------------------------------------------

// jsonValue renders v exactly the way JavaScript's JSON.stringify does, which
// matters because the server re-serialises the same objects to verify them.
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

// sortedCompact renders obj as compact JSON with its keys sorted.  The COSY
// payload is signed byte-for-byte, so the order is part of the wire format.
func sortedCompact(obj map[string]any) string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
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

// newUUID returns a random RFC 4122 version-4 UUID in the lower-case canonical
// form the vendor's request ids use.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform we support; degrade to a
		// time-derived id rather than panicking.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ---------------------------------------------------------------------------
// primitives
// ---------------------------------------------------------------------------

// aesCBCEncryptPKCS7 encrypts plain under a 16-byte key with AES-128-CBC,
// using the key as the IV as well (the protocol derives both from the same temp
// key) and PKCS#7 padding.
func aesCBCEncryptPKCS7(plain, key []byte) ([]byte, error) {
	if len(key) != aes.BlockSize {
		return nil, fmt.Errorf("qwenwork: COSY temp key must be %d bytes, got %d", aes.BlockSize, len(key))
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
		return nil, errors.New("qwenwork: nil COSY public key")
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
// access token and reused: `info` and `cosyKey` are stable for a session, and
// re-deriving them per request would rotate the identity the server tracks.
type cosySession struct {
	machineID   string
	tempKey     string
	cosyKey     string // base64(RSAES-PKCS1-v1_5(tempKey))
	info        string // base64(AES-128-CBC(identity, tempKey))
	accessToken string
}

// newCosySession builds a fresh signing session for one account.
func newCosySession(uid, nickname, email, accessToken string) (cosySession, error) {
	pub, err := cosyPublicKey()
	if err != nil {
		return cosySession{}, err
	}
	tempKey, err := randomHex(aes.BlockSize)
	if err != nil {
		return cosySession{}, fmt.Errorf("qwenwork: generating COSY temp key: %w", err)
	}
	// Key names are the vendor's, not ours: the desktop client writes `name`
	// and `security_oauth_token`, not `nickname`/`accessToken`.  The server
	// re-serialises this object to verify the signature, so the exact names
	// matter.
	identity := sortedCompact(map[string]any{
		"uid":                  uid,
		"aid":                  uid,
		"name":                 nickname,
		"email":                email,
		"security_oauth_token": accessToken,
	})
	encrypted, err := aesCBCEncryptPKCS7([]byte(identity), []byte(tempKey))
	if err != nil {
		return cosySession{}, fmt.Errorf("qwenwork: sealing COSY identity: %w", err)
	}
	wrapped, err := rsaEncryptPKCS1v15(pub, []byte(tempKey))
	if err != nil {
		return cosySession{}, fmt.Errorf("qwenwork: wrapping COSY temp key: %w", err)
	}
	return cosySession{
		machineID:   newUUID(),
		tempKey:     tempKey,
		cosyKey:     base64.StdEncoding.EncodeToString(wrapped),
		info:        base64.StdEncoding.EncodeToString(encrypted),
		accessToken: accessToken,
	}, nil
}

// cosyPayloadB64 renders the signed payload segment of the bearer token.
func cosyPayloadB64(info, requestID string) string {
	return base64.StdEncoding.EncodeToString([]byte(sortedCompact(map[string]any{
		"cosyVersion": cosyVersion,
		"ideVersion":  ideVersion,
		"info":        info,
		"requestId":   requestID,
		"version":     "v1",
	})))
}

// cosySignature is the md5 over the five newline-joined fields the server
// re-derives.  The body is the exact byte string that goes on the wire, so it
// must be marshalled once and reused for both the signature and the request.
func cosySignature(payloadB64, cosyKey string, unixDate int64, body, path string) string {
	return md5Hex(payloadB64 + "\n" + cosyKey + "\n" + strconv.FormatInt(unixDate, 10) + "\n" + body + "\n" + path)
}

// pathForSignature returns the request path with a leading "/algo" segment
// removed: the endpoint is mounted under /algo on the edge but signed without
// it.  Only an exact path segment counts, so "/algoish/api" keeps its prefix
// (removing four characters because they happen to match would sign a
// different path from the one actually requested).
func pathForSignature(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("qwenwork: parsing request URL: %w", err)
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

// cosyRequest carries everything one signed request needs.  The request ids and
// the clock are fields rather than being read inside so that a test can pin
// them and get a deterministic signature.
type cosyRequest struct {
	UID        string // cosy-user: the account uid
	Body       string // the exact bytes that go on the wire
	RawURL     string // full request URL
	ModelKey   string // x-model-key; empty when the call is not model-scoped
	Accept     string
	UserAgent  string
	RequestID  string // uuid inside the signed payload
	XRequestID string // x-request-id header
	UnixDate   int64
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
	payload := cosyPayloadB64(s.info, r.RequestID)
	date := strconv.FormatInt(r.UnixDate, 10)
	h := map[string]string{
		"accept":                     accept,
		"content-type":               "application/json",
		"user-agent":                 userAgent,
		"x-request-id":               r.XRequestID,
		"x-qwenwork-version":         ideVersion,
		"x-qwenwork-release-version": releaseVersion,
		"x-qwenwork-build":           buildNumber,
		"x-qwenwork-platform":        cosyPlatform,
		"x-qwenwork-arch":            cosyArch,
		"x-qwenwork-channel":         cosyChannel,
		"cosy-version":               cosyVersion,
		"cosy-clienttype":            cosyClientType,
		"cosy-business-product":      cosyProduct,
		"cosy-business-type":         cosyBusinessType,
		"cosy-scene":                 cosyScene,
		"cosy-machineos":             cosyMachineOS,
		"login-version":              cosyLoginVersion,
		"authorization":              "Bearer COSY." + payload + "." + cosySignature(payload, s.cosyKey, r.UnixDate, r.Body, path),
		"cosy-key":                   s.cosyKey,
		"cosy-user":                  r.UID,
		"cosy-date":                  date,
		"cosy-machineid":             s.machineID,
		"accept-encoding":            "identity",
		"connection":                 "keep-alive",
		"cache-control":              "no-cache",
	}
	if r.ModelKey != "" {
		h["x-model-key"] = r.ModelKey
		h["x-model-source"] = "system"
	}
	return h, nil
}
