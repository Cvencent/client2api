package minimaxcode

// The task board and the scheduler slot are thin adapters over signin.go, so
// these tests are mostly about the *contract*: what the board says when the
// account is parked, when the vendor has no "today", when the reward was
// already collected -- and, above all, that the batch name this module declares
// really lands on the scheduler's timetable.  That last one cannot be checked
// by reading the literal back: the scheduler's own constants are unexported, so
// the test wires a real registry into a real Runner and looks at the timetable
// it computes.

import (
	"context"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// ---------------------------------------------------------------------------
// the batch
// ---------------------------------------------------------------------------

func TestBatchesDeclareOneCheckinChore(t *testing.T) {
	c, _ := newTestClient(t, signinConfig(), (&scriptedSignin{}).handler())

	batches := c.Batches()
	if len(batches) != 1 {
		t.Fatalf("Batches() returned %d batches, want 1: %+v", len(batches), batches)
	}
	b := batches[0]
	if b.Name != "checkin" {
		t.Errorf("batch name = %q, want %q", b.Name, "checkin")
	}
	if len(b.Codes) != 1 || b.Codes[0] != signinActionDaily {
		t.Errorf("batch codes = %v, want [%s]", b.Codes, signinActionDaily)
	}
	// Batches() is called on every scheduler tick and whenever the panel
	// renders the schedule, so it must be pure.  Rebuilding the client is not
	// needed to prove that, but asking twice does prove nothing is memoised
	// into a shared slice the caller could corrupt.
	again := c.Batches()
	if len(again) != 1 || again[0].Name != b.Name {
		t.Errorf("Batches() is not stable across calls: %+v then %+v", b, again)
	}
}

func TestTheCheckinBatchReachesTheSchedulersTimetable(t *testing.T) {
	// The whole point of implementing BatchPlanner: before this, the daily
	// sign-in only happened when an operator pressed a button.
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, ft := newTestClient(t, signinConfig(), s.handler())

	reg := core.NewRegistry()
	reg.Add(c)

	now := time.Date(2026, 3, 4, 1, 0, 0, 0, time.UTC)
	var slept []time.Duration
	r := scheduler.New(scheduler.Deps{
		Registry: reg,
		Logf:     func(string, ...any) {},
		Now:      func() time.Time { return now },
		Sleep: func(ctx context.Context, d time.Duration) bool {
			slept = append(slept, d)
			return true
		},
	})
	r.Reconfigure(scheduler.DefaultConfig())

	next := r.Status().Next
	if _, ok := next["checkin"]; !ok {
		t.Fatalf("the timetable has no checkin slot for this client; Next = %v", next)
	}
	if next["checkin"].IsZero() {
		t.Errorf("checkin is scheduled at the zero time: %v", next["checkin"])
	}
	// A batch name the reference timetable does not know would be silently
	// unschedulable, so the negative case is worth pinning: nothing else this
	// module did not declare may appear.
	for _, name := range []string{"travel", "activity", "keepalive", "blackcat", "growth"} {
		if _, ok := next[name]; ok {
			t.Errorf("the timetable gained a %q slot this client never declared", name)
		}
	}

	rep, ok := r.RunBatchNow(context.Background(), "minimaxcode", "checkin")
	if !ok {
		t.Fatal("RunBatchNow refused the client or the batch")
	}
	if rep.Ran != 1 {
		t.Fatalf("ran %d tasks, want 1 (report %+v)", rep.Ran, rep)
	}
	if rep.Refused != 0 || rep.Failed != 0 {
		t.Errorf("report has refused=%d failed=%d errors=%v, want none", rep.Refused, rep.Failed, rep.Errors)
	}
	if s.statusCalls != 1 || s.claimCalls != 1 {
		t.Errorf("vendor saw status=%d claim=%d calls, want 1 and 1", s.statusCalls, s.claimCalls)
	}
	// The batch runs the code through RunTask, which is the check-in itself.
	if ft.count() != 2 {
		t.Errorf("made %d HTTP requests, want 2", ft.count())
	}
	// The settle pause exists so a read-back cannot double-run; it is the
	// scheduler's business, but the batch must at least have asked for one.
	if len(slept) == 0 {
		t.Error("the batch never asked for its settle pause")
	}
}

// ---------------------------------------------------------------------------
// the board
// ---------------------------------------------------------------------------

// boardTasks is the single-row board, or a fatal if the shape is wrong.
func boardTasks(t *testing.T, c *Client, accountID string) core.TaskInfo {
	t.Helper()
	list, err := c.Tasks(context.Background(), accountID)
	if err != nil {
		t.Fatalf("Tasks(%q): %v", accountID, err)
	}
	if len(list) != 1 {
		t.Fatalf("Tasks(%q) returned %d rows, want 1: %+v", accountID, len(list), list)
	}
	return list[0]
}

func TestTasksReportsATodayThatCanBeClaimed(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	task := boardTasks(t, c, "acct-1")

	if task.Code != signinActionDaily {
		t.Errorf("code = %q, want %q", task.Code, signinActionDaily)
	}
	if task.Title != "每日签到" {
		t.Errorf("title = %q", task.Title)
	}
	if task.Group != "签到" {
		t.Errorf("group = %q, want 签到", task.Group)
	}
	if task.Desc != "七天一轮，今天第 1 天" {
		t.Errorf("desc = %q", task.Desc)
	}
	if !task.Auto {
		t.Error("a claimable day must be Auto: the panel renders the 执行 button from that")
	}
	if !task.Claimable || task.Claimed || task.Locked {
		t.Errorf("flags = claimable:%v claimed:%v locked:%v, want true/false/false",
			task.Claimable, task.Claimed, task.Locked)
	}
	if task.Credit != 800 {
		t.Errorf("credit = %d, want 800", task.Credit)
	}
	if task.Current != 1 || task.Target != 7 {
		t.Errorf("progress = %d/%d, want 1/7", task.Current, task.Target)
	}
	if !strings.Contains(task.Note, "今天可以领取") {
		t.Errorf("note %q does not say the day is claimable", task.Note)
	}
	// bonus_points is already folded into points; the note is the only place
	// the extra is worth showing, and it must not be added to Credit.
	if !strings.Contains(task.Note, "400") {
		t.Errorf("note %q does not mention the 400 bonus", task.Note)
	}
}

func TestTasksMarksAnAlreadyClaimedDay(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimed, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	task := boardTasks(t, c, "acct-1")

	if !task.Claimed {
		t.Error("an already-claimed day must be reported as Claimed")
	}
	if task.Claimable {
		t.Error("an already-claimed day must not also look claimable")
	}
	if !task.Auto {
		t.Error("already claimed is a normal, automatable state")
	}
	if !strings.Contains(task.Note, "已经领过") {
		t.Errorf("note %q does not say the day was already collected", task.Note)
	}
}

func TestTasksLocksADayTheVendorWillNotGive(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayUpcoming, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	task := boardTasks(t, c, "acct-1")

	if task.Auto {
		t.Error("a day the vendor has not opened must not offer an 执行 button")
	}
	if !task.Locked {
		t.Error("a day the vendor has not opened must be Locked, not merely not-Auto")
	}
	if !strings.Contains(task.Note, "未开始") {
		t.Errorf("note %q does not name the vendor's own day state", task.Note)
	}
}

func TestTasksExplainsATimezoneWithNoToday(t *testing.T) {
	// The vendor decides which day is "today" from the timezone_id it is
	// handed, so a wrong zone comes back as a valid board with no current day.
	// Reporting that as an empty board would send the operator hunting for a
	// vendor outage instead of fixing one config field.
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayDisabled, 800, false),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	list, err := c.Tasks(context.Background(), "acct-1")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("got %d rows, want the explanatory row: %+v", len(list), list)
	}
	if !strings.Contains(list[0].Note, "Asia/Shanghai") {
		t.Errorf("note %q does not name the timezone that failed to line up", list[0].Note)
	}
	if list[0].Auto {
		t.Error("there is nothing to run, so the row must not be Auto")
	}
}

func TestTasksReportsAParkedAccountWithoutCallingTheVendor(t *testing.T) {
	cfg := signinConfig()
	cfg["accounts"] = []map[string]any{
		{"id": "acct-1", "label": "Test account", "access_token": testToken, "enabled": false},
	}
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, cfg, s.handler())

	task := boardTasks(t, c, "acct-1")

	if task.Auto {
		t.Error("a parked account must not offer an 执行 button")
	}
	if !strings.Contains(task.Note, "停用") {
		t.Errorf("note %q does not say the account is parked", task.Note)
	}
	if s.statusCalls != 0 {
		t.Errorf("asked the vendor about a parked account %d times, want 0", s.statusCalls)
	}
}

func TestTasksWithNoAccountIsAnEmptyBoardNotAnError(t *testing.T) {
	cfg := signinConfig()
	cfg["accounts"] = []map[string]any{}
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, cfg, s.handler())

	list, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("an unconfigured client must not be an error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("got %d rows, want none: %+v", len(list), list)
	}
	if s.statusCalls != 0 {
		t.Errorf("called the vendor with no account configured %d times", s.statusCalls)
	}
}

func TestTasksRejectsAnUnknownAccount(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	_, err := c.Tasks(context.Background(), "nope")
	if err == nil {
		t.Fatal("Tasks accepted an account id that does not exist")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error %q does not name the account", err)
	}
	if s.statusCalls != 0 {
		t.Errorf("asked the vendor for an unknown account %d times", s.statusCalls)
	}
}

// ---------------------------------------------------------------------------
// running the chore
// ---------------------------------------------------------------------------

func TestRunTaskRunsTheDailySignin(t *testing.T) {
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimable, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res, err := c.RunTask(context.Background(), "acct-1", signinActionDaily)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask did not report success: %+v", res)
	}
	if res.Code != signinActionDaily {
		t.Errorf("code = %q, want %q", res.Code, signinActionDaily)
	}
	if res.AccountID != "acct-1" {
		t.Errorf("account = %q, want acct-1", res.AccountID)
	}
	if res.Credit != 800 {
		t.Errorf("credit = %d, want the 800 that was claimed", res.Credit)
	}
	if s.claimCalls != 1 {
		t.Errorf("posted the claim %d times, want 1", s.claimCalls)
	}
}

func TestRunTaskDoesNotReportCreditForARepeat(t *testing.T) {
	// The board sums Credit.  A repeat must therefore report zero rather than
	// re-announcing a grant that happened on an earlier run.
	s := &scriptedSignin{
		statusBody: signinPanelJSON(t, signinDayClaimed, 800, true),
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res, err := c.RunTask(context.Background(), "acct-1", signinActionDaily)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("an already-claimed day is a success for the scheduler: %+v", res)
	}
	if res.Credit != 0 {
		t.Errorf("credit = %d, want 0 on a repeat", res.Credit)
	}
	if s.claimCalls != 0 {
		t.Errorf("posted the claim %d times on an already-claimed day", s.claimCalls)
	}
}

func TestRunTaskRejectsAnUnknownCode(t *testing.T) {
	c, ft := newTestClient(t, signinConfig(), (&scriptedSignin{}).handler())

	_, err := c.RunTask(context.Background(), "acct-1", "travel")
	if err == nil {
		t.Fatal("RunTask accepted a code this client does not offer")
	}
	if !strings.Contains(err.Error(), "travel") {
		t.Errorf("error %q does not name the offending code", err)
	}
	if ft.count() != 0 {
		t.Errorf("made %d requests for an unknown code, want 0", ft.count())
	}
}

func TestRunTaskReportsAVendorRefusalAsAResult(t *testing.T) {
	// core.TaskProvider: an upstream refusal is a TaskResult with OK false, not
	// an error.  If this ever returned an error the scheduler would count a
	// legitimate "already done" as a transport failure.
	s := &scriptedSignin{
		statusBody: `{"base_resp":{"status_code":1406010011,"status_msg":"invalid timezone_id"}}`,
		claimBody:  claimOKBody,
	}
	c, _ := newTestClient(t, signinConfig(), s.handler())

	res, err := c.RunTask(context.Background(), "acct-1", signinActionDaily)
	if err != nil {
		t.Fatalf("a vendor refusal must not be a Go error: %v", err)
	}
	if res.OK {
		t.Fatalf("reported success for a vendor refusal: %+v", res)
	}
	if res.Error == "" {
		t.Error("the refusal carries no reason")
	}
	if res.Code != signinActionDaily {
		t.Errorf("code = %q, want %q even on a refusal", res.Code, signinActionDaily)
	}
}

func TestTaskCreditOfToleratesEveryNumericShape(t *testing.T) {
	// Checkin always stores points as an int64 today; the other shapes are
	// accepted so a future change to that map cannot silently zero the board.
	cases := []struct {
		name string
		data map[string]any
		want int64
	}{
		{"int64", map[string]any{"points": int64(800)}, 800},
		{"int", map[string]any{"points": 800}, 800},
		{"float64", map[string]any{"points": float64(800)}, 800},
		{"missing", map[string]any{}, 0},
		{"wrong type", map[string]any{"points": "800"}, 0},
	}
	for _, tc := range cases {
		if got := taskCreditOf(tc.data); got != tc.want {
			t.Errorf("%s: taskCreditOf = %d, want %d", tc.name, got, tc.want)
		}
	}
}
