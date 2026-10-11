package kilocode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// --- config ---------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig(nil): %v", err)
	}
	if cfg.base() != defaultBaseURL {
		t.Fatalf("base = %q, want %q", cfg.base(), defaultBaseURL)
	}
	if !cfg.freeOnly() {
		t.Fatalf("free_only should default to true")
	}
	if !cfg.anonymousEnabled() {
		t.Fatalf("anonymous should default to true")
	}
	if cfg.usesMaxCompletionTokens() {
		t.Fatalf("max_tokens should be the default cap field")
	}
	if got := cfg.modelsTTL(); got != 10*time.Minute {
		t.Fatalf("modelsTTL = %v", got)
	}
}

func TestConfigTriState(t *testing.T) {
	raw := json.RawMessage(`{"free_only": false, "anonymous": false}`)
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if cfg.freeOnly() {
		t.Fatalf("free_only false should be honoured")
	}
	if cfg.anonymousEnabled() {
		t.Fatalf("anonymous false should be honoured")
	}
}

func TestConfigInvalidIsLoggedNotFatal(t *testing.T) {
	c := newTestClient(t, `{not json`, nil)
	if c.cfgErr == nil {
		t.Fatalf("expected a remembered config error")
	}
	if c.cfg.base() != defaultBaseURL {
		t.Fatalf("defaults should survive a bad config, got %q", c.cfg.base())
	}
}

// --- models ---------------------------------------------------------------

const modelsFixture = `{"data":[
  {"id":"z-ai/glm-5.3-flash","name":"GLM 5.3 Flash","context_length":200000,
   "architecture":{"output_modalities":["text"],"input_modalities":["text"]},
   "top_provider":{"context_length":200000,"max_completion_tokens":32768,"is_moderated":false},
   "pricing":{"prompt":"0","completion":"0"},"isFree":true},
  {"id":"kilo-auto/free","name":"Auto Free","context_length":256000,
   "architecture":{"output_modalities":["text"]},
   "top_provider":{"context_length":256000,"max_completion_tokens":32768},
   "pricing":{"prompt":"0","completion":"0"},"isFree":false},
  {"id":"stepfun/step-5-preview-free","name":"Step 5","context_length":1000000,
   "architecture":{"output_modalities":["text"]},
   "top_provider":{"max_completion_tokens":64000},
   "pricing":{"prompt":"0","completion":"0"}},
  {"id":"openai/gpt-5.4","name":"GPT-5.4","context_length":400000,
   "top_provider":{"context_length":400000,"max_completion_tokens":128000},
   "pricing":{"prompt":"0.0000025","completion":"0.00001"}}
]}`

func TestParseModelsBodyFreeDetection(t *testing.T) {
	list, err := parseModelsBody([]byte(modelsFixture))
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("got %d models, want 4", len(list))
	}
	byID := map[string]core.Model{}
	for _, m := range list {
		byID[m.ID] = m
	}
	freeCases := []string{"z-ai/glm-5.3-flash", "kilo-auto/free", "stepfun/step-5-preview-free"}
	for _, id := range freeCases {
		if !modelIsFree(byID[id]) {
			t.Errorf("%s should be free", id)
		}
	}
	if modelIsFree(byID["openai/gpt-5.4"]) {
		t.Errorf("paid model marked free")
	}
	if got := byID["z-ai/glm-5.3-flash"].Extra["max_output_tokens"]; got != 32768 {
		t.Errorf("max_output_tokens = %v, want 32768", got)
	}
	if got := byID["openai/gpt-5.4"].Extra["pricing_prompt"]; got != "0.0000025" {
		t.Errorf("pricing_prompt = %v", got)
	}
}

func TestRestrictToFree(t *testing.T) {
	list, _ := parseModelsBody([]byte(modelsFixture))
	cfg, _ := parseConfig(nil) // free_only defaults true
	got := restrictToFree(list, cfg)
	if len(got) != 3 {
		t.Fatalf("restrictToFree kept %d, want 3", len(got))
	}
	for _, m := range got {
		if !modelIsFree(m) {
			t.Errorf("kept a paid model: %s", m.ID)
		}
	}
}

func TestAuthorOf(t *testing.T) {
	cases := map[string]string{
		"z-ai/glm-5.3-flash": "z-ai",
		"nvidia/foo:free":    "nvidia",
		"kilo-auto/free":     "kilo-auto",
		"bare":               "",
	}
	for in, want := range cases {
		if got := authorOf(in); got != want {
			t.Errorf("authorOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- body -----------------------------------------------------------------

func TestBuildChatBodyDefaultMaxTokens(t *testing.T) {
	cfg, _ := parseConfig(nil)
	maxTok := 128
	req := &core.ChatRequest{
		Model:     "z-ai/glm-5.3-flash",
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &maxTok,
	}
	raw, err := buildChatBody(cfg, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["stream"] != true {
		t.Errorf("stream should always be true upstream")
	}
	if body["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v, want 128", body["max_tokens"])
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Errorf("max_completion_tokens should be absent by default")
	}
	if _, ok := body["served_by"]; ok {
		t.Errorf("ServedBy must never be copied into the body")
	}
}

func TestBuildChatBodyRejectsEmpty(t *testing.T) {
	cfg, _ := parseConfig(nil)
	if _, err := buildChatBody(cfg, nil); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("nil request: err = %v, want ErrUnsupported", err)
	}
	if _, err := buildChatBody(cfg, &core.ChatRequest{Model: "x"}); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("no messages: err = %v, want ErrUnsupported", err)
	}
}

func TestRequestMode(t *testing.T) {
	if got := requestMode(map[string]any{"mode": "plan"}); got != "plan" {
		t.Errorf("mode = %q", got)
	}
	if got := requestMode(nil); got != "" {
		t.Errorf("mode = %q, want empty", got)
	}
}

func TestNormalizeFinish(t *testing.T) {
	cases := map[string]string{
		"stop": "stop", "length": "length", "tool_calls": "tool_calls",
		"content_filter": "content_filter", "error": "stop", "banana": "stop", "": "stop",
	}
	for in, want := range cases {
		if got := normalizeFinish(in); got != want {
			t.Errorf("normalizeFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- errors ---------------------------------------------------------------

func TestParseErrorEnvelopeStringAndObject(t *testing.T) {
	code, msg := parseErrorEnvelope([]byte(`{"error":"The requested model does not exist","error_type":"model_not_found"}`))
	if code != 0 || !strings.Contains(msg, "does not exist") {
		t.Errorf("string envelope: code=%d msg=%q", code, msg)
	}
	code, msg = parseErrorEnvelope([]byte(`{"error":{"message":"rate limited","code":429}}`))
	if code != 429 || msg != "rate limited" {
		t.Errorf("object envelope: code=%d msg=%q", code, msg)
	}
}

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		status int
		text   string
		want   failureKind
	}{
		{402, "", kindCredit},
		{200, "insufficient credit", kindCredit},
		{429, "too many requests", kindRateLimit},
		{429, "200 requests per hour exceeded", kindDailyQuota},
		{401, "", kindAuth},
		{404, "model not found", kindNotFound},
		{500, "", kindServer},
		{400, "bad body", kindClient},
		{200, "flagged by moderation", kindBlocked},
	}
	for _, c := range cases {
		if got := classifyFailure(c.status, 0, c.text); got != c.want {
			t.Errorf("classifyFailure(%d, %q) = %q, want %q", c.status, c.text, got, c.want)
		}
	}
}

// --- pool -----------------------------------------------------------------

func TestAnonymousIsSelectableWithNoKey(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	now := c.now()
	if c.pool.enabled() != 1 {
		t.Fatalf("enabled = %d, want 1 (anonymous)", c.pool.enabled())
	}
	acct, err := c.pool.acquire(now, c.limit())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !acct.Anonymous || acct.ID != anonymousID {
		t.Fatalf("acquire returned %q (anonymous=%v), want the anonymous identity", acct.ID, acct.Anonymous)
	}
	c.pool.release(acct.ID)
}

func TestKeyIsPreferredOverAnonymous(t *testing.T) {
	c := newTestClient(t, `{"api_key":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"}`, nil)
	now := c.now()
	for i := 0; i < 3; i++ {
		acct, err := c.pool.acquire(now, 0)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if acct.Anonymous {
			t.Fatalf("a keyed credential was available but acquire picked anonymous")
		}
		c.pool.release(acct.ID)
	}
}

func TestAnonymousCanBeDisabled(t *testing.T) {
	c := newTestClient(t, `{"anonymous": true, "free_only": false}`, nil)
	// With free_only off and only the anonymous identity present, there is no
	// usable credential once it is disabled.
	if err := c.SetAccountEnabled(context.Background(), anonymousID, false); err != nil {
		t.Fatalf("disable anonymous: %v", err)
	}
	if _, err := c.pool.acquire(c.now(), c.limit()); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("acquire after disabling the only credential: %v, want ErrNotConfigured", err)
	}
}

func TestRemoveAnonymousRefused(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	if err := c.RemoveAccount(context.Background(), anonymousID); err == nil {
		t.Fatalf("removing the anonymous identity should be refused")
	}
}

// --- credentials ----------------------------------------------------------

func TestKeyFingerprintStableAndOpaque(t *testing.T) {
	key := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmMifQ.signature"
	id1 := accountIDFor(key)
	id2 := accountIDFor("  " + key + "  ")
	if id1 != id2 {
		t.Fatalf("fingerprint not stable: %q vs %q", id1, id2)
	}
	if strings.Contains(id1, key) {
		t.Fatalf("account id leaked the key")
	}
}

func TestLooksLikeAPIKey(t *testing.T) {
	if !looksLikeAPIKey("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmMifQ.sig") {
		t.Errorf("JWT should look like a key")
	}
	if looksLikeAPIKey("just a label") {
		t.Errorf("a sentence is not a key")
	}
}

func TestParseCredentialFile(t *testing.T) {
	key := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmMifQ.signature"
	keys := parseCredentialFile([]byte(`{"data":{"api_key":"` + key + `"}}`))
	if len(keys) != 1 || keys[0].Key != key {
		t.Fatalf("parseCredentialFile = %+v", keys)
	}
}

func TestAccountsListsAnonymous(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	recs, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		if r.ID == anonymousID {
			found = true
			if !r.Enabled {
				t.Errorf("anonymous should be enabled by default")
			}
		}
	}
	if !found {
		t.Fatalf("anonymous identity missing from Accounts()")
	}
}

// --- free_only guard ------------------------------------------------------

func TestChatRefusesPaidModelUnderFreeOnly(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "openai/gpt-5.4",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// --- streaming ------------------------------------------------------------

const streamFixture = "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Po\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ng\",\"reasoning\":\"think\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

func TestChatStreamParsesSSE(t *testing.T) {
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamFixture)
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":"`+srv.URL+`","anonymous":true}`, srv.Client())
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kilo-auto/free",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, reasoning, _, usage, finish, err := drainStream(stream)
	if err != nil {
		t.Fatalf("drainStream: %v", err)
	}
	if text != "Pong" {
		t.Errorf("text = %q, want Pong", text)
	}
	if reasoning != "think" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if usage == nil || usage.TotalTokens != 5 {
		t.Errorf("usage = %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want stop", finish)
	}
	if seenAuth != "" {
		t.Errorf("anonymous request must not send Authorization, got %q", seenAuth)
	}
}

func TestChatSendsBearerForKey(t *testing.T) {
	key := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJhYmMifQ.sig"
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":"`+srv.URL+`","api_key":"`+key+`"}`, srv.Client())
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kilo-auto/free",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = stream.Close()
	if seenAuth != "Bearer "+key {
		t.Errorf("Authorization = %q, want Bearer key", seenAuth)
	}
}

func TestChatStreamToolCallFragmentsMerge(t *testing.T) {
	fixture := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fixture)
	}))
	defer srv.Close()
	c := newTestClient(t, `{"base_url":"`+srv.URL+`"}`, srv.Client())
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kilo-auto/free",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_, _, calls, _, finish, err := drainStream(stream)
	if err != nil {
		t.Fatalf("drainStream: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "lookup" || calls[0].Arguments != `{"a":1}` {
		t.Fatalf("calls = %+v", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestChatReturnsTypedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"The requested model 'x' does not exist","error_type":"model_not_found"}`)
	}))
	defer srv.Close()
	c := newTestClient(t, `{"base_url":"`+srv.URL+`"}`, srv.Client())
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "kilo-auto/free",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	var f *core.Failure
	if !errors.As(err, &f) {
		t.Fatalf("err = %v, want *core.Failure", err)
	}
	if f.Kind != core.FailureOther {
		t.Errorf("kind = %v, want Other (not-found does not blame the account)", f.Kind)
	}
}

// --- model limits ---------------------------------------------------------

func TestModelMaxOutputTokensFromCache(t *testing.T) {
	c := newTestClient(t, `{}`, nil)
	list, err := parseModelsBody([]byte(modelsFixture))
	if err != nil {
		t.Fatal(err)
	}
	c.models.store(restrictToFree(list, c.cfg), c.now())
	if got, ok := c.ModelMaxOutputTokens(context.Background(), "z-ai/glm-5.3-flash"); !ok || got != 32768 {
		t.Fatalf("ModelMaxOutputTokens = %d, %v; want 32768, true", got, ok)
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "unknown/model"); ok {
		t.Fatalf("unknown model should answer ok=false")
	}
}

// --- helpers --------------------------------------------------------------

func newTestClient(t *testing.T, cfg string, hc *http.Client) *Client {
	t.Helper()
	raw := cfg
	deps := core.Deps{DataDir: t.TempDir(), Config: json.RawMessage(raw), HTTPClient: hc}
	cl, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := cl.(*Client)
	if !ok {
		t.Fatalf("New returned %T", cl)
	}
	return c
}
