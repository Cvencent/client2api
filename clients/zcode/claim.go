package zcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// ZCode's promotional plans ("活动套餐") are served from the plan host next to
// the start-plan chat channel, and they are gated the same way: a plan JWT, plus
// for the claim call itself a fresh Aliyun traceless-verification parameter that
// only a real browser can mint.
//
// This file ports the zcode-switch reference's claim flow -- preview, pick,
// claim, map the vendor's business codes.  Two ways to obtain the token are
// wired in: the operator's browser, via the panel (see captcha.go), and the
// captcha_command seam the JWT chat channel already needs.  Neither ships with
// this module and nothing here invents one: with neither available the action
// reports that it is not configured instead of guessing at a token.
//
// Two things are deliberately NOT ported from the reference.  It keeps a
// server-side idempotency key; the vendor has none, so the guard here is the
// same one it uses -- re-preview before every attempt and treat the vendor's
// 1003 as the authority.  And it ticks on a 10-minute timer from its frontend.
//
// This file used to expose the flow as an operator-driven action only, on the
// grounds that an unattended loop spending a browser-run captcha every ten
// minutes is a decision for the operator rather than a default.  The operator
// has since made that decision, so tasks.go now wires the flow into the
// scheduler -- but at the scheduler's "checkin" cadence (09:00 and 21:00 in
// configs/client2api.json), not every ten minutes, and a run with no solver
// configured refuses in one line instead of retrying.  The action itself is
// unchanged: the panel's manual button and the scheduler call the same
// Checkin.
const (
	planPreviewPath = "/api/v1/zcode-plan/billing/preview"
	planClaimPath   = "/api/v1/zcode-plan/billing/claim"
	planBalancePath = "/api/v1/zcode-plan/billing/balance"

	// eventReportPath is the vendor's client-telemetry endpoint.  The claim
	// flow depends on it -- see reportActivation.
	eventReportPath = "/api/v1/event/report"

	// claimAction is the only action this module offers.
	claimAction = "claim"

	// claimMaxBytes bounds a billing response.  The live documents are ~3 KB;
	// the ceiling exists so a hostile or broken upstream cannot stream forever.
	claimMaxBytes = 1 << 20

	// claimMaxPlans bounds one action's work.  A preview that lists more plans
	// than this is not a promotion, and each extra plan costs a captcha.
	claimMaxPlans = 5
)

// activationEvents are reported, in this order, immediately before every
// preview.
//
// The vendor decides whether to hand out the daily plan from the account's
// liveness signal, so a preview sent without them is answered with an empty
// plan list however eligible the account is.  The reference measured both
// sides: `{"code":0,"data":{"plans":[]}}` without the pair, and
// `{"code":0,"data":{"plans":[{"plan_id":"zcode-v3-start-plan-trust-..."}]}}`
// with it.  Its own conclusion is worth repeating here, because the empty
// answer looks like a vendor decision and is not one: the daily grant is not a
// random push, it is a server decision made from activity.
var activationEvents = []string{"app_launch", "app_daily_active"}

// activationPlatform is the platform value the telemetry endpoint is told.
//
// It is deliberately NOT c.cfg.Identity.Platform.  The three endpoints do not
// agree on what they accept: the captcha-config route answers `platform=win32`
// with HTTP 400 code 3001 and wants the configured identity (or `unknown`),
// while this route is the one the reference feeds this literal.  So each value
// is the one measured to work for its own endpoint, never a copy of another
// endpoint's parameter.
const activationPlatform = "win32"

// The vendor's claim business codes.  They are relayed, not translated: the
// panel's account table is the operator's only window into what the vendor
// decided, and "1003" is what the vendor's own client shows.
const (
	claimCodeNoPlan         = 1001
	claimCodeUnavailable    = 1002
	claimCodeAlreadyClaimed = 1003
	claimCodeNotEligible    = 1004
	claimCodeDailyQuota     = 1005
	claimCodeBadArgs        = 3001
	claimCodeCaptcha        = 3007
)

// claimCodeText is the reference's message table for a claim business code.
func claimCodeText(code int) string {
	switch code {
	case claimCodeNoPlan:
		return "no such plan"
	case claimCodeUnavailable:
		return "the promotion has ended or this plan cannot be claimed yet"
	case claimCodeAlreadyClaimed:
		return "this plan has already been claimed"
	case claimCodeNotEligible:
		return "this account is not eligible for the plan"
	case claimCodeDailyQuota:
		return "today's claim quota is used up"
	case claimCodeBadArgs:
		return "the claim arguments were rejected"
	case claimCodeCaptcha:
		return "the Aliyun captcha was rejected"
	default:
		return ""
	}
}

// claimError is a vendor refusal in business-code form.  It is an error for the
// panel's balance route and a fact for the check-in route, so it carries the
// code rather than flattening it into a string.
type claimError struct {
	code int
	msg  string
	// nextAt is the vendor's "try again at" hint, only set for a daily-quota
	// refusal that carried one.
	nextAt time.Time
}

func (e *claimError) Error() string {
	text := claimCodeText(e.code)
	if text == "" {
		text = "the vendor refused the claim"
	}
	if e.msg != "" {
		text += ": " + e.msg
	}
	if !e.nextAt.IsZero() {
		text += fmt.Sprintf(" (try again at %s)", e.nextAt.UTC().Format(time.RFC3339))
	}
	return text
}

// asClaimError extracts a vendor refusal from a wrapped error.
func asClaimError(err error) *claimError {
	var ce *claimError
	if errors.As(err, &ce) {
		return ce
	}
	return nil
}

// claimGrant is one entitlement of a plan.  The reference reads every field
// under two names because the vendor has shipped both spellings; we do the same
// so a rename on their side cannot silently drop a grant.
type claimGrant struct {
	Name        string  `json:"show_name"`
	NameAlt     string  `json:"showName"`
	Units       float64 `json:"grant_units"`
	UnitsAlt    float64 `json:"grantUnits"`
	Period      string  `json:"period"`
	Meter       string  `json:"meter"`
	UnitType    string  `json:"unit_type"`
	UnitTypeAlt string  `json:"unitType"`
}

func (g claimGrant) name() string     { return firstNonEmpty(g.Name, g.NameAlt) }
func (g claimGrant) unitType() string { return firstNonEmpty(g.UnitType, g.UnitTypeAlt) }

func (g claimGrant) units() float64 {
	if g.Units != 0 {
		return g.Units
	}
	return g.UnitsAlt
}

// claimPlan is one promotional plan the vendor is offering.
type claimPlan struct {
	ID       string       `json:"plan_id"`
	IDAlt    string       `json:"planId"`
	Name     string       `json:"name"`
	Desc     string       `json:"description"`
	Priority float64      `json:"priority"`
	Status   string       `json:"status"`
	StartsAt int64        `json:"starts_at"`
	EndsAt   int64        `json:"ends_at"`
	Grants   []claimGrant `json:"entitlements"`
}

func (p claimPlan) id() string { return firstNonEmpty(p.ID, p.IDAlt) }

// tokenGrants keeps only the model-usage token entitlements.  The vendor also
// ships time-boxed and non-token rows; the reference filters to the same shape,
// and a grant with no display name has nothing to show an operator.
func (p claimPlan) tokenGrants() []claimGrant {
	out := make([]claimGrant, 0, len(p.Grants))
	for _, g := range p.Grants {
		if g.Meter != "model_usage" || g.unitType() != "token" || g.name() == "" {
			continue
		}
		out = append(out, g)
	}
	return out
}

// grantSummary renders a plan's grants as one operator-readable line, e.g.
// "GLM-5.3 3,000,000 tokens per day".
func grantSummary(grants []claimGrant) string {
	if len(grants) == 0 {
		return ""
	}
	parts := make([]string, 0, len(grants))
	for _, g := range grants {
		parts = append(parts, fmt.Sprintf("%s %s tokens %s", g.name(), humanUnits(g.units()), periodLabel(g.Period)))
	}
	return strings.Join(parts, ", ")
}

// periodLabel turns the vendor's period token into words.
func periodLabel(period string) string {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", "one_time", "onetime", "once":
		return "one-time"
	case "daily":
		return "per day"
	case "weekly":
		return "per week"
	case "monthly":
		return "per month"
	default:
		return "per " + strings.ToLower(period)
	}
}

// humanUnits groups an integer quantity with commas.
func humanUnits(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// planEnvelope is the vendor's common response shape.
type planEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// open turns a non-zero business code into a *claimError and hands back the
// payload otherwise.  A vendor refusal is never a transport error: the code and
// the message are what the operator needs, so they survive as typed data.
func (e *planEnvelope) open(path string) (json.RawMessage, error) {
	if e.Code == 0 {
		return e.Data, nil
	}
	ce := &claimError{code: e.Code, msg: strings.TrimSpace(e.Msg)}
	// A daily-quota refusal carries the next window's end in data.plan.
	if e.Code == claimCodeDailyQuota && len(e.Data) > 0 {
		var data struct {
			Plan struct {
				EndsAt int64 `json:"ends_at"`
			} `json:"plan"`
		}
		if json.Unmarshal(e.Data, &data) == nil && data.Plan.EndsAt > 0 {
			ce.nextAt = time.Unix(data.Plan.EndsAt, 0)
		}
	}
	return nil, ce
}

// planDo performs one authenticated billing call.
//
// It reuses the identity headers the chat path sends -- which is the point of
// extracting them: a second header set would drift from the one proven against
// the live vendor -- and adds the plan JWT.  It deliberately does NOT send the
// start-plan trace trio: those three are what the chat channel needs, and the
// billing endpoints answered live without them.
func (c *Client) planDo(ctx context.Context, method, path string, acct *Account, verifyParam string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("zcode: encode %s body: %w", path, err)
		}
		rdr = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, configHost+path, rdr)
	if err != nil {
		return fmt.Errorf("zcode: build %s request: %w", path, err)
	}
	c.applyIdentityHeaders(req.Header)
	if body != nil {
		setHeader(req.Header, "Content-Type", "application/json")
	}
	if acct != nil {
		if acct.Mode != modeJWT || acct.jwt == "" {
			return fmt.Errorf("%w: plan billing needs a ZCode plan (jwt) credential; %s is %s",
				core.ErrNotConfigured, acct.ID, firstNonEmpty(acct.Mode, "unknown"))
		}
		setHeader(req.Header, "Authorization", "Bearer "+acct.jwt)
	}
	if verifyParam != "" {
		setHeader(req.Header, "X-Aliyun-Captcha-Verify-Param", verifyParam)
		// Without the matching region header the upstream answers 3007.  The
		// region that comes back with a browser-minted param wins: it says
		// where the token was actually minted.
		if region := c.captchaRegion(ctx); region != "" {
			setHeader(req.Header, "X-Aliyun-Captcha-Verify-Region", region)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("zcode: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw := readLimited(resp.Body, claimMaxBytes)
	if out == nil {
		// Only the liveness report takes this path, and it is the one caller
		// with nothing to decode -- so this is where a rejected report would
		// otherwise be indistinguishable from an accepted one.
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

// clip shortens s for an error message without splitting a rune.
func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// planEnvelopeData decodes the vendor's envelope and turns a non-zero code into
// a *claimError.  A vendor refusal is never a transport error: the code and the
// message are what the operator needs, so they survive as typed data.
func planEnvelopeData(raw json.RawMessage, path string) (json.RawMessage, error) {
	var env planEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("zcode: decode %s envelope: %w", path, err)
	}
	if env.Code != 0 {
		ce := &claimError{code: env.Code, msg: strings.TrimSpace(env.Msg)}
		// A daily-quota refusal carries the next window's end in data.plan.
		if env.Code == claimCodeDailyQuota && len(env.Data) > 0 {
			var data struct {
				Plan struct {
					EndsAt int64 `json:"ends_at"`
				} `json:"plan"`
			}
			if json.Unmarshal(env.Data, &data) == nil && data.Plan.EndsAt > 0 {
				ce.nextAt = time.Unix(data.Plan.EndsAt, 0)
			}
		}
		return nil, ce
	}
	return env.Data, nil
}

// reportActivation tells the vendor this client is alive.
//
// The two events are what the vendor keys the daily plan grant on, so they go
// out before every preview rather than once per process: the vendor dedups by
// device and date, so repeating them costs one small request and covers the
// case where the day's first preview happens before the events have landed.
//
// A failure is deliberately not fatal.  The reference swallows it for the same
// reason ("reporting failure does not block; the next call makes it up"), and
// the caller already treats an empty preview as a fact rather than an error.
// It is logged, though, so an operator can tell "the vendor says there is
// nothing today" apart from "the liveness signal never got through".
func (c *Client) reportActivation(ctx context.Context) {
	for _, event := range activationEvents {
		body := map[string]string{
			"event":       event,
			"device_mid":  c.pool.deviceMid(),
			"platform":    activationPlatform,
			"app_version": c.appVersion(),
		}
		// No credential: this endpoint is unauthenticated, and a nil account is
		// how planDo says so.  The identity headers -- including the
		// X-Device-Mid the vendor checks -- are still installed.
		if err := c.planDo(ctx, http.MethodPost, eventReportPath, nil, "", body, nil); err != nil {
			c.pool.log("zcode: %s event report failed: %v", event, err)
			return
		}
	}
}

// claimPreview lists what the vendor is currently offering this account,
// highest priority first.  An empty list is a fact ("nothing to claim"), not an
// error.
func (c *Client) claimPreview(ctx context.Context, acct *Account) ([]claimPlan, error) {
	// The preview is the request the vendor answers from liveness, so the
	// liveness report has to precede it.  Both the panel's board and the
	// scheduler's batch reach the vendor through here, so neither needs to
	// remember to do it.
	c.reportActivation(ctx)

	used := c.appVersion()
	plans, err := c.previewOnce(ctx, acct)
	if err != nil || len(plans) > 0 {
		return plans, err
	}
	// An empty plan list is also what a stale client version looks like: the
	// vendor only hands the current Start Plan to callers running a current
	// build.  Ask the official release manifest once whether a newer client
	// exists, and if it does, re-ask as that build.  The refresh is cached,
	// so at most one manifest read happens per hour, and a vendor that has
	// genuinely stopped running promotions still answers empty afterwards.
	if v := c.pool.refreshAppVersion(ctx); v != "" && v != used {
		return c.previewOnce(ctx, acct)
	}
	return plans, err
}

// previewOnce performs one preview request at the resolved app version.
func (c *Client) previewOnce(ctx context.Context, acct *Account) ([]claimPlan, error) {
	q := url.Values{}
	q.Set("app_version", c.appVersion())
	q.Set("platform", c.cfg.Identity.Platform)

	var env planEnvelope
	if err := c.planDo(ctx, http.MethodGet, planPreviewPath+"?"+q.Encode(), acct, "", nil, &env); err != nil {
		return nil, err
	}
	data, err := env.open(planPreviewPath)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Plans []claimPlan `json:"plans"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, fmt.Errorf("zcode: decode %s plans: %w", planPreviewPath, err)
		}
	}

	plans := make([]claimPlan, 0, len(payload.Plans))
	for _, p := range payload.Plans {
		if p.id() == "" {
			continue
		}
		plans = append(plans, p)
	}
	// The reference sorts by priority descending and then by plan id, so two
	// runs against the same vendor document pick the same plan.
	sort.SliceStable(plans, func(i, j int) bool {
		if plans[i].Priority != plans[j].Priority {
			return plans[i].Priority > plans[j].Priority
		}
		return plans[i].id() < plans[j].id()
	})
	return plans, nil
}

// claimPlanOnce posts one claim.  It returns the vendor's own start/end stamps
// on success.
func (c *Client) claimPlanOnce(ctx context.Context, acct *Account, planID, verifyParam string) (startsAt, endsAt int64, err error) {
	var env planEnvelope
	body := map[string]string{"plan_id": planID}
	if err := c.planDo(ctx, http.MethodPost, planClaimPath, acct, verifyParam, body, &env); err != nil {
		return 0, 0, err
	}
	data, err := env.open(planClaimPath)
	if err != nil {
		return 0, 0, err
	}

	var payload struct {
		Plan struct {
			StartsAt int64 `json:"starts_at"`
			EndsAt   int64 `json:"ends_at"`
		} `json:"plan"`
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return 0, 0, fmt.Errorf("zcode: decode %s: %w", planClaimPath, err)
		}
	}
	return payload.Plan.StartsAt, payload.Plan.EndsAt, nil
}

// planBalance is one row of the vendor's per-entitlement balance list.
//
// Remaining and Available are pointers because a genuinely empty tranche reads
// as 0 and an absent field also reads as 0; the reference distinguishes the two
// when it picks which number to report, and so do we.
type planBalance struct {
	PlanID        string `json:"plan_id"`
	PlanIDAlt     string `json:"planId"`
	EntitlementID string `json:"entitlement_id"`
	ShowName      string `json:"show_name"`
	Meter         string `json:"meter"`
	UnitType      string `json:"unit_type"`
	TotalUnits    int64  `json:"total_units"`
	UsedUnits     int64  `json:"used_units"`
	Remaining     *int64 `json:"remaining_units"`
	Available     *int64 `json:"available_units"`
	Period        string `json:"period"`
	ExpiresAt     int64  `json:"expires_at"`
}

func (b planBalance) planID() string { return firstNonEmpty(b.PlanID, b.PlanIDAlt) }

// remaining is the spendable quantity.  The reference prefers remaining_units
// and falls back to available_units; the live vendor sends both and they agree.
func (b planBalance) remaining() int64 {
	if b.Remaining != nil {
		return *b.Remaining
	}
	if b.Available != nil {
		return *b.Available
	}
	if r := b.TotalUnits - b.UsedUnits; r > 0 {
		return r
	}
	return 0
}

// planBalances is the vendor's balance document: the active plans plus one row
// per entitlement bucket.
type planBalances struct {
	Plans    []claimPlan   `json:"plans"`
	Balances []planBalance `json:"balances"`
}

// planBalanceOf reads one account's plan balance document.
func (c *Client) planBalanceOf(ctx context.Context, acct *Account) (*planBalances, error) {
	used := c.appVersion()
	doc, err := c.balanceOnce(ctx, acct)
	if err != nil || len(doc.Balances) > 0 || len(doc.Plans) > 0 {
		return doc, err
	}
	// Same version gate as the preview: an empty balance document can mean
	// "no active plan" or "this build is too old to be told about one".  Only
	// the manifest can tell them apart, and only when it advertises a build
	// newer than the one just used.
	if v := c.pool.refreshAppVersion(ctx); v != "" && v != used {
		return c.balanceOnce(ctx, acct)
	}
	return doc, err
}

// balanceOnce performs one balance read at the resolved app version.
func (c *Client) balanceOnce(ctx context.Context, acct *Account) (*planBalances, error) {
	q := url.Values{}
	q.Set("app_version", c.appVersion())

	var env planEnvelope
	if err := c.planDo(ctx, http.MethodGet, planBalancePath+"?"+q.Encode(), acct, "", nil, &env); err != nil {
		return nil, err
	}
	data, err := env.open(planBalancePath)
	if err != nil {
		return nil, err
	}

	out := &planBalances{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("zcode: decode %s: %w", planBalancePath, err)
		}
	}
	return out, nil
}

// planNameFor maps a plan id to its display name, so a balance row can say
// which promotion it came from instead of only which entitlement it is.
func (b *planBalances) planNameFor(planID string) string {
	for _, p := range b.Plans {
		if p.id() == planID {
			return p.Name
		}
	}
	return ""
}

// balanceUnit is what a core.Balance.Credits counts for this vendor.  The panel
// prints the number with this label and never converts between vendors: this
// one grants time-boxed tokens, workbuddy sells 积分.
const balanceUnit = "tokens"

// AccountBalance implements core.BalanceProvider.
//
// The units are tokens, not credits: this vendor's plans are time-boxed token
// grants, and pretending otherwise would invent a conversion rate.  The panel
// labels the column from the module, so the number keeps its own unit.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	acct := c.pool.find(id)
	if acct == nil {
		return core.Balance{}, fmt.Errorf("zcode: account %q not found", id)
	}
	doc, err := c.planBalanceOf(ctx, acct)
	if err != nil {
		return core.Balance{}, err
	}

	now := time.Now()
	var bal core.Balance
	for _, row := range doc.Balances {
		left := row.remaining()
		bal.Credits += left
		bal.Total += row.TotalUnits
		if row.ExpiresAt <= 0 {
			continue
		}
		at := time.Unix(row.ExpiresAt, 0)
		// Inclusive at the boundary: a tranche expiring exactly at the deadline
		// is expiring inside the window, and under-reporting expiring tokens is
		// the dangerous direction for the number an operator acts on.
		if soon > 0 && !at.After(now.Add(soon)) {
			bal.Expiring += left
		}
		// The earliest tranche is only interesting while it still holds
		// something: an empty one would otherwise always win.
		if left <= 0 {
			continue
		}
		if bal.EarliestAt.IsZero() || at.Before(bal.EarliestAt) {
			bal.EarliestAt = at
			bal.EarliestRemaining = left
		}
	}
	bal.Unit = balanceUnit
	return bal, nil
}

// AccountPackages implements core.PackageProvider: one row per entitlement
// bucket, biggest first, which is what the packages view sorts by.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	acct := c.pool.find(id)
	if acct == nil {
		return core.PackageReport{}, fmt.Errorf("zcode: account %q not found", id)
	}
	doc, err := c.planBalanceOf(ctx, acct)
	if err != nil {
		return core.PackageReport{}, err
	}

	out := core.PackageReport{Packages: make([]core.CreditPackage, 0, len(doc.Balances))}
	for _, row := range doc.Balances {
		left := row.remaining()
		out.Remain += left
		out.Size += row.TotalUnits

		name := row.ShowName
		if plan := doc.planNameFor(row.planID()); plan != "" {
			name = plan + " · " + row.ShowName
		}
		pkg := core.CreditPackage{
			Name:           name,
			Remain:         left,
			Used:           row.UsedUnits,
			Size:           row.TotalUnits,
			PackageCode:    row.planID(),
			SubProductCode: row.EntitlementID,
			SubProductName: row.ShowName,
			// A one-time grant is not a cycle; anything the vendor gives a
			// period to is, which is how the panel tells a signup gift from a
			// daily allowance.
			Cycle: !isOneTime(row.Period),
		}
		if row.ExpiresAt > 0 {
			at := time.Unix(row.ExpiresAt, 0).UTC()
			pkg.ExpiresAt = row.ExpiresAt
			pkg.EndTime = at.Format(time.RFC3339)
		}
		out.Packages = append(out.Packages, pkg)
	}
	sort.SliceStable(out.Packages, func(i, j int) bool {
		return out.Packages[i].Remain > out.Packages[j].Remain
	})
	return out, nil
}

// isOneTime reports whether a period token means "granted once".
func isOneTime(period string) bool {
	switch strings.ToLower(strings.TrimSpace(period)) {
	case "", "one_time", "onetime", "once":
		return true
	default:
		return false
	}
}

// CheckinActions implements core.CheckinProvider.  ZCode has no daily check-in;
// what it has is a promotion you claim, so that is the action.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	return []core.CheckinAction{{
		ID:    claimAction,
		Label: "领取活动套餐 (claim a promotion)",
		Help: "Reads the vendor's claimable plans and claims the highest-priority one. " +
			"The claim endpoint requires a live Aliyun captcha. In the panel that runs in your own browser " +
			"(the module reports the scene and the page runs the vendor's SDK); headless it needs " +
			"captcha_command configured. With neither the action reports that it is not configured.",
	}}
}

// Checkin implements core.CheckinProvider by claiming a promotion.
//
// Every vendor answer is a result, never an error: the operator asked a
// question ("what can this account get?") and the vendor's business code is the
// answer.  "There is nothing to claim" -- an empty preview, or the vendor's
// 1003 -- is the steady state rather than a refusal, so it comes back with
// ok=true.  An error is reserved for the cases where nothing could be attempted
// at all.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	started := time.Now()
	res := core.CheckinResult{AccountID: id, Action: strings.TrimSpace(action)}
	done := func() (core.CheckinResult, error) {
		res.ElapsedMS = time.Since(started).Milliseconds()
		res.At = time.Now().UTC().Format(time.RFC3339)
		return res, nil
	}

	if res.Action != "" && res.Action != claimAction {
		res.Error = fmt.Sprintf("unknown action %q; this module only offers %q", res.Action, claimAction)
		return done()
	}
	res.Action = claimAction

	acct := c.pool.find(id)
	if acct == nil {
		res.Error = fmt.Sprintf("account %q not found", id)
		return done()
	}
	if acct.Mode != modeJWT {
		res.Error = fmt.Sprintf("plan billing needs a ZCode plan (jwt) credential; %s is %s",
			acct.ID, firstNonEmpty(acct.Mode, "unknown"))
		return done()
	}

	plans, err := c.claimPreview(ctx, acct)
	if err != nil {
		c.claimFailure(&res, err)
		return done()
	}
	if len(plans) == 0 {
		// Nothing to claim is the steady state, not a refusal.  The vendor
		// hands the daily plan out once, so every sweep after the first sees an
		// empty list; reporting that as OK false would leave the board
		// permanently red and bury the runs that really did go wrong.  The
		// vendor's own code is still relayed, because the panel shows it.
		res.OK = true
		res.Code = claimCodeUnavailable
		res.Message = "nothing is claimable right now"
		return done()
	}
	if len(plans) > claimMaxPlans {
		plans = plans[:claimMaxPlans]
	}

	// Walk the plans in priority order.  The vendor has no idempotency key, so
	// the guard is the vendor's own 1003: a plan that turns out to be claimed
	// already is not a failure of the action, it is the next plan's turn.
	var last *claimError
	for _, plan := range plans {
		verifyParam, err := c.solveCaptcha(ctx, c.pool.regionFor(ctx))
		if err != nil {
			// No solver is a configuration fact, not a vendor refusal.
			c.claimFailure(&res, err)
			return done()
		}
		startsAt, endsAt, err := c.claimPlanOnce(ctx, acct, plan.id(), verifyParam)
		if err == nil {
			res.OK = true
			res.Code = 0
			res.Message = fmt.Sprintf("claimed %s", firstNonEmpty(plan.Name, plan.id()))
			res.Data = map[string]any{
				"plan_id":   plan.id(),
				"plan_name": plan.Name,
				"grants":    grantSummary(plan.tokenGrants()),
				"starts_at": time.Unix(startsAt, 0).UTC().Format(time.RFC3339),
				"ends_at":   time.Unix(endsAt, 0).UTC().Format(time.RFC3339),
			}
			return done()
		}

		ce := asClaimError(err)
		if ce == nil {
			// A transport or decode failure says nothing about the next plan,
			// so stop rather than spending another captcha.
			c.claimFailure(&res, err)
			return done()
		}
		last = ce
		if ce.code != claimCodeAlreadyClaimed && ce.code != claimCodeNoPlan {
			break
		}
	}

	if last != nil {
		res.Code = last.code
		res.Message = last.Error()
		// "Already claimed" is what a second sweep in the same period sees, so
		// it is the same steady state as an empty preview: the action ran and
		// the vendor said there is nothing left to do.  Every other code stays
		// a refusal -- not eligible, quota used up and a rejected argument all
		// say something is wrong with this account or this call.
		if last.code == claimCodeAlreadyClaimed {
			res.OK = true
		}
		return done()
	}
	res.Code = claimCodeUnavailable
	res.Message = "nothing is claimable right now"
	return done()
}

// claimFailure records an error that stopped the action before the vendor
// decided anything.  The vendor's own code, when there is one, is kept.
//
// It writes through a pointer on purpose: `done` closes over the caller's
// result variable, so mutating a copy here would hand back a blank result and
// swallow the only explanation the operator would ever see.
func (c *Client) claimFailure(res *core.CheckinResult, err error) {
	if ce := asClaimError(err); ce != nil {
		res.Code = ce.code
		res.Message = ce.Error()
		return
	}
	res.Error = err.Error()
}
