package loomy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The credit ledger and the daily gift allowance.  Every test drives a
// stubTransport, so the assertions cover the exact request that went out as well
// as the parsed answer.

const (
	pointsPath   = "/api/v1/points/records"
	firstLogin   = "/api/v1/points/first-login"
	creditLedger = `{"permanentBalance":120.0,"dailyBalance":30.0}`
)

// asInt64 accepts whatever JSON decoding or Go arithmetic produced for a count.
func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		t.Fatalf("value %v (%T) is not a number", v, v)
		return 0
	}
}

func TestAccountBalanceReadsTheCreditLedger(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	bal, err := c.AccountBalance(context.Background(), "acc", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 150 || bal.Total != 150 {
		t.Fatalf("Credits/Total = %d/%d, want 150/150", bal.Credits, bal.Total)
	}
	if bal.Unit != "积分" {
		t.Fatalf("Unit = %q, want 积分", bal.Unit)
	}

	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("sent %d requests, want 1", len(reqs))
	}
	if got := reqs[0].Method + " " + reqs[0].URL.Path; got != "GET "+pointsPath {
		t.Fatalf("request = %q, want GET %s", got, pointsPath)
	}
	if got := reqs[0].URL.Query().Get("recordType"); got != "all" {
		t.Fatalf("recordType = %q, want all", got)
	}
}

func TestAccountBalanceAddsTheDailyQuotaToTheTotal(t *testing.T) {
	// The permanent pool is what you spend; the daily quota is what the total
	// ceiling is measured against, so Total is the larger of the two readings.
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"permanentBalance":120.0,"dailyBalance":30.0,"dailyQuota":50.0,"dailyConsumed":10.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	bal, err := c.AccountBalance(context.Background(), "acc", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 150 {
		t.Fatalf("Credits = %d, want 150", bal.Credits)
	}
	if bal.Total != 170 {
		t.Fatalf("Total = %d, want 170 (120 permanent + 50 daily quota)", bal.Total)
	}
}

func TestAccountBalanceHonoursAnExplicitAvailableBalance(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"permanentBalance":120.0,"dailyBalance":30.0,"availableBalance":99.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	bal, err := c.AccountBalance(context.Background(), "acc", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 99 || bal.Total != 99 {
		t.Fatalf("Credits/Total = %d/%d, want 99/99", bal.Credits, bal.Total)
	}
}

func TestAccountBalanceNeverClaimsAnExpiringTranche(t *testing.T) {
	// This vendor has no expiring tranche, so a caller asking what expires soon
	// must be told "nothing", not given an invented number.
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	bal, err := c.AccountBalance(context.Background(), "acc", 30*24*time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Expiring != 0 {
		t.Fatalf("Expiring = %d, want 0: Loomy credits do not expire", bal.Expiring)
	}
}

func TestAccountBalanceRejectsALedgerWithNoBalance(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "acc", 0); err == nil {
		t.Fatal("AccountBalance accepted a ledger with no balance at all")
	} else if !strings.Contains(err.Error(), "no balance") {
		t.Fatalf("error = %v, want a complaint about a missing balance", err)
	}
}

func TestAccountBalanceToleratesAnEmptyLedger(t *testing.T) {
	// data:null is what the vendor sends for a brand-new account.  It must be
	// read as "no balance yet", not as a decode failure.
	rt := alwaysJSON(http.StatusOK, okEnvelope(`null`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	_, err := c.AccountBalance(context.Background(), "acc", 0)
	if err == nil {
		t.Fatal("AccountBalance accepted a null ledger")
	}
	if !strings.Contains(err.Error(), "no balance") {
		t.Fatalf("error = %v, want the missing-balance complaint (a decode error means the null path broke)", err)
	}
}

func TestAccountBalanceRejectsAnUnknownAccount(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)

	_, err := c.AccountBalance(context.Background(), "nope", 0)
	if err == nil {
		t.Fatal("AccountBalance accepted an unknown account id")
	}
	if !strings.Contains(err.Error(), `no account "nope"`) {
		t.Fatalf("error = %v, want a no-account complaint", err)
	}
	if rt.count() != 0 {
		t.Fatalf("sent %d requests for an unknown account, want 0", rt.count())
	}
}

func TestAccountBalanceParksASessionTheVendorRejects(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, failureEnvelope(loomyAuthErrorCode, "登录状态失效"))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "acc", 0); err == nil {
		t.Fatal("AccountBalance accepted a rejected session")
	}
	acc, ok := c.store.lookup("acc")
	if !ok {
		t.Fatal("the account vanished")
	}
	if !acc.dead {
		t.Fatal("a rejected session was not parked; it will be retried forever")
	}
}

func TestAccountBalanceSendsOnlyTheTokenHeader(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "acc", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	req := rt.requests()[0]
	if got := req.Header.Get("token"); got != testToken {
		t.Fatalf("token header = %q, want the session", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q: the business endpoints reject it", got)
	}
}

func TestAccountPackagesSplitsPermanentFromDaily(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	report, err := c.AccountPackages(context.Background(), "acc")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if report.Remain != 150 || report.Size != 150 {
		t.Fatalf("Remain/Size = %d/%d, want 150/150", report.Remain, report.Size)
	}
	if len(report.Packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(report.Packages))
	}
	perm, daily := report.Packages[0], report.Packages[1]
	if perm.Name != "永久积分" || perm.Remain != 120 || perm.Size != 120 {
		t.Fatalf("permanent package = %+v, want 120/120 named 永久积分", perm)
	}
	if daily.Name != "每日赠送" || daily.Remain != 30 || daily.Size != 30 {
		t.Fatalf("daily package = %+v, want 30/30 named 每日赠送", daily)
	}
	if !daily.Cycle {
		t.Fatal("the daily gift was not marked as a repeating allowance")
	}
	if perm.Cycle {
		t.Fatal("the permanent pool was marked as a repeating allowance")
	}
}

func TestAccountPackagesUsesTheDailyQuotaAsTheSizeWhenTheVendorGivesOne(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"permanentBalance":120.0,"dailyBalance":30.0,"dailyQuota":50.0,"dailyConsumed":10.0,"dailyCycleDate":"2026-09-26"}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	report, err := c.AccountPackages(context.Background(), "acc")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	daily := report.Packages[1]
	if daily.Size != 50 {
		t.Fatalf("daily Size = %d, want the declared quota 50", daily.Size)
	}
	if daily.Used != 10 {
		t.Fatalf("daily Used = %d, want the consumed 10", daily.Used)
	}
	if daily.EndTime != "2026-09-26" {
		t.Fatalf("daily EndTime = %q, want the cycle date", daily.EndTime)
	}
	if report.Size != 170 {
		t.Fatalf("report Size = %d, want 120+50", report.Size)
	}
}

func TestAccountPackagesInfersUseWhenOnlyTheBalanceIsGiven(t *testing.T) {
	// dailyQuota 50 with 30 left and no consumed figure: 20 must have been used.
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"permanentBalance":120.0,"dailyBalance":30.0,"dailyQuota":50.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	report, err := c.AccountPackages(context.Background(), "acc")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if got := report.Packages[1].Used; got != 20 {
		t.Fatalf("daily Used = %d, want 20 inferred from 50-30", got)
	}
}

func TestAccountPackagesRejectsAnUnknownAccount(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(creditLedger))
	c := newTestClient(t, `{}`, rt)

	if _, err := c.AccountPackages(context.Background(), "nope"); err == nil {
		t.Fatal("AccountPackages accepted an unknown account id")
	}
}

func TestCheckinActionsIsEmptyWithoutACredential(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	if actions := c.CheckinActions(context.Background()); len(actions) != 0 {
		t.Fatalf("got %d actions with no credential, want none", len(actions))
	}
}

func TestCheckinActionsOffersTheDailyGift(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	seedAccount(t, c, "acc", testToken, 0)

	actions := c.CheckinActions(context.Background())
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want exactly 1", len(actions))
	}
	if actions[0].ID != checkinActionDaily {
		t.Fatalf("action id = %q, want %q", actions[0].ID, checkinActionDaily)
	}
	if strings.TrimSpace(actions[0].Label) == "" {
		t.Fatal("the action has no label for the panel to render")
	}
}

func TestCheckinClaimsTheDailyAllowance(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"alreadyProcessed":false,"dailyQuota":50.0,"dailyBalance":50.0,"dailyConsumed":0.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.Checkin(context.Background(), "acc", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("Checkin failed: %+v", res)
	}
	if res.Action != checkinActionDaily {
		t.Fatalf("Action = %q, want %q", res.Action, checkinActionDaily)
	}
	if res.Message != "granted 50 of today's 50 积分" {
		t.Fatalf("Message = %q", res.Message)
	}
	if got := asInt64(t, res.Data["granted"]); got != 50 {
		t.Fatalf("granted = %d, want 50", got)
	}
	if got := asInt64(t, res.Data["daily_balance"]); got != 50 {
		t.Fatalf("daily_balance = %d, want 50", got)
	}
	if _, ok := res.Data["already_processed"]; !ok {
		t.Fatal("already_processed is missing from the result")
	}
	if res.At == "" {
		t.Fatal("the result carries no timestamp")
	}

	reqs := rt.requests()
	if len(reqs) != 1 {
		t.Fatalf("sent %d requests, want 1", len(reqs))
	}
	if got := reqs[0].Method + " " + reqs[0].URL.Path; got != "POST "+firstLogin {
		t.Fatalf("request = %q, want POST %s", got, firstLogin)
	}
	if body := strings.TrimSpace(rt.lastBody()); body != "{}" {
		t.Fatalf("body = %q, want {}", body)
	}
}

func TestCheckinReportsAnAlreadyClaimedAllowance(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(
		`{"alreadyProcessed":true,"dailyQuota":50.0,"dailyBalance":50.0,"dailyConsumed":0.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.Checkin(context.Background(), "acc", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("an already-claimed allowance is a success, got %+v", res)
	}
	if res.Message != "today's allowance was already claimed" {
		t.Fatalf("Message = %q", res.Message)
	}
	if got := asInt64(t, res.Data["granted"]); got != 0 {
		t.Fatalf("granted = %d, want 0 for an already-claimed day", got)
	}
}

func TestCheckinDefaultsToTheDailyAction(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"alreadyProcessed":false,"dailyQuota":50.0,"dailyBalance":50.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.Checkin(context.Background(), "acc", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("an empty action should default to the daily one, got %+v", res)
	}
	if res.Action != checkinActionDaily {
		t.Fatalf("Action = %q, want %q", res.Action, checkinActionDaily)
	}
}

func TestCheckinRejectsAnUnknownAction(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.Checkin(context.Background(), "acc", "weekly")
	if err != nil {
		t.Fatalf("an unknown action is a result, not a Go error: %v", err)
	}
	if res.OK {
		t.Fatal("an unknown action was reported as a success")
	}
	if !strings.Contains(res.Error, `unknown action "weekly"`) {
		t.Fatalf("Error = %q", res.Error)
	}
	if rt.count() != 0 {
		t.Fatalf("sent %d requests for an unknown action, want 0", rt.count())
	}
}

func TestCheckinRejectsAnUnknownAccount(t *testing.T) {
	c := newTestClient(t, `{}`, nil)

	if _, err := c.Checkin(context.Background(), "nope", checkinActionDaily); err == nil {
		t.Fatal("Checkin accepted an unknown account id")
	}
}

func TestCheckinExplainsARejectedSession(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, failureEnvelope(loomyAuthErrorCode, "登录状态失效"))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	res, err := c.Checkin(context.Background(), "acc", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("a rejected session was reported as a successful check-in")
	}
	if !strings.Contains(res.Error, "log in again") {
		t.Fatalf("Error = %q, want an instruction to log in again", res.Error)
	}
	acc, ok := c.store.lookup("acc")
	if !ok || !acc.dead {
		t.Fatal("the rejected session was not parked")
	}
}

func TestCheckinSendsOnlyTheTokenHeader(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"alreadyProcessed":false,"dailyQuota":50.0,"dailyBalance":50.0}`))
	c := newTestClient(t, `{}`, rt)
	seedAccount(t, c, "acc", testToken, 0)

	if _, err := c.Checkin(context.Background(), "acc", checkinActionDaily); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	req := rt.requests()[0]
	if got := req.Header.Get("token"); got != testToken {
		t.Fatalf("token header = %q, want the session", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q: the business endpoints reject it", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}
