package openaicompat

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	requestLimitFileVersion = 1
	sourceRequestLimitKey   = "__source__"
)

type requestLimitBlock struct {
	Scope string
	Until time.Time
}

type requestLimitFile struct {
	Version  int                                       `json:"version"`
	Counters map[string]map[string]requestLimitCounter `json:"counters,omitempty"`
}

type requestLimitCounter struct {
	Timestamps []int64 `json:"timestamps,omitempty"`
	Start      int64   `json:"start,omitempty"`
	Used       int     `json:"used,omitempty"`
}

type compiledRequestLimit struct {
	scope     string
	limit     int
	window    time.Duration
	reset     string
	signature string
	location  *time.Location
	resetAt   time.Duration
}

type sourceRequestLimiter struct {
	mu       sync.Mutex
	path     string
	rules    []compiledRequestLimit
	counters map[string]map[string]requestLimitCounter
}

func newSourceRequestLimiter(path string, limits []core.SourceRequestLimit) (*sourceRequestLimiter, error) {
	l := &sourceRequestLimiter{
		path:     path,
		counters: map[string]map[string]requestLimitCounter{},
	}
	if err := l.loadLocked(); err != nil {
		return nil, err
	}
	if err := l.Configure(limits); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *sourceRequestLimiter) loadLocked() error {
	var file requestLimitFile
	err := core.ReadJSON(l.path, &file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading request limit counters: %w", err)
	}
	if file.Version != requestLimitFileVersion {
		return fmt.Errorf("unsupported request limit counter version %d", file.Version)
	}
	if file.Counters != nil {
		l.counters = file.Counters
	}
	return nil
}

func (l *sourceRequestLimiter) Configure(limits []core.SourceRequestLimit) error {
	compiled := make([]compiledRequestLimit, 0, len(limits))
	for i, limit := range limits {
		rule, err := compileRequestLimit(limit)
		if err != nil {
			return fmt.Errorf("request_limits[%d]: %w", i, err)
		}
		compiled = append(compiled, rule)
	}
	l.mu.Lock()
	l.rules = compiled
	l.mu.Unlock()
	return nil
}

func compileRequestLimit(limit core.SourceRequestLimit) (compiledRequestLimit, error) {
	scope := strings.ToLower(strings.TrimSpace(limit.Scope))
	if scope != "account" && scope != "source" {
		return compiledRequestLimit{}, fmt.Errorf("scope must be account or source")
	}
	if limit.Limit <= 0 {
		return compiledRequestLimit{}, fmt.Errorf("limit must be > 0")
	}
	window, err := time.ParseDuration(strings.TrimSpace(limit.Window))
	if err != nil || window < time.Second {
		return compiledRequestLimit{}, fmt.Errorf("window must be at least 1s")
	}
	reset := strings.ToLower(strings.TrimSpace(limit.Reset))
	if reset == "" {
		reset = "rolling"
	}
	switch reset {
	case "rolling", "first", "calendar":
	default:
		return compiledRequestLimit{}, fmt.Errorf("reset must be rolling, calendar or first")
	}
	if reset == "calendar" && (window > 24*time.Hour || (24*time.Hour)%window != 0) {
		return compiledRequestLimit{}, fmt.Errorf("calendar window must divide 24h")
	}
	location := time.Local
	if reset == "calendar" {
		if raw := strings.TrimSpace(limit.Timezone); raw != "" {
			location, err = time.LoadLocation(raw)
			if err != nil {
				return compiledRequestLimit{}, fmt.Errorf("timezone is invalid")
			}
		}
	}
	var resetAt time.Duration
	if reset == "calendar" {
		raw := strings.TrimSpace(limit.ResetAt)
		if raw == "" {
			raw = "00:00"
		}
		clock, err := time.Parse("15:04", raw)
		if err != nil {
			return compiledRequestLimit{}, fmt.Errorf("reset_at must be HH:MM")
		}
		resetAt = time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute
	}
	return compiledRequestLimit{
		scope:     scope,
		limit:     limit.Limit,
		window:    window,
		reset:     reset,
		signature: limit.Signature(),
		location:  location,
		resetAt:   resetAt,
	}, nil
}

func (l *sourceRequestLimiter) PruneAccounts(ids map[string]struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, counters := range l.counters {
		for subject := range counters {
			if subject == sourceRequestLimitKey {
				continue
			}
			if _, ok := ids[subject]; !ok {
				delete(counters, subject)
			}
		}
	}
}

func (l *sourceRequestLimiter) AccountBlocked(accountID string, now time.Time) *requestLimitBlock {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out *requestLimitBlock
	for _, rule := range l.rules {
		if rule.scope != "account" {
			continue
		}
		if block := l.blockedLocked(rule, accountID, now); block != nil && laterBlock(out, block) {
			out = block
		}
	}
	return out
}

func (l *sourceRequestLimiter) SourceBlocked(now time.Time) *requestLimitBlock {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out *requestLimitBlock
	for _, rule := range l.rules {
		if rule.scope != "source" {
			continue
		}
		if block := l.blockedLocked(rule, sourceRequestLimitKey, now); block != nil && laterBlock(out, block) {
			out = block
		}
	}
	return out
}

func (l *sourceRequestLimiter) Acquire(accountID string, now time.Time) (*requestLimitBlock, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.rules) == 0 {
		return nil, nil
	}

	type pendingCharge struct {
		rule    compiledRequestLimit
		subject string
		counter requestLimitCounter
	}
	charges := make([]pendingCharge, 0, len(l.rules))
	for _, rule := range l.rules {
		subject := accountID
		if rule.scope == "source" {
			subject = sourceRequestLimitKey
		}
		counter := l.counterLocked(rule.signature, subject)
		counter = refreshRequestLimitCounter(rule, counter, now)
		if block := blockForCounter(rule, counter, now); block != nil {
			return block, nil
		}
		charges = append(charges, pendingCharge{rule: rule, subject: subject, counter: counter})
	}

	type snapshot struct {
		signature string
		subject   string
		counter   requestLimitCounter
		exists    bool
	}
	snapshots := make([]snapshot, 0, len(charges))
	for _, charge := range charges {
		counters := l.counters[charge.rule.signature]
		counter, exists := counters[charge.subject]
		snapshots = append(snapshots, snapshot{
			signature: charge.rule.signature,
			subject:   charge.subject,
			counter:   counter,
			exists:    exists,
		})
	}
	for _, charge := range charges {
		counter := chargeRequestLimit(charge.rule, charge.counter, now)
		if l.counters[charge.rule.signature] == nil {
			l.counters[charge.rule.signature] = map[string]requestLimitCounter{}
		}
		l.counters[charge.rule.signature][charge.subject] = counter
	}
	if err := l.saveLocked(); err != nil {
		for _, snap := range snapshots {
			counters := l.counters[snap.signature]
			if snap.exists {
				counters[snap.subject] = snap.counter
			} else {
				delete(counters, snap.subject)
			}
		}
		return nil, err
	}
	return nil, nil
}

func (l *sourceRequestLimiter) blockedLocked(rule compiledRequestLimit, subject string, now time.Time) *requestLimitBlock {
	counter := refreshRequestLimitCounter(rule, l.counterLocked(rule.signature, subject), now)
	return blockForCounter(rule, counter, now)
}

func (l *sourceRequestLimiter) counterLocked(signature, subject string) requestLimitCounter {
	if counters := l.counters[signature]; counters != nil {
		return counters[subject]
	}
	return requestLimitCounter{}
}

func (l *sourceRequestLimiter) saveLocked() error {
	return core.WriteJSONAtomic(l.path, requestLimitFile{
		Version:  requestLimitFileVersion,
		Counters: l.counters,
	})
}

func refreshRequestLimitCounter(rule compiledRequestLimit, counter requestLimitCounter, now time.Time) requestLimitCounter {
	switch rule.reset {
	case "rolling":
		cutoff := now.Add(-rule.window).UnixMilli()
		kept := make([]int64, 0, len(counter.Timestamps))
		for _, stamp := range counter.Timestamps {
			if stamp > cutoff {
				kept = append(kept, stamp)
			}
		}
		counter.Timestamps = kept
		sort.Slice(counter.Timestamps, func(i, j int) bool { return counter.Timestamps[i] < counter.Timestamps[j] })
	case "first":
		if counter.Start == 0 || now.Sub(time.UnixMilli(counter.Start)) >= rule.window {
			counter = requestLimitCounter{}
		}
	case "calendar":
		start := calendarPeriodStart(rule, now)
		if counter.Start != start.UnixMilli() {
			counter = requestLimitCounter{Start: start.UnixMilli()}
		}
	}
	return counter
}

func chargeRequestLimit(rule compiledRequestLimit, counter requestLimitCounter, now time.Time) requestLimitCounter {
	switch rule.reset {
	case "rolling":
		counter.Timestamps = append(counter.Timestamps, now.UnixMilli())
	case "first":
		if counter.Start == 0 || now.Sub(time.UnixMilli(counter.Start)) >= rule.window {
			counter = requestLimitCounter{Start: now.UnixMilli()}
		}
		counter.Used++
	case "calendar":
		start := calendarPeriodStart(rule, now)
		if counter.Start != start.UnixMilli() {
			counter = requestLimitCounter{Start: start.UnixMilli()}
		}
		counter.Used++
	}
	return counter
}

func blockForCounter(rule compiledRequestLimit, counter requestLimitCounter, now time.Time) *requestLimitBlock {
	switch rule.reset {
	case "rolling":
		if len(counter.Timestamps) < rule.limit {
			return nil
		}
		return &requestLimitBlock{
			Scope: rule.scope,
			Until: time.UnixMilli(counter.Timestamps[0]).Add(rule.window),
		}
	case "first", "calendar":
		if counter.Used < rule.limit {
			return nil
		}
		return &requestLimitBlock{
			Scope: rule.scope,
			Until: time.UnixMilli(counter.Start).Add(rule.window),
		}
	default:
		return nil
	}
}

func calendarPeriodStart(rule compiledRequestLimit, now time.Time) time.Time {
	local := now.In(rule.location)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, rule.location)
	anchor := midnight.Add(rule.resetAt)
	if local.Before(anchor) {
		anchor = anchor.AddDate(0, 0, -1)
	}
	elapsed := local.Sub(anchor)
	if elapsed < 0 {
		elapsed = 0
	}
	return anchor.Add(elapsed / rule.window * rule.window)
}

func laterBlock(current, candidate *requestLimitBlock) bool {
	return candidate != nil && (current == nil || candidate.Until.After(current.Until))
}
