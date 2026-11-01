package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The marketing channel is how the vendor's popup reaches an account: a
// delivery query names a campaign, a confirmation on that campaign is what
// makes the advertised plan claimable, and the billing claim does the rest.
// These tests pin the unattended version of that flow -- the same three steps
// without a client, without a popup, and without an operator pressing anything.

// marketingDeliveryFixture renders one WEEKEND-style popup delivery whose
// primary button claims the named plan.
func marketingDeliveryFixture(campaignID, planID string) string {
	body := map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"server_time": 1,
			"language":    "zh-CN",
			"deliveries": []any{map[string]any{
				"campaign_id":       campaignID,
				"priority":          10,
				"resource_position": "popup",
				"popup": map[string]any{
					"title": map[string]any{"format": "plaintext", "content": "ZCODE WEEKEND BUILD"},
					"buttons": []any{
						map[string]any{
							"text":   map[string]any{"format": "plaintext", "content": "领取"},
							"action": map[string]any{"type": "claim_zcode_plan", "args": map[string]any{"plan_id": planID}},
						},
						map[string]any{
							"text":   map[string]any{"format": "plaintext", "content": "关闭"},
							"action": map[string]any{"type": "close"},
						},
					},
				},
			}},
		},
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// marketingRoutes routes an empty preview through a pending WEEKEND delivery,
// a successful confirmation and a successful claim: the full unattended path.
func marketingRoutes(t *testing.T, campaignID, planID string, confirmRefused bool) map[string]func(*http.Request) (*http.Response, error) {
	t.Helper()
	confirmCode := 0
	if confirmRefused {
		confirmCode = 1003 // "already confirmed" is what a second sweep sees
	}
	return map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[],"balances":[]}}`), nil
		},
		planCurrentPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
		marketingTouchPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, marketingDeliveryFixture(campaignID, planID)), nil
		},
		marketingActionPath: func(r *http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":`+itoa(confirmCode)+`,"msg":""}`), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"plan":{"starts_at":1,"ends_at":2}}}`), nil
		},
	}
}

// TestCheckinClaimsThroughTheMarketingChannel drives the whole popup flow
// unattended: empty preview, a pending delivery advertising a plan, the
// confirmation, and the claim the confirmation unlocked.  The result must read
// as success and lift an exhausted verdict -- a fresh grant is proof the quota
// is back, whatever channel delivered it.
func TestCheckinClaimsThroughTheMarketingChannel(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, marketingRoutes(t, "wk-campaign-1", "zcode-v3-start-plan-1031-wk", false))
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	c.pool.mark(id, func(a *Account) {
		a.State = stateExhausted
		a.LastError = "HTTP 200 code 1005: exceed quota limit"
	})

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || res.Skipped {
		t.Fatalf("res = %+v, want the campaign claim to succeed", res)
	}
	if got, _ := res.Data["plan_id"].(string); got != "zcode-v3-start-plan-1031-wk" {
		t.Errorf("plan_id = %v, want the delivery's plan", res.Data["plan_id"])
	}

	// The wire order is the flow: query, confirm, claim.  The confirm body
	// carries the campaign id and the literal "confirm" -- an unattended loop
	// never declines on the operator's behalf.
	var confirmBody struct {
		CampaignID string `json:"campaign_id"`
		ActionType string `json:"action_type"`
	}
	found := false
	for i := 0; i < ft.count(); i++ {
		if !strings.Contains(ft.requestAt(i).URL.Path, marketingActionPath) {
			continue
		}
		if err := json.Unmarshal([]byte(ft.bodyAt(i)), &confirmBody); err != nil {
			t.Fatalf("confirm body %d is not JSON: %v", i, err)
		}
		found = true
	}
	if !found {
		t.Fatal("no confirmation reached the vendor")
	}
	if confirmBody.CampaignID != "wk-campaign-1" || confirmBody.ActionType != "confirm" {
		t.Fatalf("confirm body = %+v, want the campaign id and a literal confirm", confirmBody)
	}
	if pathCount(ft, planClaimPath) != 1 {
		t.Fatalf("claim count = %d, want exactly one claim after the confirmation", pathCount(ft, planClaimPath))
	}

	if acct := c.pool.find(id); acct == nil || acct.State != stateReady {
		t.Fatalf("a campaign claim did not revive the account: %+v", acct)
	}
}

// TestCheckinLeavesNonPlanCampaignsAlone is the consent boundary.  A delivery
// whose buttons never advertise a plan claim is the vendor asking the operator
// something an unattended loop has no authority to answer, so the sweep
// confirms nothing and reports the steady "nothing is claimable".
func TestCheckinLeavesNonPlanCampaignsAlone(t *testing.T) {
	routes := marketingRoutes(t, "banner-ad", "zcode-v3-start-plan-1031-wk", false)
	routes[marketingTouchPath] = func(*http.Request) (*http.Response, error) {
		// Same shape, but the only action opens a URL -- a plan-less campaign.
		body := map[string]any{
			"code": 0, "msg": "",
			"data": map[string]any{
				"server_time": 1, "language": "zh-CN",
				"deliveries": []any{map[string]any{
					"campaign_id":       "banner-ad",
					"priority":          10,
					"resource_position": "popup",
					"popup": map[string]any{
						"buttons": []any{map[string]any{
							"text":   map[string]any{"format": "plaintext", "content": "查看"},
							"action": map[string]any{"type": "open_url", "args": map[string]any{"url": "https://zcode.z.ai"}},
						}},
					},
				}},
			},
		}
		raw, _ := json.Marshal(body)
		return jsonResponse(http.StatusOK, string(raw)), nil
	}

	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, routes)
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK || !res.Skipped {
		t.Fatalf("res = %+v, want a plan-less campaign to stay unclaimed", res)
	}
	if pathCount(ft, marketingActionPath) != 0 {
		t.Error("a plan-less campaign was confirmed; consent belongs to the operator")
	}
	if pathCount(ft, planClaimPath) != 0 {
		t.Error("a claim went out without a confirmation")
	}
}

// TestCheckinMarketingSteadyStateAfterAClaim is the already-claimed sweep: the
// confirmation answers 1003 and the claim is not retried.  The flow must read
// the steady state as done, not as a refusal, and clear a stale quota verdict
// exactly like a direct already-claimed answer does.
func TestCheckinMarketingSteadyStateAfterAClaim(t *testing.T) {
	// This run's delivery advertises a plan, the confirmation is refused with
	// 1003 (already confirmed earlier today), and the billing claim answers
	// 1003 for the same reason.  Every leg is the vendor saying "settled".
	routes := marketingRoutes(t, "wk-campaign-1", "zcode-v3-start-plan-1031-wk", true)
	routes[planClaimPath] = func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
	}

	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, routes))
	id := addJWTAccount(t, c)

	c.pool.mark(id, func(a *Account) {
		a.State = stateExhausted
		a.LastError = "HTTP 200 code 1005: exceed quota limit"
	})

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want an already-claimed campaign to read as success", res)
	}
	if acct := c.pool.find(id); acct == nil || acct.State != stateReady {
		t.Fatalf("the steady state did not revive the account: %+v", acct)
	}
}

// TestHeldPlanWindowReadsBalanceFirst pins the endpoint authority: the current
// endpoint began answering an empty envelope for accounts that hold a plan
// (measured 2026-10-30), while the balance document still names it -- the same
// document the desktop client reads for its own hasActiveStartPlan flag.  A
// balance-only answer must therefore be enough.
func TestHeldPlanWindowReadsBalanceFirst(t *testing.T) {
	now := time.Now()
	balance := heldPlanFixture(now.Add(-time.Hour).Unix(), now.Add(8*time.Hour).Unix())

	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planBalancePath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, balance), nil
		},
		planCurrentPath: func(*http.Request) (*http.Response, error) {
			// The live shape since the weekend plan: code 0, empty envelope.
			return jsonResponse(http.StatusOK, `{"code":0,"msg":"","logid":"x"}`), nil
		},
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
	})
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	acct := c.pool.find(id)
	if acct == nil {
		t.Fatal("account vanished")
	}
	if _, ok := c.heldPlanWindow(context.Background(), acct); !ok {
		t.Fatal("heldPlanWindow = false, want the balance document to name the held plan")
	}
	if n := pathCount(ft, planBalancePath); n == 0 {
		t.Error("the balance document was never read")
	}
}

// TestCheckinHeldGrantSurvivesAnEmptyCurrent is the check-in leg of the same
// fix: an account holding a live plan whose current read is empty must still
// settle as done instead of "nothing is claimable", which is exactly what kept
// the funded weekend account parked as exhausted on 2026-10-30.
func TestCheckinHeldGrantSurvivesAnEmptyCurrent(t *testing.T) {
	now := time.Now()
	balance := heldPlanFixture(now.Add(-time.Hour).Unix(), now.Add(8*time.Hour).Unix())

	routes := emptyPreviewRoutes(`{"code":0,"msg":"","logid":"x"}`)
	routes[planBalancePath] = func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, balance), nil
	}

	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, routes))
	id := addJWTAccount(t, c)

	c.pool.mark(id, func(a *Account) {
		a.State = stateExhausted
		a.LastError = "HTTP 200 code 1005: exceed quota limit"
	})

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || res.Skipped {
		t.Fatalf("res = %+v, want the balance-named grant to settle the check-in", res)
	}
	if acct := c.pool.find(id); acct == nil || acct.State != stateReady {
		t.Fatalf("state = %+v, want revived", acct)
	}
}
