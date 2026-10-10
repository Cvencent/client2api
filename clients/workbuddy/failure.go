package workbuddy

import (
	"context"
	"errors"

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
