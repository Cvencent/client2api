package core

import (
	"errors"
	"time"
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
	// FailureContextWindow is an upstream refusal because the assembled prompt
	// is larger than the model's serving limit.  Rotating accounts cannot make
	// the same request smaller, but the gateway must preserve the 400 and emit
	// a code that lets the caller trigger conversation compaction.
	FailureContextWindow FailureKind = "context_window_exceeded"
	// FailureOther is anything unclassified.  Never rotated: the gateway
	// cannot know whether a second attempt would help, and the reference's
	// own amplification analysis says guessing is worse than failing.
	FailureOther FailureKind = "other"
	// FailureModelRefused is a model-scoped refusal: this platform or account
	// tier cannot serve THIS model (vendor "model blocked", "not allowed for
	// user type", "service info not found").  The account is healthy, so
	// nothing is parked; the gateway moves the request to the next platform
	// in the chain and records the model as unavailable on the refusing one.
	FailureModelRefused FailureKind = "model_refused"
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
	// RetryAfter, when positive, is the vendor's own Retry-After hint for a
	// rate limit or overload.  It is a hint, not a promise: the gateway caps
	// it before parking anything.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	Err        error         `json:"-"`
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

// RetryAfterOf returns the vendor's Retry-After hint carried by err, or 0 when
// the error is not a classified failure or carries no hint.
func RetryAfterOf(err error) time.Duration {
	if f, ok := AsFailure(err); ok {
		return f.RetryAfter
	}
	return 0
}

// action is what the gateway does with one classified refusal.
//
// It answers two independent questions that used to be answered in two places
// (Retryable in this package, modelRefusal in internal/gateway), which is why
// the same defect had to be fixed twice: a kind added to one list and not the
// other changed behaviour in a way neither file described.
type action struct {
	// rotate: try another account on the SAME platform.  This is the vendor
	// throttling or rejecting this credential, so a sibling credential usually
	// clears it.
	rotate bool
	// nextPlatform: move the request to the next candidate platform.  This is
	// the platform (or the account's tier) refusing this model or this request
	// shape, so another account on the same platform would refuse identically.
	nextPlatform bool
	// park: record a vendor verdict against the account (exhausted, cooling,
	// invalid).  Modules do this themselves; the table records the intent so a
	// reader can see the whole policy in one place.
	park bool
	// why documents the reasoning, so the next person adding a kind does not
	// have to reverse-engineer it from call sites.
	why string
}

// failureActions is the single decision table.  Every FailureKind appears
// exactly once; TestEveryFailureKindHasAnAction fails when a new kind is added
// here without a row, so the policy cannot drift away from the vocabulary.
//
// The three columns are deliberately separate.  A 429 rotates but does not move
// platforms (another account on the same platform will serve it).  A model
// refusal moves platforms but does not rotate (the account is healthy; every
// account on that platform says the same thing).  An exhausted balance parks
// and rotates, because the next account may still have credit.
var failureActions = map[FailureKind]action{
	// The edge blocked the egress IP, not the account.  Retrying anywhere from
	// this IP repeats the block, so the gateway rotates once and feeds the
	// IP-level gate; that gate, not this table, is what stops the rotation.
	FailureWAF: {rotate: true, why: "edge block describes the egress IP; the IP gate ends the rotation"},

	// Vendor throttling.  Same platform, another account.
	FailureRateLimited: {rotate: true, why: "throttling follows the credential, not the platform"},

	// No balance or plan left.  Park the account and try a sibling.
	FailureQuota: {rotate: true, park: true, why: "an empty wallet is account-scoped and worth remembering"},

	// Rejected credential.  The pool may hold a fresher one.
	FailureAuth: {rotate: true, park: true, why: "one rotation is worth it before giving up"},

	// Stale conversation or session id: a new account starts a new session.
	FailureSessionDead: {rotate: true, why: "a fresh account starts a fresh session"},

	// Transport or 5xx.  The platform may be fine on its next attempt.
	FailureUpstream: {rotate: true, why: "transport and 5xx are often transient"},

	// Content policy.  Rotating the same text onto another account is exactly
	// the behaviour that gets a pool flagged; the gateway degrades the prompt
	// and retries once instead.
	FailureContentBlocked: {why: "the vendor objected to the text, not the account; degrade and retry once"},

	// The prompt is bigger than this model's window.  Another account cannot
	// make it smaller, but the request is preserved as a 400 so the caller can
	// compact; the same-platform trim retry is handled separately.
	FailureContextWindow: {why: "the same bytes stay too large anywhere; preserve the 400 for compaction"},

	// Unclassified.  Guessing that a second attempt helps is worse than
	// failing: the reference's amplification analysis says so, and the pool is
	// what pays for a wrong guess.
	FailureOther: {why: "unclassified: retrying on a guess is worse than failing"},

	// This platform cannot serve THIS model for this caller tier ("model
	// blocked", "not allowed for user type", "service info not found").  The
	// account is healthy, so nothing is parked -- but every account here would
	// answer the same, so the request moves to the next platform.
	FailureModelRefused: {nextPlatform: true, why: "model-scoped: the account is healthy, the platform cannot serve this id"},
}

// actionFor returns the table row for k.  A kind the table does not know is
// treated as unclassified rather than as a licence to rotate.
func actionFor(k FailureKind) action {
	if a, ok := failureActions[k]; ok {
		return a
	}
	return failureActions[FailureOther]
}

// Retryable reports whether the gateway should try another account for this
// kind.  Content blocks and unclassified errors are deliberately excluded:
// the reference rotates for transport/account problems and *degrades* for
// content problems, because retrying the same text on a new account is the
// behaviour that gets an account pool flagged in the first place.
func Retryable(k FailureKind) bool {
	return actionFor(k).rotate
}

// RotateOn reports whether another account on the same platform is worth
// trying.  It is Retryable under a name that says which of the two questions
// it answers.
func RotateOn(k FailureKind) bool { return actionFor(k).rotate }

// NextPlatformOn reports whether the request should move to the next candidate
// platform when this kind of refusal comes back.
//
// It replaces the gateway's own modelRefusal predicate, which had grown a
// legacy "FailureOther with status 404" arm (kept from the 0.1.11
// model-not-found fix, before FailureModelRefused existed).  The status is not
// part of the question any more: a kind that means "this model is not served
// here" says so directly, and an unclassified error stays unclassified no
// matter which status it arrived with.
func NextPlatformOn(k FailureKind) bool { return actionFor(k).nextPlatform }

// ParksAccount reports whether this kind is worth recording as a verdict on
// the account (exhausted / cooling / invalid).
func ParksAccount(k FailureKind) bool { return actionFor(k).park }

// FailureActionReason returns the documented reasoning for one kind, for the
// panel and for tests that want to assert the table is complete.
func FailureActionReason(k FailureKind) string { return actionFor(k).why }

// KnownFailureKind reports whether the table has a row for this kind.  It is
// what the completeness test uses: a kind in the vocabulary with no row means
// the policy silently falls back to "unclassified".
func KnownFailureKind(k FailureKind) bool {
	_, ok := failureActions[k]
	return ok
}

// FailureKinds returns the vocabulary in a stable order, so a test can walk it.
func FailureKinds() []FailureKind {
	return []FailureKind{
		FailureWAF, FailureRateLimited, FailureQuota, FailureAuth, FailureSessionDead,
		FailureUpstream, FailureContentBlocked, FailureContextWindow, FailureOther,
		FailureModelRefused,
	}
}

// Fail builds a *Failure, filling in the client name when the caller did not.
func Fail(client, account string, kind FailureKind, status int, err error) *Failure {
	return &Failure{Kind: kind, Client: client, Account: account, Status: status, Err: err}
}
