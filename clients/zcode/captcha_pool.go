package zcode

// captcha_pool.go implements the opt-in prefetch pool for the Aliyun
// verification parameter.
//
// The legacy cache in captcha_browser.go deliberately reuses one parameter
// for captchaParamTTL.  That is useful for a short burst, but it is the wrong
// shape for an agent turn loop: a parameter may only be spent once, and a
// parameter minted tens of seconds ago is closer to the vendor's rejection
// window.  The pool stores one not-yet-used parameter, removes it on take, and
// replenishes in the background.  A prefetch failure is only an optimization
// miss; the next request still mints synchronously.

import (
	"strings"
	"sync"
	"time"
)

const defaultCaptchaPoolTTL = 30 * time.Second

// captchaPool is a one-slot, take-once pool.  Capacity one is intentional:
// requests arrive serially in the multi-step loop this is for, and a deeper
// queue would only make older parameters more likely to expire.
type captchaPool struct {
	mu       sync.Mutex
	cur      captchaParam
	ttl      time.Duration
	inflight bool
}

func newCaptchaPool(ttl time.Duration) *captchaPool {
	if ttl <= 0 {
		ttl = defaultCaptchaPoolTTL
	}
	return &captchaPool{ttl: ttl}
}

// put stores one unused parameter.  Empty values are ignored.  A newer value
// replaces an older one, so a successful synchronous mint can never leave a
// stale prefetched value behind.
func (p *captchaPool) put(param, region string, now time.Time) {
	if p == nil {
		return
	}
	param = strings.TrimSpace(param)
	if param == "" {
		return
	}
	p.mu.Lock()
	p.cur = captchaParam{
		param:   param,
		region:  strings.TrimSpace(region),
		expires: now.Add(p.ttl),
	}
	p.mu.Unlock()
}

// hasFresh is read-only and exists for diagnostics and tests.  It must never
// consume the pooled value.
func (p *captchaPool) hasFresh(now time.Time) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur.param != "" && now.Before(p.cur.expires)
}

// take removes and returns the pooled parameter.  A parameter at its deadline
// is discarded rather than sent upstream.
func (p *captchaPool) take(now time.Time) (captchaParam, bool) {
	if p == nil {
		return captchaParam{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur.param == "" || !now.Before(p.cur.expires) {
		p.cur = captchaParam{}
		return captchaParam{}, false
	}
	got := p.cur
	p.cur = captchaParam{}
	return got, true
}

// invalidate drops the pooled value after the vendor rejected a parameter.
func (p *captchaPool) invalidate() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.cur = captchaParam{}
	p.mu.Unlock()
}

// beginPrefetch reserves the single background refill slot.
func (p *captchaPool) beginPrefetch() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inflight {
		return false
	}
	p.inflight = true
	return true
}

func (p *captchaPool) endPrefetch() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.inflight = false
	p.mu.Unlock()
}

func (p *captchaPool) prefetching() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight
}
