package trae

// weblogin_test.go — offline tests for the panel web OAuth login.
//
// Nothing here reaches the network.  The two upstream calls a login makes
// (ExchangeToken, GetUserInfo) are served by a fake http.RoundTripper, and the
// only sockets opened are the loopback listeners StartLogin itself binds —
// which is the feature under test.  No test dials a non-loopback address.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// Distinctive secrets so a leak assertion cannot pass by accident.
const (
	testLoginRefresh = "REFRESH-TOKEN-SECRET-abcdef0123456789"
	testLoginAccess  = "ACCESS-TOKEN-SECRET-fedcba9876543210"
	testLoginUID     = "u-login-1"
	testLoginNick    = "operator"
)

// 2100-01-01 in seconds, comfortably below the 1e12 millisecond threshold.
const loginFarFuture = 4102444800

var (
	loginExchangeOK   = `{"Result":{"Token":"` + testLoginAccess + `","RefreshToken":"` + testLoginRefresh + `","TokenExpireAt":` + fmt.Sprint(loginFarFuture) + `,"RefreshExpireAt":` + fmt.Sprint(loginFarFuture) + `}}`
	loginUserInfoOK   = `{"Result":{"UserID":"` + testLoginUID + `","ScreenName":"` + testLoginNick + `","EnterpriseID":"ent-9"}}`
	loginCallbackBody = `{"error":"boom"}`
)

// ---- transport -------------------------------------------------------------

// loginTransport answers the two endpoints a panel login touches and records
// every request so the tests can assert on the exact wire shape.
type loginTransport struct {
	exchangeStatus int
	exchangeBody   string
	userStatus     int
	userBody       string

	mu      sync.Mutex
	paths   []string
	bodies  map[string]string
	headers map[string]http.Header
}

func (lt *loginTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)

	lt.mu.Lock()
	lt.paths = append(lt.paths, r.URL.Path)
	if lt.bodies == nil {
		lt.bodies = map[string]string{}
		lt.headers = map[string]http.Header{}
	}
	lt.bodies[r.URL.Path] = string(raw)
	lt.headers[r.URL.Path] = r.Header.Clone()
	lt.mu.Unlock()

	switch r.URL.Path {
	case epExchange:
		if lt.exchangeStatus != 0 && lt.exchangeStatus != http.StatusOK {
			return jsonResponse(lt.exchangeStatus, loginCallbackBody), nil
		}
		return jsonResponse(http.StatusOK, firstNonEmpty(lt.exchangeBody, loginExchangeOK)), nil
	case epUserInfo:
		if lt.userStatus != 0 && lt.userStatus != http.StatusOK {
			return jsonResponse(lt.userStatus, loginCallbackBody), nil
		}
		return jsonResponse(http.StatusOK, firstNonEmpty(lt.userBody, loginUserInfoOK)), nil
	}
	return jsonResponse(http.StatusNotFound, `{}`), nil
}

func (lt *loginTransport) body(path string) string {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.bodies[path]
}

func (lt *loginTransport) header(path string) http.Header {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return lt.headers[path]
}

func (lt *loginTransport) calledPaths() []string {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	return append([]string(nil), lt.paths...)
}

// ---- helpers ---------------------------------------------------------------

// callbackQuery builds the query string the console redirects back with, in the
// shape callback.go:117 documents:
//
//	/authorize?refreshToken=...&userInfo={...}&userJwt={...}
func callbackQuery(refreshToken, jwtToken string) string {
	v := url.Values{}
	if refreshToken != "" {
		v.Set("refreshToken", refreshToken)
	}
	userInfo, _ := json.Marshal(map[string]any{
		"UserID":     testLoginUID,
		"ScreenName": testLoginNick,
		"TenantID":   "ent-9",
	})
	v.Set("userInfo", string(userInfo))
	userJwt, _ := json.Marshal(map[string]any{
		"Token":           jwtToken,
		"RefreshToken":    refreshToken,
		"TokenExpireAt":   loginFarFuture,
		"RefreshExpireAt": loginFarFuture,
	})
	v.Set("userJwt", string(userJwt))
	return v.Encode()
}

// callbackURLFromLoginURL pulls the loopback redirect target out of the URL the
// operator is handed — the only place the chosen port appears.
func callbackURLFromLoginURL(t *testing.T, loginURL string) string {
	t.Helper()
	u, err := url.Parse(loginURL)
	if err != nil {
		t.Fatalf("parse the login URL: %v", err)
	}
	cb := u.Query().Get("auth_callback_url")
	if cb == "" {
		t.Fatalf("the login URL carries no auth_callback_url: %s", loginURL)
	}
	return cb
}

func callbackHost(t *testing.T, loginURL string) string {
	t.Helper()
	u, err := url.Parse(callbackURLFromLoginURL(t, loginURL))
	if err != nil {
		t.Fatalf("parse the callback URL: %v", err)
	}
	return u.Host
}

// driveAuthorize feeds the session's own handler the request the browser would
// make, without opening a socket.
func driveAuthorize(t *testing.T, c *Client, sessionID, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	sess := c.loginSession(sessionID)
	if sess == nil {
		t.Fatalf("login session %q is not registered", sessionID)
	}
	req := httptest.NewRequest(http.MethodGet, loginCallbackPath+"?"+rawQuery, nil)
	req.Host = "127.0.0.1:18080"
	rec := httptest.NewRecorder()
	sess.srv.Handler.ServeHTTP(rec, req)
	return rec
}

// waitForLoginState polls until the session reaches want.  It fails fast on a
// terminal state that is not want, so a wrong outcome is reported as the actual
// message rather than a timeout.
func waitForLoginState(t *testing.T, c *Client, sessionID, want string, timeout time.Duration) core.LoginState {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	var last core.LoginState
	for time.Now().Before(deadline) {
		st, err := c.PollLogin(ctx, sessionID)
		if err != nil {
			t.Fatalf("PollLogin: %v", err)
		}
		last = st
		if st.State == want {
			return st
		}
		if st.State != core.LoginPending {
			t.Fatalf("login settled as %q, wanted %q: %s", st.State, want, st.Message)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("login never reached %q within %s (last: %q / %s)", want, timeout, last.State, last.Message)
	return last
}

// assertPortFree proves the listener was released: binding the same address
// again must succeed.
// loginAccounts unwraps the (records, error) pair that core.AccountManager
// requires, so the assertions below stay readable.
func loginAccounts(t *testing.T, c *Client) []core.AccountRecord {
	t.Helper()
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	return recs
}

// assertPortFree proves the callback listener is gone.  The release is not
// instantaneous: a poll can observe a TTL timeout (expireIfStale) in the same
// instant the session goroutine's own timer fires, so the test can get here
// before shutdown has run.  Retry briefly rather than flake the suite.
func assertPortFree(t *testing.T, hostPort string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var err error
	for {
		var ln net.Listener
		ln, err = net.Listen("tcp", hostPort)
		if err == nil {
			_ = ln.Close()
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the callback listener on %s was not released: %v", hostPort, err)
}

// ---- capability + config ---------------------------------------------------

func TestLoginProviderCapability(t *testing.T) {
	c := panelClient(t, nil, nil)

	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Login {
		t.Fatal("CapabilitiesOf reports Login = false for the trae client")
	}
	if _, ok := core.AsLoginProvider(c); !ok {
		t.Fatal("AsLoginProvider did not narrow the trae client")
	}
}

func TestLoginConfigDefaults(t *testing.T) {
	cfg := loadConfig(nil, nil)

	if got := cfg.oauthHost(); got != "https://api.trae.com.cn" {
		t.Errorf("oauthHost = %q", got)
	}
	if got := cfg.consoleHost(); got != "https://www.trae.cn" {
		t.Errorf("consoleHost = %q", got)
	}
	if got := cfg.loginCallbackHost(); got != "127.0.0.1" {
		t.Errorf("loginCallbackHost = %q", got)
	}
	if got := cfg.loginCallbackPort(); got != 0 {
		t.Errorf("loginCallbackPort = %d, want 0 (an OS-chosen free port)", got)
	}
	if got := cfg.loginSessionTTL(); got != 10*time.Minute {
		t.Errorf("loginSessionTTL = %s, want the reference's 10m", got)
	}
	if got := cfg.loginAppVersion(); got != defaultIdeVersion {
		t.Errorf("loginAppVersion = %q, want the IDE version %q", got, defaultIdeVersion)
	}
	if got := cfg.loginPluginVersion(); got != "2.3.62834" {
		t.Errorf("loginPluginVersion = %q", got)
	}
	if !cfg.loginEnabled() {
		t.Error("loginEnabled defaults to false; the panel button would be dead")
	}

	// Overrides, including trailing slashes on the hosts.
	raw := json.RawMessage(`{"oauth_host":"https://example.test/","console_host":"https://console.test/",` +
		`"login_callback_host":"127.0.0.1","login_callback_port":18080,"login_session_ttl_sec":90,` +
		`"login_app_version":"9.9.9","login_plugin_version":"1.2.3","login_enabled":false}`)
	cfg = loadConfig(raw, nil)
	if got := cfg.oauthHost(); got != "https://example.test" {
		t.Errorf("oauthHost override = %q", got)
	}
	if got := cfg.consoleHost(); got != "https://console.test" {
		t.Errorf("consoleHost override = %q", got)
	}
	if got := cfg.loginCallbackPort(); got != 18080 {
		t.Errorf("loginCallbackPort override = %d", got)
	}
	if got := cfg.loginSessionTTL(); got != 90*time.Second {
		t.Errorf("loginSessionTTL override = %s", got)
	}
	if got := cfg.loginAppVersion(); got != "9.9.9" {
		t.Errorf("loginAppVersion override = %q", got)
	}
	if got := cfg.loginPluginVersion(); got != "1.2.3" {
		t.Errorf("loginPluginVersion override = %q", got)
	}
	if cfg.loginEnabled() {
		t.Error("loginEnabled override was ignored")
	}
}

// ---- URL construction ------------------------------------------------------

func TestBuildLoginURLShape(t *testing.T) {
	c := panelClient(t, nil, nil)
	const (
		mid = "aabbccddeeff00112233445566778899"
		did = "0011223344556677"
		cb  = "http://127.0.0.1:18080/authorize"
	)

	got := c.buildLoginURL(mid, did, cb)
	if !strings.HasPrefix(got, defaultConsoleHost+"/authorization?") {
		t.Fatalf("login URL does not target the console /authorization page: %s", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()

	want := map[string]string{
		"login_version":     "1",
		"auth_from":         "solo",
		"login_channel":     "native_ide",
		"plugin_version":    "2.3.62834",
		"auth_type":         "local",
		"client_id":         defaultClientID,
		"redirect":          "0",
		"auth_callback_url": cb,
		"login_trace_id":    machineTraceID(mid, did),
		"machine_id":        mid,
		"device_id":         did,
		"x_device_id":       did,
		"x_machine_id":      mid,
		"x_device_brand":    "PC",
		"x_device_type":     "PC",
		"x_os_version":      "1.0",
		"x_app_version":     defaultIdeVersion,
		"x_app_type":        "stable",
	}
	for key, val := range want {
		if got := q.Get(key); got != val {
			t.Errorf("login URL param %q = %q, want %q", key, got, val)
		}
	}
	if len(q) != len(want) {
		t.Errorf("login URL carries %d params, want %d: %v", len(q), len(want), q)
	}
}

func TestMachineTraceID(t *testing.T) {
	cases := []struct{ name, machine, device, want string }{
		{"long pair keeps the last 16", "0123456789abcdef", "fedcba9876543210", "fedcba9876543210"},
		{"short pair is left-padded", "abc", "def", "0000000000abcdef"},
		{"empty pair is all zeros", "", "", "0000000000000000"},
		{"exactly 16 is untouched", "0123456789abcdef", "", "0123456789abcdef"},
		{"17 chars keeps the tail", "0123456789abcdefZ", "", "123456789abcdefZ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := machineTraceID(tc.machine, tc.device); got != tc.want {
				t.Errorf("machineTraceID(%q,%q) = %q, want %q", tc.machine, tc.device, got, tc.want)
			}
		})
	}
}

// ---- callback parsing ------------------------------------------------------

func TestParseLoginCallback(t *testing.T) {
	encode := func(v url.Values) string {
		return loginCallbackPath + "?" + v.Encode()
	}
	jsonStr := func(m map[string]any) string {
		b, _ := json.Marshal(m)
		return string(b)
	}

	t.Run("refresh token plus identity", func(t *testing.T) {
		v := url.Values{}
		v.Set("refreshToken", "RT")
		v.Set("userInfo", jsonStr(map[string]any{"UserID": "u1", "ScreenName": "nick", "TenantID": "ent"}))
		v.Set("userJwt", jsonStr(map[string]any{"Token": "AT"}))
		cred, err := parseLoginCallback(encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if cred.RefreshToken != "RT" {
			t.Errorf("RefreshToken = %q", cred.RefreshToken)
		}
		if cred.UID != "u1" || cred.Nickname != "nick" || cred.EnterpriseID != "ent" {
			t.Errorf("identity = %q/%q/%q", cred.UID, cred.Nickname, cred.EnterpriseID)
		}
		if cred.AccessToken != "" {
			t.Errorf("AccessToken = %q, want empty when a refresh token is present", cred.AccessToken)
		}
	})

	t.Run("userJwt refresh token is the fallback", func(t *testing.T) {
		v := url.Values{}
		v.Set("userJwt", jsonStr(map[string]any{"Token": "AT", "RefreshToken": "RT-JWT"}))
		cred, err := parseLoginCallback(encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if cred.RefreshToken != "RT-JWT" {
			t.Errorf("RefreshToken = %q", cred.RefreshToken)
		}
	})

	t.Run("userJwt token alone carries an expiry", func(t *testing.T) {
		v := url.Values{}
		v.Set("userJwt", jsonStr(map[string]any{"Token": "AT-ONLY", "TokenExpireAt": loginFarFuture}))
		cred, err := parseLoginCallback(encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if cred.AccessToken != "AT-ONLY" || cred.RefreshToken != "" {
			t.Errorf("tokens = %q/%q", cred.AccessToken, cred.RefreshToken)
		}
		if cred.ExpiresAt.IsZero() || cred.ExpiresAt.Unix() != loginFarFuture {
			t.Errorf("ExpiresAt = %v, want unix %d", cred.ExpiresAt, loginFarFuture)
		}
	})

	t.Run("millisecond expiry is normalised", func(t *testing.T) {
		v := url.Values{}
		v.Set("userJwt", jsonStr(map[string]any{"Token": "AT", "TokenExpireAt": int64(loginFarFuture) * 1000}))
		cred, err := parseLoginCallback(encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if got := cred.ExpiresAt.Unix(); got != loginFarFuture {
			t.Errorf("ExpiresAt = %d, want %d (ms should fold to s)", got, loginFarFuture)
		}
	})

	t.Run("double-encoded userInfo still parses", func(t *testing.T) {
		v := url.Values{}
		v.Set("refreshToken", "RT")
		// QueryEscape the JSON, then let Encode escape it again: the value that
		// reaches the handler is still escaped, which parseJSONParam must cope
		// with (callback.go:77-92).
		v.Set("userInfo", url.QueryEscape(jsonStr(map[string]any{"UserID": "u2"})))
		cred, err := parseLoginCallback(encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if cred.UID != "u2" {
			t.Errorf("UID = %q, want u2", cred.UID)
		}
	})

	t.Run("nothing usable is an error", func(t *testing.T) {
		_, err := parseLoginCallback(loginCallbackPath + "?foo=bar")
		if err == nil {
			t.Fatal("expected an error for a callback with no token")
		}
		if !strings.Contains(err.Error(), "callback missing refreshToken and userJwt.Token") {
			t.Errorf("error = %q, want the reference's wording", err)
		}
	})

	t.Run("empty url is an error", func(t *testing.T) {
		if _, err := parseLoginCallback("   "); err == nil {
			t.Fatal("expected an error for an empty callback URL")
		}
	})

	t.Run("absolute url is accepted", func(t *testing.T) {
		v := url.Values{}
		v.Set("refreshToken", "RT")
		cred, err := parseLoginCallback("http://127.0.0.1:18080" + encode(v))
		if err != nil {
			t.Fatalf("parseLoginCallback: %v", err)
		}
		if cred.RefreshToken != "RT" {
			t.Errorf("RefreshToken = %q", cred.RefreshToken)
		}
	})
}

// ---- token exchange --------------------------------------------------------

func TestExchangeLoginTokenShapes(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantAccess  string
		wantRefresh string
		wantErr     string
		// noExpiry marks a response that carries no expiry field at all.  The
		// module deliberately leaves ExpiresAt zero in that case (auth.go:496
		// reports it as "unknown expiry", and expiryExpired treats unknown as
		// usable), so the blanket non-zero assertion below must not apply.
		noExpiry bool
	}{
		{
			name:        "Result wrapped (the CN shape)",
			body:        `{"Result":{"Token":"AT1","RefreshToken":"RT2","TokenExpireAt":4102444800,"RefreshExpireAt":4102444800}}`,
			wantAccess:  "AT1",
			wantRefresh: "RT2",
		},
		{
			name:        "flat legacy shape",
			body:        `{"Token":"AT1","RefreshToken":"RT2","TokenExpireAt":4102444800}`,
			wantAccess:  "AT1",
			wantRefresh: "RT2",
		},
		{
			name:        "no rotation keeps the callback token",
			body:        `{"Result":{"Token":"AT1","TokenExpireAt":4102444800}}`,
			wantAccess:  "AT1",
			wantRefresh: "RT-ORIG",
		},
		{
			name:        "flat no rotation keeps the callback token",
			body:        `{"Token":"AT1"}`,
			wantAccess:  "AT1",
			wantRefresh: "RT-ORIG",
			noExpiry:    true,
		},
		{
			name:        "millisecond expiry folds",
			body:        `{"Result":{"Token":"AT1","TokenExpireAt":4102444800000}}`,
			wantAccess:  "AT1",
			wantRefresh: "RT-ORIG",
		},
		{
			name:    "missing token is an error",
			body:    `{"Result":{"RefreshToken":"RT2"}}`,
			wantErr: "no access token",
		},
		{
			name:    "empty Result object is an error",
			body:    `{"Result":{}}`,
			wantErr: "no access token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &loginTransport{exchangeBody: tc.body}
			c := panelClient(t, nil, rt)
			a := &Auth{RefreshToken: "RT-ORIG", Host: c.cfg.oauthHost()}

			err := c.exchangeLoginToken(context.Background(), a)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("exchangeLoginToken: %v", err)
			}
			if got := a.Token(); got != tc.wantAccess {
				t.Errorf("access token = %q, want %q", got, tc.wantAccess)
			}
			if got := a.RefreshTokenValue(); got != tc.wantRefresh {
				t.Errorf("refresh token = %q, want %q", got, tc.wantRefresh)
			}
			if !tc.noExpiry && a.Expiry().IsZero() {
				t.Error("expiry was not set")
			}
			if tc.noExpiry && !a.Expiry().IsZero() {
				t.Errorf("expiry = %s, want unknown: the response carried none", a.Expiry())
			}

			// The request must match the reference (client.go:171-221) and must
			// NOT carry an auth header.
			var sent map[string]any
			if err := json.Unmarshal([]byte(rt.body(epExchange)), &sent); err != nil {
				t.Fatalf("decode the exchange request: %v", err)
			}
			if sent["ClientID"] != defaultClientID {
				t.Errorf("ClientID = %v", sent["ClientID"])
			}
			if sent["RefreshToken"] != "RT-ORIG" {
				t.Errorf("RefreshToken = %v, want the callback token", sent["RefreshToken"])
			}
			if sent["ClientSecret"] != "-" {
				t.Errorf("ClientSecret = %v", sent["ClientSecret"])
			}
			if sent["UserID"] != "" {
				t.Errorf("UserID = %v, want an empty string", sent["UserID"])
			}
			hdr := rt.header(epExchange)
			if ct := hdr.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			if hdr.Get("X-Cloudide-Token") != "" || hdr.Get("Authorization") != "" {
				t.Error("the token exchange must not carry an auth header")
			}
		})
	}
}

func TestExchangeLoginTokenDurationFallback(t *testing.T) {
	rt := &loginTransport{exchangeBody: `{"Result":{"Token":"AT1","TokenExpireDuration":3600}}`}
	c := panelClient(t, nil, rt)
	before := time.Now()
	a := &Auth{RefreshToken: "RT"}
	if err := c.exchangeLoginToken(context.Background(), a); err != nil {
		t.Fatalf("exchangeLoginToken: %v", err)
	}
	if got := a.Expiry(); got.Before(before.Add(3500 * time.Second)) {
		t.Errorf("expiry = %s, want ~1h from now", got)
	}
}

func TestLoginUserInfoShapes(t *testing.T) {
	cases := []struct{ name, body string }{
		{"Result wrapped", `{"Result":{"UserID":"u1","ScreenName":"nick"}}`},
		{"flat", `{"UserID":"u1","ScreenName":"nick"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &loginTransport{userBody: tc.body}
			c := panelClient(t, nil, rt)
			a := &Auth{AccessToken: "AT"}
			uid, nick, err := c.loginUserInfo(context.Background(), a)
			if err != nil {
				t.Fatalf("loginUserInfo: %v", err)
			}
			if uid != "u1" || nick != "nick" {
				t.Errorf("identity = %q/%q", uid, nick)
			}
			if got := rt.header(epUserInfo).Get("X-Cloudide-Token"); got != "AT" {
				t.Errorf("X-Cloudide-Token = %q, want the access token", got)
			}
			var sent map[string]any
			if err := json.Unmarshal([]byte(rt.body(epUserInfo)), &sent); err != nil {
				t.Fatalf("decode the user info request: %v", err)
			}
			if sent["ReqSource"] != "IDE" {
				t.Errorf("ReqSource = %v", sent["ReqSource"])
			}
			if sent["IDEVersion"] != defaultIdeVersion {
				t.Errorf("IDEVersion = %v", sent["IDEVersion"])
			}
		})
	}
}

// ---- the flow --------------------------------------------------------------

func TestWebLoginHappyPath(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.State != core.LoginPending {
		t.Fatalf("State = %q, want pending", start.State)
	}
	if start.SessionID == "" {
		t.Fatal("no session id")
	}
	if start.URL == "" {
		t.Fatal("no URL for the operator to open")
	}
	if start.AccountID != "" {
		t.Errorf("a pending login already reports account %q", start.AccountID)
	}

	cbURL := callbackURLFromLoginURL(t, start.URL)
	parsed, err := url.Parse(cbURL)
	if err != nil {
		t.Fatalf("parse the callback URL: %v", err)
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("the callback URL has no host:port: %v", err)
	}
	if host != "127.0.0.1" {
		t.Errorf("the callback listener is on %q, not loopback", host)
	}
	if parsed.Path != loginCallbackPath {
		t.Errorf("callback path = %q", parsed.Path)
	}
	if port == "" || port == "0" {
		t.Fatalf("no real port was chosen: %q", parsed.Host)
	}

	// Pending until the redirect lands.
	st, err := c.PollLogin(ctx, start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("State before the callback = %q, want pending", st.State)
	}

	rec := driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))
	if rec.Code != http.StatusOK {
		t.Fatalf("the callback handler returned %d: %s", rec.Code, rec.Body.String())
	}

	done := waitForLoginState(t, c, start.SessionID, core.LoginSuccess, 5*time.Second)
	if done.AccountID == "" {
		t.Fatal("a successful login reported no account id")
	}
	if done.Message == "" {
		t.Error("a successful login reported no message")
	}

	// Exactly the two upstream calls, in order.
	paths := rt.calledPaths()
	if len(paths) != 2 || paths[0] != epExchange || paths[1] != epUserInfo {
		t.Fatalf("upstream calls = %v, want [%s %s]", paths, epExchange, epUserInfo)
	}

	// The credential landed in the module's own account store, tagged as a
	// panel login rather than a discovered one.
	recs := loginAccounts(t, c)
	if len(recs) != 1 {
		t.Fatalf("Accounts() returned %d records, want 1", len(recs))
	}
	if recs[0].ID != done.AccountID {
		t.Errorf("account id = %q, want %q", recs[0].ID, done.AccountID)
	}
	if !recs[0].Enabled {
		t.Error("the logged-in account is disabled")
	}
	if origin := fmt.Sprint(recs[0].Fields["origin"]); origin != originLogin {
		t.Errorf("origin = %q, want %q", origin, originLogin)
	}
	if managed := fmt.Sprint(recs[0].Fields["managed"]); managed != "true" {
		t.Errorf("managed = %q, want true", managed)
	}
	if recs[0].Label != testLoginNick {
		t.Errorf("label = %q, want the nickname %q", recs[0].Label, testLoginNick)
	}

	// The credential is on disk (that is the point) and the pool picked it up.
	raw, err := os.ReadFile(filepath.Join(c.dataDir, accountsFileName))
	if err != nil {
		t.Fatalf("read the account store: %v", err)
	}
	if !strings.Contains(string(raw), testLoginAccess) {
		t.Error("the exchanged access token was never persisted")
	}
	if c.pool.Len() != 1 {
		t.Errorf("pool has %d accounts, want 1", c.pool.Len())
	}

	assertPortFree(t, parsed.Host)
}

// TestWebLoginLoopbackListener is the only test that opens a socket: it proves
// the listener StartLogin bound really serves the redirect.  Loopback only.
func TestWebLoginLoopbackListener(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)

	start, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cbURL := callbackURLFromLoginURL(t, start.URL)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cbURL + "?" + callbackQuery(testLoginRefresh, testLoginAccess))
	if err != nil {
		t.Fatalf("loopback callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback returned %d: %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), testLoginRefresh) || strings.Contains(string(body), testLoginAccess) {
		t.Fatal("the landing page echoed a token back to the browser")
	}

	done := waitForLoginState(t, c, start.SessionID, core.LoginSuccess, 5*time.Second)
	if done.AccountID == "" {
		t.Fatal("no account id after the loopback login")
	}
}

func TestWebLoginBadQuery(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)

	start, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	rec := driveAuthorize(t, c, start.SessionID, "foo=bar&userInfo=%7B%7D")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 2*time.Second)
	if !strings.Contains(done.Message, "callback missing refreshToken and userJwt.Token") {
		t.Errorf("message = %q", done.Message)
	}
	if paths := rt.calledPaths(); len(paths) != 0 {
		t.Errorf("a bad callback still called the upstream: %v", paths)
	}
	if recs := loginAccounts(t, c); len(recs) != 0 {
		t.Errorf("a bad callback stored %d account(s)", len(recs))
	}
}

func TestWebLoginExchangeFailure(t *testing.T) {
	rt := &loginTransport{exchangeStatus: http.StatusInternalServerError}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 5*time.Second)
	if !strings.Contains(done.Message, "exchange the login token") {
		t.Errorf("message = %q, want the exchange step named", done.Message)
	}
	if done.AccountID != "" {
		t.Errorf("a failed login reported account %q", done.AccountID)
	}
	if paths := rt.calledPaths(); len(paths) != 1 || paths[0] != epExchange {
		t.Errorf("upstream calls = %v, want only the exchange", paths)
	}
	if recs := loginAccounts(t, c); len(recs) != 0 {
		t.Errorf("a failed login stored %d account(s)", len(recs))
	}
}

func TestWebLoginUserInfoFailure(t *testing.T) {
	rt := &loginTransport{userStatus: http.StatusUnauthorized}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 5*time.Second)
	if !strings.Contains(done.Message, "confirm the login") {
		t.Errorf("message = %q, want the confirmation step named", done.Message)
	}
	if recs := loginAccounts(t, c); len(recs) != 0 {
		t.Errorf("a login that failed confirmation stored %d account(s)", len(recs))
	}
}

func TestWebLoginNoUserIDIsRejected(t *testing.T) {
	rt := &loginTransport{userBody: `{"Result":{"ScreenName":"nobody"}}`}
	c := panelClient(t, nil, rt)

	start, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	// No userInfo in the callback either, so there is no id to key on.
	v := url.Values{}
	v.Set("refreshToken", testLoginRefresh)
	driveAuthorize(t, c, start.SessionID, v.Encode())

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 5*time.Second)
	if !strings.Contains(done.Message, "no user id") {
		t.Errorf("message = %q", done.Message)
	}
}

func TestWebLoginCancel(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cbHost := callbackHost(t, start.URL)

	if err := c.CancelLogin(ctx, start.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}

	// The state is definite, not a hang.
	st, err := c.PollLogin(ctx, start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after cancel: %v", err)
	}
	if st.State != core.LoginCancelled {
		t.Fatalf("State = %q, want cancelled", st.State)
	}

	// No credential, no upstream call, no bound port.
	if recs := loginAccounts(t, c); len(recs) != 0 {
		t.Errorf("cancel stored %d account(s)", len(recs))
	}
	if paths := rt.calledPaths(); len(paths) != 0 {
		t.Errorf("cancel called the upstream: %v", paths)
	}
	assertPortFree(t, cbHost)

	// A redirect that arrives after the cancel is refused, not exchanged.
	rec := driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))
	if rec.Code != http.StatusGone {
		t.Errorf("a late callback returned %d, want 410", rec.Code)
	}
	if paths := rt.calledPaths(); len(paths) != 0 {
		t.Errorf("a late callback called the upstream: %v", paths)
	}

	// Cancelling twice is harmless.
	if err := c.CancelLogin(ctx, start.SessionID); err != nil {
		t.Errorf("second CancelLogin: %v", err)
	}
}

func TestWebLoginCancelAfterSuccessIsANoop(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))
	done := waitForLoginState(t, c, start.SessionID, core.LoginSuccess, 5*time.Second)

	if err := c.CancelLogin(ctx, start.SessionID); err != nil {
		t.Fatalf("CancelLogin after success: %v", err)
	}
	st, err := c.PollLogin(ctx, start.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Errorf("State = %q, want the success to survive a cancel", st.State)
	}
	if st.AccountID != done.AccountID {
		t.Errorf("account id = %q, want %q", st.AccountID, done.AccountID)
	}
	if recs := loginAccounts(t, c); len(recs) != 1 {
		t.Errorf("Accounts() = %d, want the stored account to survive", len(recs))
	}
}

func TestWebLoginTimeout(t *testing.T) {
	cfg := loadConfig(nil, nil)
	cfg.LoginSessionTTLSec = 1 // keep the test fast; the default is 600
	rt := &loginTransport{}
	c := panelClient(t, cfg, rt)

	start, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	cbHost := callbackHost(t, start.URL)

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 6*time.Second)
	if !strings.Contains(done.Message, "timed out") {
		t.Errorf("message = %q", done.Message)
	}
	if recs := loginAccounts(t, c); len(recs) != 0 {
		t.Errorf("a timed-out login stored %d account(s)", len(recs))
	}
	assertPortFree(t, cbHost)

	// The listener is gone, so a late redirect is refused rather than accepted.
	rec := driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))
	if rec.Code != http.StatusGone {
		t.Errorf("a post-timeout callback returned %d, want 410", rec.Code)
	}
}

func TestWebLoginUnknownSession(t *testing.T) {
	c := panelClient(t, nil, nil)
	ctx := context.Background()

	if _, err := c.PollLogin(ctx, "deadbeef"); err == nil {
		t.Error("PollLogin accepted an unknown session")
	}
	if err := c.CancelLogin(ctx, "deadbeef"); err == nil {
		t.Error("CancelLogin accepted an unknown session")
	}
	if _, err := c.PollLogin(ctx, "   "); err == nil {
		t.Error("PollLogin accepted a blank session id")
	}
	if err := c.CancelLogin(ctx, ""); err == nil {
		t.Error("CancelLogin accepted a blank session id")
	}

	// A cancelled context is reported, not swallowed.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.PollLogin(cancelled, "deadbeef"); err == nil {
		t.Error("PollLogin ignored a cancelled context")
	}
	if _, err := c.StartLogin(cancelled); err == nil {
		t.Error("StartLogin ignored a cancelled context")
	}
}

func TestWebLoginDisabled(t *testing.T) {
	cfg := loadConfig(nil, nil)
	off := false
	cfg.LoginEnabled = &off
	c := panelClient(t, cfg, nil)

	if _, err := c.StartLogin(context.Background()); err == nil {
		t.Fatal("StartLogin started a listener while login is disabled")
	}
}

func TestWebLoginRejectsNonGET(t *testing.T) {
	c := panelClient(t, nil, &loginTransport{})
	start, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	sess := c.loginSession(start.SessionID)
	req := httptest.NewRequest(http.MethodPost, loginCallbackPath, nil)
	rec := httptest.NewRecorder()
	sess.srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /authorize = %d, want 405", rec.Code)
	}
	if st, _ := c.PollLogin(context.Background(), start.SessionID); st.State != core.LoginPending {
		t.Errorf("a rejected method changed the session state to %q", st.State)
	}
}

func TestWebLoginSessionMapIsBounded(t *testing.T) {
	cfg := loadConfig(nil, nil)
	cfg.LoginSessionTTLSec = 1
	c := panelClient(t, cfg, nil)
	ctx := context.Background()

	first, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(ctx, first.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}

	// Age the finished session past the reap horizon and start another: the
	// stale entry must be dropped, not accumulated.
	sess := c.loginSession(first.SessionID)
	sess.mu.Lock()
	sess.finishedAt = time.Now().Add(-2 * cfg.loginSessionTTL())
	sess.mu.Unlock()

	second, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer func() { _ = c.CancelLogin(ctx, second.SessionID) }()

	c.loginMu.Lock()
	n := len(c.logins)
	_, stale := c.logins[first.SessionID]
	c.loginMu.Unlock()

	if stale {
		t.Error("the finished session was never reaped")
	}
	if n != 1 {
		t.Errorf("the session map holds %d entries, want 1", n)
	}
}

func TestWebLoginSessionsUseDistinctPorts(t *testing.T) {
	c := panelClient(t, nil, &loginTransport{})
	ctx := context.Background()

	first, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	second, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if first.SessionID == second.SessionID {
		t.Fatal("two sessions share a session id")
	}
	h1, h2 := callbackHost(t, first.URL), callbackHost(t, second.URL)
	if h1 == h2 {
		t.Fatalf("two concurrent sessions share the callback address %s", h1)
	}

	// A fixed port, by contrast, must be refused rather than silently shared.
	cfg := loadConfig(nil, nil)
	u, err := url.Parse(callbackURLFromLoginURL(t, first.URL))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	cfg.LoginCallbackPort, _ = net.LookupPort("tcp", port)
	pinned := panelClient(t, cfg, &loginTransport{})
	if _, err := pinned.StartLogin(ctx); err == nil {
		t.Error("StartLogin bound a port that is already in use")
	}

	_ = c.CancelLogin(ctx, first.SessionID)
	_ = c.CancelLogin(ctx, second.SessionID)
	assertPortFree(t, h1)
	assertPortFree(t, h2)
}

func TestWebLoginFixedCallbackPort(t *testing.T) {
	cfg := loadConfig(nil, nil)
	// Bind port 0 first to learn a free one, release it, then pin it.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	cfg.LoginCallbackPort = port
	c := panelClient(t, cfg, &loginTransport{})
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	want := fmt.Sprintf("127.0.0.1:%d", port)
	if got := callbackHost(t, start.URL); got != want {
		t.Errorf("callback host = %q, want the pinned %q", got, want)
	}
	if err := c.CancelLogin(ctx, start.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	assertPortFree(t, want)
}

func TestWebLoginShutdownLoginSessions(t *testing.T) {
	c := panelClient(t, nil, &loginTransport{})
	ctx := context.Background()

	first, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	second, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	h1, h2 := callbackHost(t, first.URL), callbackHost(t, second.URL)

	if err := c.ShutdownLoginSessions(); err != nil {
		t.Fatalf("ShutdownLoginSessions: %v", err)
	}
	assertPortFree(t, h1)
	assertPortFree(t, h2)

	// The sessions are gone, so the panel gets a definite error rather than a
	// hang on a listener that no longer exists.
	if _, err := c.PollLogin(ctx, first.SessionID); err == nil {
		t.Error("PollLogin still finds a session after shutdown")
	}
	// And a new session still works afterwards.
	third, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin after shutdown: %v", err)
	}
	_ = c.CancelLogin(ctx, third.SessionID)
}

func TestWebLoginFingerprintReuse(t *testing.T) {
	t.Run("config wins", func(t *testing.T) {
		cfg := loadConfig(nil, nil)
		cfg.MachineID = "machine-from-config"
		cfg.DeviceID = "device-from-config"
		c := panelClient(t, cfg, nil)
		mid, did := c.loginFingerprint()
		if mid != "machine-from-config" || did != "device-from-config" {
			t.Errorf("fingerprint = %q/%q", mid, did)
		}
	})

	t.Run("discovered account is reused", func(t *testing.T) {
		c := panelClient(t, nil, nil)
		c.pool = NewPool([]*Auth{testAuth("u1", "tok")})
		mid, did := c.loginFingerprint()
		if mid != strings.Repeat("a", 64) || did != "2235771921399404" {
			t.Errorf("fingerprint = %q/%q, want the discovered pair", mid, did)
		}
	})

	t.Run("minted when nothing is known", func(t *testing.T) {
		c := panelClient(t, nil, nil)
		mid, did := c.loginFingerprint()
		if len(mid) != 32 || len(did) != 32 {
			t.Errorf("minted fingerprint = %q/%q, want hex32 like the reference", mid, did)
		}
		if mid == did {
			t.Error("the minted machine and device ids are identical")
		}
	})
}

// ---- never leak ------------------------------------------------------------

func TestWebLoginNeverLeaksToken(t *testing.T) {
	rt := &loginTransport{}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// The URL the operator copies must not carry a token.
	if strings.Contains(start.URL, testLoginRefresh) || strings.Contains(start.URL, testLoginAccess) {
		t.Fatal("the authorize URL carries a token")
	}

	driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))
	done := waitForLoginState(t, c, start.SessionID, core.LoginSuccess, 5*time.Second)
	if done.AccountID == "" {
		t.Fatal("the success snapshot carried no account id")
	}

	// Poll repeatedly: no snapshot may ever expose the secret, and the state
	// must be stable once terminal.
	var surfaces []string
	for i := 0; i < 3; i++ {
		st, err := c.PollLogin(ctx, start.SessionID)
		if err != nil {
			t.Fatalf("PollLogin: %v", err)
		}
		if st.State != core.LoginSuccess {
			t.Fatalf("State = %q after success", st.State)
		}
		b, err := json.Marshal(st)
		if err != nil {
			t.Fatalf("marshal LoginState: %v", err)
		}
		surfaces = append(surfaces, string(b), st.URL, st.Message, st.Code, st.AccountID, st.SessionID)
	}

	startJSON, _ := json.Marshal(start)
	surfaces = append(surfaces, string(startJSON), start.URL, start.Message, start.Code, start.AccountID)

	for _, rec := range loginAccounts(t, c) {
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal AccountRecord: %v", err)
		}
		surfaces = append(surfaces, string(b))
	}
	for _, s := range c.pool.Snapshot() {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal AccountStatus: %v", err)
		}
		surfaces = append(surfaces, string(b))
	}
	surfaces = append(surfaces, c.pool.Summary())

	// The module's own mask must not be reversible either.
	for _, a := range c.pool.Accounts() {
		surfaces = append(surfaces, a.Masked())
	}

	for _, s := range surfaces {
		if strings.Contains(s, testLoginRefresh) {
			t.Fatalf("a login-facing surface leaked the refresh token: %s", s)
		}
		if strings.Contains(s, testLoginAccess) {
			t.Fatalf("a login-facing surface leaked the access token: %s", s)
		}
	}

	// The credential IS on disk under the module's private DataDir, mode 0600 —
	// that is the whole point of the flow, and it is the only place it lands.
	path := filepath.Join(c.dataDir, accountsFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), testLoginRefresh) {
		t.Error("the refresh token was never persisted")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	// Windows has no POSIX permission bits: os.Stat reports 0666 for every
	// regular file there, so the 0600 the writer requested is not observable.
	// Assert it only where the OS actually models it.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("the account store is %o, want 0600", perm)
		}
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the account store is not a regular file: %s", info.Mode())
	}
}

func TestWebLoginRedactsFailures(t *testing.T) {
	// A hostile upstream error body that embeds the token must not reach the
	// operator verbatim.
	rt := &loginTransport{
		exchangeStatus: http.StatusBadRequest,
		exchangeBody:   `{"error":"refresh_token=` + testLoginRefresh + ` rejected"}`,
	}
	c := panelClient(t, nil, rt)
	ctx := context.Background()

	start, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	driveAuthorize(t, c, start.SessionID, callbackQuery(testLoginRefresh, testLoginAccess))

	done := waitForLoginState(t, c, start.SessionID, core.LoginFailed, 5*time.Second)
	if strings.Contains(done.Message, testLoginRefresh) {
		t.Errorf("the failure message leaked the refresh token: %s", done.Message)
	}
	if done.Message == "" {
		t.Error("the failure carried no message at all")
	}
}
