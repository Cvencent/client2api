package workbuddy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Account pool.  The reference implementation keeps its selection policy in
// internal/pool; this is an independent, much smaller implementation with the
// same observable behaviour: round-robin over usable accounts, short cooldowns
// for transient failures, long cooldowns for credit exhaustion, and per-account
// health exposed through Status().Accounts.
//
// On top of that account-level policy this pool tracks a per-model cooldown
// (see modelCooldown) and offers a scored selection path (PickForModel).  The
// model-agnostic path -- Pick -- deliberately keeps its historic deterministic
// rotation, so callers that know nothing about models see no change.

const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// reserveCreditNote marks the park the low-balance guard created.  It is a
// machine word, not an ErrKind: the guard is our policy rather than the
// vendor's verdict, and the panel translates it for the operator.
const reserveCreditNote = "low_credit"

const (
	// longCreditCooldown parks an account whose credits are gone.  WorkBuddy
	// grants are per account, so another account may still work.
	longCreditCooldown = 6 * time.Hour
	// accountFaultCooldown parks an account the upstream refuses to serve
	// (trial not activated, request illegal).
	accountFaultCooldown = 12 * time.Hour
	// wafCooldown parks an account that tripped the edge WAF.
	wafCooldown = 30 * time.Minute
	// invalidCooldown is used when the refresh token itself was rejected.
	invalidCooldown = time.Hour
	// shortCooldown is the fallback for transient failures without a
	// Retry-After hint.
	shortCooldown = 20 * time.Second
	// softRateCooldown is the fallback for a rate limit without a hint.
	softRateCooldown = 60 * time.Second
	// notFoundCooldown parks an account after an upstream 404.  It is
	// deliberately its own constant rather than reusing the soft-rate one: a
	// 404 is an occasionally missing path, not a statement that this account is
	// being rate limited, and charging it the escalating soft-rate penalty would
	// bench a healthy account for ten minutes and double that on the next
	// hiccup.  The reference names the same constant and the same reason
	// (internal/server/handler.go: notFoundCooldown).
	notFoundCooldown = 60 * time.Second
)

// Model-level cooldown policy.  A vendor can refuse one *model* on an account
// while still serving the account's other models, and parking the whole
// credential for that would take a healthy account out of rotation.  These are
// the reference's numbers (internal/pool/cooldown.go):
//
//	modelBlockBaseTTL = 6h, doubling per consecutive hit, capped at 24h
//
// The base is a guess, not a vendor figure: code 11102 carries no reset hint,
// so the only honest thing to do is start at the reference's six hours and back
// off geometrically while the same account keeps failing the same model.
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
	// modelBlockBackoffReason prefixes the reason recorded for a 11102 park.
	// BlockModelClear keys off this prefix so that clearing a "model is gone"
	// park never clears a "model is busy until <time>" one.
	modelBlockBackoffReason = ModelBlockReason
	// softRateMax is the module-wide soft ceiling, the reference's
	// defaultSoftRateMax (internal/pool/cooldown.go).  It bounds every soft
	// (rate-limit) cooldown, whether it parks one model or the whole account:
	// the vendor's own reset hint is honoured up to this ceiling, and beyond it
	// we distrust the hint rather than park for a day on one message.
	softRateMax = 2 * time.Hour
	// softModelMax caps a per-model soft rate limit park.  It is the same
	// ceiling as softRateMax; the alias exists only to keep the model-path
	// call sites readable.
	softModelMax = softRateMax
	// softStreakShiftMax caps the doubling of the hintless backoff, exactly as
	// the reference does.
	softStreakShiftMax = 16
)

// Session-death policy.  Code 12153 ("Offline user session not found") is
// usually a stale token that a refresh repairs, which is why the first strikes
// only cool the account briefly.  Once the same account reports it three times
// in a row the refresh is not fixing anything, and the reference parks the
// credential (internal/pool/entry.go: sessionDeadThreshold = 3).
const (
	sessionDeadThreshold = 3
	// sessionDeadReason is the reference's operator-facing label.
	sessionDeadReason = "12153 session dead"
)

// Selection policy.  See weightOf.
const (
	// idleWeightPerHour / idleWeightMax give an account nobody has used lately
	// a bounded advantage.  The ceiling matters: without it a credential idle
	// for a month would win every draw forever.
	idleWeightPerHour = 0.5
	idleWeightMax     = 5.0
	// failWeightPenalty is subtracted per recorded failure, capped at
	// maxFailPenalty, so a battered account is deprioritised without being
	// excluded (it is still usable, and may be the only one left).
	failWeightPenalty = 0.25
	maxFailPenalty    = 1.0
	// minWeight is the floor a score can never fall below.  Every candidate
	// must keep some share of the rotation.
	minWeight = 0.1
	// minPickGap is how recently an account must have been picked before it is
	// deprioritised for another concurrent pick.  Two requests arriving in the
	// same millisecond must not both land on the account the other just used.
	minPickGap = 100 * time.Millisecond
)

// Account-level penalty policy.  The reference splits an account's health into
// three independent axes (internal/pool/entry.go, transition.go): a classified
// cooldown of known length, a circuit breaker for classified failures whose
// length the upstream never told us, and a degrade park for failures we could
// not classify at all.  The live values travel in core.FaultPolicy; the numbers
// below are only what NewPool starts from when the configuration says nothing.
const (
	// defaultBreakerThreshold is how many consecutive classified failures trip
	// the breaker.  Three is the reference's number: one failure is noise, two
	// is a coincidence, three is a pattern.
	defaultBreakerThreshold = 3
	// defaultBreakerCooldown is the first breaker park, doubling on every
	// subsequent trip up to defaultBreakerCooldownMax.
	defaultBreakerCooldown    = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
	// defaultDegradeThreshold is how many consecutive *unclassified* failures
	// park the account.  It is higher than the breaker's because an
	// unclassified failure is weaker evidence: we do not know that the account
	// was at fault.
	defaultDegradeThreshold   = 5
	defaultDegradeCooldown    = 10 * time.Minute
	defaultDegradeCooldownMax = 2 * time.Hour
)

// Model-cost policy.  A pick prefers the model's cheapest measured tier, which
// is how a free account is discovered in the first place; see costTierOf.
const (
	// defaultCostExploreInterval is how often a model whose cheapest known tier
	// is "measured free" may instead be sent to an unmeasured account, so the
	// ledger can find a second free tier.  It is the reference's value
	// (internal/pool/entry.go): at most 48 detours a day per model, and a
	// detour adds no upstream request at all -- it only redirects one that had
	// to be made anyway, so the risk is a possible charge rather than a new
	// call.  Zero disables the detour.
	defaultCostExploreInterval = 30 * time.Minute
	// modelCostTTL bounds how long a measurement is believed.  A vendor's
	// promotional window is a property of the time of day, so an observation
	// older than this must not steer a pick (internal/pool/entry.go).
	modelCostTTL = 6 * time.Hour
)

// hardCreditHour is the hour of day the vendor's quota day rolls over at.  A
// credit-exhausted account is parked until the next 04:00 rather than for a
// fixed window (the reference's CooldownUntilTomorrow4AM): sign-in grants land
// at 09:00 and 21:00, so waiting for 04:00 is what lets the *same* quota day
// recover the account.
const hardCreditHour = 4

// modelCostEntry is one account's measured cost for one model, smoothed across
// samples.  CostPer1k <= 0 means the upstream charged nothing for the requests
// that produced it, which is the strongest preference a pick can express.
type modelCostEntry struct {
	CostPer1k float64
	LastSeen  time.Time
	Samples   int
}

// nextDay4AM returns the next 04:00 in the given location.  Inside the
// 00:00-04:00 window it returns the *same* day's 04:00: the grants that will
// revive the account are still ahead of it, and returning tomorrow would idle
// the account for nearly a day for no reason.  time.Date handles month and year
// overflow, so no special case is needed for 31 December.
func nextDay4AM(now time.Time) time.Time {
	y, m, d := now.Date()
	at := time.Date(y, m, d, hardCreditHour, 0, 0, 0, now.Location())
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// cooldownFor maps a classified failure to a cooldown and the state to show.
// It is a pure function so the policy is directly testable.
func cooldownFor(kind ErrKind, retryAfter time.Duration) (time.Duration, string) {
	switch kind {
	case ErrHardCredit:
		return longCreditCooldown, stateExhausted
	case ErrAccountFault:
		return accountFaultCooldown, stateExhausted
	case ErrWafBlock:
		return wafCooldown, stateCooling
	case ErrSessionDead:
		return 5 * time.Minute, stateCooling
	case ErrSoftRate:
		if retryAfter > 0 && retryAfter <= retryAfterSanity {
			return retryAfter, stateCooling
		}
		return softRateCooldown, stateCooling
	case ErrServer:
		if retryAfter > 0 && retryAfter <= retryAfterSanity {
			return retryAfter, stateCooling
		}
		return shortCooldown, stateCooling
	case ErrNotFound:
		// A 404 gets the fixed not-found park, not the short transient one and
		// not the escalating soft-rate one: the reference routes it to its own
		// constant for exactly this reason.  A Retry-After hint still wins,
		// because a header is the vendor naming its own wait.
		if retryAfter > 0 && retryAfter <= retryAfterSanity {
			return retryAfter, stateCooling
		}
		return notFoundCooldown, stateCooling
	default:
		return 0, stateReady
	}
}

// retryableKind reports whether trying another account could plausibly help.
func retryableKind(kind ErrKind) bool {
	switch kind {
	case ErrHardCredit, ErrAccountFault, ErrWafBlock, ErrSessionDead, ErrSoftRate, ErrServer, ErrNotFound:
		return true
	default:
		// ErrModelBlocked is a model/account mismatch, and the remaining kinds
		// are request problems: another account would fail identically.
		return false
	}
}

// modelCooldown is one account's park of one model.  It is kept apart from the
// account-level cooldown because the two have different lifetimes and different
// audiences: while this is in force the account may still serve every other
// model.
type modelCooldown struct {
	// Until is when the park lapses.
	Until time.Time
	// ResetAt is the vendor's own reset wall clock, when it gave one.  It is
	// kept because "the vendor said 14:30" tells an operator more than "41
	// minutes left".
	ResetAt time.Time
	Reason  string
	// Hits counts consecutive parks of this model on this account; it drives
	// the code 11102 backoff.
	Hits int
}

type poolEntry struct {
	auth  *Auth
	state string
	until time.Time
	note  string
	fails int

	// sessionDeadFails counts consecutive ErrSessionDead answers; it is what
	// escalates a token that a refresh no longer repairs into a parked
	// credential.  It is runtime-only: a restart resets the count, which only
	// delays the decision, it does not lose it (the park itself is persisted).
	sessionDeadFails int

	// modelCool parks individual models; see modelCooldown.  pruneModelCool
	// keeps the map bounded.
	modelCool map[string]modelCooldown

	// faults carries the account-level breaker and degrade deadlines.  They sit
	// beside state/until rather than replacing them: until is a cooldown whose
	// length the upstream told us, while these two are the penalties for
	// failures whose length nobody knows.  usable() ORs all three, so the
	// effective penalty is whichever deadline is furthest out and the
	// mechanisms coexist without summing.
	faults core.FaultTracker

	// credits is the last balance this account reported, with the part of it
	// that lapses inside the configured window and the earliest such batch.
	// preferExpiring routes by that deadline so a grant about to lapse is
	// spent before a later one, and ReenableIfCredits uses the balance to
	// unfreeze an account that was parked for credit exhaustion.
	credits               int64
	creditsTotal          int64
	creditsExpiring       int64
	creditsEarliestExpiry time.Time
	creditsEarliestRemain int64
	creditsKnown          bool

	// modelCost is the measured per-1k-token cost of each model on this
	// account, written from real upstream usage.  It is what lets a pick
	// prefer a model's free tier and what the cost-explore detour probes.
	modelCost map[string]modelCostEntry

	// errTotal, lastErr, successCount and lastSuccess are reporting only: the
	// reference exposes them in its per-account status and the panel shows
	// them.
	errTotal     int
	lastErr      time.Time
	successCount int
	lastSuccess  time.Time

	// lastUsed and usedSeq drive the scoring and the concurrency tie-break.
	// usedSeq is monotonic because time.Now() resolves to roughly 0.5ms on
	// Windows, which cannot order two picks made back to back.
	lastUsed time.Time
	usedSeq  uint64

	// current is this account's smooth-weighted-round-robin accumulator: the
	// thing that turns a weight into a share of a long rotation.  See
	// pickWeightedLocked.
	current float64

	// inFlight is how many upstream requests are running on this account right
	// now.  It is guarded by Pool.mu rather than being an atomic: every path
	// that reads it (a pick, a snapshot, a health summary) already holds that
	// lock, and a second concurrency model in this file would be one thing too
	// many to keep correct.
	inFlight int
}

// Pool holds the discovered accounts and their health.
type Pool struct {
	mu            sync.Mutex
	entries       []*poolEntry
	cursor        int
	expiringSoon  time.Duration
	refreshWindow time.Duration
	now           func() time.Time

	// Runtime persistence; see poolstate.go.  An empty dir means memory-only.
	dir     string
	logf    func(string, ...any)
	pending map[string]persistedPoolAccount
	// dirty records that a health transition has happened since the last
	// successful write.  MarkSuccess runs on every successful request, and
	// rewriting the file to say "still ready" would be pure cost.
	dirty bool
	// wrote records that a file exists, so that falling back to "no account
	// has anything to remember" still writes the emptiness out instead of
	// leaving a stale document behind.
	wrote bool

	// pickSeq orders picks monotonically.  pickWeightedLocked uses it as the
	// tie-break when every candidate was used inside minPickGap, where wall
	// clock resolution is not enough to tell them apart.
	pickSeq uint64

	// maxInFlight and maxInFlightGlobal are the per-account ceilings on
	// concurrent upstream requests, injected from the configuration.  0 means
	// "no ceiling" for the first and "not set, fall back" for the second; see
	// inFlightLimitLocked.  Both are runtime values: they are never persisted,
	// because a restart should not inherit a ceiling the operator has since
	// raised.
	maxInFlight       int
	maxInFlightGlobal int

	// policy is the live account-level penalty policy; see core.FaultPolicy and
	// the default* constants above.  It is replaced wholesale by SetPolicy,
	// which the configuration and a live reload both call.
	policy core.FaultPolicy

	// idlePerHour and idleMax tune the idle bonus in weightOf.  They are
	// injected rather than compiled in so an operator can decide how strongly
	// to favour a rested account; 0 means "keep the default".
	idlePerHour float64
	idleMax     float64

	// softRateMax bounds every soft (rate-limit) cooldown, whether it parks one
	// model or the whole account.  0 means "keep the default".
	softRateMax time.Duration

	// preferExpiring routes to the account whose credit lapses soonest.  The
	// reference defaults it on: a grant that expires unspent is simply lost.
	preferExpiring bool

	// reserveCredits is the low-balance guard: an account whose last known
	// balance is at or below it is parked until the balance rises above it.
	// Zero is the default and means "a known zero balance parks"; a negative
	// value disables the guard.  An unknown balance never parks, because
	// "never measured" is not "empty".
	reserveCredits int64

	// costExploreInterval is how often a model whose cheapest known tier is
	// "measured free" may be sent to an unmeasured account instead.  Zero
	// disables the detour.  exploreLast keys the timer by realm and model, and
	// exploreEvents counts detours for the panel.
	costExploreInterval time.Duration
	exploreLast         map[string]time.Time
	exploreEvents       int64
}

// log reports a persistence problem.  It never fails a request.
func (p *Pool) log(format string, args ...any) {
	if p == nil || p.logf == nil {
		return
	}
	p.logf(format, args...)
}

// NewPool builds a pool over the given accounts.
func NewPool(accounts []*Auth, refreshWindow time.Duration) *Pool {
	p := &Pool{
		refreshWindow: refreshWindow,
		now:           time.Now,
		policy:        core.DefaultFaultPolicy(),
		idlePerHour:   idleWeightPerHour,
		idleMax:       idleWeightMax,
		softRateMax:   softRateMax,
		// A grant that lapses unspent is lost, so the reference defaults
		// prefer_expiring to true (cmd/server/config.go).
		preferExpiring:      true,
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
	}
	p.Replace(accounts)
	return p
}

// Replace merges a freshly loaded account list, preserving the health state of
// accounts that are still present.
func (p *Pool) Replace(accounts []*Auth) {
	if p == nil {
		return
	}
	if p.now == nil {
		p.now = time.Now
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := map[string]*poolEntry{}
	for _, e := range p.entries {
		if e.auth != nil {
			previous[e.auth.ID()] = e
		}
	}
	entries := make([]*poolEntry, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if old, ok := previous[a.ID()]; ok {
			old.auth = a
			entries = append(entries, old)
			continue
		}
		fresh := &poolEntry{auth: a, state: stateReady}
		p.applyStagedLocked(fresh)
		entries = append(entries, fresh)
	}
	p.entries = entries
	if p.cursor >= len(p.entries) {
		p.cursor = 0
	}
	// A merge can retire a record (an account whose cooldown was the only thing
	// in the file) or hand a staged one out, so it always recomputes the
	// document; saveLocked decides whether that is actually a change.
	p.dirty = true
	p.saveLocked()
}

// Len returns the number of accounts.
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Accounts returns a snapshot of the credentials.
func (p *Pool) Accounts() []*Auth {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Auth, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.auth)
	}
	return out
}

// usable reports whether an entry can be used at the given time.
//
// Three penalties gate it and they are ORed, not summed: the classified
// cooldown in until, the circuit breaker and the degrade park.  That is what
// makes "the mechanisms coexist and the longest one wins" true by construction
// rather than by arithmetic -- whichever deadline is furthest out is the one
// that keeps the account parked.
func (e *poolEntry) usable(now time.Time) bool {
	if e == nil || e.auth == nil {
		return false
	}
	if e.faults.Blocked(now) {
		return false
	}
	if e.state == stateInvalid {
		return now.After(e.until)
	}
	return !now.Before(e.until)
}

// Ready reports whether at least one account is usable right now.
func (p *Pool) Ready() bool {
	if p == nil {
		return false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.usable(now) {
			return true
		}
	}
	return false
}

// Pick returns the next usable account, round-robin, skipping the accounts in
// skip.  ok is false when no account can be used.
//
// This is the model-agnostic path and it is deliberately unchanged: callers
// that know nothing about models keep the deterministic rotation they have
// always had, and the pool's own tests pin it.  A caller that does know the
// model should use PickForModel, which adds the per-model health check and the
// scored draw on top.
func (p *Pool) Pick(skip map[string]bool) (*Auth, bool) {
	if p == nil {
		return nil, false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(skip, now)
}

// realmMatches reports whether an entry may serve a request aimed at realm.  An
// empty realm means "the caller did not name one", which admits every account:
// that is the behaviour every pre-realm caller had, and it is what makes the
// bare-name path a preference rather than a hard route.
//
// The realm lives on the credential, not on the pool entry, so this is the only
// correct place to read it -- a cached copy on the entry would go stale the
// moment an account was re-imported under a different realm.
func realmMatches(e *poolEntry, realm string) bool {
	if realm == "" {
		return true
	}
	if e == nil || e.auth == nil {
		return false
	}
	return e.auth.RealmName() == realm
}

// pickLocked is the historical round-robin, shared by Pick and by the
// all-models-cooling fallback of PickForModel.  The caller holds p.mu.
func (p *Pool) pickLocked(skip map[string]bool, now time.Time) (*Auth, bool) {
	return p.pickLockedRealm(skip, "", now)
}

// pickLockedRealm is pickLocked restricted to one realm, and is where the
// rotation actually lives.  The caller holds p.mu.
func (p *Pool) pickLockedRealm(skip map[string]bool, realm string, now time.Time) (*Auth, bool) {
	n := len(p.entries)
	if n == 0 {
		return nil, false
	}
	best, found := 0, false
	for _, e := range p.entries {
		if e == nil || e.auth == nil || !e.usable(now) || !realmMatches(e, realm) {
			continue
		}
		if p.fullLocked(e) {
			continue
		}
		if skip[e.auth.ID()] {
			continue
		}
		prio := core.AccountPriority("workbuddy", e.auth.ID())
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return nil, false
	}
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		e := p.entries[idx]
		if !e.usable(now) {
			continue
		}
		if !realmMatches(e, realm) {
			continue
		}
		if core.AccountPriority("workbuddy", e.auth.ID()) != best {
			continue
		}
		// A full account is skipped like an unusable one: the rotation has to
		// spread the burst, and returning a credential that Acquire would
		// immediately refuse would turn backpressure into a failed request.
		if p.fullLocked(e) {
			continue
		}
		// Cheap lazy sweep: the pick path is the one place every account is
		// walked, so it is where lapsed per-model parks are dropped.
		e.pruneModelCool(now)
		if e.auth != nil && skip[e.auth.ID()] {
			continue
		}
		p.cursor = (idx + 1) % n
		p.markPickedLocked(e, now)
		return e.auth, true
	}
	return nil, false
}

// Find returns the named account when it is present, usable and not skipped.
// It is what the conversation-affinity path calls after the binding table has
// confirmed the account is still usable: resolving the binding and then
// picking must not be able to disagree.
func (p *Pool) Find(skip map[string]bool, accountID string) (*Auth, bool) {
	if p == nil || accountID == "" {
		return nil, false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil || e.auth.ID() != accountID {
			continue
		}
		if !e.usable(now) || skip[accountID] {
			return nil, false
		}
		if p.fullLocked(e) {
			// Occupancy is not health: the binding stays valid and the next
			// request for the same conversation lands here again, but this one
			// has to go to another account.
			return nil, false
		}
		p.markPickedLocked(e, now)
		return e.auth, true
	}
	return nil, false
}

// SetMaxInFlight is the per-account ceiling on concurrent upstream requests.
// It keeps the reference's exact semantics: 0 means "no ceiling", and a
// negative value means "the configuration said nothing", so the ceiling already
// in force survives a partial reload instead of being cleared.
func (p *Pool) SetMaxInFlight(n int) {
	if p == nil || n < 0 {
		return
	}
	p.mu.Lock()
	p.maxInFlight = n
	p.mu.Unlock()
}

// SetMaxInFlightGlobal is the ceiling for the realm whose risk control is
// stricter (the reference added it for the WAF 403s only that realm produced).
// 0 means "not set": the tier then falls back to the plain ceiling, because
// "unset" and "allow nothing" must not be the same value.
func (p *Pool) SetMaxInFlightGlobal(n int) {
	if p == nil || n < 0 {
		return
	}
	p.mu.Lock()
	p.maxInFlightGlobal = n
	p.mu.Unlock()
}

// Limits reports the ceilings in force: per account, and for the stricter
// realm.  The startup log and the tests both want to see the live values.
func (p *Pool) Limits() (perAccount, global int) {
	if p == nil {
		return 0, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.maxInFlight, p.maxInFlightGlobal
}

// inFlightLimitLocked is the ceiling that applies to one entry.  The caller
// holds p.mu.
func (p *Pool) inFlightLimitLocked(e *poolEntry) int {
	if p.maxInFlightGlobal > 0 && e != nil && e.auth != nil && e.auth.IsGlobal() {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// fullLocked reports whether one entry has no free slot.  A ceiling of 0 is
// "unlimited" and can never be full, which is what keeps the behaviour of an
// unconfigured pool exactly what it was before ceilings existed.
func (p *Pool) fullLocked(e *poolEntry) bool {
	if e == nil {
		return false
	}
	limit := p.inFlightLimitLocked(e)
	return limit > 0 && e.inFlight >= limit
}

// Acquire takes one in-flight slot on a.  It returns false when the account is
// at its ceiling; the caller then uses another account rather than queueing,
// because queueing would still deliver the burst the ceiling exists to avoid.
//
// An account that is not in the pool cannot hold a slot and is admitted: the
// pool counts what it owns, and refusing an unknown credential here would
// invent a limit the operator never set.
func (p *Pool) Acquire(a *Auth) bool {
	if p == nil || a == nil {
		return true
	}
	id := a.ID()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil || e.auth.ID() != id {
			continue
		}
		if p.fullLocked(e) {
			return false
		}
		e.inFlight++
		return true
	}
	return true
}

// Release returns a slot taken by Acquire.  It is idempotent: releasing an
// account that holds nothing is a no-op, so a response body that is closed
// twice cannot drive the count below zero and quietly raise the real ceiling.
func (p *Pool) Release(a *Auth) {
	if p == nil || a == nil {
		return
	}
	id := a.ID()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil || e.auth.ID() != id {
			continue
		}
		if e.inFlight > 0 {
			e.inFlight--
		}
		return
	}
}

// InFlight reports the live in-flight total and how many usable accounts are at
// their ceiling.  It counts exactly what Acquire counts, because a panel that
// disagreed with the admission decision would be worse than no panel.
func (p *Pool) InFlight() (total, full int) {
	if p == nil {
		return 0, 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil {
			continue
		}
		total += e.inFlight
		if e.usable(now) && p.fullLocked(e) {
			full++
		}
	}
	return total, full
}

// AllFull reports whether every usable account is at its ceiling.
//
// It separates "this module has nowhere to send the request right now"
// (backpressure: back off and retry) from "this module has no account at all"
// (a configuration problem).  A pool with no usable account returns false, so
// the caller keeps reporting the configuration problem it already had.
func (p *Pool) AllFull() bool {
	if p == nil {
		return false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	usable := 0
	for _, e := range p.entries {
		if e == nil || !e.usable(now) {
			continue
		}
		usable++
		if !p.fullLocked(e) {
			return false
		}
	}
	return usable > 0
}

// poolCounts is the census core.Health is rendered from: one pass under the
// lock, so a status read cannot see two halves of two different moments.
type poolCounts struct {
	total    int
	ready    int
	cooling  int
	disabled int
	full     int
	realms   map[string]bool // realm -> has a usable account
}

// counts walks the pool once.  "ready" is an account a request could be sent to
// right now, which is deliberately health and not occupancy: an account at its
// in-flight ceiling is still healthy, and full is reported separately.
func (p *Pool) counts() poolCounts {
	var c poolCounts
	if p == nil {
		return c
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil {
			continue
		}
		c.total++
		if e.usable(now) {
			c.ready++
			if c.realms == nil {
				c.realms = map[string]bool{}
			}
			c.realms[e.auth.RealmName()] = true
			if p.fullLocked(e) {
				c.full++
			}
			continue
		}
		// Not usable: a parked credential with no wall clock waits for an
		// operator, anything with one is cooling.
		if e.until.IsZero() {
			c.disabled++
		} else {
			c.cooling++
		}
	}
	return c
}

// UsableForModel reports whether one account may serve one model right now.
// It is the liveness predicate the affinity table is fed: an account that is
// parked, or that is cooling down for this exact model, must not be handed back
// as a sticky binding.
func (p *Pool) UsableForModel(accountID, model string) bool {
	return p.UsableForModelInRealm(accountID, model, "")
}

// UsableForModelInRealm is UsableForModel plus the realm check.  The affinity
// table is fed this predicate, and an account bound to a conversation must not
// be handed back for a model the account's realm does not serve: the binding
// would look healthy and then answer 11102 on every turn.
func (p *Pool) UsableForModelInRealm(accountID, model, realm string) bool {
	if p == nil || accountID == "" {
		return false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil || e.auth == nil || e.auth.ID() != accountID {
			continue
		}
		if !e.usable(now) {
			return false
		}
		if !realmMatches(e, realm) {
			return false
		}
		e.pruneModelCool(now)
		return !e.modelCooled(now, model)
	}
	return false
}

// PickForModel returns the next usable account for model, skipping the accounts
// in skip.
//
// The policy is the reference's, scaled to this pool:
//
//  1. Build the candidate set: usable for the account AND not parked for this
//     model.  An account whose 11102 park or 6004 rate limit is still in force
//     is not a candidate for that model, but remains one for every other.
//  2. Score the candidates (weightOf) and choose by smooth weighted round
//     robin: the highest accumulator wins and pays back the round's total, so
//     each account takes a share of the rotation equal to its score's share of
//     the sum.  Accounts used inside minPickGap score nothing for this round,
//     so two concurrent requests do not land on the same credential.
//  3. If no candidate survives, fall back to the plain round-robin.  Every
//     account being parked for this model is a real possibility; refusing to
//     serve at all would turn a model-level problem into an outage, so the
//     request still goes somewhere and the caller decides what the answer is
//     worth.
//
// An empty model means "this caller does not know the model" and delegates to
// Pick, which is what keeps the two paths honest about what they promise.
func (p *Pool) PickForModel(skip map[string]bool, model string) (*Auth, bool) {
	return p.PickForModelInRealm(skip, model, "")
}

// PickForModelInRealm is PickForModel restricted to one realm.
//
// The realm is a hard filter here, because the caller is the only layer that
// knows whether the operator actually named a realm: this function is told
// "serve this model from this realm" and answers honestly when no such account
// exists.  The soft-preference fallback for a bare model name lives in Chat,
// which owns that knowledge.  An empty realm keeps the pre-realm behaviour.
func (p *Pool) PickForModelInRealm(skip map[string]bool, model, realm string) (*Auth, bool) {
	if p == nil {
		return nil, false
	}
	if model == "" && realm == "" {
		return p.Pick(skip)
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()

	n := len(p.entries)
	if n == 0 {
		return nil, false
	}
	cands := make([]scoredEntry, 0, n)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		e := p.entries[idx]
		if e == nil || e.auth == nil || !e.usable(now) {
			continue
		}
		if !realmMatches(e, realm) {
			continue
		}
		if skip[e.auth.ID()] {
			continue
		}
		e.pruneModelCool(now)
		e.pruneModelCost(now)
		if model != "" && e.modelCooled(now, model) {
			continue
		}
		if p.fullLocked(e) {
			continue
		}
		cands = append(cands, scoredEntry{e: e, idx: idx})
	}
	cands = core.LowestPriorityTier("workbuddy", cands, func(c scoredEntry) string { return c.e.auth.ID() })
	if len(cands) == 0 {
		// Only the model-scoped path has anything to warn about: with an empty
		// model the plain rotation below is the intended answer, not a fallback.
		if model != "" {
			p.log("workbuddy: every usable account is cooling down for model %s%s; falling back to round-robin",
				model, realmSuffix(realm))
		}
		return p.pickLockedRealm(skip, realm, now)
	}

	// Prefer the model's cheapest measured tier.  A tier is free when the
	// upstream was measured to charge nothing for this model, unknown when we
	// have no observation, and charged when it was measured to charge.
	// Unknown outranks charged on purpose: a new account's free window can only
	// be found by trying it, and if a known-charged account always beat an
	// unknown one, the free accounts would never be reached -- and so never
	// learned.  This is a hard filter rather than a sort key because the draw
	// below is weighted: merely ranking charged accounts lower would still let
	// them win.
	bestTier := costTierCharged
	for _, c := range cands {
		if ti := c.e.costTierOf(model, now); ti < bestTier {
			bestTier = ti
		}
	}
	// Cost explore: when the cheapest known tier is "measured free" but an
	// unmeasured account is available, redirect this pick there once per
	// interval so the ledger can discover a second free tier.  The detour adds
	// no upstream request at all -- it only changes which account serves one
	// that had to be made anyway.  The timer is written under the same lock
	// that read it, so concurrent picks serialise and only one can pass the
	// window.
	if p.costExploreInterval > 0 && bestTier == costTierFree && model != "" {
		hasUnknown := false
		for _, c := range cands {
			if c.e.costTierOf(model, now) == costTierUnknown {
				hasUnknown = true
				break
			}
		}
		key := realm + "\x1f" + model
		if hasUnknown && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.exploreEvents++
			// The account is not known yet -- the detour only widens the pool
			// the draw below runs over -- so the line names the model and the
			// window, not a uid.  Naming a candidate here would be a lie.
			p.log("workbuddy: cost explore model=%s%s window=%s",
				model, realmSuffix(realm), p.costExploreInterval)
			bestTier = costTierUnknown
		}
	}
	if bestTier != costTierCharged {
		eligible := make([]scoredEntry, 0, len(cands))
		for _, c := range cands {
			if c.e.costTierOf(model, now) == bestTier {
				eligible = append(eligible, c)
			}
		}
		if len(eligible) > 0 {
			cands = eligible
		}
	}

	// Spend the grant that lapses soonest first: an allowance that expires
	// unspent is simply lost, while a later one keeps.  Only accounts that
	// actually reported expiring credit take part, so a pool with no balance
	// data keeps its historic rotation.
	if p.preferExpiring {
		if urgent := p.earliestExpiryLocked(cands, now); urgent != nil {
			p.markPickedLocked(urgent, now)
			return urgent.auth, true
		}
	}

	best := p.pickWeightedLocked(cands, now)
	if best == nil || best.auth == nil {
		return nil, false
	}
	p.markPickedLocked(best, now)
	return best.auth, true
}

// realmSuffix renders the realm for a log line, and renders nothing at all when
// the caller did not name one.  A stray "in realm " would be a lie.
func realmSuffix(realm string) string {
	if realm == "" {
		return ""
	}
	return " in realm " + realm
}

// Realms returns the distinct realms the pool holds accounts for, domestic
// first.  It is what lets the catalogue path ask each realm in turn instead of
// asking whichever account the rotation happened to hand out, which is the bug
// that made a CN-only model list look complete on a dual-realm install.
//
// It deliberately reports realms whose accounts are currently parked: a cooling
// account is still the reason we know the realm exists, and the caller picks a
// live account per realm anyway.
func (p *Pool) Realms() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, e := range p.entries {
		if e == nil || e.auth == nil {
			continue
		}
		seen[e.auth.RealmName()] = true
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	// Domestic first, then international, then anything a future realm adds.
	// The order is what makes the published catalogue stable between refreshes.
	for _, r := range []string{realmCN, realmGlobal} {
		if seen[r] {
			out = append(out, r)
			delete(seen, r)
		}
	}
	rest := make([]string, 0, len(seen))
	for r := range seen {
		rest = append(rest, r)
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// markPickedLocked records that an account was just handed out.  The caller
// holds p.mu.
func (p *Pool) markPickedLocked(e *poolEntry, now time.Time) {
	if e == nil {
		return
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
}

// weightOf scores one candidate.  The reference's formula weights remaining
// credits first (credits/maxCredits*10); WorkBuddy exposes no credit balance on
// the credential itself, so the credit term is absent here and the score is
// built from the two signals this module actually has:
//
//   - idleness: an account nobody has touched recently is the one whose own
//     prompt cache we are least likely to disturb, and the one least likely to
//     be sitting in a soft window.  Bounded by idleWeightMax, otherwise a
//     credential idle for a month would win every draw forever.
//   - consecutive failures: a battered account is deprioritised without being
//     excluded, because it may be the only one left.
//
// The floor is minWeight: every candidate must keep some chance of being drawn.
func (p *Pool) weightOf(e *poolEntry, now time.Time) float64 {
	if e == nil {
		return 0
	}
	idlePerHour, idleMax := p.weights()
	w := 1.0
	if e.lastUsed.IsZero() {
		w += idleMax
	} else if idle := now.Sub(e.lastUsed).Hours(); idle > 0 {
		if idle > idleMax/idlePerHour {
			idle = idleMax / idlePerHour
		}
		w += idle * idlePerHour
	}
	if e.fails > 0 {
		pen := float64(e.fails) * failWeightPenalty
		if pen > maxFailPenalty {
			pen = maxFailPenalty
		}
		w -= pen
	}
	if w < minWeight {
		w = minWeight
	}
	return w
}

// weights returns the idle-bonus parameters, falling back to the compiled-in
// defaults when the configuration left them unset.  A zero or negative value
// would make the idle bonus either infinite or inverted, so it is never used.
func (p *Pool) weights() (perHour, max float64) {
	perHour, max = p.idlePerHour, p.idleMax
	if perHour <= 0 {
		perHour = idleWeightPerHour
	}
	if max <= 0 {
		max = idleWeightMax
	}
	return perHour, max
}

// scoredEntry is one candidate: its entry, its index in p.entries (the final
// tie-break, so a tie can never depend on iteration order), and the weight this
// round gave it.
type scoredEntry struct {
	e   *poolEntry
	idx int
	w   float64
}

// pickWeightedLocked chooses one candidate.  The caller holds p.mu.
//
// The algorithm is smooth weighted round-robin, not a random draw.
//
// The reference (internal/pool/pick.go:331) draws stochastically from the top
// five by weight.  This module cannot: its long-standing tests pin the exact
// rotation -- which credential serves the first request, which one is tried
// second after a failover.  A uniform draw among equal weights makes that first
// pick a coin flip and broke those tests with no functional gain.  SWRR keeps
// what the weights are for -- an account with more idle time and fewer failures
// takes a larger share of the rotation, in exact proportion -- while staying
// deterministic, so a homogeneous pool rotates exactly as it always did.
//
// Mechanics: every candidate's accumulator grows by its weight, the highest
// accumulator wins, and the winner's accumulator drops by the round's total
// weight.  Over a run of rounds each account's share converges on its weight
// over the sum; when the weights are all equal it degenerates to plain
// alternation.  minPickGap is applied by scoring a just-used account zero for
// this round: it still takes part, it just cannot win.
func (p *Pool) pickWeightedLocked(cands []scoredEntry, now time.Time) *poolEntry {
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0].e
	}
	var total float64
	for i := range cands {
		w := p.weightOf(cands[i].e, now)
		if !cands[i].e.lastUsed.IsZero() && now.Sub(cands[i].e.lastUsed) < minPickGap {
			w = 0
		}
		cands[i].w = w
		cands[i].e.current += w
		total += w
	}
	best := 0
	for i := 1; i < len(cands); i++ {
		if betterCandidate(cands[i], cands[best]) {
			best = i
		}
	}
	if total > 0 {
		cands[best].e.current -= total
	}
	return cands[best].e
}

// betterCandidate reports whether a should win over b: the fuller accumulator
// first, then the account used longest ago, then the pool's own order.
func betterCandidate(a, b scoredEntry) bool {
	if a.e.current != b.e.current {
		return a.e.current > b.e.current
	}
	if a.e.usedSeq != b.e.usedSeq {
		return a.e.usedSeq < b.e.usedSeq
	}
	return a.idx < b.idx
}

// MarkSuccess clears any cooldown.
//
// A served request is also the only evidence that an account has recovered, so
// it clears the breaker and the degrade counter too.  That makes the two
// penalty axes self-healing without a second wiring step: every existing
// MarkSuccess call site -- chat, check-in, every batch chore -- already reports
// the successes the axes need to hear about.
func (p *Pool) MarkSuccess(a *Auth) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.findLocked(a); e != nil {
		now := p.now()
		e.successCount++
		e.lastSuccess = now
		// Blocked rather than the raw deadlines: an expired breaker is not a
		// transition worth writing the state file over, and this runs on every
		// successful request.
		wasBlocked := e.faults.Blocked(now)
		e.faults.NoteSuccess()
		// This runs on every successful request, so only a real transition is
		// worth marking: an account that is already ready stays unwritten.
		if wasBlocked || e.state != stateReady || !e.until.IsZero() || e.note != "" || e.fails != 0 || e.sessionDeadFails != 0 {
			e.state = stateReady
			e.until = time.Time{}
			e.note = ""
			e.fails = 0
			e.sessionDeadFails = 0
			p.dirty = true
		}
	}
	p.saveLocked()
}

// Revive clears every runtime penalty recorded for one account: the cooldown or
// park (with its exhausted/invalid verdict), the failure counter, the
// session-death streak and every per-model park.  It is what the panel's
// "revive" button means, so it is the one transition that is allowed to clear
// state a MarkSuccess would never touch -- a 12153 park whose deadline has not
// passed, or a model park the operator does not believe.
//
// It reports whether the pool knows the account at all.  An unknown id is the
// caller's 404, so a silent no-op would be wrong: the operator would be told the
// account was revived while nothing in the pool had changed.
func (p *Pool) Revive(a *Auth) bool {
	if p == nil || a == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return false
	}
	e.state = stateReady
	e.until = time.Time{}
	e.note = ""
	e.fails = 0
	e.sessionDeadFails = 0
	e.modelCool = nil
	// The breaker and the degrade park are penalties like any other, so the
	// operator's override has to reach them; a revive that left a six-hour
	// breaker standing would look broken from the panel.
	e.faults.Revive()
	// A revived account starts a fresh rotation: an accumulator carried over
	// from before the park would bias its share for rounds to come.
	e.current = 0
	// A record still staged from disk would re-park the account on the next
	// Replace, so the override has to reach it too.
	delete(p.pending, e.auth.ID())
	// Unlike MarkSuccess this always marks the document: the point of a revive
	// is to make the cleared verdict durable, even when the entry already
	// looked healthy in memory and only the file still said otherwise.
	p.dirty = true
	p.saveLocked()
	return true
}

// MarkSuccessForModel is MarkSuccess for a request whose model is known.  It
// clears the account state exactly as MarkSuccess does and additionally lifts a
// "this model is not available on this account" park for that model, because
// the success is direct evidence that the park is stale.
//
// Parks for other models are deliberately left alone: a success on model N says
// nothing about model M.  A rate-limit park is left alone too, because it
// encodes the vendor's own reset and a success elsewhere must not cut that
// window short.
func (p *Pool) MarkSuccessForModel(a *Auth, model string) {
	if p == nil || a == nil {
		return
	}
	p.MarkSuccess(a)
	if model == "" {
		return
	}
	p.ClearModelCooldown(a, model)
}

// MarkFailure applies the cooldown policy for a classified failure.
//
// The returned duration is the park this failure earned: for an account-level
// failure it is the account's cooldown, and for the escalating session-dead
// case it is the credential park.  The kind is returned unchanged so that the
// caller's retry decision is unaffected.
func (p *Pool) MarkFailure(a *Auth, err error) (ErrKind, time.Duration) {
	if p == nil || a == nil {
		return ErrNone, 0
	}
	kind, retryAfter, _, resetAt := failureDetail(err)
	d, state := cooldownFor(kind, retryAfter)
	if kind == ErrHardCredit {
		// Out of credit is not a duration the vendor gave us; it is a wait for
		// the next quota day.  The quota turns over at 04:00 and the sign-in
		// grants that refill a spent account land at 09:00 and 21:00, so a
		// fixed window can expire inside the very day that has nothing left and
		// buy another hard-credit answer for the trouble.
		if until := nextDay4AM(p.now()); until.After(p.now()) {
			d, state = until.Sub(p.now()), stateExhausted
		}
	}
	if kind == ErrSoftRate {
		// The vendor's soft-rate messages date their own reset ("... 将在 <t>
		// 重置").  When we have that instant, honouring it beats any backoff we
		// could invent -- it is the vendor telling us exactly when it will
		// serve the account again.
		if soft, ok := p.softAccountCooldown(p.now(), resetAt); ok {
			d, state = soft, stateCooling
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.findLocked(a); e != nil {
		e.fails++
		if kind == ErrSessionDead {
			// A dead session is usually a stale token that a refresh repairs,
			// which is why the first strikes only cool the account briefly.
			// Once the same account reports it sessionDeadThreshold times in a
			// row the refresh is not repairing anything and the credential is
			// parked, exactly as the reference does.
			e.sessionDeadFails++
			if e.sessionDeadFails >= sessionDeadThreshold {
				e.state = stateInvalid
				e.until = p.now().Add(invalidCooldown)
				e.note = sessionDeadReason
				e.modelCool = nil
				p.dirty = true
				p.saveLocked()
				p.log("workbuddy: account %s reported a dead session %d times in a row; parked for %v",
					core.MaskSecret(a.ID()), e.sessionDeadFails, invalidCooldown)
				return kind, invalidCooldown
			}
		} else if kind != ErrNone {
			// Any other classified answer proves the session is alive.
			e.sessionDeadFails = 0
		}
		if d > 0 {
			e.until = p.now().Add(d)
			e.state = state
			// Parking the whole credential makes per-model entries moot, and
			// keeping them would leave a park outliving the account park it is
			// subsumed by.
			e.modelCool = nil
			// A parked account sits out the rotation rounds its accumulator is
			// measured over, so the accumulator is reset rather than left
			// carrying a surplus or a deficit from before the park.
			e.current = 0
		}
		// Feed whichever penalty axis this failure belongs to.  Folding it in
		// here rather than adding a second call site is deliberate: MarkFailure
		// already holds both the classified kind and the original error, and a
		// separate feeder is one that a future call site can forget.
		p.noteFaultLocked(e, err, p.now())
		e.note = kind.String()
		p.dirty = true
	}
	p.saveLocked()
	return kind, d
}

// failureDetail pulls the classified parts out of an error.  An error that was
// never classified -- a context cancellation, a local build failure -- yields
// ErrNone and no detail, which is what keeps such failures from parking an
// account.
func failureDetail(err error) (kind ErrKind, retryAfter time.Duration, modelScoped bool, resetAt time.Time) {
	var ue *Error
	if asError(err, &ue) {
		kind = ue.Kind
		retryAfter = ue.RetryAfter
		modelScoped = ue.ModelScoped
		resetAt = ue.ResetAt
	}
	return
}

// MarkFailureForModel applies the cooldown policy for a classified failure when
// the model is known.
//
// Two failures are model-scoped rather than account-scoped, and both come
// straight out of the vendor's own vocabulary:
//
//   - ErrModelBlocked, business code 11102 ("service info not found"), means
//     this account cannot use this model.  The account can still serve every
//     other model, so parking it would take a healthy credential out of
//     rotation over a request-level property.
//   - ErrSoftRate carrying the model-scoped business code 6004 is a rate limit
//     the message dates itself.  Only that model is parked, until the vendor's
//     own reset.
//
// Everything else falls through to MarkFailure, so account-level policy is
// exactly what it was.  The returned duration is the park that was applied --
// account-level when the failure was account-level, model-level otherwise -- and
// is zero when nothing was parked.
func (p *Pool) MarkFailureForModel(a *Auth, model string, err error) (ErrKind, time.Duration) {
	if p == nil || a == nil {
		return ErrNone, 0
	}
	kind, retryAfter, modelScoped, resetAt := failureDetail(err)
	if model != "" {
		switch {
		case kind == ErrModelBlocked:
			return kind, p.MarkModelBlocked(a, model, ModelBlockReason)
		case kind == ErrSoftRate && modelScoped:
			return kind, p.MarkModelRateLimited(a, model, resetAt, retryAfter, modelRateLimitReason)
		}
	}
	return p.MarkFailure(a, err)
}

// --- per-model cooldown ----------------------------------------------------

// modelRateLimitReason labels a 6004 park.  Like ModelBlockReason it is a fixed
// operator-facing string: the vendor's own text is not kept, because it can
// echo request material.
const modelRateLimitReason = "6004 model rate limited"

// modelCooled reports whether one model is currently parked on this account.
// An empty model is never parked: "unknown model" must not match a park that
// was recorded for a real one.
func (e *poolEntry) modelCooled(now time.Time, model string) bool {
	if e == nil || model == "" || len(e.modelCool) == 0 {
		return false
	}
	mc, ok := e.modelCool[model]
	if !ok || mc.Until.IsZero() {
		return false
	}
	return now.Before(mc.Until)
}

// pruneModelCool drops lapsed parks.  Without it the map is unbounded in the
// number of distinct models an account ever failed on -- an account that
// wandered through a model catalogue would accumulate an entry per model it hit
// once.  It is called on the pick path (under the write lock) and on every
// read that renders state, so the map only ever holds live parks.
func (e *poolEntry) pruneModelCool(now time.Time) {
	if e == nil || len(e.modelCool) == 0 {
		return
	}
	for m, mc := range e.modelCool {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCool, m)
		}
	}
}

// parkModel stores a per-model park.  The caller holds p.mu.
func (e *poolEntry) parkModel(model string, mc modelCooldown) {
	if e == nil || model == "" {
		return
	}
	if e.modelCool == nil {
		e.modelCool = map[string]modelCooldown{}
	}
	e.modelCool[model] = mc
}

// MarkModelBlocked parks one model on one account after the vendor said the
// model does not exist there (business code 11102).
//
// There is no reset hint to honour -- the answer is deterministic, not a
// window -- so the park backs off geometrically: modelBlockBaseTTL on the first
// hit, doubled per consecutive hit, capped at modelBlockMaxTTL.  The reference
// chose those numbers (internal/pool/cooldown.go: modelBlockBaseTTL = 6h,
// modelBlockShift = 4, modelBlockMaxTTL = 24h); they are a policy choice, not a
// vendor figure, which is exactly why the cap exists.
//
// A model park is runtime-only: it is not written to the pool state file, so a
// restart forgets it and pays one 11102 to learn it again.  Persisting it would
// mean a schema change to a document whose exact contents other tests pin.
func (p *Pool) MarkModelBlocked(a *Auth, model, reason string) time.Duration {
	if p == nil || a == nil || model == "" {
		return 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return 0
	}
	hits := 1
	if prev, ok := e.modelCool[model]; ok && prev.Reason == reason && !prev.Until.IsZero() && now.Before(prev.Until) {
		hits = prev.Hits + 1
	}
	shift := hits - 1
	if shift > modelBlockShift {
		shift = modelBlockShift
	}
	ttl := modelBlockBaseTTL << shift
	if ttl <= 0 || ttl > modelBlockMaxTTL {
		ttl = modelBlockMaxTTL
	}
	e.parkModel(model, modelCooldown{Until: now.Add(ttl), Reason: reason, Hits: hits})
	p.log("workbuddy: model %s parked on account %s for %v (%s, hit %d)",
		model, core.MaskSecret(a.ID()), ttl.Round(time.Second), reason, hits)
	return ttl
}

// MarkModelRateLimited parks one model on one account after a model-scoped rate
// limit.
//
// resetAt is the vendor's own reset wall clock, parsed by ParseRateReset out of
// the 6004 message.  When it is set the park honours it, capped at softModelMax
// so that one message cannot park a model for a day.  When the message carried
// no date the reference's bounded doubling is used instead: base, doubled per
// consecutive hintless park, capped at softModelMax.
func (p *Pool) MarkModelRateLimited(a *Auth, model string, resetAt time.Time, base time.Duration, reason string) time.Duration {
	if p == nil || a == nil || model == "" {
		return 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return 0
	}
	e.pruneModelCool(now)
	hits := 1
	if prev, ok := e.modelCool[model]; ok {
		// Only a hintless park is a "consecutive hit" of the same kind.  A park
		// with a reset date already carries the vendor's answer.
		if prev.ResetAt.IsZero() && !prev.Until.IsZero() && now.Before(prev.Until) {
			hits = prev.Hits + 1
		}
	}
	mc := modelCooldown{Reason: reason, Hits: hits}
	var ttl time.Duration
	if !resetAt.IsZero() {
		until := resetAt
		if ceiling := now.Add(softModelMax); until.After(ceiling) {
			until = ceiling
		}
		if until.Before(now) {
			// The vendor's reset is already in the past: there is nothing to
			// wait for, so record the reason with a zero park rather than
			// silently dropping it.
			until = now
		}
		mc.Until = until
		mc.ResetAt = resetAt
		ttl = until.Sub(now)
	} else {
		ttl = p.softModelDuration(base, hits)
		mc.Until = now.Add(ttl)
	}
	e.parkModel(model, mc)
	p.log("workbuddy: model %s rate limited on account %s for %v (%s, hit %d)",
		model, core.MaskSecret(a.ID()), ttl.Round(time.Second), reason, hits)
	return ttl
}

// softModelDuration is the reference's hintless soft backoff: base doubled once
// per consecutive hit, capped at the module-wide soft ceiling.  The shift is
// capped so an absurd streak cannot overflow the multiplication.
func (p *Pool) softModelDuration(base time.Duration, hits int) time.Duration {
	if base <= 0 {
		base = softRateCooldown
	}
	if hits < 1 {
		hits = 1
	}
	shift := hits - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d := base << shift
	if cap := p.softCap(); d <= 0 || d > cap {
		d = cap
	}
	return d
}

// softAccountCooldown turns the vendor's own reset instant into an account-wide
// cooldown, capped at the module-wide soft ceiling so that a single message
// cannot park an account for the rest of the day.  It reports false when there
// is no usable instant -- no hint, or a hint already in the past -- so that the
// caller keeps the policy cooldownFor already computed.
func (p *Pool) softAccountCooldown(now, resetAt time.Time) (time.Duration, bool) {
	if resetAt.IsZero() {
		return 0, false
	}
	until := resetAt
	if ceiling := now.Add(p.softCap()); until.After(ceiling) {
		until = ceiling
	}
	if !until.After(now) {
		return 0, false
	}
	return until.Sub(now), true
}

// softCap is the module-wide soft ceiling.  A configured value wins; anything
// else -- never set, or set to something unusable -- falls back to the package
// default, because a cap of zero would mean "no cap", which is the one outcome
// the reference never allows.
func (p *Pool) softCap() time.Duration {
	if p != nil && p.softRateMax > 0 {
		return p.softRateMax
	}
	return softRateMax
}

// ClearModelCooldown lifts a "this model does not exist on this account" park,
// reporting whether one was there.  Only the 11102 park is cleared: a
// rate-limit park records the vendor's own reset, and a later success on a
// different path must not cut that window short.
func (p *Pool) ClearModelCooldown(a *Auth, model string) bool {
	if p == nil || a == nil || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return false
	}
	mc, ok := e.modelCool[model]
	if !ok || !strings.HasPrefix(mc.Reason, modelBlockBackoffReason) {
		return false
	}
	delete(e.modelCool, model)
	return true
}

// MarkInvalid parks an account whose credentials cannot be repaired.
func (p *Pool) MarkInvalid(a *Auth, note string) {
	if p == nil || a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.findLocked(a); e != nil {
		e.state = stateInvalid
		e.until = p.now().Add(invalidCooldown)
		e.note = note
		e.modelCool = nil
		e.current = 0
		p.dirty = true
	}
	p.saveLocked()
}

func (p *Pool) findLocked(a *Auth) *poolEntry {
	if a == nil {
		return nil
	}
	for _, e := range p.entries {
		if e.auth == a {
			return e
		}
	}
	id := a.ID()
	for _, e := range p.entries {
		if e.auth != nil && e.auth.ID() == id {
			return e
		}
	}
	return nil
}

// Snapshot renders the pool for Status().Accounts.
func (p *Pool) Snapshot() []core.AccountStatus {
	if p == nil {
		return nil
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.entries))
	for _, e := range p.entries {
		if e.auth == nil {
			continue
		}
		state := e.state
		if state == "" {
			state = stateUnknown
		}
		if !e.usable(now) {
			if state == stateReady || state == stateUnknown {
				state = stateCooling
			}
		}
		as := core.AccountStatus{
			ID:      e.auth.ID(),
			Label:   e.auth.Label(),
			Enabled: e.usable(now),
			State:   state,
		}
		if exp := e.auth.ExpiryTime(); !exp.IsZero() {
			as.ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		note := e.note
		if !e.until.IsZero() && now.Before(e.until) {
			left := e.until.Sub(now).Round(time.Second)
			if note == "" {
				note = "cooling"
			}
			note = note + " (" + left.String() + " left)"
		}
		as.Note = note
		extra := map[string]any{"realm": e.auth.RealmName()}
		// in_flight is reported even at rest: a column that disappears when it
		// reads zero is harder to read than one that says 0, and the ceiling
		// is only worth showing when there is one.
		extra["in_flight"] = e.inFlight
		if lim := p.inFlightLimitLocked(e); lim > 0 {
			extra["in_flight_limit"] = lim
		}
		if e.fails > 0 {
			extra["failures"] = e.fails
		}
		// A per-model park is the only reason an account that looks ready can
		// still refuse one model, so it has to be visible: "state=ready" plus a
		// 502 for one model is otherwise inexplicable from Status() alone.
		e.pruneModelCool(now)
		if len(e.modelCool) > 0 {
			parks := make([]string, 0, len(e.modelCool))
			for m, mc := range e.modelCool {
				if mc.Until.IsZero() {
					parks = append(parks, m)
					continue
				}
				parks = append(parks, fmt.Sprintf("%s until %s", m, mc.Until.UTC().Format(time.RFC3339)))
			}
			sort.Strings(parks)
			extra["model_cooldowns"] = parks
		}
		as.Extra = extra
		out = append(out, as)
	}
	return out
}

// Summary is a one-line, secret-free description of the pool.
func (p *Pool) Summary() string {
	if p == nil {
		return "no accounts"
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return "no accounts"
	}
	ready, cooling, parked := 0, 0, 0
	for _, e := range p.entries {
		switch {
		case e.usable(now):
			ready++
		case e.state == stateInvalid || e.state == stateExhausted:
			parked++
		default:
			cooling++
		}
	}
	parts := []string{}
	parts = append(parts, plural(ready, "ready"))
	if cooling > 0 {
		parts = append(parts, plural(cooling, "cooling"))
	}
	if parked > 0 {
		parts = append(parts, plural(parked, "parked"))
	}
	return strings.Join(parts, ", ")
}

func plural(n int, word string) string {
	return strings.Join([]string{itoa(n), word}, " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
