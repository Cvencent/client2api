package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// checkin_test.go covers the daily-reward surface offline.  Nothing here
// reaches the network: every test drives a fake RoundTripper and a temp data
// directory.

// --- fixtures ---------------------------------------------------------------

// checkinRT answers the billing check-in route and the console conversation
// route, and fails anything else.
func checkinRT(status int, body string) *fakeRT {
	return &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, dailyCheckinPathV2),
			strings.HasSuffix(req.URL.Path, dailyCheckinPath):
			return jsonResponse(status, body), nil
		case strings.HasSuffix(req.URL.Path, dailyActivityPath):
			return jsonResponse(status, body), nil
		default:
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		}
	}}
}

const (
	cnCreds   = "cn.json"
	intlCreds = "intl.json"
)

func cnAccountFiles() map[string]string {
	return map[string]string{
		cnCreds: credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", realmCN,
			"copilot.tencent.com", "uid-cn-0001", time.Now().Add(24*time.Hour).Unix()),
	}
}

func intlAccountFiles() map[string]string {
	return map[string]string{
		intlCreds: credJSON("access-token-abcdefgh", "refresh-token-abcdefgh", realmGlobal,
			"workbuddy.ai", "uid-intl-0001", time.Now().Add(24*time.Hour).Unix()),
	}
}

// --- actions ----------------------------------------------------------------

func TestWorkbuddyCheckinActionsFollowTheConfiguredRealms(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"cn only", cnAccountFiles(), []string{checkinActionCN}},
		{"intl only", intlAccountFiles(), []string{checkinActionIntl}},
		{"no accounts", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := panelClient(t, nil, tc.files)
			got := c.CheckinActions(context.Background())
			if len(got) != len(tc.want) {
				t.Fatalf("got %d actions, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Errorf("action[%d].ID = %q, want %q", i, got[i].ID, id)
				}
				if got[i].Label == "" {
					t.Errorf("action[%d] has no label", i)
				}
				if got[i].Help == "" {
					t.Errorf("action[%d] has no help text", i)
				}
			}
		})
	}
}

func TestWorkbuddyCheckinActionsOfferBothRealms(t *testing.T) {
	files := cnAccountFiles()
	files[intlCreds] = intlAccountFiles()[intlCreds]
	c, _ := panelClient(t, nil, files)
	got := c.CheckinActions(context.Background())
	if len(got) != 2 {
		t.Fatalf("got %d actions, want 2: %+v", len(got), got)
	}
	if got[0].ID != checkinActionCN || got[1].ID != checkinActionIntl {
		t.Fatalf("unexpected action order: %+v", got)
	}
}

func TestWorkbuddyCapabilitiesAdvertiseCheckin(t *testing.T) {
	c, _ := panelClient(t, nil, cnAccountFiles())
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Checkin {
		t.Fatal("caps.Checkin = false, want true for a CN account")
	}
	if !caps.CheckinReady {
		t.Fatal("caps.CheckinReady = false, want true for a CN account")
	}
	if len(caps.Actions) != 1 || caps.Actions[0].ID != checkinActionCN {
		t.Fatalf("caps.Actions = %+v, want the CN action", caps.Actions)
	}
}

func TestWorkbuddyCapabilitiesWithoutAccountsOfferNoCheckin(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	caps := core.CapabilitiesOf(context.Background(), c)
	if caps.CheckinReady {
		t.Fatalf("caps.CheckinReady = true with no accounts: %+v", caps.Actions)
	}
	// The module still HAS check-in: the capability matrix asks the
	// client-level question and must keep answering yes.
	if !caps.Checkin {
		t.Fatal("caps.Checkin = false with no accounts: WorkBuddy implements CheckinProvider")
	}
}

// --- CN check-in ------------------------------------------------------------

func TestWorkbuddyCheckinCN(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":0,"msg":"签到成功","data":{"credits":10}}`)
	c, dir := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error=%q message=%q", res.Error, res.Message)
	}
	if res.Code != 0 {
		t.Errorf("Code = %d, want 0", res.Code)
	}
	if res.Message != "签到成功" {
		t.Errorf("Message = %q", res.Message)
	}
	if res.AccountID != "uid-cn-0001" {
		t.Errorf("AccountID = %q", res.AccountID)
	}
	if res.Action != checkinActionCN {
		t.Errorf("Action = %q", res.Action)
	}
	if res.At == "" {
		t.Error("At is empty")
	}
	if res.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d", res.ElapsedMS)
	}

	// The stamp must be persisted into the credential file, not just held in RAM.
	raw, err := os.ReadFile(filepath.Join(dir, cnCreds))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("credential file is not JSON: %v", err)
	}
	stamp, _ := doc["lastCheckin"].(string)
	if stamp == "" {
		t.Fatalf("lastCheckin missing from the credential file: %s", raw)
	}
	if !strings.HasPrefix(stamp, time.Now().Format("2006-01-02")) {
		t.Errorf("lastCheckin = %q, want today's date prefix", stamp)
	}
	if got, _ := res.Data["last_checkin"].(string); got != stamp {
		t.Errorf("result last_checkin = %q, file has %q", got, stamp)
	}
	if done, _ := res.Data["already_done"].(bool); done {
		t.Error("already_done = true for a fresh check-in")
	}
}

func TestWorkbuddyCheckinCNAlreadyDoneCountsAsSuccess(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":10001,"msg":"今日已签到"}`)
	c, _ := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false for code 10001: error=%q", res.Error)
	}
	if res.Code != 10001 {
		t.Errorf("Code = %d, want 10001", res.Code)
	}
	if done, _ := res.Data["already_done"].(bool); !done {
		t.Error("already_done = false, want true for code 10001")
	}
	if res.Action != checkinActionCN {
		t.Errorf("Action = %q, want the CN default", res.Action)
	}
}

func TestWorkbuddyCheckinCNAlreadyDoneHTTP400CountsAsSuccess(t *testing.T) {
	// The live CN endpoint reports an idempotent repeat with HTTP 400 plus
	// business code 10001; that is still "already checked in", not a transport
	// failure that should land in the task centre as a refusal.
	rt := checkinRT(http.StatusBadRequest, `{"code":10001,"msg":"today already checked in"}`)
	c, _ := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false for HTTP 400 code 10001: error=%q", res.Error)
	}
	if res.Code != 10001 {
		t.Errorf("Code = %d, want 10001", res.Code)
	}
	if done, _ := res.Data["already_done"].(bool); !done {
		t.Error("already_done = false, want true for HTTP 400 code 10001")
	}
}

func TestWorkbuddyCheckinCNRefusalIsAResult(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":40001,"msg":"活动已结束"}`)
	c, _ := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN)
	if err != nil {
		t.Fatalf("a business refusal must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a refusal")
	}
	if res.Code != 40001 {
		t.Errorf("Code = %d, want 40001", res.Code)
	}
	if !strings.Contains(res.Error, "40001") {
		t.Errorf("Error = %q, want it to name the code", res.Error)
	}
	if res.Message != "活动已结束" {
		t.Errorf("Message = %q", res.Message)
	}
}

func TestWorkbuddyCheckinCNRequestShape(t *testing.T) {
	var (
		gotMethod string
		gotURL    string
		gotBody   string
		gotHdr    http.Header
	)
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		gotMethod = req.Method
		gotURL = req.URL.String()
		gotHdr = req.Header.Clone()
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return jsonResponse(http.StatusOK, `{"code":0,"msg":"ok"}`), nil
	}}
	c, _ := panelClient(t, rt, cnAccountFiles())

	if _, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	const wantURL = "https://www.codebuddy.cn/v2/billing/meter/daily-checkin"
	if gotURL != wantURL {
		t.Errorf("url = %q, want %q", gotURL, wantURL)
	}
	if strings.TrimSpace(gotBody) != "{}" {
		t.Errorf("body = %q, want {}", gotBody)
	}
	if got := gotHdr.Get("Authorization"); got != "Bearer access-token-abcdefgh" {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotHdr.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("X-CodeBuddy-Request = %q, want 1", got)
	}
	if got := gotHdr.Get("X-User-Id"); got != "uid-cn-0001" {
		t.Errorf("X-User-Id = %q", got)
	}
	if got := gotHdr.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := gotHdr.Get("User-Agent"); got == "" {
		t.Error("User-Agent is empty")
	}
	// The check-in must not send the CLI identity headers the chat path uses.
	if got := gotHdr.Get("X-Client-Name"); got != "" {
		t.Errorf("X-Client-Name = %q, want it absent on the billing route", got)
	}
}

// --- intl daily activity ----------------------------------------------------

func TestWorkbuddyDailyActivity(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":0,"msg":"","data":{"id":"conv-42"}}`)
	c, dir := panelClient(t, rt, intlAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-intl-0001", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, error=%q", res.Error)
	}
	if res.Action != checkinActionIntl {
		t.Errorf("Action = %q, want the intl default", res.Action)
	}
	if got, _ := res.Data["conversation_id"].(string); got != "conv-42" {
		t.Errorf("conversation_id = %q", got)
	}

	raw, err := os.ReadFile(filepath.Join(dir, intlCreds))
	if err != nil {
		t.Fatalf("read credential file: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("credential file is not JSON: %v", err)
	}
	if stamp, _ := doc["lastDailyChat"].(string); stamp == "" {
		t.Fatalf("lastDailyChat missing from the credential file: %s", raw)
	}
}

func TestWorkbuddyDailyActivityWithoutAConversationIsAResult(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":0,"data":{}}`)
	c, _ := panelClient(t, rt, intlAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-intl-0001", checkinActionIntl)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true without a conversation id")
	}
	if res.Error == "" {
		t.Fatal("Error is empty")
	}
}

func TestWorkbuddyDailyActivityRequestShape(t *testing.T) {
	var (
		gotURL  string
		gotBody map[string]any
		gotHdr  http.Header
	)
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		gotHdr = req.Header.Clone()
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &gotBody)
		return jsonResponse(http.StatusOK, `{"code":0,"data":{"id":"c1"}}`), nil
	}}
	c, _ := panelClient(t, rt, intlAccountFiles())

	if _, err := c.Checkin(context.Background(), "uid-intl-0001", checkinActionIntl); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	const wantURL = "https://www.workbuddy.ai/console/as/conversations/"
	if gotURL != wantURL {
		t.Errorf("url = %q, want %q", gotURL, wantURL)
	}
	if got, _ := gotBody["prompt"].(string); got != dailyActivityPrompt {
		t.Errorf("prompt = %v", gotBody["prompt"])
	}
	if got, _ := gotBody["model"].(string); got != dailyActivityModel {
		t.Errorf("model = %v", gotBody["model"])
	}
	if got, _ := gotBody["conversationOrigin"].(string); got != "workbuddy-app" {
		t.Errorf("conversationOrigin = %v", gotBody["conversationOrigin"])
	}
	if _, ok := gotBody["plugins"].([]any); !ok {
		t.Errorf("plugins = %v, want an array", gotBody["plugins"])
	}
	if got := gotHdr.Get("Authorization"); got != "Bearer access-token-abcdefgh" {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotHdr.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Errorf("Origin = %q", got)
	}
	if got := gotHdr.Get("Referer"); got != "https://www.workbuddy.ai/app" {
		t.Errorf("Referer = %q", got)
	}
	if got := gotHdr.Get("X-User-Id"); got != "uid-intl-0001" {
		t.Errorf("X-User-Id = %q", got)
	}
}

// --- refusals ---------------------------------------------------------------

func TestWorkbuddyCheckinRealmMismatchIsAResult(t *testing.T) {
	tests := []struct {
		name   string
		files  map[string]string
		id     string
		action string
		hint   string
	}{
		{"CN action on an intl account", intlAccountFiles(), "uid-intl-0001", checkinActionCN, checkinActionIntl},
		{"intl action on a CN account", cnAccountFiles(), "uid-cn-0001", checkinActionIntl, checkinActionCN},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
				t.Errorf("a realm mismatch must not reach the network, got %s", req.URL)
				return jsonResponse(http.StatusInternalServerError, `{}`), nil
			}}
			c, _ := panelClient(t, rt, tc.files)
			res, err := c.Checkin(context.Background(), tc.id, tc.action)
			if err != nil {
				t.Fatalf("Checkin: %v", err)
			}
			if res.OK {
				t.Fatal("OK = true for a realm mismatch")
			}
			if !strings.Contains(res.Error, tc.hint) {
				t.Errorf("Error = %q, want it to point at %q", res.Error, tc.hint)
			}
			if len(rt.calls) != 0 {
				t.Errorf("made %d requests, want 0", len(rt.calls))
			}
		})
	}
}

func TestWorkbuddyCheckinUnknownActionIsAResult(t *testing.T) {
	c, _ := panelClient(t, nil, cnAccountFiles())
	res, err := c.Checkin(context.Background(), "uid-cn-0001", "no-such-action")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for an unknown action")
	}
	if !strings.Contains(res.Error, "no-such-action") {
		t.Errorf("Error = %q", res.Error)
	}
}

func TestWorkbuddyCheckinUnknownAccountIsAnError(t *testing.T) {
	c, _ := panelClient(t, nil, cnAccountFiles())
	if _, err := c.Checkin(context.Background(), "nobody", checkinActionCN); err == nil {
		t.Fatal("want a Go error for an unknown account")
	}
}

func TestWorkbuddyCheckinParkedAccountIsAResult(t *testing.T) {
	c, _ := panelClient(t, nil, cnAccountFiles())
	if err := c.SetAccountEnabled(context.Background(), "uid-cn-0001", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	res, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a parked account")
	}
	if !strings.Contains(res.Error, "parked") {
		t.Errorf("Error = %q, want it to say the account is parked", res.Error)
	}
}

func TestWorkbuddyCheckinHTTPFailureIsAResult(t *testing.T) {
	rt := checkinRT(http.StatusUnauthorized, `{"code":401,"msg":"token expired"}`)
	c, _ := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN)
	if err != nil {
		t.Fatalf("an upstream HTTP failure must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for HTTP 401")
	}
	if res.Error == "" {
		t.Fatal("Error is empty")
	}
}

// --- stamps -----------------------------------------------------------------

func TestWorkbuddyCheckinStampSurvivesReload(t *testing.T) {
	rt := checkinRT(http.StatusOK, `{"code":0,"msg":"ok"}`)
	c, dir := panelClient(t, rt, cnAccountFiles())
	if _, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	// A brand-new client over the same directory must see the stamp.
	reloaded, err := New(core.Deps{DataDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rc := reloaded.(*Client)
	accounts := rc.pool.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts after reload", len(accounts))
	}
	if stamp := accounts[0].LastCheckinValue(); stamp == "" {
		t.Fatal("LastCheckin did not survive the reload")
	}
	if !didToday(accounts[0].LastCheckinValue(), time.Now()) {
		t.Errorf("stamp %q is not today", accounts[0].LastCheckinValue())
	}
}

func TestWorkbuddyDidToday(t *testing.T) {
	now := time.Date(2026, 3, 4, 15, 30, 0, 0, time.Local)
	tests := []struct {
		stamp string
		want  bool
	}{
		{"", false},
		{"   ", false},
		{"2026-03-04 09:00:00", true},
		{"2026-03-04T09:00:00Z", true},
		{"2026-03-03 23:59:59", false},
		{"2026-03-05 00:00:00", false},
		{"garbage", false},
	}
	for _, tc := range tests {
		if got := didToday(tc.stamp, now); got != tc.want {
			t.Errorf("didToday(%q) = %v, want %v", tc.stamp, got, tc.want)
		}
	}
}

func TestWorkbuddyParseAuthReadsStamps(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"a","refreshToken":"r","domain":"copilot.tencent.com"},
		"account":{"uid":"u1"},"lastCheckin":"2026-01-02 03:04:05","lastDailyChat":"2026-01-03 04:05:06"}`)
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if a.LastCheckinValue() != "2026-01-02 03:04:05" {
		t.Errorf("LastCheckin = %q", a.LastCheckinValue())
	}
	if a.LastDailyChatValue() != "2026-01-03 04:05:06" {
		t.Errorf("LastDailyChat = %q", a.LastDailyChatValue())
	}
}

func TestWorkbuddySaveAtomicRoundTripsStamps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "wb-round-trip.json")
	a := &Auth{
		AccessToken:   "access-token-abcdefgh",
		RefreshToken:  "refresh-token-abcdefgh",
		UID:           "u-rt",
		Realm:         realmCN,
		Domain:        "copilot.tencent.com",
		FilePath:      p,
		LastCheckin:   "2026-01-02 03:04:05",
		LastDailyChat: "2026-01-03 04:05:06",
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	back, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if back.LastCheckinValue() != "2026-01-02 03:04:05" {
		t.Errorf("LastCheckin round trip = %q", back.LastCheckinValue())
	}
	if back.LastDailyChatValue() != "2026-01-03 04:05:06" {
		t.Errorf("LastDailyChat round trip = %q", back.LastDailyChatValue())
	}
}

func TestWorkbuddyMarkCheckinUsesTheReferenceLayout(t *testing.T) {
	a := &Auth{}
	a.MarkCheckin()
	got := a.LastCheckinValue()
	if len(got) != len("2006-01-02 15:04:05") {
		t.Fatalf("stamp %q does not have the reference layout", got)
	}
	if !strings.HasPrefix(got, time.Now().Format("2006-01-02")) {
		t.Errorf("stamp %q is not today", got)
	}
	a.MarkDailyChat()
	if !didToday(a.LastDailyChatValue(), time.Now()) {
		t.Errorf("daily chat stamp %q is not today", a.LastDailyChatValue())
	}
}

// TestWorkbuddyCheckinNeverLeaksTheToken guards the panel contract: nothing the
// operator sees may contain the access token.
func TestWorkbuddyCheckinNeverLeaksTheToken(t *testing.T) {
	const token = "access-token-abcdefgh"
	rt := checkinRT(http.StatusOK, `{"code":40001,"msg":"rejected"}`)
	c, _ := panelClient(t, rt, cnAccountFiles())

	res, err := c.Checkin(context.Background(), "uid-cn-0001", checkinActionCN)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	blob, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), token) {
		t.Fatalf("the result leaked the access token: %s", blob)
	}
}
