package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/browser"
	"client2api/internal/core"
)

// fakeBrowser stands in for the controlled browser so the login flow can be
// tested without launching anything.
type fakeBrowser struct {
	cookies []browser.Cookie
	closed  bool
}

func (f *fakeBrowser) Cookies(ctx context.Context, urls []string) ([]browser.Cookie, error) {
	return f.cookies, nil
}

func (f *fakeBrowser) Close() { f.closed = true }

// withFakeLaunch swaps the launcher for the duration of one test.
func withFakeLaunch(t *testing.T, br loginBrowser, err error) *loginBrowser {
	t.Helper()
	prev := launchBrowser
	launchBrowser = func(ctx context.Context, opts browser.LaunchOpts) (loginBrowser, error) {
		if err != nil {
			return nil, err
		}
		return br, nil
	}
	t.Cleanup(func() { launchBrowser = prev })
	return &br
}

// TestPollLoginStoresSessionAndSucceeds pins the happy path: cookies appear,
// the probe accepts them, and an account is stored.
func TestPollLoginStoresSessionAndSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"user_id":"42","nickname":"tester"}}`))
	}))
	defer srv.Close()

	fake := &fakeBrowser{cookies: []browser.Cookie{
		{Name: "sessionid", Value: "abc", Domain: ".doubao.com", HTTPOnly: true},
		{Name: "ttwid", Value: "xyz", Domain: ".doubao.com"},
	}}
	withFakeLaunch(t, fake, nil)

	c := newWebClient(t, srv.URL)
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}

	st, err = c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if st.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s), want success", st.State, st.Message)
	}
	if st.AccountID == "" {
		t.Fatalf("no account id after a successful login")
	}
	if accounts, _ := c.Accounts(context.Background()); len(accounts) != 1 {
		t.Fatalf("got %d accounts, want 1", len(accounts))
	}
	if !fake.closed {
		t.Fatalf("the controlled browser was not closed after a successful login")
	}
}

// TestPollLoginReplacesTheSameUserSession pins the stable identity: a second
// sign-in rotates the cookie jar but must update the existing account row, not
// create another one.
func TestPollLoginReplacesTheSameUserSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"user_id":"42","nickname":"tester"}}`))
	}))
	defer srv.Close()

	first := &fakeBrowser{cookies: []browser.Cookie{
		{Name: "sessionid", Value: "first", Domain: ".doubao.com"},
	}}
	withFakeLaunch(t, first, nil)
	c := newWebClient(t, srv.URL)
	st, _ := c.StartLogin(context.Background())
	if _, err := c.PollLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("first PollLogin: %v", err)
	}

	second := &fakeBrowser{cookies: []browser.Cookie{
		{Name: "sessionid", Value: "second", Domain: ".doubao.com"},
	}}
	withFakeLaunch(t, second, nil)
	st, _ = c.StartLogin(context.Background())
	if _, err := c.PollLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("second PollLogin: %v", err)
	}
	accounts, _ := c.Accounts(context.Background())
	if len(accounts) != 1 {
		t.Fatalf("got %d accounts after re-login, want 1", len(accounts))
	}
}

// TestPollLoginStaysPendingWithoutSessionCookie pins the "browser open, not
// signed in yet" case.
func TestPollLoginStaysPendingWithoutSessionCookie(t *testing.T) {
	fake := &fakeBrowser{cookies: []browser.Cookie{
		{Name: "ttwid", Value: "xyz", Domain: ".doubao.com"},
	}}
	withFakeLaunch(t, fake, nil)
	c := newWebClient(t, "http://127.0.0.1:1")
	st, _ := c.StartLogin(context.Background())
	st, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
}

// TestStartLoginHandlesLaunchFailure pins the manual fallback: a browser that
// cannot launch still leaves a pending session with a usable message.
func TestStartLoginHandlesLaunchFailure(t *testing.T) {
	withFakeLaunch(t, nil, errFakeLaunch)
	c := newWebClient(t, "http://127.0.0.1:1")
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin must not fail the request: %v", err)
	}
	if st.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", st.State)
	}
	if st.Message == "" {
		t.Fatalf("no message explaining the fallback")
	}
}

type fakeLaunchErr struct{}

func (fakeLaunchErr) Error() string { return "no browser found" }

var errFakeLaunch = fakeLaunchErr{}
