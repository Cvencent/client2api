package kimi

// Tests for renewing a panel login.
//
// The device flow hands back a short-lived access token and a long-lived refresh
// token.  Without renewal the account the operator just created dies within the
// hour and the panel tells them to sign in again, which reads as the login not
// having worked at all.  These tests pin the renewal contract:
//
//   - the refresh token is what is presented, never the access token;
//   - a refused renewal never empties the account;
//   - a grant with no stated expiry is left alone;
//   - the renewed grant is persisted, because a rotating vendor invalidates the
//     stored refresh token the moment it answers.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// refreshServer stands in for the vendor's token endpoint and records what the
// module actually sent, so the request can be asserted on rather than assumed.
type refreshServer struct {
	srv    *httptest.Server
	status int
	body   string

	mu       sync.Mutex
	requests int
	form     url.Values
	auth     string
}

func newRefreshServer(t *testing.T, status int, body string) *refreshServer {
	t.Helper()

	rs := &refreshServer{status: status, body: body}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != deviceTokenPath {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()

		rs.mu.Lock()
		rs.requests++
		rs.form = r.PostForm
		rs.auth = r.Header.Get("Authorization")
		rs.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if rs.status != 0 {
			w.WriteHeader(rs.status)
		}
		_, _ = io.WriteString(w, rs.body)
	}))
	t.Cleanup(rs.srv.Close)

	return rs
}

func (rs *refreshServer) snapshot() (int, url.Values, string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.requests, rs.form, rs.auth
}

// seededClient builds a client whose stored grant is in a known state.
func seededClient(t *testing.T, host string, grant storedToken) *Client {
	t.Helper()
	isolateCredentials(t)
	noCLIOnPath(t)

	c, _ := newClient(t, map[string]any{"oauth_host": host})
	grant.OAuthHost = host
	if err := c.saveToken(grant); err != nil {
		t.Fatalf("seeding the stored grant: %v", err)
	}
	return c
}

// expiredGrant is a grant that is past its stated expiry but still renewable.
func expiredGrant() storedToken {
	return storedToken{
		AccessToken:  "tok-old",
		RefreshToken: "ref-old",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
		Scope:        "coding",
	}
}

const renewedToken = `{"access_token":"tok-new","refresh_token":"ref-new","expires_in":1800,"token_type":"Bearer","scope":"coding"}`

// The renewal must present the refresh token - not the access token - and it
// must persist what came back, or a rotating vendor strands the operator.
func TestTokenRenewalUsesTheStoredRefreshToken(t *testing.T) {
	rs := newRefreshServer(t, http.StatusOK, renewedToken)
	c := seededClient(t, rs.srv.URL, expiredGrant())

	tok, ok, err := c.freshToken(context.Background())
	if err != nil {
		t.Fatalf("freshToken: %v", err)
	}
	if !ok {
		t.Fatal("freshToken reported no credential for a stored grant")
	}
	if tok.AccessToken != "tok-new" {
		t.Errorf("access_token = %q, want %q", tok.AccessToken, "tok-new")
	}
	if tok.RefreshToken != "ref-new" {
		t.Errorf("refresh_token = %q, want the rotated %q", tok.RefreshToken, "ref-new")
	}
	if tok.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expires_at = %d, which is not in the future", tok.ExpiresAt)
	}

	requests, form, auth := rs.snapshot()
	if requests != 1 {
		t.Fatalf("the vendor was asked %d times, want 1", requests)
	}
	if got := form.Get("grant_type"); got != refreshGrantType {
		t.Errorf("grant_type = %q, want %q", got, refreshGrantType)
	}
	if got := form.Get("refresh_token"); got != "ref-old" {
		t.Errorf("refresh_token = %q, want %q", got, "ref-old")
	}
	if got := form.Get("client_id"); got != kimiCodeClientID {
		t.Errorf("client_id = %q, want %q", got, kimiCodeClientID)
	}
	// A device-code client has no secret; presenting the access token as a
	// bearer credential would be a different, wrong grant.
	if auth != "" {
		t.Errorf("the refresh request carried Authorization: %q", auth)
	}

	stored, ok := c.loadToken()
	if !ok {
		t.Fatal("the renewed grant was not persisted")
	}
	if stored.AccessToken != "tok-new" {
		t.Errorf("stored access_token = %q, want the renewed %q", stored.AccessToken, "tok-new")
	}
}

// A vendor that refuses the renewal must not cost the operator the account: the
// grant stays on disk and is still handed back, and the error says why.
func TestTokenRenewalKeepsTheGrantWhenTheVendorRefuses(t *testing.T) {
	rs := newRefreshServer(t, http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"refresh token is no longer valid"}`)
	c := seededClient(t, rs.srv.URL, expiredGrant())

	tok, ok, err := c.freshToken(context.Background())
	if err == nil {
		t.Fatal("a refused renewal was reported as a success")
	}
	if !ok {
		t.Error("a refused renewal dropped the account instead of leaving it to the vendor to judge")
	}
	if tok.AccessToken != "tok-old" {
		t.Errorf("access_token = %q; a refused renewal must return the stored grant", tok.AccessToken)
	}
	if !strings.Contains(err.Error(), "refresh token is no longer valid") {
		t.Errorf("the refusal did not explain itself: %v", err)
	}
	if !strings.Contains(err.Error(), "sign in again") {
		t.Errorf("the refusal did not say what to do: %v", err)
	}

	stored, ok := c.loadToken()
	if !ok {
		t.Fatal("the stored grant vanished after a refused renewal")
	}
	if stored.AccessToken != "tok-old" {
		t.Errorf("a refused renewal rewrote the grant to %q", stored.AccessToken)
	}
}

// A grant the vendor stated no expiry for has no deadline to anticipate, and
// renewing it would spend a refresh token on a credential that already works.
func TestTokenWithNoStatedExpiryIsNeverRenewed(t *testing.T) {
	rs := newRefreshServer(t, http.StatusOK, renewedToken)
	grant := expiredGrant()
	grant.ExpiresAt = 0
	c := seededClient(t, rs.srv.URL, grant)

	tok, ok, err := c.freshToken(context.Background())
	if err != nil {
		t.Fatalf("freshToken: %v", err)
	}
	if !ok || tok.AccessToken != "tok-old" {
		t.Fatalf("freshToken returned (%q, %v), want the stored grant", tok.AccessToken, ok)
	}
	if requests, _, _ := rs.snapshot(); requests != 0 {
		t.Errorf("a grant with no stated expiry was renewed %d time(s)", requests)
	}
}

// Renewal is anticipatory: it happens inside the skew, never far outside it.
func TestTokenIsRenewedOnlyInsideTheSkew(t *testing.T) {
	cases := []struct {
		name     string
		expires  time.Duration
		requests int
	}{
		{"comfortably valid", 30 * time.Minute, 0},
		{"inside the skew", tokenRefreshSkew - time.Minute, 1},
		{"already past", -time.Minute, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRefreshServer(t, http.StatusOK, renewedToken)
			grant := expiredGrant()
			grant.ExpiresAt = time.Now().Add(tc.expires).Unix()
			c := seededClient(t, rs.srv.URL, grant)

			if _, _, err := c.freshToken(context.Background()); err != nil {
				t.Fatalf("freshToken: %v", err)
			}
			if requests, _, _ := rs.snapshot(); requests != tc.requests {
				t.Errorf("the vendor was asked %d times, want %d", requests, tc.requests)
			}
		})
	}
}

// A server that rotates by omission (or repeats the old token) must not blank
// the refresh token, which would make the next renewal impossible.
func TestTokenRenewalSurvivesARotationWithoutANewRefreshToken(t *testing.T) {
	rs := newRefreshServer(t, http.StatusOK, `{"access_token":"tok-new","expires_in":1800}`)
	c := seededClient(t, rs.srv.URL, expiredGrant())

	tok, _, err := c.freshToken(context.Background())
	if err != nil {
		t.Fatalf("freshToken: %v", err)
	}
	if tok.RefreshToken != "ref-old" {
		t.Errorf("refresh_token = %q; an omitted rotation must keep the stored one", tok.RefreshToken)
	}
}

// With nothing to renew with there is no request to make, and that is not an
// error: the vendor still gets to judge the access token on the real call.
func TestTokenWithoutARefreshTokenIsNotRenewed(t *testing.T) {
	rs := newRefreshServer(t, http.StatusOK, renewedToken)
	grant := expiredGrant()
	grant.RefreshToken = ""
	c := seededClient(t, rs.srv.URL, grant)

	tok, ok, err := c.freshToken(context.Background())
	if err != nil {
		t.Fatalf("freshToken: %v", err)
	}
	if !ok || tok.AccessToken != "tok-old" {
		t.Fatalf("freshToken returned (%q, %v), want the stored grant", tok.AccessToken, ok)
	}
	if requests, _, _ := rs.snapshot(); requests != 0 {
		t.Errorf("a grant with no refresh token was renewed %d time(s)", requests)
	}
}

// The panel must not call a renewable account broken: it reports the renewal
// instead, and only says "sign in again" when nothing can renew it.
func TestTokenStatusDistinguishesRenewableFromDead(t *testing.T) {
	renewable := expiredGrant()
	c := seededClient(t, "https://auth.example.invalid", renewable)

	st, ok := c.tokenStatus()
	if !ok {
		t.Fatal("tokenStatus reported no account for a renewable grant")
	}
	if st.State != "ready" {
		t.Errorf("state = %q, want %q for a grant that renews itself", st.State, "ready")
	}
	if !strings.Contains(st.Note, "renewed") {
		t.Errorf("note %q does not mention the renewal", st.Note)
	}

	dead := expiredGrant()
	dead.RefreshToken = ""
	c2 := seededClient(t, "https://auth.example.invalid", dead)
	st2, ok := c2.tokenStatus()
	if !ok {
		t.Fatal("tokenStatus reported no account for the stored grant")
	}
	if st2.State != "invalid" {
		t.Errorf("state = %q, want %q for a grant nothing can renew", st2.State, "invalid")
	}
	if !strings.Contains(st2.Note, "sign in again") {
		t.Errorf("note %q does not say what to do", st2.Note)
	}
}

// Client status drives the panel's readiness chip, so a grant that merely needs
// renewing must not flicker the whole client to "not signed in".
func TestStatusStaysReadyWhileTheGrantCanStillBeRenewed(t *testing.T) {
	rs := newRefreshServer(t, http.StatusOK, renewedToken)
	c := seededClient(t, rs.srv.URL, expiredGrant())

	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Status reported ready=false (%s) for a renewable grant", st.Detail)
	}
	if !strings.Contains(st.Detail, "renewed") {
		t.Errorf("detail %q does not mention the renewal", st.Detail)
	}
	// Status is a cheap probe: it must not have renewed anything itself.
	if requests, _, _ := rs.snapshot(); requests != 0 {
		t.Errorf("Status made %d network request(s)", requests)
	}
}
