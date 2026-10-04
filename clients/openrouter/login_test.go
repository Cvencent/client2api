package openrouter

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- PKCE login -----------------------------------------------------------

// callbackOf pulls the callback_url out of an authorization URL, which is how
// the test learns where the loopback listener is actually bound.
func callbackOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorization URL %q does not parse: %v", authURL, err)
	}
	cb := u.Query().Get("callback_url")
	if cb == "" {
		t.Fatalf("authorization URL %q carries no callback_url", authURL)
	}
	if !strings.HasPrefix(cb, "http://127.0.0.1:") {
		t.Fatalf("callback_url = %q, want a loopback listener", cb)
	}
	return cb
}

func stateOf(t *testing.T, authURL string) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("authorization URL %q does not parse: %v", authURL, err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("authorization URL %q carries no state", authURL)
	}
	return state
}

func getCallback(t *testing.T, cb, query string) *http.Response {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	sep := "?"
	if strings.Contains(cb, "?") {
		sep = "&"
	}
	resp, err := client.Get(cb + sep + query)
	if err != nil {
		t.Fatalf("GET %s%s%s: %v", cb, sep, query, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestLoginAuthorizationURLCarriesPKCE(t *testing.T) {
	c := newTestClient(t, "", nil)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	if st.SessionID == "" {
		t.Fatal("StartLogin returned no session id")
	}
	u, err := url.Parse(st.URL)
	if err != nil {
		t.Fatalf("authorization URL %q does not parse: %v", st.URL, err)
	}
	if u.Host != "openrouter.ai" || u.Path != "/auth" {
		t.Fatalf("authorization URL host/path = %q%s, want openrouter.ai/auth", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("callback_url") == "" {
		t.Fatal("authorization URL carries no callback_url")
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		t.Fatal("authorization URL carries no code_challenge")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if strings.ContainsAny(challenge, " +/=") {
		t.Fatalf("code_challenge = %q, want base64url without padding", challenge)
	}
	if q.Get("state") == "" {
		t.Fatal("authorization URL carries no state")
	}
}

func TestLoginRejectsAWrongState(t *testing.T) {
	c := newTestClient(t, "", nil)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	cb := callbackOf(t, st.URL)

	resp := getCallback(t, cb, "code=abc&state=not-the-state")
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("callback with a wrong state answered %d, want a refusal", resp.StatusCode)
	}

	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed", polled.State)
	}
}

func TestLoginExchangesTheCodeForAKey(t *testing.T) {
	up := &fakeUpstream{handle: func(req *http.Request, body string) reply {
		if req.Method != http.MethodPost || req.URL.Path != "/api/v1/auth/keys" {
			return reply{status: http.StatusNotFound, body: `{"error":"unexpected path"}`}
		}
		return reply{status: http.StatusOK, body: fmt.Sprintf(`{"key":%q}`, testKey)}
	}}
	c := newTestClient(t, "", up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cb := callbackOf(t, st.URL)
	state := stateOf(t, st.URL)

	resp := getCallback(t, cb, "code=auth-code&state="+state)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback answered %d, want 200", resp.StatusCode)
	}

	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success", polled.State, polled.Message)
	}
	if polled.AccountID == "" {
		t.Fatal("a successful login returned no account id")
	}
	rec, ok := c.pool.byID(polled.AccountID)
	if !ok {
		t.Fatalf("account %q is not in the pool", polled.AccountID)
	}
	if rec.APIKey != testKey {
		t.Fatalf("stored key = %q, want the key the vendor returned", rec.APIKey)
	}
	if rec.Source != sourcePanel {
		t.Fatalf("source = %q, want %q", rec.Source, sourcePanel)
	}
	if up.count() != 1 {
		t.Fatalf("upstream saw %d requests, want exactly 1 exchange", up.count())
	}
	body := up.jsonAt(t, 0)
	if body["code"] != "auth-code" {
		t.Fatalf("exchange code = %v, want auth-code", body["code"])
	}
	if verifier, _ := body["code_verifier"].(string); verifier == "" {
		t.Fatal("exchange carried no code_verifier")
	}
	if body["code_challenge_method"] != "S256" {
		t.Fatalf("exchange code_challenge_method = %v, want S256", body["code_challenge_method"])
	}
}

func TestLoginExchangeFailureIsReported(t *testing.T) {
	up := &fakeUpstream{handle: func(req *http.Request, body string) reply {
		return reply{status: http.StatusBadRequest, body: `{"error":{"message":"code expired"}}`}
	}}
	c := newTestClient(t, "", up)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cb := callbackOf(t, st.URL)
	state := stateOf(t, st.URL)
	getCallback(t, cb, "code=auth-code&state="+state)

	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed", polled.State)
	}
	if c.pool.len() != 0 {
		t.Fatalf("a failed exchange stored %d account(s)", c.pool.len())
	}
}

func TestLoginPollBeforeTheCallbackStaysPending(t *testing.T) {
	c := newTestClient(t, "", nil)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	t.Cleanup(func() { _ = c.CancelLogin(context.Background(), st.SessionID) })
	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", polled.State)
	}
}

func TestCancelLoginStopsTheListener(t *testing.T) {
	c := newTestClient(t, "", nil)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cb := callbackOf(t, st.URL)
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("second CancelLogin: %v", err)
	}
	polled, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after cancel: %v", err)
	}
	if polled.State != core.LoginCancelled {
		t.Fatalf("state = %q, want cancelled", polled.State)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(cb + "?code=x&state=y")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the loopback listener answered after cancel with %d", resp.StatusCode)
	}
}

func TestUnknownSessionIsRefused(t *testing.T) {
	c := newTestClient(t, "", nil)
	if _, err := c.PollLogin(context.Background(), "nope"); err == nil {
		t.Fatal("PollLogin accepted an unknown session id")
	}
	if err := c.CancelLogin(context.Background(), "nope"); err == nil {
		t.Fatal("CancelLogin accepted an unknown session id")
	}
}
