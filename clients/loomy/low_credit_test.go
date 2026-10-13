package loomy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestZeroBalanceParksTheAccountAndBlocksSoftProbes(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	bal, err := c.AccountBalance(context.Background(), "loomy-1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 0 {
		t.Fatalf("Credits = %d, want 0", bal.Credits)
	}

	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished")
	}
	if acc.selectable(time.Now().UTC()) {
		t.Fatal("a zero-balance account remained selectable")
	}
	if c.store.backgroundProbeDue("loomy-1", time.Now().UTC(), nil) {
		t.Fatal("a zero-balance account was handed to the soft background sweep")
	}

	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].State != "cooling" {
		t.Fatalf("records = %+v, want one cooling account", records)
	}
}

func TestCheckinRefreshesBalanceAndRevivesAZeroBalanceAccount(t *testing.T) {
	reads := 0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		switch r.URL.Path {
		case pointsPath:
			reads++
			if reads == 1 {
				return jsonResponse(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
			}
			return jsonResponse(http.StatusOK, okEnvelope(`{"balance":50,"availableBalance":50}`))
		case firstLogin:
			return jsonResponse(http.StatusOK, okEnvelope(
				`{"alreadyProcessed":false,"dailyQuota":50,"dailyBalance":50,"dailyConsumed":0}`))
		default:
			return jsonResponse(http.StatusOK, okEnvelope(`{}`))
		}
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance before check-in: %v", err)
	}
	before, _ := c.store.lookup("loomy-1")
	if before.selectable(time.Now().UTC()) {
		t.Fatal("the zero-balance account was still selectable before check-in")
	}

	res, err := c.Checkin(context.Background(), "loomy-1", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("Checkin failed: %+v", res)
	}

	after, _ := c.store.lookup("loomy-1")
	if !after.selectable(time.Now().UTC()) {
		t.Fatalf("the account did not recover after the signed-in balance update")
	}
	if reads != 2 {
		t.Fatalf("balance reads = %d, want one before and one after check-in", reads)
	}
}

func TestReserveCreditsMinusOneDisablesTheLoomyZeroBalanceGuard(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.ApplyPlatformPolicy(core.PlatformConfig{ReserveCredits: -1})

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	acc, _ := c.store.lookup("loomy-1")
	if !acc.selectable(time.Now().UTC()) {
		t.Fatal("reserve_credits=-1 did not disable the zero-balance guard")
	}
}

func TestZeroBalanceParkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
	c := newTestClientInDir(t, dir, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}

	reloaded := newTestClientInDir(t, filepath.Dir(c.store.path), "{}", nil)
	acc, ok := reloaded.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished across restart")
	}
	if acc.selectable(time.Now().UTC()) {
		t.Fatal("the zero-balance park did not survive restart")
	}
}

func TestCheckinThatLeavesZeroBalanceKeepsTheAccountParked(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		switch r.URL.Path {
		case pointsPath:
			return jsonResponse(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
		case firstLogin:
			return jsonResponse(http.StatusOK, okEnvelope(
				`{"alreadyProcessed":true,"dailyQuota":0,"dailyBalance":0,"dailyConsumed":0}`))
		default:
			return jsonResponse(http.StatusOK, okEnvelope(`{}`))
		}
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance before check-in: %v", err)
	}
	res, err := c.Checkin(context.Background(), "loomy-1", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("Checkin failed: %+v", res)
	}

	acc, _ := c.store.lookup("loomy-1")
	if acc.selectable(time.Now().UTC()) {
		t.Fatal("a check-in that did not fund the account revived it")
	}
	if c.store.backgroundProbeDue("loomy-1", time.Now().UTC(), nil) {
		t.Fatal("a still-empty account was handed to the soft background sweep")
	}
}

func TestAKeepaliveSuccessDoesNotClearAZeroBalancePark(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		switch r.URL.Path {
		case pointsPath:
			return jsonResponse(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
		default:
			return jsonResponse(http.StatusOK, okEnvelope(`{"userid":"123"}`))
		}
	}}
	c := newTestClient(t, `{"account_base":"https://account.example.test","api_key":"x"}`, rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if _, err := c.RunTask(context.Background(), "loomy-1", choreKeepalive); err != nil {
		t.Fatalf("keepalive: %v", err)
	}

	acc, _ := c.store.lookup("loomy-1")
	if acc.selectable(time.Now().UTC()) {
		t.Fatal("keepalive success cleared the zero-balance park")
	}
}

func TestASuccessfulModelProbeDoesNotClearAZeroBalancePark(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path == pointsPath {
			return jsonResponse(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
		}
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	res, err := c.TestAccount(context.Background(), "loomy-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want OK", res)
	}

	acc, _ := c.store.lookup("loomy-1")
	if acc.selectable(time.Now().UTC()) {
		t.Fatal("a successful model probe cleared the zero-balance park")
	}
}

func TestDisablingTheGuardClearsAnExistingZeroBalancePark(t *testing.T) {
	rt := alwaysJSON(http.StatusOK, okEnvelope(`{"balance":0,"availableBalance":0}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.AccountBalance(context.Background(), "loomy-1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	c.ApplyPlatformPolicy(core.PlatformConfig{ReserveCredits: -1})

	acc, _ := c.store.lookup("loomy-1")
	if !acc.selectable(time.Now().UTC()) {
		t.Fatal("disabling the guard left the account parked")
	}
}
