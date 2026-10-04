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

	// checkinActionUG is the single action this module offers.
	checkinActionUG = "daily-checkin"

	// ugRegion is the region the check-in host speaks for.  api.trae.cn is the
	// CN endpoint, so an account pinned to another region is not offered it.
	ugRegion = "CN"

	// ugUserAgentPrefix is prefixed with the configured IDE version, giving the
	// "Trae/0.1.52" the reference sends.
	ugUserAgentPrefix = "Trae/"

	// checkinTimeout bounds one check-in round trip.  The whole action is at
	// most two short JSON calls.
	checkinTimeout = 30 * time.Second
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
			return []core.CheckinAction{{
				ID:    checkinActionUG,
				Label: "领取每日签到积分",
				Help:  "Asks api.trae.cn whether today's credits were claimed and claims them if not. CN accounts only.",
			}}
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

	if a := strings.TrimSpace(action); a != "" && a != checkinActionUG {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, checkinActionUG)
		return c.finish(res, start), nil
	}
	res.Action = checkinActionUG

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
	if !st.Enable {
		res.Error = "daily check-in is not enabled for this account"
		if msg := status.message(); msg != "" {
			res.Error += ": " + msg
		}
		return c.finish(res, start), nil
	}
	if st.CheckedIn {
		// The reward for today is already in hand.  That is a success the
		// operator asked about, not a failure.
		res.OK = true
		res.Code = int(status.Code)
		res.Message = firstNonEmpty(status.message(), "already checked in today")
		res.Data = map[string]any{"already_done": true, "credits": st.Credits}
		c.pool.MarkSuccess(a)
		return c.finish(res, start), nil
	}

	// Step 2: claim.
	var claim ugCheckinReply
	if err := c.doJSON(ctx, c.cfg.authHost()+ugCheckinClaimPath, c.ugHeaders(a), []byte("{}"), &claim); err != nil {
		return c.upstreamFailure(a, res, start, "claim the check-in credits", err), nil
	}
	cl := claim.state()
	res.OK = true
	res.Code = int(claim.Code)
	res.Message = firstNonEmpty(claim.message(), "checked in")
	res.Data = map[string]any{
		"already_done": false,
		"credits":      cl.Credits,
	}
	if cl.Credits == 0 {
		// Fall back to the status reading: some builds report the running
		// balance on status and nothing on claim.
		res.Data["credits"] = st.Credits
	}
	c.pool.MarkSuccess(a)
	c.log("checkin %s: claimed %d credit(s)", a.Masked(), cl.Credits)
	return c.finish(res, start), nil
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
