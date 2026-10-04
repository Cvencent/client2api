package qwenwork

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// checkinClient builds a client whose pool holds the accounts named in cfg and
// whose vendor traffic goes through rt.
func checkinClient(t *testing.T, cfg string, rt *fakeTransport) *Client {
	t.Helper()
	clearCredentialEnv(t)

	deps := core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(cfg),
		HTTPClient: &http.Client{Transport: rt},
		Logf:       func(string, ...any) {},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	qc, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", c)
	}
	return qc
}

// checkinScript answers the check-in endpoints from a fixed pair of bodies:
// every GET gets statusBody, every POST gets claimBody.
func checkinScript(statusBody, claimBody string) func(int, *http.Request, string) (*http.Response, error) {
	return func(_ int, req *http.Request, _ string) (*http.Response, error) {
		if req.Method == http.MethodPost {
			return fakeResponse(req, http.StatusOK, claimBody), nil
		}
		return fakeResponse(req, http.StatusOK, statusBody), nil
	}
}

// oneAccount is the config every check-in test starts from: a single configured
// account, so the pool has exactly one credential and no refresh token.
const oneAccount = `{"accounts":[{"uid":"1","access_token":"t1"}]}`

// claimableStatus is the one status value that makes Checkin write.
const claimableStatus = `{"status":"CLAIMABLE","rewardCredits":100}`

// runCheckin calls Checkin with the daily action and returns everything the
// caller wants to inspect.
func runCheckin(t *testing.T, c *Client, id string) (core.CheckinResult, error) {
	t.Helper()
	return c.Checkin(context.Background(), id, checkinActionDaily)
}

// ---------------------------------------------------------------------------
// the capability itself
// ---------------------------------------------------------------------------

func TestCheckinOffersOneActionPerAccountPool(t *testing.T) {
	rt := &fakeTransport{}
	qc := checkinClient(t, oneAccount, rt)

	actions := qc.CheckinActions(context.Background())
	if len(actions) != 1 {
		t.Fatalf("CheckinActions = %+v, want exactly one action", actions)
	}
	if actions[0].ID != checkinActionDaily {
		t.Errorf("action id = %q, want %q", actions[0].ID, checkinActionDaily)
	}
	if actions[0].Label == "" {
		t.Error("the action needs a label: an unlabelled button is unusable")
	}
}

func TestCheckinOffersNothingWithoutAnAccount(t *testing.T) {
	rt := &fakeTransport{}
	qc := checkinClient(t, `{}`, rt)

	if actions := qc.CheckinActions(context.Background()); len(actions) != 0 {
		t.Errorf("CheckinActions = %+v, want none: a button that cannot act makes the "+
			"operator pay for the discovery with a failed request", actions)
	}
}

// ---------------------------------------------------------------------------
// the two-step happy paths
// ---------------------------------------------------------------------------

func TestCheckinTreatsClaimedTodayAsSuccessWithoutWriting(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(`{"status":"CLAIMED_TODAY","currentStreakDays":3}`, "")
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Errorf("ok = false (error %q): the vendor calls this a success, not a refusal", res.Error)
	}
	if res.Data["already_done"] != true {
		t.Errorf("data = %+v, want already_done=true", res.Data)
	}
	if res.Data["current_streak_days"] != int64(3) {
		t.Errorf("data = %+v, want the streak the vendor reported", res.Data)
	}
	if rt.count() != 1 {
		t.Errorf("made %d requests, want 1: an already-claimed day must cost a read and no write", rt.count())
	}
	if got := rt.at(0).method; got != http.MethodGet {
		t.Errorf("method = %q, want GET", got)
	}
}

func TestCheckinClaimsWhenTheVendorOffersAReward(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"CLAIMED","rewardCredits":100,"expiresAt":1790000000000}`)
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok = false (error %q)", res.Error)
	}
	if res.Data["result"] != checkinResultClaimed {
		t.Errorf("data = %+v, want result=%s", res.Data, checkinResultClaimed)
	}
	if res.Data["claimed_credits"] != float64(100) {
		t.Errorf("data = %+v, want claimed_credits=100", res.Data)
	}
	if res.Data["credits_expire_at"] != int64(1790000000000) {
		t.Errorf("data = %+v, want the expiry the vendor sent", res.Data)
	}
	if !strings.Contains(res.Message, "100") {
		t.Errorf("message = %q, want the reward amount", res.Message)
	}
	if rt.count() != 2 {
		t.Fatalf("made %d requests, want the status read then the claim", rt.count())
	}
	if got := rt.at(1).method; got != http.MethodPost {
		t.Errorf("second request method = %q, want POST", got)
	}
	if !strings.HasSuffix(rt.at(1).url, checkinClaimPath) {
		t.Errorf("second request url = %q, want it to end in %s", rt.at(1).url, checkinClaimPath)
	}
}

func TestCheckinTreatsAnAlreadyClaimedClaimAsSuccess(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"ALREADY_CLAIMED"}`)
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Errorf("ok = false (error %q): losing the race is not a failure", res.Error)
	}
	if res.Data["already_done"] != true {
		t.Errorf("data = %+v, want already_done=true", res.Data)
	}
}

func TestCheckinUsesTheOnlyUsableAccountWhenNoIDIsGiven(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(`{"status":"CLAIMED_TODAY"}`, "")
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok = false (error %q)", res.Error)
	}
	if res.AccountID != "uid:1" {
		t.Errorf("account = %q, want the only account in the pool", res.AccountID)
	}
}

// ---------------------------------------------------------------------------
// the paths that must NOT write
// ---------------------------------------------------------------------------

func TestCheckinNeverWritesWhenTheCampaignIsOff(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{checkinStatusDisabled, "DISABLED"},
		{checkinStatusNotStarted, "NOT_STARTED"},
		{checkinStatusEnded, "ENDED"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			rt := &fakeTransport{}
			// A POST would answer CLAIMED; the test fails if one is ever sent.
			rt.handler = checkinScript(`{"status":"`+tc.status+`"}`, `{"result":"CLAIMED","rewardCredits":100}`)
			qc := checkinClient(t, oneAccount, rt)

			res, err := runCheckin(t, qc, "uid:1")
			if err != nil {
				t.Fatalf("Checkin: %v", err)
			}
			if res.OK {
				t.Errorf("ok = true for status %s, want a refusal", tc.status)
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("error = %q, want it to name %s", res.Error, tc.want)
			}
			if rt.count() != 1 {
				t.Errorf("made %d requests, want 1: a switched-off campaign must not be "+
					"claimed blind", rt.count())
			}
		})
	}
}

func TestCheckinReportsAnUnrecognisedStatusWithoutWriting(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(`{"status":"SOMETHING_NEW"}`, `{"result":"CLAIMED"}`)
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("ok = true for a status we do not understand")
	}
	if !strings.Contains(res.Error, "unrecognised") || !strings.Contains(res.Error, "SOMETHING_NEW") {
		t.Errorf("error = %q, want it to quote the status it could not read", res.Error)
	}
	if rt.count() != 1 {
		t.Errorf("made %d requests, want 1: an unknown status must not be treated as claimable", rt.count())
	}
}

func TestCheckinReportsAnUnrecognisedClaimResult(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"SOMETHING_NEW"}`)
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("ok = true for a claim result we do not understand")
	}
	if !strings.Contains(res.Error, "unrecognised") || !strings.Contains(res.Error, "SOMETHING_NEW") {
		t.Errorf("error = %q, want it to quote the result it could not read", res.Error)
	}
}

func TestCheckinRefusesADisabledAccountWithoutTouchingTheUpstream(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"CLAIMED"}`)
	qc := checkinClient(t, oneAccount, rt)

	if err := qc.pool.setEnabled("uid:1", false); err != nil {
		t.Fatalf("setEnabled: %v", err)
	}

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("ok = true for a parked account")
	}
	if !strings.Contains(res.Error, "parked") {
		t.Errorf("error = %q, want it to say the account is parked", res.Error)
	}
	if rt.count() != 0 {
		t.Errorf("made %d requests, want none for a parked account", rt.count())
	}
}

// ---------------------------------------------------------------------------
// caller mistakes and unconfigured clients
// ---------------------------------------------------------------------------

func TestCheckinRejectsAnUnknownActionWithoutTouchingTheUpstream(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"CLAIMED"}`)
	qc := checkinClient(t, oneAccount, rt)

	res, err := qc.Checkin(context.Background(), "uid:1", "weekly")
	if err != nil {
		t.Fatalf("an unknown action is a refusal, not an error: %v", err)
	}
	if res.OK {
		t.Error("ok = true for an action this client does not offer")
	}
	if !strings.Contains(res.Error, "unknown action") {
		t.Errorf("error = %q, want it to name the problem", res.Error)
	}
	if rt.count() != 0 {
		t.Errorf("made %d requests, want none for an action we cannot perform", rt.count())
	}
}

func TestCheckinOnAnUnknownAccountIsAnError(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"CLAIMED"}`)
	qc := checkinClient(t, oneAccount, rt)

	if _, err := runCheckin(t, qc, "uid:nope"); err == nil {
		t.Fatal("an id that is not in the pool is a caller mistake and must be an error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the account is unknown", err)
	}
	if rt.count() != 0 {
		t.Errorf("made %d requests, want none for an account that does not exist", rt.count())
	}
}

func TestCheckinWithoutAccountsIsARefusal(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(claimableStatus, `{"result":"CLAIMED"}`)
	qc := checkinClient(t, `{}`, rt)

	res, err := runCheckin(t, qc, "")
	if err != nil {
		t.Fatalf("an unconfigured client is a refusal, not an error: %v", err)
	}
	if res.OK {
		t.Error("ok = true without a credential")
	}
	if !strings.Contains(res.Error, "not configured") {
		t.Errorf("error = %q, want core.ErrNotConfigured's wording", res.Error)
	}
	if rt.count() != 0 {
		t.Errorf("made %d requests, want none without a credential", rt.count())
	}
}

// ---------------------------------------------------------------------------
// envelope handling
// ---------------------------------------------------------------------------

func TestCheckinUnwrapsTheDataEnvelope(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(
		`{"code":0,"data":{"status":"CLAIMABLE","rewardCredits":5}}`,
		`{"code":0,"data":{"result":"CLAIMED","rewardCredits":5}}`,
	)
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok = false (error %q): the payload lives under data", res.Error)
	}
	if res.Data["claimed_credits"] != float64(5) {
		t.Errorf("data = %+v, want the reward from inside the envelope", res.Data)
	}
}

func TestCheckinPayloadUnwrapsLikeTheVendor(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{"data object", `{"code":0,"data":{"status":"X"}}`, `{"status":"X"}`, true},
		{"bare object", `{"status":"X"}`, `{"status":"X"}`, true},
		{"null data", `{"data":null}`, `{"data":null}`, true},
		{"scalar data", `{"data":"X"}`, `{"data":"X"}`, true},
		{"array", `[1,2]`, "", false},
		{"empty", ``, "", false},
		{"null", `null`, "", false},
		{"garbage", `not json`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := checkinPayload(json.RawMessage(tc.raw))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if string(got) != tc.want {
				t.Errorf("payload = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCheckinStatusDataCarriesOnlyWhatTheVendorSent(t *testing.T) {
	bare := checkinStatusData(checkinStatus{Status: checkinStatusClaimable})
	if len(bare) != 1 || bare["status"] != checkinStatusClaimable {
		t.Errorf("data = %+v, want only the status: a missing field must read as "+
			"\"not reported\", never as a zero the vendor never claimed", bare)
	}

	credits := 12.0
	streak := int64(4)
	full := checkinStatusData(checkinStatus{
		Status:            checkinStatusClaimedToday,
		RewardCredits:     &credits,
		CurrentStreakDays: &streak,
	})
	if full["reward_credits"] != 12.0 || full["current_streak_days"] != int64(4) {
		t.Errorf("data = %+v, want the fields the vendor did send", full)
	}
	if _, ok := full["total_claim_days"]; ok {
		t.Errorf("data = %+v, want no key for a field the vendor left out", full)
	}
}

// ---------------------------------------------------------------------------
// account health
// ---------------------------------------------------------------------------

func TestCheckinDoesNotParkAHealthyAccountOnALocalFailure(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, _ *http.Request, _ string) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:443: connection refused")
	}
	qc := checkinClient(t, oneAccount, rt)

	_, err := runCheckin(t, qc, "uid:1")
	if err == nil {
		t.Fatal("a dead transport must fail the call")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %v, want the transport failure", err)
	}
	if len(qc.pool.usable()) != 1 {
		t.Errorf("the account was parked for our own transport problem: %+v", qc.pool.snapshot())
	}
}

func TestCheckinParksTheAccountWhenTheVendorSaysOutOfCredit(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusTooManyRequests, `{"code":14018,"message":"credits exhausted"}`), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("ok = true on a quota exhaustion")
	}
	if res.Data["error_kind"] != kindQuota.String() {
		t.Errorf("data = %+v, want error_kind=%s", res.Data, kindQuota.String())
	}
	snap := qc.pool.snapshot()
	if len(snap) != 1 || snap[0].State != stateExhausted {
		t.Errorf("pool = %+v, want the account exhausted for a day", snap)
	}
}

// TestCheckinDoesNotParkTheAccountOnA404 is the case this machine actually
// hits: the server has never enabled the credits-growth-card operation, so the
// endpoint answers 404.  That is a client error, not a credential problem, and
// parking the account for it would hide a healthy credential for a minute.
func TestCheckinDoesNotParkTheAccountOnA404(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		return fakeResponse(req, http.StatusNotFound, ""), nil
	}
	qc := checkinClient(t, oneAccount, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Error("ok = true on a 404")
	}
	if res.Data["error_kind"] != kindClient.String() {
		t.Errorf("data = %+v, want error_kind=%s", res.Data, kindClient.String())
	}
	if !strings.Contains(res.Error, "404") {
		t.Errorf("error = %q, want it to name the status", res.Error)
	}
	snap := qc.pool.snapshot()
	if len(snap) != 1 || snap[0].State != stateReady {
		t.Errorf("pool = %+v, want the account still ready: a 404 is the request's fault, "+
			"not the credential's", snap)
	}
}

// ---------------------------------------------------------------------------
// token renewal
// ---------------------------------------------------------------------------

func TestCheckinRenewsTheTokenAndRetriesOnce(t *testing.T) {
	rt := &fakeTransport{}
	seen := 0
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "deviceToken") {
			return fakeResponse(req, http.StatusOK, `{"token":"t2"}`), nil
		}
		if req.Method == http.MethodPost {
			return fakeResponse(req, http.StatusOK, `{"result":"CLAIMED","rewardCredits":7}`), nil
		}
		seen++
		if seen == 1 {
			return fakeResponse(req, http.StatusUnauthorized, `{"message":"token expired"}`), nil
		}
		return fakeResponse(req, http.StatusOK, claimableStatus), nil
	}
	qc := checkinClient(t, `{"accounts":[{"uid":"1","access_token":"t1","refresh_token":"r1"}]}`, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("ok = false (error %q): a stale token must be renewed, not reported", res.Error)
	}
	if rt.count() != 4 {
		t.Fatalf("made %d requests, want status(401), refresh, status, claim", rt.count())
	}
	if got := rt.at(3).header.Get("Authorization"); got != "Bearer t2" {
		t.Errorf("claim Authorization = %q, want the renewed token", got)
	}
	if !qc.pool.ready() {
		t.Error("the renewed account must still be usable")
	}
}

func TestCheckinMarksTheAccountDeadWhenTheRefreshFails(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = func(_ int, req *http.Request, _ string) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "deviceToken") {
			return fakeResponse(req, http.StatusUnauthorized, `{"message":"refresh token rejected"}`), nil
		}
		return fakeResponse(req, http.StatusUnauthorized, `{"message":"token expired"}`), nil
	}
	qc := checkinClient(t, `{"accounts":[{"uid":"1","access_token":"t1","refresh_token":"r1"}]}`, rt)

	res, err := runCheckin(t, qc, "uid:1")
	if err != nil {
		t.Fatalf("a dead credential is a refusal, not an error: %v", err)
	}
	if res.OK {
		t.Error("ok = true when the credential could not be renewed")
	}
	if res.Data["error_kind"] != kindAuth.String() {
		t.Errorf("data = %+v, want error_kind=%s", res.Data, kindAuth.String())
	}
	if !strings.Contains(res.Error, "could not read the daily check-in status") {
		t.Errorf("error = %q, want it to name the step that failed", res.Error)
	}
	snap := qc.pool.snapshot()
	if len(snap) != 1 {
		t.Fatalf("pool = %+v, want one account", snap)
	}
	if snap[0].Enabled || snap[0].State != stateInvalid {
		t.Errorf("account = %+v, want it marked dead: a refresh token the vendor rejects "+
			"cannot be recovered by waiting", snap[0])
	}
	if !strings.Contains(snap[0].Note, "refresh token rejected") {
		t.Errorf("note = %q, want the reason the vendor gave", snap[0].Note)
	}
}

// ---------------------------------------------------------------------------
// what goes on the wire
// ---------------------------------------------------------------------------

func TestCheckinSendsOnlyTheHeadersTheVendorNeeds(t *testing.T) {
	rt := &fakeTransport{}
	rt.handler = checkinScript(`{"status":"CLAIMED_TODAY"}`, "")
	qc := checkinClient(t, oneAccount, rt)

	if _, err := runCheckin(t, qc, "uid:1"); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	req := rt.at(0)
	if req.method != http.MethodGet {
		t.Errorf("method = %q, want GET: the status read must not write", req.method)
	}
	if !strings.HasSuffix(req.url, checkinStatusPath) {
		t.Errorf("url = %q, want it to end in %s", req.url, checkinStatusPath)
	}
	if !strings.HasPrefix(req.url, defaultBaseURL) {
		t.Errorf("url = %q, want it on %s", req.url, defaultBaseURL)
	}
	if got := req.header.Get("User-Agent"); got != checkinUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, checkinUserAgent)
	}
	if got := req.header.Get("Authorization"); got != "Bearer t1" {
		t.Errorf("Authorization = %q, want the account's token", got)
	}
	if got := req.header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
	for k := range req.header {
		if strings.HasPrefix(strings.ToLower(k), "x-qwenwork") {
			t.Errorf("header %s is set, but this module is not that desktop client and "+
				"cannot know its version or build number", k)
		}
	}
}
