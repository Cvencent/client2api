package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

// panel_tasks_test.go proves the panel-facing task surface offline.  Every case
// drives the shared taskStub (through a thin spy) and the temp data directory,
// so nothing here reaches the network.
//
// The assertions are about the wire, not about "no error": which channel carried
// which code, which chores were skipped as already settled, how many batches went
// out, and what the read-back reports.  Those are exactly the regressions a
// no-error assertion would let through.

// panelAcceptSpy observes the accept and claim requests the module sends, and
// can refuse accepts from the Nth call onwards.  It wraps the shared taskStub
// rather than reimplementing its routes: the stub already answers and counts
// everything, this only adds the per-channel attribution the tests assert on.
type panelAcceptSpy struct {
	mu sync.Mutex

	requests int
	accepts  []panelAcceptCall
	claims   []panelClaimCall

	// failAcceptFrom refuses every accept call from this 1-based index onwards,
	// which is how a partially refused board is built.
	failAcceptFrom int
}

// panelAcceptCall is one accept request: the channel it went out on and the
// codes it carried.
type panelAcceptCall struct {
	mp    bool
	codes []string
}

// panelClaimCall is one claim request: the code and the channel it used.
type panelClaimCall struct {
	code string
	mp   bool
}

func (s *panelAcceptSpy) wrap(next func(*http.Request) (*http.Response, error)) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		s.mu.Lock()
		s.requests++
		s.mu.Unlock()

		switch {
		case strings.HasSuffix(req.URL.Path, tasksAcceptPath):
			raw, _ := io.ReadAll(req.Body)
			// The stub reads the body again; hand back what we took.
			req.Body = io.NopCloser(bytes.NewReader(raw))
			var payload struct {
				Codes []string `json:"task_codes"`
			}
			_ = json.Unmarshal(raw, &payload)
			s.mu.Lock()
			s.accepts = append(s.accepts, panelAcceptCall{
				mp:    req.Header.Get(mpPlatformHeader) == mpPlatformValue,
				codes: payload.Codes,
			})
			refuse := s.failAcceptFrom > 0 && len(s.accepts) >= s.failAcceptFrom
			s.mu.Unlock()
			if refuse {
				return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"upstream refused"}`), nil
			}

		case strings.HasSuffix(req.URL.Path, "/claim"):
			parts := strings.Split(strings.TrimSuffix(req.URL.Path, "/claim"), "/")
			s.mu.Lock()
			s.claims = append(s.claims, panelClaimCall{
				code: parts[len(parts)-1],
				mp:   req.Header.Get(mpPlatformHeader) == mpPlatformValue,
			})
			s.mu.Unlock()
		}
		return next(req)
	}
}

func (s *panelAcceptSpy) acceptCalls() []panelAcceptCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]panelAcceptCall(nil), s.accepts...)
}

func (s *panelAcceptSpy) claimCalls() []panelClaimCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]panelClaimCall(nil), s.claims...)
}

func (s *panelAcceptSpy) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

// panelTaskSpy builds a client whose transport spies on and then serves the stub.
func panelTaskSpy(t *testing.T, s *taskStub) (*Client, *panelAcceptSpy) {
	t.Helper()
	spy := &panelAcceptSpy{}
	c, _ := panelClient(t, &fakeRT{handler: spy.wrap(s.handle)}, cnAccountFiles())
	return c, spy
}

// --- AcceptTasks ------------------------------------------------------------

// TestWorkbuddyPanelAcceptTasksSplitsTheChannels pins the routing: the
// miniprogram chores live behind a different channel and must never be folded
// into the web accept call.
func TestWorkbuddyPanelAcceptTasksSplitsTheChannels(t *testing.T) {
	tests := []struct {
		name  string
		codes []string
		want  []panelAcceptCall
	}{
		{
			"web codes go out on the web channel",
			[]string{"chat_5", "first_buddy"},
			[]panelAcceptCall{{mp: false, codes: []string{"chat_5", "first_buddy"}}},
		},
		{
			"miniprogram codes go out on the miniprogram channel",
			[]string{"Sequential_Tasks_1", "school_season"},
			[]panelAcceptCall{{mp: true, codes: []string{"Sequential_Tasks_1", "school_season"}}},
		},
		{
			"a mixed list is split, not sent as one call",
			[]string{"chat_5", "Sequential_Tasks_1", "first_buddy", "school_season"},
			[]panelAcceptCall{
				{mp: false, codes: []string{"chat_5", "first_buddy"}},
				{mp: true, codes: []string{"Sequential_Tasks_1", "school_season"}},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, spy := panelTaskSpy(t, &taskStub{})

			if err := c.AcceptTasks(context.Background(), "uid-cn-0001", tc.codes); err != nil {
				t.Fatalf("AcceptTasks: %v", err)
			}
			if got := spy.acceptCalls(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("accepts = %+v, want %+v", got, tc.want)
			}
			if got := spy.totalCalls(); got != len(tc.want) {
				t.Fatalf("made %d upstream call(s), want %d", got, len(tc.want))
			}
		})
	}
}

// An empty list is a caller error, and an unknown account must not reach the
// vendor at all.
func TestWorkbuddyPanelAcceptTasksRejectsAnEmptyOrUnknownCaller(t *testing.T) {
	tests := []struct {
		name    string
		account string
		codes   []string
		wantID  string
	}{
		{"an empty code list", "uid-cn-0001", nil, ""},
		{"only blank codes", "uid-cn-0001", []string{"", "  "}, ""},
		{"an unknown account", "nobody", []string{"chat_5"}, "nobody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, spy := panelTaskSpy(t, &taskStub{})

			err := c.AcceptTasks(context.Background(), tc.account, tc.codes)
			if err == nil {
				t.Fatalf("AcceptTasks(%v) returned no error", tc.codes)
			}
			if tc.wantID != "" && !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("error = %q, want it to name the id %q", err, tc.wantID)
			}
			if n := spy.totalCalls(); n != 0 {
				t.Fatalf("made %d upstream call(s), want 0", n)
			}
		})
	}
}

// --- AcceptAllTasks ---------------------------------------------------------

// TestWorkbuddyPanelAcceptAllSkipsSettledChores keeps the reference's skip rule:
// claimed, accepted, completed and locked rows are not offered again.
func TestWorkbuddyPanelAcceptAllSkipsSettledChores(t *testing.T) {
	const lockedRow = `{"task_code":"create_canvas","title":"canvas","reward_credit":1,` +
		`"reward_energy":0,"target":1,"current":0,"accept_status":"not_accepted","locked":true}`
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(
			taskRowJSON("chat_5", "claimed", 5, 5),
			taskRowJSON("first_buddy", "accepted", 0, 1),
			taskRowJSON("Model_chat_GLM5.2", "completed", 9, 9),
			lockedRow,
			taskRowJSON("template_5", "not_accepted", 0, 1),
		)
	}}
	c, spy := panelTaskSpy(t, s)

	got, err := c.AcceptAllTasks(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AcceptAllTasks: %v", err)
	}
	want := []panelAcceptCall{{mp: false, codes: []string{"template_5"}}}
	if calls := spy.acceptCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("accepts = %+v, want only the outstanding chore %+v", calls, want)
	}
	if got.Accepted != 1 || len(got.Failed) != 0 {
		t.Fatalf("result = %+v, want one accepted and nothing failed", got)
	}
	if got.Message == panelAllAccepted {
		t.Fatalf("a board with an outstanding chore reported %q", got.Message)
	}
}

func TestWorkbuddyPanelAcceptAllWithNothingOutstanding(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(
			taskRowJSON("chat_5", "claimed", 5, 5),
			taskRowJSON("first_buddy", "accepted", 0, 1),
		)
	}}
	c, spy := panelTaskSpy(t, s)

	got, err := c.AcceptAllTasks(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AcceptAllTasks: %v", err)
	}
	if n := len(spy.acceptCalls()); n != 0 {
		t.Fatalf("made %d accept call(s) for a fully settled board, want 0", n)
	}
	if got.Accepted != 0 || len(got.Failed) != 0 {
		t.Fatalf("result = %+v, want nothing accepted and nothing failed", got)
	}
	if got.Message != panelAllAccepted {
		t.Fatalf("message = %q, want %q", got.Message, panelAllAccepted)
	}
}

// A board larger than one batch must go out in batches, and a refused batch is
// reported in Failed -- never as an error, so the operator can retry.
func TestWorkbuddyPanelAcceptAllBatchesAndReportsPartialFailure(t *testing.T) {
	var codes []string
	for i := 0; i < acceptAllBatch+1; i++ {
		codes = append(codes, fmt.Sprintf("chore_%02d", i))
	}
	rows := make([]string, 0, len(codes))
	for _, code := range codes {
		rows = append(rows, taskRowJSON(code, "not_accepted", 0, 1))
	}
	s := &taskStub{board: func(bool, int) string { return boardJSON(rows...) }}
	c, spy := panelTaskSpy(t, s)
	spy.mu.Lock()
	spy.failAcceptFrom = 2
	spy.mu.Unlock()

	got, err := c.AcceptAllTasks(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("a partial refusal must not be an error: %v", err)
	}
	calls := spy.acceptCalls()
	if len(calls) != 2 {
		t.Fatalf("made %d accept call(s), want 2 batches (%d then 1)", len(calls), acceptAllBatch)
	}
	if len(calls[0].codes) != acceptAllBatch || len(calls[1].codes) != 1 {
		t.Fatalf("batch sizes = %d/%d, want %d/1",
			len(calls[0].codes), len(calls[1].codes), acceptAllBatch)
	}
	if got.Accepted != acceptAllBatch {
		t.Fatalf("accepted = %d, want the %d codes of the first batch", got.Accepted, acceptAllBatch)
	}
	if want := []string{codes[acceptAllBatch]}; !reflect.DeepEqual(got.Failed, want) {
		t.Fatalf("failed = %v, want %v", got.Failed, want)
	}
	if got.Message != panelAcceptFailed {
		t.Fatalf("message = %q, want %q", got.Message, panelAcceptFailed)
	}
}

// The miniprogram stage is part of accept-all, not only of a single accept.
func TestWorkbuddyPanelAcceptAllRunsTheMiniprogramStage(t *testing.T) {
	s := &taskStub{board: func(mp bool, _ int) string {
		if mp {
			return boardJSON(taskRowJSON("Sequential_Tasks_1", "not_accepted", 0, 1))
		}
		return boardJSON(taskRowJSON("chat_5", "not_accepted", 0, 5))
	}}
	c, spy := panelTaskSpy(t, s)

	got, err := c.AcceptAllTasks(context.Background(), "uid-cn-0001")
	if err != nil {
		t.Fatalf("AcceptAllTasks: %v", err)
	}
	want := []panelAcceptCall{
		{mp: false, codes: []string{"chat_5"}},
		{mp: true, codes: []string{"Sequential_Tasks_1"}},
	}
	if calls := spy.acceptCalls(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("accepts = %+v, want %+v", calls, want)
	}
	if got.Accepted != 2 || len(got.Failed) != 0 {
		t.Fatalf("result = %+v, want both channels counted", got)
	}
}

func TestWorkbuddyPanelAcceptAllUnknownAccountNamesTheID(t *testing.T) {
	c, spy := panelTaskSpy(t, &taskStub{})

	_, err := c.AcceptAllTasks(context.Background(), "nobody")
	if err == nil {
		t.Fatal("AcceptAllTasks on an unknown account returned no error")
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("error = %q, want it to name the id", err)
	}
	if n := spy.totalCalls(); n != 0 {
		t.Fatalf("made %d upstream call(s), want 0", n)
	}
}

// --- ClaimTask --------------------------------------------------------------

// TestWorkbuddyPanelClaimTaskMapsTheVendorsAnswers pins both idempotent-success
// shapes: a zero reward and the vendor's own already-claimed flag are answers,
// not failures.
func TestWorkbuddyPanelClaimTaskMapsTheVendorsAnswers(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		claimBody string
		want      core.TaskClaim
		wantMP    bool
	}{
		{
			"a paid claim reports the reward",
			"chat_5",
			`{"code":0,"msg":"","data":{"already_claimed":false,"credit":100,"energy":2}}`,
			core.TaskClaim{Credit: 100, Energy: 2, Message: "已领取 100 积分 2 能量"},
			false,
		},
		{
			"a zero reward is an already-claimed success, not an error",
			"chat_5",
			`{"code":0,"msg":"","data":{"already_claimed":false,"credit":0,"energy":0}}`,
			core.TaskClaim{AlreadyClaimed: true, Message: panelAlreadyClaimed},
			false,
		},
		{
			"the vendor's own already-claimed flag is a success",
			"chat_5",
			`{"code":0,"msg":"","data":{"already_claimed":true,"credit":0,"energy":0}}`,
			core.TaskClaim{AlreadyClaimed: true, Message: panelAlreadyClaimed},
			false,
		},
		{
			"a miniprogram code is claimed on the miniprogram channel",
			"Sequential_Tasks_1",
			`{"code":0,"msg":"","data":{"credit":5,"energy":1}}`,
			core.TaskClaim{Credit: 5, Energy: 1, Message: "已领取 5 积分 1 能量"},
			true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, spy := panelTaskSpy(t, &taskStub{claimBody: tc.claimBody})

			got, err := c.ClaimTask(context.Background(), "uid-cn-0001", tc.code)
			if err != nil {
				t.Fatalf("ClaimTask: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("claim = %+v, want %+v", got, tc.want)
			}
			claims := spy.claimCalls()
			if len(claims) != 1 {
				t.Fatalf("claims = %+v, want exactly one", claims)
			}
			if claims[0].code != tc.code || claims[0].mp != tc.wantMP {
				t.Fatalf("claim went out as %+v, want code %q on mp=%v",
					claims[0], tc.code, tc.wantMP)
			}
		})
	}
}

func TestWorkbuddyPanelClaimTaskRejectsAnEmptyOrUnknownCaller(t *testing.T) {
	tests := []struct {
		name    string
		account string
		code    string
		wantID  string
	}{
		{"an empty code", "uid-cn-0001", "  ", ""},
		{"an unknown account", "nobody", "chat_5", "nobody"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, spy := panelTaskSpy(t, &taskStub{})

			_, err := c.ClaimTask(context.Background(), tc.account, tc.code)
			if err == nil {
				t.Fatal("ClaimTask returned no error")
			}
			if tc.wantID != "" && !strings.Contains(err.Error(), tc.wantID) {
				t.Fatalf("error = %q, want it to name the id %q", err, tc.wantID)
			}
			if n := spy.totalCalls(); n != 0 {
				t.Fatalf("made %d upstream call(s), want 0", n)
			}
		})
	}
}

// --- RunTaskAuto ------------------------------------------------------------

// panelAutoBoard is the asynchronous scorer seen from here: the chore is only
// credited once the events have arrived.
func panelAutoBoard() func(bool, int) string {
	return func(_ bool, reports int) string {
		if reports > 0 {
			return boardJSON(taskRowJSON("chat_5", "accepted", 1, 1))
		}
		return boardJSON(taskRowJSON("chat_5", "accepted", 0, 1))
	}
}

// TestWorkbuddyPanelRunTaskAutoReportsAClaimFailure is the panel's retry path:
// the chore completed, only the collection failed, so the read-back must show it
// claimable and carry the claim error instead of reporting a failed run.
func TestWorkbuddyPanelRunTaskAutoReportsAClaimFailure(t *testing.T) {
	s := &taskStub{board: panelAutoBoard(), claimStatus: http.StatusInternalServerError}
	c, spy := panelTaskSpy(t, s)

	res, err := c.RunTaskAuto(context.Background(), "uid-cn-0001", "chat_5")
	if err != nil {
		t.Fatalf("a failed claim must be reported in the result, not as an error: %v", err)
	}
	if !res.OK || !res.Attempt {
		t.Fatalf("result = %+v, want an attempted, successful run", res)
	}
	if res.Skipped || res.Claimed {
		t.Fatalf("result = %+v, want neither skipped nor claimed", res)
	}
	if !res.Claimable {
		t.Fatalf("result = %+v, want the read-back to still be claimable", res)
	}
	if res.ClaimError == "" {
		t.Fatal("ClaimError is empty, want the failed collection reported")
	}
	if res.ProgressBefore != "0/1" || res.ProgressAfter != "1/1" {
		t.Fatalf("progress %q -> %q, want 0/1 -> 1/1",
			res.ProgressBefore, res.ProgressAfter)
	}
	if !strings.Contains(res.Message, "达标但领奖失败") || !strings.Contains(res.Message, "「领取」") {
		t.Fatalf("message = %q, want the retry wording", res.Message)
	}
	if res.Credit != 0 || res.Energy != 0 {
		t.Fatalf("reward = %d/%d, want nothing credited on a failed claim", res.Credit, res.Energy)
	}
	if n := s.reportCount(); n != 1 {
		t.Fatalf("reported %d events, want 1", n)
	}
	if claims := spy.claimCalls(); len(claims) != 1 || claims[0].code != "chat_5" {
		t.Fatalf("claims = %+v, want one claim for chat_5", claims)
	}
}

func TestWorkbuddyPanelRunTaskAutoClaimsAndReportsTheReward(t *testing.T) {
	c, spy := panelTaskSpy(t, &taskStub{board: panelAutoBoard()})

	res, err := c.RunTaskAuto(context.Background(), "uid-cn-0001", "chat_5")
	if err != nil {
		t.Fatalf("RunTaskAuto: %v", err)
	}
	if !res.OK || !res.Attempt || !res.Claimed {
		t.Fatalf("result = %+v, want an attempted, claimed run", res)
	}
	if res.Skipped || res.ClaimError != "" {
		t.Fatalf("result = %+v, want neither skipped nor a claim error", res)
	}
	if !res.Claimable {
		t.Fatalf("result = %+v, want the read-back to be claimable", res)
	}
	if res.Credit != 100 || res.Energy != 2 {
		t.Fatalf("reward = %d/%d, want 100/2 from the claim", res.Credit, res.Energy)
	}
	if !strings.Contains(res.Message, "已自动领奖 +100 分 +2 能") {
		t.Fatalf("message = %q, want the reference's claim tail", res.Message)
	}
	if res.ProgressBefore != "0/1" || res.ProgressAfter != "1/1" {
		t.Fatalf("progress %q -> %q, want 0/1 -> 1/1", res.ProgressBefore, res.ProgressAfter)
	}
	if claims := spy.claimCalls(); len(claims) != 1 || claims[0].mp {
		t.Fatalf("claims = %+v, want one claim on the web channel", claims)
	}
}

// An already-rewarded chore is skipped, and nothing is attempted on its behalf.
func TestWorkbuddyPanelRunTaskAutoSkipsAnAlreadyRewardedChore(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("chat_5", "claimed", 5, 5))
	}}
	c, spy := panelTaskSpy(t, s)

	res, err := c.RunTaskAuto(context.Background(), "uid-cn-0001", "chat_5")
	if err != nil {
		t.Fatalf("RunTaskAuto: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("result = %+v, want Skipped = true", res)
	}
	if res.Attempt {
		t.Fatalf("result = %+v, want no attempt for an already-rewarded chore", res)
	}
	if res.Claimed || res.ClaimError != "" {
		t.Fatalf("result = %+v, want no claim at all", res)
	}
	if res.ProgressBefore != "5/5" || res.ProgressAfter != "5/5" {
		t.Fatalf("progress %q -> %q, want 5/5 -> 5/5", res.ProgressBefore, res.ProgressAfter)
	}
	if res.Message != panelClaimedSkipped {
		t.Fatalf("message = %q, want %q", res.Message, panelClaimedSkipped)
	}
	if n := s.reportCount(); n != 0 {
		t.Fatalf("reported %d events for an already-rewarded chore, want 0", n)
	}
	if n := len(spy.claimCalls()); n != 0 {
		t.Fatalf("claimed %d time(s) for an already-rewarded chore, want 0", n)
	}
	if n := spy.totalCalls(); n != 1 {
		t.Fatalf("made %d upstream call(s), want only the one board read", n)
	}
}

// A chore with no automated action is the reference's 501 sentence, verbatim,
// and it must cost zero upstream calls.
func TestWorkbuddyPanelRunTaskAutoWithoutAnAction(t *testing.T) {
	c, spy := panelTaskSpy(t, &taskStub{})

	res, err := c.RunTaskAuto(context.Background(), "uid-cn-0001", "Expert_lighthouse")
	if err == nil {
		t.Fatal("a chore with no runner returned no error")
	}
	if err.Error() != panelNoAutoAction {
		t.Fatalf("error = %q, want the reference sentence %q", err.Error(), panelNoAutoAction)
	}
	if res.Attempt || res.Skipped || res.Claimed || res.ClaimError != "" {
		t.Fatalf("result = %+v, want an untouched result", res)
	}
	if n := spy.totalCalls(); n != 0 {
		t.Fatalf("made %d upstream call(s), want 0", n)
	}
}

func TestWorkbuddyPanelRunTaskAutoUnknownAccountNamesTheID(t *testing.T) {
	c, spy := panelTaskSpy(t, &taskStub{})

	_, err := c.RunTaskAuto(context.Background(), "nobody", "chat_5")
	if err == nil {
		t.Fatal("RunTaskAuto on an unknown account returned no error")
	}
	if strings.Contains(err.Error(), panelNoAutoAction) {
		t.Fatalf("error = %q: an unknown account is not a missing action", err)
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("error = %q, want it to name the id", err)
	}
	if n := spy.totalCalls(); n != 0 {
		t.Fatalf("made %d upstream call(s), want 0", n)
	}
}
