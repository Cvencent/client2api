package zcode

// The task board and the scheduler wiring are the two surfaces that turn the
// claim from a button into a chore.  The panel lights its 任务 tab from a type
// assertion, so a missing interface would look like a missing feature rather
// than a broken one -- hence the explicit assertions in tasks.go and the
// end-to-end timetable test below.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// claimEnv is a client that can actually claim: one plan (JWT) account, and a
// captcha_command pointing at a script that prints a VERIFY_PARAM line.  With
// solver false it is the same client minus the solver, which is the state this
// deployment ships in.
func claimEnv(t *testing.T, routes map[string]func(*http.Request) (*http.Response, error), solver bool) (*Client, *fakeTransport, string) {
	t.Helper()
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, routes)
	c := env.client(t, ft)
	if solver {
		// A pinned region keeps regionFor from probing the vendor.
		c.cfg.CaptchaRegion = "cn"
		installSolver(t, c, "token-from-solver")
	}
	return c, ft, addJWTAccount(t, c)
}

// previewRoutes is the happy path: the vendor offers the two fixture plans.
func previewRoutes() map[string]func(*http.Request) (*http.Response, error) {
	return map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"code":0,"msg":"","data":{"plan":{"starts_at":1790692703,"ends_at":1790697600}}}`), nil
		},
	}
}

func TestBatchesDeclareTheCheckinClaim(t *testing.T) {
	// Batches() runs on every scheduler tick and whenever the panel renders the
	// schedule, so it must be pure and must not need an account or a solver.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	b := c.Batches()
	if len(b) != 1 {
		t.Fatalf("Batches() = %+v, want exactly the checkin batch", b)
	}
	if b[0].Name != claimBatchCheckin {
		t.Errorf("batch name = %q, want %q", b[0].Name, claimBatchCheckin)
	}
	if len(b[0].Codes) != 1 || b[0].Codes[0] != claimAction {
		t.Errorf("batch codes = %v, want [%s]", b[0].Codes, claimAction)
	}
	// Two calls must not hand back a shared slice the caller could corrupt.
	again := c.Batches()
	if len(again) != 1 || again[0].Name != b[0].Name {
		t.Errorf("Batches() is not stable across calls: %+v then %+v", b, again)
	}
}

func TestTheCheckinBatchReachesTheSchedulersTimetable(t *testing.T) {
	// The whole point of implementing BatchPlanner: before this, the claim only
	// happened when an operator pressed a button.  The scheduler's batch-name
	// constants are unexported, so reading the literal back would prove nothing;
	// this wires a real registry into a real Runner and inspects the timetable
	// it computes.
	c, ft, id := claimEnv(t, previewRoutes(), true)

	reg := core.NewRegistry()
	reg.Add(c)

	now := time.Date(2026, 3, 4, 1, 0, 0, 0, time.UTC)
	var slept []time.Duration
	r := scheduler.New(scheduler.Deps{
		Registry: reg,
		Logf:     func(string, ...any) {},
		Now:      func() time.Time { return now },
		// runBatch's settle pause always sleeps, so a real one would stall the
		// suite; recording the durations keeps the assertion meaningful.
		Sleep: func(ctx context.Context, d time.Duration) bool {
			slept = append(slept, d)
			return true
		},
	})
	r.Reconfigure(scheduler.DefaultConfig())

	next := r.Status().Next
	if _, ok := next[claimBatchCheckin]; !ok {
		t.Fatalf("the timetable has no checkin slot for this client; Next = %v", next)
	}
	if next[claimBatchCheckin].IsZero() {
		t.Errorf("checkin is scheduled at the zero time: %v", next[claimBatchCheckin])
	}
	// A batch name the reference timetable does not know would be silently
	// unschedulable, so nothing else may appear.
	for _, name := range []string{"travel", "activity", "keepalive", "blackcat", "growth"} {
		if _, ok := next[name]; ok {
			t.Errorf("the timetable gained a %q slot this client never declared", name)
		}
	}

	rep, ok := r.RunBatchNow(context.Background(), "zcode", claimBatchCheckin)
	if !ok {
		t.Fatal("RunBatchNow refused the client or the batch")
	}
	if rep.Ran != 1 {
		t.Fatalf("ran %d tasks, want 1 (report %+v)", rep.Ran, rep)
	}
	if rep.Refused != 0 || rep.Failed != 0 {
		t.Errorf("report has refused=%d failed=%d errors=%v, want none", rep.Refused, rep.Failed, rep.Errors)
	}
	if rep.Accounts != 1 {
		t.Errorf("accounts = %d, want the one plan account %q", rep.Accounts, id)
	}
	// The preview and the claim, and nothing else: the batch must not re-read
	// the board or probe the region.  The two liveness events ride with the
	// preview, so the totals are exact rather than "at least".
	if n := pathCount(ft, planPreviewPath); n != 1 {
		t.Errorf("made %d preview requests, want 1", n)
	}
	if n := pathCount(ft, planClaimPath); n != 1 {
		t.Errorf("made %d claim requests, want 1", n)
	}
	if n := pathCount(ft, eventReportPath); n != len(activationEvents) {
		t.Errorf("made %d liveness reports, want %d", n, len(activationEvents))
	}
	if ft.count() != 4 {
		t.Errorf("made %d HTTP requests, want 4 (2 events + preview + claim)", ft.count())
	}
	if len(slept) == 0 {
		t.Error("the batch never asked for its settle pause")
	}
}

func TestTasksReportsAClaimablePlan(t *testing.T) {
	c, _, _ := claimEnv(t, previewRoutes(), true)

	tasks, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(tasks), tasks)
	}
	got := tasks[0]
	if got.Code != claimAction {
		t.Errorf("code = %q, want %q", got.Code, claimAction)
	}
	if got.Title != "领取活动套餐" || got.Group != "活动套餐" {
		t.Errorf("title/group = %q/%q", got.Title, got.Group)
	}
	if !got.Claimable || !got.Auto || got.Locked || got.Claimed {
		t.Errorf("flags = claimable:%v auto:%v locked:%v claimed:%v, want claimable+auto only",
			got.Claimable, got.Auto, got.Locked, got.Claimed)
	}
	// The row describes the plan the claim would actually take: the priority
	// 110 one, with its token grants summarised.
	if !strings.Contains(got.Desc, "ZCode Trust Build") {
		t.Errorf("desc = %q, want the highest-priority plan", got.Desc)
	}
	if !strings.Contains(got.Desc, "GLM-5.3-Flash 100,000,000 tokens one-time") {
		t.Errorf("desc = %q, want the grant summary", got.Desc)
	}
	if !strings.Contains(got.Note, "共 2 个可领取") {
		t.Errorf("note = %q, want the plan count", got.Note)
	}
	if !strings.Contains(got.Note, "其余：ZCode Start Plan") {
		t.Errorf("note = %q, want the plans this row will not take", got.Note)
	}
	// Credit stays zero: a plan grants tokens, and the board's reward column is
	// denominated in 积分.
	if got.Credit != 0 {
		t.Errorf("credit = %d, want 0 for a token grant", got.Credit)
	}
}

func TestTasksWithoutASolverSaysSoAndCallsNothing(t *testing.T) {
	// With neither a solver nor a browser attached, the board's own 领取 button
	// cannot work: RunTask has no captcha token to hand over.  So the row has to
	// stay locked AND say where the operator should go instead -- the account
	// list, where the click happens in their own browser and the panel can run
	// the vendor's SDK.  It must not spend a request discovering what it
	// already knows.
	c, ft, _ := claimEnv(t, previewRoutes(), false)

	tasks, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("got %d rows, want 1", len(tasks))
	}
	if !tasks[0].Locked || tasks[0].Auto {
		t.Errorf("row = %+v, want locked and not auto", tasks[0])
	}
	// The note must name the browser route, not captcha_command: a headless
	// server cannot open one, and telling the operator to configure a solver
	// would be advice for a different deployment.
	if !strings.Contains(tasks[0].Note, "浏览器") || !strings.Contains(tasks[0].Note, "账号列表") {
		t.Errorf("note = %q, want the browser route named", tasks[0].Note)
	}
	if ft.count() != 0 {
		t.Errorf("made %d HTTP requests, want 0", ft.count())
	}
}

func TestTasksWithoutAPlanAccountSaysSo(t *testing.T) {
	// No accounts at all: the board still has to name the reason, because an
	// empty list would be indistinguishable from a broken module.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	tasks, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Locked {
		t.Fatalf("tasks = %+v, want one locked row", tasks)
	}
	if !strings.Contains(tasks[0].Note, "jwt") {
		t.Errorf("note = %q, want the jwt channel named", tasks[0].Note)
	}
}

func TestTasksForAnAPIKeyAccountSaysSo(t *testing.T) {
	// Plan billing is a JWT-only channel.  An api_key account with no JWT
	// sibling must be told apart from "no account": they need different fixes.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, previewRoutes()))
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{fieldAPIKey: testAPIKey},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	tasks, err := c.Tasks(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Locked {
		t.Fatalf("tasks = %+v, want one locked row", tasks)
	}
	if !strings.Contains(tasks[0].Note, "jwt") {
		t.Errorf("note = %q, want the credential kind named", tasks[0].Note)
	}
}

func TestTasksWithNothingClaimableSaysSo(t *testing.T) {
	c, _, _ := claimEnv(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
	}, true)

	tasks, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 || !tasks[0].Locked {
		t.Fatalf("tasks = %+v, want one locked row", tasks)
	}
	if !strings.Contains(tasks[0].Note, "没有可领取") {
		t.Errorf("note = %q, want the empty board explained", tasks[0].Note)
	}
}

func TestTasksRejectsAnUnknownAccount(t *testing.T) {
	c, _, _ := claimEnv(t, previewRoutes(), true)

	// An empty board for a typo would look like a vendor problem, so this is an
	// error rather than a row.
	if _, err := c.Tasks(context.Background(), "nobody"); err == nil {
		t.Fatal("Tasks accepted an unknown account id")
	} else if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("err = %v, want the id in the message", err)
	}
}

func TestTasksCachesThePreviewBetweenReads(t *testing.T) {
	// The panel re-reads /tasks every 3s while a run is live, and preview is a
	// billing endpoint, so the second read inside the TTL must not go out.
	c, ft, _ := claimEnv(t, previewRoutes(), true)

	for i := 0; i < 3; i++ {
		if _, err := c.Tasks(context.Background(), ""); err != nil {
			t.Fatalf("Tasks #%d: %v", i, err)
		}
	}
	if n := pathCount(ft, planPreviewPath); n != 1 {
		t.Errorf("made %d preview requests for 3 board reads, want 1", n)
	}
}

func TestRunTaskClaimsAndDropsTheBoardCache(t *testing.T) {
	c, ft, id := claimEnv(t, previewRoutes(), true)

	// Prime the cache so the assertion below can tell "dropped" from "never had".
	if _, err := c.Tasks(context.Background(), ""); err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if n := pathCount(ft, planPreviewPath); n != 1 {
		t.Fatalf("priming the board made %d preview requests, want 1", n)
	}

	res, err := c.RunTask(context.Background(), "", claimAction)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want OK", res)
	}
	if res.Code != claimAction {
		t.Errorf("code = %q, want %q", res.Code, claimAction)
	}
	// An empty account id means "whichever you would use"; the result has to
	// name the credential that was actually used.
	if res.AccountID != id {
		t.Errorf("account_id = %q, want the resolved %q", res.AccountID, id)
	}
	if res.Message != "claimed ZCode Trust Build" {
		t.Errorf("message = %q", res.Message)
	}
	// preview + claim, i.e. the cached preview was discarded rather than reused.
	if n := pathCount(ft, planPreviewPath); n != 2 {
		t.Errorf("made %d preview requests, want 2 (primed, then fresh after the claim)", n)
	}
	if n := pathCount(ft, planClaimPath); n != 1 {
		t.Errorf("made %d claim requests, want 1", n)
	}

	// And the claim is visible on the very next board read.
	if _, err := c.Tasks(context.Background(), ""); err != nil {
		t.Fatalf("Tasks after the claim: %v", err)
	}
	if n := pathCount(ft, planPreviewPath); n != 3 {
		t.Errorf("made %d preview requests, want 3: the board cache survived the claim", n)
	}
}

func TestRunTaskRejectsAnUnknownCode(t *testing.T) {
	c, ft, _ := claimEnv(t, previewRoutes(), true)

	if _, err := c.RunTask(context.Background(), "", "nope"); err == nil {
		t.Fatal("RunTask accepted an unknown code")
	}
	if ft.count() != 0 {
		t.Errorf("made %d HTTP requests for an unknown code, want 0", ft.count())
	}
}

func TestRunTaskReportsAVendorRefusalAsAResult(t *testing.T) {
	// A refusal is a fact about the vendor, not a failure of the run: the
	// scheduler counts results with OK false as "refused", which is what keeps
	// an unattended batch from looking like an outage.
	c, _, id := claimEnv(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"code":1004,"msg":"not eligible"}`), nil
		},
	}, true)

	res, err := c.RunTask(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("RunTask turned a vendor refusal into an error: %v", err)
	}
	if res.OK {
		t.Fatalf("result = %+v, want OK false", res)
	}
	// TaskResult.Code is the task code, not the vendor's business code, so the
	// vendor's reason has to survive in the message.
	if res.Code != claimAction {
		t.Errorf("code = %q, want %q", res.Code, claimAction)
	}
	if !strings.Contains(res.Message, "not eligible for the plan") {
		t.Errorf("message = %q, want the vendor's reason", res.Message)
	}
}

func TestRunTaskTreatsAnAlreadyClaimedPlanAsSuccess(t *testing.T) {
	// 1003 is what every sweep after the first sees, and the scheduler counts
	// OK false as "refused".  Reporting the steady state as a refusal would
	// leave the board red for ever and hide the runs that really did fail.
	c, _, id := claimEnv(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
		},
	}, true)

	res, err := c.RunTask(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want OK true: already claimed is not a failure", res)
	}
	if !strings.Contains(res.Message, "already been claimed") {
		t.Errorf("message = %q, want the vendor's reason", res.Message)
	}
}

func TestRunTaskNamesTheReasonAnAccountCannotClaim(t *testing.T) {
	// A scheduled batch runs over every enabled account, including an api_key
	// account with no JWT sibling.  That is a refusal with a reason, not an
	// error, and the reason has to survive into Message because that is the
	// only field the scheduler logs.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, previewRoutes()))
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{fieldAPIKey: testAPIKey},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	res, err := c.RunTask(context.Background(), rec.ID, claimAction)
	if err != nil {
		t.Fatalf("RunTask turned a wrong-credential account into an error: %v", err)
	}
	if res.OK {
		t.Fatalf("result = %+v, want OK false", res)
	}
	if !strings.Contains(res.Message, "jwt") {
		t.Errorf("message = %q, want the credential kind named", res.Message)
	}
}

func TestRunTaskWithoutAPlanAccountIsAnError(t *testing.T) {
	// "Could not be attempted at all" is the one case that is an error, because
	// the scheduler's gate needs to know the difference between a refusal and a
	// module that cannot run.
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if _, err := c.RunTask(context.Background(), "", claimAction); err == nil {
		t.Fatal("RunTask ran with no plan account")
	}
}

func TestRunTaskRejectsAnUnknownAccount(t *testing.T) {
	c, ft, _ := claimEnv(t, previewRoutes(), true)

	if _, err := c.RunTask(context.Background(), "nobody", claimAction); err == nil {
		t.Fatal("RunTask accepted an unknown account id")
	}
	if ft.count() != 0 {
		t.Errorf("made %d HTTP requests, want 0", ft.count())
	}
}
