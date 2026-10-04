package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- fake vendor ------------------------------------------------------------

// loginSecretToken is a credential-shaped string that must never reach a
// LoginState, an error string, or the panel's account listing.
const loginSecretToken = "SECRET-ACCESS-TOKEN-9f8e7d6c5b4a"

// vendorResp is one scripted answer from the fake plugin-auth upstream.
type vendorResp struct {
	status int
	body   string
}

// vendorCall records one request the module made, so a test can pin the exact
// URL, method, body and realm headers.
type vendorCall struct {
	method  string
	url     string
	body    string
	headers http.Header
}

// fakeVendor stands in for the three plugin-auth endpoints.  It is a
// RoundTripper, so it plugs into panelClient exactly like a real upstream.
type fakeVendor struct {
	mu sync.Mutex

	stateStatus int
	stateBody   string

	// token is consumed in order; the last entry repeats forever, which is how
	// "pending, pending, done" is expressed.
	token        []vendorResp
	tokenCalls   int
	accountCalls int
	account      vendorResp

	calls []vendorCall
}

func (v *fakeVendor) RoundTrip(req *http.Request) (*http.Response, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	call := vendorCall{method: req.Method, url: req.URL.String(), headers: req.Header.Clone()}
	if req.Body != nil {
		if raw, err := io.ReadAll(req.Body); err == nil {
			call.body = string(raw)
		}
		_ = req.Body.Close()
	}
	v.calls = append(v.calls, call)

	switch {
	case strings.HasSuffix(req.URL.Path, "/v2/plugin/auth/state"):
		status, body := v.stateStatus, v.stateBody
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"code":1,"msg":"no state configured"}`
		}
		return jsonResponse(status, body), nil
	case strings.HasSuffix(req.URL.Path, "/v2/plugin/auth/token"):
		if len(v.token) == 0 {
			return jsonResponse(http.StatusOK, `{"code":1,"msg":"login ing"}`), nil
		}
		i := v.tokenCalls
		v.tokenCalls++
		if i >= len(v.token) {
			i = len(v.token) - 1
		}
		r := v.token[i]
		if r.status == 0 {
			r.status = http.StatusOK
		}
		if r.body == "" {
			r.body = `{"code":1,"msg":"login ing"}`
		}
		return jsonResponse(r.status, r.body), nil
	case strings.HasSuffix(req.URL.Path, "/v2/plugin/login/account"):
		v.accountCalls++
		status, body := v.account.status, v.account.body
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"code":0,"msg":"","data":{}}`
		}
		return jsonResponse(status, body), nil
	}
	return jsonResponse(http.StatusNotFound, `{"code":1,"msg":"unexpected path"}`), nil
}

func (v *fakeVendor) call(i int) vendorCall {
	v.mu.Lock()
	defer v.mu.Unlock()
	if i < 0 || i >= len(v.calls) {
		return vendorCall{}
	}
	return v.calls[i]
}

func (v *fakeVendor) count(pathSuffix string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, c := range v.calls {
		if strings.Contains(c.url, pathSuffix) {
			n++
		}
	}
	return n
}

// --- fixtures ---------------------------------------------------------------

// loginClient builds a Client with no accounts and a scripted vendor.
func loginClient(t *testing.T, v *fakeVendor, files map[string]string) (*Client, string) {
	t.Helper()
	isolateVendorStores(t)
	return panelClient(t, v, files)
}

func stateFixture(state, url string) string {
	body, _ := json.Marshal(map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]string{"state": state, "authUrl": url},
	})
	return string(body)
}

func tokenFixture(access, refresh, domain string, expiresIn int64) string {
	body, _ := json.Marshal(map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]any{
			"accessToken":  access,
			"refreshToken": refresh,
			"expiresIn":    expiresIn,
			"domain":       domain,
		},
	})
	return string(body)
}

func accountFixture(uid, enterpriseID, nickname string) string {
	body, _ := json.Marshal(map[string]any{
		"code": 0,
		"msg":  "",
		"data": map[string]string{"uid": uid, "enterpriseId": enterpriseID, "nickname": nickname},
	})
	return string(body)
}

// credentialFiles returns every parseable credential in dir.  Files that are
// not credentials (a model cache, say) are ignored.
func credentialFiles(t *testing.T, dir string) []*Auth {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		a, err := ParseAuth(raw)
		if err != nil {
			continue
		}
		// ParseAuth does not know where it was read from; LoadAccounts fills
		// this in, and the origin=panel rule keys off it.
		a.FilePath = filepath.Join(dir, e.Name())
		out = append(out, a)
	}
	return out
}

func startLogin(t *testing.T, c *Client) core.LoginState {
	t.Helper()
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	return st
}

func pollLogin(t *testing.T, c *Client, sessionID string) core.LoginState {
	t.Helper()
	st, err := c.PollLogin(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	return st
}

// --- realm selection --------------------------------------------------------

// TestWebLoginEndpointsPerRealm pins the URLs and Origin/Referer the reference
// implementation asserts in internal/panel/login_realm_test.go: global uses the
// workbuddy.ai base, everything else the copilot.tencent.com base.
func TestWebLoginEndpointsPerRealm(t *testing.T) {
	cases := []struct {
		realm    string
		wantBase string
		wantOrig string
	}{
		{"cn", "https://copilot.tencent.com", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com", "https://www.codebuddy.cn"},
		{"global", "https://www.workbuddy.ai", "https://www.workbuddy.ai"},
		{"intl", "https://www.workbuddy.ai", "https://www.workbuddy.ai"},
		{"International", "https://www.workbuddy.ai", "https://www.workbuddy.ai"},
	}
	c, _ := panelClient(t, nil, nil)
	for _, tc := range cases {
		t.Run("realm="+tc.realm, func(t *testing.T) {
			realm := normalizeLoginRealm(tc.realm)
			if got := c.up.loginBase(realm) + loginStatePath; got != tc.wantBase+"/v2/plugin/auth/state?platform=CLI" {
				t.Fatalf("state endpoint = %q, want %q", got, tc.wantBase+"/v2/plugin/auth/state?platform=CLI")
			}
			if got := c.up.loginBase(realm) + loginTokenPath; got != tc.wantBase+"/v2/plugin/auth/token?state=" {
				t.Fatalf("token endpoint = %q", got)
			}
			if got := c.up.loginBase(realm) + loginAccountPath; got != tc.wantBase+"/v2/plugin/login/account?state=" {
				t.Fatalf("account endpoint = %q", got)
			}
			if got := loginOrigin(realm); got != tc.wantOrig {
				t.Fatalf("origin = %q, want %q", got, tc.wantOrig)
			}
		})
	}
}

// TestWebLoginRealmSelection drives the whole flow on both realms and checks
// that the request carries the right host and realm headers, and that the
// credential lands with the right realm.
func TestWebLoginRealmSelection(t *testing.T) {
	cases := []struct {
		configRealm string
		wantBase    string
		wantOrigin  string
		wantRealm   string
		domain      string
	}{
		{"cn", "https://copilot.tencent.com", "https://www.codebuddy.cn", realmCN, "codebuddy.cn"},
		{"global", "https://www.workbuddy.ai", "https://www.workbuddy.ai", realmGlobal, "workbuddy.ai"},
		{"intl", "https://www.workbuddy.ai", "https://www.workbuddy.ai", realmGlobal, "workbuddy.ai"},
	}
	for _, tc := range cases {
		t.Run(tc.configRealm, func(t *testing.T) {
			v := &fakeVendor{
				stateBody: stateFixture("state-"+tc.configRealm, "https://example.test/auth?state=state-"+tc.configRealm),
				token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-1", tc.domain, 3600)}},
				account:   vendorResp{body: accountFixture("uid-"+tc.configRealm, "ent-1", "Realm Tester")},
			}
			c, dir := loginClient(t, v, nil)
			c.cfg.LoginRealm = tc.configRealm

			st := startLogin(t, c)
			if st.State != core.LoginPending {
				t.Fatalf("state = %q, want pending", st.State)
			}
			first := v.call(0)
			if first.method != http.MethodPost {
				t.Fatalf("state method = %q, want POST", first.method)
			}
			if want := tc.wantBase + "/v2/plugin/auth/state?platform=CLI"; first.url != want {
				t.Fatalf("state url = %q, want %q", first.url, want)
			}
			if got := first.headers.Get("Origin"); got != tc.wantOrigin {
				t.Fatalf("Origin = %q, want %q", got, tc.wantOrigin)
			}
			if got := first.headers.Get("Referer"); got != tc.wantOrigin+"/" {
				t.Fatalf("Referer = %q, want %q", got, tc.wantOrigin+"/")
			}

			done := pollLogin(t, c, st.SessionID)
			if done.State != core.LoginSuccess {
				t.Fatalf("poll state = %q (%s), want success", done.State, done.Message)
			}
			if want := tc.wantBase + "/v2/plugin/auth/token?state=state-" + tc.configRealm; v.call(1).url != want {
				t.Fatalf("token url = %q, want %q", v.call(1).url, want)
			}
			if want := tc.wantBase + "/v2/plugin/login/account?state=state-" + tc.configRealm; v.call(2).url != want {
				t.Fatalf("account url = %q, want %q", v.call(2).url, want)
			}
			if got := v.call(2).headers.Get("Authorization"); got != "Bearer "+loginSecretToken {
				t.Fatalf("account Authorization = %q", got)
			}

			creds := credentialFiles(t, dir)
			if len(creds) != 1 {
				t.Fatalf("wrote %d credentials, want 1", len(creds))
			}
			if got := creds[0].RealmName(); got != tc.wantRealm {
				t.Fatalf("persisted realm = %q, want %q", got, tc.wantRealm)
			}
		})
	}
}

// TestWebLoginRealmInferredFromDomain mirrors the reference's BackfillRealm
// (panel/login.go:228-233): a cn sign-in whose token says workbuddy.ai is filed
// as global, otherwise the account would be sent to the wrong host.
func TestWebLoginRealmInferredFromDomain(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-infer", "https://example.test/auth"),
		token:     []vendorResp{{body: tokenFixture(loginSecretToken, "r", "workbuddy.ai", 3600)}},
		account:   vendorResp{body: accountFixture("uid-infer", "", "Infer")},
	}
	c, dir := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmCN

	st := startLogin(t, c)
	if got := pollLogin(t, c, st.SessionID); got.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s)", got.State, got.Message)
	}
	creds := credentialFiles(t, dir)
	if len(creds) != 1 {
		t.Fatalf("wrote %d credentials, want 1", len(creds))
	}
	if got := creds[0].RealmName(); got != realmGlobal {
		t.Fatalf("realm = %q, want global (inferred from domain)", got)
	}
}

// TestWorkbuddyLoginRealmsAreTheTwoVendorServices pins what the picker shows.
// The two realms are separate services with separate credential stores and
// separate model catalogues, so the list is not cosmetic: an account added to
// the wrong one is a credential that can never serve the models the operator
// wanted, and nothing downstream can repair that.
func TestWorkbuddyLoginRealmsAreTheTwoVendorServices(t *testing.T) {
	c, _ := panelClient(t, nil, nil)
	rp, ok := core.AsRealmLoginProvider(c)
	if !ok {
		t.Fatalf("the module does not advertise a realm-aware login")
	}
	realms := rp.LoginRealms(context.Background())
	if len(realms) != 2 {
		t.Fatalf("LoginRealms = %+v, want exactly two", realms)
	}
	if realms[0].Code != realmCN || realms[1].Code != realmGlobal {
		t.Errorf("realms = %+v, want %s then %s", realms, realmCN, realmGlobal)
	}
	for _, r := range realms {
		if r.Name == "" {
			t.Errorf("realm %q carries no label, so the picker would show a bare code", r.Code)
		}
		if r.Help == "" {
			t.Errorf("realm %q carries no help text", r.Code)
		}
	}
	// The capability report is what the panel actually reads; the picker is
	// rendered from it and never from the login call's return value.
	if got := core.CapabilitiesOf(context.Background(), c); len(got.Realms) != 2 {
		t.Errorf("capabilities carried %+v, want the same two realms", got.Realms)
	}
}

// TestWorkbuddyThePickedRealmBeatsTheConfiguredOne is the whole point of the
// picker: a client configured for the mainland service must still start an
// international sign-in when the operator asks for one, and vice versa.  Without
// this the picker would render, accept the choice, and land the credential on
// the configured default realm -- the one outcome worse than not offering it.
func TestWorkbuddyThePickedRealmBeatsTheConfiguredOne(t *testing.T) {
	cases := []struct {
		configured string
		picked     string
		wantBase   string
		wantRealm  string
	}{
		{"cn", realmGlobal, "https://www.workbuddy.ai", realmGlobal},
		{"global", realmCN, "https://copilot.tencent.com", realmCN},
		{"cn", "intl", "https://www.workbuddy.ai", realmGlobal},
		{"cn", "", "https://copilot.tencent.com", realmCN},
		{"global", "", "https://www.workbuddy.ai", realmGlobal},
	}
	for _, tc := range cases {
		t.Run(tc.configured+"/pick="+tc.picked, func(t *testing.T) {
			v := &fakeVendor{
				stateBody: stateFixture("state-pick", "https://example.test/auth?state=state-pick"),
				token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-1", "example.test", 3600)}},
				account:   vendorResp{body: accountFixture("uid-pick", "ent-1", "Picker")},
			}
			c, _ := loginClient(t, v, nil)
			c.cfg.LoginRealm = tc.configured

			st, err := c.StartLoginRealm(context.Background(), tc.picked)
			if err != nil {
				t.Fatalf("StartLoginRealm(%q): %v", tc.picked, err)
			}
			if st.State != core.LoginPending {
				t.Fatalf("state = %q, want pending", st.State)
			}
			if got := v.call(0).url; got != tc.wantBase+"/v2/plugin/auth/state?platform=CLI" {
				t.Fatalf("state url = %q, want the %s host", got, tc.wantRealm)
			}
			if got := v.call(0).headers.Get("Origin"); got != loginOrigin(tc.wantRealm) {
				t.Errorf("Origin = %q, want the %s origin", got, tc.wantRealm)
			}
			// The realm has to come back on the state, or the panel cannot
			// label what it is waiting for and the operator cannot tell which
			// service they are about to sign in to.
			if got := st.Realm; got != tc.wantRealm {
				t.Errorf("state carried realm %q, want %q", got, tc.wantRealm)
			}
		})
	}
}

// TestWorkbuddyStartLoginStillUsesTheConfiguredRealm guards the delegating
// entry point: StartLogin is what every caller that predates the picker uses,
// so it must keep meaning "the realm in the config".
func TestWorkbuddyStartLoginStillUsesTheConfiguredRealm(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-legacy", "https://example.test/auth?state=state-legacy"),
		token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-1", "example.test", 3600)}},
		account:   vendorResp{body: accountFixture("uid-legacy", "ent-1", "Legacy")},
	}
	c, _ := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmGlobal

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if got := v.call(0).url; got != "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI" {
		t.Fatalf("state url = %q, want the configured global host", got)
	}
	if st.Realm != realmGlobal {
		t.Errorf("realm = %q, want %q", st.Realm, realmGlobal)
	}
}

// --- happy path -------------------------------------------------------------

func TestWebLoginHappyPath(t *testing.T) {
	const (
		session = "state-happy"
		authURL = "https://www.codebuddy.cn/login?state=state-happy"
	)
	v := &fakeVendor{
		stateBody: stateFixture(session, authURL),
		token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-happy", "codebuddy.cn", 3600)}},
		account:   vendorResp{body: accountFixture("uid-happy-1", "ent-7", "Happy Tester")},
	}
	c, dir := loginClient(t, v, nil)

	st := startLogin(t, c)
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	if st.SessionID != session {
		t.Fatalf("session id = %q, want %q", st.SessionID, session)
	}
	if st.URL != authURL {
		t.Fatalf("url = %q, want %q", st.URL, authURL)
	}
	if st.Message == "" {
		t.Fatal("a pending session must carry a message for the operator")
	}
	if st.AccountID != "" {
		t.Fatalf("account id = %q, want empty while pending", st.AccountID)
	}

	// Nothing may be written before the vendor confirms.
	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("StartLogin wrote %d credentials", len(creds))
	}

	call := v.call(0)
	if call.body != "{}" {
		t.Fatalf("state body = %q, want {}", call.body)
	}
	if got := call.headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := call.headers.Get("X-Requested-With"); got != "XMLHttpRequest" {
		t.Fatalf("X-Requested-With = %q", got)
	}
	if got := call.headers.Get("X-CodeBuddy-Request"); got != "1" {
		t.Fatalf("X-CodeBuddy-Request = %q", got)
	}
	if got := call.headers.Get("User-Agent"); got != codeBuddyCLIUA {
		t.Fatalf("User-Agent = %q, want %q", got, codeBuddyCLIUA)
	}

	done := pollLogin(t, c, st.SessionID)
	if done.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s), want success", done.State, done.Message)
	}
	if done.AccountID != "uid-happy-1" {
		t.Fatalf("account id = %q, want uid-happy-1", done.AccountID)
	}
	if !strings.Contains(done.Message, "Happy Tester") {
		t.Fatalf("message = %q, want it to name the account", done.Message)
	}

	creds := credentialFiles(t, dir)
	if len(creds) != 1 {
		t.Fatalf("wrote %d credentials, want 1", len(creds))
	}
	a := creds[0]
	if a.AccessToken != loginSecretToken {
		t.Fatalf("stored access token = %q", a.AccessToken)
	}
	if a.RefreshToken != "refresh-happy" {
		t.Fatalf("stored refresh token = %q", a.RefreshToken)
	}
	if a.UIDValue() != "uid-happy-1" {
		t.Fatalf("stored uid = %q", a.UIDValue())
	}
	if a.EnterpriseID != "ent-7" {
		t.Fatalf("stored enterprise = %q", a.EnterpriseID)
	}
	if a.Nickname != "Happy Tester" {
		t.Fatalf("stored nickname = %q", a.Nickname)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("expiry %d is not in the future", a.ExpiresAt)
	}
	// The panel file name prefix is what makes the record report origin=panel.
	if !strings.HasPrefix(filepath.Base(a.FilePath), panelFilePrefix) {
		t.Fatalf("file %q does not use the panel prefix", filepath.Base(a.FilePath))
	}

	// Listed by Accounts() and usable without a restart.
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	var found *core.AccountRecord
	for i := range recs {
		if recs[i].ID == "uid-happy-1" {
			found = &recs[i]
		}
	}
	if found == nil {
		t.Fatalf("the new account is not listed: %+v", recs)
	}
	if !found.Enabled {
		t.Fatal("the new account must be enabled")
	}
	if got := found.Fields["origin"]; got != originPanel {
		t.Fatalf("origin = %q, want %q", got, originPanel)
	}
	if !c.pool.Ready() {
		t.Fatal("the new credential is not usable without a restart")
	}
	status := c.Status(context.Background())
	if len(status.Accounts) == 0 {
		t.Fatal("Status() does not report the new account")
	}
}

// TestWebLoginPendingThenSuccess walks the ordinary "operator is still typing
// their password" case: the vendor answers code!=0 twice, then the token.
func TestWebLoginPendingThenSuccess(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-slow", "https://example.test/auth"),
		token: []vendorResp{
			{body: `{"code":1,"msg":"login ing"}`},
			{body: `{"code":1,"msg":"login ing"}`},
			{body: tokenFixture(loginSecretToken, "refresh-slow", "codebuddy.cn", 120)},
		},
		account: vendorResp{body: accountFixture("uid-slow", "", "Slow Tester")},
	}
	c, dir := loginClient(t, v, nil)

	st := startLogin(t, c)
	for i := 1; i <= 2; i++ {
		got := pollLogin(t, c, st.SessionID)
		if got.State != core.LoginPending {
			t.Fatalf("poll %d state = %q (%s), want pending", i, got.State, got.Message)
		}
		if !strings.Contains(got.Message, "login ing") {
			t.Fatalf("poll %d message = %q, want the vendor's reason", i, got.Message)
		}
	}
	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("a pending poll wrote %d credentials", len(creds))
	}
	if got := v.count("/v2/plugin/login/account"); got != 0 {
		t.Fatalf("account endpoint called %d times while pending", got)
	}

	done := pollLogin(t, c, st.SessionID)
	if done.State != core.LoginSuccess {
		t.Fatalf("final state = %q (%s), want success", done.State, done.Message)
	}
	if got := v.count("/v2/plugin/auth/token"); got != 3 {
		t.Fatalf("token endpoint called %d times, want 3", got)
	}
	if creds := credentialFiles(t, dir); len(creds) != 1 {
		t.Fatalf("wrote %d credentials, want 1", len(creds))
	}
}

// --- expiry -----------------------------------------------------------------

func TestWebLoginSnapshotExpiry(t *testing.T) {
	pending := &panelLogin{
		sessionID: "s",
		state:     core.LoginPending,
		url:       "https://example.test/auth",
		expiresAt: time.Now().Add(-time.Second),
		message:   loginWaitingMessage,
	}
	st := pending.snapshot()
	if st.State != core.LoginFailed {
		t.Fatalf("expired pending session reported %q, want failed", st.State)
	}
	if st.Message != loginExpiredMessage {
		t.Fatalf("message = %q, want %q", st.Message, loginExpiredMessage)
	}

	// A finished session must not be rewritten by the clock.
	done := &panelLogin{
		sessionID: "s2",
		state:     core.LoginSuccess,
		expiresAt: time.Now().Add(-time.Hour),
		accountID: "uid-x",
	}
	if got := done.snapshot(); got.State != core.LoginSuccess || got.AccountID != "uid-x" {
		t.Fatalf("finished session = %+v, want success/uid-x", got)
	}
}

func TestWebLoginExpiresAndStopsPolling(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-exp", "https://example.test/auth"),
		token:     []vendorResp{{body: `{"code":1,"msg":"login ing"}`}},
	}
	c, dir := loginClient(t, v, nil)
	one := 1
	c.cfg.LoginTTLSeconds = &one
	if got := c.loginTTL(); got != time.Second {
		t.Fatalf("loginTTL() = %v, want 1s", got)
	}

	st := startLogin(t, c)
	if got := pollLogin(t, c, st.SessionID); got.State != core.LoginPending {
		t.Fatalf("first poll = %q (%s), want pending", got.State, got.Message)
	}
	if got := v.count("/v2/plugin/auth/token"); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}

	time.Sleep(1100 * time.Millisecond)

	after := pollLogin(t, c, st.SessionID)
	if after.State != core.LoginFailed {
		t.Fatalf("poll after expiry = %q (%s), want failed", after.State, after.Message)
	}
	if after.Message != loginExpiredMessage {
		t.Fatalf("message = %q, want %q", after.Message, loginExpiredMessage)
	}
	if got := v.count("/v2/plugin/auth/token"); got != 1 {
		t.Fatalf("the vendor was polled %d times after expiry; a poll after expiry must not hang or retry", got)
	}
	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("an expired session wrote %d credentials", len(creds))
	}
}

// --- refusal ----------------------------------------------------------------

// TestWebLoginDenied covers the vendor rejecting the state outright.  Pending
// is expressed as HTTP 200 + code!=0, so a 403 cannot mean "keep waiting".
func TestWebLoginDenied(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-denied", "https://example.test/auth"),
		token:     []vendorResp{{status: http.StatusForbidden, body: `{"code":403,"msg":"state rejected"}`}},
	}
	c, dir := loginClient(t, v, nil)

	st := startLogin(t, c)
	got := pollLogin(t, c, st.SessionID)
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q (%s), want failed", got.State, got.Message)
	}
	if got.Message == "" {
		t.Fatal("a failed session must explain itself")
	}
	if n := v.count("/v2/plugin/login/account"); n != 0 {
		t.Fatalf("the account endpoint was called %d times after a refusal", n)
	}
	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("a refused login wrote %d credentials", len(creds))
	}
}

func TestWebLoginUpstream5xx(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-500", "https://example.test/auth"),
			token:     []vendorResp{{status: http.StatusServiceUnavailable, body: `{"code":500,"msg":"upstream down"}`}},
		}
		c, dir := loginClient(t, v, nil)
		st := startLogin(t, c)
		got := pollLogin(t, c, st.SessionID)
		if got.State != core.LoginFailed {
			t.Fatalf("state = %q (%s), want failed", got.State, got.Message)
		}
		if creds := credentialFiles(t, dir); len(creds) != 0 {
			t.Fatalf("a 5xx poll wrote %d credentials", len(creds))
		}
	})

	t.Run("state", func(t *testing.T) {
		v := &fakeVendor{stateStatus: http.StatusBadGateway, stateBody: `{"code":502,"msg":"bad gateway"}`}
		c, _ := loginClient(t, v, nil)
		if _, err := c.StartLogin(context.Background()); err == nil {
			t.Fatal("StartLogin accepted a 5xx from the state endpoint")
		} else if !strings.Contains(err.Error(), "auth state") {
			t.Fatalf("error = %v, want it to name the state call", err)
		}
	})

	t.Run("transport", func(t *testing.T) {
		// status 0: the request never reached a server.
		v := &fakeVendor{
			stateBody: stateFixture("state-net", "https://example.test/auth"),
			token:     []vendorResp{{status: 0, body: `{"code":0,"msg":"","data":{"accessToken":"","refreshToken":""}}`}},
		}
		c, _ := loginClient(t, v, nil)
		st := startLogin(t, c)
		// A 200 with an empty token is "not finished yet", not a failure.
		if got := pollLogin(t, c, st.SessionID); got.State != core.LoginPending {
			t.Fatalf("empty token state = %q (%s), want pending", got.State, got.Message)
		}
	})
}

// --- malformed input --------------------------------------------------------

func TestWebLoginMalformedJSON(t *testing.T) {
	t.Run("state-not-json", func(t *testing.T) {
		v := &fakeVendor{stateBody: `<html>login</html>`}
		c, _ := loginClient(t, v, nil)
		if _, err := c.StartLogin(context.Background()); err == nil {
			t.Fatal("StartLogin accepted a non-JSON state response")
		}
	})

	t.Run("state-missing-fields", func(t *testing.T) {
		v := &fakeVendor{stateBody: `{"code":0,"msg":"","data":{"state":"only-a-state"}}`}
		c, _ := loginClient(t, v, nil)
		if _, err := c.StartLogin(context.Background()); err == nil {
			t.Fatal("StartLogin accepted a state response without authUrl")
		}
	})

	t.Run("token-not-an-object", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-m1", "https://example.test/auth"),
			token:     []vendorResp{{body: `{"code":0,"msg":"","data":"not an object"}`}},
		}
		c, _ := loginClient(t, v, nil)
		st := startLogin(t, c)
		if got := pollLogin(t, c, st.SessionID); got.State != core.LoginPending {
			t.Fatalf("state = %q (%s), want pending", got.State, got.Message)
		}
	})

	t.Run("token-envelope-not-json", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-m2", "https://example.test/auth"),
			token:     []vendorResp{{body: `<!doctype html><html>waf</html>`}},
		}
		c, _ := loginClient(t, v, nil)
		st := startLogin(t, c)
		got := pollLogin(t, c, st.SessionID)
		// The reference treats an unreadable 2xx as "not finished yet"
		// (panel/login.go:174-179); killing the session here would be worse.
		if got.State != core.LoginPending {
			t.Fatalf("state = %q (%s), want pending", got.State, got.Message)
		}
	})

	t.Run("account-not-json", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-m3", "https://example.test/auth"),
			token:     []vendorResp{{body: tokenFixture(loginSecretToken, "r", "codebuddy.cn", 60)}},
			account:   vendorResp{body: `<html>nope</html>`},
		}
		c, dir := loginClient(t, v, nil)
		st := startLogin(t, c)
		got := pollLogin(t, c, st.SessionID)
		if got.State != core.LoginFailed {
			t.Fatalf("state = %q (%s), want failed without a uid", got.State, got.Message)
		}
		if creds := credentialFiles(t, dir); len(creds) != 0 {
			t.Fatalf("a uid-less login wrote %d credentials", len(creds))
		}
	})
}

// TestWebLoginRejectsUnsafeUID covers the path-traversal guard the reference
// calls out at panel/login.go:204-210: the uid becomes part of a file name.
func TestWebLoginRejectsUnsafeUID(t *testing.T) {
	for _, uid := range []string{"evil/../evil", "..\\..\\evil", "uid with space", strings.Repeat("a", 65)} {
		t.Run(uid, func(t *testing.T) {
			v := &fakeVendor{
				stateBody: stateFixture("state-bad", "https://example.test/auth"),
				token:     []vendorResp{{body: tokenFixture(loginSecretToken, "r", "codebuddy.cn", 60)}},
				account:   vendorResp{body: accountFixture(uid, "", "Evil")},
			}
			c, dir := loginClient(t, v, nil)
			st := startLogin(t, c)
			got := pollLogin(t, c, st.SessionID)
			if got.State != core.LoginFailed {
				t.Fatalf("state = %q (%s), want failed", got.State, got.Message)
			}
			if creds := credentialFiles(t, dir); len(creds) != 0 {
				t.Fatalf("an unsafe uid wrote %d credentials", len(creds))
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.json")); err == nil {
				t.Fatal("the unsafe uid escaped the data directory")
			}
		})
	}
}

// --- cancel -----------------------------------------------------------------

func TestWebLoginCancel(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-cancel", "https://example.test/auth"),
		token:     []vendorResp{{body: `{"code":1,"msg":"login ing"}`}},
	}
	c, dir := loginClient(t, v, nil)

	st := startLogin(t, c)
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	got := pollLogin(t, c, st.SessionID)
	if got.State != core.LoginCancelled {
		t.Fatalf("state = %q (%s), want cancelled", got.State, got.Message)
	}
	if got.AccountID != "" {
		t.Fatalf("account id = %q, want empty", got.AccountID)
	}
	if n := v.count("/v2/plugin/auth/token"); n != 0 {
		t.Fatalf("the vendor was polled %d times after cancel", n)
	}
	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("a cancelled login wrote %d credentials", len(creds))
	}
}

// TestWebLoginCancelBeforePersistLeavesNoCredential covers the narrow window
// where the token arrives and the operator cancels at the same moment.
func TestWebLoginCancelBeforePersistLeavesNoCredential(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-race", "https://example.test/auth"),
		token:     []vendorResp{{body: tokenFixture(loginSecretToken, "r", "codebuddy.cn", 60)}},
		account:   vendorResp{body: accountFixture("uid-race", "", "Race")},
	}
	c, dir := loginClient(t, v, nil)

	st := startLogin(t, c)
	sess := c.loginByID(st.SessionID)
	if sess == nil {
		t.Fatal("the session was not recorded")
	}
	// A cancel that lands after the poll but before the write.
	sess.markCancelled()
	c.completeLogin(context.Background(), sess, &loginCredential{
		accessToken: loginSecretToken,
		realm:       realmCN,
		uid:         "uid-race",
	})

	if creds := credentialFiles(t, dir); len(creds) != 0 {
		t.Fatalf("a cancel mid-flight left %d credentials behind", len(creds))
	}
	if got := pollLogin(t, c, st.SessionID); got.State != core.LoginCancelled {
		t.Fatalf("state = %q, want cancelled", got.State)
	}
}

func TestWebLoginUnknownSession(t *testing.T) {
	c, _ := loginClient(t, &fakeVendor{}, nil)

	if _, err := c.PollLogin(context.Background(), ""); err == nil {
		t.Fatal("PollLogin accepted an empty session id")
	}
	if _, err := c.PollLogin(context.Background(), "nope"); err == nil {
		t.Fatal("PollLogin accepted an unknown session id")
	}
	if err := c.CancelLogin(context.Background(), ""); err == nil {
		t.Fatal("CancelLogin accepted an empty session id")
	}
	if err := c.CancelLogin(context.Background(), "nope"); err == nil {
		t.Fatal("CancelLogin accepted an unknown session id")
	}
}

// TestWebLoginAlreadyUsable pins the house-style short circuit: when the pool
// already works, the panel is told so instead of starting a second credential.
func TestWebLoginAlreadyUsable(t *testing.T) {
	files := map[string]string{
		panelFileName("uid-existing"): credJSON("access-existing-1", "refresh-existing", realmCN, "codebuddy.cn", "uid-existing", 0),
	}
	v := &fakeVendor{}
	c, _ := loginClient(t, v, files)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success", st.State, st.Message)
	}
	if st.Message != loginAlreadyUsableMessage {
		t.Fatalf("message = %q", st.Message)
	}
	if n := v.count("/v2/plugin/auth/state"); n != 0 {
		t.Fatalf("the vendor was contacted %d times despite a usable account", n)
	}
}

// --- redaction --------------------------------------------------------------

// TestWebLoginNeverLeaksToken is the credential-safety assertion: whatever the
// vendor echoes, no operator-visible string and no listing may carry the token.
func TestWebLoginNeverLeaksToken(t *testing.T) {
	t.Run("vendor-echoed-token-is-redacted", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-leak", "https://example.test/auth"),
			token:     []vendorResp{{body: `{"code":7,"msg":"login ing access_token=` + loginSecretToken + `"}`}},
		}
		c, dir := loginClient(t, v, nil)

		st := startLogin(t, c)
		if strings.Contains(st.Message, loginSecretToken) {
			t.Fatalf("StartLogin leaked the token: %q", st.Message)
		}
		for i := 0; i < 3; i++ {
			got := pollLogin(t, c, st.SessionID)
			if strings.Contains(got.Message, loginSecretToken) {
				t.Fatalf("PollLogin leaked the token: %q", got.Message)
			}
			if !strings.Contains(got.Message, "<redacted>") {
				t.Fatalf("the vendor message was not redacted: %q", got.Message)
			}
		}
		if creds := credentialFiles(t, dir); len(creds) != 0 {
			t.Fatalf("nothing should have been stored, found %d", len(creds))
		}
	})

	t.Run("successful-login-is-not-rendered", func(t *testing.T) {
		v := &fakeVendor{
			stateBody: stateFixture("state-quiet", "https://example.test/auth"),
			token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-quiet", "codebuddy.cn", 3600)}},
			account:   vendorResp{body: accountFixture("uid-quiet", "ent-quiet", "Quiet Tester")},
		}
		c, dir := loginClient(t, v, nil)
		st := startLogin(t, c)
		done := pollLogin(t, c, st.SessionID)
		if done.State != core.LoginSuccess {
			t.Fatalf("state = %q (%s)", done.State, done.Message)
		}
		if strings.Contains(done.Message, loginSecretToken) {
			t.Fatalf("the success message leaked the token: %q", done.Message)
		}

		// The token really is on disk -- otherwise this test proves nothing.
		creds := credentialFiles(t, dir)
		if len(creds) != 1 || creds[0].AccessToken != loginSecretToken {
			t.Fatalf("the token was not stored, so redaction was not exercised")
		}

		recs, err := c.Accounts(context.Background())
		if err != nil {
			t.Fatalf("Accounts: %v", err)
		}
		blob, err := json.Marshal(recs)
		if err != nil {
			t.Fatalf("marshal accounts: %v", err)
		}
		if strings.Contains(string(blob), loginSecretToken) {
			t.Fatalf("Accounts() leaked the token: %s", blob)
		}
		status := c.Status(context.Background())
		blob, err = json.Marshal(status)
		if err != nil {
			t.Fatalf("marshal status: %v", err)
		}
		if strings.Contains(string(blob), loginSecretToken) {
			t.Fatalf("Status() leaked the token: %s", blob)
		}
	})
}
