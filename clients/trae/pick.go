package trae

// pick.go — the scored selection path and the per-model cooldown: the trae half
// of the reference's internal/pool/pick.go and internal/pool/cooldown.go.
//
// Two selection paths live side by side here, on purpose:
//
//   - Pick (pool.go) is the historic deterministic rotation.  It is what every
//     existing caller and test already relies on, and it is unchanged.
//   - PickForModel is the scored path, used by Chat, which always knows the
//     model it is about to ask for.  It ranks usable accounts by a weight
//     derived from credits, idleness and recorded failures, draws among the
//     best few, and defers the account it handed out microseconds ago.
//
// The weight is where trae differs from workbuddy: trae's credentials carry
// real, absolute credit balances (Auth.IdeCredits / Auth.WorkCredits, refreshed
// from the notify_usage event -- see docs/upstream/trae.md:91-94), so the
// reference's `credits / maxCredits * 10` term can be reproduced here with real
// data instead of being omitted.

import (
	"math"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// Model-park backoff.  The numbers are the reference's
	// (internal/pool/cooldown.go: modelBlockBaseTTL = 6h, modelBlockShift = 4,
	// modelBlockMaxTTL = 24h).  They are a policy choice, not a vendor figure:
	// trae's taxonomy (docs/upstream/trae.md:98-114) has no code that means
	// "this model is unavailable to this account", so nothing here is armed
	// automatically; see MarkFailureForModel.
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift   = 4
	modelBlockMaxTTL  = 24 * time.Hour
	// modelBlockReason labels a park created by MarkModelBlocked, and
	// modelBlockBackoffReason is the prefix ClearModelCooldown keys off so that
	// clearing a "model is gone" park never clears a "model is busy until
	// <time>" one.  Unlike workbuddy these are local strings: there is no
	// vendor message to borrow.
	modelBlockReason        = "model unavailable"
	modelBlockBackoffReason = modelBlockReason
	// softRateMax is the module-wide soft ceiling, the reference's
	// defaultSoftRateMax: the vendor's own reset hint is honoured up to it and
	// distrusted beyond it.
	softRateMax = 2 * time.Hour
	// softModelMax is the same ceiling for a model-scoped soft park.
	softModelMax       = softRateMax
	softStreakShiftMax = 16
	// modelRateLimitReason labels a soft model park.
	modelRateLimitReason = "model rate limited"
)

// Selection weights.  See weightOf for what each term is for.
const (
	// idleWeightPerHour / idleWeightMax give an account nobody has used lately a
	// bounded advantage.  The ceiling matters: without it an account idle for a
	// month would win every draw forever.
	idleWeightPerHour = 0.5
	idleWeightMax     = 5.0
	// creditWeightMax is the ceiling of the credit term, matching the
	// reference's `credits / maxCredits * 10`.
	creditWeightMax = 10.0
	// failWeightPenalty is subtracted per recorded failure, capped at
	// maxFailPenalty, so a battered account is deprioritised without being
	// excluded: it is still usable and may be the only account left.
	failWeightPenalty = 0.25
	maxFailPenalty    = 1.0
	// minWeight is the floor a score can never fall below.  Every candidate
	// keeps some chance of being drawn.
	minWeight = 0.1
	// minPickGap is how recently an account must have been handed out before it
	// contributes no weight to another pick.  Two requests arriving in the same
	// millisecond must not both land on the account the other just took.
	minPickGap = 100 * time.Millisecond
)

// modelCooldown is one model parked on one account.
type modelCooldown struct {
	// Until is when the park lapses.
	Until time.Time
	// ResetAt is the vendor's own reset instant when the park came from one.
	ResetAt time.Time
	// Reason is the operator-facing label.
	Reason string
	// Hits counts consecutive parks of the same kind, which is what the
	// geometric backoff doubles on.
	Hits int
}

// markPickedLocked records that this pool just handed an account out.  The
// caller holds p.mu.
func (p *Pool) markPickedLocked(e *poolEntry, now time.Time) {
	if e == nil {
		return
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
}

// Find returns a specific usable account, reporting whether it could be used for
// this request.  It exists for stickiness: the affinity table names an account by
// id and the caller has to turn that back into a credential without going
// through the picker.
//
// An account that is parked, or that the caller has already tried in this
// request, is reported absent rather than returned -- the caller's contract is
// "no answer means pick normally", never "use it anyway".
func (p *Pool) Find(skip map[string]bool, accountID string) (*Auth, bool) {
	if accountID == "" {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.entries {
		if e.auth == nil || e.auth.ID() != accountID {
			continue
		}
		if skip[accountID] || !e.usable(now) {
			return nil, false
		}
		e.pruneModelCool(now)
		p.markPickedLocked(e, now)
		return e.auth, true
	}
	return nil, false
}

// UsableForModel reports whether one account could serve one model right now.
//
// It is the predicate the affinity table is fed, so it has to mean exactly what
// the picker means, per model: parked accounts are out, and so is an account
// that is cooling down for this particular model.  An empty model means "the
// caller does not know", and then only account-level health is consulted.
func (p *Pool) UsableForModel(accountID, model string) bool {
	if accountID == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.entries {
		if e.auth == nil || e.auth.ID() != accountID {
			continue
		}
		if !e.usable(now) {
			return false
		}
		e.pruneModelCool(now)
		return !e.modelCooled(now, model)
	}
	return false
}

// PickForModel returns the account this pool considers best for one model.
//
// The scored path: every usable account that is not cooling down for this model
// is a candidate, the best pickTopN by weight are drawn among, and the account
// handed out within minPickGap is deferred.  When every usable account is
// cooling down for this model the pick falls back to the model-agnostic
// rotation -- a model park must never turn an otherwise servable request into a
// failure, and it must never let a parked account sit unused while a healthy
// one exists.
//
// An empty model delegates straight to Pick, so a caller that knows nothing
// about models sees no change at all.
func (p *Pool) PickForModel(skip map[string]bool, model string) (*Auth, bool) {
	if model == "" {
		return p.Pick(skip)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	cands := make([]scoredEntry, 0, len(p.entries))
	cooling := 0
	for idx, e := range p.entries {
		if e.auth == nil || skip[e.auth.ID()] || !e.usable(now) {
			continue
		}
		e.pruneModelCool(now)
		if e.modelCooled(now, model) {
			cooling++
			continue
		}
		cands = append(cands, scoredEntry{e: e, idx: idx})
	}
	cands = core.LowestPriorityTier("trae", cands, func(c scoredEntry) string { return c.e.auth.ID() })
	if len(cands) == 0 {
		if cooling > 0 {
			p.log("trae: every usable account is cooling down for model %s; falling back to the model-agnostic pick", model)
		}
		return p.pickLocked(skip, now)
	}
	e := p.pickWeightedLocked(cands, now)
	if e == nil {
		return nil, false
	}
	p.markPickedLocked(e, now)
	return e.auth, true
}

// weightOf scores one candidate.  Higher is better.
//
// The terms, and why each exists:
//
//   - credits: the reference's `credits / maxCredits * 10`.  trae's credentials
//     carry absolute balances, so the divisor is the best balance in this very
//     shortlist rather than an absolute ceiling nobody knows.  An account with
//     more headroom should be preferred; an account with none should not be
//     starved of all chance, which is why the term is additive on a base of 1.
//   - idleness: a bounded bonus for an account nobody has used lately.  This is
//     what stops the credit term from monopolising the draw, and it is the
//     reference's exploration pressure expressed as weight rather than as a
//     quota: a low-use account always has a real chance, without needing a
//     global pick counter or a configured fraction.
//   - failures: a capped subtraction.  A battered account should lose draws
//     before it loses its place in the pool.
func (p *Pool) weightOf(e *poolEntry, now time.Time, maxCredits float64) float64 {
	w := 1.0
	if maxCredits > 0 && e.auth != nil {
		credits := float64(e.auth.IdeCredits + e.auth.WorkCredits)
		if credits < 0 {
			credits = 0
		}
		w += credits / maxCredits * creditWeightMax
	}
	if e.lastUsed.IsZero() {
		// Never used: the full idle credit, not a zero age.
		w += idleWeightMax
	} else if hours := now.Sub(e.lastUsed).Hours(); hours > 0 {
		w += math.Min(hours*idleWeightPerHour, idleWeightMax)
	}
	if pen := float64(e.fails) * failWeightPenalty; pen > 0 {
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

// scoredEntry is one candidate: its entry, its position in the pool (the final
// tie-break, so a tie can never depend on map iteration), and the weight this
// round gave it.
type scoredEntry struct {
	e   *poolEntry
	idx int
	w   float64
}

// pickWeightedLocked chooses one candidate.  The caller holds p.mu and has
// already filtered the candidates to usable, not-skipped, not-model-cooled
// accounts.
//
// The algorithm is smooth weighted round-robin, not a random draw.
//
// The reference (internal/pool/pick.go:331) draws stochastically from the top
// five by weight.  This module cannot: its long-standing tests pin the exact
// rotation -- which account serves the first request, which one is tried second
// after a failover, that a plan-limit park leaves the other account untouched.
// A uniform draw among equal weights makes that first pick a coin flip, and it
// broke four tests with no functional gain.  SWRR keeps what the weights are
// for -- an account with more credits, more idle time and fewer failures takes
// a larger share of the rotation in exact proportion -- while staying
// deterministic, so a homogeneous pool still rotates exactly as it always did.
//
// Mechanics: every candidate's accumulator grows by its weight, the highest
// accumulator wins, and the winner's accumulator drops by the round's total
// weight.  Over a run of rounds that yields each account a share equal to its
// weight over the sum, and when the weights are all equal it degenerates to
// plain alternation.  minPickGap is applied by giving a candidate that was
// handed out moments ago a weight of zero: it still takes part, it just cannot
// win this round.
func (p *Pool) pickWeightedLocked(cands []scoredEntry, now time.Time) *poolEntry {
	if len(cands) == 0 {
		return nil
	}
	if len(cands) == 1 {
		return cands[0].e
	}
	maxCredits := 0.0
	for i := range cands {
		if cands[i].e.auth == nil {
			continue
		}
		if c := float64(cands[i].e.auth.IdeCredits + cands[i].e.auth.WorkCredits); c > maxCredits {
			maxCredits = c
		}
	}
	var total float64
	for i := range cands {
		w := p.weightOf(cands[i].e, now, maxCredits)
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

// --- per-model cooldown ----------------------------------------------------

// modelCooled reports whether one model is currently parked on this account.
// An empty model is never parked: "unknown model" must not match a park that was
// recorded for a real one.
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
// number of distinct models an account ever failed on -- an account wandering
// through a model catalogue would accumulate an entry per model it touched
// once.  It runs on the pick path and on every render, so the map only ever
// holds live parks.
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

// MarkModelBlocked parks one model on one account, and reports for how long.
//
// There is no reset hint to honour -- a "this model is not available here"
// answer is deterministic, not a window -- so the park backs off geometrically:
// modelBlockBaseTTL on the first hit, doubled per consecutive hit of the same
// kind, capped at modelBlockMaxTTL.  The reference chose those numbers; they are
// policy, which is exactly why the cap exists.
//
// A model park is runtime-only.  It is deliberately not written to the pool
// state file: that document's exact contents are pinned by the poolstate tests,
// and a restart simply pays one failure to learn the park again.
func (p *Pool) MarkModelBlocked(a *Auth, model, reason string) time.Duration {
	if a == nil || model == "" {
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
	p.log("trae: model %s parked on account %s for %v (%s, hit %d)",
		model, core.MaskSecret(a.ID()), ttl.Round(time.Second), reason, hits)
	return ttl
}

// MarkModelRateLimited parks one model on one account after a model-scoped rate
// limit, and reports for how long.
//
// resetAt is the vendor's own reset instant when the message dated itself.  When
// it is set the park honours it, capped at softModelMax so one message cannot
// park a model for a day.  When there is no date the reference's bounded
// doubling is used instead: base, doubled per consecutive hintless park, capped
// at softModelMax.
func (p *Pool) MarkModelRateLimited(a *Auth, model string, resetAt time.Time, base time.Duration, reason string) time.Duration {
	if a == nil || model == "" {
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
		// Only a hintless park is a consecutive hit of the same kind: a park
		// that carries a reset date already encodes the vendor's answer.
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
			until = now
		}
		mc.Until = until
		mc.ResetAt = resetAt
		ttl = until.Sub(now)
	} else {
		ttl = softModelDuration(base, hits)
		mc.Until = now.Add(ttl)
	}
	e.parkModel(model, mc)
	p.log("trae: model %s rate limited on account %s for %v (%s, hit %d)",
		model, core.MaskSecret(a.ID()), ttl.Round(time.Second), reason, hits)
	return ttl
}

// softModelDuration is the reference's hintless soft backoff: base doubled once
// per consecutive hit, capped at softModelMax.  The shift is capped so an absurd
// streak cannot overflow the multiplication.
func softModelDuration(base time.Duration, hits int) time.Duration {
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
	if d <= 0 || d > softModelMax {
		d = softModelMax
	}
	return d
}

// ClearModelCooldown lifts a "this model is not available on this account" park,
// reporting whether one was there.  Only that kind is cleared: a rate-limit park
// records the vendor's own reset, and a success on another path must not cut
// that window short.
func (p *Pool) ClearModelCooldown(a *Auth, model string) bool {
	if a == nil || model == "" {
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

// MarkSuccessForModel is MarkSuccess for a request whose model is known: it
// clears the account state exactly as MarkSuccess does, and additionally lifts a
// "model is not available here" park for that model, because the success is
// direct evidence that the park is stale.
//
// Parks for other models are left alone -- a success on model N says nothing
// about model M -- and a rate-limit park is left alone too, because it encodes
// the vendor's own reset.
func (p *Pool) MarkSuccessForModel(a *Auth, model string) {
	if a == nil {
		return
	}
	p.MarkSuccess(a)
	if model == "" {
		return
	}
	p.ClearModelCooldown(a, model)
}

// MarkFailureForModel applies the cooldown policy for a failure when the model is
// known.
//
// It currently delegates to MarkFailure for every input, and that is a
// deliberate, evidence-backed decision rather than an unfinished one: trae's
// taxonomy (docs/upstream/trae.md:98-114) contains no code that means "this model
// is unavailable to this account".  Its codes are plan (1005), auth (1001/4010),
// quota (4008), param (4001/4023), soft rate (429/4011), contention (9074),
// not-found (404) and 5xx -- every one of them either says something about the
// account or about the request as written.  Inventing a mapping from, say, 404
// to "park this model" would park models on guesses.
//
// The model-scoped machinery above is therefore reachable only through its
// explicit API (MarkModelBlocked / MarkModelRateLimited), and this function
// exists so that the day trae's upstream grows such a code -- or the mid-stream
// SSE error path in sse.go:246-255, which is outside this change's file list,
// starts carrying one -- the wiring is a one-line change here rather than a new
// call site in Chat.
func (p *Pool) MarkFailureForModel(a *Auth, model string, err error) (ErrKind, time.Duration) {
	if a == nil {
		return ErrNone, 0
	}
	// The model-scoped interception belongs here, between this line and the
	// account-level policy below, the day a model-scoped code exists:
	//
	//	var e *Error
	//	if model != "" && errors.As(err, &e) && modelScopedKind(e.Kind) {
	//		return e.Kind, p.MarkModelRateLimited(a, model, e.ResetAt, 0, modelRateLimitReason)
	//	}
	//
	// Until then the plain policy is the honest answer, and the underscore in
	// the signature keeps every call site identical to workbuddy's.
	_ = model
	return p.MarkFailure(a, err)
}
