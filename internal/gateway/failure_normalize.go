package gateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"client2api/internal/core"
)

// normalizeUnclassifiedFailure is the safety net the routing loop needs before
// it decides an error is "unclassified, so give up".
//
// WHY THIS EXISTS.  The shared decision table can only act on a *core.Failure.
// A module that returns a bare transport error -- `*url.Error` wrapping an EOF,
// a TLS reset, a dial timeout -- hands the gateway something with no kind at
// all.  The old routing loop read that as "I cannot tell whether a sibling
// would help" and answered 502 immediately, even when the request's candidate
// chain held healthy platforms behind the broken one.  That is how one vendor's
// flaky edge turned into a failed task while every other platform for the same
// model was serving fine.
//
// The judgement here is deliberately narrow.  Only an error that is provably a
// transport fault is promoted; anything else keeps its existing answer, because
// guessing "retry elsewhere" on a genuine request-shape refusal would multiply
// the same rejected body across every platform and burn the pool.
//
// A classified error is not re-guessed: a module that named a kind knows more
// than the error text does.  The one exception is a payload limit, explained on
// normalizeClassifiedFailure.
func normalizeUnclassifiedFailure(err error) error {
	if err == nil {
		return nil
	}
	if f, ok := core.AsFailure(err); ok {
		return normalizeClassifiedFailure(err, f)
	}
	// A sentinel is already the module's verdict and must keep its shape:
	// ErrBusy is backpressure, ErrNotConfigured a missing credential,
	// ErrUnsupported a request this module cannot express.  The routing loop
	// treats the first two as "skip this platform" on its own.
	if errors.Is(err, core.ErrBusy) || errors.Is(err, core.ErrNotConfigured) || errors.Is(err, core.ErrUnsupported) {
		return err
	}
	// A caller that walked away is not evidence about the platform.
	if errors.Is(err, context.Canceled) {
		return err
	}
	// A deadline is a stall: the platform did not answer in time, which is
	// exactly the case another platform may cover.
	if errors.Is(err, context.DeadlineExceeded) {
		return core.Fail("", "", core.FailureUpstream, 0, err)
	}
	if isTransportFault(err) {
		return core.Fail("", "", core.FailureUpstream, 0, err)
	}
	return err
}

// normalizeClassifiedFailure re-examines a kind a module already chose, for the
// one case where a module's own vocabulary is systematically too coarse: a
// payload limit.
//
// HTTP 413 means "too large for THIS endpoint", and endpoints differ by orders
// of magnitude -- a 32 KiB edge limit sits next to a 10 MiB one.  A module that
// folds 413 into its generic client-error bucket lands on FailureOther, which
// the decision table treats as terminal, so the caller gets a hard error while
// a roomier platform sits next in the chain.  The live log shows exactly that:
// three 413s from openai-compat providers, each 72-114 s in, none of them ever
// trying a sibling.
//
// Raising it to FailureUpstream restores both rotation and failover, and the
// vendor's 413 stays on the Failure so a caller that runs out of candidates
// still sees the real status instead of a generic 502.
//
// Nothing else is rewritten.  A 400/422 request-shape refusal genuinely is
// identical on every platform, and a kind that already names a platform problem
// does not need help.
func normalizeClassifiedFailure(err error, f *core.Failure) error {
	if f == nil || f.Kind != core.FailureOther {
		return err
	}
	if f.Status == http.StatusRequestEntityTooLarge {
		return core.Fail(f.Client, f.Account, core.FailureUpstream, f.Status, err)
	}
	return err
}

// isTransportFault reports whether err came from the network layer rather than
// from the vendor's own answer.
//
// The checks are structural first (the concrete error types Go's HTTP stack
// produces) and only then textual, because a module may wrap the original error
// in its own message.  The textual list is intentionally short and specific: a
// loose match would promote a vendor 4xx whose message happens to contain one
// of these words, and rotating that to another platform is precisely the
// amplification the decision table exists to avoid.
func isTransportFault(err error) bool {
	var ue *url.Error
	if errors.As(err, &ue) {
		return true
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range transportFaultMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// transportFaultMarkers are the spellings this repository's modules have been
// seen to wrap a transport failure into.  Each one is a symptom that cannot be
// produced by a well-formed vendor refusal.
var transportFaultMarkers = []string{
	"connection reset by peer",
	"connection refused",
	"no such host",
	"i/o timeout",
	"tls handshake timeout",
	"unexpected eof",
	"http2: ",
	": eof",
	" eof\"",
	"broken pipe",
}
