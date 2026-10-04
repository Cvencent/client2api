package core

// Reading an identity out of a vendor's own token.
//
// Two modules receive a JWT from their vendor and need exactly one thing out of
// it: the id of the account it was issued to.  That id is what lets the panel
// show one account once instead of once per credential — the CLI's login and
// the module's own login are the same Kimi user, the plan JWT and the
// coding-plan API key are the same Zhipu user (see AccountRecord.Identity).
//
// The token is treated as opaque.  Its signature is never checked: there is
// nothing here to check it against, and the value is only ever used to *label*
// a credential, never to authorise anything.  Nothing in this file returns,
// stores or logs the token itself.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
)

// JWTClaim reads the first of names that the token's payload carries as a
// non-empty value.  It returns "" for anything that is not a three-part token,
// whose payload is not base64url, is not a JSON object, or carries none of the
// names — an unreadable token is a token whose account is simply unknown.
//
// The names are tried in order, so a caller that wants "user_id, falling back
// to the subject" passes them in that order.
//
// A numeric claim is accepted, and is read with json.Number rather than as a
// float64: vendor account ids are routinely larger than 2^53, and a float64
// would silently round one into a *different* account's id.
func JWTClaim(token string, names ...string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var claims map[string]any
	if err := dec.Decode(&claims); err != nil {
		return ""
	}
	for _, name := range names {
		switch v := claims[name].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case json.Number:
			if s := strings.TrimSpace(v.String()); s != "" {
				return s
			}
		}
	}
	return ""
}

// JWTIdentity is JWTClaim for the claim names vendors actually use for an
// account id, most specific first.  It exists so every module spells the list
// the same way.
func JWTIdentity(token string) string {
	return JWTClaim(token, "user_id", "userId", "uid", "sub")
}

// JWTExpiry reads a token's exp claim as unix seconds, or 0 when it carries
// none.  A non-positive value is also reported as 0: "expired at the epoch" and
// "expired before the epoch" are both ways of saying the token states no usable
// expiry, and every caller treats 0 as "unknown".  Like JWTClaim it never
// verifies anything; an expiry read from an unverified token is a display hint,
// not an authorisation decision.
func JWTExpiry(token string) int64 {
	raw := JWTClaim(token, "exp")
	if raw == "" {
		return 0
	}
	// JWTClaim renders a numeric claim through json.Number, so a whole number
	// arrives without a fractional part; anything else is not an expiry.
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
