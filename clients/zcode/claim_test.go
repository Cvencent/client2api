package zcode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// The three optional interfaces this file adds must actually be implemented,
// because the panel lights its buttons from a type assertion and a typo here
// would look like a missing feature rather than a broken one.
var (
	_ core.CheckinProvider = (*Client)(nil)
	_ core.BalanceProvider = (*Client)(nil)
	_ core.PackageProvider = (*Client)(nil)
)

// addJWTAccount adds a plan credential through the panel path, which is how an
// operator gets one, and returns its id.
func addJWTAccount(t *testing.T, c *Client) string {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldKind:   kindJWT,
		fieldJWT:    makeJWT(`{"user_id":"u-claim"}`),
		fieldRegion: regionBigmodel,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec.ID
}

// installSolver points captcha_command at a shell script that prints a
// VERIFY_PARAM line, mirroring how a real solver reports one.
func installSolver(t *testing.T, c *Client, param string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "solver.sh")
	body := "#!/bin/sh\necho 'VERIFY_PARAM=" + param + "'\n"
	if runtime.GOOS == "windows" {
		script = filepath.Join(dir, "solver.cmd")
		body = "@echo off\r\necho VERIFY_PARAM=" + param + "\r\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write solver: %v", err)
	}
	if runtime.GOOS == "windows" {
		c.cfg.CaptchaCommand = "cmd.exe"
		c.cfg.CaptchaArgs = []string{"/c", script}
		return
	}
	c.cfg.CaptchaCommand = script
}

// eventReportFixture is the vendor's ack for a liveness event.  Every preview
// is preceded by two of these, so the route is answered by default rather than
// by each test: a test that had to remember it would be testing the harness.
const eventReportFixture = `{"code":0,"msg":""}`

// routeTransport answers by path fragment so a test can describe a whole
// session (preview, claim, balance) without counting requests.
func routeTransport(t *testing.T, routes map[string]func(*http.Request) (*http.Response, error)) *fakeTransport {
	t.Helper()
	if routes == nil {
		routes = map[string]func(*http.Request) (*http.Response, error){}
	}
	if _, ok := routes[eventReportPath]; !ok {
		routes[eventReportPath] = func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, eventReportFixture), nil
		}
	}
	// The release manifest is consulted whenever the vendor answers with an
	// empty plan list (a stale client version looks exactly like "nothing to
	// claim").  Answer with the configured version so that probe is a no-op
	// here and the tests keep asserting the version they configured.
	if _, ok := routes[releaseManifestPath]; !ok {
		routes[releaseManifestPath] = func(*http.Request) (*http.Response, error) {
			return newResponse(http.StatusOK, "text/yaml", "version: "+defaultAppVersion+"\n"), nil
		}
	}
	return &fakeTransport{handler: func(r *http.Request) (*http.Response, error) {
		for fragment, h := range routes {
			if strings.Contains(r.URL.Path, fragment) {
				return h(r)
			}
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		return jsonResponse(http.StatusNotFound, `{"code":404,"msg":"no route"}`), nil
	}}
}

// activationEventsOf returns the event names reported to the vendor, in order.
// It reads the recorded request bodies, so it proves what went on the wire
// rather than that some code path ran.
func activationEventsOf(t *testing.T, ft *fakeTransport) []string {
	t.Helper()
	var events []string
	for i := 0; i < ft.count(); i++ {
		if !strings.Contains(ft.requestAt(i).URL.Path, eventReportPath) {
			continue
		}
		var body struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(ft.bodyAt(i)), &body); err != nil {
			t.Fatalf("event report %d body is not JSON: %v", i, err)
		}
		events = append(events, body.Event)
	}
	return events
}

// pathCount counts the requests that went to one endpoint.  Assertions about a
// cache or a dedup mean "one preview", not "one request": every preview is
// preceded by the two liveness events, so a bare total would silently absorb a
// real regression as long as the events kept being sent.
func pathCount(ft *fakeTransport, fragment string) int {
	n := 0
	for i := 0; i < ft.count(); i++ {
		if strings.Contains(ft.requestAt(i).URL.Path, fragment) {
			n++
		}
	}
	return n
}

// TestEveryPreviewReportsLivenessFirst pins the ORDER, not merely the presence:
// the vendor answers the preview out of the activity signal, so a report that
// arrives after it is the same as no report at all.
func TestEveryPreviewReportsLivenessFirst(t *testing.T) {
	c, ft, id := claimEnv(t, previewRoutes(), true)

	if _, err := c.Checkin(context.Background(), id, claimAction); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	events := activationEventsOf(t, ft)
	if len(events) != len(activationEvents) {
		t.Fatalf("reported %v, want exactly %v", events, activationEvents)
	}
	for i, want := range activationEvents {
		if events[i] != want {
			t.Errorf("event %d = %q, want %q", i, events[i], want)
		}
	}

	firstEvent, firstPreview := -1, -1
	for i := 0; i < ft.count(); i++ {
		p := ft.requestAt(i).URL.Path
		if firstEvent < 0 && strings.Contains(p, eventReportPath) {
			firstEvent = i
		}
		if firstPreview < 0 && strings.Contains(p, planPreviewPath) {
			firstPreview = i
		}
	}
	if firstEvent < 0 || firstPreview < 0 {
		t.Fatalf("first event at %d, first preview at %d: both must have been sent", firstEvent, firstPreview)
	}
	if firstEvent > firstPreview {
		t.Errorf("the liveness report went out after the preview (event %d, preview %d)", firstEvent, firstPreview)
	}
}

// TestTheLivenessReportNamesTheDeviceAndTheEndpoint pins the body the vendor
// validates.  The device id rides in the BODY on this route alone -- everywhere
// else it is the X-Device-Mid header -- and the platform is this endpoint's own
// value rather than the identity the other endpoints are given, so a "tidy-up"
// that made them one constant would break exactly this.
func TestTheLivenessReportNamesTheDeviceAndTheEndpoint(t *testing.T) {
	c, ft, id := claimEnv(t, previewRoutes(), true)

	if _, err := c.Checkin(context.Background(), id, claimAction); err != nil {
		t.Fatalf("Checkin: %v", err)
	}

	seen := 0
	for i := 0; i < ft.count(); i++ {
		req := ft.requestAt(i)
		if !strings.Contains(req.URL.Path, eventReportPath) {
			continue
		}
		seen++
		var body struct {
			Event      string `json:"event"`
			DeviceMid  string `json:"device_mid"`
			Platform   string `json:"platform"`
			AppVersion string `json:"app_version"`
		}
		if err := json.Unmarshal([]byte(ft.bodyAt(i)), &body); err != nil {
			t.Fatalf("event report %d body: %v", i, err)
		}
		if want := c.pool.deviceMid(); body.DeviceMid == "" || body.DeviceMid != want {
			t.Errorf("event %d device_mid = %q, want the pool's %q", i, body.DeviceMid, want)
		}
		if body.Platform != activationPlatform {
			t.Errorf("event %d platform = %q, want %q", i, body.Platform, activationPlatform)
		}
		if want := c.cfg.Identity.AppVersion; body.AppVersion != want {
			t.Errorf("event %d app_version = %q, want %q", i, body.AppVersion, want)
		}
		if req.Method != http.MethodPost {
			t.Errorf("event %d used %s, want POST", i, req.Method)
		}
		// The header is what the vendor checks on every other route, so it
		// must not have been dropped here just because the body also has it.
		if got := req.Header.Get("X-Device-Mid"); got == "" {
			t.Errorf("event %d carried no X-Device-Mid header", i)
		}
		// And the route is unauthenticated: a bearer token here would be a
		// credential sent somewhere it is not needed.
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("event %d sent Authorization %q, but the endpoint takes none", i, got)
		}
	}
	if seen != len(activationEvents) {
		t.Errorf("saw %d liveness reports, want %d", seen, len(activationEvents))
	}
}

// TestAFailedLivenessReportDoesNotStopTheClaim: the report is a hint, and the
// vendor's own client swallows a failure rather than skipping the day.  A
// rejected report must therefore cost the account nothing.
func TestAFailedLivenessReportDoesNotStopTheClaim(t *testing.T) {
	routes := previewRoutes()
	routes[eventReportPath] = func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError, `{"code":500,"msg":"nope"}`), nil
	}
	c, _, id := claimEnv(t, routes, true)

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want the claim to have gone through anyway", res)
	}
}

// TestAPreviewWithNoLivenessReportIsStillRead pins the other half of "not
// fatal": with the endpoint failing at the transport level the preview must
// still be attempted, because an empty answer from the vendor is a fact the
// caller handles and a missing report is not a reason to refuse.
func TestAPreviewWithNoLivenessReportIsStillRead(t *testing.T) {
	c, ft, id := claimEnv(t, previewRoutes(), true)
	ft.handler = func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, eventReportPath) {
			return nil, errors.New("connection reset")
		}
		if strings.Contains(r.URL.Path, planPreviewPath) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		}
		return jsonResponse(http.StatusOK, `{"code":0,"msg":"","data":{"plan":{"starts_at":1,"ends_at":2}}}`), nil
	}

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want the claim to have gone through without the report", res)
	}
	if n := pathCount(ft, planPreviewPath); n != 1 {
		t.Errorf("made %d preview requests, want 1", n)
	}
}

const claimPreviewFixture = `{
  "code": 0,
  "msg": "",
  "data": {
    "server_time": 1790692703,
    "plans": [
      {
        "plan_id": "zcode-v3-start-plan-0817",
        "name": "ZCode Start Plan",
        "description": "daily allowance",
        "priority": 90,
        "status": "active",
        "entitlements": [
          {"show_name": "GLM-5.3", "meter": "model_usage", "unit_type": "token", "grant_units": 3000000, "period": "daily"},
          {"show_name": "GLM-5.3-Flash", "meter": "model_usage", "unit_type": "token", "grant_units": 5000000, "period": "daily"},
          {"show_name": "ignored row", "meter": "storage", "unit_type": "token", "grant_units": 1, "period": "daily"},
          {"show_name": "", "meter": "model_usage", "unit_type": "token", "grant_units": 1, "period": "daily"}
        ]
      },
      {
        "plan_id": "zcode-v3-start-plan-trust-0929",
        "name": "ZCode Trust Build",
        "description": "signup gift",
        "priority": 110,
        "status": "active",
        "entitlements": [
          {"show_name": "GLM-5.3-Flash", "meter": "model_usage", "unit_type": "token", "grant_units": 100000000, "period": "one_time"}
        ]
      }
    ]
  }
}`

const claimBalanceFixture = `{
  "code": 0,
  "msg": "",
  "data": {
    "server_time": 1790692703,
    "plans": [
      {"plan_id": "zcode-v3-start-plan-trust-0929", "name": "ZCode Trust Build", "priority": 110, "status": "active", "ends_at": 1790697600},
      {"plan_id": "zcode-v3-start-plan-0817", "name": "ZCode Start Plan", "priority": 90, "status": "active", "ends_at": 1790956799}
    ],
    "balances": [
      {
        "bucket_id": "b1",
        "plan_id": "zcode-v3-start-plan-trust-0929",
        "entitlement_id": "e1",
        "show_name": "GLM-5.3-Flash",
        "meter": "model_usage",
        "unit_type": "token",
        "total_units": 100000000,
        "used_units": 0,
        "remaining_units": 100000000,
        "available_units": 100000000,
        "period": "one_time",
        "expires_at": 1790697600
      },
      {
        "bucket_id": "b2",
        "plan_id": "zcode-v3-start-plan-0817",
        "entitlement_id": "e2",
        "show_name": "GLM-5.3",
        "meter": "model_usage",
        "unit_type": "token",
        "total_units": 3000000,
        "used_units": 1000000,
        "remaining_units": 2000000,
        "available_units": 2000000,
        "period": "daily",
        "expires_at": 1790956799
      },
      {
        "bucket_id": "b3",
        "plan_id": "zcode-v3-start-plan-0817",
        "entitlement_id": "e3",
        "show_name": "GLM-5.3-Flash",
        "meter": "model_usage",
        "unit_type": "token",
        "total_units": 5000000,
        "used_units": 5000000,
        "remaining_units": 0,
        "available_units": 0,
        "period": "daily",
        "expires_at": 1790956799
      }
    ]
  }
}`

func TestClaimPreviewSortsByPriorityAndKeepsOnlyTokenGrants(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
	}))
	id := addJWTAccount(t, c)

	plans, err := c.claimPreview(context.Background(), c.pool.find(id))
	if err != nil {
		t.Fatalf("claimPreview: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("got %d plans: %+v", len(plans), plans)
	}
	// Highest priority first, regardless of the order the vendor sent.
	if plans[0].id() != "zcode-v3-start-plan-trust-0929" {
		t.Errorf("first plan = %q, want the priority 110 plan", plans[0].id())
	}
	if plans[1].id() != "zcode-v3-start-plan-0817" {
		t.Errorf("second plan = %q", plans[1].id())
	}

	// Only the model-usage token rows with a display name survive.
	grants := plans[1].tokenGrants()
	if len(grants) != 2 {
		t.Fatalf("token grants = %+v, want the two model_usage token rows", grants)
	}
	if got := grantSummary(grants); got != "GLM-5.3 3,000,000 tokens per day, GLM-5.3-Flash 5,000,000 tokens per day" {
		t.Errorf("grantSummary = %q", got)
	}
}

func TestClaimPreviewRefusalCarriesTheVendorCode(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":1001,"msg":"no such plan"}`), nil
		},
	}))
	id := addJWTAccount(t, c)

	_, err := c.claimPreview(context.Background(), c.pool.find(id))
	ce := asClaimError(err)
	if ce == nil {
		t.Fatalf("err = %v, want a claimError", err)
	}
	if ce.code != claimCodeNoPlan {
		t.Errorf("code = %d, want %d", ce.code, claimCodeNoPlan)
	}
	if !strings.Contains(err.Error(), "no such plan") {
		t.Errorf("err = %v, want the vendor's message", err)
	}
}

func TestClaimDailyQuotaRefusalCarriesTheNextWindow(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"code":1005,"msg":"today's claim quota is used up","data":{"plan":{"ends_at":1790697600}}}`), nil
		},
	}))
	id := addJWTAccount(t, c)
	c.cfg.CaptchaRegion = "cn"
	installSolver(t, c, "token-from-solver")

	_, _, err := c.claimPlanOnce(context.Background(), c.pool.find(id), "p1", "token-from-solver", "cn")
	ce := asClaimError(err)
	if ce == nil {
		t.Fatalf("err = %v, want a claimError", err)
	}
	if ce.nextAt.IsZero() {
		t.Fatal("nextAt was not read from data.plan.ends_at")
	}
	if want := time.Unix(1790697600, 0).UTC().Format(time.RFC3339); !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the retry hint %s", err, want)
	}
}

func TestCheckinClaimsTheHighestPriorityPlan(t *testing.T) {
	var claimBody string
	var claimReq *http.Request
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(r *http.Request) (*http.Response, error) {
			claimReq = r
			buf := make([]byte, 256)
			n, _ := r.Body.Read(buf)
			claimBody = string(buf[:n])
			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"","data":{"plan":{"starts_at":1790692703,"ends_at":1790697600}}}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want a successful claim", res)
	}
	if res.Data["plan_id"] != "zcode-v3-start-plan-trust-0929" {
		t.Errorf("claimed %v, want the priority 110 plan", res.Data["plan_id"])
	}
	if got, _ := res.Data["grants"].(string); !strings.Contains(got, "100,000,000 tokens one-time") {
		t.Errorf("grants = %q", got)
	}

	if claimReq == nil {
		t.Fatal("the claim endpoint was never called")
	}
	if got := claimReq.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("Authorization = %q, want a Bearer plan token", got)
	}
	if got := claimReq.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "token-from-solver" {
		t.Errorf("captcha param = %q", got)
	}
	if got := claimReq.Header.Get("X-Aliyun-Captcha-Verify-Region"); got != "cn" {
		t.Errorf("captcha region = %q, want the configured cn", got)
	}
	// The start-plan trace trio belongs to the chat channel only.
	if got := claimReq.Header.Get("x-zcode-session-type"); got != "" {
		t.Errorf("billing must not send the chat trace headers, got x-zcode-session-type=%q", got)
	}
	if !strings.Contains(claimBody, "zcode-v3-start-plan-trust-0929") {
		t.Errorf("claim body = %q", claimBody)
	}
}

func TestCheckinMovesOnWhenAPlanIsAlreadyClaimed(t *testing.T) {
	var claimed []string
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(r *http.Request) (*http.Response, error) {
			var body struct {
				PlanID string `json:"plan_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claimed = append(claimed, body.PlanID)
			if len(claimed) == 1 {
				return jsonResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
			}
			return jsonResponse(http.StatusOK,
				`{"code":0,"data":{"plan":{"starts_at":1,"ends_at":2}}}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want the second plan to be claimed", res)
	}
	if len(claimed) != 2 {
		t.Fatalf("claims = %v, want two attempts", claimed)
	}
	if claimed[1] != "zcode-v3-start-plan-0817" {
		t.Errorf("second attempt = %q", claimed[1])
	}
}

func TestCheckinWithoutASolverReportsNotConfigured(t *testing.T) {
	claimed := false
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			claimed = true
			return jsonResponse(http.StatusOK, `{"code":0}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("a claim without a solver must not report success")
	}
	if !strings.Contains(res.Error, "captcha_command") {
		t.Errorf("res.Error = %q, want it to name captcha_command", res.Error)
	}
	if claimed {
		t.Error("no claim request may be sent without a captcha")
	}
}

func TestCheckinRefusesANonPlanAccount(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	res, err := c.Checkin(context.Background(), rec.ID, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatal("an api-key account has no plan to claim")
	}
	if !strings.Contains(res.Error, "jwt") {
		t.Errorf("res.Error = %q, want it to explain the plan credential requirement", res.Error)
	}
}

func TestCheckinReportsNothingClaimable(t *testing.T) {
	// An empty preview is the steady state, not a refusal.  The plan is handed
	// out once, so every sweep after the first sees this answer; OK false would
	// make the scheduler count it as refused for ever and leave the board red.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want OK true: an empty preview is not a failure", res)
	}
	if res.Code != claimCodeUnavailable {
		t.Errorf("code = %d, want %d", res.Code, claimCodeUnavailable)
	}
	if res.Message == "" {
		t.Error("message is empty; the panel would have nothing to show")
	}
}

func TestCheckinTreatsAnAlreadyClaimedPlanAsSuccess(t *testing.T) {
	// The vendor's 1003 is what a second sweep in the same period gets.  The
	// action did run and the vendor said there is nothing left to do, so it
	// must not be counted as a refusal.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)
	installSolver(t, c, "token-from-solver")

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want OK true: already claimed is the steady state", res)
	}
	if res.Code != claimCodeAlreadyClaimed {
		t.Errorf("code = %d, want %d", res.Code, claimCodeAlreadyClaimed)
	}
	if !strings.Contains(res.Message, "already been claimed") {
		t.Errorf("message = %q, want the vendor's reason", res.Message)
	}
}

func TestCheckinRejectsAnUnknownAction(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	res, err := c.Checkin(context.Background(), "any", "sign-in")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !strings.Contains(res.Error, claimAction) {
		t.Errorf("res.Error = %q, want it to name the only action", res.Error)
	}
}

func TestCheckinAdvertisesExactlyOneAction(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	actions := c.CheckinActions(context.Background())
	if len(actions) != 1 || actions[0].ID != claimAction {
		t.Fatalf("actions = %+v, want only %q", actions, claimAction)
	}
	if actions[0].Label == "" || actions[0].Help == "" {
		t.Errorf("action = %+v, want a label and help text", actions[0])
	}
	// 活动套餐走的是计划账单接口，只有计划凭据（jwt）那一条通道能领。一个账号的
	// 编码计划 API key 是它的兄弟通道，账号行上和 JWT 并列显示，但那一行不该出现
	// 领取按钮。所以这个动作必须用 Channels 把自己限定在 jwt 通道上，面板按行过滤
	// （见 internal/panel/index.html 的 checkinActionsFor）。
	if got := actions[0].Channels; len(got) != 1 || got[0] != kindJWT {
		t.Errorf("action channels = %v, want [%q]: the coding-plan api-key row must not offer the claim", got, kindJWT)
	}
}

func TestAccountBalanceCountsTokensAndTheEarliestLiveTranche(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimBalanceFixture), nil
		},
	}))
	id := addJWTAccount(t, c)

	bal, err := c.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	// 100,000,000 + 2,000,000 + 0 -- an exhausted bucket contributes nothing.
	if bal.Credits != 102_000_000 {
		t.Errorf("credits = %d, want 102,000,000", bal.Credits)
	}
	if bal.Total != 108_000_000 {
		t.Errorf("total = %d, want 108,000,000", bal.Total)
	}
	// The trust grant expires first (1790697600 < 1790956799).
	if bal.EarliestAt.Unix() != 1790697600 {
		t.Errorf("earliest = %v, want the trust grant's expiry", bal.EarliestAt)
	}
	if bal.EarliestRemaining != 100_000_000 {
		t.Errorf("earliest remaining = %d", bal.EarliestRemaining)
	}
}

func TestAccountBalanceTreatsAnExpiringTrancheAsExpiring(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimBalanceFixture), nil
		},
	}))
	id := addJWTAccount(t, c)

	// A window wide enough to cover both tranches.
	// 窗口从 fixture 自己的 server_time 量起，足够盖住两个 tranche。不要用
	// time.Until(固定的到期时间)：那会在墙上时钟越过硬编码时间戳之后变成负数，
	// 测试过期（2026-10-02 就是这么坏的）。
	soon := time.Duration(1790956799-1790692703) * time.Second
	bal, err := c.AccountBalance(context.Background(), id, soon)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Expiring != 102_000_000 {
		t.Errorf("expiring = %d, want both live tranches", bal.Expiring)
	}
}

func TestAccountBalanceRefusalIsAnError(t *testing.T) {
	// The panel's contract: unlike a check-in, a balance it cannot read is an
	// error, because it has nothing sensible to display without a number.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":1002,"msg":"the promotion has ended"}`), nil
		},
	}))
	id := addJWTAccount(t, c)

	if _, err := c.AccountBalance(context.Background(), id, 0); err == nil {
		t.Fatal("a vendor refusal must be an error for the balance route")
	} else if ce := asClaimError(err); ce == nil || ce.code != claimCodeUnavailable {
		t.Errorf("err = %v, want the vendor's code %d", err, claimCodeUnavailable)
	}
}

func TestAccountBalanceUnknownAccountIsAnError(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if _, err := c.AccountBalance(context.Background(), "nobody", 0); err == nil {
		t.Fatal("an unknown account must be an error")
	}
	if _, err := c.AccountPackages(context.Background(), "nobody"); err == nil {
		t.Fatal("an unknown account must be an error")
	}
}

func TestAccountPackagesLabelsThePlanAndMarksCycles(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimBalanceFixture), nil
		},
	}))
	id := addJWTAccount(t, c)

	rep, err := c.AccountPackages(context.Background(), id)
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if rep.Remain != 102_000_000 || rep.Size != 108_000_000 {
		t.Errorf("report totals = %d/%d", rep.Remain, rep.Size)
	}
	if len(rep.Packages) != 3 {
		t.Fatalf("packages = %+v", rep.Packages)
	}
	// Biggest first, so the operator sees the trust grant at the top.
	top := rep.Packages[0]
	if top.Name != "ZCode Trust Build · GLM-5.3-Flash" {
		t.Errorf("top name = %q, want the plan name and the entitlement", top.Name)
	}
	if top.Cycle {
		t.Error("a one_time grant is not a cycle")
	}
	if top.PackageCode != "zcode-v3-start-plan-trust-0929" || top.SubProductCode != "e1" {
		t.Errorf("top codes = %q/%q", top.PackageCode, top.SubProductCode)
	}
	if top.EndTime == "" || top.ExpiresAt != 1790697600 {
		t.Errorf("top expiry = %q/%d", top.EndTime, top.ExpiresAt)
	}

	for _, pkg := range rep.Packages {
		if pkg.SubProductName == "GLM-5.3" && !pkg.Cycle {
			t.Error("a daily allowance is a cycle")
		}
	}
}

func TestPlanBalancePrefersRemainingOverAvailable(t *testing.T) {
	// The vendor sends both today and they agree; the test pins the order so a
	// future disagreement resolves the same way the reference resolves it.
	remaining := int64(7)
	available := int64(9)
	row := planBalance{Remaining: &remaining, Available: &available, TotalUnits: 10, UsedUnits: 3}
	if got := row.remaining(); got != 7 {
		t.Errorf("remaining = %d, want the remaining_units value", got)
	}

	// An absent field must not be read as "nothing left" when the totals say
	// otherwise.
	row = planBalance{TotalUnits: 10, UsedUnits: 4}
	if got := row.remaining(); got != 6 {
		t.Errorf("remaining = %d, want the total-minus-used fallback", got)
	}
}

func TestPlanBalanceNameLookupFallsBackToTheEntitlement(t *testing.T) {
	doc := &planBalances{Plans: []claimPlan{{ID: "p1", Name: "ZCode Start Plan"}}}
	if got := doc.planNameFor("p1"); got != "ZCode Start Plan" {
		t.Errorf("planNameFor = %q", got)
	}
	if got := doc.planNameFor("missing"); got != "" {
		t.Errorf("planNameFor(unknown) = %q, want empty", got)
	}
}

func TestHumanUnitsAndPeriodLabels(t *testing.T) {
	cases := map[float64]string{0: "0", 999: "999", 1000: "1,000", 3000000: "3,000,000", 100000000: "100,000,000"}
	for in, want := range cases {
		if got := humanUnits(in); got != want {
			t.Errorf("humanUnits(%v) = %q, want %q", in, got, want)
		}
	}
	periods := map[string]string{
		"":         "one-time",
		"one_time": "one-time",
		"daily":    "per day",
		"weekly":   "per week",
		"monthly":  "per month",
		"hourly":   "per hourly",
	}
	for in, want := range periods {
		if got := periodLabel(in); got != want {
			t.Errorf("periodLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
