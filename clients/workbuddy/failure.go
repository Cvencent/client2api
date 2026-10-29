package workbuddy

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"client2api/internal/core"
)

// coreFailureKind projects this module's finer upstream vocabulary onto the
// shared failure contract. Request-shape refusals deliberately land on
// FailureOther: another account would reject the same request, while the
// gateway still needs the upstream status so a 400 does not become a 502.
// Context overflow is the exception because its wire code triggers compaction.
func coreFailureKind(kind ErrKind) core.FailureKind {
	switch kind {
	case ErrHardCredit:
		return core.FailureQuota
	case ErrSoftRate:
		return core.FailureRateLimited
	case ErrSessionDead:
		return core.FailureSessionDead
	case ErrAccountFault:
		return core.FailureAuth
	case ErrWafBlock:
		return core.FailureWAF
	case ErrContentBlocked:
		return core.FailureContentBlocked
	case ErrPromptTooLong:
		return core.FailureContextWindow
	case ErrServer:
		return core.FailureUpstream
	case ErrModelBlocked:
		return core.FailureModelRefused
	case ErrFunctionParams:
		// The request's tool schema did not fit THIS platform's validator.  The
		// account is healthy and another account here would run the same model,
		// so rotate nothing -- but a sibling platform with a more permissive
		// validator is a real chance to serve the turn.  FailureModelRefused is
		// exactly that shape: no rotation, yes next platform.
		return core.FailureModelRefused
	default:
		return core.FailureOther
	}
}

// classifyFailure wraps the module's terminal error in the gateway contract.
// Context cancellation is left untouched so the gateway can still answer 504,
// and an error that is already classified is returned as-is.
func classifyFailure(err error, accountID string) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := core.AsFailure(err); ok {
		return err
	}
	var ue *Error
	if asError(err, &ue) {
		return core.Fail("workbuddy", accountID, coreFailureKind(ue.Kind), ue.Status, err)
	}
	return err
}

// refreshRefusalIsTerminal reports whether a failed token refresh is a verdict
// about the credential rather than a transient fault.
//
// The refresh endpoint answers in three shapes: the vendor's own credential
// rejection (HTTP 401, or a business envelope naming the refresh token), a
// transport or 5xx hiccup, and a well-formed 2xx whose body carries no
// accessToken.  Only the first and third mean "this credential has to be
// re-imported".  A dial timeout or a 502 says nothing about the token, and
// parking a healthy account for an hour over one of those is exactly the
// mistake the module's separate breaker/degrade axes exist to avoid.
//
// This is deliberately not "any refresh error": the panel's refresh button and
// the chat path's on-demand refresh both land here, and both can hit a flaky
// network without the vendor ever having judged the credential.
func refreshRefusalIsTerminal(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if asError(err, &ue) {
		switch ue.Kind {
		case ErrSessionDead, ErrAccountFault:
			return true
		}
		// A 401 that the classifier put in some other bucket is still a 401:
		// the credential did not authenticate.  A 403 is not treated this way
		// because WorkBuddy also uses it for edge-WAF blocks and channel
		// refusals, which time clears on its own.
		return ue.Status == http.StatusUnauthorized
	}
	// RefreshToken's "no accessToken in response" error is the vendor handing
	// back an empty grant; its own text names re-login as the remedy.
	return strings.Contains(err.Error(), "re-login required")
}
