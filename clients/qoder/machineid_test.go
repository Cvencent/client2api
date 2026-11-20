package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"client2api/internal/core"
)

// stubUMID replaces the vendor executable for one test.  The real binary is a
// Windows PE image, so a test cannot ship one; this only exercises the module's
// plumbing around the call.
func stubUMID(t *testing.T, identities map[string]machineIdentity) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "runtime-info.exe")
	if err := os.WriteFile(bin, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	t.Setenv(umidPathEnv, bin)

	prev := runUMID
	runUMID = func(_ context.Context, _, env string) ([]byte, error) {
		id, ok := identities[env]
		if !ok {
			return nil, os.ErrNotExist
		}
		return json.Marshal(id)
	}
	t.Cleanup(func() { runUMID = prev })
}

// identityForTest mints one realm's identity through a fresh cache entry.  The
// data directory doubles as the cache key, so each test gets its own.
func identityForTest(t *testing.T, c *Client, realm string) (machineIdentity, error) {
	t.Helper()
	c.deps.DataDir = t.TempDir()
	return c.machineIdentityFor(context.Background(), realm)
}

// TestMachineIdentityMintsPerRealm pins the realm/environment mapping and that
// the minted pair is what the campaign routes require.
func TestMachineIdentityMintsPerRealm(t *testing.T) {
	stubUMID(t, map[string]machineIdentity{
		"0": {Token: "cn-token", Type: "cn-type", Code: "cn-code"},
		"4": {Token: "global-token", Type: "global-type", Code: "global-code"},
	})
	c := &Client{}

	global, err := identityForTest(t, c, realmGlobal)
	if err != nil {
		t.Fatalf("global identity: %v", err)
	}
	if global.Token != "global-token" || global.Type != "global-type" || global.Code != "global-code" {
		t.Fatalf("global identity = %#v", global)
	}
	cn, err := identityForTest(t, c, realmCN)
	if err != nil {
		t.Fatalf("cn identity: %v", err)
	}
	if cn.Token != "cn-token" || cn.Type != "cn-type" {
		t.Fatalf("cn identity = %#v", cn)
	}
	if !global.present() || !cn.present() {
		t.Fatal("a minted identity must carry both halves of the pair")
	}
}

// TestMachineIdentityReusesTheCache pins that a panel draw does not spawn the
// vendor's binary again.
func TestMachineIdentityReusesTheCache(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "runtime-info.exe")
	if err := os.WriteFile(bin, []byte("stub"), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	t.Setenv(umidPathEnv, bin)

	var runs int
	prev := runUMID
	runUMID = func(context.Context, string, string) ([]byte, error) {
		runs++
		return []byte(`{"machineToken":"t","machineType":"y"}`), nil
	}
	t.Cleanup(func() { runUMID = prev })

	c := &Client{deps: core.Deps{DataDir: t.TempDir()}}
	for i := 0; i < 3; i++ {
		if _, err := c.machineIdentityFor(context.Background(), realmGlobal); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	if runs != 1 {
		t.Fatalf("the fingerprint binary ran %d times, want 1", runs)
	}
}

// TestCampaignRoutesSendTheFingerprint pins the actual fix: the list and the
// claim must carry the UMID pair, because the vendor only answers them with the
// real campaign set (the daily claim included) when they do.
func TestCampaignRoutesSendTheFingerprint(t *testing.T) {
	var gotList, gotClaim http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			gotClaim = r.Header.Clone()
		} else {
			gotList = r.Header.Clone()
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"status":"CLAIMED","reward":{"type":"CREDITS","amount":100}}`))
			return
		}
		_, _ = w.Write([]byte(`{"claimable":true,"campaigns":[]}`))
	}))
	defer server.Close()

	u := newUpstream(config{OpenAPIBase: server.URL, GlobalOpenAPIBase: server.URL}, server.Client(), nil)
	u.machine = func(context.Context, string) (machineIdentity, error) {
		return machineIdentity{Token: "fp-token", Type: "fp-type", Code: "fp-code"}, nil
	}
	u.machineID = func() string { return "device-uuid" }

	if _, err := u.campaignsFor(context.Background(), realmGlobal, "device-token"); err != nil {
		t.Fatalf("campaignsFor: %v", err)
	}
	if _, err := u.claimCampaignFor(context.Background(), realmGlobal, "device-token", "c1"); err != nil {
		t.Fatalf("claimCampaignFor: %v", err)
	}
	for label, h := range map[string]http.Header{"list": gotList, "claim": gotClaim} {
		if got := h.Get("Cosy-MachineToken"); got != "fp-token" {
			t.Fatalf("%s Cosy-MachineToken = %q", label, got)
		}
		if got := h.Get("Cosy-MachineType"); got != "fp-type" {
			t.Fatalf("%s Cosy-MachineType = %q", label, got)
		}
		if got := h.Get("Cosy-MachineCode"); got != "fp-code" {
			t.Fatalf("%s Cosy-MachineCode = %q", label, got)
		}
		if got := h.Get("Cosy-MachineId"); got != "device-uuid" {
			t.Fatalf("%s Cosy-MachineId = %q", label, got)
		}
		if got := h.Get("Cosy-MachineOS"); got == "" {
			t.Fatalf("%s is missing Cosy-MachineOS", label)
		}
	}
}

// TestPlainRoutesStayUngated pins the other half: the balance and identity reads
// keep the plain shape.  Sending a fingerprint there would work, but it is not
// required and it would make every read depend on an install being present.
func TestPlainRoutesStayUngated(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"u1","name":"n"}`))
	}))
	defer server.Close()

	u := newUpstream(config{OpenAPIBase: server.URL, GlobalOpenAPIBase: server.URL}, server.Client(), nil)
	u.machine = func(context.Context, string) (machineIdentity, error) {
		return machineIdentity{Token: "fp-token", Type: "fp-type"}, nil
	}
	if _, err := u.userInfoFor(context.Background(), realmGlobal, "device-token"); err != nil {
		t.Fatalf("userInfoFor: %v", err)
	}
	if got.Get("Cosy-MachineToken") != "" {
		t.Fatalf("userinfo carried a fingerprint: %q", got.Get("Cosy-MachineToken"))
	}
}

// TestCampaignRouteSurvivesAMissingFingerprint pins the degradation: a host with
// no install still reaches the vendor and gets whatever it answers, rather than
// failing locally.  That keeps a no-install gateway exactly as capable as it was.
func TestCampaignRouteSurvivesAMissingFingerprint(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claimable":false,"campaigns":[]}`))
	}))
	defer server.Close()

	var logs []string
	u := newUpstream(config{OpenAPIBase: server.URL, GlobalOpenAPIBase: server.URL}, server.Client(), func(f string, a ...any) {
		logs = append(logs, f)
	})
	u.machine = func(context.Context, string) (machineIdentity, error) {
		return machineIdentity{}, os.ErrNotExist
	}
	if _, err := u.campaignsFor(context.Background(), realmGlobal, "device-token"); err != nil {
		t.Fatalf("campaignsFor must not fail without a fingerprint: %v", err)
	}
	if got.Get("Cosy-MachineToken") != "" {
		t.Fatal("a missing fingerprint must not be invented")
	}
	if len(logs) == 0 {
		t.Fatal("a missing fingerprint was not logged")
	}
}

// TestSplitCampaignsIgnoresMultiDayPacks pins the misreport fix: a week-long
// trial pack that is already CLAIMED is not evidence that today's daily reward
// was collected.
func TestSplitCampaignsIgnoresMultiDayPacks(t *testing.T) {
	const day = 86400
	list := &campaignsResponse{Campaigns: []campaign{
		{CampaignID: "trial", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaimed,
			StartAt: 1000, EndAt: 1000 + 7*day},
		{CampaignID: "daily", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaimed,
			StartAt: 1000, EndAt: 1000 + day - 1},
	}}
	_, claimed := splitCampaigns(list)
	if claimed != 1 {
		t.Fatalf("alreadyClaimed = %d, want 1 (the daily window only)", claimed)
	}
}

// TestCheckinSaysNoDailyRewardWhenOnlyTrialPacksAreClaimed pins the operator
// message: with nothing claimable and only a trial pack claimed, the module must
// not claim "今天已领取".
func TestCheckinSaysNoDailyRewardWhenOnlyTrialPacksAreClaimed(t *testing.T) {
	const day = 86400
	body := `{"claimable":false,"campaigns":[{"campaignId":"trial","campaignKey":"act-20261112-658",` +
		`"actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","startAt":1000,"endAt":` +
		strconv.Itoa(1000+7*day) + `,"benefit":{"kind":"CREDITS","amount":2000}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{storedAccount: storedAccount{ID: "a1", Token: "t", Enabled: true}})
	res, err := c.Checkin(context.Background(), "a1", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if res.Message == "今天的奖励已经领取过了" {
		t.Fatal("a claimed trial pack was reported as today's reward")
	}
	if !res.Skipped {
		t.Fatalf("result = %#v, want skipped (nothing daily was claimable)", res)
	}
}
