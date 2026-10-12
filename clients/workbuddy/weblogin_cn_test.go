package workbuddy

import (
	"context"
	"net/url"
	"regexp"
	"testing"

	"client2api/internal/core"
)

// TestWebLoginCNMatchesDesktopClientContract pins the mainland client's
// actual plugin-auth contract: authentication uses workbuddy.cn and the
// no-account calls carry the same X-No-* markers as the desktop app.  The
// global realm is deliberately not part of this test.
func TestWebLoginCNMatchesDesktopClientContract(t *testing.T) {
	v := &fakeVendor{
		stateBody: stateFixture("state-cn-contract", "https://www.workbuddy.cn/login?state=state-cn-contract"),
		token:     []vendorResp{{body: tokenFixture(loginSecretToken, "refresh-cn", "workbuddy.cn", 3600)}},
		account:   vendorResp{body: accountFixture("uid-cn-contract", "ent-cn", "CN Tester")},
	}
	c, _ := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmCN

	st := startLogin(t, c)
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	const base = "https://www.workbuddy.cn"
	if got, want := v.call(0).url, base+"/v2/plugin/auth/state?platform=workbuddy"; got != want {
		t.Fatalf("state url = %q, want %q", got, want)
	}
	assertCNNoAuthHeaders(t, v.call(0), false)

	if got := pollLogin(t, c, st.SessionID); got.State != core.LoginSuccess {
		t.Fatalf("poll state = %q (%s), want success", got.State, got.Message)
	}
	if got, want := v.call(1).url, base+"/v2/plugin/auth/token?state=state-cn-contract"; got != want {
		t.Fatalf("token url = %q, want %q", got, want)
	}
	assertCNNoAuthHeaders(t, v.call(1), false)

	if got, want := v.call(2).url, base+"/v2/plugin/login/account?state=state-cn-contract"; got != want {
		t.Fatalf("account url = %q, want %q", got, want)
	}
	for _, name := range []string{"X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		if got := v.call(2).headers.Get(name); got != "true" {
			t.Fatalf("account %s = %q, want true", name, got)
		}
	}
	if got := v.call(2).headers.Get("X-No-Authorization"); got != "" {
		t.Fatalf("account X-No-Authorization = %q, want empty", got)
	}
}

func TestWebLoginCNAuthURLAddsVersionAndClientLoginSession(t *testing.T) {
	const state = "state-cn-login-session"
	const hostSessionID = "host-provided-session"
	const authURL = "https://www.workbuddy.cn/login/?platform=workbuddy&state=" + state + "&loginSessionId=" + hostSessionID
	v := &fakeVendor{
		stateBody: stateFixture(state, authURL),
	}
	c, _ := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmCN
	c.up.ClientVersion = "5.7.7"

	st := startLogin(t, c)
	if st.SessionID != state {
		t.Fatalf("session id = %q, want %q", st.SessionID, state)
	}
	got, err := url.Parse(st.URL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got.Scheme != "https" || got.Host != "www.workbuddy.cn" || got.Path != "/login/" {
		t.Fatalf("auth URL changed unexpectedly: %q", st.URL)
	}
	if got.Query().Get("platform") != "workbuddy" || got.Query().Get("state") != state {
		t.Fatalf("auth URL lost the vendor parameters: %q", st.URL)
	}
	if got, want := got.Query().Get("version"), "5.7.7"; got != want {
		t.Fatalf("version = %q, want the desktop client version %q in %q", got, want, st.URL)
	}
	// The desktop client calls URL searchParams.set(), so its own per-login
	// UUID replaces any value the vendor happened to return.
	if got := got.Query().Get("loginSessionId"); !isUUIDv4(got) {
		t.Fatalf("loginSessionId = %q, want a client-generated UUID v4 in %q", got, st.URL)
	} else if got == hostSessionID {
		t.Fatalf("loginSessionId = %q, want the host value to be replaced", got)
	}

	sess := c.loginByID(st.SessionID)
	if sess == nil {
		t.Fatalf("session %q disappeared", st.SessionID)
	}
	if got := got.Query().Get("loginSessionId"); sess.loginSessionID == "" || sess.loginSessionID != got {
		t.Fatalf("stored loginSessionID = %q, URL value = %q, want the same stable value", sess.loginSessionID, got)
	}
}

func TestWebLoginCNAuthURLUsesAFreshLoginSessionPerAttempt(t *testing.T) {
	const state = "state-cn-fresh-login-session"
	v := &fakeVendor{
		stateBody: stateFixture(state, "https://www.workbuddy.cn/login?state="+state),
	}
	c, _ := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmCN

	first, err := c.startLoginSession(context.Background(), realmCN)
	if err != nil {
		t.Fatalf("first startLoginSession: %v", err)
	}
	second, err := c.startLoginSession(context.Background(), realmCN)
	if err != nil {
		t.Fatalf("second startLoginSession: %v", err)
	}
	if first.loginSessionID == "" || second.loginSessionID == "" {
		t.Fatalf("loginSessionID missing: first=%q second=%q", first.loginSessionID, second.loginSessionID)
	}
	if first.loginSessionID == second.loginSessionID {
		t.Fatalf("loginSessionID reused across attempts: %q", first.loginSessionID)
	}
}

func TestWebLoginGlobalAuthURLIsNotRewritten(t *testing.T) {
	const state = "state-global-login-session"
	const authURL = "https://www.workbuddy.ai/login?platform=workbuddy&state=" + state
	v := &fakeVendor{
		stateBody: stateFixture(state, authURL),
	}
	c, _ := loginClient(t, v, nil)
	c.cfg.LoginRealm = realmGlobal

	st := startLogin(t, c)
	if st.SessionID != state {
		t.Fatalf("session id = %q, want %q", st.SessionID, state)
	}
	if st.URL != authURL {
		t.Fatalf("auth URL = %q, want the vendor URL unchanged %q", st.URL, authURL)
	}
}

func assertCNNoAuthHeaders(t *testing.T, call vendorCall, allowAuthorization bool) {
	t.Helper()
	for _, name := range []string{"X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info"} {
		want := "true"
		if name == "X-No-Authorization" && allowAuthorization {
			want = ""
		}
		if got := call.headers.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

var uuidV4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func isUUIDv4(s string) bool {
	return uuidV4Pattern.MatchString(s)
}
