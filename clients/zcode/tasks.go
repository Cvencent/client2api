package zcode

// Task board and scheduler wiring for ZCode.
//
// The module's only automatable chore is the promotional-plan claim that
// claim.go already implements, so both optional interfaces here are thin
// adapters over it rather than a second protocol:
//
//	core.TaskProvider   the panel's 任务看板 row (read the board, run the claim)
//	core.BatchPlanner   the scheduler's "checkin" slot
//
// The scheduler wants BOTH.  internal/scheduler/scheduler.go:415 plan() skips a
// client unless it implements TaskProvider *and* BatchPlanner, and a declared
// batch only fires when the config has an enabled group with that name, so
// implementing only one of the two would leave this client invisible on the
// timetable.  Before this file it was in exactly that state: the claim existed,
// but only as a button the operator had to press.
//
// claim.go:30-36 records why that was the original author's choice -- the
// reference ticks its claim every ten minutes, and an unattended loop that
// spends a browser-run captcha on that cadence is a decision for the operator,
// not a default.  The operator has since made it, and this file is the result.
// The cadence here is the scheduler's "checkin" group (09:00 and 21:00 in
// configs/client2api.json), not every ten minutes, and a run with no solver
// configured refuses in one line instead of retrying.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// claimBatchCheckin is the scheduler group the claim belongs to.  It is
	// the literal the scheduler's own unexported batchCheckin carries
	// (internal/scheduler/scheduler.go:78) and the key configs/client2api.json
	// uses under schedule, so the three have to agree; a mismatch is not a
	// compile error, it is a batch that silently never fires.
	claimBatchCheckin = "checkin"

	// claimBoardTimeout bounds the preview a board read performs.  Like
	// modelsFetchTimeout (models.go:51) it is deliberately far shorter than
	// Config.TimeoutSeconds (600s): this is a small billing read, and a board
	// that hangs for ten minutes is worse than one that reports a failure.
	claimBoardTimeout = 10 * time.Second

	// claimBoardTTL bounds how often the board re-reads that preview.  The
	// panel re-reads /tasks every 3s while a run is live (internal/panel/
	// index.html waitForTask), and preview is a billing endpoint, so a few
	// seconds of staleness buys a lot less traffic.  RunTask drops the cache,
	// so a claim is visible on the very next read.
	claimBoardTTL = 10 * time.Second
)

// Batches implements core.BatchPlanner.
//
// Static on purpose: the scheduler calls this on every tick and the panel may
// call it while rendering the schedule, so it must not touch the network or the
// account pool -- it only says what this client *could* run.  It declares the
// batch even when no captcha solver is configured, because the run then reports
// the missing solver as a refusal, which is how an operator finds out that this
// client has something worth claiming.
func (c *Client) Batches() []core.Batch {
	return []core.Batch{{
		Name:  claimBatchCheckin,
		Codes: []string{claimAction},
	}}
}

// claimBoard is the module's short-lived copy of one account's claim state.
// Every field is guarded by mu and the zero value is ready to use.
type claimBoard struct {
	mu            sync.Mutex
	plans         []claimPlan
	active        *planBalances
	activeChecked bool
	current       *planBalances
	account       string
	at            time.Time
}

// claimBoardSnapshot is the board's read-only view of the cached state.
type claimBoardSnapshot struct {
	plans         []claimPlan
	active        *planBalances
	activeChecked bool
	// current is the plan document read from billing/current.  It is what
	// names a grant the account already holds, which a preview never lists.
	current *planBalances
}

// boardFor returns the claimable plans and, when the preview is empty, the
// active balance document for acct.  It re-reads the vendor at most once per
// claimBoardTTL so the panel's frequent board polling does not turn into a
// billing burst.
func (c *Client) boardFor(ctx context.Context, acct *Account) (claimBoardSnapshot, error) {
	now := time.Now()
	c.board.mu.Lock()
	if c.board.account == acct.ID && now.Sub(c.board.at) < claimBoardTTL {
		snap := claimBoardSnapshot{
			plans:         append([]claimPlan(nil), c.board.plans...),
			active:        c.board.active,
			activeChecked: c.board.activeChecked,
			current:       c.board.current,
		}
		c.board.mu.Unlock()
		return snap, nil
	}
	c.board.mu.Unlock()

	plans, err := c.claimPreview(ctx, acct)
	if err != nil {
		// A failed read is never cached: the board has to be able to show the
		// vendor's refusal the moment it happens.
		return claimBoardSnapshot{}, err
	}
	snap := claimBoardSnapshot{plans: plans}
	if len(plans) == 0 {
		// An empty preview is also the state after a successful claim.  The
		// balance document is the only other read that can distinguish that
		// from "the vendor has no active plan", so make the distinction before
		// rendering the row.  A failed balance read is left unchecked; the
		// caller can say so without turning the whole board into an error.
		if active, balanceErr := c.planBalanceOf(ctx, acct); balanceErr == nil {
			snap.active = active
			snap.activeChecked = true
		}
		if !snap.activeChecked || !hasActivePlanBalance(snap.active) {
			// A daily grant reaches the account without a claim, so it can be
			// live while both the preview and the balance endpoint answer with
			// nothing the board knows how to render.  billing/current is the
			// endpoint that names it, and without it the board would keep
			// telling the operator there is nothing to claim while a grant is
			// funding the account.
			if current, currentErr := c.planCurrentOf(ctx, acct); currentErr == nil {
				snap.current = current
			}
		}
	}
	c.board.mu.Lock()
	c.board.plans = append([]claimPlan(nil), plans...)
	c.board.active = snap.active
	c.board.activeChecked = snap.activeChecked
	c.board.current = snap.current
	c.board.account = acct.ID
	c.board.at = now
	c.board.mu.Unlock()
	return snap, nil
}

// forgetBoard drops the cache so the next board read sees the claim that just
// landed instead of the preview that preceded it.
func (c *Client) forgetBoard() {
	c.board.mu.Lock()
	c.board.plans, c.board.active, c.board.activeChecked, c.board.current = nil, nil, false, nil
	c.board.account, c.board.at = "", time.Time{}
	c.board.mu.Unlock()
}

// planAccount resolves the account a board or run call means.
//
// An empty id means "whichever account you would use anyway" -- that is core's
// TaskProvider contract, and it is how the panel's task board calls this.  A
// non-empty id that does not exist is an error, because quietly showing an
// empty board for a typo would look like a vendor problem.
//
// A nil Account with a nil error means there is simply no account that can
// serve plan billing, which the caller renders as a board that says so.
func (c *Client) planAccount(id string) (*Account, error) {
	if c == nil || c.pool == nil {
		if strings.TrimSpace(id) != "" {
			return nil, fmt.Errorf("zcode: account %q not found", id)
		}
		return nil, nil
	}
	if strings.TrimSpace(id) != "" {
		acct := c.pool.find(id)
		if acct == nil {
			return nil, fmt.Errorf("zcode: account %q not found", id)
		}
		if resolved, err := c.claimChannelFor(acct); err == nil {
			return resolved, nil
		}
		// Keep the selected row's identity when it has no JWT sibling.  The
		// caller renders that as "this credential cannot claim" instead of
		// pretending the whole module has no account.
		return acct, nil
	}
	for _, a := range c.pool.planAccounts() {
		return a, nil
	}
	return nil, nil
}

// Tasks implements core.TaskProvider.  The board has one row: the promotional
// plan claim, described by whatever the vendor currently offers or by the
// active plan when the preview is empty because it was already claimed.
//
// The row is always returned, even when it cannot be run.  No JWT account, a
// missing captcha solver, a plan list that is empty -- all three are states the
// operator can act on, and a board that hid the row would leave them guessing
// which of the three it was.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	acct, err := c.planAccount(accountID)
	if err != nil {
		return nil, err
	}
	if acct == nil {
		return []core.TaskInfo{claimRow("没有可用的 ZCode 计划 (jwt) 账号；活动套餐只走 jwt 通道")}, nil
	}
	if acct.Mode != modeJWT {
		return []core.TaskInfo{claimRow(fmt.Sprintf(
			"plan billing needs a ZCode plan (jwt) credential; %s is %s",
			acct.ID, firstNonEmpty(acct.Mode, "unknown")))}, nil
	}
	if !c.pool.captchaReady() {
		// No local solver means the built-in browser is unavailable and
		// captcha_command is empty, so RunTask cannot mint a token.  A grant the
		// vendor already handed over needs no token, though, so report that
		// first and only then fall back to the missing-solver note.  The claim
		// itself is still possible from the accounts list, whose 领取 button
		// runs the vendor's SDK in the operator's browser (see captcha.go), so
		// the note points there and the row stays locked.
		if held, err := c.heldGrantRow(ctx, acct); err != nil || held != nil {
			return held, err
		}
		return []core.TaskInfo{claimRow(
			"这个看板的领取按钮没有浏览器可用；请在账号列表里点「领取」，那一步会用你自己的浏览器过验证码")}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, claimBoardTimeout)
	defer cancel()

	snap, err := c.boardFor(ctx, acct)
	if err != nil {
		return nil, err
	}
	plans := snap.plans
	if len(plans) == 0 {
		// A held grant outranks the balance view: it is the same fact the
		// Checkin leg now acts on, and it carries the expiry the operator
		// actually needs (a daily window closes on its own).
		if win, ok := activePlanWindow(snap.current, time.Now()); ok {
			return []core.TaskInfo{heldPlanRow(win)}, nil
		}
		if snap.activeChecked && hasActivePlanBalance(snap.active) {
			return []core.TaskInfo{activePlanRow(snap.active)}, nil
		}
		note := "厂商当前没有可领取的活动套餐"
		if !snap.activeChecked {
			note += "（已领取计划暂无法确认）"
		}
		return []core.TaskInfo{claimRow(note)}, nil
	}
	if len(plans) > claimMaxPlans {
		plans = plans[:claimMaxPlans]
	}
	return []core.TaskInfo{claimBoardRow(plans)}, nil
}

// heldGrantRow renders the board's answer when the account already holds a
// grant from the vendor, or nil when it holds none.
//
// It is a separate read from the claim board because it has to run even when no
// captcha solver is available: a grant is handed over without one, so gating it
// behind the solver would hide exactly the state an operator needs to see.  A
// failed read returns nil so the caller falls back to its own note rather than
// turning the board into an error.
func (c *Client) heldGrantRow(ctx context.Context, acct *Account) ([]core.TaskInfo, error) {
	if c == nil || c.pool == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, claimBoardTimeout)
	defer cancel()

	if win, ok := c.heldPlanWindow(ctx, acct); ok {
		return []core.TaskInfo{heldPlanRow(win)}, nil
	}
	return nil, nil
}

// claimRow is the row the board shows whenever the claim cannot run.  It is
// locked rather than merely not-auto: the panel renders the note and no button
// either way, and "未解锁" plus the reason is the honest reading of "there is
// nothing to take right now".
func claimRow(note string) core.TaskInfo {
	return core.TaskInfo{
		Code:   claimAction,
		Title:  "领取活动套餐",
		Group:  "活动套餐",
		Locked: true,
		Note:   note,
	}
}

// claimBoardRow renders the one row the board shows when a claim is possible.
//
// One row, not one per plan, because the module has exactly one action: Checkin
// claims the highest-priority plan, so a row per plan would put a 领取 button
// beside plans that button would not claim.  The others are named in the note
// instead.  Credit stays zero on purpose -- a plan grants tokens, and the
// board's reward column is denominated in 积分.
func claimBoardRow(plans []claimPlan) core.TaskInfo {
	top := plans[0]
	info := core.TaskInfo{
		Code:      claimAction,
		Title:     "领取活动套餐",
		Desc:      firstNonEmpty(top.Name, top.id()),
		Group:     "活动套餐",
		Claimable: true,
		Auto:      true,
	}
	if grants := grantSummary(top.tokenGrants()); grants != "" {
		info.Desc += "：" + grants
	}
	note := fmt.Sprintf("将领取优先级最高的套餐（共 %d 个可领取）", len(plans))
	if len(plans) > 1 {
		names := make([]string, 0, len(plans)-1)
		for _, p := range plans[1:] {
			names = append(names, firstNonEmpty(p.Name, p.id()))
		}
		note += "；其余：" + strings.Join(names, "、")
	}
	info.Note = note
	return info
}

// hasActivePlanBalance reports whether the balance document contains an active
// plan or entitlement.  The two collections are checked independently because
// the vendor has returned both shapes over time, and either one is enough to
// prove that an empty preview means "already claimed" rather than "never
// offered".
func hasActivePlanBalance(doc *planBalances) bool {
	return doc != nil && (len(doc.Plans) > 0 || len(doc.Balances) > 0)
}

// heldPlanRow renders a plan the account already holds but did not have to
// claim.  It is deliberately Claimed and Locked -- there is no button, because
// the vendor granted it -- and it names the expiry, which is the one thing an
// operator needs from a window that closes by itself.
func heldPlanRow(win planWindow) core.TaskInfo {
	name := firstNonEmpty(win.Name, win.PlanID, "活动套餐")
	info := core.TaskInfo{
		Code:    claimAction,
		Title:   "活动套餐生效中",
		Desc:    name,
		Group:   "活动套餐",
		Claimed: true,
		Locked:  true,
	}
	until := win.EndsAt.Local().Format("01-02 15:04")
	if !win.StartsAt.IsZero() {
		info.Note = fmt.Sprintf("厂商按账号活跃度自动发放，%s 起生效，%s 到期，无需手动领取",
			win.StartsAt.Local().Format("01-02 15:04"), until)
	} else {
		info.Note = fmt.Sprintf("厂商按账号活跃度自动发放，%s 到期，无需手动领取", until)
	}
	return info
}

// activePlanRow renders the steady state after a successful claim.  The row is
// deliberately Claimed and Locked: the panel already renders that pair as
// "已领取 / 已完成", and no claim button should be offered again.
func activePlanRow(doc *planBalances) core.TaskInfo {
	top := topActivePlan(doc)
	info := core.TaskInfo{
		Code:    claimAction,
		Title:   "活动套餐已领取",
		Desc:    firstNonEmpty(top.Name, top.id(), "活动套餐"),
		Group:   "活动套餐",
		Claimed: true,
		Locked:  true,
	}
	summary := activeGrantSummary(doc)
	if summary == "" {
		summary = grantSummary(top.tokenGrants())
	}
	if summary != "" {
		info.Desc += "：" + summary
	}
	info.Note = "账号已领取该活动，权益正在生效，无需重复领取"
	return info
}

// topActivePlan selects the highest-priority plan in a balance document.  The
// balance endpoint is not required to sort its rows, and the panel should name
// the same plan the claim preview would have named.
func topActivePlan(doc *planBalances) claimPlan {
	if doc == nil || len(doc.Plans) == 0 {
		if doc != nil && len(doc.Balances) > 0 {
			return claimPlan{ID: doc.Balances[0].planID()}
		}
		return claimPlan{}
	}
	top := doc.Plans[0]
	for _, p := range doc.Plans[1:] {
		if p.Priority > top.Priority || (p.Priority == top.Priority && p.id() < top.id()) {
			top = p
		}
	}
	return top
}

// activeGrantSummary summarises the remaining token buckets in a balance
// document.  Unlike claimPreview, the balance endpoint normally carries the
// grant amounts as balance rows rather than plan entitlements, so reading the
// rows is what keeps the board useful in the post-claim state.
func activeGrantSummary(doc *planBalances) string {
	if doc == nil {
		return ""
	}
	parts := make([]string, 0, len(doc.Balances))
	for _, row := range doc.Balances {
		if row.Meter != "model_usage" || !strings.EqualFold(row.UnitType, "token") || row.ShowName == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s 剩余 %s tokens", row.ShowName, humanUnits(float64(row.remaining()))))
	}
	return strings.Join(parts, ", ")
}

// RunTask implements core.TaskProvider by running the claim.
//
// The claim already IS a complete chore -- preview, pick, claim, with the
// vendor's own idempotency -- so this delegates instead of re-implementing it
// and translates the CheckinResult into the task vocabulary.  A vendor refusal
// stays a RESULT with OK false; only "could not be attempted at all" is an
// error, exactly as both core interfaces require.
//
// That is also why an unattended zcode batch cannot become a failure storm:
// Checkin reports every refusal -- no solver, wrong credential -- as a
// result, so the scheduler counts it as refused rather than failed
// (internal/scheduler/scheduler.go:703-717).
func (c *Client) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	if code != claimAction {
		return core.TaskResult{}, fmt.Errorf("zcode: unknown task %q; this client only offers %q", code, claimAction)
	}
	// Resolve the empty id the panel sends to a concrete account, so the result
	// names the credential that was actually used instead of "".
	acct, err := c.planAccount(accountID)
	if err != nil {
		return core.TaskResult{}, err
	}
	if acct == nil {
		return core.TaskResult{}, fmt.Errorf("zcode: no account can serve plan billing; add a ZCode plan (jwt) credential")
	}

	res, err := c.Checkin(ctx, acct.ID, claimAction)
	// Whatever happened, the preview behind the board row is stale now.
	c.forgetBoard()
	if err != nil {
		return core.TaskResult{}, err
	}
	if !res.OK && res.Error != "" && res.Message == "" {
		res.Message = res.Error
	}
	// CheckinResult splits "the action did not run" (Error) from "here is what
	// the vendor said" (Message); the scheduler only logs Message, so a refusal
	// that carries its reason in Error would print as a bare "refused:".
	// firstNonEmpty keeps the reason visible without inventing one.
	message := res.Message
	if message == "" {
		message = res.Error
	}
	return core.TaskResult{
		OK:        res.OK,
		Code:      claimAction,
		AccountID: res.AccountID,
		Message:   message,
		Error:     res.Error,
		ElapsedMS: res.ElapsedMS,
		At:        res.At,
	}, nil
}

var (
	_ core.TaskProvider = (*Client)(nil)
	_ core.BatchPlanner = (*Client)(nil)
)
