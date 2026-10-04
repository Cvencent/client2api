package trae

// trae_test.go — offline tests.  Nothing here touches the network: transport is
// a fake http.RoundTripper, credentials live in t.TempDir(), and the SSE
// fixtures are inline string literals recorded from the upstream.
//
// Live checks live behind CLIENT2API_LIVE=1 in live_test.go.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---- helpers --------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// testClient builds a Client wired to a fake transport, bypassing New() so no
// discovery happens.
func testClient(t *testing.T, cfg *Config, auths []*Auth, rt http.RoundTripper) *Client {
	t.Helper()
	if cfg == nil {
		cfg = loadConfig(nil, nil)
	}
	hc := &http.Client{Transport: rt}
	c := &Client{
		name:         clientName,
		cfg:          cfg,
		now:          time.Now,
		log:          func(string, ...any) {},
		httpClient:   hc,
		streamClient: hc,
	}
	c.pool = NewPool(auths)
	return c
}

func testAuth(id, token string) *Auth {
	return &Auth{
		UserID:      id,
		AccessToken: token,
		ExpiresAt:   time.Now().Add(time.Hour),
		MachineID:   strings.Repeat("a", 64),
		DeviceID:    "2235771921399404",
		Host:        "https://api.trae.cn",
	}
}

func unmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, b)
	}
	return m
}

// ---- payload rewriting ----------------------------------------------------

func TestBuildBodyAppliesEveryRule(t *testing.T) {
	temp := 0.7
	maxTok := 128
	req := &core.ChatRequest{
		Model: "glm-5.2",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []core.ToolCall{
				{ID: "call_1", Type: "function", Name: "get_weather", Arguments: `{"city":"SF"}`},
			}},
		},
		Tools: []core.Tool{{
			Type:        "function",
			Name:        "get_weather",
			Description: "look up weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		ToolChoice:  json.RawMessage(`{"type":"function","function":{"name":"get_weather"}}`),
		Temperature: &temp,
		MaxTokens:   &maxTok,
		Stop:        []string{"END"},
		Stream:      false, // must be forced true upstream
	}

	body, err := BuildBody(req, loadConfig(nil, nil).payloadOptions())
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}
	obj := unmarshal(t, body)

	if obj["stream"] != true {
		t.Errorf("stream must be forced true, got %v", obj["stream"])
	}
	if obj["function"] != defaultFunction {
		t.Errorf("function must be forced to %q, got %v", defaultFunction, obj["function"])
	}
	if obj["model"] != "glm-5.2" || obj["config_name"] != "glm-5.2" {
		t.Errorf("model must be written into both fields, got model=%v config_name=%v", obj["model"], obj["config_name"])
	}

	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages shape: %#v", obj["messages"])
	}
	m0 := msgs[0].(map[string]any)
	parts, ok := m0["content"].([]any)
	if !ok {
		t.Fatalf("content must be an array, got %T (%v)", m0["content"], m0["content"])
	}
	if p0 := parts[0].(map[string]any); p0["type"] != "text" || p0["text"] != "hi" {
		t.Errorf("content part = %#v", p0)
	}

	m1 := msgs[1].(map[string]any)
	calls, ok := m1["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %#v", m1["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if _, present := call["function"]; present {
		t.Error("assistant tool_calls[].function must be renamed to function_call")
	}
	fc, ok := call["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("function_call missing: %#v", call)
	}
	if fc["name"] != "get_weather" {
		t.Errorf("function_call.name = %v", fc["name"])
	}

	tools := obj["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if _, isString := fn["parameters"].(string); !isString {
		t.Errorf("tools[].function.parameters must be a JSON string, got %T", fn["parameters"])
	}
	if obj["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice should be the function name, got %v", obj["tool_choice"])
	}
	if obj["max_tokens"].(float64) != 128 {
		t.Errorf("max_tokens = %v", obj["max_tokens"])
	}
	if obj["temperature"].(float64) != 0.7 {
		t.Errorf("temperature = %v", obj["temperature"])
	}
}

func TestPrepareBodyFixtures(t *testing.T) {
	opts := payloadOptions{Function: defaultFunction, Model: defaultModel}
	cases := []struct {
		name  string
		in    string
		check func(t *testing.T, out map[string]any)
	}{
		{
			name: "content string becomes an array",
			in:   `{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`,
			check: func(t *testing.T, out map[string]any) {
				m := out["messages"].([]any)[0].(map[string]any)
				arr, ok := m["content"].([]any)
				if !ok || len(arr) != 1 {
					t.Fatalf("content = %#v", m["content"])
				}
				if arr[0].(map[string]any)["text"] != "hello" {
					t.Errorf("part = %#v", arr[0])
				}
			},
		},
		{
			name: "content array passes through untouched",
			in:   `{"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`,
			check: func(t *testing.T, out map[string]any) {
				arr := out["messages"].([]any)[0].(map[string]any)["content"].([]any)
				if len(arr) != 2 {
					t.Fatalf("content = %#v", arr)
				}
			},
		},
		{
			name: "empty model falls back to the configured default in both fields",
			in:   `{"messages":[{"role":"user","content":"x"}]}`,
			check: func(t *testing.T, out map[string]any) {
				if out["model"] != defaultModel || out["config_name"] != defaultModel {
					t.Errorf("model=%v config_name=%v", out["model"], out["config_name"])
				}
			},
		},
		{
			name: "tool parameters object becomes a JSON string",
			in:   `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","properties":{"n":{"type":"number"}}}}}]}`,
			check: func(t *testing.T, out map[string]any) {
				fn := out["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
				s, ok := fn["parameters"].(string)
				if !ok {
					t.Fatalf("parameters type = %T", fn["parameters"])
				}
				var back map[string]any
				if err := json.Unmarshal([]byte(s), &back); err != nil {
					t.Fatalf("parameters is not valid JSON: %v", err)
				}
				if back["type"] != "object" {
					t.Errorf("parameters round-trip = %#v", back)
				}
			},
		},
		{
			name: "tool_choice none drops tools",
			in:   `{"messages":[{"role":"user","content":"x"}],"tool_choice":"none","tools":[{"type":"function","function":{"name":"f"}}]}`,
			check: func(t *testing.T, out map[string]any) {
				if _, present := out["tool_choice"]; present {
					t.Error("tool_choice should be removed")
				}
				if _, present := out["tools"]; present {
					t.Error("tools should be removed when tool_choice is none")
				}
			},
		},
		{
			name: "tool_choice auto is normalised to the scalar form",
			in:   `{"messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"auto"}}`,
			check: func(t *testing.T, out map[string]any) {
				if out["tool_choice"] != "auto" {
					t.Errorf("tool_choice = %#v", out["tool_choice"])
				}
			},
		},
		{
			name: "assistant call without a name is dropped",
			in:   `{"messages":[{"role":"assistant","content":"x","tool_calls":[{"id":"1","function":{"name":"","arguments":"{}"}}]}]}`,
			check: func(t *testing.T, out map[string]any) {
				m := out["messages"].([]any)[0].(map[string]any)
				if _, present := m["tool_calls"]; present {
					t.Errorf("nameless tool_calls must be dropped: %#v", m["tool_calls"])
				}
			},
		},
		{
			name: "stream is forced true and function forced",
			in:   `{"stream":false,"function":"chat_v3","messages":[{"role":"user","content":"x"}]}`,
			check: func(t *testing.T, out map[string]any) {
				if out["stream"] != true {
					t.Errorf("stream = %v", out["stream"])
				}
				if out["function"] != defaultFunction {
					t.Errorf("function = %v", out["function"])
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := PrepareBodyBytes([]byte(tc.in), opts)
			if err != nil {
				t.Fatalf("PrepareBodyBytes: %v", err)
			}
			tc.check(t, unmarshal(t, out))
		})
	}
}

func TestPrepareBodyExtraBodyDoesNotOverride(t *testing.T) {
	obj := map[string]any{"model": "glm-5.2", "messages": []any{}}
	PrepareBody(obj, payloadOptions{
		Function: defaultFunction,
		Model:    defaultModel,
		Extra:    map[string]any{"model": "hijack", "presence_penalty": 0.5},
	})
	if obj["model"] != "glm-5.2" {
		t.Errorf("extra_body must not override a set field, got %v", obj["model"])
	}
	if obj["presence_penalty"] != 0.5 {
		t.Errorf("extra_body should add new fields, got %v", obj["presence_penalty"])
	}
}

// ---- SSE parsing ----------------------------------------------------------

const soloStreamFixture = "id:1\nevent:metadata\ndata:{\"model\":\"\",\"session_id\":\"abc\",\"prompt_completion_id\":0}\n\n" +
	"id:2\nevent:timing_cost\ndata:{\"name\":\"llm_raw_chat_v2\"}\n\n" +
	"event:output\ndata:{\"response\":\"Hello\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
	"event:output\ndata:{\"response\":\" world\",\"reasoning_content\":\"thinking\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":21,\"completion_tokens\":142,\"total_tokens\":163,\"reasoning_tokens\":135}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func TestSSEScannerFrames(t *testing.T) {
	sc := newSSEScanner(strings.NewReader(soloStreamFixture))
	var got []string
	for {
		ev, err := sc.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, ev.Event)
	}
	want := []string{"metadata", "timing_cost", "output", "output", "token_usage", "done"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("frames = %v, want %v", got, want)
	}
}

func TestSSEScannerHandlesCommentsAndMultilineData(t *testing.T) {
	// ": keep-alive" is a comment; two data: lines in one frame are concatenated.
	body := ": keep-alive\n\nevent:output\ndata:{\"response\":\ndata:\"hi\"}\n\n"
	sc := newSSEScanner(strings.NewReader(body))
	ev, err := sc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "output" || ev.Response != "hi" {
		t.Errorf("frame = %#v", ev)
	}
	if _, err := sc.Next(); err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestSSEScannerRejectsOversizedFrame(t *testing.T) {
	// An upstream that keeps sending data: lines without ever closing the
	// frame must not be able to grow the buffer without bound.
	huge := "event:output\n" + "data:" + strings.Repeat("x", maxFrameBytes+16) + "\n"
	sc := newSSEScanner(strings.NewReader(huge))
	if _, err := sc.Next(); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("Next err = %v, want errFrameTooLarge", err)
	}
	// The scanner stays failed: the oversize verdict is reported once and every
	// later call agrees, so a caller looping on Next cannot slip past it.
	if _, err := sc.Next(); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("second Next err = %v, want errFrameTooLarge", err)
	}
}

func TestSSEScannerAllowsFrameUnderTheLimit(t *testing.T) {
	// One byte under the cap must still parse, so the guard is a ceiling and
	// not a regression on large-but-legitimate tool-call payloads.
	payload := strings.Repeat("y", maxFrameBytes-64)
	body := "event:output\ndata:{\"response\":\"" + payload + "\"}\n\n"
	sc := newSSEScanner(strings.NewReader(body))
	ev, err := sc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "output" || len(ev.Response) != len(payload) {
		t.Fatalf("frame event=%q len=%d, want output/%d", ev.Event, len(ev.Response), len(payload))
	}
}

func TestStreamMapsFramesToCoreEvents(t *testing.T) {
	a := testAuth("u1", "tok")
	c := testClient(t, nil, []*Auth{a}, nil)
	st := newStream(c, a, io.NopCloser(strings.NewReader(soloStreamFixture)))

	var deltas []string
	var reasoning string
	var usage *core.Usage
	var finish string
	for {
		ev, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			deltas = append(deltas, ev.Delta)
			reasoning += ev.Reasoning
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			finish = ev.Finish
		}
	}
	if strings.Join(deltas, "") != "Hello world" {
		t.Errorf("deltas = %v", deltas)
	}
	if reasoning != "thinking" {
		t.Errorf("reasoning = %q", reasoning)
	}
	if usage == nil || usage.PromptTokens != 21 || usage.CompletionTokens != 142 || usage.TotalTokens != 163 || usage.ReasoningTokens != 135 {
		t.Errorf("usage = %#v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}

	// Recv must keep returning io.EOF once the stream is finished.
	if _, err := st.Recv(); err != io.EOF {
		t.Errorf("second post-EOF Recv = %v", err)
	}
	// Close must be idempotent.
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStreamSynthesisesDoneWhenUpstreamEnds(t *testing.T) {
	body := "event:output\ndata:{\"response\":\"partial\"}\n\n"
	a := testAuth("u1", "tok")
	c := testClient(t, nil, []*Auth{a}, nil)
	st := newStream(c, a, io.NopCloser(strings.NewReader(body)))

	ev, err := st.Recv()
	if err != nil || ev.Type != core.EventDelta || ev.Delta != "partial" {
		t.Fatalf("first event = %#v, %v", ev, err)
	}
	ev, err = st.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != core.EventDone || ev.Finish != "stop" {
		t.Errorf("synthetic done = %#v", ev)
	}
}

// TestErrorInsideHTTP200 is the important one: a business failure arrives as
// event:error inside an otherwise healthy HTTP 200 SSE stream.
func TestErrorInsideHTTP200(t *testing.T) {
	body := "event:metadata\ndata:{\"session_id\":\"abc\"}\n\n" +
		"event:error\ndata:{\"code\":1005,\"message\":\"plan limit reached\"}\n\n"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return sseResponse(body), nil })
	a := testAuth("u1", "tok")
	c := testClient(t, nil, []*Auth{a}, rt)

	st, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	ev, err := st.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if ev.Type != core.EventError {
		t.Fatalf("event = %#v", ev)
	}
	var te *Error
	if !errors.As(ev.Err, &te) {
		t.Fatalf("error type = %T", ev.Err)
	}
	if te.Kind != ErrPlanLimit || te.Code != 1005 || te.Status != 200 {
		t.Errorf("classified error = %#v", te)
	}

	snap := c.pool.Snapshot()
	if len(snap) != 1 || snap[0].State != stateExhausted {
		t.Errorf("1005 must park the account as exhausted, got %#v", snap)
	}
}

// ---- error classification / failover set ----------------------------------

func TestClassifyAndFailoverSet(t *testing.T) {
	statusCases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan_limit"}`, ErrPlanLimit},
		{200, `{"code":4008,"message":"quota"}`, ErrQuota},
		{200, `{"code":1001,"message":"auth"}`, ErrAuth},
		{200, `{"code":4010,"message":"token"}`, ErrAuth},
		{200, `{"code":4001,"message":"cannot unmarshal string into LLMRawMessageContent"}`, ErrParam},
		{200, `{"code":4011,"message":"rate"}`, ErrSoftRate},
		{200, `{"code":9074,"message":"checkin"}`, ErrRetryLater},
		{401, `{"message":"session dead"}`, ErrSessionDead},
		{429, ``, ErrSoftRate},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{502, ``, ErrServer},
		{400, `bad request`, ErrClient},
		{200, `{"ok":true}`, ErrNone},
	}
	for _, tc := range statusCases {
		if got := Classify(tc.status, []byte(tc.body)); got != tc.want {
			t.Errorf("Classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}

	// The failover set is exactly {4008, 1001, 4010}.
	for _, code := range []int64{4008, 1001, 4010} {
		if !FailoverCode(code) {
			t.Errorf("FailoverCode(%d) must be true", code)
		}
	}
	for _, code := range []int64{1005, 4001, 4011, 9074, 0} {
		if FailoverCode(code) {
			t.Errorf("FailoverCode(%d) must be false", code)
		}
	}

	// 1005 must never be failover, and must never be retryable across accounts.
	if retryableKind(ClassifyCode(1005)) {
		t.Error("plan_limit must not be retryable across accounts")
	}
	if !retryableKind(ClassifyCode(4008)) {
		t.Error("quota must be retryable across accounts")
	}
	if !retryableKind(ClassifyCode(1001)) {
		t.Error("auth must be retryable across accounts")
	}
	if retryableKind(ClassifyCode(4001)) {
		t.Error("a parameter error must not be retried")
	}
}

func TestCooldownFor(t *testing.T) {
	cases := []struct {
		kind  ErrKind
		want  time.Duration
		state string
	}{
		{ErrPlanLimit, planLimitCooldown, stateExhausted},
		{ErrQuota, quotaCooldown, stateExhausted},
		{ErrAuth, authCooldown, stateCooling},
		{ErrSoftRate, softRateCooldown, stateCooling},
		{ErrRetryLater, retryLaterCooldown, stateCooling},
		{ErrNotFound, notFoundCooldown, stateCooling},
		{ErrServer, serverCooldown, stateCooling},
		{ErrClient, clientCooldown, stateCooling},
		{ErrSessionDead, sessionDeadCooldown, stateInvalid},
		{ErrNone, 0, stateReady},
	}
	for _, tc := range cases {
		got, state := cooldownFor(tc.kind)
		if got != tc.want || state != tc.state {
			t.Errorf("cooldownFor(%v) = (%v, %q), want (%v, %q)", tc.kind, got, state, tc.want, tc.state)
		}
	}
}

func TestPoolRoundRobinAndCooldown(t *testing.T) {
	a := testAuth("A", "ta")
	b := testAuth("B", "tb")
	p := NewPool([]*Auth{a, b})
	now := time.Now()
	p.now = func() time.Time { return now }

	first, ok := p.Pick(nil)
	if !ok {
		t.Fatal("Pick returned nothing")
	}
	second, _ := p.Pick(nil)
	if first.ID() == second.ID() {
		t.Error("round-robin should alternate accounts")
	}

	kind, cd := p.MarkFailure(first, &Error{Kind: ErrQuota, Status: 200})
	if kind != ErrQuota || cd != quotaCooldown {
		t.Errorf("MarkFailure = (%v, %v)", kind, cd)
	}
	// The failed account is skipped while cooling.
	for i := 0; i < 4; i++ {
		got, ok := p.Pick(nil)
		if !ok {
			t.Fatal("expected the healthy account to be picked")
		}
		if got.ID() == first.ID() {
			t.Errorf("account %s is cooling but was picked", first.ID())
		}
	}
	// After the cooldown it comes back.
	now = now.Add(quotaCooldown + time.Second)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		got, ok := p.Pick(nil)
		if !ok {
			t.Fatal("Pick failed after cooldown")
		}
		seen[got.ID()] = true
	}
	if !seen[first.ID()] {
		t.Error("account should be selectable again after its cooldown")
	}
}

func TestPoolReplacePreservesHealth(t *testing.T) {
	a := testAuth("A", "ta")
	p := NewPool([]*Auth{a})
	p.MarkFailure(a, &Error{Kind: ErrPlanLimit})
	if p.Ready() {
		t.Error("an exhausted pool must not be ready")
	}
	// Re-discovering the same account must not resurrect it.
	p.Replace([]*Auth{testAuth("A", "ta")})
	if p.Ready() {
		t.Error("Replace must preserve the exhausted state of a surviving account")
	}
}

func TestPoolSnapshotRendersCredits(t *testing.T) {
	a := testAuth("A", "ta")
	a.SetCredits(120, 7, "credits")
	p := NewPool([]*Auth{a})
	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %#v", snap)
	}
	if snap[0].Extra["ide_credits"] != int64(120) || snap[0].Extra["work_credits"] != int64(7) {
		t.Errorf("credits = %#v", snap[0].Extra)
	}
	if snap[0].Extra["billing_mode"] != "credits" {
		t.Errorf("billing_mode = %#v", snap[0].Extra)
	}
}

// ---- failover -------------------------------------------------------------

func TestChatFailsOverOnAuthError(t *testing.T) {
	a := testAuth("A", "token-A")
	b := testAuth("B", "token-B")
	var seen []string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		auth := r.Header.Get("Authorization")
		seen = append(seen, auth)
		if strings.Contains(auth, "token-A") {
			return jsonResponse(401, `{"code":1001,"message":"auth failed"}`), nil
		}
		return sseResponse("event:output\ndata:{\"response\":\"ok\"}\n\nevent:done\ndata:{\"finish_reason\":\"stop\"}\n\n"), nil
	})
	c := testClient(t, nil, []*Auth{a, b}, rt)

	st, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat should have failed over, got %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected two upstream attempts, saw %v", seen)
	}
	ev, err := st.Recv()
	if err != nil || ev.Type != core.EventDelta || ev.Delta != "ok" {
		t.Fatalf("first event = %#v, %v", ev, err)
	}

	snap := c.pool.Snapshot()
	states := map[string]string{}
	for _, s := range snap {
		states[s.ID] = s.State
	}
	if states["A"] != stateCooling {
		t.Errorf("account A should be cooling after 1001, got %q", states["A"])
	}
	if states["B"] != stateReady {
		t.Errorf("account B should be ready, got %q", states["B"])
	}
}

func TestChatDoesNotFailOverOnPlanLimit(t *testing.T) {
	a := testAuth("A", "token-A")
	b := testAuth("B", "token-B")
	calls := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return jsonResponse(403, `{"code":1005,"message":"plan_limit"}`), nil
	})
	c := testClient(t, nil, []*Auth{a, b}, rt)

	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("a plan limit must not trigger a second attempt, saw %d calls", calls)
	}
}

func TestChatWithoutAccountsIsNotConfigured(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Errorf("err = %v, want core.ErrNotConfigured", err)
	}
}

// ---- models ---------------------------------------------------------------

func TestModelsOfflineFallback(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the static catalogue must never be empty")
	}
	for _, m := range models {
		if m.OwnedBy != clientName {
			t.Errorf("model %q OwnedBy = %q", m.ID, m.OwnedBy)
		}
	}
}

func TestModelsLiveThenCached(t *testing.T) {
	calls := 0
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.URL.Path, "get_detail_param") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		return jsonResponse(200, `{"config_info_list":[{"config_name":"glm-5.2","display_config":{"display_name":"GLM 5.2"}},{"config_name":"kimi-k3"}]}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", "t")}, rt)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "glm-5.2" {
		t.Fatalf("models = %#v", models)
	}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if calls != 1 {
		t.Errorf("the catalogue should be cached, saw %d calls", calls)
	}
}

// ---- RefreshModels (the panel's "re-fetch from upstream" button) ----------

func TestRefreshModelsUpdatesCatalogueAndBypassesCache(t *testing.T) {
	calls := 0
	catalogue := `{"config_info_list":[{"config_name":"glm-5.2"}]}`
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.URL.Path, "get_detail_param") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		return jsonResponse(200, catalogue), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", "t")}, rt)

	first, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(first) != 1 || first[0].ID != "glm-5.2" {
		t.Fatalf("Models = %#v", first)
	}
	// A second Models() inside the TTL must not touch the network.
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("second Models: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Models should be cached, saw %d upstream calls", calls)
	}

	// The vendor changes its catalogue.  RefreshModels has to see the new
	// list even though the cache is still warm.
	catalogue = `{"config_info_list":[{"config_name":"kimi-k3"},{"config_name":"glm-5"}]}`
	refreshed, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if calls != 2 {
		t.Fatalf("RefreshModels must bypass the cache, saw %d upstream calls", calls)
	}
	if len(refreshed) != 2 || refreshed[0].ID != "kimi-k3" {
		t.Fatalf("refreshed = %#v", refreshed)
	}

	// The refreshed list is what Models() now serves, and it is cached again.
	again, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models after refresh: %v", err)
	}
	if len(again) != 2 || again[0].ID != "kimi-k3" {
		t.Fatalf("Models after refresh = %#v", again)
	}
	if calls != 2 {
		t.Errorf("the refreshed list should be cached, saw %d upstream calls", calls)
	}
}

func TestRefreshModelsFailureKeepsLastGoodList(t *testing.T) {
	failing := false
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		if failing {
			return jsonResponse(503, `{"message":"upstream is down"}`), nil
		}
		return jsonResponse(200, `{"config_info_list":[{"config_name":"glm-5.2"}]}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", "t")}, rt)

	good, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(good) != 1 {
		t.Fatalf("good = %#v", good)
	}

	failing = true
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report an error")
	}
	// A failed refresh must never empty the catalogue.
	if len(models) != 1 || models[0].ID != "glm-5.2" {
		t.Fatalf("a failed refresh must keep the last good list, got %#v", models)
	}
}

func TestRefreshModelsWithoutAccountStillServesStaticList(t *testing.T) {
	c := testClient(t, nil, nil, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no account: the transport must not be used")
		return jsonResponse(200, `{}`), nil
	}))

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("refreshing without an account must report an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	for _, m := range models {
		if m.OwnedBy != clientName {
			t.Errorf("model %q OwnedBy = %q", m.ID, m.OwnedBy)
		}
	}
}

func TestRefreshModelsErrorOmitsToken(t *testing.T) {
	// A bare token has no label for core.Redact to key on, so this also pins
	// the explicit replacement in scrubSecret.
	const token = "TRAE-BARE-TOKEN-abcdefghijklmnop"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"message":"invalid credential `+token+`"}`), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", token)}, rt)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	if !strings.HasPrefix(err.Error(), clientName+":") {
		t.Errorf("the error should be prefixed with the module name: %v", err)
	}
}

// ---- credential discovery / tc container ----------------------------------

// tcFixture is a recorded tc container: the plaintext is the auth blob below,
// sealed with the same salt tables the desktop app uses.  It pins the
// decryptor's algorithm (a change to the key schedule breaks this test).
const (
	tcFixture = "dGMFEAAAAwoRGB8mLTQ7QklQV15lbHN6gYiPlp2kq7K5wMfO1dwlz0vaMO0l8CjRxHuiieGfXs3NFZX/R9BVrD2L05QE9BwLlu3m3SkneXNrTHuqUY+c73yUoDQn2XAo+We2pKyipnXwaiGBwYLcE0q5LUZ0tfgDLFRdL3VWQOyFzc4meLQW+AN89fXIxPPs5L362zxXMcJn3L0QQSAJ+j1NKHmqQLMISh4eha2fqrcC1vPi0bjNYDfr4L9fCqtz8/wGgfH7hHWfaSEZFMIoSNuEoGP+Z+wzd2H4CSLXRgiVYYduxB3vE1xJkrQozL8A5GT9atE8qD7XyysONo1NIeTWA6YLWV8haa6NQx+OVvC0Il0HSc7Np3bqKT2g6Opu2fmxtuN7AQUzrRWEqyhXLtO2rmwxQnjRxsGSR9pjAEmc5lqZePB+ZZKMajkzc02JWmy8t+WXs37zsSjPR9MPlSds6NSPtSCqm6HMOeXPlZJFP44STQlukRjapGjgWX7zI7rkPzxN3oC5zlsVV9nF9qF/xQbAqvTM0h2ru8mcqQ+28lm/Nwknm9Y3XlpxGvUU4vimpTEwpqO4QVYv0/+H5cboEzKrgVpCSY1uyGm6xLV52HuHPxM="
	tcPlain   = `{"token":"FIXTURE_ACCESS_TOKEN_0001","refreshToken":"FIXTURE_REFRESH_TOKEN_0001","expiredAt":"2026-10-03T18:33:21.390Z","refreshExpiredAt":"2027-03-18T18:33:21.390Z","host":"https://api.trae.cn","userId":"3595881099822378","userRegion":{"region":"CN","_aiRegion":"CN"},"account":{"username":"fixture-user","email":"fixture@example.com","scope":"marscode"}}`
)

func TestTCDecryptorAgainstRecordedFixture(t *testing.T) {
	plain, err := DecryptTCBase64(tcFixture)
	if err != nil {
		t.Fatalf("DecryptTCBase64: %v", err)
	}
	if string(plain) != tcPlain {
		t.Fatalf("plaintext mismatch:\n got %s\nwant %s", plain, tcPlain)
	}
}

func TestTCDecryptorRejectsGarbage(t *testing.T) {
	if _, err := DecryptTC([]byte("not a container")); !errors.Is(err, ErrNotTCContainer) {
		t.Errorf("err = %v, want ErrNotTCContainer", err)
	}
	// A tampered container must fail the integrity check rather than return
	// garbage.
	raw, err := base64.StdEncoding.DecodeString(tcFixture)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if _, err := DecryptTC(raw); !errors.Is(err, ErrTCHashMismatch) {
		t.Errorf("err = %v, want ErrTCHashMismatch", err)
	}
}

func TestLoadAuthFromStorage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	store := map[string]any{
		storageKeyAuth:                              tcFixture,
		"telemetry.machineId":                       strings.Repeat("a", 64),
		"telemetry.devDeviceId":                     "14fa1417-379e-407e-8abb-db063f76669d",
		"iCubeAuthInfo://icube-dc:2235771921399404": "e30=",
	}
	raw, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig(nil, nil)
	a, err := loadAuthFromStorage(path, cfg)
	if err != nil {
		t.Fatalf("loadAuthFromStorage: %v", err)
	}
	if a.Token() != "FIXTURE_ACCESS_TOKEN_0001" {
		t.Errorf("token = %q", a.Token())
	}
	if a.RefreshTokenValue() != "FIXTURE_REFRESH_TOKEN_0001" {
		t.Errorf("refresh token = %q", a.RefreshTokenValue())
	}
	if a.UserID != "3595881099822378" {
		t.Errorf("user id = %q", a.UserID)
	}
	if a.DeviceID != "2235771921399404" {
		t.Errorf("device id = %q", a.DeviceID)
	}
	if a.MachineID != strings.Repeat("a", 64) {
		t.Errorf("machine id = %q", a.MachineID)
	}
	if a.Label() != "fixture-user" {
		t.Errorf("label = %q", a.Label())
	}
	if a.Expiry().IsZero() {
		t.Error("expiry should be parsed from expiredAt")
	}
	wantExpiry, err := time.Parse(time.RFC3339, "2026-10-03T18:33:21.390Z")
	if err != nil {
		t.Fatalf("parse fixture expiry: %v", err)
	}
	if !a.Expiry().Equal(wantExpiry) {
		t.Errorf("expiry = %s, want %s", a.Expiry().UTC().Format(time.RFC3339), wantExpiry.UTC().Format(time.RFC3339))
	}
	if got, want := a.Expired(), !time.Now().Before(wantExpiry); got != want {
		t.Errorf("Expired() = %v, want %v for an expiry of %s", got, want, wantExpiry.UTC().Format(time.RFC3339))
	}
}

func TestLoadAuthFromStorageWithoutCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAuthFromStorage(path, loadConfig(nil, nil)); !errors.Is(err, errNoCredential) {
		t.Errorf("err = %v, want errNoCredential", err)
	}
}

func TestExtractSoloDeviceID(t *testing.T) {
	store := map[string]json.RawMessage{
		"iCubeAuthInfo://icube-dc:2235771921399404": json.RawMessage(`"x"`),
		"iCubeAuthInfo://usertag":                   json.RawMessage(`"y"`),
	}
	if got := extractSoloDeviceID(store); got != "2235771921399404" {
		t.Errorf("device id = %q", got)
	}
	if got := extractSoloDeviceID(map[string]json.RawMessage{"a": json.RawMessage(`"b"`)}); got != "" {
		t.Errorf("device id = %q, want empty", got)
	}
}

func TestHashDeviceID(t *testing.T) {
	got := hashDeviceID("cc42f8b3ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if len(got) != 19 {
		t.Errorf("hashDeviceID length = %d, want 19", len(got))
	}
	for _, r := range got {
		if r < '0' || r > '9' {
			t.Fatalf("hashDeviceID = %q, not all digits", got)
		}
	}
	if hashDeviceID("") != "" {
		t.Error("hashDeviceID(\"\") should be empty")
	}
	if hashDeviceID("abc") != hashDeviceID("abc") {
		t.Error("hashDeviceID must be deterministic")
	}
}

func TestDiscoverAuthsUsesExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "storage.json")
	store := map[string]any{storageKeyAuth: tcFixture, "telemetry.machineId": "m"}
	raw, _ := json.Marshal(store)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(nil, nil)
	cfg.StoragePath = path
	auths := discoverAuths(cfg, nil)
	if len(auths) != 1 {
		t.Fatalf("auths = %d, want 1", len(auths))
	}
	if auths[0].Product != "config" && auths[0].Source != path {
		t.Errorf("source = %q", auths[0].Source)
	}
}

// ---- config ---------------------------------------------------------------

func TestLoadConfigDefaultsAndDegradation(t *testing.T) {
	if cfg := loadConfig(nil, nil); cfg.ideVersion() != defaultIdeVersion || cfg.functionName() != defaultFunction || cfg.defaultModel() != defaultModel {
		t.Errorf("defaults = %q %q %q", cfg.ideVersion(), cfg.functionName(), cfg.defaultModel())
	}
	if cfg := loadConfig(json.RawMessage(`{"ide_version":"3.3.67","self_renew":true}`), nil); cfg.ideVersion() != "3.3.67" || !cfg.selfRenew() {
		t.Errorf("override not applied: %q %v", cfg.ideVersion(), cfg.selfRenew())
	}
	// Malformed JSON degrades to defaults instead of failing the module.
	cfg := loadConfig(json.RawMessage(`{not json`), nil)
	if cfg.ideVersion() != defaultIdeVersion {
		t.Errorf("malformed config should fall back to defaults, got %q", cfg.ideVersion())
	}
	if cfg.selfRenew() {
		t.Error("self_renew must default to off")
	}
	if cfg.autoDiscover() != true {
		t.Error("auto_discover must default to on")
	}
}

func TestHeaderVersionsAreConfigurable(t *testing.T) {
	cfg := loadConfig(json.RawMessage(`{
		"ide_version":"3.3.67",
		"ide_version_code":"20260401",
		"app_version":"3.3.67",
		"device_brand":"82RF",
		"os_version":"Windows 10",
		"user_agent":"Trae/3.3.67"
	}`), nil)
	a := testAuth("u", "tok")
	c := testClient(t, cfg, []*Auth{a}, nil)
	h := c.soloHeaders(a, true)

	want := map[string]string{
		"X-Ide-Version":      "3.3.67",
		"X-Ide-Version-Code": "20260401",
		"X-App-Version-Code": "20260401",
		"X-App-Version":      "3.3.67",
		"X-Device-Brand":     "82RF",
		"X-OS-Version":       "Windows 10",
		"User-Agent":         "Trae/3.3.67",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// The identity headers must be present on a stream request.
	for _, k := range []string{"Authorization", "X-Cloudide-Token", "X-Ide-Token", "X-Uid", "X-App-Id", "X-Machine-Id", "X-Device-Id", "X-Request-ID", "X-Trae-Request-ID", "Accept"} {
		if h.Get(k) == "" {
			t.Errorf("header %s missing", k)
		}
	}
	if !strings.HasPrefix(h.Get("Authorization"), "Cloud-IDE-JWT ") {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
}

// TestTraceHeadersMatchTheReferenceShape pins the two trace headers the Node
// reference sends on every API request (`trae2api/src/auth.js:1179-1181`).
// x-flow-traceparent deliberately keeps the reference's literal "04" version
// field instead of a canonical W3C "00", so this test would fail if someone
// "fixed" it.
func TestTraceHeadersMatchTheReferenceShape(t *testing.T) {
	a := testAuth("u", "tok")
	c := testClient(t, loadConfig(nil, nil), []*Auth{a}, nil)

	isHex := func(s string, n int) bool {
		if len(s) != n {
			return false
		}
		for i := 0; i < len(s); i++ {
			ch := s[i]
			if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
				return false
			}
		}
		return true
	}

	for _, stream := range []bool{true, false} {
		h := c.soloHeaders(a, stream)
		trace := h.Get("X-Custom-Trace-Id")
		if !isHex(trace, 32) {
			t.Errorf("stream=%v X-Custom-Trace-Id = %q, want 32 lowercase hex", stream, trace)
		}
		tp := h.Get("X-Flow-Traceparent")
		const wantLen = len("04-") + 32 + len("-") + 16 + len("-01")
		prefix := "04-" + trace + "-"
		if len(tp) != wantLen || !strings.HasPrefix(tp, prefix) || !strings.HasSuffix(tp, "-01") {
			t.Fatalf("stream=%v X-Flow-Traceparent = %q, want 04-%s-<16 hex>-01", stream, tp, trace)
		}
		span := strings.TrimSuffix(strings.TrimPrefix(tp, prefix), "-01")
		if !isHex(span, 16) {
			t.Errorf("stream=%v span = %q, want 16 lowercase hex", stream, span)
		}
	}

	first := c.soloHeaders(a, true).Get("X-Custom-Trace-Id")
	second := c.soloHeaders(a, true).Get("X-Custom-Trace-Id")
	if first == second {
		t.Errorf("X-Custom-Trace-Id repeated across requests: %q", first)
	}
}

// ---- status ---------------------------------------------------------------

func TestStatusReportsAccountsAndModels(t *testing.T) {
	const secret = "SECRET-ACCESS-TOKEN-XYZ-0001"
	a := testAuth("A", secret)
	c := testClient(t, nil, []*Auth{a}, nil)
	st := c.Status(context.Background())

	if st.Name != clientName {
		t.Errorf("name = %q", st.Name)
	}
	if !st.Ready {
		t.Errorf("a fresh account should make the client ready: %s", st.Detail)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != stateReady {
		t.Errorf("accounts = %#v", st.Accounts)
	}
	if len(st.Models) == 0 {
		t.Error("models must be listed")
	}
	if st.UpdatedAt.IsZero() {
		t.Error("UpdatedAt must be set")
	}
	// A token must never appear in the status payload.
	blob, _ := json.Marshal(st)
	if strings.Contains(string(blob), secret) {
		t.Errorf("status leaked the access token: %s", blob)
	}
}

func TestStatusNotReadyWhenTokenExpiredAndSelfRenewOff(t *testing.T) {
	a := testAuth("A", "ta")
	a.ExpiresAt = time.Now().Add(-time.Hour)
	c := testClient(t, nil, []*Auth{a}, nil)
	st := c.Status(context.Background())
	if st.Ready {
		t.Errorf("expired token with self_renew off must not be ready: %s", st.Detail)
	}
	if !strings.Contains(st.Detail, "self_renew off") {
		t.Errorf("detail should explain why: %s", st.Detail)
	}
}

func TestNameMatchesRegistration(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	if c.Name() != clientName {
		t.Errorf("Name() = %q, want %q", c.Name(), clientName)
	}
	found := false
	for _, n := range core.Registered() {
		if n == clientName {
			found = true
		}
	}
	if !found {
		t.Error("the module must be registered as a client")
	}
}

// TestTraeChatNamesTheServedAccount pins the gateway-facing attribution.  The
// credential that served a turn is known only inside Chat, and before this slot
// existed every success was filed under "(unrouted)".  Auth.ID() prefers the
// Trae user id, so the slot must carry exactly that.
func TestTraeChatNamesTheServedAccount(t *testing.T) {
	a := testAuth("trae-user-1", "token-1")
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse("event:output\ndata:{\"response\":\"ok\"}\n\nevent:done\ndata:{\"finish_reason\":\"stop\"}\n\n"), nil
	})
	c := testClient(t, nil, []*Auth{a}, rt)

	var served string
	st, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "glm-5.2",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if ev, err := st.Recv(); err != nil || ev.Type != core.EventDelta || ev.Delta != "ok" {
		t.Fatalf("first event = %#v, %v", ev, err)
	}

	if served != "trae-user-1" {
		t.Errorf("ServedBy = %q, want %q", served, "trae-user-1")
	}
}
