package qoder

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// tasks.go implements the panel's task centre from the vendor's own campaign
// list.  Qoder does not expose a WorkBuddy-style growth board, so this module
// deliberately shows only what the vendor actually publishes:
//
//   - every campaign is a row, including informational "view details" ones;
//   - only a CLAIM_BENEFIT campaign whose status is CLAIMABLE is automated;
//   - a claimed campaign is reported as claimed, never re-collected.
//
// The campaign list is the same source the daily check-in already uses, so a
// campaign claim here is a real vendor action rather than a synthetic task.

// taskBoardTTL keeps one open panel from turning the campaign read into a
// stream of vendor calls.  A claim clears the cache immediately.
const taskBoardTTL = 10 * time.Second

const (
	qoderTaskGroup = "活动任务"
)

// Tasks implements core.TaskProvider.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No credential yet is a normal panel state.  Returning an empty board
		// is more honest than inventing a task or an account.
		return nil, nil
	}
	return c.taskBoardFor(ctx, acc)
}

// RunTask implements core.TaskProvider.  Qoder has no separate "accept" step:
// a campaign that is claimable is collected directly, and everything else
// explains why it needs the official client.
func (c *Client) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	trimmed := strings.TrimSpace(code)
	res := core.TaskResult{Code: trimmed, At: time.Now().UTC().Format(time.RFC3339)}
	if trimmed == "" {
		return res, errors.New("qoder: no task code was given")
	}

	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return res, err
	}
	if !ok {
		res.Error = "qoder: no account is available for the task board"
		return res, nil
	}
	res.AccountID = acc.ID

	row, found, err := c.taskRow(ctx, acc, trimmed)
	if err != nil {
		return res, err
	}
	if !found {
		return res, fmt.Errorf("qoder: this account has no campaign %q", trimmed)
	}
	if row.Claimed {
		res.OK = true
		res.Skipped = true
		res.Message = "该活动已经领取过了"
		return res, nil
	}
	if !row.Claimable || !row.Auto {
		res.Skipped = true
		res.Message = firstNonEmpty(row.Note, "该活动需要在 Qoder 客户端内手动完成")
		return res, nil
	}

	start := time.Now()
	claim, err := c.claimCampaign(ctx, acc, trimmed)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = redactErr(err)
		return res, nil
	}
	res.OK = true
	res.Credit = claim.Credit
	res.Message = firstNonEmpty(claim.Message, "活动奖励已领取")
	return res, nil
}

// ClaimTask implements core.TaskClaimer.  The claim route is addressed by the
// campaign id, which is why Tasks uses the id as the task code.
func (c *Client) ClaimTask(ctx context.Context, accountID, code string) (core.TaskClaim, error) {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return core.TaskClaim{}, errors.New("qoder: no task code was given")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	acc, ok, err := c.taskAccount(accountID)
	if err != nil {
		return core.TaskClaim{}, err
	}
	if !ok {
		return core.TaskClaim{}, errors.New("qoder: no account is available for the task board")
	}

	row, found, err := c.taskRow(ctx, acc, trimmed)
	if err != nil {
		return core.TaskClaim{}, err
	}
	if !found {
		return core.TaskClaim{}, fmt.Errorf("qoder: this account has no campaign %q", trimmed)
	}
	if row.Claimed {
		return core.TaskClaim{AlreadyClaimed: true, Message: "该活动已经领取过了"}, nil
	}
	if !row.Claimable {
		return core.TaskClaim{Message: firstNonEmpty(row.Note, "该活动当前不可领取")}, nil
	}

	claim, err := c.claimCampaign(ctx, acc, trimmed)
	if err != nil {
		return core.TaskClaim{}, err
	}
	return claim, nil
}

// RunTaskAuto implements core.TaskAutoRunner.  For Qoder, automating a task
// means collecting a campaign that the vendor already marked CLAIMABLE.  There
// is no progress to fake and no accept step to call.
func (c *Client) RunTaskAuto(ctx context.Context, accountID, code string) (res core.AutoTaskResult, err error) {
	trimmed := strings.TrimSpace(code)
	res = core.AutoTaskResult{TaskResult: core.TaskResult{
		Code:      trimmed,
		AccountID: strings.TrimSpace(accountID),
		At:        time.Now().UTC().Format(time.RFC3339),
	}}
	if trimmed == "" {
		return res, errors.New("qoder: no task code was given")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	acc, ok, terr := c.taskAccount(accountID)
	if terr != nil {
		return res, terr
	}
	if !ok {
		return res, errors.New("qoder: no account is available for the task board")
	}
	res.AccountID = acc.ID

	row, found, terr := c.taskRow(ctx, acc, trimmed)
	if terr != nil {
		return res, terr
	}
	if !found {
		return res, fmt.Errorf("qoder: this account has no campaign %q", trimmed)
	}
	res.ProgressBefore = qoderProgressText(row)
	if row.Claimed {
		res.OK = true
		res.Skipped = true
		res.ProgressAfter = res.ProgressBefore
		res.Message = "该活动已经领取过了"
		return res, nil
	}
	res.Claimable = row.Claimable
	if !row.Claimable || !row.Auto {
		// Not an automated action: the panel turns this into a 501-style
		// explanation and keeps the row visible so the operator can open it in
		// the official client.
		return res, errors.New(firstNonEmpty(row.Note, "qoder: this campaign cannot be claimed from the panel"))
	}

	start := time.Now()
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()
	res.Attempt = true

	claim, cerr := c.claimCampaign(ctx, acc, trimmed)
	if cerr != nil {
		res.Error = redactErr(cerr)
		res.ClaimError = res.Error
		return res, nil
	}
	res.OK = true
	res.Claimed = true
	res.Credit = claim.Credit
	res.ProgressAfter = "claimed"
	res.Message = firstNonEmpty(claim.Message, "活动奖励已领取")
	return res, nil
}

// taskAccount resolves the credential a board read or a run means.  An empty id
// follows the pool's own rotation; a non-empty unknown id is an error because
// an empty board for a typo looks like a vendor outage.
func (c *Client) taskAccount(id string) (account, bool, error) {
	if trimmed := strings.TrimSpace(id); trimmed != "" {
		acc, ok := c.store.lookup(trimmed)
		if !ok {
			return account{}, false, fmt.Errorf("qoder: no account %q", trimmed)
		}
		return acc, true, nil
	}
	now := time.Now().UTC()
	if list := c.store.candidates(now); len(list) > 0 {
		return list[0], true, nil
	}
	if list := c.store.snapshot(); len(list) > 0 {
		return list[0], true, nil
	}
	return account{}, false, nil
}

// taskBoardFor reads and briefly caches the vendor's campaign list.
func (c *Client) taskBoardFor(ctx context.Context, acc account) ([]core.TaskInfo, error) {
	now := time.Now().UTC()
	c.taskMu.Lock()
	if c.taskBoard != nil && c.taskAccountID == acc.ID && now.Sub(c.taskBoardAt) < taskBoardTTL {
		// An empty board is cached too: without the timestamp check a vendor
		// account with no running campaigns would be re-read on every draw.
		rows := c.taskBoard
		c.taskMu.Unlock()
		return rows, nil
	}
	c.taskMu.Unlock()

	list, err := c.up.campaigns(ctx, acc.Token)
	if err != nil {
		if !callerGone(err) {
			c.penalise(acc.ID, err, now)
		}
		return nil, err
	}
	c.store.clearPenalties(acc.ID)

	rows := campaignsToTasks(list)
	c.taskMu.Lock()
	c.taskBoard, c.taskAccountID, c.taskBoardAt = rows, acc.ID, now
	c.taskMu.Unlock()
	return rows, nil
}

// taskRow returns one campaign task, refreshing the board when the cache is
// empty.  The second result is false when the board has no such campaign.
func (c *Client) taskRow(ctx context.Context, acc account, code string) (core.TaskInfo, bool, error) {
	rows, err := c.taskBoardFor(ctx, acc)
	if err != nil {
		return core.TaskInfo{}, false, err
	}
	for _, row := range rows {
		if row.Code == code {
			return row, true, nil
		}
	}
	return core.TaskInfo{}, false, nil
}

// forgetTaskBoard drops the cache after a claim so the next read sees the
// vendor's new status rather than the pre-claim snapshot.
func (c *Client) forgetTaskBoard() {
	c.taskMu.Lock()
	c.taskBoard, c.taskAccountID, c.taskBoardAt = nil, "", time.Time{}
	c.taskMu.Unlock()
}

// claimCampaign is the one place that mutates campaign state.  It also records
// a credential rejection the same way balance reads do.
func (c *Client) claimCampaign(ctx context.Context, acc account, campaignID string) (core.TaskClaim, error) {
	credit := c.cachedCampaignCredit(acc.ID, campaignID)
	status, err := c.up.claimCampaign(ctx, acc.Token, campaignID)
	if err != nil {
		if !callerGone(err) {
			c.penalise(acc.ID, err, time.Now().UTC())
		}
		return core.TaskClaim{}, err
	}
	c.store.reset(acc.ID, time.Now().UTC())
	c.forgetTaskBoard()

	if strings.EqualFold(strings.TrimSpace(status), campaignStatusClaimed) {
		return core.TaskClaim{Credit: credit, Message: fmt.Sprintf("已领取 %d 积分", credit)}, nil
	}
	// A 2xx with no explicit status is still a successful claim on this API.
	return core.TaskClaim{Credit: credit, Message: "活动奖励已领取"}, nil
}

// cachedCampaignCredit reads the reward amount from the cached board.  The
// value is a display detail, so a missing cache entry yields zero rather than
// turning a successful claim into a failure; the amount is captured before the
// claim clears the cache.
func (c *Client) cachedCampaignCredit(accountID, campaignID string) int64 {
	c.taskMu.Lock()
	rows, owner := c.taskBoard, c.taskAccountID
	c.taskMu.Unlock()
	if owner != accountID {
		return 0
	}
	for _, row := range rows {
		if row.Code == campaignID {
			return row.Credit
		}
	}
	return 0
}

// campaignsToTasks maps the vendor's campaign objects to the panel's board.
// Claimable rows sort first and already-claimed rows last; everything else
// keeps the vendor's own relative order.
func campaignsToTasks(list *campaignsResponse) []core.TaskInfo {
	if list == nil {
		return nil
	}
	rows := make([]core.TaskInfo, 0, len(list.Campaigns))
	for _, cmp := range list.Campaigns {
		id := strings.TrimSpace(firstNonEmpty(cmp.CampaignID, cmp.CampaignKey))
		if id == "" {
			continue
		}
		action := strings.ToUpper(strings.TrimSpace(cmp.ActionType))
		status := strings.ToUpper(strings.TrimSpace(cmp.ClaimStatus))
		claimable := action == campaignActionClaim && status == campaignStatusClaim
		claimed := action == campaignActionClaim && status == campaignStatusClaimed

		row := core.TaskInfo{
			Code:      id,
			Title:     firstNonEmpty(cmp.Title, cmp.Name, cmp.CampaignName, cmp.CampaignKey, id),
			Desc:      strings.TrimSpace(cmp.Description),
			Group:     qoderTaskGroup,
			Credit:    int64(math.Round(cmp.Benefit.Amount)),
			Target:    1,
			Claimable: claimable,
			Claimed:   claimed,
			Auto:      claimable,
		}
		switch {
		case claimed:
			row.Current = 1
			row.Note = "该活动已领取"
		case claimable:
			row.Note = "可从面板直接领取"
		case action == campaignActionClaim:
			row.Note = "活动当前不可领取；请稍后在 Qoder 客户端内查看"
			row.Locked = true
		default:
			row.Note = "该活动需要前往 Qoder 客户端查看详情"
			row.Locked = true
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Claimable != rows[j].Claimable {
			return rows[i].Claimable
		}
		if rows[i].Claimed != rows[j].Claimed {
			return !rows[i].Claimed
		}
		return false
	})
	return rows
}

func qoderProgressText(row core.TaskInfo) string {
	switch {
	case row.Claimed:
		return "claimed"
	case row.Claimable:
		return "claimable"
	default:
		return "pending"
	}
}
