package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

func TestAccountPackagesSplitsSubscriptionAndAddOn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathQuota {
			t.Fatalf("path = %q, want %q", r.URL.Path, pathQuota)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"expiresAt":1760000000000,
			"userQuota":{"total":1000,"used":400,"remaining":600,"unit":"credits"},
			"addOnQuota":{"total":100,"used":20,"remaining":80,"unit":"credits"}
		}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
	})
	report, err := c.AccountPackages(context.Background(), "a1")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if report.Remain != 680 || report.Size != 1100 {
		t.Fatalf("report = %#v, want remain=680 size=1100", report)
	}
	if len(report.Packages) != 2 {
		t.Fatalf("packages = %#v, want two tranches", report.Packages)
	}
	if report.Packages[0].Name != "订阅额度" || report.Packages[0].Remain != 600 || report.Packages[0].Size != 1000 {
		t.Fatalf("subscription tranche = %#v", report.Packages[0])
	}
	if report.Packages[1].Name != "活动加赠" || report.Packages[1].Remain != 80 || report.Packages[1].Size != 100 {
		t.Fatalf("add-on tranche = %#v", report.Packages[1])
	}
	if report.Packages[0].EndTime == "" {
		t.Fatalf("subscription expiry was not carried through")
	}
}

func TestAccountPackagesRejectsUnknownAccount(t *testing.T) {
	c := testClient(t, defaultOpenAPIBase)
	if _, err := c.AccountPackages(context.Background(), "missing"); err == nil {
		t.Fatal("unknown account did not fail")
	}
}

func TestTasksMapsCampaignsToBoard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathCampaigns {
			t.Fatalf("path = %q, want %q", r.URL.Path, pathCampaigns)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"campaigns":[
			{"campaignId":"c1","title":"每日签到","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}},
			{"campaignId":"c2","title":"已领活动","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":50}},
			{"campaignId":"c3","title":"查看详情","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}
		]}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
	})
	rows, err := c.Tasks(context.Background(), "a1")
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	byCode := map[string]core.TaskInfo{}
	for _, row := range rows {
		byCode[row.Code] = row
	}
	if got := byCode["c1"]; !got.Claimable || !got.Auto || got.Claimed || got.Credit != 100 {
		t.Fatalf("c1 = %#v", got)
	}
	if got := byCode["c2"]; !got.Claimed || got.Claimable || got.Auto {
		t.Fatalf("c2 = %#v", got)
	}
	if got := byCode["c3"]; got.Auto || !got.Locked || got.Claimable {
		t.Fatalf("c3 = %#v", got)
	}
}

func TestClaimTaskPostsCampaignClaim(t *testing.T) {
	var posts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts = append(posts, r.URL.EscapedPath())
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		_, _ = w.Write([]byte(`{"campaigns":[
			{"campaignId":"c1","title":"每日签到","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}}
		]}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
	})
	claim, err := c.ClaimTask(context.Background(), "a1", "c1")
	if err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	if claim.Credit != 100 {
		t.Fatalf("claim = %#v, want 100 credits", claim)
	}
	if len(posts) != 1 || posts[0] != "/sash/api/v1/me/campaigns/c1/claim" {
		t.Fatalf("posts = %#v", posts)
	}
}

func TestRunTaskAutoClaimsOnlyClaimableCampaigns(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts++
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		_, _ = w.Write([]byte(`{"campaigns":[
			{"campaignId":"c1","title":"每日签到","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"amount":100}},
			{"campaignId":"c3","title":"查看详情","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"}
		]}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
	})

	res, err := c.RunTaskAuto(context.Background(), "a1", "c1")
	if err != nil {
		t.Fatalf("RunTaskAuto: %v", err)
	}
	if !res.OK || !res.Claimed || !res.Attempt || res.Credit != 100 {
		t.Fatalf("res = %#v", res)
	}

	if _, err := c.RunTaskAuto(context.Background(), "a1", "c3"); err == nil {
		t.Fatal("non-claimable campaign did not fail")
	}
	if posts != 1 {
		t.Fatalf("POST count = %d, want 1", posts)
	}
}

func TestRunTaskReportsAlreadyClaimedWithoutPosting(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			posts++
		}
		_, _ = w.Write([]byte(`{"campaigns":[
			{"campaignId":"c2","title":"已领活动","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"amount":50}}
		]}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
	})
	res, err := c.RunTask(context.Background(), "a1", "c2")
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if !res.OK || !res.Skipped {
		t.Fatalf("res = %#v, want OK and skipped", res)
	}
	if posts != 0 {
		t.Fatalf("POST count = %d, want 0", posts)
	}
}

func TestModelMaxOutputTokensReadsCachedCatalogue(t *testing.T) {
	c := testClient(t, defaultOpenAPIBase)
	c.models = catalogue([]modelInfo{{ID: "qmodel", DisplayName: "Qwen", MaxOutput: 8192}})
	got, ok := c.ModelMaxOutputTokens(context.Background(), "qmodel")
	if !ok || got != 8192 {
		t.Fatalf("limit = %d, %v; want 8192, true", got, ok)
	}
}

func TestTaskAccountPrefersSelectableCredential(t *testing.T) {
	c := testClient(t, defaultOpenAPIBase,
		account{storedAccount: storedAccount{ID: "parked", Token: "t1", Enabled: false}},
		account{storedAccount: storedAccount{ID: "ready", Token: "t2", Enabled: true}},
	)
	acc, ok, err := c.taskAccount("")
	if err != nil || !ok {
		t.Fatalf("taskAccount: %#v, %v, %v", acc, ok, err)
	}
	if acc.ID != "ready" {
		t.Fatalf("account = %q, want ready", acc.ID)
	}
}

func TestCampaignsToTasksSkipsRowsWithoutAnID(t *testing.T) {
	rows := campaignsToTasks(&campaignsResponse{Campaigns: []campaign{
		{Title: "no id", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaim},
		{CampaignID: "c1", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaim},
	}})
	if len(rows) != 1 || rows[0].Code != "c1" {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].Note == "" {
		t.Fatalf("claimable row note = %q", rows[0].Note)
	}
}
