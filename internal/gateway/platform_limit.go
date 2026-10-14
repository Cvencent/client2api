package gateway

// platform_limit.go implements the operator's per-platform in-flight ceiling.
//
// The account pools already cap concurrency per account, but a vendor's
// rate-limit is often per session, per process or per egress IP, and it fires
// before the documented quota is touched.  Tabbit is the clearest example: a
// short burst of runs from one browser session answers HTTP 429 with the
// marketing page, no matter how many accounts the pool holds.  The pool cannot
// see that, because every request legitimately picked a different account.
//
// The brake therefore belongs to the gateway, one level above the pools: it
// counts requests that are in flight against a platform as a whole.  A slot is
// taken before the module is called and released when the returned stream is
// closed (or when the call failed before producing one), so a long-lived SSE
// response holds its slot for exactly as long as the vendor is streaming.

import (
	"context"
	"errors"
	"sync"
	"time"

	"client2api/internal/core"
)

// platformLimiter is a counting semaphore for one platform.
type platformLimiter struct {
	mu   sync.Mutex
	max  int
	used int
}

// acquire reports whether a slot was taken.  A max of 0 means "no ceiling".
func (l *platformLimiter) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max <= 0 {
		return true
	}
	if l.used >= l.max {
		return false
	}
	l.used++
	return true
}

func (l *platformLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.used > 0 {
		l.used--
	}
}

// limiterSweepInterval bounds how often the idle limiter maps are compacted.
// Account ids can be transient (a pool rebuild or a re-login changes them), so
// without a sweep, one entry per historical credential would stay for the
// process lifetime. A live limiter is never evicted.
const limiterSweepInterval = 10 * time.Minute

// sweepLimitersLocked drops limiter entries that are idle and therefore
// cannot affect an in-flight request. Callers hold s.limMu.
func (s *server) sweepLimitersLocked(now time.Time) {
	if !s.limiterSweep.IsZero() && now.Before(s.limiterSweep) {
		return
	}
	s.limiterSweep = now.Add(limiterSweepInterval)
	for name, l := range s.limiters {
		l.mu.Lock()
		idle := l.used == 0
		l.mu.Unlock()
		if idle {
			delete(s.limiters, name)
		}
	}
	for key, l := range s.accountLimiters {
		l.mu.Lock()
		idle := l.used == 0
		l.mu.Unlock()
		if idle {
			delete(s.accountLimiters, key)
		}
	}
}

// takePlatformLimiter resolves, refreshes and acquires one platform slot while
// holding limMu, so an idle sweep cannot detach the limiter between lookup and
// acquisition. The returned release func is nil when max is zero.
func (s *server) takePlatformLimiter(name string) (func(), error) {
	max := 0
	if s.opts.Registry != nil {
		max = s.opts.Registry.MaxInFlightFor(name)
	}
	s.limMu.Lock()
	defer s.limMu.Unlock()
	s.sweepLimitersLocked(time.Now())
	if s.limiters == nil {
		s.limiters = map[string]*platformLimiter{}
	}
	l := s.limiters[name]
	if l == nil {
		l = &platformLimiter{}
		s.limiters[name] = l
	}
	l.mu.Lock()
	l.max = max
	if l.max <= 0 {
		l.mu.Unlock()
		return func() {}, nil
	}
	if l.used >= l.max {
		l.mu.Unlock()
		return nil, core.ErrBusy
	}
	l.used++
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.release) }, nil
}

// acquirePlatformSlot takes one in-flight slot for the platform.  When every
// slot is busy it returns core.ErrBusy, which the gateway maps to HTTP 429 with
// a Retry-After, and which the account-rotation loop deliberately does not
// retry (retrying a saturated platform would only deepen the backlog).
func (s *server) acquirePlatformSlot(name string) (func(), error) {
	return s.takePlatformLimiter(name)
}

// releaseStream wraps a stream so its platform slot is returned on Close.  The
// gateway always closes a stream it opened, so this is the single release point
// for both the streaming and the buffered paths.
type releaseStream struct {
	core.Stream
	release func()
	// account, when non-nil, returns the per-account slot the module took
	// through ChatRequest.AcquireAccountSlot.  It is a func so it reads the
	// request's current lease when the stream closes, not when it opened.
	account func()
}

func (r *releaseStream) Close() error {
	err := r.Stream.Close()
	if r.release != nil {
		r.release()
		r.release = nil
	}
	if r.account != nil {
		r.account()
		r.account = nil
	}
	return err
}

// accountKey identifies one account inside one platform.  Platform is part of
// the key so the same account id on two platforms cannot share a budget.
type accountKey struct {
	platform string
	account  string
}

// takeAccountLimiter resolves, refreshes and acquires one per-account slot
// while holding limMu, so an idle sweep cannot detach the limiter between
// lookup and acquisition. The returned release func is nil when max is zero.
func (s *server) takeAccountLimiter(platform, accountID string) (func(), error) {
	max := 0
	if s.opts.Registry != nil {
		max = s.opts.Registry.MaxInFlightPerAccountFor(platform)
	}
	key := accountKey{platform: platform, account: accountID}
	s.limMu.Lock()
	defer s.limMu.Unlock()
	s.sweepLimitersLocked(time.Now())
	if s.accountLimiters == nil {
		s.accountLimiters = map[accountKey]*platformLimiter{}
	}
	l := s.accountLimiters[key]
	if l == nil {
		l = &platformLimiter{}
		s.accountLimiters[key] = l
	}
	l.mu.Lock()
	l.max = max
	if l.max <= 0 {
		l.mu.Unlock()
		return func() {}, nil
	}
	if l.used >= l.max {
		l.mu.Unlock()
		return nil, core.ErrBusy
	}
	l.used++
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.release) }, nil
}

// acquireAccountSlot takes one in-flight slot for (platform, account), after
// checking the operator's per-account model policy.  model is the upstream id
// this attempt will ask the vendor for.
//
// When the account is at its ceiling it returns core.ErrBusy, and the module
// then picks another account; a platform whose every account is full reports
// busy.  A model the operator switched off for this account is reported the
// same way on purpose: another account may still serve it, which is exactly
// what a module does with ErrBusy, and once no account is left the gateway
// fails over to the next platform that offers the model.  An empty account id
// means the module did not name one, so nothing is gated.
func (s *server) acquireAccountSlot(platform, accountID, model string) (func(), error) {
	if accountID == "" {
		return func() {}, nil
	}
	if s.opts.ModelPolicies.IsDisabled(platform, accountID, model) {
		return nil, &modelPolicyRefusal{model: model}
	}
	return s.takeAccountLimiter(platform, accountID)
}

// withPlatformSlot acquires a slot for the platform and returns a release func
// that is safe to call exactly once.  It is used by openStream so both the
// initial candidate and every failover candidate are counted.
func (s *server) withPlatformSlot(ctx context.Context, name string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.acquirePlatformSlot(name)
}

// modelPolicyRefusal is the operator's blacklist refusing one account.  It
// wraps core.ErrBusy on purpose: every module already reacts to ErrBusy by
// picking another account, and a module that later reports "no account could
// serve" must be able to fail over to a platform that still can.  It is a
// distinct type only so the usage row and logs can say "switched off" instead
// of the misleading "local concurrency is full".
type modelPolicyRefusal struct {
	model string
}

func (e *modelPolicyRefusal) Error() string {
	return "model " + e.model + " is switched off for this account by the account model policy"
}

func (e *modelPolicyRefusal) Unwrap() error { return core.ErrBusy }

// IsModelPolicyRefusal reports whether err came from the operator's per-account
// model blacklist rather than a full in-flight slot.  Both are backpressure on
// the wire; only the explanation differs.
func IsModelPolicyRefusal(err error) bool {
	var refusal *modelPolicyRefusal
	return errors.As(err, &refusal)
}
