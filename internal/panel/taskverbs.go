package panel

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Per-account task verbs
//
// The reference task board has one button per vendor verb: accept this chore,
// accept everything outstanding, claim this reward, automate this chore,
// automate everything.  They are separate upstream calls with separate failure
// modes -- a claim can be retried on its own once a chore has passed -- so they
// stay separate here: one optional core interface each, and a module that
// cannot serve one answers 501.  The frontend then hides exactly that button
// instead of offering a call that cannot possibly work.
//
// The routes mirror the reference's paths, which nest the verbs under the
// account they act on:
//
//	GET  <base>/accounts/<uid>/tasks             the board for one account
//	POST <base>/accounts/<uid>/tasks/accept      {"task_codes": [...]}
//	POST <base>/accounts/<uid>/tasks/accept_all  (no body)
//	POST <base>/accounts/<uid>/tasks/claim       {"task_code": "..."}
//	POST <base>/accounts/<uid>/tasks/auto        {"task_code": "..."}
//	POST <base>/accounts/<uid>/tasks/auto_all    (no body)
// ---------------------------------------------------------------------------

const (
	// taskVerbTimeout bounds one per-task verb.  A single chore runs in the
	// vendor's own time; this is only a ceiling, so a wedged connection cannot
	// hold a panel worker forever.
	taskVerbTimeout = 60 * time.Second
	// autoAllTimeout mirrors the reference: five minutes of wall clock.  Past
	// it the panel answers 504 while the round keeps running in the background,
	// which is exactly what the message it prints promises.
	autoAllTimeout = 5 * time.Minute
)

// autoAllGap is the reference's reportGap between chores.  Vendors score
// bursts, and an automation round is precisely the shape they score.  It is a
// variable only so a test can shrink a round to milliseconds; nothing in the
// product changes it.
var autoAllGap = 1050 * time.Millisecond

// Wording the reference fixes and operators have memorised.  Kept verbatim in
// the cases where the outcome is the panel's own judgement (busy, nothing left
// to accept, a chore no interface can drive); everything else is the module's
// own message, passed through.
const (
	msgTaskBusy            = "该账号有任务动作正在执行中，请等本轮结束后再试"
	msgTaskAutoNoInterface = "该任务需要客户端内交互（无对应接口），无法自动完成；请按任务说明在官方客户端操作"
	msgTaskNoSuch          = "该账号没有此任务"
	msgAllAccepted         = "所有任务均已接受"
	msgAcceptPartial       = "部分任务接受失败（上游拒绝），可重试"
	msgAlreadyClaimed      = "该奖励此前已领取"
	msgAutoAllTimeout      = "执行超时（任务仍在后台继续）"
	// bulkAcceptRow labels the synthetic row the accept phase contributes to
	// an auto_all report, so the caller can see the phase that is not a chore.
	bulkAcceptRow = "(批量接受)"
)

// accountLocks serialises the task verbs per account.  The reference keeps this
// lock in its pool; here it lives with the panel, because the panel is what
// knows which account a button was pressed for.  Keyed by client as well as
// account: two vendors may well use the same uid.
type accountLocks struct {
	mu   sync.Mutex
	held map[string]bool
}

func newAccountLocks() *accountLocks { return &accountLocks{} }

// try takes the lock, reporting false when someone else already holds it.  A
// nil table allows everything: a panel built without this field (a test
// literal, or a future constructor that forgets it) must not panic, and
// "no lock" is exactly the behaviour such a panel had before.
func (l *accountLocks) try(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[string]bool{}
	}
	if l.held[key] {
		return false
	}
	l.held[key] = true
	return true
}

// release lets the next caller in.  Releasing a lock nobody holds is a no-op,
// so the timeout path racing its own worker cannot unlock someone else's turn.
func (l *accountLocks) release(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, key)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// accountTasks answers GET <base>/accounts/<uid>/tasks: one account's board,
// the view every verb above reports back against.
func (p *panel) accountTasks(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ctx, cancel := p.ctx(r, taskScanTimeout)
	defer cancel()
	if !p.accountExists(ctx, c, id) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	list, err := tp.Tasks(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"client":  c.Name(),
		"account": id,
		"tasks":   redactTasks(list),
	})
}

// taskAccept answers POST <base>/accounts/<uid>/tasks/accept.
func (p *panel) taskAccept(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ac, ok := core.AsTaskAccepter(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot accept tasks")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Codes []string `json:"task_codes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if len(body.Codes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}
	ctx, cancel := p.ctx(r, taskVerbTimeout)
	defer cancel()
	if !p.accountExists(ctx, c, id) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	if err := ac.AcceptTasks(ctx, id, body.Codes); err != nil {
		writeErr(w, http.StatusBadGateway, "accept: "+core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// taskAcceptAll answers POST <base>/accounts/<uid>/tasks/accept_all.
func (p *panel) taskAcceptAll(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ba, ok := core.AsBulkTaskAccepter(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot accept tasks")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, taskVerbTimeout)
	defer cancel()
	if !p.accountExists(ctx, c, id) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	res, err := ba.AcceptAllTasks(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "accept all: "+core.Redact(err.Error()))
		return
	}
	failed := res.Failed
	if failed == nil {
		// Always an array: the frontend reads .length, and null would make it
		// special-case the happy path.
		failed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accepted": res.Accepted,
		"failed":   failed,
		"message":  firstNonEmpty(res.Message, acceptSummary(res)),
	})
}

// taskClaim answers POST <base>/accounts/<uid>/tasks/claim.
func (p *panel) taskClaim(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	cl, ok := core.AsTaskClaimer(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot claim task rewards")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Code string `json:"task_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	ctx, cancel := p.ctx(r, taskVerbTimeout)
	defer cancel()
	if !p.accountExists(ctx, c, id) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	res, err := cl.ClaimTask(ctx, id, body.Code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+core.Redact(err.Error()))
		return
	}
	// A reward of zero credit and zero energy is the vendor's way of saying
	// nothing was waiting, which is the same outcome as an explicit
	// already-claimed -- and it is not an error the operator should see.
	if res.AlreadyClaimed || (res.Credit == 0 && res.Energy == 0) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":              true,
			"already_claimed": true,
			"message":         firstNonEmpty(res.Message, msgAlreadyClaimed),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"credit":  res.Credit,
		"energy":  res.Energy,
		"message": res.Message,
	})
}

// taskAuto answers POST <base>/accounts/<uid>/tasks/auto.
func (p *panel) taskAuto(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ar, ok := core.AsTaskAutoRunner(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot automate tasks")
		return
	}
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var body struct {
		Code string `json:"task_code"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Code == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	ctx, cancel := p.ctx(r, taskVerbTimeout)
	defer cancel()
	if !p.accountExists(ctx, c, id) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	key := c.Name() + "/" + id
	if !p.taskLocks.try(key) {
		writeErr(w, http.StatusConflict, msgTaskBusy)
		return
	}
	defer p.taskLocks.release(key)

	// The board is consulted before running, for the same reason the reference
	// looks its chores up first: "no such chore" (404) and "this chore needs a
	// human" (501) are different answers, and only the module knows which.
	list, err := tp.Tasks(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+core.Redact(err.Error()))
		return
	}
	task := findTask(list, body.Code)
	if task == nil {
		writeErr(w, http.StatusNotFound, msgTaskNoSuch)
		return
	}
	if !task.Auto {
		writeErr(w, http.StatusNotImplemented, msgTaskAutoNoInterface)
		return
	}
	res, err := ar.RunTaskAuto(ctx, id, body.Code)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "执行失败: "+core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, autoVerbBody(body.Code, res))
}

// taskAutoAll answers POST <base>/accounts/<uid>/tasks/auto_all.
func (p *panel) taskAutoAll(w http.ResponseWriter, r *http.Request, c core.Client, id string) {
	ar, ok := core.AsTaskAutoRunner(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" cannot automate tasks")
		return
	}
	tp, ok := core.AsTaskProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no task board")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	exists := p.accountExists(ctx, c, id)
	cancel()
	if !exists {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	key := c.Name() + "/" + id
	if !p.taskLocks.try(key) {
		writeErr(w, http.StatusConflict, msgTaskBusy)
		return
	}

	// The round outlives the request.  A whole automation sweep takes vendor
	// time, so the budget is the panel's own and the context must not be the
	// request's -- otherwise the 504 below would be describing work that had
	// just been cancelled.
	runCtx, cancelRun := context.WithTimeout(context.WithoutCancel(r.Context()), autoAllTimeout)
	// GoSafe, not a bare "go": the round is minutes of vendor work, and a panic
	// in it would kill the gateway.  Recovered, the caller would otherwise wait
	// out the whole budget for a result that is never coming, so the report
	// answers on the same channel it is selecting on.
	done := make(chan []autoRow, 1)
	p.safeGoThen("panel auto_all round", func(msg string) {
		done <- []autoRow{{TaskCode: "panic", Status: "error", Message: core.Redact(msg)}}
	}, func() {
		defer cancelRun()
		defer p.taskLocks.release(key)
		done <- p.runAutoAll(runCtx, c, tp, ar, id)
	})

	select {
	case rows := <-done:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": rows})
	case <-runCtx.Done():
		// The round may have finished in the same instant the budget expired:
		// report the work rather than a timeout nobody can act on.
		select {
		case rows := <-done:
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": rows})
		default:
			writeErr(w, http.StatusGatewayTimeout, msgAutoAllTimeout)
		}
	}
}

// ---------------------------------------------------------------------------
// The auto_all round
// ---------------------------------------------------------------------------

// autoRow is one line of an auto_all report.  The shape follows the
// reference's results[] entries so an operator's habits keep working.
type autoRow struct {
	TaskCode      string `json:"task_code"`
	Desc          string `json:"desc,omitempty"`
	Status        string `json:"status"` // done | skipped | error
	Message       string `json:"message,omitempty"`
	ProgressAfter string `json:"progress_after,omitempty"`
	Claimable     bool   `json:"claimable,omitempty"`
	Claimed       bool   `json:"claimed,omitempty"`
	Credit        int64  `json:"credit,omitempty"`
	Energy        int64  `json:"energy,omitempty"`
	ClaimError    string `json:"claim_error,omitempty"`
}

// runAutoAll accepts everything outstanding, then automates every chore the
// module marked as automatable, reporting one row per step.  It never returns
// an error: a round is a report, and a failure is one line of it.
func (p *panel) runAutoAll(ctx context.Context, c core.Client, tp core.TaskProvider, ar core.TaskAutoRunner, id string) []autoRow {
	rows := make([]autoRow, 0, 8)

	// Accept first, exactly like the reference: a chore whose only missing
	// step is being taken on gets its chance in the same round.
	if ba, ok := core.AsBulkTaskAccepter(c); ok {
		row := autoRow{TaskCode: bulkAcceptRow}
		res, err := ba.AcceptAllTasks(ctx, id)
		if err != nil {
			row.Status = "error"
			row.Message = core.Redact(err.Error())
		} else {
			row.Status = "done"
			row.Message = firstNonEmpty(res.Message, acceptSummary(res))
		}
		rows = append(rows, row)
		if !p.autoPause(ctx) {
			return rows
		}
	}

	list, err := tp.Tasks(ctx, id)
	if err != nil {
		rows = append(rows, autoRow{
			TaskCode: "(列表)",
			Status:   "error",
			Message:  "list tasks: " + core.Redact(err.Error()),
		})
		return rows
	}
	for _, t := range list {
		if ctx.Err() != nil {
			return rows
		}
		if !t.Auto {
			// Not a failure, and not a row: the reference iterates only the
			// chores it has an action for, and one that needs a human cannot
			// be automated by anybody.
			continue
		}
		row := autoRow{TaskCode: t.Code, Desc: t.Title}
		if t.Claimed {
			row.Status = "skipped"
			row.Message = msgAlreadyClaimed
			rows = append(rows, row)
			if !p.autoPause(ctx) {
				return rows
			}
			continue
		}
		res, err := ar.RunTaskAuto(ctx, id, t.Code)
		if err != nil {
			row.Status = "error"
			row.Message = core.Redact(err.Error())
			rows = append(rows, row)
			if !p.autoPause(ctx) {
				return rows
			}
			continue
		}
		row.Status = "done"
		if res.Skipped {
			row.Status = "skipped"
		}
		row.Message = firstNonEmpty(res.Message, res.Error)
		if row.Status == "skipped" {
			row.Message = firstNonEmpty(res.Message, msgAlreadyClaimed)
		}
		row.ProgressAfter = res.ProgressAfter
		row.Claimable = res.Claimable
		row.Claimed = res.Claimed
		row.Credit = res.Credit
		row.Energy = res.Energy
		row.ClaimError = core.Redact(res.ClaimError)
		rows = append(rows, row)
		if !p.autoPause(ctx) {
			return rows
		}
	}
	return rows
}

// autoPause waits out one inter-chore gap, reporting false when the round's
// budget ran out first.  A cancelled round stops between chores rather than
// mid-call: the vendor has already been asked, and abandoning the answer would
// leave the operator with no idea which chores were attempted.
func (p *panel) autoPause(ctx context.Context) bool {
	t := time.NewTimer(autoAllGap)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// autoVerbBody renders one auto result in the reference's shape.  Every key the
// frontend reads is present when it applies, and verify_supported is always
// true: the panel did read the progress back, whatever it found.
func autoVerbBody(code string, res core.AutoTaskResult) map[string]any {
	out := map[string]any{
		"ok":               true,
		"task_code":        code,
		"verify_supported": true,
		"attempt":          res.Attempt,
		"claimable":        res.Claimable,
	}
	if res.ProgressBefore != "" {
		out["progress_before"] = res.ProgressBefore
	}
	if res.ProgressAfter != "" {
		out["progress_after"] = res.ProgressAfter
	}
	msg := firstNonEmpty(res.Message, res.Error)
	if res.Skipped {
		out["skipped"] = true
		msg = firstNonEmpty(res.Message, msgAlreadyClaimed)
	}
	if msg != "" {
		out["message"] = msg
	}
	if res.Claimed {
		out["claimed"] = true
		out["credit"] = res.Credit
		out["energy"] = res.Energy
	}
	if res.ClaimError != "" {
		out["claim_error"] = core.Redact(res.ClaimError)
	}
	return out
}

// findTask returns the chore with this code, or nil when the board has none.
func findTask(list []core.TaskInfo, code string) *core.TaskInfo {
	for i := range list {
		if list[i].Code == code {
			return &list[i]
		}
	}
	return nil
}

// acceptSummary words the accept-all outcome when the module supplied no
// message of its own.
func acceptSummary(res core.BulkAccept) string {
	switch {
	case len(res.Failed) > 0:
		return msgAcceptPartial
	case res.Accepted == 0:
		return msgAllAccepted
	default:
		return fmt.Sprintf("已接受 %d 个任务", res.Accepted)
	}
}
