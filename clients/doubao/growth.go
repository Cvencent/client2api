package doubao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// growth.go speaks the Doubao PC client's reward centre: the "agent explore"
// page's daily check-in and its board of chores, which pay out in 额度 (quota
// grants) rather than the old credit points.
//
// Everything here was read out of the desktop client's own signed-in session
// and confirmed live (2026-11-13).  Two things are load-bearing and easy to get
// wrong:
//
//   - The growth endpoints key on the PC product identity.  The same path the
//     web aid (582478) is refused with code 710010702 "system error" is served
//     under the PC aid (497858).  All calls here therefore go through pcPost.
//   - A campaign "task" is claimed by campaign_code, not by an opaque task id:
//     ClaimCampaignReward wants campaign_group_code + campaign_item_code with
//     an empty task_id, and answers success with task_status 3 (or a biz_error
//     of "confirming, retry").
//
// The task_status vocabulary is the client's own CampaignTaskStatus enum
// (Unknown=0, Created=1, Completed=2, Rewarded=3, InProgress=4), read out of
// the running client's loaded bundle.  The client treats a row as "reached"
// when status is Completed or Rewarded, and as claimable only when Completed.
// A check-in reports its own nested daily_checkin object.

// Campaign task / milestone status.  These are the vendor's own numbers.
const (
	taskStatusUnknown    = 0
	taskStatusCreated    = 1
	taskStatusCompleted  = 2
	taskStatusRewarded   = 3
	taskStatusInProgress = 4

	taskKindDailySignin = "daily_signin"

	rewardTypeQuota = "quota_grant"
)

// campaignTask is one chore or check-in row on the board.
type campaignTask struct {
	CampaignCode      string `json:"campaign_code"`
	CampaignGroupCode string `json:"campaign_group_code"`
	CampaignType      string `json:"campaign_type"`
	TaskKind          string `json:"task_kind"`
	Title             string `json:"title"`
	Subtitle          string `json:"subtitle"`
	Description       string `json:"description"`
	TaskStatus        int    `json:"task_status"`
	RewardMode        int    `json:"reward_mode"`
	RewardSummaries   []struct {
		RewardAmount int64  `json:"reward_amount"`
		RewardType   string `json:"reward_type"`
	} `json:"reward_summaries"`
	// DailyCheckin is present only on the daily_signin task.  A nil pointer
	// means "not the check-in row"; a non-nil one carries today's state.
	DailyCheckin *dailyCheckin `json:"daily_checkin"`
}

// dailyCheckin is the check-in task's own state.
type dailyCheckin struct {
	CheckedInToday  bool   `json:"checked_in_today"`
	ContinuousDays  int64  `json:"continuous_days"`
	LastCheckinDate string `json:"last_checkin_date"`
	TotalDays       int64  `json:"total_days"`
	RewardStatus    int    `json:"reward_status"`
	Reward          struct {
		RewardAmount int64  `json:"reward_amount"`
		RewardType   string `json:"reward_type"`
	} `json:"reward"`
}

// campaignDetail is the answer to GET v3/detail: tasks, milestones, progress
// and the invite campaign.
type campaignDetail struct {
	CampaignCode string `json:"campaign_code"`
	Progress     struct {
		TotalAmount int64 `json:"total_amount"`
	} `json:"progress"`
	Tasks      []campaignTask   `json:"tasks"`
	Milestones []campaignTask   `json:"milestones"`
	Fission    *fissionCampaign `json:"fission_campaign"`
	Raw        json.RawMessage  `json:"-"`
}

type fissionCampaign struct {
	CampaignCode string `json:"campaign_code"`
	CampaignData struct {
		InviteCode string `json:"invite_code"`
		InviteLink string `json:"invite_link"`
		EndTime    string `json:"end_time"`
	} `json:"campaign_data"`
}

// checkinReply is the answer to POST landing/checkin.  A repeated check-in is
// reported with Idempotent true and is still a success.
type checkinReply struct {
	CheckedIn      bool   `json:"checked_in"`
	CheckinDate    string `json:"checkin_date"`
	ContinuousDays int64  `json:"continuous_days"`
	Idempotent     bool   `json:"idempotent"`
	RewardAmount   int64  `json:"reward_amount"`
	RewardType     string `json:"reward_type"`
}

// claimReply is the answer to POST item/reward/claim.  A non-nil BizError means
// the reward did not land this attempt; status 3 is a definite success.
type claimReply struct {
	TaskStatus int `json:"task_status"`
	BizError   *struct {
		Code        int    `json:"code"`
		Message     string `json:"message"`
		ToastStatus string `json:"toast_status"`
	} `json:"biz_error"`
}

// quotaRecord is one row of the grant ledger (quota/records).
type quotaRecord struct {
	Amount       string `json:"amount"`
	CreateTimeMS int64  `json:"create_time_ms"`
	TaskCode     string `json:"task_code"`
	TaskName     string `json:"task_name"`
	Unit         string `json:"unit"`
}

type quotaRecordsReply struct {
	HasMore bool          `json:"has_more"`
	Records []quotaRecord `json:"records"`
}

// campaignBoard is the parsed check-in + task view the panel renders.
type campaignBoard struct {
	Group      string
	TotalQuota int64
	Signin     *campaignTask
	Tasks      []campaignTask
	Milestones []campaignTask
	InviteCode string
	InviteLink string
	LoadedAt   time.Time
}

// campaignCacheTTL keeps one open panel from turning the board draw into a
// stream of upstream calls.  The panel redraws on every poll.
const campaignCacheTTL = 60 * time.Second

// campaignBoardFor reads (and briefly caches) the reward centre for one
// account, trying the known campaign group codes in order.
func (c *Client) campaignBoardFor(ctx context.Context, acc account, force bool) (*campaignBoard, error) {
	key := acc.ID
	c.campMu.Lock()
	if !force {
		if b, ok := c.campaigns[key]; ok && time.Since(b.LoadedAt) < campaignCacheTTL {
			c.campMu.Unlock()
			return b, nil
		}
	}
	c.campMu.Unlock()

	var lastErr error
	for _, group := range campaignGroups {
		board, err := c.fetchCampaign(ctx, acc, group)
		if err == nil && board != nil {
			c.campMu.Lock()
			if c.campaigns == nil {
				c.campaigns = map[string]*campaignBoard{}
			}
			c.campaigns[key] = board
			c.campMu.Unlock()
			return board, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("doubao: no growth campaign is served to this account")
	}
	return nil, lastErr
}

// fetchCampaign reads one campaign group and splits its tasks.
func (c *Client) fetchCampaign(ctx context.Context, acc account, group string) (*campaignBoard, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()

	var detail campaignDetail
	err := c.up.pcPost(cctx, &acc, pathCampaignV3Detail,
		map[string]any{"campaign_group_code": group}, &detail)
	if err != nil {
		return nil, err
	}
	board := &campaignBoard{
		Group:      firstNonEmpty(detail.CampaignCode, group),
		TotalQuota: detail.Progress.TotalAmount,
		LoadedAt:   time.Now(),
	}
	for _, t := range detail.Tasks {
		if strings.EqualFold(t.TaskKind, taskKindDailySignin) || strings.EqualFold(t.CampaignCode, "daily_signin") {
			signin := t
			board.Signin = &signin
			continue
		}
		board.Tasks = append(board.Tasks, t)
	}
	board.Milestones = detail.Milestones
	if detail.Fission != nil {
		board.InviteCode = detail.Fission.CampaignData.InviteCode
		board.InviteLink = detail.Fission.CampaignData.InviteLink
	}
	return board, nil
}

// forgetCampaign drops one account's cached board after a mutation, so the
// next read sees the vendor's new state rather than the pre-claim snapshot.
func (c *Client) forgetCampaign(id string) {
	c.campMu.Lock()
	delete(c.campaigns, id)
	c.campMu.Unlock()
}

// ---------------------------------------------------------------------------
// CheckinProvider
// ---------------------------------------------------------------------------

const checkinActionDaily = "daily"

var _ core.CheckinProvider = (*Client)(nil)

// CheckinActions offers the daily check-in as soon as one account exists.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if len(c.store.snapshot()) == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionDaily,
		Label: "领取每日额度",
		Help: "读取电脑端权益中心的成长活动（GET /alice/ug/campaign/v3/detail/），" +
			"再 POST /alice/ug/campaign/landing/checkin 领取当日额度。每日可签，" +
			"累计连签天数；厂商答「今日已签到」(idempotent) 算成功。",
	}}
}

// Checkin performs the daily check-in for one account.  An upstream refusal is
// a result with OK false; only an unknown account is a Go error.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	acc, ok := c.store.byID(id)
	if !ok {
		return core.CheckinResult{}, fmt.Errorf("doubao: unknown account %q", id)
	}
	if action == "" {
		action = checkinActionDaily
	}
	res := core.CheckinResult{
		AccountID: id,
		Action:    action,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if action != checkinActionDaily {
		res.Error = fmt.Sprintf("doubao: unknown action %q", action)
		return res, nil
	}

	start := time.Now()
	board, err := c.campaignBoardFor(ctx, acc, true)
	if err != nil {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = core.Redact(err.Error())
		if isAuthError(err) {
			c.store.noteFailure(id, true, c.failureCooldown(true), err)
		}
		return res, nil
	}
	// A board with no check-in row means this account's campaign has none:
	// skipped, not failed.
	if board.Signin == nil {
		res.OK = true
		res.Skipped = true
		res.Message = "该账号的权益中心没有每日签到"
		return res, nil
	}
	res.Data = map[string]any{
		"campaign_group":  board.Group,
		"continuous_days": board.Signin.DailyCheckin.continuousDays(),
	}
	// Already checked in today: report success without a vendor call.
	if board.Signin.DailyCheckin != nil && board.Signin.DailyCheckin.CheckedInToday {
		res.OK = true
		res.Message = fmt.Sprintf("今天的额度已经领过了（已连签 %d 天）", board.Signin.DailyCheckin.ContinuousDays)
		res.Data["already_checked"] = true
		return res, nil
	}

	reply, err := c.signin(ctx, acc, board.Group)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = core.Redact(err.Error())
		if isAuthError(err) || isRateLimitError(err) {
			c.store.noteFailure(id, isAuthError(err), c.failureCooldown(isAuthError(err)), err)
		}
		return res, nil
	}
	c.store.noteSuccess(id)
	c.forgetCampaign(id)
	res.OK = true
	res.Data["checked_in"] = reply.CheckedIn
	res.Data["continuous_days"] = reply.ContinuousDays
	res.Data["reward_amount"] = reply.RewardAmount
	if reply.Idempotent {
		res.Message = "今天的额度已经领过了"
	} else if reply.RewardAmount > 0 {
		res.Message = fmt.Sprintf("已领取 %d 额度（连签 %d 天）", reply.RewardAmount, reply.ContinuousDays)
	} else {
		res.Message = "签到成功"
	}
	return res, nil
}

// continuousDays is a nil-safe accessor.
func (d *dailyCheckin) continuousDays() int64 {
	if d == nil {
		return 0
	}
	return d.ContinuousDays
}

// signin posts the daily check-in.
func (c *Client) signin(ctx context.Context, acc account, group string) (*checkinReply, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()
	var reply checkinReply
	if err := c.up.pcPost(cctx, &acc, pathCampaignCheckin, map[string]any{"scene": group}, &reply); err != nil {
		return nil, err
	}
	if !reply.CheckedIn {
		return &reply, nil
	}
	return &reply, nil
}

// ---------------------------------------------------------------------------
// TaskProvider
// ---------------------------------------------------------------------------

var _ core.TaskProvider = (*Client)(nil)

// Tasks lists the reward centre's chores as the panel's board.
func (c *Client) Tasks(ctx context.Context, accountID string) ([]core.TaskInfo, error) {
	acc, err := c.accountFor(accountID)
	if err != nil {
		return nil, err
	}
	board, err := c.campaignBoardFor(ctx, acc, false)
	if err != nil {
		return nil, err
	}
	out := make([]core.TaskInfo, 0, len(board.Tasks)+len(board.Milestones)+1)
	if board.Signin != nil {
		out = append(out, campaignTaskInfo(*board.Signin, board.Group))
	}
	for _, t := range board.Tasks {
		out = append(out, campaignTaskInfo(t, board.Group))
	}
	for _, m := range board.Milestones {
		out = append(out, campaignTaskInfo(m, board.Group))
	}
	// The invite campaign is informational: the module cannot invite on the
	// operator's behalf, so it is shown locked with the operator's own code.
	if board.InviteCode != "" {
		out = append(out, core.TaskInfo{
			Code:  "invite_campaign",
			Title: "邀请好友",
			Desc:  "邀请链接：" + board.InviteLink,
			Group: "邀请",
			Auto:  false,
			Note:  "邀请码 " + board.InviteCode + "；邀请需要真人在自己设备上打开，网关不代做",
		})
	}
	return out, nil
}

// campaignTaskInfo renders one campaign row.
//
// auto is deliberately NOT a constant: the gateway can claim a reward that is
// already waiting (the check-in, or a chore the operator finished in the
// client), but it cannot perform a work chore for the operator.  Offering a
// button on a row the gateway can only refuse would be the panel lying about
// what "run" does, so each row decides.
func campaignTaskInfo(t campaignTask, group string) core.TaskInfo {
	info := core.TaskInfo{
		Code:  t.CampaignCode,
		Title: firstNonEmpty(t.Title, t.CampaignCode),
		Desc:  firstNonEmpty(t.Subtitle, t.Description),
		Group: group,
	}
	for _, r := range t.RewardSummaries {
		info.Credit += r.RewardAmount
	}
	switch t.TaskStatus {
	case taskStatusRewarded:
		// The reward has been granted.
		info.Claimed = true
		info.Current, info.Target = 1, 1
	case taskStatusCompleted:
		// The chore is done and the reward is waiting: the gateway can collect
		// it, so the row is actionable.
		info.Claimable = true
		info.Auto = true
		info.Current, info.Target = 1, 1
	case taskStatusCreated:
		// Not done yet.  A work chore must be performed in the client first,
		// so the gateway cannot run it; the check-in row is the exception
		// (handled below), because "created" there means "not yet checked in
		// today" and IS claimable.
		info.Current, info.Target = 0, 1
		info.Note = "需在豆包电脑端完成该任务"
	case taskStatusInProgress:
		info.Current, info.Target = 0, 1
		info.Note = "进行中"
	default:
		info.Locked = true
		info.Note = "厂商当前不开放"
	}
	// The check-in row carries its own state and is the one chore the gateway
	// performs end to end.
	if t.DailyCheckin != nil {
		info.Auto = true
		if t.DailyCheckin.CheckedInToday {
			info.Claimed, info.Claimable = true, false
			info.Current, info.Target = 1, 1
			info.Note = ""
			info.Desc = fmt.Sprintf("已连签 %d 天", t.DailyCheckin.ContinuousDays)
		} else {
			// Not yet checked in today: the button must be offered.
			info.Claimed, info.Claimable = false, true
			info.Current, info.Target = 0, 1
			info.Note = ""
			if t.DailyCheckin.ContinuousDays > 0 {
				info.Desc = fmt.Sprintf("已连签 %d 天", t.DailyCheckin.ContinuousDays)
			} else {
				info.Desc = "每日可领"
			}
		}
	}
	if info.Desc == "" {
		info.Desc = "累计额度奖励"
	}
	return info
}

// RunTask performs one chore.  The daily check-in and every reward task are the
// same underlying POST; whether it is a check-in or a reward claim depends on
// the row.
func (c *Client) RunTask(ctx context.Context, accountID, code string) (core.TaskResult, error) {
	acc, err := c.accountFor(accountID)
	if err != nil {
		return core.TaskResult{}, err
	}
	res := core.TaskResult{Code: code, AccountID: acc.ID, At: time.Now().UTC().Format(time.RFC3339)}
	start := time.Now()

	board, err := c.campaignBoardFor(ctx, acc, true)
	if err != nil {
		res.Error = core.Redact(err.Error())
		return res, nil
	}
	res.CheckinAction = checkinActionDaily

	// The check-in row goes through the check-in route.
	if board.Signin != nil && strings.EqualFold(code, board.Signin.CampaignCode) {
		reply, cerr := c.signin(ctx, acc, board.Group)
		res.ElapsedMS = time.Since(start).Milliseconds()
		if cerr != nil {
			res.Error = core.Redact(cerr.Error())
			return res, nil
		}
		c.forgetCampaign(acc.ID)
		res.OK = true
		res.Credit = reply.RewardAmount
		if reply.Idempotent {
			res.Message = "今日已签到"
		} else {
			res.Message = fmt.Sprintf("已领取 %d 额度", reply.RewardAmount)
		}
		return res, nil
	}

	// Everything else is a reward claim on the matching task row.
	task, found := board.findTask(code)
	if !found {
		res.Error = fmt.Sprintf("doubao: this campaign has no task %q", code)
		return res, nil
	}
	if task.TaskStatus == taskStatusRewarded {
		res.OK = true
		res.Skipped = true
		res.Message = "该任务奖励已领取"
		return res, nil
	}
	// A work chore the operator has not performed yet cannot be claimed from
	// the gateway: the vendor only pays out once the client reports the chore
	// done (status Completed).  Saying so beats a claim that can only fail.
	if task.TaskStatus == taskStatusCreated || task.TaskStatus == taskStatusInProgress {
		res.Error = fmt.Sprintf("doubao: %q 需要先在豆包电脑端完成该任务，完成后再来领取", code)
		return res, nil
	}
	reply, cerr := c.claim(ctx, acc, board.Group, task.CampaignCode)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if cerr != nil {
		res.Error = core.Redact(cerr.Error())
		return res, nil
	}
	c.forgetCampaign(acc.ID)
	if reply.BizError != nil {
		// "confirming, retry" (code 3) is not a failure the operator should
		// read as final; say so and let the next run settle it.
		res.Message = reply.BizError.Message
		res.Error = reply.BizError.Message
		return res, nil
	}
	if reply.TaskStatus != taskStatusRewarded {
		res.Error = fmt.Sprintf("doubao: the vendor did not mark %q as rewarded (status %d)", code, reply.TaskStatus)
		return res, nil
	}
	res.OK = true
	for _, r := range task.RewardSummaries {
		res.Credit += r.RewardAmount
	}
	if res.Credit > 0 {
		res.Message = fmt.Sprintf("已领取 %d 额度", res.Credit)
	} else {
		res.Message = "已领取"
	}
	return res, nil
}

// claim posts one reward claim.
func (c *Client) claim(ctx context.Context, acc account, group, itemCode string) (*claimReply, error) {
	cctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()
	var reply claimReply
	err := c.up.pcPost(cctx, &acc, pathCampaignClaim, map[string]any{
		"task_id":             "",
		"campaign_group_code": group,
		"campaign_item_code":  itemCode,
	}, &reply)
	if err != nil {
		return nil, err
	}
	return &reply, nil
}

// findTask locates a task or milestone by campaign_code.
func (b *campaignBoard) findTask(code string) (campaignTask, bool) {
	if b == nil {
		return campaignTask{}, false
	}
	for _, t := range append(append([]campaignTask{}, b.Tasks...), b.Milestones...) {
		if strings.EqualFold(t.CampaignCode, code) {
			return t, true
		}
	}
	if b.Signin != nil && strings.EqualFold(b.Signin.CampaignCode, code) {
		return *b.Signin, true
	}
	return campaignTask{}, false
}

// pickAccount resolves the account a task call runs against.
func (c *Client) accountFor(accountID string) (account, error) {
	if strings.TrimSpace(accountID) != "" {
		acc, ok := c.store.byID(accountID)
		if !ok {
			return account{}, fmt.Errorf("doubao: unknown account %q", accountID)
		}
		return acc, nil
	}
	for _, acc := range c.store.snapshot() {
		if acc.selectable(time.Now()) {
			return acc, nil
		}
	}
	return account{}, errors.New("doubao: no usable account")
}

// quotaLedger reads the recent grant rows (used for the balance detail).
func (c *Client) quotaLedger(ctx context.Context, acc account, size int) ([]quotaRecord, error) {
	if size <= 0 {
		size = 20
	}
	cctx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()
	var reply quotaRecordsReply
	if err := c.up.pcPost(cctx, &acc, pathCampaignQuotaRec,
		map[string]any{"cursor": "", "page_size": size}, &reply); err != nil {
		return nil, err
	}
	records := reply.Records
	sort.SliceStable(records, func(i, j int) bool { return records[i].CreateTimeMS > records[j].CreateTimeMS })
	return records, nil
}
