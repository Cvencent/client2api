package core

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

// BearerPrefix is the one scheme this gateway understands on the
// Authorization header.  It is matched case-insensitively, because HTTP
// auth schemes are case-insensitive and a client library that sends "bearer"
// must not be rejected.
const BearerPrefix = "bearer "

// VerifyBearer reports whether an inbound request may proceed.
//
// Ported from the reference internal/httpauth.  The comparison is constant
// time over SHA-256 digests rather than over the raw strings: comparing raw
// strings with == leaks the shared prefix length through timing, and digesting
// first also makes the comparison length-independent.
//
// accepted forms, in order: "Authorization: Bearer <key>",
// "Authorization: <key>" (some OpenAI SDK builds), and "X-Api-Key: <key>".
//
// An empty configured key disables auth entirely.  The reference treats that
// as a supported single-operator mode rather than an error, and so does this
// gateway; the panel is responsible for warning about it.
func VerifyBearer(authorization, apiKeyHeader, key string) bool {
	if key == "" {
		return true
	}
	got := strings.TrimSpace(authorization)
	if len(got) >= len(BearerPrefix) && strings.EqualFold(got[:len(BearerPrefix)], BearerPrefix) {
		got = strings.TrimSpace(got[len(BearerPrefix):])
	}
	if got == "" {
		got = strings.TrimSpace(apiKeyHeader)
	}
	if got == "" {
		return false
	}
	want := sha256.Sum256([]byte(key))
	have := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(want[:], have[:]) == 1
}
