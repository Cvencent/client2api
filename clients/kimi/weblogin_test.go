package kimi

// Tests for the panel-driven web login (weblogin.go, token.go) and the direct
// HTTPS path (direct.go).
//
// Every test here runs against an httptest.Server.  Nothing in this file may
// touch the real auth.kimi.com: the flow is exercised end to end against a
// local stand-in that speaks the same wire protocol, so the suite stays
// offline and deterministic.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// A stand-in for the vendor's OAuth endpoints
// ---------------------------------------------------------------------------

// fakeOAuth is a minimal RFC 8628 issuer.  Each token poll returns the next
// queued answer, so a test can script "pending, then slow down, then success".
type fakeOAuth struct {
	srv *httptest.Server

	mu       sync.Mutex
	deviceID string
	userCode string
	verifyAt string
	expires  int
	interval int
	answers  []string // raw JSON bodies for successive token polls
	polls    int

	// deviceStatus/tokenStatus let a test force an HTTP failure.
	deviceStatus int
	tokenStatus  int

	// sawAuthHeader records the Authorization header of the last token poll.
	sawAuthHeader string
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{
		deviceID: "device-code-under-test",
		userCode: "TEST-CODE",
		verifyAt: "https://example.invalid/authorize_device?user_code=TEST-CODE",
		expires:  1800,
		interval: 5,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case deviceAuthPath:
		f.mu.Lock()
		status := f.deviceStatus
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"server_error"}`)
			return
		}
		body := map[string]any{
			"device_code":               f.deviceID,
			"user_code":                 f.userCode,
			"verification_uri":          "https://example.invalid/authorize_device",
			"verification_uri_complete": f.verifyAt,
			"expires_in":                f.expires,
			"interval":                  f.interval,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)

	case deviceTokenPath:
		// A real device-code client never sends the token; proving that here
		// keeps a future refactor from quietly adding one.
		f.mu.Lock()
		f.sawAuthHeader = r.Header.Get("Authorization")
		status := f.tokenStatus
		f.polls++
		var answer string
		if len(f.answers) > 0 {
			answer = f.answers[0]
			f.answers = f.answers[1:]
		} else {
			answer = `{"error":"authorization_pending"}`
		}
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"server_error"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(answer, `"access_token"`) {
			_, _ = io.WriteString(w, answer)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, answer)

	default:
		http.NotFound(w, r)
	}
}

// queue scripts the answers successive token polls receive.
func (f *fakeOAuth) queue(answers ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, answers...)
}

func (f *fakeOAuth) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// deviceClient builds a client wired to the stand-in issuer.
func deviceClient(t *testing.T, f *fakeOAuth, extra map[string]any) *Client {
	t.Helper()
	isolateCredentials(t)
	noCLIOnPath(t)

	cfg := map[string]any{"oauth_host": f.srv.URL}
	for k, v := range extra {
		cfg[k] = v
	}
	c, _ := newClient(t, cfg)
	return c
}

const successToken = `{"access_token":"tok-abcdefghijklmnopqrstuvwxyz","refresh_token":"ref-zyxwvutsrqponmlkjihgfedcba","expires_in":3600,"token_type":"Bearer","scope":"coding"}`

// ---------------------------------------------------------------------------
// Starting a login
// ---------------------------------------------------------------------------

func TestDeviceLoginStartAsksTheVendorAndHandsBackACode(t *testing.T) {
	f := newFakeOAuth(t)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state.State != core.LoginPending {
		t.Fatalf("state = %q, want pending (%s)", state.State, state.Message)
	}
	if !strings.HasPrefix(state.SessionID, deviceSessionPrefix) {
		t.Errorf("session = %q, want the %s prefix", state.SessionID, deviceSessionPrefix)
	}
	if state.Code != "TEST-CODE" {
		t.Errorf("code = %q, want TEST-CODE", state.Code)
	}
	if state.URL != f.verifyAt {
		t.Errorf("url = %q, want %q", state.URL, f.verifyAt)
	}
	// The message must not tell the operator to install a CLI: the whole point
	// of this flow is that no CLI is involved.
	if strings.Contains(state.Message, "Install it with") {
		t.Errorf("message = %q, want no CLI install guidance", state.Message)
	}
	if !strings.Contains(state.Message, "not needed") {
		t.Errorf("message = %q, want it to say the CLI is not needed", state.Message)
	}
	if state.AccountID != "" {
		t.Errorf("account_id = %q, want empty while pending", state.AccountID)
	}
}

func TestDeviceLoginStartReportsAVendorFailure(t *testing.T) {
	f := newFakeOAuth(t)
	f.deviceStatus = http.StatusInternalServerError
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin returned an error instead of a failed session: %v", err)
	}
	if state.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed", state.State)
	}
	if !strings.Contains(state.Message, "500") {
		t.Errorf("message = %q, want the HTTP status", state.Message)
	}
}

// A vendor that is simply unreachable must not look like a programming error.
func TestDeviceLoginStartReportsAnUnreachableVendor(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, map[string]any{"oauth_host": "http://127.0.0.1:1"})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin returned an error instead of a failed session: %v", err)
	}
	if state.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed", state.State)
	}
	if state.Message == "" {
		t.Error("a failed login must explain itself")
	}
}

// ---------------------------------------------------------------------------
// Polling a login
// ---------------------------------------------------------------------------

func TestDeviceLoginPollIsPendingUntilTheBrowserConfirms(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(
		`{"error":"authorization_pending"}`,
		`{"error":"authorization_pending"}`,
		successToken,
	)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The interval is 5s, so two immediate polls would be paced away.  Clear
	// the pacing to prove the *answer* is what drives the state.
	for i := 0; i < 2; i++ {
		c.acct.mu.Lock()
		if sess, ok := c.acct.device[state.SessionID]; ok {
			sess.nextPoll = zeroTime
		}
		c.acct.mu.Unlock()

		polled, err := c.PollLogin(context.Background(), state.SessionID)
		if err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
		if polled.State != core.LoginPending {
			t.Fatalf("poll %d: state = %q, want pending (%s)", i, polled.State, polled.Message)
		}
	}

	c.acct.mu.Lock()
	if sess, ok := c.acct.device[state.SessionID]; ok {
		sess.nextPoll = zeroTime
	}
	c.acct.mu.Unlock()

	done, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("state = %q, want success (%s)", done.State, done.Message)
	}
	if done.AccountID != webLoginID {
		t.Errorf("account_id = %q, want %q", done.AccountID, webLoginID)
	}
	if !strings.Contains(done.Message, "not required") {
		t.Errorf("message = %q, want it to say the CLI is not required", done.Message)
	}

	// Success sticks.
	again, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != core.LoginSuccess {
		t.Errorf("state = %q after a second poll, want success", again.State)
	}
}

func TestDeviceLoginSlowDownWidensTheInterval(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(`{"error":"slow_down"}`)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	c.acct.mu.Lock()
	before := c.acct.device[state.SessionID].interval
	c.acct.mu.Unlock()

	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if polled.State != core.LoginPending {
		t.Fatalf("state = %q, want pending", polled.State)
	}

	c.acct.mu.Lock()
	after := c.acct.device[state.SessionID].interval
	c.acct.mu.Unlock()

	if after != before+slowDownIncrement {
		t.Errorf("interval = %s, want %s", after, before+slowDownIncrement)
	}
	if !strings.Contains(polled.Message, "slow down") {
		t.Errorf("message = %q, want it to admit the vendor asked for slower polling", polled.Message)
	}
}

// The panel may poll faster than the vendor allows; a poll inside the interval
// must not spend a request.
func TestDeviceLoginHonoursThePollInterval(t *testing.T) {
	f := newFakeOAuth(t)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	first := f.pollCount()

	// The interval is 5s and no time has passed, so this must be answered
	// locally.
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	if f.pollCount() != first {
		t.Errorf("the vendor saw %d polls, want %d: the interval was ignored", f.pollCount(), first)
	}
}

func TestDeviceLoginRejectsExpiredAndDeniedCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		{"expired", `{"error":"expired_token"}`, "expired"},
		{"denied", `{"error":"access_denied"}`, "denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOAuth(t)
			f.queue(tc.answer)
			c := deviceClient(t, f, nil)

			state, err := c.StartLogin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			polled, err := c.PollLogin(context.Background(), state.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if polled.State != core.LoginFailed {
				t.Fatalf("state = %q, want failed", polled.State)
			}
			if !strings.Contains(polled.Message, tc.want) {
				t.Errorf("message = %q, want it to mention %q", polled.Message, tc.want)
			}
		})
	}
}

// A transient outage must not kill a login the operator is halfway through.
func TestDeviceLoginSurvivesATransientTokenFailure(t *testing.T) {
	f := newFakeOAuth(t)
	f.tokenStatus = http.StatusInternalServerError
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if polled.State != core.LoginPending {
		t.Fatalf("state = %q, want pending: a 5xx is transient", polled.State)
	}
	if !strings.Contains(polled.Message, "still pending") {
		t.Errorf("message = %q, want it to say the session survives", polled.Message)
	}
}

func TestDeviceLoginCancelAndUnknownSessions(t *testing.T) {
	f := newFakeOAuth(t)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CancelLogin(context.Background(), state.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if polled.State != core.LoginCancelled {
		t.Fatalf("state = %q, want cancelled", polled.State)
	}

	if _, err := c.PollLogin(context.Background(), deviceSessionPrefix+"nope"); err == nil {
		t.Error("PollLogin accepted an unknown device session")
	}
	if err := c.CancelLogin(context.Background(), deviceSessionPrefix+"nope"); err == nil {
		t.Error("CancelLogin accepted an unknown device session")
	}
	if _, err := c.PollLogin(context.Background(), ""); err == nil {
		t.Error("PollLogin accepted an empty session")
	}
	if err := c.CancelLogin(context.Background(), ""); err == nil {
		t.Error("CancelLogin accepted an empty session")
	}
}

// The device-code exchange must never present an access token it does not have.
func TestDeviceLoginSendsNoAuthorizationHeader(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	got := f.sawAuthHeader
	f.mu.Unlock()
	if got != "" {
		t.Errorf("the token poll sent Authorization: %q, want none", got)
	}
}

// ---------------------------------------------------------------------------
// The stored token
// ---------------------------------------------------------------------------

func TestDeviceLoginStoresTheTokenInsideTheDataDir(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}

	path := c.tokenPath()
	if path == "" {
		t.Fatal("tokenPath is empty; the token has nowhere to live")
	}
	if !strings.HasPrefix(path, c.cfg.dataDir) {
		t.Errorf("token path %q escapes the module's DataDir %q", path, c.cfg.dataDir)
	}
	// The module must not write into the CLI's own store: that would be
	// reaching outside its sandbox to mutate another program's login.
	if strings.Contains(path, ".kimi-code") {
		t.Errorf("token path %q writes into the CLI's own credentials directory", path)
	}

	tok, ok := c.loadToken()
	if !ok {
		t.Fatalf("no token was stored at %s", path)
	}
	if tok.AccessToken != "tok-abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("access_token = %q", tok.AccessToken)
	}
	if tok.RefreshToken != "ref-zyxwvutsrqponmlkjihgfedcba" {
		t.Errorf("refresh_token = %q", tok.RefreshToken)
	}
	if tok.OAuthHost != f.srv.URL {
		t.Errorf("oauth_host = %q, want %q", tok.OAuthHost, f.srv.URL)
	}
	if tok.ExpiresAt == 0 {
		t.Error("expires_at was not recorded")
	}
}

// A token file that is missing or corrupt degrades to "log in again" - it must
// never take the module down.
func TestTokenStoreDegradesQuietly(t *testing.T) {
	c := deviceClient(t, newFakeOAuth(t), nil)

	if _, ok := c.loadToken(); ok {
		t.Error("loadToken found a token that was never written")
	}

	path := c.tokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "not json", "{}", `{"access_token":""}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := c.loadToken(); ok {
			t.Errorf("loadToken accepted %q", body)
		}
	}
	if _, ok := c.tokenStatus(); ok {
		t.Error("tokenStatus reported an account for an unusable token file")
	}
}

func TestClearTokenIsIdempotent(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.loadToken(); !ok {
		t.Fatal("the token was not stored")
	}
	if err := c.clearToken(); err != nil {
		t.Fatalf("clearToken: %v", err)
	}
	if _, ok := c.loadToken(); ok {
		t.Error("the token survived clearToken")
	}
	if err := c.clearToken(); err != nil {
		t.Errorf("clearing an absent token returned an error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The token's effect on Status and Chat
// ---------------------------------------------------------------------------

func TestStoredTokenMakesStatusReadyWithoutTheCLI(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	// Before the login there is no CLI and no token: not ready.
	if st := c.Status(context.Background()); st.Ready {
		t.Fatalf("Status is ready before any login: %s", st.Detail)
	}

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}

	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("Status is not ready after a web login: %s", st.Detail)
	}
	if !strings.Contains(st.Detail, "CLI is not required") {
		t.Errorf("detail = %q, want it to explain the CLI is optional", st.Detail)
	}

	var found bool
	for _, a := range st.Accounts {
		if a.ID == webLoginID {
			found = true
			if !a.Enabled {
				t.Error("the web login account is not enabled")
			}
			if a.State != "ready" {
				t.Errorf("state = %q, want ready", a.State)
			}
			if a.ExpiresAt == "" {
				t.Error("the account does not report its expiry")
			}
		}
	}
	if !found {
		t.Errorf("accounts = %+v, want one with id %q", st.Accounts, webLoginID)
	}
}

// The token is a secret.  Nothing the panel renders may contain it.
func TestStoredTokenNeverLeaksThroughThePanel(t *testing.T) {
	const secret = "tok-abcdefghijklmnopqrstuvwxyz"

	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	polled, err := c.PollLogin(context.Background(), state.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	st := c.Status(context.Background())
	blobs := []string{state.Message, polled.Message, st.Detail}
	for _, a := range st.Accounts {
		blobs = append(blobs, a.ID, a.Label, a.State, a.ExpiresAt, a.Note)
		for k, v := range a.Extra {
			blobs = append(blobs, k, toText(v))
		}
	}
	for _, b := range blobs {
		if strings.Contains(b, secret) {
			t.Fatalf("the access token leaked into a panel-visible field: %q", b)
		}
	}
}

func toText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A disabled web login must not keep the module ready on the strength of its
// own token.
func TestDisablingTheWebLoginIsHonoured(t *testing.T) {
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, nil)

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	if !c.Status(context.Background()).Ready {
		t.Fatal("the web login did not make Status ready")
	}

	if err := c.SetAccountEnabled(context.Background(), webLoginID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if c.Status(context.Background()).Ready {
		t.Error("Status is still ready after the web login was disabled")
	}
}

// ---------------------------------------------------------------------------
// The direct HTTPS chat path
// ---------------------------------------------------------------------------

// fakeAPI serves a chat completions endpoint in the vendor's shape.
type fakeAPI struct {
	srv *httptest.Server

	mu     sync.Mutex
	auth   string
	body   map[string]any
	status int
	reply  string
	ct     string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		reply: strings.Join([]string{
			`data: {"choices":[{"delta":{"content":"hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n",
		ct: "text/event-stream",
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.status
	reply, ct := f.reply, f.ct
	f.auth = r.Header.Get("Authorization")
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.body = body
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"Invalid Authentication","type":"invalid_authentication_error"}}`)
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = io.WriteString(w, reply)
}

func (f *fakeAPI) seen() (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth, f.body
}

// loggedInClient returns a client with a token already stored and the coding
// API pointed at the stand-in.
func loggedInClient(t *testing.T, api *fakeAPI) *Client {
	t.Helper()
	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, map[string]any{"api_base": api.srv.URL})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDirectChatStreamsWithoutTheCLI(t *testing.T) {
	api := newFakeAPI(t)
	c := loggedInClient(t, api)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kimi-k2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	var done bool
	var usage *core.Usage
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			done = true
			if ev.Finish != "stop" {
				t.Errorf("finish = %q, want stop", ev.Finish)
			}
		case core.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	if !done {
		t.Error("the stream never reported done")
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want hello", text.String())
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want total 5", usage)
	}

	auth, body := api.seen()
	if auth != "Bearer tok-abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("Authorization = %q, want the stored bearer token", auth)
	}
	if body["model"] != "kimi-k2" {
		t.Errorf("model = %v, want kimi-k2", body["model"])
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want true", body["stream"])
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v, want one", body["messages"])
	}
}

func TestDirectChatHandlesAWholeDocumentResponse(t *testing.T) {
	api := newFakeAPI(t)
	api.reply = `{"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	api.ct = "application/json"
	c := loggedInClient(t, api)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == core.EventDelta {
			text.WriteString(ev.Delta)
		}
	}
	if text.String() != "hello" {
		t.Errorf("text = %q, want hello", text.String())
	}
}

// A rejected token must not take a machine that has a working CLI offline.
func TestDirectChatFallsBackToTheCLIWhenTheTokenIsRejected(t *testing.T) {
	api := newFakeAPI(t)
	api.status = http.StatusUnauthorized

	f := newFakeOAuth(t)
	f.queue(successToken)
	c := deviceClient(t, f, map[string]any{"api_base": api.srv.URL})

	state, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.PollLogin(context.Background(), state.SessionID); err != nil {
		t.Fatal(err)
	}

	// With no CLI either, the error has to name both failures.
	_, err = c.Chat(context.Background(), &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat succeeded although both routes are unavailable")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want the upstream status", err)
	}
	if !strings.Contains(err.Error(), "no fallback") {
		t.Errorf("error = %v, want it to say the CLI could not cover", err)
	}
}

// prefer_http:false keeps every request on the CLI.
func TestPreferHTTPFalseSkipsTheDirectPath(t *testing.T) {
	api := newFakeAPI(t)
	c := loggedInClient(t, api)

	off := false
	c.cfg.PreferHTTP = &off

	_, err := c.Chat(context.Background(), &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("Chat succeeded without a CLI although prefer_http is off")
	}
	if !strings.Contains(err.Error(), "kimi CLI not found") {
		t.Errorf("error = %v, want the CLI-missing message", err)
	}
	if auth, _ := api.seen(); auth != "" {
		t.Errorf("the coding API was called (Authorization %q) although prefer_http is off", auth)
	}
}

// ---------------------------------------------------------------------------
// Config plumbing
// ---------------------------------------------------------------------------

func TestLoginModeDefaultsToDevice(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{}, loginModeDevice},
		{Config{LoginMode: ""}, loginModeDevice},
		{Config{LoginMode: "nonsense"}, loginModeDevice},
		{Config{LoginMode: "device"}, loginModeDevice},
		{Config{LoginMode: "cli"}, loginModeCLI},
		{Config{LoginMode: "CLI"}, loginModeCLI},
		{Config{LoginMode: " cli "}, loginModeCLI},
	} {
		if got := tc.cfg.loginMode(); got != tc.want {
			t.Errorf("loginMode(%q) = %q, want %q", tc.cfg.LoginMode, got, tc.want)
		}
	}
}

func TestPreferHTTPDefaultsToTrue(t *testing.T) {
	var c Config
	if !c.preferHTTP() {
		t.Error("preferHTTP defaults to false; a panel login would then be pointless")
	}
	off := false
	c.PreferHTTP = &off
	if c.preferHTTP() {
		t.Error("preferHTTP ignored an explicit false")
	}
}

func TestOAuthHostPrecedence(t *testing.T) {
	t.Setenv("KIMI_CODE_OAUTH_HOST", "https://env.example")
	t.Setenv("KIMI_OAUTH_HOST", "https://env2.example")

	c := &Client{}
	if got := c.oauthHost(); got != "https://env.example" {
		t.Errorf("oauthHost = %q, want the env override", got)
	}
	c.cfg.OAuthHost = "https://config.example/"
	if got := c.oauthHost(); got != "https://config.example" {
		t.Errorf("oauthHost = %q, want the config value with its slash trimmed", got)
	}
}

func TestUpstreamDetailIsBoundedAndRedacted(t *testing.T) {
	if got := upstreamDetail(nil); got != "" {
		t.Errorf("upstreamDetail(nil) = %q, want empty", got)
	}
	long := strings.Repeat("x", 500)
	if got := upstreamDetail([]byte(long)); len(got) > 320 {
		t.Errorf("upstreamDetail returned %d bytes, want it bounded", len(got))
	}
	jwt := `{"error":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTYifQ.c2lnbmF0dXJlLWhlcmU"}`
	if got := upstreamDetail([]byte(jwt)); strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") {
		t.Errorf("upstreamDetail leaked a JWT: %q", got)
	}
}

func TestBuildOpenAIRequestCarriesToolsAndParts(t *testing.T) {
	req := &core.ChatRequest{
		Model: "m",
		Messages: []core.Message{
			{Role: "system", Content: "be brief"},
			{Role: "user", Parts: []core.ContentPart{{Type: "text", Text: "look"}, {Type: "image_url", ImageURL: "https://example.invalid/a.png", Detail: "low"}}},
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Type: "function", Name: "f", Arguments: "{}"}}},
			{Role: "tool", ToolCallID: "call_1", Content: "result"},
		},
		Tools: []core.Tool{{Type: "function", Name: "f", Description: "does f", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	// No shaping: this test is about tools and content parts, so it pins the
	// request the module built before prompt_cache_key and thinking existed.
	got := buildOpenAIRequest(req, "fallback", requestShaping{})

	if got.Model != "m" {
		t.Errorf("model = %q", got.Model)
	}
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(got.Messages))
	}
	parts, ok := got.Messages[1].Content.([]map[string]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v, want two parts", got.Messages[1].Content)
	}
	if got.Messages[2].ToolCalls[0].Function.Name != "f" {
		t.Errorf("tool call = %#v", got.Messages[2].ToolCalls)
	}
	if got.Messages[3].ToolCallID != "call_1" {
		t.Errorf("tool_call_id = %q", got.Messages[3].ToolCallID)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "f" {
		t.Errorf("tools = %#v", got.Tools)
	}
}

func TestBuildOpenAIRequestFallsBackToTheDefaultModel(t *testing.T) {
	got := buildOpenAIRequest(&core.ChatRequest{}, "default-model", requestShaping{})
	if got.Model != "default-model" {
		t.Errorf("model = %q, want the default", got.Model)
	}
}

// zeroTime defeats poll pacing in tests: setting nextPoll to the zero time
// makes the next poll go out immediately.
var zeroTime time.Time

// TestKimiChatNamesTheServedAccount pins the gateway-facing attribution for the
// direct HTTPS branch.  The credential that served a turn is known only inside
// Chat, and before this slot existed every success was filed under
// "(unrouted)".  loggedInClient drives the real device-flow login and the
// direct path against a stand-in vendor, so this is the module's normal
// success path, not a hand-built request.
func TestKimiChatNamesTheServedAccount(t *testing.T) {
	api := newFakeAPI(t)
	c := loggedInClient(t, api)

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kimi-k2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if served != webLoginID {
		t.Errorf("ServedBy = %q, want %q", served, webLoginID)
	}
}
