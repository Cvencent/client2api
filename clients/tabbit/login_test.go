package tabbit

// Offline tests for the browser-login hand-off (login.go).  Nothing here needs
// a browser, Node.js, Playwright or a real sidecar: the transport is a fake
// RoundTripper and the "operator" is the test itself advancing the state.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const wantsLoginURL = "https://web.tabbit.com/login?callback=close&flow=history_opt_in&theme=mn"

// downTransport refuses every connection, like a machine where tabbit2api has
// never been started.
func downTransport() roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:50124: connectex: No connection could be made because the target machine actively refused it")
	}
}

// sidecarTransport answers /health and /v1/models while up is true.  A closed
// port and an empty catalogue are different failures on purpose: the first is
// "not running", the second is "running but not signed in".
func sidecarTransport(up *atomic.Bool, healthBody, modelsBody string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if !up.Load() {
			return nil, errors.New("dial tcp 127.0.0.1:50124: connectex: No connection could be made")
		}
		switch r.URL.Path {
		case "/health":
			return jsonResponse(http.StatusOK, healthBody), nil
		case "/v1/models":
			return jsonResponse(http.StatusOK, modelsBody), nil
		}
		return jsonResponse(http.StatusNotFound, `{"error":"not found"}`), nil
	}
}

const (
	healthyHealth = `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.com"}`
	twoModels     = `{"object":"list","data":[{"id":"tabbit/priority"},{"id":"tabbit/GPT-5.5"}]}`
)

func loginClient(t *testing.T, cfg string, tp roundTripFunc) *Client {
	t.Helper()
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	return newTestClient(t, cfg, &http.Client{Transport: tp})
}

func TestTabbitLoginHandsOffTheBrowserFlow(t *testing.T) {
	c := loginClient(t, "", downTransport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.URL != wantsLoginURL {
		t.Errorf("login url = %q, want %q", st.URL, wantsLoginURL)
	}
	if st.State != core.LoginPending {
		t.Errorf("state = %q, want pending", st.State)
	}
	if !strings.HasPrefix(st.SessionID, loginSessionPrefix) {
		t.Errorf("session id = %q, want the %q namespace", st.SessionID, loginSessionPrefix)
	}
	if st.Code != "" {
		t.Errorf("a browser hand-off must not invent a code, got %q", st.Code)
	}
	if !strings.Contains(st.Message, "导入凭据") {
		t.Errorf("the message must point at the import action: %q", st.Message)
	}
	if !strings.Contains(st.Message, "web-token") {
		t.Errorf("the message must name what the import stores: %q", st.Message)
	}
	if !strings.Contains(st.Message, "not answering yet") {
		t.Errorf("the message must report that the sidecar is down: %q", st.Message)
	}
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Login {
		t.Fatal("tabbit implements LoginProvider but CapabilitiesOf does not report login")
	}
}

func TestTabbitLoginURLFollowsTheConfiguredWebHost(t *testing.T) {
	cases := []struct {
		name    string
		webHost string
		want    string
	}{
		{"unset falls back to the live host", "", wantsLoginURL},
		{"the upstream default is still reachable", "web.tabbit.ai",
			"https://web.tabbit.ai/login?callback=close&flow=history_opt_in&theme=mn"},
		{"a pasted url is reduced to its host", "https://web.tabbit.com/", wantsLoginURL},
		{"surrounding space is ignored", "  web.tabbit.com  ", wantsLoginURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := `{"base_url":"http://sidecar.test"}`
			if tc.webHost != "" {
				cfg = `{"base_url":"http://sidecar.test","web_host":"` + tc.webHost + `"}`
			}
			c := loginClient(t, cfg, downTransport())
			if got := loginURL(c.locate().webHost); got != tc.want {
				t.Errorf("loginURL() for web_host %q = %q, want %q", tc.webHost, got, tc.want)
			}
		})
	}
}

// The panel writes `web_host` onto the endpoint it stores, while the module
// config can only be edited by the operator.  A field that is stored and then
// ignored would be exactly the "looks done but is not" defect, so the stored
// value has to reach the URL the operator is told to open.
func TestTabbitLoginUsesTheHostStoredThroughThePanel(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	if got := loginURL(c.locate().webHost); got != wantsLoginURL {
		t.Fatalf("with nothing configured the URL must use the documented host: %q", got)
	}
	addEndpoint(t, c, map[string]string{
		"base_url": "http://sidecar.test",
		"web_host": "https://web.tabbit.ai/",
	})
	want := "https://web.tabbit.ai/login?callback=close&flow=history_opt_in&theme=mn"
	if got := loginURL(c.locate().webHost); got != want {
		t.Errorf("a host stored through the panel must reach the login URL: got %q, want %q", got, want)
	}
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(st.URL, "web.tabbit.ai") {
		t.Errorf("StartLogin returned %q, want the host stored through the panel", st.URL)
	}
}

func TestTabbitLoginPollsUntilTheSidecarListsModels(t *testing.T) {
	var up atomic.Bool
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, twoModels))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	waiting, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if waiting.State != core.LoginPending {
		t.Fatalf("state while the sidecar is down = %q, want pending", waiting.State)
	}
	if !strings.Contains(waiting.Message, "not usable yet") {
		t.Errorf("the pending message must name the failure: %q", waiting.Message)
	}

	up.Store(true)
	done, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("state = %q (message %q), want success", done.State, done.Message)
	}
	if done.AccountID != "http://sidecar.test" {
		t.Errorf("account id = %q, want the sidecar endpoint", done.AccountID)
	}
	if !strings.Contains(done.Message, "2 model") {
		t.Errorf("the success message must name the evidence: %q", done.Message)
	}
	if !strings.Contains(done.Message, "Nothing was stored") {
		t.Errorf("the success message must not imply a credential landed here: %q", done.Message)
	}

	// The catalogue the login proved is now the cached one, so the panel and
	// the gateway see the ids without another upstream call.
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "priority" {
		t.Errorf("catalogue after login = %v, want the two bare ids", ids(models))
	}

	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil || again.State != core.LoginSuccess {
		t.Errorf("a finished session must stay finished: state %q, err %v", again.State, err)
	}
}

func TestTabbitLoginStaysPendingWhileTheSidecarListsNothing(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, `{"object":"list","data":[]}`))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginPending {
		t.Fatalf("state = %q (message %q), want pending: a sidecar that lists nothing is running but not signed in", got.State, got.Message)
	}
	if !strings.Contains(got.Message, "listed no models") || !strings.Contains(got.Message, "does not look signed in") {
		t.Errorf("the message must distinguish empty from unreachable: %q", got.Message)
	}
}

func TestTabbitLoginWarnsAboutHostDrift(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	// The sidecar reports the host it drives; here it disagrees with the host
	// the login URL uses, which is the documented drift hazard.
	drift := sidecarTransport(&up, `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.ai"}`, twoModels)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, drift)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(st.Message, "web.tabbit.ai") || !strings.Contains(st.Message, "clients.tabbit.web_host") {
		t.Errorf("the message must surface host drift and how to fix it: %q", st.Message)
	}
}

func TestTabbitLoginRefusesAnUnknownSession(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	ctx := context.Background()

	_, err := c.PollLogin(ctx, "tabbit-login-nope")
	if err == nil {
		t.Fatal("PollLogin must refuse a session it never handed out")
	}
	if !strings.Contains(err.Error(), "tabbit-login-nope") {
		t.Errorf("the error must name the session: %v", err)
	}
	if _, err := c.PollLogin(ctx, "   "); err == nil {
		t.Fatal("PollLogin must refuse an empty session id")
	}
	if err := c.CancelLogin(ctx, ""); err == nil {
		t.Fatal("CancelLogin must refuse an empty session id")
	}
}

func TestTabbitLoginCancelIsIdempotent(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, twoModels))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(ctx, st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	if err := c.CancelLogin(ctx, st.SessionID); err != nil {
		t.Errorf("cancelling twice must be a no-op, got %v", err)
	}
	if _, err := c.PollLogin(ctx, st.SessionID); err == nil {
		t.Fatal("a cancelled session must not be pollable")
	}
	if _, ok := c.getLogin(st.SessionID); ok {
		t.Fatal("the cancelled session is still in the store")
	}
}

func TestTabbitLoginExpiresAForgottenSession(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	c.loginMu.Lock()
	c.logins[st.SessionID].startedAt = time.Now().Add(-loginSessionTTL - time.Minute)
	c.loginMu.Unlock()

	expired, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if expired.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed after the window", expired.State)
	}
	if !strings.Contains(expired.Message, "start a new login") {
		t.Errorf("the expired message must say what to do: %q", expired.Message)
	}
	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if again.State != core.LoginFailed {
		t.Errorf("an expired session must stay failed, got %q", again.State)
	}
}
