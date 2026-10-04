package codearts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// credits.go is the free-quota side of CodeArts: the daily check-in and the
// credit balance it feeds.
//
// All three endpoints live on the snap engine and are signed the same way as
// the model catalogue — with the unsigned Agent-Type / X-Language pair
// appended after signing:
//
//	GET  /snap-manager/v1/statistics/plugin   account and package information
//	GET  /v1/ops/delivery?channel=IDE         the activity list
//	POST /v1/ops/claim                        claim one activity
//	POST /v1/ops/confirm                      confirm a claim the vendor
//	                                          wants acknowledged
//
// Two shapes are worth stating because getting them wrong is silent:
//
//   - statistics/plugin answers with a BARE object, not a {code,message,data}
//     envelope, while the ops endpoints use the envelope.
//   - the activity fields are `campaignId` (a number, not a string),
//     `benefitAmount` (not `amount`), `claimable`, `status`, `type`, `title`.
//
// The check-in is gated on the account having a credit package: an account
// without one has nothing to claim, and reporting a claim it cannot make would
// be a lie.  That case is reported as `inactive`, not as a failure.

const (
	// checkinActionID is the single action this module offers.  A module that
	// returns no actions shows no check-in button at all, which is the honest
	// answer for a vendor without a daily check-in; CodeArts has one.
	checkinActionID = "daily-login"
	// dailyLoginType is the activity type the daily check-in carries.
	dailyLoginType = "USER_LOGIN"
)

// checkinAction is the one activity this module claims.
var checkinAction = core.CheckinAction{
	ID:    checkinActionID,
	Label: "Daily sign-in",
	Help:  "Claim the CodeArts daily sign-in credit. Only accounts with a credit package can claim it.",
}

// claimedStatuses are the activity states that mean "already claimed".
var claimedStatuses = map[string]bool{
	"CLAIMED":   true,
	"CONFIRMED": true,
	"CONSUMED":  true,
}

// CheckinActions reports the check-in this module supports
// (core.CheckinProvider).  It returns one action, or none when there is no
// credential to claim for.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.pool.len() == 0 {
		return nil
	}
	return []core.CheckinAction{checkinAction}
}

// Checkin performs one check-in for one account.
//
// A vendor refusal is a RESULT, not an error: the panel shows OK=false with the
// reason, and the module keeps working.  Only a broken account id is an error.
func (c *Client) Checkin(ctx context.Context, accountID, action string) (core.CheckinResult, error) {
	started := time.Now()
	res := core.CheckinResult{AccountID: accountID, Action: action, At: time.Now().UTC().Format(time.RFC3339)}
	if action != "" && action != checkinActionID {
		return res, fmt.Errorf("codearts: unknown check-in action %q", action)
	}
	e := c.pool.pick(nil)
	if accountID != "" {
		if found := c.findEntry(accountID); found != nil {
			e = found
		} else {
			return res, fmt.Errorf("codearts: no such account %q", accountID)
		}
	}
	if e == nil {
		return res, core.ErrNotConfigured
	}
	res.AccountID = e.id()

	finish := func(r core.CheckinResult, err error) (core.CheckinResult, error) {
		r.ElapsedMS = time.Since(started).Milliseconds()
		return r, err
	}

	info, err := c.fetchStatistics(ctx, e.account())
	if err != nil {
		res.Message = core.Redact(err.Error())
		return finish(res, nil)
	}
	if !info.hasCreditPackage() {
		res.Message = "this account has no credit package, so it has nothing to claim"
		res.Data = map[string]any{"status": "inactive"}
		return finish(res, nil)
	}

	items, err := c.fetchActivities(ctx, e.account())
	if err != nil {
		res.Message = core.Redact(err.Error())
		return finish(res, nil)
	}
	activity, ok := findDailyLogin(items)
	if !ok {
		res.Message = "the vendor did not offer a daily sign-in activity"
		return finish(res, nil)
	}
	if activity.claimed() {
		res.OK = true
		res.Message = "already claimed today"
		res.Data = map[string]any{"status": activity.Status}
		return finish(res, nil)
	}
	if !activity.Claimable {
		res.Message = "the daily sign-in is not claimable right now"
		res.Data = map[string]any{"status": activity.Status}
		return finish(res, nil)
	}

	claim, err := c.claimActivity(ctx, e.account(), activity.CampaignID)
	if err != nil {
		res.Message = core.Redact(err.Error())
		return finish(res, nil)
	}
	res.Data = map[string]any{"campaign_id": activity.CampaignID, "benefit_amount": activity.BenefitAmount}
	// Some claims must be acknowledged before they count.  The reference
	// implementation confirms exactly when the claim response carries an id.
	if claim.ID != 0 {
		if err := c.confirmActivity(ctx, e.account(), activity.CampaignID); err != nil {
			res.Message = "claimed, but the confirmation failed: " + core.Redact(err.Error())
			return finish(res, nil)
		}
	}
	res.OK = true
	res.Message = "claimed"
	return finish(res, nil)
}

// findEntry returns a pooled entry by id, or nil.
func (c *Client) findEntry(id string) *entry {
	id = strings.TrimSpace(id)
	for _, e := range c.pool.all() {
		if e.id() == id {
			return e
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the endpoints
// ---------------------------------------------------------------------------

// signedSnap performs one signed request against the snap engine with the
// unsigned Agent-Type / X-Language pair the credits endpoints expect.
func (c *Client) signedSnap(ctx context.Context, acct account, method, path string, body []byte, out any) error {
	rawURL := c.cfg.endpoint(path)
	unsigned := map[string]string{
		"Agent-Type": "PromptCenter",
		"X-Language": "zh-cn",
	}
	if body == nil {
		unsigned["Content-Type"] = "application/json"
	}
	return c.doJSON(ctx, acct, method, rawURL, body, nil, unsigned, out)
}

// creditMetric is one metric object inside a statistics response.  The credit
// numbers live on the metric itself; the root object's package_credit_remain
// is often absent or zero even when the account has credit left.
type creditMetric struct {
	Name           string  `json:"name"`
	Value          float64 `json:"value"`
	Amount         float64 `json:"package_credit_amount"`
	Remain         float64 `json:"package_credit_remain"`
	Used           float64 `json:"package_credit_used"`
	ExpiringAmount float64 `json:"package_credit_expiring_amount"`
	Show           bool    `json:"show"`
}

// creditTotals is what the panel needs from a statistics response, in the
// vendor's own credit unit.  Used is kept fractional because the vendor reports
// hundredths even when both Credits and Total are whole numbers.
type creditTotals struct {
	Remain float64
	Total  float64
	Used   float64
}

// pluginStatistics is /snap-manager/v1/statistics/plugin.
//
// It answers with a bare object, so it is decoded directly rather than through
// the {code,message,data} envelope the ops endpoints use.
type pluginStatistics struct {
	Package struct {
		IsCreditPackage bool `json:"is_credit_package"`
	} `json:"package"`
	Metrics             []creditMetric `json:"metrics"`
	PackageCreditRemain float64        `json:"package_credit_remain"`
}

// hasCreditPackage reports whether the account has anything to claim against.
func (s *pluginStatistics) hasCreditPackage() bool {
	return s != nil && s.Package.IsCreditPackage
}

// creditTotals reads the remaining, total and consumed credit from the metrics
// list.
//
// The total metric is authoritative when it is present; only when it is absent
// are the per-category metrics summed.  A metric that is absent is NOT the same
// as a metric that is zero, so the distinction is preserved.
func (s *pluginStatistics) creditTotals() (creditTotals, bool) {
	if s == nil {
		return creditTotals{}, false
	}
	for _, m := range s.Metrics {
		if m.Name == "usageTotalPackageCredit" {
			return creditTotals{Remain: m.Remain, Total: m.Amount, Used: m.Used}, true
		}
	}
	var out creditTotals
	found := false
	for _, m := range s.Metrics {
		switch m.Name {
		case "usageBasicPackageCredit", "usageOnDemandPackageCredit", "usageBonusPackageCredit":
			out.Remain += m.Remain
			out.Total += m.Amount
			out.Used += m.Used
			found = true
		}
	}
	if found {
		return out, true
	}
	if s.PackageCreditRemain > 0 {
		return creditTotals{Remain: s.PackageCreditRemain}, true
	}
	return creditTotals{}, false
}

// creditTotal preserves the old one-number helper for callers that only need
// the remaining figure.
func (s *pluginStatistics) creditTotal() (float64, bool) {
	totals, ok := s.creditTotals()
	return totals.Remain, ok
}

// fetchStatistics reads the account's package and credit information.
func (c *Client) fetchStatistics(ctx context.Context, acct account) (*pluginStatistics, error) {
	var out pluginStatistics
	if err := c.signedSnap(ctx, acct, http.MethodGet, statisticsPath, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// opsEnvelope is the {code,message,data} wrapper the ops endpoints use.
type opsEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// ok reports whether the envelope says the call succeeded.
func (e *opsEnvelope) ok() bool {
	return e != nil && (e.Code == 0 || e.Code == 200)
}

// activity is one claimable activity.
//
// The field names are the vendor's, including the two that differ from what
// one would guess: the id is `campaignId` (a number) and the amount is
// `benefitAmount` rather than `amount`.
type activity struct {
	CampaignID    int64   `json:"campaignId"`
	BenefitAmount float64 `json:"benefitAmount"`
	Claimable     bool    `json:"claimable"`
	Status        string  `json:"status"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
}

// claimed reports whether this activity has already been claimed.
func (a activity) claimed() bool {
	return claimedStatuses[strings.ToUpper(strings.TrimSpace(a.Status))]
}

// activityPage is the `data` object of the delivery response.
type activityPage struct {
	Items []activity `json:"items"`
}

// fetchActivities reads the activity list.
func (c *Client) fetchActivities(ctx context.Context, acct account) ([]activity, error) {
	var env opsEnvelope
	if err := c.signedSnap(ctx, acct, http.MethodGet, deliveryPath+"?channel=IDE", nil, &env); err != nil {
		return nil, err
	}
	if !env.ok() {
		return nil, fmt.Errorf("codearts: the activity list was refused: %s", cleanErrorText(env.Message))
	}
	var page activityPage
	if err := json.Unmarshal(env.Data, &page); err != nil {
		return nil, fmt.Errorf("codearts: the activity list is not the JSON it should be: %w", err)
	}
	return page.Items, nil
}

// claimResponse is the answer to a claim.
type claimResponse struct {
	ID int64 `json:"id"`
}

// claimActivity claims one activity.
func (c *Client) claimActivity(ctx context.Context, acct account, campaignID int64) (claimResponse, error) {
	body, err := json.Marshal(map[string]any{"campaignId": campaignID, "channel": "IDE"})
	if err != nil {
		return claimResponse{}, err
	}
	var env opsEnvelope
	if err := c.signedSnap(ctx, acct, http.MethodPost, claimPath, body, &env); err != nil {
		return claimResponse{}, err
	}
	if !env.ok() {
		return claimResponse{}, fmt.Errorf("codearts: the claim was refused: %s", cleanErrorText(env.Message))
	}
	var out claimResponse
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &out); err != nil {
			// A claim that succeeded but answered an unexpected shape is not a
			// failure: the credit is granted either way.
			return claimResponse{}, nil
		}
	}
	return out, nil
}

// confirmActivity acknowledges a claim the vendor wants confirmed.
func (c *Client) confirmActivity(ctx context.Context, acct account, campaignID int64) error {
	body, err := json.Marshal(map[string]any{"campaignId": campaignID})
	if err != nil {
		return err
	}
	var env opsEnvelope
	if err := c.signedSnap(ctx, acct, http.MethodPost, confirmPath, body, &env); err != nil {
		return err
	}
	if !env.ok() {
		return fmt.Errorf("codearts: the confirmation was refused: %s", cleanErrorText(env.Message))
	}
	return nil
}

// findDailyLogin picks the daily sign-in activity out of the list.
//
// The type is the vendor's `USER_LOGIN`.  The list can carry other activities
// (seasonal campaigns, for instance), so matching on the type rather than on
// position is what keeps the module claiming the right one.
func findDailyLogin(items []activity) (activity, bool) {
	for _, a := range items {
		if strings.EqualFold(strings.TrimSpace(a.Type), dailyLoginType) {
			return a, true
		}
	}
	return activity{}, false
}

// ---------------------------------------------------------------------------
// balance
// ---------------------------------------------------------------------------

// AccountBalance reports the account's remaining credit (core.BalanceProvider).
//
// The credit is reported in the vendor's own unit, which is what
// core.Balance.Unit is for.  A vendor error here IS an error — the panel shows
// it rather than a fabricated zero — but a missing metric is reported as an
// empty balance with a note, because "the vendor did not say" is not the same
// as "you have nothing left".
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	e := c.findEntry(id)
	if e == nil {
		e = c.pool.pick(nil)
	}
	if e == nil {
		return core.Balance{}, core.ErrNotConfigured
	}
	info, err := c.fetchStatistics(ctx, e.account())
	if err != nil {
		return core.Balance{}, c.terminal(err, e.id())
	}
	totals, ok := info.creditTotals()
	if !ok {
		return core.Balance{Unit: "积分"}, nil
	}
	return core.Balance{
		Credits: int64(totals.Remain),
		Used:    totals.Used,
		Total:   int64(totals.Total),
		Unit:    "积分",
	}, nil
}

// checkinPaths are the paths the credits endpoints live at, relative to the
// snap origin.
const (
	statisticsPath = "/snap-manager/v1/statistics/plugin"
	deliveryPath   = "/v1/ops/delivery"
	claimPath      = "/v1/ops/claim"
	confirmPath    = "/v1/ops/confirm"
)
