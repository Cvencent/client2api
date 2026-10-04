package kimi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Account health
//
// `enabled` in accounts.json remembers what the *operator* decided.  On its own
// that is not enough: a login that has died -- the CLI was uninstalled, the
// vendor refused the refresh token, the executable the operator pinned stopped
// working -- is still enabled, so it keeps being picked and keeps failing on
// every request, forever.  This file is the other half: a small,
// credential-free memory of what this module has learned about each account
// between runs.
//
// Design rules, in order of importance:
//
//   - Nothing here is a credential.  A record holds a state word, a deadline, a
//     short *already redacted* reason and two timestamps.  sanitizeHealthReason
//     is the single funnel every stored string passes through, so there is
//     exactly one place to audit for a leaked token.
//   - Health is a cache, never an authority.  A state file that cannot be read
//     or written degrades to "nothing is remembered": it can never fail a
//     request and it can never switch an account off.
//   - Only locally observable facts are classified (see classifyLocally).  No
//     vendor error code is invented and no vendor payload is parsed; a failure
//     this module cannot classify does not park the account.
//   - Cooling is bounded and heals by time.  Dead never heals by time: only an
//     operator action, or new positive evidence about the very thing that was
//     missing (see reviveOnEvidence), brings a dead account back.
// ---------------------------------------------------------------------------

// Account health states, as stored in the `health` map of accounts.json.
const (
	healthHealthy = "healthy"
	healthCooling = "cooling"
	healthDead    = "dead"
)

// Failure causes.  This is this module's own vocabulary: short, stable codes
// derived only from conditions observable on this machine.  They are
// deliberately not vendor error codes.
const (
	causeCLINotFound       = "cli-not-found"
	causeNoCredential      = "no-credential"
	causeProcessFailed     = "process-failed"
	causeTimeout           = "timeout"
	causeCredentialRefused = "credential-refused"
	causeHTTPStatus        = "http-status"
	causeUnclassified      = "unclassified"
)

const (
	// cooldownCLIMissing covers "there is no kimi CLI here".  It is short
	// because the check is local and cheap: the point is to stop re-deciding
	// the same question on every panel refresh, not to hide a broken install.
	cooldownCLIMissing = 2 * time.Minute
	// cooldownProcessFailed covers a CLI that started and exited non-zero for a
	// reason this module cannot name.  Unknown causes get bounded time, never
	// a permanent judgement.
	cooldownProcessFailed = 90 * time.Second
	cooldownTimeout       = 30 * time.Second
	cooldownUpstream      = 60 * time.Second

	// healthFlushEvery debounces level updates (last_used, a fresh reason on an
	// account that is already cooling).  A state *change* is written through
	// immediately -- see updateHealth.
	healthFlushEvery = 5 * time.Second
	healthReasonMax  = 200
)

// healthRecord is what this module remembers about one account between runs.
// Every field is either a fixed word from this file or a timestamp; no part of
// it is derived from a token.
type healthRecord struct {
	State         string `json:"state,omitempty"`
	Cause         string `json:"cause,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      string `json:"last_used,omitempty"`
}

// healthStore mirrors the `health` map of accounts.json in memory.  It is lazy:
// nothing is read until something asks, so a Client that never chats never
// touches the file.
type healthStore struct {
	mu        sync.Mutex
	records   map[string]healthRecord
	loaded    bool
	lastFlush time.Time
	// now is injectable so the pick path can be tested without sleeping.  It is
	// read under mu and is expected to be set once, before the client is used
	// concurrently.
	now func() time.Time
}

func (c *Client) healthNowLocked() time.Time {
	if c.health.now != nil {
		return c.health.now().UTC()
	}
	return time.Now().UTC()
}

// healthNow is the current time as the health layer sees it.
func (c *Client) healthNow() time.Time {
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	return c.healthNowLocked()
}

// healthEnsureLocked loads the persisted records once.  A state file that
// cannot be read yields "nothing is remembered" and, deliberately, is not
// cached as the answer: a file that becomes readable again is honoured.
func (c *Client) healthEnsureLocked() {
	if c.health.loaded {
		return
	}
	st, err := c.loadState()
	if err != nil {
		return
	}
	c.health.loaded = true
	if len(st.Health) == 0 {
		return
	}
	if c.health.records == nil {
		c.health.records = make(map[string]healthRecord, len(st.Health))
	}
	for id, rec := range st.Health {
		c.health.records[id] = rec
	}
}

// healthOf returns what is remembered about id, or the zero value.  A cooling
// record whose deadline has passed is dropped here -- and from the file --
// because cooling is the one state that heals without anyone acting.  An
// unparsable deadline counts as expired: a malformed record must never park a
// working account.
//
// An unknown state string is reported as-is but is *not* selectable (see
// selectable): this module only lets through the states it understands.
func (c *Client) healthOf(id string) healthRecord {
	id = strings.TrimSpace(id)
	if id == "" {
		return healthRecord{}
	}
	c.health.mu.Lock()
	c.healthEnsureLocked()
	rec, ok := c.health.records[id]
	healed := false
	if ok && rec.State == healthCooling {
		until, err := parseHealthTime(rec.CooldownUntil)
		if err != nil || !c.healthNowLocked().Before(until) {
			delete(c.health.records, id)
			ok, healed = false, true
		}
	}
	c.health.mu.Unlock()
	if healed {
		c.flushHealth(false)
	}
	if !ok {
		return healthRecord{}
	}
	return rec
}

// selectable reports whether the account may be picked right now.  A cooling
// account is out until its deadline; a dead one is out until something revives
// it, because time alone never does.  Anything this module does not understand
// is treated as not selectable too -- an unreadable *value* is not the same
// thing as an unreadable file, and this one was written by something else.
func (c *Client) selectable(id string) bool {
	switch c.healthOf(id).State {
	case "", healthHealthy:
		return true
	default:
		return false
	}
}

// healthNote explains, in one line an operator can act on, why id is not
// selectable right now.  It is empty when the account is selectable.
func (c *Client) healthNote(id string) string {
	rec := c.healthOf(id)
	switch rec.State {
	case healthDead:
		return "this account is marked dead after " + healthTrail(rec) + ", and time will not revive it: " + healthRevival(rec)
	case healthCooling:
		return "this account is cooling down after " + healthTrail(rec) + "; it becomes selectable again after " +
			rec.CooldownUntil + " (" + healthRevival(rec) + ")"
	case "", healthHealthy:
		return ""
	default:
		return fmt.Sprintf("this account carries an unreadable health state %q, so it is not being used: re-enable it in the panel", rec.State)
	}
}

// healthTrail renders the short "cause: last error" chain.
func healthTrail(rec healthRecord) string {
	trail := rec.Cause
	if trail == "" {
		trail = causeUnclassified
	}
	if rec.LastError != "" {
		trail += ": " + rec.LastError
	}
	return trail
}

// healthRevival names what brings the account back.  It is kept honest: the
// only things that revive a dead account are listed here, and nothing else in
// this module pretends otherwise.
func healthRevival(rec healthRecord) string {
	switch rec.Cause {
	case causeNoCredential:
		return "run `kimi login` (credential evidence appearing revives it), or re-enable it in the panel"
	case causeCredentialRefused, causeHTTPStatus:
		return "sign in again from the panel, which replaces the token and revives it, or re-enable it in the panel"
	case causeCLINotFound:
		return "install the kimi CLI (or point clients.kimi.binary at it), then re-enable it in the panel"
	default:
		return "re-enable it in the panel once the cause is fixed"
	}
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

// markCooling parks the account until now+d.  A cooling account is retried by
// itself once the deadline passes: that is the whole difference between
// cooling and dead.
func (c *Client) markCooling(id, cause, reason string, d time.Duration) {
	if d <= 0 {
		d = cooldownProcessFailed
	}
	until := healthTime(c.healthNow().Add(d))
	c.updateHealth(id, func(rec *healthRecord) {
		rec.State = healthCooling
		rec.Cause = cause
		rec.CooldownUntil = until
		rec.LastError = reason
	})
}

// markDead parks the account until something explicitly revives it.  Only call
// this for a condition this module can prove locally and that time cannot fix.
func (c *Client) markDead(id, cause, reason string) {
	c.updateHealth(id, func(rec *healthRecord) {
		rec.State = healthDead
		rec.Cause = cause
		rec.CooldownUntil = ""
		rec.LastError = reason
	})
}

// markUsed records a completed run: the account is healthy and this is when it
// last worked.
func (c *Client) markUsed(id string) {
	now := healthTime(c.healthNow())
	c.updateHealth(id, func(rec *healthRecord) {
		rec.State = healthHealthy
		rec.Cause = ""
		rec.CooldownUntil = ""
		rec.LastError = ""
		rec.LastUsed = now
	})
}

// rememberFailure records a reason without changing selectability.  It is what
// an unclassifiable failure gets: the operator can see what happened, and no
// account is parked for a guess.
func (c *Client) rememberFailure(id, cause, reason string) {
	c.updateHealth(id, func(rec *healthRecord) {
		if rec.State == "" {
			rec.State = healthHealthy
		}
		if cause != "" {
			rec.Cause = cause
		}
		rec.LastError = reason
	})
}

// revive forgets everything remembered about the account.  It is wired to the
// operator actions that are allowed to bring a dead account back: enabling it,
// re-importing its binding, signing in again.
func (c *Client) revive(id string) {
	c.reviveMany([]string{id})
}

// reviveMany is revive for several accounts, writing the file once.
func (c *Client) reviveMany(ids []string) {
	changed := false
	for _, id := range ids {
		if _, had := c.forget(id, "", false); had {
			changed = true
		}
	}
	if changed {
		c.flushHealth(true)
	}
}

// reviveOnEvidence forgets a *dead* record when the local condition that killed
// it has been disproved.  This is the second, narrow route back: evidence
// appearing on disk is something an operator action cannot be the only witness
// to (a `kimi login` in a terminal happens outside the panel).  It only ever
// clears a dead record whose cause matches, so a cooling account is still held
// to its deadline -- cooling is about not hammering, not about distrust.
func (c *Client) reviveOnEvidence(id, cause string, evidence bool) {
	if !evidence {
		return
	}
	if _, had := c.forget(id, cause, true); had {
		c.flushHealth(true)
	}
}

// forget drops a record.  With onlyDead set it drops it only when it is dead
// and (when cause is non-empty) died of that cause.
func (c *Client) forget(id, cause string, onlyDead bool) (healthRecord, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return healthRecord{}, false
	}
	c.health.mu.Lock()
	defer c.health.mu.Unlock()
	c.healthEnsureLocked()
	rec, ok := c.health.records[id]
	if !ok {
		return healthRecord{}, false
	}
	if onlyDead && rec.State != healthDead {
		return healthRecord{}, false
	}
	if onlyDead && cause != "" && rec.Cause != cause {
		return healthRecord{}, false
	}
	delete(c.health.records, id)
	return rec, true
}

// updateHealth applies one transition and persists it.  An edge -- the state,
// cause or deadline changed -- is written through immediately, because the very
// next request has to see it.  A level update (a new last_used, or a fresh
// reason on an account that is already cooling) is debounced, so a busy Chat
// loop does not rewrite the file once per request.
func (c *Client) updateHealth(id string, apply func(*healthRecord)) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	c.health.mu.Lock()
	c.healthEnsureLocked()
	old, had := c.health.records[id]
	rec := old
	apply(&rec)
	rec.Cause = strings.TrimSpace(rec.Cause)
	rec.LastError = sanitizeHealthReason(rec.LastError)
	edge := !had || old.State != rec.State || old.Cause != rec.Cause || old.CooldownUntil != rec.CooldownUntil
	if rec.State == healthHealthy && rec.Cause == "" && rec.CooldownUntil == "" &&
		rec.LastError == "" && rec.LastUsed == "" {
		// A healthy account with nothing to remember is simply absent: an empty
		// record would be a claim ("I know this is healthy") this module cannot
		// back up.
		delete(c.health.records, id)
	} else {
		if c.health.records == nil {
			c.health.records = map[string]healthRecord{}
		}
		c.health.records[id] = rec
	}
	c.health.mu.Unlock()
	c.flushHealth(edge)
}

// flushHealth writes the memory into accounts.json, at most every
// healthFlushEvery unless force is set.  A write failure is logged and dropped:
// health is a cache, so a read-only DataDir must never turn into a failed
// request, and this function never returns an error to its caller.
//
// The records are copied while the read-modify-write cycle holds acct.mu, so
// two racing flushes can never write a stale map over a newer one.
func (c *Client) flushHealth(force bool) {
	c.health.mu.Lock()
	now := c.healthNowLocked()
	if !force && !c.health.lastFlush.IsZero() && now.Sub(c.health.lastFlush) < healthFlushEvery {
		c.health.mu.Unlock()
		return
	}
	c.health.lastFlush = now
	c.health.mu.Unlock()

	_, err := c.mutateState(func(st *accountState) error {
		c.health.mu.Lock()
		defer c.health.mu.Unlock()
		if len(c.health.records) == 0 {
			st.Health = nil
			return nil
		}
		health := make(map[string]healthRecord, len(c.health.records))
		for id, rec := range c.health.records {
			health[id] = rec
		}
		st.Health = health
		return nil
	})
	if err != nil {
		c.deps.Log("kimi: could not write account health to %s: %v", accountsFileName, err)
	}
}

// sanitizeHealthReason makes a string safe to persist: redacted, one line,
// bounded.  This is the only function that decides what reaches the file.
func sanitizeHealthReason(s string) string {
	s = redactSecrets(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > healthReasonMax {
		s = s[:healthReasonMax] + "…"
	}
	return s
}

func healthTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func parseHealthTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339, strings.TrimSpace(s))
}

// ---------------------------------------------------------------------------
// Classification
//
// What this module can see locally: whether the executable exists, whether the
// child process exited non-zero, whether the request deadline passed, and what
// HTTP status the vendor answered with.  Nothing else -- in particular no
// vendor error body is parsed to guess a code.
// ---------------------------------------------------------------------------

// upstreamStatusError carries the HTTP status a vendor endpoint answered with,
// so the health layer can tell a refused credential from a transient upstream
// problem without parsing message text.  Text, when set, is the complete
// message: it is there so a call site whose wording predates this type can keep
// its exact text.
type upstreamStatusError struct {
	Where  string
	Status int
	Detail string
	Text   string
}

func (e *upstreamStatusError) Error() string {
	if e.Text != "" {
		return e.Text
	}
	return fmt.Sprintf("kimi: %s returned HTTP %d%s", e.Where, e.Status, e.Detail)
}

// credentialRefusedError is the vendor refusing the stored grant outright.
// Unlike an expired token this cannot be renewed, so it is the one vendor
// answer that is remembered as dead.
type credentialRefusedError struct {
	Status int
	Detail string
}

func (e *credentialRefusedError) Error() string {
	return fmt.Sprintf("kimi: renewing the access token was refused (HTTP %d)%s, so sign in again from the panel", e.Status, e.Detail)
}

// localVerdict is what a failure says about an account, in this module's own
// vocabulary.
type localVerdict struct {
	Cause    string
	Reason   string
	Dead     bool
	Cooldown time.Duration
}

// classifyLocally maps a failure onto a verdict.  ok=false means "this module
// cannot say anything about the account from this failure"; the caller then
// decides whether the failure site is evidence enough on its own.
func classifyLocally(err error) (localVerdict, bool) {
	if err == nil {
		return localVerdict{}, false
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return localVerdict{Cause: causeTimeout, Reason: err.Error(), Cooldown: cooldownTimeout}, true
	case errors.Is(err, errBinaryNotFound):
		return localVerdict{Cause: causeCLINotFound, Reason: err.Error(), Cooldown: cooldownCLIMissing}, true
	case errors.Is(err, errNotLoggedIn):
		return localVerdict{Cause: causeNoCredential, Reason: err.Error(), Dead: true}, true
	}

	var refused *credentialRefusedError
	if errors.As(err, &refused) {
		return localVerdict{
			Cause:  causeCredentialRefused,
			Reason: fmt.Sprintf("the vendor refused the stored credential (HTTP %d)", refused.Status),
			Dead:   true,
		}, true
	}

	var status *upstreamStatusError
	if errors.As(err, &status) {
		reason := fmt.Sprintf("%s answered HTTP %d", status.Where, status.Status)
		if status.Status == http.StatusUnauthorized || status.Status == http.StatusForbidden {
			// The vendor rejected the credential itself: retrying it changes
			// nothing until the operator signs in again.  Every other status --
			// including 429 and 5xx -- is treated as transient.
			return localVerdict{Cause: causeHTTPStatus, Reason: reason, Dead: true}, true
		}
		return localVerdict{Cause: causeHTTPStatus, Reason: reason, Cooldown: cooldownUpstream}, true
	}
	return localVerdict{}, false
}

// noteFailure remembers a failure against an account, classifying what can be
// classified locally.  fallbackCooldown > 0 means "at this call site an
// unclassified failure is still evidence that the account cannot serve right
// now" (the CLI runner, where a child process this module started failed).  A
// zero fallbackCooldown records the reason only and leaves the account
// selectable, which is what an unclassifiable failure deserves.
func (c *Client) noteFailure(id string, err error, fallbackCause string, fallbackCooldown time.Duration) {
	if err == nil || strings.TrimSpace(id) == "" {
		return
	}
	if errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		// The caller walked away.  That says nothing about the account.
		return
	}
	if v, ok := classifyLocally(err); ok {
		if v.Dead {
			c.markDead(id, v.Cause, v.Reason)
			return
		}
		c.markCooling(id, v.Cause, v.Reason, v.Cooldown)
		return
	}
	if fallbackCooldown > 0 {
		c.markCooling(id, fallbackCause, err.Error(), fallbackCooldown)
		return
	}
	c.rememberFailure(id, fallbackCause, err.Error())
}

// noteCLIFailure records what a finished CLI run says about the account it ran
// as.  It takes the context error separately because the timeout message this
// module builds deliberately does not wrap context.DeadlineExceeded (it is
// reported to the caller as a plain sentence), so the only place that fact is
// still available is here.
//
// A non-zero exit is evidence that this invocation failed.  It is not evidence
// of *why*, so the account cools for a bounded time instead of being parked for
// good: guessing "dead" from an unnamed process failure would take a working
// account offline until a human noticed.
func (c *Client) noteCLIFailure(id string, failure, ctxErr error) {
	if failure == nil || strings.TrimSpace(id) == "" {
		return
	}
	switch {
	case errors.Is(ctxErr, context.DeadlineExceeded):
		c.markCooling(id, causeTimeout, failure.Error(), cooldownTimeout)
	case errors.Is(ctxErr, context.Canceled):
		return
	default:
		c.noteFailure(id, failure, causeProcessFailed, cooldownProcessFailed)
	}
}

// ---------------------------------------------------------------------------
// Panel surfacing
// ---------------------------------------------------------------------------

// applyHealth folds the stored health into a panel row: the fields carry the
// raw record, a note that is not healthy gains one sentence saying why, and the
// row's state is lowered to match.  The state matters as much as the note: a
// row that reads "ready" while the request path refuses the account is the
// worst kind of lie -- the operator sees green and the next request gets a 503.
// A healthy account adds only whatever it remembers (its last_used), never a
// scary sentence, and keeps the state the row decided for itself.
func (c *Client) applyHealth(fields map[string]any, note, state, id string) (string, string) {
	rec := c.healthOf(id)
	healthFields(fields, rec)
	if rec.State == "" || rec.State == healthHealthy {
		return note, state
	}
	return healthNoteSuffix(note, c.healthNote(id)), healthStateFor(rec.State, state)
}

// healthStateFor maps the stored health onto the vocabulary the panel colours
// rows by (internal/panel/index.html tallies ready / cooling / invalid /
// exhausted), so a dead account cannot keep a green badge.  Any other health
// value keeps the row's own answer rather than inventing one.
func healthStateFor(health, state string) string {
	switch health {
	case healthDead:
		return "invalid"
	case healthCooling:
		return "cooling"
	}
	return state
}

// healthFields adds the stored health to a panel row's field map, so the
// operator can see *why* an account is cooling without opening the JSON file.
// It is deliberately additive: an account with nothing remembered adds no keys
// at all, which keeps the pre-existing rows byte-identical.
func healthFields(fields map[string]any, rec healthRecord) {
	if fields == nil || rec.State == "" {
		return
	}
	fields["health"] = rec.State
	if rec.Cause != "" {
		fields["health_cause"] = rec.Cause
	}
	if rec.CooldownUntil != "" {
		fields["cooldown_until"] = rec.CooldownUntil
	}
	if rec.LastError != "" {
		fields["last_error"] = rec.LastError
	}
	if rec.LastUsed != "" {
		fields["last_used"] = rec.LastUsed
	}
}

// healthNoteSuffix appends the health explanation to an existing note, once.
// The note and the explanation are separate sentences, so a note that does not
// end in punctuation gets one: the panel renders this verbatim under the
// account id, and "…over HTTPS Health: …" reads as a single run-on sentence.
func healthNoteSuffix(note, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return note
	}
	note = strings.TrimRight(note, " \t")
	if note == "" {
		return reason
	}
	sep := " Health: "
	switch note[len(note)-1] {
	case '.', '!', '?':
	default:
		sep = ". Health: "
	}
	return note + sep + reason + "."
}
