package lobsterai

import (
	"sync"
	"time"
)

// rate_guard.go is the per-account request-rate guard (issue #179): a
// consumer-grade account is not an API throughput port.  The vendor's risk
// model reads call *frequency* -- a scripted burst (168 chats in two hours)
// is exactly the shape a personal-use account is not supposed to produce --
// and in_flight_limit only bounds how many run at once, not how many per
// window.
//
// The guard is a rolling window per account.  When the window is full the
// account simply stops being selectable for this tick: the pool hands the
// request to a sibling, and when no sibling has room either the request
// surfaces as core.ErrBusy (a 429 that tells the client to come back) --
// never a failure, never a cooldown row, never a "账号异常" the operator
// would chase.  Normal conversational traffic (a few requests per minute,
// one account) cannot reach the default cap.

const (
	// defaultRateWindow is the rolling window.  Ten minutes is long enough
	// that a scripted loop cannot ride out it by waiting one tick.
	defaultRateWindow = 10 * time.Minute
	// defaultRateCap is conservative: ~2 chats a minute sustained, well above
	// a human conversation and far below a script.
	defaultRateCap = 20
)

// rateGuard tracks per-account chat timestamps inside the window.
type rateGuard struct {
	mu      sync.Mutex
	window  time.Duration
	cap     int
	records map[string][]time.Time
}

func newRateGuard(cap int) *rateGuard {
	if cap <= 0 {
		cap = defaultRateCap
	}
	return &rateGuard{window: defaultRateWindow, cap: cap, records: map[string][]time.Time{}}
}

// check reports whether accountID has room for another chat right now.  It
// never records: probing candidates during selection must not consume budget,
// or every trial tick would eat the window (the bug that made the first
// wiring refuse a healthy sibling).
func (g *rateGuard) check(accountID string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.countLocked(accountID, now) < g.cap
}

// record notes one admitted chat.  Only the caller that actually reserved the
// account calls this.
func (g *rateGuard) record(accountID string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.countLocked(accountID, now)
	g.records[accountID] = append(g.records[accountID], now)
}

// countLocked prunes expired stamps and returns the live count.  Callers hold
// the lock.
func (g *rateGuard) countLocked(accountID string, now time.Time) int {
	stamps := g.records[accountID]
	fresh := stamps[:0]
	for _, ts := range stamps {
		if now.Sub(ts) < g.window {
			fresh = append(fresh, ts)
		}
	}
	g.records[accountID] = fresh
	return len(fresh)
}

// used reports how many ticks the account has spent in the current window,
// for the operator's visibility line.
func (g *rateGuard) used(accountID string, now time.Time) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, ts := range g.records[accountID] {
		if now.Sub(ts) < g.window {
			n++
		}
	}
	return n
}
