package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// batches_test.go covers the scheduled chore surface offline: what Batches()
// publishes, and what each chore does with the vendor's answers.  Nothing here
// reaches the network -- every test drives a fake RoundTripper and a temp data
// directory, exactly like tasks_test.go and checkin_test.go.
//
// The load-bearing assertion is that a scheduled chore never reads the growth
// board: RunTask's board path refuses a code the board does not carry, so if a
// chore were ever routed through it the whole batch would quietly do nothing.

// --- fixtures ---------------------------------------------------------------

// batchOther answers the routes the shared taskStub does not know, keyed by path
// suffix, so a test can state the exact answer the vendor gives.
func batchOther(handlers map[string]string) func(*http.Request) (*http.Response, error) {
	return func(req *http.Request) (*http.Response, error) {
		for suffix, body := range handlers {
			if strings.HasSuffix(req.URL.Path, suffix) {
				return jsonResponse(200, body), nil
			}
		}
		return jsonResponse(http.StatusInternalServerError, `{"code":1,"msg":"unexpected route"}`), nil
	}
}

// batchClient builds a CN client over the shared task stub plus extra routes, and
// hands back the transport so a test can count what was sent.
func batchClient(t *testing.T, s *taskStub, extra map[string]string) (*Client, *fakeRT) {
	t.Helper()
	if extra != nil {
		s.other = batchOther(extra)
	}
	rt := &fakeRT{handler: s.handle}
	c, _ := panelClient(t, rt, cnAccountFiles())
	return c, rt
}

// noRefreshFiles is one CN account without a refresh token, the shape the
// keepalive chore must refuse without spending an upstream call.
func noRefreshFiles() map[string]string {
	return map[string]string{
		"cn.json": credJSON("access-token-abcdefgh", "", realmCN,
			"copilot.tencent.com", "uid-norefresh-0001", 0),
	}
}

// --- the published batches --------------------------------------------------

func TestWorkbuddyBatchesPublishTheReferenceNames(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	got := c.Batches()

	want := []string{batchNameCheckin, batchNameTravel, batchNameActivity, batchNameKeepalive, batchNameBlackcat, batchNameGrowth}
	if len(got) != len(want) {
		t.Fatalf("got %d batches, want %d (%v)", len(got), len(want), batchNames(got))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("batch[%d] = %q, want %q (%v)", i, got[i].Name, name, batchNames(got))
		}
	}

	// Names are the keys the scheduler's groups are matched by, so a rename here
	// is a batch that can never fire.
	for _, b := range got {
		if len(b.Codes) == 0 {
			t.Fatalf("batch %q declares no codes", b.Name)
		}
		if b.Gate != "" {
			t.Fatalf("batch %q declares gate %q, want none", b.Name, b.Gate)
		}
		// The scheduler's own defaults (45s / 5s) were measured against the
		// reference; leaving them zero keeps this module from inventing a pace.
		if b.AccountGap != 0 || b.Settle != 0 {
			t.Fatalf("batch %q sets AccountGap=%v Settle=%v, want the scheduler defaults",
				b.Name, b.AccountGap, b.Settle)
		}
	}

	byName := map[string][]string{}
	for _, b := range got {
		byName[b.Name] = b.Codes
	}
	// Adoption runs before the trip: the travel endpoints refuse an account whose
	// buddy chore has not been credited yet.
	if codes := byName[batchNameTravel]; len(codes) != 2 || codes[0] != "first_buddy" || codes[1] != choreTravel {
		t.Fatalf("travel codes = %v, want [first_buddy travel]", codes)
	}
	// The night slot is an ordinary board chore, so its batch publishes the board
	// code rather than a synthetic one.
	if codes := byName[batchNameBlackcat]; len(codes) != 1 || codes[0] != "black_cat" {
		t.Fatalf("blackcat codes = %v, want [black_cat]", codes)
	}
	// Growth is resolved at run time: a frozen list would fire at chores the
	// vendor has not unlocked yet.
	if codes := byName[batchNameGrowth]; len(codes) != 1 || codes[0] != choreGrowth {
		t.Fatalf("growth codes = %v, want [%s]", codes, choreGrowth)
	}

	// The scheduler calls Batches() on every tick and the panel may render it, so
	// one caller's edit must not leak into the next answer.
	got[0].Codes[0] = "clobbered"
	if again := c.Batches(); again[0].Codes[0] != choreCheckin {
		t.Fatalf("Batches() returned shared state: second call gave %q", again[0].Codes[0])
	}
}

func TestWorkbuddyBatchChoresAreNotBoardCodes(t *testing.T) {
	for _, code := range []string{choreCheckin, choreTravel, choreActivity, choreKeepalive, choreGrowth} {
		if _, ok := batchChoreFor(code); !ok {
			t.Fatalf("chore %q has no runner", code)
		}
	}
	// Padded input is what a config file hands over; the dispatch trims.
	if _, ok := batchChoreFor("  " + choreCheckin + " "); !ok {
		t.Fatal("a padded chore code was not recognised")
	}
	// black_cat is a real board row, so it must keep going through the board
	// path (its runner enforces the night window); it is not a chore.
	if _, ok := batchChoreFor("black_cat"); ok {
		t.Fatal("black_cat was registered as a scheduled chore; it is a board code")
	}
	if _, ok := batchChoreFor("no_such_chore"); ok {
		t.Fatal("an unknown code was accepted as a chore")
	}
}

// --- check-in ---------------------------------------------------------------

func TestWorkbuddyScheduledCheckinSkipsTheInternationalRealm(t *testing.T) {
	rt := &fakeRT{}
	c, _ := panelClient(t, rt, intlAccountFiles())
	a := wbIntlAuth(t, c)
	if !a.IsGlobal() {
		t.Fatal("fixture account is not global")
	}

	res, err := c.RunTask(context.Background(), a.ID(), choreCheckin)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("the international realm credited a check-in")
	}
	if !res.Skipped {
		t.Fatal("the international realm check-in was not marked as skipped")
	}
	if !strings.Contains(res.Message, "international realm") {
		t.Fatalf("message = %q, want it to name the international realm", res.Message)
	}
	// The reference scheduler spends no call on a realm that cannot credit the
	// reward: one would only be risk-control noise.
	if paths := wbPaths(rt); len(paths) != 0 {
		t.Fatalf("the global skip sent %v, want no request", paths)
	}
}

func TestWorkbuddyScheduledCheckinKeepsTheUpstreamRefusalMessage(t *testing.T) {
	s := &taskStub{}
	c, _ := batchClient(t, s, map[string]string{
		dailyCheckinPathV2: wbRefusal(40001, "activity ended"),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreCheckin)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true for a business refusal")
	}
	if !strings.Contains(res.Message, "activity ended") {
		t.Fatalf("message = %q, want the vendor's own refusal reason", res.Message)
	}
}

func TestWorkbuddyScheduledCheckinRunsOnTheCnRealm(t *testing.T) {
	s := &taskStub{}
	// The chore is the reward *plus its tail*: the reference hangs the
	// consecutive-login pass off the end of the check-in run
	// (internal/scheduler/scheduler.go:375, "末尾追加连登管家"), so a run that
	// stopped at the reward would leave every 7d/14d/28d tier unredeemed on the
	// one day it became redeemable.  Every route the tail touches is stubbed
	// here -- batchOther answers 500 for anything unstubbed, so without these the
	// pass would stop at its first read and the path assertion below would be
	// pinning an aborted chore rather than the real one.
	c, rt := batchClient(t, s, map[string]string{
		dailyCheckinPathV2:    okEnvelope(),
		dailyCheckinPath:      okEnvelope(),
		heatmapPath:           wbEnvelope(`{"cells":[]}`),
		claimGiftPath:         okEnvelope(),
		claimCompensationPath: okEnvelope(),
		streakPath:            wbEnvelope(`{"streak":{"days":3}}`),
		lotterySummaryPath:    wbEnvelope(`{"chances":0}`),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreCheckin)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("check-in refused: error=%q message=%q", res.Error, res.Message)
	}
	if !strings.Contains(res.Message, "credited") {
		t.Fatalf("message = %q, want it to report the credit", res.Message)
	}
	// The reward is first and the tail follows in one fixed order.  Pinning the
	// sequence is the point of this test: the tail is idempotent and every step
	// may refuse, so only the wire shape proves the pass ran in full.
	want := []string{
		dailyCheckinPathV2,
		heatmapPath,
		claimGiftPath,
		claimCompensationPath,
		streakPath,
		lotterySummaryPath,
	}
	if got := strings.Join(wbPaths(rt), " "); got != strings.Join(want, " ") {
		t.Fatalf("the check-in chore sent %v, want %v", wbPaths(rt), want)
	}
	// The whole point of the dispatch: a chore must not be routed through the
	// board path, which would refuse it for having no row.
	if n := s.listCount(); n != 0 {
		t.Fatalf("the scheduled check-in read the board %d time(s), want 0", n)
	}
}

// --- travel -----------------------------------------------------------------

func TestWorkbuddyScheduledTravelSendsTheCatOutWhenIdle(t *testing.T) {
	s := &taskStub{}
	c, rt := batchClient(t, s, map[string]string{
		travelStatusPath: wbEnvelope(`{"state":"idle","daily_limit_reached":false}`),
		travelDepartPath: okEnvelope(),
		travelClaimPath:  wbEnvelope(`{"reward_credit":30}`),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreTravel)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("travel refused: error=%q message=%q", res.Error, res.Message)
	}
	if !strings.Contains(res.Message, "sent out") {
		t.Fatalf("message = %q, want the departure report", res.Message)
	}
	if n := wbCalls(rt, travelDepartPath); n != 1 {
		t.Fatalf("depart sends = %d, want 1", n)
	}
	if n := wbCalls(rt, travelClaimPath); n != 0 {
		t.Fatalf("claim sends = %d, want 0 (nothing had arrived)", n)
	}
}

func TestWorkbuddyScheduledTravelClaimsAnArrivedTrip(t *testing.T) {
	// The shared stub answers any path ending in /claim -- the growth board's
	// reward route shares that tail -- so the travel reward is served through
	// claimBody rather than the extra-route map.
	s := &taskStub{claimBody: wbEnvelope(`{"reward_credit":30}`)}
	c, rt := batchClient(t, s, map[string]string{
		travelStatusPath: wbEnvelope(`{"state":"arrived","record_id":77,"reward_credit":30}`),
		travelDepartPath: okEnvelope(),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreTravel)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("travel refused: error=%q message=%q", res.Error, res.Message)
	}
	if res.Credit != 30 {
		t.Fatalf("credit = %d, want 30", res.Credit)
	}
	if n := wbCalls(rt, travelClaimPath); n != 1 {
		t.Fatalf("claim sends = %d, want 1", n)
	}
	if n := wbCalls(rt, travelDepartPath); n != 0 {
		t.Fatalf("depart sends = %d, want 0 (the cat was already out)", n)
	}
}

func TestWorkbuddyScheduledTravelHonoursTheDailyLimit(t *testing.T) {
	s := &taskStub{}
	c, rt := batchClient(t, s, map[string]string{
		travelStatusPath: wbEnvelope(`{"state":"idle","daily_limit_reached":true}`),
		travelDepartPath: okEnvelope(),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreTravel)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("a second trip was sent on the same day")
	}
	if !res.Skipped {
		t.Fatal("the daily-limit travel result was not marked as skipped")
	}
	if !strings.Contains(res.Message, "already been sent") {
		t.Fatalf("message = %q, want the daily-limit report", res.Message)
	}
	if n := wbCalls(rt, travelDepartPath); n != 0 {
		t.Fatalf("depart sends = %d, want 0", n)
	}
}

func TestWorkbuddyScheduledTravelReportsATravellingCat(t *testing.T) {
	s := &taskStub{}
	c, rt := batchClient(t, s, map[string]string{
		travelStatusPath: wbEnvelope(`{"state":"traveling","record_id":77}`),
		travelDepartPath: okEnvelope(),
		travelClaimPath:  wbEnvelope(`{"reward_credit":30}`),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreTravel)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("a travelling cat was reported as a failure: %q", res.Message)
	}
	if !strings.Contains(res.Message, "travelling") {
		t.Fatalf("message = %q, want the travelling report", res.Message)
	}
	if n := wbCalls(rt, travelDepartPath); n != 0 {
		t.Fatalf("depart sends = %d, want 0", n)
	}
	if n := wbCalls(rt, travelClaimPath); n != 0 {
		t.Fatalf("claim sends = %d, want 0", n)
	}
}

// --- activity ---------------------------------------------------------------

func TestWorkbuddyScheduledActivityReadsTheStreakBack(t *testing.T) {
	s := &taskStub{}
	c, rt := batchClient(t, s, map[string]string{
		streakPath: wbEnvelope(`{"streak":{"days":3}}`),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreActivity)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("activity refused: error=%q message=%q", res.Error, res.Message)
	}
	if s.reportCount() != 1 {
		t.Fatalf("activity reports = %d, want 1", s.reportCount())
	}
	if n := wbCalls(rt, streakPath); n != 1 {
		t.Fatalf("streak reads = %d, want 1 (the read-back is the point)", n)
	}
	if !strings.Contains(res.Message, "3 day(s)") {
		t.Fatalf("message = %q, want the read-back streak", res.Message)
	}
}

func TestWorkbuddyScheduledActivityWarnsWhenTheStreakStaysZero(t *testing.T) {
	s := &taskStub{}
	c, _ := batchClient(t, s, map[string]string{
		streakPath: wbEnvelope(`{"streak":{"days":0}}`),
	})
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreActivity)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	// A dropped report is not a transport failure: the vendor answered 200.  The
	// chore reports success and says the scoring did not move.
	if !res.OK {
		t.Fatalf("a 200 activity report was treated as a failure: %q", res.Message)
	}
	if !strings.Contains(res.Message, "still 0") {
		t.Fatalf("message = %q, want the dropped-report warning", res.Message)
	}
}

// --- keepalive --------------------------------------------------------------

func TestWorkbuddyScheduledKeepaliveRefusesWithoutARefreshToken(t *testing.T) {
	rt := &fakeRT{}
	c, _ := panelClient(t, rt, noRefreshFiles())

	res, err := c.RunTask(context.Background(), "uid-norefresh-0001", choreKeepalive)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK {
		t.Fatal("an account without a refresh token was reported as kept alive")
	}
	if !strings.Contains(res.Message, "no refresh token") {
		t.Fatalf("message = %q, want it to name the missing refresh token", res.Message)
	}
	if paths := wbPaths(rt); len(paths) != 0 {
		t.Fatalf("the refusal sent %v, want no request", paths)
	}
}

// --- growth scan ------------------------------------------------------------

func TestWorkbuddyScheduledGrowthClaimsWhatIsAlreadyFinished(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		// skill_1 has no runner (it needs a real Skill tool call in the client),
		// so it is not Auto and the scan has to leave it where it is.
		return boardJSON(
			taskRowJSON("chat_5", "accepted", 5, 5),
			taskRowJSON("skill_1", "not_accepted", 0, 1),
		)
	}}
	c, _ := batchClient(t, s, nil)
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreGrowth)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("growth scan refused: error=%q message=%q", res.Error, res.Message)
	}
	s.mu.Lock()
	claims := append([]string(nil), s.claims...)
	s.mu.Unlock()
	if len(claims) != 1 || claims[0] != "chat_5" {
		t.Fatalf("claims = %v, want [chat_5]", claims)
	}
	// The default claim answer pays 100 credit / 2 energy, and the scan must add
	// it up so the batch report carries the total.
	if res.Credit != 100 || res.Energy != 2 {
		t.Fatalf("totals = %d credit / %d energy, want 100 / 2", res.Credit, res.Energy)
	}
	if !strings.Contains(res.Message, "ran 0 chore(s), claimed 1") {
		t.Fatalf("message = %q, want the scan summary", res.Message)
	}
}

func TestWorkbuddyScheduledGrowthSkipsAClaimedRow(t *testing.T) {
	s := &taskStub{board: func(bool, int) string {
		return boardJSON(taskRowJSON("chat_5", "claimed", 5, 5))
	}}
	c, _ := batchClient(t, s, nil)
	a := wbCNAuth(t, c)

	res, err := c.RunTask(context.Background(), a.ID(), choreGrowth)
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("growth scan refused: %q", res.Message)
	}
	s.mu.Lock()
	claims := len(s.claims)
	s.mu.Unlock()
	if claims != 0 {
		t.Fatalf("claims = %d, want 0 for an already-claimed row", claims)
	}
	if !strings.Contains(res.Message, "ran 0 chore(s), claimed 0") {
		t.Fatalf("message = %q, want a no-op summary", res.Message)
	}
}

// --- the board path is untouched -------------------------------------------

func TestWorkbuddyRunTaskStillRefusesAnUnknownCode(t *testing.T) {
	s := &taskStub{}
	c, _ := batchClient(t, s, nil)
	a := wbCNAuth(t, c)

	// Both a chore code and a real code are accepted; anything else must still be
	// the same hard error it always was, or a typo would look like a no-op.
	if _, err := c.RunTask(context.Background(), a.ID(), "no_such_code"); err == nil {
		t.Fatal("an unknown task code was accepted")
	} else if !strings.Contains(err.Error(), "has no runner") {
		t.Fatalf("error = %v, want the no-runner error", err)
	}
}

// batchNames renders the batch names for a failure message.
func batchNames(bs []core.Batch) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}
