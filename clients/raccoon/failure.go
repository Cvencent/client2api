package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"client2api/internal/core"
)

// failure.go translates this module's pool vocabulary into the gateway's
// shared failure contract.
//
// The pool's kinds are finer than the contract's: they decide cooldowns inside
// one module, while the contract decides whether the gateway tries another
// account or another platform.  Without this projection the module hands the
// gateway a bare `*url.Error` ("... : EOF"), which carries no kind at all --
// the gateway cannot classify it, so it answers 502 and stops routing.  That is
// how a transient network fault on one platform became a failed task while
// every sibling platform was healthy.

// coreFailureKind projects one pool kind onto the shared vocabulary.
func coreFailureKind(kind errKind) core.FailureKind {
	switch kind {
	case kindQuota:
		return core.FailureQuota
	case kindTransient:
		// 429 and 5xx: throttling or a bad gateway moment, both usually
		// cleared by a sibling account.
		return core.FailureRateLimited
	case kindNetwork:
		// A transport fault (EOF, connection reset, dial timeout).  No verdict
		// about the credential; another platform may still answer.
		return core.FailureUpstream
	case kindAuth:
		return core.FailureSessionDead
	case kindClient:
		// A request-shape refusal: every account and every platform would
		// reject the same body, so it must not rotate.  The status is kept so
		// the caller still sees the vendor's own 4xx.
		return core.FailureOther
	default:
		return core.FailureUpstream
	}
}

// classifyFailure wraps the module's terminal error in the gateway contract so
// the shared decision table can act on it.  An error that is already classified,
// and a caller cancellation, are returned untouched: a cancelled caller must
// still surface as a 504 rather than being mistaken for a vendor verdict.
func classifyFailure(err error, accountID string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := core.AsFailure(err); ok {
		return err
	}
	kind := coreFailureKind(classifyErr(err))
	status := 0
	var ae *apiError
	if errors.As(err, &ae) {
		status = ae.Status
	} else if se := statusOf(err); se != 0 && se >= http.StatusBadRequest && se <= 599 {
		status = se
	}
	return core.Fail("raccoon", accountID, kind, status, err)
}

// poolExhausted builds the headline for a walk that ended without a serving
// account.
//
// "every usable account on this platform failed" was the sentence the operator
// saw after a single 413: the refusal is about the request body, not the
// account, so the walk stops at the first account and every other account is
// never asked.  Reading "all accounts failed" then sends them hunting a health
// problem that does not exist.  Naming the attempted count -- and, on an early
// stop, the reason the walk went no further -- keeps the sentence true and
// shortens the diagnosis.
func poolExhausted(tried int, siblingFutile bool) error {
	if tried == 0 {
		// Nothing was called: the picker found no account to try at all, so
		// there is no count to report and no account-level refusal to explain.
		return core.ErrPlatformExhausted
	}
	count := fmt.Sprintf("%d accounts", tried)
	if tried == 1 {
		count = "1 account"
	}
	if siblingFutile {
		return fmt.Errorf("%w after %s: this refusal is not account-specific, so the other accounts would answer the same", core.ErrPlatformExhausted, count)
	}
	return fmt.Errorf("%w after %s", core.ErrPlatformExhausted, count)
}
