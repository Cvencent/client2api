package loomy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// invite.go implements the two code surfaces the Loomy desktop client exposes
// on its points page.  They are deliberately separate:
//
//	POST /points/activation                 {"inviteCode":"P276D2","deviceId":"…"}
//	POST /points/redemption-codes/redeem    {"code":"…"}
//
// The first binds the account to an invitation code and is the call behind
// "使用邀请码兑换积分"; the second spends a redemption-code voucher.  Calling
// the first on every account is what makes the operator's invite code apply to
// the whole pool, including accounts added later through SMS or a pasted token.

var _ core.InviteCodeProvider = (*Client)(nil)
var _ core.InviteCodeFleetBinder = (*Client)(nil)

// inviteActivation is the shared response of the activation status read and the
// activation write.  The vendor reports both a boolean and the code that was
// actually applied, so the result can distinguish "already ours" from "bound
// to a different code".
type inviteActivation struct {
	Activated             bool    `json:"activated"`
	AlreadyActivated      bool    `json:"alreadyActivated"`
	AppliedInvitationCode string  `json:"appliedInvitationCode"`
	InviteeReward         float64 `json:"inviteeReward"`
	RegisterReward        float64 `json:"registerReward"`
	CurrentBalance        float64 `json:"currentBalance"`
	PermanentBalance      float64 `json:"permanentBalance"`
	DailyBalance          float64 `json:"dailyBalance"`
	DailyQuota            float64 `json:"dailyQuota"`
	DailyConsumed         float64 `json:"dailyConsumed"`
	DailyCycleDate        string  `json:"dailyCycleDate"`
	InvitationCodes       []struct {
		InviteCode string `json:"inviteCode"`
		UsedCount  int    `json:"usedCount"`
		MaxUses    int    `json:"maxUses"`
		Status     string `json:"status"`
	} `json:"invitationCodes"`
}

type redemptionOutcome struct {
	Points         float64 `json:"points"`
	CurrentBalance float64 `json:"currentBalance"`
}

// inviteActivationStatus performs the read-only check the desktop client does
// before showing whether the "兑换积分" button has already been used.
func (u *upstream) inviteActivationStatus(ctx context.Context, token string) (inviteActivation, error) {
	data, err := u.business(ctx, http.MethodGet, "/points/activation", nil, token, nil)
	if err != nil {
		return inviteActivation{}, err
	}
	var out inviteActivation
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &out); err != nil {
			return inviteActivation{}, fmt.Errorf("loomy: reading invite activation: %w", err)
		}
	}
	return out, nil
}

// activateInvite binds inviteCode to the account.  The deviceId is stable per
// account, not per process: the vendor uses it for anti-abuse and the desktop
// client sends its own stable campus device id here.
func (u *upstream) activateInvite(ctx context.Context, token, inviteCode, deviceID string) (inviteActivation, error) {
	body, err := json.Marshal(map[string]string{
		"inviteCode": inviteCode,
		"deviceId":   deviceID,
	})
	if err != nil {
		return inviteActivation{}, fmt.Errorf("loomy: encoding invite activation: %w", err)
	}
	data, err := u.business(ctx, http.MethodPost, "/points/activation", nil, token, body)
	if err != nil {
		return inviteActivation{}, err
	}
	var out inviteActivation
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &out); err != nil {
			return inviteActivation{}, fmt.Errorf("loomy: reading invite activation: %w", err)
		}
	}
	return out, nil
}

// redeemCode redeems a points redemption code (not an invitation code).
func (u *upstream) redeemCode(ctx context.Context, token, code string) (redemptionOutcome, error) {
	body, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return redemptionOutcome{}, fmt.Errorf("loomy: encoding redemption code: %w", err)
	}
	data, err := u.business(ctx, http.MethodPost, "/points/redemption-codes/redeem", nil, token, body)
	if err != nil {
		return redemptionOutcome{}, err
	}
	var out redemptionOutcome
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &out); err != nil {
			return redemptionOutcome{}, fmt.Errorf("loomy: reading redemption result: %w", err)
		}
	}
	return out, nil
}

// InviteCodeDefault implements core.InviteCodeProvider.
func (c *Client) InviteCodeDefault(context.Context) string {
	if c == nil {
		return ""
	}
	return c.cfg.inviteCode()
}

// InviteCodeBindAllSafe reports that Loomy's activation endpoint is idempotent
// and accepts the code on any account, so a fleet-wide sweep is meaningful.
func (c *Client) InviteCodeBindAllSafe() bool { return true }

// BindAccountInviteCode implements core.InviteCodeProvider.
//
// It first asks the vendor whether the account is already activated.  A
// matching code is a successful no-op; a different applied code is reported as
// a refusal rather than overwritten, because the vendor's endpoint is not a
// general-purpose "change invitation" call.
func (c *Client) BindAccountInviteCode(ctx context.Context, id, code string) (core.InviteCodeResult, error) {
	acc, ok := c.store.lookup(strings.TrimSpace(id))
	if !ok {
		return core.InviteCodeResult{}, fmt.Errorf("loomy: no account %q", id)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		code = c.cfg.inviteCode()
	}
	res := core.InviteCodeResult{
		AccountID: acc.ID,
		Code:      code,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if code == "" {
		res.Error = "loomy: no invite code was supplied and no default is configured"
		return res, nil
	}

	// A failed status read is not a reason to skip the write: the endpoint may
	// be temporarily unavailable while activation still works.  Only a
	// definitive "already activated" short-circuits it.
	if status, err := c.up.inviteActivationStatus(ctx, acc.AccessToken); err == nil && status.Activated {
		res.OK = true
		res.Activated = true
		res.Already = true
		res.AppliedCode = strings.TrimSpace(status.AppliedInvitationCode)
		if res.AppliedCode != "" && !strings.EqualFold(res.AppliedCode, code) {
			res.OK = false
			res.Already = false
			res.Error = "loomy: this account is already bound to invite code " + res.AppliedCode
			return res, nil
		}
		if res.AppliedCode == "" {
			res.AppliedCode = code
		}
		res.Message = "invite code was already bound"
		c.store.setInviteState(acc.ID, code, true)
		return res, nil
	}

	out, err := c.up.activateInvite(ctx, acc.AccessToken, code, inviteDeviceID(acc))
	if err != nil {
		res.Error = redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			c.penaliseRecoverable(acc.ID, err, time.Now().UTC())
		}
		return res, nil
	}
	if !out.Activated && !out.AlreadyActivated {
		res.Error = "loomy: the vendor did not confirm invite activation"
		return res, nil
	}
	res.OK = true
	res.Activated = out.Activated || out.AlreadyActivated
	res.Already = out.AlreadyActivated
	res.AppliedCode = strings.TrimSpace(out.AppliedInvitationCode)
	if res.AppliedCode == "" {
		res.AppliedCode = code
	}
	if res.Already && !strings.EqualFold(res.AppliedCode, code) {
		res.OK = false
		res.Already = false
		res.Activated = false
		res.Error = "loomy: this account is already bound to invite code " + res.AppliedCode
		return res, nil
	}
	res.Reward = int64(out.InviteeReward + 0.5)
	if res.Already {
		res.Message = "invite code was already bound"
	} else if res.Reward > 0 {
		res.Message = fmt.Sprintf("invite code bound; received %d 积分", res.Reward)
	} else {
		res.Message = "invite code bound"
	}
	c.store.setInviteState(acc.ID, code, true)
	return res, nil
}

// RedeemAccountCode implements core.InviteCodeProvider.  This is the other
// points-page action: it redeems a redemption code, not an invitation code.
func (c *Client) RedeemAccountCode(ctx context.Context, id, code string) (core.RedemptionResult, error) {
	acc, ok := c.store.lookup(strings.TrimSpace(id))
	if !ok {
		return core.RedemptionResult{}, fmt.Errorf("loomy: no account %q", id)
	}
	code = strings.TrimSpace(code)
	res := core.RedemptionResult{
		AccountID: acc.ID,
		Code:      code,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if code == "" {
		res.Error = "loomy: enter a redemption code"
		return res, nil
	}
	out, err := c.up.redeemCode(ctx, acc.AccessToken, code)
	if err != nil {
		res.Error = redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			c.penaliseRecoverable(acc.ID, err, time.Now().UTC())
		}
		return res, nil
	}
	res.OK = true
	res.Points = int64(out.Points + 0.5)
	res.Balance = int64(out.CurrentBalance + 0.5)
	if res.Points > 0 {
		res.Message = fmt.Sprintf("redeemed %d 积分", res.Points)
	} else {
		res.Message = "redemption succeeded"
	}
	return res, nil
}

// inviteDeviceID derives a stable, account-scoped device id.  The desktop client
// sends "loomy-campus-fp-" followed by a SHA-256 fingerprint; using the same
// namespace keeps the server-side contract while preventing every account in
// the pool from sharing one device identity.
func inviteDeviceID(acc account) string {
	seed := strings.TrimSpace(acc.UserID)
	if seed == "" {
		seed = strings.TrimSpace(acc.ID)
	}
	sum := sha256.Sum256([]byte("loomy-campus-device:" + seed))
	return "loomy-campus-fp-" + hex.EncodeToString(sum[:])
}

// bindInviteAfterLogin runs the same binding step used by auto-login and the
// operator-owned phone flow.  A binding failure is reported to the caller but
// never turns a successful login into a failed account: the session is the
// valuable result, and the operator can retry the invite code from the panel.
func (c *Client) bindInviteAfterLogin(ctx context.Context, accountID string, logf func(string, ...any)) (core.InviteCodeResult, error) {
	if c == nil || c.store == nil || c.store.count() == 0 || strings.TrimSpace(c.cfg.inviteCode()) == "" {
		return core.InviteCodeResult{AccountID: accountID}, nil
	}
	res, err := c.BindAccountInviteCode(ctx, accountID, "")
	if err != nil {
		if logf != nil {
			logf("loomy: binding invite code for %s failed: %v", accountID, err)
		}
		return res, err
	}
	if logf != nil {
		switch {
		case res.OK && res.Already:
			logf("邀请码 %s 已绑定，无需重复操作", res.AppliedCode)
		case res.OK:
			logf("邀请码 %s 绑定成功", res.AppliedCode)
		default:
			logf("邀请码绑定失败：%s", res.Error)
		}
	}
	return res, nil
}
