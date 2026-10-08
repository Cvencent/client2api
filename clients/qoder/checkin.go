package qoder

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// checkin.go implements core.CheckinProvider: the daily credits activity.
//
// Qoder CN runs a "claim 100 credits a day" campaign.  The flow is two calls and
// they are deliberately kept apart: GET /sash/api/v1/me/campaigns is read-only
// and reports which campaigns are CLAIMABLE, and only a CLAIMABLE
// CLAIM_BENEFIT campaign is then POSTed to its claim route.  Merely opening the
// panel therefore never claims anything.

const checkinActionDaily = "daily"

// Campaign action and status vocabulary.
const (
	campaignActionClaim   = "CLAIM_BENEFIT"
	campaignStatusClaimed = "CLAIMED"
	campaignStatusClaim   = "CLAIMABLE"
)

// CheckinActions implements core.CheckinProvider.  The vendor really does run a
// daily claim, so the button is offered as soon as one account exists.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.store.count() == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionDaily,
		Label: "领取每日 100 Credits",
		Help: "先读 GET /sash/api/v1/me/campaigns，把状态是 CLAIMABLE 的领取类活动逐个 " +
			"POST 到 /sash/api/v1/me/campaigns/{campaignId}/claim。活动每天 10:00（UTC+8）刷新，" +
			"领取后 30 天有效；厂商答「今天已领取」算成功，不算失败。",
	}}
}

// Checkin performs the daily claim.  An upstream refusal is a result with OK
// false; only an unknown account is a Go error.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.CheckinResult{}, fmt.Errorf("qoder: no account %q", id)
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
		res.Error = fmt.Sprintf("qoder: unknown action %q", action)
		return res, nil
	}

	start := time.Now()
	list, err := c.up.campaigns(ctx, acc.Token)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = redactErr(err)
		if failureKind(err) == core.FailureAuth {
			res.Error = "令牌已失效，请在 Qoder CN 客户端重新登录后导入"
			c.penalise(id, err, time.Now().UTC())
		}
		return res, nil
	}
	c.store.clearPenalties(id)

	claimable, alreadyClaimed := splitCampaigns(list)
	res.Data = map[string]any{
		"campaigns":       len(list.Campaigns),
		"claimable":       len(claimable),
		"already_claimed": alreadyClaimed,
	}

	if len(claimable) == 0 {
		res.OK = true
		if alreadyClaimed > 0 {
			res.Message = "今天的奖励已经领取过了"
		} else {
			res.Skipped = true
			res.Message = "该账号当前没有可领取的每日奖励"
		}
		return res, nil
	}

	claimed, failed := 0, 0
	total := 0.0
	var failures []string
	for _, cmp := range claimable {
		status, claimErr := c.up.claimCampaign(ctx, acc.Token, cmp.CampaignID)
		if claimErr != nil {
			failed++
			if failureKind(claimErr) == core.FailureAuth {
				c.penalise(id, claimErr, time.Now().UTC())
			}
			failures = append(failures, redactErr(claimErr))
			continue
		}
		if strings.EqualFold(status, campaignStatusClaimed) {
			claimed++
			total += cmp.Benefit.Amount
			continue
		}
		failed++
		failures = append(failures, fmt.Sprintf("%s 返回状态 %q", cmp.CampaignKey, status))
	}
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.Data["claimed"] = claimed
	res.Data["failed"] = failed
	if total > 0 {
		res.Data["credits"] = total
	}

	switch {
	case claimed > 0 && failed == 0:
		res.OK = true
		c.store.reset(id, time.Now().UTC())
		if total > 0 {
			res.Message = fmt.Sprintf("已领取 %d 个活动，共 %g Credits", claimed, total)
		} else {
			res.Message = fmt.Sprintf("已领取 %d 个活动", claimed)
		}
	case claimed > 0:
		res.OK = false
		res.Message = fmt.Sprintf("领取了 %d 个活动，%d 个失败", claimed, failed)
		res.Error = strings.Join(failures, "; ")
	default:
		res.OK = false
		res.Error = strings.Join(failures, "; ")
		if res.Error == "" {
			res.Error = "厂商没有把活动状态改为 CLAIMED"
		}
	}
	return res, nil
}

// splitCampaigns separates the campaigns this module may claim from the count
// of launch activities it has already collected.  A campaign of another action
// type (the vendor also publishes VIEW_DETAILS campaigns) is never claimed.
func splitCampaigns(list *campaignsResponse) ([]campaign, int) {
	if list == nil {
		return nil, 0
	}
	var claimable []campaign
	claimed := 0
	for _, cmp := range list.Campaigns {
		if !strings.EqualFold(strings.TrimSpace(cmp.ActionType), campaignActionClaim) {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(cmp.ClaimStatus)) {
		case campaignStatusClaim:
			claimable = append(claimable, cmp)
		case campaignStatusClaimed:
			claimed++
		}
	}
	return claimable, claimed
}
