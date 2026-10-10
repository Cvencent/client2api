package qoder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
)

func testClient(t *testing.T, base string, accounts ...account) *Client {
	t.Helper()
	cfg := config{OpenAPIBase: base}
	u := newUpstream(cfg, http.DefaultClient, nil)
	s := &store{all: make([]*account, 0, len(accounts))}
	for i := range accounts {
		acc := accounts[i]
		s.all = append(s.all, &acc)
	}
	return &Client{cfg: cfg, up: u, store: s}
}

func TestClaimCampaignPostsToCampaignClaimRoute(t *testing.T) {
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
	}))
	defer server.Close()

	u := newUpstream(config{OpenAPIBase: server.URL}, server.Client(), nil)
	status, err := u.claimCampaign(context.Background(), "device-token", "daily/one")
	if err != nil {
		t.Fatalf("claimCampaign: %v", err)
	}
	if status != campaignStatusClaimed {
		t.Fatalf("status = %q, want %q", status, campaignStatusClaimed)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	wantPath := "/sash/api/v1/me/campaigns/daily%2Fone/claim"
	if gotPath != wantPath {
		t.Fatalf("path = %q, want %q", gotPath, wantPath)
	}
}

func TestSplitCampaignsOnlyReturnsClaimBenefits(t *testing.T) {
	list := &campaignsResponse{Campaigns: []campaign{
		{CampaignID: "one", CampaignKey: "daily", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaim},
		{CampaignID: "two", CampaignKey: "daily", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaimed},
		{CampaignID: "three", CampaignKey: "detail", ActionType: "VIEW_DETAILS", ClaimStatus: campaignStatusClaim},
	}}

	claimable, alreadyClaimed := splitCampaigns(list)
	if len(claimable) != 1 || claimable[0].CampaignID != "one" {
		t.Fatalf("claimable = %#v, want only campaign one", claimable)
	}
	if alreadyClaimed != 1 {
		t.Fatalf("alreadyClaimed = %d, want 1", alreadyClaimed)
	}
}

func TestCheckinReportsAlreadyClaimedWithoutPosting(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"daily","campaignKey":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"CREDIT","amount":100}}]}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
		cooldownTill:  time.Now().Add(time.Hour),
		lastError:     "stale failure",
	})
	res, err := c.Checkin(context.Background(), "a1", checkinActionDaily)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK || res.Skipped {
		t.Fatalf("result = %#v, want OK and not skipped", res)
	}
	if posts != 0 {
		t.Fatalf("POST count = %d, want 0", posts)
	}
	if got := res.Data["already_claimed"]; got != 1 {
		t.Fatalf("already_claimed = %#v, want 1", got)
	}
	got, ok := c.store.lookup("a1")
	if !ok {
		t.Fatal("account disappeared")
	}
	if !got.cooldownTill.IsZero() || got.lastError != "" {
		t.Fatalf("successful campaign read left penalties in place: %#v", got)
	}
}

func TestAccountBalanceSumsUserAndAddOnPools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathQuota {
			t.Fatalf("path = %q, want %q", r.URL.Path, pathQuota)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"userQuota":{"total":1000,"used":400,"remaining":600,"unit":"credits"},
			"addOnQuota":{"total":100,"used":20,"remaining":80,"unit":"credits"}
		}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL, account{storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true}})
	bal, err := c.AccountBalance(context.Background(), "a1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 680 || bal.Total != 1100 || bal.Used != 420 {
		t.Fatalf("balance = %#v, want credits=680 total=1100 used=420", bal)
	}
	if bal.Unit != "credits" {
		t.Fatalf("unit = %q, want credits", bal.Unit)
	}
}

func TestAccountBalanceSuccessClearsPenaltiesWithoutChangingLRU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userQuota":{"remaining":10},"addOnQuota":{"remaining":20}}`))
	}))
	defer server.Close()

	lastUsed := time.Now().Add(-time.Hour).UTC()
	c := testClient(t, server.URL, account{
		storedAccount: storedAccount{ID: "a1", Token: "token", Enabled: true},
		dead:          true,
		cooldownTill:  time.Now().Add(time.Hour),
		lastError:     "stale failure",
		lastUsed:      lastUsed,
	})
	if _, err := c.AccountBalance(context.Background(), "a1", 0); err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	got, ok := c.store.lookup("a1")
	if !ok {
		t.Fatal("account disappeared")
	}
	if got.dead || !got.cooldownTill.IsZero() || got.lastError != "" {
		t.Fatalf("successful balance read left penalties in place: %#v", got)
	}
	if !got.lastUsed.Equal(lastUsed) {
		t.Fatalf("balance read changed LRU timestamp: got %s want %s", got.lastUsed, lastUsed)
	}
}

func TestAddAccountUsesVendorUserIDForStableAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathUserInfo {
			t.Fatalf("path = %q, want %q", r.URL.Path, pathUserInfo)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"user-123","name":"测试用户","security_mobile":"13800000000"}`))
	}))
	defer server.Close()

	c := testClient(t, server.URL)
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"token": "device-token"},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if rec.ID != "qoder-user-123" {
		t.Fatalf("id = %q, want qoder-user-123", rec.ID)
	}
	if rec.Identity != "user-123" {
		t.Fatalf("identity = %q, want user-123", rec.Identity)
	}
}

func TestHasImportedPathMatchesOnlyItsOrigin(t *testing.T) {
	c := testClient(t, defaultOpenAPIBase, account{storedAccount: storedAccount{
		ID:         "a1",
		Token:      "token",
		Enabled:    true,
		ImportPath: `C:\Users\me\AppData\Roaming\com.qodercn.app.stable\auth.v1.dat`,
	}})

	if !c.hasImportedPath(`C:\Users\me\AppData\Roaming\com.qodercn.app.stable\auth.v1.dat`) {
		t.Fatal("matching import path was not recognised")
	}
	if c.hasImportedPath(`C:\Users\other\AppData\Roaming\com.qodercn.app.stable\auth.v1.dat`) {
		t.Fatal("unrelated import path was reported as imported")
	}
}

func TestStorePutClearsPenaltiesWhenTokenChanges(t *testing.T) {
	old := account{
		storedAccount: storedAccount{ID: "a1", Token: "old-token", Enabled: true},
		dead:          true,
		cooldownTill:  time.Now().Add(time.Hour),
		lastError:     "old failure",
	}
	s := &store{all: []*account{&old}}

	if err := s.put(account{storedAccount: storedAccount{ID: "a1", Token: "new-token", Enabled: true}}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := s.lookup("a1")
	if !ok {
		t.Fatal("account was not stored")
	}
	if got.dead || !got.cooldownTill.IsZero() || got.lastError != "" {
		t.Fatalf("runtime penalties survived a token change: %#v", got)
	}
}

func TestDecryptSafeStorageBlobAcceptsV10GCM(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	nonce := []byte("nonce-123456")
	plain := []byte(`{"schemaVersion":1,"token":"dt-abc"}`)
	blob, err := encryptSafeStorageForTest(key, nonce, plain)
	if err != nil {
		t.Fatalf("encrypt fixture: %v", err)
	}

	got, err := decryptSafeStorageBlob(blob, key)
	if err != nil {
		t.Fatalf("decryptSafeStorageBlob: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("plaintext = %q, want %q", got, plain)
	}
}

func TestDecryptSafeStorageBlobRejectsTampering(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	blob, err := encryptSafeStorageForTest(key, []byte("nonce-123456"), []byte("payload"))
	if err != nil {
		t.Fatalf("encrypt fixture: %v", err)
	}
	blob[len(blob)-1] ^= 0xff

	if _, err := decryptSafeStorageBlob(blob, key); err == nil {
		t.Fatal("tampered blob decrypted successfully")
	}
}

func TestSafeStorageKeyFromLocalStateUnwrapsDPAPIKey(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	state, err := json.Marshal(map[string]any{
		"os_crypt": map[string]string{
			"encrypted_key": base64.StdEncoding.EncodeToString(append([]byte("DPAPI"), []byte("wrapped")...)),
		},
	})
	if err != nil {
		t.Fatalf("marshal local state: %v", err)
	}

	var wrapped []byte
	got, err := safeStorageKeyFromLocalState(state, func(in []byte) ([]byte, error) {
		wrapped = append([]byte(nil), in...)
		return key, nil
	})
	if err != nil {
		t.Fatalf("safeStorageKeyFromLocalState: %v", err)
	}
	if string(wrapped) != "wrapped" {
		t.Fatalf("DPAPI input = %q, want wrapped", wrapped)
	}
	if string(got) != string(key) {
		t.Fatalf("key = %x, want %x", got, key)
	}
}

func TestParseExpiryAndConfigDefaults(t *testing.T) {
	cfg, err := parseConfig([]byte(`{"request_timeout":45,"cooldown":"2m"}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := cfg.requestTimeout(); got != 45*time.Second {
		t.Fatalf("requestTimeout = %s, want 45s", got)
	}
	if got := cfg.cooldown(); got != 2*time.Minute {
		t.Fatalf("cooldown = %s, want 2m", got)
	}
	if got := cfg.authCooldown(); got != defaultAuthCooldown {
		t.Fatalf("authCooldown = %s, want default %s", got, defaultAuthCooldown)
	}
}

func encryptSafeStorageForTest(key, nonce, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return nil, err
	}
	out := append([]byte("v10"), nonce...)
	out = gcm.Seal(out, nonce, plain, nil)
	return out, nil
}

func TestAccountRecordDoesNotExposeToken(t *testing.T) {
	now := time.Now().UTC()
	rec := accountRecord(&account{storedAccount: storedAccount{
		ID: "a1", Token: "very-secret", Enabled: true,
	}}, now)
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal account record: %v", err)
	}
	if stringContains(string(raw), "very-secret") {
		t.Fatalf("serialized account record leaked token: %s", raw)
	}
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestNewRegistersConcreteCapabilities(t *testing.T) {
	c, err := New(core.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.(core.AccountManager); !ok {
		t.Fatal("client does not implement AccountManager")
	}
	if _, ok := c.(core.CredentialImporter); !ok {
		t.Fatal("client does not implement CredentialImporter")
	}
	if _, ok := c.(core.BalanceProvider); !ok {
		t.Fatal("client does not implement BalanceProvider")
	}
	if _, ok := c.(core.CheckinProvider); !ok {
		t.Fatal("client does not implement CheckinProvider")
	}
	if _, ok := c.(core.Reviver); !ok {
		t.Fatal("client does not implement Reviver")
	}
	if _, ok := c.(core.SMSProvider); !ok {
		t.Fatal("client does not implement SMSProvider")
	}
	if _, ok := c.(core.AutoLoginProvider); !ok {
		t.Fatal("client does not implement AutoLoginProvider")
	}
	for name, ok := range map[string]bool{
		"PackageProvider":     implements[core.PackageProvider](c),
		"TaskProvider":        implements[core.TaskProvider](c),
		"TaskClaimer":         implements[core.TaskClaimer](c),
		"TaskAutoRunner":      implements[core.TaskAutoRunner](c),
		"HealthProvider":      implements[core.HealthProvider](c),
		"PoolStatsReporter":   implements[core.PoolStatsReporter](c),
		"ModelLimitsProvider": implements[core.ModelLimitsProvider](c),
		"ConversationBinder":  implements[core.ConversationBinder](c),
		"LiveReloader":        implements[core.LiveReloader](c),
	} {
		if !ok {
			t.Fatalf("client does not implement %s", name)
		}
	}
}

func implements[T any](v any) bool {
	_, ok := v.(T)
	return ok
}

func Example_splitCampaigns() {
	claimable, claimed := splitCampaigns(&campaignsResponse{Campaigns: []campaign{
		{CampaignID: "daily", ActionType: campaignActionClaim, ClaimStatus: campaignStatusClaim},
	}})
	fmt.Println(len(claimable), claimed)
	// Output: 1 0
}
