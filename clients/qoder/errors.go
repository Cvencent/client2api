package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"client2api/internal/core"
)

// errors.go turns the vendor's failure vocabulary into one type and then into
// the contract's FailureKind.
//
// Qoder's OpenAPI answers with several envelope shapes depending on the route:
// the classic `{success,msgCode,msgInfo,message}` object, a bare `{code,message}`,
// and the campaigns service's `{error_code,error_message,details}`.  Rather than
// model all three, this module reads whichever key is present and keeps the HTTP
// status as the primary signal.

// apiError is one failed exchange with the vendor.
type apiError struct {
	// Code is the vendor's business code, when the body carried one.
	Code string
	// Status is the HTTP status; 0 when no response was ever received.
	Status int
	// Message is the vendor's own text where there is one.
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Status != 0 && e.Message != "":
		return fmt.Sprintf("qoder: upstream HTTP %d: %s", e.Status, e.Message)
	case e.Status != 0 && e.Code != "":
		return fmt.Sprintf("qoder: upstream HTTP %d (%s)", e.Status, e.Code)
	case e.Status != 0:
		return fmt.Sprintf("qoder: upstream HTTP %d", e.Status)
	case e.Code != "" && e.Message != "":
		return "qoder: upstream " + e.Code + ": " + e.Message
	case e.Code != "":
		return "qoder: upstream " + e.Code
	case e.Message != "":
		return "qoder: " + e.Message
	}
	return "qoder: upstream failure"
}

// unauthorized reports whether the vendor rejected the bearer token.
func (e *apiError) unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// failureKind maps an error onto the contract's failure vocabulary.
func failureKind(err error) core.FailureKind {
	var api *apiError
	if errors.As(err, &api) {
		switch {
		case api.unauthorized():
			return core.FailureAuth
		case api.Status == http.StatusTooManyRequests:
			return core.FailureRateLimited
		case api.Status == http.StatusPaymentRequired:
			return core.FailureQuota
		case api.Status >= 500:
			return core.FailureUpstream
		}
		return core.FailureUpstream
	}
	return core.FailureUpstream
}

// envelope keys this module understands.  A body is only parsed for a message
// and a code; the HTTP status is what classifies the failure.
func decodeErrorMessage(payload []byte) (code, message string) {
	var fields map[string]json.RawMessage
	if len(payload) == 0 || json.Unmarshal(payload, &fields) != nil {
		return "", ""
	}
	message = firstScalar(fields, "error_message", "message", "msgInfo", "msg", "errorMessage", "detail")
	code = firstScalar(fields, "error_code", "code", "msgCode", "errCode", "status")
	return code, message
}

func firstScalar(fields map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			return s
		}
		var n json.Number
		if err := json.Unmarshal(raw, &n); err == nil && n.String() != "" {
			return n.String()
		}
	}
	return ""
}

// callerGone reports whether a request failed because the caller walked away
// rather than because of anything the vendor said.
//
// Only context.Canceled counts.  Every deadline this module sets is its own
// WithTimeout child, and a child that runs out reports DeadlineExceeded -- that
// one IS evidence about the credential, so it still parks the account.
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
