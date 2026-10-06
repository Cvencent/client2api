package zcode

import (
	"context"
	"net/http"
	"testing"
)

// "Already claimed" is the vendor telling the module the account holds the
// plan, so it is just as much evidence that quota is back as a fresh grant: an
// account parked with a stale quota verdict must come back at the next sweep
// instead of staying out of rotation while it still holds the quota the
// operator already claimed.  This is the exact "已经领了 1 亿，状态还是冷却中"
// report.
func TestCheckinClearsAStaleQuotaVerdictWhenThePlanWasAlreadyClaimed(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		planPreviewPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, claimPreviewFixture), nil
		},
		planClaimPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"code":1003,"msg":"already claimed"}`), nil
		},
	}))
	c.cfg.CaptchaRegion = "cn"
	id := addJWTAccount(t, c)
	installSolver(t, c, "token-from-solver")

	c.pool.mark(id, func(a *Account) {
		a.State = stateExhausted
		a.LastError = "HTTP 200 code 1005: exceed quota limit"
	})

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want already-claimed to read as success", res)
	}
	if rec := recordsByID(t, c)[id]; rec.State != stateReady {
		t.Errorf("state after an already-claimed sweep = %q, want %q", rec.State, stateReady)
	}
}
