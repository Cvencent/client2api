package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newTestClient(t *testing.T, rawCfg string) *Client {
	t.Helper()
	c, err := New(core.Deps{DataDir: t.TempDir(), Config: json.RawMessage(rawCfg), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.(*Client)
}

func collect(t *testing.T, s core.Stream) []core.Event {
	t.Helper()
	var out []core.Event
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ev)
	}
	return out
}

// ---------------------------------------------------------------------------
// TestGeminiOAuthClientDefaultAndPrecedence pins the sign-in credentials: the
// module ships the Cloud Code IDE's own installed-app client, so the panel's
// "get a sign-in link" works on a stock install, while the operator's config
// and environment still override either half.
func TestGeminiOAuthClientDefaultAndPrecedence(t *testing.T) {
	t.Setenv(envClientID, "")
	t.Setenv(envClientSecret, "")

	cfg := Config{}.normalize()
	if got := cfg.clientID(); got != embeddedClientID {
		t.Fatalf("clientID() = %q, want the embedded client %q", got, embeddedClientID)
	}
	if got := cfg.clientSecret(); got != embeddedClientSecret {
		t.Fatal("clientSecret() did not fall back to the embedded secret")
	}
	if err := cfg.requireOAuthClient(); err != nil {
		t.Fatalf("embedded OAuth client was rejected: %v", err)
	}

	t.Setenv(envClientID, "env-client-id")
	t.Setenv(envClientSecret, "env-client-secret")
	if cfg.clientID() != "env-client-id" || cfg.clientSecret() != "env-client-secret" {
		t.Fatal("environment did not override the embedded OAuth client")
	}

	cfg.ClientID, cfg.ClientSecret = "config-id", "config-secret"
	if cfg.clientID() != cfg.ClientID || cfg.clientSecret() != cfg.ClientSecret {
		t.Fatal("config OAuth client did not win over environment")
	}
}

// TestStartLoginWorksWithoutConfiguredOAuthClient is the regression for the
// refusal the panel showed on a stock install: "get a sign-in link" answered
// "client is not configured (no account)" before the listener ever opened.
func TestStartLoginWorksWithoutConfiguredOAuthClient(t *testing.T) {
	t.Setenv(envClientID, "")
	t.Setenv(envClientSecret, "")
	c := newTestClient(t, "")

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer func() { _ = c.CancelLogin(context.Background(), st.SessionID) }()
	if st.URL == "" {
		t.Fatal("StartLogin returned no URL")
	}
	if !strings.Contains(st.URL, embeddedClientID) {
		t.Fatalf("sign-in URL does not carry the embedded client: %s", st.URL)
	}
	if !strings.Contains(st.URL, "redirect_uri=http%3A%2F%2Flocalhost%3A") {
		t.Fatalf("sign-in URL has no loopback redirect: %s", st.URL)
	}
}

// envelope / serialisation
// ---------------------------------------------------------------------------

func TestEnvelopeIsAlphabeticalAndCarriesTierSuffix(t *testing.T) {
	req := &core.ChatRequest{
		Model:    upstreamFlash,
		Messages: []core.Message{{Role: "user", Content: "hello"}},
		Options:  map[string]any{"reasoning_effort": "high"},
	}
	env, err := buildEnvelope(defaultProject, req.Model, req, nil, "infer", 0)
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	if env["model"] != "gemini-3.8-flash-high" {
		t.Fatalf("model = %v, want gemini-3.8-flash-high", env["model"])
	}
	raw, err := marshalRequest(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Top-level keys must appear in alphabetical order in the bytes.
	keys := sortKeys(env)
	for i := 1; i < len(keys); i++ {
		prev := strings.Index(string(raw), `"`+keys[i-1]+`"`)
		cur := strings.Index(string(raw), `"`+keys[i]+`"`)
		if prev < 0 || cur < 0 || prev > cur {
			t.Fatalf("keys not alphabetical: %v in %s", keys, raw)
		}
	}
}

func TestEnvelopeIncludesThoughtsAlwaysAndOmitsBudgetWhenTiered(t *testing.T) {
	req := &core.ChatRequest{Model: upstreamFlash, Messages: []core.Message{{Role: "user", Content: "hi"}},
		Options: map[string]any{"reasoning_effort": "tiered"}}
	env, err := buildEnvelope(defaultProject, req.Model, req, nil, "infer", 0)
	if err != nil {
		t.Fatalf("buildEnvelope: %v", err)
	}
	request := env["request"].(map[string]any)
	gc := request["generationConfig"].(map[string]any)
	thinking := gc["thinkingConfig"].(map[string]any)
	if thinking["includeThoughts"] != true {
		t.Errorf("includeThoughts = %v, want true", thinking["includeThoughts"])
	}
	if _, ok := thinking["thinkingBudget"]; ok {
		t.Errorf("tiered must omit thinkingBudget, got %v", thinking)
	}
	if env["model"] != "gemini-3.8-flash-tiered" {
		t.Errorf("model = %v", env["model"])
	}
}

func TestEnvelopeBudgetForEachTier(t *testing.T) {
	for tier, want := range map[string]int{"low": 1000, "medium": 4000, "high": 10000} {
		req := &core.ChatRequest{Model: upstreamFlash, Messages: []core.Message{{Role: "user", Content: "hi"}},
			Options: map[string]any{"reasoning_effort": tier}}
		env, _ := buildEnvelope(defaultProject, req.Model, req, nil, "infer", 0)
		gc := env["request"].(map[string]any)["generationConfig"].(map[string]any)
		got := gc["thinkingConfig"].(map[string]any)["thinkingBudget"]
		if got != want {
			t.Errorf("tier %s budget = %v, want %d", tier, got, want)
		}
	}
}

func TestRequestIDHasAgentShape(t *testing.T) {
	id := newRequestID()
	if !strings.HasPrefix(id, "agent/") || strings.Count(id, "/") != 2 {
		t.Fatalf("requestId = %q", id)
	}
}

// ---------------------------------------------------------------------------
// identity headers
// ---------------------------------------------------------------------------

func TestIdentityHeadersVerbatimAndNoAPIKey(t *testing.T) {
	h := identityHeadersFor("tok", false)
	want := map[string]string{
		"User-Agent":         "antigravity/4.3.0 (cmdc-pak)",
		"x-client-name":      "antigravity",
		"x-client-version":   "4.3.0",
		"x-machine-id":       "cmdc-pak",
		"x-vscode-sessionid": "proxy",
	}
	for k, v := range want {
		if h[k] != v {
			t.Errorf("header %s = %q, want %q", k, h[k], v)
		}
	}
	if _, ok := h["x-goog-api-key"]; ok {
		t.Error("x-goog-api-key must not be sent")
	}
	if _, ok := h["x-goog-api-client"]; ok {
		t.Error("x-goog-api-client must not be sent")
	}
	if _, ok := h["Accept"]; ok {
		t.Error("streaming requests must not carry Accept")
	}
	if h["Authorization"] != "Bearer tok" {
		t.Errorf("Authorization = %q", h["Authorization"])
	}
}

func TestExtraHeadersCannotOverrideIdentityOrAuth(t *testing.T) {
	dst := identityHeadersFor("tok", false)
	applyExtraHeaders(dst, map[string]string{
		"Authorization": "Bearer evil",
		"x-client-name": "evil",
		"X-Custom":      "ok",
	})
	if dst["Authorization"] != "Bearer tok" {
		t.Errorf("Authorization was overridden: %q", dst["Authorization"])
	}
	if dst["x-client-name"] != "antigravity" {
		t.Errorf("identity header was overridden: %q", dst["x-client-name"])
	}
	if dst["X-Custom"] != "ok" {
		t.Errorf("X-Custom extra header was dropped")
	}
}

// ---------------------------------------------------------------------------
// contents translation
// ---------------------------------------------------------------------------

func TestContentsRolesAndToolResponseByFunctionName(t *testing.T) {
	req := &core.ChatRequest{Model: upstreamFlash, Messages: []core.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_1", Name: "exec", Arguments: `{"cmd":"ls"}`}}},
		{Role: "tool", ToolCallID: "call_1", Content: "a.go"},
	}}
	contents, system, err := translateContents(req, nil)
	if err != nil {
		t.Fatalf("translateContents: %v", err)
	}
	if system != "sys" {
		t.Errorf("system = %q", system)
	}
	if contents[0].Role != "user" {
		t.Errorf("first role = %q, want user", contents[0].Role)
	}
	last := contents[len(contents)-1]
	if last.Role != "user" {
		t.Errorf("tool turn role = %q, want user", last.Role)
	}
	if last.Parts[0].FunctionResponse == nil || last.Parts[0].FunctionResponse.Name != "exec" {
		t.Fatalf("functionResponse = %+v, want name exec", last.Parts[0].FunctionResponse)
	}
}

func TestAssistantRoleBecomesModel(t *testing.T) {
	req := &core.ChatRequest{Model: upstreamFlash, Messages: []core.Message{
		{Role: "assistant", Content: "ok"},
	}}
	contents, _, _ := translateContents(req, nil)
	if len(contents) != 1 || contents[0].Role != "model" {
		t.Fatalf("contents = %+v, want one model turn", contents)
	}
}

func TestSchemaSanitisationDropsUnknownKeys(t *testing.T) {
	in := map[string]any{
		"$schema":              "http://json-schema.org/draft-07/schema#",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"cmd": map[string]any{"type": []any{"string", "null"}, "default": ""}},
	}
	out := sanitizeSchema(in)
	if _, ok := out["$schema"]; ok {
		t.Error("$schema must be dropped")
	}
	if _, ok := out["additionalProperties"]; ok {
		t.Error("additionalProperties must be dropped")
	}
	cmd := out["properties"].(map[string]any)["cmd"].(map[string]any)
	if cmd["type"] != "string" || cmd["nullable"] != true {
		t.Errorf("union type not collapsed: %+v", cmd)
	}
	if _, ok := cmd["default"]; ok {
		t.Error("default must be dropped")
	}
}

// ---------------------------------------------------------------------------
// session derivation
// ---------------------------------------------------------------------------

func TestSessionIDDependsOnProjectTextAndLane(t *testing.T) {
	a := deriveSessionID("p1", "hello", "infer", 0)
	if a != deriveSessionID("p1", "hello", "infer", 0) {
		t.Error("same input must produce the same session id")
	}
	if a == deriveSessionID("p2", "hello", "infer", 0) {
		t.Error("a different project must change the session id")
	}
	if a == deriveSessionID("p1", "other", "infer", 0) {
		t.Error("a different first user text must change the session id")
	}
	if a == deriveSessionID("p1", "hello", "smoke", 0) {
		t.Error("a different lane must change the session id")
	}
	if a == deriveSessionID("p1", "hello", "infer", 1) {
		t.Error("a bumped generation must change the session id")
	}
}

// ---------------------------------------------------------------------------
// model admission
// ---------------------------------------------------------------------------

func TestChatRejectsUnknownModel(t *testing.T) {
	c := newTestClient(t, `{}`)
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "gemini-9.9-fake", Messages: []core.Message{{Role: "user", Content: "x"}}}); err == nil {
		t.Fatal("an unknown model was accepted")
	}
}

func TestCanonicalModelIDStripsTier(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"gemini-3.8-flash", "gemini-3.8-flash"},
		{"gemini-3.8-flash-high", "gemini-3.8-flash"},
		{"gemini-3.8-flash-tiered", "gemini-3.8-flash"},
	} {
		if got := canonicalModelID(tc.in); got != tc.want {
			t.Errorf("canonicalModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCatalogueAdvertisesEffortsAndNoLite(t *testing.T) {
	c := newTestClient(t, `{}`)
	models, _ := c.Models(context.Background())
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	if models[0].ID != upstreamFlash {
		t.Fatalf("model id = %q", models[0].ID)
	}
	efforts, _ := models[0].Extra["supported_efforts"].([]string)
	if len(efforts) != 4 {
		t.Fatalf("efforts = %v", efforts)
	}
}

// ---------------------------------------------------------------------------
// SSE consumption
// ---------------------------------------------------------------------------

func sseStream(t *testing.T, frames ...string) *chatStream {
	t.Helper()
	body := strings.Join(frames, "\n\n") + "\n\n"
	return newSSEStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)), newSigStore(""))
}

func TestSSEConsumesTextReasoningAndUsage(t *testing.T) {
	s := sseStream(t,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"think","thought":true}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hello "}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"world"}]}}],"usageMetadata":{"promptTokenCount":12,"cachedContentTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":15}}}`,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}]}}`,
		`data: [DONE]`,
	)
	events := collect(t, s)
	var reasoning, text strings.Builder
	var usage *core.Usage
	finish := ""
	for _, ev := range events {
		switch ev.Type {
		case core.EventDelta:
			reasoning.WriteString(ev.Reasoning)
			text.WriteString(ev.Delta)
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			finish = ev.Finish
		}
	}
	if reasoning.String() != "think" {
		t.Errorf("reasoning = %q", reasoning.String())
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q", text.String())
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 3 || usage.TotalTokens != 15 || usage.CachedTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}
}

func TestSSEToolCallEmitsOneDelta(t *testing.T) {
	s := sseStream(t,
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"exec","args":{"cmd":"ls"}}}]}}]}}`,
		`data: {"response":{"candidates":[{"finishReason":"STOP"}]}}`,
		`data: [DONE]`,
	)
	events := collect(t, s)
	var tool *core.Event
	finish := ""
	for i := range events {
		if events[i].Type == core.EventToolCall {
			tool = &events[i]
		}
		if events[i].Type == core.EventDone {
			finish = events[i].Finish
		}
	}
	if tool == nil || tool.ToolCall == nil {
		t.Fatal("no tool call event")
	}
	if tool.ToolCall.Name != "exec" || tool.ToolCall.Arguments != `{"cmd":"ls"}` {
		t.Errorf("tool call = %+v", tool.ToolCall)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q, want tool_calls", finish)
	}
}

func TestSSEEmptyTurnIsAnError(t *testing.T) {
	s := sseStream(t, `data: {"response":{"candidates":[{"finishReason":"STOP"}]}}`, `data: [DONE]`)
	events := collect(t, s)
	found := false
	for _, ev := range events {
		if ev.Type == core.EventError {
			found = true
		}
	}
	if !found {
		t.Fatalf("an empty turn was not reported as an error: %+v", events)
	}
}

func TestSSEXplicitUnparseableFrameIsSkipped(t *testing.T) {
	s := sseStream(t,
		`data: {not json`,
		`: heartbeat`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}}`,
		`data: [DONE]`,
	)
	events := collect(t, s)
	text := ""
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
	}
	if text != "ok" {
		t.Fatalf("text = %q, want ok", text)
	}
}

func TestBufferedBodyParsesSingleResponse(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1,"totalTokenCount":6}}`
	s, _, err := newBufferedStream(context.Background(), func() {}, io.NopCloser(strings.NewReader(body)), nil)
	if err != nil {
		t.Fatalf("newBufferedStream: %v", err)
	}
	events := collect(t, s)
	text := ""
	var usage *core.Usage
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
		if ev.Type == core.EventUsage {
			usage = ev.Usage
		}
	}
	if text != "answer" {
		t.Errorf("text = %q", text)
	}
	if usage == nil || usage.PromptTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
}

// ---------------------------------------------------------------------------
// signature store
// ---------------------------------------------------------------------------

func TestSigStoreRoundTripAndToolFallback(t *testing.T) {
	dir := t.TempDir()
	path := sigStorePath(dir)
	s := newSigStore(path)
	s.Put("exec", `{"a":1}`, "  SIG123  ")
	if got := s.Get("exec", `{"a":1}`); got != "SIG123" {
		t.Errorf("exact lookup = %q", got)
	}
	// A drifted argument set still finds the latest signature for the tool.
	if got := s.Get("exec", `{"a":2}`); got != "SIG123" {
		t.Errorf("tool fallback = %q", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	reloaded := newSigStore(path)
	if got := reloaded.Get("exec", `{"a":1}`); got != "SIG123" {
		t.Errorf("reloaded lookup = %q", got)
	}
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

func TestSignatureAndSessionOverflowDetectors(t *testing.T) {
	if !isSignatureError("Invalid signature") {
		t.Error("a signature error was not detected")
	}
	if !isSessionOverflow("The input token count exceeds the maximum number of tokens allowed 1048576") {
		t.Error("a session overflow was not detected")
	}
	if isSessionOverflow("some other 400") {
		t.Error("a plain 400 was mistaken for a session overflow")
	}
}

// ---------------------------------------------------------------------------
// upstream round trip (httptest)
// ---------------------------------------------------------------------------

func TestChatStreamsThroughFakeUpstream(t *testing.T) {
	var gotBody string
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}`+"\n\n")
		_, _ = io.WriteString(w, `data: [DONE]`+"\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, `{"accounts":[{"access_token":"tok","refresh_token":"r","expiry":"`+
		timeNowPlusHour()+`"}]}`)
	c.pool.mu.Lock()
	acct := c.pool.accts[0]
	acct.Project = "proj"
	c.pool.mu.Unlock()
	// Point both endpoints at the fake server.
	restore := overrideEndpoints(srv.URL)
	defer restore()

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model: upstreamFlash, Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collect(t, stream)
	text := ""
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
	}
	if text != "hi" {
		t.Fatalf("text = %q", text)
	}
	if gotHeaders.Get("x-client-name") != "antigravity" || gotHeaders.Get("x-machine-id") != "cmdc-pak" {
		t.Errorf("identity headers missing upstream: %v", gotHeaders)
	}
	if gotHeaders.Get("Authorization") != "Bearer tok" {
		t.Errorf("Authorization = %q", gotHeaders.Get("Authorization"))
	}
	if !strings.Contains(gotBody, `"model":"gemini-3.8-flash-medium"`) {
		t.Errorf("body did not carry the tier suffix: %s", gotBody)
	}
}

func timeNowPlusHour() string {
	return time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05Z07:00")
}

// TestGeminiProxyOverridesSharedHTTPClient pins the Gemini-only proxy escape
// hatch. The process usually injects one shared HTTPClient built from the
// gateway-level proxy, but Google sign-in and inference must be able to use a
// different egress (for example, a proxy that handles Google when the global
// route is direct).
func TestGeminiProxyOverridesSharedHTTPClient(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"parts":[{"text":"via-proxy"}]}}]}}`+"\n\n")
		_, _ = io.WriteString(w, `data: [DONE]`+"\n\n")
	}))
	defer proxy.Close()

	shared := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("Gemini used the shared HTTPClient instead of its configured proxy")
		return nil, nil
	})}
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"proxy":"` + proxy.URL + `","accounts":[{"access_token":"tok","refresh_token":"r","expiry":"` + timeNowPlusHour() + `"}]}`),
		HTTPClient: shared,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g := c.(*Client)
	g.pool.mu.Lock()
	g.pool.accts[0].Project = "proj"
	g.pool.mu.Unlock()
	restore := overrideEndpoints("http://gemini.invalid")
	defer restore()

	stream, err := g.Chat(context.Background(), &core.ChatRequest{
		Model:    upstreamFlash,
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collect(t, stream)
	text := ""
	for _, ev := range events {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
	}
	if text != "via-proxy" {
		t.Fatalf("text = %q, want via-proxy", text)
	}
	if proxyHits == 0 {
		t.Fatal("the Gemini proxy was not used")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// overrideEndpoints points both inference hosts at a fake server for the
// duration of a test.
func overrideEndpoints(url string) func() {
	prevDaily, prevSandbox := endpointDaily, endpointSandbox
	endpointDaily, endpointSandbox = url, url
	return func() { endpointDaily, endpointSandbox = prevDaily, prevSandbox }
}

// A content-array request leaves the same text in Content and in a text part.
// The wire parts must carry the caller's prose once, not twice.
func TestTranslateMessagePartsDoesNotRepeatTheTextPart(t *testing.T) {
	m := core.Message{
		Role:    "user",
		Content: "hello world",
		Parts:   []core.ContentPart{{Type: "text", Text: "hello world"}},
	}
	parts, err := translateMessageParts(m, "user", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 1 || parts[0].Text != "hello world" {
		t.Fatalf("parts = %#v, want exactly one text part carrying the body", parts)
	}
	// With no flattened body the text part is the only copy and must survive.
	partsOnly, err := translateMessageParts(core.Message{
		Role:  "user",
		Parts: []core.ContentPart{{Type: "text", Text: "hello world"}},
	}, "user", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(partsOnly) != 1 || partsOnly[0].Text != "hello world" {
		t.Fatalf("parts-only parts = %#v, want the text part kept", partsOnly)
	}
}
