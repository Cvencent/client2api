package minimaxcode

// Daily sign-in (签到) and credits (积分) for MiniMax Code.
//
// The desktop client keeps these in a second API namespace, /minimax-cloud/api/v1,
// which lives on the same origin as the model endpoint:
//
//	GET  https://agent.minimax.cn/minimax-cloud/api/v1/signin/status?timezone_id=…
//	POST https://agent.minimax.cn/minimax-cloud/api/v1/signin/claim   {}
//	GET  https://agent.minimax.cn/minimax-cloud/api/v1/credit/details
//
// (the origin is proven by the client calling /minimax-cloud/api/v1/skill-hub
// alongside /mavis/api/v1/llm/v1/messages).  The shapes below were read out of
// the shipped app.asar: daily-signin.d.ts declares the panel and the claim, and
// the i18n catalogue names the two credit kinds.  This module previously
// reported no check-in and no quota for this product; that was wrong, and the
// README is corrected alongside this file.
//
// Two vendor behaviours drive the code here:
//
//   - Every route answers HTTP 200 and carries the real verdict in
//     base_resp.status_code, so a 200 is not a success.  classifyEnvelope
//     already knows that shape.
//   - signin/status REQUIRES timezone_id, and the vendor computes "today" in
//     it.  Probing a +08:00 account with Europe/London returned a panel where
//     no day was today and day 1 read Disabled, so a wrong zone is not a
//     cosmetic problem.  See detectSigninTimezone.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// The /minimax-cloud/api/v1 routes this module uses.
	signinStatusPath = "/minimax-cloud/api/v1/signin/status"
	signinClaimPath  = "/minimax-cloud/api/v1/signin/claim"
	creditDetailPath = "/minimax-cloud/api/v1/credit/details"

	// signinActionDaily is the one action CheckinActions offers.
	signinActionDaily = "daily-signin"

	signinTimeout = 30 * time.Second

	// signinBodyLimit bounds every reply from these routes.
	signinBodyLimit = 1 << 20
)

// Sign-in day states, verbatim from daily-signin.d.ts.
const (
	signinDayUpcoming  = 1
	signinDayClaimable = 2
	signinDayClaimed   = 3
	signinDayDisabled  = 4
)

// Claim outcomes, verbatim from daily-signin.d.ts.
const (
	signinClaimClaimed        = 1
	signinClaimAlreadyClaimed = 2
)

// credit_type is a two-value enum: the i18n catalogue names exactly
// usage_credit_type_purchased ("购买") and usage_credit_type_free ("签到").
// The numbers are not in the extracted type declarations, so they were read off
// a live account: it held exactly one tranche, granted 800.00 and expiring 30
// days later, and the sign-in panel offers 800 points on day 1 with a
// documented 30-day life for 赠予积分.  So 2 is the sign-in grant and 1 the
// purchased one.  An unrecognised value is labelled rather than dropped.
const (
	creditTypePurchased = 1
	creditTypeSignin    = 2
)

// balanceUnit is what a core.Balance.Credits counts for this vendor.  The panel
// prints the number with this label and never converts between vendors: this
// one grants 积分, zcode grants time-boxed tokens.
const balanceUnit = "积分"

// defaultSigninTimezone is the fallback when the machine's own zone cannot be
// matched to an IANA name.  The CN host is this module's default base URL, so
// +08:00 is the least surprising guess.
const defaultSigninTimezone = "Asia/Shanghai"

// signinTimezones maps a UTC offset in seconds to the IANA names the vendor
// accepts for it, best guess first.  The vendor validates timezone_id against
// the real IANA database (Not/AZone is rejected as invalid timezone_id), so an
// offset has to be turned into a genuine zone name rather than a raw "+08:00".
//
// A zone whose abbreviation is computed rather than fixed cannot be picked from
// the offset alone -- +01:00 is London's BST in summer and Berlin's CET in
// winter -- so detectSigninTimezone breaks the tie with the machine's own
// abbreviation before falling back to the first entry.
var signinTimezones = map[int][]string{
	-10 * 3600:    {"Pacific/Honolulu"},
	-9 * 3600:     {"America/Anchorage"},
	-8 * 3600:     {"America/Los_Angeles"},
	-7 * 3600:     {"America/Denver", "America/Phoenix"},
	-6 * 3600:     {"America/Chicago", "America/Mexico_City"},
	-5 * 3600:     {"America/New_York", "America/Toronto"},
	-4 * 3600:     {"America/Halifax", "America/Santiago"},
	-3 * 3600:     {"America/Sao_Paulo", "America/Argentina/Buenos_Aires"},
	0:             {"UTC", "Europe/London"},
	1 * 3600:      {"Europe/London", "Europe/Berlin", "Europe/Paris"},
	2 * 3600:      {"Europe/Berlin", "Europe/Paris", "Europe/Athens"},
	3 * 3600:      {"Europe/Moscow", "Europe/Istanbul"},
	4 * 3600:      {"Asia/Dubai"},
	5 * 3600:      {"Asia/Karachi", "Asia/Tashkent"},
	5*3600 + 1800: {"Asia/Kolkata"},
	7 * 3600:      {"Asia/Bangkok", "Asia/Jakarta"},
	8 * 3600:      {"Asia/Shanghai", "Asia/Singapore", "Asia/Hong_Kong"},
	9 * 3600:      {"Asia/Tokyo", "Asia/Seoul"},
	10 * 3600:     {"Australia/Sydney", "Australia/Brisbane"},
	12 * 3600:     {"Pacific/Auckland"},
}

// localSigninTimezone memoises the machine-zone lookup, which reads the
// timezone database off disk.
var localSigninTimezone = sync.OnceValue(detectSigninTimezone)

// detectSigninTimezone names the machine's own zone the way the desktop client
// would.  TZ wins when it is set and loadable; otherwise the current UTC offset
// selects a candidate list and the current abbreviation picks within it.
func detectSigninTimezone() string {
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}

	now := time.Now()
	abbr, offset := now.Zone()
	candidates := signinTimezones[offset]
	for _, name := range candidates {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		if got, _ := now.In(loc).Zone(); got == abbr {
			return name
		}
	}
	if len(candidates) > 0 {
		return candidates[0]
	}
	return defaultSigninTimezone
}

// cloudReply is the wrapper most /minimax-cloud/api/v1 routes use.  data is
// left raw so each caller can decode its own shape.
//
// Not every route uses it: signin/status and signin/claim nest their payload
// under data, while credit/details puts details and total_count at the top
// level beside base_resp.  cloudCall therefore hands back the raw body too, and
// each caller picks whichever spelling its route actually uses.
type cloudReply struct {
	BaseResp struct {
		StatusCode int64  `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
	Data json.RawMessage `json:"data"`
}

// signinDay is one row of the seven-day panel.
type signinDay struct {
	DayNo       int   `json:"day_no"`
	Points      int64 `json:"points"`
	BonusPoints int64 `json:"bonus_points"`
	Status      int   `json:"status"`
	IsToday     bool  `json:"is_today"`
}

// signinPanel is the whole board.  The vendor validates that it has exactly
// seven days with unique day_no values in 1..7 and at most one of each of
// Claimable and is_today; this module only reads it.
type signinPanel struct {
	Scene int         `json:"scene"`
	Days  []signinDay `json:"days"`
}

// today returns the row the vendor marked as today's, if any.
func (p signinPanel) today() (signinDay, bool) {
	for _, d := range p.Days {
		if d.IsToday {
			return d, true
		}
	}
	return signinDay{}, false
}

// signinClaim is the reply to a claim.
type signinClaim struct {
	ClaimID     string       `json:"claim_id"`
	ClaimResult int          `json:"claim_result"`
	DayNo       int          `json:"day_no"`
	Points      int64        `json:"points"`
	ExpireAtMS  int64        `json:"expire_at_ms"`
	Panel       *signinPanel `json:"panel"`
}

// creditRow is one granted tranche of credits.  Every amount is a decimal
// STRING on the wire, because credits are billed fractionally.
type creditRow struct {
	CreditType      int    `json:"credit_type"`
	GrantedAmount   string `json:"granted_amount"`
	RemainingAmount string `json:"remaining_amount"`
	ConsumedAmount  string `json:"consumed_amount"`
	GrantedAtMS     int64  `json:"granted_at_ms"`
	ExpireAtMS      int64  `json:"expire_at_ms"`
}

// creditDetails is the reply to credit/details.
type creditDetails struct {
	Details    []creditRow `json:"details"`
	TotalCount int         `json:"total_count"`
}

func (r creditRow) granted() int64   { v, _ := creditAmount(r.GrantedAmount); return v }
func (r creditRow) remaining() int64 { v, _ := creditAmount(r.RemainingAmount); return v }
func (r creditRow) consumed() int64  { v, _ := creditAmount(r.ConsumedAmount); return v }

// creditAmount parses a decimal-string amount into whole credits.
//
// The rounding is a real loss of precision that the panel's int64 fields force:
// the vendor tracks "781.47" and the panel can only show "781".  Rounding to
// nearest, rather than truncating, keeps the displayed number honest.
//
// A negative amount is reported as zero, mirroring the desktop client, which
// clamps a negative remaining_amount to "0" rather than showing a debt.
func creditAmount(raw string) (int64, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if s == "" {
		return 0, false
	}
	if strings.HasPrefix(s, "-") {
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return 0, false
		}
		return 0, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	if f < 0 {
		return 0, true
	}
	return int64(f + 0.5), true
}

// creditTypeLabel names a tranche the way the desktop client's credit list does.
func creditTypeLabel(t int) string {
	switch t {
	case creditTypeSignin:
		return "签到积分"
	case creditTypePurchased:
		return "购买积分"
	default:
		return fmt.Sprintf("积分 (type %d)", t)
	}
}

// signinDayStatusName renders a day state for an operator-facing message.
func signinDayStatusName(status int) string {
	switch status {
	case signinDayUpcoming:
		return "未开始"
	case signinDayClaimable:
		return "可领取"
	case signinDayClaimed:
		return "已领取"
	case signinDayDisabled:
		return "不可用"
	default:
		return fmt.Sprintf("未知状态 %d", status)
	}
}

// cloudOrigin is the scheme://host an account talks to.  The /minimax-cloud
// namespace is addressed relative to the model endpoint's origin, so a
// configured base URL that points at a proxy still routes these calls to the
// same place as chat.
func (c *Client) cloudOrigin(acct *Account) string {
	u, err := url.Parse(c.baseURL(acct))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// cloudCall performs one JSON request against the /minimax-cloud/api/v1
// namespace.
//
// The headers are deliberately narrower than applyHeaders: these routes are not
// the Anthropic Messages endpoint, so announcing anthropic-version would be a
// lie about the protocol.  What is sent is what the live probes needed.
//
// A non-zero base_resp inside an HTTP 200 is turned into the module's own typed
// upstream error, so a rejected sign-in penalises the account exactly the way a
// rejected chat does.
//
// It returns both the decoded envelope and the raw body, because the routes in
// this namespace disagree about where the payload lives (see cloudReply).
func (c *Client) cloudCall(ctx context.Context, acct *Account, method, path string, query url.Values, payload []byte) (*cloudReply, []byte, error) {
	origin := c.cloudOrigin(acct)
	if origin == "" {
		return nil, nil, fmt.Errorf("minimaxcode: cannot derive an origin from base URL %q", c.baseURL(acct))
	}
	target := origin + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var body io.Reader
	if len(payload) > 0 {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, nil, fmt.Errorf("minimaxcode: build %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
	if len(payload) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	secret := ""
	if acct != nil {
		secret = acct.secret()
		if tok := strings.TrimSpace(acct.Token); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("minimaxcode: %s: %w", path, err)
	}
	raw := readLimited(resp.Body, signinBodyLimit)
	_ = resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		ue := classify(resp.StatusCode, raw)
		ue.Msg = redactSecret(ue.Msg, secret)
		return nil, nil, ue
	}
	if ue := classifyEnvelope(resp.StatusCode, raw); ue != nil {
		ue.Msg = redactSecret(ue.Msg, secret)
		return nil, nil, ue
	}

	reply := &cloudReply{}
	if err := json.Unmarshal(raw, reply); err != nil {
		return nil, nil, fmt.Errorf("minimaxcode: decode %s: %w", path, err)
	}
	return reply, raw, nil
}

// signinCredential resolves an account id to a credential that is fresh enough
// to send.  It reports false only when no such account exists.
func (c *Client) signinCredential(ctx context.Context, id string) (Account, bool) {
	if c == nil || c.pool == nil {
		return Account{}, false
	}
	if _, ok := c.pool.current(id); !ok {
		return Account{}, false
	}
	return c.ensureFresh(ctx, id), true
}

// signinAccount additionally distinguishes a parked (disabled) account, which is
// a result the operator can act on, from one that does not exist at all.
func (c *Client) signinAccount(ctx context.Context, id string) (acct Account, parked bool, ok bool) {
	if c == nil || c.pool == nil {
		return Account{}, false, false
	}
	cur, found := c.pool.current(id)
	if !found {
		return Account{}, false, false
	}
	if !cur.Enabled {
		return cur, true, true
	}
	return c.ensureFresh(ctx, id), false, true
}

// signinPanelOf asks the vendor for the seven-day board.
func (c *Client) signinPanelOf(ctx context.Context, acct Account) (signinPanel, error) {
	query := url.Values{"timezone_id": {c.cfg.signinTimezone()}}
	reply, _, err := c.cloudCall(ctx, &acct, http.MethodGet, signinStatusPath, query, nil)
	if err != nil {
		return signinPanel{}, err
	}
	var panel signinPanel
	if err := json.Unmarshal(reply.Data, &panel); err != nil {
		return signinPanel{}, fmt.Errorf("minimaxcode: decode the sign-in panel: %w", err)
	}
	return panel, nil
}

// claimSignin posts the claim.  The vendor takes an empty JSON object, but it
// wants timezone_id here exactly as it does on the status read: without it the
// route answers base_resp 1406010011 "invalid timezone_id" and nothing is
// claimed.  The desktop client gets away with a bare POST only because its
// shared axios instance injects the parameter.
func (c *Client) claimSignin(ctx context.Context, acct Account) (signinClaim, error) {
	query := url.Values{"timezone_id": {c.cfg.signinTimezone()}}
	reply, _, err := c.cloudCall(ctx, &acct, http.MethodPost, signinClaimPath, query, []byte("{}"))
	if err != nil {
		return signinClaim{}, err
	}
	var claim signinClaim
	if err := json.Unmarshal(reply.Data, &claim); err != nil {
		return signinClaim{}, fmt.Errorf("minimaxcode: decode the sign-in claim: %w", err)
	}
	return claim, nil
}

// creditRowsOf lists the account's granted credit tranches.
//
// This route is the odd one out: it answers details and total_count at the top
// level, with no data wrapper, so the raw body is decoded rather than
// reply.Data.
func (c *Client) creditRowsOf(ctx context.Context, acct Account) ([]creditRow, error) {
	_, raw, err := c.cloudCall(ctx, &acct, http.MethodGet, creditDetailPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var doc creditDetails
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("minimaxcode: decode the credit details: %w", err)
	}
	return doc.Details, nil
}

// CheckinActions offers the daily claim when there is an account that could
// actually perform it.  A button that cannot work is a lie the operator pays for
// with a failed request, so an empty pool offers nothing.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c == nil || c.pool == nil {
		return nil
	}
	if len(c.pool.selectableSnapshots()) == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    signinActionDaily,
		Label: "领取每日签到积分",
		Help:  "MiniMax Code 七天一轮的每日签到；签到积分 30 天内有效，只在该账号自己的时区里计算“今天”。",
	}}
}

// Checkin runs status-then-claim, and never claims blind.
//
// Everything the vendor says -- already claimed, no day available, a rejected
// token -- comes back as a CheckinResult with OK=false and a nil error.  A Go
// error is reserved for "this could not even be attempted".
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	start := time.Now()
	res := core.CheckinResult{AccountID: id}

	if action != "" && action != signinActionDaily {
		res.Error = fmt.Sprintf("unknown action %q; this client only offers %q", action, signinActionDaily)
		return c.finishSignin(res, start), nil
	}
	res.Action = signinActionDaily

	acct, parked, ok := c.signinAccount(ctx, id)
	if !ok {
		return res, fmt.Errorf("minimaxcode: account %q not found", id)
	}
	if parked {
		res.Error = "the account is parked; enable it before claiming a reward"
		return c.finishSignin(res, start), nil
	}

	ctx, cancel := context.WithTimeout(ctx, signinTimeout)
	defer cancel()

	panel, err := c.signinPanelOf(ctx, acct)
	if err != nil {
		return c.signinFailure(acct, res, start, "read the check-in status", err), nil
	}

	today, found := panel.today()
	if !found {
		// The vendor computes "today" in the timezone it was given, so a panel
		// with no current day means the zone does not line up with the account.
		// Claiming anyway would be a blind post, and reporting success would be
		// a lie.
		res.Error = fmt.Sprintf("the vendor marked no day as today for timezone %q; set this client's \"timezone\" to the account's own zone",
			c.cfg.signinTimezone())
		res.Data = map[string]any{"timezone": c.cfg.signinTimezone(), "days": len(panel.Days)}
		return c.finishSignin(res, start), nil
	}

	// points already includes bonus_points -- the desktop client's own type
	// comment says so -- so the two must not be added together.
	if today.Status == signinDayClaimed {
		res.OK = true
		res.Message = fmt.Sprintf("今天已签到：第 %d 天", today.DayNo)
		res.Data = map[string]any{"already_done": true, "day_no": today.DayNo, "points": today.Points}
		c.pool.noteSuccess(acct.ID)
		return c.finishSignin(res, start), nil
	}
	if today.Status != signinDayClaimable {
		res.Error = fmt.Sprintf("the vendor offers nothing to claim today (day %d is %s)", today.DayNo, signinDayStatusName(today.Status))
		res.Data = map[string]any{"day_no": today.DayNo, "status": today.Status}
		return c.finishSignin(res, start), nil
	}

	claim, err := c.claimSignin(ctx, acct)
	if err != nil {
		return c.signinFailure(acct, res, start, "claim the daily reward", err), nil
	}

	res.OK = true
	dayNo := claim.DayNo
	if dayNo == 0 {
		dayNo = today.DayNo
	}
	switch claim.ClaimResult {
	case signinClaimAlreadyClaimed:
		res.Message = fmt.Sprintf("今天已签到：第 %d 天", dayNo)
		res.Data = map[string]any{"already_done": true, "day_no": dayNo, "points": today.Points}
	default:
		gained := claim.Points
		if gained == 0 {
			gained = today.Points
		}
		res.Message = fmt.Sprintf("签到成功：第 %d 天，获得 %d 积分", dayNo, gained)
		data := map[string]any{"already_done": false, "day_no": dayNo, "points": gained}
		if claim.ClaimID != "" {
			data["claim_id"] = claim.ClaimID
		}
		if claim.ExpireAtMS > 0 {
			// UTC, matching the package rows: the panel shows this string as-is
			// and an unqualified local time would be read as the wrong day.
			data["expires_at"] = msToTime(claim.ExpireAtMS).UTC().Format(time.RFC3339)
		}
		if today.BonusPoints > 0 {
			data["bonus_points"] = today.BonusPoints
		}
		res.Data = data
	}
	c.pool.noteSuccess(acct.ID)
	return c.finishSignin(res, start), nil
}

// signinFailure folds a rejected call into a result and applies the pool penalty.
func (c *Client) signinFailure(acct Account, res core.CheckinResult, start time.Time, what string, err error) core.CheckinResult {
	kind := core.FailureOther
	var ue *upstreamError
	if errors.As(err, &ue) {
		kind = ue.failureKind()
	}
	c.pool.noteFailure(acct.ID, kind, err.Error())
	res.Error = fmt.Sprintf("could not %s: %s", what, err.Error())
	// noteFailure returns nothing, so ask the pool what it actually did rather
	// than announcing the configured cooldown: only the cooling kinds set a
	// deadline, and telling an operator to wait when nothing was parked is a
	// lie they would act on.
	if after, ok := c.pool.current(acct.ID); ok && !after.CooldownUntil.IsZero() {
		if left := time.Until(after.CooldownUntil); left > 0 {
			res.Error += fmt.Sprintf(" (cooling down for %s after a %s failure)", left.Round(time.Second), kind)
		}
	}
	res.Data = map[string]any{"error_kind": string(kind)}
	return c.finishSignin(res, start)
}

func (c *Client) finishSignin(res core.CheckinResult, start time.Time) core.CheckinResult {
	res.ElapsedMS = time.Since(start).Milliseconds()
	res.At = time.Now().Format(time.RFC3339)
	return res
}

// AccountBalance reports the account's credit balance.
//
// Unlike Checkin, a vendor error here IS an error: the panel has nothing
// sensible to display without a number, so it answers 502 rather than 200.
//
// The units are credits, which is what this product actually grants -- there is
// no token count on this endpoint and inventing one would be a fabrication.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	acct, ok := c.signinCredential(ctx, id)
	if !ok {
		return core.Balance{}, fmt.Errorf("minimaxcode: account %q not found", id)
	}
	rows, err := c.creditRowsOf(ctx, acct)
	if err != nil {
		return core.Balance{}, err
	}

	now := time.Now()
	var bal core.Balance
	for _, row := range rows {
		left := row.remaining()
		bal.Credits += left
		bal.Total += row.granted()
		if row.ExpireAtMS <= 0 {
			continue
		}
		at := msToTime(row.ExpireAtMS)
		// Inclusive at the boundary: a tranche expiring exactly at the deadline
		// is expiring inside the window, and under-reporting expiring credits is
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

// AccountPackages lists the granted credit tranches as packages, so the panel's
// package view shows the same grants the credit list does.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	acct, ok := c.signinCredential(ctx, id)
	if !ok {
		return core.PackageReport{}, fmt.Errorf("minimaxcode: account %q not found", id)
	}
	rows, err := c.creditRowsOf(ctx, acct)
	if err != nil {
		return core.PackageReport{}, err
	}

	out := core.PackageReport{Packages: make([]core.CreditPackage, 0, len(rows))}
	for _, row := range rows {
		left := row.remaining()
		granted := row.granted()
		used := row.consumed()
		if used == 0 && granted > left {
			used = granted - left
		}

		pkg := core.CreditPackage{
			Name:   creditTypeLabel(row.CreditType),
			Remain: left,
			Size:   granted,
			Used:   used,
			// A granted tranche is a one-off, never a renewing cycle.
			Cycle: false,
		}
		if row.GrantedAtMS > 0 {
			pkg.CreatedAt = msToTime(row.GrantedAtMS).UTC().Format(time.RFC3339)
		}
		if row.ExpireAtMS > 0 {
			pkg.ExpiresAt = row.ExpireAtMS
			pkg.EndTime = msToTime(row.ExpireAtMS).UTC().Format(time.RFC3339)
		}

		out.Remain += left
		out.Size += granted
		out.Packages = append(out.Packages, pkg)
	}

	sort.SliceStable(out.Packages, func(i, j int) bool {
		return out.Packages[i].Remain > out.Packages[j].Remain
	})
	return out, nil
}

var (
	_ core.CheckinProvider = (*Client)(nil)
	_ core.BalanceProvider = (*Client)(nil)
	_ core.PackageProvider = (*Client)(nil)
)
