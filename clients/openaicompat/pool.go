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
type providerRecord struct {
	ID       string
	Label    string
	APIKey   string
	BaseURL  string
	Models   []string
	Headers  map[string]string
	Disabled bool
	Source   string

	inFlight int
	lastUsed time.Time
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

func (p *providerPool) ready(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if !a.Disabled {
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
			State:   stateOf(a),
			Extra: map[string]any{
				"source":    firstNonEmpty(a.Source, sourceConfig),
				"base_url":  a.BaseURL,
				"in_flight": a.inFlight,
			},
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
		out = append(out, core.AccountRecord{
			ID:      a.ID,
			Label:   a.Label,
			Enabled: !a.Disabled,
			State:   stateOf(a),
			Fields:  fields,
		})
	}
	return out
}

func stateOf(a *providerRecord) string {
	if a.Disabled {
		return "invalid"
	}
	return "ready"
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
