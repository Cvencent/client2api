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
// **The vendor only hands this reward to its own desktop app.** What that app
// sends is a plain authenticated POST -- the same method, URL, headers and
// empty body this module sends -- but replaying it from the gateway is answered
// with `{"code":0,"data":{"granted":false}}` for every account and every
// variation tried (device id, client version, Origin, cookies, UA, org header,
// body shape). What gives the caller away is not the request: a real account's
// ledger shows the extension landing on exactly 10/08 18:41, 10/16 08:22,
// 10/20 18:12 and 10/23 21:41, each within seconds of that app's own
// `app-startup` log line; on 10/20 the gateway's 07:00 attempt was refused while
// the same account's 18:12 attempt -- fired by the app -- was granted. The
// discriminator is the caller, not the request shape and not the clock.
//
// `granted:false` is therefore the settled answer for a gateway call, not a
// transient one, and the module records it as UNAVAILABLE (Skipped) so the row
// greys out until the next Shanghai midnight instead of reading as a failure.
// The ledger is still consulted for the one benign reading of a refusal:
//
//	- a `daily_grant` for today whose expiry reaches past today proves the
//	  extension is already in hand   -> OK, Data["already_done"] = true
//	- no such row                     -> the desktop-only refusal -> Skipped
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
		Help:  "领取本账号当天 300 桌面登录积分（POST /api/web/desktop/v1/login/points/grant）。这条奖励厂商只发给它自己的桌面客户端：网关调用一律得到 granted:false，本模块记成「不可用（需桌面端登录领取）」，不再当失败。",
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
// See the file header: the vendor hands this reward to its own desktop app and
// to nobody else, so a refusal is the SETTLED answer for a gateway call, not a
// statement that can still change later in the day. The row is therefore
// recorded as unavailable -- the panel greys it out until the next Shanghai
// midnight -- with the operator told where the reward can actually be claimed.
//
// The ledger is still worth a look, because it separates the two readings of a
// refusal. A `daily_grant` stamped for today whose expiry reaches beyond today
// is a completed extension: the day's allowance is protected, which is the
// honest "already claimed" answer the panel wants to show. Anything else --
// no such row, or a ledger that cannot be read -- is the desktop-only refusal.
func (c *Client) resolveRefusedGrant(ctx context.Context, res core.CheckinResult, e *entry, start time.Time) core.CheckinResult {
	if inHand, err := c.dailyRewardInHand(ctx, e); err == nil && inHand {
		res.OK = true
		res.Skipped = false
		res.Message = "the vendor already extended today's 300 daily login points"
		res.Data["already_done"] = true
		return c.finishCheckin(res, start)
	}
	res.OK = false
	res.Skipped = true
	res.Error = ""
	res.Message = "厂商只把这条奖励发给它自己的桌面客户端，请在桌面端登录领取"
	res.Data["needs_desktop_client"] = true
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
