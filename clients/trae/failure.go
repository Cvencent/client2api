package trae

// failure.go — trae's opt-in to the shared error-classification contract
// (internal/core/failure.go).
//
// The module used to hand the gateway a bare *trae.Error.  core treats a plain
// error as unclassified: it is never rotated, never attributed to an account,
// never fed to the egress-IP WAF gate, and writeUpstreamError always renders it
// as a generic 502 upstream_error.  This file maps trae's own ErrKind taxonomy
// onto core.FailureKind and wraps the terminal error, so that the machinery in
// internal/gateway/server.go (openStream / writeUpstreamError) actually fires.
//
// The mapping is chosen so the documented failover set survives the trip:
//
//	vendor code / cause      trae ErrKind        core.FailureKind
//	1005 plan limit          ErrPlanLimit        FailureQuota
//	4008 quota               ErrQuota            FailureQuota
//	1001 / 4010 auth         ErrAuth             FailureAuth
//	401 session dead         ErrSessionDead      FailureSessionDead
//	429 / 4011 rate limit    ErrSoftRate         FailureRateLimited
//	9074 checkin contention  ErrRetryLater       FailureRateLimited
//	5xx / network            ErrServer/Transport FailureUpstream
//	4001 / 4023 param        ErrParam            FailureOther
//	404 not found            ErrNotFound         FailureOther
//	any other 4xx            ErrClient           FailureOther
//
// There is no WAF / risk-control code in trae's vocabulary (verified by grep),
// so FailureWAF is deliberately never emitted here.

import (
	"errors"

	"client2api/internal/core"
)

// failureKindFor maps one trae ErrKind onto the shared vocabulary.
//
// ErrParam, ErrNotFound and ErrClient land on FailureOther even though they are
// genuine upstream answers: they are request-level, trae's own retryableKind()
// already refuses to rotate them, and core.Retryable excludes FailureOther.
// Mapping them onto FailureUpstream instead would make the gateway start
// rotating errors the module deliberately does not rotate on -- a new retry
// path, which this task must not introduce.
func failureKindFor(kind ErrKind) core.FailureKind {
	switch kind {
	case ErrPlanLimit, ErrQuota:
		return core.FailureQuota
	case ErrSoftRate, ErrRetryLater:
		return core.FailureRateLimited
	case ErrAuth:
		return core.FailureAuth
	case ErrSessionDead:
		return core.FailureSessionDead
	case ErrServer, ErrTransport:
		return core.FailureUpstream
	default:
		return core.FailureOther
	}
}

// classifyTerminal wraps the error trae is about to return in a *core.Failure
// naming the account that produced it, so the gateway can rotate, back off,
// attribute the failure in /v1/status and map a meaningful HTTP status.
//
// account must be the account behind the terminal answer, not the first one
// tried: the loop can have moved on before it ran out of attempts, and blaming
// the first account would cooldown the wrong credential.
//
// An error that is not a *trae.Error -- a context cancellation, a JSON decode
// failure, a local build error -- is returned untouched: there is nothing to
// classify it from, and core treats it as unclassified exactly as before.
func classifyTerminal(account string, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if !errors.As(err, &e) {
		return err
	}
	return core.Fail(clientName, account, failureKindFor(e.Kind), e.Status, err)
}
