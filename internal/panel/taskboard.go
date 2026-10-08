package panel

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Task board surface
//
//	GET  <base>/tasks?account=<id>   list the vendor's chores
//	POST <base>/tasks/<code>/run     {"account":"…"} start one
//	GET  <base>/tasks/runs/<id>      poll one run
//
// A run is asynchronous on purpose.  The vendors' anti-abuse rules force
// multi-second (in one case 45s) gaps between the events that make up a single
// chore, and "run everything" walks a couple of dozen of them, so a synchronous
// POST would sit far past any sane write deadline: the operator would see a
// spinner with no evidence anything was happening, and a dropped connection
// would cancel a half-finished chore.  Starting a run and polling it keeps the
// HTTP surface honest and lets the board show which row is busy.
//
// The store lives in the panel process and is deliberately lossy: it holds a
// bounded, time-limited window purely so the UI can render "last run: …".  No
// module should ever depend on it.
// ---------------------------------------------------------------------------

const (
	// taskRunTTL is how long a finished run stays visible on the board.
	taskRunTTL = 6 * time.Hour
	// taskRunMaxStore bounds memory when an operator hammers the buttons.
	taskRunMaxStore = 400
	// taskRunTimeout is the wall clock a single chore may take.  Generous
	// because a few of them sleep between events by design; still finite so a
	// wedged upstream cannot pin a goroutine forever.
	taskRunTimeout = 45 * time.Minute
)

// taskRunStates.  Kept as plain strings so the JSON is readable on the wire.
const (
	taskRunRunning = "running"
	taskRunDone    = "done"
)

type taskRun struct {
	ID         string          `json:"id"`
	Client     string          `json:"client"`
	Account    string          `json:"account,omitempty"`
	Code       string          `json:"code"`
	State      string          `json:"state"`
	StartedAt  string          `json:"started_at"`
	FinishedAt string          `json:"finished_at,omitempty"`
	ElapsedMS  int64           `json:"elapsed_ms,omitempty"`
	Result     core.TaskResult `json:"result"`
}

// taskRuns is the panel's in-memory run journal.  Every method takes the lock;
// nothing here does I/O, so holding it across a chore is never a risk.
type taskRuns struct {
	mu   sync.Mutex
	byID map[string]*taskRun
	seq  uint64
}

func newTaskRuns() *taskRuns {
	return &taskRuns{byID: make(map[string]*taskRun, 16)}
}

// start records a new run and returns its id.  It prunes in the same critical
// section so a long-lived panel cannot grow without bound.
func (s *taskRuns) start(client, account, code string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-taskRunTTL)
	for id, run := range s.byID {
		if run.State != taskRunDone {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, run.FinishedAt); err == nil && t.Before(cutoff) {
			delete(s.byID, id)
		}
	}
	// Hard cap: drop finished runs oldest-first if we are somehow over.
	if len(s.byID) >= taskRunMaxStore {
		oldestID, oldest := "", time.Time{}
		for id, run := range s.byID {
			if run.State != taskRunDone {
				continue
			}
			t, err := time.Parse(time.RFC3339Nano, run.FinishedAt)
			if err != nil {
				continue
			}
			if oldestID == "" || t.Before(oldest) {
				oldestID, oldest = id, t
			}
		}
		if oldestID != "" {
			delete(s.byID, oldestID)
		}
	}

	s.seq++
	id := strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(s.seq, 36)
	s.byID[id] = &taskRun{
		ID:        id,
		Client:    client,
		Account:   account,
		Code:      code,
		State:     taskRunRunning,
		StartedAt: time.Now().Format(time.RFC3339Nano),
	}
	return id
}

// finish closes out a run.  A missing id is ignored: the store is a journal,
// not a source of truth, so a race with pruning must never panic a chore.
func (s *taskRuns) finish(id string, res core.TaskResult, elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.byID[id]
	if !ok {
		return
	}
	run.State = taskRunDone
	run.FinishedAt = time.Now().Format(time.RFC3339Nano)
	run.ElapsedMS = elapsed.Milliseconds()
	run.Result = res
}

func (s *taskRuns) get(id string) (taskRun, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.byID[id]
	if !ok {
		return taskRun{}, false
	}
	return *run, true
}

// forClient returns this client's runs newest-first, capped at limit.
func (s *taskRuns) forClient(client string, limit int) []taskRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]taskRun, 0, 16)
	for _, run := range s.byID {
		if run.Client == client {
			out = append(out, *run)
		}
	}
	// Newest first.  Sorting by id is not safe (it is base36 of a nanosecond
	// stamp plus a counter, so it is ordered in practice) — sort on the
	// recorded start time, which is what the operator actually sees.
	sortByStartDesc(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// forClientAccount returns one account's runs newest-first. An empty account
// keeps the platform-wide view for callers that did not choose an account.
func (s *taskRuns) forClientAccount(client, account string, limit int) []taskRun {
	runs := s.forClient(client, 0)
	if account != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if run.Account == account {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	if limit > 0 && len(runs) > limit {
		runs = runs[:limit]
	}
	return runs
}

func sortByStartDesc(runs []taskRun) {
	// Insertion sort: the slice is tiny and almost always already ordered.
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].StartedAt > runs[j-1].StartedAt; j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// taskList answers GET <base>/tasks.
//
// The board needs three things in one round trip: the chores, the capability
// (so the UI knows whether to draw buttons at all) and the recent runs (so a
// row can say "last run: ok · +100").  Folding them together keeps the poll
// cheap — this endpoint is hit on a timer while a run is in flight.
func (p *panel) taskList(w http.ResponseWriter, r *http.Request, c core.Client) {
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	accountID := r.URL.Query().Get("account")
	if r.URL.Query().Get("all") == "1" {
		p.taskListAll(w, r, c, tp)
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()

	list, err := tp.Tasks(ctx, accountID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"client":  c.Name(),
		"account": accountID,
		"tasks":   redactTasks(list),
		"runs":    p.runs.forClientAccount(c.Name(), accountID, 50),
	})
}

type taskAccountRow struct {
	Account string          `json:"account"`
	Label   string          `json:"label,omitempty"`
	State   string          `json:"state,omitempty"`
	Tasks   []core.TaskInfo `json:"tasks"`
	Runs    []taskRun       `json:"runs"`
	Error   string          `json:"error,omitempty"`
}

// taskListAll answers GET <base>/tasks?all=1. It reads each enabled account
// independently so the board can say exactly which account is done and which
// one still has work, rather than blending their task state together.
func (p *panel) taskListAll(w http.ResponseWriter, r *http.Request, c core.Client, tp core.TaskProvider) {
	accounts := []core.AccountRecord{{}}
	if am, ok := core.AsAccountManager(c); ok {
		ctx, cancel := p.ctx(r, taskScanTimeout)
		defer cancel()
		list, err := am.Accounts(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		accounts = accounts[:0]
		for _, account := range list {
			if account.Enabled {
				accounts = append(accounts, account)
			}
		}
	}

	ctx, cancel := p.ctx(r, taskScanTimeout)
	defer cancel()
	rows := make([]taskAccountRow, len(accounts))
	sem := make(chan struct{}, taskScanConc)
	var wg sync.WaitGroup
	for i, account := range accounts {
		wg.Add(1)
		p.safeGo("panel task board account scan", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = taskAccountRow{
				Account: account.ID,
				Label:   core.Redact(account.Label),
				State:   account.State,
				Tasks:   []core.TaskInfo{},
				Runs:    p.runs.forClientAccount(c.Name(), account.ID, 50),
			}
			list, err := tp.Tasks(ctx, account.ID)
			if err != nil {
				rows[i].Error = core.Redact(err.Error())
				return
			}
			rows[i].Tasks = redactTasks(list)
		})
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"client": c.Name(), "accounts": rows})
}

// taskRun answers POST <base>/tasks/<code>/run.  It starts the chore and
// returns immediately with the run id the caller polls.
func (p *panel) taskRun(w http.ResponseWriter, r *http.Request, c core.Client, code string) {
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	if code == "" {
		writeErr(w, http.StatusBadRequest, "missing task code")
		return
	}
	var body struct {
		Account string `json:"account"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	id := p.runs.start(c.Name(), body.Account, code)

	// Deliberately NOT derived from the request: the response is written long
	// before the chore finishes, and r.Context() dies with it — a client that
	// navigated away would otherwise cancel a run it asked for.
	//
	// GoSafe, not a bare "go": a panic in a module's RunTask would otherwise
	// kill the gateway.  Recovered, the run would stay "running" on the board
	// forever, so the report closes it with the trace.
	started := time.Now()
	p.safeGoThen("panel task run", func(msg string) {
		p.runs.finish(id, core.TaskResult{
			Code:      code,
			AccountID: body.Account,
			OK:        false,
			Error:     core.Redact(msg),
			At:        time.Now().Format(time.RFC3339),
		}, time.Since(started))
	}, func() {
		ctx, cancel := context.WithTimeout(context.Background(), taskRunTimeout)
		defer cancel()
		res, err := tp.RunTask(ctx, body.Account, code)
		if err != nil {
			res = core.TaskResult{
				Code:      code,
				AccountID: body.Account,
				OK:        false,
				Error:     core.Redact(err.Error()),
				At:        time.Now().Format(time.RFC3339),
			}
		}
		res.Code = firstNonEmpty(res.Code, code)
		res.AccountID = firstNonEmpty(res.AccountID, body.Account)
		if res.At == "" {
			res.At = time.Now().Format(time.RFC3339)
		}
		res.ElapsedMS = time.Since(started).Milliseconds()
		if err == nil && res.OK {
			p.refreshBalanceAfterTask(c, res.AccountID)
		}
		p.runs.finish(id, res, time.Since(started))
	})

	writeJSON(w, http.StatusAccepted, map[string]any{
		"client": c.Name(),
		"run":    taskRun{ID: id, Client: c.Name(), Account: body.Account, Code: code, State: taskRunRunning, StartedAt: time.Now().Format(time.RFC3339Nano)},
	})
}

// taskRunStatus answers GET <base>/tasks/runs/<id>.
func (p *panel) taskRunStatus(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	run, ok := p.runs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such run: "+id)
		return
	}
	// A run id carries no client identity, but the route does; refusing a
	// mismatch keeps a stale id from one client from looking like another's.
	if run.Client != c.Name() {
		writeErr(w, http.StatusNotFound, "no such run for "+c.Name()+": "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c.Name(), "run": run})
}

// redactTasks masks anything secret-looking that upstream put in a title or
// note.  The panel never prints an upstream string without this pass.
func redactTasks(list []core.TaskInfo) []core.TaskInfo {
	out := make([]core.TaskInfo, 0, len(list))
	for _, t := range list {
		t.Title = core.Redact(t.Title)
		t.Desc = core.Redact(t.Desc)
		t.Note = core.Redact(t.Note)
		t.Group = core.Redact(t.Group)
		out = append(out, t)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
