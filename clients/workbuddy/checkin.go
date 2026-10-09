package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// Daily rewards.
//
// WorkBuddy grants a reward once per calendar day, but the two realms spell it
// differently and hit different hosts:
//
//   - CN      — a real check-in: POST <billing>/v2/billing/meter/daily-checkin
//     with an empty JSON object.  The billing host is separate from the chat
//     host and uses the lighter BillingHeaders set, not the CLI identity.
//   - intl    — no check-in at all.  The only thing credited is starting a web
//     conversation, which is why this path posts to the console host rather
//     than to the chat completion endpoint.
//
// The reference implementation gates its *scheduler* on a locally stored stamp,
// but its panel route calls the endpoint unconditionally, so that a manual
// click always reports what the upstream actually says.  This module does the
// same: the stamp is recorded for display, never used to short-circuit the
// request.
const (
	checkinActionCN   = "daily-checkin"
	checkinActionIntl = "daily-activity"

	// The CN billing endpoint answers 10001 when today's reward already
	// exists.  The reference implementation treats it as success.
	checkinCodeAlreadyDone = 10001

	// dailyActivityPath is the console route that creates a web conversation.
	dailyActivityPath = "/console/as/conversations/"

	dailyActivityModel  = "deepseek-v4.1-flash"
	dailyActivityPrompt = "Hi"

	checkinTimeout = 30 * time.Second
	checkinMsgMax  = 300

	// The console route is a browser route; it rejects non-browser agents.
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

// CheckinActions reports what the configured accounts can actually do today.
// A client with no accounts advertises nothing, so the panel shows no button
// that could only fail.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if ctx != nil && ctx.Err() != nil {
		return nil
	}
	c.refreshAccounts(false)
	var cn, intl bool
	for _, a := range c.pool.Accounts() {
		if a.IsGlobal() {
			intl = true
		} else {
			cn = true
		}
	}
	out := make([]core.CheckinAction, 0, 2)
	if cn {
		out = append(out, core.CheckinAction{
			ID:    checkinActionCN,
			Label: "Daily check-in (CN)",
			Help: "POST " + dailyCheckinPathV2 + " on the CN billing host. Rewards are granted once per " +
				"calendar day; the upstream answers code 10001 when today's reward already exists, which counts as success.",
		})
	}
	if intl {
		out = append(out, core.CheckinAction{
			ID:    checkinActionIntl,
			Label: "Daily activity (international)",
			Help: "Creates one web conversation. The international realm credits no check-in, and only the " +
				"console route counts as activity.",
		})
	}
	return out
}

// Checkin performs one daily-reward action.  An upstream refusal is reported as
// CheckinResult{OK:false} with a nil error; only an unknown account (or a
// request that could not be attempted at all) returns a Go error.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.refreshAccounts(false)
	a := c.findAuth(id)
	if a == nil {
		if _, parked := c.findDisabled(id); parked {
			return core.CheckinResult{
				OK:        false,
				AccountID: id,
				Action:    action,
				Error:     "the account is parked; enable it before claiming a reward",
			}, nil
		}
		return core.CheckinResult{}, fmt.Errorf("account %q not found", id)
	}

	global := a.IsGlobal()
	if strings.TrimSpace(action) == "" {
		if global {
			action = checkinActionIntl
		} else {
			action = checkinActionCN
		}
	}
	res := core.CheckinResult{
		AccountID: a.ID(),
		Action:    action,
		At:        time.Now().UTC().Format(time.RFC3339),
	}

	switch action {
	case checkinActionCN:
		if global {
			res.Error = "the daily check-in exists only on the CN realm; use " + checkinActionIntl
			return res, nil
		}
		return c.checkinCN(ctx, a, res), nil
	case checkinActionIntl:
		if !global {
			res.Error = "daily activity is reported only by the international realm; use " + checkinActionCN
			return res, nil
		}
		return c.dailyActivity(ctx, a, res), nil
	default:
		res.Error = fmt.Sprintf("unknown action %q", action)
		return res, nil
	}
}

// checkinCN posts the CN check-in and stamps the account on success.
func (c *Client) checkinCN(ctx context.Context, a *Auth, res core.CheckinResult) core.CheckinResult {
	paths := c.up.checkinMeterPaths(a)
	if len(paths) == 0 {
		res.Error = "no check-in path is configured for this realm"
		return res
	}
	endpoint := c.up.billingBase(a) + paths[0]

	start := time.Now()
	env, err := c.postJSON(ctx, endpoint, []byte("{}"), func(req *http.Request) {
		c.up.BillingHeaders(req, a)
	})
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		c.pool.MarkFailure(a, err)
		res.Error = core.Redact(describeFailure(err))
		return res
	}

	res.Code = env.Code
	res.Message = core.Redact(truncate(strings.TrimSpace(env.Msg), checkinMsgMax))
	if env.Code == 0 || env.Code == checkinCodeAlreadyDone {
		res.OK = true
		a.MarkCheckin()
		if serr := a.SaveAtomic(); serr != nil {
			c.deps.Logf("workbuddy: check-in succeeded but the stamp could not be saved: %v", serr)
		}
		c.pool.MarkNonChatSuccess(a)
		res.Data = map[string]any{
			"last_checkin": a.LastCheckinValue(),
			"already_done": env.Code == checkinCodeAlreadyDone,
		}
		return res
	}

	res.Error = fmt.Sprintf("the upstream refused the check-in: code=%d %s", env.Code, res.Message)
	return res
}

// dailyActivity creates the web conversation the international realm counts as
// a day of activity.
func (c *Client) dailyActivity(ctx context.Context, a *Auth, res core.CheckinResult) core.CheckinResult {
	endpoint := c.up.webBase(a) + dailyActivityPath
	body, err := json.Marshal(map[string]any{
		"prompt":             dailyActivityPrompt,
		"model":              dailyActivityModel,
		"conversationOrigin": "workbuddy-app",
		"plugins": []map[string]string{
			{"name": "weixinpay", "marketplace": "codebuddy-builtin"},
		},
	})
	if err != nil {
		res.Error = "could not build the request body: " + err.Error()
		return res
	}

	start := time.Now()
	env, err := c.postJSON(ctx, endpoint, body, func(req *http.Request) {
		c.webHeaders(req, a)
	})
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = core.Redact(describeFailure(err))
		return res
	}

	res.Code = env.Code
	res.Message = core.Redact(truncate(strings.TrimSpace(env.Msg), checkinMsgMax))
	var payload struct {
		ID string `json:"id"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &payload)
	}
	if env.Code == 0 && strings.TrimSpace(payload.ID) != "" {
		res.OK = true
		a.MarkDailyChat()
		if serr := a.SaveAtomic(); serr != nil {
			c.deps.Logf("workbuddy: daily activity succeeded but the stamp could not be saved: %v", serr)
		}
		res.Data = map[string]any{
			"conversation_id": payload.ID,
			"last_daily_chat": a.LastDailyChatValue(),
		}
		return res
	}

	if res.Error == "" {
		res.Error = "the web conversation was not created, so the day is probably not credited"
	}
	return res
}

// webHeaders mirrors the browser route the console expects.  It deliberately
// does not use the CLI identity headers the chat path sends.
func (c *Client) webHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if uid := a.UIDValue(); uid != "" {
		req.Header.Set("X-User-Id", uid)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	origin := c.up.webBase(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/app")
	if ua := strings.TrimSpace(c.up.UserAgent); ua != "" {
		req.Header.Set("User-Agent", ua)
	} else {
		req.Header.Set("User-Agent", webUserAgent)
	}
}

// postJSON performs a POST and returns the gateway envelope *without* judging
// its business code, because these routes have more than one success code.
// Only transport failures and HTTP 4xx/5xx are errors here.
func (c *Client) postJSON(ctx context.Context, endpoint string, body []byte, decorate func(*http.Request)) (apiEnvelope, error) {
	var env apiEnvelope
	hc := c.up.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, checkinTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return env, err
	}
	if decorate != nil {
		decorate(req)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return env, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return env, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		// WorkBuddy reports the idempotent "already checked in today"
		// outcome as HTTP 400 plus its normal success business code.  The
		// caller must see that envelope so the task centre can count it as a
		// success (or at least a no-op), not as an upstream transport error.
		if resp.StatusCode == http.StatusBadRequest {
			if err := json.Unmarshal(raw, &env); err == nil && env.Code == checkinCodeAlreadyDone {
				return env, nil
			}
		}
		return env, &Error{
			Kind:   Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    truncate(string(raw), 200),
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 200))
	}
	return env, nil
}
