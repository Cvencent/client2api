package zcode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// fakeTransport is an http.RoundTripper that records what was sent and replies
// from a scripted handler.  No test in this file touches the network.
type fakeTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	handler  func(*http.Request) (*http.Response, error)
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		body = string(b)
		req.Body = io.NopCloser(strings.NewReader(body))
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.bodies = append(f.bodies, body)
	handler := f.handler
	f.mu.Unlock()

	if handler == nil {
		return nil, errors.New("fakeTransport: no handler installed")
	}
	return handler(req)
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeTransport) requestAt(i int) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.requests) {
		return nil
	}
	return f.requests[i]
}

func (f *fakeTransport) bodyAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.bodies) {
		return ""
	}
	return f.bodies[i]
}

func newResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    &http.Request{},
	}
}

func jsonResponse(status int, body string) *http.Response {
	return newResponse(status, "application/json", body)
}

func sseResponse(body string) *http.Response {
	return newResponse(http.StatusOK, "text/event-stream", body)
}

func collect(t *testing.T, s core.Stream) []core.Event {
	t.Helper()
	defer func() { _ = s.Close() }()

	var out []core.Event
	for i := 0; i < 5000; i++ {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv() error: %v", err)
		}
		out = append(out, ev)
	}
	t.Fatal("stream never terminated")
	return nil
}

func assertJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not valid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want is not valid JSON: %v", err)
	}
	gb, _ := json.MarshalIndent(g, "", "  ")
	wb, _ := json.MarshalIndent(w, "", "  ")
	if string(gb) != string(wb) {
		t.Fatalf("body mismatch\n--- got ---\n%s\n--- want ---\n%s", gb, wb)
	}
}

func marshalBody(t *testing.T, req *core.ChatRequest, cfg *Config, acct *Account) []byte {
	t.Helper()
	body, err := buildRequestBody(req, cfg, acct, req.Model)
	if err != nil {
		t.Fatalf("buildRequestBody: %v", err)
	}
	raw, err := marshalNoEscape(body)
	if err != nil {
		t.Fatalf("marshalNoEscape: %v", err)
	}
	return raw
}

// mappingConfig disables the identity preamble and cache markers so the
// OpenAI -> Anthropic mapping can be asserted in isolation.
func mappingConfig() *Config {
	no := false
	cfg := loadConfig(nil, nil)
	cfg.InjectSystemBlocks = &no
	cfg.InjectCacheControl = &no
	return cfg
}

func makeJWT(payload string) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return head + "." + body + ".signature"
}

// isolateHome points the credential discovery at an empty temporary home so the
// developer's real ~/.zcode can never influence a test.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return home
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func TestConfigDefaults(t *testing.T) {
	cfg := loadConfig(nil, nil)

	if cfg.Identity.Agent != "glm" {
		t.Errorf("agent = %q, want glm", cfg.Identity.Agent)
	}
	if cfg.Identity.AppVersion != defaultAppVersion {
		t.Errorf("app version = %q", cfg.Identity.AppVersion)
	}
	if cfg.Identity.OSCategory != "windows" {
		t.Errorf("os category = %q, want windows", cfg.Identity.OSCategory)
	}
	if cfg.timeout() != 600*time.Second {
		t.Errorf("timeout = %v", cfg.timeout())
	}
	if cfg.attempts() != 5 {
		t.Errorf("attempts = %d", cfg.attempts())
	}
	if cfg.cooldown() != 300*time.Second {
		t.Errorf("cooldown = %v", cfg.cooldown())
	}
	if !cfg.autoDiscover() || !cfg.injectSystemBlocks() || !cfg.injectCacheControl() {
		t.Error("boolean defaults should all be true")
	}
	if cfg.UpstreamBase != "" {
		t.Errorf("upstream_base must default to empty, got %q", cfg.UpstreamBase)
	}
	if got := cfg.modelIDs(); len(got) != 2 || got[0] != "GLM-5.3" || got[1] != "GLM-5.3-Flash" {
		t.Errorf("modelIDs = %v", got)
	}
}

func TestConfigMalformedDegradesToDefaults(t *testing.T) {
	var logged bool
	cfg := loadConfig(json.RawMessage(`{"accounts": `), func(string, ...any) { logged = true })

	if !logged {
		t.Error("a malformed config should be reported through logf")
	}
	if cfg.Identity.Agent != "glm" || cfg.timeout() != 600*time.Second {
		t.Error("malformed config should fall back to defaults")
	}
}

func TestConfigOverrides(t *testing.T) {
	raw := `{
	  "upstream_base": "http://127.0.0.1:3000/",
	  "auto_discover": false,
	  "models": ["GLM-5.3"],
	  "max_tokens_default": 1024,
	  "max_account_attempts": 2,
	  "cooldown_seconds": 10,
	  "timeout_seconds": -1,
	  "inject_system_blocks": false,
	  "captcha_region": "cn"
	}`
	cfg := loadConfig(json.RawMessage(raw), nil)

	if cfg.UpstreamBase != "http://127.0.0.1:3000/" {
		t.Errorf("upstream_base = %q", cfg.UpstreamBase)
	}
	if cfg.autoDiscover() {
		t.Error("auto_discover should be false")
	}
	if cfg.attempts() != 2 || cfg.cooldown() != 10*time.Second {
		t.Error("numeric overrides not applied")
	}
	if cfg.timeout() != 0 {
		t.Errorf("a negative timeout_seconds means no extra deadline, got %v", cfg.timeout())
	}
	if cfg.injectSystemBlocks() {
		t.Error("inject_system_blocks should be false")
	}
	if got := cfg.modelIDs(); len(got) != 1 || got[0] != "GLM-5.3" {
		t.Errorf("modelIDs = %v", got)
	}
}

func TestJoinMessages(t *testing.T) {
	tests := []struct{ base, want string }{
		{"https://open.bigmodel.cn/api/anthropic", "https://open.bigmodel.cn/api/anthropic/v1/messages"},
		{"https://zcode.z.ai/api/v1/zcode-plan/anthropic/", "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"},
		{"http://127.0.0.1:3000", "http://127.0.0.1:3000/v1/messages"},
		{"http://127.0.0.1:3000/v1/messages", "http://127.0.0.1:3000/v1/messages"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := joinMessages(tc.base); got != tc.want {
			t.Errorf("joinMessages(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestDefaultBaseURL(t *testing.T) {
	tests := []struct {
		provider, mode, want string
	}{
		{providerZai, modeJWT, baseZaiPlan},
		{providerZai, modeAPIKey, baseZaiAPIKey},
		{providerBigmodel, modeAPIKey, baseBigmodelKey},
		{providerBigmodel, modeJWT, baseZaiPlan},
	}
	for _, tc := range tests {
		if got := defaultBaseURL(tc.provider, tc.mode); got != tc.want {
			t.Errorf("defaultBaseURL(%q,%q) = %q, want %q", tc.provider, tc.mode, got, tc.want)
		}
	}
}

func TestOSCategoryFor(t *testing.T) {
	tests := map[string]string{
		"win32-x64": "windows",
		"darwin":    "macos",
		"linux":     "linux",
	}
	for in, want := range tests {
		if got := osCategoryFor(in); got != want {
			t.Errorf("osCategoryFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// OpenAI -> Anthropic body mapping
// ---------------------------------------------------------------------------

func TestBuildBodySimpleConversation(t *testing.T) {
	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	want := `{
	  "model": "GLM-5.3",
	  "max_tokens": 4096,
	  "system": [{"type": "text", "text": "be terse"}],
	  "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
	}`
	assertJSONEqual(t, got, want)
}

func TestBuildBodyToolRoundTrip(t *testing.T) {
	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "user", Content: "weather in SF?"},
			{Role: "assistant", ToolCalls: []core.ToolCall{
				{ID: "call_1", Type: "function", Name: "get_weather", Arguments: `{"city":"SF"}`},
			}},
			{Role: "tool", ToolCallID: "call_1", Content: `{"temp":21}`},
		},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	want := `{
	  "model": "GLM-5.3",
	  "max_tokens": 4096,
	  "messages": [
	    {"role": "user", "content": [{"type": "text", "text": "weather in SF?"}]},
	    {"role": "assistant", "content": [
	      {"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "SF"}}
	    ]},
	    {"role": "user", "content": [
	      {"type": "tool_result", "tool_use_id": "call_1", "content": "{\"temp\":21}"}
	    ]}
	  ]
	}`
	assertJSONEqual(t, got, want)
}

func TestBuildBodyAssistantTextAndToolUse(t *testing.T) {
	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "assistant", Content: "let me check", ToolCalls: []core.ToolCall{
				{ID: "call_9", Name: "lookup", Arguments: `{"q":"x"}`},
			}},
		},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	want := `{
	  "model": "GLM-5.3",
	  "max_tokens": 4096,
	  "messages": [{"role": "assistant", "content": [
	    {"type": "text", "text": "let me check"},
	    {"type": "tool_use", "id": "call_9", "name": "lookup", "input": {"q": "x"}}
	  ]}]
	}`
	assertJSONEqual(t, got, want)
}

func TestBuildBodyInvalidToolArguments(t *testing.T) {
	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "c", Name: "t", Arguments: "not json"}}},
		},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	if !strings.Contains(string(got), `"_raw":"not json"`) {
		t.Fatalf("unparseable arguments should be preserved as _raw, got %s", got)
	}
}

func TestBuildBodyContentParts(t *testing.T) {
	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "user", Parts: []core.ContentPart{
				{Type: "text", Text: "what is this"},
				{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
				{Type: "image_url", ImageURL: "https://example.com/remote.png"},
			}},
		},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	want := `{
	  "model": "GLM-5.3",
	  "max_tokens": 4096,
	  "messages": [{"role": "user", "content": [
	    {"type": "text", "text": "what is this"},
	    {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}}
	  ]}]
	}`
	assertJSONEqual(t, got, want)
}

func TestBuildBodySamplingOptions(t *testing.T) {
	temp := 1.5
	topP := 0.3
	maxTok := 100

	req := &core.ChatRequest{
		Model:       "GLM-5.3",
		Messages:    []core.Message{{Role: "user", Content: "hi"}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   &maxTok,
		Stop:        []string{"END", ""},
		Stream:      true,
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	want := `{
	  "model": "GLM-5.3",
	  "max_tokens": 100,
	  "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
	  "stream": true,
	  "temperature": 1,
	  "top_p": 0.3,
	  "stop_sequences": ["END"]
	}`
	assertJSONEqual(t, got, want)
}

func TestBuildBodyMaxTokensPrecedence(t *testing.T) {
	cfg := mappingConfig()
	cfg.MaxTokensDefault = 2048

	// config default beats the built-in default
	got := marshalBody(t, &core.ChatRequest{Model: "m", Messages: []core.Message{{Role: "user", Content: "x"}}}, cfg, nil)
	if !strings.Contains(string(got), `"max_tokens":2048`) {
		t.Fatalf("config default should apply, got %s", got)
	}

	// an explicit request value beats the config default
	maxTok := 7
	got = marshalBody(t, &core.ChatRequest{Model: "m", MaxTokens: &maxTok, Messages: []core.Message{{Role: "user", Content: "x"}}}, cfg, nil)
	if !strings.Contains(string(got), `"max_tokens":7`) {
		t.Fatalf("request max_tokens should win, got %s", got)
	}
}

func TestBuildBodyToolChoice(t *testing.T) {
	tools := []core.Tool{{
		Type:        "function",
		Name:        "get_weather",
		Description: "look up weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}
	base := func(choice string) *core.ChatRequest {
		req := &core.ChatRequest{
			Model:      "GLM-5.3",
			Messages:   []core.Message{{Role: "user", Content: "hi"}},
			Tools:      tools,
			ToolChoice: json.RawMessage(choice),
		}
		return req
	}

	tests := []struct {
		name       string
		choice     string
		wantChoice string // empty means "absent"
		wantTools  bool
	}{
		{"absent", `null`, "", true},
		{"auto", `"auto"`, "", true},
		{"none drops tools", `"none"`, "", false},
		{"required becomes any", `"required"`, `{"type":"any"}`, true},
		{"named function", `{"type":"function","function":{"name":"get_weather"}}`, `{"type":"tool","name":"get_weather"}`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := marshalBody(t, base(tc.choice), mappingConfig(), nil)

			var doc map[string]any
			if err := json.Unmarshal(got, &doc); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, hasTools := doc["tools"]
			if hasTools != tc.wantTools {
				t.Errorf("tools present = %v, want %v (%s)", hasTools, tc.wantTools, got)
			}
			if tc.wantChoice == "" {
				if _, ok := doc["tool_choice"]; ok {
					t.Errorf("tool_choice should be absent, got %s", got)
				}
				return
			}
			choiceJSON, _ := json.Marshal(doc["tool_choice"])
			assertJSONEqual(t, choiceJSON, tc.wantChoice)
		})
	}
}

func TestBuildBodyToolSchemaFallback(t *testing.T) {
	req := &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools:    []core.Tool{{Name: "no_schema"}},
	}
	got := marshalBody(t, req, mappingConfig(), nil)
	if !strings.Contains(string(got), `"input_schema":{"type":"object"}`) {
		t.Fatalf("a tool without parameters needs a default object schema, got %s", got)
	}
}

func TestIdentityPreambleInjected(t *testing.T) {
	cfg := loadConfig(nil, nil) // defaults: injection on
	req := &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}, {Role: "system", Content: "caller system"}},
	}
	got := marshalBody(t, req, cfg, nil)

	var doc struct {
		System []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.System) != 5 {
		t.Fatalf("want 3 identity blocks + model block + caller block, got %d (%s)", len(doc.System), got)
	}
	if doc.System[0].Text != "You are ZCode, an interactive coding agent" {
		t.Errorf("first identity block = %q", doc.System[0].Text)
	}
	if !strings.Contains(doc.System[1].Text, "# Harness") {
		t.Error("second identity block should carry the harness section")
	}
	if !strings.Contains(doc.System[1].Text, "file_path:line_number") {
		t.Error("the \\u0060 escape should decode to a backtick in the harness block")
	}
	if !strings.Contains(doc.System[2].Text, "# Environment") {
		t.Error("third identity block should carry the environment section")
	}
	if !strings.Contains(doc.System[3].Text, "- You are powered by the model named GLM-5.3.") {
		t.Errorf("model statement block = %q", doc.System[3].Text)
	}
	if doc.System[4].Text != "caller system" {
		t.Errorf("caller system block = %q", doc.System[4].Text)
	}
	for i, b := range doc.System {
		if i >= 4 {
			break
		}
		if b.CacheControl == nil || b.CacheControl.Type != "ephemeral" {
			t.Errorf("identity block %d should carry an ephemeral cache marker", i)
		}
	}
}

func TestIdentityPreambleDisabled(t *testing.T) {
	no := false
	cfg := loadConfig(nil, nil)
	cfg.InjectSystemBlocks = &no

	req := &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "system", Content: "only mine"}},
	}
	got := marshalBody(t, req, cfg, nil)
	if strings.Contains(string(got), "You are ZCode") {
		t.Fatalf("identity preamble should be absent, got %s", got)
	}
	if !strings.Contains(string(got), `"text":"only mine"`) {
		t.Fatalf("caller system should survive, got %s", got)
	}
}

func TestCacheControlMarker(t *testing.T) {
	no := false
	cfg := loadConfig(nil, nil)
	cfg.InjectSystemBlocks = &no

	req := &core.ChatRequest{
		Model: "GLM-5.3",
		Messages: []core.Message{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "answer"},
			{Role: "user", Content: "second"},
		},
	}
	got := marshalBody(t, req, cfg, nil)

	var doc struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	last := doc.Messages[len(doc.Messages)-1].Content
	cc, ok := last[len(last)-1]["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Fatalf("last content block should carry an ephemeral marker, got %s", got)
	}
	first := doc.Messages[0].Content[0]
	if _, ok := first["cache_control"]; ok {
		t.Error("earlier blocks should not be marked")
	}
}

func TestMetadataUserIDOnlyForJWT(t *testing.T) {
	cfg := mappingConfig()
	req := &core.ChatRequest{Model: "GLM-5.3", Messages: []core.Message{{Role: "user", Content: "hi"}}}

	jwtAcct := &Account{ID: "a", Mode: modeJWT, UserID: "u-42"}
	if got := marshalBody(t, req, cfg, jwtAcct); !strings.Contains(string(got), `"metadata":{"user_id":"u-42"}`) {
		t.Fatalf("jwt account should inject metadata.user_id, got %s", got)
	}

	keyAcct := &Account{ID: "b", Mode: modeAPIKey, UserID: "u-42"}
	if got := marshalBody(t, req, cfg, keyAcct); strings.Contains(string(got), "metadata") {
		t.Fatalf("api-key account should not inject metadata, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// Anthropic -> core.Event decoding
// ---------------------------------------------------------------------------

const streamFixture = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":4}}}

event: ping
data: {"type":"ping"}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicSSEDecoding(t *testing.T) {
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(streamFixture)), "GLM-5.3")
	events := collect(t, stream)

	if len(events) != 7 {
		for i, e := range events {
			t.Logf("event %d: %+v", i, e)
		}
		t.Fatalf("got %d events, want 7", len(events))
	}

	if events[0].Type != core.EventDelta || events[0].Delta != "Hello" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Type != core.EventDelta || events[1].Delta != " world" {
		t.Errorf("event 1 = %+v", events[1])
	}
	if events[2].Type != core.EventToolCall || events[2].ToolCall == nil {
		t.Fatalf("event 2 = %+v", events[2])
	}
	if events[2].ToolCall.Index != 0 || events[2].ToolCall.ID != "toolu_1" || events[2].ToolCall.Name != "get_weather" {
		t.Errorf("tool call header = %+v", events[2].ToolCall)
	}
	if events[3].ToolCall == nil || events[3].ToolCall.Arguments != `{"city":` {
		t.Errorf("event 3 = %+v", events[3].ToolCall)
	}
	if events[4].ToolCall == nil || events[4].ToolCall.Arguments != `"SF"}` {
		t.Errorf("event 4 = %+v", events[4].ToolCall)
	}

	usage := events[5]
	if usage.Type != core.EventUsage || usage.Usage == nil {
		t.Fatalf("event 5 = %+v", usage)
	}
	if usage.Usage.PromptTokens != 14 || usage.Usage.CompletionTokens != 5 || usage.Usage.TotalTokens != 19 || usage.Usage.CachedTokens != 4 {
		t.Errorf("usage = %+v", *usage.Usage)
	}

	done := events[6]
	if done.Type != core.EventDone || done.Finish != "tool_calls" {
		t.Errorf("event 6 = %+v", done)
	}
}

func TestAnthropicSSEThinkingDeltas(t *testing.T) {
	fixture := `data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}

data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

data: {"type":"message_stop"}

`
	events := collect(t, newAnthropicStream(io.NopCloser(strings.NewReader(fixture)), "m"))
	if len(events) != 4 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Type != core.EventDelta || events[0].Reasoning != "let me think" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Delta != "done" || events[1].Reasoning != "" {
		t.Errorf("event 1 = %+v", events[1])
	}
	if events[2].Usage == nil || events[2].Usage.CompletionTokens != 3 {
		t.Errorf("event 2 = %+v", events[2])
	}
	if events[3].Finish != "stop" {
		t.Errorf("finish = %q, want stop", events[3].Finish)
	}
}

func TestAnthropicSSEErrorFrame(t *testing.T) {
	fixture := `data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}

`
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(fixture)), "m")

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if ev.Type != core.EventError || ev.Err == nil || !strings.Contains(ev.Err.Error(), "overloaded") {
		t.Fatalf("event = %+v", ev)
	}

	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("after an error event Recv should report io.EOF, got %v", err)
	}
}

func TestStreamTerminatesOnceWithEOF(t *testing.T) {
	stream := newAnthropicStream(io.NopCloser(strings.NewReader(streamFixture)), "m")
	collect(t, stream)

	// The stream is exhausted; every further Recv must report io.EOF.
	for i := 0; i < 3; i++ {
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv #%d after end = %v, want io.EOF", i, err)
		}
	}
}

func TestStreamHandlesDoneSentinelAndMissingStop(t *testing.T) {
	fixture := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\ndata: [DONE]\n\n"
	events := collect(t, newAnthropicStream(io.NopCloser(strings.NewReader(fixture)), "m"))

	if len(events) != 2 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Delta != "hi" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Type != core.EventDone || events[1].Finish != "stop" {
		t.Errorf("event 1 = %+v", events[1])
	}
}

func TestStreamUnparseableFrameIsSkipped(t *testing.T) {
	fixture := "data: not json\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	events := collect(t, newAnthropicStream(io.NopCloser(strings.NewReader(fixture)), "m"))

	if len(events) != 2 || events[0].Delta != "ok" {
		t.Fatalf("a bad frame should be skipped, got %+v", events)
	}
}

const nonStreamFixture = `{
  "id": "msg_mock_001",
  "type": "message",
  "role": "assistant",
  "model": "GLM-5.3",
  "content": [
    {"type": "text", "text": "Hello from mock upstream"},
    {"type": "tool_use", "id": "toolu_9", "name": "lookup", "input": {"q": "x"}}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 5}
}`

func TestResponseEventsNonStream(t *testing.T) {
	events, err := responseEvents([]byte(nonStreamFixture), "GLM-5.3")
	if err != nil {
		t.Fatalf("responseEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Type != core.EventDelta || events[0].Delta != "Hello from mock upstream" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].ToolCall == nil || events[1].ToolCall.Name != "lookup" {
		t.Fatalf("event 1 = %+v", events[1])
	}
	// The upstream's original spacing is preserved verbatim, so compare as JSON.
	assertJSONEqual(t, []byte(events[1].ToolCall.Arguments), `{"q":"x"}`)
	if events[2].Usage == nil || events[2].Usage.PromptTokens != 10 || events[2].Usage.CompletionTokens != 5 {
		t.Errorf("event 2 = %+v", events[2])
	}
	if events[3].Finish != "stop" {
		t.Errorf("event 3 = %+v", events[3])
	}
}

func TestFinishReasonMapping(t *testing.T) {
	tests := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"refusal":       "content_filter",
		"weird":         "stop",
		"":              "",
	}
	for in, want := range tests {
		if got := finishReason(in); got != want {
			t.Errorf("finishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// error classification
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
		code   int
	}{
		{"quota exhausted 402", 402, `{"error":{"message":"insufficient balance","type":"upstream_error"}}`, kindExhausted, 0},
		{"quota exhausted 400", 400, `{"code":1002,"message":"额度已用完"}`, kindExhausted, 1002},
		{"rate limited", 429, `{"error":{"message":"slow down"}}`, kindRateLimit, 0},
		{"429 with quota wording stays a rate limit", 429, `{"error":{"message":"quota exceeded for this key"}}`, kindRateLimit, 0},
		{"auth invalid", 401, `{"error":{"message":"invalid api key","type":"authentication_error"}}`, kindInvalid, 0},
		{"forbidden without captcha wording", 403, `{"error":{"message":"permission denied"}}`, kindInvalid, 0},
		{"captcha challenge 403", 403, `{"error":{"message":"captcha verify failed"}}`, kindCaptcha, 0},
		{"captcha code 3007", 400, `{"code":3007,"message":"captcha required"}`, kindCaptcha, 3007},
		{"model not allowed 3006", 200, `{"code":3006,"message":"model not allowed"}`, kindModel, 3006},
		{"concurrency 3009", 200, `{"code":3009,"message":"concurrency exceeded"}`, kindConcurrency, 3009},
		{"risk control 3012", 405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`, kindRisk, 3012},
		{"server error", 500, `{"error":"internal"}`, kindOther, 0},
		{"not found", 404, `{"error":{"message":"no such route","type":"invalid_request_error"}}`, kindOther, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.status, []byte(tc.body))
			if got.Kind != tc.want {
				t.Errorf("kind = %q, want %q", got.Kind, tc.want)
			}
			if got.Code != tc.code {
				t.Errorf("code = %d, want %d", got.Code, tc.code)
			}
			if got.Status != tc.status {
				t.Errorf("status = %d, want %d", got.Status, tc.status)
			}
		})
	}
}

func TestClassifyEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string // "" means healthy
	}{
		{"healthy message", nonStreamFixture, ""},
		{"code 3006 in a 200", `{"code":3006,"message":"model not allowed"}`, kindModel},
		{"code 3009 in a 200", `{"code":3009,"message":"concurrency exceeded"}`, kindConcurrency},
		{"code 0 is healthy", `{"code":0,"data":{"ok":true}}`, ""},
		{"error object in a 200", `{"error":{"message":"boom"}}`, kindOther},
		{"not json", `<html>nope</html>`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyEnvelope(http.StatusOK, []byte(tc.body))
			if tc.want == "" {
				if got != nil {
					t.Fatalf("expected no failure, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("expected a failure, got nil")
			}
			if got.Kind != tc.want {
				t.Errorf("kind = %q, want %q", got.Kind, tc.want)
			}
		})
	}
}

func TestExtractMessage(t *testing.T) {
	tests := []struct{ body, want string }{
		{`{"message":"a"}`, "a"},
		{`{"msg":"b"}`, "b"},
		{`{"error":{"message":"c"}}`, "c"},
		{`{"error":"d"}`, "d"},
		{`plain text`, "plain text"},
	}
	for _, tc := range tests {
		if got := extractMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("extractMessage(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestRedactNeverLeaksASecret(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	msg := "upstream rejected key " + secret
	got := redact(msg, secret)
	if strings.Contains(got, secret) {
		t.Fatalf("secret survived redaction: %s", got)
	}
	if !strings.Contains(got, core.MaskSecret(secret)) {
		t.Errorf("redacted form should use core.MaskSecret, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// credentials
// ---------------------------------------------------------------------------

func TestLooksLikeJWTAndUserID(t *testing.T) {
	token := makeJWT(`{"user_id":"u-7","sub":"s-7"}`)
	if !looksLikeJWT(token) {
		t.Fatal("a three-part base64url token should look like a JWT")
	}
	if looksLikeJWT("0123456789abcdef0123456789abcdef") {
		t.Fatal("a 32-hex api key is not a JWT")
	}
	if got := jwtUserID(token); got != "u-7" {
		t.Errorf("jwtUserID = %q, want u-7", got)
	}
	if got := jwtUserID(makeJWT(`{"sub":"only-sub"}`)); got != "only-sub" {
		t.Errorf("jwtUserID fallback = %q", got)
	}
	if got := jwtUserID("garbage"); got != "" {
		t.Errorf("jwtUserID on garbage = %q", got)
	}
}

func writeZcodeV2(t *testing.T, home, configJSON, credentialsJSON string) {
	t.Helper()
	dir := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if configJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(configJSON), 0o600); err != nil {
			t.Fatalf("write config.json: %v", err)
		}
	}
	if credentialsJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(credentialsJSON), 0o600); err != nil {
			t.Fatalf("write credentials.json: %v", err)
		}
	}
}

func TestDiscoveryReadsDesktopClientStores(t *testing.T) {
	home := isolateHome(t)
	jwt := makeJWT(`{"user_id":"u-1","sub":"u-1"}`)

	writeZcodeV2(t, home, `{
	  "provider": {
	    "builtin:bigmodel": {
	      "enabled": true,
	      "kind": "anthropic",
	      "name": "BigModel",
	      "options": {"apiKey": "0123456789abcdef0123456789abcdef", "baseURL": "https://open.bigmodel.cn/api/anthropic"}
	    },
	    "builtin:bigmodel-coding-plan": {
	      "enabled": true,
	      "kind": "anthropic",
	      "options": {"apiKey": "0123456789abcdef0123456789abcdef", "baseURL": "https://open.bigmodel.cn/api/anthropic"}
	    },
	    "builtin:bigmodel-start-plan": {
	      "enabled": true,
	      "kind": "anthropic",
	      "options": {"apiKey": "`+jwt+`", "baseURL": "https://zcode.z.ai/api/v1/zcode-plan/anthropic"}
	    },
	    "builtin:encrypted": {
	      "enabled": true,
	      "kind": "anthropic",
	      "options": {"apiKey": "enc:v1:opaque", "baseURL": "https://open.bigmodel.cn/api/anthropic"}
	    },
	    "builtin:other-kind": {
	      "enabled": true,
	      "kind": "openai",
	      "options": {"apiKey": "ffffffffffffffffffffffffffffffff", "baseURL": "https://example.com"}
	    }
	  }
	}`, `{"zcodejwttoken":"`+jwt+`","oauth:bigmodel:access_token":"eyJhbGciOiJI.access"}`)

	found := discover(nil)

	byID := map[string]Account{}
	for _, d := range found {
		byID[d.ID] = d.Account
	}

	if _, ok := byID["zcode-config:builtin:bigmodel"]; !ok {
		t.Fatal("the bigmodel api-key entry should be discovered")
	}
	if _, ok := byID["zcode-config:builtin:bigmodel-coding-plan"]; !ok {
		t.Error("the second provider entry should be discovered too")
	}
	if _, ok := byID["zcode-config:builtin:bigmodel-start-plan"]; !ok {
		t.Error("the start-plan entry should be discovered")
	}
	if _, ok := byID["zcode-credentials:zcodejwttoken"]; !ok {
		t.Error("the credentials.json JWT should be discovered")
	}
	if _, ok := byID["zcode-config:builtin:encrypted"]; ok {
		t.Error("enc:v1: values must not become accounts")
	}
	if _, ok := byID["zcode-config:builtin:other-kind"]; ok {
		t.Error("non-anthropic providers must not become accounts")
	}

	key := byID["zcode-config:builtin:bigmodel"]
	if key.Mode != modeAPIKey || key.Provider != providerBigmodel {
		t.Errorf("api-key account = %+v", key)
	}
	plan := byID["zcode-config:builtin:bigmodel-start-plan"]
	if plan.Mode != modeJWT || plan.Provider != providerZai || plan.UserID != "u-1" {
		t.Errorf("start-plan account = %+v", plan)
	}
}

func TestPoolDedupesAndAppliesLifecycle(t *testing.T) {
	home := isolateHome(t)
	jwt := makeJWT(`{"user_id":"u-1"}`)

	writeZcodeV2(t, home, `{"provider":{
	  "a":{"kind":"anthropic","options":{"apiKey":"0123456789abcdef0123456789abcdef","baseURL":"https://open.bigmodel.cn/api/anthropic"}},
	  "b":{"kind":"anthropic","options":{"apiKey":"0123456789abcdef0123456789abcdef","baseURL":"https://open.bigmodel.cn/api/anthropic"}},
	  "c":{"kind":"anthropic","options":{"apiKey":"`+jwt+`","baseURL":"https://zcode.z.ai/api/v1/zcode-plan/anthropic"}}
	}}`, "")

	dataDir := t.TempDir()
	cfg := loadConfig(nil, nil)
	p := newPool(dataDir, cfg, http.DefaultClient, nil)

	if got := len(p.accountsForStatus()); got != 2 {
		t.Fatalf("got %d accounts, want 2 (the duplicate key collapses)", got)
	}
	// The jwt account needs a captcha solver, so only the api-key one is usable.
	if got := p.usableCount(); got != 1 {
		t.Fatalf("usableCount = %d, want 1", got)
	}

	statuses := p.accountsForStatus()
	var jwtNote string
	for _, s := range statuses {
		if s.Extra["mode"] == modeJWT {
			jwtNote = s.Note
		}
	}
	if !strings.Contains(jwtNote, "captcha") {
		t.Errorf("the jwt account should explain that it needs a solver, note = %q", jwtNote)
	}

	// Put the api-key account into cooling and confirm the state survives a
	// process restart (a fresh pool over the same data dir).
	var keyID string
	for _, s := range statuses {
		if s.Extra["mode"] == modeAPIKey {
			keyID = s.ID
		}
	}
	p.mark(keyID, func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
	})

	if got := p.usableCount(); got != 0 {
		t.Fatalf("a cooling account must not be selectable, usableCount = %d", got)
	}

	reloaded := newPool(dataDir, cfg, http.DefaultClient, nil)
	if got := reloaded.usableCount(); got != 0 {
		t.Fatalf("cooling state did not persist, usableCount = %d", got)
	}

	// Cooling in the past reads as ready again.
	reloaded.mark(keyID, func(a *Account) { a.CooldownUntil = time.Now().Add(-time.Minute) })
	if got := reloaded.usableCount(); got != 1 {
		t.Fatalf("an expired cooldown should read as ready, usableCount = %d", got)
	}

	// The persisted state file must not contain the credential.
	raw, err := os.ReadFile(filepath.Join(dataDir, accountsFile))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(raw), "0123456789abcdef") || strings.Contains(string(raw), jwt) {
		t.Fatalf("persisted state leaked a credential: %s", raw)
	}
}

func TestPoolNeverSelectsTheSameAccountTwiceInOneRequest(t *testing.T) {
	home := isolateHome(t)
	writeZcodeV2(t, home, `{"provider":{
	  "a":{"kind":"anthropic","options":{"apiKey":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","baseURL":"https://open.bigmodel.cn/api/anthropic"}},
	  "b":{"kind":"anthropic","options":{"apiKey":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","baseURL":"https://open.bigmodel.cn/api/anthropic"}}
	}}`, "")

	p := newPool(t.TempDir(), loadConfig(nil, nil), http.DefaultClient, nil)
	exclude := map[string]bool{}

	first := p.next(exclude)
	second := p.next(exclude)
	third := p.next(exclude)

	if first == nil || second == nil {
		t.Fatal("two distinct accounts should be selectable")
	}
	if first.ID == second.ID {
		t.Fatalf("the same account was selected twice: %s", first.ID)
	}
	if third != nil {
		t.Fatalf("a third selection should be impossible, got %s", third.ID)
	}
}

// ---------------------------------------------------------------------------
// region discovery
// ---------------------------------------------------------------------------

func TestRegionIsReadFromTheEndpoint(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"configs":{"captcha":{"enabled":true,"prefix":"no8xfe","region":"sgp","sceneId":"11xygtvd"}}}}`), nil
	}}
	p := newPool(t.TempDir(), loadConfig(nil, nil), &http.Client{Transport: transport}, nil)

	info := p.regionFor(t.Context())
	if info.Region != "sgp" || info.SceneID != "11xygtvd" || info.Prefix != "no8xfe" {
		t.Fatalf("region info = %+v", info)
	}

	// A second call is served from the cache.
	_ = p.regionFor(t.Context())
	if got := transport.count(); got != 1 {
		t.Errorf("region lookups should be cached, got %d requests", got)
	}

	req := transport.requestAt(0)
	if req == nil || !strings.Contains(req.URL.String(), "/api/v1/client/configs") {
		t.Fatalf("unexpected request: %v", req)
	}
}

// TestRegionFetchSendsTheConfiguredPlatform pins a real defect: this request
// hard-coded platform=win32, which the vendor answers with
// HTTP 400 {"code":3001,"msg":"parameter error"}.  The lookup then failed
// silently, so the scene id and the region were never available and the
// browser captcha path could not run at all.
func TestRegionFetchSendsTheConfiguredPlatform(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"configs":{"captcha":{"region":"cn"}}}}`), nil
	}}
	p := newPool(t.TempDir(), loadConfig(nil, nil), &http.Client{Transport: transport}, nil)

	_ = p.regionFor(t.Context())
	req := transport.requestAt(0)
	if req == nil {
		t.Fatal("no request was made")
	}
	got := req.URL.Query().Get("platform")
	if got == "win32" {
		t.Fatalf("platform = %q, which the vendor rejects with HTTP 400 code 3001", got)
	}
	if want := p.cfg.Identity.Platform; got != want {
		t.Errorf("platform = %q, want the configured identity %q", got, want)
	}
	if got := req.URL.Query().Get("app_version"); got == "" {
		t.Error("app_version must be sent")
	}
}

// TestRegionFetchFollowsTheConfiguredPlatform: the value is not a constant to
// swap for a better constant; it is whatever identity the client sends.
func TestRegionFetchFollowsTheConfiguredPlatform(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"configs":{"captcha":{"region":"cn"}}}}`), nil
	}}
	cfg := loadConfig(nil, nil)
	cfg.Identity.Platform = "darwin-arm64"
	p := newPool(t.TempDir(), cfg, &http.Client{Transport: transport}, nil)

	_ = p.regionFor(t.Context())
	req := transport.requestAt(0)
	if req == nil {
		t.Fatal("no request was made")
	}
	if got := req.URL.Query().Get("platform"); got != "darwin-arm64" {
		t.Errorf("platform = %q, want darwin-arm64", got)
	}
}

func TestRegionConfigOverrideWins(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"data":{"configs":{"captcha":{"region":"sgp"}}}}`), nil
	}}
	cfg := loadConfig(nil, nil)
	cfg.CaptchaRegion = "cn"

	p := newPool(t.TempDir(), cfg, &http.Client{Transport: transport}, nil)
	if got := p.regionFor(t.Context()).Region; got != "cn" {
		t.Fatalf("region = %q, want the configured cn", got)
	}
	if got := transport.count(); got != 0 {
		t.Errorf("an explicit region must not trigger a network call, got %d", got)
	}
}

func TestRegionFetchFailureIsNotFatal(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusInternalServerError, `{"code":3001,"message":"bad platform"}`), nil
	}}
	p := newPool(t.TempDir(), loadConfig(nil, nil), &http.Client{Transport: transport}, nil)
	if got := p.regionFor(t.Context()).Region; got != "" {
		t.Fatalf("region = %q, want empty on failure", got)
	}
}

// ---------------------------------------------------------------------------
// Chat end to end (still offline)
// ---------------------------------------------------------------------------

func newTestClient(t *testing.T, configJSON string, transport *fakeTransport) *Client {
	t.Helper()
	c, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(configJSON),
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}
	return client
}

// firstAccount loads the pool and returns its first account, so a test can
// exercise header assembly without going through a full Chat call.
func firstAccount(t *testing.T, c *Client) *Account {
	t.Helper()
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.pool.ensureLocked()
	if len(c.pool.accounts) == 0 {
		t.Fatal("the pool has no accounts")
	}
	return c.pool.accounts[0]
}

func TestChatStopgapNonStreaming(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := newTestClient(t, `{"upstream_base":"http://127.0.0.1:3000","auto_discover":false}`, transport)

	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collect(t, stream)

	if len(events) != 4 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Delta != "Hello from mock upstream" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[3].Type != core.EventDone || events[3].Finish != "stop" {
		t.Errorf("event 3 = %+v", events[3])
	}

	req := transport.requestAt(0)
	if req == nil {
		t.Fatal("no request recorded")
	}
	if got := req.URL.String(); got != "http://127.0.0.1:3000/v1/messages" {
		t.Errorf("url = %q", got)
	}
	if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := req.Header.Get("X-ZCode-Agent"); got != "glm" {
		t.Errorf("X-ZCode-Agent = %q", got)
	}
	if got := req.Header.Get("X-Device-Mid"); got == "" {
		t.Error("X-Device-Mid should be sent")
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("the stopgap route must not send credentials, got %q", got)
	}

	// The identity preamble must actually be on the wire.
	if body := transport.bodyAt(0); !strings.Contains(body, "You are ZCode, an interactive coding agent") {
		t.Errorf("identity preamble missing from the request body: %s", body)
	}
}

func TestChatStopgapStreaming(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return sseResponse(streamFixture), nil
	}}
	c := newTestClient(t, `{"upstream_base":"http://127.0.0.1:3000","auto_discover":false}`, transport)

	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collect(t, stream)

	if len(events) != 7 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if !strings.Contains(transport.bodyAt(0), `"stream":true`) {
		t.Errorf("streaming request should ask the upstream to stream: %s", transport.bodyAt(0))
	}
}

func TestChatWithoutCredentials(t *testing.T) {
	isolateHome(t)
	transport := &fakeTransport{}
	c := newTestClient(t, `{"auto_discover":false}`, transport)

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if got := transport.count(); got != 0 {
		t.Errorf("no request should be attempted without credentials, got %d", got)
	}
}

func TestChatRejectsAModelTheUpstreamRefuses(t *testing.T) {
	isolateHome(t)
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"code":3006,"message":"model not allowed"}`), nil
	}}
	c := newTestClient(t, `{"auto_discover":false,"accounts":[
	  {"id":"k1","provider":"bigmodel","mode":"api_key","api_key":"0123456789abcdef0123456789abcdef"}
	]}`, transport)

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported", err)
	}
	// A model rejection is not account-specific: it must not be retried.
	if got := transport.count(); got != 1 {
		t.Errorf("got %d attempts, want exactly 1", got)
	}
}

func TestChatFailsOverToTheNextAccountOnRiskControl(t *testing.T) {
	isolateHome(t)
	attempt := 0
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		attempt++
		if attempt == 1 {
			return jsonResponse(http.StatusMethodNotAllowed, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`), nil
		}
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := newTestClient(t, `{"auto_discover":false,"accounts":[
	  {"id":"k1","provider":"bigmodel","mode":"api_key","api_key":"11111111111111111111111111111111"},
	  {"id":"k2","provider":"bigmodel","mode":"api_key","api_key":"22222222222222222222222222222222"}
	]}`, transport)

	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat should have failed over, got %v", err)
	}
	events := collect(t, stream)
	if len(events) == 0 || events[len(events)-1].Type != core.EventDone {
		t.Fatalf("events = %+v", events)
	}
	if got := transport.count(); got != 2 {
		t.Errorf("got %d attempts, want 2", got)
	}
	if got := transport.requestAt(1).Header.Get("X-Api-Key"); got != "22222222222222222222222222222222" {
		t.Errorf("second attempt used %q", got)
	}

	// The first account is now cooling.
	var cooling bool
	for _, a := range c.Status(t.Context()).Accounts {
		if a.ID == "k1" && a.State == stateCooling {
			cooling = true
		}
	}
	if !cooling {
		t.Errorf("k1 should be cooling after 3012: %+v", c.Status(t.Context()).Accounts)
	}
}

func TestChatJWTAccountNeedsASolver(t *testing.T) {
	isolateHome(t)
	transport := &fakeTransport{}
	c := newTestClient(t, `{"auto_discover":false,"accounts":[
	  {"id":"j1","provider":"zai","mode":"jwt","jwt":"`+makeJWT(`{"user_id":"u"}`)+`"}
	]}`, transport)

	_, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if got := transport.count(); got != 0 {
		t.Errorf("a jwt-only pool must not send anything without a solver, got %d", got)
	}
}

func TestApplyHeadersJWTChannel(t *testing.T) {
	isolateHome(t)
	jwt := makeJWT(`{"user_id":"u-9"}`)
	c := newTestClient(t, `{"auto_discover":false,"captcha_region":"cn","accounts":[
	  {"id":"j1","provider":"zai","mode":"jwt","jwt":"`+jwt+`"}
	]}`, &fakeTransport{})

	acct := firstAccount(t, c)
	req, err := http.NewRequest(http.MethodPost, "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	c.applyHeaders(req, acct, "cn", "PARAM123")

	if got := req.Header.Get("Authorization"); got != "Bearer "+jwt {
		t.Errorf("Authorization = %q", got)
	}
	if req.Header.Get("X-Api-Key") != "" {
		t.Error("the jwt channel must not send x-api-key")
	}
	for _, h := range []string{"X-Request-Id", "X-Zcode-Session-Type", "X-Zcode-Trace-Id"} {
		if req.Header.Get(h) == "" {
			t.Errorf("jwt channel is missing %s", h)
		}
	}
	if got := req.Header.Get("X-Zcode-Session-Type"); got != "main" {
		t.Errorf("X-Zcode-Session-Type = %q, want main", got)
	}
	// Sending either of these on the start-plan channel trips risk control.
	for _, h := range []string{"X-Query-Id", "X-Session-Id"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s must not be sent on the jwt channel", h)
		}
	}
	if got := req.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "PARAM123" {
		t.Errorf("captcha param = %q", got)
	}
	if got := req.Header.Get("X-Aliyun-Captcha-Verify-Region"); got != "cn" {
		t.Errorf("captcha region = %q", got)
	}
	if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := req.Header.Get("X-Zcode-Agent"); got != "glm" {
		t.Errorf("X-ZCode-Agent = %q", got)
	}
	if got := req.Header.Get("X-Os-Category"); got != "windows" {
		t.Errorf("X-Os-Category = %q", got)
	}
}

func TestApplyHeadersAPIKeyChannel(t *testing.T) {
	isolateHome(t)
	key := "0123456789abcdef0123456789abcdef"
	c := newTestClient(t, `{"auto_discover":false,"accounts":[
	  {"id":"k1","provider":"bigmodel","mode":"api_key","api_key":"`+key+`"}
	]}`, &fakeTransport{})

	acct := firstAccount(t, c)
	req, err := http.NewRequest(http.MethodPost, "https://open.bigmodel.cn/api/anthropic/v1/messages", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	c.applyHeaders(req, acct, "", "")

	if got := req.Header.Get("X-Api-Key"); got != key {
		t.Errorf("x-api-key = %q", got)
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("the api-key channel must not send Authorization")
	}
	for _, h := range []string{"X-Request-Id", "X-Zcode-Session-Type", "X-Zcode-Trace-Id", "X-Aliyun-Captcha-Verify-Param"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s must not be sent on the api-key channel", h)
		}
	}
}

func TestSolveCaptchaWithoutCommand(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})

	_, err := c.solveCaptcha(t.Context(), regionInfo{Region: "cn"})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
}

func TestParseVerifyParam(t *testing.T) {
	tests := []struct {
		out  string
		want string
		ok   bool
	}{
		{"VERIFY_PARAM=abc123\n", "abc123", true},
		{"noise\nVERIFY_PARAM=xyz\nmore noise\n", "xyz", true},
		{"  VERIFY_PARAM=padded  \n", "padded", true},
		{"VERIFY_PARAM=\n", "", false},
		{"nothing here", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := parseVerifyParam(tc.out)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseVerifyParam(%q) = (%q,%v), want (%q,%v)", tc.out, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCaptchaArgs(t *testing.T) {
	cfg := loadConfig(nil, nil)
	cfg.CaptchaArgs = []string{"{scene}", "--region={region}", "{prefix}", "plain"}

	got := captchaArgs(cfg, regionInfo{Region: "cn", SceneID: "11xygtvd", Prefix: "no8xfe"})
	want := []string{"11xygtvd", "--region=cn", "no8xfe", "plain"}
	if len(got) != len(want) {
		t.Fatalf("captchaArgs = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("captchaArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSolveCaptchaRunsTheConfiguredCommand(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no known shell interpreter")
	}
	isolateHome(t)

	dir := t.TempDir()
	script := filepath.Join(dir, "solver.sh")
	body := "#!/bin/sh\necho 'VERIFY_PARAM=from-solver'\n"
	if runtime.GOOS == "windows" {
		script = filepath.Join(dir, "solver.cmd")
		body = "@echo off\r\necho VERIFY_PARAM=from-solver\r\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write solver: %v", err)
	}

	command := script
	var args []string
	if runtime.GOOS == "windows" {
		command = "cmd.exe"
		args = []string{"/c", script}
	}

	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})
	c.cfg.CaptchaCommand = command
	c.cfg.CaptchaArgs = args

	param, err := c.solveCaptcha(t.Context(), regionInfo{Region: "cn"})
	if err != nil {
		t.Fatalf("solveCaptcha: %v", err)
	}
	if param != "from-solver" {
		t.Fatalf("param = %q", param)
	}
}

func TestSolveCaptchaReportsAFailingCommand(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})
	c.cfg.CaptchaCommand = filepath.Join(t.TempDir(), "does-not-exist")

	if _, err := c.solveCaptcha(t.Context(), regionInfo{}); err == nil {
		t.Fatal("a missing solver should be reported")
	} else if errors.Is(err, core.ErrNotConfigured) {
		t.Fatal("a broken solver is an operational failure, not a configuration gap")
	}
}

func TestChatRejectsAnEmptyModel(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})

	if _, err := c.Chat(t.Context(), &core.ChatRequest{}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(t.Context(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("nil request err = %v, want core.ErrUnsupported", err)
	}
}

// ---------------------------------------------------------------------------
// Models / Status
// ---------------------------------------------------------------------------

func TestModels(t *testing.T) {
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})

	models, err := c.Models(t.Context())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "GLM-5.3" || models[1].ID != "GLM-5.3-Flash" {
		t.Fatalf("models = %+v", models)
	}
	if c.Name() != "zcode" {
		t.Errorf("Name() = %q", c.Name())
	}
}

func TestStatusIsCheapAndHonest(t *testing.T) {
	isolateHome(t)
	transport := &fakeTransport{}
	c := newTestClient(t, `{"auto_discover":false,"accounts":[
	  {"id":"k1","provider":"bigmodel","mode":"api_key","api_key":"0123456789abcdef0123456789abcdef"}
	]}`, transport)

	st := c.Status(t.Context())

	if got := transport.count(); got != 0 {
		t.Errorf("Status must not make network calls, got %d", got)
	}
	if !st.Ready {
		t.Errorf("Ready = false with a usable account: %s", st.Detail)
	}
	if st.Name != "zcode" {
		t.Errorf("Name = %q", st.Name)
	}
	if len(st.Models) != 2 {
		t.Errorf("Models = %v", st.Models)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("Accounts = %+v", st.Accounts)
	}
	if st.Accounts[0].State != stateReady {
		t.Errorf("state = %q", st.Accounts[0].State)
	}
	if st.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should be set")
	}

	// The secret must never appear anywhere in the rendered status.
	rendered, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if strings.Contains(string(rendered), "0123456789abcdef0123456789abcdef") {
		t.Fatalf("status leaked the credential: %s", rendered)
	}
}

func TestStatusExplainsTheStopgapOverride(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"upstream_base":"http://127.0.0.1:3000","auto_discover":false}`, &fakeTransport{})

	st := c.Status(t.Context())
	if !st.Ready {
		t.Error("the stopgap override should read as ready")
	}
	if !strings.Contains(st.Detail, "upstream_base") {
		t.Errorf("Detail = %q", st.Detail)
	}
}

func TestStatusExplainsAnEmptyPool(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})

	st := c.Status(t.Context())
	if st.Ready {
		t.Error("an empty pool must not be ready")
	}
	if !strings.Contains(st.Detail, "no zcode credentials") {
		t.Errorf("Detail = %q", st.Detail)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func TestPrintableASCII(t *testing.T) {
	if !printableASCII("ZCode/3.11.2") {
		t.Error("ascii should pass")
	}
	if printableASCII("bad\nvalue") || printableASCII("中文") {
		t.Error("control characters and non-ascii must be rejected")
	}
}

func TestRandomUUIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		u := randomUUID()
		if len(u) != 36 || strings.Count(u, "-") != 4 {
			t.Fatalf("uuid = %q", u)
		}
		if u[14] != '4' {
			t.Fatalf("uuid version nibble = %q in %q", u[14], u)
		}
		if seen[u] {
			t.Fatalf("duplicate uuid %q", u)
		}
		seen[u] = true
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 10); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
}

func TestClamp01(t *testing.T) {
	tests := map[float64]float64{-1: 0, 0.5: 0.5, 2: 1}
	for in, want := range tests {
		if got := clamp01(in); got != want {
			t.Errorf("clamp01(%v) = %v, want %v", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// live (opt-in)
// ---------------------------------------------------------------------------

// TestLiveUpstream is the only test that touches the network.  It is skipped
// unless CLIENT2API_LIVE=1, and it defaults to the documented local stopgap
// (an Anthropic-wire gateway on 127.0.0.1:3000).  Point CLIENT2API_LIVE_BASE at
// any other Anthropic-wire base to use it instead.
func TestLiveUpstream(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE") != "1" {
		t.Skip("set CLIENT2API_LIVE=1 to exercise a real upstream")
	}

	base := strings.TrimSpace(os.Getenv("CLIENT2API_LIVE_BASE"))
	if base == "" {
		base = "http://127.0.0.1:3000"
	}
	model := strings.TrimSpace(os.Getenv("CLIENT2API_LIVE_MODEL"))
	if model == "" {
		model = "GLM-5.3"
	}

	client, err := New(core.Deps{
		DataDir:    t.TempDir(),
		Config:     json.RawMessage(`{"upstream_base":"` + base + `","auto_discover":false}`),
		HTTPClient: &http.Client{Timeout: 180 * time.Second},
		Logf:       t.Logf,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	prompt := "Reply with exactly: hello from zcode"

	t.Run("non-streaming", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		stream, err := client.Chat(ctx, &core.ChatRequest{
			Model:    model,
			Messages: []core.Message{{Role: "user", Content: prompt}},
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		events := collect(t, stream)

		var text string
		var done bool
		for _, ev := range events {
			switch ev.Type {
			case core.EventDelta:
				text += ev.Delta
			case core.EventDone:
				done = true
				t.Logf("finish=%q", ev.Finish)
			case core.EventUsage:
				if ev.Usage != nil {
					t.Logf("usage: prompt=%d completion=%d total=%d cached=%d",
						ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.TotalTokens, ev.Usage.CachedTokens)
				}
			case core.EventError:
				t.Errorf("upstream error: %v", ev.Err)
			}
		}
		if strings.TrimSpace(text) == "" {
			t.Error("no text was produced")
		}
		if !done {
			t.Error("no EventDone was emitted")
		}
		t.Logf("non-streaming text: %q", text)
	})

	t.Run("streaming", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()

		stream, err := client.Chat(ctx, &core.ChatRequest{
			Model:    model,
			Messages: []core.Message{{Role: "user", Content: prompt}},
			Stream:   true,
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		events := collect(t, stream)

		var text string
		var done bool
		for _, ev := range events {
			switch ev.Type {
			case core.EventDelta:
				text += ev.Delta
			case core.EventDone:
				done = true
				t.Logf("finish=%q", ev.Finish)
			case core.EventError:
				t.Errorf("upstream error: %v", ev.Err)
			}
		}
		if strings.TrimSpace(text) == "" {
			t.Error("no text was produced")
		}
		if !done {
			t.Error("no EventDone was emitted")
		}
		t.Logf("streaming text: %q", text)
	})
}

// TestZcodeChatNamesTheServedAccount pins the gateway-facing attribution.  The
// credential that served a turn is known only inside Chat, and before this slot
// existed every success was filed under "(unrouted)".  The pool holds the one
// account accountsConfigJSON(1) builds, whose id is k1, and the assertion is on
// the value Chat left on the slot after a real success.
func TestZcodeChatNamesTheServedAccount(t *testing.T) {
	isolateHome(t)
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := newTestClient(t, accountsConfigJSON(1), transport)

	var served string
	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:    "GLM-5.3",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collect(t, stream)

	if served != "k1" {
		t.Errorf("ServedBy = %q, want %q", served, "k1")
	}
}
