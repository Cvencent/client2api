package gateway

import (
	"sync"
	"time"

	"client2api/internal/core"
)

// InFlightTrackers owns the gateway's in-flight bookkeeping: one slot counter
// per platform and one per (platform, account).  The gateway takes and
// releases slots through it while routing, and the host hands the same
// instance to the panel so the account pool can render, for every platform,
// how many requests an account is carrying right now.  Sharing by reference
// (the way SessionPlatforms is shared) is what makes the panel read the very
// numbers the gateway enforces instead of a copy that can drift.
type InFlightTrackers struct {
	mu sync.Mutex
	// nextSweep is the earliest time idle entries may be compacted.  Account
	// ids can be transient (a pool rebuild or a re-login changes them), so
	// without a sweep one entry per historical credential would stay for the
	// process lifetime.  A live limiter is never evicted.
	nextSweep time.Time
	platform  map[string]*platformLimiter
	account   map[accountKey]*platformLimiter
}

// NewInFlightTrackers builds an empty tracker store.
func NewInFlightTrackers() *InFlightTrackers {
	return &InFlightTrackers{
		platform: map[string]*platformLimiter{},
		account:  map[accountKey]*platformLimiter{},
	}
}

// AccountInFlight reports how many requests the gateway is carrying against
// one account of one platform right now.  This is the number the operator's
// per-account ceiling (max_in_flight_per_account) counts, so it is what the
// account pool's 在途 column should display when a module does not keep an
// in-flight count of its own.
func (t *InFlightTrackers) AccountInFlight(platform, accountID string) int {
	if t == nil || accountID == "" {
		return 0
	}
	t.mu.Lock()
	l := t.account[accountKey{platform: platform, account: accountID}]
	t.mu.Unlock()
	if l == nil {
		return 0
	}
	// used is guarded by the limiter's own mutex: release() runs when a stream
	// closes and never holds t.mu, so reading it under t.mu alone would race
	// with the request path.
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

// PlatformInFlight reports how many requests the gateway is carrying against
// one platform right now.  The platform brake only counts when the operator set
// a ceiling, so an unlimited platform always answers 0.
func (t *InFlightTrackers) PlatformInFlight(name string) int {
	if t == nil || name == "" {
		return 0
	}
	t.mu.Lock()
	l := t.platform[name]
	t.mu.Unlock()
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.used
}

// sweepLocked drops limiter entries that are idle and therefore cannot affect
// an in-flight request.  Callers hold t.mu.
func (t *InFlightTrackers) sweepLocked(now time.Time) {
	if !t.nextSweep.IsZero() && now.Before(t.nextSweep) {
		return
	}
	t.nextSweep = now.Add(limiterSweepInterval)
	for name, l := range t.platform {
		l.mu.Lock()
		idle := l.used == 0
		l.mu.Unlock()
		if idle {
			delete(t.platform, name)
		}
	}
	for key, l := range t.account {
		l.mu.Lock()
		idle := l.used == 0
		l.mu.Unlock()
		if idle {
			delete(t.account, key)
		}
	}
}

// acquirePlatform resolves, refreshes and acquires one platform slot while
// holding t.mu, so an idle sweep cannot detach the limiter between lookup and
// acquisition.  A max of 0 means "no ceiling": the release func is a no-op and
// nothing is counted, exactly the platformLimiter.acquire semantics.
func (t *InFlightTrackers) acquirePlatform(name string, max int) (func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(time.Now())
	l := t.platform[name]
	if l == nil {
		l = &platformLimiter{}
		t.platform[name] = l
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

// acquireAccount resolves, refreshes and acquires one per-account slot while
// holding t.mu.  Unlike the platform brake, the counter runs even when max is
// zero: an unlimited platform still owes the panel a truthful in-flight
// number, and the refusal -- the only thing that needs a ceiling -- simply
// never fires at 0.
func (t *InFlightTrackers) acquireAccount(platform, accountID string, max int) (func(), error) {
	key := accountKey{platform: platform, account: accountID}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(time.Now())
	l := t.account[key]
	if l == nil {
		l = &platformLimiter{}
		t.account[key] = l
	}
	l.mu.Lock()
	l.max = max
	if max > 0 && l.used >= max {
		l.mu.Unlock()
		return nil, core.ErrBusy
	}
	l.used++
	l.mu.Unlock()
	var once sync.Once
	return func() { once.Do(l.release) }, nil
}
