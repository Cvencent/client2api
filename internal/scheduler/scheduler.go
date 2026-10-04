// Package scheduler runs a client module's declared daily batches on a
// timetable.
//
// It is deliberately vendor-blind.  It knows nothing about any task code, any
// chore name and any protocol: it learns *what* to run from a client that
// implements core.BatchPlanner and it executes the codes through
// core.TaskProvider.  A registered client that implements neither is simply
// never scheduled, exactly like every other optional capability in core.
//
// The timetable is always interpreted in CST (China Standard Time, UTC+8), a
// fixed-offset zone, never in the host's local zone: the reference vendor's
// daily reset is defined in Beijing time, so a gateway running in UTC or in
// America/New_York must still fire at the same absolute instant.
//
// # Timetable
//
//	Config.Enabled            master switch; false means Run blocks, idle
//	Group{Enabled, Hours}     one batch's hours of day in CST, 0-23
//	BalanceRefresh{...}       an independent periodic tick each Every
//
// Batch names the reference gateway schedules are "checkin", "travel",
// "activity", "keepalive", "blackcat" and "growth"; a module may plan others,
// but only those with a matching group in Config can ever fire.
//
// # Pacing
//
// Pacing is protocol, not politeness: the vendor rolls back sub-second bursts,
// so a batch waits Batch.AccountGap (DefaultAccountGap when zero) between two
// consecutive accounts, with +/-20% jitter so the traffic is not a metronome,
// and waits Batch.Settle (DefaultSettle when zero) afterwards before the
// report is published, because upstream scoring lags and an immediate
// read-back would double-run.
//
// # Everything is injectable
//
// Deps.Now and Deps.Sleep carry the clock, so tests never wait on real time;
// Deps.Sleep returning false means the context died and Run returns.
package scheduler

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// CST is the reference gateway's fixed China Standard Time zone.  The
// timetable is always interpreted in this zone regardless of the host's TZ.
var CST = time.FixedZone("CST", 8*60*60)

// Defaults used when a module leaves the pacing unspecified.
const (
	// DefaultAccountGap is the wait between two consecutive accounts when
	// Batch.AccountGap is zero.  Never fire two accounts back to back.
	DefaultAccountGap = 45 * time.Second
	// DefaultSettle is the post-batch wait before the report is trusted when
	// Batch.Settle is zero.
	DefaultSettle = 5 * time.Second
	// BalanceTickName is the synthetic key Status.Next carries the next
	// balance-refresh tick under, so a panel can show it next to the batches
	// without Status growing a vendor-shaped field.
	BalanceTickName = "balance_refresh"
)

// jitterFrac is the width of the pacing jitter, symmetric around the nominal
// duration.
const jitterFrac = 0.20

// historyMax bounds the finished-run journal.  The panel shows a page of
// it; keeping more would only grow the status payload.
const historyMax = 200

// TriggerSchedule and TriggerManual name what started a batch run.  They
// are the two ways a run can begin: the timetable, or an operator.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Batch names the reference timetable knows about.  A module is free to use
// other names; such a batch is runnable through RunBatchNow but has no
// timetable of its own and is therefore never scheduled by Run.
const (
	batchCheckin   = "checkin"
	batchTravel    = "travel"
	batchActivity  = "activity"
	batchKeepalive = "keepalive"
	batchBlackcat  = "blackcat"
	batchGrowth    = "growth"
)

// Group is one scheduled batch's timetable.
type Group struct {
	Enabled bool
	Hours   []int // hours of day in CST, 0-23
}

// Config is the operator's timetable.
type Config struct {
	Enabled   bool
	Checkin   Group
	Travel    Group
	Activity  Group
	Keepalive Group
	Blackcat  Group
	Growth    Group
	// Clients overrides one platform's timetable for named batches.  The
	// outer key is a client name and the inner key a batch name, both
	// matched case-insensitively.  A (client, batch) pair that is absent
	// here falls back to the shared group above, so an install that never
	// writes an override keeps exactly the reference timetable.
	Clients map[string]map[string]Group
	// BalanceRefresh is a vendor-neutral periodic tick, independent of the
	// batches above.  The scheduler itself does nothing vendor-specific on a
	// tick: it calls Deps.OnBalanceRefresh when the host set one.
	BalanceRefresh struct {
		Enabled bool
		Every   time.Duration
	}
}

// DefaultConfig returns the reference's recommended timetable.
func DefaultConfig() Config {
	cfg := Config{
		Enabled:   true,
		Checkin:   Group{Enabled: true, Hours: []int{8}},
		Travel:    Group{Enabled: true, Hours: []int{9}},
		Activity:  Group{Enabled: true, Hours: []int{10}},
		Keepalive: Group{Enabled: true, Hours: []int{8, 14, 20}},
		Blackcat:  Group{Enabled: true, Hours: []int{23}},
		Growth:    Group{Enabled: true, Hours: []int{12}},
	}
	cfg.BalanceRefresh.Enabled = true
	cfg.BalanceRefresh.Every = 5 * time.Minute
	return cfg
}

// groupByName returns the timetable for one batch name.  Names are matched
// case-insensitively because they arrive from operator JSON and from a
// module's own Batches.
func (c Config) groupByName(name string) (Group, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case batchCheckin:
		return c.Checkin, true
	case batchTravel:
		return c.Travel, true
	case batchActivity:
		return c.Activity, true
	case batchKeepalive:
		return c.Keepalive, true
	case batchBlackcat:
		return c.Blackcat, true
	case batchGrowth:
		return c.Growth, true
	}
	return Group{}, false
}

// GroupFor returns the timetable that applies to one client's batch.  A
// per-client override wins over the shared group; when the operator wrote
// neither, the second result is false and the caller must not schedule
// that batch at all.  Client and batch names are matched
// case-insensitively, because both arrive from operator JSON.
func (c Config) GroupFor(client, batch string) (Group, bool) {
	if g, ok := c.Override(client, batch); ok {
		return g, true
	}
	return c.groupByName(batch)
}

// Override returns the per-client entry for one batch, reporting false when
// the operator never wrote one.  The panel uses it to tell "this platform
// has its own hours" from "this platform follows the shared timetable".
func (c Config) Override(client, batch string) (Group, bool) {
	for name, byBatch := range c.Clients {
		if !strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(client)) {
			continue
		}
		for b, g := range byBatch {
			if strings.EqualFold(strings.TrimSpace(b), strings.TrimSpace(batch)) {
				return g, true
			}
		}
	}
	return Group{}, false
}

// NamedGroup pairs a batch name with one timetable.
type NamedGroup struct {
	Name  string
	Group Group
}

// Groups lists the shared timetable in the reference's order, so the panel
// can render "follow the shared hours" without hard-coding the batch names.
func (c Config) Groups() []NamedGroup {
	src := c.groups()
	out := make([]NamedGroup, 0, len(src))
	for _, e := range src {
		out = append(out, NamedGroup{Name: e.name, Group: *e.g})
	}
	return out
}

// Shared returns the shared (not per-client) timetable for one batch name.
func (c Config) Shared(batch string) (Group, bool) {
	return c.groupByName(batch)
}

// groups lists every group with its name, in the reference's order.
func (c *Config) groups() []struct {
	name string
	g    *Group
} {
	return []struct {
		name string
		g    *Group
	}{
		{batchCheckin, &c.Checkin},
		{batchTravel, &c.Travel},
		{batchActivity, &c.Activity},
		{batchKeepalive, &c.Keepalive},
		{batchBlackcat, &c.Blackcat},
		{batchGrowth, &c.Growth},
	}
}

// normalizeHoursInPlace drops out-of-range values, de-duplicates and sorts,
// writing the survivors back into h's own backing array.  The second result
// reports whether anything was out of range.
func normalizeHoursInPlace(h []int) ([]int, bool) {
	if len(h) == 0 {
		return h, false
	}
	seen := make(map[int]struct{}, len(h))
	out := h[:0] // same backing array: the filter below writes in place
	dropped := false
	for _, v := range h {
		if v < 0 || v > 23 {
			dropped = true
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Ints(out)
	return out, dropped
}

// Validate normalizes every Hours slice in place: it drops values outside
// 0..23, de-duplicates and sorts.  It returns an error naming the offending
// batch if any hour is out of range.  A batch with Enabled true and no hours
// returns an error (it could never fire).
//
// A zero Config is valid and simply means "nothing enabled".
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("scheduler: nil config")
	}
	groups := c.groups()
	var outOfRange []string
	for _, e := range groups {
		h, dropped := normalizeHoursInPlace(e.g.Hours)
		e.g.Hours = h
		if dropped {
			outOfRange = append(outOfRange, e.name)
		}
	}
	// Per-client overrides are normalized the same way, and named
	// "client/batch" so a bad hour points at the platform that wrote it.
	overrides := make([]string, 0, len(c.Clients))
	for client := range c.Clients {
		overrides = append(overrides, client)
	}
	sort.Strings(overrides)
	for _, client := range overrides {
		byBatch := c.Clients[client]
		batches := make([]string, 0, len(byBatch))
		for batch := range byBatch {
			batches = append(batches, batch)
		}
		sort.Strings(batches)
		for _, batch := range batches {
			g := byBatch[batch]
			h, dropped := normalizeHoursInPlace(g.Hours)
			g.Hours = h
			byBatch[batch] = g
			if dropped {
				outOfRange = append(outOfRange, client+"/"+batch)
			}
		}
	}
	if len(outOfRange) > 0 {
		return fmt.Errorf("scheduler: batch %s has hours outside 0..23", strings.Join(outOfRange, ", "))
	}
	for _, e := range groups {
		if e.g.Enabled && len(e.g.Hours) == 0 {
			return fmt.Errorf("scheduler: batch %q is enabled but has no hours", e.name)
		}
	}
	for _, client := range overrides {
		for batch, g := range c.Clients[client] {
			if g.Enabled && len(g.Hours) == 0 {
				return fmt.Errorf("scheduler: batch %q for %s is enabled but has no hours", batch, client)
			}
		}
	}
	return nil
}

// NextFire returns the first instant strictly after now whose hour in loc is
// one of hours.  Empty hours means the zero time (never fires).
//
// Fires happen on the hour, in loc: a batch scheduled for 09:00 in every zone
// is the same absolute instant whichever zone the host or now is expressed in.
func NextFire(now time.Time, hours []int, loc *time.Location) time.Time {
	if loc == nil {
		loc = CST
	}
	hs, _ := normalizeHoursInPlace(append([]int(nil), hours...))
	if len(hs) == 0 {
		return time.Time{}
	}
	n := now.In(loc)
	midnight := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	// Three days is enough for any zone (a fixed-offset zone needs two); the
	// extra day keeps a DST zone from returning a zero time by accident.
	for day := 0; day < 3; day++ {
		d := midnight.AddDate(0, 0, day)
		for _, h := range hs {
			cand := time.Date(d.Year(), d.Month(), d.Day(), h, 0, 0, 0, loc)
			if cand.After(now) {
				return cand
			}
		}
	}
	return time.Time{}
}

// jitter returns d spread by +/-20%, so repeated gaps are not a metronome.
// A non-positive duration is returned unchanged.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	factor := 1 + (rand.Float64()*2-1)*jitterFrac
	out := time.Duration(float64(d) * factor)
	if out < 0 {
		out = 0
	}
	return out
}

// Deps is what the runner needs from its host.
type Deps struct {
	Registry *core.Registry
	Logf     func(format string, args ...any)
	// Now and Sleep are injectable so tests never wait on a real clock.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) bool
	// OnBalanceRefresh is called on every balance-refresh tick when
	// BalanceRefresh.Enabled is set.  It is the host's own work: the
	// scheduler has no idea what a balance is.
	OnBalanceRefresh func(ctx context.Context)
	// HistoryPath, when non-empty, persists the finished-run journal as
	// JSON so the panel can still show yesterday's runs after a restart.
	// Empty keeps the history in memory for the life of the process.
	HistoryPath string
}

// Runner fires batches at their scheduled hours.
type Runner struct {
	mu      sync.RWMutex
	cfg     Config
	last    map[string]Report
	balNext time.Time
	// history is the finished-run journal, oldest first.  It is bounded
	// by historyMax and persisted to historyPath when the host set one.
	history     []RunRecord
	historyPath string

	reg   *core.Registry
	logf  func(format string, args ...any)
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
	onBal func(ctx context.Context)

	// wake lets Reconfigure disturb a Run that is blocked because nothing is
	// scheduled at all.
	wake chan struct{}
}

// New returns a Runner that is configured to do nothing.  The host installs
// the operator's timetable with Reconfigure; until then Run is idle, so a
// process that never configures a schedule never touches a vendor.
func New(deps Deps) *Runner {
	r := &Runner{
		last:  map[string]Report{},
		reg:   deps.Registry,
		logf:  deps.Logf,
		now:   deps.Now,
		sleep: deps.Sleep,
		onBal: deps.OnBalanceRefresh,
		wake:  make(chan struct{}, 1),
	}
	r.historyPath = deps.HistoryPath
	if r.historyPath != "" {
		// A missing or corrupt file is not an error: the journal is a
		// convenience, and losing it must never keep the gateway from
		// starting.
		var hist []RunRecord
		if err := core.ReadJSON(r.historyPath, &hist); err == nil && len(hist) > 0 {
			if len(hist) > historyMax {
				hist = hist[len(hist)-historyMax:]
			}
			r.history = hist
		}
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.sleep == nil {
		r.sleep = sleepCtx
	}
	r.log("[scheduler] initialized (idle until Reconfigure)")
	return r
}

func (r *Runner) log(format string, args ...any) {
	if r.logf == nil {
		return
	}
	r.logf(format, args...)
}

// sleepCtx is the production Deps.Sleep.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// config returns the current timetable and its next balance tick.
func (r *Runner) config() (Config, time.Time) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg, r.balNext
}

// Reconfigure swaps the timetable.  Safe to call concurrently with Run and it
// must take effect at the next wake-up, not retroactively.
//
// It never fails: an invalid timetable is normalized, logged and installed, so
// one bad hour cannot stop the scheduler dead.
func (r *Runner) Reconfigure(cfg Config) {
	now := r.now()
	if err := cfg.Validate(); err != nil {
		r.log("[scheduler] invalid timetable: %v", err)
	}
	cfg = cloneConfig(cfg)
	var balNext time.Time
	if cfg.BalanceRefresh.Enabled && cfg.BalanceRefresh.Every > 0 {
		balNext = now.Add(cfg.BalanceRefresh.Every)
	}
	r.mu.Lock()
	r.cfg = cfg
	r.balNext = balNext
	r.mu.Unlock()

	r.log("[scheduler] reconfigured: enabled=%v checkin=%v travel=%v activity=%v keepalive=%v blackcat=%v growth=%v balance=%v/%v",
		cfg.Enabled, hoursOf(cfg.Checkin), hoursOf(cfg.Travel), hoursOf(cfg.Activity),
		hoursOf(cfg.Keepalive), hoursOf(cfg.Blackcat), hoursOf(cfg.Growth),
		cfg.BalanceRefresh.Enabled, cfg.BalanceRefresh.Every)
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// cloneConfig copies the Hours slices so a caller that reuses its own slices
// cannot change the live timetable behind our back.
func cloneConfig(cfg Config) Config {
	groups := cfg.groups()
	for _, e := range groups {
		if len(e.g.Hours) > 0 {
			h := make([]int, len(e.g.Hours))
			copy(h, e.g.Hours)
			e.g.Hours = h
		}
	}
	if len(cfg.Clients) > 0 {
		cp := make(map[string]map[string]Group, len(cfg.Clients))
		for client, byBatch := range cfg.Clients {
			cb := make(map[string]Group, len(byBatch))
			for batch, g := range byBatch {
				if len(g.Hours) > 0 {
					h := make([]int, len(g.Hours))
					copy(h, g.Hours)
					g.Hours = h
				}
				cb[batch] = g
			}
			cp[client] = cb
		}
		cfg.Clients = cp
	}
	return cfg
}

func hoursOf(g Group) string {
	if !g.Enabled {
		return "off"
	}
	return fmt.Sprint(g.Hours)
}

// fire is one pending (client, batch) firing.
type fire struct {
	client string
	batch  string
	at     time.Time
}

// plan computes every pending firing strictly after now.
//
// A batch only counts when a registered client both plans it and can run it:
// core.BatchPlanner says what it is, core.TaskProvider says how to run it.  A
// batch whose name has no timetable group -- or whose group is disabled -- is
// never scheduled.
func (r *Runner) plan(now time.Time, cfg Config) []fire {
	if r.reg == nil || !cfg.Enabled {
		return nil
	}
	var out []fire
	for _, c := range r.reg.All() {
		batches := core.PlannedBatches(c)
		if len(batches) == 0 {
			continue
		}
		for _, b := range batches {
			if !runnableBatch(c, b) {
				continue
			}
			g, known := cfg.GroupFor(c.Name(), b.Name)
			if !known || !g.Enabled {
				continue
			}
			at := NextFire(now, g.Hours, CST)
			if at.IsZero() {
				continue
			}
			out = append(out, fire{client: c.Name(), batch: b.Name, at: at})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].at.Equal(out[j].at) {
			return out[i].at.Before(out[j].at)
		}
		if out[i].client != out[j].client {
			return out[i].client < out[j].client
		}
		return out[i].batch < out[j].batch
	})
	return out
}

// balanceNext reports the next balance tick.  A tick is only planned when the
// master switch and the tick itself are enabled with a positive period.
func (r *Runner) balanceNext(now time.Time, cfg Config, stored time.Time) (time.Time, bool) {
	if !cfg.Enabled || !cfg.BalanceRefresh.Enabled || cfg.BalanceRefresh.Every <= 0 {
		return time.Time{}, false
	}
	if stored.IsZero() {
		return now.Add(cfg.BalanceRefresh.Every), true
	}
	return stored, true
}

// Run blocks until ctx is cancelled, firing batches at their scheduled hours.
func (r *Runner) Run(ctx context.Context) {
	r.log("[scheduler] run started")
	defer r.log("[scheduler] run stopped")

	for {
		if ctx.Err() != nil {
			return
		}
		cfg, balStored := r.config()
		if !cfg.Enabled {
			// Idle: no spinning, but a Reconfigure still wakes us up.
			r.log("[scheduler] idle: master switch off")
			if !r.wait(ctx) {
				return
			}
			continue
		}

		now := r.now()
		fires := r.plan(now, cfg)
		balDue, hasBal := r.balanceNext(now, cfg, balStored)

		var earliest time.Time
		if hasBal {
			earliest = balDue
		}
		for _, f := range fires {
			if earliest.IsZero() || f.at.Before(earliest) {
				earliest = f.at
			}
		}
		if earliest.IsZero() {
			// Nothing is enabled (or nobody plans a scheduled batch): block.
			r.log("[scheduler] idle: nothing scheduled")
			if !r.wait(ctx) {
				return
			}
			continue
		}

		if d := earliest.Sub(now); d > 0 {
			if !r.sleep(ctx, d) {
				r.log("[scheduler] stopping: context done while waiting %s", d)
				return
			}
		}

		now = r.now()
		for _, f := range fires {
			if f.at.After(now) {
				continue
			}
			r.runBatch(ctx, f.client, f.batch)
			if ctx.Err() != nil {
				return
			}
		}
		if hasBal && !balDue.After(now) {
			r.tickBalance(ctx, cfg)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

// wait blocks until the context dies or the timetable changes.
func (r *Runner) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-r.wake:
		return true
	}
}

// tickBalance advances the tick and hands it to the host's hook.
func (r *Runner) tickBalance(ctx context.Context, cfg Config) {
	every := cfg.BalanceRefresh.Every
	r.mu.Lock()
	r.balNext = r.now().Add(every)
	hook := r.onBal
	r.mu.Unlock()
	r.log("[scheduler] balance refresh tick (every %s)", every)
	if hook != nil {
		hook(ctx)
	}
}

// RunBalanceRefreshNow hands the balance-refresh tick to the host's hook right
// now, ignoring the timetable, and reports whether a hook was there to take it.
//
// The false answer is the same "not implemented" verdict every other optional
// mechanism gives: a process whose host never wired Deps.OnBalanceRefresh has
// no balance to refresh, and the panel must not draw a button for it.
//
// It deliberately does not touch r.balNext.  A manual press is the operator
// asking for one now, not a renegotiation of the schedule: moving the stored
// tick would let two clicks in a row silently postpone the next automatic
// refresh.
func (r *Runner) RunBalanceRefreshNow(ctx context.Context) bool {
	r.mu.Lock()
	hook := r.onBal
	r.mu.Unlock()
	if hook == nil {
		return false
	}
	r.log("[scheduler] balance refresh now (manual)")
	hook(ctx)
	return true
}

// RunBatchNow runs one batch immediately, ignoring the timetable, and returns
// what happened.  The second result is false when no client by that name
// exists or it defines no such batch.
//
// It ignores both Config.Enabled and the batch's Group.Enabled on purpose: it
// is the operator pressing a button.
func (r *Runner) RunBatchNow(ctx context.Context, client, batch string) (Report, bool) {
	if r.reg == nil {
		return Report{}, false
	}
	c, ok := r.reg.Get(client)
	if !ok {
		r.log("[scheduler] %s/%s: no such client", client, batch)
		return Report{}, false
	}
	b, ok := core.BatchOf(c, batch)
	if !ok {
		r.log("[scheduler] %s/%s: client does not plan that batch", client, batch)
		return Report{}, false
	}
	if !runnableBatch(c, b) {
		r.log("[scheduler] %s/%s: client cannot run that batch", client, batch)
		return Report{}, false
	}
	rep := r.runBatchAs(ctx, client, batch, TriggerManual)
	return rep, true
}

// runBatch executes one scheduled batch for one client.
func (r *Runner) runBatch(ctx context.Context, clientName, batchName string) Report {
	return r.runBatchAs(ctx, clientName, batchName, TriggerSchedule)
}

// runBatchAs executes one batch for one client and returns its report.  It
// never aborts on one account or one code.  trigger is what the run journal
// records as the reason: the timetable, or an operator's button.
func (r *Runner) runBatchAs(ctx context.Context, clientName, batchName, trigger string) Report {
	rep := Report{Client: clientName, Batch: batchName, Started: r.now()}
	finish := func(b core.Batch) Report {
		rep.Duration = r.now().Sub(rep.Started)
		settle := b.Settle
		if settle <= 0 {
			settle = DefaultSettle
		}
		// Upstream scoring lags; trusting a read-back immediately would
		// double-run.  Tests route this through Deps.Sleep.
		r.sleep(ctx, settle)
		r.record(rep, trigger)
		r.log("[scheduler] %s/%s done: accounts=%d ran=%d refused=%d failed=%d in %s",
			rep.Client, rep.Batch, rep.Accounts, rep.Ran, rep.Refused, rep.Failed, rep.Duration)
		return rep
	}

	if r.reg == nil {
		r.log("[scheduler] %s/%s: no registry", clientName, batchName)
		rep.Duration = r.now().Sub(rep.Started)
		r.record(rep, trigger)
		return rep
	}
	c, ok := r.reg.Get(clientName)
	if !ok {
		r.log("[scheduler] %s/%s: client vanished", clientName, batchName)
		rep.Duration = r.now().Sub(rep.Started)
		r.record(rep, trigger)
		return rep
	}
	b, ok := core.BatchOf(c, batchName)
	if !ok {
		r.log("[scheduler] %s/%s: batch no longer planned", clientName, batchName)
		rep.Duration = r.now().Sub(rep.Started)
		r.record(rep, trigger)
		return rep
	}
	if core.IsCheckinBatch(c, b) {
		return r.runCheckinBatchAs(ctx, c, b, trigger)
	}
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		r.log("[scheduler] %s/%s: client is not a TaskProvider", clientName, batchName)
		rep.Duration = r.now().Sub(rep.Started)
		r.record(rep, trigger)
		return rep
	}

	// Every code in Codes, then every code in Claim.
	codes := make([]string, 0, len(b.Codes)+len(b.Claim))
	codes = append(codes, b.Codes...)
	codes = append(codes, b.Claim...)

	accounts, err := accountsOf(ctx, c)
	if err != nil {
		// A discovery failure is a Go error, but it must not abort the batch:
		// fall back to the module's own default account ("") and say so.
		rep.Failed++
		rep.Errors = append(rep.Errors, err.Error())
		r.log("[scheduler] %s/%s: listing accounts failed, using the default account: %v", clientName, batchName, err)
	}
	rep.Accounts = len(accounts)

	gap := b.AccountGap
	if gap <= 0 {
		gap = DefaultAccountGap
	}

	ranAnAccount := false
	for _, acc := range accounts {
		if ctx.Err() != nil {
			r.log("[scheduler] %s/%s: context done, stopping mid-batch", clientName, batchName)
			break
		}
		if b.Gate != "" {
			open, gerr := gateOpen(ctx, tp, acc, b.Gate)
			if gerr != nil {
				rep.Failed++
				rep.Errors = append(rep.Errors, gerr.Error())
				r.log("[scheduler] %s/%s account %q: reading tasks for gate %q failed: %v",
					clientName, batchName, acc, b.Gate, gerr)
				continue
			}
			if !open {
				r.log("[scheduler] %s/%s account %q: gated on %q, skipping", clientName, batchName, acc, b.Gate)
				continue
			}
		}
		if ranAnAccount {
			if !r.sleep(ctx, jitter(gap)) {
				r.log("[scheduler] %s/%s: context done while pacing accounts", clientName, batchName)
				break
			}
		}
		ranAnAccount = true

		for _, code := range codes {
			if ctx.Err() != nil {
				break
			}
			res, rerr := tp.RunTask(ctx, acc, code)
			if rerr != nil {
				// Transport/Go error: record it, keep going.
				rep.Failed++
				rep.Errors = append(rep.Errors, rerr.Error())
				r.log("[scheduler] %s/%s account %q code %q failed: %v", clientName, batchName, acc, code, rerr)
				continue
			}
			rep.Ran++
			if !res.OK {
				// A vendor refusal -- already claimed, prerequisite missing,
				// anti-cheat rollback -- is legitimate, not an error.
				rep.Refused++
				r.log("[scheduler] %s/%s account %q code %q refused: %s", clientName, batchName, acc, code, res.Message)
			}
		}
	}
	return finish(b)
}

// accountsOf resolves the account IDs a batch runs over.  A client with no
// account manager (or one that holds no records) gets the single default
// account "" -- core.TaskProvider documents that as "whichever account you
// would use anyway".  When the manager does hold records, only the enabled
// ones run; deciding all of them off means running nothing.
func accountsOf(ctx context.Context, c core.Client) ([]string, error) {
	am, ok := core.AsAccountManager(c)
	if !ok {
		return []string{""}, nil
	}
	recs, err := am.Accounts(ctx)
	if err != nil {
		return []string{""}, err
	}
	if len(recs) == 0 {
		return []string{""}, nil
	}
	ids := make([]string, 0, len(recs))
	for _, rec := range recs {
		if rec.Enabled {
			ids = append(ids, rec.ID)
		}
	}
	return ids, nil
}

// gateOpen reports whether the batch's gate is present and still incomplete.
// A missing gate and an already satisfied gate both mean "not worth running",
// which is a skip, not a failure.
func gateOpen(ctx context.Context, tp core.TaskProvider, accountID, gate string) (bool, error) {
	tasks, err := tp.Tasks(ctx, accountID)
	if err != nil {
		return false, err
	}
	for _, t := range tasks {
		if t.Code != gate {
			continue
		}
		if t.Claimed {
			return false, nil
		}
		if t.Target > 0 && t.Current >= t.Target {
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

// record publishes one report as the batch's last run and appends it to the
// bounded run journal.  The file write happens outside the lock: a slow disk
// must not stall Status or a concurrent batch.
func (r *Runner) record(rep Report, trigger string) {
	run := RunRecord{
		Client:   rep.Client,
		Batch:    rep.Batch,
		Trigger:  trigger,
		Started:  rep.Started,
		Duration: rep.Duration,
		Accounts: rep.Accounts,
		Ran:      rep.Ran,
		Refused:  rep.Refused,
		Failed:   rep.Failed,
	}
	if len(rep.Errors) > 0 {
		run.Error = strings.Join(rep.Errors, "; ")
	}

	r.mu.Lock()
	if r.last == nil {
		r.last = map[string]Report{}
	}
	r.last[rep.Client+"/"+rep.Batch] = rep
	r.history = append(r.history, run)
	if len(r.history) > historyMax {
		overflow := len(r.history) - historyMax
		kept := make([]RunRecord, historyMax)
		copy(kept, r.history[overflow:])
		r.history = kept
	}
	snapshot := append([]RunRecord(nil), r.history...)
	path := r.historyPath
	r.mu.Unlock()

	if path != "" {
		if err := core.WriteJSONAtomic(path, snapshot); err != nil {
			r.log("[scheduler] writing run history: %v", err)
		}
	}
}

// RunRecord is one finished batch run, as the run-history view shows it.
type RunRecord struct {
	Client   string        `json:"client"`
	Batch    string        `json:"batch"`
	Trigger  string        `json:"trigger"` // TriggerSchedule or TriggerManual
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Accounts int           `json:"accounts"`
	Ran      int           `json:"ran"`
	Refused  int           `json:"refused"`
	Failed   int           `json:"failed"`
	Error    string        `json:"error,omitempty"`
}

// History returns the run journal newest-first.  The copy is shallow on
// purpose: RunRecord has no slices, so a caller cannot reach back into the
// runner's state.
func (r *Runner) History() []RunRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]RunRecord, len(r.history))
	for i := range r.history {
		out[i] = r.history[len(r.history)-1-i]
	}
	return out
}

// Config returns a copy of the live timetable, so a read-only caller (the
// panel's schedule view) can show the effective hours and the per-platform
// overrides without reading the config file back.
func (r *Runner) Config() Config {
	cfg, _ := r.config()
	return cfg
}

// Report summarizes one batch execution.
type Report struct {
	Client   string        `json:"client"`
	Batch    string        `json:"batch"`
	Started  time.Time     `json:"started"`
	Duration time.Duration `json:"duration"`
	Accounts int           `json:"accounts"`
	Ran      int           `json:"ran"`
	Refused  int           `json:"refused"` // TaskResult.OK == false: legitimate, NOT an error
	Failed   int           `json:"failed"`  // transport/Go errors
	Errors   []string      `json:"errors,omitempty"`
}

// Status is a read-only snapshot for the panel.
//
// Next is computed from the live timetable at the moment Status is called, so
// it lists exactly the enabled batches some registered client actually plans,
// keyed by batch name (the earliest client when several plan the same one).
// When the balance tick is enabled it appears there under BalanceTickName.
type Status struct {
	Enabled bool                 `json:"enabled"`
	Next    map[string]time.Time `json:"next"` // batch name -> next fire
	// NextByClient is the same plan keyed "client/batch", so the panel can
	// name the platform that owns each fire instead of showing one shared
	// time for every platform that plans the same batch.
	NextByClient map[string]time.Time `json:"next_by_client"`
	Last         map[string]Report    `json:"last"` // "client/batch" -> last run
}

// Status returns a read-only snapshot.
func (r *Runner) Status() Status {
	cfg, balStored := r.config()
	now := r.now()

	st := Status{
		Enabled:      cfg.Enabled,
		Next:         map[string]time.Time{},
		NextByClient: map[string]time.Time{},
		Last:         map[string]Report{},
	}
	if cfg.Enabled {
		for _, f := range r.plan(now, cfg) {
			if prev, ok := st.Next[f.batch]; !ok || f.at.Before(prev) {
				st.Next[f.batch] = f.at
			}
			key := f.client + "/" + f.batch
			if prev, ok := st.NextByClient[key]; !ok || f.at.Before(prev) {
				st.NextByClient[key] = f.at
			}
		}
		if at, ok := r.balanceNext(now, cfg, balStored); ok {
			st.Next[BalanceTickName] = at
		}
	}
	r.mu.RLock()
	for k, v := range r.last {
		st.Last[k] = v
	}
	r.mu.RUnlock()
	return st
}

// runnableBatch reports whether this build can execute one planned batch.
// The synthetic check-in batch needs CheckinProvider; every batch a module
// declares for itself needs TaskProvider.
func runnableBatch(c core.Client, b core.Batch) bool {
	if core.IsCheckinBatch(c, b) {
		_, ok := core.AsCheckinProvider(c)
		return ok
	}
	_, ok := core.AsTaskProvider(c)
	return ok
}

// runCheckinBatchAs executes the synthetic check-in batch PlannedBatches gives
// a module that can check in for one account but declares no task codes.  It
// paces accounts exactly like the task-code path and maps every
// CheckinResult onto the same Report shape, so the run journal keeps one form.
func (r *Runner) runCheckinBatchAs(ctx context.Context, c core.Client, b core.Batch, trigger string) Report {
	rep := Report{Client: c.Name(), Batch: b.Name, Started: r.now()}
	settle := b.Settle
	if settle <= 0 {
		settle = DefaultSettle
	}
	cp, ok := core.AsCheckinProvider(c)
	if !ok {
		r.log("[scheduler] %s/%s: client is not a CheckinProvider", rep.Client, rep.Batch)
		rep.Duration = r.now().Sub(rep.Started)
		r.record(rep, trigger)
		return rep
	}
	accounts, err := accountsOf(ctx, c)
	if err != nil {
		rep.Failed++
		rep.Errors = append(rep.Errors, err.Error())
		r.log("[scheduler] %s/%s: listing accounts failed, using the default account: %v", rep.Client, rep.Batch, err)
	}
	rep.Accounts = len(accounts)
	gap := b.AccountGap
	if gap <= 0 {
		gap = DefaultAccountGap
	}
	ranAnAccount := false
	for _, acc := range accounts {
		if ctx.Err() != nil {
			break
		}
		if ranAnAccount {
			if !r.sleep(ctx, jitter(gap)) {
				break
			}
		}
		ranAnAccount = true
		res, cerr := cp.Checkin(ctx, acc, "")
		rep.Ran++
		if cerr != nil {
			rep.Failed++
			rep.Errors = append(rep.Errors, cerr.Error())
			r.log("[scheduler] %s/%s account %q checkin failed: %v", rep.Client, rep.Batch, acc, cerr)
			continue
		}
		if !res.OK {
			rep.Refused++
			r.log("[scheduler] %s/%s account %q checkin refused: %s", rep.Client, rep.Batch, acc, res.Message)
		}
	}
	rep.Duration = r.now().Sub(rep.Started)
	r.sleep(ctx, settle)
	r.record(rep, trigger)
	r.log("[scheduler] %s/%s done: accounts=%d ran=%d refused=%d failed=%d in %s",
		rep.Client, rep.Batch, rep.Accounts, rep.Ran, rep.Refused, rep.Failed, rep.Duration)
	return rep
}
