package codearts

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

// upstream_test.go covers failure classification, error-text hygiene, the chat
// request body, and the header-ordering trap as it is actually applied on the
// wire (not just inside the signer).

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   errKind
	}{
		{200, "", kindNone},
		{401, "", kindAuth},
		{403, "", kindAuth},
		{200, `{"error_code":"APIG.0602"}`, kindAuth},
		{200, `{"error_msg":"Invalid token"}`, kindAuth},
		{402, "", kindQuota},
		{429, "", kindQuota},
		{200, `{"error_msg":"insufficient quota"}`, kindQuota},
		{400, `{"error_code":"TM.00001041"}`, kindQueue},
		{400, `{"error_msg":"peak usage, try again after 10s"}`, kindQueue},
		{400, `{"error_msg":"high demand"}`, kindQueue},
		{400, `{"error_msg":"the model does not exist"}`, kindClient},
		{404, "", kindClient},
		{500, "", kindTransient},
		{503, "", kindTransient},
	}
	for _, c := range cases {
		if got := classifyStatus(c.status, c.body); got != c.want {
			t.Errorf("classifyStatus(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// A queue rejection is reported to the gateway as rate-limited, because that
// is the kind the gateway's rotation policy retries.
func TestFailureKindMapping(t *testing.T) {
	cases := map[errKind]core.FailureKind{
		kindQuota:     core.FailureQuota,
		kindAuth:      core.FailureAuth,
		kindQueue:     core.FailureRateLimited,
		kindTransient: core.FailureRateLimited,
		kindNetwork:   core.FailureRateLimited,
		kindClient:    core.FailureOther,
		kindNone:      core.FailureUpstream,
	}
	for in, want := range cases {
		if got := failureKind(in); got != want {
			t.Errorf("failureKind(%v) = %v, want %v", in, got, want)
		}
	}
}

// A queue body is only a queue body on a 400: a 200 that merely mentions the
// phrase must not be classified as one.
func TestIsQueueBodyRequiresA400(t *testing.T) {
	if isQueueBody(200, "TM.00001041") {
		t.Error("a 200 was classified as a queue rejection")
	}
	if !isQueueBody(400, "TM.00001041") {
		t.Error("a 400 naming TM.00001041 was not classified as a queue rejection")
	}
}

func TestIsAuthBody(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   bool
	}{
		{401, "", true}, {403, "", true},
		{200, "APIG.0602", true}, {200, "token expired", true},
		{400, "bad request", false},
	} {
		if got := isAuthBody(c.status, c.body); got != c.want {
			t.Errorf("isAuthBody(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestIsQueueErrorCode(t *testing.T) {
	for _, code := range []string{"TM.00001041", "InferHub.ModelArts.81111.429", "TPM limit", "rate_limit", "排队", "限流"} {
		if !isQueueErrorCode(code) {
			t.Errorf("isQueueErrorCode(%q) = false, want true", code)
		}
	}
	for _, code := range []string{"", "   ", "APIG.0602", "invalid_request"} {
		if isQueueErrorCode(code) {
			t.Errorf("isQueueErrorCode(%q) = true, want false", code)
		}
	}
}

// Error text must never carry a credential into a log line or the panel.
func TestCleanErrorText(t *testing.T) {
	got := cleanErrorText("<html><body>secret_access_key: ABCDEFGHIJKLMNOP\n\n  too   many\nrequests</body></html>")
	if strings.Contains(got, "ABCDEFGHIJKLMNOP") {
		t.Errorf("cleanErrorText = %q, want the secret scrubbed", got)
	}
	if strings.Contains(got, "<") || strings.Contains(got, "\n") || strings.Contains(got, "  ") {
		t.Errorf("cleanErrorText = %q, want tags and runs of whitespace collapsed", got)
	}
	if !strings.Contains(got, "too many requests") {
		t.Errorf("cleanErrorText = %q, want the useful part kept", got)
	}
}

func TestCleanErrorTextCaps(t *testing.T) {
	got := cleanErrorText(strings.Repeat("x", maxErrorText*3))
	if len(got) > maxErrorText+8 {
		t.Errorf("cleanErrorText returned %d characters, want it capped near %d", len(got), maxErrorText)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("cleanErrorText = %q, want an ellipsis marking the truncation", got)
	}
}

func TestCleanErrorTextScrubsABearerToken(t *testing.T) {
	got := cleanErrorText("failed with Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefg")
	if strings.Contains(got, "eyJhbGciOiJIUzI1NiJ9") {
		t.Errorf("cleanErrorText = %q, want the token scrubbed", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter(nil); d != 0 {
		t.Errorf("parseRetryAfter(nil) = %v, want 0", d)
	}
	h := http.Header{}
	h.Set("Retry-After", "30")
	if d := parseRetryAfter(h); d < 29*time.Second || d > 31*time.Second {
		t.Errorf("Retry-After: 30 → %v, want about 30s", d)
	}
	h = http.Header{}
	h.Set("Retry-After-Ms", "1500")
	if d := parseRetryAfter(h); d < 1400*time.Millisecond || d > 1600*time.Millisecond {
		t.Errorf("Retry-After-Ms: 1500 → %v, want about 1.5s", d)
	}
	h = http.Header{}
	h.Set("Retry-After", time.Now().Add(45*time.Second).UTC().Format(http.TimeFormat))
	if d := parseRetryAfter(h); d < 40*time.Second || d > 50*time.Second {
		t.Errorf("an HTTP date → %v, want about 45s", d)
	}
	// A hostile header must not park an account for a week.
	h = http.Header{}
	h.Set("Retry-After", "999999")
	if d := parseRetryAfter(h); d != 2*time.Hour {
		t.Errorf("a huge Retry-After → %v, want it capped at 2h", d)
	}
	h = http.Header{}
	h.Set("Retry-After", "0")
	if d := parseRetryAfter(h); d != 0 {
		t.Errorf("Retry-After: 0 → %v, want 0", d)
	}
}

func TestClassifyErr(t *testing.T) {
	if k, _ := classifyErr(nil); k != kindNone {
		t.Errorf("classifyErr(nil) = %v, want kindNone", k)
	}
	if k, _ := classifyErr(context.DeadlineExceeded); k != kindTransient {
		t.Errorf("classifyErr(DeadlineExceeded) = %v, want kindTransient", k)
	}
	if k, _ := classifyErr(context.Canceled); k != kindTransient {
		t.Errorf("classifyErr(Canceled) = %v, want kindTransient", k)
	}
	if k, _ := classifyErr(io.ErrUnexpectedEOF); k != kindNetwork {
		t.Errorf("classifyErr(a transport error) = %v, want kindNetwork", k)
	}
	ue := newUpstreamError(429, "", nil)
	if k, _ := classifyErr(ue); k != kindQuota {
		t.Errorf("classifyErr(429) = %v, want kindQuota", k)
	}
	if _, ok := asUpstreamError(ue); !ok {
		t.Error("asUpstreamError did not recognise its own type")
	}
}

func TestMessageText(t *testing.T) {
	if got := messageText(core.Message{Role: "user", Content: "plain"}); got != "plain" {
		t.Errorf("messageText = %q, want plain", got)
	}
	if got := messageText(core.Message{Role: "user"}); got != "" {
		t.Errorf("messageText of an empty message = %q, want empty", got)
	}
	got := messageText(core.Message{Role: "user", Parts: []core.ContentPart{
		{Type: "text", Text: "first"},
		{Type: "", Text: "second"},
		{Type: "image_url", ImageURL: "https://example.invalid/a.png"},
	}})
	if got != "first\nsecond\n[image: https://example.invalid/a.png]" {
		t.Errorf("messageText = %q, want the parts joined with the image described", got)
	}
	// An image with no URL is described rather than silently dropped.
	if got := messageText(core.Message{Parts: []core.ContentPart{{Type: "image_url"}}}); !strings.Contains(got, "unavailable") {
		t.Errorf("messageText = %q, want the missing URL called out", got)
	}
}

func TestWireToolCalls(t *testing.T) {
	if got := wireToolCalls(nil); got != nil {
		t.Errorf("wireToolCalls(nil) = %+v, want nil", got)
	}
	got := wireToolCalls([]core.ToolCall{{ID: "c1", Name: "read"}})
	if len(got) != 1 {
		t.Fatalf("got %d calls, want 1", len(got))
	}
	if got[0].Type != "function" {
		t.Errorf("type = %q, want the default function", got[0].Type)
	}
	if got[0].Function.Arguments != "{}" {
		t.Errorf("arguments = %q, want the {} default", got[0].Function.Arguments)
	}
}

// ---------------------------------------------------------------------------
// the chat request body
// ---------------------------------------------------------------------------

func TestBuildChatBodyShape(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	raw, err := c.buildChatBody(&core.ChatRequest{
		Model:    "deepseek-v4-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}, "session-1")
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if got["stream"] != true {
		t.Error("stream = false, want true (the module only ever streams)")
	}
	if got["prompt_cache_key"] != "session-1" {
		t.Errorf("prompt_cache_key = %v, want the session id", got["prompt_cache_key"])
	}
	if got["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_summary = %v, want auto", got["reasoning_summary"])
	}
	if got["tool_stream"] != true {
		t.Error("tool_stream = false, want true")
	}
	if got["max_tokens"] != float64(defaultMaxTokens) {
		t.Errorf("max_tokens = %v, want %d", got["max_tokens"], defaultMaxTokens)
	}
	inc, _ := got["include"].([]any)
	if len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v, want the reasoning.encrypted_content opt-in", got["include"])
	}
	// These three are present only when the operator asked for them.
	if _, ok := got["thinking"]; ok {
		t.Error("thinking was sent even though reasoning is not off")
	}
	if _, ok := got["tools"]; ok {
		t.Error("tools was sent even though the caller supplied none")
	}
	if _, ok := got["temperature"]; ok {
		t.Error("temperature was sent even though the caller set none")
	}
}

func TestBuildChatBodyThinkingOnlyWhenReasoningIsOff(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","reasoning_effort":"off"}`)
	raw, err := c.buildChatBody(&core.ChatRequest{Model: "m", Messages: []core.Message{{Role: "user", Content: "hi"}}}, "s")
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var got struct {
		Thinking *wireThinking `json:"thinking"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Thinking == nil || got.Thinking.Type != "disabled" {
		t.Fatalf("thinking = %+v, want {disabled}", got.Thinking)
	}
}

// THE deepseek-v4 400 TRAP: an assistant history message that omits
// `reasoning_content` is rejected with "Missing `reasoning_content` field", so
// the key is always serialised on an assistant message — as an empty string
// when there is none.
//
// The presence check has to be done on the raw key, not on a decoded string
// field: `""` and an absent key decode identically, so asserting
// `ReasoningContent == ""` would pass even with the key missing.  That is how
// this test originally failed to catch the bug.
func TestBuildChatBodyAlwaysSerialisesReasoningContent(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	raw, err := c.buildChatBody(&core.ChatRequest{
		Model: "deepseek-v4-flash",
		Messages: []core.Message{
			{Role: "assistant", Content: "an earlier answer"},
			{Role: "assistant", Content: "a reasoned answer", Reasoning: "because"},
			{Role: "user", Content: "hi"},
		},
	}, "s")
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}

	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(got.Messages))
	}
	// Both assistant messages must carry the key, the silent one included.
	for i, want := range []string{"", "because"} {
		v, ok := got.Messages[i]["reasoning_content"]
		if !ok {
			t.Fatalf("messages[%d] has no reasoning_content key at all: %v", i, got.Messages[i])
		}
		if s, _ := v.(string); s != want {
			t.Errorf("messages[%d].reasoning_content = %q, want %q", i, v, want)
		}
	}
	// The field is meaningless on a user message, so it is left off there.
	if _, ok := got.Messages[2]["reasoning_content"]; ok {
		t.Errorf("the user message carries reasoning_content: %v", got.Messages[2])
	}
}

func TestBuildChatBodyCarriesTools(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	raw, err := c.buildChatBody(&core.ChatRequest{
		Model:    "m",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Tools: []core.Tool{{
			Name:        "read",
			Description: "read a file",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	}, "s")
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var got struct {
		Tools []wireTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(got.Tools))
	}
	if got.Tools[0].Type != "function" {
		t.Errorf("type = %q, want the function default", got.Tools[0].Type)
	}
	if got.Tools[0].Function.Name != "read" || string(got.Tools[0].Function.Parameters) != `{"type":"object"}` {
		t.Errorf("tool = %+v, want the definition passed through", got.Tools[0])
	}
}

// The caller's own cap wins: a caller that asked for a number gets that
// number, and the per-model budget is only what the gateway fills in when
// nobody asked.
func TestMaxTokensFor(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	if got := c.maxTokensFor(&core.ChatRequest{}); got != defaultMaxTokens {
		t.Errorf("maxTokensFor with no cap = %d, want the default %d", got, defaultMaxTokens)
	}
	small := 128
	if got := c.maxTokensFor(&core.ChatRequest{MaxTokens: &small}); got != 128 {
		t.Errorf("maxTokensFor = %d, want the caller's 128", got)
	}
	zero := 0
	if got := c.maxTokensFor(&core.ChatRequest{MaxTokens: &zero}); got != defaultMaxTokens {
		t.Errorf("maxTokensFor with a zero cap = %d, want the default", got)
	}
	if got := c.maxTokensFor(nil); got != defaultMaxTokens {
		t.Errorf("maxTokensFor(nil) = %d, want the default", got)
	}
}

// ---------------------------------------------------------------------------
// the header-ordering trap, on the wire
// ---------------------------------------------------------------------------

// signedHeaderList parses the SignedHeaders= list out of an Authorization
// header.
func signedHeaderList(t *testing.T, auth string) []string {
	t.Helper()
	const marker = "SignedHeaders="
	i := strings.Index(auth, marker)
	if i < 0 {
		t.Fatalf("Authorization %q has no SignedHeaders", auth)
	}
	rest := auth[i+len(marker):]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	return strings.Split(rest, ";")
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// THE TRAP.  `maas_type: benefit` must be inside the signature or the vendor
// answers InferHub.002002009.404; `Agent-Type` and `X-Language` must be
// appended afterwards or the vendor answers 401 APIG.0301.  This test asserts
// the trap where it actually matters — on the request that goes out.
func TestDoSignedAppliesTheHeaderOrderingTrap(t *testing.T) {
	var (
		auth       string
		sawAgentTy string
		sawLang    string
		sawMaaSTyp string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		sawAgentTy = r.Header.Get("Agent-Type")
		sawLang = r.Header.Get("X-Language")
		sawMaaSTyp = r.Header.Get("maas_type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"}`)
	acct := account{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "SKEXAMPLE", SecurityToken: "TOKEXAMPLE"}

	resp, err := c.doSigned(context.Background(), acct, http.MethodPost, srv.URL+"/api/v2/chat/completions",
		[]byte(`{"a":1}`),
		map[string]string{"maas_type": "benefit"},
		map[string]string{"Agent-Type": "PromptCenter", "X-Language": "zh-cn"})
	if err != nil {
		t.Fatalf("doSigned: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	signed := signedHeaderList(t, auth)
	if !contains(signed, "maas_type") {
		t.Errorf("SignedHeaders = %v, want maas_type inside the signature", signed)
	}
	if contains(signed, "agent-type") || contains(signed, "x-language") {
		t.Errorf("SignedHeaders = %v, want Agent-Type and X-Language OUTSIDE the signature", signed)
	}
	// They are still sent — just after signing.
	if sawAgentTy != "PromptCenter" || sawLang != "zh-cn" {
		t.Errorf("Agent-Type/X-Language = %q/%q, want PromptCenter/zh-cn on the request", sawAgentTy, sawLang)
	}
	if sawMaaSTyp != "benefit" {
		t.Errorf("maas_type = %q, want benefit on the request", sawMaaSTyp)
	}
}

// A non-benefit model must NOT carry the benefit header: the vendor refuses a
// benefit request for a paid model.
func TestDoSignedOmitsTheBenefitHeaderWhenNotAsked(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"}`)
	resp, err := c.doSigned(context.Background(), account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"},
		http.MethodPost, srv.URL+"/x", []byte(`{}`), nil, nil)
	if err != nil {
		t.Fatalf("doSigned: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if contains(signedHeaderList(t, auth), "maas_type") {
		t.Error("maas_type was signed even though no benefit header was asked for")
	}
}

// The signing map's `host` is the request's own host and must never be copied
// onto the request as a header.
func TestDoSignedDoesNotSendAHostHeader(t *testing.T) {
	var (
		gotHost   string
		explicitH string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		// Go folds an explicit Host header into r.Host and removes it from
		// the header map, so an empty r.Header["Host"] is what proves the
		// module did not set one of its own.
		explicitH = r.Header.Get("Host")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"A","secret_access_key":"S","security_token":"T"}`)
	resp, err := c.doSigned(context.Background(), account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"},
		http.MethodGet, srv.URL+"/x", nil, nil, nil)
	if err != nil {
		t.Fatalf("doSigned: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	wantHost := strings.TrimPrefix(srv.URL, "http://")
	if gotHost != wantHost {
		t.Errorf("Host = %q, want the server's own address %q", gotHost, wantHost)
	}
	if explicitH != "" {
		t.Errorf("an explicit Host header was sent: %q", explicitH)
	}
}

// A GET carries no content-type, signed or otherwise.
func TestDoSignedGetHasNoContentType(t *testing.T) {
	var ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"A","secret_access_key":"S","security_token":"T"}`)
	resp, err := c.doSigned(context.Background(), account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"},
		http.MethodGet, srv.URL+"/x", nil, nil, nil)
	if err != nil {
		t.Fatalf("doSigned: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if ct != "" {
		t.Errorf("Content-Type = %q on a GET, want none", ct)
	}
}

func TestDoJSONClassifiesAFailingResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error_msg":"too many requests"}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"A","secret_access_key":"S","security_token":"T"}`)
	var out map[string]any
	err := c.doJSON(context.Background(), account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"},
		http.MethodGet, srv.URL+"/x", nil, nil, nil, &out)
	if err == nil {
		t.Fatal("a 429 produced no error")
	}
	ue, ok := asUpstreamError(err)
	if !ok {
		t.Fatalf("err = %T, want *upstreamError", err)
	}
	if ue.Kind != kindQuota {
		t.Errorf("kind = %v, want kindQuota", ue.Kind)
	}
	if ue.RetryAfter < 11*time.Second || ue.RetryAfter > 13*time.Second {
		t.Errorf("RetryAfter = %v, want about 12s from the header", ue.RetryAfter)
	}
}

func TestDoJSONRejectsANonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{"base_url":`+jsonString(srv.URL)+`,"access_key_id":"A","secret_access_key":"S","security_token":"T"}`)
	var out map[string]any
	err := c.doJSON(context.Background(), account{AccessKeyID: "A", SecretAccessKey: "S", SecurityToken: "T"},
		http.MethodGet, srv.URL+"/x", nil, nil, nil, &out)
	if err == nil {
		t.Fatal("a non-JSON body produced no error")
	}
	if !strings.Contains(err.Error(), "not the JSON") {
		t.Errorf("err = %q, want the decode failure named", err.Error())
	}
}
