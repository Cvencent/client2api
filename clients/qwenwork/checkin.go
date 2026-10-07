package qwenwork

// checkin.go implements the daily check-in ("每日签到") that QwenWork's own
// desktop client offers through its "credits growth card".
//
// # Where the wire contract comes from
//
// The vendor keeps the whole feature in one place — the QwenWorkCN desktop
// bundle's out/main/main.js, in the createCategoryLogger("ConfigService")
// block:
//
//	DAILY_CHECK_IN_STATUS_PATH = "/sash/api/v1/me/daily-check-in/status"
//	DAILY_CHECK_IN_CLAIM_PATH  = "/sash/api/v1/me/daily-check-in/claim"
//	FETCH_TIMEOUT_MS           = 8e3
//	headers  Accept/Content-Type: application/json, User-Agent: "QoderWork",
//	         Authorization: Bearer <access token>
//	base     getSashApiBaseUrl(): https://<OPENAPI_DOMAIN> in production,
//	         i.e. gateway.qwenwork.cn, overridable through SASH_API_BASE_URL or
//	         CREDITS_GROWTH_CARD_API_BASE_URL in ~/.qodercn/.env
//
// gateway.qwenwork.cn is the very host this module already talks to for chat
// (cosy.go's defaultBaseURL), so both paths below go through cfg.endpoint() and
// an operator's own base_url is honoured.
//
// The vendor wraps both replies in an envelope and unwraps it with
// unwrapData(): when the body is an object whose "data" member is also an
// object, the payload is that member; otherwise the body is the payload.  Its
// own parsers then read:
//
//	status  -> CLAIMABLE | CLAIMED_TODAY | DISABLED | NOT_STARTED | ENDED
//	           plus rewardCredits, nextClaimAt, currentStreakDays,
//	           totalClaimDays, totalRewardCredits, lastClaimedAt,
//	           rewardExpiresAt
//	claim   -> CLAIMED | ALREADY_CLAIMED, plus rewardCredits, expiresAt
//
// # What is deliberately not copied
//
// The vendor also sends six X-QwenWork-* headers naming its app version, build
// number, platform, architecture and release channel.  We are not that desktop
// client and we do not know those values, so this module sends none of them:
// inventing a version number to look like somebody else's binary would put a
// lie in the request, and nothing in the vendor's code suggests the endpoint
// needs them.
//
// # Honest limits
//
//   - The status path is a read; the claim path is the only write, and it is
//     fired exactly once per Checkin call.
//   - The vendor only offers the button when the server has pushed the
//     "announcement:credits-growth-card" operation with checkinEnabled true.
//     On this machine the server has never pushed it (the desktop log repeats
//     `Dispatch null on initial sync ... announcement:credits-growth-card`),
//     so a live 404 is the expected answer here and the success path has not
//     been observed against a real account.  The shapes above come from the
//     vendor's own parsers, never from guesswork: an unrecognised status or
//     result is reported as unrecognised instead of being forced into one of
//     the known values.
//   - There is no "already claimed" error code.  The vendor signals it with
//     status CLAIMED_TODAY / result ALREADY_CLAIMED, which is a success, so it
//     is reported as ok=true with data["already_done"]=true.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// checkinActionDaily is the only action this client offers.  One action is
	// the honest shape: the vendor has exactly one daily check-in.
	checkinActionDaily = "daily"

	checkinStatusPath = "/sash/api/v1/me/daily-check-in/status"
	checkinClaimPath  = "/sash/api/v1/me/daily-check-in/claim"

	// checkinUserAgent is the exact value the vendor's own check-in client
	// sends.  It is deliberately not cfg.userAgent(): this endpoint belongs to
	// the credits-growth-card service, not to the chat gateway, and the vendor
	// hard-codes this string for it.
	checkinUserAgent = "QoderWork"

	// checkinTimeout is more generous than the vendor's 8 s.  A check-in is a
	// once-a-day click, so waiting a little longer is better than reporting a
	// timeout the operator would have to retry.
	checkinTimeout = 20 * time.Second
)

// The daily check-in status values the vendor's own client recognises.
const (
	checkinStatusClaimable    = "CLAIMABLE"
	checkinStatusClaimedToday = "CLAIMED_TODAY"
	checkinStatusDisabled     = "DISABLED"
	checkinStatusNotStarted   = "NOT_STARTED"
	checkinStatusEnded        = "ENDED"
)

// The claim results the vendor's own client recognises.
const (
	checkinResultClaimed        = "CLAIMED"
	checkinResultAlreadyClaimed = "ALREADY_CLAIMED"
)

// checkinStatus is the subset of the status payload worth carrying to the
// operator.  Every numeric field is a pointer so "the vendor did not send it"
// stays distinguishable from a real zero.
type checkinStatus struct {
	Status             string   `json:"status"`
	RewardCredits      *float64 `json:"rewardCredits"`
	NextClaimAt        *int64   `json:"nextClaimAt"`
	CurrentStreakDays  *int64   `json:"currentStreakDays"`
	TotalClaimDays     *int64   `json:"totalClaimDays"`
	TotalRewardCredits *float64 `json:"totalRewardCredits"`
	LastClaimedAt      *int64   `json:"lastClaimedAt"`
	RewardExpiresAt    *int64   `json:"rewardExpiresAt"`
}

// checkinClaim is the claim payload.
type checkinClaim struct {
	Result        string   `json:"result"`
	RewardCredits *float64 `json:"rewardCredits"`
	ExpiresAt     *int64   `json:"expiresAt"`
}

// checkinAuthError is the "the access token was rejected and the refresh token
// could not replace it" verdict.  It is not an upstream HTTP verdict, so it
// needs a type of its own to reach checkinFail as something other than a local
// failure; by the time it is built the account has already been marked dead.
type checkinAuthError struct{ msg string }

func (e *checkinAuthError) Error() string { return e.msg }

// CheckinActions reports the actions this client offers.  It returns nothing
// when the pool is empty: a button that cannot do anything is worse than no
// button, because it makes the operator pay for the discovery with a failed
// request.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.pool == nil || c.pool.len() == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionDaily,
		Label: "每日签到",
		Help:  "读取厂商的签到状态，可领取时立即领取。厂商返回「今天已签到」算成功，不算失败。",
	}}
}

// Checkin reads the vendor's check-in status and claims the reward when the
// vendor says one is waiting.
//
// The two-step shape is deliberate.  Claiming blind would make every click a
// write, and the vendor has no idempotency key on this endpoint; asking first
// means a repeat click on an already-claimed day costs one read and no write.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := time.Now()
	res := core.CheckinResult{AccountID: strings.TrimSpace(id)}

	if a := strings.TrimSpace(action); a != "" && a != checkinActionDaily {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, checkinActionDaily)
		return c.finishCheckin(res, start), nil
	}
	res.Action = checkinActionDaily

	e, err := c.checkinAccount(id)
	if err != nil {
		if errors.Is(err, core.ErrNotConfigured) {
			res.Error = err.Error()
			return c.finishCheckin(res, start), nil
		}
		// An unknown account id is a caller mistake, not a vendor refusal.
		return res, err
	}
	res.AccountID = e.acct.id()

	if e.acct.Disabled {
		res.Error = "the account is parked; enable it before checking in"
		return c.finishCheckin(res, start), nil
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, checkinTimeout)
	defer cancel()

	// Renew first when the credential is already due.  A check-in fired with a
	// token we knew was about to expire would park a healthy account over a
	// refresh we could have done ourselves.
	if e.acct.needsRefresh(time.Now(), c.cfg.refreshMargin()) {
		_ = c.tryRefresh(ctx, e)
	}

	var rawStatus json.RawMessage
	if err := c.checkinDo(ctx, e, http.MethodGet, checkinStatusPath, &rawStatus); err != nil {
		if !c.checkinFail(&res, e, err, "read the daily check-in status") {
			return res, err
		}
		return c.finishCheckin(res, start), nil
	}
	c.noteUpstream(true, kindNone, "")
	c.pool.markUsed(e)

	st, ok := decodeCheckinStatus(rawStatus)
	if !ok {
		res.Error = "the vendor's check-in status reply carried no readable payload"
		return c.finishCheckin(res, start), nil
	}
	res.Data = checkinStatusData(st)

	switch st.Status {
	case checkinStatusClaimedToday:
		res.OK = true
		res.Message = "already checked in today"
		res.Data["already_done"] = true
		return c.finishCheckin(res, start), nil
	case checkinStatusClaimable:
		// The only branch that writes.
	case checkinStatusDisabled:
		res.Error = "the vendor has the daily check-in switched off for this account (status=DISABLED)"
		return c.finishCheckin(res, start), nil
	case checkinStatusNotStarted:
		res.Error = "the check-in campaign has not started yet (status=NOT_STARTED)"
		return c.finishCheckin(res, start), nil
	case checkinStatusEnded:
		res.Error = "the check-in campaign has ended (status=ENDED)"
		return c.finishCheckin(res, start), nil
	default:
		res.Error = fmt.Sprintf("the vendor returned an unrecognised check-in status %q", st.Status)
		return c.finishCheckin(res, start), nil
	}

	var rawClaim json.RawMessage
	if err := c.checkinDo(ctx, e, http.MethodPost, checkinClaimPath, &rawClaim); err != nil {
		if !c.checkinFail(&res, e, err, "claim the daily check-in") {
			return res, err
		}
		return c.finishCheckin(res, start), nil
	}
	c.noteUpstream(true, kindNone, "")
	c.pool.markUsed(e)

	claim, ok := decodeCheckinClaim(rawClaim)
	if !ok {
		res.Error = "the vendor's check-in claim reply carried no readable payload"
		return c.finishCheckin(res, start), nil
	}
	res.Data["result"] = claim.Result
	if claim.RewardCredits != nil {
		res.Data["claimed_credits"] = *claim.RewardCredits
	}
	if claim.ExpiresAt != nil {
		res.Data["credits_expire_at"] = *claim.ExpiresAt
	}

	switch claim.Result {
	case checkinResultClaimed:
		res.OK = true
		res.Message = "checked in"
		if claim.RewardCredits != nil {
			res.Message = fmt.Sprintf("checked in for %g credits", *claim.RewardCredits)
		}
	case checkinResultAlreadyClaimed:
		res.OK = true
		res.Message = "already checked in today"
		res.Data["already_done"] = true
	default:
		res.Error = fmt.Sprintf("the vendor returned an unrecognised check-in result %q", claim.Result)
	}
	return c.finishCheckin(res, start), nil
}

// checkinAccount resolves the account a check-in applies to.
//
// An empty id means "whichever account is ready", which is what a caller with
// no account selected sends; an id that is not in the pool is a caller mistake
// and stays a real error.  A pool with nothing in it is not a mistake — it is
// an unconfigured client, and core.ErrNotConfigured is how this module says so
// everywhere else.
func (c *Client) checkinAccount(id string) (*entry, error) {
	if c.pool == nil || c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}
	if strings.TrimSpace(id) == "" {
		e := c.pool.pick(nil)
		if e == nil {
			return nil, fmt.Errorf("qwenwork: no account is usable for the daily check-in (%s)", c.pool.summary())
		}
		return e, nil
	}
	e := c.pool.find(id)
	if e == nil {
		return nil, fmt.Errorf("account %q not found", id)
	}
	return e, nil
}

// checkinDo performs one check-in request.
//
// When the vendor rejects the access token and the account still has a refresh
// token, the token is renewed and the request is sent once more, so a stale
// token never costs the operator a click.  A renewal that fails too means the
// credential is genuinely dead: the account is marked dead here and the caller
// is handed a checkinAuthError explaining that, rather than the 401 that merely
// revealed it.
func (c *Client) checkinDo(ctx context.Context, e *entry, method, path string, out any) error {
	url := c.cfg.endpoint(path)
	err := c.doJSON(ctx, method, url, "", checkinHeaders(c.pool.accountOf(e)), out)
	if err == nil {
		return nil
	}
	ue, ok := asUpstreamError(err)
	if !ok || ue.Kind != kindAuth || c.pool.refreshTokenOf(e) == "" {
		return err
	}
	if refreshErr := c.tryRefresh(ctx, e); refreshErr != nil {
		msg := cleanErrorText(refreshErr.Error())
		c.noteUpstream(false, kindAuth, msg)
		c.pool.markDead(e, msg)
		return &checkinAuthError{msg: msg}
	}
	return c.doJSON(ctx, method, url, "", checkinHeaders(c.pool.accountOf(e)), out)
}

// checkinHeaders builds the request headers.  The vendor's own client sends
// these four plus six X-QwenWork-* identity headers; see the file comment for
// why we send only these.
func checkinHeaders(acct account) map[string]string {
	return map[string]string{
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    checkinUserAgent,
		"Authorization": "Bearer " + acct.AccessToken,
	}
}

// checkinFail applies the account-health policy to one failed check-in request
// and writes the operator-facing explanation into res.
//
// It returns false when the failure was not the upstream's verdict — a dead
// caller context or a local transport failure.  The caller must then return the
// raw error: no credential deserves a cooldown for our own problem, which is
// the same rule the chat path follows.
func (c *Client) checkinFail(res *core.CheckinResult, e *entry, err error, what string) bool {
	if res.Data == nil {
		res.Data = map[string]any{}
	}
	var dead *checkinAuthError
	if errors.As(err, &dead) {
		// Already marked dead by checkinDo; do not park it a second time.
		res.Error = fmt.Sprintf("could not %s: %s", what, dead.msg)
		res.Data["error_kind"] = kindAuth.String()
		return true
	}
	ue, ok := asUpstreamError(err)
	if !ok {
		return false
	}
	if ue.Status == http.StatusNotFound {
		res.OK = false
		res.Skipped = true
		res.Message = "the vendor has not enabled daily check-in for this account"
		res.Error = fmt.Sprintf("could not %s: %s", what, ue.Message)
		res.Data["error_kind"] = ue.Kind.String()
		return true
	}
	c.noteUpstream(false, ue.Kind, ue.Message)
	c.pool.markFailureWith(e, ue.Kind, ue.Message, ue.RetryAfter)
	res.Error = fmt.Sprintf("could not %s: %s", what, ue.Message)
	res.Data["error_kind"] = ue.Kind.String()
	return true
}

// checkinPayload unwraps the vendor's envelope exactly as the vendor's own
// unwrapData() does: when the body is an object and its "data" member is also
// an object, that member is the payload; otherwise the body is the payload.
// The bool is false when the body is not a JSON object at all.
func checkinPayload(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &outer); err != nil || outer == nil {
		return nil, false
	}
	if inner, ok := outer["data"]; ok {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(inner, &probe); err == nil && probe != nil {
			return inner, true
		}
	}
	return raw, true
}

func decodeCheckinStatus(raw json.RawMessage) (checkinStatus, bool) {
	payload, ok := checkinPayload(raw)
	if !ok {
		return checkinStatus{}, false
	}
	var st checkinStatus
	if err := json.Unmarshal(payload, &st); err != nil {
		return checkinStatus{}, false
	}
	return st, true
}

func decodeCheckinClaim(raw json.RawMessage) (checkinClaim, bool) {
	payload, ok := checkinPayload(raw)
	if !ok {
		return checkinClaim{}, false
	}
	var cl checkinClaim
	if err := json.Unmarshal(payload, &cl); err != nil {
		return checkinClaim{}, false
	}
	return cl, true
}

// checkinStatusData renders the status payload for the operator.  Only the
// fields the vendor actually sent appear, so a missing one reads as "not
// reported" rather than as a zero the vendor never claimed.
func checkinStatusData(st checkinStatus) map[string]any {
	data := map[string]any{"status": st.Status}
	if st.RewardCredits != nil {
		data["reward_credits"] = *st.RewardCredits
	}
	if st.NextClaimAt != nil {
		data["next_claim_at"] = *st.NextClaimAt
	}
	if st.CurrentStreakDays != nil {
		data["current_streak_days"] = *st.CurrentStreakDays
	}
	if st.TotalClaimDays != nil {
		data["total_claim_days"] = *st.TotalClaimDays
	}
	if st.TotalRewardCredits != nil {
		data["total_reward_credits"] = *st.TotalRewardCredits
	}
	if st.LastClaimedAt != nil {
		data["last_claimed_at"] = *st.LastClaimedAt
	}
	if st.RewardExpiresAt != nil {
		data["reward_expires_at"] = *st.RewardExpiresAt
	}
	return data
}

// finishCheckin stamps the two fields every result carries so the panel can
// show how long the click took and when it happened.
func (c *Client) finishCheckin(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.At = time.Now().Format(time.RFC3339)
	return res
}

var _ core.CheckinProvider = (*Client)(nil)
