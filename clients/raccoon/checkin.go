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
// There is no status endpoint and no "already claimed" error code. The reply is
// a bare {granted, popup?} object: `granted:true` means points moved, and
// `granted:false` is the server declining this attempt. **The window the server
// enforces is not documented anywhere in the client** — the hook fires on every
// launch and lets the reply decide — so this module must not claim the reward is
// once per day, once per account, or anything else. It asks, and reports the
// answer. The vendor's own hook is fire-and-forget: it logs a failure to the
// console and shows a popup only when `granted` is true.
//
// This is NOT the daily 300 credits — those are granted server-side
// (`daily_grant` in the points ledger), have no endpoint at all (see
// balance.go), and are not claimed by anything here.

const (
	// checkinActionLoginPoints is the single action this module offers.
	checkinActionLoginPoints = "login-points"

	// pointsGrantTimeout bounds the one round trip. It is a small JSON POST;
	// 60 s (the module default) would leave a panel click spinning far longer
	// than the vendor ever takes.
	pointsGrantTimeout = 20 * time.Second
)

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
		Label: "领取登录积分",
		Help:  "Asks the vendor to grant this account's desktop login points. The vendor decides whether anything is handed out this time, so granted:false is reported as a refusal, not an error. This is not the 300 daily credits, which have no endpoint.",
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
		// The vendor's own client treats this as "nothing to show", not as an
		// error. It is still a refusal for us: no points moved. We do not guess
		// at why — the reply carries no reason, and the client documents no
		// window.
		res.Error = "the vendor did not grant desktop login points this time"
		return c.finishCheckin(res, start), nil
	}

	res.OK = true
	res.Message = "the vendor granted the desktop login points"
	if reply.Popup != nil {
		res.Message += " (the reply carried a popup; see data.popup)"
	}
	return c.finishCheckin(res, start), nil
}

// finishCheckin stamps the elapsed time and the local clock reading.
func (c *Client) finishCheckin(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = c.now().Sub(start).Milliseconds()
	res.At = c.now().Format(time.RFC3339)
	return res
}

// Compile-time proof that this module satisfies the optional contract.
var _ core.CheckinProvider = (*Client)(nil)
