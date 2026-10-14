package zcode

// captcha_backoff.go is the mint-failure circuit breaker for the built-in
// browser/captcha_command path.
//
// Why it exists.  Minting a verification parameter is the one step in this
// module that spends a device-level resource: Aliyun scores the machine's
// reputation and caps how many parameters one device may ask for in an hour.
// A request that is already doomed -- quota exhausted for the day, a model
// parked, a vendor outage -- still runs the normal retry loop, and every
// retry used to mint a fresh parameter.  The session that motivated this file
// burned twelve parameters on six requests that could not have succeeded, and
// a second process on the same machine competed for the same device quota at
// the same time.  Continuing to mint does not restore reputation; it only
// deepens the deficit.
//
// The gate counts only *produce* failures: the browser not answering, the
// widget not returning a parameter, captcha_command exiting non-zero.  A
// parameter the vendor later refuses (upstream 3007) is deliberately NOT
// counted, because that may be a vendor-side wobble rather than a statement
// about this device -- that decision is what keeps a transient 3007 from
// tripping the breaker.  A successful mint clears the streak.
//
// Values follow the reference implementation measured against the same
// upstream: three consecutive failures trip the breaker, the first cooldown is
// one minute, and it doubles per extra failure up to a half-hour ceiling so a
// long outage never turns into a permanent ban.

import (
	"sync"
	"time"
)

const (
	// captchaBackoffThreshold is how many consecutive produce failures trip
	// the breaker.  Below it a mint is still attempted normally.
	captchaBackoffThreshold = 3
	// captchaBackoffBase is the first cooldown once the threshold is reached.
	captchaBackoffBase = time.Minute
	// captchaBackoffMax caps the doubling so the gate always recovers.
	captchaBackoffMax = 30 * time.Minute
)

// captchaBackoff is a consecutive-failure counter with exponential cooldown.
// It is stateful and shared by every account: the resource it protects is the
// device, not a credential, so one instance on the client is correct.
type captchaBackoff struct {
	mu     sync.Mutex
	streak int
	until  time.Time
}

func newCaptchaBackoff() *captchaBackoff { return &captchaBackoff{} }

// remaining reports how much longer the gate is closed.  Zero means a mint may
// proceed.  The deadline is cleared when it lapses but the streak is kept, so
// the next failure doubles from where the last cooldown left off rather than
// restarting at one minute.
func (b *captchaBackoff) remaining(now time.Time) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until.IsZero() {
		return 0
	}
	remain := b.until.Sub(now)
	if remain <= 0 {
		b.until = time.Time{}
		return 0
	}
	return remain
}

// noteSuccess clears the streak and any cooldown.  Called when a mint produced
// a parameter, whether or not the vendor later accepted it.
func (b *captchaBackoff) noteSuccess() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.streak = 0
	b.until = time.Time{}
	b.mu.Unlock()
}

// noteFailure records one produce failure and returns the resulting cooldown
// (zero when under the threshold).  The exponent is `streak - threshold` so the
// third failure waits exactly the base interval instead of base times a power.
func (b *captchaBackoff) noteFailure(now time.Time) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.streak++
	if b.streak < captchaBackoffThreshold {
		return 0
	}
	cooldown := captchaBackoffBase
	for i := b.streak - captchaBackoffThreshold; i > 0 && cooldown < captchaBackoffMax; i-- {
		cooldown *= 2
	}
	if cooldown > captchaBackoffMax {
		cooldown = captchaBackoffMax
	}
	b.until = now.Add(cooldown)
	return cooldown
}
