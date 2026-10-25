package zcode

// Offline tests for the panel sign-in flow (core.LoginProvider).
//
// Nothing here touches the network: every upstream call is answered by
// fakeTransport, every credential store is a temporary directory, and the
// vendor's responses are fixtures rather than a live service.  The wire shapes
// are the ones the reference implementation's mock upstream uses
// (zcode2api-lab/dengyie/tests/mock_upstream/server.py).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// Distinctive fixtures.  None of them can be confused with a hex fingerprint or
// a random account id, so the leak assertions below are meaningful.
const (
	testPollToken   = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testFlowID      = "mock-flow-1"
	testAuthorize   = "https://zcode.z.ai/authorize?flow=" + testFlowID
	testAccessToken = "zai-access-token-ABCDEFGHIJ"
	testBizToken    = "biz-token-KLMNOPQRST"
	testKeyID       = "keyid-abcdef0123456789"
	testKeySecret   = "secret-fedcba9876543210"
	testKeyMaterial = testKeyID + "." + testKeySecret
	testGatewayJWT  = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.signature-part"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func initResponse() *http.Response {
	return jsonResponse(http.StatusOK,
		`{"data":{"flow_id":"`+testFlowID+`","authorize_url":"`+testAuthorize+`"}}`)
}

func pollPending() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":{"status":"pending"}}`)
}

func pollReady() *http.Response {
	return jsonResponse(http.StatusOK,
		`{"data":{"status":"ready","token":"`+testGatewayJWT+`",`+
			`"zai":{"access_token":"`+testAccessToken+`"}}}`)
}

func pollReadyJWTOnly() *http.Response {
	return jsonResponse(http.StatusOK,
		`{"data":{"status":"ready","token":"`+testGatewayJWT+`"}}`)
}

func pollDenied() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":{"status":"failed","message":"user denied"}}`)
}

func loginExchangeResponse() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":{"access_token":"`+testBizToken+`"}}`)
}

func customerInfoResponse() *http.Response {
	return jsonResponse(http.StatusOK,
		`{"data":{"organizations":[{"organizationId":"mock-org-1",`+
			`"organizationName":"默认机构","projects":[{"projectId":"mock-proj-1",`+
			`"projectName":"默认项目"}]}]}}`)
}

func emptyKeysResponse() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":[]}`)
}

func createdKeyResponse() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":{"name":"`+exchangeKeyName+`","apiKey":"`+testKeyID+`"}}`)
}

func secretResponse() *http.Response {
	return jsonResponse(http.StatusOK, `{"data":{"secretKey":"`+testKeySecret+`"}}`)
}

// loginRoutes is the four-endpoint script of the sign-in flow.  A nil entry
// fails the test rather than silently answering.
type loginRoutes struct {
	initFn  func(*http.Request) (*http.Response, error)
	pollFn  func(*http.Request) (*http.Response, error)
	loginFn func(*http.Request) (*http.Response, error)
	infoFn  func(*http.Request) (*http.Response, error)
	keysFn  func(*http.Request) (*http.Response, error)
	copyFn  func(*http.Request) (*http.Response, error)
}

// happyRoutes is a full, successful sign-in.
func happyRoutes() loginRoutes {
	return loginRoutes{
		initFn:  static(initResponse),
		pollFn:  static(pollReady),
		loginFn: static(loginExchangeResponse),
		infoFn:  static(customerInfoResponse),
		keysFn:  keysRoute,
		copyFn:  static(secretResponse),
	}
}

// keysRoute answers the two calls the same URL takes: listing the keys finds
// none, and the POST that creates one returns the new key id.
func keysRoute(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		return createdKeyResponse(), nil
	}
	return emptyKeysResponse(), nil
}

func static(resp func() *http.Response) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) { return resp(), nil }
}

func (r loginRoutes) transport() *fakeTransport {
	return &fakeTransport{handler: func(req *http.Request) (*http.Response, error) {
		path := req.URL.Path
		var fn func(*http.Request) (*http.Response, error)
		switch {
		case strings.HasSuffix(path, oauthInitPath):
			fn = r.initFn
		case strings.Contains(path, oauthPollPath+"/"):
			fn = r.pollFn
		case strings.HasSuffix(path, exchangeLoginPath):
			fn = r.loginFn
		case strings.HasSuffix(path, exchangeCustomerPath):
			fn = r.infoFn
		case strings.Contains(path, "/api_keys/copy/"):
			fn = r.copyFn
		case strings.Contains(path, "/api_keys"):
			fn = r.keysFn
		}
		if fn == nil {
			return jsonResponse(http.StatusNotFound, `{"error":{"message":"unrouted request"}}`), nil
		}
		return fn(req)
	}}
}

// ---------------------------------------------------------------------------
// assertions
// ---------------------------------------------------------------------------

// assertNoDisguiseHeaders enforces the vendor's device-binding rule: the flow's
// own two requests carry the bearer token and nothing else.
func assertNoDisguiseHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	if req == nil {
		t.Fatal("no request was sent")
	}
	for _, h := range []string{
		"X-Platform", "X-ZCode-Session-Type", "X-Zcode-Trace-Id",
		"X-Device-Mid", "X-OS-Category", "X-Release-Channel", "Cookie",
	} {
		if v := req.Header.Get(h); v != "" {
			t.Errorf("the sign-in flow must not send %s (got %q)", h, v)
		}
	}
	if ua := req.Header.Get("User-Agent"); strings.HasPrefix(ua, "ZCode/") {
		t.Errorf("the sign-in flow must not impersonate the desktop client: user-agent %q", ua)
	}
}

// assertNoLeak fails if any rendered value contains one of the secrets.
func assertNoLeak(t *testing.T, what string, value any, secrets ...string) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaks %q: %s", what, secret, raw)
		}
		for _, n := range []int{4, 8, 12, 16} {
			if len(secret) > n && strings.Contains(string(raw), secret[:n]) {
				t.Fatalf("%s leaks a %d-character prefix of %q: %s", what, n, secret, raw)
			}
		}
	}
}

func loginStateText(s core.LoginState) string {
	return strings.Join([]string{s.SessionID, s.State, s.URL, s.Code, s.Message, s.AccountID}, "|")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// StartLogin
// ---------------------------------------------------------------------------

func TestStartLoginHappyPath(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.State != core.LoginPending {
		t.Errorf("state = %q, want %q", st.State, core.LoginPending)
	}
	if st.SessionID == "" {
		t.Error("the panel needs a session id to poll with")
	}
	if st.URL != testAuthorize {
		t.Errorf("url = %q, want the vendor's authorize_url", st.URL)
	}
	if st.Code != "" {
		t.Errorf("code = %q; the reference flow returns no user code", st.Code)
	}
	if st.Message == "" {
		t.Error("the panel needs something to show the operator")
	}

	if transport.count() != 1 {
		t.Fatalf("start must send exactly one request, sent %d", transport.count())
	}
	req := transport.requestAt(0)
	if req.Method != http.MethodPost {
		t.Errorf("method = %s", req.Method)
	}
	if req.URL.Path != "/api/v1"+oauthInitPath {
		t.Errorf("path = %s", req.URL.Path)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Fatalf("authorization = %q", auth)
	}
	token := strings.TrimPrefix(auth, "Bearer ")
	if len(token) != 2*pollTokenBytes {
		t.Errorf("poll token is %d chars, want %d (secrets.token_hex(32))", len(token), 2*pollTokenBytes)
	}
	assertNoDisguiseHeaders(t, req)
	if body := strings.TrimSpace(transport.bodyAt(0)); body != `{"provider":"zai"}` {
		t.Errorf("init body = %s", body)
	}

	// The session is on disk, and it is the only place the poll token lives.
	statePath := filepath.Join(env.dataDir, webLoginFile)
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("the pending session must be persisted: %v", err)
	}
	if !strings.Contains(readFile(t, statePath), token) {
		t.Error("the persisted session must carry the poll token it was started with")
	}
	// ...and it must never reach the panel.
	assertNoLeak(t, "LoginState", st, token)
	if _, err := os.Stat(filepath.Join(env.dataDir, managedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("start must not store a credential")
	}
}

func TestStartLoginRejectsIncompleteResponse(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.initFn = static(func() *http.Response {
		return jsonResponse(http.StatusOK, `{"data":{"flow_id":"only-a-flow-id"}}`)
	})
	c := env.client(t, routes.transport())

	_, err := c.StartLogin(context.Background())
	if err == nil {
		t.Fatal("an init response without an authorize_url must fail")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error = %v", err)
	}
}

func TestStartLoginSurfacesUpstreamFailure(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.initFn = static(func() *http.Response {
		return jsonResponse(http.StatusBadGateway, `{"error":{"message":"upstream is down"}}`)
	})
	c := env.client(t, routes.transport())

	_, err := c.StartLogin(context.Background())
	if err == nil {
		t.Fatal("a 502 from init must fail")
	}
	for _, want := range []string{"502", "upstream is down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

func TestStartLoginWithoutDataDir(t *testing.T) {
	transport := happyRoutes().transport()
	raw, err := New(core.Deps{Config: json.RawMessage(`{"auto_discover":false}`),
		HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := raw.(*Client)

	if _, err := c.StartLogin(context.Background()); err == nil {
		t.Fatal("without a data dir a credential could not be stored, so start must fail")
	}
	if transport.count() != 0 {
		t.Error("start must not contact the vendor when it cannot store the result")
	}
}

// ---------------------------------------------------------------------------
// PollLogin: the happy paths
// ---------------------------------------------------------------------------

func TestPollLoginPendingThenReadyStoresAPIKey(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var mu sync.Mutex
	polls := 0
	routes := happyRoutes()
	routes.pollFn = func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			return pollPending(), nil
		}
		return pollReady(), nil
	}
	transport := routes.transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	first, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (pending): %v", err)
	}
	if first.State != core.LoginPending {
		t.Fatalf("first poll state = %q, want %q", first.State, core.LoginPending)
	}
	if first.URL != testAuthorize {
		t.Errorf("a pending poll must keep showing the URL, got %q", first.URL)
	}
	if first.AccountID != "" {
		t.Errorf("a pending poll must not name an account, got %q", first.AccountID)
	}
	// The poll carries the bearer token and nothing else.
	pollReq := transport.requestAt(1)
	if pollReq.Method != http.MethodGet {
		t.Errorf("poll method = %s", pollReq.Method)
	}
	if want := "/api/v1" + oauthPollPath + "/" + testFlowID; pollReq.URL.Path != want {
		t.Errorf("poll path = %s, want %s", pollReq.URL.Path, want)
	}
	if got := pollReq.Header.Get("Content-Type"); got != "" {
		t.Errorf("the poll request must not send a content type, got %q", got)
	}
	if got := pollReq.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("poll authorization = %q", got)
	}
	assertNoDisguiseHeaders(t, pollReq)

	second, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (ready): %v", err)
	}
	if second.State != core.LoginSuccess {
		t.Fatalf("second poll state = %q (%s), want %q", second.State, second.Message, core.LoginSuccess)
	}
	if second.AccountID == "" {
		t.Fatal("a successful poll must name the stored account")
	}

	// The exchange walk, in order, with the business token on the API calls.
	var sawLogin, sawInfo, sawKeys, sawCopy bool
	for i := 0; i < transport.count(); i++ {
		req := transport.requestAt(i)
		switch {
		case strings.HasSuffix(req.URL.Path, exchangeLoginPath):
			sawLogin = true
			if req.Method != http.MethodPost {
				t.Errorf("z/login method = %s", req.Method)
			}
			if got := strings.TrimSpace(transport.bodyAt(i)); got != `{"token":"`+testAccessToken+`"}` {
				t.Errorf("z/login body = %s", got)
			}
			if req.Header.Get("Authorization") != "" {
				t.Error("z/login must not carry a bearer token")
			}
		case strings.HasSuffix(req.URL.Path, exchangeCustomerPath):
			sawInfo = true
			if got := req.Header.Get("Authorization"); got != "Bearer "+testBizToken {
				t.Errorf("getCustomerInfo authorization = %q", got)
			}
		case strings.Contains(req.URL.Path, "/api_keys/copy/"):
			sawCopy = true
			if !strings.HasSuffix(req.URL.Path, "/copy/"+testKeyID) {
				t.Errorf("copy path = %s", req.URL.Path)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer "+testBizToken {
				t.Errorf("copy authorization = %q", got)
			}
		case strings.Contains(req.URL.Path, "/api_keys"):
			sawKeys = true
			if got := req.Header.Get("Authorization"); got != "Bearer "+testBizToken {
				t.Errorf("api_keys authorization = %q", got)
			}
		}
	}
	if !sawLogin || !sawInfo || !sawKeys || !sawCopy {
		t.Fatalf("the exchange walk was incomplete: login=%v info=%v keys=%v copy=%v",
			sawLogin, sawInfo, sawKeys, sawCopy)
	}

	// The credential landed in the module's own store, marked as a panel login.
	recs := recordsByID(t, c)
	rec, ok := recs[second.AccountID]
	if !ok {
		t.Fatalf("account %q is not in the panel view: %+v", second.AccountID, recs)
	}
	if rec.Fields["kind"] != kindAPIKey {
		t.Errorf("kind = %v, want %v", rec.Fields["kind"], kindAPIKey)
	}
	if rec.Fields["provider"] != providerZai {
		t.Errorf("provider = %v", rec.Fields["provider"])
	}
	if rec.Fields["origin"] != originPanelLogin {
		t.Errorf("origin = %v, want %q", rec.Fields["origin"], originPanelLogin)
	}
	if rec.Fields["managed"] != true {
		t.Errorf("managed = %v", rec.Fields["managed"])
	}
	assertNoSecret(t, rec, testKeyMaterial)

	store := readFile(t, env.storePath())
	if !strings.Contains(store, testKeyMaterial) {
		t.Error("the API key must be persisted so the account survives a restart")
	}
	if strings.Contains(store, testPollToken) || strings.Contains(store, testAccessToken) {
		t.Error("the store must never keep the sign-in tokens")
	}

	// A repeated poll answers from memory: it must not run the exchange twice.
	before := transport.count()
	third, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (repeat): %v", err)
	}
	if third.State != core.LoginSuccess || third.AccountID != second.AccountID {
		t.Errorf("repeat poll = %+v", third)
	}
	if transport.count() != before {
		t.Errorf("a repeat poll re-contacted the vendor (%d -> %d)", before, transport.count())
	}
	if recs := recordsByID(t, c); len(recs) != 1 {
		t.Errorf("the flow stored %d accounts, want 1", len(recs))
	}
	// The pending-session file is gone once the flow is finished.
	if _, err := os.Stat(filepath.Join(env.dataDir, webLoginFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a finished session must not stay on disk")
	}
}

func TestPollLoginReadyWithJWTOnly(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(pollReadyJWTOnly)
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s)", got.State, got.Message)
	}
	rec := recordsByID(t, c)[got.AccountID]
	if rec.Fields["kind"] != kindJWT {
		t.Errorf("kind = %v, want %v", rec.Fields["kind"], kindJWT)
	}
	if rec.Fields["origin"] != originPanelLogin {
		t.Errorf("origin = %v", rec.Fields["origin"])
	}
	assertNoSecret(t, rec, testGatewayJWT)
}

func TestPollLoginExchangeFailureFailsAndStoresNothing(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.loginFn = static(func() *http.Response {
		return jsonResponse(http.StatusInternalServerError, `{"error":{"message":"exchange denied"}}`)
	})
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	// The access token is a *sign-in* token, not an account credential: a
	// failed API-key exchange is a failure.  The gateway JWT the vendor handed
	// over for this sign-in must never be persisted as an account, because it
	// expires silently and would hide the real error.  (Contrast
	// TestPollLoginReadyWithJWTOnly: with no access token at all, the JWT *is*
	// the issued credential and is stored.)
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q (%s), want %q", got.State, got.Message, core.LoginFailed)
	}
	if got.AccountID != "" {
		t.Errorf("account id = %q, want none", got.AccountID)
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("accounts = %d, want none", len(recs))
	}
	if _, err := os.Stat(env.storePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the credential store must not be written after a failed sign-in: %v", err)
	}
}

func TestPollLoginExchangeFailureWithoutJWTFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(func() *http.Response {
		return jsonResponse(http.StatusOK,
			`{"data":{"status":"ready","zai":{"access_token":"`+testAccessToken+`"}}}`)
	})
	routes.loginFn = static(func() *http.Response {
		return jsonResponse(http.StatusInternalServerError, `{"error":{"message":"exchange denied"}}`)
	})
	transport := routes.transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if !strings.Contains(got.Message, "500") {
		t.Errorf("message = %q, want it to mention the status", got.Message)
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("a failed exchange must store nothing, got %+v", recs)
	}
	assertNoLeak(t, "failure message", got, testAccessToken, testPollToken)
}

func TestPollLoginDenied(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(pollDenied)
	transport := routes.transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if !strings.Contains(got.Message, "user denied") {
		t.Errorf("message = %q, want the vendor's reason", got.Message)
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("a denied flow must store nothing, got %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, webLoginFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a terminal session must not stay on disk")
	}

	// Terminal sessions answer from memory, so the panel's next tick is not an
	// error and the vendor is not contacted again.
	before := transport.count()
	again, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (after denial): %v", err)
	}
	if again.State != core.LoginFailed {
		t.Errorf("state after denial = %q", again.State)
	}
	if transport.count() != before {
		t.Error("a terminal session must not be polled again")
	}
}

// ---------------------------------------------------------------------------
// PollLogin: the failure modes
// ---------------------------------------------------------------------------

func TestPollLoginExpiredSessionFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"login_ttl_seconds":300}`)
	routes := happyRoutes()
	transport := routes.transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// Age the session past its window instead of sleeping for the TTL.
	c.logins.mu.Lock()
	for _, s := range c.logins.byID {
		s.ExpiresAt = time.Now().Add(-time.Second)
	}
	c.logins.mu.Unlock()

	before := transport.count()
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("an expired session must be a state, not an error: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if !strings.Contains(got.Message, "expired") {
		t.Errorf("message = %q", got.Message)
	}
	if transport.count() != before {
		t.Error("an expired session must not be polled")
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("an expired flow must store nothing, got %+v", recs)
	}
}

func TestPollLoginUpstream5xxStaysPending(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var mu sync.Mutex
	polls := 0
	routes := happyRoutes()
	routes.pollFn = func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			return jsonResponse(http.StatusServiceUnavailable, `{"error":{"message":"try later"}}`), nil
		}
		return pollReady(), nil
	}
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	first, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if first.State != core.LoginPending {
		t.Fatalf("a 5xx must keep the flow alive, got %q", first.State)
	}
	if !strings.Contains(first.Message, "503") {
		t.Errorf("message = %q", first.Message)
	}
	// ...and the flow is still usable.
	second, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (retry): %v", err)
	}
	if second.State != core.LoginSuccess {
		t.Fatalf("state after the vendor recovered = %q (%s)", second.State, second.Message)
	}
}

func TestPollLoginTransportErrorStaysPending(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var mu sync.Mutex
	polls := 0
	routes := happyRoutes()
	routes.pollFn = func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			return nil, errors.New("dial tcp: connection refused")
		}
		return pollReady(), nil
	}
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	first, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("a transport failure must not abort the flow: %v", err)
	}
	if first.State != core.LoginPending {
		t.Fatalf("state = %q, want %q", first.State, core.LoginPending)
	}
	if !strings.Contains(first.Message, "could not be reached") {
		t.Errorf("message = %q", first.Message)
	}
}

func TestPollLoginClientErrorIsTerminal(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(func() *http.Response {
		return jsonResponse(http.StatusUnauthorized, `{"error":{"message":"poll denied"}}`)
	})
	transport := routes.transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if !strings.Contains(got.Message, "401") {
		t.Errorf("message = %q, want it to mention the status", got.Message)
	}

	before := transport.count()
	again, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin (after 401): %v", err)
	}
	if again.State != core.LoginFailed {
		t.Errorf("state = %q", again.State)
	}
	if transport.count() != before {
		t.Error("a terminal 401 must not be retried")
	}
}

func TestPollLoginMalformedJSONFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(func() *http.Response {
		return newResponse(http.StatusOK, "text/html", "<html>not json</html>")
	})
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if !strings.Contains(got.Message, "unreadable") {
		t.Errorf("message = %q", got.Message)
	}
}

func TestPollLoginReadyWithoutCredentialFails(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	routes := happyRoutes()
	routes.pollFn = static(func() *http.Response {
		return jsonResponse(http.StatusOK, `{"data":{"status":"ready"}}`)
	})
	c := env.client(t, routes.transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginFailed {
		t.Fatalf("state = %q, want %q", got.State, core.LoginFailed)
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("nothing may be stored, got %+v", recs)
	}
}

func TestPollLoginUnknownSession(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	if _, err := c.PollLogin(context.Background(), "no-such-session"); err == nil {
		t.Error("polling an unknown session must fail")
	}
	if _, err := c.PollLogin(context.Background(), "   "); err == nil {
		t.Error("an empty session id must fail")
	}
	if err := c.CancelLogin(context.Background(), "no-such-session"); err == nil {
		t.Error("cancelling an unknown session must fail")
	}
	if err := c.CancelLogin(context.Background(), ""); err == nil {
		t.Error("an empty session id must fail to cancel")
	}
}

// ---------------------------------------------------------------------------
// CancelLogin
// ---------------------------------------------------------------------------

func TestCancelLoginLeavesNothingBehind(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}

	got, err := c.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after cancel: %v", err)
	}
	if got.State != core.LoginCancelled {
		t.Fatalf("state = %q, want %q", got.State, core.LoginCancelled)
	}
	if got.AccountID != "" {
		t.Errorf("a cancelled flow must not name an account, got %q", got.AccountID)
	}
	if transport.count() != 1 {
		t.Errorf("a cancelled flow must not be polled (sent %d requests)", transport.count())
	}
	if recs := recordsByID(t, c); len(recs) != 0 {
		t.Errorf("a cancelled flow must store nothing, got %+v", recs)
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, webLoginFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the cancelled session must be removed from disk")
	}
	if _, err := os.Stat(filepath.Join(env.dataDir, managedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("cancel must leave no credential behind")
	}
}

// ---------------------------------------------------------------------------
// persistence and redaction
// ---------------------------------------------------------------------------

func TestPollLoginSurvivesRestart(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	transport := happyRoutes().transport()

	first := env.client(t, transport)
	st, err := first.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}

	// A second client over the same data dir is what a gateway restart looks
	// like: the pending session has to be readable from disk.
	second := env.client(t, transport)
	got, err := second.PollLogin(context.Background(), st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin after restart: %v", err)
	}
	if got.State != core.LoginSuccess {
		t.Fatalf("state = %q (%s)", got.State, got.Message)
	}
	if got.AccountID == "" {
		t.Error("the credential must be stored after a restart")
	}
	if recs := recordsByID(t, second); len(recs) != 1 {
		t.Errorf("the restarted client sees %d accounts, want 1", len(recs))
	}
}

func TestWebLoginFileIsPruned(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, happyRoutes().transport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	c.logins.mu.Lock()
	for _, s := range c.logins.byID {
		s.ExpiresAt = time.Now().Add(-webLoginRetention - time.Minute)
	}
	c.logins.mu.Unlock()

	// Starting again prunes the dead session and rewrites the file.
	if _, err := c.StartLogin(context.Background()); err != nil {
		t.Fatalf("StartLogin (second): %v", err)
	}
	c.logins.mu.Lock()
	_, stale := c.logins.byID[st.SessionID]
	c.logins.mu.Unlock()
	if stale {
		t.Error("a session past its retention window must be forgotten")
	}
}

func TestLoginFlowNeverLeaksTokens(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	var mu sync.Mutex
	polls := 0
	routes := happyRoutes()
	routes.pollFn = func(*http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		if polls == 1 {
			return pollPending(), nil
		}
		return pollReady(), nil
	}
	routes.loginFn = static(func() *http.Response {
		// A failing exchange exercises the error path, which is where a token
		// is most likely to escape into a message.
		return jsonResponse(http.StatusInternalServerError,
			`{"error":{"message":"token `+testAccessToken+` was rejected"}}`)
	})
	transport := routes.transport()
	c := env.client(t, transport)

	secrets := []string{testPollToken, testAccessToken, testBizToken, testKeyID, testKeySecret, testGatewayJWT}

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	assertNoLeak(t, "StartLogin state", st, secrets...)
	// The poll token is on the wire, so grab it to prove it never comes back.
	auth := transport.requestAt(0).Header.Get("Authorization")
	live := strings.TrimPrefix(auth, "Bearer ")
	if live == "" {
		t.Fatal("no poll token was sent")
	}
	assertNoLeak(t, "StartLogin state (live token)", st, live)

	states := []core.LoginState{st}
	for i := 0; i < 3; i++ {
		got, err := c.PollLogin(context.Background(), st.SessionID)
		if err != nil {
			assertNoLeak(t, "PollLogin error", err.Error(), live)
			break
		}
		states = append(states, got)
		assertNoLeak(t, "PollLogin state", got, append([]string{live}, secrets...)...)
	}

	for i, s := range states {
		assertNoLeak(t, "state "+loginStateText(s), s, live)
		if i > 0 && strings.Contains(loginStateText(s), live) {
			t.Fatal("a poll state leaked the poll token")
		}
	}

	// The panel's account records are the other way out to the operator.
	for id, rec := range recordsByID(t, c) {
		assertNoSecret(t, rec, testKeyMaterial)
		assertNoLeak(t, "record "+id, rec, live, testAccessToken, testBizToken, testGatewayJWT)
	}

	// And the module's own files: the credential store may hold the API key
	// (that is its job) but never the sign-in tokens.  A store that was never
	// written is the strongest possible answer here, because the failed
	// exchange above has nothing legitimate to persist.
	raw, err := os.ReadFile(env.storePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read %s: %v", env.storePath(), err)
	}
	store := string(raw)
	for name, secret := range map[string]string{
		"the live poll token": live,
		"the access token":    testAccessToken,
		"the business token":  testBizToken,
		"the gateway JWT":     testGatewayJWT,
	} {
		if secret != "" && strings.Contains(store, secret) {
			t.Fatalf("the credential store leaked %s", name)
		}
	}
}

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

func TestLoginConfigDefaultsAndOverrides(t *testing.T) {
	base := loadConfig(json.RawMessage(`{"auto_discover":false}`), nil)
	if got := base.oauthAPIBase(); got != "https://zcode.z.ai/api/v1" {
		t.Errorf("oauth api base = %q", got)
	}
	if got := base.oauthExchangeBase(); got != "https://api.z.ai" {
		t.Errorf("oauth exchange base = %q", got)
	}
	if got := base.loginTTL(); got != 300*time.Second {
		t.Errorf("login ttl = %v, want 5m (the reference's LOGIN_FLOW_TTL)", got)
	}
	if base.OAuthClientID != "" {
		t.Errorf("the reference flow sends no client id, got %q", base.OAuthClientID)
	}

	over := loadConfig(json.RawMessage(`{"oauth_api_base":"http://127.0.0.1:9/api/v1/",`+
		`"oauth_exchange_base":"http://127.0.0.1:9/","oauth_client_id":"cid-1","login_ttl_seconds":30}`), nil)
	if got := over.oauthAPIBase(); got != "http://127.0.0.1:9/api/v1" {
		t.Errorf("overridden api base = %q (a trailing slash must be trimmed)", got)
	}
	if got := over.oauthExchangeBase(); got != "http://127.0.0.1:9" {
		t.Errorf("overridden exchange base = %q", got)
	}
	if got := over.loginTTL(); got != 30*time.Second {
		t.Errorf("overridden ttl = %v", got)
	}
}

func TestLoginUsesConfiguredBasesAndClientID(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"oauth_api_base":"https://api.example.test/api/v1",`+
		`"oauth_exchange_base":"https://exchange.example.test","oauth_client_id":"cid-42"}`)
	transport := happyRoutes().transport()
	c := env.client(t, transport)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	req := transport.requestAt(0)
	if req.URL.Host != "api.example.test" || req.URL.Path != "/api/v1"+oauthInitPath {
		t.Errorf("init went to %s", req.URL)
	}
	if body := strings.TrimSpace(transport.bodyAt(0)); body != `{"client_id":"cid-42","provider":"zai"}` {
		t.Errorf("init body = %s", body)
	}

	if _, err := c.PollLogin(context.Background(), st.SessionID); err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	for i := 0; i < transport.count(); i++ {
		if host := transport.requestAt(i).URL.Host; host == "zcode.z.ai" || host == "api.z.ai" {
			t.Errorf("request %d went to the default host %s despite the override", i, host)
		}
	}
}

// TestUserCodeFromAuthorizeURL covers the one field the reference does not
// document: when the vendor puts a short code in the authorisation URL, the
// panel gets it as LoginState.Code.
func TestUserCodeFromAuthorizeURL(t *testing.T) {
	cases := map[string]string{
		"https://zcode.z.ai/authorize?user_code=WXYZ-1234": "WXYZ-1234",
		"https://zcode.z.ai/authorize?code=ABCD-5678":      "ABCD-5678",
		"https://zcode.z.ai/authorize?flow=abc":            "",
		"https://zcode.z.ai/authorize":                     "",
		"://not a url":                                     "",
	}
	for raw, want := range cases {
		if got := userCodeFromURL(raw); got != want {
			t.Errorf("userCodeFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
