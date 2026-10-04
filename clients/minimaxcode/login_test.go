package minimaxcode

// login_test.go drives the panel's MiniMax Code sign-in against a scripted
// transport.  No test in this file touches the network: the device-code and
// token endpoints are answered by the same fakeTransport the rest of the
// package's tests use, so what is asserted here is the module's own behaviour,
// not the vendor's availability.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// loginConfig points the sign-in at a host the fake transport answers.  The
// device URL is spelled out rather than derived so a change to the derivation
// rule cannot silently send a test to the real vendor.
func loginConfig() map[string]any {
	return map[string]any{
		"oauth_device_url": "https://accounts.test/oauth2/device/code",
		"oauth_token_url":  "https://accounts.test/oauth2/token",
	}
}

// forcePoll rewinds a session's rate limiter so the next PollLogin talks to the
// vendor instead of answering from the session.  The limiter itself is covered
// by its own assertion; every other test wants the exchange now, not after the
// vendor's interval has elapsed.
func forcePoll(t *testing.T, c *Client, session string) {
	t.Helper()
	s, ok := c.getLogin(session)
	if !ok {
		t.Fatalf("no sign-in session %q to force", session)
	}
	s.mu.Lock()
	s.nextPoll = time.Now().Add(-time.Second)
	s.mu.Unlock()
}

// managedRow reads back the panel's own store, which is what survives a
// restart and what the renewal path depends on.
func managedRow(t *testing.T, c *Client, id string) managedAccount {
	t.Helper()
	raw, err := os.ReadFile(c.pool.path(managedFile))
	if err != nil {
		t.Fatalf("read %s: %v", managedFile, err)
	}
	var store managedStore
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatalf("decode %s: %v", managedFile, err)
	}
	for _, a := range store.Accounts {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("%s holds no row %q: %s", managedFile, id, raw)
	return managedAccount{}
}

// TestLoginDeviceFlowAdoptsTheCredential is the whole happy path: ask for a
// device code, hand the operator the URL and code, rate-limit the panel's
// faster poll, then trade an approval for a credential and persist the refresh
// half so the row can renew itself later.
func TestLoginDeviceFlowAdoptsTheCredential(t *testing.T) {
	var mu sync.Mutex
	tokenCalls := 0
	c, ft := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/oauth2/device/code":
			return jsonResponse(http.StatusOK,
				`{"user_code":"ABCD-EFGH","device_code":"dev-123",`+
					`"verification_uri":"https://account.minimax.cn/device",`+
					`"expires_in":600,"interval":1}`), nil
		case "/oauth2/token":
			mu.Lock()
			tokenCalls++
			n := tokenCalls
			mu.Unlock()
			if n == 1 {
				return jsonResponse(http.StatusOK, `{"status":"pending"}`), nil
			}
			return jsonResponse(http.StatusOK,
				`{"access_token":"mmoat_login","refresh_token":"mmrt_login",`+
					`"token_type":"Bearer","expires_in":3600}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{"error":"not_found"}`), nil
	})

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Errorf("StartLogin state = %q, want pending", st.State)
	}
	if st.SessionID == "" {
		t.Fatal("StartLogin returned no session id")
	}
	if st.URL != "https://account.minimax.cn/device" {
		t.Errorf("StartLogin url = %q", st.URL)
	}
	if st.Code != "ABCD-EFGH" {
		t.Errorf("StartLogin code = %q, want the user code", st.Code)
	}

	// The device request is the module's own, so it must carry the desktop
	// client's identity and a PKCE challenge, not an empty form.
	body := ft.bodyAt(0)
	for _, want := range []string{
		"client_id=mcode-public",
		"scope=agent.default",
		"audience=agent-backend",
		"code_challenge=",
		"code_challenge_method=S256",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("device request is missing %q: %s", want, body)
		}
	}

	// The panel polls every two seconds, which is faster than the vendor asked
	// for; an early poll must answer from the session rather than mint a
	// request the vendor would answer with slow_down.
	before := ft.count()
	early, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (early): %v", err)
	}
	if early.State != core.LoginPending {
		t.Errorf("early poll state = %q, want pending", early.State)
	}
	if ft.count() != before {
		t.Errorf("an early poll reached the vendor: %d requests, want %d", ft.count(), before)
	}

	forcePoll(t, c, st.SessionID)
	pending, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (pending): %v", err)
	}
	if pending.State != core.LoginPending {
		t.Errorf("pending poll state = %q, want pending", pending.State)
	}
	if tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1", tokenCalls)
	}

	forcePoll(t, c, st.SessionID)
	done, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (success): %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s), want success", done.State, done.Message)
	}
	if done.AccountID == "" {
		t.Error("successful poll returned no account id")
	}

	// The exchange names the device code and proves the verifier, otherwise the
	// vendor would refuse it.
	token := ft.bodyAt(1)
	for _, want := range []string{
		"device_code=dev-123",
		"grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Adevice_code",
		"code_verifier=",
		"client_id=mcode-public",
	} {
		if !strings.Contains(token, want) {
			t.Errorf("token request is missing %q: %s", want, token)
		}
	}

	row := managedRow(t, c, done.AccountID)
	if row.Token != "mmoat_login" {
		t.Errorf("stored token = %q", row.Token)
	}
	if row.RefreshToken != "mmrt_login" {
		t.Errorf("stored refresh token = %q, want the one just issued", row.RefreshToken)
	}
	if row.ClientID != oauthClientIDDesktop {
		t.Errorf("stored client id = %q", row.ClientID)
	}
	if row.ExpiresAt == "" {
		t.Errorf("stored expiry is empty, so the row cannot be renewed on schedule")
	}

	// The panel reads the row back through the pool, and it must announce that
	// it is renewable, because that is what puts 重登 in front of a stale row.
	var found bool
	for _, rec := range c.pool.recordsForPanel() {
		if rec.ID != done.AccountID {
			continue
		}
		found = true
		if rec.Fields["refreshable"] != true {
			t.Errorf("row %s does not report itself refreshable: %v", rec.ID, rec.Fields)
		}
	}
	if !found {
		t.Errorf("pool has no row for %s", done.AccountID)
	}

	// A finished session is swept when the next sign-in starts: the panel stops
	// polling at the terminal state, so nothing else would ever collect it.
	if _, err := c.StartLogin(context.Background()); err != nil {
		t.Fatalf("second StartLogin: %v", err)
	}
	if _, ok := c.getLogin(st.SessionID); ok {
		t.Error("the finished session survived the next sign-in's sweep")
	}
}

// TestLoginCNUserCodeVariant covers the shape the CN service has been observed
// to answer with: no device code, an expiry in milliseconds, and polling keyed
// by the user code.  Polling with an empty device_code would fail on exactly
// the accounts this module exists for.
func TestLoginCNUserCodeVariant(t *testing.T) {
	c, ft := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/oauth2/device/code":
			return jsonResponse(http.StatusOK,
				`{"user_code":"CN-1234","verification_uri":"https://account.minimax.cn/device",`+
					`"expired_in":600000,"interval":1000}`), nil
		case "/oauth2/token":
			return jsonResponse(http.StatusOK,
				`{"access_token":"mmoat_cn","refresh_token":"mmrt_cn","expires_in":1200}`), nil
		}
		return jsonResponse(http.StatusNotFound, `{"error":"not_found"}`), nil
	})

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.Code != "CN-1234" {
		t.Errorf("StartLogin code = %q", st.Code)
	}

	forcePoll(t, c, st.SessionID)
	done, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s), want success", done.State, done.Message)
	}

	token := ft.bodyAt(1)
	if !strings.Contains(token, "user_code=CN-1234") {
		t.Errorf("CN variant did not poll by user code: %s", token)
	}
	if strings.Contains(token, "device_code=") {
		t.Errorf("CN variant sent a device code it was never given: %s", token)
	}
}

// TestLoginRejectsAVendorRefusal: an error from the authorization endpoint is
// the operator's problem to fix, so it must surface as a start error and not as
// a session that polls forever.
func TestLoginRejectsAVendorRefusal(t *testing.T) {
	c, _ := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"error":"invalid_client","error_description":"unknown application"}`), nil
	})

	if _, err := c.StartLogin(context.Background()); err == nil {
		t.Fatal("StartLogin accepted a refused device authorization")
	}
}

// TestLoginPollRejectsAnUnknownSession: the panel may poll a session that never
// existed or was already swept, and it must hear about that rather than read a
// fabricated success.
func TestLoginPollRejectsAnUnknownSession(t *testing.T) {
	c, _ := newTestClient(t, loginConfig(), nil)

	if _, err := c.PollLogin(context.Background(), "minimaxcode-login-nope"); err == nil {
		t.Error("PollLogin accepted an unknown session")
	}
	if err := c.CancelLogin(context.Background(), "minimaxcode-login-nope"); err != nil {
		t.Errorf("CancelLogin refused an unknown session: %v", err)
	}
}

// TestLoginCancelForgetsTheSession pins the documented contract: cancelling is
// idempotent and removes the session, so a later poll cannot resurrect it.
func TestLoginCancelForgetsTheSession(t *testing.T) {
	c, _ := newTestClient(t, loginConfig(), func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK,
			`{"user_code":"ABCD-EFGH","device_code":"dev-123",`+
				`"verification_uri":"https://account.minimax.cn/device","expires_in":600}`), nil
	})

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	if _, ok := c.getLogin(st.SessionID); ok {
		t.Error("cancelled session is still in the table")
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Errorf("second CancelLogin is not idempotent: %v", err)
	}
}
