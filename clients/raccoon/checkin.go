package raccoon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// checkin.go implements the desktop login reward for Raccoon (商汤小浣熊).
//
// The vendor's own desktop client calls exactly one endpoint, from a React hook
// that runs on mount whenever the operator is already signed in:
//
//	POST /api/web/desktop/v1/login/points/grant
//	X-Client-Platform: desktop-windows   (required)
//	X-Client-Version:  v1.0.35
//
// **This endpoint extends the daily 300 points, it does not mint them.** The
// vendor's own文案 states the rule: 每日积分每天 00:00 发放，默认当日 24:00
// 清零，当天登录后有效期延长至 7 天. The points ledger agrees -- the 00:00
// issuance is a `daily_grant` of +300 expiring the SAME day, and a successful
// desktop grant adds another `daily_grant` of +300 whose `expire_at` reaches
// seven days out, which is what the popup announces.
//
// That makes `granted:false` a statement about THIS attempt, not about the
// day: the extension did not apply this time. Treating it as a day-level
// verdict is a bug with teeth -- the panel latches a skipped action as
// "unavailable" until the next Shanghai midnight and disables the button, so
// one refusal at 01:00 silently burned the reward for the rest of the day even
// though the vendor was still handing it out at 21:41.
//
// So a refusal is resolved against the ledger instead of guessed at:
//
//   - a `daily_grant` for today whose expiry reaches past today proves the
//     reward is in hand            -> OK, Data["already_done"] = true
//   - no such row                  -> a RETRYABLE refusal, never Skipped
//   - the ledger cannot be read    -> also retryable; a nicety must not
//     fabricate a verdict
//
// The vendor's own hook is fire-and-forget: it logs a failure to the console
// and shows a popup only when `granted` is true.

const (
	// checkinActionLoginPoints is the single action this module offers.
	checkinActionLoginPoints = "login-points"

	// pointsGrantTimeout bounds the one round trip. It is a small JSON POST;
	// 60 s (the module default) would leave a panel click spinning far longer
	// than the vendor ever takes.
	pointsGrantTimeout = 20 * time.Second

	// billsTimeout bounds the ledger read used to resolve a refusal.
	billsTimeout = 20 * time.Second

	// billsScanLimit is how far back the resolution reads. The vendor returns
	// the ledger newest-first, so the 200 most recent rows cover far more than
	// the current day even for an account that talks a lot.
	billsScanLimit = 200
)

// checkinDayLocation is the calendar the vendor grants on. Every ledger
// timestamp the vendor emits carries a +08:00 offset, so the day boundary is
// Asia/Shanghai regardless of where the gateway host is.
var checkinDayLocation = time.FixedZone("CST", 8*60*60)

// pointsGrantReply is the body of the desktop points-grant endpoint. The
// envelope is already unwrapped by Client.do, so this is the `data` object.
//
// `popup` is kept as a raw map: the panel can show whatever the vendor put
// there, and this module does not pretend to know its schema.
type pointsGrantReply struct {
	Granted bool           `json:"granted"`
	Popup   map[string]any `json:"popup"`
}

// ---- CheckinProvider -------------------------------------------------------

// CheckinActions advertises the desktop login reward, but only while at least
// one account exists. Offering a button that cannot work would be a lie the
// operator pays for with a failed request.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.pool == nil || c.pool.len() == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionLoginPoints,
		Label: "领取每日 300 积分",
		Help:  "领取本账号当天 300 桌面登录积分（POST /api/web/desktop/v1/login/points/grant）。服务端每天发放一次；重复请求或不在发放时段会返回 granted:false，本模块把它记成拒绝而不是错误。",
	}}
}

// Checkin asks the vendor to grant the desktop login points for one account.
//
// Everything the upstream says — not granted, token rejected, contention —
// comes back as a CheckinResult with OK=false and a nil error. A Go error is
// reserved for "this could not even be attempted".
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := c.now()
	res := core.CheckinResult{AccountID: id}

	if a := strings.TrimSpace(action); a != "" && a != checkinActionLoginPoints {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, checkinActionLoginPoints)
		return c.finishCheckin(res, start), nil
	}
	res.Action = checkinActionLoginPoints

	e, err := c.balanceAccount(id)
	if err != nil {
		// "No account at all" is a fact the operator can see in the panel, not
		// a failed request; it must still never be reported as a grant.
		if errors.Is(err, core.ErrNotConfigured) {
			res.Error = err.Error()
			return c.finishCheckin(res, start), nil
		}
		return res, err
	}
	if e.acct.Disabled {
		res.Error = "the account is parked; enable it before claiming a reward"
		return c.finishCheckin(res, start), nil
	}

	ctx, cancel := withTimeout(ctx, pointsGrantTimeout)
	defer cancel()

	// Renew first when the credential is due: a grant fired with a stale token
	// would park a healthy account over a refresh we could have done.
	c.ensureFresh(ctx, e)

	var reply pointsGrantReply
	if err := c.do(ctx, http.MethodPost, pathPointsGrant,
		raccoonHeaders(e.acct.cred(), c.cfg.platform(), c.cfg.version()), nil, &reply); err != nil {
		kind := classifyErr(err)
		msg := truncate(c.scrub(err.Error()), 240)
		if kind == kindAuth {
			c.pool.markDead(e, msg)
		} else {
			c.pool.markFailure(e, kind, msg)
		}
		c.noteFailure(err)
		res.Error = fmt.Sprintf("could not ask for the desktop login points: %s", msg)
		res.Data = map[string]any{"granted": false, "error_kind": string(kind)}
		return c.finishCheckin(res, start), nil
	}
	c.noteUpstreamOK()
	c.pool.markUsed(e)

	res.Data = map[string]any{"granted": reply.Granted}
	if len(reply.Popup) > 0 {
		res.Data["popup"] = reply.Popup
	}

	if !reply.Granted {
		return c.resolveRefusedGrant(ctx, res, e, start), nil
	}

	res.OK = true
	res.Message = "the vendor granted the daily 300 login points"
	if reply.Popup != nil {
		res.Message += " (the reply carried a popup; see data.popup)"
	}
	return c.finishCheckin(res, start), nil
}

// resolveRefusedGrant decides what a `granted:false` reply actually means.
//
// The endpoint only EXTENDS today's 300-point allowance, so a refusal inside
// the day is not evidence that today's allowance is spent -- see the file
// header. Latching it as a day-level "unavailable" disables the row until the
// next Shanghai midnight and loses the reward for every remaining hour.
//
// The ledger is the authority, so ask it. A `daily_grant` stamped for today
// whose expiry reaches beyond today is a completed extension: that IS the
// "already claimed today" answer the panel wants to show. Anything else is a
// refusal the operator may retry, and it must stay retryable rather than
// being recorded as a settled no.
func (c *Client) resolveRefusedGrant(ctx context.Context, res core.CheckinResult, e *entry, start time.Time) core.CheckinResult {
	if inHand, err := c.dailyRewardInHand(ctx, e); err == nil && inHand {
		res.OK = true
		res.Skipped = false
		res.Message = "the vendor already extended today's 300 daily login points"
		res.Data["already_done"] = true
		return c.finishCheckin(res, start)
	}
	// Either the ledger holds no completed extension for today, or it could
	// not be read. Both mean "not settled": the operator may try again later
	// in the day, which is exactly how the vendor's own client behaves when
	// its mount-time grant is refused.
	res.Skipped = false
	res.Error = "the vendor did not grant the daily 300 login points this time (granted:false); this is not final for the day -- the vendor also extends the day's 300 points on later sign-ins, so retry later"
	return c.finishCheckin(res, start)
}

// dailyRewardInHand reports whether today's 300-point allowance has already
// been extended past today.
//
// The discriminator is the expiry, not the row's existence. The vendor issues
// the base allowance at 00:00 with an expiry of the SAME day (and clears it at
// 24:00), while the desktop reward pushes the expiry out to seven days. Only
// the latter means "today's reward is in hand".
func (c *Client) dailyRewardInHand(ctx context.Context, e *entry) (bool, error) {
	ctx, cancel := withTimeout(ctx, billsTimeout)
	defer cancel()

	items, err := c.fetchBills(ctx, e.acct.cred(), billsScanLimit, 0)
	if err != nil {
		return false, err
	}
	return dailyExtensionToday(items, c.now()), nil
}

// dailyExtensionToday is the ledger predicate dailyRewardInHand wraps. It is
// separated out so the rule can be pinned against a literal vendor ledger
// without a server: grant day is today (in CST) and expire day is after today.
func dailyExtensionToday(items []billItem, now time.Time) bool {
	today := now.In(checkinDayLocation).Format("2006-01-02")
	for _, it := range items {
		if it.BizType != "daily_grant" {
			continue
		}
		granted := parseBillTime(it.GrantAt)
		if granted.IsZero() {
			// Some rows carry only created_at; fall back to it before giving
			// up, because an unparsed row must not read as "not in hand".
			granted = parseBillTime(it.CreatedAt)
		}
		if granted.IsZero() || granted.In(checkinDayLocation).Format("2006-01-02") != today {
			continue
		}
		expires := parseBillTime(it.ExpireAt)
		if expires.IsZero() {
			continue
		}
		if expires.In(checkinDayLocation).Format("2006-01-02") > today {
			return true
		}
	}
	return false
}

// parseBillTime parses the RFC3339 timestamps the ledger carries. A malformed
// value yields the zero time so the caller can skip the row.
func parseBillTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return time.Time{}
	}
	return t
}

// finishCheckin stamps the elapsed time and the local clock reading.
func (c *Client) finishCheckin(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = c.now().Sub(start).Milliseconds()
	res.At = c.now().Format(time.RFC3339)
	return res
}

// Compile-time proof that this module satisfies the optional contract.
var _ core.CheckinProvider = (*Client)(nil)
