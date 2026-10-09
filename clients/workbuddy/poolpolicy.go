package workbuddy

// poolpolicy.go holds the parts of the account pool an operator can tune, plus
// the two feedback loops that only make sense once those knobs exist.
//
// pool.go implements the mechanics: how an account is classified, how a failure
// becomes a cooldown, how a pick is weighted.  This file adds
//
//   - the tuning surface (breaker, degrade, idle weights, the soft-rate ceiling,
//     the expiring-credit preference, the cost-explore window), so the values a
//     configuration file names actually reach the pool;
//   - the credit loop: the balance and its expiry flow back in, so an account
//     parked for exhausted credit unfreezes by itself once the vendor grants
//     more, and a grant about to lapse is spent before a later one;
//   - the cost loop: what each model actually cost on each account, so a pick
//     can prefer the account that was measured to be free for that model.
//
// Every method tolerates a nil pool or a nil account.  Several of them are
// reached from the panel while a client is still starting up, and a nil-deref
// there would take the whole panel down.

import (
	"math"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// Cost tiers for a model on one account.  See PickForModelInRealm for why
// "unknown" outranks "charged".
const (
	costTierFree    = 0
	costTierUnknown = 1
	costTierCharged = 2
)

// --- the measured cost ledger -----------------------------------------------

// modelCostOf reports the measured cost of a model on this account, if the
// observation is still believed.  An observation older than modelCostTTL is
// refused rather than aged: a vendor's promotional window is a property of the
// time of day, so yesterday's "this model is free" must not steer today's pick.
func (e *poolEntry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if e == nil || model == "" || len(e.modelCost) == 0 {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok || mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false
	}
	return mc, true
}

// costTierOf classifies a model on this account for the picker.
func (e *poolEntry) costTierOf(model string, now time.Time) int {
	mc, ok := e.modelCostOf(model, now)
	if !ok {
		return costTierUnknown
	}
	if mc.CostPer1k <= 0 {
		return costTierFree
	}
	return costTierCharged
}

// pruneModelCost drops observations that have aged out.  Without it the ledger
// is only filtered where it is read, and a model seen once a year ago would
// occupy a map slot forever.
func (e *poolEntry) pruneModelCost(now time.Time) {
	if e == nil || len(e.modelCost) == 0 {
		return
	}
	for model, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			delete(e.modelCost, model)
		}
	}
}

// NoteModelCost records what one request actually cost on one account, and
// deducts it from the balance last read so the expiry detail stays honest
// between refreshes.
//
// The stored cost is an exponential moving average rather than the latest
// sample: a single request's token count varies far too much to classify an
// account from it.  A zero cost is meaningful -- it is how a free window is
// discovered -- so the ledger keeps it instead of treating it as "no data".
func (p *Pool) NoteModelCost(a *Auth, model string, credit float64, tokens int) {
	if p == nil || a == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" || tokens <= 0 {
		return
	}
	per1k := credit / float64(tokens) * 1000
	if math.IsNaN(per1k) || math.IsInf(per1k, 0) || per1k < 0 {
		per1k = 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return
	}
	if spend := int64(credit + 0.5); spend > 0 {
		consumeCredits(e, spend)
		// Spending the last credit parks the account right here instead of
		// waiting up to a sweep for the next request to fail against it.
		p.applyReserveLocked(e, now)
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry, 2)
	}
	prev, seen := e.modelCost[model]
	next := modelCostEntry{CostPer1k: per1k, LastSeen: now, Samples: 1}
	if seen && prev.Samples > 0 {
		const alpha = 0.3
		next.Samples = prev.Samples + 1
		next.CostPer1k = prev.CostPer1k*(1-alpha) + per1k*alpha
	}
	e.modelCost[model] = next
	if seen && prev.CostPer1k <= 0 && per1k > 0 {
		// The one transition an operator needs to see: a model that was free
		// has started costing credit, so the cheap picks are about to stop
		// being cheap.
		p.log("workbuddy: model %s on %s is no longer free (%.3f credit/1k tokens)",
			model, e.auth.Label(), per1k)
	}
	p.dirty = true
	p.saveLocked()
}

// ModelCost reports the measured cost of a model on an account.
func (p *Pool) ModelCost(a *Auth, model string) (modelCostEntry, bool) {
	if p == nil || a == nil {
		return modelCostEntry{}, false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return modelCostEntry{}, false
	}
	return e.modelCostOf(model, now)
}

// ModelCosts reports every live observation for one account, keyed by model.
func (p *Pool) ModelCosts(a *Auth) map[string]modelCostEntry {
	if p == nil || a == nil {
		return nil
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return nil
	}
	out := make(map[string]modelCostEntry, len(e.modelCost))
	for model := range e.modelCost {
		if mc, ok := e.modelCostOf(model, now); ok {
			out[model] = mc
		}
	}
	return out
}

// --- earliest-expiry routing ------------------------------------------------

// earliestExpiryLocked picks, among the candidates that reported credit expiring
// inside the configured window, the one whose soonest batch lapses first.
//
// An allowance that expires unspent is simply lost while a later one keeps, so
// the soonest deadline is the one worth spending.  Ties go to the larger
// remaining batch and then to the account id, so the choice is deterministic.
//
// Accounts inside minPickGap are skipped when any urgent candidate is outside
// it: an account picked a moment ago was picked on purpose, and routing to it
// again is exactly what the gap exists to prevent.  If every urgent account is
// that fresh, the soonest one still wins -- the alternative would be spending a
// later grant first.
func (p *Pool) earliestExpiryLocked(cands []scoredEntry, now time.Time) *poolEntry {
	urgent := make([]scoredEntry, 0, len(cands))
	for _, c := range cands {
		e := c.e
		if e == nil || e.creditsExpiring <= 0 || e.creditsEarliestRemain <= 0 {
			continue
		}
		if e.creditsEarliestExpiry.IsZero() || !e.creditsEarliestExpiry.After(now) {
			continue
		}
		urgent = append(urgent, c)
	}
	if len(urgent) == 0 {
		return nil
	}
	sort.SliceStable(urgent, func(i, j int) bool {
		a, b := urgent[i].e, urgent[j].e
		if !a.creditsEarliestExpiry.Equal(b.creditsEarliestExpiry) {
			return a.creditsEarliestExpiry.Before(b.creditsEarliestExpiry)
		}
		if a.creditsEarliestRemain != b.creditsEarliestRemain {
			return a.creditsEarliestRemain > b.creditsEarliestRemain
		}
		return a.auth.ID() < b.auth.ID()
	})
	for _, c := range urgent {
		if now.Sub(c.e.lastUsed) >= minPickGap {
			return c.e
		}
	}
	return urgent[0].e
}

// --- the tuning surface -----------------------------------------------------

// faultPolicyLocked returns the live policy, falling back to the defaults when
// the pool was built without one.  The zero value of FaultPolicy would disarm
// both penalties (a threshold of 0 trips on the first failure, a cooldown of 0
// parks for no time), so it must never be used as-is.
func (p *Pool) faultPolicyLocked() core.FaultPolicy {
	if p.policy.BreakerThreshold <= 0 || p.policy.DegradeThreshold <= 0 {
		return core.DefaultFaultPolicy()
	}
	return p.policy
}

// SetPolicy installs the breaker and degrade policy.  Unset fields fall back to
// the reference defaults, so a configuration that names only one key still
// behaves like the reference everywhere else.
//
// A tracker that already carries counters keeps them: the new policy governs the
// next trip, which is what an operator who just changed a threshold expects.
func (p *Pool) SetPolicy(policy core.FaultPolicy) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.policy = policy.Normalised()
	p.mu.Unlock()
}

// Policy reports the live policy.
func (p *Pool) Policy() core.FaultPolicy {
	if p == nil {
		return core.DefaultFaultPolicy()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.faultPolicyLocked()
}

// SetWeights tunes the idle bonus: an account untouched for an hour gains
// idlePerHour of pick weight, up to idleMax.  A non-positive value keeps the
// current setting, matching the reference's SetWeights.
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if idlePerHour > 0 {
		p.idlePerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleMax = idleMax
	}
	p.mu.Unlock()
}

// Weights reports the live idle-bonus settings.
func (p *Pool) Weights() (perHour, max float64) {
	if p == nil {
		return idleWeightPerHour, idleWeightMax
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.weights()
}

// SetSoftRateMax caps every soft (rate-limit) cooldown.  Without a cap a
// repeatedly rate-limited account backs off exponentially into a ban-shaped
// window of its own making.  A non-positive value keeps the current setting.
func (p *Pool) SetSoftRateMax(d time.Duration) {
	if p == nil || d <= 0 {
		return
	}
	p.mu.Lock()
	p.softRateMax = d
	p.mu.Unlock()
}

// SetPreferExpiring turns earliest-expiry routing on or off.  Off means the
// expiry detail is ignored entirely, not merely de-prioritised.
func (p *Pool) SetPreferExpiring(enabled bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.preferExpiring = enabled
	p.mu.Unlock()
}

// PreferExpiring reports whether earliest-expiry routing is on.
func (p *Pool) PreferExpiring() bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.preferExpiring
}

// SetCostExploreInterval sets how often a model whose cheapest known tier is
// "measured free" may be sent to an unmeasured account instead, so the ledger
// can discover a second free tier.
//
// Zero is a legal value and means "never explore" -- deliberately unlike the
// other setters, which treat a non-positive value as "keep the current
// setting".  Without a way to spell zero the detour could not be turned off at
// all.  A negative value is rejected.
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	if p == nil || d < 0 {
		return
	}
	p.mu.Lock()
	p.costExploreInterval = d
	if p.exploreLast == nil {
		p.exploreLast = make(map[string]time.Time, 2)
	}
	p.mu.Unlock()
}

// CostExploreInterval reports the live explore window.
func (p *Pool) CostExploreInterval() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.costExploreInterval
}

// CostExploreStatus reports how many detours have happened and when each
// realm/model pair last took one.  The internal key separator is rendered as
// "|" so the map is safe to serialise into a panel response.
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	if p == nil {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]time.Time, len(p.exploreLast))
	for k, v := range p.exploreLast {
		out[strings.ReplaceAll(k, "\x1f", "|")] = v
	}
	return p.exploreEvents, out
}

// --- the credit loop --------------------------------------------------------

// consumeCredits deducts a spend from the stored balance and from the expiring
// buckets, dropping the earliest-expiry pair once nothing is left in it.
func consumeCredits(e *poolEntry, spend int64) {
	if e == nil || spend <= 0 {
		return
	}
	if spend > e.credits {
		spend = e.credits
	}
	e.credits -= spend
	if spend > e.creditsExpiring {
		spend = e.creditsExpiring
	}
	e.creditsExpiring -= spend
	if spend > e.creditsEarliestRemain {
		spend = e.creditsEarliestRemain
	}
	e.creditsEarliestRemain -= spend
	if e.creditsEarliestRemain == 0 {
		e.creditsEarliestExpiry = time.Time{}
	}
}

// SetCreditsDetailed records what an account's balance actually is, including
// the part of it that expires inside the caller's window and the earliest such
// batch.
//
// Negative inputs are clamped, and an expiry that has already passed or carries
// no remaining credit is dropped rather than believed: routing by a deadline
// that has gone would send every request to the same account.
//
// A positive balance also lifts a credit park, which is the point of the whole
// loop -- the vendor granting more is exactly the evidence that the park no
// longer applies.
func (p *Pool) SetCreditsDetailed(a *Auth, credits, total, expiring int64, earliestAt time.Time, earliestRemaining int64) {
	if p == nil || a == nil {
		return
	}
	now := p.now()
	if credits < 0 {
		credits = 0
	}
	if total < 0 {
		total = 0
	}
	if expiring < 0 {
		expiring = 0
	}
	if expiring > credits {
		expiring = credits
	}
	if earliestRemaining < 0 {
		earliestRemaining = 0
	}
	if earliestRemaining > credits {
		earliestRemaining = credits
	}
	if earliestAt.IsZero() || !earliestAt.After(now) || earliestRemaining == 0 {
		earliestAt = time.Time{}
		earliestRemaining = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return
	}
	e.credits = credits
	e.creditsTotal = total
	e.creditsExpiring = expiring
	e.creditsEarliestExpiry = earliestAt
	e.creditsEarliestRemain = earliestRemaining
	e.creditsKnown = true
	p.reenableIfCreditsLocked(e, credits, now)
	// Lift a hard-credit park first, then let the low-balance guard have the
	// last word: a balance that came back but is still at or below the reserve
	// must not put the account back into rotation.
	p.applyReserveLocked(e, now)
	p.dirty = true
	p.saveLocked()
}

// ReenableIfCredits is the aggregate-balance entry point, for the login and
// import paths where only "this account has N credit" is known and not what
// expires when.  It therefore drops any expiry detail it cannot substantiate
// rather than leaving a stale one to steer routing.
func (p *Pool) ReenableIfCredits(a *Auth, remain, total int64) bool {
	if p == nil || a == nil {
		return false
	}
	if remain < 0 {
		remain = 0
	}
	if total < 0 {
		total = 0
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return false
	}
	e.credits = remain
	e.creditsTotal = total
	e.creditsKnown = true
	e.creditsExpiring = 0
	e.creditsEarliestExpiry = time.Time{}
	e.creditsEarliestRemain = 0
	revived := p.reenableIfCreditsLocked(e, remain, now)
	// The guard answers last, because a balance that is known but still at or
	// below the reserve is not usable even though it is no longer a vendor park.
	p.applyReserveLocked(e, now)
	if e.state != stateReady {
		revived = false
	}
	p.dirty = true
	p.saveLocked()
	return revived
}

// reenableIfCreditsLocked lifts the one park that a balance can answer for.
//
// Only a credit park is evidence-based here: the vendor saying the account can
// pay again is direct proof.  A soft rate limit, a WAF park, a session death and
// a tripped breaker each have their own recovery evidence -- the vendor's reset
// clock, a successful request -- and clearing them on a balance refresh would
// compress their real lifetime to one refresh interval.  That is a measured
// failure mode, not a precaution: with two accounts in the pool, clearing the
// cooling domain on every refresh reduced every rate-limit cooldown to the
// five-minute refresh period.
func (p *Pool) reenableIfCreditsLocked(e *poolEntry, remain int64, now time.Time) bool {
	if e == nil {
		return false
	}
	if remain <= 0 || e.state == stateInvalid {
		return false
	}
	if e.state != stateExhausted || e.note != ErrHardCredit.String() {
		return false
	}
	e.state = stateReady
	e.until = time.Time{}
	e.note = ""
	e.modelCool = nil
	e.fails = 0
	// A credit park the operator can see should not survive the account
	// becoming usable again.
	e.current = 0
	p.log("workbuddy: account %s has credit again (%d); leaving the credit park", e.auth.Label(), remain)
	_ = now
	return true
}

// SetReserveCredits installs the low-balance guard and re-evaluates every
// account against it immediately, so an operator who changes the threshold
// sees the pool's decisions without waiting for the next balance sweep.
//
// A negative value turns the guard off.  Zero is the documented default and
// parks only a *known* zero balance.
func (p *Pool) SetReserveCredits(n int64) {
	if p == nil {
		return
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reserveCredits = n
	changed := false
	for _, e := range p.entries {
		if p.applyReserveLocked(e, now) {
			changed = true
		}
	}
	if changed {
		p.dirty = true
	}
	p.saveLocked()
}

// ReserveCredits reports the installed threshold.  Negative means the guard
// is off.
func (p *Pool) ReserveCredits() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reserveCredits
}

// reserveBlockedLocked reports whether the guard parks this entry.  Only a
// known balance can park: an account that has never answered a balance read
// stays usable, which is what keeps a fresh install from parking everything.
func (p *Pool) reserveBlockedLocked(e *poolEntry) bool {
	if p == nil || e == nil || p.reserveCredits < 0 || !e.creditsKnown {
		return false
	}
	return e.credits <= p.reserveCredits
}

// applyReserveLocked parks or revives one entry to match the guard.  It only
// touches the park the guard itself created: a rate-limit, credential or
// breaker park owns the entry until its own deadline, and the next balance
// sweep re-applies the guard once that park lapses.
func (p *Pool) applyReserveLocked(e *poolEntry, now time.Time) bool {
	if p == nil || e == nil || e.state == stateInvalid {
		return false
	}
	parked := e.state == stateExhausted && e.note == reserveCreditNote
	if p.reserveBlockedLocked(e) {
		if !parked && e.state != stateReady && e.state != stateUnknown {
			return false
		}
		if parked && e.until.After(now) {
			return false
		}
		e.state = stateExhausted
		e.note = reserveCreditNote
		if until := nextDay4AM(now); until.After(now) {
			e.until = until
		} else {
			e.until = now.Add(longCreditCooldown)
		}
		e.modelCool = nil
		e.current = 0
		return true
	}
	if !parked {
		return false
	}
	e.state = stateReady
	e.until = time.Time{}
	e.note = ""
	e.modelCool = nil
	e.current = 0
	return true
}

// SetExpiringSoon installs the window used to compute each account's
// expiring-credit snapshot.  A changed window invalidates every stored
// snapshot; an unchanged reload leaves the pool alone.
func (p *Pool) SetExpiringSoon(d time.Duration) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	changed := p.expiringSoon != d
	if changed {
		p.expiringSoon = d
	}
	p.mu.Unlock()
	if changed {
		p.ClearExpiringSnapshots()
	}
	return changed
}

// ClearExpiringSnapshots drops every account's expiry detail.
//
// The window the detail was computed against is a configuration value, so a
// reload that changes it invalidates all of it at once: an account that was
// "expiring soon" under a seven-day window is not necessarily one under a
// one-hour window, and routing by the stale answer would be worse than not
// routing by expiry at all.
func (p *Pool) ClearExpiringSnapshots() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e == nil {
			continue
		}
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemain = 0
	}
	p.dirty = true
	p.saveLocked()
}

// Credits reports the last balance this account reported.  known is false until
// the first refresh lands, which is different from "zero credit" -- a caller
// that conflates the two would park every account on startup.
func (p *Pool) Credits(a *Auth) (remain, total int64, known bool) {
	if p == nil || a == nil {
		return 0, 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return 0, 0, false
	}
	return e.credits, e.creditsTotal, e.creditsKnown
}

// CooldownUntilTomorrow4AM parks an account that has run out of credit until the
// next 04:00 and returns how long that is.
//
// The vendor's quota day turns over at 04:00 and the sign-in grants that refill
// a spent account land at 09:00 and 21:00, so waiting for 04:00 is what lets the
// same day recover.  A fixed six hours would instead retry inside the very quota
// day that has nothing left, which is how one hard-credit answer turns into a
// stream of them.
func (p *Pool) CooldownUntilTomorrow4AM(a *Auth, reason string) time.Duration {
	if p == nil || a == nil {
		return 0
	}
	now := p.now()
	d := nextDay4AM(now).Sub(now)
	if d <= 0 {
		d = longCreditCooldown
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return 0
	}
	e.state = stateExhausted
	e.until = now.Add(d)
	e.note = strings.TrimSpace(reason)
	e.modelCool = nil
	e.current = 0
	p.dirty = true
	p.saveLocked()
	return d
}

// --- which axis a failure feeds ---------------------------------------------

// faultKind decides which penalty axis a failure feeds, if any.
//
// The two axes are deliberately exclusive.  The breaker is for failures the
// upstream classified authoritatively -- a 5xx, a rejected token refresh -- which
// already carry a penalty of their own; feeding the same event to both would
// punish it twice.  The degrade counter is for failures we could not classify: an
// unknown 4xx or a transport error, which is exactly the case with no
// authoritative "wait this long".
//
// A transport error feeds only degrade and never the breaker: "we could not
// reach the network" is not the account's fault, and tripping a six-hour breaker
// over a flaky link would take a healthy account out of rotation for the
// afternoon.
func faultKind(err error) (breaker, degrade bool) {
	var ue *Error
	if !asError(err, &ue) {
		return false, true
	}
	switch ue.Kind {
	case ErrServer:
		return true, false
	case ErrClient:
		return false, true
	default:
		// A rate limit's reset, a session death's park, a model block's backoff
		// and a WAF block's cool-down are all authoritative answers with a
		// penalty already attached.
		return false, false
	}
}

// noteFaultLocked feeds whichever axis this failure belongs to and logs a trip.
// It must be called with p.mu held.
func (p *Pool) noteFaultLocked(e *poolEntry, err error, now time.Time) {
	if e == nil {
		return
	}
	breaker, degrade := faultKind(err)
	if !breaker && !degrade {
		return
	}
	policy := p.faultPolicyLocked()
	if breaker {
		until, tripped := e.faults.NoteFailure(policy, now)
		if tripped {
			p.log("workbuddy: account %s tripped the breaker after %d classified failures; parked until %s",
				e.auth.Label(), policy.BreakerThreshold, until.UTC().Format(time.RFC3339))
		}
		return
	}
	until, tripped := e.faults.NoteUnpunishedFailure(policy, now)
	if tripped {
		p.log("workbuddy: account %s failed %d unclassified requests in a row; parked until %s",
			e.auth.Label(), policy.DegradeThreshold, until.UTC().Format(time.RFC3339))
	}
}

// --- persistence accessors --------------------------------------------------

// FaultSnapshot reports an account's breaker and degrade state, for the status
// surface and for the state file.
func (p *Pool) FaultSnapshot(a *Auth) (core.FaultSnapshot, bool) {
	if p == nil || a == nil {
		return core.FaultSnapshot{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return core.FaultSnapshot{}, false
	}
	return e.faults.Snapshot(), true
}

// RestoreFaults seeds an account's breaker and degrade state from a snapshot.
// An expired deadline inside the snapshot is ignored by the tracker itself, so a
// stale file cannot park an account.
func (p *Pool) RestoreFaults(a *Auth, s core.FaultSnapshot) bool {
	if p == nil || a == nil {
		return false
	}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil {
		return false
	}
	e.faults.Restore(s, now)
	p.dirty = true
	p.saveLocked()
	return true
}

// --- configuration ----------------------------------------------------------

// onOff renders a boolean for a startup or reload log line.
func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// applyPoolTuning folds the process-wide pool policy onto this pool.
//
// Every field of the tuning group is optional, so a reload that touches one knob
// leaves the rest of the pool exactly as it was; a nil group -- a caller that
// never configured a pool -- changes nothing at all.  Each knob logs what it
// ended up as, because these values used to be parsed, defaulted and logged
// while no pool ever read them: the only way an operator can tell the wiring
// works now is to see the pool report the numbers back.
func (c *Client) applyPoolTuning(t *core.PoolTuning) {
	if c == nil || c.pool == nil || t == nil {
		return
	}
	policy := c.pool.Policy()
	changed := false
	assignInt := func(dst *int, src *int) {
		if src != nil && *src != *dst {
			*dst = *src
			changed = true
		}
	}
	assignDur := func(dst *time.Duration, src *time.Duration) {
		if src != nil && *src != *dst {
			*dst = *src
			changed = true
		}
	}
	assignInt(&policy.BreakerThreshold, t.BreakerThreshold)
	assignDur(&policy.BreakerCooldown, t.BreakerCooldown)
	assignDur(&policy.BreakerCooldownMax, t.BreakerCooldownMax)
	assignInt(&policy.DegradeThreshold, t.DegradeThreshold)
	assignDur(&policy.DegradeCooldown, t.DegradeCooldown)
	assignDur(&policy.DegradeCooldownMax, t.DegradeCooldownMax)
	if changed {
		c.pool.SetPolicy(policy)
		// Read back rather than trusting the input: SetPolicy normalises, so
		// this is the policy the pool will actually apply.
		applied := c.pool.Policy()
		c.logf("workbuddy: pool breaker trips after %d failures and parks for %s, doubling to at most %s; degradation parks after %d unclassified failures for %s, clamped to %s",
			applied.BreakerThreshold, applied.BreakerCooldown, applied.BreakerCooldownMax,
			applied.DegradeThreshold, applied.DegradeCooldown, applied.DegradeCooldownMax)
	}
	if t.SoftRateMax != nil {
		c.pool.SetSoftRateMax(*t.SoftRateMax)
		c.logf("workbuddy: soft rate-limit cooldowns are capped at %s", t.SoftRateMax.String())
	}
	if t.IdleWeightPerHour != nil || t.IdleWeightMax != nil {
		perHour, max := c.pool.Weights()
		if t.IdleWeightPerHour != nil {
			perHour = *t.IdleWeightPerHour
		}
		if t.IdleWeightMax != nil {
			max = *t.IdleWeightMax
		}
		c.pool.SetWeights(perHour, max)
		gotPerHour, gotMax := c.pool.Weights()
		c.logf("workbuddy: idle compensation is %.2f weight per idle hour, up to %.2f", gotPerHour, gotMax)
	}
	if t.PreferExpiring != nil {
		c.pool.SetPreferExpiring(*t.PreferExpiring)
		c.logf("workbuddy: earliest-expiry routing is %s", onOff(c.pool.PreferExpiring()))
	}
	if t.CostExploreInterval != nil {
		c.pool.SetCostExploreInterval(*t.CostExploreInterval)
		if d := c.pool.CostExploreInterval(); d > 0 {
			c.logf("workbuddy: cost exploration detours an unknown-cost account every %s", d)
		} else {
			c.logf("workbuddy: cost exploration is off")
		}
	}
	if t.ExpiringSoon != nil {
		if c.pool.SetExpiringSoon(*t.ExpiringSoon) {
			c.logf("workbuddy: expiring-credit window moved to %s; cached expiry snapshots cleared", t.ExpiringSoon.String())
		}
	}

}

// ApplyPlatformPolicy implements core.PlatformPolicyApplier: the operator's
// per-platform policy is pushed here on startup and on every live reload.
//
// Only the low-balance guard is a pool decision; priority, the model blacklist
// and the in-flight ceilings stay in the gateway, which owns routing.
func (c *Client) ApplyPlatformPolicy(cfg core.PlatformConfig) {
	if c == nil || c.pool == nil {
		return
	}
	c.pool.SetReserveCredits(int64(cfg.ReserveCredits))
	if cfg.ReserveCredits < 0 {
		c.logf("workbuddy: low-balance guard is off")
		return
	}
	c.logf("workbuddy: accounts at or below %d credit are parked until the balance rises", cfg.ReserveCredits)
}
