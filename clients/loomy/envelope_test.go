package loomy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// The vendor speaks two response shapes and only one of them is an envelope.
//
// `GET /api/v1/points/records` answers
// `{"code":"000000","desc":"成功","trace_id":…,"data":{…}}`, but
// `GET /api/v1/models` answers a bare OpenAI-shaped list object with no `code`
// key at all:
//
//	{"reasoning_enabled":true,"object":"list","data":[…]}
//
// Both bodies below were captured from the live API on 2026-10-01; the
// catalogue is the real response trimmed to two rows.
//
// Reading the code-less shape as "business error (no code)" was a live defect:
// it failed every catalogue refresh and, worse, made `fetchModels` penalise a
// perfectly healthy credential, which parked the account and turned every later
// chat into "every account is parked or cooling down".
const liveModelsBody = `{"reasoning_enabled":true,` +
	`"reasoning_catalog_version":"sha256:2453f3858222e5b498e7e5f3275377b874aee22c12a0e9922ebb26f1deebbe93",` +
	`"object":"list","data":[` +
	`{"reasoning_efforts":["none","low","medium","high","xhigh"],"default_reasoning_effort":"low",` +
	`"id":"deepseek-v4-flash-0731","name":"DeepSeek V4 Flash 0731（x3.0）","object":"model",` +
	`"created":1785723402,"owned_by":"loomy","type":"chat","protocol":"openai_chat",` +
	`"context_length":1048576,"max_output_tokens":384000,` +
	`"capabilities":{"reasoning":true,"vision":false,"function_calling":true,"streaming":true,` +
	`"system_message":true,"input_modalities":["text"],"output_modalities":["text"]},` +
	`"is_default_model":true},` +
	`{"id":"GLM-5.3-Flash","name":"GLM-5.3-Flash（x1.0）","object":"model","type":"chat",` +
	`"context_length":1048576,"max_output_tokens":131072,` +
	`"capabilities":{"reasoning":true,"input_modalities":["text"]}}]}`

// A payload with no `code` key is a bare payload, not a failed envelope.  The
// whole body is handed on as Data so that the shape-specific parsers
// (`parseRemoteModels` accepts both an object wrapping `data` and a bare array)
// can decide what it means.
func TestParseEnvelopeTreatsACodeLessPayloadAsSuccess(t *testing.T) {
	env := parseEnvelope([]byte(liveModelsBody))
	if !env.OK {
		t.Fatalf("a payload with no code key was read as a failure: %q", env.Message)
	}
	if env.Code != "" {
		t.Errorf("Code = %q, want empty", env.Code)
	}
	if string(env.Data) != liveModelsBody {
		t.Errorf("Data = %q, want the whole payload passed through", env.Data)
	}
}

// The new branch must not swallow the vendor's real failures: a `code` key that
// is present -- whatever it holds -- still goes through the envelope rules.
func TestParseEnvelopeStillReadsAFailedEnvelopeAsAFailure(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"vendor auth code", `{"code":"100002","desc":"登录状态失效"}`, "登录状态失效"},
		{"bad request code", `{"code":"100001","desc":"手机号格式错误"}`, "手机号格式错误"},
		{"empty code with a desc", `{"code":"","desc":"boom"}`, "boom"},
		{"empty code with nothing else", `{"code":""}`, "business error (no code)"},
		{"null code", `{"code":null,"desc":"boom"}`, "boom"},
		{"code only", `{"code":"123456"}`, "business error 123456"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := parseEnvelope([]byte(tc.body))
			if env.OK {
				t.Fatalf("a failed envelope was read as success: %s", tc.body)
			}
			if env.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", env.Message, tc.wantMsg)
			}
		})
	}
}

// A body that is neither a JSON object nor a JSON array is still its own
// outcome: the vendor answers a wrong request with an HTML error page often
// enough that this cannot become a bare-payload success.
func TestParseEnvelopeStillRejectsANonJSONBody(t *testing.T) {
	bodies := map[string]string{
		"html error page":  "<html><body>502 Bad Gateway</body></html>",
		"truncated object": "{",
		"empty body":       "",
		"bare scalar":      `"boom"`,
		"malformed array":  "[not json",
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			env := parseEnvelope([]byte(body))
			if env.OK {
				t.Fatalf("%q was read as success", body)
			}
			if env.Message != "the response is not a JSON object" {
				t.Errorf("Message = %q", env.Message)
			}
		})
	}
}

func TestModelsAcceptsTheLiveCodeLessShape(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/models" {
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
		return jsonResponse(http.StatusOK, liveModelsBody)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	got := map[string]core.Model{}
	for _, m := range models {
		got[m.ID] = m
	}
	if len(got) != 2 {
		t.Fatalf("models = %v, want the two live rows", got)
	}
	first, ok := got["deepseek-v4-flash-0731"]
	if !ok {
		t.Fatalf("the live default model is missing from %v", got)
	}
	// `fallback` is only set on the built-in table, so its absence proves the
	// live body was parsed rather than silently replaced by the fallback.
	if _, fallback := first.Extra["fallback"]; fallback {
		t.Fatal("the live catalogue was discarded and the built-in table returned")
	}
	if ctxLen, _ := first.Extra["context_length"].(int); ctxLen != 1048576 {
		t.Errorf("deepseek-v4-flash-0731 context = %v, want 1048576", first.Extra["context_length"])
	}
	if reasoning, _ := first.Extra["reasoning"].(bool); !reasoning {
		t.Error("deepseek-v4-flash-0731 lost its reasoning capability")
	}
	efforts, _ := first.Extra["reasoning_efforts"].([]string)
	if len(efforts) != 5 || efforts[0] != "none" || efforts[4] != "xhigh" {
		t.Errorf("reasoning_efforts = %v, want the vendor's five", efforts)
	}
	if _, ok := got["GLM-5.3-Flash"]; !ok {
		t.Errorf("GLM-5.3-Flash is missing from %v", got)
	}

	// The budget the vendor publishes must survive into the catalogue, and the
	// module must be able to report it without touching the network.
	if limit, _ := first.Extra["max_output_tokens"].(int); limit != 384000 {
		t.Errorf("deepseek-v4-flash-0731 max_output_tokens = %v, want the vendor's 384000", first.Extra["max_output_tokens"])
	}
	if limit, _ := got["GLM-5.3-Flash"].Extra["max_output_tokens"].(int); limit != 131072 {
		t.Errorf("GLM-5.3-Flash max_output_tokens = %v, want the vendor's 131072", got["GLM-5.3-Flash"].Extra["max_output_tokens"])
	}
	if n, ok := c.ModelMaxOutputTokens(context.Background(), "deepseek-v4-flash-0731"); !ok || n != 384000 {
		t.Errorf("ModelMaxOutputTokens(deepseek-v4-flash-0731) = %d, %v; want 384000, true", n, ok)
	}
	if n, ok := c.ModelMaxOutputTokens(context.Background(), "GLM-5.3-Flash"); !ok || n != 131072 {
		t.Errorf("ModelMaxOutputTokens(GLM-5.3-Flash) = %d, %v; want 131072, true", n, ok)
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "Hy-Image-3.5-preview"); ok {
		t.Error("an image model was given a budget, but it is not a chat model")
	}
}

// A bare array with no envelope at all is the third shape the reference
// accepts; it must keep working.
func TestModelsAcceptsABareArrayWithNoCode(t *testing.T) {
	body := `[{"id":"deepseek-v4-flash-0731","name":"DeepSeek V4 Flash 0731","type":"chat","context_length":1048576}]`
	rt := &stubTransport{handler: func(*http.Request, string) *http.Response {
		return jsonResponse(http.StatusOK, body)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "deepseek-v4-flash-0731" {
		t.Fatalf("models = %v", models)
	}
}

// The regression that matters: a successful refresh must leave the credential
// exactly as it found it.  Before the fix this account ended up in cooldown
// with failures=1 and the message "business error (no code)".
func TestAModelRefreshWithTheLiveShapeDoesNotParkTheAccount(t *testing.T) {
	rt := &stubTransport{handler: func(*http.Request, string) *http.Response {
		return jsonResponse(http.StatusOK, liveModelsBody)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished from the pool")
	}
	if acc.dead {
		t.Errorf("a successful refresh parked the account as dead (%s)", acc.lastError)
	}
	if acc.failures != 0 {
		t.Errorf("failures = %d (%s), want 0: the credential was blamed for a body it read correctly",
			acc.failures, acc.lastError)
	}
	if !acc.selectable(time.Now()) {
		t.Errorf("a successful refresh put the account in cooldown until %s (%s)",
			acc.cooldownTill.Format(time.RFC3339), acc.lastError)
	}
}

// And the user-visible consequence, end to end: the chat that used to answer
// "every account is parked or cooling down" must reach the vendor.
func TestChatWorksAfterARefreshWithTheLiveShape(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path == "/api/v1/models" {
			return jsonResponse(http.StatusOK, liveModelsBody)
		}
		if r.URL.Path == "/api/v1/chat/completions" {
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
		}
		return jsonResponse(http.StatusOK, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat after a live-shaped refresh: %v", err)
	}
	defer stream.Close()

	var text string
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		text += event.Delta
	}
	if text != "pong" {
		t.Errorf("text = %q, want %q", text, "pong")
	}
}
