package loomy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"client2api/internal/core"
)

// quota.go implements the credit-facing optional interfaces: the balance the
// panel shows, the tranche breakdown, and the one check-in action Loomy
// genuinely has.
//
// The vendor keeps two pools.  `permanent` is bought or granted credit that
// stays.  `daily` is a gift allowance that is NOT carried over: it resets each
// day and is never replenished once spent, so it is a daily allowance rather
// than a running balance.  Reading and claiming are strictly separate calls --
// `GET /points/records` has no side effects and `POST /points/first-login` is
// the idempotent write -- which is what stops merely opening the panel from
// silently checking in.

// balanceUnit is what this vendor sells.  Both pools are denominated in it.
const balanceUnit = "积分"

const checkinActionDaily = "daily"

// creditSnapshot is the vendor's credit payload.  Every figure is a pointer so
// that "the vendor did not report this" stays distinguishable from zero --
// a missing quota must never be rendered as a real zero.
type creditSnapshot struct {
	Balance          *float64 `json:"balance"`
	AvailableBalance *float64 `json:"availableBalance"`
	PermanentBalance *float64 `json:"permanentBalance"`
	DailyBalance     *float64 `json:"dailyBalance"`
	CurrentBalance   *float64 `json:"currentBalance"`
	DailyQuota       *float64 `json:"dailyQuota"`
	DailyConsumed    *float64 `json:"dailyConsumed"`
	DailyCycleDate   string   `json:"dailyCycleDate"`
	AlreadyProcessed *bool    `json:"alreadyProcessed"`
}

// creditDetail is the normalised view of the two pools.
type creditDetail struct {
	Permanent      int64
	Daily          int64
	Available      int64
	DailyQuota     int64
	HasDailyQuota  bool
	DailyConsumed  int64
	DailyCycleDate string
}

// detail normalises a snapshot.
//
// `balance` is the permanent pool and is the only field the vendor guarantees.
// The available figure falls back to permanent+daily exactly as the reference
// does, because a server that omits `availableBalance` still knows both halves.
func (s creditSnapshot) detail() (creditDetail, error) {
	permanent, hasPermanent := roundInt64(s.PermanentBalance)
	if !hasPermanent {
		// Some responses carry the permanent pool under the plain `balance`
		// name instead.
		permanent, hasPermanent = roundInt64(s.Balance)
	}
	daily, _ := roundInt64(s.DailyBalance)
	if !hasPermanent {
		// `currentBalance` is the closest thing to a total in the
		// first-login snapshot; accept it rather than reporting nothing.
		permanent, hasPermanent = roundInt64(s.CurrentBalance)
	}
	if !hasPermanent {
		return creditDetail{}, fmt.Errorf("loomy: the credit ledger reported no balance")
	}

	detail := creditDetail{
		Permanent:      permanent,
		Daily:          daily,
		Available:      permanent + daily,
		DailyCycleDate: s.DailyCycleDate,
	}
	if available, ok := roundInt64(s.AvailableBalance); ok {
		detail.Available = available
	}
	if quota, ok := roundInt64(s.DailyQuota); ok {
		detail.DailyQuota = quota
		detail.HasDailyQuota = true
	}
	if consumed, ok := roundInt64(s.DailyConsumed); ok {
		detail.DailyConsumed = consumed
	}
	return detail, nil
}

func roundInt64(v *float64) (int64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, false
	}
	return int64(math.Round(*v)), true
}

// creditSnapshot reads the ledger.  This is the read-only half.
func (u *upstream) creditSnapshot(ctx context.Context, token string) (*creditSnapshot, error) {
	data, err := u.business(ctx, http.MethodGet, "/points/records", pointsRecordsQuery(), token, nil)
	if err != nil {
		return nil, err
	}
	var snap creditSnapshot
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &snap); err != nil {
			return nil, fmt.Errorf("loomy: reading the credit ledger: %w", err)
		}
	}
	return &snap, nil
}

// claimOutcome is the result of the daily grant.  It is a separate shape from
// creditSnapshot because the grant response also carries `dailyQuota`, which the
// ledger read does not return at all.
type claimOutcome struct {
	AlreadyProcessed bool
	DailyQuota       int64
	HasDailyQuota    bool
	DailyBalance     int64
	DailyConsumed    int64
	Granted          int64
}

// claimDailyQuota performs the write half: one idempotent POST that grants
// today's allowance.
func (u *upstream) claimDailyQuota(ctx context.Context, token string) (claimOutcome, error) {
	data, err := u.business(ctx, http.MethodPost, "/points/first-login", nil, token, []byte("{}"))
	if err != nil {
		return claimOutcome{}, err
	}

	var snap creditSnapshot
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &snap); err != nil {
			return claimOutcome{}, fmt.Errorf("loomy: reading the daily grant: %w", err)
		}
	}

	out := claimOutcome{}
	if snap.AlreadyProcessed != nil {
		out.AlreadyProcessed = *snap.AlreadyProcessed
	}
	out.DailyQuota, out.HasDailyQuota = roundInt64(snap.DailyQuota)
	out.DailyBalance, _ = roundInt64(snap.DailyBalance)
	out.DailyConsumed, _ = roundInt64(snap.DailyConsumed)

	switch {
	case out.AlreadyProcessed:
		// Nothing new was added, so no grant is reported.
	case out.HasDailyQuota && out.DailyConsumed <= out.DailyQuota:
		out.Granted = out.DailyQuota - out.DailyConsumed
	case out.HasDailyQuota:
		out.Granted = out.DailyQuota
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Panel-facing credit interfaces
// ---------------------------------------------------------------------------

// AccountBalance implements core.BalanceProvider.
//
// `soon` is ignored on purpose.  It asks how much credit is about to expire, and
// this vendor has no expiring tranche to report: the daily allowance resets at
// the next daily boundary rather than expiring at a time the server tells us,
// and the permanent pool never expires.  Reporting a number here would mean
// inventing a deadline, so Expiring stays zero and the caller is told the truth.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.Balance{}, fmt.Errorf("loomy: no account %q", id)
	}

	snap, err := c.up.creditSnapshot(ctx, acc.AccessToken)
	if err != nil {
		// A balance read is one more piece of evidence about the credential, so
		// a rejection here parks the account rather than being thrown away --
		// unless the caller is the one that left, which says nothing about the
		// credential.  The panel reads balances on every redraw and cancels the
		// read in flight when it does, so without this guard a busy panel would
		// park every account it looked at.
		if !callerGone(err) {
			c.penalise(id, err, time.Now().UTC())
		}
		return core.Balance{}, err
	}
	detail, err := snap.detail()
	if err != nil {
		return core.Balance{}, err
	}

	// The ceiling the two pools could hold today: the permanent credit plus one
	// day's allowance.  Without a reported quota there is no ceiling to show, so
	// the total is simply what is available.
	total := detail.Available
	if detail.HasDailyQuota {
		total = detail.Permanent + detail.DailyQuota
	}
	return core.Balance{
		Credits: detail.Available,
		Total:   total,
		Unit:    balanceUnit,
	}, nil
}

// AccountPackages implements core.PackageProvider: the two pools as tranches.
func (c *Client) AccountPackages(ctx context.Context, id string) (core.PackageReport, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.PackageReport{}, fmt.Errorf("loomy: no account %q", id)
	}

	snap, err := c.up.creditSnapshot(ctx, acc.AccessToken)
	if err != nil {
		// Same rule as AccountBalance above: a caller that walked away is not
		// evidence about the credential, so it must not park the account.
		if !callerGone(err) {
			c.penalise(id, err, time.Now().UTC())
		}
		return core.PackageReport{}, err
	}
	detail, err := snap.detail()
	if err != nil {
		return core.PackageReport{}, err
	}

	// The permanent pool's size is not reported anywhere, so its size is what is
	// left: claiming a larger size would be a fabricated number, and a size
	// smaller than the remainder would be nonsense.
	permanent := core.CreditPackage{
		Name:   "永久积分",
		Remain: detail.Permanent,
		Size:   detail.Permanent,
	}

	daily := core.CreditPackage{
		Name:   "每日赠送",
		Remain: detail.Daily,
		Size:   detail.Daily,
		Cycle:  true,
	}
	if detail.HasDailyQuota {
		daily.Size = detail.DailyQuota
		daily.Used = detail.DailyConsumed
		if daily.Used == 0 && detail.DailyQuota >= detail.Daily {
			daily.Used = detail.DailyQuota - detail.Daily
		}
	}
	if detail.DailyCycleDate != "" {
		daily.EndTime = detail.DailyCycleDate
	}

	report := core.PackageReport{
		Remain:   detail.Available,
		Size:     permanent.Size + daily.Size,
		Packages: []core.CreditPackage{permanent, daily},
	}
	return report, nil
}

// CheckinActions implements core.CheckinProvider.  Loomy really does have one
// daily grant, so this is offered; a module with no check-in must not invent
// one, which is why nothing else appears here.
func (c *Client) CheckinActions(ctx context.Context) []core.CheckinAction {
	if c.store.count() == 0 {
		return nil
	}
	return []core.CheckinAction{{
		ID:    checkinActionDaily,
		Label: "Claim the daily gift allowance",
		Help: "POST /points/first-login on the Loomy business host. The allowance resets each " +
			"day and is NOT carried over or replenished once spent, so this is a daily reset " +
			"rather than a growing balance. The call is idempotent and reports whether today's " +
			"grant was already collected.",
	}}
}

// Checkin performs the daily grant.  An upstream refusal is a result with OK
// false; only an unknown account is a Go error.
func (c *Client) Checkin(ctx context.Context, id, action string) (core.CheckinResult, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.CheckinResult{}, fmt.Errorf("loomy: no account %q", id)
	}
	if action == "" {
		action = checkinActionDaily
	}

	res := core.CheckinResult{
		AccountID: id,
		Action:    action,
		At:        time.Now().UTC().Format(time.RFC3339),
	}
	if action != checkinActionDaily {
		res.Error = fmt.Sprintf("loomy: unknown action %q", action)
		return res, nil
	}

	start := time.Now()
	outcome, err := c.up.claimDailyQuota(ctx, acc.AccessToken)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			res.Error = "the session is expired or rejected; log in again and import the new token"
			c.penalise(id, err, time.Now().UTC())
		}
		return res, nil
	}

	res.OK = true
	switch {
	case outcome.AlreadyProcessed:
		// Deliberately not worded as a fresh grant: nothing was added.
		res.Message = "today's allowance was already claimed"
	case outcome.HasDailyQuota:
		res.Message = fmt.Sprintf("granted %d of today's %d %s", outcome.Granted, outcome.DailyQuota, balanceUnit)
	default:
		res.Message = "the daily allowance was granted"
	}
	res.Data = map[string]any{
		"already_processed": outcome.AlreadyProcessed,
		"granted":           outcome.Granted,
		"daily_balance":     outcome.DailyBalance,
		"daily_consumed":    outcome.DailyConsumed,
	}
	if outcome.HasDailyQuota {
		res.Data["daily_quota"] = outcome.DailyQuota
	}
	return res, nil
}
