package openrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Credential sources.  The source decides whether the module OWNS a record:
// only records it owns are written to credentials.json, because a key that came
// from the config file or the environment is already durable somewhere else and
// copying it into the module's data directory would duplicate a secret for no
// reason.
const (
	sourceConfig   = "config"
	sourceEnv      = "env"
	sourceImported = "imported"
	sourcePanel    = "panel"
)

// accountRecord is one credential plus the health the pool tracks for it.
//
// The key lives here and only here.  It never reaches the panel: records() and
// statuses() are the only renderings, and both drop it.
type accountRecord struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	APIKey  string `json:"api_key,omitempty"`
	Source  string `json:"source"`
	AddedAt string `json:"added_at,omitempty"`

	// Health.  All of it is mirrored into state.json, so a park, a disable or
	// a "this key was rejected" verdict survives a restart.
	//
	// A park is scoped to what the verdict actually proved:
	//
	//   - CooldownUntil blocks every model (throttling, a rejected key, a
	//     breaker verdict);
	//   - CreditUntil blocks the PAID ids only.  Zero-cost ids spend no credit,
	//     so "this wallet is empty" is no reason to bench them;
	//   - FreeUntil blocks the zero-cost ids only: it is the vendor's free-tier
	//     daily cap, which says nothing about the paid ids;
	//   - ModelParks holds per-model parks -- "this key cannot call THIS id" --
	//     keyed by model id.
	Disabled      bool              `json:"disabled,omitempty"`
	Note          string            `json:"note,omitempty"`
	CooldownUntil string            `json:"cooldown_until,omitempty"`
	CreditUntil   string            `json:"credit_until,omitempty"`
	FreeUntil     string            `json:"free_until,omitempty"`
	ModelParks    map[string]string `json:"model_parks,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	ErrCount      int               `json:"err_count,omitempty"`
	Invalid       bool              `json:"invalid,omitempty"`
	LastUsedAt    string            `json:"last_used_at,omitempty"`

	// Runtime only, guarded by the pool mutex.
	inFlight int
	lastUsed time.Time
}

// credentialRecord is the on-disk shape of credentials.json: the records the
// module owns, and nothing else.
type credentialRecord struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	APIKey  string `json:"api_key"`
	Source  string `json:"source"`
	AddedAt string `json:"added_at,omitempty"`
}

// healthRecord is the on-disk shape of state.json.  It has no key field at
// all, which is what makes "a health file can never leak a credential" a
// structural fact rather than a promise.
type healthRecord struct {
	ID            string            `json:"id"`
	Disabled      bool              `json:"disabled,omitempty"`
	Note          string            `json:"note,omitempty"`
	CooldownUntil string            `json:"cooldown_until,omitempty"`
	CreditUntil   string            `json:"credit_until,omitempty"`
	FreeUntil     string            `json:"free_until,omitempty"`
	ModelParks    map[string]string `json:"model_parks,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	ErrCount      int               `json:"err_count,omitempty"`
	Invalid       bool              `json:"invalid,omitempty"`
	LastUsedAt    string            `json:"last_used_at,omitempty"`
}

const (
	credentialsVersion = 1
	stateVersion       = 1
)

// pool is the credential set.  It is deliberately tiny: OpenRouter is a
// static-key vendor, so there is no refresh, no session and no login state --
// only "which key, is it usable right now, and how many requests is it
// already carrying".
type pool struct {
	mu    sync.Mutex
	accts []*accountRecord
}

func newPool() *pool { return &pool{} }

// --- reads ----------------------------------------------------------------

func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accts)
}

// snapshot returns copies of every record, keys included.  Internal use only:
// the key is needed to build a request, and must never be rendered or logged.
func (p *pool) snapshot() []accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]accountRecord, 0, len(p.accts))
	for _, a := range p.accts {
		out = append(out, *a)
	}
	return out
}

// byID returns a copy of one record.
func (p *pool) byID(id string) (accountRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return accountRecord{}, false
	}
	return *a, true
}

func (p *pool) lookupLocked(id string) *accountRecord {
	for _, a := range p.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// admission is what the caller knows about the request before an account is
// picked: whether it is a zero-cost model (a credit park must not block it),
// and which model it names (a model park is per key AND per model).
type admission struct {
	free  bool
	model string
}

// ready counts the accounts a request could go to right now.  A key whose
// wallet is empty still serves the zero-cost ids, so it counts.
func (p *pool) ready(now time.Time, limit int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if p.anySelectableLocked(a, now, limit) {
			n++
		}
	}
	return n
}

// anySelectableLocked is ready()'s predicate: an account counts when at least
// one class of model could reach it.  A model park cannot rule that out (the
// other ids on the key still work), so no model is named here.
func (p *pool) anySelectableLocked(a *accountRecord, now time.Time, limit int) bool {
	return p.selectableLocked(a, now, limit, admission{}) ||
		p.selectableLocked(a, now, limit, admission{free: true})
}

// enabled counts the accounts that are switched on and carry a key, whether or
// not they are currently usable.
func (p *pool) enabled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if !a.Disabled && a.APIKey != "" {
			n++
		}
	}
	return n
}

// selectableLocked is the whole admission policy in one place: switched on,
// carrying a key, not parked for anything that applies to this request, and
// under the in-flight ceiling.
//
// The scoping is the point.  A key whose wallet is empty is still the right
// answer for a zero-cost model, and a key that has burned its free-tier daily
// cap is still the right answer for a paid one; only a park on the account
// itself (throttling, a rejected key, the breaker) blocks both.
func (p *pool) selectableLocked(a *accountRecord, now time.Time, limit int, adm admission) bool {
	if a.Disabled || a.APIKey == "" {
		return false
	}
	if p.coolingDownLocked(a, now) {
		return false
	}
	if !adm.free && p.activeStampLocked(a.CreditUntil, now) {
		return false
	}
	if adm.free && p.activeStampLocked(a.FreeUntil, now) {
		return false
	}
	if adm.model != "" && p.modelParkedLocked(a, adm.model, now) {
		return false
	}
	if limit > 0 && a.inFlight >= limit {
		return false
	}
	return true
}

// coolingDownLocked reports whether the account is parked for every model.
// An unparseable timestamp is treated as "not parked": a corrupted state file
// must not be able to wedge every credential permanently.
func (p *pool) coolingDownLocked(a *accountRecord, now time.Time) bool {
	return p.activeStampLocked(a.CooldownUntil, now)
}

// activeStampLocked reports whether a stored RFC3339 park is still in force.
func (p *pool) activeStampLocked(stamp string, now time.Time) bool {
	if stamp == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return false
	}
	return now.Before(until)
}

// modelParkedLocked reports whether this exact model is parked on this key.
// The key still serves every other id.
func (p *pool) modelParkedLocked(a *accountRecord, model string, now time.Time) bool {
	if len(a.ModelParks) == 0 {
		return false
	}
	return p.activeStampLocked(a.ModelParks[strings.TrimSpace(model)], now)
}

// firstReady returns the account an idempotent read (models, balance) should
// use: the least-recently-used selectable one, else any enabled one, so a
// balance call still works while every account is parked.
func (p *pool) firstReady(now time.Time, limit int) (accountRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var best *accountRecord
	bestPriority := 0
	for _, a := range p.accts {
		if !p.anySelectableLocked(a, now, limit) {
			continue
		}
		priority := core.AccountPriority("openrouter", a.ID)
		if best == nil || priority < bestPriority || (priority == bestPriority && a.lastUsed.Before(best.lastUsed)) {
			best = a
			bestPriority = priority
		}
	}
	if best != nil {
		return *best, true
	}
	for _, a := range p.accts {
		if !a.Disabled && a.APIKey != "" {
			return *a, true
		}
	}
	return accountRecord{}, false
}

// --- admission ------------------------------------------------------------

// acquire takes the least-recently-used account this class of request may use
// and marks one request in flight against it.
//
// The three failures are distinct on purpose: "nothing configured" and "all
// disabled" are configuration problems (503 at the gateway), while "everything
// is parked or busy" is backpressure (429).
func (p *pool) acquire(now time.Time, limit int, adm admission) (accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return accountRecord{}, fmt.Errorf("%w: no OpenRouter credential; set clients.%s.api_key, export OPENROUTER_API_KEY, or import a key from the panel", core.ErrNotConfigured, clientName)
	}
	enabled := 0
	for _, a := range p.accts {
		if !a.Disabled && a.APIKey != "" {
			enabled++
		}
	}
	if enabled == 0 {
		return accountRecord{}, fmt.Errorf("%w: every OpenRouter credential is disabled", core.ErrNotConfigured)
	}
	var best *accountRecord
	for _, a := range p.accts {
		if !p.selectableLocked(a, now, limit, adm) {
			continue
		}
		if best == nil || a.lastUsed.Before(best.lastUsed) {
			best = a
		}
	}
	if best == nil {
		return accountRecord{}, fmt.Errorf("%w: every OpenRouter credential is parked for this model or at its in-flight ceiling", core.ErrBusy)
	}
	best.inFlight++
	best.lastUsed = now
	return *best, nil
}

// release gives back the in-flight slot.  It is safe to call for an id that has
// gone away (a panel removal mid-request) and never drives the counter
// negative.
func (p *pool) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(id); a != nil && a.inFlight > 0 {
		a.inFlight--
	}
}

// --- mutation -------------------------------------------------------------

// reload replaces the credential set, carrying the runtime counters across for
// ids that survive.
func (p *pool) reload(recs []accountRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := make(map[string]*accountRecord, len(p.accts))
	for _, a := range p.accts {
		prev[a.ID] = a
	}
	out := make([]*accountRecord, 0, len(recs))
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		rec := r
		if old := prev[r.ID]; old != nil {
			rec.inFlight = old.inFlight
			if rec.lastUsed.IsZero() {
				rec.lastUsed = old.lastUsed
			}
		}
		out = append(out, &rec)
	}
	p.accts = out
	p.sortLocked()
}

func (p *pool) upsert(rec accountRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(rec.ID); a != nil {
		// Keep the health the operator can see, take the new credential.
		a.Label = rec.Label
		a.APIKey = rec.APIKey
		a.Source = rec.Source
		a.Disabled = rec.Disabled
		p.sortLocked()
		return
	}
	recCopy := rec
	p.accts = append(p.accts, &recCopy)
	p.sortLocked()
}

func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accts {
		if a.ID != id {
			continue
		}
		p.accts = append(p.accts[:i], p.accts[i+1:]...)
		return true
	}
	return false
}

func (p *pool) setEnabled(id string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return false
	}
	a.Disabled = !enabled
	if enabled {
		// Re-enabling by hand is an operator override: clear every verdict that
		// switched it off, or the next failure would immediately re-park it.
		a.CooldownUntil = ""
		a.CreditUntil = ""
		a.FreeUntil = ""
		a.ModelParks = nil
		a.Invalid = false
		a.ErrCount = 0
		a.LastError = ""
		a.Note = ""
	}
	return true
}

func (p *pool) setLabel(id, label string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return false
	}
	a.Label = label
	return true
}

func (p *pool) setNote(id, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(id); a != nil {
		a.Note = note
	}
}

// setCooldown parks the account for EVERY model: throttling, a rejected key
// and the breaker are verdicts on the credential itself.
func (p *pool) setCooldown(id string, until time.Time, reason string) {
	p.parkLocked(id, until, reason, func(a *accountRecord, stamp string) { a.CooldownUntil = stamp })
}

// setCreditPark parks the account for the PAID ids only: the wallet is empty,
// so a zero-cost id must still route here.
func (p *pool) setCreditPark(id string, until time.Time, reason string) {
	p.parkLocked(id, until, reason, func(a *accountRecord, stamp string) { a.CreditUntil = stamp })
}

// setFreePark parks the account for the ZERO-COST ids only: the vendor's
// free-tier daily counter is spent, which says nothing about paid credit.
func (p *pool) setFreePark(id string, until time.Time, reason string) {
	p.parkLocked(id, until, reason, func(a *accountRecord, stamp string) { a.FreeUntil = stamp })
}

// parkModel parks one (key, model) pair and nothing else.  Every other id on
// the key stays usable, which is what "this account cannot call THIS free
// model" has to mean for the pool.
func (p *pool) parkModel(id, model string, until time.Time, reason string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	p.parkLocked(id, until, reason, func(a *accountRecord, stamp string) {
		if a.ModelParks == nil {
			a.ModelParks = map[string]string{}
		}
		a.ModelParks[model] = stamp
	})
}

// clearModelPark lifts one (key, model) park.
func (p *pool) clearModelPark(id, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return
	}
	delete(a.ModelParks, strings.TrimSpace(model)) // a nil map is fine here
	if len(a.ModelParks) == 0 {
		a.ModelParks = nil
	}
}

// clearParks lifts every park on the account.  It is what the operator's
// manual refresh means: "I asked the vendor whether this key is alive", an
// override rather than a routing verdict.
func (p *pool) clearParks(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return
	}
	a.CooldownUntil = ""
	a.CreditUntil = ""
	a.FreeUntil = ""
	a.ModelParks = nil
	a.LastError = ""
	a.ErrCount = 0
	a.Invalid = false
	a.Note = ""
}

// parkLocked stores one scoped park plus the reason behind it.
func (p *pool) parkLocked(id string, until time.Time, reason string, set func(*accountRecord, string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return
	}
	set(a, until.UTC().Format(time.RFC3339))
	if reason != "" {
		a.LastError = reason
	}
}

func (p *pool) clearCooldown(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(id); a != nil {
		a.CooldownUntil = ""
	}
}
func (p *pool) markInvalid(id string, invalid bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(id); a != nil {
		a.Invalid = invalid
	}
}

// noteError counts a failure the classifier could not attribute to capacity.
// Only the threshold-th consecutive one parks the account: a single odd 400
// must not take a healthy key out of rotation.
func (p *pool) noteError(id, msg string, now time.Time, threshold int, park time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return
	}
	a.LastError = msg
	a.ErrCount++
	if threshold > 0 && a.ErrCount >= threshold {
		a.ErrCount = 0
		a.CooldownUntil = now.Add(park).UTC().Format(time.RFC3339)
	}
}

// noteSuccess clears the verdicts a read proves wrong -- throttling, the
// breaker, a rejected key -- and reports whether anything actually changed so
// the caller only pays for a write when there is one to make.
//
// It deliberately keeps the class parks: a catalogue or balance read does not
// prove the wallet is funded, nor that the free-tier counter has reset.
func (p *pool) noteSuccess(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return false
	}
	changed := a.CooldownUntil != "" || a.LastError != "" || a.ErrCount != 0 || a.Invalid
	a.CooldownUntil = ""
	a.LastError = ""
	a.ErrCount = 0
	a.Invalid = false
	if !a.hasParkLocked() {
		a.Note = ""
	}
	return changed
}

// chatSuccess records a completed completion.
//
// Each class of success lifts only what it actually proved.  A zero-cost
// completion shows the free tier works, not that the wallet is funded; a paid
// completion shows the wallet works, not that the free-tier counter has reset.
// That asymmetry is what keeps an empty wallet serving the free ids: nothing
// here can silently clear the credit park, and nothing re-parks it either.
func (p *pool) chatSuccess(id, model string, free bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.lookupLocked(id)
	if a == nil {
		return false
	}
	changed := a.CooldownUntil != "" || a.LastError != "" || a.ErrCount != 0 || a.Invalid
	a.CooldownUntil = ""
	a.LastError = ""
	a.ErrCount = 0
	a.Invalid = false
	if free {
		if a.FreeUntil != "" {
			a.FreeUntil = ""
			changed = true
		}
		if key := strings.TrimSpace(model); key != "" {
			if _, ok := a.ModelParks[key]; ok {
				delete(a.ModelParks, key)
				changed = true
			}
		}
	} else if a.CreditUntil != "" {
		a.CreditUntil = ""
		changed = true
	}
	if len(a.ModelParks) == 0 {
		a.ModelParks = nil
	}
	if !a.hasParkLocked() {
		a.Note = ""
	}
	return changed
}

// touchLastUsed records a successful call so the health file can show it.
func (p *pool) touchLastUsed(id string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.lookupLocked(id); a != nil {
		a.LastUsedAt = now.UTC().Format(time.RFC3339)
	}
}

func (p *pool) sortLocked() {
	sort.Slice(p.accts, func(i, j int) bool { return p.accts[i].ID < p.accts[j].ID })
}

// --- renderings -----------------------------------------------------------

// stateOf is the panel's one-word verdict.
//
// A class park is rendered as its own state rather than as a plain cooldown:
// "low_credit" still serves the zero-cost ids and "quota_exceeded" still
// serves the paid ones, and the panel is where the operator has to be able to
// tell those apart.
func (p *pool) stateOf(a *accountRecord, now time.Time) string {
	if a.Disabled || a.APIKey == "" {
		return "invalid"
	}
	if a.Invalid && p.coolingDownLocked(a, now) {
		return "invalid"
	}
	if p.coolingDownLocked(a, now) {
		return "cooling"
	}
	if p.activeStampLocked(a.CreditUntil, now) {
		return "low_credit"
	}
	if p.activeStampLocked(a.FreeUntil, now) {
		return "quota_exceeded"
	}
	for _, stamp := range a.ModelParks {
		if p.activeStampLocked(stamp, now) {
			return "quota_exceeded"
		}
	}
	return "ready"
}

// hasParkLocked reports whether any park -- active or expired -- is stored on
// the record.  It decides whether the human note still has something to
// describe, so it counts expired parks too.
func (a *accountRecord) hasParkLocked() bool {
	return a.CooldownUntil != "" || a.CreditUntil != "" || a.FreeUntil != "" || len(a.ModelParks) > 0
}

// expiresAt is the timestamp the panel's "还有 …" column counts down to: the
// earliest horizon that a request of either class would have to wait for.
func (a *accountRecord) expiresAt() string {
	for _, stamp := range []string{a.CooldownUntil, a.CreditUntil, a.FreeUntil} {
		if stamp != "" {
			return stamp
		}
	}
	for _, stamp := range a.ModelParks {
		if stamp != "" {
			return stamp
		}
	}
	return ""
}

// scrubNote renders a note for the panel.
//
// core.Redact only catches LABELLED forms ("api_key=…", "Bearer …"), so a
// vendor that echoes the bare key inside a sentence ("invalid key sk-or-v1-…")
// would otherwise pass straight through.  Masking this record's own key first
// makes "a panel note can never carry a credential" structural rather than a
// promise every call site has to keep.
func scrubNote(a *accountRecord) string {
	note := a.Note
	if a.APIKey != "" {
		note = strings.ReplaceAll(note, a.APIKey, core.MaskSecret(a.APIKey))
	}
	return core.Redact(note)
}

// statuses renders one core.AccountStatus per record.  Notes are redacted and
// keys are absent by construction.
func (p *pool) statuses(now time.Time) []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.accts))
	for _, a := range p.accts {
		st := core.AccountStatus{
			ID:      a.ID,
			Label:   safeLabel(*a),
			Enabled: !a.Disabled,
			State:   p.stateOf(a, now),
			Note:    scrubNote(a),
		}
		if stamp := a.expiresAt(); stamp != "" {
			st.ExpiresAt = stamp
		}
		extra := map[string]any{
			"source":    a.Source,
			"in_flight": a.inFlight,
		}
		if a.ErrCount > 0 {
			extra["failures"] = a.ErrCount
		}
		if a.CreditUntil != "" {
			extra["credit_until"] = a.CreditUntil
		}
		if a.FreeUntil != "" {
			extra["free_until"] = a.FreeUntil
		}
		if len(a.ModelParks) > 0 {
			parks := make(map[string]string, len(a.ModelParks))
			for id, stamp := range a.ModelParks {
				parks[id] = stamp
			}
			extra["model_parks"] = parks
		}
		if a.LastError != "" {
			extra["last_error"] = core.Redact(a.LastError)
		}
		st.Extra = extra
		out = append(out, st)
	}
	return out
}

// records renders the panel's account table.  Every record is listed,
// including parked and disabled ones, so an operator can always see and revive
// what they turned off.
func (p *pool) records(now time.Time) []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountRecord, 0, len(p.accts))
	for _, a := range p.accts {
		fields := map[string]any{
			"source":    a.Source,
			"removable": a.Source != sourceConfig && a.Source != sourceEnv,
			"in_flight": a.inFlight,
		}
		if a.ErrCount > 0 {
			fields["failures"] = a.ErrCount
		}
		if a.CooldownUntil != "" {
			fields["cooldown_until"] = a.CooldownUntil
		}
		if a.CreditUntil != "" {
			fields["credit_until"] = a.CreditUntil
		}
		if a.FreeUntil != "" {
			fields["free_until"] = a.FreeUntil
		}
		if len(a.ModelParks) > 0 {
			parks := make(map[string]string, len(a.ModelParks))
			for id, stamp := range a.ModelParks {
				parks[id] = stamp
			}
			fields["model_parks"] = parks
		}
		out = append(out, core.AccountRecord{
			ID:        a.ID,
			Label:     safeLabel(*a),
			Enabled:   !a.Disabled,
			State:     p.stateOf(a, now),
			ExpiresAt: a.expiresAt(),
			Note:      scrubNote(a),
			Fields:    fields,
		})
	}
	return out
}

// safeLabel never returns something that looks like a credential: the vendor's
// own /key.label defaults to a masked form of the key, and an operator may have
// pasted a key into the label field.
func safeLabel(a accountRecord) string {
	label := a.Label
	if label == "" {
		switch a.Source {
		case sourceConfig:
			label = "configured key"
		case sourceEnv:
			label = "OPENROUTER_API_KEY"
		case sourceImported:
			label = "imported key"
		default:
			label = "key"
		}
	}
	if looksLikeAPIKey(label) || (a.APIKey != "" && label == a.APIKey) {
		return core.MaskSecret(label)
	}
	return core.Redact(label)
}

// summary is the one human line Status() puts in Detail.
func (p *pool) summary(now time.Time, limit int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return "no credential configured"
	}
	ready, cooling, off := 0, 0, 0
	for _, a := range p.accts {
		if a.Disabled {
			// A switched-off credential is "disabled", not "parked": counting it
			// as both would make the line read "1 ready, 1 parked, 1 disabled"
			// for a pool of exactly two.
			off++
			continue
		}
		if p.stateOf(a, now) == "ready" {
			ready++
		} else {
			// Every non-ready state -- a full cooldown, a rejected key, an
			// empty wallet, a spent free-tier cap -- is a park on the row.
			cooling++
		}
	}
	line := fmt.Sprintf("%d credential(s): %d ready, %d parked, %d disabled", len(p.accts), ready, cooling, off)
	if limit > 0 {
		line += fmt.Sprintf("; max_in_flight=%d", limit)
	}
	return line
}

// stats reports the pool counters /panel and /healthz render.
func (p *pool) stats(limit int) (inFlight, full int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		inFlight += a.inFlight
		if limit > 0 && a.inFlight >= limit {
			full++
		}
	}
	return inFlight, full
}

// --- persistence ----------------------------------------------------------

// loadCredentials reads credentials.json.  A missing or corrupt file is not an
// error: it means "no imported keys", and the module still has the config and
// environment paths.
func loadCredentials(path string) []credentialRecord {
	var store struct {
		Version  int                `json:"version"`
		Accounts []credentialRecord `json:"accounts"`
	}
	if err := core.ReadJSON(path, &store); err != nil {
		return nil
	}
	out := make([]credentialRecord, 0, len(store.Accounts))
	for _, r := range store.Accounts {
		if r.APIKey == "" {
			continue
		}
		if r.Source == "" || r.Source == sourceConfig || r.Source == sourceEnv {
			// Anything the module does not own is not durable here; treat a
			// stray row as imported so it is at least listed.
			r.Source = sourceImported
		}
		out = append(out, r)
	}
	return out
}

func saveCredentials(path string, recs []credentialRecord) error {
	if len(recs) == 0 {
		// Removing the last owned credential must also remove the file, not
		// leave an empty store behind that looks like state.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return core.WriteJSONAtomic(path, map[string]any{
		"version":  credentialsVersion,
		"accounts": recs,
	})
}

func loadState(path string) map[string]healthRecord {
	var store struct {
		Version  int            `json:"version"`
		Accounts []healthRecord `json:"accounts"`
	}
	out := map[string]healthRecord{}
	if err := core.ReadJSON(path, &store); err != nil {
		return out
	}
	for _, r := range store.Accounts {
		if r.ID != "" {
			out[r.ID] = r
		}
	}
	return out
}

func saveState(path string, recs []healthRecord) error {
	if len(recs) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return core.WriteJSONAtomic(path, map[string]any{
		"version":  stateVersion,
		"accounts": recs,
	})
}

// applyHealth layers the persisted health over a freshly built record.
func applyHealth(rec accountRecord, h healthRecord) accountRecord {
	rec.Disabled = h.Disabled
	rec.Note = h.Note
	rec.CooldownUntil = h.CooldownUntil
	rec.CreditUntil = h.CreditUntil
	rec.FreeUntil = h.FreeUntil
	if len(h.ModelParks) > 0 {
		rec.ModelParks = make(map[string]string, len(h.ModelParks))
		for k, v := range h.ModelParks {
			rec.ModelParks[k] = v
		}
	}
	rec.LastError = h.LastError
	rec.ErrCount = h.ErrCount
	rec.Invalid = h.Invalid
	rec.LastUsedAt = h.LastUsedAt
	if ts, err := time.Parse(time.RFC3339, h.LastUsedAt); err == nil {
		rec.lastUsed = ts
	}
	return rec
}

func healthOf(rec accountRecord) healthRecord {
	var parks map[string]string
	if len(rec.ModelParks) > 0 {
		parks = make(map[string]string, len(rec.ModelParks))
		for k, v := range rec.ModelParks {
			parks[k] = v
		}
	}
	return healthRecord{
		ID:            rec.ID,
		Disabled:      rec.Disabled,
		Note:          rec.Note,
		CooldownUntil: rec.CooldownUntil,
		CreditUntil:   rec.CreditUntil,
		FreeUntil:     rec.FreeUntil,
		ModelParks:    parks,
		LastError:     rec.LastError,
		ErrCount:      rec.ErrCount,
		Invalid:       rec.Invalid,
		LastUsedAt:    rec.LastUsedAt,
	}
}

// marshalJSON is a tiny indirection so tests can assert the on-disk shapes
// without reaching for encoding/json directly.
func marshalJSON(v any) ([]byte, error) { return json.Marshal(v) }
