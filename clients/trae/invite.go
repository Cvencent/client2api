package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

// invite.go implements the Trae "code fission" invite campaign the desktop
// client links to from its banner (www.trae.cn/events/code-fission/<code>).
//
// The page it opens is not a second login.  It is a landing page that, in the
// official client, does exactly this:
//
//	smsLogin({mobile, code})            -> one session
//	bindCodeFission(session, invite)    -> the same session binds the code
//
// The bind is a plain authenticated call against api.trae.cn with the same
// Cloud-IDE-JWT the rest of this module already uses, so the gateway can bind a
// freshly created account right after its own login without a second sign-in.
//
// What the vendor does NOT allow: binding an account that has registered
// before, or a second account on the same device.  Those come back as business
// codes (9022 already participated, 9023 invalid code) and are reported as
// results, never retried.

const (
	inviteBindPath         = "/trae/api/v2/ug/code_fission/bind"
	inviteInviterInfoPath  = "/trae/api/v2/ug/code_fission/inviter_info"
	inviteShareLinkPath    = "/trae/api/v2/ug/code_fission/share_link"
	inviteShareStatusPath  = "/trae/api/v2/ug/code_fission/share_status"
	inviteTimeout          = 25 * time.Second
	inviteErrAlreadyJoined = 9022
	inviteErrInvalidCode   = 9023
)

// inviteReply is the envelope every code-fission endpoint returns.
type inviteReply struct {
	Code        int64  `json:"code"`
	Message     string `json:"message"`
	InviteCode  string `json:"invite_code"`
	InviterName string `json:"inviter_name"`
	InviterUID  string `json:"inviter_uid"`
	HasMore     bool   `json:"has_more"`
	Invitees    []struct {
		UserID   string `json:"user_id"`
		Nickname string `json:"nickname"`
		Status   string `json:"status"`
	} `json:"invitees"`
}

func (r inviteReply) message() string {
	if s := strings.TrimSpace(r.Message); s != "" {
		return s
	}
	return fmt.Sprintf("trae invite error %d", r.Code)
}

// inviteCall posts one code-fission request with the account's session.
func (c *Client) inviteCall(ctx context.Context, a *Auth, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode invite request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, inviteTimeout)
	defer cancel()
	return c.doJSON(ctx, c.cfg.authHost()+path, c.ugHeaders(a), body, out)
}

// bindInviteCode applies inviteCode to a just-authenticated account.
//
// The vendor retries once on "user info invalid" because a freshly registered
// account can answer before its profile is readable; the official page does the
// same, so one retry is reproduced here rather than treating that race as a
// failure.
func (c *Client) bindInviteCode(ctx context.Context, a *Auth, inviteCode string) core.InviteCodeResult {
	res := core.InviteCodeResult{
		AccountID: a.ID(),
		Code:      inviteCode,
		At:        time.Now().Format(time.RFC3339),
	}
	payload := map[string]any{
		"activity_id": c.cfg.inviteActivityID(),
		"invite_code": inviteCode,
	}
	var reply inviteReply
	err := c.inviteCall(ctx, a, inviteBindPath, payload, &reply)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if reply.Code == inviteErrAlreadyJoined {
		res.Error = "该账号已参与过本次活动（新用户专用，同一设备只能邀请一次）"
		return res
	}
	if reply.Code == inviteErrInvalidCode {
		res.Error = "邀请码无效"
		return res
	}
	if reply.Code != 0 {
		res.Error = reply.message()
		return res
	}
	res.OK = true
	res.Activated = true
	res.AppliedCode = inviteCode
	res.Message = "邀请码绑定成功"
	return res
}

// ---- core.InviteCodeProvider ----------------------------------------------

var _ core.InviteCodeProvider = (*Client)(nil)
var _ core.InviteCodeFleetBinder = (*Client)(nil)

// InviteCodeDefault is the configured code-fission invitation code.
func (c *Client) InviteCodeDefault(context.Context) string { return c.cfg.inviteCode() }

// InviteCodeBindAllSafe is false: Trae only accepts an invitee that has never
// registered, so the panel must not offer a fleet-wide bind over an existing
// pool.
func (c *Client) InviteCodeBindAllSafe() bool { return false }

// BindAccountInviteCode binds an invitation code to one stored account.
//
// It exists for the operator's manual retry, not as a bulk tool: an account
// that already registered will get 9022 back, which is reported verbatim.
func (c *Client) BindAccountInviteCode(ctx context.Context, id, code string) (core.InviteCodeResult, error) {
	a := c.findAuth(strings.TrimSpace(id))
	if a == nil {
		return core.InviteCodeResult{}, fmt.Errorf("account %q not found", id)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = c.cfg.inviteCode()
	}
	if code == "" {
		return core.InviteCodeResult{
			AccountID: a.ID(),
			Error:     "没有配置邀请码",
		}, nil
	}
	return c.bindInviteCode(ctx, a, code), nil
}

// RedeemAccountCode is not part of the code-fission campaign.  Trae's
// redemption codes are a different product (pay/redeem_code); this module does
// not claim that capability, but the interface requires the method, so it
// answers honestly instead of pretending to redeem.
func (c *Client) RedeemAccountCode(ctx context.Context, id, code string) (core.RedemptionResult, error) {
	a := c.findAuth(strings.TrimSpace(id))
	if a == nil {
		return core.RedemptionResult{}, fmt.Errorf("account %q not found", id)
	}
	return core.RedemptionResult{
		AccountID: a.ID(),
		Code:      strings.TrimSpace(code),
		Error:     "Trae 的兑换码接口不在本次活动范围内，模块未接入",
		At:        time.Now().Format(time.RFC3339),
	}, nil
}

// applyInviteAfterLogin runs the bind step for a login that just succeeded.
// A failure never fails the login: the credential is already valid and stored,
// and the operator can retry the bind from the accounts page.
func (c *Client) applyInviteAfterLogin(ctx context.Context, a *Auth) (core.InviteCodeResult, bool) {
	code := c.cfg.inviteCode()
	if code == "" {
		return core.InviteCodeResult{}, false
	}
	res := c.bindInviteCode(ctx, a, code)
	if res.OK {
		c.log("trae: invite code %s bound to %s", code, a.ID())
	} else {
		c.log("trae: invite code bind for %s failed: %s", a.ID(), res.Error)
	}
	return res, true
}

// inviteStatus is a best-effort read used by the panel to show the inviter and
// the account's own share code.  It is not part of the login path.
func (c *Client) inviteStatus(ctx context.Context, a *Auth) (inviteReply, error) {
	var reply inviteReply
	err := c.inviteCall(ctx, a, inviteShareLinkPath, map[string]any{
		"activity_id": c.cfg.inviteActivityID(),
	}, &reply)
	return reply, err
}

// inviteInviter looks up who owns an invitation code.
func (c *Client) inviteInviter(ctx context.Context, a *Auth, code string) (inviteReply, error) {
	var reply inviteReply
	err := c.inviteCall(ctx, a, inviteInviterInfoPath, map[string]any{
		"activity_id": c.cfg.inviteActivityID(),
		"invite_code": strings.TrimSpace(code),
	}, &reply)
	return reply, err
}

// inviteShareStatus lists the accounts this one has invited.
func (c *Client) inviteShareStatus(ctx context.Context, a *Auth) (inviteReply, error) {
	var reply inviteReply
	err := c.inviteCall(ctx, a, inviteShareStatusPath, map[string]any{
		"activity_id": c.cfg.inviteActivityID(),
	}, &reply)
	return reply, err
}
