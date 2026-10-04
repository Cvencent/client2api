package workbuddy

import (
	"strconv"
	"strings"
	"time"
)

// Promotion scheduling.  The upstream runs more than one campaign per model at
// the same time — a badge-only one during the day and a half-price one at night
// — so a promotion is only "in force" when its schedule says so.  Without this
// the panel would advertise the night discount at noon.

// promoZone is the promotion timezone.  The upstream always quotes Asia/Shanghai
// (UTC+8, no daylight saving), so a FixedZone is used instead of
// time.LoadLocation: Windows ships no IANA database and LoadLocation fails
// there.
var promoZone = time.FixedZone("CST", 8*3600)

// promoClock parses "HH:MM" into a minute of the day.  A malformed value returns
// (-1, false) so the caller skips the window rather than treating it as
// midnight.
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// promoActive reports whether p is in force at now: enabled, inside
// validFrom/validUntil, and inside at least one daily window.  A window may
// cross midnight (23:00→7:50).  A nil schedule means "every day, all day".
func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if p == nil || !p.Enabled {
		return false
	}
	sc := p.Schedule
	if sc == nil {
		return true
	}
	if sc.ValidFrom != "" {
		if from, err := time.Parse(time.RFC3339, sc.ValidFrom); err == nil && now.Before(from) {
			return false
		}
	}
	if sc.ValidUntil != "" {
		if until, err := time.Parse(time.RFC3339, sc.ValidUntil); err == nil && !now.Before(until) {
			return false
		}
	}
	if len(sc.Daily) == 0 {
		return true
	}
	cur := now.Hour()*60 + now.Minute()
	for _, w := range sc.Daily {
		st, ok1 := promoClock(w.Start)
		ed, ok2 := promoClock(w.End)
		if !ok1 || !ok2 {
			continue
		}
		if st <= ed {
			if cur >= st && cur < ed {
				return true
			}
			continue
		}
		// The window wraps past midnight, e.g. 23:00→7:50.
		if cur >= st || cur < ed {
			return true
		}
	}
	return false
}
