package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// A relay that fronts DeepSeek answers a request carrying tools with 400
// MODEL_TOOL_CHOICE_NOT_SUPPORTED ("当前模型或上游不支持工具调用") even after
// the choice was already downgraded to "auto" -- the relay's upstream simply
// cannot serve tools for that model.  The refusal is about the model on this
// relay, not about the credential and not about the content, so it must be
// classified as a model-scoped refusal: the account stays healthy and the
// gateway is free to fail a model-group request over to a sibling platform
// that does serve tools.
func TestClassifyToolRefusalAsModelRefused(t *testing.T) {
	cases := []struct {
		name   string
		status int
		text   string
	}{
		{"vendor chinese message", 400, "当前模型或上游不支持工具调用"},
		{"vendor code name", 400, "MODEL_TOOL_CHOICE_NOT_SUPPORTED: 当前模型或上游不支持工具调用"},
		{"english message", 400, "this model does not support tool calling"},
		{"english tools plural", 400, "tools are not supported by the upstream for this model"},
		{"function call variant", 400, "当前模型不支持函数调用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFailure(tc.status, tc.text); got != kindModelTurnedAway {
				t.Fatalf("classifyFailure(%d, %q) = %q, want %q", tc.status, tc.text, got, kindModelTurnedAway)
			}
			if coreKindFor(kindModelTurnedAway) != core.FailureModelRefused {
				t.Fatalf("coreKindFor(kindModelTurnedAway) = %v, want FailureModelRefused", coreKindFor(kindModelTurnedAway))
			}
		})
	}
}

// A message naming both a missing model and tools stays a missing model: the
// not-found table is checked first on purpose.
func TestClassifyModelNotFoundBeatsToolWording(t *testing.T) {
	if got := classifyFailure(400, "model not found; tool calling is unavailable"); got != kindNotFound {
		t.Fatalf("classifyFailure = %q, want %q", got, kindNotFound)
	}
}

// End to end through source.Chat: the relay answers 400 with the vendor's
// tool-refusal message, the failure comes back classified, and the account is
// NOT parked or cooled -- the key is healthy, the relay just cannot serve
// tools for that model.
func TestSourceChatToolRefusalLeavesAccountReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"vendor/model"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"MODEL_TOOL_CHOICE_NOT_SUPPORTED","message":"当前模型或上游不支持工具调用"}}`))
	}))
	defer srv.Close()

	c, err := newSource("tool-relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	req := &core.ChatRequest{
		Model:    "vendor/model",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
		Tools:    []core.Tool{{Type: "function", Name: "lookup", Parameters: []byte(`{"type":"object","properties":{}}`)}},
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	_, chatErr := c.Chat(context.Background(), req)
	if chatErr == nil {
		t.Fatal("Chat succeeded, want the tool refusal")
	}
	f, ok := core.AsFailure(chatErr)
	if !ok {
		t.Fatalf("Chat error is not a classified failure: %v", chatErr)
	}
	if f.Kind != core.FailureModelRefused {
		t.Fatalf("failure kind = %v, want FailureModelRefused", f.Kind)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("failure status = %d, want 400", f.Status)
	}
	for _, st := range c.Status(context.Background()).Accounts {
		if st.State != "ready" {
			t.Fatalf("account state after tool refusal = %q, want ready (the key is not at fault)", st.State)
		}
	}
}
