package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// errors.go turns the vendor's two failure vocabularies -- a business code in a
// 200 response, and an HTTP status -- into one type, and then into the
// contract's FailureKind.

// Business codes.  Loomy answers HTTP 200 for business failures, so a code is
// the only thing that distinguishes success from a refusal.
const (
	loomyOKCode = "000000"
	// loomyAuthErrorCode is the business gateway's "login invalid" code.
	loomyAuthErrorCode = "100002"
	// loomyAuthErrorCodeCAccount is the account service's own "login invalid"
	// code.  The two hosts validate the same session with different
	// vocabularies, and the vendor's desktop client treats either as a dead
	// session (AUTH_ERROR_CODES = {'020002','100002'} in its account-service
	// module), so this module must accept both.
	loomyAuthErrorCodeCAccount = "020002"
	loomyBadRequestCode        = "100001"
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
		e.Code == loomyAuthErrorCodeCAccount ||
		e.Status == http.StatusUnauthorized ||
		e.Status == http.StatusForbidden
}

// promptTooLongMarkers are the vendor's own ways of saying "the assembled
// prompt is over the limit this model will serve".
//
// The Chinese pair is the wording the backend behind DeepSeek-V4.1-Flash
// actually uses: a 400 whose body is
// {"error":{"message":"内容太长了，请精简输入或开启新对话",
// "metadata":{"provider_name":"alibaba-dashscope"}}}.  It names no token count,
// so no window can be learned from it, but it is still a request-shape refusal:
// the credential is healthy, and rotating to the next one only sends the same
// oversized body again.
//
// The English markers are the spellings the same family of backends uses when
// the account's locale is not Chinese.  They are deliberately narrow: a bare
// "too long" would also match "the request took too long".
var promptTooLongMarkers = []string{
	"内容太长",
	"请精简输入",
	"开启新对话",
	"prompt is too long",
	"context length exceeded",
	"context_length_exceeded",
	"maximum context length",
}

// wireUnicodeEscapeRE matches a JSON \uXXXX escape inside an upstream body.
var wireUnicodeEscapeRE = regexp.MustCompile(`(?i)\\u([0-9a-f]{4})`)

// unescapeWireEscapes decodes \uXXXX sequences so a vendor that escaped its own
// Chinese refusal on the wire is still recognised.  Anything that is not a
// valid escape is left exactly as it was.
func unescapeWireEscapes(s string) string {
	if !strings.Contains(s, `\u`) {
		return s
	}
	return wireUnicodeEscapeRE.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.ParseUint(m[2:], 16, 32)
		if err != nil {
			return m
		}
		return string(rune(n))
	})
}

// promptTooLong reports whether this refusal is the vendor saying the assembled
// prompt is larger than the model will serve.
func (e *apiError) promptTooLong() bool {
	if e == nil {
		return false
	}
	msg := strings.ToLower(unescapeWireEscapes(e.Message))
	for _, marker := range promptTooLongMarkers {
		if strings.Contains(msg, strings.ToLower(marker)) {
			return true
		}
	}
	return false
}

// isTransportFailure reports whether err describes the connection rather than
// the vendor's answer about this credential.
//
// The distinction matters because this module's cooldown ladder ends in a
// PERMANENT "abnormal" park: four failed rounds and the account leaves rotation
// until an operator revives it.  That is the right response to a credential the
// vendor keeps rejecting, and the wrong response to a platform outage -- during
// the 2026-10-29 wedge, 24 healthy sessions were parked this way by connection
// EOFs that said nothing about any of them.
//
// A transport failure carries no HTTP status and no business code, by
// definition: the vendor never answered.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	var api *apiError
	if errors.As(err, &api) {
		return api.Status == 0 && api.Code == ""
	}
	// Anything that is not the module's own typed refusal came from the
	// transport stack (a *url.Error, a TLS error, a context deadline).
	return true
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
		case api.promptTooLong():
			// Another account cannot make the same body smaller, and the
			// gateway needs this kind to answer 400 + context_window_exceeded
			// instead of turning the refusal into a 502 upstream error.
			return core.FailureContextWindow
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

// classifyFailure wraps the one refusal the gateway has to act on: a prompt-size
// rejection, which has to reach the caller as 400 + context_window_exceeded
// carrying the credential it came from.
//
// Every other error is deliberately returned untouched.  The gateway retries a
// module error whose kind core.Retryable reports true by running Chat again,
// and this module already walks its own account pool internally; tagging a
// transport or 5xx failure as retryable here would multiply one request's
// account burn by the gateway's own rotation.  Sentinels (core.ErrNotConfigured,
// core.ErrBusy, core.ErrUnsupported) must keep their shape for the same reason.
func classifyFailure(err error, accountID string) error {
	if err == nil {
		return err
	}
	if _, ok := core.AsFailure(err); ok {
		return err
	}
	var api *apiError
	if errors.As(err, &api) && failureKind(err) == core.FailureContextWindow {
		return core.Fail("loomy", accountID, core.FailureContextWindow, api.Status, err)
	}
	return err
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
