package zcode

// The marketing touch channel is how the vendor's client gets its promotional
// popups -- the "ZCODE WEEKEND BUILD" dialog that offers a weekend plan is one
// of them.  The flow has two halves, both measured live on 2026-10-30:
//
//   - A delivery query.  GET marketing/touch answers with a list of pending
//     deliveries for this (device, account) pair.  Each carries a campaign_id
//     and a rendered popup whose buttons carry typed actions; the WEEKEND one
//     carries a claim_zcode_plan action with the plan id inside.
//   - A confirmation.  POST marketing/touch/action with {campaign_id,
//     action_type:"confirm"} is what pressing the popup's button reports.  The
//     vendor treats that confirmation as the operator's consent and makes the
//     plan claimable -- a billing claim for the same plan id answers 1004
//     ("not eligible") before it and succeeds after it.  The claim itself is
//     the billing flow claim.go already implements.
//
// The schemas are ported from the desktop client's own validator (app.asar):
// a delivery is either a banner or a popup, both carry campaign_id (1..128
// chars) and priority (0..100); a popup button action is one of close,
// open_url, claim_zcode_plan{plan_id}, navigate, copy_text.
//
// Two measured quirks shape the code.  This endpoint hangs a proxied request
// -- the desktop client connects directly -- so marketing runs on the module's
// direct client (see New: same TLS profile, no proxy), never on the shared
// one.  And the query answers anonymously with an empty delivery list, so an
// empty answer says nothing about the credential; only a listed campaign does.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const (
	marketingTouchPath  = "/api/v1/marketing/touch"
	marketingActionPath = "/api/v1/marketing/touch/action"

	// marketingConfirm is the action_type the client sends when the operator
	// presses the popup's primary button.  "cancel" is the dismiss path and is
	// deliberately never sent: an unattended loop has no business declining a
	// campaign on the operator's behalf.
	marketingConfirm = "confirm"

	// marketingMaxDeliveries bounds one query's work, mirroring the client's
	// own max(32) on the response array.
	marketingMaxDeliveries = 32
)

// marketingAction names the typed action a popup button carries.
type marketingAction struct {
	Type string `json:"type"`
	Args struct {
		PlanID string `json:"plan_id"`
		URL    string `json:"url"`
		Text   string `json:"text"`
	} `json:"args"`
}

// marketingButton is one popup button.  Only the action is interesting here:
// text and theme are presentation, and the claim decision is "confirm the
// campaign that advertises a plan claim", not "read the copy".
type marketingButton struct {
	Action marketingAction `json:"action"`
}

// marketingPopup is the popup variant of a delivery.
type marketingPopup struct {
	Buttons []marketingButton `json:"buttons"`
}

// marketingDelivery is one pending vendor campaign.  Banner and popup share
// the campaign fields; only the popup variant is read for plan actions.
type marketingDelivery struct {
	CampaignID       string         `json:"campaign_id"`
	Priority         int            `json:"priority"`
	ResourcePosition string         `json:"resource_position"`
	Popup            marketingPopup `json:"popup"`
}

// marketingTouchResponse is the delivery query's envelope.
type marketingTouchResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		ServerTime int64               `json:"server_time"`
		Language   string              `json:"language"`
		Deliveries []marketingDelivery `json:"deliveries"`
	} `json:"data"`
}

// marketingActionResponse is the confirmation's envelope; the client insists
// on code 0 and reads nothing else.
type marketingActionResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// marketingDo performs one marketing-channel request on the module's direct
// client.  It mirrors planDo's identity headers and adds the account's plan
// JWT when there is one: the delivery list is keyed on the (device, account)
// pair, and the desktop client sends its credential here exactly as it does to
// the billing endpoints.
func (c *Client) marketingDo(ctx context.Context, method, path string, acct *Account, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("zcode: encode %s body: %w", path, err)
		}
		rdr = strings.NewReader(string(raw))
	}

	req, err := http.NewRequestWithContext(ctx, method, configHost+path, rdr)
	if err != nil {
		return fmt.Errorf("zcode: build %s request: %w", path, err)
	}
	c.applyIdentityHeaders(req.Header)
	if body != nil {
		setHeader(req.Header, "Content-Type", "application/json")
	}
	if acct != nil && acct.Mode == modeJWT && acct.jwt != "" {
		setHeader(req.Header, "Authorization", "Bearer "+acct.jwt)
	}

	if c.directHTTP == nil {
		return fmt.Errorf("zcode: %s needs a direct connection and none was built", path)
	}
	resp, err := c.directHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("zcode: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw := readLimited(resp.Body, claimMaxBytes)
	if out == nil {
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("zcode: %s answered HTTP %d: %s", path, resp.StatusCode, clip(string(raw), 200))
		}
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("zcode: decode %s: %w (body: %s)", path, err, clip(string(raw), 200))
	}
	return nil
}

// marketingDeliveries asks the vendor what campaigns are pending for this
// account.  An empty list is the steady answer outside a campaign window; a
// transport failure is reported so the caller can tell "nothing today" from
// "the question never got through".
func (c *Client) marketingDeliveries(ctx context.Context, acct *Account) ([]marketingDelivery, error) {
	q := url.Values{}
	q.Set("seq", "1")

	var resp marketingTouchResponse
	if err := c.marketingDo(ctx, http.MethodGet, marketingTouchPath+"?"+q.Encode(), acct, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, &claimError{code: resp.Code, msg: strings.TrimSpace(resp.Msg)}
	}
	out := make([]marketingDelivery, 0, len(resp.Data.Deliveries))
	for _, d := range resp.Data.Deliveries {
		if d.CampaignID == "" {
			continue
		}
		out = append(out, d)
		if len(out) >= marketingMaxDeliveries {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority > out[j].Priority
		}
		return out[i].CampaignID < out[j].CampaignID
	})
	return out, nil
}

// deliveryPlanID extracts the plan id a delivery's buttons advertise, or ""
// when the campaign carries no plan claim.  A popup may render several
// buttons; the claimable one is the claim_zcode_plan action, and the first
// wins because the client renders them in order.
func (d marketingDelivery) planID() string {
	for _, b := range d.Popup.Buttons {
		if b.Action.Type == "claim_zcode_plan" && b.Action.Args.PlanID != "" {
			return b.Action.Args.PlanID
		}
	}
	return ""
}

// marketingConfirm accepts one campaign on the operator's behalf.  The vendor
// answers code 0 and nothing else; a non-zero code is a refusal relayed as a
// claimError so the check-in result keeps the vendor's own numbers.
func (c *Client) marketingConfirm(ctx context.Context, acct *Account, campaignID string) error {
	var resp marketingActionResponse
	body := map[string]string{
		"campaign_id": campaignID,
		"action_type": marketingConfirm,
	}
	if err := c.marketingDo(ctx, http.MethodPost, marketingActionPath, acct, body, &resp); err != nil {
		return err
	}
	if resp.Code != 0 {
		return &claimError{code: resp.Code, msg: strings.TrimSpace(resp.Msg)}
	}
	return nil
}

// claimViaMarketing runs the popup flow without the popup: read the pending
// deliveries, confirm the ones that advertise a plan, and return the plan ids
// it confirmed, highest priority first.  A delivery without a plan action is
// left alone -- an unattended loop confirms consent for plan claims, not for
// whatever else a campaign might want to open.
func (c *Client) claimViaMarketing(ctx context.Context, acct *Account) []string {
	deliveries, err := c.marketingDeliveries(ctx, acct)
	if err != nil {
		c.pool.log("zcode: marketing deliveries for %s failed: %v", acct.ID, err)
		return nil
	}
	var confirmed []string
	for _, d := range deliveries {
		planID := d.planID()
		if planID == "" {
			continue
		}
		if err := c.marketingConfirm(ctx, acct, d.CampaignID); err != nil {
			c.pool.log("zcode: confirming campaign %s for %s refused: %v", d.CampaignID, acct.ID, err)
			continue
		}
		c.pool.log("zcode: confirmed campaign %s (plan %s) for %s", d.CampaignID, planID, acct.ID)
		confirmed = append(confirmed, planID)
	}
	return confirmed
}
