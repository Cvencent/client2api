package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"client2api/internal/core"
)

// errors.go turns the vendor's two failure vocabularies -- a business code in a
// 200 response, and an HTTP status -- into one type, and then into the
// contract's FailureKind.

// Business codes.  Loomy answers HTTP 200 for business failures, so a code is
// the only thing that distinguishes success from a refusal.
const (
	loomyOKCode         = "000000"
	loomyAuthErrorCode  = "100002" // login invalid: the session is dead
	loomyBadRequestCode = "100001"
)

// envelope is the vendor's response wrapper.  The `/points/*` endpoints return
// it on success and on failure alike -- but not every business endpoint does.
// `GET /models` answers with a bare OpenAI-shaped list object that carries no
// `code` key at all (verified against the live vendor), so "has an envelope" is
// not something this module may assume.
type envelope struct {
	OK      bool
	Code    string
	Message string
	Data    json.RawMessage
}

// parseEnvelope unwraps one response body.
//
// Three outcomes, in this order:
//
//  1. not a JSON object or array -> a failure with an empty code, which is
//     exactly what the reference does: the vendor answers a wrong request with
//     an HTML error page often enough that "not JSON" has to be a first-class
//     outcome rather than an unmarshalling error.
//  2. no `code` key at all -> a *bare* payload, reported as success with the
//     whole body as Data.  `GET /models` is the live example: it returns
//     `{"object":"list","data":[...]}` and no code, and reading that as
//     "business error (no code)" used to fail the catalogue refresh and park a
//     perfectly healthy credential.  A bare JSON *array* is the same case: the
//     reference's `parseLoomyRemoteModels` accepts either shape, and
//     `parseRemoteModels` here already accepts both, so handing it the whole
//     body is enough.
//  3. a `code` key -> the vendor's envelope: `000000` is success, anything else
//     is a business failure.
//
// Only the *absence* of the key means "bare".  A key that is present but empty
// or null is still read as a failed envelope, so a malformed envelope keeps
// reporting itself instead of being silently promoted to a success.
func parseEnvelope(payload []byte) envelope {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return envelope{Message: "the response is not a JSON object"}
	}
	if trimmed[0] != '{' {
		if trimmed[0] == '[' && json.Valid(trimmed) {
			return envelope{OK: true, Data: payload}
		}
		return envelope{Message: "the response is not a JSON object"}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return envelope{Message: "the response is not a JSON object"}
	}

	rawCode, hasCode := fields["code"]
	if !hasCode {
		return envelope{OK: true, Data: payload}
	}

	code := scalarString(rawCode)
	// The vendor spells its message field `desc`; the OpenAI-shaped endpoints
	// spell it `message`.  Accept both, preferring the vendor's own.
	message := scalarString(fields["desc"])
	if message == "" {
		message = scalarString(fields["message"])
	}

	if code != loomyOKCode {
		if message == "" {
			if code == "" {
				message = "business error (no code)"
			} else {
				message = "business error " + code
			}
		}
		return envelope{Code: code, Message: message}
	}
	return envelope{OK: true, Code: code, Data: fields["data"]}
}

// apiError is one failed exchange with the vendor.
type apiError struct {
	// Code is the vendor's business code; empty when the failure happened at
	// the transport or HTTP layer.
	Code string
	// Status is the HTTP status; 0 when no response was ever received.
	Status int
	// Message is the vendor's own text where there is one.
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return "loomy: upstream " + e.Code + ": " + e.Message
	case e.Code != "":
		return "loomy: upstream " + e.Code
	case e.Status != 0 && e.Message != "":
		return fmt.Sprintf("loomy: upstream HTTP %d: %s", e.Status, e.Message)
	case e.Status != 0:
		return fmt.Sprintf("loomy: upstream HTTP %d", e.Status)
	case e.Message != "":
		return "loomy: " + e.Message
	}
	return "loomy: upstream failure"
}

// sessionDead reports whether this failure means the credential is finished.
//
// 100002 is the vendor's own "login invalid".  HTTP 401/403 is included because
// the chat endpoint answers with a status rather than a code when the bearer
// token is not even shaped like a session.
func (e *apiError) sessionDead() bool {
	return e.Code == loomyAuthErrorCode ||
		e.Status == http.StatusUnauthorized ||
		e.Status == http.StatusForbidden
}

// failureKind maps an error onto the contract's failure vocabulary.
func failureKind(err error) core.FailureKind {
	var api *apiError
	if errors.As(err, &api) {
		switch {
		case api.sessionDead():
			return core.FailureSessionDead
		case api.Status == http.StatusTooManyRequests:
			return core.FailureRateLimited
		case api.Status == http.StatusPaymentRequired:
			return core.FailureQuota
		case api.Code == loomyBadRequestCode:
			// The vendor rejected the request shape.  That is this module's
			// bug or the caller's, never something to retry.
			return core.FailureOther
		}
		return core.FailureUpstream
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return core.FailureUpstream
	}
	return core.FailureUpstream
}

// errSessionExpired is the honest report for a credential the vendor has
// already rejected.  There is no refresh endpoint to call: the operator has to
// log in again and import the new session.
func errSessionExpired(accountID string) error {
	msg := "loomy: the session has expired and Loomy has no refresh endpoint; " +
		"log in again (SMS or WeChat) and import the new session token"
	if accountID != "" {
		msg += " for account " + accountID
	}
	return &apiError{Code: loomyAuthErrorCode, Message: msg}
}

// callerGone reports whether a request failed because the caller walked away
// rather than because of anything the vendor said.
//
// A cancelled caller is not evidence about the credential.  The operator pressed
// Stop, the panel moved to another view, or the browser closed the tab; the
// request failed for a reason that says nothing about this account.  Parking it
// takes a healthy credential out of the pool, and with max_attempts > 1 a single
// cancellation burns the remaining candidates too -- after which every request
// is answered with "every account is parked or cooling down" for the whole
// cooldown window.
//
// The Chat rotation loop has had this guard from the start; the balance,
// package, catalogue and probe paths need the same one, because the panel
// cancels the reads in flight every time it redraws.
//
// Only context.Canceled counts.  Every deadline this module sets is its own
// WithTimeout child, and a child that runs out reports DeadlineExceeded -- that
// one IS evidence about the credential (it did not answer in time), so it still
// has to park the account.  A child reports Canceled only when an ancestor was
// cancelled, so Canceled is exactly the caller-walked-away signal.
func callerGone(err error) bool {
	return errors.Is(err, context.Canceled)
}

// redactErr renders an error for a log line or a panel string, with any secret
// material masked.
func redactErr(err error) string {
	if err == nil {
		return ""
	}
	return core.Redact(err.Error())
}

// truncate bounds an upstream body before it is echoed into an error message.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
