package loomy

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"client2api/internal/core"
)

// onboarding_test.go covers the 新手任务 board.  The vendor's service is the
// only source of the completion flags, so everything here is about what this
// module does with them: it must not invent a chore, must not invent a price,
// and must not pretend a run performed the chore itself.

// boardEnvelope renders the response shape the live service really returned:
// a `tasks` object of flags, plus `earned` and `total`.
func boardEnvelope(t *testing.T, flags map[string]bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"tasks": flags, "earned": 0, "total": 10000})
	if err != nil {
		t.Fatalf("marshalling the board: %v", err)
	}
	return okEnvelope(string(raw))
}

// completeEnvelope is the completion response the vendor's own client reads.
func completeEnvelope(already bool, balance *float64) string {
	payload := map[string]any{"alreadyCompleted": already}
	if balance != nil {
		payload["balance"] = *balance
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return okEnvelope(string(raw))
}

// onboardingTransport answers both onboarding paths from one flag set and
// reports the completion call as a fresh one.
func onboardingTransport(t *testing.T, flags map[string]bool) *stubTransport {
	t.Helper()
	return &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		switch {
		case strings.HasSuffix(r.URL.Path, onboardingCompletePath):
			return jsonResponse(http.StatusOK, completeEnvelope(false, nil))
		case strings.HasSuffix(r.URL.Path, onboardingTasksPath):
			return jsonResponse(http.StatusOK, boardEnvelope(t, flags))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			return jsonResponse(http.StatusNotFound, `{}`)
		}
	}}
}

// rowsByCode indexes a board.
func rowsByCode(rows []core.TaskInfo) map[string]core.TaskInfo {
	out := make(map[string]core.TaskInfo, len(rows))
	for _, row := range rows {
		out[row.Code] = row
	}
	return out
}

// The board's prices and keys are the vendor's, not this module's invention.
// The live service reports total=10000, so a registry that did not sum to it
// would have this gateway disagreeing with the vendor about the same board.
func TestTheBoardIsTheVendorsOwnRegistry(t *testing.T) {
	var sum int64
	seen := map[string]bool{}
	for _, ch := range onboardingRegistry {
		if ch.Key == "" || ch.Title == "" || ch.Group == "" {
			t.Errorf("registry entry %+v is missing a key, title or group", ch)
		}
		if seen[ch.Key] {
			t.Errorf("registry repeats key %q", ch.Key)
		}
		seen[ch.Key] = true
		if ch.Points <= 0 {
			t.Errorf("registry prices %q at %d", ch.Key, ch.Points)
		}
		sum += ch.Points
	}
	if sum != 10000 {
		t.Errorf("registry sums to %d, want the 10000 the vendor reports as total", sum)
	}

	// The eight keys GET /api/v1/onboarding/tasks returned, verbatim.
	want := []string{
		"configure_remote", "create_soul", "first_message", "generate_ppt",
		"install_skill", "pick_skill", "set_schedule", "share_soul",
	}
	got := make([]string, 0, len(seen))
	for key := range seen {
		got = append(got, key)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("registry keys = %v, want the eight the vendor reports: %v", got, want)
	}
}

func TestTheBoardReportsTheVendorsFlagsWithTheirPrices(t *testing.T) {
	rt := onboardingTransport(t, map[string]bool{"first_message": true, "share_soul": true})
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	rows, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(rows) != len(onboardingRegistry) {
		t.Fatalf("board has %d rows, want %d", len(rows), len(onboardingRegistry))
	}
	byCode := rowsByCode(rows)

	done := byCode["first_message"]
	if !done.Claimed || done.Auto || done.Locked {
		t.Errorf("first_message = %+v, want claimed and not runnable", done)
	}
	if done.Current != 1 || done.Target != 1 {
		t.Errorf("first_message progress = %d/%d, want 1/1", done.Current, done.Target)
	}
	if done.Credit != 500 || done.Title != "发送你的第一条消息" || done.Group != "初识 Loomy" {
		t.Errorf("first_message = %+v, want the vendor's own title, group and price", done)
	}

	todo := byCode["generate_ppt"]
	if !todo.Auto || todo.Claimed || todo.Locked {
		t.Errorf("generate_ppt = %+v, want runnable and unclaimed", todo)
	}
	if todo.Current != 0 || todo.Target != 1 {
		t.Errorf("generate_ppt progress = %d/%d, want 0/1", todo.Current, todo.Target)
	}
	if todo.Credit != 1500 {
		t.Errorf("generate_ppt credit = %d, want 1500", todo.Credit)
	}
	if todo.Note == "" {
		t.Error("a runnable row must explain what running it does")
	}

	// The vendor's own order, not a sort of this module's own.
	if rows[0].Code != "first_message" || rows[len(rows)-1].Code != "share_soul" {
		t.Errorf("board runs %q..%q, want the vendor's order", rows[0].Code, rows[len(rows)-1].Code)
	}

	// The credential goes on the header the points API reads, and nowhere else.
	req := rt.requests()[0]
	if req.Method != http.MethodGet {
		t.Errorf("method = %s, want GET", req.Method)
	}
	if want := "/api/v1" + onboardingTasksPath; !strings.HasSuffix(req.URL.Path, want) {
		t.Errorf("path = %s, want ...%s", req.URL.Path, want)
	}
	if got := req.Header.Get("token"); got != testToken {
		t.Errorf("token header = %q, want the session", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q; this API reads only the token header", got)
	}
}

// The vendor's response object is a flag map, and the vendor's own client
// reads a missing key as "not done" (it looks each registry key up and treats a
// miss as false).  Locking a chore the response happens to omit would hide a
// task that still runs, so this module reads it the same way.
func TestAChoreTheVendorDoesNotMentionIsSimplyNotDone(t *testing.T) {
	rt := onboardingTransport(t, map[string]bool{"first_message": true})
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	rows, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(rows) != len(onboardingRegistry) {
		t.Fatalf("board has %d rows, want the full registry of %d", len(rows), len(onboardingRegistry))
	}
	for _, row := range rows {
		if row.Code == "first_message" {
			continue
		}
		if row.Locked {
			t.Errorf("%s = %+v, want it treated as not done, not as retired", row.Code, row)
		}
		if !row.Auto || row.Claimed || row.Current != 0 {
			t.Errorf("%s = %+v, want an unclaimed runnable row", row.Code, row)
		}
	}
}

// A chore the vendor grew is shown rather than dropped, and the row says
// plainly that this module has no wording or price for it.
func TestTheBoardShowsAChoreTheRegistryDoesNotKnow(t *testing.T) {
	rt := onboardingTransport(t, map[string]bool{
		"first_message":     true,
		"brand_new_thing":   true,
		"another_new_thing": false,
	})
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	rows, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if want := len(onboardingRegistry) + 2; len(rows) != want {
		t.Fatalf("board has %d rows, want %d: an unknown chore must be shown, not dropped", len(rows), want)
	}
	tail := rows[len(rows)-2:]
	if tail[0].Code != "another_new_thing" || tail[1].Code != "brand_new_thing" {
		t.Errorf("unknown chores = %q, %q; want them sorted after the registry", tail[0].Code, tail[1].Code)
	}
	for _, row := range tail {
		if row.Auto {
			t.Errorf("%s is offered although this module does not know it: %+v", row.Code, row)
		}
		if !row.Locked || row.Note == "" {
			t.Errorf("%s = %+v, want locked with an explanation", row.Code, row)
		}
		if row.Title != row.Code || row.Credit != 0 {
			t.Errorf("%s = %+v, want the bare key and no invented price", row.Code, row)
		}
	}
	if done := tail[1]; !done.Claimed || done.Current != 1 {
		t.Errorf("brand_new_thing = %+v, want the vendor's completion flag honoured", done)
	}
	if todo := tail[0]; todo.Claimed || todo.Current != 0 {
		t.Errorf("another_new_thing = %+v, want an unclaimed row", todo)
	}
}

func TestTheBoardIsCachedButAFailedReadIsNot(t *testing.T) {
	flags := map[string]bool{}
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		return jsonResponse(http.StatusOK, boardEnvelope(t, flags))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	for i := 0; i < 3; i++ {
		if _, err := c.Tasks(context.Background(), ""); err != nil {
			t.Fatalf("Tasks #%d: %v", i, err)
		}
	}
	if got := rt.count(); got != 1 {
		t.Errorf("three board reads made %d requests, want 1: the board is cached", got)
	}

	// A read that failed must not be remembered, or one transient 500 would
	// freeze an empty board in place for the whole TTL.
	failing := alwaysJSON(http.StatusInternalServerError, `{"code":"500","desc":"boom"}`)
	c2 := newTestClient(t, "{}", failing)
	seedAccount(t, c2, "loomy-1", testToken, 0)
	for i := 0; i < 2; i++ {
		if _, err := c2.Tasks(context.Background(), ""); err == nil {
			t.Fatalf("Tasks #%d reported success on a 500", i)
		}
	}
	if got := failing.count(); got != 2 {
		t.Errorf("two failed reads made %d requests, want 2: failures are never cached", got)
	}
}

func TestRunTaskPostsTheKeyTheVendorClientPosts(t *testing.T) {
	balance := 10500.0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if strings.HasSuffix(r.URL.Path, onboardingCompletePath) {
			return jsonResponse(http.StatusOK, completeEnvelope(false, &balance))
		}
		return jsonResponse(http.StatusOK, boardEnvelope(t, map[string]bool{}))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	res, err := c.RunTask(context.Background(), "", "create_soul")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask = %+v, want OK", res)
	}
	if res.Code != "create_soul" || res.AccountID != "loomy-1" {
		t.Errorf("result = %+v, want the chore code and the account it used", res)
	}
	if res.Credit != 1500 {
		t.Errorf("Credit = %d, want the registry price 1500", res.Credit)
	}
	if !strings.Contains(res.Message, "10500") {
		t.Errorf("Message = %q, want the balance the vendor reported", res.Message)
	}

	// Exactly the request the vendor's own client sends.
	req := rt.requests()[0]
	if req.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", req.Method)
	}
	if want := "/api/v1" + onboardingCompletePath; !strings.HasSuffix(req.URL.Path, want) {
		t.Errorf("path = %s, want ...%s", req.URL.Path, want)
	}
	if got := req.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := req.Header.Get("token"); got != testToken {
		t.Errorf("token header = %q, want the session", got)
	}
	var sent map[string]any
	mustJSON(t, rt.lastBody(), &sent)
	if len(sent) != 1 || sent["key"] != "create_soul" {
		t.Errorf("body = %s, want exactly {\"key\":\"create_soul\"}", rt.lastBody())
	}
}

// The vendor's service is idempotent and reports a repeat through
// alreadyCompleted rather than an error.  A repeat is therefore a success --
// but it awarded nothing, and must not be worded as if it had.
func TestRunTaskTreatsARepeatAsASuccessThatAwardedNothing(t *testing.T) {
	balance := 10000.0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if strings.HasSuffix(r.URL.Path, onboardingCompletePath) {
			return jsonResponse(http.StatusOK, completeEnvelope(true, &balance))
		}
		return jsonResponse(http.StatusOK, boardEnvelope(t, map[string]bool{}))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	res, err := c.RunTask(context.Background(), "", "first_message")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask = %+v, want a repeat to be a success, not a refusal", res)
	}
	if res.Credit != 0 {
		t.Errorf("Credit = %d, want 0: the vendor said nothing was added", res.Credit)
	}
	if !strings.Contains(res.Message, "此前已完成") {
		t.Errorf("Message = %q, want it to say the chore was already done", res.Message)
	}
}

// An unknown code is refused here, not forwarded as a guess.  The live service
// answers business code 100001 ("未知的 task key") for one, and the vendor's own
// client refuses it before the request too.
func TestRunTaskRefusesAnUnknownCodeWithoutCallingTheVendor(t *testing.T) {
	rt := &stubTransport{}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	_, err := c.RunTask(context.Background(), "", "__probe_does_not_exist__")
	if err == nil {
		t.Fatal("RunTask accepted a code the registry does not contain")
	}
	if !strings.Contains(err.Error(), "__probe_does_not_exist__") {
		t.Errorf("error = %q, want the rejected code named", err)
	}
	if got := rt.count(); got != 0 {
		t.Errorf("%d requests went out; an unknown code must be refused locally", got)
	}
}

// With no credential there is nothing to ask, so the board is empty and a run
// is a refusal with an explanation.  Neither may be an invented row or a
// fabricated success.
func TestTheBoardWithoutAnAccountIsEmptyAndInventsNothing(t *testing.T) {
	rt := &stubTransport{}
	c := newTestClient(t, "{}", rt)

	rows, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks with no account: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("board = %+v, want nothing: there is no credential to ask with", rows)
	}
	if got := rt.count(); got != 0 {
		t.Errorf("%d requests went out with no credential", got)
	}

	res, err := c.RunTask(context.Background(), "", "first_message")
	if err != nil {
		t.Fatalf("RunTask with no account must be a refusal, not an error: %v", err)
	}
	if res.OK || res.Error == "" {
		t.Errorf("RunTask = %+v, want OK false with an explanation", res)
	}
}

func TestAnUnknownAccountIDIsAnError(t *testing.T) {
	rt := &stubTransport{}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Tasks(context.Background(), "no-such-account"); err == nil {
		t.Error("Tasks accepted an account id that does not exist")
	}
	if _, err := c.RunTask(context.Background(), "no-such-account", "first_message"); err == nil {
		t.Error("RunTask accepted an account id that does not exist")
	}
	if got := rt.count(); got != 0 {
		t.Errorf("%d requests went out for an unknown account", got)
	}
}

// The vendor's onboarding state belongs to the account, not to a healthy
// session, so a parked credential can still answer.  Reporting "no account"
// here would tell the operator something untrue.
func TestTheBoardStillReadsAParkedAccountsFlags(t *testing.T) {
	rt := onboardingTransport(t, map[string]bool{"first_message": true})
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)
	if err := c.store.setEnabled("loomy-1", false); err != nil {
		t.Fatalf("disabling the account: %v", err)
	}

	rows, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(rows) != len(onboardingRegistry) {
		t.Fatalf("board has %d rows for a parked credential, want %d", len(rows), len(onboardingRegistry))
	}
	if got := rt.count(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// A run changes the board, so the cache must not survive it -- otherwise the
// row the operator just ran would still read as runnable.
func TestARunRefreshesTheBoardInsteadOfServingTheStaleOne(t *testing.T) {
	flags := map[string]bool{}
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if strings.HasSuffix(r.URL.Path, onboardingCompletePath) {
			flags["first_message"] = true
			return jsonResponse(http.StatusOK, completeEnvelope(false, nil))
		}
		return jsonResponse(http.StatusOK, boardEnvelope(t, flags))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	before, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if row := rowsByCode(before)["first_message"]; !row.Auto {
		t.Fatalf("first_message = %+v, want runnable before the run", row)
	}

	if _, err := c.RunTask(context.Background(), "", "first_message"); err != nil {
		t.Fatalf("RunTask: %v", err)
	}

	after, err := c.Tasks(context.Background(), "")
	if err != nil {
		t.Fatalf("Tasks after the run: %v", err)
	}
	if row := rowsByCode(after)["first_message"]; !row.Claimed || row.Auto {
		t.Errorf("first_message = %+v, want it claimed straight after the run", row)
	}
	if got := rt.count(); got != 3 {
		t.Errorf("requests = %d, want board + completion + board", got)
	}
}

// A session the vendor rejects is not retried: it is parked for the long auth
// cooldown, and the result says what the operator has to do about it.
func TestARunOnADeadSessionParksTheAccount(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(http.StatusOK, failureEnvelope("100002", "缺少 token")))
	seedAccount(t, c, "loomy-1", testToken, 0)

	res, err := c.RunTask(context.Background(), "", "first_message")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("RunTask reported success on a 100002 response")
	}
	if !strings.Contains(res.Error, "log in again") {
		t.Errorf("Error = %q, want the only remedy this vendor has", res.Error)
	}
	if strings.Contains(res.Error, testToken) {
		t.Errorf("Error = %q, want no session token in it", res.Error)
	}
	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished")
	}
	if !acc.dead {
		t.Error("a session the vendor rejected must be parked, not retried")
	}
}
