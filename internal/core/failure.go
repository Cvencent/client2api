package core

import (
	"errors"
)

// FailureKind classifies an upstream refusal so that the gateway and the
// per-client account pools can agree on what to do with it.
//
// Modules opt in by returning a *Failure from Chat/Models/Refresh.  A plain
// error is treated as unclassified and is never rotated, so a module that has
// not been taught this vocabulary keeps working exactly as before.
type FailureKind string

const (
	// FailureWAF is an edge/WAF block: a 403 carrying no business envelope.
	// It is the only kind that feeds the IP-level fail-fast gate, because a
	// WAF block describes the egress IP and not the account.
	FailureWAF FailureKind = "waf"
	// FailureRateLimited is a soft rate limit (429, or the vendor's own
	// "model busy" code).  Rotating to another account usually clears it.
	FailureRateLimited FailureKind = "rate_limited"
	// FailureQuota means the account's balance or plan is exhausted.
	FailureQuota FailureKind = "quota"
	// FailureAuth is a rejected credential; one rotation is still worthwhile
	// because the pool may hold a fresher account.
	FailureAuth FailureKind = "auth"
	// FailureSessionDead is an invalidated conversation or session id.
	FailureSessionDead FailureKind = "session_dead"
	// FailureUpstream is a transport or 5xx failure.
	FailureUpstream FailureKind = "upstream"
	// FailureContentBlocked is an upstream content-policy refusal.  Rotating
	// would not help -- the content is what the vendor objected to -- so the
	// gateway triggers the degraded-prompt window and retries once instead.
	FailureContentBlocked FailureKind = "content_blocked"
	// FailureOther is anything unclassified.  Never rotated: the gateway
	// cannot know whether a second attempt would help, and the reference's
	// own amplification analysis says guessing is worse than failing.
	FailureOther FailureKind = "other"
)

// Failure is a classified upstream error.
//
// Modules should return one from their Client methods whenever they can name
// the account that failed, so that the gateway can rotate, back off, feed the
// IP-level WAF gate and attribute the failure in /v1/status.  Wrapping with
// fmt.Errorf("%w", ...) is fine: the gateway uses errors.As.
type Failure struct {
	Kind    FailureKind `json:"kind"`
	Client  string      `json:"client,omitempty"`
	Account string      `json:"account,omitempty"`
	Status  int         `json:"status,omitempty"`
	Err     error       `json:"-"`
}

func (f *Failure) Error() string {
	if f == nil {
		return "<nil>"
	}
	if f.Err != nil {
		return f.Err.Error()
	}
	return string(f.Kind) + " failure"
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// AccountID lets the gateway attribute a failure without knowing the module.
func (f *Failure) AccountID() string {
	if f == nil {
		return ""
	}
	return f.Account
}

// AccountIDer is the optional interface an error implements when it knows
// which account caused it.  It exists separately from *Failure so that a
// module can participate in attribution without adopting FailureKind.
type AccountIDer interface {
	AccountID() string
}

// ErrorAccountID returns the account named by err, or "" when it is unknown.
func ErrorAccountID(err error) string {
	if err == nil {
		return ""
	}
	var a AccountIDer
	if errors.As(err, &a) {
		return a.AccountID()
	}
	return ""
}

// ErrorClient returns the module named by err, or "" when it is unknown.
func ErrorClient(err error) string {
	if f, ok := AsFailure(err); ok {
		return f.Client
	}
	return ""
}

// AsFailure extracts the innermost *Failure from err.
func AsFailure(err error) (*Failure, bool) {
	if err == nil {
		return nil, false
	}
	var f *Failure
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// FailureKindOf reports err's kind, defaulting to FailureOther.
func FailureKindOf(err error) FailureKind {
	if f, ok := AsFailure(err); ok {
		return f.Kind
	}
	return FailureOther
}

// Retryable reports whether the gateway should try another account for this
// kind.  Content blocks and unclassified errors are deliberately excluded:
// the reference rotates for transport/account problems and *degrades* for
// content problems, because retrying the same text on a new account is the
// behaviour that gets an account pool flagged in the first place.
func Retryable(k FailureKind) bool {
	switch k {
	case FailureWAF, FailureRateLimited, FailureQuota, FailureAuth,
		FailureSessionDead, FailureUpstream:
		return true
	case FailureContentBlocked, FailureOther:
		return false
	}
	return false
}

// Fail builds a *Failure, filling in the client name when the caller did not.
func Fail(client, account string, kind FailureKind, status int, err error) *Failure {
	return &Failure{Kind: kind, Client: client, Account: account, Status: status, Err: err}
}
