package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The vendor hands the daily window over without a claim and without a
// captcha: it decides from the account's liveness signal (see
// reportActivation) and answers billing/current with the grant.  A build that
// only ever read billing/preview reported "nothing to claim" while the account
// was holding one, which is what kept a funded credential parked as exhausted.
// These tests pin the two ends of that fix: the check-in leg must recognise a
// held grant, and the board must be able to show it.

// heldPlanFixture is the live shape of a granted window, taken from the
// vendor's own answer: awakened at 21:00, expiring the next morning, with the
// token grant hanging off the plan rather than off a balance row.
func heldPlanFixture(startsAt, endsAt int64) string {
	body := map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"server_time": startsAt,
			"plans": []any{map[string]any{
				"plan_id":   "zcode-v3-start-plan-1027",
				"name":      "ZCODE GLOBAL BUILD",
				"priority":  111,
				"status":    "active",
				"starts_at": startsAt,
				"ends_at":   endsAt,
				"entitlements": []any{map[string]any{
					"entitlement_id": "ent-1",
					"show_name":      "GLM-5.3-Flash",
					"meter":          "model_usage",
					"unit_type":      "token",
					"grant_units":    300000000,
					"period":         "one_time",
				}},
			}},
		},
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

// emptyPreviewRoutes is the state after a grant lands: nothing is offered for
// claiming because the vendor already gave the account its window.
func emptyPreviewRoutes(current string) map[string]func(*http.Request) (*http.Response, error) {
	return map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":0,"data":{"server_time":1,"plans":[]}}`), nil
		},
		planCurrentPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, current), nil
		},
	}
}

// TestCheckinRecognisesAHeldGrant drives the real path: the preview is empty,
// but the account holds a live window.  The action must report success without
// claiming anything -- there is nothing to click through -- and it must lift an
// exhausted verdict, because a live grant is proof the quota is back.
func TestCheckinRecognisesAHeldGrant(t *testing.T) {
	now := time.Now()
	current := heldPlanFixture(now.Add(-time.Hour).Unix(), now.Add(8*time.Hour).Unix())

	// A client with no solver on purpose: a held grant must not need one, and
	// installing one here would weaken the assertion to "a solver was
	// configured".
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, emptyPreviewRoutes(current))
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
		t.Fatalf("res = %+v, want a successful result for a held grant", res)
	}
	if got, _ := res.Data["plan_name"].(string); got != "ZCODE GLOBAL BUILD" {
		t.Errorf("plan_name = %v, want the held plan named", res.Data["plan_name"])
	}
	if pathCount(ft, planClaimPath) != 0 {
		t.Error("a held grant must not post a claim")
	}

	acct := c.pool.find(id)
	if acct == nil {
		t.Fatal("account vanished")
	}
	if acct.State != stateReady {
		t.Errorf("state after a held grant = %q, want %q", acct.State, stateReady)
	}
	if acct.LastError != "" {
		t.Errorf("a cleared verdict left an error behind: %q", acct.LastError)
	}
}

// TestCheckinKeepsAnExpiredGrantParked is the other side of the same rule.  A
// plan whose status still says active after its window closed is over, and
// treating it as live would put an account back into rotation on quota it can
// no longer spend.
func TestCheckinKeepsAnExpiredGrantParked(t *testing.T) {
	now := time.Now()
	current := heldPlanFixture(now.Add(-10*time.Hour).Unix(), now.Add(-time.Minute).Unix())

	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, emptyPreviewRoutes(current)))
	id := addJWTAccount(t, c)

	c.pool.mark(id, func(a *Account) {
		a.State = stateExhausted
		a.LastError = "exhausted"
	})

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.OK {
		t.Fatalf("res = %+v, want an expired grant to stay unavailable", res)
	}
	if acct := c.pool.find(id); acct == nil || acct.State != stateExhausted {
		t.Fatalf("an expired grant revived the account: %+v", acct)
	}
}

// TestTasksShowsAHeldGrantAndItsExpiry covers the board.  The row has to name
// the plan and the expiry, and it must not offer a button: the vendor granted
// this one, so there is nothing for an operator to press.
func TestTasksShowsAHeldGrantAndItsExpiry(t *testing.T) {
	now := time.Now()
	starts := now.Add(-time.Hour).Truncate(time.Second)
	ends := now.Add(8 * time.Hour).Truncate(time.Second)
	current := heldPlanFixture(starts.Unix(), ends.Unix())

	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, emptyPreviewRoutes(current)))
	id := addJWTAccount(t, c)

	tasks, err := c.Tasks(context.Background(), id)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want one row", tasks)
	}
	row := tasks[0]
	if !row.Claimed || !row.Locked {
		t.Errorf("row = %+v, want a claimed locked row", row)
	}
	if row.Claimable || row.Auto {
		t.Errorf("a held grant must offer no button: %+v", row)
	}
	if row.Desc != "ZCODE GLOBAL BUILD" {
		t.Errorf("Desc = %q, want the held plan named", row.Desc)
	}
	// The expiry is what an operator needs from a window that closes itself.
	until := ends.Local().Format("01-02 15:04")
	if !strings.Contains(row.Note, until) {
		t.Errorf("Note = %q, want the expiry %q in it", row.Note, until)
	}
}
