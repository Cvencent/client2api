package minimaxcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// fixtures
//
// The bodies below are trimmed copies of what the live vendor actually
// answered, so the tests assert against the real shape -- including the odd
// parts, like credit/details having no data wrapper at all.
// ---------------------------------------------------------------------------

const (
	signinStatusPathTest = "/minimax-cloud/api/v1/signin/status"
	signinClaimPathTest  = "/minimax-cloud/api/v1/signin/claim"
	creditDetailPathTest = "/minimax-cloud/api/v1/credit/details"
)

// signinPanelJSON builds a seven-day board whose first day carries the state
// under test.  The vendor validates that the board really has seven days.
func signinPanelJSON(t *testing.T, todayStatus int, points int64, isToday bool) string {
	t.Helper()
	days := make([]map[string]any, 0, 7)
	for i := 1; i <= 7; i++ {
		day := map[string]any{
			"day_no":       i,
			"points":       int64(800),
			"bonus_points": int64(400),
			"status":       signinDayUpcoming,
			"is_today":     false,
		}
		if i == 1 {
			day["status"] = todayStatus
			day["points"] = points
			day["is_today"] = isToday
		}
		days = append(days, day)
	}
	raw, err := json.Marshal(map[string]any{
		"data":      map[string]any{"scene": 2, "days": days},
		"base_resp": map[string]any{"status_code": 0, "status_msg": "ok"},
	})
	if err != nil {
		t.Fatalf("marshal panel: %v", err)
	}
	return string(raw)
}

const claimOKBody = `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{` +
	`"claim_id":"2105333467448352775","claim_result":1,"day_no":1,"points":800,` +
	`"expire_at_ms":1793376000000}}`

// creditDetailsBody is the real credit/details payload: details and total_count
// sit at the TOP level, with no data wrapper.
const creditDetailsBody = `{"details":[` +
	`{"credit_type":2,"granted_amount":"800.00","remaining_amount":"781.47",` +
	`"consumed_amount":"18.53","granted_at_ms":1790667507262,"expire_at_ms":1793203200000}` +
	`],"total_count":1,"base_resp":{"status_code":0,"status_msg":"ok"}}`

// signinConfig is one enabled account with a pinned timezone, so the tests can
// assert on the timezone_id the module sends instead of on whatever zone the
// developer's machine happens to be in.
func signinConfig() map[string]any {
	return map[string]any{
		"timezone": "Asia/Shanghai",
		"accounts": []map[string]any{
			{"id": "acct-1", "label": "Test account", "access_token": testToken},
		},
	}
}

// scriptedSignin routes the three cloud routes to canned bodies and counts how
// often each was hit, so a test can prove the claim was never posted.
type scriptedSignin struct {
	statusBody  string
	statusCode  int
	claimBody   string
	creditBody  string
	statusCalls int
	claimCalls  int
}

func (s *scriptedSignin) handler() func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case signinStatusPathTest:
			s.statusCalls++
			code := s.statusCode
			if code == 0 {
				code = http.StatusOK
			}
			return jsonResponse(code, s.statusBody), nil
		case signinClaimPathTest:
			s.claimCalls++
			return jsonResponse(http.StatusOK, s.claimBody), nil
		case creditDetailPathTest:
			return jsonResponse(http.StatusOK, s.creditBody), nil
		default:
			return nil, fmt.Errorf("scriptedSignin: unexpected request to %s", req.URL.Path)
		}
	}
}

func checkin(t *testing.T, c *Client, action string) core.CheckinResult {
	t.Helper()
	res, err := c.Checkin(context.Background(), "acct-1", action)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// the claim
// ---------------------------------------------------------------------------

func TestCheckinClaimsWhenTodayIsClaimable(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, ft := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, signinActionDaily)

	if !res.OK {
		t.Fatalf("Checkin did not report success: %+v", res)
	}
	if res.Action != signinActionDaily {
		t.Errorf("action = %q, want %q", res.Action, signinActionDaily)
	}
	if !strings.Contains(res.Message, "800") {
		t.Errorf("message %q does not mention the 800 credits gained", res.Message)
	}
	if res.Data["already_done"] != false {
		t.Errorf("already_done = %v, want false", res.Data["already_done"])
	}
	if got := res.Data["claim_id"]; got != "2105333467448352775" {
		t.Errorf("claim_id = %v, want the vendor's claim id", got)
	}
	if got := res.Data["expires_at"]; got != "2026-10-30T16:00:00Z" {
		t.Errorf("expires_at = %v, want the claim's expiry in UTC", got)
	}
	// bonus_points is already folded into points; adding the two would double
	// the reward the operator reads.
	if got := res.Data["points"]; got != int64(800) {
		t.Errorf("points = %v, want 800 (bonus included, not added)", got)
	}
	if res.ElapsedMS < 0 || res.At == "" {
		t.Errorf("result is not stamped: %+v", res)
	}

	if ft.count() != 2 {
		t.Fatalf("made %d requests, want 2 (status then claim)", ft.count())
	}
	ask := ft.requestAt(0)
	if ask.URL.Path != signinStatusPathTest {
		t.Errorf("first call went to %s, want the status route", ask.URL.Path)
	}
	if got := ask.URL.Query().Get("timezone_id"); got != "Asia/Shanghai" {
		t.Errorf("status timezone_id = %q, want Asia/Shanghai", got)
	}
	post := ft.requestAt(1)
	if post.Method != http.MethodPost {
		t.Errorf("claim method = %s, want POST", post.Method)
	}
	if post.URL.Path != signinClaimPathTest {
		t.Errorf("second call went to %s, want the claim route", post.URL.Path)
	}
	// The live vendor rejects the claim with 1406010011 "invalid timezone_id"
	// unless the parameter is present, even though the desktop client's shared
	// axios instance is what supplies it there.
	if got := post.URL.Query().Get("timezone_id"); got != "Asia/Shanghai" {
		t.Errorf("claim timezone_id = %q, want Asia/Shanghai", got)
	}
	if got := ft.bodyAt(1); got != "{}" {
		t.Errorf("claim body = %q, want an empty JSON object", got)
	}
}

func TestCheckinDoesNotClaimAgainWhenTodayIsAlreadyDone(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimed, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, signinActionDaily)

	if !res.OK {
		t.Fatalf("an already-claimed day is a success, got %+v", res)
	}
	if res.Data["already_done"] != true {
		t.Errorf("already_done = %v, want true", res.Data["already_done"])
	}
	if !strings.Contains(res.Message, "第 1 天") {
		t.Errorf("message %q does not name the day", res.Message)
	}
	// The whole point of status-then-claim: never post the claim blind.
	if s.claimCalls != 0 {
		t.Errorf("posted the claim %d times on an already-claimed day, want 0", s.claimCalls)
	}
}

func TestCheckinRefusesToClaimWhenNoDayIsToday(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayDisabled, 800, false),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, signinActionDaily)

	if res.OK {
		t.Fatalf("reported success for a panel with no current day: %+v", res)
	}
	if !strings.Contains(res.Error, "Asia/Shanghai") {
		t.Errorf("error %q does not name the timezone that failed to line up", res.Error)
	}
	if s.claimCalls != 0 {
		t.Errorf("claimed blind %d times, want 0", s.claimCalls)
	}
}

func TestCheckinReportsAVendorRefusalAsAResultNotAnError(t *testing.T) {
	s := &scriptedSignin{
		statusBody: `{"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`,
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, signinActionDaily)

	if res.OK {
		t.Fatalf("a rejected status read is not a success: %+v", res)
	}
	if !strings.Contains(res.Error, "invalid timezone_id") {
		t.Errorf("error %q does not carry the vendor's own message", res.Error)
	}
	if s.claimCalls != 0 {
		t.Errorf("claimed after a failed status read %d times, want 0", s.claimCalls)
	}
	// A vendor complaint that is not one of the cooling kinds parks nothing, so
	// the result must not tell the operator to wait.
	if strings.Contains(res.Error, "cooling down") {
		t.Errorf("announced a cooldown that was not applied: %q", res.Error)
	}
}

func TestCheckinAnnouncesTheCooldownOnlyWhenOneWasApplied(t *testing.T) {
	s := &scriptedSignin{
		statusBody: `{"error":{"message":"too many requests"}}`,
		statusCode: http.StatusTooManyRequests,
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, signinActionDaily)

	if res.OK {
		t.Fatalf("a 429 is not a success: %+v", res)
	}
	if !strings.Contains(res.Error, "cooling down") {
		t.Errorf("a rate-limited read parks the account, but error %q does not say so", res.Error)
	}
	if got := res.Data["error_kind"]; got != string(core.FailureRateLimited) {
		t.Errorf("error_kind = %v, want %q", got, core.FailureRateLimited)
	}
}

func TestCheckinRefusesAnUnknownActionWithoutCallingTheVendor(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, ft := newTestClient(t, signinConfig(), s.handler())

	res := checkin(t, c, "something-else")

	if res.OK {
		t.Fatalf("an unknown action is not a success: %+v", res)
	}
	if !strings.Contains(res.Error, signinActionDaily) {
		t.Errorf("error %q does not name the action this client does offer", res.Error)
	}
	if ft.count() != 0 {
		t.Errorf("made %d requests for an action it does not implement, want 0", ft.count())
	}
}

func TestCheckinRefusesAParkedAccount(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, ft := newTestClient(t, signinConfig(), s.handler())
	if err := c.SetAccountEnabled(context.Background(), "acct-1", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	res := checkin(t, c, signinActionDaily)

	if res.OK {
		t.Fatalf("a disabled account cannot claim: %+v", res)
	}
	if !strings.Contains(res.Error, "parked") {
		t.Errorf("error %q does not explain that the account is disabled", res.Error)
	}
	if ft.count() != 0 {
		t.Errorf("called the vendor for a parked account %d times, want 0", ft.count())
	}
}

func TestCheckinReportsAMissingAccountAsAnError(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	_, err := c.Checkin(context.Background(), "nobody", signinActionDaily)

	if err == nil {
		t.Fatal("Checkin on an unknown account returned no error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not say the account is missing", err)
	}
}

func TestCheckinActionsOffersNothingWhenNoAccountCanAct(t *testing.T) {
	s := &scriptedSignin{statusBody: signinPanelJSON(t, signinDayClaimable, 800, true)}
	empty, _ := newTestClient(t, map[string]any{"accounts": []map[string]any{}}, s.handler())
	if got := empty.CheckinActions(context.Background()); len(got) != 0 {
		t.Errorf("offered %d actions with no account, want none", len(got))
	}

	withAccount, _ := newTestClient(t, signinConfig(), s.handler())
	got := withAccount.CheckinActions(context.Background())
	if len(got) != 1 {
		t.Fatalf("offered %d actions, want 1", len(got))
	}
	if got[0].ID != signinActionDaily {
		t.Errorf("action id = %q, want %q", got[0].ID, signinActionDaily)
	}
	if got[0].Label == "" || got[0].Help == "" {
		t.Errorf("action is missing its operator-facing text: %+v", got[0])
	}
}

// ---------------------------------------------------------------------------
// balance and packages
// ---------------------------------------------------------------------------

func TestAccountBalanceReadsTheUnwrappedCreditDetails(t *testing.T) {
	s := &scriptedSignin{creditBody: creditDetailsBody}
	c, ft := newTestClient(t, signinConfig(), s.handler())

	bal, err := c.AccountBalance(context.Background(), "acct-1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}

	if bal.Credits != 781 {
		t.Errorf("credits = %d, want 781 (781.47 rounded)", bal.Credits)
	}
	if bal.Total != 800 {
		t.Errorf("total = %d, want 800", bal.Total)
	}
	if bal.EarliestRemaining != 781 {
		t.Errorf("earliest_remaining = %d, want 781", bal.EarliestRemaining)
	}
	want := time.Unix(1793203200, 0)
	if !bal.EarliestAt.Equal(want) {
		t.Errorf("earliest_at = %s, want %s", bal.EarliestAt, want)
	}
	// soon == 0 means "do not compute that bucket at all".
	if bal.Expiring != 0 {
		t.Errorf("expiring = %d with soon=0, want 0", bal.Expiring)
	}
	if ft.count() != 1 {
		t.Errorf("made %d requests, want 1", ft.count())
	}
	if got := ft.requestAt(0).URL.Path; got != creditDetailPathTest {
		t.Errorf("called %s, want the credit details route", got)
	}
}

func TestAccountBalanceCountsTranchesExpiringInsideTheWindow(t *testing.T) {
	s := &scriptedSignin{creditBody: creditDetailsBody}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	soon, err := c.AccountBalance(context.Background(), "acct-1", time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if soon.Expiring != 0 {
		t.Errorf("expiring = %d for a tranche a year out, want 0", soon.Expiring)
	}

	far, err := c.AccountBalance(context.Background(), "acct-1", 100000*time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if far.Expiring != 781 {
		t.Errorf("expiring = %d for a tranche inside the window, want 781", far.Expiring)
	}
}

func TestAccountBalanceIgnoresAFullySpentTranche(t *testing.T) {
	// The first row is empty and expires soonest; if it were allowed to win the
	// "earliest" slot the operator would be told nothing is left.
	body := `{"details":[` +
		`{"credit_type":2,"granted_amount":"800.00","remaining_amount":"0.00",` +
		`"consumed_amount":"800.00","granted_at_ms":1790667507262,"expire_at_ms":1793203200000},` +
		`{"credit_type":1,"granted_amount":"200.00","remaining_amount":"120.00",` +
		`"consumed_amount":"80.00","granted_at_ms":1790667507262,"expire_at_ms":1796203200000}` +
		`],"total_count":2,"base_resp":{"status_code":0,"status_msg":"ok"}}`
	s := &scriptedSignin{creditBody: body}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	bal, err := c.AccountBalance(context.Background(), "acct-1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}

	if bal.Credits != 120 {
		t.Errorf("credits = %d, want 120", bal.Credits)
	}
	if bal.EarliestRemaining != 120 {
		t.Errorf("earliest_remaining = %d, want the tranche that still holds something", bal.EarliestRemaining)
	}
	if want := time.Unix(1796203200, 0); !bal.EarliestAt.Equal(want) {
		t.Errorf("earliest_at = %s, want %s", bal.EarliestAt, want)
	}
}

func TestAccountPackagesNamesAndSortsTheTranches(t *testing.T) {
	body := `{"details":[` +
		`{"credit_type":2,"granted_amount":"800.00","remaining_amount":"781.47",` +
		`"consumed_amount":"18.53","granted_at_ms":1790667507262,"expire_at_ms":1793203200000},` +
		`{"credit_type":1,"granted_amount":"200.00","remaining_amount":"100.00",` +
		`"consumed_amount":"100.00","granted_at_ms":1790667507262,"expire_at_ms":1796203200000}` +
		`],"total_count":2,"base_resp":{"status_code":0,"status_msg":"ok"}}`
	s := &scriptedSignin{creditBody: body}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	out, err := c.AccountPackages(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}

	if len(out.Packages) != 2 {
		t.Fatalf("got %d packages, want 2", len(out.Packages))
	}
	if out.Remain != 881 {
		t.Errorf("remain = %d, want 881", out.Remain)
	}
	if out.Size != 1000 {
		t.Errorf("size = %d, want 1000", out.Size)
	}
	if out.Packages[0].Remain < out.Packages[1].Remain {
		t.Errorf("packages are not sorted by remaining credits: %+v", out.Packages)
	}
	first := out.Packages[0]
	if first.Name != "签到积分" {
		t.Errorf("name = %q, want the check-in label the desktop client uses", first.Name)
	}
	if first.Size != 800 || first.Used != 19 {
		t.Errorf("package = %+v, want size 800 and 19 used", first)
	}
	if first.Cycle {
		t.Error("a granted tranche is a one-off, not a renewing cycle")
	}
	if first.EndTime != "2026-10-28T16:00:00Z" {
		t.Errorf("end_time = %q, want the RFC3339 UTC expiry", first.EndTime)
	}
	if first.ExpiresAt != 1793203200000 {
		t.Errorf("expires_at = %d, want the raw millisecond stamp", first.ExpiresAt)
	}
	if first.CreatedAt != "2026-09-29T07:38:27Z" {
		t.Errorf("created_at = %q, want the grant time", first.CreatedAt)
	}
	if out.Packages[1].Name != "购买积分" {
		t.Errorf("second package name = %q, want the purchased label", out.Packages[1].Name)
	}
}

func TestAccountBalanceReportsAMissingAccountAsAnError(t *testing.T) {
	s := &scriptedSignin{creditBody: creditDetailsBody}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	if _, err := c.AccountBalance(context.Background(), "nobody", 0); err == nil {
		t.Fatal("AccountBalance on an unknown account returned no error")
	}
	if _, err := c.AccountPackages(context.Background(), "nobody"); err == nil {
		t.Fatal("AccountPackages on an unknown account returned no error")
	}
}

// ---------------------------------------------------------------------------
// the pieces the routes are built from
// ---------------------------------------------------------------------------

func TestCreditAmountRoundsAndClampsNegatives(t *testing.T) {
	cases := []struct {
		raw   string
		want  int64
		valid bool
	}{
		{"800.00", 800, true},
		{"781.47", 781, true},
		{"781.50", 782, true},
		{"1,234.00", 1234, true},
		{"  12.4 ", 12, true},
		{"-5.00", 0, true}, // the desktop client clamps a debt to zero
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range cases {
		got, ok := creditAmount(tc.raw)
		if ok != tc.valid {
			t.Errorf("creditAmount(%q) valid = %v, want %v", tc.raw, ok, tc.valid)
			continue
		}
		if got != tc.want {
			t.Errorf("creditAmount(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestSigninTimezonePrefersTheConfiguredZone(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimed, 800, true),
		claimBody:  claimOKBody,
	}
	cfg := signinConfig()
	cfg["timezone"] = "Europe/Berlin"
	c, ft := newTestClient(t, cfg, s.handler())

	checkin(t, c, signinActionDaily)

	if got := ft.requestAt(0).URL.Query().Get("timezone_id"); got != "Europe/Berlin" {
		t.Errorf("timezone_id = %q, want the configured zone", got)
	}
}

func TestSigninTimezoneFallsBackToALoadableZone(t *testing.T) {
	// Whatever the host is, the fallback must be a name the vendor accepts:
	// an unloadable or invented zone is answered with 1406010011.
	zone := detectSigninTimezone()
	if zone == "" {
		t.Fatal("detectSigninTimezone returned an empty zone")
	}
	if _, err := time.LoadLocation(zone); err != nil {
		t.Errorf("detected zone %q does not load: %v", zone, err)
	}
	if strings.ContainsAny(zone, " \t") {
		t.Errorf("detected zone %q contains whitespace", zone)
	}
}

func TestCloudCallSurfacesANonZeroBaseResp(t *testing.T) {
	s := &scriptedSignin{
		statusBody: `{"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	acct := Account{ID: "acct-1", Token: testToken, Enabled: true}
	_, _, err := c.cloudCall(context.Background(), &acct, http.MethodGet, signinStatusPathTest, nil, nil)
	if err == nil {
		t.Fatal("cloudCall accepted an HTTP 200 carrying a non-zero base_resp")
	}
	var ue *upstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want the module's own *upstreamError", err)
	}
	if !strings.Contains(ue.Msg, "invalid timezone_id") {
		t.Errorf("message %q does not carry the vendor's text", ue.Msg)
	}
}

func TestCloudCallRedactsTheTokenFromAnUpstreamEcho(t *testing.T) {
	s := &scriptedSignin{
		statusBody: `{"base_resp":{"status_code":1004,"status_msg":"token ` + testToken + ` rejected"}}`,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	acct := Account{ID: "acct-1", Token: testToken, Enabled: true}
	_, _, err := c.cloudCall(context.Background(), &acct, http.MethodGet, signinStatusPathTest, nil, nil)
	if err == nil {
		t.Fatal("cloudCall accepted a rejected credential")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error %q leaks the credential", err)
	}
}

func TestCloudCallSendsNoAnthropicVersionHeader(t *testing.T) {
	// These routes are not the Messages endpoint; announcing the protocol would
	// be a lie about what is being spoken.
	s := &scriptedSignin{statusBody: signinPanelJSON(t, signinDayClaimed, 800, true)}
	c, ft := newTestClient(t, signinConfig(), s.handler())

	acct := Account{ID: "acct-1", Token: testToken, Enabled: true}
	if _, _, err := c.cloudCall(context.Background(), &acct, http.MethodGet, signinStatusPathTest, nil, nil); err != nil {
		t.Fatalf("cloudCall: %v", err)
	}

	req := ft.requestAt(0)
	if got := req.Header.Get("anthropic-version"); got != "" {
		t.Errorf("anthropic-version = %q, want it absent on a cloud route", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want the account's bearer token", got)
	}
}
