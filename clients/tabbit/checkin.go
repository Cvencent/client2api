package tabbit

// checkin.go implements Tabbit's daily sign-in (每日签到).
//
// The endpoint pair was read off the vendor's own web bundle, not guessed.  The
// bundle's commerce API client (base "/api/commerce") carries exactly these two
// calls:
//
//	async signIn(e)            -> POST /activity/v1/sign-in          body: e
//	async getSignInStatus(s)   -> GET  /activity/v1/sign-in/status   ?scene_codes=<s>
//	                             (the bundle defaults the scene to "desktop_pet")
//
// The reward is credited back as quota, not as a credit balance: the vendor's
// own i18n table calls the source "Sign-in Reward" and files it under the usage
// ledger next to "Usage Restored" and "Usage Reset Coupon".  Tabbit's quota
// endpoint reports a percentage of the current cycle's allowance, so a daily
// grant shows up there as usage_percentage moving — which is exactly what the
// panel's balance column already renders.
//
// What the bundle does NOT document, and this module therefore does not claim:
//   · how much a sign-in is worth.  The operator says 3% of the allowance; the
//     bundle carries no amount, so the reply's own number is reported and no
//     figure is hard-coded here.
//   · whether the window is per calendar day, per rolling 24h, or per cycle.
//     The vendor decides, and the reply is the only authority.
//
// Only web-token accounts can sign in: a sidecar endpoint is a local bridge
// with no vendor session behind it, so asking the vendor would be a guaranteed
// 401.  CheckinActions reports no action at all in that case rather than
// offering a button that cannot work.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// webSignInScene is the scene the vendor's own client signs in for.  It is
	// the bundle's default for getSignInStatus, and the panel posts the same
	// string so the two calls cannot drift apart.
	webSignInScene = "desktop_pet"

	// webSignInPath / webSignInStatusPath sit under the vendor's commerce API.
	webSignInPath       = "/api/commerce/activity/v1/sign-in"
	webSignInStatusPath = webSignInPath + "/status"

	// signInTimeout bounds the one round trip.  It is a small JSON POST; the
	// module's default budget would leave a panel click spinning far longer than
	// the vendor takes.
	signInTimeout = 20 * time.Second
)

// signInRequest is the body the vendor's client sends.  Only the scene is known
// to be required; the vendor's own client sends the same object its UI builds,
// and an unknown field is better left out than guessed at.
type signInRequest struct {
	SceneCode string `json:"scene_code"`
	// RequestNo is the caller-generated idempotency key the endpoint now
	// requires: without it the vendor answers 422 VALIDATION_ERROR with
	// details[].field="body.request_no".  A fresh v4 UUID per claim is the shape
	// the vendor's own bundle uses for the rest of this key family (client_run_id,
	// page_instance_id, x-signature), and the sibling /activity/v1/participate
	// call passes the same caller-made key.
	RequestNo string `json:"request_no"`
}

// signInReply is what this module decodes from the POST.  The vendor's real
// reply is richer than the panel needs, so only the fields the operator is shown
// are named; anything else stays out of the struct rather than being invented.
//
// Granted is the honest success bit: a vendor that answers 200 while granting
// nothing has credited nothing, and the panel must say so instead of printing a
// success toast over an unchanged balance.
type signInReply struct {
	Success   bool    `json:"success"`
	Granted   bool    `json:"granted"`
	Rewarded  float64 `json:"rewarded"`
	SceneCode string  `json:"scene_code"`
	Streak    int     `json:"streak"`
	Message   string  `json:"message"`
}

// webSignInStatus is the GET /sign-in/status answer.  signed_in arrives as a
// bool or as the strings "true"/"false", and the numbers as either JSON numbers or
// quoted strings — the quota endpoint on this same host does exactly that, see
// flexNumber.  Decoding strictly here would turn a status read into a parse
// error over a field the panel only uses for an explanation.
type webSignInStatus struct {
	SignedIn flexBool   `json:"signed_in"`
	Streak   flexNumber `json:"streak"`
	Rewarded flexNumber `json:"rewarded"`
}

// flexBool accepts true, "true", 1 and "1" — the shapes the vendor has used for
// boolean-ish fields.  Anything else decodes to false, so a renamed field shows
// up as "not signed in" rather than as a failed read.
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.ToLower(s) {
	case "", "0", "false", "null":
		*b = false
	default:
		*b = true
	}
	return nil
}

// ---- CheckinProvider -------------------------------------------------------

// CheckinActions advertises the daily sign-in, but only while a web-token
// account exists.  A sidecar-only module reports nothing: the button would be
// a guaranteed 401.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c == nil || !c.hasWebTokenAccount() {
		return nil
	}
	return []core.CheckinAction{{
		ID:    webSignInScene,
		Label: "领取每日签到额度",
		Help: "向厂商的签到接口 POST 一次，由厂商决定这次是否发放。" +
			"签到额度记在用量里，所以签到后余额列的已用百分比会变小。" +
			"厂商答「今天已签到」算未成功，不是错误。",
	}}
}

// Checkin claims the daily sign-in for one account.
//
// Everything the upstream says — already signed in today, session rejected,
// rate limited — comes back as a CheckinResult with OK=false and a nil error.  A
// Go error is reserved for "this could not even be attempted" (no such account,
// not a web-token account, the request could not be built).
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := time.Now()
	res := core.CheckinResult{AccountID: id}

	if a := strings.TrimSpace(action); a != "" && a != webSignInScene {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, webSignInScene)
		return finishSignIn(res, start), nil
	}
	res.Action = webSignInScene

	if c == nil {
		return res, fmt.Errorf("tabbit: no client")
	}
	key := strings.TrimSpace(id)
	if key == "" {
		return res, fmt.Errorf("tabbit: no account id was given")
	}
	ep, ok := c.lookupEndpoint(key)
	if !ok {
		return res, fmt.Errorf("tabbit: account %q not found", key)
	}
	if epKind(ep) != kindWebToken {
		// A sidecar endpoint is a local bridge: there is no vendor session to
		// sign in with, and asking anyway would burn a round trip on a 401.
		res.Error = fmt.Sprintf("account %q is a sidecar account, which has no vendor session to sign in with", key)
		return finishSignIn(res, start), nil
	}
	if !ep.Enabled {
		res.Error = "the account is disabled; enable it before signing in"
		return finishSignIn(res, start), nil
	}

	wa := c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL)

	ctx, cancel := context.WithTimeout(ctx, signInTimeout)
	defer cancel()

	body, err := json.Marshal(signInRequest{SceneCode: webSignInScene, RequestNo: newUUID()})
	if err != nil {
		return res, err
	}
	resp, err := c.webDo(ctx, wa, http.MethodPost, webSignInPath, nil, body, "application/json")
	if err != nil {
		res.Error = fmt.Sprintf("could not ask Tabbit for the daily sign-in: %s", core.Redact(truncate(err.Error(), 240)))
		return finishSignIn(res, start), nil
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		// webError already spells out the fix for a dead cookie (press 导入凭据).
		res.Error = core.Redact(truncate(webErrorMessage(wa, "POST "+webSignInPath, resp.StatusCode, raw).Error(), 300))
		return finishSignIn(res, start), nil
	}

	var reply signInReply
	if err := json.Unmarshal(bytes.TrimSpace(raw), &reply); err != nil {
		res.Error = fmt.Sprintf("parsing the Tabbit sign-in reply: %v", err)
		return finishSignIn(res, start), nil
	}

	res.Data = map[string]any{"granted": reply.Granted, "scene_code": webSignInScene}
	if reply.Streak > 0 {
		res.Data["streak"] = reply.Streak
	}
	if reply.Rewarded > 0 {
		res.Data["rewarded"] = reply.Rewarded
	}

	// The vendor answering 200 is not the same as the vendor granting.  Report
	// the refusal as a refusal: the operator must not be told they got a reward
	// that never landed.
	if !reply.Granted {
		res.Error = signInRefusal(reply)
		return finishSignIn(res, start), nil
	}
	res.OK = true
	res.Message = signInSuccess(reply)
	return finishSignIn(res, start), nil
}

// signInRefusal explains a 200 that granted nothing, in the operator's language
// and without inventing a reason the vendor did not give.
func signInRefusal(r signInReply) string {
	if m := strings.TrimSpace(r.Message); m != "" {
		return "Tabbit did not grant the daily sign-in this time: " + m
	}
	return "Tabbit did not grant the daily sign-in this time (it is most likely already signed in today)"
}

// signInSuccess reports what the vendor actually said it handed out.  The
// amount is the vendor's own number, never a hard-coded percentage: the bundle
// documents no amount, so this module does not claim one.
func signInSuccess(r signInReply) string {
	msg := "Tabbit granted the daily sign-in"
	if r.Rewarded > 0 {
		msg += fmt.Sprintf(" (+%g)", r.Rewarded)
	}
	if r.Streak > 0 {
		msg += fmt.Sprintf(" · %d-day streak", r.Streak)
	}
	if m := strings.TrimSpace(r.Message); m != "" {
		msg += " · " + m
	}
	return msg
}

// finishSignIn stamps the elapsed time and the local clock reading, so the panel
// can show how long the round trip took.
func finishSignIn(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.At = time.Now().Format(time.RFC3339)
	return res
}

// webSignInStatus reads whether the sign-in is already recorded today.  It is
// the read side of the same feature: the panel can call it to explain a refusal
// without the operator clicking first.
func (c *Client) webSignInStatus(ctx context.Context, wa webAuth) (webSignInStatus, error) {
	var st webSignInStatus
	q := url.Values{"scene_codes": {webSignInScene}}
	resp, err := c.webDo(ctx, wa, http.MethodGet, webSignInStatusPath, q, nil, "application/json")
	if err != nil {
		return st, fmt.Errorf("reading the Tabbit sign-in status: %w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		return st, webError(wa, "GET "+webSignInStatusPath, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &st); err != nil {
		return st, fmt.Errorf("parsing the Tabbit sign-in status: %w", err)
	}
	return st, nil
}

// hasWebTokenAccount reports whether at least one stored account can actually
// reach the vendor.  CheckinActions uses it so the panel never shows a button
// that is guaranteed to fail.
func (c *Client) hasWebTokenAccount() bool {
	if c == nil {
		return false
	}
	for _, ep := range c.webAccounts() {
		if ep.Enabled && strings.TrimSpace(ep.Token) != "" {
			return true
		}
	}
	return false
}

// Compile-time proof that this module satisfies the optional contract.
var _ core.CheckinProvider = (*Client)(nil)
