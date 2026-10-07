package panel

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Scheduled-batch surface
//
//	GET  <base>/batches                     what this module can run in one go
//	POST <base>/batches/<name>/run          start it for every enabled account
//	GET  <base>/batches/runs/<id>           poll it
//	GET  <base>/batches/queue               what is waiting
//	POST <base>/batches/queue/run           drain the queue now
//
//	POST <base>/<group>_all                 the reference's one-click verbs
//
// The reference gateway exposed one "run it for everything" button per daily
// chore: checkin_all, travel_all, activity_all, keepalive_all, balance_all.
// Those are not five features, they are one feature with five names, so this
// file implements the shape once and lets each module *declare* what its
// batches contain (core.BatchPlanner).  The panel therefore never learns a
// vendor's task codes, and a module that declares no batches simply has no
// buttons.
//
// Everything here is asynchronous and journalled, for exactly the reason the
// task board is (see taskboard.go): pacing is protocol.  The vendors roll back
// sub-second bursts, so a full sweep deliberately sleeps between accounts, and
// a synchronous POST would outlive any sane write deadline.  A dropped
// connection must not cancel a half-finished sweep either, so the run gets its
// own context rather than the request's.
// ---------------------------------------------------------------------------

const (
	// batchRunTTL is how long a finished sweep stays visible.
	batchRunTTL = 6 * time.Hour
	// batchRunMax bounds the journal when an operator hammers the buttons.
	batchRunMax = 100
	// batchRunTimeout is the wall clock one sweep may take.  Generous: N
	// accounts times M chores times the inter-account gap, and the gap alone
	// is tens of seconds.
	batchRunTimeout = 6 * time.Hour
	// batchStepTimeout bounds a single chore inside a sweep.
	batchStepTimeout = 30 * time.Minute
	// defaultAccountGap is used when a module declares no gap of its own.
	defaultAccountGap = 45 * time.Second
	// defaultSettle is the shortest pause worth taking after a sweep so a
	// re-read does not see stale upstream scoring.
	defaultSettle = 5 * time.Second
)

// batchRunStates.
const (
	batchQueued  = "queued"
	batchRunning = "running"
	batchDone    = "done"
)

// batchStep is one chore on one account.
type batchStep struct {
	Account   string `json:"account"`
	Code      string `json:"code"`
	OK        bool   `json:"ok"`
	Refused   bool   `json:"refused,omitempty"` // a legitimate "no" from the vendor
	Skipped   bool   `json:"skipped,omitempty"` // deliberately not run / not applicable
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
	Credit    int64  `json:"credit,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// batchRun is the journal entry for one sweep.
type batchRun struct {
	ID         string      `json:"id"`
	Client     string      `json:"client"`
	Batch      string      `json:"batch"`
	State      string      `json:"state"`
	StartedAt  string      `json:"started_at,omitempty"`
	FinishedAt string      `json:"finished_at,omitempty"`
	ElapsedMS  int64       `json:"elapsed_ms,omitempty"`
	QueuedAt   string      `json:"queued_at,omitempty"`
	Accounts   int         `json:"accounts"`
	Done       int         `json:"done"`
	Ran        int         `json:"ran"`
	Refused    int         `json:"refused"`
	Failed     int         `json:"failed"`
	Skipped    int         `json:"skipped"`
	Credit     int64       `json:"credit"`
	Steps      []batchStep `json:"steps"`
	Error      string      `json:"error,omitempty"`
}

// batchRuns is the sweep journal.  Deliberately lossy and in-memory: it exists
// so the UI can say "last sweep: 3 accounts, +900", never as a source of truth.
type batchRuns struct {
	mu   sync.Mutex
	byID map[string]*batchRun
	seq  uint64
}

func newBatchRuns() *batchRuns {
	return &batchRuns{byID: make(map[string]*batchRun, 8)}
}

func (s *batchRuns) add(run *batchRun) *batchRun {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-batchRunTTL)
	for id, r := range s.byID {
		if r.State != batchDone {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, r.FinishedAt); err == nil && t.Before(cutoff) {
			delete(s.byID, id)
		}
	}
	for len(s.byID) >= batchRunMax {
		oldestID, oldest := "", time.Time{}
		for id, r := range s.byID {
			if r.State != batchDone {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, r.FinishedAt)
			if err != nil {
				continue
			}
			if oldestID == "" || t.Before(oldest) {
				oldestID, oldest = id, t
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.byID, oldestID)
	}

	s.seq++
	run.ID = strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(s.seq, 36)
	s.byID[run.ID] = run
	return run
}

func (s *batchRuns) get(id string) (batchRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return batchRun{}, false
	}
	return *r, true
}

// mutate applies fn under the lock so a concurrent poll never sees a torn step
// list.  A missing id is ignored: racing the pruner must never panic a sweep.
func (s *batchRuns) mutate(id string, fn func(*batchRun)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.byID[id]; ok {
		fn(r)
	}
}

// forClient returns this client's sweeps newest-first.
func (s *batchRuns) forClient(client string, limit int) []batchRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]batchRun, 0, 8)
	for _, r := range s.byID {
		if r.Client == client {
			out = append(out, *r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].StartedAt, out[j].StartedAt
		if ti == "" {
			ti = out[i].QueuedAt
		}
		if tj == "" {
			tj = out[j].QueuedAt
		}
		return ti > tj
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// all returns every client's sweeps newest-first, capped at limit.  The
// tasks centre's run journal merges this with the scheduler's own history,
// so it needs the whole panel rather than one client.
func (s *batchRuns) all(limit int) []batchRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]batchRun, 0, len(s.byID))
	for _, r := range s.byID {
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].StartedAt, out[j].StartedAt
		if ti == "" {
			ti = out[i].QueuedAt
		}
		if tj == "" {
			tj = out[j].QueuedAt
		}
		return ti > tj
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---------------------------------------------------------------------------
// The queue
//
// Sweeps run one at a time per panel.  Two clicks on two buttons must not race
// each other into the same upstream, and the vendors' own pacing assumes a
// serial sweep; queueing instead of refusing also means an operator can press
// all six buttons and walk away.
// ---------------------------------------------------------------------------

type queuedSweep struct {
	runID  string
	client string
	batch  core.Batch
}

type batchQueue struct {
	mu      sync.Mutex
	items   []queuedSweep
	running bool
	runs    *batchRuns
	// run is the executor, injected so tests can drive the queue without a
	// registry or a vendor.
	run func(ctx context.Context, q queuedSweep)
	// report receives the trace of a panic that escaped run.  Without it the
	// worker would die silently -- the sweep would sit on "running" forever
	// and nothing would appear in the panel's log ring.
	report func(string)
}

func newBatchQueue(runs *batchRuns) *batchQueue {
	return &batchQueue{runs: runs}
}

// enqueue appends a sweep and starts the worker if it is idle.
func (q *batchQueue) enqueue(j queuedSweep) {
	q.mu.Lock()
	q.items = append(q.items, j)
	idle := !q.running
	if idle {
		q.running = true
	}
	q.mu.Unlock()
	if idle {
		// GoSafe, not a bare "go": the worker outlives the response that
		// enqueued it, so a panic here would otherwise take the gateway down.
		core.GoSafe("panel batch worker", q.report, q.worker)
	}
}

func (q *batchQueue) worker() {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		j := q.items[0]
		q.items = q.items[1:]
		run := q.run
		q.mu.Unlock()

		if run != nil {
			// Recover, not GoSafe: the sweep has to keep this worker's loop --
			// and therefore q.running's reset above -- alive.  A panic that
			// escaped here would leave the queue permanently busy and every
			// later sweep waiting for a worker that no longer exists.
			core.Recover("panel batch sweep", q.report, func() { run(context.Background(), j) })
		}
	}
}

// pending lists the sweeps still waiting, oldest first.
func (q *batchQueue) pending() []queuedSweep {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]queuedSweep, len(q.items))
	copy(out, q.items)
	return out
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// batchView is the wire form of one module-declared batch, enriched with what
// the operator needs to decide whether to press the button.
type batchView struct {
	Name         string   `json:"name"`
	Codes        []string `json:"codes"`
	Claim        []string `json:"claim,omitempty"`
	Gate         string   `json:"gate,omitempty"`
	AccountGapMS int64    `json:"account_gap_ms"`
	SettleMS     int64    `json:"settle_ms"`
	Accounts     int      `json:"accounts"`
	Enabled      int      `json:"enabled"`
	Last         any      `json:"last,omitempty"`
	Blocked      string   `json:"blocked,omitempty"`
}

func (p *panel) batchViews(c core.Client, am core.AccountManager) []batchView {
	var accounts []core.AccountRecord
	if am != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		accounts, _ = am.Accounts(ctx)
		cancel()
	}
	last := map[string]any{}
	for _, r := range p.sweeps.forClient(c.Name(), 0) {
		if _, seen := last[r.Batch]; seen {
			continue
		}
		if r.State == batchDone {
			last[r.Batch] = r
		}
	}
	out := make([]batchView, 0, 8)
	for _, b := range core.PlannedBatches(c) {
		if b.Name == "" {
			continue
		}
		gap := b.AccountGap
		if gap <= 0 {
			gap = defaultAccountGap
		}
		settle := b.Settle
		if settle <= 0 {
			settle = defaultSettle
		}
		enabled := 0
		for _, a := range accounts {
			if a.Enabled {
				enabled++
			}
		}
		v := batchView{
			Name:         b.Name,
			Codes:        b.Codes,
			Claim:        b.Claim,
			Gate:         b.Gate,
			AccountGapMS: gap.Milliseconds(),
			SettleMS:     settle.Milliseconds(),
			Accounts:     len(accounts),
			Enabled:      enabled,
		}
		if b.Gate != "" {
			v.Blocked = "gated on " + b.Gate
		}
		if l, ok := last[b.Name]; ok {
			v.Last = l
		}
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// execSweep is the queue worker's body: it resolves the client, runs the batch
// and journals the outcome.  It owns its own context rather than borrowing the
// request's, because a dropped connection must not cancel a half-finished
// sweep — the operator would have no way to tell what actually ran.
func (p *panel) execSweep(_ context.Context, j queuedSweep) {
	if p.opts.Registry == nil {
		p.failSweep(j.runID, "no registry")
		return
	}
	client, ok := p.opts.Registry.Get(j.client)
	if !ok {
		p.failSweep(j.runID, "no such client: "+j.client)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), batchRunTimeout)
	defer cancel()
	p.runBatch(ctx, j.runID, client, j.batch)
}

func (p *panel) failSweep(id, msg string) {
	p.sweeps.mutate(id, func(r *batchRun) {
		r.State = batchDone
		r.Error = msg
		r.FinishedAt = time.Now().Format(time.RFC3339Nano)
	})
}

// runBatch walks a batch once, for every enabled account, with the pacing the
// module asked for.  Progress is written to the journal as it happens so a poll
// shows which chore is busy, and it never returns early on a per-account or
// per-chore failure: the whole point of a sweep is that one dead account cannot
// cost the operator the other five.
func (p *panel) runBatch(ctx context.Context, id string, c core.Client, b core.Batch) {
	if core.IsCheckinBatch(c, b) {
		if cp, ok := core.AsCheckinProvider(c); ok {
			p.runCheckinBatch(ctx, id, c, b, cp)
			return
		}
	}
	p.sweeps.mutate(id, func(r *batchRun) {
		r.State = batchRunning
		r.StartedAt = time.Now().Format(time.RFC3339Nano)
	})

	finish := func(err string) {
		p.sweeps.mutate(id, func(r *batchRun) {
			if r.StartedAt != "" {
				if t, e := time.Parse(time.RFC3339Nano, r.StartedAt); e == nil {
					r.ElapsedMS = time.Since(t).Milliseconds()
				}
			}
			r.State = batchDone
			r.FinishedAt = time.Now().Format(time.RFC3339Nano)
			if err != "" {
				r.Error = err
			}
		})
	}

	tp, ok := core.AsTaskProvider(c)
	if !ok {
		finish(c.Name() + " does not run tasks")
		return
	}
	var checkinActions []core.CheckinAction
	if cp, ok := core.AsCheckinProvider(c); ok {
		checkinActions = cp.CheckinActions(ctx)
	}
	am, ok := core.AsAccountManager(c)
	if !ok {
		finish(c.Name() + " does not manage accounts")
		return
	}

	accounts, err := am.Accounts(ctx)
	if err != nil {
		finish(core.Redact(err.Error()))
		return
	}

	codes := make([]string, 0, len(b.Codes)+len(b.Claim))
	codes = append(codes, b.Codes...)
	codes = append(codes, b.Claim...)

	gap := b.AccountGap
	if gap <= 0 {
		gap = defaultAccountGap
	}

	first := true
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		if ctx.Err() != nil {
			break
		}

		// Pacing is protocol: the vendors roll back sub-second bursts, so the
		// gap between accounts is not politeness.  A randomised gap keeps a
		// sweep from looking like a metronome.
		if !first {
			if !skipSleep(ctx, core.JitterDur(gap)) {
				break
			}
		}
		first = false

		// Progress, not a total: this counts accounts the sweep has actually
		// started, so it climbs by one every gap.  batchView.Accounts is the
		// different number -- how many accounts the module lists at all -- and
		// the two share the "accounts" key in their respective JSON documents.
		// A live zcode run read accounts:1 against /batches' accounts:3 for
		// exactly this reason, and only one of the three had been reached.
		p.sweeps.mutate(id, func(r *batchRun) { r.Accounts++ })

		if !core.CheckinActionAllowsAccount(checkinActions, "", a) {
			p.sweeps.mutate(id, func(r *batchRun) {
				r.Skipped++
				r.Steps = append(r.Steps, batchStep{
					Account: a.ID,
					Code:    core.CheckinBatchName,
					Skipped: true,
					Message: "action is not scoped to this account channel",
				})
			})
			p.sweeps.mutate(id, func(r *batchRun) { r.Done++ })
			continue
		}

		if gated(ctx, tp, a.ID, b.Gate) {
			p.sweeps.mutate(id, func(r *batchRun) {
				r.Skipped++
				r.Steps = append(r.Steps, batchStep{
					Account: a.ID,
					Code:    b.Gate,
					Skipped: true,
					Message: "gate not satisfied",
				})
				r.Done++
			})
			continue
		}

		for _, code := range codes {
			if ctx.Err() != nil {
				break
			}
			if !core.CheckinActionAllowsAccount(checkinActions, code, a) {
				p.sweeps.mutate(id, func(r *batchRun) {
					r.Skipped++
					r.Steps = append(r.Steps, batchStep{
						Account: a.ID,
						Code:    code,
						Skipped: true,
						Message: "action is not scoped to this account channel",
					})
				})
				continue
			}
			step := p.runOne(ctx, tp, a.ID, code)
			p.sweeps.mutate(id, func(r *batchRun) {
				r.Steps = append(r.Steps, step)
				if step.Skipped {
					r.Skipped++
					return
				}
				r.Ran++
				// Refused is tested before Error on purpose.  runOne copies
				// TaskResult.Error into the step even when the module answered
				// OK=false, so a vendor refusal routinely carries BOTH fields --
				// and testing Error first counted every such refusal as a
				// failure while the step's own JSON said "refused": true.
				// Ordering them this way is safe because a genuine Go error
				// returns from runOne early with Refused still false, so
				// Failed keeps catching exactly what it used to.
				switch {
				case step.Refused:
					r.Refused++
				case step.Error != "":
					r.Failed++
				case step.OK:
					r.Credit += step.Credit
				}
			})
		}
		p.sweeps.mutate(id, func(r *batchRun) { r.Done++ })
	}

	// Settle before declaring victory: upstream scoring lags several seconds,
	// so the operator must not be told "done" and immediately re-read a board
	// that still shows yesterday's numbers.
	settle := b.Settle
	if settle <= 0 {
		settle = defaultSettle
	}
	skipSleep(ctx, settle)

	errMsg := ""
	if err := ctx.Err(); err != nil {
		errMsg = err.Error()
	}
	finish(errMsg)
}

// runOne executes a single chore and normalises the three possible outcomes.
// A vendor refusal is TaskResult{OK:false} and is NOT an error — that
// distinction is the whole reason the board can show "already claimed" without
// painting it red.
func (p *panel) runOne(ctx context.Context, tp core.TaskProvider, account, code string) batchStep {
	step := batchStep{Account: account, Code: code}
	started := time.Now()
	stepCtx, cancel := context.WithTimeout(ctx, batchStepTimeout)
	defer cancel()

	res, err := tp.RunTask(stepCtx, account, code)
	step.ElapsedMS = time.Since(started).Milliseconds()
	if err != nil {
		step.Error = core.Redact(err.Error())
		return step
	}
	step.OK = res.OK
	step.Skipped = res.Skipped
	step.Message = res.Message
	if res.Error != "" {
		step.Error = res.Error
		if !res.OK && !res.Skipped {
			step.Refused = true
		}
	}
	if !res.OK && !res.Skipped && res.Error == "" {
		step.Refused = true
	}
	step.Credit = res.Credit
	return step
}

// gated reports whether the batch's prerequisite chore is still outstanding.
// An empty gate always passes; a gate that the board does not list at all also
// passes, because a module that does not expose the chore cannot be waiting on
// it.
func gated(ctx context.Context, tp core.TaskProvider, account, gate string) bool {
	if gate == "" {
		return false
	}
	listCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	list, err := tp.Tasks(listCtx, account)
	if err != nil {
		return false
	}
	for _, t := range list {
		if t.Code != gate {
			continue
		}
		// Present and already finished: the gate is satisfied, so this
		// account's sweep may proceed.
		if t.Claimed {
			return false
		}
		if t.Target > 0 && t.Current >= t.Target && !t.Claimable {
			return false
		}
		return true
	}
	return false
}

func skipSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	return core.SleepCtx(ctx, d)
}

// allVerbs maps the reference's one-click endpoints onto batch names.
//
// balance_all is the odd one out and is handled before this table is consulted:
// a balance is not a chore any module plans, so "balance" resolves to no batch
// and the verb is served by the scheduler's own fleet-wide refresh instead.
// Keeping the entry here is still what makes the alias self-consistent, which
// is what TestBatchVerbsMatchTheAliasTable pins.
var allVerbs = map[string]string{
	"checkin_all":   "checkin",
	"travel_all":    "travel",
	"activity_all":  "activity",
	"keepalive_all": "keepalive",
	"blackcat_all":  "blackcat",
	"growth_all":    "growth",
	"balance_all":   "balance",
}

// balanceVerb is the batch name allVerbs resolves balance_all to.  No module
// declares such a batch, so the dispatcher intercepts it by name and sends it
// to the scheduler instead of to batchStart.
const balanceVerb = "balance"

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// batchList answers GET <base>/batches.
func (p *panel) batchList(w http.ResponseWriter, r *http.Request, c core.Client) {
	am, _ := core.AsAccountManager(c)
	views := p.batchViews(c, am)
	if views == nil {
		views = []batchView{}
	}
	waiting := make([]map[string]any, 0, 4)
	for _, j := range p.queue.pending() {
		waiting = append(waiting, map[string]any{
			"run_id": j.runID,
			"client": j.client,
			"batch":  j.batch.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client":  c.Name(),
		"batches": views,
		"queue":   waiting,
		"runs":    p.sweeps.forClient(c.Name(), 20),
	})
}

// batchStart answers POST <base>/batches/<name>/run and the *_all aliases.
//
// It journals the sweep as queued and returns 202 immediately: the sweep itself
// takes tens of seconds per account and must not be tied to this request.
func (p *panel) batchStart(w http.ResponseWriter, r *http.Request, c core.Client, name string) {
	b, ok := core.BatchOf(c, name)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no batch named "+name)
		return
	}
	if core.IsCheckinBatch(c, b) {
		if _, ok := core.AsCheckinProvider(c); !ok {
			writeErr(w, http.StatusNotImplemented, c.Name()+" does not check in")
			return
		}
	} else if _, ok := core.AsTaskProvider(c); !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" does not run tasks")
		return
	}
	run := p.sweeps.add(&batchRun{
		Client:   c.Name(),
		Batch:    b.Name,
		State:    batchQueued,
		QueuedAt: time.Now().Format(time.RFC3339Nano),
		Steps:    []batchStep{},
	})
	p.queue.enqueue(queuedSweep{runID: run.ID, client: c.Name(), batch: b})
	writeJSON(w, http.StatusAccepted, toJSONRun(*run))
}

// balanceAll answers the reference's POST /panel/api/balance_all.
//
// It is deliberately not a batch.  A batch is a chore a module runs per
// account, and a balance is not a chore: no module plans one, so routing this
// verb through batchStart could only ever answer 501 "has no batch named
// balance".  The reference's balanceAll called the scheduler's own fleet-wide
// refresh, and so does this port -- the client in the path is ignored because
// the refresh sweeps every module that can report a balance.
//
// 501 when no scheduler is wired, and 501 when one is wired but its host never
// gave it a balance hook: both mean "this build cannot do that", which is the
// same answer every other optional mechanism gives.
func (p *panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.opts.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	ctx, cancel := p.ctx(r, 2*time.Minute)
	defer cancel()
	if !p.opts.Scheduler.RunBalanceRefreshNow(ctx) {
		writeErr(w, http.StatusNotImplemented, "balance refresh is not wired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "batch": balanceVerb, "state": "refreshed"})
}

// batchRunStatus answers GET <base>/batches/runs/<id>.
func (p *panel) batchRunStatus(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	run, ok := p.sweeps.get(id)
	if !ok || run.Client != c.Name() {
		writeErr(w, http.StatusNotFound, "no such sweep: "+id)
		return
	}
	writeJSON(w, http.StatusOK, toJSONRun(run))
}

// batchQueueList answers GET <base>/batches/queue.
func (p *panel) batchQueueList(w http.ResponseWriter, r *http.Request, c core.Client) {
	out := make([]map[string]any, 0, 4)
	for _, j := range p.queue.pending() {
		if j.client != c.Name() {
			continue
		}
		out = append(out, map[string]any{
			"run_id": j.runID,
			"batch":  j.batch.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c.Name(), "queue": out})
}

// toJSONRun renders a sweep.  Steps are newest-last, which is the order the
// operator watched them happen in.
func toJSONRun(run batchRun) map[string]any {
	if run.Steps == nil {
		run.Steps = []batchStep{}
	}
	return map[string]any{
		"id":          run.ID,
		"client":      run.Client,
		"batch":       run.Batch,
		"state":       run.State,
		"queued_at":   run.QueuedAt,
		"started_at":  run.StartedAt,
		"finished_at": run.FinishedAt,
		"elapsed_ms":  run.ElapsedMS,
		"accounts":    run.Accounts,
		"done":        run.Done,
		"ran":         run.Ran,
		"refused":     run.Refused,
		"failed":      run.Failed,
		"skipped":     run.Skipped,
		"credit":      run.Credit,
		"steps":       run.Steps,
		"error":       run.Error,
	}
}

// batchNameFromAllVerb resolves an *_all path segment.
func batchNameFromAllVerb(seg string) (string, bool) {
	name, ok := allVerbs[strings.ToLower(seg)]
	return name, ok
}

// allVerb reports whether a path segment is one of the reference's one-click
// batch verbs, so the dispatcher can route it without a per-client table.
func allVerb(seg string) bool {
	_, ok := allVerbs[strings.ToLower(seg)]
	return ok
}

// runCheckinBatch runs the synthetic check-in batch for every enabled account.
// It mirrors runBatch's pacing and journalling but calls the module's
// CheckinProvider instead of RunTask: the synthetic batch is the same chore as
// the account row's check-in button, only swept for the whole pool.
func (p *panel) runCheckinBatch(ctx context.Context, id string, c core.Client, b core.Batch, cp core.CheckinProvider) {
	p.sweeps.mutate(id, func(r *batchRun) {
		r.State = batchRunning
		r.StartedAt = time.Now().Format(time.RFC3339Nano)
	})

	finish := func(err string) {
		p.sweeps.mutate(id, func(r *batchRun) {
			if r.StartedAt != "" {
				if t, e := time.Parse(time.RFC3339Nano, r.StartedAt); e == nil {
					r.ElapsedMS = time.Since(t).Milliseconds()
				}
			}
			r.State = batchDone
			r.FinishedAt = time.Now().Format(time.RFC3339Nano)
			if err != "" {
				r.Error = err
			}
		})
	}

	am, ok := core.AsAccountManager(c)
	if !ok {
		finish(c.Name() + " does not manage accounts")
		return
	}
	accounts, err := am.Accounts(ctx)
	if err != nil {
		finish(core.Redact(err.Error()))
		return
	}
	checkinActions := cp.CheckinActions(ctx)

	gap := b.AccountGap
	if gap <= 0 {
		gap = defaultAccountGap
	}

	first := true
	for _, a := range accounts {
		if !a.Enabled {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if !first {
			if !skipSleep(ctx, core.JitterDur(gap)) {
				break
			}
		}
		first = false

		p.sweeps.mutate(id, func(r *batchRun) { r.Accounts++ })

		step := batchStep{Account: a.ID, Code: core.CheckinBatchName}
		if !core.CheckinActionAllowsAccount(checkinActions, "", a) {
			step.Skipped = true
			step.Message = "action is not scoped to this account channel"
			p.sweeps.mutate(id, func(r *batchRun) {
				r.Skipped++
				r.Steps = append(r.Steps, step)
				r.Done++
			})
			continue
		}
		started := time.Now()
		stepCtx, cancel := context.WithTimeout(ctx, batchStepTimeout)
		res, cerr := cp.Checkin(stepCtx, a.ID, "")
		cancel()
		step.ElapsedMS = time.Since(started).Milliseconds()
		if cerr != nil {
			step.Error = core.Redact(cerr.Error())
		} else {
			step.OK = res.OK
			step.Skipped = res.Skipped
			step.Message = res.Message
			if res.Error != "" {
				step.Error = res.Error
				if !res.OK && !res.Skipped {
					step.Refused = true
				}
			}
			if !res.OK && !res.Skipped && res.Error == "" {
				step.Refused = true
			}
		}
		p.sweeps.mutate(id, func(r *batchRun) {
			r.Steps = append(r.Steps, step)
			if step.Skipped {
				r.Skipped++
				return
			}
			r.Ran++
			switch {
			case step.Refused:
				r.Refused++
			case step.Error != "":
				r.Failed++
			}
		})
		p.sweeps.mutate(id, func(r *batchRun) { r.Done++ })
	}

	settle := b.Settle
	if settle <= 0 {
		settle = defaultSettle
	}
	skipSleep(ctx, settle)

	errMsg := ""
	if err := ctx.Err(); err != nil {
		errMsg = err.Error()
	}
	finish(errMsg)
}
