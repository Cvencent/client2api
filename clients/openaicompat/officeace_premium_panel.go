package openaicompat

// officeace_premium_panel.go exposes what the OfficeAce premium wire can do to
// the panel's optional capability interfaces.  internal/core discovers these
// by type assertion, so adding the methods is the whole wiring: nothing is
// registered here and no vendor call is invented.
//
// The daily claim and the credit/package breakdown are the two premium
// features the OfficeAce client actually has.  There is no growth-task board
// and no voucher list behind the wire, so this module deliberately implements
// neither TaskProvider nor VoucherProvider rather than offering a button that
// could only fail.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	officeAceCheckinAction = "daily-checkin"
	// officeAceBalanceUnit is what one officeace_points is called.
	officeAceBalanceUnit = "积分"
)

var (
	_ core.CheckinProvider = (*officeAceSource)(nil)
	_ core.BalanceProvider = (*officeAceSource)(nil)
	_ core.PackageProvider = (*officeAceSource)(nil)
)

func officeAceContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// CheckinActions offers the daily bonus claim only while at least one stored
// account carries a cloud session.  A pool of import-only accounts shows no
// button, exactly as the capability contract asks.
func (o *officeAceSource) CheckinActions(ctx context.Context) []core.CheckinAction {
	if o == nil || o.source == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	if !o.officeAceHasPremiumAccount() {
		return nil
	}
	return []core.CheckinAction{{
		ID:    officeAceCheckinAction,
		Label: "每日签到（OfficeAce）",
		Help: "在 OfficeAce 模型网关上 POST /v1/subscription/bonus/claim。云端每天发放一次“每日签到有礼”奖励；" +
			"该接口是幂等的，返回空的 bonus_skus 表示今天的奖励已经领过。",
	}}
}

// Checkin claims the day's reward for one account.  A cloud refusal is a
// CheckinResult with OK false; only an unknown account or a missing session is
// a Go error.
func (o *officeAceSource) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	ctx = officeAceContext(ctx)
	if strings.TrimSpace(action) == "" {
		action = officeAceCheckinAction
	}
	res := core.CheckinResult{
		AccountID: strings.TrimSpace(id),
		Action:    action,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if action != officeAceCheckinAction {
		res.Error = fmt.Sprintf("unknown action %q", action)
		return res, nil
	}
	p, err := o.premiumFor(id)
	if err != nil {
		return core.CheckinResult{}, err
	}
	if _, err := p.subscription(ctx); err != nil {
		res.Error = core.Redact(err.Error())
		return res, nil
	}
	start := time.Now()
	bonuses, err := p.claimBonus(ctx)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = core.Redact(err.Error())
		return res, nil
	}
	res.OK = true
	names := make([]string, 0, len(bonuses))
	for _, bonus := range bonuses {
		names = append(names, firstNonEmpty(strings.TrimSpace(bonus.ActivityName), strings.TrimSpace(bonus.ActivityID)))
	}
	if len(names) == 0 {
		res.Message = "今天的奖励已经领过了（本次没有新的可领奖励）"
	} else {
		res.Message = "已领取：" + strings.Join(names, "、")
	}
	data := map[string]any{"claimed": names}
	if sub, serr := p.subscription(ctx); serr == nil {
		points := sub.points(time.Now())
		data["credits"] = points.remaining()
		data["total"] = points.Total
		data["used"] = points.Used
		data["unlimited"] = points.Unlimited
		if plan := sub.planName(); plan != "" {
			data["plan"] = plan
		}
	}
	res.Data = data
	return res, nil
}

// AccountBalance reports the live officeace_points position plus the online
// search meter when the plan carries one.
func (o *officeAceSource) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	p, err := o.premiumFor(id)
	if err != nil {
		return core.Balance{}, err
	}
	sub, err := p.subscription(officeAceContext(ctx))
	if err != nil {
		return core.Balance{}, err
	}
	now := time.Now()
	points := sub.points(now)
	bal := core.Balance{
		Credits:   points.remaining(),
		Total:     points.Total,
		Used:      float64(points.Used),
		Unlimited: points.Unlimited,
		Unit:      officeAceBalanceUnit,
	}
	if points.Unlimited {
		bal.Total = -1
	}
	if total, used, ok := sub.searchQuota(); ok {
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		meterTotal := total
		meterRemaining := remaining
		bal.Lines = append(bal.Lines, core.BalanceLine{
			Label:  "联网搜索",
			Unit:   "次",
			Amount: &meterRemaining,
			Total:  &meterTotal,
		})
	}
	return bal, nil
}

// AccountPackages breaks the credit position into the plan quota and each
// bonus activity, which is exactly how the client groups them.
func (o *officeAceSource) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	p, err := o.premiumFor(id)
	if err != nil {
		return core.PackageReport{}, err
	}
	sub, err := p.subscription(officeAceContext(ctx))
	if err != nil {
		return core.PackageReport{}, err
	}
	now := time.Now()
	out := make([]core.CreditPackage, 0, len(sub.Skus)+len(sub.Bonus))
	var remain, size int64
	for _, sku := range sub.Skus {
		for _, quota := range sku.Quotas {
			if quota.Attr != "officeace_points" || !officeAceBonusValid(quota.Expired, now) {
				continue
			}
			total := officeAceInt(quota.Value)
			if total < 0 {
				continue
			}
			used := officeAceInt(quota.Current)
			left := total - used
			if left < 0 {
				left = 0
			}
			remain += left
			size += total
			out = append(out, core.CreditPackage{
				Name:           firstNonEmpty(strings.TrimSpace(sku.Name), strings.TrimSpace(sku.Code)),
				Remain:         left,
				Used:           used,
				Size:           total,
				EndTime:        strings.TrimSpace(quota.Expired),
				ExpiresAt:      officeAceEpochMillis(quota.Expired),
				CreatedAt:      strings.TrimSpace(quota.Effective),
				PackageCode:    strings.TrimSpace(sku.Code),
				SubProductName: "套餐",
			})
		}
	}
	for _, bonus := range sub.Bonus {
		if !officeAceBonusValid(bonus.Expired, now) {
			continue
		}
		total := officeAceInt(bonus.Points)
		if total < 0 {
			continue
		}
		used := officeAceInt(bonus.CurrentValue)
		left := total - used
		if left < 0 {
			left = 0
		}
		remain += left
		size += total
		out = append(out, core.CreditPackage{
			Name:           firstNonEmpty(strings.TrimSpace(bonus.ActivityName), strings.TrimSpace(bonus.ActivityID)),
			Remain:         left,
			Used:           used,
			Size:           total,
			EndTime:        strings.TrimSpace(bonus.Expired),
			ExpiresAt:      officeAceEpochMillis(bonus.Expired),
			CreatedAt:      strings.TrimSpace(bonus.Effective),
			PackageCode:    strings.TrimSpace(bonus.ActivityID),
			SubProductName: "活动奖励",
		})
	}
	return core.PackageReport{Remain: remain, Size: size, Packages: out}, nil
}

// officeAceEpochMillis turns an RFC3339 timestamp into the millisecond epoch
// the panel sorts on.  An absent or unparseable value is 0, not a wrong date.
func officeAceEpochMillis(raw string) int64 {
	t, ok := officeAceParseTime(raw)
	if !ok {
		return 0
	}
	return t.UnixMilli()
}
