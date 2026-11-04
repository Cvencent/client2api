package zcode

import (
	"sync"
	"time"
)

// waf_backoff.go is the per-account circuit breaker for repeated WAF
// judgements (3012 / risk-control 405), issue #198.
//
// Why it exists.  The IP-level WAF gate (internal/core/guard.go IPGate)
// counts *distinct* accounts inside a 60s window -- "two is the minimum
// meaningful definition of more than one account".  On a single-available-
// account platform that threshold can never be met: zcode's other credential
// was exhausted, so 27 WAF judgements in seven hours all came from the one
// account, and every one of them paid the full toll first (a captcha mint --
// an Edge process through CDP -- then the 3012 round trip) at zero success
// probability.  Each attempt also fired a conspicuous automation signal from
// this IP at a vendor whose risk window was already closed against it.
//
// The breaker counts consecutive WAF judgements per account.  Three in a row
// park the account in a WAF-shaped cooldown that starts at 30 minutes (the
// historical length of the vendor's risk windows) and doubles per extra
// judgement up to a 4-hour ceiling -- the vendor's window does expire (the
// 10/30 incident recovered on its own), so the probe must always be able to
// reach it.  A cooldown expiry admits one probe implicitly (the account is
// simply selectable again); another 3012 doubles from where the last cooldown
// left off.  A 200 clears the streak completely.
//
// Not counted: ordinary 4xx/5xx, auth failures, quota exhaustion -- only
// kindRisk (the WAF-shaped verdict) feeds this gate, and only a real 200
// resets it.

const (
	// wafBackoffThreshold is how many consecutive WAF judgements trip the
	// breaker for one account.
	wafBackoffThreshold = 3
	// wafBackoffBase is the first WAF-shaped cooldown once the threshold is
	// reached.  The vendor's risk windows historically last tens of minutes.
	wafBackoffBase = 30 * time.Minute
	// wafBackoffMax caps the doubling.  Four hours: long enough to stop
	// hammering a closed window, short enough that expiry is probed the same
	// day.
	wafBackoffMax = 4 * time.Hour
	// wafBackoffWindow bounds "consecutive": judgements further apart than
	// this do not accumulate into a streak (an isolated 3012 days ago says
	// nothing about now).
	wafBackoffWindow = 10 * time.Minute
)

// wafBackoff is one account's consecutive-WAF-judgement counter with
// exponential cooldown.  One instance per account -- the resource it protects
// is the account's standing with the vendor's risk model.
type wafBackoff struct {
	mu     sync.Mutex
	streak int
	last   time.Time
	until  time.Time
}

func newWAFBackoff() *wafBackoff { return &wafBackoff{} }

// remaining reports how much longer the account is parked for WAF reasons.
// Zero means a request may proceed (a probe, once the deadline lapses).
func (b *wafBackoff) remaining(now time.Time) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until.IsZero() {
		return 0
	}
	remain := b.until.Sub(now)
	// Read-only on purpose (issue #198): the probe-detection in noteWAF keys
	// on "a deadline lapsed", so a caller that merely ASKS whether the gate
	// is open must not clear the deadline -- selection probes happen many
	// times before the probe request itself runs.
	if remain <= 0 {
		return 0
	}
	return remain
}

// noteSuccess clears the streak and any cooldown: a real 200 is the strongest
// evidence the vendor's window has expired.
func (b *wafBackoff) noteSuccess(now time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.streak = 0
	b.last = time.Time{}
	b.until = time.Time{}
	b.mu.Unlock()
}

// noteWAF records one WAF judgement and returns the resulting cooldown (zero
// when under the threshold).  The streak semantics have two halves:
//
//   - judgements inside wafBackoffWindow accumulate (a scripted loop's hits
//     are one story);
//   - the FIRST judgement after a cooldown lapse continues the previous
//     streak -- that request is the probe the design promises, and its
//     failure is exactly the "another 3012" that doubles (issue #198).
//     Without this, the 10-minute window would erase the streak during the
//     30-minute cooldown and the escalation would never leave the base.
//
// A judgement with no cooldown in between and outside the window starts fresh
// (an isolated 3012 days ago says nothing about now).
func (b *wafBackoff) noteWAF(now time.Time) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	continuedProbe := false
	if !b.until.IsZero() && now.After(b.until) {
		// The deadline lapsed: this is the probe.  Keep the streak, clear the
		// deadline so remaining() reads 0 from here on.
		b.until = time.Time{}
		continuedProbe = true
	}
	if !continuedProbe && !b.last.IsZero() && now.Sub(b.last) > wafBackoffWindow {
		b.streak = 0
	}
	b.last = now
	b.streak++
	if b.streak < wafBackoffThreshold {
		return 0
	}
	cooldown := wafBackoffBase
	for i := b.streak - wafBackoffThreshold; i > 0 && cooldown < wafBackoffMax; i-- {
		cooldown *= 2
	}
	if cooldown > wafBackoffMax {
		cooldown = wafBackoffMax
	}
	b.until = now.Add(cooldown)
	return cooldown
}
