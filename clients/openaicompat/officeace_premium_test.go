package openaicompat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const officeAcePremiumSubscriptionJSON = `{
  "domain_id": "acct-test",
  "subscribe_status": "SUBSCRIBED",
  "skus": [{
    "cbc_resource_id": "res-1",
    "sku_code": "officeace.personal.package.trial",
    "sku_name": "个人体验版",
    "effective_time": "2026-10-16T09:21:49.739Z",
    "expired_time": "2099-01-01T00:00:00.000Z",
    "subscribe_status": "SUBSCRIBED",
    "quotas": [
      {"sku_attr_code": "officeace_points", "sku_value": "500", "current_value": "100",
       "effective_time": "2026-10-16T09:21:49.739Z", "expired_time": "2099-01-01T00:00:00.000Z"},
      {"sku_attr_code": "online_search_count", "sku_value": "200", "current_value": "5",
       "effective_time": "2026-10-16T09:21:49.739Z", "expired_time": "2099-01-01T00:00:00.000Z"}
    ]
  }],
  "bonus_skus": [
    {"activity_id": "bonus-new-user-2026", "activity_name": "新用户专属福利", "points": "4000",
     "current_value": "0", "effective_time": "2026-10-16T01:21:46.218Z", "expired_time": "2099-01-01T00:00:00.000Z"},
    {"activity_id": "bonus-daily-checkin-2026", "activity_name": "每日签到有礼", "points": "1000",
     "current_value": "0", "effective_time": "2026-10-16T01:21:46.366Z", "expired_time": "2099-01-01T00:00:00.000Z"}
  ],
  "model_infos": []
}`

func officeAceTestScalar(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return officeAceEncodeScalar(key)
}

// officeAcePremiumSource builds a source whose OfficeAce wrapper talks to the
// given server for every endpoint it uses.
func officeAcePremiumSource(t *testing.T, srv *httptest.Server) *officeAceSource {
	t.Helper()
	s, err := newSource(officeAceSourceID, core.SourceConfig{BaseURL: srv.URL + "/v2"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	o := &officeAceSource{source: s, eps: officeAceEndpoints{
		ClawBase:     srv.URL,
		AuthBase:     srv.URL,
		IAMBase:      srv.URL,
		KeystoneBase: srv.URL,
		ClientID:     "test-client",
	}}
	o.enableDesktopModelSelection()
	o.bindAuthExhaustedHook()
	return o
}

func officeAceAddAccountWithSession(t *testing.T, o *officeAceSource, srv *httptest.Server, sess officeAceStoredSession) string {
	t.Helper()
	ctx := context.Background()
	rec, err := o.source.addAccount(ctx, core.AccountSpec{
		ID:     "oa-test",
		Label:  "test-user",
		Fields: map[string]string{"api_key": "test-app-key", "app_secret": "test-app-secret"},
	}, srv.URL+"/v2")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := o.saveOfficeAceSession(rec.ID, sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return rec.ID
}

func TestOfficeAcePremiumCheckinBalancePackages(t *testing.T) {
	var claimCalls, subscriptionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), officeAceV11SignAlgorithm) {
			http.Error(w, `{"error_code":"APIG.0301"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathBonusClaim:
			claimCalls.Add(1)
			if r.Header.Get("X-Security-Token") != "sec-token" {
				http.Error(w, "missing security token", http.StatusUnauthorized)
				return
			}
			if claimCalls.Load() == 1 {
				_, _ = io.WriteString(w, `{"bonus_skus":[{"activity_id":"bonus-daily-checkin-2026","activity_name":"每日签到有礼","points":"1000","current_value":"0"}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"bonus_skus":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			subscriptionCalls.Add(1)
			if r.Header.Get("x-subscription-type") != "v2" {
				http.Error(w, "missing subscription header", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:        "cn-southwest-2",
		AccessKey:     "access",
		SecretKey:     "secret",
		SecurityToken: "sec-token",
		ProjectID:     "project-1",
		ExpiresAt:     time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	})

	ctx := context.Background()
	caps := core.CapabilitiesOf(ctx, o)
	if !caps.Checkin || !caps.CheckinReady || !caps.Balance || !caps.Packages {
		t.Fatalf("capabilities = %+v", caps)
	}
	if len(caps.Actions) != 1 || caps.Actions[0].ID != officeAceCheckinAction {
		t.Fatalf("actions = %+v", caps.Actions)
	}

	first, err := o.Checkin(ctx, id, "")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !first.OK || !strings.Contains(first.Message, "每日签到有礼") {
		t.Fatalf("first checkin = %+v", first)
	}
	second, err := o.Checkin(ctx, id, officeAceCheckinAction)
	if err != nil {
		t.Fatalf("second checkin: %v", err)
	}
	if !second.OK || !strings.Contains(second.Message, "已经领过") {
		t.Fatalf("second checkin = %+v", second)
	}
	if claimCalls.Load() != 2 {
		t.Fatalf("claim calls = %d, want 2", claimCalls.Load())
	}

	bal, err := o.AccountBalance(ctx, id, 0)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Credits != 5400 || bal.Total != 5500 || bal.Used != 100 || bal.Unit != officeAceBalanceUnit {
		t.Fatalf("balance = %+v", bal)
	}
	if len(bal.Lines) != 1 || bal.Lines[0].Label != "联网搜索" || bal.Lines[0].Amount == nil || *bal.Lines[0].Amount != 195 {
		t.Fatalf("balance lines = %+v", bal.Lines)
	}

	report, err := o.AccountPackages(ctx, id)
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if report.Remain != 5400 || report.Size != 5500 || len(report.Packages) != 3 {
		t.Fatalf("package report = %+v", report)
	}

	// An unknown account is a Go error, never an upstream call.
	if _, err := o.Checkin(ctx, "missing", ""); err == nil {
		t.Fatal("checkin on an unknown account must error")
	}
}

func TestOfficeAceEnsureTrialSkipsExistingEntitlement(t *testing.T) {
	var skuCalls, orderCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			skuCalls.Add(1)
			http.Error(w, "an existing trial must not query the SKU", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			orderCalls.Add(1)
			http.Error(w, "an existing trial must not be ordered again", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:        "cn-southwest-2",
		AccessKey:     "access",
		SecretKey:     "secret",
		SecurityToken: "sec-token",
		ProjectID:     "project-1",
		ExpiresAt:     time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	})
	if err := o.ensureOfficeAceTrial(context.Background(), id); err != nil {
		t.Fatalf("ensure trial: %v", err)
	}
	if skuCalls.Load() != 0 || orderCalls.Load() != 0 {
		t.Fatalf("existing trial caused sku=%d order=%d calls", skuCalls.Load(), orderCalls.Load())
	}
}

func TestOfficeAceTrialOrderUsesV3SignatureAndAutoPays(t *testing.T) {
	var orderBody officeAceTrialOrder
	var orderCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, `{"subscribe_status":"SUBSCRIBED","skus":[],"bonus_skus":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			if !strings.HasPrefix(r.Header.Get("Authorization"), officeAceSignAlgorithm) {
				http.Error(w, `{"error_code":"APIG.0301","message":"not SDK signed"}`, http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Security-Token") != "v3-token" || r.Header.Get("X-Project-ID") != "v3-project" {
				http.Error(w, "missing v3 credential headers", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"product_sales_cycles":[{"id":"atomic-trial-product","status":"ONSALE"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			orderCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), officeAceSignAlgorithm) {
				http.Error(w, `{"error_code":"APIG.0301","message":"not SDK signed"}`, http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Security-Token") != "v3-token" || r.Header.Get("X-Project-ID") != "v3-project" {
				http.Error(w, "missing v3 credential headers", http.StatusUnauthorized)
				return
			}
			if err := json.NewDecoder(r.Body).Decode(&orderBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, `{"status":"SUBSCRIBED","order_id":"order-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:          "cn-southwest-2",
		AccessKey:       "v11-access",
		SecretKey:       "v11-secret",
		SecurityToken:   "v11-token",
		ProjectID:       "v11-project",
		ExpiresAt:       "2099-01-01T00:00:00Z",
		V3AccessKey:     "v3-access",
		V3SecretKey:     "v3-secret",
		V3SecurityToken: "v3-token",
		V3ProjectID:     "v3-project",
		V3ExpiresAt:     "2099-01-01T00:00:00Z",
	})

	if err := o.ensureOfficeAceTrial(context.Background(), id); err != nil {
		t.Fatalf("ensure trial: %v", err)
	}
	if orderCalls.Load() != 1 {
		t.Fatalf("order calls = %d, want 1", orderCalls.Load())
	}
	if orderBody.AutoRenewal != 0 || orderBody.AutoPay != 1 {
		t.Fatalf("auto flags = renewal:%d pay:%d, want 0/1 (the zero-priced trial must settle immediately)", orderBody.AutoRenewal, orderBody.AutoPay)
	}
}

func TestOfficeAceBalanceDoesNotCreateAnOrder(t *testing.T) {
	var subscriptionCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			subscriptionCalls.Add(1)
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			http.Error(w, "a balance read must never query the SKU", http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			http.Error(w, "a balance read must never create an order", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:          "cn-southwest-2",
		AccessKey:       "access",
		SecretKey:       "secret",
		SecurityToken:   "token",
		ProjectID:       "project-1",
		ExpiresAt:       "2099-01-01T00:00:00Z",
		V3AccessKey:     "v3-access",
		V3SecretKey:     "v3-secret",
		V3SecurityToken: "v3-token",
		V3ProjectID:     "project-1",
		V3ExpiresAt:     "2099-01-01T00:00:00Z",
	})

	bal, err := o.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Credits != 5400 {
		t.Fatalf("balance = %+v", bal)
	}
	if subscriptionCalls.Load() != 1 {
		t.Fatalf("subscription calls = %d, want exactly one read", subscriptionCalls.Load())
	}
}

func TestOfficeAceCheckinRequiresASession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	o := officeAcePremiumSource(t, srv)
	ctx := context.Background()
	rec, err := o.source.addAccount(ctx, core.AccountSpec{
		ID:     "oa-nosession",
		Fields: map[string]string{"api_key": "k", "app_secret": "s"},
	}, srv.URL+"/v2")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if actions := o.CheckinActions(ctx); len(actions) != 0 {
		t.Fatalf("actions without a session = %+v", actions)
	}
	if _, err := o.Checkin(ctx, rec.ID, ""); err == nil {
		t.Fatal("checkin without a cloud session must error")
	}
}

func TestOfficeAcePremiumRefreshesOnStaleSignature(t *testing.T) {
	var subscriptionCalls, tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			if subscriptionCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error_code":"APIG.0301"}`)
				return
			}
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumTokenPath:
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
				http.Error(w, "bad refresh request", http.StatusBadRequest)
				return
			}
			if r.Header.Get("DPoP") == "" {
				http.Error(w, "missing DPoP", http.StatusBadRequest)
				return
			}
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "new-access",
					"secret_access_key": "new-secret",
					"security_token":    "new-token",
					"project_id":        "project-1",
					"expiration":        "2099-01-01T00:00:00Z",
				},
				"refresh_token": "new-refresh",
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSecurityTok:
			http.Error(w, "STS should not be needed", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:           "cn-southwest-2",
		AccessKey:        "old-access",
		SecretKey:        "old-secret",
		SecurityToken:    "old-token",
		ProjectID:        "project-1",
		ExpiresAt:        "2099-01-01T00:00:00Z",
		RefreshToken:     "old-refresh",
		RefreshExpiresAt: "2099-01-01T00:00:00Z",
		DPoPScalar:       officeAceTestScalar(t),
	})

	bal, err := o.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Credits != 5400 {
		t.Fatalf("balance = %+v", bal)
	}
	if tokenCalls.Load() != 1 || subscriptionCalls.Load() != 2 {
		t.Fatalf("refresh calls token=%d subscription=%d", tokenCalls.Load(), subscriptionCalls.Load())
	}
	row, ok := o.source.officeAceFindRow(id)
	if !ok || row.OfficeAce == nil {
		t.Fatal("session row disappeared")
	}
	if row.OfficeAce.AccessKey != "new-access" || row.OfficeAce.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed session = %+v", row.OfficeAce)
	}
}

func TestOfficeAcePremiumRefreshesOnExpiredSecurityToken(t *testing.T) {
	var subscriptionCalls, tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			if subscriptionCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error_msg":"Bad request: the security token has expired","error_code":"APIG.0602"}`)
				return
			}
			if r.Header.Get("X-Security-Token") != "new-token" {
				http.Error(w, "stale security token", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumTokenPath:
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "old-refresh" {
				http.Error(w, "bad refresh request", http.StatusBadRequest)
				return
			}
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "new-access",
					"secret_access_key": "new-secret",
					"security_token":    "new-token",
					"project_id":        "project-1",
					"expiration":        "2099-01-01T00:00:00Z",
				},
				"refresh_token": "new-refresh",
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:           "cn-southwest-2",
		AccessKey:        "old-access",
		SecretKey:        "old-secret",
		SecurityToken:    "old-token",
		ProjectID:        "project-1",
		ExpiresAt:        "2099-01-01T00:00:00Z",
		RefreshToken:     "old-refresh",
		RefreshExpiresAt: "2099-01-01T00:00:00Z",
		DPoPScalar:       officeAceTestScalar(t),
	})

	bal, err := o.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Credits != 5400 {
		t.Fatalf("balance = %+v", bal)
	}
	if tokenCalls.Load() != 1 || subscriptionCalls.Load() != 2 {
		t.Fatalf("refresh calls token=%d subscription=%d", tokenCalls.Load(), subscriptionCalls.Load())
	}
	row, ok := o.source.officeAceFindRow(id)
	if !ok || row.OfficeAce == nil {
		t.Fatal("session row disappeared")
	}
	if row.OfficeAce.AccessKey != "new-access" || row.OfficeAce.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed session = %+v", row.OfficeAce)
	}
}

func TestOfficeAcePremiumRefreshesBeforeSecurityTokenExpires(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 51, 0, 0, time.UTC)
	var subscriptionCalls, tokenCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			subscriptionCalls.Add(1)
			if r.Header.Get("X-Security-Token") != "new-token" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error_msg":"Bad request: the security token has expired","error_code":"APIG.0602"}`)
				return
			}
			_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumTokenPath:
			tokenCalls.Add(1)
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "new-access",
					"secret_access_key": "new-secret",
					"security_token":    "new-token",
					"project_id":        "project-1",
					"expiration":        "2099-01-01T00:00:00Z",
				},
				"refresh_token": "new-refresh",
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	o.source.now = func() time.Time { return now }
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:           "cn-southwest-2",
		AccessKey:        "old-access",
		SecretKey:        "old-secret",
		SecurityToken:    "old-token",
		ProjectID:        "project-1",
		ExpiresAt:        now.Add(9 * time.Minute).Format(time.RFC3339),
		RefreshToken:     "old-refresh",
		RefreshExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339),
		DPoPScalar:       officeAceTestScalar(t),
	})

	bal, err := o.AccountBalance(context.Background(), id, 0)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if bal.Credits != 5400 {
		t.Fatalf("balance = %+v", bal)
	}
	if tokenCalls.Load() != 1 || subscriptionCalls.Load() != 1 {
		t.Fatalf("refresh calls token=%d subscription=%d", tokenCalls.Load(), subscriptionCalls.Load())
	}
	row, ok := o.source.officeAceFindRow(id)
	if !ok || row.OfficeAce == nil || row.OfficeAce.SecurityToken != "new-token" {
		t.Fatalf("refreshed session = %+v", row.OfficeAce)
	}
}

func TestOfficeAcePremiumSerializesConcurrentRefresh(t *testing.T) {
	var subscriptionCalls, tokenCalls, oldTokenCalls atomic.Int32
	bothOldRequests := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			subscriptionCalls.Add(1)
			switch r.Header.Get("X-Security-Token") {
			case "old-token":
				if oldTokenCalls.Add(1) == 2 {
					close(bothOldRequests)
				}
				<-bothOldRequests
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error_msg":"Bad request: the security token has expired","error_code":"APIG.0602"}`)
			case "new-token":
				_, _ = io.WriteString(w, officeAcePremiumSubscriptionJSON)
			default:
				http.Error(w, "unexpected security token", http.StatusUnauthorized)
			}
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumTokenPath:
			call := tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "old-refresh" || call != 1 {
				http.Error(w, "refresh token already used", http.StatusBadRequest)
				return
			}
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "new-access",
					"secret_access_key": "new-secret",
					"security_token":    "new-token",
					"project_id":        "project-1",
					"expiration":        "2099-01-01T00:00:00Z",
				},
				"refresh_token": "new-refresh",
			}
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:           "cn-southwest-2",
		AccessKey:        "old-access",
		SecretKey:        "old-secret",
		SecurityToken:    "old-token",
		ProjectID:        "project-1",
		ExpiresAt:        "2099-01-01T00:00:00Z",
		RefreshToken:     "old-refresh",
		RefreshExpiresAt: "2099-01-01T00:00:00Z",
		DPoPScalar:       officeAceTestScalar(t),
	})

	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := o.AccountBalance(context.Background(), id, 0)
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("balance: %v", err)
		}
	}
	if tokenCalls.Load() != 1 || subscriptionCalls.Load() != 4 {
		t.Fatalf("refresh calls token=%d subscription=%d", tokenCalls.Load(), subscriptionCalls.Load())
	}
	row, ok := o.source.officeAceFindRow(id)
	if !ok || row.OfficeAce == nil || row.OfficeAce.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed session = %+v", row.OfficeAce)
	}
}

func TestOfficeAceTrialOrderUnpaidTenantNamesThePaymentCenter(t *testing.T) {
	var orderBody officeAceTrialOrder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, `{"subscribe_status":"SUBSCRIBED","skus":[],"bonus_skus":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			_, _ = io.WriteString(w, `{"product_sales_cycles":[{"id":"atomic-trial-product","status":"ONSALE"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			if err := json.NewDecoder(r.Body).Decode(&orderBody); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error_code":"AgentArts.00000148","error_msg":"租户存在未支付订单。","error_suggestion":"请去华为云控制台费用中心确认。"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:          "cn-southwest-2",
		AccessKey:       "access",
		SecretKey:       "secret",
		SecurityToken:   "token",
		ProjectID:       "project-1",
		ExpiresAt:       "2099-01-01T00:00:00Z",
		V3AccessKey:     "v3-access",
		V3SecretKey:     "v3-secret",
		V3SecurityToken: "v3-token",
		V3ProjectID:     "project-1",
		V3ExpiresAt:     "2099-01-01T00:00:00Z",
	})

	err := o.ensureOfficeAceTrial(context.Background(), id)
	if err == nil {
		t.Fatal("an unpaid-order refusal must be reported")
	}
	if !strings.Contains(err.Error(), "费用中心") || !strings.Contains(err.Error(), "支付或取消") {
		t.Fatalf("error must tell the operator how to clear the unpaid order: %v", err)
	}
	if orderBody.AutoPay != 1 {
		t.Fatalf("auto_pay = %d, want 1", orderBody.AutoPay)
	}
}

func TestOfficeAceProfileImportCarriesCloudSession(t *testing.T) {
	doc := officeAceProfileDocument{}
	if err := json.Unmarshal([]byte(`{
      "userName":"test-user","accountId":"acct-1","user_id":"user-1",
      "principalUrn":"iam::acct-1:user:test-user",
      "credential":{"access":"ak","secret":"sk","sts_token":"st","project_id":"proj",
        "expires_at":"2099-01-01T00:00:00.000Z","refresh_token":"rt",
        "refresh_expires_at":"2099-02-01T00:00:00.000Z"},
      "credentialV3":{"access":"v3ak","secret":"v3sk","sts_token":"v3st","expires_at":"2099-01-01T00:00:00.000Z"},
      "dpop":{"privateJwk":{"d":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}},
      "modelInfo":{"model_api_url_base":"modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com",
        "model_auth_info":{"model_app_key":"`+testOfficeAceAppKey+`","model_app_secret":"`+testOfficeAceAppSecret+`"}}
    }`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sess := officeAceSessionFromProfile(doc)
	if sess == nil {
		t.Fatal("profile session was not extracted")
	}
	if sess.Region != "cn-southwest-2" || sess.AccessKey != "ak" || sess.RefreshToken != "rt" || sess.V3AccessKey != "v3ak" {
		t.Fatalf("session = %+v", sess)
	}
}

func TestOfficeAceFetchV3AcceptsCreated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != officeAcePremiumPathSecurityTok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"credential":{"access":"v3-access","secret":"v3-secret","securitytoken":"v3-token","expires_at":"2099-01-01T00:00:00Z"}}`)
	}))
	defer srv.Close()

	got, ok := officeAceFetchV3(context.Background(), srv.Client(), srv.URL, "v5-access", "v5-secret", "v5-token", "project-1", time.Now())
	if !ok {
		t.Fatal("the IAM security-token endpoint returns 201 Created; that must count as success")
	}
	if got.Access != "v3-access" || got.Secret != "v3-secret" || got.SecurityToken != "v3-token" || got.ProjectID != "project-1" {
		t.Fatalf("v3 credential = %+v", got)
	}
}

// TestOfficeAceV3RetriesOnIAMAuthRefusal replays the 2026-10-24 login where the
// trial SKU query was refused with 401 APIGW.0301 ("Incorrect IAM
// authentication information") right after a successful sign-in.  That code is
// an IAM-auth refusal like APIG.0301, so the request must clear the stored V3
// credential, mint a fresh one and retry instead of giving up on the trial.
func TestOfficeAceV3RetriesOnIAMAuthRefusal(t *testing.T) {
	var skuCalls, mintCalls, orderCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			if !strings.HasPrefix(r.Header.Get("Authorization"), officeAceV11SignAlgorithm) {
				http.Error(w, `{"error_code":"APIG.0301"}`, http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"subscribe_status":"SUBSCRIBED","skus":[],"bonus_skus":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			skuCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), officeAceSignAlgorithm) {
				http.Error(w, `{"error_code":"APIGW.0301","error_msg":"not SDK signed"}`, http.StatusUnauthorized)
				return
			}
			if r.Header.Get("X-Security-Token") != "fresh-v3-token" {
				// The stored credential is refused exactly the way the live
				// gateway refused the post-login order.
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error_code":"APIGW.0301","error_msg":"Incorrect IAM authentication information: get token error,status:400"}`)
				return
			}
			_, _ = io.WriteString(w, `{"product_sales_cycles":[{"id":"atomic-trial-product","status":"ONSALE"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSecurityTok:
			mintCalls.Add(1)
			_, _ = io.WriteString(w, `{"credential":{"access":"fresh-v3-access","secret":"fresh-v3-secret","securitytoken":"fresh-v3-token","expires_at":"2099-01-01T00:00:00Z"}}`)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			orderCalls.Add(1)
			if r.Header.Get("X-Security-Token") != "fresh-v3-token" {
				http.Error(w, "the order must ride the re-minted credential", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"status":"SUBSCRIBED","order_id":"order-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:          "cn-southwest-2",
		AccessKey:       "v11-access",
		SecretKey:       "v11-secret",
		SecurityToken:   "v11-token",
		ProjectID:       "project-1",
		ExpiresAt:       "2099-01-01T00:00:00Z",
		V3AccessKey:     "stale-v3-access",
		V3SecretKey:     "stale-v3-secret",
		V3SecurityToken: "stale-v3-token",
		V3ProjectID:     "project-1",
		V3ExpiresAt:     "2099-01-01T00:00:00Z",
	})

	if err := o.ensureOfficeAceTrial(context.Background(), id); err != nil {
		t.Fatalf("ensure trial: %v", err)
	}
	if skuCalls.Load() != 2 {
		t.Fatalf("sku calls = %d, want one refusal and one retry", skuCalls.Load())
	}
	if mintCalls.Load() != 1 {
		t.Fatalf("v3 mints = %d, want 1", mintCalls.Load())
	}
	if orderCalls.Load() != 1 {
		t.Fatalf("order calls = %d, want 1", orderCalls.Load())
	}
}

// TestOfficeAcePanelReadRepairsMissingTrial covers the other half of the
// incident: the login-time order failed, so the trial stayed missing.  The
// panel's package read is the repair hook -- it places the order once, and the
// repair window keeps a cloud that keeps refusing from being probed on every
// refresh.
func TestOfficeAcePanelReadRepairsMissingTrial(t *testing.T) {
	var skuCalls, orderCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			// The vendor never shows the trial here: this account stays in the
			// broken state the login left behind, so every read would hit the
			// repair path without the throttle.
			_, _ = io.WriteString(w, `{"subscribe_status":"SUBSCRIBED","skus":[],"bonus_skus":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skus/"+officeAceTrialSKUCode:
			skuCalls.Add(1)
			_, _ = io.WriteString(w, `{"product_sales_cycles":[{"id":"atomic-trial-product","status":"ONSALE"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == officeAcePremiumPathSubscriptionPeriod:
			orderCalls.Add(1)
			_, _ = io.WriteString(w, `{"status":"SUBSCRIBED","order_id":"order-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddAccountWithSession(t, o, srv, officeAceStoredSession{
		Region:          "cn-southwest-2",
		AccessKey:       "v11-access",
		SecretKey:       "v11-secret",
		SecurityToken:   "v11-token",
		ProjectID:       "project-1",
		ExpiresAt:       "2099-01-01T00:00:00Z",
		V3AccessKey:     "v3-access",
		V3SecretKey:     "v3-secret",
		V3SecurityToken: "v3-token",
		V3ProjectID:     "project-1",
		V3ExpiresAt:     "2099-01-01T00:00:00Z",
	})
	ctx := context.Background()

	if _, err := o.AccountPackages(ctx, id); err != nil {
		t.Fatalf("first packages read: %v", err)
	}
	if skuCalls.Load() != 1 || orderCalls.Load() != 1 {
		t.Fatalf("first read did not repair: sku=%d order=%d", skuCalls.Load(), orderCalls.Load())
	}
	if _, err := o.AccountBalance(ctx, id, 0); err != nil {
		t.Fatalf("second read (balance): %v", err)
	}
	if _, err := o.AccountPackages(ctx, id); err != nil {
		t.Fatalf("third read (packages): %v", err)
	}
	if skuCalls.Load() != 1 || orderCalls.Load() != 1 {
		t.Fatalf("the repair window did not throttle: sku=%d order=%d", skuCalls.Load(), orderCalls.Load())
	}
}

// officeAceDrainedSubscriptionJSON is a subscription whose points are spent to
// the last one: exactly the state the live OfficeAce gateway answers with a
// bare 401 APIG.1009 on the model key.
const officeAceDrainedSubscriptionJSON = `{
  "subscribe_status": "SUBSCRIBED",
  "skus": [{
    "sku_code": "officeace.personal.package.trial",
    "subscribe_status": "SUBSCRIBED",
    "quotas": [
      {"sku_attr_code": "officeace_points", "sku_value": "4600", "current_value": "4600",
       "effective_time": "2026-10-16T09:21:49.739Z", "expired_time": "2099-01-01T00:00:00.000Z"}
    ]
  }],
  "bonus_skus": []
}`

func officeAceChatServer(t *testing.T, subscription string, chatCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatCalls.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error_msg":"AppKey or AppSecret is invalid","error_code":"APIG.1009"}`)
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, subscription)
		default:
			http.NotFound(w, r)
		}
	}))
}

func officeAceAddChatAccount(t *testing.T, o *officeAceSource, srv *httptest.Server) string {
	t.Helper()
	rec, err := o.source.addAccount(context.Background(), core.AccountSpec{
		ID:     "oa-test",
		Label:  "test-user",
		Fields: map[string]string{"api_key": "test-app-key", "app_secret": "test-app-secret", "models": "m"},
	}, srv.URL+"/v2")
	if err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := o.saveOfficeAceSession(rec.ID, officeAceStoredSession{
		Region:        "cn-southwest-2",
		AccessKey:     "access",
		SecretKey:     "secret",
		SecurityToken: "sec-token",
		ProjectID:     "project-1",
		ExpiresAt:     time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return rec.ID
}

func officeAceChatOnce(t *testing.T, s *source) error {
	t.Helper()
	req := &core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "ping"}},
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	_, err := s.Chat(context.Background(), req)
	return err
}

// TestOfficeAceDrainedPackageParksQuotaNotInvalid pins the exhaustion veto: a
// zero-credit account whose model key is refused with a bare 401 must cool
// down like any other out-of-quota account, not read 账号异常 -- a refill
// (daily check-in, a new trial) revives it, so the row must stay on the
// cooling path instead of demanding a re-login.
func TestOfficeAceDrainedPackageParksQuotaNotInvalid(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	// The panel sweep has seen the drained package, so the wrapper knows the
	// refusal is the exhaustion verdict.
	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	if err := officeAceChatOnce(t, o.source); err == nil {
		t.Fatal("chat against a drained account unexpectedly succeeded")
	}
	records, err := o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %+v, want one account", records)
	}
	if got := records[0].State; got != "cooling" {
		t.Fatalf("state after a drained-package 401 = %q, want cooling", got)
	}
	// The quota park must hold the account out of rotation: a request inside
	// the cooldown window answers busy without another vendor roundtrip.
	if err := officeAceChatOnce(t, o.source); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second chat during the quota cooldown = %v, want busy", err)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("chat calls = %d, want 1 (the cooldown must park, not retry)", got)
	}
}

// TestOfficeAceBalanceReadSupersedesDeadKeyVerdict covers the self-heal half:
// an account parked as invalid before its first credit read (a restart wipes
// the cache) is downgraded to the quota cooldown by the next balance sweep
// that observes zero credits.
func TestOfficeAceBalanceReadSupersedesDeadKeyVerdict(t *testing.T) {
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	if err := officeAceChatOnce(t, o.source); err == nil {
		t.Fatal("chat against a refused key unexpectedly succeeded")
	}
	records, err := o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if got := records[0].State; got != "invalid" {
		t.Fatalf("state with an empty credit cache = %q, want invalid", got)
	}
	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	records, err = o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if got := records[0].State; got != "cooling" {
		t.Fatalf("state after a zero-credit read = %q, want cooling", got)
	}
}

// TestOfficeAceLiveCreditKeepsDeadKeyInvalid is the guard on the veto itself:
// when the account still has credits, the same 401 is a verdict about the key
// and must stay invalid with the re-login path.
func TestOfficeAceLiveCreditKeepsDeadKeyInvalid(t *testing.T) {
	var chatCalls atomic.Int32
	live := `{
  "subscribe_status": "SUBSCRIBED",
  "skus": [{
    "sku_code": "officeace.personal.package.trial",
    "subscribe_status": "SUBSCRIBED",
    "quotas": [
      {"sku_attr_code": "officeace_points", "sku_value": "5300", "current_value": "0",
       "effective_time": "2026-10-16T09:21:49.739Z", "expired_time": "2099-01-01T00:00:00.000Z"}
    ]
  }],
  "bonus_skus": []
}`
	srv := officeAceChatServer(t, live, &chatCalls)
	defer srv.Close()

	o := officeAcePremiumSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	if err := officeAceChatOnce(t, o.source); err == nil {
		t.Fatal("chat against a refused key unexpectedly succeeded")
	}
	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	records, err := o.source.Accounts(context.Background())
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if got := records[0].State; got != "invalid" {
		t.Fatalf("state with credits remaining = %q, want invalid", got)
	}
	// An invalid key is not offered again at all.
	if err := officeAceChatOnce(t, o.source); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("second chat with an invalid key = %v, want not-configured", err)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("chat calls = %d, want 1 (an invalid key must not retry)", got)
	}
}

// TestOfficeAceExhaustedVerdictExpires pins the freshness window: a stale
// zero-credit read must not pin the veto forever, or an account that refilled
// while the panel sweep was off would cool down instead of surfacing the
// dead-key verdict.
func TestOfficeAceExhaustedVerdictExpires(t *testing.T) {
	o := &officeAceSource{source: &source{}}
	now := time.Now()
	o.noteOfficeAcePoints("a", 0, now)
	if !o.officeAceCreditsExhausted("a", now.Add(29*time.Minute)) {
		t.Fatal("a fresh zero-credit read must count as exhausted")
	}
	if o.officeAceCreditsExhausted("a", now.Add(31*time.Minute)) {
		t.Fatal("the exhaustion verdict must expire with its window")
	}
	if o.officeAceCreditsExhausted("missing", now) {
		t.Fatal("an account without a cached read must not count as exhausted")
	}
	o.noteOfficeAcePoints("b", -1, now)
	if o.officeAceCreditsExhausted("b", now) {
		t.Fatal("an unlimited position must not count as exhausted")
	}
}
