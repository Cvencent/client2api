package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// loginProvider narrows the client to the contract the panel drives.  Going
// through the interface keeps the tests honest: they exercise exactly what the
// panel sees, not an unexported method that happens to exist.
func loginProvider(t *testing.T, c *Client) core.LoginProvider {
	t.Helper()
	lp, ok := any(c).(core.LoginProvider)
	if !ok {
		t.Fatalf("qoder client does not implement core.LoginProvider; the panel has no browser login to offer")
	}
	return lp
}

// loginClient is a test client whose device flow talks to a local OpenAPI
// server while the authorisation pages keep the vendor's public host shape.
func loginClient(t *testing.T, openAPI, authBase string) *Client {
	t.Helper()
	c := testClient(t, openAPI)
	c.cfg.AuthBase = authBase
	c.deps = core.Deps{DataDir: t.TempDir()}
	return c
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestQoderAdvertisesBrowserLogin(t *testing.T) {
	c, err := New(core.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Login {
		t.Fatal("capabilities.login = false, want true for a module with browser login")
	}
}

func TestStartLoginBuildsBrowserDeviceURL(t *testing.T) {
	c := loginClient(t, "https://openapi.example", "https://qoder.example")
	lp := loginProvider(t, c)

	st, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want %q", st.State, core.LoginPending)
	}
	if strings.TrimSpace(st.SessionID) == "" {
		t.Fatal("StartLogin returned an empty session id")
	}

	signIn, err := url.Parse(st.URL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", st.URL, err)
	}
	if signIn.Scheme != "https" || signIn.Host != "qoder.example" || signIn.Path != "/users/sign-in" {
		t.Fatalf("sign-in URL = %q, want https://qoder.example/users/sign-in", st.URL)
	}
	q := signIn.Query()
	if q.Get("biz_variant") != authBizVariant {
		t.Fatalf("biz_variant = %q, want %q", q.Get("biz_variant"), authBizVariant)
	}

	device, err := url.Parse(q.Get("oauth_callback"))
	if err != nil {
		t.Fatalf("parse oauth_callback %q: %v", q.Get("oauth_callback"), err)
	}
	if device.Path != "/device/selectAccounts" {
		t.Fatalf("device path = %q, want /device/selectAccounts", device.Path)
	}
	dq := device.Query()
	if got := dq.Get("challenge_method"); got != "S256" {
		t.Fatalf("challenge_method = %q, want S256", got)
	}
	if got := dq.Get("client_id"); got != defaultAuthClientID {
		t.Fatalf("client_id = %q, want %q", got, defaultAuthClientID)
	}
	if strings.TrimSpace(dq.Get("nonce")) == "" {
		t.Fatal("device URL carries no nonce")
	}
	if !uuidRE.MatchString(dq.Get("machine_id")) {
		t.Fatalf("machine_id = %q, want a UUID", dq.Get("machine_id"))
	}
	if strings.TrimSpace(dq.Get("challenge")) == "" {
		t.Fatal("device URL carries no challenge")
	}
}

// TestPollLoginPendingThenSuccessStoresAccount walks the whole flow: a 404
// keeps the session pending, a token response lands the credential, and the
// PKCE verifier sent on the poll really is the pre-image of the challenge in
// the URL the operator opened.
func TestPollLoginPendingThenSuccessStoresAccount(t *testing.T) {
	var challenge string
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathDevicePoll:
			polls++
			verifier := r.URL.Query().Get("verifier")
			if verifier == "" {
				t.Errorf("poll carried no verifier")
			}
			if got := pkceChallenge(verifier); got != challenge {
				t.Errorf("poll verifier hashes to %q, but the URL challenge was %q", got, challenge)
			}
			if r.URL.Query().Get("challenge_method") != "S256" {
				t.Errorf("poll challenge_method = %q, want S256", r.URL.Query().Get("challenge_method"))
			}
			w.Header().Set("Content-Type", "application/json")
			if polls == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"token":"dt-new","refresh_token":"drt-new","expires_in":3600,"refresh_token_expires_in":7200}`))
		case pathUserInfo:
			if got := r.Header.Get("Authorization"); got != "Bearer dt-new" {
				t.Errorf("userinfo Authorization = %q, want Bearer dt-new", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"user-9","name":"Nine","security_mobile":"13800000000"}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	c := loginClient(t, server.URL, "https://qoder.example")
	lp := loginProvider(t, c)

	start, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	device, err := url.Parse(mustQuery(t, start.URL).Get("oauth_callback"))
	if err != nil {
		t.Fatalf("parse oauth_callback: %v", err)
	}
	challenge = device.Query().Get("challenge")

	pending, err := lp.PollLogin(context.Background(), start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin pending: %v", err)
	}
	if pending.State != core.LoginPending {
		t.Fatalf("first poll state = %q, want %q", pending.State, core.LoginPending)
	}

	done, err := lp.PollLogin(context.Background(), start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin success: %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("second poll state = %q, want %q (%s)", done.State, core.LoginSuccess, done.Message)
	}
	if done.AccountID != "qoder-user-9" {
		t.Fatalf("account_id = %q, want qoder-user-9", done.AccountID)
	}

	acc, ok := c.store.lookup("qoder-user-9")
	if !ok {
		t.Fatal("successful login did not store an account")
	}
	if acc.Token != "dt-new" || acc.RefreshToken != "drt-new" {
		t.Fatalf("stored credential = %q/%q, want dt-new/drt-new", acc.Token, acc.RefreshToken)
	}
	if acc.UserID != "user-9" || acc.Phone != "13800000000" || acc.Name != "Nine" {
		t.Fatalf("stored identity = %#v, want the userinfo answer", acc.storedAccount)
	}
	if acc.ExpiresAtMS <= time.Now().UnixMilli() {
		t.Fatalf("stored expiry = %d, want a future timestamp", acc.ExpiresAtMS)
	}
}

func TestPollLoginUnknownSession(t *testing.T) {
	c := loginClient(t, "https://openapi.example", "https://qoder.example")
	lp := loginProvider(t, c)
	if _, err := lp.PollLogin(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("PollLogin accepted an unknown session id")
	}
}

func TestCancelLoginStopsTheSession(t *testing.T) {
	c := loginClient(t, "https://openapi.example", "https://qoder.example")
	lp := loginProvider(t, c)

	start, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := lp.CancelLogin(context.Background(), start.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	st, err := lp.PollLogin(context.Background(), start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after cancel: %v", err)
	}
	if st.State != core.LoginCancelled {
		t.Fatalf("state = %q, want %q", st.State, core.LoginCancelled)
	}
	if err := lp.CancelLogin(context.Background(), "does-not-exist"); err == nil {
		t.Fatal("CancelLogin accepted an unknown session id")
	}
}

func TestPollLoginFailureStoresNoCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathDevicePoll {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	c := loginClient(t, server.URL, "https://qoder.example")
	lp := loginProvider(t, c)

	start, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	done, err := lp.PollLogin(context.Background(), start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", done.State, core.LoginFailed)
	}
	if got := c.store.count(); got != 0 {
		t.Fatalf("store holds %d accounts after a failed login, want 0", got)
	}
}

func TestMachineIDIsStableAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	c := loginClient(t, "https://openapi.example", "https://qoder.example")
	c.deps = core.Deps{DataDir: dir}
	lp := loginProvider(t, c)

	first, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	second, err := lp.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	id1 := deviceMachineID(t, first.URL)
	id2 := deviceMachineID(t, second.URL)
	if id1 == "" || id1 != id2 {
		t.Fatalf("machine_id changed between sessions: %q then %q", id1, id2)
	}

	// A fresh client over the same data dir must reuse the persisted id rather
	// than mint a new device identity on every panel restart.
	other := loginClient(t, "https://openapi.example", "https://qoder.example")
	other.deps = core.Deps{DataDir: dir}
	third, err := loginProvider(t, other).StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin on the second client: %v", err)
	}
	if got := deviceMachineID(t, third.URL); got != id1 {
		t.Fatalf("persisted machine_id = %q, want %q", got, id1)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}

func deviceMachineID(t *testing.T, signInURL string) string {
	t.Helper()
	device, err := url.Parse(mustQuery(t, signInURL).Get("oauth_callback"))
	if err != nil {
		t.Fatalf("parse oauth_callback: %v", err)
	}
	return device.Query().Get("machine_id")
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
