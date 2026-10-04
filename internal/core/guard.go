package core

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Rotation timing
//
// Ported from the reference internal/server/backoff.go, which is the single
// source of truth shared by the rotation backoff and the WAF soft-cooldown
// base.  The numbers are the documented ones: base 500ms (matching the
// official intl CLI's computeRequestRetryDelayMs), cap 8s, and a +/-25%
// jitter so that concurrent retries do not re-converge into the same
// fixed-period burst that the vendor's density heuristic punishes.
// ---------------------------------------------------------------------------

var (
	// RotateBackoffBase is the first inter-attempt wait.
	RotateBackoffBase = 500 * time.Millisecond
	// RotateBackoffCap bounds the exponential growth.
	RotateBackoffCap = 8 * time.Second
	// JitterFraction widens every wait by +/- this fraction.
	JitterFraction = 0.25
	// MaxRotate is how many *extra* attempts a single inbound request may
	// make.  The reference defaults to 3; it is a live-configurable value
	// here because operators tune it per upstream.
	MaxRotate = 3
)

// JitterDur applies a uniform +/- JitterFraction jitter to d.
// d <= 0 is returned unchanged (a zero wait must stay zero).
func JitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*JitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// BackoffAfter returns the wait before rotation attempt n (0-based: the first
// switch happens after attempt 0 fails) using the default base.
func BackoffAfter(n int) time.Duration {
	return BackoffFrom(RotateBackoffBase, n)
}

// BackoffFrom is BackoffAfter with an explicit base, so a caller holding a
// live-configured value does not have to mutate the package default.
//
// The wait is base<<n capped at RotateBackoffCap, then jittered.  Doubling in
// a loop rather than shifting means a caller that lowers the base does not
// have to maintain a shift limit.
func BackoffFrom(base time.Duration, n int) time.Duration {
	d := base
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < RotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // doubled past the integer range: treat as the cap
			return JitterDur(RotateBackoffCap)
		}
	}
	if d > RotateBackoffCap {
		d = RotateBackoffCap
	}
	return JitterDur(d)
}

// SleepCtx waits for d, returning false as soon as ctx is done.  A cancelled
// client must not be held for the length of a backoff.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if err := ctx.Err(); err != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// WAF IP-level gate
//
// Ported from the reference internal/server/wafip.go.  A WAF 403 blocks the
// gateway's egress IP rather than one account, so rotating on it multiplies a
// single client request into MaxRotate requests against an IP that is already
// being refused.  The gate therefore counts *distinct* accounts that hit a WAF
// block inside WAFIPWindow and, on reaching WAFIPThreshold, stops rotating
// until the window elapses.  One account repeatedly 403-ing never trips it:
// that is an account-level problem and belongs to the pool's soft cooldown.
//
// The state is per-process and intentionally not persisted: the window is 60s,
// so rebuilding it after a restart costs nothing.
// ---------------------------------------------------------------------------

var (
	// WAFIPWindow is both the judgement window and the activation length.
	WAFIPWindow = 60 * time.Second
	// WAFIPThreshold is the number of distinct accounts that must hit a WAF
	// block inside the window.  Two is the minimum meaningful definition of
	// "more than one account"; the reference measured three accounts blocked
	// within one second, so two stops the bleeding one rotation earlier.
	WAFIPThreshold = 2
)

// IPGate is the WAF IP-level fail-fast state machine.  The zero value is not
// usable because logf is nil-checked; build it with NewIPGate.
type IPGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time
	until time.Time
	logf  func(string, ...any)
}

// NewIPGate builds a gate that logs activations through logf (may be nil).
func NewIPGate(logf func(string, ...any)) *IPGate {
	return &IPGate{hits: map[string]time.Time{}, logf: logf}
}

// NoteWAF records one WAF block for account uid and reports whether the
// IP-level gate is (now) active.  A caller that gets true must stop rotating.
//
//   - already active: does not renew and does not record (the reference's
//     deliberate "natural expiry" semantics) -> true;
//   - otherwise: records uid (a repeat hit from the same account overwrites,
//     because the criterion is the number of *distinct* accounts), prunes
//     hits older than the window, and activates for WAFIPWindow once the
//     threshold is reached.  Activation clears the window so that a later
//     block needs a fresh set of distinct accounts.
func (g *IPGate) NoteWAF(uid string) bool {
	if g == nil {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if now.Before(g.until) {
		return true
	}
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	g.hits[uid] = now
	for u, t := range g.hits {
		if now.Sub(t) > WAFIPWindow {
			delete(g.hits, u)
		}
	}
	if len(g.hits) >= WAFIPThreshold {
		g.until = now.Add(WAFIPWindow)
		if g.logf != nil {
			g.logf("warn: waf ip-level block: %d accounts hit waf 403 within %s, rotate fail-fast until %s",
				len(g.hits), WAFIPWindow, g.until.Format(time.RFC3339))
		}
		g.hits = map[string]time.Time{}
		return true
	}
	return false
}

// Active reports whether the IP-level gate is currently open.
func (g *IPGate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Until returns the activation deadline (zero when never activated).
func (g *IPGate) Until() time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until
}

// Reset clears the gate.  Used by tests and by an operator action.
func (g *IPGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits = map[string]time.Time{}
	g.until = time.Time{}
}

// ---------------------------------------------------------------------------
// Degrade gate
//
// Ported from the reference internal/server/degrade.go.  A content-policy
// refusal is usually a false positive triggered by the fingerprint sentences a
// vendor CLI injects, so the mitigation is not to retry the same text on
// another account but to fall back to the minimal neutral system prompt -- the
// same philosophy as the body sanitiser, one layer deeper.  The window lasts
// until the next 00:00 CST so that the fallback cannot become permanent
// through repeated triggering.
// ---------------------------------------------------------------------------

// DegradeGate is the degraded-prompt state machine.  The zero value is usable.
type DegradeGate struct {
	mu       sync.Mutex
	until    time.Time
	triggers int
}

// Active reports whether the degraded prompt should be used right now.
func (g *DegradeGate) Active() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger opens the degraded window until the next 00:00 CST.  A trigger
// inside an open window does not renew it, so the reset stays anchored to the
// first trigger's midnight.
func (g *DegradeGate) Trigger() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		g.until = NextMidnightCST(time.Now())
	}
	g.triggers++
}

// Until returns the window's end (zero when never triggered).
func (g *DegradeGate) Until() time.Time {
	if g == nil {
		return time.Time{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.until
}

// Triggers reports how many times the window was opened since start.
func (g *DegradeGate) Triggers() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.triggers
}

// Reset closes the window (operator action / tests).
func (g *DegradeGate) Reset() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.until = time.Time{}
}

// NextMidnightCST returns the first Asia/Shanghai 00:00 strictly after now.
// A fixed +08:00 offset is used so the result does not depend on the host's
// timezone configuration.
//
// Boundary semantics: 23:59 -> the next day's 00:00; exactly 00:00 -> the day
// after, since the current one is not strictly after now.
func NextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}

// ---------------------------------------------------------------------------
// Guard
// ---------------------------------------------------------------------------

// Guard bundles the process-level state that is deliberately *not* owned by
// any single account: the egress-IP WAF gate and the degraded-prompt window.
// It is built once in main and handed to every module through Deps so that all
// clients share one view of the egress IP.
type Guard struct {
	IP      *IPGate
	Degrade *DegradeGate
}

// NewGuard builds a Guard whose activations are logged through logf.
func NewGuard(logf func(string, ...any)) *Guard {
	return &Guard{IP: NewIPGate(logf), Degrade: &DegradeGate{}}
}

// ReportWAF feeds a WAF block from account uid into the IP gate and reports
// whether rotation must stop.
func (g *Guard) ReportWAF(uid string) bool {
	if g == nil {
		return false
	}
	return g.IP.NoteWAF(uid)
}

// ReportContentBlock opens the degraded window.
func (g *Guard) ReportContentBlock() {
	if g == nil {
		return
	}
	g.Degrade.Trigger()
}

// ContentBlocked reports whether the degraded window is open.  This is the read
// side of ReportContentBlock, for a module that rewrites its own system prompt:
// the reference keeps serving the minimal prompt for the rest of the window,
// not only for the one retry that opened it, so the module has to be able to
// ask rather than wait to be told.
func (g *Guard) ContentBlocked() bool {
	if g == nil {
		return false
	}
	return g.Degrade.Active()
}

// Degrader is an optional capability: a module that can rewrite its own
// request to use the minimal degraded system prompt.
//
// The gateway owns the decision (a content-policy refusal triggers the window
// and earns exactly one retry) while the module owns the rewrite, because only
// the module knows where its vendor expects the system prompt to live.  A
// module that does not implement Degrader is never retried on a content block,
// which is the correct default: resending identical text cannot succeed.
type Degrader interface {
	// Degrade rewrites req in place and reports whether it changed anything.
	Degrade(req *ChatRequest) bool
}

// AsDegrader reports whether c can rewrite a content-refused request.
func AsDegrader(c Client) (Degrader, bool) {
	d, ok := c.(Degrader)
	return d, ok
}

// GuardSnapshot is the JSON view of the guard served by /v1/status and the
// panel overview.
type GuardSnapshot struct {
	WAFActive     bool   `json:"waf_active"`
	WAFUntil      string `json:"waf_until,omitempty"`
	DegradeActive bool   `json:"degrade_active"`
	DegradeUntil  string `json:"degrade_until,omitempty"`
	DegradeHits   int    `json:"degrade_triggers"`
}

// Snapshot renders the guard's state for the panel and status endpoints.
func (g *Guard) Snapshot() GuardSnapshot {
	if g == nil {
		return GuardSnapshot{}
	}
	s := GuardSnapshot{
		WAFActive:     g.IP.Active(),
		DegradeActive: g.Degrade.Active(),
		DegradeHits:   g.Degrade.Triggers(),
	}
	if u := g.IP.Until(); !u.IsZero() {
		s.WAFUntil = u.Format(time.RFC3339)
	}
	if u := g.Degrade.Until(); !u.IsZero() {
		s.DegradeUntil = u.Format(time.RFC3339)
	}
	return s
}
