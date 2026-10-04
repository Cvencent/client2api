package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Failure classification.
//
// Zen's verdicts are finer than the gateway's vocabulary, and one of them is
// actively misleading if you go by status code alone:
//
//	POST /chat/completions {"model":"bogus-model-xyz", ...}   (no auth)
//	  -> HTTP 401  {"type":"error","error":{"type":"ModelError",
//	                "message":"Model bogus-model-xyz is not supported"}}
//
// The model check runs BEFORE the auth check and both answer 401.  A
// status-only classifier would read that as "the key is bad" and disable a
// perfectly healthy account because a caller mistyped a model id.  So the
// vendor's own `error.error.type` is consulted first and the status code is only
// a fallback.
// ---------------------------------------------------------------------------

type failureKind string

const (
	kindNone      failureKind = ""
	kindModel     failureKind = "model"     // unknown model: the caller's fault, no account impact
	kindAuth      failureKind = "auth"      // the key was rejected
	kindQuota     failureKind = "quota"     // out of credit: park for a long time
	kindFreeTier  failureKind = "freetier"  // the free-tier handshake was refused: transient
	kindRate      failureKind = "rate"      // throttled: park briefly
	kindServer    failureKind = "server"    // 5xx
	kindClient    failureKind = "client"    // 4xx this module did not anticipate
	kindTransport failureKind = "transport" // the request never got an answer
)

// coreKindFor maps the module's finer verdict onto the gateway's vocabulary.
func coreKindFor(k failureKind) core.FailureKind {
	switch k {
	case kindAuth:
		return core.FailureAuth
	case kindQuota:
		return core.FailureQuota
	case kindRate:
		return core.FailureRateLimited
	case kindFreeTier:
		// The free tier is gated by request shape, not by the credential, and
		// the gate opens and closes on its own.  Retrying is right, so this is a
		// rate-limit verdict rather than an auth failure.
		return core.FailureRateLimited
	case kindServer, kindTransport:
		return core.FailureUpstream
	default:
		return core.FailureOther
	}
}

// ---------------------------------------------------------------------------
// The vendor's error envelope.
// ---------------------------------------------------------------------------

type errorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
	// Message is the fallback for a flatter shape.
	Message string `json:"message"`
}

// errorTextOf pulls the human-readable part out of an error body.  The body is
// JSON even when the response is labelled text/plain (Zen labels every error
// that way), and a 404 from a wrong base URL is an HTML page, so a decode
// failure is expected rather than exceptional.
func errorTextOf(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil {
		if msg := strings.TrimSpace(env.Error.Message); msg != "" {
			return msg
		}
		if msg := strings.TrimSpace(env.Message); msg != "" {
			return msg
		}
	}
	return truncate(core.Redact(trimmed), 300)
}

// errorTypeOf returns the vendor's error type, e.g. "AuthError".
func errorTypeOf(body []byte) string {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if t := strings.TrimSpace(env.Error.Type); t != "" {
		return t
	}
	return strings.TrimSpace(env.Type)
}

// Marker lists for the cases where the envelope carries no usable type.
//
// These are heuristics and are documented as such: only the AuthError and
// ModelError type names were observed live.  They are consulted only when the
// type is absent or unrecognised, so a future named type never depends on them.
var (
	modelMarkers    = []string{"is not supported", "not supported", "unknown model", "model not found", "no such model"}
	freeTierMarkers = []string{"free tier", "free-tier", "freetier"}
	rateMarkers     = []string{"rate limit", "ratelimit", "too many requests", "slow down"}
	quotaMarkers    = []string{"insufficient credit", "out of credit", "no credit", "credit balance",
		"insufficient balance", "insufficient funds", "payment required", "billing", "quota"}
	authMarkers = []string{"missing api key", "invalid api key", "unauthorized", "unauthenticated",
		"not authenticated", "authentication", "api key"}
)

// containsMarker reports whether text carries any of markers, case-insensitively.
func containsMarker(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, m := range markers {
		if m == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// classifyType maps the vendor's error type name to a verdict.
//
// Only "AuthError" and "ModelError" were observed live.  The remaining arms are
// substring matches on the vendor's own type name — a much narrower guess than
// pattern-matching the prose, and strictly better than the status code, which
// is 401 for both of the cases we did observe.
func classifyType(typ string) failureKind {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "":
		return kindNone
	case "autherror", "authenticationerror", "unauthorizederror", "invalidapikeyerror":
		return kindAuth
	case "modelerror", "modelnotfounderror", "unsupportedmodelerror":
		return kindModel
	case "freetiererror", "freetier":
		return kindFreeTier
	}
	lower := strings.ToLower(typ)
	switch {
	case strings.Contains(lower, "freetier") || strings.Contains(lower, "free tier"):
		// Must precede the generic "auth" arm below: the vendor's free-tier
		// refusal is not a bad key.
		return kindFreeTier
	case strings.Contains(lower, "ratelimit") || strings.Contains(lower, "toomanyrequest"):
		return kindRate
	case strings.Contains(lower, "credit") || strings.Contains(lower, "quota") ||
		strings.Contains(lower, "billing") || strings.Contains(lower, "payment"):
		return kindQuota
	case strings.Contains(lower, "auth"):
		return kindAuth
	case strings.Contains(lower, "model"):
		return kindModel
	}
	return kindNone
}

// verdict is the classifier's decision for one failed request.
type verdict struct {
	kind   failureKind
	typ    string
	msg    string
	status int
}

// text renders the verdict for an error message.
func (v verdict) text() string {
	switch {
	case v.msg != "" && v.typ != "":
		return fmt.Sprintf("%s: %s", v.typ, v.msg)
	case v.msg != "":
		return v.msg
	case v.typ != "":
		return v.typ
	default:
		return fmt.Sprintf("HTTP %d", v.status)
	}
}

// classify decides what an upstream failure means.
//
// Order matters and is the whole point of this file:
//  1. the vendor's named error type, when present and recognised;
//  2. text markers — rate before quota before auth, because reading a billing
//     refusal as an auth failure would disable an account that is merely out of
//     money, while reading it as throttling merely parks it too long;
//  3. the status code.
func classify(status int, body []byte) verdict {
	v := verdict{status: status}
	v.typ = errorTypeOf(body)
	v.msg = errorTextOf(body)

	if k := classifyType(v.typ); k != kindNone {
		v.kind = k
		return v
	}

	switch {
	case containsMarker(v.msg, modelMarkers):
		v.kind = kindModel
	case containsMarker(v.msg, rateMarkers):
		v.kind = kindRate
	case containsMarker(v.msg, quotaMarkers):
		v.kind = kindQuota
	case containsMarker(v.msg, freeTierMarkers):
		// Must precede authMarkers and the 401/403 status fallback: the
		// free-tier gate answers 403, which would otherwise disable the account.
		v.kind = kindFreeTier
	case containsMarker(v.msg, authMarkers):
		v.kind = kindAuth
	default:
		switch {
		case status == 402:
			v.kind = kindQuota
		case status == 429:
			v.kind = kindRate
		case status == 401 || status == 403:
			v.kind = kindAuth
		case status >= 500:
			v.kind = kindServer
		case status >= 400:
			v.kind = kindClient
		default:
			v.kind = kindServer
		}
	}
	return v
}

// upstreamError carries the vendor's own words through to the caller.  The
// vendor's numeric code stays inside the message: error.code belongs to the
// gateway, never to the module.
type upstreamError struct {
	Op     string
	Status int
	Type   string
	Msg    string
}

func (e *upstreamError) Error() string {
	parts := []string{clientName + ": " + e.Op}
	if e.Status > 0 {
		parts = append(parts, fmt.Sprintf("HTTP %d", e.Status))
	}
	if e.Type != "" {
		parts = append(parts, e.Type)
	}
	if e.Msg != "" {
		parts = append(parts, e.Msg)
	}
	return core.Redact(strings.Join(parts, ": "))
}

// acctID is the account id of a record, or "" for nil.
func acctID(a *accountRecord) string {
	if a == nil {
		return ""
	}
	return a.ID
}

// classifyHTTP turns a non-2xx response into the error the gateway should see,
// and records the verdict against the account.
//
// Recording belongs here rather than at the call site: a classifier that
// classifies but does not record is a footgun, because every caller then has to
// remember to re-derive the verdict in order to park the account.
//
// An unknown model becomes a wrapped core.ErrUnsupported (HTTP 400) rather than
// a typed core.Failure, because the request — not the account — is what is
// wrong, and noteFailure deliberately does nothing for that verdict.
func (c *Client) classifyHTTP(op string, acct *accountRecord, status int, body []byte) error {
	v := classify(status, body)
	c.noteFailure(acct, v.kind, v.text())
	if v.kind == kindModel {
		return fmt.Errorf("%w: %s", core.ErrUnsupported, v.text())
	}
	return core.Fail(clientName, acctID(acct), coreKindFor(v.kind), status, &upstreamError{
		Op:     op,
		Status: status,
		Type:   v.typ,
		Msg:    v.msg,
	})
}

// classifyErrFor classifies a transport-level error and records it against the
// account.  A reset connection is not the vendor refusing anything, so it is
// treated as an upstream/server failure rather than as a client mistake.
func (c *Client) classifyErrFor(acct *accountRecord, op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	k := kindTransport
	msg := describeError(err)
	c.noteFailure(acct, k, msg)
	return core.Fail(clientName, acctID(acct), coreKindFor(k), 0, &upstreamError{Op: op, Msg: msg})
}

// softErrorThreshold is how many soft failures an account may accumulate before
// it is parked.  One 400 is not "rate limited for an hour".
const softErrorThreshold = 3

// noteFailure records a verdict against an account.
//
// Only the capacity verdicts park the account, and each for its own duration:
// a key the vendor rejected is disabled, running out of credit parks for
// quota_cooldown, throttling parks for cooldown.  A bad request from the caller
// quota_cooldown, throttling parks for cooldown.  A closed free-tier gate parks
// for cooldown too — it is never disabled, because it reopens by itself.  A bad
// and an unknown model are recorded against the module, not the account — they
// say nothing about the account's health.
func (c *Client) noteFailure(acct *accountRecord, k failureKind, msg string) {
	if acct == nil {
		return
	}
	now := c.now()
	switch k {
	case kindAuth:
		c.pool.disable(acct.ID, firstNonEmpty(msg, "the vendor rejected this key"))
	case kindQuota:
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.quotaCooldown()), firstNonEmpty(msg, "out of credit"))
	case kindRate:
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.cooldown()), firstNonEmpty(msg, "throttled"))
	case kindFreeTier:
		// Never disable: the free-tier gate opens and closes on its own, and a
		// disabled account would never be retried.  Park it briefly and rotate.
		c.pool.setCooldown(acct.ID, now.Add(c.cfg.cooldown()), firstNonEmpty(msg, "free tier temporarily unavailable"))
	case kindModel, kindClient:
		// Nothing: the account is fine, the request was not.
		return
	default:
		c.pool.noteError(acct.ID, msg, now, softErrorThreshold)
	}
	c.persist()
}

// noteSuccess clears whatever a previous failure left behind.
func (c *Client) noteSuccess(acct *accountRecord) {
	if acct == nil {
		return
	}
	c.pool.clearCooldown(acct.ID)
}

// describeError renders an error for the panel and the log, redacted and
// bounded.
func describeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(core.Redact(err.Error()), 300)
}

// scrubSecret removes a literal credential from a message before it is logged.
// core.Redact works on shapes; this catches a bare key that has no shape.
func scrubSecret(msg, secret string) string {
	msg = core.Redact(msg)
	if secret == "" {
		return msg
	}
	if strings.Contains(msg, secret) {
		msg = strings.ReplaceAll(msg, secret, core.MaskSecret(secret))
	}
	return msg
}
