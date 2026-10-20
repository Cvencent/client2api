package trae

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// checkin.go implements the daily credit check-in for Trae CN.
//
// The reference implementation (trae2api-web) drives this as status-then-claim:
// ask /checkin_credits/status, and only post /checkin_credits/claim when the
// account is enabled and has not checked in yet.  "Already done today" is the
// `checked_in` boolean in the status body, not an error code, so there is no
// idempotency code to special-case here.
//
// It speaks the light `ug` header set (client UA + Cloud-IDE-JWT + region +
// device id) against api.trae.cn.  The chat path's SOLO fingerprint headers are
// NOT used: they are a different identity for a different host.

const (
	// ugCheckinStatusPath and ugCheckinClaimPath are the two halves of the
	// daily reward.
	ugCheckinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	ugCheckinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"

	// The 1024 lottery reuses the activity centre rather than the check-in
	// endpoints. activity/info is the read side; activity/action is the only
	// mutating call and is never retried.
	ugActivityInfoPath   = "/trae/api/v2/ug/activity/info"
	ugActivityActionPath = "/trae/api/v2/ug/activity/action"
	ugCheckinLotteryID   = "checkin_1024"

	// checkinActionUG performs the daily check-in and, when the vendor exposes
	// the 1024 lottery for this account, the one attached draw. checkinActionLottery
	// is the explicit recovery entry for accounts that signed in today before
	// this gateway learned about the lottery.
	checkinActionUG      = "daily-checkin"
	checkinActionLottery = "checkin-lottery-1024"

	// ugRegion is the region the check-in host speaks for.  api.trae.cn is the
	// CN endpoint, so an account pinned to another region is not offered it.
	ugRegion = "CN"

	// ugUserAgentPrefix is prefixed with the configured IDE version, giving the
	// "Trae/0.1.52" the reference sends.
	ugUserAgentPrefix = "Trae/"

	// checkinTimeout bounds one check-in round trip. The combined path is at
	// most four short JSON calls: status, claim, activity info and action.
	checkinTimeout = 45 * time.Second
)

// ugCheckinReply is the response of both endpoints.
//
// The upstream has been observed in two shapes: the state fields flat at the
// top level, and the same fields wrapped in "data".  Both are decoded and
// folded by state(), so a change of wrapping on the wire cannot silently read
// as "not checked in".
type ugCheckinReply struct {
	Code    int64  `json:"code"`
	Msg     string `json:"msg"`
	Message string `json:"message"`

	CheckedIn bool  `json:"checked_in"`
	Credits   int64 `json:"credits"`
	Enable    bool  `json:"enable"`

	Data *ugCheckinReply `json:"data"`
}

// ugActivityInfoReply is the activity-centre snapshot. The common 1024
// response wraps commercial_activities in data; accepting both the flat and
// wrapped shapes keeps a server-side wrapping change from looking like
// "activity not enabled".
type ugActivityInfoReply struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    *struct {
		CommercialActivities []ugCommercialActivity `json:"commercial_activities"`
	} `json:"data"`
	CommercialActivities []ugCommercialActivity `json:"commercial_activities"`
}

func (r ugActivityInfoReply) activities() []ugCommercialActivity {
	if r.Data != nil && r.Data.CommercialActivities != nil {
		return r.Data.CommercialActivities
	}
	return r.CommercialActivities
}

type ugWorkExtra struct {
	Eligible  *bool  `json:"eligible"`
	Grantable *bool  `json:"grantable"`
	Granted   *bool  `json:"granted"`
	PrizeType int    `json:"prize_type"`
	PrizeText string `json:"prize_text"`
	Credits   int64  `json:"general_credits"`
	CouponFen int64  `json:"coupon_face_amount_fen"`
}

type ugCommercialActivity struct {
	ActivityID string       `json:"activity_id"`
	Enabled    *bool        `json:"Enabled"`
	WorkExtra  *ugWorkExtra `json:"work_extra"`
}

// ugLotteryActionReply mirrors the action envelope. work_extra repeats the
// draw result, and code 9102 means "sign in first"; both are treated as
// answers, not transport errors.
type ugLotteryActionReply struct {
	Code      int64        `json:"code"`
	Message   string       `json:"message"`
	WorkExtra *ugWorkExtra `json:"work_extra"`
	Data      *struct {
		Code      int64        `json:"code"`
		Message   string       `json:"message"`
		WorkExtra *ugWorkExtra `json:"work_extra"`
		Granted   *bool        `json:"granted"`
		PrizeType int          `json:"prize_type"`
		PrizeText string       `json:"prize_text"`
		Credits   int64        `json:"general_credits"`
		CouponFen int64        `json:"coupon_face_amount_fen"`
	} `json:"data"`
}

func (r ugLotteryActionReply) effectiveCode() int64 {
	if r.Code != 0 {
		return r.Code
	}
	if r.Data != nil {
		return r.Data.Code
	}
	return 0
}

func (r ugLotteryActionReply) effectiveMessage() string {
	if strings.TrimSpace(r.Message) != "" {
		return r.Message
	}
	if r.Data != nil {
		return r.Data.Message
	}
	return ""
}

func (r ugLotteryActionReply) effectiveWorkExtra() *ugWorkExtra {
	if r.Data != nil && r.Data.WorkExtra != nil {
		return r.Data.WorkExtra
	}
	if r.WorkExtra != nil {
		return r.WorkExtra
	}
	if r.Data != nil && r.Data.Granted != nil {
		return &ugWorkExtra{
			Granted:   r.Data.Granted,
			PrizeType: r.Data.PrizeType,
			PrizeText: r.Data.PrizeText,
			Credits:   r.Data.Credits,
			CouponFen: r.Data.CouponFen,
		}
	}
	return nil
}

// state folds the flat and wrapped shapes.  The nested copy wins when present.
func (r ugCheckinReply) state() ugCheckinReply {
	if r.Data != nil {
		return *r.Data
	}
	return r
}

// message is the most human-readable text the reply carries.
func (r ugCheckinReply) message() string {
	if s := strings.TrimSpace(r.Msg); s != "" {
		return s
	}
	return strings.TrimSpace(r.Message)
}

// ---- eligibility -----------------------------------------------------------

// checkinEligible reports whether an account may use the CN check-in host.
// Region is authoritative when the credential carried one; otherwise the
// product directory name decides, because the international build is "Trae"
// and the CN builds are "Trae CN" / "TRAE SOLO CN".
func checkinEligible(a *Auth) bool {
	if a == nil {
		return false
	}
	if r := strings.ToUpper(strings.TrimSpace(a.Region)); r != "" {
		return strings.HasPrefix(r, ugRegion)
	}
	return !strings.EqualFold(strings.TrimSpace(a.Product), "Trae")
}

// parkedAccount reports whether the panel id names an account the operator
// switched off.  Such an account is a result, not an error: the operator can
// see it and turn it back on.
func (c *Client) parkedAccount(id string) bool {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	if c.store == nil {
		return false
	}
	for _, sa := range c.store.Accounts {
		if sa.ID == id && !sa.Enabled {
			return true
		}
	}
	return c.store.suppressed(id)
}

// ---- CheckinProvider -------------------------------------------------------

// CheckinActions advertises the daily check-in, but only when at least one
// eligible account exists.  Offering a button that cannot work would be a lie
// the operator pays for with a failed request.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	for _, a := range c.pool.Accounts() {
		if checkinEligible(a) {
			return []core.CheckinAction{
				{
					ID:    checkinActionUG,
					Label: "领取每日签到积分",
					Help:  "Asks api.trae.cn whether today's credits were claimed, claims them if not, and participates in the attached 1024 lottery when the vendor marks it grantable. CN accounts only.",
				},
				{
					ID:    checkinActionLottery,
					Label: "参与 1024 抽奖",
					Help:  "Reads the activity state and performs today's checkin_1024 draw without claiming check-in credits again. Use this for an account that was signed in before the lottery was wired in. CN accounts only.",
				},
			}
		}
	}
	return nil
}

// Checkin claims the daily credits for one account.
//
// Everything the upstream says — already claimed, disabled, token rejected,
// contention — comes back as a CheckinResult with OK=false and a nil error.
// A Go error is reserved for "this could not even be attempted".
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := c.now()
	res := core.CheckinResult{AccountID: id}

	action = strings.TrimSpace(action)
	if action != "" && action != checkinActionUG && action != checkinActionLottery {
		res.Error = fmt.Sprintf("unknown action %q; this client offers %q and %q", action, checkinActionUG, checkinActionLottery)
		return c.finish(res, start), nil
	}
	if action == "" {
		action = checkinActionUG
	}
	res.Action = action

	a := c.findAuth(id)
	if a == nil {
		if c.parkedAccount(id) {
			res.Error = "the account is parked; enable it before claiming a reward"
			return c.finish(res, start), nil
		}
		return res, fmt.Errorf("account %q not found", id)
	}
	if !checkinEligible(a) {
		res.Error = fmt.Sprintf("daily check-in is only available to %s accounts; this one is %q",
			ugRegion, firstNonEmpty(a.Region, a.Product, "unknown"))
		return c.finish(res, start), nil
	}

	ctx, cancel := context.WithTimeout(ctx, checkinTimeout)
	defer cancel()

	// Step 1: ask.  Never claim blind — a second claim for the same day is the
	// one request that could look abusive to the upstream.
	var status ugCheckinReply
	if err := c.doJSON(ctx, c.cfg.authHost()+ugCheckinStatusPath, c.ugHeaders(a), []byte("{}"), &status); err != nil {
		return c.upstreamFailure(a, res, start, "read the check-in status", err), nil
	}
	st := status.state()
	if status.Code != 0 && !st.Enable {
		// The status endpoint reports auth rejections as HTTP 200 with
		// code=1001 and enable=false. Never interpret that as campaign
		// ineligibility, or the panel would permanently grey the row for a
		// transient session failure.
		return c.upstreamFailure(a, res, start, "read the check-in status", &Error{Kind: ErrAuth, Code: status.Code, Msg: status.message()}), nil
	}
	if !st.Enable {
		res.Skipped = true
		res.Error = "daily check-in is not enabled for this account"
		if msg := status.message(); msg != "" {
			res.Error += ": " + msg
		}
		return c.finish(res, start), nil
	}
	claimed := false
	credits := st.Credits
	if !st.CheckedIn {
		// Step 2: claim. Never claim blind — a second claim for the same day
		// is the one request that could look abusive to the upstream.
		var claim ugCheckinReply
		if err := c.doJSON(ctx, c.cfg.authHost()+ugCheckinClaimPath, c.ugHeaders(a), []byte("{}"), &claim); err != nil {
			return c.upstreamFailure(a, res, start, "claim the check-in credits", err), nil
		}
		cl := claim.state()
		claimed = true
		credits = cl.Credits
		if credits == 0 {
			credits = st.Credits
		}
		res.Message = firstNonEmpty(claim.message(), status.message(), "checked in")
		res.Code = int(claim.Code)
		c.log("checkin %s: claimed %d credit(s)", a.Masked(), cl.Credits)
	} else {
		res.Message = firstNonEmpty(status.message(), "already checked in today")
		res.Code = int(status.Code)
	}

	// Step 3: the 1024 draw is attached to the signed state. The dedicated
	// action can also use this branch to recover a draw missed by an older
	// build; the ordinary action still signs in first so one click does both.
	lottery, lotteryErr := c.participateCheckinLottery(ctx, a)
	if lotteryErr != nil {
		// A draw failure must not turn a successful sign-in into a retryable
		// check-in failure: retrying would try to claim the same day again.
		// A dedicated lottery click reports the failure directly instead.
		if action == checkinActionLottery {
			return c.upstreamFailure(a, res, start, "participate in the 1024 lottery", lotteryErr), nil
		}
		c.log("checkin %s: lottery failed after sign-in: %v", a.Masked(), lotteryErr)
	}

	if action == checkinActionLottery && lotteryErr == nil {
		res.Data = lottery
		state, _ := lottery["state"].(string)
		switch state {
		case "result", "granted":
			res.OK = true
			res.Message = firstNonEmpty(prizeMessage(lottery), "1024 lottery completed")
		case "unavailable":
			res.Skipped = true
			res.Error = firstNonEmpty(stringValue(lottery["reason"]), "the 1024 lottery is not available for this account")
		default:
			res.Error = firstNonEmpty(stringValue(lottery["message"]), "the 1024 lottery result is not confirmed; try again later")
		}
		if res.OK {
			c.pool.MarkSuccess(a)
		}
		return c.finish(res, start), nil
	}

	res.OK = true
	res.Data = map[string]any{
		"already_done": !claimed,
		"credits":      credits,
	}
	if lotteryErr == nil {
		res.Data["lottery"] = lottery
	}
	c.pool.MarkSuccess(a)
	return c.finish(res, start), nil
}

func stringValue(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func prizeMessage(lottery map[string]any) string {
	if s := stringValue(lottery["prize_text"]); s != "" {
		return s
	}
	if n, ok := lottery["credits"].(int64); ok && n > 0 {
		return fmt.Sprintf("won %d credits", n)
	}
	return ""
}

// lotteryInfoBody mirrors the activity-centre version_code field without
// baking the client's release version into this module.
func lotteryInfoBody(version string) []byte {
	return []byte(fmt.Sprintf(`{"version_code":%q,"req_source":1}`, strings.TrimSpace(version)))
}

// participateCheckinLottery performs the official client's post-check-in 1024
// draw. It deliberately reads the activity state first: the action endpoint
// mutates the account, and the client only calls it when enabled + eligible +
// grantable + not granted. A missing activity is a normal "not applicable"
// answer for every account outside the promotion.
func (c *Client) participateCheckinLottery(ctx context.Context, a *Auth) (map[string]any, error) {
	var info ugActivityInfoReply
	if err := c.doJSON(ctx, c.cfg.authHost()+ugActivityInfoPath, c.ugHeaders(a), lotteryInfoBody(c.cfg.appVersion()), &info); err != nil {
		return nil, err
	}
	activity := findUGActivity(info.activities(), ugCheckinLotteryID)
	if activity == nil || activity.Enabled == nil || !*activity.Enabled {
		return map[string]any{"state": "unavailable", "reason": "activity is not enabled for this account"}, nil
	}
	we := activity.WorkExtra
	if we == nil {
		return map[string]any{"state": "unknown", "reason": "activity has no work_extra"}, nil
	}
	if we.Granted != nil && *we.Granted {
		return ugLotteryResult(we, "granted"), nil
	}
	if we.Eligible == nil || we.Grantable == nil || we.Granted == nil {
		return map[string]any{"state": "unknown", "reason": "activity state is incomplete"}, nil
	}
	if !*we.Eligible || !*we.Grantable {
		reason := "the vendor marks this account as not eligible for the draw"
		if *we.Eligible && !*we.Grantable {
			reason = "the vendor has not made today's draw grantable for this account"
		}
		return map[string]any{"state": "unavailable", "reason": reason, "eligible": *we.Eligible, "grantable": *we.Grantable}, nil
	}

	var action ugLotteryActionReply
	body := []byte(`{"activity_id":"checkin_1024","req_source":1}`)
	if err := c.doJSON(ctx, c.cfg.authHost()+ugActivityActionPath, c.ugHeaders(a), body, &action); err != nil {
		return nil, err
	}
	if action.effectiveCode() == 9102 {
		return map[string]any{"state": "not_signed", "code": action.effectiveCode(), "message": action.effectiveMessage()}, nil
	}
	if we := action.effectiveWorkExtra(); we != nil && we.Granted != nil && *we.Granted {
		return ugLotteryResult(we, "result"), nil
	}
	return map[string]any{"state": "unconfirmed", "code": action.effectiveCode(), "message": action.effectiveMessage()}, nil
}

func findUGActivity(list []ugCommercialActivity, id string) *ugCommercialActivity {
	for i := range list {
		if strings.EqualFold(strings.TrimSpace(list[i].ActivityID), id) {
			return &list[i]
		}
	}
	return nil
}

func ugLotteryResult(we *ugWorkExtra, state string) map[string]any {
	out := map[string]any{"state": state}
	if we == nil {
		return out
	}
	if we.PrizeText != "" {
		out["prize_text"] = we.PrizeText
	}
	if we.PrizeType != 0 {
		out["prize_type"] = we.PrizeType
	}
	if we.Credits > 0 {
		out["credits"] = we.Credits
	}
	if we.CouponFen > 0 {
		out["coupon_face_amount_fen"] = we.CouponFen
	}
	return out
}

// upstreamFailure turns a failed round trip into a result.  It also teaches the
// pool about the credential, so a rejected token is cooled down (or parked)
// instead of being retried on every panel click.
func (c *Client) upstreamFailure(a *Auth, res core.CheckinResult, start time.Time, what string, err error) core.CheckinResult {
	kind, cooldown := c.pool.MarkFailure(a, err)
	res.Error = fmt.Sprintf("could not %s: %s", what, err.Error())
	if cooldown > 0 {
		res.Error += fmt.Sprintf(" (cooling down for %s after a %s failure)", cooldown.Round(time.Second), kind)
	}
	res.Data = map[string]any{"error_kind": kind.String()}
	c.noteErr(err)
	return c.finish(res, start)
}

// finish stamps the elapsed time and the local clock reading.
func (c *Client) finish(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = c.now().Sub(start).Milliseconds()
	res.At = c.now().Format(time.RFC3339)
	return res
}

// ---- headers ---------------------------------------------------------------

// ugHeaders is the light identity the check-in host expects: the client user
// agent, the Cloud-IDE-JWT bearer, the CN region and — when the credential
// carried one — the device id.  It deliberately does not include the SOLO
// fingerprint set used by the chat path.
func (c *Client) ugHeaders(a *Auth) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", ugUserAgentPrefix+c.cfg.ideVersion())
	h.Set("Authorization", "Cloud-IDE-JWT "+a.Token())
	h.Set("X-User-Region", ugRegion)
	if id := strings.TrimSpace(a.DeviceID); id != "" {
		h.Set("X-Device-Id", id)
	}
	return h
}

// Compile-time proof that this module satisfies the optional contract.
var _ core.CheckinProvider = (*Client)(nil)
