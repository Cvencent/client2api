package workbuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// expertCatalogue renders a market-catalogue answer.
func expertCatalogue(experts ...map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"experts": experts})
	return wbEnvelope(string(raw))
}

// liveExpert is one catalogue entry with a usable market id.
func liveExpert() map[string]any {
	return map[string]any{
		"expert_id":       "ex_7f3a",
		"expert_type":     "agent",
		"display_name_zh": "编程导师",
		"profession_zh":   "帮你写代码",
		"version":         "2.1.0",
	}
}

func TestWorkbuddyExpertChoresAreWiredToTheNewRunners(t *testing.T) {
	cases := []struct {
		code string
		mp   bool
	}{
		{"Sequential_Tasks_2", true},
		{"Sequential_Tasks_4", true},
		{"Sequential_Tasks_5", true},
		{"Sequential_Tasks_7", true},
		{"expert_5", false},
		{"Expert_team_use_3", false},
		{"expert_actual_use", false},
	}
	for _, tc := range cases {
		runner, ok := taskRunnerFor(tc.code)
		if !ok {
			t.Fatalf("%s has no runner", tc.code)
		}
		if runner.run == nil {
			t.Fatalf("%s has a runner with no function", tc.code)
		}
		if runner.mp != tc.mp {
			t.Fatalf("%s channel = mp=%v, want mp=%v", tc.code, runner.mp, tc.mp)
		}
		info := taskInfo(taskRow{task: growthTask{TaskCode: tc.code}, mp: tc.mp})
		if !info.Auto {
			t.Fatalf("%s is still offered as manual: %q", tc.code, info.Note)
		}
	}

	// The chores that genuinely cannot be driven from here stay unavailable and
	// must keep saying why.
	for _, code := range []string{
		"skill_1", "Expert_lighthouse", "Expert_Philanthropy",
	} {
		if _, ok := taskRunnerFor(code); ok {
			t.Fatalf("%s unexpectedly gained a runner", code)
		}
		info := taskInfo(taskRow{task: growthTask{TaskCode: code}})
		if info.Auto {
			t.Fatalf("%s is offered as automatic although it cannot be run", code)
		}
		if strings.TrimSpace(info.Note) == "" {
			t.Fatalf("%s is unavailable without a reason", code)
		}
	}
}

// expertListStub answers the market-catalogue route and nothing else.
func expertListStub(catalogue string) *taskStub {
	return &taskStub{other: func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, expertListPath) {
			return jsonResponse(200, catalogue), nil
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
	}}
}

func TestWorkbuddyPickMarketExpert(t *testing.T) {
	t.Run("prefers a market id over a nicer first entry", func(t *testing.T) {
		stub := expertListStub(expertCatalogue(
			map[string]any{"expert_id": "local-1", "display_name_zh": "自建"},
			liveExpert(),
		))
		c := taskClient(t, stub)

		got, err := c.pickMarketExpert(context.Background(), wbCNAuth(t, c))
		if err != nil {
			t.Fatalf("pickMarketExpert: %v", err)
		}
		if got.ExpertID != "ex_7f3a" {
			t.Fatalf("expert = %q, want the ex_ entry even though it came second", got.ExpertID)
		}
	})

	t.Run("falls back to any non-empty id", func(t *testing.T) {
		stub := expertListStub(expertCatalogue(map[string]any{"expert_id": "local-1"}))
		c := taskClient(t, stub)

		got, err := c.pickMarketExpert(context.Background(), wbCNAuth(t, c))
		if err != nil {
			t.Fatalf("pickMarketExpert: %v", err)
		}
		if got.ExpertID != "local-1" {
			t.Fatalf("expert = %q", got.ExpertID)
		}
	})

	t.Run("an empty catalogue is an error", func(t *testing.T) {
		stub := expertListStub(expertCatalogue())
		c := taskClient(t, stub)

		_, err := c.pickMarketExpert(context.Background(), wbCNAuth(t, c))
		if err == nil || !strings.Contains(err.Error(), "none with an id") {
			t.Fatalf("error = %v, want it to explain the empty catalogue", err)
		}
	})

	t.Run("a catalogue failure surfaces", func(t *testing.T) {
		stub := &taskStub{other: func(req *http.Request) (*http.Response, error) {
			return jsonResponse(200, wbRefusal(11020, "the market is closed")), nil
		}}
		c := taskClient(t, stub)

		_, err := c.pickMarketExpert(context.Background(), wbCNAuth(t, c))
		if err == nil || !strings.Contains(err.Error(), "11020") {
			t.Fatalf("error = %v, want the vendor refusal", err)
		}
	})
}

func TestWorkbuddySequentialTasks2RefusesWithoutALiveExpertID(t *testing.T) {
	stub := expertListStub(expertCatalogue())
	c := taskClient(t, stub)

	msg, err := runMPExpertUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "Sequential_Tasks_2"})
	if err == nil {
		t.Fatalf("returned %q and no error although no expert id exists", msg)
	}
	if !strings.Contains(err.Error(), "no live expert id could be read from the market catalogue") {
		t.Fatalf("error = %q, want the refusal to be explicit", err)
	}
	if msg != "" {
		t.Fatalf("message = %q, want it empty on refusal", msg)
	}
	if stub.reportCount() != 0 {
		t.Fatalf("reported %d events without a live expert", stub.reportCount())
	}
	if stub.mpAccepts != 0 {
		t.Fatalf("accepted the chore %d times before it could run", stub.mpAccepts)
	}
}

// Sequential_Tasks_2 must report the live catalogue id, not an invented one.
// This costs one anti-abuse interval (the report cadence is part of the
// contract), which is why it is the only paced case here.
func TestWorkbuddySequentialTasks2ReportsTheLiveExpertID(t *testing.T) {
	stub := expertListStub(expertCatalogue(liveExpert()))
	stub.board = func(mp bool, reports int) string {
		if !mp {
			return boardJSON()
		}
		return boardJSON(taskRowJSON("Sequential_Tasks_2", "", 0, 1))
	}
	c := taskClient(t, stub)

	msg, err := runMPExpertUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "Sequential_Tasks_2"})
	if err != nil {
		t.Fatalf("runMPExpertUse: %v", err)
	}
	if msg != "miniprogram events reported" {
		t.Fatalf("message = %q", msg)
	}
	if stub.reportCount() != 1 {
		t.Fatalf("reported %d events, want exactly one expert-use event", stub.reportCount())
	}
	ev := stub.reports[0]
	if ev["eventCode"] != "expert_actual_use" {
		t.Fatalf("eventCode = %v", ev["eventCode"])
	}
	if ev["id"] != "ex_7f3a" || ev["name"] != "ex_7f3a" {
		t.Fatalf("event id/name = %v/%v, want the live market id", ev["id"], ev["name"])
	}
	if ev["expertTitle"] != "编程导师" || ev["expertType"] != "agent" {
		t.Fatalf("event identity = %v/%v", ev["expertTitle"], ev["expertType"])
	}
	if _, ok := ev["conversationId"]; ok {
		t.Fatal("the miniprogram expert event must not claim a conversation")
	}
	if _, ok := ev["activityId"]; ok {
		t.Fatal("the miniprogram expert event must not claim an activity")
	}
	if stub.mpAccepts == 0 {
		t.Fatal("the chore was never accepted")
	}
	if !containsString(stub.acceptCodes, "Sequential_Tasks_2") {
		t.Fatalf("accepted codes = %v", stub.acceptCodes)
	}
}

func TestWorkbuddySequentialTasks5ShortCircuitsAClaimedRow(t *testing.T) {
	stub := expertListStub(expertCatalogue())
	stub.board = func(mp bool, reports int) string {
		if !mp {
			return boardJSON()
		}
		return boardJSON(taskRowJSON("Sequential_Tasks_5", "claimed", 1, 1))
	}
	c := taskClient(t, stub)

	msg, err := runMPChatModel(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "Sequential_Tasks_5"})
	if err != nil {
		t.Fatalf("runMPChatModel: %v", err)
	}
	if msg != "already claimed" {
		t.Fatalf("message = %q, want the idempotent short-circuit", msg)
	}
	if stub.reportCount() != 0 {
		t.Fatalf("reported %d events for an already claimed chore", stub.reportCount())
	}
	if stub.mpAccepts == 0 {
		t.Fatal("the chore was never accepted")
	}
}

func TestWorkbuddyExpertActualUseRunsTheDesktopSequence(t *testing.T) {
	stub := &taskStub{
		board: func(mp bool, reports int) string {
			if mp {
				return boardJSON()
			}
			return boardJSON(taskRowJSON("expert_actual_use", "", 0, 1))
		},
		other: func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, expertListPath):
				return jsonResponse(200, expertCatalogue(liveExpert())), nil
			case strings.HasSuffix(req.URL.Path, chatCompletionsPath):
				return sseResponse(200, streamWithID(serverID), nil), nil
			}
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		},
	}
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertActualUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "expert_actual_use"})
	if err != nil {
		t.Fatalf("runExpertActualUse: %v", err)
	}
	if !strings.Contains(msg, "ex_7f3a") || !strings.Contains(msg, serverID) {
		t.Fatalf("message = %q, want the expert and the server requestId", msg)
	}

	// The chat really went out, addressed to the chosen expert.
	var chat *http.Request
	for _, req := range rt.calls {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			chat = req
		}
	}
	if chat == nil {
		t.Fatal("no expert chat was sent, so the reported requestId cannot be server-issued")
	}
	if got := chat.Header.Get("X-Expert-Id"); got != "ex_7f3a" {
		t.Fatalf("X-Expert-Id = %q, want the live expert", got)
	}
	conv := chat.Header.Get("X-Conversation-ID")
	if conv == "" {
		t.Fatal("the expert chat carried no conversation id")
	}

	if stub.reportCount() != 4 {
		t.Fatalf("reported %d events, want the 3 summon steps plus the use event: %v",
			stub.reportCount(), stub.reports)
	}
	wantCodes := []string{"web_element_click", "expert_summon_click", "expert_summoned", "expert_actual_use"}
	for i, code := range wantCodes {
		if stub.reports[i]["eventCode"] != code {
			t.Fatalf("report %d = %v, want %s", i, stub.reports[i]["eventCode"], code)
		}
	}
	use := stub.reports[3]
	if use["requestId"] != serverID {
		t.Fatalf("requestId = %v, want the SERVER id %q", use["requestId"], serverID)
	}
	if use["conversationId"] != conv {
		t.Fatalf("conversationId = %v, want the chat conversation %q", use["conversationId"], conv)
	}
	if use["messageId"] != "msg-"+serverID[len(serverID)-8:] {
		t.Fatalf("messageId = %v", use["messageId"])
	}
	if use["cost"] != float64(9000) || use["characterCount"] != float64(14) {
		t.Fatalf("cost/characterCount = %v/%v", use["cost"], use["characterCount"])
	}
	if use["id"] != "ex_7f3a" || use["mode"] != "craft" {
		t.Fatalf("use event = %v", use)
	}
}

func TestWorkbuddyExpertActualUseRefusesWithoutALiveExpertID(t *testing.T) {
	stub := expertListStub(expertCatalogue())
	stub.board = func(mp bool, reports int) string {
		if mp {
			return boardJSON()
		}
		return boardJSON(taskRowJSON("expert_actual_use", "", 0, 1))
	}
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertActualUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "expert_actual_use"})
	if err == nil {
		t.Fatalf("returned %q and no error although no expert id exists", msg)
	}
	if !strings.Contains(err.Error(), "no live expert id could be read from the market catalogue") {
		t.Fatalf("error = %q", err)
	}
	if stub.reportCount() != 0 {
		t.Fatal("events were reported without a live expert")
	}
	if wbCalls(rt, chatCompletionsPath) != 0 {
		t.Fatal("an expert chat was spent although no expert id was available")
	}
}

// expertFixture renders a catalogue entry with a chosen id and type.
func expertFixture(id, expertType string) map[string]any {
	return map[string]any{
		"expert_id":       id,
		"expert_type":     expertType,
		"display_name_zh": "编程导师",
		"profession_zh":   "帮你写代码",
		"version":         "2.1.0",
	}
}

// secondServerID is a second well-formed server request id, so a test can tell
// one expert's chat apart from another's.
const secondServerID = "fedcba9876543210fedcba9876543210"

// expertChainEvents is how many desktop events one completed expert chain
// reports: the three summon steps, the six a desktop chat reports, and the
// actual-use event that closes the chain.  It is the stride between one
// expert's first event and the next expert's first event in taskStub.reports.
const expertChainEvents = 3 + 6 + 1

// The five-expert chore walks the live market and completes one chain per
// expert it can.  A market with fewer entries than the target is progress, not
// failure, so the note has to report what actually happened.
func TestWorkbuddyExpertFiveChainsEveryExpertItCanUse(t *testing.T) {
	stub := &taskStub{
		other: func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, expertListPath):
				return jsonResponse(200, expertCatalogue(
					liveExpert(), expertFixture("ex_9c21", "agent"))), nil
			case strings.HasSuffix(req.URL.Path, chatCompletionsPath):
				// Each expert gets its own chat, so the requestId the events
				// carry is the one the server issued for that conversation.
				id := serverID
				if req.Header.Get("X-Expert-Id") == "ex_9c21" {
					id = secondServerID
				}
				return sseResponse(200, streamWithID(id), nil), nil
			}
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		},
	}
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "expert_5"})
	if err != nil {
		t.Fatalf("runExpertUse: %v", err)
	}
	if !strings.Contains(msg, "2 of 5") || !strings.Contains(msg, "agent") {
		t.Fatalf("message = %q, want the two chains the market allowed", msg)
	}

	if wbCalls(rt, chatCompletionsPath) != 2 {
		t.Fatalf("spent %d expert chats, want one per expert", wbCalls(rt, chatCompletionsPath))
	}
	seen := map[string]bool{}
	for _, req := range rt.calls {
		if strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
			seen[req.Header.Get("X-Expert-Id")] = true
		}
	}
	if !seen["ex_7f3a"] || !seen["ex_9c21"] {
		t.Fatalf("expert chats addressed %v, want both market experts", seen)
	}

	// One chain is the three summon events, the six events a desktop chat
	// reports, and the actual-use event that closes it.
	if stub.reportCount() != 2*expertChainEvents {
		t.Fatalf("reported %d events, want %d per expert: %v",
			stub.reportCount(), expertChainEvents, stub.reports)
	}
	chains := []struct {
		at    int
		id    string
		reqID string
	}{
		{0, "ex_7f3a", serverID},
		{expertChainEvents, "ex_9c21", secondServerID},
	}
	wantSummons := []string{"web_element_click", "expert_summon_click", "expert_summoned"}
	for _, ch := range chains {
		for i, code := range wantSummons {
			if got := stub.reports[ch.at+i]["eventCode"]; got != code {
				t.Fatalf("chain %s report %d = %v, want %s", ch.id, i, got, code)
			}
		}
		use := stub.reports[ch.at+expertChainEvents-1]
		if use["eventCode"] != "expert_actual_use" {
			t.Fatalf("chain %s ends with %v, want the actual-use event", ch.id, use["eventCode"])
		}
		if use["id"] != ch.id {
			t.Fatalf("chain %s use event carries id %v", ch.id, use["id"])
		}
		if use["requestId"] != ch.reqID {
			t.Fatalf("chain %s used requestId %v, want the id its own chat got: %s",
				ch.id, use["requestId"], ch.reqID)
		}
	}
}

// The team chore must ask the market for teams.  An agent entry would score
// nothing for it, so the filter is part of the behaviour, not a detail.
func TestWorkbuddyExpertTeamUseAsksTheMarketForTeams(t *testing.T) {
	var listBody map[string]any
	stub := &taskStub{
		other: func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, expertListPath):
				raw, _ := io.ReadAll(req.Body)
				_ = json.Unmarshal(raw, &listBody)
				return jsonResponse(200, expertCatalogue(expertFixture("ex_team01", "team"))), nil
			case strings.HasSuffix(req.URL.Path, chatCompletionsPath):
				return sseResponse(200, streamWithID(serverID), nil), nil
			}
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		},
	}
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertTeamUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "Expert_team_use_3"})
	if err != nil {
		t.Fatalf("runExpertTeamUse: %v", err)
	}
	if !strings.Contains(msg, "1 of 3") || !strings.Contains(msg, "team") {
		t.Fatalf("message = %q, want the one team chain the market allowed", msg)
	}
	if listBody == nil {
		t.Fatal("the market was never read")
	}
	if got := listBody["expert_type"]; got != "team" {
		t.Fatalf("expert_type = %v, want team", got)
	}
}

// One expert failing is not the whole chore failing: the rest of the market is
// still worth walking, and the note says how many chains did not complete.
func TestWorkbuddyExpertChainKeepsGoingAfterAFailedExpert(t *testing.T) {
	stub := &taskStub{
		other: func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, expertListPath):
				return jsonResponse(200, expertCatalogue(
					liveExpert(), expertFixture("ex_9c21", "agent"))), nil
			case strings.HasSuffix(req.URL.Path, chatCompletionsPath):
				if req.Header.Get("X-Expert-Id") == "ex_7f3a" {
					return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"boom"}`), nil
				}
				return sseResponse(200, streamWithID(secondServerID), nil), nil
			}
			return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
		},
	}
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "expert_5"})
	if err != nil {
		t.Fatalf("a single expert failure must not fail the chore: %v", err)
	}
	if !strings.Contains(msg, "1 of 5") || !strings.Contains(msg, "1 could not be completed") {
		t.Fatalf("message = %q, want one chain and one failure", msg)
	}

	// The failed expert still reported its summon steps before its chat broke,
	// so its chain is short by the chat events and the use event.
	if want := 3 + expertChainEvents; stub.reportCount() != want {
		t.Fatalf("reported %d events, want 3 + %d: %v", stub.reportCount(), expertChainEvents, stub.reports)
	}
	last := stub.reports[stub.reportCount()-1]
	if last["eventCode"] != "expert_actual_use" || last["id"] != "ex_9c21" {
		t.Fatalf("last report = %v, want the surviving expert's use event", last)
	}
	if wbCalls(rt, chatCompletionsPath) != 2 {
		t.Fatalf("spent %d expert chats, want one per expert", wbCalls(rt, chatCompletionsPath))
	}
}

// Without a live market id there is nothing to summon, and a locally invented
// id would score nothing, so the chore refuses instead of reporting.
func TestWorkbuddyExpertChainRefusesWithoutALiveExpertID(t *testing.T) {
	stub := expertListStub(expertCatalogue())
	rt := &fakeRT{handler: stub.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())

	msg, err := runExpertUse(context.Background(), c, wbCNAuth(t, c),
		&growthTask{TaskCode: "expert_5"})
	if err == nil {
		t.Fatalf("returned %q and no error although the market was empty", msg)
	}
	if !strings.Contains(err.Error(), "the expert market returned no agent experts to summon") {
		t.Fatalf("error = %q", err)
	}
	if stub.reportCount() != 0 {
		t.Fatal("events were reported without a live expert")
	}
	if wbCalls(rt, chatCompletionsPath) != 0 {
		t.Fatal("an expert chat was spent although no expert id was available")
	}
}

// The desktop chore and the miniprogram chore are two board rows for one
// behaviour, so they must report byte-identical event bodies.  They share one
// constructor precisely so the two cannot drift apart.
func TestWorkbuddyAutomationChoresReportTheSameEventBody(t *testing.T) {
	cases := []struct {
		code string
		mp   bool
		run  func(context.Context, *Client, *Auth, *growthTask) (string, error)
	}{
		{"automation_1", false, runAutomationCreate},
		{"Sequential_Tasks_4", true, runSequential4},
	}
	bodies := make([]map[string]any, 0, len(cases))
	for _, tc := range cases {
		stub := &taskStub{}
		rt := &fakeRT{handler: stub.handle}
		c, _ := panelClient(t, rt, cnAccountFiles())

		msg, err := tc.run(context.Background(), c, wbCNAuth(t, c),
			&growthTask{TaskCode: tc.code})
		if err != nil {
			t.Fatalf("%s: %v", tc.code, err)
		}
		if strings.TrimSpace(msg) == "" {
			t.Fatalf("%s returned no note", tc.code)
		}
		if stub.reportCount() != 1 {
			t.Fatalf("%s reported %d events, want exactly one", tc.code, stub.reportCount())
		}
		if tc.mp {
			// The miniprogram row has to be registered before the event lands.
			if stub.mpAccepts == 0 {
				t.Fatalf("%s did not accept its board row", tc.code)
			}
			if !containsString(stub.acceptCodes, tc.code) {
				t.Fatalf("%s accepted %v", tc.code, stub.acceptCodes)
			}
		}
		bodies = append(bodies, stub.reports[0])
	}

	first, second := bodies[0], bodies[1]
	for key, want := range map[string]any{
		"eventCode":    "automated_task_create_suc",
		"name":         "wb2api 自动化",
		"source":       "manually",
		"modelId":      taskModelID,
		"scheduleType": "once",
		"mode":         "LOCAL",
	} {
		if first[key] != want {
			t.Fatalf("automation_1 %s = %v, want %v", key, first[key], want)
		}
		if second[key] != want {
			t.Fatalf("Sequential_Tasks_4 %s = %v, want %v", key, second[key], want)
		}
	}
	if len(first) != len(second) {
		t.Fatalf("the chores reported different shapes: %v vs %v", first, second)
	}
	// Every other key has to match: the point of one constructor is that the
	// two chores cannot drift.  Only the clock differs -- timestamp is the
	// chore's own and presentAt is stamped by the reporting layer.
	perRun := map[string]bool{"timestamp": true, "presentAt": true}
	for key := range first {
		if perRun[key] {
			continue
		}
		if fmt.Sprint(first[key]) != fmt.Sprint(second[key]) {
			t.Fatalf("%s differs between the chores: %v vs %v", key, first[key], second[key])
		}
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
