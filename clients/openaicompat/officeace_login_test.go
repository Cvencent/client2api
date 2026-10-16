package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

func officeAceTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestOfficeAceHuaweiAccountLoginWritesCredential(t *testing.T) {
	var codePolls, permissionCalls, subscriptionCalls, scanCalls atomic.Int32
	var permissionSigned atomic.Bool
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/claw/auth/state":
			_, _ = io.WriteString(w, `{"state":"test-state"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/claw/auth/code":
			if r.URL.Query().Get("state") != "test-state" {
				http.Error(w, "wrong state", http.StatusBadRequest)
				return
			}
			if codePolls.Add(1) == 1 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			_, _ = io.WriteString(w, `{"code":"test-code","state":"test-state"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/oauth2/tokens":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.Form.Get("code") != "test-code" || r.Form.Get("client_id") != "test-client" || r.Form.Get("code_verifier") == "" {
				http.Error(w, "bad token request", http.StatusBadRequest)
				return
			}
			body := map[string]any{
				"credentials": map[string]string{
					"access_key_id":     "test-access-key",
					"secret_access_key": "test-secret-key",
					"security_token":    "test-security-token",
					"project_id":        "test-project",
					"expiration":        "2026-10-16T13:00:00Z",
				},
				"id_token": officeAceTestJWT(t, map[string]any{
					"sub":                "test-subject",
					"preferred_username": "test-user",
				}),
			}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/claw/client-permission-validate":
			permissionCalls.Add(1)
			if strings.Contains(r.Header.Get("Authorization"), officeAceSignAlgorithm) {
				permissionSigned.Store(true)
			}
			if r.Header.Get("X-Security-Token") != "test-security-token" {
				http.Error(w, "missing security token", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"account_id":"acct-test","principal_id":"user-test","principal_urn":"iam::123:user:test-user"}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/claw/client-subscription":
			subscriptionCalls.Add(1)
			body := map[string]any{"data": map[string]any{"model_info": map[string]any{
				"model_api_url_base": srv.URL + "/model-gateway",
				"model_auth_info": map[string]string{
					"model_app_key":    testOfficeAceAppKey,
					"model_app_secret": testOfficeAceAppSecret,
				},
			}}}
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && r.URL.Path == "/model-gateway/v2/models":
			scanCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"test-glm"},{"id":"test-deepseek"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s, err := newSource(officeAceSourceID, core.SourceConfig{BaseURL: "https://default.invalid/v2"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	o := &officeAceSource{source: s, eps: officeAceEndpoints{
		ClawBase: srv.URL,
		AuthBase: srv.URL,
		IAMBase:  srv.URL,
		ClientID: "test-client",
	}}
	ctx := context.Background()
	if !core.CapabilitiesOf(ctx, o).Login {
		t.Fatal("OfficeAce does not advertise the account-login capability")
	}

	state, err := o.StartLogin(ctx)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	if state.State != core.LoginPending || state.SessionID != "test-state" || !strings.Contains(state.URL, "/authui/login.html?service=") {
		t.Fatalf("start login state = %+v", state)
	}

	pending, err := o.PollLogin(ctx, state.SessionID)
	if err != nil || pending.State != core.LoginPending {
		t.Fatalf("first poll = %+v, %v", pending, err)
	}
	if codePolls.Load() != 1 {
		t.Fatalf("code polls = %d, want 1", codePolls.Load())
	}

	done, err := o.PollLogin(ctx, state.SessionID)
	if err != nil || done.State != core.LoginSuccess || done.AccountID == "" {
		t.Fatalf("second poll = %+v, %v", done, err)
	}
	if permissionCalls.Load() != 1 || subscriptionCalls.Load() != 1 || scanCalls.Load() != 1 {
		t.Fatalf("call counts permission=%d subscription=%d scan=%d", permissionCalls.Load(), subscriptionCalls.Load(), scanCalls.Load())
	}
	if !permissionSigned.Load() {
		t.Fatal("permission call was not SDK-HMAC-SHA256 signed")
	}

	s.mu.Lock()
	if len(s.rows) != 1 {
		s.mu.Unlock()
		t.Fatalf("stored accounts = %d, want 1", len(s.rows))
	}
	row := s.rows[0]
	s.mu.Unlock()
	if row.APIKey != testOfficeAceAppKey || row.AppSecret != testOfficeAceAppSecret {
		t.Fatal("the login did not persist the model gateway app key pair")
	}
	if row.BaseURL != srv.URL+"/model-gateway/v2" {
		t.Fatalf("account gateway = %q", row.BaseURL)
	}
	if strings.Join(row.Models, ",") != "test-glm,test-deepseek" {
		t.Fatalf("account models = %v", row.Models)
	}

	// The panel's 扫描模型 button reloads the platform config and then calls
	// RefreshModels.  That path must keep using the account's own gateway.
	if err := s.ConfigureSource(core.SourceConfig{BaseURL: "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2"}); err != nil {
		t.Fatalf("reload source: %v", err)
	}
	if _, err := s.RefreshModels(ctx); err != nil {
		t.Fatalf("refresh models: %v", err)
	}
	if scanCalls.Load() != 2 {
		t.Fatalf("model scans = %d, want 2 (account gateway was replaced)", scanCalls.Load())
	}
}

func TestOfficeAceLoginSessionTimeout(t *testing.T) {
	s := &source{now: func() time.Time { return time.Unix(int64((11*time.Minute)/time.Second), 0) }}
	o := &officeAceSource{source: s}
	session := &officeAceLoginSession{id: "expired", state: "expired", startedAt: time.Unix(0, 0), status: core.LoginPending}
	o.putLogin(session)
	got, err := o.PollLogin(context.Background(), "expired")
	if err != nil || got.State != core.LoginFailed {
		t.Fatalf("expired session = %+v, %v", got, err)
	}
}
