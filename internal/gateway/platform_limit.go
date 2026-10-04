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
	"sync"

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

// limiterFor returns the limiter for a platform, creating it on first use and
// updating its ceiling when the operator reloads the config.
func (s *server) limiterFor(name string) *platformLimiter {
	max := 0
	if s.opts.Registry != nil {
		max = s.opts.Registry.MaxInFlightFor(name)
	}
	s.limMu.Lock()
	defer s.limMu.Unlock()
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
	l.mu.Unlock()
	return l
}

// acquirePlatformSlot takes one in-flight slot for the platform.  When every
// slot is busy it returns core.ErrBusy, which the gateway maps to HTTP 429 with
// a Retry-After, and which the account-rotation loop deliberately does not
// retry (retrying a saturated platform would only deepen the backlog).
func (s *server) acquirePlatformSlot(name string) (func(), error) {
	l := s.limiterFor(name)
	if !l.acquire() {
		return nil, core.ErrBusy
	}
	var once sync.Once
	return func() { once.Do(l.release) }, nil
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

// accountLimiterFor returns the per-account limiter for a platform, creating
// it on first use and refreshing its ceiling from the live policy so the
// operator's edit counts against the very next attempt.
func (s *server) accountLimiterFor(platform, accountID string) *platformLimiter {
	max := 0
	if s.opts.Registry != nil {
		max = s.opts.Registry.MaxInFlightPerAccountFor(platform)
	}
	key := accountKey{platform: platform, account: accountID}
	s.limMu.Lock()
	defer s.limMu.Unlock()
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
	l.mu.Unlock()
	return l
}

// acquireAccountSlot takes one in-flight slot for (platform, account).  When
// the account is at its ceiling it returns core.ErrBusy, and the module then
// picks another account; a platform whose every account is full reports busy.
// An empty account id means the module did not name one, so nothing is gated.
func (s *server) acquireAccountSlot(platform, accountID string) (func(), error) {
	if accountID == "" {
		return func() {}, nil
	}
	l := s.accountLimiterFor(platform, accountID)
	if !l.acquire() {
		return nil, core.ErrBusy
	}
	var once sync.Once
	return func() { once.Do(l.release) }, nil
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
