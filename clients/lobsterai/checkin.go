package lobsterai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"client2api/internal/core"
)

// checkinActionID is the panel's action id.
const checkinActionID = "check-in"

// checkinUpstreamAction is the action name the vendor's activity context has to
// offer before a check-in is attempted.
const checkinUpstreamAction = "check_in"

// CheckinActions offers the button only when an account could actually use it.
// A button that cannot work is a lie.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	for _, acct := range c.pool.snapshot() {
		if acct.Enabled && acct.AccessToken != "" {
			return []core.CheckinAction{{
				ID:    checkinActionID,
				Label: "Daily check-in",
				Help:  "Claims today's LobsterAI activity credits (about 100 per account per day).",
			}}
		}
	}
	return nil
}

// Checkin runs the vendor's three-call sequence for one account:
//
//	GET  /api/client-activities/slot?placement=desktop_sidebar&clientVersion=..&containerApiVersion=2&platform=win32
//	GET  /api/client-activities/{code}/context?configRevision={rev}
//	POST /api/client-activities/{code}/actions/check_in  {configRevision, idempotencyKey, payload:{}}
//
// No signature is involved.  The whole sequence is reported inside the result:
// a refusal from the vendor is an answer, not a Go error.  A Go error means the
// attempt could not be made at all.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	start := c.now()
	finish := func(r core.CheckinResult) core.CheckinResult {
		now := c.now()
		r.ElapsedMS = now.Sub(start).Milliseconds()
		r.At = now.Format(time.RFC3339)
		return r
	}

	res := core.CheckinResult{AccountID: id, Action: action}
	if action != "" && action != checkinActionID {
		res.Error = fmt.Sprintf("unknown action %q", action)
		return finish(res), nil
	}
	acct := c.pool.byID(id)
	if acct == nil {
		return core.CheckinResult{}, fmt.Errorf("account %q not found", id)
	}
	if !acct.Enabled {
		res.Error = "this account is disabled"
		return finish(res), nil
	}
	if acct.AccessToken == "" {
		res.Error = "this account has no access token; sign in again"
		return finish(res), nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.checkinTimeout())
	defer cancel()
	if err := c.ensureFresh(ctx, acct); err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	// The version is a required query parameter here, and this is an
	// operator-triggered action rather than a hot path, so it is worth a
	// bounded synchronous fetch.  A failure just leaves the fallback in place.
	c.refreshVersionIfStale(ctx)
	version := c.clientVersion(ctx)

	// Step 1: which activity is on offer right now?
	slot, err := c.activityCall(ctx, acct, http.MethodGet, c.cfg.activitySlotURL(version), nil, version)
	if err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	activity := objectOf(slot["activity"])
	if firstString(slot, "slotState") != "available" || activity == nil {
		res.Error = "no LobsterAI activity is available right now"
		return finish(res), nil
	}
	code := firstString(activity, "activityCode")
	if code == "" {
		res.Error = "the activity slot carried no activity code"
		return finish(res), nil
	}
	// The revision is echoed back verbatim in the POST body, so it is kept in
	// whatever JSON shape the vendor used rather than forced into a string.
	revision := activity["configRevision"]

	// Step 2: has it already been claimed today, and is check_in on offer?
	actCtx, err := c.activityCall(ctx, acct, http.MethodGet, c.cfg.activityContextURL(code, asString(revision)), nil, version)
	if err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	if state := objectOf(actCtx["state"]); boolOf(state["claimedToday"]) {
		res.OK = true
		res.Message = "already claimed today"
		res.Data = map[string]any{"already_done": true, "activity_code": code}
		return finish(res), nil
	}
	actions := stringsOf(actCtx["actions"], "action", "id", "type", "name")
	if !containsString(actions, checkinUpstreamAction) {
		res.Error = fmt.Sprintf("the activity does not offer a %s action", checkinUpstreamAction)
		return finish(res), nil
	}

	// Step 3: claim it.  The idempotency key makes a retry safe.
	key, err := newUUIDErr()
	if err != nil {
		res.Error = "could not mint an idempotency key: " + err.Error()
		return finish(res), nil
	}
	payload, err := json.Marshal(map[string]any{
		"configRevision": revision,
		"idempotencyKey": key,
		"payload":        map[string]any{},
	})
	if err != nil {
		res.Error = "could not build the check-in body: " + err.Error()
		return finish(res), nil
	}
	claimed, err := c.activityCall(ctx, acct, http.MethodPost, c.cfg.activityCheckinURL(code), payload, version)
	if err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	result := objectOf(claimed["result"])
	if result == nil {
		result = claimed
	}
	// The vendor has used three different names for the same number.
	credits := firstInt(result, "creditsGranted", "rewardCredits", "credits")
	res.OK = true
	res.Message = fmt.Sprintf("claimed %d credits", credits)
	res.Data = map[string]any{"activity_code": code, "credits": credits}
	c.noteSuccess(acct)
	return finish(res), nil
}

// activityCall performs one check-in call and unwraps the {code,msg,data}
// envelope.  Check-in endpoints are enveloped, unlike chat.
func (c *Client) activityCall(ctx context.Context, acct *accountRecord, method, url string, body []byte, version string) (map[string]any, error) {
	spec := requestSpec{
		method:  method,
		url:     url,
		body:    body,
		bearer:  acct.AccessToken,
		version: version,
		timeout: c.cfg.checkinTimeout(),
	}
	status, respBody, err := c.doJSON(ctx, spec)
	if err != nil {
		return nil, c.classifyErrFor(acct, err)
	}
	if status != http.StatusOK {
		return nil, c.classifyHTTP("checkin", acct.ID, status, respBody)
	}
	raw, err := decodeEnvelope("checkin", respBody)
	if err != nil {
		return nil, err
	}
	obj := objectOf(anyOf(raw))
	if obj == nil {
		return nil, fmt.Errorf("checkin: the response data was not an object")
	}
	return obj, nil
}

var _ core.CheckinProvider = (*Client)(nil)
