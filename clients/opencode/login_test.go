package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// oauthTestServer stands in for OpenCode Console: the device-code endpoints
// and the workspace calls the login flow makes after approval.
type oauthTestServer struct {
	t             *testing.T
	approved      bool
	tokenCalls    int
	refreshTokens string
	configCalls   int
}

func (s *oauthTestServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/console/auth/device/code", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, map[string]any{
			"device_code":               "dev-1",
			"user_code":                 "ABCD-1234",
			"verification_uri_complete": "https://opencode.ai/console/device?code=ABCD-1234",
			"expires_in":                600,
			"interval":                  1,
		})
	})
	mux.HandleFunc("/console/auth/device/token", func(w http.ResponseWriter, r *http.Request) {
		s.tokenCalls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] == "refresh_token" {
			writeJSONTest(w, map[string]any{
				"access_token":  "access-refreshed",
				"refresh_token": "refresh-refreshed",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
			return
		}
		if !s.approved {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":             "authorization_pending",
				"error_description": "waiting for approval",
			})
			return
		}
		writeJSONTest(w, map[string]any{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	})
	mux.HandleFunc("/console/api/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-1" && r.Header.Get("Authorization") != "Bearer access-refreshed" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSONTest(w, map[string]any{"id": "user-1", "email": "user@example.test"})
	})
	mux.HandleFunc("/console/api/orgs", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, []map[string]any{{"id": "org-1", "name": "Org One"}})
	})
	mux.HandleFunc("/console/api/config", func(w http.ResponseWriter, r *http.Request) {
		s.configCalls++
		if r.Header.Get("x-org-id") != "org-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSONTest(w, map[string]any{"config": map[string]any{"provider": map[string]any{"opencode": map[string]any{"models": map[string]any{
			"gpt-5.1":          map[string]any{"cost": map[string]any{"input": 1, "output": 2}},
			"space-bunny-free": map[string]any{"cost": map[string]any{"input": 0, "output": 0}},
			"retired-model":    map[string]any{"disabled": true},
		}}}}})
	})
	return mux
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestChatRefreshesAnExpiringOAuthToken proves the chat path refreshes a
// stale OAuth token before it talks to the inference endpoint.
func TestChatRefreshesAnExpiringOAuthToken(t *testing.T) {
	var chatAuth string
	c := newFakeClient(t, Config{AuthBaseURL: "https://console.test"}, func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/device/token"):
			return jsonResponse(200, `{"access_token":"fresh","refresh_token":"r2","expires_in":3600}`), nil
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatAuth = r.Header.Get("Authorization")
			return sseResponse(happySSE), nil
		}
		return jsonResponse(http.StatusNotFound, `{}`), nil
	})
	c.pool.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "stale",
		RefreshToken: "r1", OrgID: "org-1",
		ExpiresAt:     testNow.Add(-time.Minute).Format(time.RFC3339),
		AllowedModels: []string{"gpt-5.1"},
		Enabled:       true, Source: sourcePanel,
	})

	stream, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	if chatAuth != "Bearer fresh" {
		t.Fatalf("chat Authorization = %q, want the refreshed token", chatAuth)
	}
	stored, _ := c.pool.byID("opencode:oauth")
	if stored.AccessToken != "fresh" {
		t.Fatalf("stored token = %q, want the refreshed token", stored.AccessToken)
	}
}

// TestOAuthLoginDeviceFlow drives the whole device-code flow: pending, then
// success, producing an OAuth account with the workspace's allowed models.
func TestOAuthLoginDeviceFlow(t *testing.T) {
	srv := &oauthTestServer{t: t}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := newTestClient(t, Config{AuthBaseURL: ts.URL + "/console"})
	c.deps.DataDir = t.TempDir()

	st, err := c.StartLoginRealm(context.Background(), realmOAuth)
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	if st.State != core.LoginPending || st.SessionID == "" {
		t.Fatalf("state = %+v, want pending with a session id", st)
	}
	if !strings.Contains(st.URL, "verification_uri") && !strings.Contains(st.URL, "opencode.ai") {
		t.Fatalf("URL = %q, want the verification URL", st.URL)
	}
	if st.Code != "ABCD-1234" {
		t.Fatalf("Code = %q, want the user code", st.Code)
	}

	pending, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin pending: %v", err)
	}
	if pending.State != core.LoginPending {
		t.Fatalf("state = %+v, want pending", pending)
	}

	srv.approved = true
	done, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin success: %v", err)
	}
	if done.State != core.LoginSuccess || done.AccountID == "" {
		t.Fatalf("state = %+v, want success with an account id", done)
	}
	acct, ok := c.pool.byID(done.AccountID)
	if !ok {
		t.Fatalf("account %q was not stored", done.AccountID)
	}
	if acct.AuthMode != "oauth" || acct.AccessToken != "access-1" || acct.RefreshToken != "refresh-1" {
		t.Fatalf("account = %+v, want the OAuth tokens", acct)
	}
	if acct.OrgID != "org-1" || acct.Email != "user@example.test" {
		t.Fatalf("account = %+v, want the org and email", acct)
	}
	if !containsString(acct.AllowedModels, "gpt-5.1") {
		t.Fatalf("AllowedModels = %v, want gpt-5.1", acct.AllowedModels)
	}
	if containsString(acct.AllowedModels, "space-bunny-free") {
		t.Fatalf("AllowedModels = %v, must exclude zero-cost models", acct.AllowedModels)
	}
}

// TestOAuthLoginResolvesARelativeVerificationURL proves the console's relative
// verification path (the shape the live vendor actually returns) is made
// absolute before it reaches the panel, which would otherwise open it against
// the gateway's own origin.
func TestOAuthLoginResolvesARelativeVerificationURL(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/console/auth/device/code" {
			http.NotFound(w, r)
			return
		}
		writeJSONTest(w, map[string]any{
			"device_code":               "dev-1",
			"user_code":                 "ABCD-1234",
			"verification_uri_complete": "/console/device?user_code=ABCD-1234",
			"expires_in":                600,
			"interval":                  1,
		})
	}))
	defer ts.Close()

	c := newTestClient(t, Config{AuthBaseURL: ts.URL + "/console"})
	c.deps.DataDir = t.TempDir()
	st, err := c.StartLoginRealm(context.Background(), realmOAuth)
	if err != nil {
		t.Fatalf("StartLoginRealm(oauth): %v", err)
	}
	want := ts.URL + "/console/device?user_code=ABCD-1234"
	if st.URL != want {
		t.Fatalf("URL = %q, want %q", st.URL, want)
	}
}

// TestOAuthLoginCancel proves CancelLogin stops a session without storing
// anything.
func TestOAuthLoginCancel(t *testing.T) {
	srv := &oauthTestServer{t: t}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := newTestClient(t, Config{AuthBaseURL: ts.URL + "/console"})
	c.deps.DataDir = t.TempDir()

	st, err := c.StartLoginRealm(context.Background(), realmOAuth)
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	cancelled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if cancelled.State != core.LoginCancelled {
		t.Fatalf("state = %+v, want cancelled", cancelled)
	}
	if got := len(c.pool.snapshot()); got != 0 {
		t.Fatalf("accounts = %d, want none after a cancel", got)
	}
}

// TestOAuthRefreshUpdatesTokenWithoutChangingID proves an expiring OAuth
// credential is refreshed in place.
func TestOAuthRefreshUpdatesTokenWithoutChangingID(t *testing.T) {
	srv := &oauthTestServer{t: t}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	c := newTestClient(t, Config{AuthBaseURL: ts.URL + "/console"})
	c.deps.DataDir = t.TempDir()
	id := "opencode:oauth:user-1"
	c.pool.upsert(accountRecord{
		ID: id, AuthMode: "oauth", AccessToken: "access-old",
		RefreshToken: "refresh-old", OrgID: "org-1",
		ExpiresAt: testNow.Add(-time.Minute).Format(time.RFC3339),
		Enabled:   true, Source: sourcePanel,
	})
	acct, _ := c.pool.byID(id)

	if err := c.ensureFreshOAuth(context.Background(), acct); err != nil {
		t.Fatalf("ensureFreshOAuth: %v", err)
	}
	got, ok := c.pool.byID(id)
	if !ok {
		t.Fatal("the account id changed")
	}
	if got.AccessToken != "access-refreshed" || got.RefreshToken != "refresh-refreshed" {
		t.Fatalf("account = %+v, want refreshed tokens", got)
	}
	if got.OrgID != "org-1" {
		t.Fatalf("OrgID = %q, want it preserved", got.OrgID)
	}
	if acct.AccessToken != "access-refreshed" {
		t.Fatalf("the caller's copy was not updated: %+v", acct)
	}
}

// newLoginTestClient is a hermetic client whose credentials live in a temp dir,
// so a login can persist without touching the operator's real data directory.
func newLoginTestClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()
	return c
}

// TestLoginRealmsListsFreeAndOAuth proves the panel gets a picker with both
// credential kinds this module can add.
func TestLoginRealmsListsFreeAndOAuth(t *testing.T) {
	c := newLoginTestClient(t)
	realms := c.LoginRealms(context.Background())
	if len(realms) != 2 {
		t.Fatalf("realms = %+v, want free and oauth", realms)
	}
	if realms[0].Code != realmFree || realms[1].Code != realmOAuth {
		t.Fatalf("realm codes = %q, %q; want %q, %q",
			realms[0].Code, realms[1].Code, realmFree, realmOAuth)
	}
}

// TestAnonymousLoginCreatesPublicAccount is the whole point of the "free"
// realm: one click stores the anonymous credential the free model needs.
func TestAnonymousLoginCreatesPublicAccount(t *testing.T) {
	c := newLoginTestClient(t)
	st, err := c.StartLoginRealm(context.Background(), realmFree)
	if err != nil {
		t.Fatalf("StartLoginRealm: %v", err)
	}
	if st.State != core.LoginSuccess || st.AccountID == "" {
		t.Fatalf("state = %+v, want success with an account id", st)
	}
	acct, ok := c.pool.byID(st.AccountID)
	if !ok {
		t.Fatalf("account %q was not stored", st.AccountID)
	}
	if acct.AuthMode != "anonymous" || acct.APIKey != "public" {
		t.Fatalf("account = %+v, want an anonymous public credential", acct)
	}
	if !acct.Enabled {
		t.Fatal("the anonymous account is not enabled")
	}
}

// TestAnonymousLoginIsIdempotent proves a second click does not stack duplicate
// rows in the panel.
func TestAnonymousLoginIsIdempotent(t *testing.T) {
	c := newLoginTestClient(t)
	first, err := c.StartLoginRealm(context.Background(), realmFree)
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	second, err := c.StartLoginRealm(context.Background(), realmFree)
	if err != nil {
		t.Fatalf("second login: %v", err)
	}
	if first.AccountID != second.AccountID {
		t.Fatalf("ids = %q then %q, want the same stable id", first.AccountID, second.AccountID)
	}
	if got := len(c.pool.snapshot()); got != 1 {
		t.Fatalf("accounts = %d, want 1", got)
	}
}

// TestAnonymousLoginClearsAPriorFailure proves a re-add recovers an account
// that a vendor refusal disabled, instead of leaving a stale "cooling" row.
func TestAnonymousLoginClearsAPriorFailure(t *testing.T) {
	c := newLoginTestClient(t)
	st, err := c.StartLoginRealm(context.Background(), realmFree)
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	// Simulate the vendor refusing the credential on a paid model.
	c.pool.setEnabled(st.AccountID, false)
	c.pool.noteError(st.AccountID, "AuthError: Missing API key.", c.now(), 5)

	if _, err := c.StartLoginRealm(context.Background(), realmFree); err != nil {
		t.Fatalf("second login: %v", err)
	}
	acct, ok := c.pool.byID(st.AccountID)
	if !ok {
		t.Fatalf("account %q disappeared", st.AccountID)
	}
	if !acct.Enabled {
		t.Fatal("a re-added free account is still disabled")
	}
	if acct.LastError != "" || acct.ErrCount != 0 {
		t.Fatalf("account = %+v, want the failure state cleared", acct)
	}
	if got := stateOf(acct, c.now()); got != "ready" {
		t.Fatalf("state = %q, want ready", got)
	}
}

// TestStartLoginUsesTheConfiguredDefaultRealm proves StartLogin (no realm)
// honours default_realm.
func TestStartLoginUsesTheConfiguredDefaultRealm(t *testing.T) {
	c := newTestClient(t, Config{DefaultRealm: realmFree})
	c.deps.DataDir = t.TempDir()
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Fatalf("state = %+v, want success", st)
	}
	acct, _ := c.pool.byID(st.AccountID)
	if acct == nil || acct.AuthMode != "anonymous" {
		t.Fatalf("account = %+v, want anonymous", acct)
	}
}
