package panel

import (
	"context"
	"net/http"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Fleet-wide chore queue
//
//	POST <base>/tasks/scan_all   what each account still has to do (read-only)
//	POST <base>/tasks/run_queue  enqueue those chores and drain them
//	GET  <base>/tasks/queue      what the queue is doing right now
//
// The single-chore route (POST <base>/tasks/<code>/run) is the operator doing
// one thing on purpose.  These three cover the other half of the job: a vendor
// spreads a couple of dozen chores across a pool of accounts, and nobody wants
// to press twenty buttons in the right order while watching for a rate limit.
//
// The module still owns every vendor quirk.  The queue only decides which
// declared code runs against which account next, and every item goes through
// the same TaskProvider.RunTask the single-chore route calls, so there is no
// second execution path to keep in step.
//
// Scope is per client, like the rest of the client-scoped routes.  A queue is
// a few dozen strings, so each module gets its own rather than sharing one
// global line where trae's backlog could sit in front of workbuddy's.
// ---------------------------------------------------------------------------

const (
	// taskScanTimeout bounds a whole scan.  A scan costs one upstream call per
	// account a few at a time, so this has to cover a pool, not a single call.
	taskScanTimeout = 2 * time.Minute
	// taskScanConc bounds how many accounts a scan asks at once.  Upstream
	// calls tolerate a handful at a time; a pool of twenty at once looks like
	// a scraper.
	taskScanConc = 4
	// taskQueueMinConc/taskQueueMaxConc clamp what the operator asks for.
	// Beyond four, per-account chores start colliding on the vendors' own
	// per-account locks, which turns a faster queue into a slower one.
	taskQueueMinConc = 1
	taskQueueMaxConc = 4
	// taskQueueMaxItems bounds one round.  A board is a couple of dozen chores
	// and a pool is a handful of accounts, so this is only reachable if a
	// module misreports; the cap keeps a bad scan from pinning memory.
	taskQueueMaxItems = 512
)

// Queue item states, kept as plain strings so the wire form stays readable.
const (
	queuePending = "pending"
	queueRunning = "running"
	queueDone    = "done"
	queueSkipped = "skipped"
	queueError   = "error"
)

// queueItem is one chore waiting to run, or the record of one that ran.
type queueItem struct {
	RunID   string `json:"run_id,omitempty"`
	Account string `json:"account"`
	Label   string `json:"label,omitempty"`
	Code    string `json:"code"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// taskQueue is the per-module queue.  Every method takes the lock and none of
// them does I/O, so a chore in flight never holds it.
type taskQueue struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []queueItem
	conc      int
	seq       int
}

func newTaskQueue() *taskQueue { return &taskQueue{} }

// taskQueues hands each module its own queue.
//
// A shared line would let one vendor's backlog sit in front of another's, which
// is precisely the coupling the per-module layout exists to avoid.  The queues
// are made on first use, so a module that never runs a chore never allocates
// one and the panel needs no registry of them up front.
type taskQueues struct {
	mu sync.Mutex
	m  map[string]*taskQueue
}

func newTaskQueues() *taskQueues { return &taskQueues{} }

// forClient returns the named module's queue, creating it on first use.
func (ts *taskQueues) forClient(client string) *taskQueue {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.m == nil {
		ts.m = make(map[string]*taskQueue)
	}
	q, ok := ts.m[client]
	if !ok {
		q = newTaskQueue()
		ts.m[client] = q
	}
	return q
}

// begin installs a fresh round, or reports that one is already draining.
//
// Refusing is the point: a second click must not double-run the same chores,
// and the caller is told so instead of being silently queued behind them.
func (q *taskQueue) begin(conc int, items []queueItem) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running {
		return false
	}
	q.running = true
	q.startedAt = time.Now()
	q.items = items
	q.conc = conc
	q.seq++
	return true
}

// next claims the first item still pending.  Exactly one worker can win a given
// item, which is what lets several drains share one list without a cursor.
func (q *taskQueue) next() (int, queueItem, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].Status == queuePending {
			q.items[i].Status = queueRunning
			return i, q.items[i], true
		}
	}
	return 0, queueItem{}, false
}

// markRun attaches the journal id of the run this item created.
func (q *taskQueue) markRun(idx int, runID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx >= 0 && idx < len(q.items) {
		q.items[idx].RunID = runID
	}
}

// settle closes an item out.
func (q *taskQueue) settle(idx int, status, message string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if idx >= 0 && idx < len(q.items) {
		q.items[idx].Status = status
		q.items[idx].Message = message
	}
}

func (q *taskQueue) finish() {
	q.mu.Lock()
	q.running = false
	q.mu.Unlock()
}

// isRunning is a cheap pre-check so a second click does not pay for a whole
// scan before being refused.  begin is what actually arbitrates the race.
func (q *taskQueue) isRunning() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

// concurrency reports the round's worker count.
func (q *taskQueue) concurrency() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conc
}

// snapshot is the payload the dashboard polls.  The counts are derived from the
// items on every call instead of kept alongside them, so they cannot drift.
func (q *taskQueue) snapshot(client string) map[string]any {
	q.mu.Lock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	running, startedAt, conc, seq := q.running, q.startedAt, q.conc, q.seq
	q.mu.Unlock()

	var pending, active, done, skipped, failed int
	for _, it := range items {
		switch it.Status {
		case queuePending:
			pending++
		case queueRunning:
			active++
		case queueDone:
			done++
		case queueSkipped:
			skipped++
		case queueError:
			failed++
		}
	}
	stamp := ""
	if !startedAt.IsZero() {
		stamp = startedAt.Format(time.RFC3339Nano)
	}
	return map[string]any{
		"client":     client,
		"running":    running,
		"started":    !startedAt.IsZero(),
		"started_at": stamp,
		"seq":        seq,
		"conc":       conc,
		"total":      len(items),
		"pending":    pending,
		"active":     active,
		"done":       done,
		"skipped":    skipped,
		"failed":     failed,
		"items":      items,
	}
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

// taskScanRow is one account in a scan: what it still has to do, or why the
// module could not say.
type taskScanRow struct {
	Account string          `json:"account"`
	Label   string          `json:"label,omitempty"`
	Pending []core.TaskInfo `json:"pending"`
	Error   string          `json:"error,omitempty"`
}

// taskPending is the queue's definition of "still to do": not claimed, not
// locked, and one the module says it can automate.
//
// An item past its target is still pending -- running it is what claims the
// reward -- but a locked one is not.  Vendors lock the next link of a
// sequential chore until it unlocks, and queuing it only produces an attempt
// that cannot land.
func taskPending(t core.TaskInfo) bool {
	return !t.Claimed && !t.Locked && t.Auto
}

// scanTasks asks the module what each account still has to do.  It is read-only
// and it is the only place the queue learns what to run.
//
// A module with no account manager is asked once with an empty account id --
// the interface's "whichever account you would use anyway" -- so a
// single-credential module stays usable without inventing an account for it.
func (p *panel) scanTasks(ctx context.Context, c core.Client, tp core.TaskProvider) []taskScanRow {
	accts := []core.AccountRecord{{}}
	if am, ok := core.AsAccountManager(c); ok {
		if list, err := am.Accounts(ctx); err == nil {
			accts = accts[:0]
			for _, a := range list {
				if !a.Enabled {
					// A parked credential is not a chore to run.  Scanning it
					// would have the queue wake an account an operator
					// deliberately put to sleep.
					continue
				}
				accts = append(accts, a)
			}
		}
	}

	rows := make([]taskScanRow, len(accts))
	sem := make(chan struct{}, taskScanConc)
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Add(1)
		p.safeGo("panel task scan", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = taskScanRow{Account: a.ID, Label: core.Redact(a.Label)}
			list, err := tp.Tasks(ctx, a.ID)
			if err != nil {
				rows[i].Error = core.Redact(err.Error())
				return
			}
			for _, t := range list {
				if taskPending(t) {
					rows[i].Pending = append(rows[i].Pending, t)
				}
			}
		})
	}
	wg.Wait()
	for i := range rows {
		rows[i].Pending = redactTasks(rows[i].Pending)
	}
	return rows
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// taskScanAll answers POST <base>/tasks/scan_all.  Read-only: it reports what
// every account still has to do and starts nothing, so an operator can look
// before committing to a round.
func (p *panel) taskScanAll(w http.ResponseWriter, r *http.Request, c core.Client) {
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, taskScanTimeout)
	defer cancel()

	rows := p.scanTasks(ctx, c, tp)
	pending := 0
	for _, row := range rows {
		pending += len(row.Pending)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"client":        c.Name(),
		"accounts":      rows,
		"pending_count": pending,
	})
}

// taskQueueStatus answers GET <base>/tasks/queue.  It is what the caller polls
// after starting a round.
func (p *panel) taskQueueStatus(w http.ResponseWriter, r *http.Request, c core.Client) {
	if _, ok := core.AsTaskProvider(c); !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	writeJSON(w, http.StatusOK, p.chores.forClient(c.Name()).snapshot(c.Name()))
}

// taskRunQueue answers POST <base>/tasks/run_queue.  It scans, queues every
// pending chore, and returns as soon as the workers are up: the caller polls
// GET <base>/tasks/queue instead of holding a connection open for a round that
// can take a long time.
func (p *panel) taskRunQueue(w http.ResponseWriter, r *http.Request, c core.Client) {
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	q := p.chores.forClient(c.Name())
	if q.isRunning() {
		writeErr(w, http.StatusConflict, "the task queue is already running")
		return
	}
	var body struct {
		Concurrency int `json:"concurrency"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	conc := body.Concurrency
	if conc < taskQueueMinConc {
		conc = taskQueueMinConc
	}
	if conc > taskQueueMaxConc {
		conc = taskQueueMaxConc
	}

	ctx, cancel := p.ctx(r, taskScanTimeout)
	rows := p.scanTasks(ctx, c, tp)
	cancel()

	items := make([]queueItem, 0, 16)
	for _, row := range rows {
		for _, t := range row.Pending {
			if len(items) >= taskQueueMaxItems {
				break
			}
			items = append(items, queueItem{
				Account: row.Account,
				Label:   row.Label,
				Code:    t.Code,
				Status:  queuePending,
			})
		}
	}

	// begin arbitrates: two clicks that both scanned cannot both start.
	if !q.begin(conc, items) {
		writeErr(w, http.StatusConflict, "the task queue is already running")
		return
	}
	if len(items) == 0 {
		q.finish()
		snap := q.snapshot(c.Name())
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"client":  c.Name(),
			"started": false,
			"message": "no pending chores",
			"queue":   snap,
		})
		return
	}

	// The workers below are already GoSafe'd; this outer goroutine covers the
	// round bookkeeping around them, which runs just as far past the response.
	p.safeGo("panel task queue drain", func() { p.drainTaskQueue(tp, c.Name()) })
	snap := q.snapshot(c.Name())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"client":  c.Name(),
		"started": true,
		"total":   len(items),
		"seq":     snap["seq"],
		"queue":   snap,
	})
}

// drainTaskQueue runs the current round.
//
// The context is built here rather than derived from the request: the response
// was written long before the work finishes, and a caller that navigated away
// must not cancel a round it asked for.
func (p *panel) drainTaskQueue(tp core.TaskProvider, client string) {
	q := p.chores.forClient(client)
	defer q.finish()

	workers := q.concurrency()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		// GoSafe, not a bare "go": this worker runs vendor code for minutes at a
		// time, well past the response that asked for the round.  A panic that
		// escaped here would kill the gateway; recovered, it costs this worker's
		// remaining items, and q.finish() still closes the round.
		p.safeGo("panel task queue worker", func() {
			defer wg.Done()
			for {
				idx, item, ok := q.next()
				if !ok {
					return
				}
				started := time.Now()
				runID := p.runs.start(client, item.Account, item.Code)
				q.markRun(idx, runID)

				ctx, cancel := context.WithTimeout(context.Background(), taskRunTimeout)
				res, err := tp.RunTask(ctx, item.Account, item.Code)
				cancel()

				if err != nil {
					res = core.TaskResult{
						Code:      item.Code,
						AccountID: item.Account,
						Error:     core.Redact(err.Error()),
						At:        time.Now().Format(time.RFC3339),
					}
				}
				res.Code = firstNonEmpty(res.Code, item.Code)
				res.AccountID = firstNonEmpty(res.AccountID, item.Account)
				if res.At == "" {
					res.At = time.Now().Format(time.RFC3339)
				}
				res.ElapsedMS = time.Since(started).Milliseconds()
				if err == nil && res.OK {
					p.refreshBalanceAfterTask(tp, res.AccountID)
				}
				p.runs.finish(runID, res, time.Since(started))

				// A vendor refusal is a RESULT, not a failure (see core.TaskResult):
				// "already claimed" and "prerequisite not met" both mean the chore
				// ran and did not land.  Only an attempt that could not be made at
				// all is an error on the board.
				status := queueDone
				switch {
				case res.Error != "":
					status = queueError
				case !res.OK:
					status = queueSkipped
				}
				q.settle(idx, status, core.Redact(firstNonEmpty(res.Message, res.Error)))
			}
		})
	}
	wg.Wait()
}
