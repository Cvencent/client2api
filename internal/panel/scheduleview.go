package panel

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// ---------------------------------------------------------------------------
// The tasks centre's automation surface.
//
// GET /panel/api/schedule answers one payload for the whole 定时任务 box: the
// master switch and the shared timetable, one row per (platform, batch) a
// module actually plans, and the recent run journal.  It is one request on
// purpose -- the page asks "what is scheduled, where, and what happened last"
// and a per-platform fan-out would only make the answers disagree.
//
// Everything here is read-only.  Editing is a normal PATCH /panel/api/config
// followed by POST /panel/api/reload, exactly like the platform page, so there
// is one source of truth for the timetable and one hot-reload path.
// ---------------------------------------------------------------------------

// scheduleRow is one platform's private view of one batch.
type scheduleRow struct {
	Client string `json:"client"`
	Batch  string `json:"batch"`
	// Codes is what the batch will run, so the operator can tell "this
	// platform's checkin" from another platform's checkin.
	Codes []string `json:"codes,omitempty"`
	// Hours is the effective timetable in CST: the per-platform override
	// when there is one, otherwise the shared group.
	Hours []int `json:"hours"`
	// GroupEnabled is the timetable switch (not the account count) -- the
	// master switch gates the whole page and is reported separately.
	GroupEnabled bool `json:"group_enabled"`
	// Override says whether the hours above are this platform's own.
	Override bool `json:"override"`
	// Next is the next fire for this (platform, batch), RFC3339, or empty
	// when the group is off or the master switch is off.
	Next     string `json:"next,omitempty"`
	Accounts int    `json:"accounts"`
	Ready    int    `json:"ready"`
}

// scheduleRun is one line of the run journal, from either source: the
// scheduler's own history (scheduled fires and manual presses it served) or
// the panel's sweep journal (the one-click batch buttons).
type scheduleRun struct {
	At       string   `json:"at"`
	Client   string   `json:"client"`
	Batch    string   `json:"batch"`
	Trigger  string   `json:"trigger"`
	State    string   `json:"state,omitempty"`
	Accounts int      `json:"accounts"`
	Ran      int      `json:"ran"`
	Refused  int      `json:"refused"`
	Failed   int      `json:"failed"`
	Skipped  int      `json:"skipped"`
	Elapsed  int64    `json:"elapsed_ms"`
	Refusals []string `json:"refusals,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// runJournalMax bounds what the merged journal returns.  The page shows the
// newest page of it; a caller that wants everything can raise this.
const runJournalMax = 200

// handleSchedule answers GET /panel/api/schedule.
func (p *panel) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if p.opts.Scheduler == nil {
		// No scheduler: the timetable can still be edited and saved, but
		// nothing will fire it.  Say so with wired:false rather than 501,
		// because the page has something to show either way.
		writeJSON(w, http.StatusOK, map[string]any{
			"wired": false,
			"rows":  []scheduleRow{},
			"runs":  []scheduleRun{},
		})
		return
	}

	cfg := p.opts.Scheduler.Config()
	st := p.opts.Scheduler.Status()

	rows := p.scheduleRows(cfg, st)
	groups := make([]map[string]any, 0, 8)
	for _, g := range cfg.Groups() {
		groups = append(groups, map[string]any{
			"name":    g.Name,
			"enabled": g.Group.Enabled,
			"hours":   intsOrEmpty(g.Group.Hours),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"wired":                   true,
		"enabled":                 st.Enabled,
		"balance_refresh_enabled": cfg.BalanceRefresh.Enabled,
		"balance_refresh_minutes": int(cfg.BalanceRefresh.Every / time.Minute),
		"groups":                  groups,
		"rows":                    rows,
		"runs":                    p.mergedRuns(),
	})
}

// scheduleRows walks every registered module and reports each batch it plans,
// with the timetable that actually applies to it.
func (p *panel) scheduleRows(cfg scheduler.Config, st scheduler.Status) []scheduleRow {
	out := make([]scheduleRow, 0, 16)
	if p.opts.Registry == nil {
		return out
	}
	for _, c := range p.opts.Registry.All() {
		batches := core.PlannedBatches(c)
		if len(batches) == 0 {
			continue
		}
		// One account listing per client: every batch of the same client
		// runs over the same accounts.
		total, ready := 0, 0
		if am, ok := core.AsAccountManager(c); ok {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if recs, err := am.Accounts(ctx); err == nil {
				total = len(recs)
				for _, rec := range recs {
					if rec.Enabled {
						ready++
					}
				}
			}
			cancel()
		}
		for _, b := range batches {
			if b.Name == "" {
				continue
			}
			g, _ := cfg.GroupFor(c.Name(), b.Name)
			_, override := cfg.Override(c.Name(), b.Name)
			row := scheduleRow{
				Client:       c.Name(),
				Batch:        b.Name,
				Codes:        b.Codes,
				Hours:        intsOrEmpty(g.Hours),
				GroupEnabled: g.Enabled,
				Override:     override,
				Accounts:     total,
				Ready:        ready,
			}
			if at, ok := st.NextByClient[c.Name()+"/"+b.Name]; ok && !at.IsZero() {
				row.Next = at.Format(time.RFC3339)
			}
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Client != out[j].Client {
			return out[i].Client < out[j].Client
		}
		return out[i].Batch < out[j].Batch
	})
	return out
}

// mergedRuns merges the scheduler's journal with the panel's own sweep
// journal, newest first.  The two overlap only in spirit: a scheduled fire is
// the scheduler's, a one-click batch button is the panel's, and both belong on
// the same "what has run" table.
func (p *panel) mergedRuns() []scheduleRun {
	var out []scheduleRun
	if p.opts.Scheduler != nil {
		for _, rec := range p.opts.Scheduler.History() {
			out = append(out, scheduleRun{
				At:       rec.Started.Format(time.RFC3339),
				Client:   rec.Client,
				Batch:    rec.Batch,
				Trigger:  rec.Trigger,
				State:    runOutcome(rec.Ran, rec.Refused, rec.Skipped, rec.Failed, rec.Error),
				Accounts: rec.Accounts,
				Ran:      rec.Ran,
				Refused:  rec.Refused,
				Skipped:  rec.Skipped,
				Failed:   rec.Failed,
				Refusals: append([]string(nil), rec.Refusals...),
				Elapsed:  rec.Duration.Milliseconds(),
				Error:    rec.Error,
			})
		}
	}
	var sweeps []batchRun
	if p.sweeps != nil {
		sweeps = p.sweeps.all(runJournalMax)
	}
	for _, run := range sweeps {
		at := run.StartedAt
		if at == "" {
			at = run.QueuedAt
		}
		out = append(out, scheduleRun{
			At:       at,
			Client:   run.Client,
			Batch:    run.Batch,
			Trigger:  scheduler.TriggerManual,
			State:    run.State,
			Accounts: run.Accounts,
			Ran:      run.Ran,
			Refused:  run.Refused,
			Skipped:  run.Skipped,
			Failed:   run.Failed,
			Refusals: runRefusalReasons(run),
			Error:    run.Error,
			Elapsed:  run.ElapsedMS,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	if len(out) > runJournalMax {
		out = out[:runJournalMax]
	}
	if out == nil {
		out = []scheduleRun{}
	}
	return out
}

// runRefusalReasons extracts the operator-facing reasons from a manual sweep.
func runRefusalReasons(run batchRun) []string {
	var out []string
	for _, step := range run.Steps {
		if step.Skipped || !step.Refused {
			continue
		}
		reason := core.Redact(strings.TrimSpace(firstNonEmpty(step.Message, step.Error)))
		if reason == "" {
			reason = "the vendor refused the action"
		}
		seen := false
		for _, got := range out {
			if got == reason {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, reason)
		}
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// shows.  Before this the scheduled path hard-coded "done" for every run, so
// runOutcome turns one finished run's counters into the state the task centre
// a sweep in which every account was refused was indistinguishable from a
// clean success -- and the UI rendered no status at all.
//
//   - failed  : at least one transport/Go error (or the journal's own error)
//   - partial : some work succeeded and some was refused/skipped/failed
//   - refused : every executed step was a legitimate vendor refusal
//   - skipped : every executed step was deliberately not applicable
//   - ok      : at least one step succeeded and nothing failed
//   - done    : the run finished without executing anything (nothing to do)
func runOutcome(ran, refused, skipped, failed int, errText string) string {
	success := ran - refused - skipped
	if success < 0 {
		success = 0
	}
	switch {
	case failed > 0 || strings.TrimSpace(errText) != "":
		if success > 0 {
			return "partial"
		}
		return "failed"
	case success > 0:
		if refused > 0 || skipped > 0 {
			return "partial"
		}
		return "ok"
	case refused > 0:
		return "refused"
	case skipped > 0:
		return "skipped"
	default:
		return "done"
	}
}

func intsOrEmpty(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}
