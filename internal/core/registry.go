package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Compile-time registration.  A module registers itself from init(), so the
// only place that has to know the full list is clients/all.
// ---------------------------------------------------------------------------

var (
	regMu     sync.RWMutex
	factories = map[string]Factory{}
	regOrder  []string
)

// Register makes a module available.  It panics on a duplicate name, which can
// only be a programming error.
func Register(name string, f Factory) {
	if name == "" || f == nil {
		panic("core.Register: empty name or nil factory")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := factories[name]; dup {
		panic("core.Register: duplicate client name " + name)
	}
	factories[name] = f
	regOrder = append(regOrder, name)
}

// Registered returns every compiled-in module name, in registration order.
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, len(regOrder))
	copy(out, regOrder)
	return out
}

// Build instantiates a module by name.
func Build(name string, deps Deps) (Client, error) {
	regMu.RLock()
	f, ok := factories[name]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown client %q", name)
	}
	c, err := f(deps)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("client %q factory returned nil", name)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Runtime registry + model routing
// ---------------------------------------------------------------------------

// Registry holds the live module instances and resolves a requested model name
// to (module, upstream model name).
type Registry struct {
	mu      sync.RWMutex
	clients map[string]Client
	order   []string
	aliases map[string]string // "alias" -> "client/model"
	// modelGroups carries operator-declared equivalent model members and
	// optional per-group platform priorities.  modelGroupByLookup maps the
	// group name and each member's display/canonical spelling to the group.
	modelGroups        map[string]ModelGroup
	modelGroupByLookup map[string]string
	// platforms carries the operator's per-platform routing policy: the
	// priority used when several platforms serve the same bare model id, and
	// the models this platform must not be given.  It is a blacklist: a model
	// absent from disabled is allowed.  A platform with no entry keeps the
	// documented default -- priority 0, nothing disabled.
	platforms map[string]platformPolicy
	// health suppresses a (platform, model) pair after repeated failed rounds.
	health *PlatformHealth
}

// PlatformConfig is the operator's routing policy for one client platform.
// The zero value is the documented default: priority 0 and nothing disabled.
// DisabledModels is a blacklist, so a model that is not listed stays callable;
// "default off" is expressed by listing the models this platform must not
// serve, not by enumerating the ones it may.
type PlatformConfig struct {
	// Priority orders platforms that serve the same bare model id.  Lower
	// numbers win.  Equal priorities fall back to the platform name so that
	// the same request cannot flip between processes.
	Priority int
	// PrioritySchedule overrides Priority during fixed local-time windows.
	// Each window uses Beijing time (UTC+8), is half-open [start, end), and
	// may cross midnight.  The first matching window wins.  Outside every
	// window the base Priority remains in force.
	PrioritySchedule []PriorityWindow
	// DisabledModels lists upstream model ids this platform must not be
	// given.  Matching is case-insensitive and surrounding space is ignored,
	// because the ids are typed by hand.
	DisabledModels []string

	// MaxInFlight caps how many requests may be in flight against this
	// platform at once.  Zero means no ceiling.  Some vendors rate-limit
	// per session before the documented quota is touched, so the operator
	// needs a per-platform brake independent of the account pool's own
	// per-account ceiling.
	MaxInFlight int
	// MaxInFlightPerAccount caps how many requests may be in flight against
	// any single account of this platform.  Zero means no ceiling.  It is the
	// second brake, below MaxInFlight: when one account is full the operator
	// wants the next account used, and only when every account is full does
	// the platform answer busy.
	MaxInFlightPerAccount int
	// ReserveCredits is the low-balance guard: an account whose last known
	// balance is at or below this number is parked (paused) until the balance
	// rises above it, which is how a spend-down account stops taking requests
	// that would only fail, and how a check-in or top-up brings it back.
	//
	// Zero is the default and means "a known zero balance parks".  A negative
	// value disables the guard.  A module that has never read a balance keeps
	// the account usable: "never measured" is not "empty".
	ReserveCredits int
	// AccountPriorities is the operator's per-account routing priority.  The
	// key is the account id exposed by the module.  Lower values are tried
	// first; accounts at the same value keep the module's existing rotation.
	AccountPriorities map[string]int
	// AccountNotes is the operator's own label for one account -- the phone
	// number or e-mail that credential was issued to.  The key is the account
	// id exposed by the module.  It is panel metadata rather than routing
	// policy: the registry only carries it so the panel can show it beside
	// the account and name the identity a re-login is about to sign in as.
	AccountNotes map[string]string
}

// platformPolicy is the normalised form stored in the registry.
type platformPolicy struct {
	priority              int
	prioritySchedule      []PriorityWindow
	disabled              map[string]struct{}
	maxInFlight           int
	maxInFlightPerAccount int
	reserveCredits        int
	accountPriorities     map[string]int
	accountNotes          map[string]string
}

// PriorityWindow is one local-time override for a platform's routing
// priority.  Minutes are counted from local midnight; EndMinute is
// exclusive.  StartMinute > EndMinute means the window crosses midnight.
type PriorityWindow struct {
	StartMinute int
	EndMinute   int
	Priority    int
}

// normalizeModelID is the key form for blacklist matching.
func normalizeModelID(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		clients:            map[string]Client{},
		aliases:            map[string]string{},
		modelGroups:        map[string]ModelGroup{},
		modelGroupByLookup: map[string]string{},
		platforms:          map[string]platformPolicy{},
		health:             NewPlatformHealth(),
	}
}

// SetPlatformConfigs installs the operator's per-platform routing policy,
// replacing whatever was there.  It is a whole-map replacement so that a
// reload that deletes a platform's entry really clears it.  It is safe to
// call while requests are in flight.
func (r *Registry) SetPlatformConfigs(cfgs map[string]PlatformConfig) {
	next := make(map[string]platformPolicy, len(cfgs))
	for name, cfg := range cfgs {
		pol := platformPolicy{priority: cfg.Priority, reserveCredits: cfg.ReserveCredits}
		if len(cfg.PrioritySchedule) > 0 {
			pol.prioritySchedule = append([]PriorityWindow(nil), cfg.PrioritySchedule...)
		}
		if len(cfg.AccountPriorities) > 0 {
			pol.accountPriorities = make(map[string]int, len(cfg.AccountPriorities))
			for id, priority := range cfg.AccountPriorities {
				if id != "" {
					pol.accountPriorities[id] = priority
				}
			}
		}
		if len(cfg.AccountNotes) > 0 {
			pol.accountNotes = make(map[string]string, len(cfg.AccountNotes))
			for id, note := range cfg.AccountNotes {
				if id == "" {
					continue
				}
				if note = strings.TrimSpace(note); note == "" {
					continue
				}
				pol.accountNotes[id] = note
			}
		}
		if cfg.MaxInFlight > 0 {
			pol.maxInFlight = cfg.MaxInFlight
		}
		if cfg.MaxInFlightPerAccount > 0 {
			pol.maxInFlightPerAccount = cfg.MaxInFlightPerAccount
		}
		if len(cfg.DisabledModels) > 0 {
			pol.disabled = make(map[string]struct{}, len(cfg.DisabledModels))
			for _, m := range cfg.DisabledModels {
				if key := normalizeModelID(m); key != "" {
					pol.disabled[key] = struct{}{}
				}
			}
		}
		next[name] = pol
	}
	r.mu.Lock()
	r.platforms = next
	r.mu.Unlock()

	priorities := make(map[string]map[string]int, len(next))
	for name, pol := range next {
		if len(pol.accountPriorities) > 0 {
			priorities[name] = pol.accountPriorities
		}
	}
	SetAccountPriorities(priorities)
}

// MaxInFlightFor reports the operator's per-platform in-flight ceiling, or 0
// when the platform has no ceiling configured.
func (r *Registry) MaxInFlightFor(name string) int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.platforms[name].maxInFlight
}

// MaxInFlightPerAccountFor reports the operator's per-account in-flight
// ceiling for a platform, or 0 when the platform has no ceiling configured.
// The gateway keys its account semaphores by (platform, account) so the same
// account id on two platforms cannot share a budget.
func (r *Registry) MaxInFlightPerAccountFor(name string) int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.platforms[name].maxInFlightPerAccount
}

// ReserveCreditsFor reports the operator's low-balance guard for a platform.
// An absent policy answers 0, which parks only a known zero balance; a
// negative value disables the guard for that platform.
func (r *Registry) ReserveCreditsFor(name string) int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.platforms[name].reserveCredits
}

// AccountPriority reports the operator's priority for one account on one
// platform.  Lower values are tried first; a missing entry is zero.
func (r *Registry) AccountPriority(platform, id string) int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.platforms[platform].accountPriorities[id]
}

// AccountNote reports the operator's own label for one account -- the phone
// number or e-mail it signs in with -- or "" when none was recorded.  It is
// display metadata only: routing never reads it.
func (r *Registry) AccountNote(platform, id string) string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.platforms[platform].accountNotes[id]
}

// ModelAllowed reports whether the named platform may be given the model.
// A platform with no policy allows everything.
func (r *Registry) ModelAllowed(platform, model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	pol, ok := r.platforms[platform]
	if !ok || len(pol.disabled) == 0 {
		return true
	}
	_, disabled := pol.disabled[normalizeModelID(model)]
	return !disabled
}

// platformPriorityCST keeps time-window matching independent of the host's
// local timezone: operators write the rules in Beijing time.
var platformPriorityCST = time.FixedZone("CST", 8*60*60)

// priority returns the platform's effective routing priority right now.
func (r *Registry) priority(name string) int {
	return r.priorityAt(name, time.Now())
}

// priorityAt is priority with an injectable clock for tests.  A matching
// schedule window overrides the base priority; otherwise the base value is
// returned unchanged.
func (r *Registry) priorityAt(name string, now time.Time) int {
	r.mu.RLock()
	pol := r.platforms[name]
	r.mu.RUnlock()
	local := now.In(platformPriorityCST)
	minute := local.Hour()*60 + local.Minute()
	for _, w := range pol.prioritySchedule {
		if priorityWindowContains(w, minute) {
			return w.Priority
		}
	}
	return pol.priority
}

// priorityWindowContains reports whether minute falls inside the half-open
// local-time window.  A start greater than the end means the window crosses
// midnight.
func priorityWindowContains(w PriorityWindow, minute int) bool {
	if w.StartMinute < w.EndMinute {
		return minute >= w.StartMinute && minute < w.EndMinute
	}
	return minute >= w.StartMinute || minute < w.EndMinute
}

// SetPlatformHealth replaces the health tracker.  A nil tracker restores the
// default empty one, so callers never have to nil-check before noting.
func (r *Registry) SetPlatformHealth(h *PlatformHealth) {
	if h == nil {
		h = NewPlatformHealth()
	}
	r.mu.Lock()
	r.health = h
	r.mu.Unlock()
}

func (r *Registry) platformHealth() *PlatformHealth {
	r.mu.RLock()
	h := r.health
	r.mu.RUnlock()
	return h
}

// NoteModelFailure records one failed round for a platform/model pair.
func (r *Registry) NoteModelFailure(client, model string, now time.Time) HealthEvent {
	return r.platformHealth().NoteFailure(client, model, now)
}

// NoteModelSuccess clears the failed-round state for a platform/model pair.
func (r *Registry) NoteModelSuccess(client, model string, now time.Time) {
	r.platformHealth().NoteSuccess(client, model, now)
}

// NoteModelUnavailable records that a platform cannot serve a model right now
// without treating that as an upstream failure.  The pair is demoted briefly
// so the next request prefers a healthy candidate, then retried.
func (r *Registry) NoteModelUnavailable(client, model string, now time.Time) HealthEvent {
	return r.platformHealth().NoteUnavailable(client, model, now)
}

// ModelDegraded reports whether a platform/model pair is in the short
// "cannot serve right now" demotion.
func (r *Registry) ModelDegraded(client, model string, now time.Time) bool {
	return r.platformHealth().Degraded(client, model, now)
}

// ModelSuppressed reports whether a platform/model pair is cooling down.
func (r *Registry) ModelSuppressed(client, model string, now time.Time) bool {
	return r.platformHealth().Suppressed(client, model, now)
}

// usableNow reports whether a platform can plausibly answer right now.  When
// the platform lists its accounts, their states are authoritative: a module
// whose only account is exhausted is not a better choice than one with a
// ready account, whatever its Ready flag says.  A module that reports no
// accounts falls back to its own Ready verdict.
func (r *Registry) usableNow(ctx context.Context, c Client) bool {
	st := c.Status(ctx)
	if len(st.Accounts) == 0 {
		return st.Ready
	}
	for _, a := range st.Accounts {
		if a.Enabled && strings.EqualFold(strings.TrimSpace(a.State), "ready") {
			return true
		}
	}
	return false
}

// Add installs a live module.
func (r *Registry) Add(c Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.clients[c.Name()]; !dup {
		r.order = append(r.order, c.Name())
	}
	r.clients[c.Name()] = c
}

// AddAlias maps a bare model name onto a fully qualified one.
func (r *Registry) AddAlias(alias, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if alias != "" && target != "" {
		r.aliases[normalizeModelID(alias)] = target
	}
}

// SetAliases replaces the whole alias table.  It is a replacement rather than
// a merge so that a reload which deletes an alias really removes it: with an
// additive reload a removed alias would keep resolving until the next start,
// which is exactly the kind of "I deleted it and it still works" surprise the
// panel is supposed to remove.  Safe to call while requests are in flight.
func (r *Registry) SetAliases(aliases map[string]string) {
	next := make(map[string]string, len(aliases))
	for k, v := range aliases {
		k = normalizeModelID(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			next[k] = v
		}
	}
	r.mu.Lock()
	r.aliases = next
	r.mu.Unlock()
}

// Alias returns the target of a bare-name alias.
func (r *Registry) Alias(alias string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	target, ok := r.aliases[normalizeModelID(alias)]
	return target, ok
}

// Aliases returns every alias mapped to its target, sorted by alias name.
func (r *Registry) Aliases() map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]string, len(r.aliases))
	for k, v := range r.aliases {
		out[k] = v
	}
	return out
}

// Get returns a module by name.
func (r *Registry) Get(name string) (Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name = strings.TrimSpace(name)
	if c, ok := r.clients[name]; ok {
		return c, true
	}
	for registered, c := range r.clients {
		if strings.EqualFold(registered, name) {
			return c, true
		}
	}
	return nil, false
}

// All returns the live modules in registration order.
func (r *Registry) All() []Client {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Client, 0, len(r.order))
	for _, n := range r.order {
		if c, ok := r.clients[n]; ok {
			out = append(out, c)
		}
	}
	return out
}

// Names returns the live module names in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Resolve maps a requested model to a module and the model name to send
// upstream.  Accepted forms:
//
//	"zcode/GLM-5.3"  -> module "zcode",     model "GLM-5.3"
//	"GLM-5.3"        -> the unique module whose catalog lists it
//	"<alias>"        -> whatever the alias table says
//
// The returned name is the module's own model id, never the qualified one.
//
// Some upstreams' own model ids contain a slash, so "the first segment names a
// module" is not the only reading of a slashed request.  "cline-free/gemini-3.8-flash"
// is cline's model, not a module called "cline-free"; every OpenRouter id looks
// like "anthropic/claude-sonnet-4.5".  A request whose first segment names no
// module therefore falls through to the catalog search before it is rejected.
// Candidate is one platform/model pair that can serve a request.  Free is
// true only when the module explicitly marked the model free.
type Candidate struct {
	Client Client
	Model  string
	Free   bool
	// group is the normalised model-group key that produced this
	// candidate, or "" for an ordinary catalogue match.
	group string
}

func modelFree(m Model) bool {
	free, _ := m.Extra["free"].(bool)
	return free
}

// Resolve maps a requested model to its first, best candidate.
func (r *Registry) Resolve(ctx context.Context, model string) (Client, string, error) {
	candidates, err := r.ResolveCandidates(ctx, model)
	if err != nil {
		return nil, "", err
	}
	first := candidates[0]
	return first.Client, first.Model, nil
}

// ResolveCandidates maps a requested model to every platform that can serve
// it, in failover order.  A qualified request has exactly one candidate;
// a bare id returns all owners so the gateway can rotate across platforms.
//
// Ordering is: usable account, not in failure cooldown, operator priority
// (lower first), free model, platform name.  A single-owner request keeps
// the cheap path and does not consult Status.
func (r *Registry) ResolveCandidates(ctx context.Context, model string) ([]Candidate, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model is required")
	}

	r.mu.RLock()
	alias, hasAlias := r.aliases[model]
	r.mu.RUnlock()
	if !hasAlias {
		r.mu.RLock()
		alias, hasAlias = r.aliases[normalizeModelID(model)]
		r.mu.RUnlock()
	}
	if hasAlias {
		model = alias
	}

	// Auto/<model> is the gateway's virtual routing name: strip the prefix
	// and resolve the rest exactly like a bare model id, so platform
	// priority, health and failover stay in one implementation.
	auto := false
	if len(model) >= len("auto/") && strings.EqualFold(model[:len("auto/")], "auto/") {
		model = strings.TrimSpace(model[len("auto/"):])
		if model == "" {
			return nil, fmt.Errorf("model is required after Auto/")
		}
		auto = true
	}

	prefix, rest, qualified := strings.Cut(model, "/")
	if !auto && qualified && prefix != "" && rest != "" {
		if c, found := r.Get(prefix); found {
			upstream := rest
			if models, err := c.Models(ctx); err == nil {
				for _, m := range models {
					if strings.EqualFold(strings.TrimSpace(m.ID), rest) {
						upstream = m.ID
						break
					}
				}
			}
			if !r.ModelAllowed(c.Name(), upstream) {
				return nil, fmt.Errorf("model %q is disabled on client %q; re-enable it in the platform configuration or qualify another client", upstream, c.Name())
			}
			return []Candidate{{Client: c, Model: upstream}}, nil
		}
	}

	// Operator-declared equivalence is considered only after an explicit
	// platform prefix had its chance to pin the request.
	if groupKey, group, ok := r.modelGroupForLookup(model); ok {
		return r.resolveModelGroup(ctx, groupKey, group)
	}

	// Bare name, or a slashed id whose first segment is not a module: search
	// every module's catalog for a match the platform policy still allows.
	var (
		owners   []Candidate
		disabled []string
	)
	for _, c := range r.All() {
		models, err := c.Models(ctx)
		if err != nil {
			continue
		}
		matched, ok, blocked := selectCatalogModel(models, model, !strings.ContainsAny(model, "/:"), func(m Model) bool {
			return r.ModelAllowed(c.Name(), m.ID)
		})
		if !ok {
			if blocked {
				disabled = append(disabled, c.Name())
			}
			continue
		}
		owners = append(owners, Candidate{Client: c, Model: matched.ID, Free: modelFree(matched)})
	}

	switch len(owners) {
	case 0:
		if len(disabled) > 0 {
			sort.Strings(disabled)
			return nil, fmt.Errorf("model %q is disabled on every client that serves it (%s); re-enable it in the platform configuration", model, strings.Join(disabled, ", "))
		}
		if qualified && prefix != "" && rest != "" {
			return nil, fmt.Errorf("unknown client %q in model %q, and no module lists that id; qualify it as <client>/%s", prefix, model, model)
		}
		return nil, fmt.Errorf("no client serves model %q; qualify it as <client>/%s", model, model)
	case 1:
		return owners, nil
	}

	// Several platforms can serve the same id.  Rank them: a platform that
	// can answer now beats one that cannot, a pair in failure cooldown is
	// demoted, then the operator's priority, free marker and name make the
	// order deterministic.  Status is consulted only on this multi-owner path.

	return r.rankCandidates(ctx, owners), nil
}

// rankCandidates orders the multi-platform candidates that can serve one
// request.  A group-specific priority is used when one is configured; all
// other projects keep the existing global priority and schedule fallback.
func (r *Registry) rankCandidates(ctx context.Context, owners []Candidate) []Candidate {
	if len(owners) < 2 {
		return owners
	}

	type scored struct {
		Candidate
		usable     bool
		suppressed bool
		priority   int
	}
	ranked := make([]scored, len(owners))
	now := time.Now()
	for i, o := range owners {
		ranked[i] = scored{
			Candidate: o,
			usable:    r.usableNow(ctx, o.Client),
			suppressed: r.ModelSuppressed(o.Client.Name(), o.Model, now) ||
				r.ModelDegraded(o.Client.Name(), o.Model, now),
			priority: r.priorityFor(o.Client.Name(), o.group),
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.usable != b.usable {
			return a.usable
		}
		if a.suppressed != b.suppressed {
			return !a.suppressed
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if a.Free != b.Free {
			return a.Free
		}
		return a.Client.Name() < b.Client.Name()
	})

	out := make([]Candidate, len(ranked))
	for i := range ranked {
		out[i] = ranked[i].Candidate
	}
	return out
}
