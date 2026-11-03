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

// invite.go implements the three code surfaces the Loomy desktop client
// exposes on its points page.  They are deliberately separate:
//
//	GET  /api/v1/invitation-codes           (the inviter's own codes + usage)
//	POST /api/v1/points/activation          {"inviteCode":"P276D2","deviceId":"…"}
//	POST /api/v1/points/redemption-codes/redeem {"code":"…"}
//
// The first lists the codes the account ITSELF hands out, with usedCount /
// maxUses / status -- exactly what the desktop client's invite page renders.
// The second binds the account to an invitation code and is the call behind
// "使用邀请码兑换积分"; the third spends a redemption-code voucher.  Binding a
// NEW account therefore picks a live code from the pool instead of a compiled
// default: an operator code that has spent all of its uses can no longer bind
// anything, while every account in the pool carries its own 20-use code.
//
// The /points/activation read used to be expected to carry invitationCodes;
// probing every account of a live deployment showed it only ever answers
// {activated, appliedInvitationCode}, so the separate listing endpoint is the
// authority on how many uses each code has left.

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

// inviteCodeUse is one row of the inviter's own code list: the code, how many
// accounts have already bound through it, its per-code cap and the vendor's
// own active/expired verdict.
type inviteCodeUse struct {
	InviteCode string
	UsedCount  int
	MaxUses    int
	Status     string
}

// usesLeft reports the remaining bindings, or 0 when the code is spent,
// expired or the vendor omitted the cap.
func (c inviteCodeUse) usesLeft() int {
	if !strings.EqualFold(c.Status, "active") {
		return 0
	}
	left := c.MaxUses - c.UsedCount
	if left < 0 {
		return 0
	}
	return left
}

// queryInviteCodes lists the codes the token's own account hands out.  The
// desktop client calls GET /api/v1/invitation-codes behind its invite page.
func (u *upstream) queryInviteCodes(ctx context.Context, token string) ([]inviteCodeUse, error) {
	data, err := u.business(ctx, http.MethodGet, "/invitation-codes", nil, token, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		List []struct {
			InviteCode string `json:"inviteCode"`
			UsedCount  int    `json:"usedCount"`
			MaxUses    int    `json:"maxUses"`
			Status     string `json:"status"`
		} `json:"list"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("loomy: reading invitation codes: %w", err)
		}
	}
	codes := make([]inviteCodeUse, 0, len(out.List))
	for _, row := range out.List {
		code := strings.TrimSpace(row.InviteCode)
		if code == "" {
			continue
		}
		codes = append(codes, inviteCodeUse{
			InviteCode: code,
			UsedCount:  row.UsedCount,
			MaxUses:    row.MaxUses,
			Status:     strings.TrimSpace(row.Status),
		})
	}
	return codes, nil
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

// invitePickTTL bounds how long one pool-wide code scan is trusted.  The scan
// is one GET per account; the panel's status route re-reads capabilities on
// every refresh, so an uncached pick would turn each UI poll into a 40-request
// burst against the vendor.  A bind always re-scans with a fresh read.
const invitePickTTL = 10 * time.Minute

// maxInviteScanBudget bounds one background pool scan.  The scan is one GET per
// account; the budget is one request timeout per account, capped here so a very
// large pool cannot tie the goroutine (and its context) up indefinitely.
const maxInviteScanBudget = 3 * time.Minute

// cachedInvitePick memoizes one pickInviteCode sweep.  protected by c.inviteMu.
type cachedInvitePick struct {
	code    string
	seen    []inviteCodeUse
	scanned time.Time
}

// InviteCodeDefault implements core.InviteCodeProvider.  It answers the code
// the NEXT bind would use: a live pool code with uses left when one exists,
// otherwise the configured default.
//
// It NEVER blocks on the vendor (issue #96).  The panel reads this on every
// status refresh, and the pool scan behind it is one GET per account: on a
// deployment with 52 accounts a cold scan held /panel/api/status for 3-4
// seconds, which is the spike #96 spent several rounds chasing.  The memo below
// only reduced how OFTEN that happened, not that it blocked a read.
//
// So the read now serves the last known answer and refreshes in the background
// -- the same shape gemini's warmCatalogue uses.  Until the first scan lands it
// answers the configured default, which is what the bind would have fallen back
// to anyway.  BindAccountInviteCode still re-scans fresh when it needs an
// authoritative code, so a spent code is never bound twice.
func (c *Client) InviteCodeDefault(ctx context.Context) string {
	if c == nil {
		return ""
	}
	c.warmInvitePick(ctx)
	if code, _, ok := c.cachedInvitePick(); ok && code != "" {
		return code
	}
	return c.cfg.inviteCode()
}

// cachedInvitePick returns the memoized pick without touching the network.
func (c *Client) cachedInvitePick() (string, []inviteCodeUse, bool) {
	c.inviteMu.Lock()
	defer c.inviteMu.Unlock()
	if c.invitePick == nil {
		return "", nil, false
	}
	return c.invitePick.code, c.invitePick.seen, true
}

// warmInvitePick starts one background pool scan when the memo is stale.  It
// returns immediately, so a status read is never held by the sweep.
func (c *Client) warmInvitePick(ctx context.Context) {
	if c.store == nil || c.store.count() == 0 {
		return
	}
	c.inviteMu.Lock()
	if c.invitePick != nil && time.Since(c.invitePick.scanned) < invitePickTTL {
		c.inviteMu.Unlock()
		return
	}
	if c.inviteScanning {
		// One sweep at a time: the panel polls every five seconds, and a sweep
		// takes longer than that.
		c.inviteMu.Unlock()
		return
	}
	c.inviteScanning = true
	c.inviteMu.Unlock()

	core.GoSafe("loomy invite-code scan", func(msg string) { c.logf("%s", msg) }, func() {
		defer func() {
			c.inviteMu.Lock()
			c.inviteScanning = false
			c.inviteMu.Unlock()
		}()
		// The scan outlives the request that triggered it, so it does not
		// inherit that request's context -- but it still needs a ceiling: one
		// budget per account, capped so a large pool cannot hold the goroutine
		// for hours.
		budget := c.cfg.requestTimeout()
		if n := time.Duration(c.store.count()); n > 0 {
			budget *= n
		}
		if budget > maxInviteScanBudget {
			budget = maxInviteScanBudget
		}
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		code, seen, err := c.pickInviteCode(ctx, "")
		if err != nil || code == "" {
			return
		}
		c.inviteMu.Lock()
		c.invitePick = &cachedInvitePick{code: code, seen: seen, scanned: time.Now()}
		c.inviteMu.Unlock()
	})
}

// InviteCodeBindAllSafe reports that Loomy's activation endpoint is idempotent
// and accepts the code on any account, so a fleet-wide sweep is meaningful.
func (c *Client) InviteCodeBindAllSafe() bool { return true }

// pickInviteCode scans the pool once and reports the code with the most uses
// left plus every code row it saw.  It is the uncached core; callers go
// through pickedInviteCode so the panel path reuses the sweep.
//
// The pool snapshot order is the operator's own ordering; ties are broken by
// that order, which makes repeated picks fall down the pool instead of always
// hammering one account.  exclude is the account being bound: its own code
// cannot invite itself, so it is skipped.
func (c *Client) pickInviteCode(ctx context.Context, exclude string) (string, []inviteCodeUse, error) {
	bestCode := ""
	var best inviteCodeUse
	var seen []inviteCodeUse
	for _, acc := range c.store.snapshot() {
		if acc.ID == exclude || !acc.Enabled || strings.TrimSpace(acc.AccessToken) == "" {
			continue
		}
		codes, err := c.up.queryInviteCodes(ctx, acc.AccessToken)
		if err != nil {
			// One unreachable account must not blind the picker: it is skipped,
			// and if that leaves nothing, the caller falls back to the config.
			continue
		}
		seen = append(seen, codes...)
		for _, cu := range codes {
			if cu.usesLeft() <= 0 {
				continue
			}
			if bestCode == "" || cu.usesLeft() > best.usesLeft() {
				bestCode, best = cu.InviteCode, cu
			}
		}
	}
	return bestCode, seen, nil
}

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
	pickNote := ""
	if code == "" {
		// No explicit code: pick the live code with the most remaining uses
		// from the pool's own accounts.  Only when the pool cannot answer
		// (empty, unreachable, or every code spent) does the compiled default
		// apply.  The pick runs fresh for the account being bound so a code
		// that hit its cap between the memo and now is never reused.
		if picked, _, err := c.pickInviteCode(ctx, acc.ID); err == nil && picked != "" {
			code = picked
		} else if cfgCode := c.cfg.inviteCode(); cfgCode != "" {
			code = cfgCode
			if err != nil {
				pickNote = "邀请码余量查询失败，退回配置的邀请码"
			} else {
				pickNote = "账号池里的邀请码都已用尽，退回配置的邀请码"
			}
		}
	}
	res := core.InviteCodeResult{
		AccountID: acc.ID,
		Code:      code,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	res.Message = pickNote
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
