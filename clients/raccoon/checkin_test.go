package raccoon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---- fixtures --------------------------------------------------------------

// grantRecorder is an httptest handler for the one write endpoint this module
// has. It counts calls and remembers the last request so a test can prove what
// was (or was not) sent.
type grantRecorder struct {
	mu      sync.Mutex
	calls   int
	method  string
	path    string
	header  http.Header
	body    string
	status  int
	reply   string
	onServe func()

	// billsReply answers the read-only `GET /api/web/points/v1/bills` call the
	// refusal path makes to find out whether today's extension already
	// happened. Empty means "an empty ledger", which is what most refusal
	// tests want: no completed extension, so the refusal stays retryable.
	billsReply string
	billsFail  bool
	billsCalls int
}

func (g *grantRecorder) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		g.mu.Lock()
		if r.URL.Path == pathBills {
			g.billsCalls++
			billsReply, billsFail := g.billsReply, g.billsFail
			g.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if billsFail {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"code":500000,"message":"ledger unavailable"}`)
				return
			}
			if billsReply == "" {
				billsReply = `{"code":0,"data":{"items":[]}}`
			}
			_, _ = io.WriteString(w, billsReply)
			return
		}
		g.calls++
		g.method = r.Method
		g.path = r.URL.Path
		g.header = r.Header.Clone()
		g.body = string(raw)
		status, reply, onServe := g.status, g.reply, g.onServe
		g.mu.Unlock()
		if onServe != nil {
			onServe()
		}
		w.Header().Set("Content-Type", "application/json")
		if status >= 400 {
			w.WriteHeader(status)
		}
		if reply == "" {
			reply = `{"code":0,"data":{"granted":true}}`
		}
		_, _ = io.WriteString(w, reply)
	}
}

// setBills installs the ledger answer used by the refusal path.
func (g *grantRecorder) setBills(reply string) {
	g.mu.Lock()
	g.billsReply = reply
	g.mu.Unlock()
}

// ledgerReads reports how many times the refusal path consulted the ledger.
func (g *grantRecorder) ledgerReads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.billsCalls
}

func (g *grantRecorder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *grantRecorder) lastHeader(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.header == nil {
		return ""
	}
	return g.header.Get(key)
}

func (g *grantRecorder) setOnServe(f func()) {
	g.mu.Lock()
	g.onServe = f
	g.mu.Unlock()
}

// newGrantClient builds a client whose base URL is a throwaway server and
// whose pool already holds one enabled account.
func newGrantClient(t *testing.T, g *grantRecorder) (*Client, string, string) {
	t.Helper()
	ts := httptest.NewServer(g.handler(t))
	t.Cleanup(ts.Close)

	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), ts.Client())
	id, tok := addGrantAccount(t, c, "u-1", "primary")
	return c, id, tok
}

// addGrantAccount pastes one credential the way the panel would. The token is
// deliberately long-lived so the pool never tries to refresh it behind a test's
// back.
func addGrantAccount(t *testing.T, c *Client, uid, label string) (string, string) {
	t.Helper()
	tok := tokenExpiringIn(t, 3*time.Hour)
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Label: label,
		Fields: map[string]string{
			"access_token":  tok,
			"user_id":       uid,
			"refresh_token": "rt-" + uid,
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec.ID, tok
}

// ---- CheckinActions --------------------------------------------------------

func TestCheckinActionsWithoutAccountsOffersNothing(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	if got := c.CheckinActions(context.Background()); len(got) != 0 {
		t.Fatalf("actions = %#v, want none: a button that cannot work is a failed request the operator pays for", got)
	}
	// The client-level bit is about the contract, the row-level bit is about
	// this machine's accounts. They must disagree here.
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Checkin {
		t.Error("Checkin must be true: the module implements CheckinProvider")
	}
	if caps.CheckinReady {
		t.Error("CheckinReady must be false with no account to claim with")
	}
}

func TestCheckinActionsWithAnAccountOffersTheLoginGrant(t *testing.T) {
	g := &grantRecorder{}
	c, _, _ := newGrantClient(t, g)

	got := c.CheckinActions(context.Background())
	if len(got) != 1 {
		t.Fatalf("actions = %#v, want exactly one", got)
	}
	if got[0].ID != checkinActionLoginPoints {
		t.Errorf("action id = %q, want %q", got[0].ID, checkinActionLoginPoints)
	}
	if strings.TrimSpace(got[0].Label) == "" {
		t.Error("the action must carry a label for the panel button")
	}
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Checkin || !caps.CheckinReady {
		t.Errorf("with one account both bits must be true, got checkin=%v ready=%v", caps.Checkin, caps.CheckinReady)
	}
}

// ---- the request -----------------------------------------------------------

func TestCheckinPostsTheGrantEndpointWithTheDesktopHeaders(t *testing.T) {
	g := &grantRecorder{}
	c, id, tok := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, checkinActionLoginPoints)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if g.count() != 1 {
		t.Fatalf("calls = %d, want exactly 1", g.count())
	}
	if g.method != http.MethodPost {
		t.Errorf("method = %q, want POST", g.method)
	}
	if g.path != pathPointsGrant {
		t.Errorf("path = %q, want %q", g.path, pathPointsGrant)
	}
	if want := "desktop-windows"; g.lastHeader("X-Client-Platform") != want {
		t.Errorf("X-Client-Platform = %q, want %q: the endpoint requires it",
			g.lastHeader("X-Client-Platform"), want)
	}
	if want := clientVersion; g.lastHeader("X-Client-Version") != want {
		t.Errorf("X-Client-Version = %q, want %q", g.lastHeader("X-Client-Version"), want)
	}
	if want := "Bearer " + tok; g.lastHeader("Authorization") != want {
		t.Errorf("Authorization = %q, want the account's own token", g.lastHeader("Authorization"))
	}
	if body := strings.TrimSpace(g.body); body != "" {
		t.Errorf("body = %q, want empty: the vendor sends headers only", body)
	}
	if !res.OK {
		t.Fatalf("res = %#v, want OK", res)
	}
	if res.Data["granted"] != true {
		t.Errorf("data.granted = %#v, want true", res.Data["granted"])
	}
	if res.Action != checkinActionLoginPoints {
		t.Errorf("Action = %q", res.Action)
	}
}

func TestCheckinReportsAGrantThatCarriedAPopup(t *testing.T) {
	g := &grantRecorder{
		reply: `{"code":0,"data":{"granted":true,"popup":{"title":"+10 积分","amount":10}}}`,
	}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %#v, want OK", res)
	}
	popup, ok := res.Data["popup"].(map[string]any)
	if !ok {
		t.Fatalf("data.popup = %#v, want the vendor's popup passed through", res.Data["popup"])
	}
	if popup["title"] != "+10 积分" {
		t.Errorf("popup = %#v, want the vendor's own fields untouched", popup)
	}
}

// TestCheckinNeverInventsAPointTotal pins the honesty rule: the reply carries
// no amount, so neither does the result.
func TestCheckinNeverInventsAPointTotal(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if len(res.Data) != 1 {
		t.Fatalf("data = %#v, want only `granted`: the reply carried no amount", res.Data)
	}
	for _, k := range []string{"credits", "points", "amount", "balance"} {
		if _, ok := res.Data[k]; ok {
			t.Errorf("data carries %q, which the vendor never reported", k)
		}
	}
}

// ---- refusals --------------------------------------------------------------

func TestCheckinTreatsNotGrantedAsARefusalNotAnError(t *testing.T) {
	g := &grantRecorder{reply: `{"code":0,"data":{"granted":false}}`}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("a `granted:false` reply is a refusal, not an error: %v", err)
	}
	if res.OK {
		t.Fatal("OK must be false when no points moved")
	}
	if !strings.Contains(res.Error, "did not grant") {
		t.Errorf("Error = %q, want it to say the vendor granted nothing", res.Error)
	}
	if strings.Contains(res.Error, "once per day") {
		t.Errorf("Error = %q, but the client documents no such window: the reward window is the vendor's business, and guessing at it would be a fabricated claim", res.Error)
	}
	if res.Data["granted"] != false {
		t.Errorf("data.granted = %#v, want false", res.Data["granted"])
	}
}

// TestCheckinRefusalIsNotLatchedForTheDay pins the defect that made the panel
// show "今日已签到/今日不可签" while the vendor was still handing the reward out
// hours later.
//
// The grant endpoint only EXTENDS the day's 300 points. A refusal at 01:00
// says nothing about 21:00, so recording it as a settled "unavailable" would
// disable the row until the next midnight and lose every later chance. The
// recorder's ledger answer here is "no completed extension today", which must
// leave the action retryable.
func TestCheckinRefusalIsNotLatchedForTheDay(t *testing.T) {
	g := &grantRecorder{reply: `{"code":0,"data":{"granted":false}}`}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Skipped {
		t.Error("a refused grant must NOT be recorded as a settled day-level no: the vendor keeps extending the day's points on later sign-ins, so the operator must be able to retry")
	}
	if !strings.Contains(res.Error, "not final for the day") {
		t.Errorf("Error = %q, want it to say the refusal is not the day's last word", res.Error)
	}
	if res.Data["already_done"] == true {
		t.Error("already_done must not be claimed without a completed extension in the ledger")
	}
}

// TestCheckinRefusalResolvesAgainstTheLedger proves the one case that IS a
// settled no: the ledger already holds today's extension. The vendor stamps a
// completed one with an expiry seven days out, while the base 00:00 issuance
// expires the SAME day.
func TestCheckinRefusalResolvesAgainstTheLedger(t *testing.T) {
	now := time.Now().In(checkinDayLocation)
	today := now.Format("2006-01-02")
	expiry := now.AddDate(0, 0, 6).Format("2006-01-02")
	g := &grantRecorder{reply: `{"code":0,"data":{"granted":false}}`}
	c, id, _ := newGrantClient(t, g)
	g.setBills(`{"code":0,"data":{"items":[
		{"biz_type":"daily_expire","event_name":"每日积分到期清零","points":-300,"grant_at":"` + today + `T00:00:00+08:00","expire_at":"` + today + `T23:59:59+08:00"},
		{"biz_type":"daily_grant","event_name":"每日积分发放","points":300,"grant_at":"` + today + `T00:00:00+08:00","expire_at":"` + expiry + `T23:59:59+08:00"}
	]}}`)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false with today's extension already in the ledger; the operator asked whether the day's reward is in hand and the answer is yes: %#v", res)
	}
	if res.Skipped {
		t.Error("a completed extension is a success, not an unavailable skip")
	}
	if res.Data["already_done"] != true {
		t.Errorf("data.already_done = %#v, want true", res.Data["already_done"])
	}
}

// TestCheckinRefusalStaysRetryableWhenTheVendorOnlyIssuedTheBaseAllowance
// guards the discriminator: the 00:00 row is a daily_grant too, but it expires
// the same day, so it is NOT evidence that the desktop extension happened.
func TestCheckinRefusalStaysRetryableWhenTheVendorOnlyIssuedTheBaseAllowance(t *testing.T) {
	today := time.Now().In(checkinDayLocation).Format("2006-01-02")
	g := &grantRecorder{reply: `{"code":0,"data":{"granted":false}}`}
	c, id, _ := newGrantClient(t, g)
	g.setBills(`{"code":0,"data":{"items":[
		{"biz_type":"daily_grant","event_name":"每日积分发放","points":300,"grant_at":"` + today + `T00:00:00+08:00","expire_at":"` + today + `T23:59:59+08:00"}
	]}}`)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Skipped || res.Data["already_done"] == true {
		t.Errorf("the un-extended 00:00 issuance must not read as already claimed: %#v", res)
	}
}

// TestCheckinRefusalStaysRetryableWhenTheLedgerCannotBeRead keeps the honesty
// rule: a diagnostic read that fails must not be promoted into a verdict.
func TestCheckinRefusalStaysRetryableWhenTheLedgerCannotBeRead(t *testing.T) {
	g := &grantRecorder{
		reply:     `{"code":0,"data":{"granted":false}}`,
		billsFail: true,
	}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Skipped {
		t.Error("an unreadable ledger must not turn a refusal into a settled no")
	}
	if res.OK || res.Data["already_done"] == true {
		t.Errorf("a failed ledger read must not fabricate a success: %#v", res)
	}
}

// flexFloatPtr builds the pointer the ledger structs use, mirroring f64 but
// for the module's flexFloat type.
func flexFloatPtr(v float64) *flexFloat {
	f := flexFloat(v)
	return &f
}

// TestDailyExtensionTodayMatchesTheVendorLedger pins the predicate against
// rows copied from a real account's points ledger.
func TestDailyExtensionTodayMatchesTheVendorLedger(t *testing.T) {
	now := time.Date(2026, 10, 23, 21, 41, 0, 0, checkinDayLocation)
	today := now.Format("2006-01-02")

	// The un-extended day: the 21:00-22:00 issuance expires the same day.
	baseOnly := []billItem{{
		BizType: "daily_grant", EventName: "每日积分发放", Points: flexFloatPtr(300),
		GrantAt: today + "T00:00:00+08:00", ExpireAt: today + "T23:59:59+08:00",
	}}
	if dailyExtensionToday(baseOnly, now) {
		t.Error("a same-day expiry is the base issuance, not a completed extension")
	}

	// A completed day: the 00:00 issuance PLUS the extension dated 7 days out.
	extended := append(append([]billItem(nil), baseOnly...), billItem{
		BizType: "daily_grant", EventName: "每日积分发放", Points: flexFloatPtr(300),
		GrantAt: today + "T00:00:00+08:00", ExpireAt: "2026-10-29T23:59:59+08:00",
	})
	if !dailyExtensionToday(extended, now) {
		t.Error("a 7-day expiry is the completed extension and must read as already claimed")
	}

	// Yesterday's extension must not leak into today.
	old := []billItem{{
		BizType: "daily_grant", EventName: "每日积分发放", Points: flexFloatPtr(300),
		GrantAt: "2026-10-22T00:00:00+08:00", ExpireAt: "2026-10-28T23:59:59+08:00",
	}}
	if dailyExtensionToday(old, now) {
		t.Error("yesterday's extension must not count for today")
	}

	// A reward row of another kind never counts.
	other := []billItem{{
		BizType: "reward_grant", EventName: "桌面端登录奖励", Points: flexFloatPtr(3000),
		GrantAt: today + "T00:00:00+08:00", ExpireAt: "2027-10-23T00:00:00+08:00",
	}}
	if dailyExtensionToday(other, now) {
		t.Error("only a daily_grant row can settle the daily reward")
	}
}

func TestCheckinRefusesAnUnknownActionWithoutCallingTheVendor(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "weekly-lottery")
	if err != nil {
		t.Fatalf("an unknown action is a result, not an error: %v", err)
	}
	if res.OK {
		t.Error("OK must be false")
	}
	if !strings.Contains(res.Error, checkinActionLoginPoints) {
		t.Errorf("Error = %q, want it to name the action this module does offer", res.Error)
	}
	if g.count() != 0 {
		t.Errorf("calls = %d, want 0: an unknown action must never reach the vendor", g.count())
	}
}

func TestCheckinWithoutAnAccountIsAResultNotAnError(t *testing.T) {
	c := newTestClient(t, t.TempDir(), `{}`, nil)
	res, err := c.Checkin(context.Background(), "", checkinActionLoginPoints)
	if err != nil {
		t.Fatalf("no account is a fact, not an error: %v", err)
	}
	if res.OK {
		t.Error("OK must be false")
	}
	if !strings.Contains(res.Error, "not configured") {
		t.Errorf("Error = %q, want the ErrNotConfigured wording", res.Error)
	}
}

func TestCheckinOnAParkedAccountRefusesWithoutCallingTheVendor(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)
	if err := c.SetAccountEnabled(context.Background(), id, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("OK must be false for a parked account")
	}
	if !strings.Contains(res.Error, "parked") {
		t.Errorf("Error = %q, want it to say the account is parked", res.Error)
	}
	if g.count() != 0 {
		t.Errorf("calls = %d, want 0: a parked credential must not be spent", g.count())
	}
}

func TestCheckinUnknownAccountIsAnError(t *testing.T) {
	g := &grantRecorder{}
	c, _, _ := newGrantClient(t, g)

	if _, err := c.Checkin(context.Background(), "uid:nope", ""); err == nil {
		t.Fatal("an unknown account id must be an error: the caller named something that does not exist")
	}
	if g.count() != 0 {
		t.Errorf("calls = %d, want 0", g.count())
	}
}

// ---- failures teach the pool ----------------------------------------------

func TestCheckinOnARejectedTokenParksTheAccount(t *testing.T) {
	g := &grantRecorder{status: http.StatusUnauthorized, reply: `{"code":200003,"message":"token expired"}`}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK must be false")
	}
	if res.Data["error_kind"] != string(kindAuth) {
		t.Errorf("error_kind = %#v, want %q", res.Data["error_kind"], kindAuth)
	}
	// Assert on the public surface: an entry's raw state can read "unknown"
	// while the pool still considers it selectable.
	if got := c.pool.usable(); len(got) != 0 {
		t.Errorf("usable() = %d entries after a rejected token, want 0", len(got))
	}
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Status.Accounts = %d rows, want 1", len(st.Accounts))
	}
	if got := st.Accounts[0].State; got != stateInvalid {
		t.Errorf("state = %q after a rejected token, want %q", got, stateInvalid)
	}
	if st.Accounts[0].Enabled {
		t.Error("a rejected token must disable the account until the operator revives it")
	}
}

func TestCheckinOnAClientErrorDoesNotParkAHealthyAccount(t *testing.T) {
	g := &grantRecorder{status: http.StatusBadRequest, reply: `{"code":400001,"message":"bad request"}`}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK must be false")
	}
	if res.Data["error_kind"] != string(kindClient) {
		t.Errorf("error_kind = %#v, want %q", res.Data["error_kind"], kindClient)
	}
	// Same rule as stream_test.go's TestAClientErrorNeverParksAHealthyAccount:
	// ask the pool, not the raw entry, because a non-parking failure still
	// writes an internal state name that Status() normalises back to ready.
	if got := c.pool.usable(); len(got) != 1 {
		t.Errorf("usable() = %d entries after a 400, want the account still selectable", len(got))
	}
	st := c.Status(context.Background())
	if got := st.Accounts[0].State; got != stateReady {
		t.Errorf("state = %q after a 400, want %q", got, stateReady)
	}
}

func TestCheckinNeverLeaksTheTokenIntoTheError(t *testing.T) {
	const tok = "SECRET-TOKEN-VALUE-0123456789"
	g := &grantRecorder{status: http.StatusUnauthorized, reply: `{"code":200003,"message":"token expired"}`}
	ts := httptest.NewServer(g.handler(t))
	t.Cleanup(ts.Close)
	c := newTestClient(t, t.TempDir(), fmt.Sprintf(`{"base_url":%q}`, ts.URL), ts.Client())
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": tok, "user_id": "u-leak"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	res, err := c.Checkin(context.Background(), rec.ID, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if strings.Contains(res.Error, tok) {
		t.Fatalf("Error = %q, want the token scrubbed", res.Error)
	}
	if strings.Contains(fmt.Sprint(res.Data), tok) {
		t.Fatalf("Data = %#v, want the token scrubbed", res.Data)
	}
}

// ---- shape of the result ---------------------------------------------------

func TestCheckinStampsElapsedAndTimestamp(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)

	res, err := c.Checkin(context.Background(), id, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.AccountID != id {
		t.Errorf("AccountID = %q, want %q", res.AccountID, id)
	}
	if res.Action != checkinActionLoginPoints {
		t.Errorf("Action = %q, want the default action when the caller named none", res.Action)
	}
	if res.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d", res.ElapsedMS)
	}
	if _, err := time.Parse(time.RFC3339, res.At); err != nil {
		t.Errorf("At = %q, want an RFC3339 timestamp: %v", res.At, err)
	}
}

func TestCheckinUsesTheNamedAccountNotJustTheFirst(t *testing.T) {
	g := &grantRecorder{}
	c, _, _ := newGrantClient(t, g)
	secondID, secondTok := addGrantAccount(t, c, "u-2", "second")

	var seen string
	g.setOnServe(func() {
		g.mu.Lock()
		if g.header != nil {
			seen = g.header.Get("Authorization")
		}
		g.mu.Unlock()
	})

	if _, err := c.Checkin(context.Background(), secondID, ""); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if g.count() != 1 {
		t.Fatalf("calls = %d, want 1", g.count())
	}
	if want := "Bearer " + secondTok; seen != want {
		t.Errorf("Authorization = %q, want the NAMED account's token, not the pool's first pick", seen)
	}
}

// ---- wire shape of the envelope -------------------------------------------

func TestTheGrantEndpointEnvelopeIsUnwrapped(t *testing.T) {
	g := &grantRecorder{reply: `{"code":0,"data":{"granted":true}}`}
	c, id, _ := newGrantClient(t, g)
	res, err := c.Checkin(context.Background(), id, "")
	if err != nil || !res.OK {
		t.Fatalf("res = %#v, err = %v", res, err)
	}

	// A non-zero business code inside a 200 response is still a refusal, and
	// the vendor's own message must survive.
	g2 := &grantRecorder{reply: `{"code":400001,"message":"already granted"}`}
	c2, id2, _ := newGrantClient(t, g2)
	res2, err := c2.Checkin(context.Background(), id2, "")
	if err != nil {
		t.Fatalf("a business code is a result, not an error: %v", err)
	}
	if res2.OK {
		t.Error("a non-zero business code must not read as a grant")
	}
	if !strings.Contains(res2.Error, "already granted") {
		t.Errorf("Error = %q, want the vendor's own message", res2.Error)
	}
}

// TestTheGrantIsSentOncePerClick pins that this module has no retry loop: the
// reward is idempotent server-side, but re-firing it is not our business.
func TestTheGrantIsSentOncePerClick(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)
	for i := 0; i < 3; i++ {
		if _, err := c.Checkin(context.Background(), id, ""); err != nil {
			t.Fatalf("Checkin #%d: %v", i, err)
		}
	}
	if g.count() != 3 {
		t.Errorf("calls = %d, want exactly one per click (3)", g.count())
	}
}

// TestTheGrantRequestIsEmptyJSON pins the vendor's own request shape.
func TestTheGrantRequestIsEmptyJSON(t *testing.T) {
	g := &grantRecorder{}
	c, id, _ := newGrantClient(t, g)
	if _, err := c.Checkin(context.Background(), id, ""); err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if got := strings.TrimSpace(g.body); got != "" {
		t.Errorf("body = %q", got)
	}
	if ct := g.lastHeader("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}
