package lobsterai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- fixtures ---------------------------------------------------------------

// captured is one request a fixture server saw.
type captured struct {
	method string
	path   string
	query  url.Values
	header http.Header
	body   []byte
}

// recorder keeps every request a fixture server saw, in order.
type recorder struct {
	mu   sync.Mutex
	reqs []captured
}

func (r *recorder) add(req *http.Request) captured {
	body, _ := io.ReadAll(req.Body)
	c := captured{
		method: req.Method,
		path:   req.URL.Path,
		query:  req.URL.Query(),
		header: req.Header.Clone(),
		body:   body,
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, c)
	r.mu.Unlock()
	return c
}

func (r *recorder) all() []captured {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]captured, len(r.reqs))
	copy(out, r.reqs)
	return out
}

type fixture struct {
	client *Client
	rec    *recorder
}

// newFixture builds a Client whose upstream is a test server.
func newFixture(t *testing.T, extra map[string]any, handler func(w http.ResponseWriter, r *http.Request)) *fixture {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := map[string]any{
		"base_url":    srv.URL,
		"portal_url":  srv.URL,
		"version_url": srv.URL + "/version",
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	got, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     raw,
		HTTPClient: srv.Client(),
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	return &fixture{client: c, rec: rec}
}

func (f *fixture) find(t *testing.T, path string) captured {
	t.Helper()
	for _, c := range f.rec.all() {
		if c.path == path {
			return c
		}
	}
	paths := make([]string, 0, len(f.rec.all()))
	for _, c := range f.rec.all() {
		paths = append(paths, c.method+" "+c.path)
	}
	t.Fatalf("no request to %s; saw %v", path, paths)
	return captured{}
}

// addAccount stores a token-only account, which is all any of these tests need.
func addAccount(t *testing.T, c *Client, token string) core.AccountRecord {
	t.Helper()
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"access_token": token,
		"uid":          "777",
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	return rec
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	return out
}

// --- sign-in ----------------------------------------------------------------

// parseLoginURL pulls the callback URL and the CSRF state out of the sign-in
// URL.  The vendor's URL is a hash route, so the query lives in the fragment --
// which is exactly the detail that makes this worth asserting.
func parseLoginURL(t *testing.T, raw string) (redirect, state string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse sign-in url %q: %v", raw, err)
	}
	if u.Fragment == "" {
		t.Fatalf("sign-in url has no fragment: %s", raw)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(u.Fragment, "/login?"))
	if err != nil {
		t.Fatalf("parse sign-in fragment %q: %v", u.Fragment, err)
	}
	redirect = q.Get("redirect_uri")
	state = q.Get("state")
	if redirect == "" || state == "" {
		t.Fatalf("sign-in url is missing redirect_uri or state: %s", raw)
	}
	if got := q.Get("source"); got != "electron" {
		t.Errorf("source = %q, want electron", got)
	}
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") {
		t.Errorf("redirect_uri = %q, want a loopback callback", redirect)
	}
	return redirect, state
}

func pollUntilTerminal(t *testing.T, c *Client, id string) core.LoginState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := c.PollLogin(context.Background(), id)
		if err != nil {
			t.Fatalf("PollLogin: %v", err)
		}
		if st.State != core.LoginPending {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("sign-in never reached a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLoginExchange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"data":{"value":{"version":"2026.9.4"}},"code":0,"msg":"OK"}`)
		case "/api/auth/exchange":
			io.WriteString(w, `{"code":0,"msg":"OK","data":{"accessToken":"access-1","refreshToken":"refresh-1","expiresIn":3600,"user":{"id":777,"nickname":"Lobster One"}}}`)
		default:
			http.NotFound(w, r)
		}
	})

	start, err := f.client.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if start.State != core.LoginPending {
		t.Fatalf("state = %q, want %q", start.State, core.LoginPending)
	}
	if !strings.Contains(start.URL, "/portal#/login?") {
		t.Errorf("sign-in url = %q, want the vendor's hash route", start.URL)
	}
	redirect, state := parseLoginURL(t, start.URL)

	// Stand in for the browser: the vendor would redirect here with the code.
	cb := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	resp, err := cb.Get(redirect + "?code=auth-code-1&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", resp.StatusCode)
	}

	final := pollUntilTerminal(t, f.client, start.SessionID)
	if final.State != core.LoginSuccess {
		t.Fatalf("final state = %q (%s)", final.State, final.Message)
	}
	if final.AccountID == "" {
		t.Error("a successful sign-in reported no account id")
	}

	req := f.find(t, "/api/auth/exchange")
	if got := req.header.Get("Authorization"); got != "" {
		t.Errorf("exchange sent Authorization %q; it must not carry one", got)
	}
	body := decodeBody(t, req.body)
	for _, key := range []string{"authCode", "firstKeyfrom", "latestKeyfrom", "uuid", "version"} {
		if s, _ := body[key].(string); strings.TrimSpace(s) == "" {
			t.Errorf("exchange body %s is missing or empty: %v", key, body)
		}
	}
	if body["authCode"] != "auth-code-1" {
		t.Errorf("authCode = %v, want auth-code-1", body["authCode"])
	}
	if body["firstKeyfrom"] != body["latestKeyfrom"] {
		t.Errorf("firstKeyfrom %v != latestKeyfrom %v at login; both are minted together",
			body["firstKeyfrom"], body["latestKeyfrom"])
	}
	if body["version"] != "2026.9.4" {
		t.Errorf("version = %v, want the version the manifest published", body["version"])
	}

	// The stored account must be able to renew: the uid comes from user.id and
	// the keyfrom values are persisted, while no token is ever handed to the
	// panel.
	recs, err := f.client.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("accounts = %d, want 1", len(recs))
	}
	if recs[0].Fields["uid"] != "777" {
		t.Errorf("uid = %v, want 777 (user.id)", recs[0].Fields["uid"])
	}
	if recs[0].Fields["has_refresh_token"] != true {
		t.Error("the stored account has no refresh token")
	}
	for key := range recs[0].Fields {
		if strings.Contains(key, "token") && key != "has_refresh_token" {
			t.Errorf("the panel view leaked a secret under %q", key)
		}
	}
	acct := f.client.pool.byID("lobsterai:777")
	if acct == nil {
		t.Fatal("the signed-in account is not in the pool")
	}
	if acct.FirstKeyfrom == "" || acct.LatestKeyfrom == "" || acct.UUID == "" {
		t.Errorf("the renewal fields were not persisted: %+v", acct)
	}
}

func TestLoginRejectsBadState(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	start, err := f.client.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	redirect, _ := parseLoginURL(t, start.URL)

	cb := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	resp, err := cb.Get(redirect + "?code=auth-code-1&state=wrong")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("callback status = %d, want 400 for a state mismatch", resp.StatusCode)
	}
	st := pollUntilTerminal(t, f.client, start.SessionID)
	if st.State != core.LoginFailed {
		t.Errorf("state = %q, want failed", st.State)
	}
	if !strings.Contains(st.Message, "state") {
		t.Errorf("message = %q, want it to mention the state", st.Message)
	}
}

// --- renewal ----------------------------------------------------------------

func TestRenewalReplaysKeyfromAndSendsNoAuthorization(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/refresh" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"data":{"accessToken":"access-2","refreshToken":"refresh-2","expiresIn":7200}}`)
	})

	_, err := f.client.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{
		"access_token":   "access-1",
		"refresh_token":  "refresh-1",
		"uid":            "777",
		"user_id":        "u-777",
		"uuid":           "uuid-1",
		"first_keyfrom":  "fk-1",
		"latest_keyfrom": "lk-1",
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	results, err := f.client.RefreshAccount(ctx, "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 || !results[0].OK {
		t.Fatalf("results = %+v, want one success", results)
	}

	req := f.find(t, "/api/auth/refresh")
	if got := req.header.Get("Authorization"); got != "" {
		t.Errorf("renewal sent Authorization %q; the endpoint takes none", got)
	}
	body := decodeBody(t, req.body)
	want := map[string]any{
		"firstKeyfrom":  "fk-1",
		"latestKeyfrom": "lk-1",
		"uuid":          "uuid-1",
		"userId":        "u-777",
		"refreshToken":  "refresh-1",
	}
	for key, wantVal := range want {
		if got := body[key]; got != wantVal {
			t.Errorf("renewal body %s = %v, want %v", key, got, wantVal)
		}
	}
	if s, _ := body["version"].(string); s == "" {
		t.Errorf("renewal body has no version: %v", body)
	}

	acct := f.client.pool.byID("lobsterai:777")
	if acct == nil {
		t.Fatal("the account vanished from the pool")
	}
	if acct.AccessToken != "access-2" {
		t.Errorf("access token = %q, want the renewed one", acct.AccessToken)
	}
	if acct.RefreshToken != "refresh-2" {
		t.Errorf("refresh token = %q, want the renewed one", acct.RefreshToken)
	}
	// The stored keyfrom values are replayed, never recomputed: they are what
	// the vendor issued, and inventing a new latest_keyfrom breaks renewal.
	if acct.LatestKeyfrom != "lk-1" {
		t.Errorf("latest_keyfrom = %q, want the stored lk-1", acct.LatestKeyfrom)
	}
	if acct.FirstKeyfrom != "fk-1" {
		t.Errorf("first_keyfrom = %q, want the stored fk-1", acct.FirstKeyfrom)
	}
}

func TestRenewalWithoutRefreshTokenIsReportedNotFatal(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	acct := addAccount(t, f.client, "access-1")
	results, err := f.client.RefreshAccount(context.Background(), acct.ID)
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].OK {
		t.Error("a renewal with no refresh token reported success")
	}
	if !strings.Contains(results[0].Error, "refresh token") {
		t.Errorf("error = %q, want it to name the missing refresh token", results[0].Error)
	}
}

// --- chat -------------------------------------------------------------------

// sseScript is the shape the upstream actually sends: bare SSE with no
// {code,msg,data} envelope, a space after one "data:" and none after the other,
// and a final frame that is not followed by a blank line.
const sseScript = "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"He\"}}]}\n" +
	"\n" +
	"data:{\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}\n" +
	"\n" +
	"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n" +
	"\n" +
	"data: [DONE]\n\n"

func sseHandler(script string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/proxy/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, script)
	}
}

func TestChatForcesStreamAndAggregatesForANonStreamingCaller(t *testing.T) {
	f := newFixture(t, nil, sseHandler(sseScript))
	addAccount(t, f.client, "access-1")

	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   false,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, usage, finish, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("drainSSE: %v", err)
	}
	// A caller that asked for a single answer must get one, even though the
	// upstream can only stream.
	if text != "Hello" {
		t.Errorf("text = %q, want Hello", text)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Errorf("usage = %+v, want total 5", usage)
	}

	req := f.find(t, "/api/proxy/v1/chat/completions")
	body := decodeBody(t, req.body)
	if body["stream"] != true {
		t.Errorf("stream = %v; the upstream answers 500 unless it is true", body["stream"])
	}
	if got := req.header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Accept = %q, want it to allow event-stream", got)
	}
	if got := req.header.Get("X-LobsterAI-Client-Capabilities"); got != defaultCapabilities {
		t.Errorf("X-LobsterAI-Client-Capabilities = %q", got)
	}
	if got := req.header.Get("X-LobsterAI-Client-Version"); got == "" {
		t.Error("X-LobsterAI-Client-Version is missing")
	}
	if got := req.header.Get("User-Agent"); got != defaultUserAgent {
		t.Errorf("User-Agent = %q", got)
	}
	for _, banned := range []string{"X-Domain", "X-Product", "X-Product-Code", "prompt_cache_key"} {
		if got := req.header.Get(banned); got != "" {
			t.Errorf("chat sent %s = %q, which the vendor rejects", banned, got)
		}
	}
}

// TestChatSSEBodyIsNotEnveloped pins the one endpoint that breaks the vendor's
// own convention: the chat response is bare SSE, so applying the envelope check
// to it would fail every request.
func TestChatSSEBodyIsNotEnveloped(t *testing.T) {
	f := newFixture(t, nil, sseHandler(sseScript))
	addAccount(t, f.client, "access-1")
	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, _, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("a body with no {code,msg,data} envelope was rejected: %v", err)
	}
	if text != "Hello" {
		t.Errorf("text = %q", text)
	}
}

func TestChatReasoningAndToolCallFragments(t *testing.T) {
	script := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"think "}}]}`,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"hard","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`data:{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	f := newFixture(t, nil, sseHandler(script))
	addAccount(t, f.client, "access-1")

	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, reasoning, calls, _, finish, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("drainSSE: %v", err)
	}
	if reasoning != "think hard" {
		t.Errorf("reasoning = %q, want %q", reasoning, "think hard")
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want one merged call", calls)
	}
	if calls[0].Name != "lookup" {
		t.Errorf("tool name = %q", calls[0].Name)
	}
	// The arguments arrive split across frames and must be concatenated, not
	// overwritten: keeping only the last fragment loses the whole call.
	if calls[0].Arguments != `{"q":"x"}` {
		t.Errorf("arguments = %q, want %q", calls[0].Arguments, `{"q":"x"}`)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

// TestChatMessageFallbackDoesNotDoubleCount pins the guard that keeps a whole
// message from being appended on top of the deltas that already carried it.
func TestChatMessageFallbackDoesNotDoubleCount(t *testing.T) {
	script := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"content":"A"}}]}`,
		`data: {"choices":[{"index":0,"message":{"role":"assistant","content":"AM"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	f := newFixture(t, nil, sseHandler(script))
	addAccount(t, f.client, "access-1")
	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, _, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("drainSSE: %v", err)
	}
	if text != "A" {
		t.Errorf("text = %q, want A; the whole-message fallback double-counted", text)
	}
}

func TestChatMessageFallbackStillWorks(t *testing.T) {
	script := strings.Join([]string{
		`data: {"choices":[{"index":0,"message":{"role":"assistant","content":"whole"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	f := newFixture(t, nil, sseHandler(script))
	addAccount(t, f.client, "access-1")
	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, _, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("drainSSE: %v", err)
	}
	if text != "whole" {
		t.Errorf("text = %q, want whole", text)
	}
}

func TestChatRejectsImageInput(t *testing.T) {
	f := newFixture(t, nil, sseHandler(sseScript))
	addAccount(t, f.client, "access-1")
	_, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model: "deepseek-v4-pro",
		Messages: []core.Message{{
			Role:  "user",
			Parts: []core.ContentPart{{Type: "image_url", ImageURL: "https://example.com/x.png"}},
		}},
	})
	if err == nil {
		t.Fatal("Chat accepted an image; the vendor has no image support")
	}
	if !strings.Contains(err.Error(), "text only") {
		t.Errorf("error = %q, want it to explain the text-only limit", err)
	}
}

func TestChatClassifiesAVendorRefusal(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"code":40001,"msg":"积分不足","data":null}`)
	})
	addAccount(t, f.client, "access-1")
	_, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat accepted a 402")
	}
	if kind := core.FailureKindOf(err); kind != core.FailureQuota {
		t.Errorf("kind = %q, want %q", kind, core.FailureQuota)
	}
}

// --- envelope ---------------------------------------------------------------

// TestEnvelopeCheckedOnAccountEndpoints pins the other half of the chat rule:
// every non-chat endpoint is enveloped, and a non-zero code is a failure even
// when the HTTP status is 200.
func TestEnvelopeCheckedOnAccountEndpoints(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":40100,"msg":"token rejected","data":{}}`)
	})
	addAccount(t, f.client, "access-1")
	_, err := f.client.fetchModels(context.Background())
	if err == nil {
		t.Fatal("fetchModels accepted a non-zero envelope code on an HTTP 200")
	}
	if !strings.Contains(err.Error(), "token rejected") {
		t.Errorf("error = %q, want it to carry the vendor message", err)
	}
}

func TestEnvelopeWithNoDataIsAnError(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"msg":"OK","data":null}`)
	})
	addAccount(t, f.client, "access-1")
	_, err := f.client.fetchModels(context.Background())
	if err == nil {
		t.Fatal("fetchModels accepted a success envelope with no data")
	}
	if !strings.Contains(err.Error(), "no data") {
		t.Errorf("error = %q, want it to say the response carried no data", err)
	}
}

// --- balance ----------------------------------------------------------------

func TestBalanceComesFromProfileSummary(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/profile-summary":
			io.WriteString(w, `{"code":0,"data":{"totalCreditsRemaining":5300,"creditItems":[{"type":"activity","creditsRemaining":5000,"expiresAt":"2026-12-31T00:00:00Z"},{"type":"free","creditsRemaining":300,"expiresAt":"2026-09-30T00:00:00Z"}]}}`)
		case "/api/user/quota":
			// The trap: this endpoint reports only freeCreditsTotal.
			io.WriteString(w, `{"code":0,"data":{"freeCreditsTotal":300}}`)
		default:
			http.NotFound(w, r)
		}
	})
	acct := addAccount(t, f.client, "access-1")

	bal, err := f.client.AccountBalance(ctx, acct.ID, 48*time.Hour)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 5300 {
		t.Errorf("credits = %d, want 5300 from data.totalCreditsRemaining", bal.Credits)
	}
	if bal.Unit != "积分" {
		t.Errorf("unit = %q", bal.Unit)
	}
	if bal.EarliestAt.IsZero() {
		t.Error("EarliestAt is zero; creditItems were not read")
	}
	if bal.EarliestRemaining != 300 {
		t.Errorf("earliest remaining = %d, want 300 (the free item expires first)", bal.EarliestRemaining)
	}
	for _, c := range f.rec.all() {
		if c.path == "/api/user/quota" {
			t.Fatal("balance called /api/user/quota, which omits the activity credits and under-reports")
		}
	}
	if f.find(t, "/api/user/profile-summary").path == "" {
		t.Error("balance never called profile-summary")
	}
}

func TestBalanceClampsNegativeToZero(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"totalCreditsRemaining":-12}}`)
	})
	acct := addAccount(t, f.client, "access-1")
	bal, err := f.client.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 0 {
		t.Errorf("credits = %d, want 0", bal.Credits)
	}
}

func TestBalanceMissingTotalIsAnError(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":{"freeCreditsTotal":300}}`)
	})
	acct := addAccount(t, f.client, "access-1")
	if _, err := f.client.AccountBalance(context.Background(), acct.ID, 0); err == nil {
		t.Fatal("AccountBalance invented a number from a response without totalCreditsRemaining")
	}
}

// --- check-in ---------------------------------------------------------------

func TestCheckinSequence(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/version":
			io.WriteString(w, `{"data":{"value":{"version":"2026.9.4"}},"code":0,"msg":"OK"}`)
		case "/api/client-activities/slot":
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act-1","configRevision":7}}}`)
		case "/api/client-activities/act-1/context":
			io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`)
		case "/api/client-activities/act-1/actions/check_in":
			io.WriteString(w, `{"code":0,"data":{"result":{"creditsGranted":100}}}`)
		default:
			http.NotFound(w, r)
		}
	})
	acct := addAccount(t, f.client, "access-1")

	actions := f.client.CheckinActions(ctx)
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want one", actions)
	}
	if actions[0].ID != checkinActionID {
		t.Errorf("action id = %q, want %q", actions[0].ID, checkinActionID)
	}

	res, err := f.client.Checkin(ctx, acct.ID, checkinActionID)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want success", res)
	}
	if got := res.Data["credits"]; got != 100 {
		t.Errorf("credits = %v, want 100", got)
	}

	// The three calls are ordered and each feeds the next.
	var seq []string
	for _, c := range f.rec.all() {
		if c.path == "/version" {
			continue
		}
		seq = append(seq, c.method+" "+c.path)
	}
	want := []string{
		"GET /api/client-activities/slot",
		"GET /api/client-activities/act-1/context",
		"POST /api/client-activities/act-1/actions/check_in",
	}
	if !reflect.DeepEqual(seq, want) {
		t.Errorf("sequence = %v, want %v", seq, want)
	}

	slot := f.find(t, "/api/client-activities/slot")
	for key, wantVal := range map[string]string{
		"placement":           checkinPlacement,
		"containerApiVersion": checkinContainerAPIVersion,
		"platform":            checkinPlatform,
		"clientVersion":       "2026.9.4",
	} {
		if got := slot.query.Get(key); got != wantVal {
			t.Errorf("slot %s = %q, want %q", key, got, wantVal)
		}
	}
	if got := slot.header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("slot Authorization = %q", got)
	}

	actx := f.find(t, "/api/client-activities/act-1/context")
	if got := actx.query.Get("configRevision"); got != "7" {
		t.Errorf("context configRevision = %q, want 7", got)
	}

	post := f.find(t, "/api/client-activities/act-1/actions/check_in")
	body := decodeBody(t, post.body)
	if got, ok := body["configRevision"].(float64); !ok || got != 7 {
		t.Errorf("check-in configRevision = %v, want 7", body["configRevision"])
	}
	if s, _ := body["idempotencyKey"].(string); s == "" {
		t.Errorf("check-in body has no idempotencyKey: %v", body)
	}
	if _, ok := body["payload"]; !ok {
		t.Errorf("check-in body has no payload: %v", body)
	}
	if got := post.header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("check-in Authorization = %q", got)
	}
	if got := post.header.Get("X-LobsterAI-Client-Version"); got == "" {
		t.Error("check-in did not send the client version header")
	}
}

func TestCheckinAlreadyClaimedIsAResultNotAnError(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/client-activities/slot":
			io.WriteString(w, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"act-1","configRevision":7}}}`)
		case "/api/client-activities/act-1/context":
			io.WriteString(w, `{"code":0,"data":{"state":{"claimedToday":true},"actions":["check_in"]}}`)
		default:
			http.NotFound(w, r)
		}
	})
	acct := addAccount(t, f.client, "access-1")
	res, err := f.client.Checkin(context.Background(), acct.ID, checkinActionID)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Errorf("an already-claimed day is not a failure: %+v", res)
	}
	if res.Data["already_done"] != true {
		t.Errorf("data = %v, want already_done", res.Data)
	}
	for _, c := range f.rec.all() {
		if c.path == "/api/client-activities/act-1/actions/check_in" {
			t.Error("a claim was posted for a day that was already claimed")
		}
	}
}

func TestCheckinRefusalIsAResultNotAnError(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/client-activities/slot":
			io.WriteString(w, `{"code":0,"data":{"slotState":"taken","activity":null}}`)
		default:
			http.NotFound(w, r)
		}
	})
	acct := addAccount(t, f.client, "access-1")
	res, err := f.client.Checkin(context.Background(), acct.ID, checkinActionID)
	if err != nil {
		t.Fatalf("Checkin returned a Go error for a vendor refusal: %v", err)
	}
	if res.OK {
		t.Error("a refusal was reported as success")
	}
	if res.Error == "" {
		t.Error("a refusal carried no explanation")
	}
}

func TestCheckinUnknownAccountIsAnError(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, err := f.client.Checkin(context.Background(), "nope", checkinActionID); err == nil {
		t.Fatal("Checkin accepted an unknown account")
	}
}

// --- version ----------------------------------------------------------------

func TestVersionFetchReadsDataValueVersion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"value":{"version":"2026.9.4","date":"2026-09-04","windowsX64":{"url":"https://example.com/a.exe"}}},"code":0,"msg":"OK"}`)
	})
	f.client.refreshVersionIfStale(ctx)
	v, stale := f.client.versionState()
	if v != "2026.9.4" {
		t.Fatalf("version = %q, want 2026.9.4 from data.value.version", v)
	}
	if stale {
		t.Error("the version is still stale after a successful fetch")
	}
	if got := f.client.clientVersion(ctx); got != "2026.9.4" {
		t.Errorf("clientVersion = %q", got)
	}
	req := f.find(t, "/version")
	if got := req.header.Get("Authorization"); got != "" {
		t.Errorf("the version manifest was fetched with Authorization %q", got)
	}
}

func TestVersionFetchRejectsANonZeroCode(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"value":{"version":"2026.9.4"}},"code":500,"msg":"boom"}`)
	})
	f.client.refreshVersionIfStale(context.Background())
	if v, _ := f.client.versionState(); v != "" {
		t.Errorf("version = %q, want it rejected when code != 0", v)
	}
}

func TestVersionFetchRejectsNonVersionStrings(t *testing.T) {
	for _, bad := range []string{"latest", "v0.1.0", "", "2026.9.4 extra"} {
		t.Run(bad, func(t *testing.T) {
			f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"data":{"value":{"version":"`+bad+`"}},"code":0,"msg":"OK"}`)
			})
			f.client.refreshVersionIfStale(context.Background())
			if v, _ := f.client.versionState(); v != "" {
				t.Errorf("version = %q, want %q rejected", v, bad)
			}
			if got := f.client.clientVersion(context.Background()); got != defaultClientVersion {
				t.Errorf("fallback = %q, want %q", got, defaultClientVersion)
			}
		})
	}
}

// --- models -----------------------------------------------------------------

func TestModelsFallBackToTheBuiltInCatalogue(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	models, err := f.client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != len(staticModelIDs) {
		t.Errorf("models = %d, want the %d built-in ids", len(models), len(staticModelIDs))
	}
	for _, m := range models {
		if m.OwnedBy != clientName {
			t.Errorf("model %s owned_by = %q", m.ID, m.OwnedBy)
		}
		// The built-in table carries no vendor data at all, so it must keep the
		// bridge-layer estimate rather than invent a window.
		if got, _ := m.Extra["context_length"].(int64); got != staticContextLength {
			t.Errorf("model %s context_length = %v, want the fallback %d", m.ID, m.Extra["context_length"], staticContextLength)
		}
		if _, named := m.Extra["display_name"]; named {
			t.Errorf("model %s was given a display_name by the fallback table", m.ID)
		}
	}
}

func TestModelsComeFromTheUpstream(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/available" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"data":[{"modelId":"only-model","modelName":"Only","provider":"vendor-x","apiFormat":"openai"}]}`)
	})
	addAccount(t, f.client, "access-1")
	models, err := f.client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "only-model" {
		t.Fatalf("models = %+v, want only-model", models)
	}
	if models[0].OwnedBy != "vendor-x" {
		t.Errorf("owned_by = %q, want the published provider", models[0].OwnedBy)
	}
	req := f.find(t, "/api/models/available")
	if got := req.header.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("Authorization = %q", got)
	}
	// The keyfrom query carries no refresh token.
	if got := req.query.Get("refreshToken"); got != "" {
		t.Errorf("the model list was asked with refreshToken = %q", got)
	}
}

// TestModelLimitsProviderIsNotImplemented records the honest answer: the vendor
// publishes no output budget, and the one way to learn it would be to fetch the
// list, which ModelLimitsProvider must never do.
func TestModelLimitsProviderIsNotImplemented(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, ok := core.AsModelLimits(f.client); ok {
		t.Fatal("lobsterai implements core.ModelLimitsProvider but the vendor publishes no output budget")
	}
	for _, m := range f.client.statusModels() {
		if _, ok := core.ModelOutputLimit(m); ok {
			t.Errorf("model %s carries a max_output_tokens the vendor never published", m.ID)
		}
	}
}

// TestModelsReadTheVendorsPublishedContextWindow pins the live shape: the
// vendor DOES publish a per-model contextWindow, and the module used to throw it
// away and report its 131072 bridge estimate for every model -- an 8x
// under-report on the 1000000-window models.  Rows that send null must keep the
// estimate, and the vendor's modelName must reach display_name.
func TestModelsReadTheVendorsPublishedContextWindow(t *testing.T) {
	const body = `{"code":0,"message":"success","data":[
		{"modelId":"deepseek-flash","modelName":"DeepSeek-V4.1-Flash","provider":"LobsterAI","apiFormat":"openai","contextWindow":1000000},
		{"modelId":"kimi-k2.8-preview","modelName":"Kimi-K2.8-Preview","provider":"LobsterAI","apiFormat":"openai","contextWindow":262144},
		{"modelId":"glm-5","modelName":"GLM-5","provider":"LobsterAI","apiFormat":"openai","contextWindow":null},
		{"modelId":"no-window-key","modelName":"Bare","provider":"LobsterAI","apiFormat":"openai"},
		{"modelId":"stringly-typed","modelName":"Stringy","provider":"LobsterAI","apiFormat":"openai","contextWindow":"256000"}
	]}`
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/models/available" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, body)
	})
	addAccount(t, f.client, "access-1")

	models, err := f.client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	got := map[string]core.Model{}
	for _, m := range models {
		got[m.ID] = m
	}
	if len(got) != 5 {
		t.Fatalf("models = %v, want the five live rows", got)
	}
	for id, want := range map[string]int64{
		"deepseek-flash":    1000000,
		"kimi-k2.8-preview": 262144,
		"glm-5":             staticContextLength,
		"no-window-key":     staticContextLength,
		"stringly-typed":    256000,
	} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("%s is missing from the catalogue", id)
		}
		if n, _ := m.Extra["context_length"].(int64); n != want {
			t.Errorf("%s context_length = %v, want %d", id, m.Extra["context_length"], want)
		}
	}
	if name, _ := got["deepseek-flash"].Extra["display_name"].(string); name != "DeepSeek-V4.1-Flash" {
		t.Errorf("display_name = %v, want the vendor's modelName", got["deepseek-flash"].Extra["display_name"])
	}
	// Still no output budget: the vendor publishes none, so the module must not
	// invent one even now that it reads contextWindow.
	for id, m := range got {
		if _, ok := core.ModelOutputLimit(m); ok {
			t.Errorf("%s carries a max_output_tokens the vendor never published", id)
		}
	}
	if _, ok := core.AsModelLimits(f.client); ok {
		t.Error("lobsterai began implementing core.ModelLimitsProvider")
	}
}

// --- config -----------------------------------------------------------------

func TestConfigDefaultsAndGuards(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	cfg = cfg.normalize()
	if cfg.BaseURL != defaultBaseURL {
		t.Errorf("base url = %q", cfg.BaseURL)
	}
	if cfg.PortalURL != defaultPortalURL {
		t.Errorf("portal url = %q", cfg.PortalURL)
	}
	if cfg.ClientVersion != defaultClientVersion {
		t.Errorf("client version = %q", cfg.ClientVersion)
	}
	if cfg.callbackPath() != defaultCallbackPath {
		t.Errorf("callback path = %q", cfg.callbackPath())
	}
	if cfg.chatTimeout() != defaultChatTimeout {
		t.Errorf("chat timeout = %v", cfg.chatTimeout())
	}
	if cfg.maxInFlight() != defaultMaxInFlight {
		t.Errorf("max in flight = %d", cfg.maxInFlight())
	}

	bogus, err := parseConfig(json.RawMessage(`{"client_version":"latest"}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if got := bogus.normalize().ClientVersion; got != defaultClientVersion {
		t.Errorf("client version = %q, want the fallback for a non-version string", got)
	}

	headers, err := parseConfig(json.RawMessage(`{"extra_headers":{"Authorization":"Bearer nope","x-custom":"1"}}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	nh := headers.normalize()
	if _, ok := nh.ExtraHeaders["Authorization"]; ok {
		t.Error("a reserved header survived normalization")
	}
	if nh.ExtraHeaders["x-custom"] != "1" {
		t.Error("a harmless custom header was dropped")
	}
}

func TestConfigAccountsAreLoaded(t *testing.T) {
	f := newFixture(t, map[string]any{
		"accounts": []map[string]any{{
			"label":          "from config",
			"uid":            "cfg-1",
			"access_token":   "access-cfg",
			"refresh_token":  "refresh-cfg",
			"uuid":           "uuid-cfg",
			"first_keyfrom":  "fk-cfg",
			"latest_keyfrom": "lk-cfg",
		}},
	}, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	recs, err := f.client.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("accounts = %+v, want the one the config supplied", recs)
	}
	if recs[0].ID != "lobsterai:cfg-1" {
		t.Errorf("id = %q", recs[0].ID)
	}
	if recs[0].Label != "from config" {
		t.Errorf("label = %q", recs[0].Label)
	}
	if acct := f.client.pool.byID("lobsterai:cfg-1"); acct == nil || acct.LatestKeyfrom != "lk-cfg" {
		t.Errorf("the config account's renewal fields were lost: %+v", acct)
	}
}

func TestStatusWithoutAccounts(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	st := f.client.Status(context.Background())
	if st.Name != clientName {
		t.Errorf("name = %q", st.Name)
	}
	if st.Ready {
		t.Error("a module with no account reported itself ready")
	}
	if st.Detail == "" {
		t.Error("status has no detail explaining what to do")
	}
	if len(st.Models) == 0 {
		t.Error("status listed no models")
	}
	for _, c := range f.rec.all() {
		t.Errorf("Status made a network call to %s", c.path)
	}
}

// --- the response body must outlive the request helper ----------------------

// TestDoKeepsTheBodyReadableAfterItReturns pins the fix for the defect that made
// every live LobsterAI chat fail with `context canceled`.
//
// `do` used to `defer cancel()` its per-call timeout, so the request context
// died the instant `do` returned -- while the CALLER still owned resp.Body.  A
// small JSON body usually won that race, which is why every fixture test passed;
// the SSE chat stream, read minutes later, never could.
//
// Revert experiment: put the `defer cancel()` back in `do` and the read below
// fails with `context canceled`.
func TestDoKeepsTheBodyReadableAfterItReturns(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: second\n\n")
	})

	resp, err := f.client.do(context.Background(), requestSpec{
		method:  http.MethodGet,
		url:     f.client.cfg.chatURL(),
		timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	// The read happens strictly after `do` returned: exactly the window the
	// deferred cancel used to slam shut.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body after do returned: %v", err)
	}
	if got, want := string(raw), "data: first\n\ndata: second\n\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestDoStillArmsTheTimeoutWhileTheBodyIsRead is the other half of the contract:
// tying the cancel to the body must not quietly disarm the per-call timeout.
func TestDoStillArmsTheTimeoutWhileTheBodyIsRead(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(2 * time.Second)
	})

	resp, err := f.client.do(context.Background(), requestSpec{
		method:  http.MethodGet,
		url:     f.client.cfg.chatURL(),
		timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("reading a body that outlives the per-call timeout succeeded; the timeout is no longer armed")
	}
}

// TestDoTiesTheCancelToTheBody pins the mechanism, not just the symptom: the
// timeout's cancel must run on Close, exactly once, and a request with no
// timeout must not be wrapped at all.
func TestDoTiesTheCancelToTheBody(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	withTimeout, err := f.client.do(context.Background(), requestSpec{
		method:  http.MethodGet,
		url:     f.client.cfg.chatURL(),
		timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if _, ok := withTimeout.Body.(*cancelBody); !ok {
		t.Fatalf("body is %T, want *cancelBody: the per-call timeout is not tied to the body", withTimeout.Body)
	}
	if err := withTimeout.Body.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := withTimeout.Body.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	noTimeout, err := f.client.do(context.Background(), requestSpec{
		method: http.MethodGet,
		url:    f.client.cfg.chatURL(),
	})
	if err != nil {
		t.Fatalf("do without a timeout: %v", err)
	}
	defer noTimeout.Body.Close()
	if _, ok := noTimeout.Body.(*cancelBody); ok {
		t.Fatal("a request with no timeout was wrapped in a cancelBody; there is no cancel to run")
	}
}

// TestChatSurvivesASlowFirstChunk is the end-to-end half of the same fix: the
// vendor's chat stream is read long after the request helper has returned, so a
// cancel fired at return would kill every real conversation.
func TestChatSurvivesASlowFirstChunk(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"Hello"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	addAccount(t, f.client, "access-1")

	stream, err := f.client.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, finish, err := drainSSE(stream)
	if err != nil {
		t.Fatalf("drainSSE: %v", err)
	}
	if text != "Hello" {
		t.Errorf("text = %q, want Hello", text)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
}
