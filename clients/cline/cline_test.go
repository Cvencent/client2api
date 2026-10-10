package cline

import (
	"context"
	"encoding/json"
	"errors"
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
// fixtures
// ---------------------------------------------------------------------------

// fakeServer is one httptest server standing in for both api.cline.bot and
// api.workos.com.  It records every request so a test can assert on the
// Authorization header and the decoded body rather than on an outcome alone.
type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
	handlers map[string]http.HandlerFunc
}

type recordedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{t: t, handlers: map[string]http.HandlerFunc{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   raw,
		})
		h := f.handlers[r.URL.Path]
		f.mu.Unlock()
		if h == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"no handler for ` + r.URL.Path + `"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) handle(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[path] = h
}

func (f *fakeServer) json(path string, status int, body string) {
	f.handle(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// requestsFor returns every recorded request for a path.
func (f *fakeServer) requestsFor(path string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.requests {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// count returns how many requests hit a path.
func (f *fakeServer) count(path string) int { return len(f.requestsFor(path)) }

// newTestClient builds a Client pointed at the fake server with a DataDir in a
// temp directory, and returns it as the concrete type plus its data dir.
func newTestClient(t *testing.T, f *fakeServer, extra string) *Client {
	t.Helper()
	dir := t.TempDir()
	raw := `{"api_base":"` + f.srv.URL + `","app_base":"` + f.srv.URL +
		`","workos_base":"` + f.srv.URL + `","models_timeout":"5s","chat_timeout":"10s"`
	if extra != "" {
		raw += "," + extra
	}
	raw += "}"
	deps := core.Deps{
		DataDir:    dir,
		Config:     json.RawMessage(raw),
		HTTPClient: f.srv.Client(),
		Logf:       func(format string, args ...any) { t.Logf(format, args...) },
	}
	built, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}
	return c
}

// grantBody renders a successful register/refresh envelope.
func grantBody(access, refresh, expiresAt, accountID, email, first, last string) string {
	ui := ""
	if accountID != "" || email != "" || first != "" || last != "" {
		ui = `,"userInfo":{"clineUserId":"` + accountID + `","email":"` + email +
			`","firstName":"` + first + `","lastName":"` + last + `"}`
	}
	return `{"success":true,"data":{"accessToken":"` + access + `","refreshToken":"` + refresh +
		`","expiresAt":"` + expiresAt + `","tokenType":"Bearer"` + ui + `}}`
}

// --- the workos: prefix trap ----------------------------------------------

// TestAuthorizationKeepsWorkOSPrefix is the load-bearing measurement: the
// on-disk token is workos:-prefixed and the prefix must survive into the
// Authorization header verbatim.  The fake server enforces the vendor's rule,
// so a client that stripped the prefix would fail this test.
func TestAuthorizationKeepsWorkOSPrefix(t *testing.T) {
	f := newFakeServer(t)
	const token = "workos:eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyXzAxIn0.c2ln"
	f.json(userInfoPath, http.StatusOK, `{"data":{"clineUserId":"usr-1","email":"a@b.c"}}`)

	c := newTestClient(t, f, `"access_token":"`+token+`","account_id":"usr-1"`)

	req := &core.ChatRequest{Model: "cline-free/deepseek-v4.1-flash", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	if _, err := c.Chat(context.Background(), req); err == nil {
		// The chat path has no handler, so a 404 is expected; what matters is
		// the header the request carried.
		t.Log("chat returned no error, which is fine for this assertion")
	}

	got := f.requestsFor(chatPath)
	if len(got) == 0 {
		t.Fatalf("no request reached %s", chatPath)
	}
	auth := got[0].Header.Get("Authorization")
	if auth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want %q (the workos: prefix must be preserved)", auth, "Bearer "+token)
	}
	if !strings.HasPrefix(auth, "Bearer workos:") {
		t.Fatalf("Authorization lost the workos: prefix: %q", auth)
	}
}

// TestStrippedPrefixIsRejected documents *why* the prefix matters: a server that
// enforces the vendor's rule answers 401, whose body misleadingly blames the
// client version.  The module must therefore never strip it.
func TestStrippedPrefixIsRejected(t *testing.T) {
	f := newFakeServer(t)
	const token = "workos:eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1c2VyXzAxIn0.c2ln"
	f.handle(userInfoPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"make sure you're using the latest version of Cline"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"clineUserId":"usr-1"}}`))
	})

	c := newTestClient(t, f, `"access_token":"`+token+`"`)
	acct := account{AccessToken: token}

	if _, err := c.whoAmI(context.Background(), acct); err != nil {
		t.Fatalf("whoAmI with the intact prefix should succeed: %v", err)
	}
	stripped := account{AccessToken: bareToken(token)}
	if _, err := c.whoAmI(context.Background(), stripped); err == nil {
		t.Fatal("whoAmI with the prefix stripped should be rejected (this is the trap)")
	}
}

// TestBareTokenIsNotPrefixNormalizedOnRead guards the other direction: the
// header value is whatever the credential holds, and normalizeToken is the only
// place the prefix is added.
func TestNormalizeTokenAddsPrefixOnce(t *testing.T) {
	if got := normalizeToken("eyJx"); got != "workos:eyJx" {
		t.Fatalf("normalizeToken(bare) = %q, want workos:eyJx", got)
	}
	if got := normalizeToken("workos:eyJx"); got != "workos:eyJx" {
		t.Fatalf("normalizeToken(prefixed) = %q, want it unchanged", got)
	}
	if got := bareToken("workos:eyJx"); got != "eyJx" {
		t.Fatalf("bareToken = %q, want eyJx", got)
	}
}

// --- the camelCase refresh body -------------------------------------------

// TestRefreshBodyUsesCamelCase decodes the body the module actually sent.  The
// vendor requires refreshToken/grantType; the OAuth-standard spellings fail
// with a generic auth error, so this is asserted on the decoded JSON.
func TestRefreshBodyUsesCamelCase(t *testing.T) {
	f := newFakeServer(t)
	f.json(refreshPath, http.StatusOK, grantBody("workos:new", "new-refresh", "2026-09-25T05:23:47.000Z", "usr-1", "a@b.c", "Ada", "Lovelace"))

	c := newTestClient(t, f, `"access_token":"workos:old","refresh_token":"old-refresh","account_id":"usr-1"`)
	if _, err := c.refresh(context.Background(), "old-refresh"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	got := f.requestsFor(refreshPath)
	if len(got) != 1 {
		t.Fatalf("refresh requests = %d, want 1", len(got))
	}
	var body map[string]any
	if err := json.Unmarshal(got[0].Body, &body); err != nil {
		t.Fatalf("decoding the refresh body: %v (%s)", err, got[0].Body)
	}
	if body["refreshToken"] != "old-refresh" {
		t.Fatalf("body refreshToken = %v, want old-refresh (the camelCase field is required)", body["refreshToken"])
	}
	if body["grantType"] != "refresh_token" {
		t.Fatalf("body grantType = %v, want refresh_token", body["grantType"])
	}
	if _, present := body["refresh_token"]; present {
		t.Fatal("body carries the snake_case refresh_token, which the vendor rejects")
	}
	if _, present := body["grant_type"]; present {
		t.Fatal("body carries the snake_case grant_type, which the vendor rejects")
	}
}

// TestRefreshKeepsIdentityWhenUserInfoOmitted is the identity-preservation
// trap: a refresh routinely answers without userInfo, and blanking account_id
// would break the balance endpoint, which needs the account id and rejects the
// JWT sub.
func TestRefreshKeepsIdentityWhenUserInfoOmitted(t *testing.T) {
	f := newFakeServer(t)
	f.json(refreshPath, http.StatusOK,
		`{"success":true,"data":{"accessToken":"workos:rotated","refreshToken":"r2","tokenType":"Bearer"}}`)

	c := newTestClient(t, f, `"access_token":"workos:old","refresh_token":"r1","account_id":"usr-KEEPME","email":"keep@me","nickname":"Keep Me"`)
	e := c.pool.usable()
	if len(e) != 1 {
		t.Fatalf("pool holds %d usable accounts, want 1", len(e))
	}
	before := c.pool.accountOf(e[0])
	if before.AccountID != "usr-KEEPME" {
		t.Fatalf("fixture account_id = %q, want usr-KEEPME", before.AccountID)
	}

	if err := c.tryRefresh(context.Background(), e[0]); err != nil {
		t.Fatalf("tryRefresh: %v", err)
	}

	ent := c.pool.find(before.id())
	if ent == nil {
		t.Fatal("the account vanished after the refresh (a refresh must update the record in place, not mint a new one)")
	}
	after := c.pool.accountOf(ent)
	if after.AccountID != "usr-KEEPME" {
		t.Fatalf("account_id after a userInfo-less refresh = %q, want it preserved", after.AccountID)
	}
	if after.Email != "keep@me" || after.Nickname != "Keep Me" {
		t.Fatalf("identity blanked by refresh: email=%q nickname=%q", after.Email, after.Nickname)
	}
	if after.AccessToken != "workos:rotated" {
		t.Fatalf("access token after refresh = %q, want workos:rotated", after.AccessToken)
	}
	if after.RefreshToken != "r2" {
		t.Fatalf("refresh token after refresh = %q, want r2", after.RefreshToken)
	}
}

// TestAccountIDComesFromUserInfoClineUserID pins the account_id source.
func TestAccountIDComesFromUserInfoClineUserID(t *testing.T) {
	f := newFakeServer(t)
	f.json(refreshPath, http.StatusOK, grantBody("workos:x", "r", "2026-09-25T05:23:47.000Z", "usr-01M3BCV4FYCGJKAWD3MJG3DBQM", "a@b.c", "Ada", "Lovelace"))

	c := newTestClient(t, f, `"access_token":"workos:old","refresh_token":"r"`)
	grant, err := c.refresh(context.Background(), "r")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	a := accountFromGrant(account{}, grant)
	if a.AccountID != "usr-01M3BCV4FYCGJKAWD3MJG3DBQM" {
		t.Fatalf("AccountID = %q, want the clineUserId", a.AccountID)
	}
	if a.Nickname != "Ada Lovelace" {
		t.Fatalf("Nickname = %q, want \"Ada Lovelace\" (trimmed first+last)", a.Nickname)
	}
	if a.ExpiresAt != mustUnix(t, "2026-09-25T05:23:47.000Z") {
		t.Fatalf("ExpiresAt = %d, want %d", a.ExpiresAt, mustUnix(t, "2026-09-25T05:23:47.000Z"))
	}
}

// TestTestAccountSendsARealChatRequest pins the change the operator asked for:
// the panel's 测试 button must send a real completion, not just read the
// profile.
func TestTestAccountSendsARealChatRequest(t *testing.T) {
	f := newFakeServer(t)
	f.handle(chatPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n"))
	})
	f.json(userInfoPath, http.StatusOK, `{"data":{"clineUserId":"usr-1","email":"a@b.c"}}`)
	c := newTestClient(t, f, `"access_token":"workos:tok","account_id":"usr-1"`)

	res, err := c.TestAccount(context.Background(), "acct:usr-1")
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount failed: %+v", res)
	}
	if res.Reply != "pong" {
		t.Fatalf("Reply = %q, want the streamed text", res.Reply)
	}
	if f.count(chatPath) != 1 {
		t.Fatalf("chat requests = %d, want 1", f.count(chatPath))
	}
	if f.count(userInfoPath) != 0 {
		t.Fatalf("the probe still read the profile %d time(s)", f.count(userInfoPath))
	}
}

func mustUnix(t *testing.T, s string) int64 {
	t.Helper()
	ts, ok := parseTimeString(s)
	if !ok {
		t.Fatalf("parseTimeString(%q) failed", s)
	}
	return ts.Unix()
}

// --- the failure envelope -------------------------------------------------

// TestSuccessFalseEnvelopeIsRejected is trap 2: a bare accessToken read would
// accept this envelope, because data.accessToken is present.
func TestSuccessFalseEnvelopeIsRejected(t *testing.T) {
	f := newFakeServer(t)
	f.json(registerPath, http.StatusOK,
		`{"success":false,"data":{"accessToken":"workos:should-not-be-used"},"error":"invalid_grant","message":"denied"}`)

	c := newTestClient(t, f, "")
	if _, err := c.register(context.Background(), "workos-workos", "r"); err == nil {
		t.Fatal("register accepted a success:false envelope")
	}
}

// TestMissingDataEnvelopeIsRejected covers the other half: success true but no
// token is still a failure.
func TestMissingDataEnvelopeIsRejected(t *testing.T) {
	f := newFakeServer(t)
	f.json(registerPath, http.StatusOK, `{"success":true,"data":{}}`)
	c := newTestClient(t, f, "")
	if _, err := c.register(context.Background(), "t", "r"); err == nil {
		t.Fatal("register accepted a token-less data envelope")
	}
}

// --- the catalogue --------------------------------------------------------

// TestCatalogueMergesThreeSources pins the authoritative-refresh rule: once a
// live source answers, the built-in table may only fill in entries the vendor
// still advertises.  /api/v1/models lists no cline-free/* ids at all, so the
// free array is what keeps a free id in the catalogue -- and what removes it
// again once the vendor retires the model.
func TestCatalogueMergesThreeSources(t *testing.T) {
	f := newFakeServer(t)
	// The metered list: no cline-free/* ids anywhere.
	f.json(modelsPath, http.StatusOK, `{"data":[
        {"id":"deepseek/deepseek-v4.1-flash","object":"model","created":1,"owned_by":"deepseek"},
        {"id":"anthropic/claude-sonnet-4.5","object":"model","created":1,"owned_by":"anthropic"}
    ]}`)
	// The recommended endpoint carries the free array, and one recommendation
	// that the metered list does not have.
	f.json(recommendedPath, http.StatusOK, `{
        "data":[
            {"id":"cline-free/gemini-3.8-flash","name":"Gemini 3.8 Flash","description":"x","tags":["free"]},
            {"id":"stealth/space-bunny-alpha","name":"Space Bunny Alpha","description":"y","tags":["free"]}
        ],
        "free":["cline-free/gemini-3.8-flash","stealth/space-bunny-alpha","cline-free/mimo-v2.6-flash","cline-free/muse-spark-1.3-contributor"]
    }`)

	c := newTestClient(t, f, `"access_token":"workos:t"`)
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	ids := map[string]core.Model{}
	for _, m := range models {
		ids[m.ID] = m
	}
	for _, want := range []string{
		"cline-free/mimo-v2.6-flash",
		"cline-free/gemini-3.8-flash",
		"cline-free/muse-spark-1.3-contributor",
		"stealth/space-bunny-alpha",
		"deepseek/deepseek-v4.1-flash",
		"anthropic/claude-sonnet-4.5",
	} {
		if _, ok := ids[want]; !ok {
			t.Errorf("merged catalogue is missing %q", want)
		}
	}
	if _, ok := ids["cline-free/deepseek-v4.1-flash"]; ok {
		t.Error("a free id the vendor no longer advertises survived the refresh via the built-in table")
	}
	// The metered sibling must be free=false while the surviving free id is true.
	if got := ids["deepseek/deepseek-v4.1-flash"].Extra["free"]; got != false {
		t.Errorf("deepseek/deepseek-v4.1-flash free = %v, want false", got)
	}
	if got := ids["cline-free/mimo-v2.6-flash"].Extra["free"]; got != true {
		t.Errorf("cline-free/mimo-v2.6-flash free = %v, want true", got)
	}
	// The list endpoint really did not carry the free ids.
	if f.count(modelsPath) != 1 {
		t.Fatalf("models endpoint called %d times, want 1", f.count(modelsPath))
	}
}

func TestRecommendedFreeObjectsEnterTheCatalogue(t *testing.T) {
	f := newFakeServer(t)
	f.json(modelsPath, http.StatusOK, `{"data":[
        {"id":"stepfun/step-5-preview","object":"model","owned_by":"stepfun"}
    ]}`)
	f.json(recommendedPath, http.StatusOK, `{
        "recommended":[
            {"id":"anthropic/claude-sonnet-5.5","name":"claude-sonnet-5.5"}
        ],
        "free":[
            {"id":"cline-free/step-5-preview","name":"Step 5 Preview","description":"StepFun's flagship model for agentic work","tags":[]}
        ]
    }`)

	c := newTestClient(t, f, `"access_token":"workos:t"`)
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	ids := map[string]core.Model{}
	for _, m := range models {
		ids[m.ID] = m
	}
	got, ok := ids["cline-free/step-5-preview"]
	if !ok {
		t.Fatalf("merged catalogue is missing cline-free/step-5-preview: %v", modelIDs(models))
	}
	if got.Extra["free"] != true {
		t.Fatalf("free = %v, want true", got.Extra["free"])
	}
	if got.Extra["display_name"] != "Step 5 Preview" {
		t.Fatalf("display_name = %v, want Step 5 Preview", got.Extra["display_name"])
	}
}

// TestFreenessIsNeverDecidedByName asserts the rule directly: the same model
// name in two namespaces must land on different verdicts, and an entry named
// "free" must not be free without evidence.
func TestFreenessIsNeverDecidedByName(t *testing.T) {
	cases := []struct {
		id   string
		free bool
		why  string
	}{
		{"cline-free/deepseek-v4.1-flash", true, "the cline-free/ namespace is free"},
		{"deepseek/deepseek-v4.1-flash", false, "the metered sibling is not free"},
		{"openai/gpt-free-trial", false, "a name containing \"free\" proves nothing"},
		{"something/paid:free", true, "the :free suffix is free"},
	}
	for _, tc := range cases {
		got := isFreeID(tc.id)
		if got != tc.free {
			t.Errorf("isFreeID(%q) = %v, want %v (%s)", tc.id, got, tc.free, tc.why)
		}
	}
	// And an entry's own flag is honoured when the id proves nothing.
	if !entryFree(modelEntry{ID: "vendor/model", IsFree: true}, nil) {
		t.Error("an entry with isFree=true should be free")
	}
	if entryFree(modelEntry{ID: "vendor/model", Name: "Free Tier"}, nil) {
		t.Error("a model merely NAMED free must not be free")
	}
}

// TestGeminiFlashOutputLimitIs65536 pins the measured number: sending 131072 to
// this model is rejected upstream, so the catalogue must carry 65536.
func TestGeminiFlashOutputLimitIs65536(t *testing.T) {
	f := newFakeServer(t)
	f.json(modelsPath, http.StatusOK, `{"data":[]}`)
	f.json(recommendedPath, http.StatusOK, `{"data":[],"free":[]}`)

	c := newTestClient(t, f, "")
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	limit, ok := core.OutputLimitFor(models, "cline-free/gemini-3.8-flash")
	if !ok {
		t.Fatal("no output limit for cline-free/gemini-3.8-flash")
	}
	if limit != 65536 {
		t.Fatalf("cline-free/gemini-3.8-flash limit = %d, want 65536", limit)
	}
	if limit == 131072 {
		t.Fatal("the limit is the generic 131072, which the vendor rejects for this model")
	}
	// The other free models keep their own values.
	for id, want := range map[string]int{
		"cline-free/deepseek-v4.1-flash":        131072,
		"cline-free/mimo-v2.6-flash":            131072,
		"cline-free/muse-spark-1.3-contributor": 943718,
		"stealth/space-bunny-alpha":             524288,
	} {
		got, ok := core.OutputLimitFor(models, id)
		if !ok || got != want {
			t.Errorf("%s limit = %d (ok=%v), want %d", id, got, ok, want)
		}
	}
}

// TestModelMaxOutputTokensUsesCacheOnly is the "must never fetch" rule: the
// answer comes from the cache, and a cold cache declines instead of making a
// metadata round trip inside a chat request.
func TestModelMaxOutputTokensUsesCacheOnly(t *testing.T) {
	f := newFakeServer(t)
	f.json(modelsPath, http.StatusOK, `{"data":[]}`)
	f.json(recommendedPath, http.StatusOK, `{"data":[],"free":[]}`)

	c := newTestClient(t, f, "")

	// Cold cache: nothing fetched yet, so the answer must be "cannot say" and
	// must NOT have triggered a fetch.
	if limit, ok := c.ModelMaxOutputTokens(context.Background(), "cline-free/gemini-3.8-flash"); ok {
		t.Fatalf("cold cache answered %d, want no answer", limit)
	}
	if f.count(modelsPath) != 0 || f.count(recommendedPath) != 0 {
		t.Fatalf("ModelMaxOutputTokens fetched the catalogue (models=%d recommended=%d)",
			f.count(modelsPath), f.count(recommendedPath))
	}

	// Warm the cache, then assert the answer arrives with no further calls.
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	before := f.count(modelsPath) + f.count(recommendedPath)
	limit, ok := c.ModelMaxOutputTokens(context.Background(), "cline-free/gemini-3.8-flash")
	if !ok || limit != 65536 {
		t.Fatalf("warm cache answered (%d, %v), want (65536, true)", limit, ok)
	}
	after := f.count(modelsPath) + f.count(recommendedPath)
	if after != before {
		t.Fatalf("ModelMaxOutputTokens made %d extra network call(s)", after-before)
	}
}

// TestFallbackCatalogueAnswersWithoutNetwork checks that a module with no
// credential and no reachable endpoint still names the free models.
func TestFallbackCatalogueAnswersWithoutNetwork(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["cline-free/gemini-3.8-flash"] {
		t.Fatal("the built-in table does not carry cline-free/gemini-3.8-flash")
	}
}

// --- configuration and status ---------------------------------------------

func TestNoCredentialIsErrNotConfigured(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "m"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat with no credential = %v, want ErrNotConfigured", err)
	}
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("Status reports ready with no credential")
	}
	if !strings.Contains(st.Detail, envAccessToken) {
		t.Fatalf("Status.Detail does not name the credential sources: %q", st.Detail)
	}
}

func TestNilRequestAndBlankModelAreUnsupported(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, `"access_token":"workos:t"`)
	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("Chat(nil) = %v, want ErrUnsupported", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "  "}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("Chat(blank model) = %v, want ErrUnsupported", err)
	}
}

func TestConcurrencyCeilingIsErrBusy(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, `"access_token":"workos:t","max_concurrency":1`)
	if !c.enter() {
		t.Fatal("the first enter() should succeed")
	}
	defer c.leave()
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "m"}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Chat at the ceiling = %v, want ErrBusy", err)
	}
}

func TestDurationFieldAcceptsStringAndNumber(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"chat_timeout":"90s","models_ttl":120}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.chatTimeout() != 90*time.Second {
		t.Fatalf("chat_timeout = %v, want 90s", cfg.chatTimeout())
	}
	if cfg.modelsTTL() != 120*time.Second {
		t.Fatalf("models_ttl = %v, want 120s (a bare number is seconds)", cfg.modelsTTL())
	}
}

func TestParseConfigToleratesGarbage(t *testing.T) {
	cfg, err := parseConfig(json.RawMessage(`{"chat_timeout":"nonsense"}`))
	if err != nil {
		t.Fatalf("parseConfig returned an error for a nonsense value: %v", err)
	}
	if cfg.chatTimeout() != defaultChatTimeout {
		t.Fatalf("chat_timeout = %v, want the default", cfg.chatTimeout())
	}
}

// --- reasoning effort -----------------------------------------------------

func TestReasoningEffortTableAndDefault(t *testing.T) {
	for tier, want := range reasoningEffortTable {
		if got := mapEffort(tier); got != want {
			t.Errorf("mapEffort(%q) = %q, want %q", tier, got, want)
		}
	}
	if got := mapEffort(""); got != "" {
		t.Errorf("mapEffort(\"\") = %q, want \"\" (defaulting happens in reasoningEffort)", got)
	}
	if got := reasoningEffort(nil, ""); got != reasoningEffortTable[defaultReasoningEffort] {
		t.Errorf("reasoningEffort(nil, \"\") = %q, want the default %q", got, reasoningEffortTable[defaultReasoningEffort])
	}
	// An unknown tier is passed through verbatim: the vendor rejects it with an
	// explicit message, which is more useful than a silent rewrite.
	if got := mapEffort("banana"); got != "banana" {
		t.Errorf("mapEffort(banana) = %q, want it passed through", got)
	}
}

// TestReasoningEffortWireValuesAreTheOnesTheVendorAccepts pins the exact
// spelling the vendor validates.  The live endpoint answers
// `reasoning_effort: Invalid option: expected one of
// "max"|"xhigh"|"high"|"medium"|"low"|"minimal"|"none"` and fails the stream for
// anything else, so a capitalised wire value is not a cosmetic difference: it
// breaks every request that carries it, including the built-in default.
func TestReasoningEffortWireValuesAreTheOnesTheVendorAccepts(t *testing.T) {
	accepted := map[string]bool{
		"max": true, "xhigh": true, "high": true, "medium": true,
		"low": true, "minimal": true, "none": true,
	}
	for tier, wire := range reasoningEffortTable {
		if !accepted[wire] {
			t.Errorf("reasoningEffortTable[%q] = %q, which the vendor rejects", tier, wire)
		}
		if wire != strings.ToLower(wire) {
			t.Errorf("reasoningEffortTable[%q] = %q, want lower case (the vendor's set is lower case)", tier, wire)
		}
	}
	for tier := range accepted {
		if _, ok := reasoningEffortTable[tier]; !ok {
			t.Errorf("reasoningEffortTable is missing the tier %q the vendor accepts", tier)
		}
	}
	if got := reasoningEffort(nil, ""); got != "high" {
		t.Errorf("the default wire value = %q, want \"high\"", got)
	}
}

func TestBuildBodyAlwaysSendsReasoningEffort(t *testing.T) {
	body, err := buildBody(&core.ChatRequest{
		Model:    "cline-free/deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options:  map[string]any{"reasoning_effort": "max"},
	}, 32000, "high")
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	if got["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max (the vendor's own spelling)", got["reasoning_effort"])
	}
	if got["stream"] != true {
		t.Fatalf("stream = %v, want true", got["stream"])
	}
}

func TestBuildBodyOmitsNothingWhenNoOptionGiven(t *testing.T) {
	body, err := buildBody(&core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}, 32000, "")
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	// Omitting reasoning_effort means the model does not think at all, so the
	// field must always be present.
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high (the field must never be omitted)", got["reasoning_effort"])
	}
}

// --- the device flow ------------------------------------------------------

// TestRunDeviceFlowStoresACredential drives the whole WorkOS flow against the
// fake server: start, poll once (approved), exchange, store.
func TestRunDeviceFlowStoresACredential(t *testing.T) {
	f := newFakeServer(t)
	f.handle(workOSDevicePath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://app.cline.bot/device","expires_in":600,"interval":1}`))
	})
	polls := 0
	f.handle(workOSAuthenticatePath, func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.Header().Set("Content-Type", "application/json")
		if polls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"workos-workos","refresh_token":"workos-refresh","token_type":"Bearer"}`))
	})
	f.json(registerPath, http.StatusOK, grantBody("workos:eyJfinal", "final-refresh", "2026-09-25T05:23:47.000Z", "usr-final", "final@example.com", "Ada", "Lovelace"))

	dir := t.TempDir()
	raw := `{"api_base":"` + f.srv.URL + `","app_base":"` + f.srv.URL + `","workos_base":"` + f.srv.URL +
		`","poll_interval":"1s","login_timeout":"20s"}`
	deps := core.Deps{
		DataDir:    dir,
		Config:     json.RawMessage(raw),
		HTTPClient: f.srv.Client(),
		Logf:       func(format string, args ...any) { t.Logf(format, args...) },
	}
	var out strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := RunDeviceFlow(ctx, deps, &out); err != nil {
		t.Fatalf("RunDeviceFlow: %v", err)
	}
	if !strings.Contains(out.String(), "app.cline.bot/device") {
		t.Fatalf("the flow did not print the URL to open: %q", out.String())
	}

	// The credential must be on disk and must carry the workos: prefix.
	raw2, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatalf("reading %s: %v", accountsFile, err)
	}
	if !strings.Contains(string(raw2), "workos:eyJfinal") {
		t.Fatalf("the stored credential does not carry the final token: %s", raw2)
	}
	if _, err := os.Stat(filepath.Join(dir, loginFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the login file survived a completed flow: %v", err)
	}

	// The exchange really did send the WorkOS token to the register endpoint.
	regs := f.requestsFor(registerPath)
	if len(regs) != 1 {
		t.Fatalf("register requests = %d, want 1", len(regs))
	}
	var regBody map[string]any
	if err := json.Unmarshal(regs[0].Body, &regBody); err != nil {
		t.Fatalf("decoding the register body: %v", err)
	}
	if regBody["accessToken"] != "workos-workos" {
		t.Fatalf("register body accessToken = %v, want the WorkOS token", regBody["accessToken"])
	}
}

// --- credential discovery -------------------------------------------------

// TestDiscoverDegradesWhenClineIsNotInstalled is the measured condition on this
// machine: ~/.cline/data/settings/providers.json does not exist, so discovery
// must report an empty list rather than fail.
func TestDiscoverDegradesWhenClineIsNotInstalled(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, d := range found {
		if d.Importable {
			t.Fatalf("discovery claimed an importable credential at %s", d.Path)
		}
	}
}

// TestDiscoverFindsATokenInProvidersJSON checks the importer against a fixture
// in the vendor's own shape.
func TestDiscoverFindsATokenInProvidersJSON(t *testing.T) {
	f := newFakeServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".cline", "data", "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "providers.json")
	fixture := `{"cline":{"providers":{"cline":{"settings":{"accessToken":"workos:eyJhbGciOiJIUzI1NiJ9.eyJjbGluZVVzZXJJZCI6InVzci1mcm9tLWZpbGUifQ.c2ln"}}}}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	c := newTestClient(t, f, "")
	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var importable int
	for _, d := range found {
		if d.Importable {
			importable++
			if d.Path != path {
				t.Fatalf("importable path = %q, want %q", d.Path, path)
			}
		}
	}
	if importable != 1 {
		t.Fatalf("importable credentials = %d, want 1", importable)
	}

	recs, err := c.Import(context.Background(), []string{path}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("imported %d records, want 1", len(recs))
	}
	e := c.pool.usable()
	if len(e) != 1 {
		t.Fatalf("pool holds %d accounts after import, want 1", len(e))
	}
	if e[0].acct.AccountID != "usr-from-file" {
		t.Fatalf("imported account_id = %q, want usr-from-file (read from the JWT claim)", e[0].acct.AccountID)
	}
}

// --- the account_id vs sub trap -------------------------------------------

// TestBalanceRejectsMismatchedStoredAccountID pins the trap: the balance probe
// must use the credential's own account id, and a stored id that disagrees with
// the credential is reported rather than used.
func TestBalanceRejectsMismatchedStoredAccountID(t *testing.T) {
	f := newFakeServer(t)
	// The stored id is usr-wrong, so that is the path the probe must build; the
	// vendor answers for usr-real, which is the disagreement under test.
	f.json(balancePath("usr-wrong"), http.StatusOK, `{"data":{"userId":"usr-real","balance":7},"success":true}`)

	c := newTestClient(t, f, `"access_token":"workos:t","account_id":"usr-wrong"`)
	e := c.pool.usable()
	if len(e) != 1 {
		t.Fatalf("pool holds %d accounts, want 1", len(e))
	}
	if _, err := c.AccountBalance(context.Background(), e[0].acct.id(), 0); err == nil {
		t.Fatal("AccountBalance accepted a stored account id that disagrees with the credential")
	}
}

// TestBalanceReportsCreditsWhenPresent covers the happy path of the probe.  The
// numbers and the route are the measured ones: GET
// /api/v1/users/{accountId}/balance -> {"data":{"userId":…,"balance":496429}}.
func TestBalanceReportsCreditsWhenPresent(t *testing.T) {
	f := newFakeServer(t)
	f.json(balancePath("usr-1"), http.StatusOK, `{"data":{"userId":"usr-1","balance":496429},"success":true}`)

	c := newTestClient(t, f, `"access_token":"workos:t","account_id":"usr-1"`)
	e := c.pool.usable()
	bal, err := c.AccountBalance(context.Background(), e[0].acct.id(), 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 496429 {
		t.Fatalf("balance = %+v, want credits 496429", bal)
	}
	if bal.Unit != "credits" {
		t.Fatalf("balance unit = %q, want credits", bal.Unit)
	}
	// The account-scoped route is the whole point: the profile endpoint carries
	// no credit figure, so a probe that fell back to it would answer 502.
	if n := f.count(balancePath("usr-1")); n != 1 {
		t.Fatalf("balance route hit %d times, want 1", n)
	}
	if n := f.count(userInfoPath); n != 0 {
		t.Fatalf("balance probe also hit the profile endpoint %d times; it carries no credit figure", n)
	}
}

// TestBalanceNeedsAnAccountID documents the fallback order: with no stored id the
// probe reads one out of the credential rather than refusing to answer, and with
// neither it says so plainly instead of querying a path it cannot build.
func TestBalanceNeedsAnAccountID(t *testing.T) {
	f := newFakeServer(t)
	f.json(userInfoPath, http.StatusOK, `{"data":{"clineUserId":"usr-from-token"}}`)
	f.json(balancePath("usr-from-token"), http.StatusOK, `{"data":{"userId":"usr-from-token","balance":11},"success":true}`)

	c := newTestClient(t, f, `"access_token":"workos:t"`)
	acct := account{AccessToken: "workos:t"}

	bal, err := c.balance(context.Background(), acct, "")
	if err != nil {
		t.Fatalf("balance with no stored id should fall back to the credential's own id: %v", err)
	}
	if bal.Credits != 11 {
		t.Fatalf("balance = %+v, want credits 11", bal)
	}

	// A credential that names no account anywhere cannot build the route.
	empty := newFakeServer(t)
	empty.json(userInfoPath, http.StatusOK, `{"data":{"email":"a@b.c"}}`)
	c2 := newTestClient(t, empty, `"access_token":"workos:t"`)
	if _, err := c2.balance(context.Background(), account{AccessToken: "workos:t"}, ""); err == nil {
		t.Fatal("balance invented a route for a credential that names no account id")
	}
}

// --- streaming ------------------------------------------------------------

// TestStreamEmitsDeltasUsageAndDone drives the SSE decoder over a canned body.
func TestStreamEmitsDeltasUsageAndDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		"",
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		"",
		`data: {"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
		"",
		`data: {"id":"1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
		"",
		`data: {"id":"1","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	s := newClineStream(context.Background(), nil, io.NopCloser(strings.NewReader(body)))
	defer s.Close()

	var deltas, reasoning []string
	var usage *core.Usage
	var finish string
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			if ev.Delta != "" {
				deltas = append(deltas, ev.Delta)
			}
			if ev.Reasoning != "" {
				reasoning = append(reasoning, ev.Reasoning)
			}
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			finish = ev.Finish
		}
	}
	if strings.Join(deltas, "") != "Hello" {
		t.Fatalf("deltas = %v, want Hello", deltas)
	}
	if strings.Join(reasoning, "") != "think" {
		t.Fatalf("reasoning = %v, want think", reasoning)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v, want total 5", usage)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q, want stop", finish)
	}
}

// TestStreamReportsAnEmptyStream guards the "no data at all" case, which must
// be an error rather than a silent success.
func TestStreamReportsAnEmptyStream(t *testing.T) {
	s := newClineStream(context.Background(), nil, io.NopCloser(strings.NewReader("data: [DONE]\n\n")))
	defer s.Close()
	_, err := s.Recv()
	if err == nil || !strings.Contains(err.Error(), "without sending any data") {
		t.Fatalf("Recv on an empty stream = %v, want the empty-stream error", err)
	}
}

// TestStreamSurfacesUpstreamError covers an error member mid-stream.
func TestStreamSurfacesUpstreamError(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"model is not available\"}}\n\ndata: [DONE]\n\n"
	s := newClineStream(context.Background(), nil, io.NopCloser(strings.NewReader(body)))
	defer s.Close()
	_, err := s.Recv()
	if err == nil || !strings.Contains(err.Error(), "model is not available") {
		t.Fatalf("Recv = %v, want the upstream error", err)
	}
}

// TestStreamCloseIsIdempotent checks the Stream contract.
func TestStreamCloseIsIdempotent(t *testing.T) {
	s := newClineStream(context.Background(), nil, io.NopCloser(strings.NewReader("data: [DONE]\n\n")))
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// --- tool calls and images ------------------------------------------------

func TestBuildBodyPassesToolsAndImages(t *testing.T) {
	body, err := buildBody(&core.ChatRequest{
		Model: "cline-free/deepseek-v4.1-flash",
		Messages: []core.Message{{
			Role: "user",
			Parts: []core.ContentPart{
				{Type: "text", Text: "what is this?"},
				{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
			},
		}},
		Tools: []core.Tool{{
			Type:        "function",
			Name:        "lookup",
			Description: "look something up",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
		ToolChoice: json.RawMessage(`"auto"`),
	}, 32000, "high")
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	parts, ok := first["content"].([]any)
	if !ok {
		t.Fatalf("content = %T, want an array of parts (images must survive)", first["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if _, ok := got["tools"].([]any); !ok {
		t.Fatalf("tools missing from the body: %s", body)
	}
	if got["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v, want auto", got["tool_choice"])
	}
}

func TestToolsSuppressedByNoneChoice(t *testing.T) {
	body, err := buildBody(&core.ChatRequest{
		Model:      "m",
		Messages:   []core.Message{{Role: "user", Content: "hi"}},
		Tools:      []core.Tool{{Type: "function", Name: "x"}},
		ToolChoice: json.RawMessage(`"none"`),
	}, 32000, "high")
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}
	if _, present := got["tools"]; present {
		t.Fatalf("tools were sent despite tool_choice none: %s", body)
	}
}

// --- the expired-account deadlock -----------------------------------------

// TestAnExpiredAccountIsRefreshedBeforeThePoolIsAsked guards a deadlock that
// made the whole module permanently unusable once its only credential lapsed.
//
// The pre-refresh pass used to iterate the *usable* set, and availableLocked
// reports an expired account as unavailable -- so the one account that most
// needed renewing could never appear in the list that renews accounts.  The
// pool then had nothing to hand out and Chat returned ErrNotConfigured for
// ever, which the gateway renders as "client is not configured (no account)" /
// HTTP 503.  Nothing but an operator re-running Cline's own CLI could break
// the loop.
//
// The measurement is the refresh request itself, not the returned stream: a
// client that skips the pre-refresh fails here even though a later 401-driven
// refresh could, in principle, paper over it on some other code path.
func TestAnExpiredAccountIsRefreshedBeforeThePoolIsAsked(t *testing.T) {
	f := newFakeServer(t)
	f.json(refreshPath, http.StatusOK,
		grantBody("workos:fresh", "rt-2", "2030-01-01T00:00:00.000Z", "usr-1", "a@b.c", "Ada", "Lovelace"))
	f.json(chatPath, http.StatusOK, "data: [DONE]\n\n")

	c := newTestClient(t, f, "")
	c.pool.put(account{
		ID:           "lapsed",
		AccessToken:  "workos:stale",
		RefreshToken: "rt-1",
		AccountID:    "usr-1",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	})

	s, err := c.Chat(context.Background(), &core.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("Chat with an expired but refreshable account = %v, want a stream", err)
	}
	_ = s.Close()

	if got := f.count(refreshPath); got != 1 {
		t.Fatalf("the refresh endpoint was called %d times, want 1", got)
	}
	got := f.requestsFor(chatPath)
	if len(got) != 1 {
		t.Fatalf("the chat endpoint was called %d times, want 1", len(got))
	}
	if auth := got[0].Header.Get("Authorization"); auth != "Bearer workos:fresh" {
		t.Fatalf("Authorization = %q, want the refreshed token", auth)
	}
}

// TestStatusCallsALapsedTokenExpired guards the other half of the same bug.
// statusLocked recomputes the state rather than trusting the field, but it had
// no arm for a lapsed token, so an expired account kept the "ready" it was
// loaded with: the panel showed a healthy row beside a negative expires_in
// while every request 503'd.
func TestStatusCallsALapsedTokenExpired(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	c.pool.put(account{
		ID:           "lapsed",
		AccessToken:  "workos:stale",
		RefreshToken: "rt-1",
		AccountID:    "usr-1",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	})

	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Status.Accounts = %d rows, want 1", len(st.Accounts))
	}
	if got := st.Accounts[0].State; got != stateExpired {
		t.Fatalf("state = %q, want %q", got, stateExpired)
	}
	if st.Ready {
		t.Fatal("Status reports ready while its only token has lapsed")
	}

	// The panel reads a different projection, and it used to throw its own
	// availability check away and render the stale field, so it must be pinned
	// separately: fixing only Status would leave the panel lying.
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Accounts = %d rows, want 1", len(recs))
	}
	if got := recs[0].State; got != stateExpired {
		t.Fatalf("panel state = %q, want %q", got, stateExpired)
	}
}

// TestRefreshCandidatesIgnoresAvailability pins the rule that makes the fix
// work: the candidate list is built from expiry, never from availability.
func TestRefreshCandidatesIgnoresAvailability(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	now := time.Now()

	c.pool.put(account{ID: "lapsed", AccessToken: "workos:a", RefreshToken: "rt", ExpiresAt: now.Add(-time.Hour).Unix()})
	c.pool.put(account{ID: "fresh", AccessToken: "workos:b", RefreshToken: "rt", ExpiresAt: now.Add(time.Hour).Unix()})
	c.pool.put(account{ID: "no-refresh", AccessToken: "workos:c", ExpiresAt: now.Add(-time.Hour).Unix()})

	if got := c.pool.usable(); len(got) != 1 {
		t.Fatalf("usable() = %d entries, want 1 (only the unexpired account)", len(got))
	}
	got := c.pool.refreshCandidates(now, time.Minute)
	if len(got) != 1 {
		t.Fatalf("refreshCandidates() = %d entries, want 1", len(got))
	}
	if id := got[0].acct.id(); id != "lapsed" {
		t.Fatalf("refreshCandidates()[0] = %q, want the lapsed account", id)
	}
}

// TestAClientErrorNeverParksAHealthyAccount pins the fix for a defect a live
// run found: one chat naming a stale model id (a 404, which classify maps to
// kindClient) parked a perfectly good account, and the very next chat naming a
// VALID model was refused with 503 "no healthy account".  kindOfErr's own doc
// comment promises the opposite — "anything else is a client-side failure that
// must not park the account" — so cooldownFor must leave the entry alone.
func TestAClientErrorNeverParksAHealthyAccount(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	c.pool.put(account{
		ID:          "healthy",
		AccessToken: "workos:good",
		AccountID:   "usr-1",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a healthy account")
	}
	c.pool.markFailureWith(e, kindClient, "model not found", 0)

	if got := c.pool.usable(); len(got) != 1 {
		t.Fatalf("usable() = %d entries after a client error, want the account still selectable", len(got))
	}
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Status.Accounts = %d rows, want 1", len(st.Accounts))
	}
	if got := st.Accounts[0].State; got != stateReady {
		t.Fatalf("state = %q after a client error, want %q", got, stateReady)
	}
	if v, ok := st.Accounts[0].Extra["cooldown_until"]; ok && v != nil {
		t.Fatalf("cooldown_until = %v after a client error, want none", v)
	}
	if !st.Ready {
		t.Fatal("Status reports not ready after a client error on a healthy account")
	}
}

// TestAnAuthErrorStillParksTheAccount is the other half: removing kindClient
// from the park arm must not weaken the arm that IS the credential's fault.
func TestAnAuthErrorStillParksTheAccount(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	c.pool.put(account{
		ID:          "healthy",
		AccessToken: "workos:good",
		AccountID:   "usr-1",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})

	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a healthy account")
	}
	c.pool.markFailureWith(e, kindAuth, "401", 0)

	if got := c.pool.usable(); len(got) != 0 {
		t.Fatalf("usable() = %d entries after an auth error, want 0", len(got))
	}
	st := c.Status(context.Background())
	if got := st.Accounts[0].State; got != stateCooling {
		t.Fatalf("state = %q after an auth error, want %q", got, stateCooling)
	}
}

// TestARegionBlockOnOneModelDoesNotParkTheAccount is the other half of the same
// defect class, also found live: the vendor answers 403 both for a rejected
// token and for a single model it will not serve here.  classify must read the
// body, or a request for one unavailable model cools the only account and the
// next request for a perfectly good model is refused with 503.
func TestARegionBlockOnOneModelDoesNotParkTheAccount(t *testing.T) {
	const blocked = `{"error":"access forbidden: cline-free/muse-spark-1.3-contributor is not available in your region"}`

	if got := classify(http.StatusForbidden, blocked); got != kindClient {
		t.Fatalf("classify(403, region block) = %v, want %v", got, kindClient)
	}
	// The other half: a plain 403 is still the credential's fault, and a 401 is
	// never about the model.
	if got := classify(http.StatusForbidden, `{"error":"invalid token"}`); got != kindAuth {
		t.Fatalf("classify(403, invalid token) = %v, want %v", got, kindAuth)
	}
	if got := classify(http.StatusUnauthorized, blocked); got != kindAuth {
		t.Fatalf("classify(401, region-block body) = %v, want %v", got, kindAuth)
	}

	f := newFakeServer(t)
	c := newTestClient(t, f, "")
	c.pool.put(account{
		ID:          "healthy",
		AccessToken: "workos:good",
		AccountID:   "usr-1",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	})
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("pick() returned nil for a healthy account")
	}
	c.pool.markFailureWith(e, classify(http.StatusForbidden, blocked), "access forbidden", 0)

	if got := c.pool.usable(); len(got) != 1 {
		t.Fatalf("usable() = %d entries after a region block on one model, want the account still selectable", len(got))
	}
	if got := c.Status(context.Background()).Accounts[0].State; got != stateReady {
		t.Fatalf("state = %q after a region block on one model, want %q", got, stateReady)
	}
}

// providersFixture is Cline's own settings file, in the shape the vendor client
// actually writes it: the credential lives in providers.cline.settings.auth and
// carries the refresh token, the expiry in milliseconds, the account id and a
// metadata.userInfo block beside the access token.
//
// The JWT payload deliberately names a DIFFERENT clineUserId (usr-from-jwt)
// than the auth block (usr-real), because the account id is not the JWT claim
// and a test that conflates them would not notice.
const providersFixture = `{
  "version": 1,
  "lastUsedProvider": "cline",
  "modes": {},
  "providers": {
    "cline": {
      "settings": {
        "provider": "cline",
        "auth": {
          "accessToken": "workos:eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyXzAxU1VCIiwiY2xpbmVVc2VySWQiOiJ1c3ItZnJvbS1qd3QiLCJpc3MiOiJodHRwczovL2FwaS53b3Jrb3MuY29tLyJ9.c2ln",
          "refreshToken": "rt-from-file",
          "expiresAt": 1790862741000,
          "accountId": "usr-real",
          "metadata": {
            "provider": "cline",
            "tokenType": "Bearer",
            "userInfo": {
              "subject": "user_01SUB",
              "clineUserId": "usr-real",
              "email": "someone@example.com",
              "name": "Ada"
            }
          }
        }
      },
      "updatedAt": "2026-10-01T12:52:24.926Z",
      "tokenSource": "oauth"
    }
  }
}`

// TestImportKeepsTheRefreshTokenFromProvidersJSON is the regression for the
// worst of the three Cline bugs: Import read only the access token out of the
// settings file and dropped everything beside it, so the stored account had no
// refresh token and could never be renewed.  Cline's access tokens last about
// an hour; once one lapsed the account was stuck, every request answered "client
// is not configured (no account)", and the panel still called the row ready.
func TestImportKeepsTheRefreshTokenFromProvidersJSON(t *testing.T) {
	f := newFakeServer(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir := filepath.Join(home, ".cline", "data", "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, []byte(providersFixture), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	c := newTestClient(t, f, "")
	recs, err := c.Import(context.Background(), []string{path}, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("imported %d records, want 1", len(recs))
	}

	// The fixture's expiry is the real file's, which is already in the past, so
	// the account is deliberately NOT usable: what matters is that it is now
	// refreshable, which is the only thing that can bring it back.
	entries := c.pool.entries
	if len(entries) != 1 {
		t.Fatalf("pool holds %d accounts after import, want 1", len(entries))
	}
	a := entries[0].acct
	if a.RefreshToken != "rt-from-file" {
		t.Fatalf("refresh_token = %q, want rt-from-file: without it the account can never be renewed", a.RefreshToken)
	}
	if !a.refreshable() {
		t.Fatal("the imported account is not refreshable")
	}
	if a.AccountID != "usr-real" {
		t.Fatalf("account_id = %q, want usr-real (the auth block, not the JWT claim usr-from-jwt)", a.AccountID)
	}
	// expiresAt is milliseconds since the epoch; parseExpiry reads a value above
	// 1e12 as milliseconds and converts it to seconds.
	if a.ExpiresAt != 1790862741 {
		t.Fatalf("expires_at = %d, want 1790862741 (1790862741000 ms / 1000)", a.ExpiresAt)
	}
	if a.Email != "someone@example.com" {
		t.Fatalf("email = %q, want someone@example.com", a.Email)
	}
}

// TestCredentialFieldsFallsBackToTheTokenWalk keeps the loose path alive for a
// file that is not Cline's own: a hand-made credential file whose key is not
// accessToken still imports its token, and the account id is then read out of
// the JWT.
func TestCredentialFieldsFallsBackToTheTokenWalk(t *testing.T) {
	raw := []byte(`{"some":{"other":{"shape":{"token":"workos:eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ1c2VyXzAxU1VCIiwiY2xpbmVVc2VySWQiOiJ1c3ItZnJvbS1qd3QiLCJpc3MiOiJodHRwczovL2FwaS53b3Jrb3MuY29tLyJ9.c2ln"}}}}`)
	fields := credentialFields(raw)
	if fields["access_token"] == "" {
		t.Fatal("no access token was extracted from a nested hand-made file")
	}
	if fields["refresh_token"] != "" {
		t.Fatalf("refresh_token = %q, want empty: the file carries none", fields["refresh_token"])
	}
	if fields["account_id"] != "usr-from-jwt" {
		t.Fatalf("account_id = %q, want usr-from-jwt (read from the JWT claim)", fields["account_id"])
	}
}

// TestBuiltinCatalogueSurvivesACompleteOutage guards the other half of the
// rule: when no live source answers, the built-in table is the catalogue and
// may keep ids a later vendor change has not been observed to remove yet.
func TestBuiltinCatalogueSurvivesACompleteOutage(t *testing.T) {
	f := newFakeServer(t)
	f.json(modelsPath, http.StatusInternalServerError, `{}`)
	f.json(recommendedPath, http.StatusInternalServerError, `{}`)

	c := newTestClient(t, f, ` "access_token":"workos:t" `)
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("RefreshModels reported success with no catalogue source answering")
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	for _, want := range []string{
		"cline-free/deepseek-v4.1-flash",
		"cline-free/mimo-v2.6-flash",
		"cline-free/gemini-3.8-flash",
		"cline-free/muse-spark-1.3-contributor",
		"stealth/space-bunny-alpha",
	} {
		if !ids[want] {
			t.Errorf("offline catalogue is missing %q", want)
		}
	}
}
