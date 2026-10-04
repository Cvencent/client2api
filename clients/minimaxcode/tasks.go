package minimaxcode

// Task board and scheduler wiring for MiniMax Code.
//
// This product has exactly one chore worth automating -- the seven-day daily
// sign-in that signin.go implements -- so both optional interfaces here are
// thin adapters over it rather than a second protocol:
//
//	core.TaskProvider   the panel's 任务看板 row (read the board, run the claim)
//	core.BatchPlanner   the scheduler's "checkin" slot
//
// The scheduler wants BOTH.  internal/scheduler/scheduler.go:415 plan() skips a
// client unless it implements TaskProvider *and* BatchPlanner, and a declared
// batch only fires when the config has an enabled group with that name, so
// implementing only one of the two would leave this client invisible on the
// timetable.  Before this file it was in exactly that state: the check-in
// existed, but only as a button the operator had to press.

import (
	"context"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// signinBatchCheckin is the scheduler group the daily sign-in belongs to.  It
// is the literal the scheduler's own unexported batchCheckin carries
// (internal/scheduler/scheduler.go:78) and the key configs/client2api.json uses
// under schedule.groups, so the three have to agree; a mismatch is not a
// compile error, it is a batch that silently never fires.
const signinBatchCheckin = "checkin"

// Batches implements core.BatchPlanner.
//
// Static on purpose: the scheduler calls this on every tick and the panel may
// call it while rendering the schedule, so it must not touch the network or the
// account pool -- it only says what this client *could* run.
func (c *Client) Batches() []core.Batch {
	return []core.Batch{{
		Name:  signinBatchCheckin,
		Codes: []string{signinActionDaily},
	}}
}

// taskAccount resolves the account a board or run call means.
//
// An empty id means "whichever account you would use anyway" -- that is core's
// TaskProvider contract, and it is how the panel's task board calls this.  A
// non-empty id that does not exist is an error, because quietly showing an
// empty board for a typo would look like a vendor problem.
//
// A zero Account with a nil error means there is simply no usable account: the
// caller answers with an empty board rather than a failure, which the panel
// already renders as "账号未就绪".
func (c *Client) taskAccount(ctx context.Context, id string) (Account, bool, error) {
	if strings.TrimSpace(id) != "" {
		acct, parked, ok := c.signinAccount(ctx, id)
		if !ok {
			return Account{}, false, fmt.Errorf("minimaxcode: account %q not found", id)
		}
		return acct, parked, nil
	}
	if c == nil || c.pool == nil {
		return Account{}, false, nil
	}
	for _, a := range c.pool.selectableSnapshots() {
		return c.ensureFresh(ctx, a.ID), false, nil
	}
	return Account{}, false, nil
}

// Tasks implements core.TaskProvider.  The board has exactly one row: the
// seven-day sign-in cycle, positioned at whichever day the vendor calls today.
//
// The row is always returned, even when it cannot be run.  A disabled account,
// a timezone that does not line up with the account's own day, a day the vendor
// has locked -- all three are states the operator can act on, and a board that
// hid the row would leave them guessing which of the three it was.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	acct, parked, err := c.taskAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if acct.ID == "" {
		return nil, nil
	}
	if parked {
		return []core.TaskInfo{{
			Code:  signinActionDaily,
			Title: "每日签到",
			Group: "签到",
			Note:  "账号已停用；启用后才能签到",
		}}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, signinTimeout)
	defer cancel()

	panel, err := c.signinPanelOf(ctx, acct)
	if err != nil {
		return nil, err
	}

	today, found := panel.today()
	if !found {
		// Same trap the claim guards against: the vendor computes "today" in
		// the timezone it was handed, so a board with no current day means the
		// zone does not match the account.  Saying so IS the fix.
		return []core.TaskInfo{{
			Code:  signinActionDaily,
			Title: "每日签到",
			Group: "签到",
			Note: fmt.Sprintf("厂商在时区 %q 里没有标出“今天”；把该客户端的 timezone 设成账号自己的时区",
				c.cfg.signinTimezone()),
		}}, nil
	}

	info := core.TaskInfo{
		Code:    signinActionDaily,
		Title:   "每日签到",
		Desc:    fmt.Sprintf("七天一轮，今天第 %d 天", today.DayNo),
		Group:   "签到",
		Credit:  today.Points,
		Current: int64(today.DayNo),
		Target:  int64(len(panel.Days)),
		Auto:    true,
	}
	switch today.Status {
	case signinDayClaimed:
		info.Claimed = true
		info.Note = "今天已经领过了"
	case signinDayClaimable:
		info.Claimable = true
		info.Note = "今天可以领取"
	default:
		// Locked rather than merely not-auto: the panel renders a note and no
		// button either way, but "不可用" is a vendor verdict, not a decision
		// this module made.
		info.Auto = false
		info.Locked = true
		info.Note = fmt.Sprintf("今天不能领：%s", signinDayStatusName(today.Status))
	}
	if today.BonusPoints > 0 {
		info.Note += fmt.Sprintf("（额外奖励 %d）", today.BonusPoints)
	}
	return []core.TaskInfo{info}, nil
}

// RunTask implements core.TaskProvider by running the daily claim.
//
// The check-in already IS a complete chore -- status first, then claim, with
// the vendor's own idempotency -- so this delegates instead of re-implementing
// it and translates the CheckinResult into the task vocabulary.  A vendor
// refusal stays a RESULT with OK false; only "could not be attempted at all"
// is an error, exactly as both core interfaces require.
func (c *Client) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	if code != signinActionDaily {
		return core.TaskResult{}, fmt.Errorf("minimaxcode: unknown task %q; this client only offers %q", code, signinActionDaily)
	}
	res, err := c.Checkin(ctx, accountID, signinActionDaily)
	if err != nil {
		return core.TaskResult{}, err
	}
	out := core.TaskResult{
		OK:        res.OK,
		Code:      signinActionDaily,
		AccountID: res.AccountID,
		Message:   res.Message,
		Error:     res.Error,
		ElapsedMS: res.ElapsedMS,
		At:        res.At,
	}
	// Credit is what actually landed.  A repeat carries already_done and no
	// number, so it reports zero rather than re-announcing a grant that
	// happened on an earlier run -- the board would otherwise add it up twice.
	if done, _ := res.Data["already_done"].(bool); !done {
		out.Credit = taskCreditOf(res.Data)
	}
	return out, nil
}

// taskCreditOf reads the points Checkin recorded, which it always stores as an
// int64.  The other numeric cases are accepted so a future change to that map
// cannot silently zero the board's reward column.
func taskCreditOf(data map[string]any) int64 {
	switch v := data["points"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

var (
	_ core.TaskProvider = (*Client)(nil)
	_ core.BatchPlanner = (*Client)(nil)
)
