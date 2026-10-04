package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Shared plumbing for the vendor growth/billing endpoints that were missing
// from this port (streak, travel, blackcat, trial, credits, school vouchers).
//
// The existing request paths in upstream.go/tasks.go/checkin.go each judge the
// gateway envelope themselves: taskJSON/postJSON hand the raw apiEnvelope back
// and let the caller decide, because a few routes have more than one success
// code.  The endpoints below have a single success code and upstream reports a
// refusal as `code != 0` on HTTP 200, so they want the *doJSON* semantics:
// non-zero code becomes an *Error whose Msg starts with "code=<n>".  That shape
// is load-bearing: the reference's idempotency checks match on it (trial's
// "code=14051", see trial.go), so envelopeError reproduces it byte for byte.

// envelopeError judges one gateway envelope the way Upstream.doJSON does.
// status is the HTTP status the envelope arrived with (200 for a business-code
// refusal).  A zero business code is not an error.
func envelopeError(status int, env apiEnvelope) error {
	if env.Code == 0 {
		return nil
	}
	kind := Classify(status, env.Msg)
	if kind == ErrNone {
		kind = ErrClient
	}
	return &Error{
		Kind:   kind,
		Status: status,
		Msg:    fmt.Sprintf("code=%d msg=%s", env.Code, truncate(strings.TrimSpace(env.Msg), 160)),
	}
}

// growthJSONAny marshals an optional payload and calls growthJSON.  A nil
// payload sends no body, matching the reference's `body any` helpers (GETs and
// the empty-body POSTs of the travel endpoints).
func (c *Client) growthJSONAny(ctx context.Context, a *Auth, method, path string, payload any, mp bool) (apiEnvelope, error) {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return apiEnvelope{}, err
		}
		body = raw
	}
	return c.growthJSON(ctx, a, method, path, body, mp)
}

// growthCall is the port of the reference `growthJSON`: it sends a growth-domain
// request on the chat host with the billing identity, judges the envelope, and
// returns the unwrapped data payload.
func (c *Client) growthCall(ctx context.Context, a *Auth, method, path string, payload any, mp bool) (json.RawMessage, error) {
	env, err := c.growthJSONAny(ctx, a, method, path, payload, mp)
	if err != nil {
		return nil, err
	}
	if err := envelopeError(http.StatusOK, env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// billingJSON is the port of the reference `billingJSON`: a billing-host request
// with the lighter billing identity and doJSON error semantics.  Unlike the
// check-in route it has a single success code, so a non-zero code is refused.
func (c *Client) billingJSON(ctx context.Context, a *Auth, method, path string, payload any) (json.RawMessage, error) {
	var body []byte
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = raw
	}
	env, err := c.taskJSON(ctx, method, c.up.billingBase(a)+path, body, func(req *http.Request) {
		c.up.BillingHeaders(req, a)
	})
	if err != nil {
		return nil, err
	}
	if err := envelopeError(http.StatusOK, env); err != nil {
		return nil, err
	}
	return env.Data, nil
}

// billingMeterJSON is the port of the reference `billingMeterJSON`: it walks the
// realm's ordered billing-meter path family (global has both the bare and the
// /v2 spelling, CN only /v2) and returns the first answer that is not an error,
// so one dead path does not hide the account's credits.
func (c *Client) billingMeterJSON(ctx context.Context, a *Auth, method string, payload any) (json.RawMessage, error) {
	paths := c.up.billingMeterPaths(a)
	if len(paths) == 0 {
		return nil, fmt.Errorf("no billing meter path is configured for this realm")
	}
	var lastErr error
	for _, path := range paths {
		data, err := c.billingJSON(ctx, a, method, path, payload)
		if err != nil {
			lastErr = err
			continue
		}
		return data, nil
	}
	return nil, lastErr
}

// decodePayload unmarshals an unwrapped payload, tolerating the empty and
// literal-null answers the vendor sends for "the block exists but is empty"
// (the reference ignores those too, e.g. TravelClaim's reward field).
func decodePayload(data json.RawMessage, out any) error {
	if out == nil || len(data) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return json.Unmarshal(data, out)
}
