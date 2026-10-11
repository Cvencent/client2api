package autoclaw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// checkin.go implements AutoClaw's daily credits claim.
//
// The desktop client fires exactly one request when an account transitions to
// authenticated, and the renderer can also trigger it on demand:
//
//	POST /userapi/v1/user-info
//	{"source_id":"autoclaw","device_id":"..."}
//
// The server deduplicates by calendar day and the response body is not
// meaningful: the client treats "the request was sent" as success and then
// refreshes the wallet.  This module does the same, but keeps the vendor's
// business code so a real refusal is still visible.

const (
	checkinActionDaily = "daily-credits"
	checkinTimeout     = 15 * time.Second
)

var _ core.CheckinProvider = (*Client)(nil)

// CheckinActions advertises the daily claim, but only while at least one
// account exists.  Offering a button that cannot work would be a lie the
// operator pays for with a failed request.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.pool == nil || c.pool.len() == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionDaily,
		Label: "领取每日积分",
		Help:  "POST /userapi/v1/user-info，AutoClaw 客户端登录后自动领取；服务端按天去重，重复请求仍是正常结果。",
	}}
}

// Checkin performs the daily claim for one account.
//
// Every upstream refusal comes back as a CheckinResult with OK=false and a nil
// error; a Go error is reserved for "this could not even be attempted".
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := c.now()
	res := core.CheckinResult{AccountID: id}
	if a := strings.TrimSpace(action); a != "" && a != checkinActionDaily {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, checkinActionDaily)
		return c.finishCheckin(res, start), nil
	}
	res.Action = checkinActionDaily

	e, err := c.balanceAccount(id)
	if err != nil {
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

	ctx, cancel := withTimeout(ctx, checkinTimeout)
	defer cancel()
	c.ensureFresh(ctx, e)

	payload := map[string]string{
		"source_id": "autoclaw",
		"device_id": strings.TrimSpace(e.acct.DeviceID),
	}
	err = c.do(ctx, http.MethodPost, pathUserInfo, identityHeaders(e.acct.cred(), c.cfg.version()), payload, nil)
	if err != nil {
		kind := classifyErr(err)
		msg := truncate(c.scrub(err.Error()), 240)
		if kind == kindAuth {
			c.pool.markDead(e, msg)
		} else {
			c.pool.markFailure(e, kind, msg)
		}
		c.noteFailure(err)
		res.Error = "could not claim the daily credits: " + msg
		return c.finishCheckin(res, start), nil
	}
	c.noteUpstreamOK()
	c.pool.markUsed(e)
	res.OK = true
	res.Message = "已请求每日积分；服务端按天去重，额度以余额为准"
	return c.finishCheckin(res, start), nil
}

// finishCheckin stamps the elapsed time and the local clock reading.
func (c *Client) finishCheckin(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = c.now().Sub(start).Milliseconds()
	res.At = c.now().Format(time.RFC3339)
	return res
}
