package workbuddy

// The conversation-header identity family.
//
// The vendor aggregates a conversation's usage server-side by
// X-Conversation-Request-ID.  The official client mints one id per user submit
// and reuses it for every request that submit produces — every tool-call round
// trip, every retry, every account rotation — so the backend sees one logical
// turn.  A gateway that sends a fresh random id per HTTP request instead makes
// that same turn look like dozens of unrelated requests, and the vendor's own
// per-conversation accounting stops lining up.
//
// The fix is to derive the id instead of rolling it: from the session key and
// from the content of the turn's last user message.  Two requests belonging to
// the same turn then agree without sharing any state, and a new user message
// produces a new id.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// deriveSalt is the per-process salt every derived id is keyed with.  It is
// re-rolled on restart, which is exactly what we want: the ids only have to be
// consistent with each other inside one process lifetime, and a restart should
// not let a stale id from the previous run be mistaken for the same turn.
var deriveSalt = newMessageID()

// deriveID returns sha256(salt|key) truncated to 16 bytes and hex encoded, the
// same 32-hex shape every other id in this header family carries.
//
// It is a pure derivation: no cache, no TTL, and no table that grows with the
// number of keys seen.
func deriveID(key string) string {
	sum := sha256.Sum256([]byte(deriveSalt + "|" + key))
	return hex.EncodeToString(sum[:16])
}

// requestIDForKey returns the stable id for a session key.  An empty key names
// no session, so there is nothing to aggregate and it degrades to a fresh
// random id.
func requestIDForKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return newMessageID()
	}
	return deriveID(key)
}

// turnRequestID returns the id for a turn key.  An empty key means the turn had
// no signable user message; reusing the previous turn's id would claim an
// aggregation that does not exist, so it degrades to a fresh random id.
func turnRequestID(turnKey string) string {
	if strings.TrimSpace(turnKey) == "" {
		return newMessageID()
	}
	return deriveID(turnKey)
}

// turnKey derives the aggregation key of one conversation turn: the position
// and the content signature of the LAST user message.
//
// Last, not first.  Taking the first user message would collapse an entire
// conversation into one key, so every turn would look like the same turn; the
// last one changes exactly when the user submits something new, which is the
// boundary the vendor draws.
//
// The scan stops at the first user message found from the end even when that
// message has no signable content.  Its position is stable for the whole turn,
// whereas searching further back would make the key drift from step to step as
// the history grows.
func turnKey(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		sig := contentSignature(m)
		if sig == "" {
			return ""
		}
		return fmt.Sprintf("u%d:%s", i, sig)
	}
	return ""
}

// contentSignature reduces a message to the text a turn key can be built from.
//
// A message whose content arrived as a plain string and one whose content
// arrived as an array of text parts hash identically, so a caller that switches
// between the two spellings keeps the same aggregation id.
//
// Non-text parts cannot be reproduced byte for byte at this point — the gateway
// has already parsed the inbound JSON — so each contributes a short digest of
// its payload rather than its (possibly very large) base64 body.  A turn that
// is only an image still gets a stable key instead of an empty one.
func contentSignature(m core.Message) string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	var b strings.Builder
	nonText := false
	for _, p := range m.Parts {
		switch p.Type {
		case "", "text":
			b.WriteString(p.Text)
		default:
			nonText = true
			sum := sha256.Sum256([]byte(p.ImageURL + "\x00" + p.Detail))
			fmt.Fprintf(&b, "\n[%s:%s]\n", p.Type, hex.EncodeToString(sum[:4]))
		}
	}
	if !nonText {
		return b.String()
	}
	return strings.TrimSpace(b.String())
}

// conversationRequestID picks the X-Conversation-Request-ID for one request.
//
// Precedence, mirroring the reference: an id the caller supplied wins; then the
// turn key with the session key salted in (a composite, so identical turn text
// in two different conversations cannot collide); then the bare turn key (no
// session key to salt with); then the session-level id, for the leftover case
// where the turn had no signable user message at all; and only when nothing is
// available a request-scoped random value.
//
// The caller derives this once per inbound request and reuses it verbatim for
// every attempt, which is what makes the whole turn one aggregation unit.
func conversationRequestID(passthrough, sessionKey string, req *core.ChatRequest) string {
	if v := strings.TrimSpace(passthrough); v != "" {
		return v
	}
	tk := turnKey(req)
	switch {
	case tk != "" && sessionKey != "":
		return turnRequestID(sessionKey + ":" + tk)
	case tk != "":
		return turnRequestID(tk)
	case sessionKey != "":
		return requestIDForKey(sessionKey)
	default:
		return turnRequestID("")
	}
}
