package doubao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"client2api/internal/core"
)

// growth_test.go pins the growth (campaign / check-in / quota) contract that was
// read out of the live desktop client on 2026-11-13.  Two facts are load-bearing
// and easy to regress:
//
//   - every growth call carries the PC product aid (497858), not the web aid;
//     the same path under the web aid is refused with 710010702.
//   - a reward task is claimed by campaign_code, with an empty task_id.

// growthSrv answers the growth endpoints for one account.  It records the last
// request seen per path so a test can assert the wire shape.
type growthSrv struct {
	*httptest.Server
	seen   map[string][]byte
	query  url.Values
	detail string
}

func newGrowthSrv(t *testing.T, detail string) *growthSrv {
	t.Helper()
	g := &growthSrv{seen: map[string][]byte{}, detail: detail}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		g.seen[r.URL.Path] = raw
		g.query = r.URL.Query()
		switch r.URL.Path {
		case pathCampaignV3Detail:
			if g.detail == "" {
				_, _ = w.Write([]byte(`{"code":0,"data":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":` + g.detail + `}`))
		case pathCampaignCheckin:
			_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":true,"checkin_date":"2026-11-13","continuous_days":1,"idempotent":false,"reward_amount":200,"reward_type":"quota_grant"}}`))
		case pathCampaignClaim:
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_status":3}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.Close)
	return g
}

// v3Detail builds a v3/detail data object with a check-in row, one chore and
// one milestone.
func v3Detail(checkedIn bool) string {
	signinStatus := 1
	if checkedIn {
		signinStatus = 3
	}
	signin := map[string]any{
		"campaign_code": "daily_signin", "campaign_group_code": defaultCampaignGroup,
		"task_kind": "daily_signin", "title": "每日签到", "task_status": signinStatus,
		"daily_checkin": map[string]any{
			"checked_in_today": checkedIn, "continuous_days": 3, "total_days": 4,
			"reward": map[string]any{"reward_amount": 200, "reward_type": "quota_grant"},
		},
	}
	task := map[string]any{
		"campaign_code": "pc_task_complete_work_task_daily", "task_kind": "chat_activity",
		"title": "每日完成一项工作任务", "task_status": taskStatusCompleted,
		"reward_summaries": []any{map[string]any{"reward_amount": 50, "reward_type": "quota_grant"}},
	}
	// A second work chore NOT yet done (Created): it must read as not claimable.
	pending := map[string]any{
		"campaign_code": "pc_task_generate_excel_v2", "task_kind": "chat_activity",
		"title": "用 Excel 完成一项任务", "task_status": taskStatusCreated,
		"reward_summaries": []any{map[string]any{"reward_amount": 100, "reward_type": "quota_grant"}},
	}
	ms := map[string]any{
		"campaign_code": "points_milestone_5000", "task_kind": "points_milestone",
		"title": "累计获得 5000 点额度", "milestone_threshold": 5000, "task_status": 1,
		"reward_summaries": []any{map[string]any{"reward_amount": 1300, "reward_type": "quota_grant"}},
	}
	detail := map[string]any{
		"campaign_code": defaultCampaignGroup,
		"progress":      map[string]any{"total_amount": 800},
		"tasks":         []any{signin, task, pending},
		"milestones":    []any{ms},
		"fission_campaign": map[string]any{
			"campaign_code": "invite_campaign_normal_v1",
			"campaign_data": map[string]any{"invite_code": "ABC123", "invite_link": "https://www.doubao.com/x"},
		},
	}
	b, _ := json.Marshal(detail)
	return string(b)
}

func newGrowthClient(t *testing.T, base string) *Client {
	t.Helper()
	c := newWebClient(t, base)
	_, err := c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	if err != nil {
		t.Fatalf("store.add: %v", err)
	}
	return c
}

// TestCampaignUsesThePcProductAid is the fact that cost a long debugging
// session: the growth endpoints are refused under the web aid.
func TestCampaignUsesThePcProductAid(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)

	if _, err := c.campaignBoardFor(context.Background(), mustAccount(t, c, "a1"), true); err != nil {
		t.Fatalf("campaignBoardFor: %v", err)
	}
	if got := g.query.Get("aid"); got != paramPCAid {
		t.Fatalf("aid = %q, want the PC aid %q (the web aid is refused with 710010702)", got, paramPCAid)
	}
	if got := g.query.Get("pkg_type"); got != paramPCPkgType {
		t.Fatalf("pkg_type = %q, want %q", got, paramPCPkgType)
	}
	if got := g.query.Get("doubao_pc_version"); got != paramPCPCVersion {
		t.Fatalf("doubao_pc_version = %q, want %q", got, paramPCPCVersion)
	}
}

func mustAccount(t *testing.T, c *Client, id string) account {
	t.Helper()
	a, ok := c.store.byID(id)
	if !ok {
		t.Fatalf("no account %q", id)
	}
	return a
}

// TestCheckinPostsTheGroupScene pins the check-in wire shape and the success
// mapping (idempotent repeat counts as done).
func TestCheckinPostsTheGroupScene(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)

	res, err := c.Checkin(context.Background(), "a1", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("Checkin not OK: %+v", res)
	}
	var body map[string]any
	_ = json.Unmarshal(g.seen[pathCampaignCheckin], &body)
	if body["scene"] != defaultCampaignGroup {
		t.Fatalf("checkin scene = %v, want %q", body["scene"], defaultCampaignGroup)
	}
	if res.Data["reward_amount"] != int64(200) {
		t.Fatalf("reward_amount = %v, want 200", res.Data["reward_amount"])
	}
}

// TestCheckinSkipsWhenAlreadyCheckedIn avoids a pointless vendor round trip.
func TestCheckinSkipsWhenAlreadyCheckedIn(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(true))
	c := newGrowthClient(t, g.URL)

	res, err := c.Checkin(context.Background(), "a1", "")
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || res.Data["already_checked"] != true {
		t.Fatalf("want already-checked success, got %+v", res)
	}
	if _, called := g.seen[pathCampaignCheckin]; called {
		t.Fatal("the check-in POST was sent even though the board already said checked_in_today")
	}
}

// TestRunTaskClaimsByCampaignCode pins the claim body: campaign_code as
// campaign_item_code, empty task_id.
func TestRunTaskClaimsByCampaignCode(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)

	res, err := c.RunTask(context.Background(), "a1", "pc_task_complete_work_task_daily")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK {
		t.Fatalf("RunTask not OK: %+v", res)
	}
	var body map[string]any
	_ = json.Unmarshal(g.seen[pathCampaignClaim], &body)
	if body["campaign_item_code"] != "pc_task_complete_work_task_daily" {
		t.Fatalf("campaign_item_code = %v", body["campaign_item_code"])
	}
	if body["campaign_group_code"] != defaultCampaignGroup {
		t.Fatalf("campaign_group_code = %v", body["campaign_group_code"])
	}
	if body["task_id"] != "" {
		t.Fatalf("task_id = %v, want empty", body["task_id"])
	}
	if res.Credit != 50 {
		t.Fatalf("Credit = %d, want 50", res.Credit)
	}
}

// TestTasksListsTheBoard renders the check-in, chores and milestones, and marks
// the invite campaign as not automatable.
func TestTasksListsTheBoard(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)

	tasks, err := c.Tasks(context.Background(), "a1")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	byCode := map[string]core.TaskInfo{}
	for _, tk := range tasks {
		byCode[tk.Code] = tk
	}
	signin, ok := byCode["daily_signin"]
	if !ok {
		t.Fatal("the board is missing the daily_signin row")
	}
	if !signin.Claimable || signin.Claimed {
		t.Fatalf("signin row = %+v, want claimable and not claimed", signin)
	}
	if byCode["pc_task_complete_work_task_daily"].Credit != 50 {
		t.Fatalf("work-task credit = %d, want 50", byCode["pc_task_complete_work_task_daily"].Credit)
	}
	// A Completed chore is claimable; a Created one is not (it must be done in
	// the client first).
	if !byCode["pc_task_complete_work_task_daily"].Claimable {
		t.Fatalf("Completed chore = %+v, want claimable", byCode["pc_task_complete_work_task_daily"])
	}
	if pcx, ok := byCode["pc_task_generate_excel_v2"]; !ok || pcx.Claimable || pcx.Claimed {
		t.Fatalf("Created chore = %+v, want neither claimable nor claimed", pcx)
	}
	if inv, ok := byCode["invite_campaign"]; !ok || inv.Auto {
		t.Fatalf("invite campaign = %+v, want present and not auto", inv)
	}
}

// TestRunTaskRefusesAChoreNotDoneYet keeps the module from spending a claim on
// a chore the vendor will refuse: a Created work task must be done in the
// client first.
func TestRunTaskRefusesAChoreNotDoneYet(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)
	res, err := c.RunTask(context.Background(), "a1", "pc_task_generate_excel_v2")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "先在豆包电脑端完成") {
		t.Fatalf("Created chore = %+v, want a do-it-first refusal", res)
	}
	if _, called := g.seen[pathCampaignClaim]; called {
		t.Fatal("a claim was sent for a chore that is not done yet")
	}
}

// TestRunTaskRefusesAnUnknownCode keeps the module honest about what it serves.
func TestRunTaskRefusesAnUnknownCode(t *testing.T) {
	g := newGrowthSrv(t, v3Detail(false))
	c := newGrowthClient(t, g.URL)
	res, err := c.RunTask(context.Background(), "a1", "no_such_task")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "no task") {
		t.Fatalf("unknown code = %+v, want an honest refusal", res)
	}
}

// TestCheckinIsOfferedOnceAnAccountExists pins the capability gate.
func TestCheckinIsOfferedOnceAnAccountExists(t *testing.T) {
	c := newGrowthClient(t, "http://127.0.0.1:1")
	acts := c.CheckinActions(context.Background())
	if len(acts) != 1 || acts[0].ID != checkinActionDaily {
		t.Fatalf("CheckinActions = %+v, want one daily action", acts)
	}
}

// TestBoardFallsBackToThePreviousGroup covers the versioned group code: v3
// empty, v2 served.
func TestBoardFallsBackToThePreviousGroup(t *testing.T) {
	var groups []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathCampaignV3Detail {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		g, _ := body["campaign_group_code"].(string)
		groups = append(groups, g)
		if g == "pc_incentive_task_mvp_v2" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"campaign_code":"pc_incentive_task_mvp_v2","tasks":[],"milestones":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":null}`))
	}))
	defer srv.Close()
	c := newGrowthClient(t, srv.URL)

	board, err := c.campaignBoardFor(context.Background(), mustAccount(t, c, "a1"), true)
	if err != nil {
		t.Fatalf("campaignBoardFor: %v", err)
	}
	if board.Group != "pc_incentive_task_mvp_v2" {
		t.Fatalf("board group = %q, want the v2 fallback", board.Group)
	}
	if len(groups) != 2 || groups[0] != "pc_incentive_task_mvp_v3" {
		t.Fatalf("group probe order = %v, want v3 then v2", groups)
	}
}

// TestIncentiveQuotaIsParsedFromTheSummary keeps the 额度 meter wired to the
// balance row.
func TestIncentiveQuotaIsParsedFromTheSummary(t *testing.T) {
	var reply subscriptionQuotaReply
	raw := `{"data":{"incentive_quota_section":{"entitlement_count":4,"window_limit_groups":[{"feature_group":"doubao_activity_reward_quota","feature_group_name":"活动赠送额度","window_limits":[{"window_type":3,"used_percent":8,"end_time":1795427832209,"total_amount":"800","used_amount":"71"}]}]}}}`
	if err := json.Unmarshal([]byte(raw), &reply); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	q, ok := parseIncentiveQuota(reply)
	if !ok {
		t.Fatal("parseIncentiveQuota found nothing")
	}
	if q.Total != 800 || q.Used != 71 || q.Remaining != 729 {
		t.Fatalf("quota = %+v, want total 800 used 71 remaining 729", q)
	}
	if q.Label != "活动赠送额度" {
		t.Fatalf("label = %q", q.Label)
	}
}
