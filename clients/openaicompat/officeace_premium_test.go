package openaicompat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
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
