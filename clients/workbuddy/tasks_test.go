package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// tasks_test.go covers the growth-centre task board and its runners offline.
// Nothing here reaches the network: every test drives the shared fakeRT
// transport and a temp data directory, exactly like checkin_test.go.

// --- fixtures ---------------------------------------------------------------

// taskRowJSON renders one board row.  Only the fields the mapping uses are
// emitted, so a test can state exactly what it asserts on.
func taskRowJSON(code, accept string, current, target int64) string {
	return fmt.Sprintf(
		`{"task_code":%q,"title":"%s","description":"%s desc","reward_credit":100,"reward_energy":2,`+
			`"task_type":"daily","tag":"","target":%d,"current":%d,"accept_status":%q}`,
		code, code, code, target, current, accept)
}

// boardJSON wraps rows in the gateway envelope the board endpoint returns.
func boardJSON(rows ...string) string {
	return `{"code":0,"msg":"","data":{"tasks":[` + strings.Join(rows, ",") + `]}}`
}

func okEnvelope() string { return `{"code":0,"msg":"","data":{}}` }

// taskStub answers every route the task surface touches and records what it
// saw, so a test can assert on the events that were actually reported.
type taskStub struct {
	mu sync.Mutex

	// board renders the current board for a channel; reports is the number of
	// activity events received so far, which lets a test move the progress the
	// way the asynchronous scorer does.
	board func(mp bool, reports int) string
	// buddyFirst overrides the /activity/growth/buddy/first response.
	buddyFirst func() (*http.Response, error)
	// claimStatus/claimBody override the reward-claim response; claimBody is used
	// verbatim when claimStatus is zero too.
	claimStatus int
	claimBody   string
	// reportStatus/reportBody override the activity-report response.
	reportStatus int
	reportBody   string
	// other handles anything this stub does not know about.
	other func(req *http.Request) (*http.Response, error)

	lists       int
	mpLists     int
	accepts     int
	mpAccepts   int
	acceptCodes []string
	reports     []map[string]any
	claims      []string
}

func (s *taskStub) handle(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := req.URL.Path
	mp := req.Header.Get(mpPlatformHeader) == mpPlatformValue

	switch {
	case strings.HasSuffix(path, tasksListPath) && req.Method == http.MethodGet:
		s.lists++
		if mp {
			s.mpLists++
		}
		if s.board == nil {
			return jsonResponse(200, boardJSON()), nil
		}
		return jsonResponse(200, s.board(mp, len(s.reports))), nil

	case strings.HasSuffix(path, tasksAcceptPath):
		s.accepts++
		if mp {
			s.mpAccepts++
		}
		var payload struct {
			Codes []string `json:"task_codes"`
		}
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &payload)
		s.acceptCodes = append(s.acceptCodes, payload.Codes...)
		return jsonResponse(200, okEnvelope()), nil

	case strings.HasSuffix(path, reportPath):
		var events []map[string]any
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &events)
		s.reports = append(s.reports, events...)
		if s.reportStatus != 0 {
			return jsonResponse(s.reportStatus, s.reportBody), nil
		}
		return jsonResponse(200, okEnvelope()), nil

	case strings.HasSuffix(path, "/claim"):
		parts := strings.Split(strings.TrimSuffix(path, "/claim"), "/")
		code := parts[len(parts)-1]
		s.claims = append(s.claims, code)
		body := s.claimBody
		if body == "" {
			body = `{"code":0,"msg":"","data":{"already_claimed":false,"credit":100,"energy":2}}`
		}
		if s.claimStatus != 0 {
			return jsonResponse(s.claimStatus, body), nil
		}
		return jsonResponse(200, body), nil

	case strings.HasSuffix(path, "/activity/growth/buddy/first"):
		if s.buddyFirst != nil {
			return s.buddyFirst()
		}
		return jsonResponse(200, okEnvelope()), nil

	case strings.HasSuffix(path, "/activity/growth/buddy/agreement"),
		strings.HasSuffix(path, appearanceSetPth):
		return jsonResponse(200, okEnvelope()), nil
	}

	if s.other != nil {
		return s.other(req)
	}
	return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
}

func (s *taskStub) reportCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reports)
}

func (s *taskStub) listCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists
}

// taskClient builds a Client whose transport is the stub.
func taskClient(t *testing.T, s *taskStub) *Client {
	t.Helper()
	c, _ := panelClient(t, &fakeRT{handler: s.handle}, cnAccountFiles())
	return c
}

// A module built without a logger must still get through a path that logs.
// deps.Log is nil-safe, but the board fallback (tasks.go:348) and the chore
// runners call deps.Logf straight, so New has to normalise the field.
func TestWorkbuddyBoardFallbackSurvivesAMissingLogger(t *testing.T) {
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return `{"code":1,"msg":"miniprogram board down"}`
		}
		return boardJSON(taskRowJSON("chat_5", "accepted", 1, 5))
	}}
	// panelClient (and so taskClient) builds core.Deps{DataDir: dir} with no
	// Logf at all, which is exactly the caller this must survive.
	c := taskClient(t, s)
	if c.deps.Logf == nil {
		t.Fatal("New left deps.Logf nil: the board fallback would panic instead of logging")
	}

	got, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(got) != 1 || got[0].Code != "chat_5" {
		t.Fatalf("got %v, want just chat_5 from the default board", taskCodes(got))
	}
	if s.mpLists == 0 {
		t.Fatal("the miniprogram board was never read, so the fallback never logged")
	}
}

// --- task board -------------------------------------------------------------

func TestWorkbuddyTasksMergesTheTwoBoardsAndDedupes(t *testing.T) {
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return boardJSON(
				taskRowJSON("chat_5", "accepted", 2, 5),
				taskRowJSON("school_season", "accepted", 0, 4),
				taskRowJSON("Sequential_Tasks_3", "accepted", 0, 5),
			)
		}
		return boardJSON(
			taskRowJSON("chat_5", "accepted", 0, 5),
			taskRowJSON("first_buddy", "not_accepted", 0, 1),
		)
	}}
	c := taskClient(t, s)

	got, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	want := []string{"chat_5", "first_buddy", "school_season", "Sequential_Tasks_3"}
	if len(got) != len(want) {
		t.Fatalf("got %d tasks %v, want %d", len(got), taskCodes(got), len(want))
	}
	for i, code := range want {
		if got[i].Code != code {
			t.Fatalf("task[%d] = %q, want %q (codes: %v)", i, got[i].Code, code, taskCodes(got))
		}
	}
	// chat_5 appears on both boards; the web row must win because the
	// miniprogram channel does not own that code.
	if got[0].Current != 0 || got[0].Target != 5 {
		t.Fatalf("chat_5 progress = %d/%d, want 0/5 (the web row must win)", got[0].Current, got[0].Target)
	}
	if !got[2].Auto {
		t.Fatalf("school_season Auto = false, want true (it has a runner)")
	}
	if got[2].Group != "miniprogram" {
		t.Fatalf("school_season Group = %q, want miniprogram", got[2].Group)
	}
	// An ordinary chore keeps the vendor's own grouping rather than being
	// relabelled by channel.
	if got[0].Group != "daily" {
		t.Fatalf("chat_5 Group = %q, want the vendor's own daily", got[0].Group)
	}
	if got[1].Credit != 100 || got[1].Energy != 2 {
		t.Fatalf("first_buddy reward = %d/%d, want 100/2", got[1].Credit, got[1].Energy)
	}
	if got[1].Desc == "" {
		t.Fatalf("first_buddy Desc is empty")
	}
}

func TestWorkbuddyTasksReadsTheProgressObject(t *testing.T) {
	// The vendor sends progress flat, as a bare number, or as an object; an
	// object with anything in it wins over the flat fields, and an empty one
	// leaves them alone.
	s := &taskStub{board: func(bool, int) string {
		return `{"code":0,"msg":"","data":{"tasks":[
			{"task_code":"chat_5","title":"chat","target":0,"current":0,"progress":{"current":3,"target":4}},
			{"task_code":"template_5","title":"tpl","target":5,"current":1,"progress":7},
			{"task_code":"Model_chat_GLM5.2","title":"glm","target":9,"current":9,"accept_status":"accepted","progress":{"current":1,"target":2}},
			{"task_code":"first_buddy","title":"buddy","target":1,"current":0,"accept_status":"accepted","progress":{"current":0,"target":0}}
		]}}`
	}}
	c := taskClient(t, s)

	got, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d tasks, want 4", len(got))
	}
	if got[0].Current != 3 || got[0].Target != 4 {
		t.Fatalf("object progress = %d/%d, want 3/4", got[0].Current, got[0].Target)
	}
	if got[0].Claimable {
		t.Fatalf("chat_5 at 3/4 reported Claimable = true")
	}
	if got[1].Current != 7 || got[1].Target != 5 {
		t.Fatalf("bare-number progress = %d/%d, want 7/5", got[1].Current, got[1].Target)
	}
	if !got[1].Claimable {
		t.Fatalf("template_5 at 7/5 is not Claimable, want claimable " +
			"(rule: !claimed && target > 0 && current >= target)")
	}
	if got[2].Current != 1 || got[2].Target != 2 {
		t.Fatalf("the object must override the flat 9/9: got %d/%d, want 1/2", got[2].Current, got[2].Target)
	}
	if got[3].Current != 0 || got[3].Target != 1 {
		t.Fatalf("an empty progress object must leave the flat fields: got %d/%d, want 0/1",
			got[3].Current, got[3].Target)
	}
}

func TestWorkbuddyTasksMarksUnportedChoresManual(t *testing.T) {
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return boardJSON(taskRowJSON("Sequential_Tasks_2", "accepted", 0, 1))
		}
		return boardJSON(
			taskRowJSON("chat_5", "accepted", 0, 5),
			taskRowJSON("skill_1", "accepted", 0, 1),
			taskRowJSON("Expert_lighthouse", "accepted", 0, 1),
			taskRowJSON("Expert_Philanthropy", "accepted", 0, 1),
			taskRowJSON("some_new_chore", "accepted", 0, 1),
		)
	}}
	c := taskClient(t, s)

	got, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	byCode := map[string]core.TaskInfo{}
	for _, ti := range got {
		byCode[ti.Code] = ti
	}
	if !byCode["chat_5"].Auto {
		t.Fatalf("chat_5 Auto = false, want true")
	}
	// Sequential_Tasks_2 used to be listed here as unported.  It is ported now
	// (runMPExpertUse); the miniprogram half of the fixture still serves it, so
	// it must report Auto = true instead of an explanation.
	if ti := byCode["Sequential_Tasks_2"]; !ti.Auto {
		t.Fatalf("Sequential_Tasks_2 Auto = false, want true (it has a runner now)")
	}
	for _, code := range []string{"skill_1", "Expert_lighthouse", "Expert_Philanthropy", "some_new_chore"} {
		ti, ok := byCode[code]
		if !ok {
			t.Fatalf("%s missing from the board", code)
		}
		if ti.Auto {
			t.Fatalf("%s Auto = true, but no runner is ported", code)
		}
		if strings.TrimSpace(ti.Note) == "" {
			t.Fatalf("%s has Auto=false but no Note explaining why", code)
		}
	}
	if !strings.Contains(byCode["Expert_lighthouse"].Note, "连接器") {
		t.Fatalf("Expert_lighthouse Note = %q, want it to name the connector requirement",
			byCode["Expert_lighthouse"].Note)
	}
}

func TestWorkbuddyTasksWithoutAUsableAccountIsAnEmptyList(t *testing.T) {
	c, _ := panelClient(t, nil, nil)

	got, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks on an empty pool: %v, want nil", err)
	}
	if got == nil {
		t.Fatalf("Tasks returned a nil slice, want an empty one")
	}
	if len(got) != 0 {
		t.Fatalf("Tasks returned %d tasks, want 0", len(got))
	}
}

func TestWorkbuddyTasksUnknownAccountIsAnError(t *testing.T) {
	c := taskClient(t, &taskStub{})

	if _, err := c.Tasks(context.Background(), "no-such-account"); err == nil {
		t.Fatalf("Tasks with an unknown account returned no error")
	}
}

func TestWorkbuddyTasksRefusalIsAnError(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return `{"code":7,"msg":"board offline","data":{}}`
	}}
	c := taskClient(t, s)

	if _, err := c.Tasks(context.Background(), ""); err == nil {
		t.Fatalf("Tasks ignored a business-code refusal")
	}
}

func TestWorkbuddyTaskBoardIsCachedForTheTTL(t *testing.T) {
	s := &taskStub{}
	c := taskClient(t, s)

	for i := 0; i < 3; i++ {
		if _, err := c.Tasks(context.Background(), ""); err != nil {
			t.Fatalf("Tasks #%d: %v", i, err)
		}
	}
	// One default-board read and one miniprogram read, then the cache serves.
	if got := s.listCount(); got != 2 {
		t.Fatalf("board reads = %d, want 2 (one per channel, then cached)", got)
	}
}

// TestMain shrinks this module's wall-clock pacing for the whole test binary.
//
// The chore runners genuinely sleep between upstream calls: mpChatEventGap is
// 45s because the vendor rolls the entire batch back at tighter spacing, and
// that value is real, not decorative. But the tests here are about *which*
// events get reported and *what shape* the calls have, and they were paying
// 45-50s of real sleeping per run to prove it -- which made this package the
// slowest in the tree by 3x (147s against a 44s runner-up) and the dominant
// cost of `go test ./...`.
//
// Shrinking the durations costs no coverage: the same code runs, the same calls
// are counted, the same order is asserted. Only the clock moves.
//
// It is done here rather than per-test on purpose. Roughly thirty tests reach a
// paced path, and annotating each one is easy to get wrong -- a new test would
// silently inherit the 45s wait again. internal/gateway already uses this same
// TestMain approach, for this same reason.
//
// Nothing here weakens the shipped values. They are the defaults* above, and
// TestWorkbuddyTaskAntiAbuseGapIsRespected asserts on those constants directly,
// so tuning the real gap down still fails the suite.
//
// mpChatJitter goes to 0 rather than something small because the runners guard
// it with `if mpChatJitter > 0` before calling rand.Int64N, which panics on 0.
func TestMain(m *testing.M) {
	savedGap, savedJitter := mpChatEventGap, mpChatJitter
	savedReport, savedPoll, savedAction := reportGap, claimPollGap, mpActionGap
	savedAutoPoll := autoPollInterval
	savedSMSInterval := defaultSMSInterval
	savedAgree, savedTab := agreeGateSettle, phoneTabSettle
	savedExpertGap := expertSummonGap
	savedCodeSettle := codeButtonSettle

	mpChatEventGap = time.Millisecond
	mpChatJitter = 0
	reportGap = time.Millisecond
	claimPollGap = time.Millisecond
	mpActionGap = time.Millisecond
	autoPollInterval = time.Millisecond
	defaultSMSInterval = time.Millisecond
	agreeGateSettle = time.Millisecond
	phoneTabSettle = time.Millisecond
	expertSummonGap = time.Millisecond
	codeButtonSettle = time.Millisecond

	code := m.Run()

	// Restore production timing for anything that runs after the suite.
	mpChatEventGap, mpChatJitter = savedGap, savedJitter
	reportGap, claimPollGap, mpActionGap = savedReport, savedPoll, savedAction
	autoPollInterval = savedAutoPoll
	defaultSMSInterval = savedSMSInterval
	agreeGateSettle, phoneTabSettle = savedAgree, savedTab
	expertSummonGap = savedExpertGap
	codeButtonSettle = savedCodeSettle
	os.Exit(code)
}

// TestWorkbuddyTaskAntiAbuseGapIsRespected is a spec guard rather than a
// behavioural test: the whole miniprogram batch is rolled back at tighter
// spacing, so the constant must not be tuned down casually.
//
// It reads the production constants rather than the mutable vars, because
// TestMain has those shrunk for the whole run: asserting on them would only
// prove the test binary is fast, not that production is safe.
func TestWorkbuddyTaskAntiAbuseGapIsRespected(t *testing.T) {
	// These read the production constants, not the vars: TestMain shrinks the
	// vars for the whole run, so asserting on them here would only prove the
	// test binary is fast. What must not move is what production ships.
	if defaultMPChatEventGap < 45*time.Second {
		t.Fatalf("defaultMPChatEventGap = %v, want at least 45s", defaultMPChatEventGap)
	}
	if defaultMPChatJitter <= 0 {
		t.Fatalf("defaultMPChatJitter = %v, want a non-zero jitter", defaultMPChatJitter)
	}
	if defaultReportGap < time.Second {
		t.Fatalf("defaultReportGap = %v, want at least 1s", defaultReportGap)
	}
	// mpActionGap is the pause between accepting a chore and reading it back.
	// The accept call can answer 200 before the chore has registered, so this
	// one is load-bearing: at 0 the re-read races the registration and the batch
	// silently does less work than it claims.
	if defaultMPActionGap < time.Second {
		t.Fatalf("defaultMPActionGap = %v, want at least 1s", defaultMPActionGap)
	}
	if claimPollTries < 2 || defaultClaimPollGap < time.Second {
		t.Fatalf("claim polling = %d tries / %v, too coarse for the asynchronous scorer",
			claimPollTries, defaultClaimPollGap)
	}
	// The login poll cadence is the same kind of contract: it mirrors the vendor
	// panel's own 3s refresh, and dropping it to zero would turn a real wait
	// into a busy loop against the upstream login endpoint.
	if defaultAutoPollInterval < time.Second {
		t.Fatalf("defaultAutoPollInterval = %v, want at least 1s", defaultAutoPollInterval)
	}
	// Same contract for the SMS retry pause: 5s is what the platform needs
	// between checks, and the poll count is the part that carries behaviour.
	if defaultSMSIntervalValue < time.Second {
		t.Fatalf("defaultSMSIntervalValue = %v, want at least 1s", defaultSMSIntervalValue)
	}
	if defaultExpertSummonGap < time.Second {
		t.Fatalf("defaultExpertSummonGap = %v, want at least 1s: the vendor reads a faster chain as automation",
			defaultExpertSummonGap)
	}
	if defaultCodeButtonSettle <= 0 {
		t.Fatalf("defaultCodeButtonSettle = %v, want positive", defaultCodeButtonSettle)
	}
	if defaultAgreeGateSettle <= 0 || defaultPhoneTabSettle <= 0 {
		t.Fatalf("SPA settle pauses = %v / %v, want both positive",
			defaultAgreeGateSettle, defaultPhoneTabSettle)
	}
	// The vars must still start from the production defaults, so a package that
	// forgets TestMain's restore (or another test that forgets to restore after
	// shrinking them) cannot quietly leave the real runners slow -- or fast.
	if mpChatEventGap != defaultMPChatEventGap && mpChatEventGap != time.Millisecond {
		t.Errorf("mpChatEventGap = %v, want either the production default %v or the test value",
			mpChatEventGap, defaultMPChatEventGap)
	}
	// Every code the brief requires must actually have a runner.
	for _, code := range []string{
		"chat_5", "first_buddy", "Model_chat_GLM5.2", "RichMeow_Chat", "Buddy_App",
		"automation_1", "template_5", "playbook_prompt", "create_canvas", "Hp_Appearance",
		"school_season", "Sequential_Tasks_1", "Sequential_Tasks_3", "Sequential_Tasks_6",
	} {
		if _, ok := taskRunnerFor(code); !ok {
			t.Fatalf("no runner for %s", code)
		}
	}
	// And the two the reference deliberately leaves alone must NOT have one.
	for _, code := range []string{"skill_1", "Expert_lighthouse", "Expert_Philanthropy"} {
		if _, ok := taskRunnerFor(code); ok {
			t.Fatalf("%s must not have a runner", code)
		}
	}
}

// --- runners ----------------------------------------------------------------

func TestWorkbuddyRunTaskAlreadyClaimedIsANoOp(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("chat_5", "claimed", 5, 5))
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("an already-claimed chore must be a successful no-op, got %+v", res)
	}
	if !strings.Contains(res.Message, "already claimed") {
		t.Fatalf("message = %q, want it to say already claimed", res.Message)
	}
	if n := s.reportCount(); n != 0 {
		t.Fatalf("reported %d events for an already-claimed chore, want 0", n)
	}
	if len(s.claims) != 0 {
		t.Fatalf("claimed %v for an already-claimed chore", s.claims)
	}
	if res.Credit != 100 || res.Energy != 2 {
		t.Fatalf("reward = %d/%d, want the board's 100/2", res.Credit, res.Energy)
	}
}

func TestWorkbuddyRunTaskChat5ReportsAndClaims(t *testing.T) {
	// The board only credits the chore once two events have arrived, which is
	// what the asynchronous scorer looks like from here.
	s := &taskStub{board: func(_ bool, reports int) string {
		if reports >= 2 {
			return boardJSON(taskRowJSON("chat_5", "accepted", 2, 2))
		}
		return boardJSON(taskRowJSON("chat_5", "accepted", 0, 2))
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask not ok: %+v", res)
	}
	if res.Error != "" {
		t.Fatalf("RunTask error = %q, want empty", res.Error)
	}
	if n := s.reportCount(); n != 2 {
		t.Fatalf("reported %d events, want 2 (target 2 with none credited)", n)
	}
	if len(s.claims) != 1 || s.claims[0] != "chat_5" {
		t.Fatalf("claims = %v, want exactly [chat_5]", s.claims)
	}
	if res.Credit != 100 || res.Energy != 2 {
		t.Fatalf("reward = %d/%d, want 100/2 from the claim", res.Credit, res.Energy)
	}
	if !strings.Contains(res.Message, "claimed 100 credit") {
		t.Fatalf("message = %q, want it to report the claim", res.Message)
	}
	if res.Code != "chat_5" || res.AccountID != "uid-cn-0001" {
		t.Fatalf("result identifies %q/%q", res.Code, res.AccountID)
	}
	if res.At == "" || res.ElapsedMS < 0 {
		t.Fatalf("result is missing At/ElapsedMS: %+v", res)
	}

	// The reported event must be the full CLI activity shape: the vendor answers
	// 200 and silently drops an event without userId.
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := s.reports[0]
	for _, key := range []string{"eventCode", "userId", "conversationId", "requestId", "timestamp",
		"inputLength", "mode", "traceId", "parentConversationId", "agentName", "agentType"} {
		if _, ok := ev[key]; !ok {
			t.Fatalf("event is missing %q: %v", key, ev)
		}
	}
	if ev["eventCode"] != "chat_request_send" {
		t.Fatalf("eventCode = %v, want chat_request_send", ev["eventCode"])
	}
	if ev["userId"] != "uid-cn-0001" {
		t.Fatalf("userId = %v, want the account uid", ev["userId"])
	}
	if ev["agentType"] != "conversation" {
		t.Fatalf("agentType = %v, want the CLI shape", ev["agentType"])
	}
}

func TestWorkbuddyRunTaskClaimsAnAlreadyFinishedChore(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("chat_5", "accepted", 5, 5))
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask not ok: %+v", res)
	}
	if n := s.reportCount(); n != 0 {
		t.Fatalf("reported %d events for an already-finished chore, want 0", n)
	}
	if len(s.claims) != 1 {
		t.Fatalf("claims = %v, want one claim", s.claims)
	}
	if !strings.Contains(res.Message, "already complete") {
		t.Fatalf("message = %q, want it to say the progress was already complete", res.Message)
	}
}

func TestWorkbuddyRunTaskClaimFailureStillReportsTheChore(t *testing.T) {
	s := &taskStub{
		board: func(bool, int) string {
			return boardJSON(taskRowJSON("chat_5", "accepted", 5, 5))
		},
		claimStatus: http.StatusForbidden,
		claimBody:   `{"code":403,"msg":"task not completed"}`,
	}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("a failed claim must not undo the chore, got %+v", res)
	}
	if !strings.Contains(res.Message, "could not be claimed") {
		t.Fatalf("message = %q, want it to explain the failed claim", res.Message)
	}
}

func TestWorkbuddyRunTaskRefusalIsAResultNotAnError(t *testing.T) {
	s := &taskStub{
		board: func(bool, int) string {
			return boardJSON(taskRowJSON("first_buddy", "accepted", 0, 1))
		},
		buddyFirst: func() (*http.Response, error) {
			return jsonResponse(http.StatusBadRequest,
				`{"code":1,"msg":"first_buddy task not completed yet"}`), nil
		},
	}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "first_buddy")
	if err != nil {
		t.Fatalf("a vendor refusal must not be a Go error, got %v", err)
	}
	if res.OK {
		t.Fatalf("a vendor refusal must report OK=false, got %+v", res)
	}
	// Module contract §6: a refusal is `TaskResult{OK:false, Error:"…"}`.
	if res.Error == "" {
		t.Fatalf("a vendor refusal must explain itself in Error, got %+v", res)
	}
	if !strings.Contains(res.Error, "adoption gate") {
		t.Fatalf("Error = %q, want it to name the closed adoption gate", res.Error)
	}
	// Nothing was claimed, so the run must not report a reward.
	if res.Credit != 0 || res.Energy != 0 {
		t.Fatalf("a refused run reported a reward of %d/%d", res.Credit, res.Energy)
	}
	// The prerequisite event was still reported; that is the whole point of the
	// refusal message.
	if n := s.reportCount(); n != 1 {
		t.Fatalf("reported %d prerequisite events, want 1", n)
	}
}

func TestWorkbuddyRunTaskUnknownCodeIsAnError(t *testing.T) {
	c := taskClient(t, &taskStub{})

	if _, err := c.RunTask(context.Background(), "", "definitely_not_a_chore"); err == nil {
		t.Fatalf("RunTask with an unknown code returned no error")
	}
}

func TestWorkbuddyRunTaskWithoutAChoreOnTheBoardIsAResult(t *testing.T) {
	c := taskClient(t, &taskStub{})

	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("a chore missing from the board must be a result, not an error: %v", err)
	}
	if res.OK {
		t.Fatalf("res.OK = true for a chore the account does not have: %+v", res)
	}
	// OK:false always carries its reason in Error (§6).
	if !strings.Contains(res.Error, "no task") {
		t.Fatalf("Error = %q, want it to say the board has no such task", res.Error)
	}
}

func TestWorkbuddyRunTaskWithoutAUsableAccountIsAnError(t *testing.T) {
	c, _ := panelClient(t, nil, nil)

	if _, err := c.RunTask(context.Background(), "", "chat_5"); err == nil {
		t.Fatalf("RunTask with no usable account returned no error")
	}
}

func TestWorkbuddyRunTaskUnknownAccountIsAnError(t *testing.T) {
	c := taskClient(t, &taskStub{})

	if _, err := c.RunTask(context.Background(), "nobody", "chat_5"); err == nil {
		t.Fatalf("RunTask with an unknown account returned no error")
	}
}

func TestWorkbuddyRunTaskReportsAStalledProgress(t *testing.T) {
	// The events land but the scorer never catches up: that is a result with
	// instructions, not a failure.
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("chat_5", "accepted", 0, 1))
	}}
	c := taskClient(t, s)

	start := time.Now()
	res, err := c.RunTask(context.Background(), "", "chat_5")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("res.OK = false when the events were reported: %+v", res)
	}
	if !strings.Contains(res.Message, "0/1") {
		t.Fatalf("message = %q, want the current/target progress", res.Message)
	}
	if !strings.Contains(res.Message, "asynchronously") {
		t.Fatalf("message = %q, want it to explain the scorer lag", res.Message)
	}
	// It must have polled rather than giving up on the first re-read.
	if elapsed := time.Since(start); elapsed < claimPollGap*(claimPollTries-1) {
		t.Fatalf("polled for only %v, want at least %v", elapsed, claimPollGap*(claimPollTries-1))
	}
	if len(s.claims) != 0 {
		t.Fatalf("claimed %v while the progress was still short", s.claims)
	}
}

func TestWorkbuddyRunTaskMiniprogramUsesTheMiniprogramChannel(t *testing.T) {
	// The miniprogram runner waits ~45s before the first event, so this only
	// exercises the paths that do not sleep: a chore the board reports as
	// already claimed, and one it does not carry at all.
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return boardJSON(taskRowJSON("Sequential_Tasks_3", "claimed", 5, 5))
		}
		return boardJSON()
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "Sequential_Tasks_3")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK || !strings.Contains(res.Message, "already claimed") {
		t.Fatalf("miniprogram no-op = %+v", res)
	}
	if n := s.reportCount(); n != 0 {
		t.Fatalf("reported %d miniprogram events, want 0", n)
	}
}

func TestWorkbuddyRunTaskMiniprogramAcceptIsVerified(t *testing.T) {
	// The accept call must go out on the miniprogram channel with the header
	// that makes the vendor find the task at all, and a chore that is already
	// finished must be claimed rather than re-reported.
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return boardJSON(
				taskRowJSON("Sequential_Tasks_3", "accepted", 5, 5),
				taskRowJSON("Sequential_Tasks_6", "claimed", 10, 10),
			)
		}
		return boardJSON()
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "Sequential_Tasks_3")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask not ok: %+v", res)
	}
	if s.accepts != 1 {
		t.Fatalf("accept calls = %d, want 1", s.accepts)
	}
	// For a code in mpTaskCodes the accept must go out on the *miniprogram*
	// channel; on the default channel the vendor cannot find the task at all.
	if s.mpAccepts != 1 {
		t.Fatalf("miniprogram accept calls = %d of %d, want 1 (X-Client-Platform: miniprogram)",
			s.mpAccepts, s.accepts)
	}
	if len(s.acceptCodes) != 1 || s.acceptCodes[0] != "Sequential_Tasks_3" {
		t.Fatalf("accepted codes = %v, want [Sequential_Tasks_3]", s.acceptCodes)
	}
	// A chore whose progress is already met must not re-fire its 45s-spaced
	// events; the accept plus the claim is the whole run.
	if n := s.reportCount(); n != 0 {
		t.Fatalf("reported %d miniprogram events for a finished chore, want 0", n)
	}
	if len(s.claims) != 1 || s.claims[0] != "Sequential_Tasks_3" {
		t.Fatalf("claims = %v", s.claims)
	}
	if s.mpLists == 0 {
		t.Fatalf("the miniprogram board was never read")
	}
}

func TestWorkbuddyRunTaskDesktopSequenceShape(t *testing.T) {
	// RichMeow_Chat has no progress gate: the six-event desktop sequence is the
	// whole chore.  Pin the shape the vendor judges.
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("RichMeow_Chat", "accepted", 0, 1))
	}}
	c := taskClient(t, s)

	res, err := c.RunTask(context.Background(), "", "RichMeow_Chat")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask not ok: %+v", res)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) != 6 {
		t.Fatalf("reported %d events, want the 6-event desktop sequence", len(s.reports))
	}
	wantOrder := []string{
		"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response",
	}
	for i, want := range wantOrder {
		if got := s.reports[i]["eventCode"]; got != want {
			t.Fatalf("event[%d] = %v, want %q", i, got, want)
		}
	}
	fp := s.reports[0]
	for _, key := range []string{"machineId", "sessionId", "ideVersion", "extName", "product", "userId"} {
		if _, ok := fp[key]; !ok {
			t.Fatalf("the desktop fingerprint is missing %q", key)
		}
	}
	if fp["extName"] != "workbuddy-desktop" {
		t.Fatalf("extName = %v, want workbuddy-desktop", fp["extName"])
	}
}

func TestWorkbuddyRunTaskBuddyAppAndAppearanceRoutes(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(
			taskRowJSON("Buddy_App", "accepted", 0, 1),
			taskRowJSON("Hp_Appearance", "accepted", 0, 1),
		)
	}}
	c := taskClient(t, s)

	if res, err := c.RunTask(context.Background(), "", "Buddy_App"); err != nil || !res.OK {
		t.Fatalf("Buddy_App: res=%+v err=%v", res, err)
	}
	s.mu.Lock()
	if len(s.reports) != 5 {
		s.mu.Unlock()
		t.Fatalf("Buddy_App reported %d events, want 5", len(s.reports))
	}
	for _, ev := range s.reports {
		if ev["buddyId"] != buddyAppID || ev["mode"] != "LOCAL" {
			s.mu.Unlock()
			t.Fatalf("buddy app event is missing the binding: %v", ev)
		}
	}
	s.mu.Unlock()

	if res, err := c.RunTask(context.Background(), "", "Hp_Appearance"); err != nil || !res.OK {
		t.Fatalf("Hp_Appearance: res=%+v err=%v", res, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, ev := range s.reports {
		if ev["eventCode"] == "appearance_skin_apply" {
			found = true
			if ev["id"] != appearanceThemeKey {
				t.Fatalf("appearance id = %v, want %q", ev["id"], appearanceThemeKey)
			}
		}
	}
	if !found {
		t.Fatalf("no appearance_skin_apply event was reported")
	}
}

// --- helpers ----------------------------------------------------------------

func taskCodes(infos []core.TaskInfo) []string {
	out := make([]string, 0, len(infos))
	for _, ti := range infos {
		out = append(out, ti.Code)
	}
	return out
}
