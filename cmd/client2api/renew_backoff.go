package main

import (
	"strings"
	"sync"
	"time"
)

// renewBackoff is how long one account stays out of the renewal sweep after
// three consecutive refresh failures (issue #260).
//
// The failure counters the log already carries are honest -- the deduper folds
// the repeated lines -- but they only quiet the LOG.  trae's dead refresh token
// still bought one doomed oauth round trip every five-minute sweep tick for
// eleven days (~900 calls), because the module never writes back a state the
// #48 gate could read: its RefreshAccount returns the terminal error and the
// account stays "ready" forever.  This backoff is the sweep-side answer that
// does not wait for any module: three consecutive failures put the account on
// the hour clock, one success clears it.
const (
	// renewBackoffThreshold is how many consecutive failures of one
	// (client, account) arm the backoff.
	renewBackoffThreshold = 3
	// renewBackoffWindow is how long an armed backoff lasts.
	renewBackoffWindow = time.Hour
)

// renewBackoffTable keeps the per-account consecutive-failure bookkeeping the
// renewal sweep consults before it knocks on the vendor again.  Keyed by
// (client, account) -- the same identity the log deduper uses -- and safe for
// concurrent sweeps, which can overlap on the manual path.
type renewBackoffTable struct {
	mu     sync.Mutex
	seen   map[string]renewBackoffState
	window time.Duration
}

type renewBackoffState struct {
	fails    int
	lastFail time.Time
}

func newRenewBackoffTable() *renewBackoffTable {
	return &renewBackoffTable{seen: map[string]renewBackoffState{}, window: renewBackoffWindow}
}

// backoffKey is the identity of one account's renewal health.
func backoffKey(client, account string) string {
	return strings.Join([]string{client, account}, "\x00")
}

// blocked reports whether this account is inside its backoff window.  A window
// that has lapsed is cleared on the spot: one fresh attempt is allowed to
// re-probe, exactly like the fold backoff's lapse semantics.
func (t *renewBackoffTable) blocked(client, account string, now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.seen[backoffKey(client, account)]
	if !ok || st.fails < renewBackoffThreshold {
		return false
	}
	if now.Sub(st.lastFail) > t.window {
		delete(t.seen, backoffKey(client, account))
		return false
	}
	return true
}

// recordFailure bumps the consecutive-failure counter.  It returns whether the
// threshold was just reached, so the caller logs the arming once instead of
// every tick.
func (t *renewBackoffTable) recordFailure(client, account string, now time.Time) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := backoffKey(client, account)
	st := t.seen[key]
	st.fails++
	st.lastFail = now
	t.seen[key] = st
	return st.fails == renewBackoffThreshold
}

// recordSuccess clears the counter: one served renewal is evidence the vendor
// is answering again.
func (t *renewBackoffTable) recordSuccess(client, account string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, backoffKey(client, account))
}

// renewBackoff is the process-wide table: the renewal sweep runs on the shared
// five-minute tick, and a per-call table would forget a failure between sweeps,
// which is exactly the probeLogDeduper lifecycle bug (issue #7).
var (
	renewBackoffOnce sync.Once
	renewBackoff     *renewBackoffTable
)

func sharedRenewBackoff() *renewBackoffTable {
	renewBackoffOnce.Do(func() {
		renewBackoff = newRenewBackoffTable()
	})
	return renewBackoff
}
