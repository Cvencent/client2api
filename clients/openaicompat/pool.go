package openaicompat

import (
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Where a provider row came from.  A row the operator wrote into the module
// config is durable elsewhere; only sourcePanel rows live in this module's own
// accounts.json and may be edited from the panel.
const (
	sourceConfig = "config"
	sourcePanel  = "panel"
)

// providerRecord is one configured upstream source, with the health the pool
// tracks for it.
//
// Health lives HERE because a provider row used to be a credential plus a base
// URL with no state at all: every request retried every provider in turn, so a
// key that had been revoked, expired, or rate-limited into oblivion was picked
// again on the very next request.  That is the "the key is dead but it keeps
// being tried" behaviour an operator sees as the gateway hammering an endpoint
// that has not answered in hours.
type providerRecord struct {
	ID        string
	Label     string
	APIKey    string
	AppSecret string
	BaseURL   string
	Models    []string
	Headers   map[string]string
	Disabled  bool
	Source    string

	inFlight int
	lastUsed time.Time

	// failures counts consecutive failed rounds since this provider last served
	// a request.  A success resets it, so one transient blip costs nothing while
	// a persistently dead key still parks.
	failures int
	// cooldownUntil is when a parked provider may be tried again; zero means it
	// is not parked.
	cooldownUntil time.Time
	// lastError is the redacted reason it was parked, for the panel.
	lastError string
}

// providerPool is the set of configured providers.
type providerPool struct {
	mu    sync.Mutex
	accts []*providerRecord
}

func newProviderPool() *providerPool { return &providerPool{} }

func (p *providerPool) reload(recs []providerRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := make(map[string]*providerRecord, len(p.accts))
	for _, a := range p.accts {
		prev[a.ID] = a
	}
	out := make([]*providerRecord, 0, len(recs))
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.ID == "" || seen[strings.ToLower(r.ID)] {
			continue
		}
		seen[strings.ToLower(r.ID)] = true
		rec := r
		if old := prev[r.ID]; old != nil {
			rec.inFlight = old.inFlight
			rec.lastUsed = old.lastUsed
			// Health survives a reload: the operator editing one provider
			// must not resurrect a key the vendor has been refusing.
			rec.failures = old.failures
			rec.cooldownUntil = old.cooldownUntil
			rec.lastError = old.lastError
		}
		out = append(out, &rec)
	}
	p.accts = out
}

func (p *providerPool) byID(id string) (providerRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if strings.EqualFold(a.ID, id) {
			return *a, true
		}
	}
	return providerRecord{}, false
}

// snapshot returns a copy of the whole pool, for callers that iterate.
func (p *providerPool) snapshot() []providerRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]providerRecord, 0, len(p.accts))
	for _, a := range p.accts {
		out = append(out, *a)
	}
	return out
}

// parked reports whether a provider is inside its failure cooldown right now.
func (p *providerPool) parked(now time.Time, a *providerRecord) bool {
	return !a.cooldownUntil.IsZero() && now.Before(a.cooldownUntil)
}

// isParked is parked for one id, taking the lock.
func (p *providerPool) isParked(now time.Time, id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if strings.EqualFold(a.ID, id) {
			return p.parked(now, a)
		}
	}
	return false
}

// noteSuccess clears a provider's failure record.  It runs only when the
// vendor actually served a request, so a provider that merely answered "429"
// cannot launder a broken key into a healthy one.
func (p *providerPool) noteSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if strings.EqualFold(a.ID, id) {
			a.failures = 0
			a.cooldownUntil = time.Time{}
			a.lastError = ""
			return
		}
	}
}

// noteFailure records one failed round and parks the provider once its
// consecutive-failure budget is spent.
//
// The budget exists because a single failure is not evidence: an edge can drop
// one connection, a provider can 502 once mid-deploy.  It is only when a
// provider fails `threshold` times in a row without serving anything in between
// that continuing to send it traffic stops making sense -- at that point either
// the key is dead or the endpoint is, and either way the gateway should stop
// choosing it until the cooldown expires.
//
// parking accounts for the failure KIND: a rejected key (auth) is parked for
// longer than a transient 5xx, because the operator has to act on it rather
// than wait it out.
func (p *providerPool) noteFailure(id string, threshold int, until time.Time, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, a := range p.accts {
		if !strings.EqualFold(a.ID, id) {
			continue
		}
		a.failures++
		a.lastError = msg
		if threshold > 0 && a.failures >= threshold && until.After(now) {
			a.cooldownUntil = until
		}
		return
	}
}

// record returns the live health of one provider, for the panel.
func (p *providerPool) record(id string) (providerRecord, bool) {
	return p.byID(id)
}

func (p *providerPool) ready(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if !a.Disabled && !p.parked(now, a) {
			n++
		}
	}
	return n
}

// noteInFlight adjusts a provider's live request count.  delta is +1 when a
// stream opens and -1 when it closes.  It is the same counter PoolStats and the
// panel read, and it stamps lastUsed so the operator can see which provider is
// actually serving.  An id no longer in the pool is ignored, which keeps a
// stream that outlives a reload from unbalancing the books.
func (p *providerPool) noteInFlight(id string, delta int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if strings.EqualFold(a.ID, id) {
			a.inFlight += delta
			if a.inFlight < 0 {
				a.inFlight = 0
			}
			if delta > 0 {
				a.lastUsed = time.Now()
			}
			return
		}
	}
}

// stats reports the live in-flight total and how many enabled providers are at
// their per-provider ceiling.
func (p *providerPool) stats(limit int) (inFlight, full int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		inFlight += a.inFlight
		if limit > 0 && !a.Disabled && a.inFlight >= limit {
			full++
		}
	}
	return inFlight, full
}

// disabled counts providers the operator switched off.
func (p *providerPool) disabled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if a.Disabled {
			n++
		}
	}
	return n
}

func (p *providerPool) summary(now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return "no provider configured"
	}
	ready, disabled := 0, 0
	for _, a := range p.accts {
		if a.Disabled {
			disabled++
			continue
		}
		ready++
	}
	return pluralize(len(p.accts), "provider") + ": " + itoa(ready) + " ready, " + itoa(disabled) + " disabled"
}

func (p *providerPool) statuses(now time.Time) []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.accts))
	for _, a := range p.accts {
		st := core.AccountStatus{
			ID:      a.ID,
			Label:   a.Label,
			Enabled: !a.Disabled,
			State:   p.stateAt(now, a),
			Extra: map[string]any{
				"source":    firstNonEmpty(a.Source, sourceConfig),
				"base_url":  a.BaseURL,
				"in_flight": a.inFlight,
			},
		}
		if a.failures > 0 {
			st.Extra["failures"] = a.failures
		}
		if !a.cooldownUntil.IsZero() && now.Before(a.cooldownUntil) {
			st.Extra["cooldown_until"] = a.cooldownUntil.UTC().Format(time.RFC3339)
		}
		if a.lastError != "" {
			st.Extra["last_error"] = a.lastError
		}
		out = append(out, st)
	}
	return out
}

// records renders the panel's account table.  Every provider is listed,
// including disabled ones, so an operator can always see and revive what they
// turned off.  A record never carries the key itself.
func (p *providerPool) records(now time.Time) []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountRecord, 0, len(p.accts))
	for _, a := range p.accts {
		src := firstNonEmpty(a.Source, sourceConfig)
		fields := map[string]any{
			"source":    src,
			"removable": src == sourcePanel,
			"base_url":  a.BaseURL,
			"in_flight": a.inFlight,
		}
		if len(a.Models) > 0 {
			fields["models"] = strings.Join(a.Models, ", ")
		}
		if a.failures > 0 {
			fields["failures"] = a.failures
		}
		if !a.cooldownUntil.IsZero() && now.Before(a.cooldownUntil) {
			fields["cooldown_until"] = a.cooldownUntil.UTC().Format(time.RFC3339)
		}
		if a.lastError != "" {
			fields["last_error"] = a.lastError
		}
		out = append(out, core.AccountRecord{
			ID:      a.ID,
			Label:   a.Label,
			Enabled: !a.Disabled,
			State:   p.stateAt(now, a),
			Note:    p.noteAt(now, a),
			Fields:  fields,
		})
	}
	return out
}

// stateAt is the pool state one provider is in right now.  A parked provider
// reads as "cooling" so the panel and the picker agree; the operator can still
// see it, and it becomes selectable again when the window expires.
func (p *providerPool) stateAt(now time.Time, a *providerRecord) string {
	if a.Disabled {
		return "invalid"
	}
	if a.cooldownUntil.IsZero() || !now.Before(a.cooldownUntil) {
		return "ready"
	}
	// A rejected key needs the operator, so it is reported as an expired
	// credential rather than ordinary throttling.
	if a.lastError != "" && isAuthMessage(a.lastError) {
		return "expired"
	}
	return "cooling"
}

// noteAt is the one-line explanation the panel shows next to a parked row.
func (p *providerPool) noteAt(now time.Time, a *providerRecord) string {
	if a.Disabled {
		return ""
	}
	if a.cooldownUntil.IsZero() || !now.Before(a.cooldownUntil) {
		return ""
	}
	msg := "parked after " + itoa(a.failures) + " consecutive failures until " +
		a.cooldownUntil.UTC().Format(time.RFC3339)
	if a.lastError != "" {
		msg += ": " + a.lastError
	}
	return msg
}

// isAuthMessage reports whether a recorded failure text reads as a rejected
// credential rather than a transient fault, so the panel can tell an operator
// "re-import this key" apart from "wait for the cooldown".
func isAuthMessage(msg string) bool {
	return containsMarker(msg, authMarkers)
}

// --- tiny helpers -----------------------------------------------------------

func pluralize(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
