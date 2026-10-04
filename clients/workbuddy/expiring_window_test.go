package workbuddy

import (
	"testing"
	"time"
)

// TestSetExpiringSoonClearsOnlyOnChange pins the reload guard: a changed
// window invalidates every cached expiry bucket, while an unchanged reload must
// leave the pool alone.
func TestSetExpiringSoonClearsOnlyOnChange(t *testing.T) {
	a := &Auth{AccessToken: "at-111111111111", UID: "u-expiring"}
	p := reserveTestPool(t, a)
	p.SetCreditsDetailed(a, 100, 5000, 40, time.Now().Add(time.Hour), 40)
	if got := p.entries[0].creditsExpiring; got != 40 {
		t.Fatalf("fixture expiring = %d, want 40", got)
	}

	if changed := p.SetExpiringSoon(24 * time.Hour); !changed {
		t.Fatal("the first window install must count as a change")
	}
	if got := p.entries[0].creditsExpiring; got != 0 {
		t.Fatalf("expiring after a window change = %d, want the stale snapshot cleared", got)
	}

	p.SetCreditsDetailed(a, 100, 5000, 40, time.Now().Add(time.Hour), 40)
	if changed := p.SetExpiringSoon(24 * time.Hour); changed {
		t.Fatal("installing the same window must not rewrite the pool")
	}
	if got := p.entries[0].creditsExpiring; got != 40 {
		t.Fatalf("expiring after an unchanged reload = %d, want 40 preserved", got)
	}
}
