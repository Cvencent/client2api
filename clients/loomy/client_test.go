package loomy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// client_test.go covers the module as the gateway sees it: registration, the
// request path, and -- just as importantly -- what this module refuses to claim.

// Compile-time proof of the optional interfaces this module really implements.
// Every one of them is backed by a call this vendor genuinely has.
var (
	_ core.AccountManager      = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.PackageProvider     = (*Client)(nil)
	_ core.CheckinProvider     = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.TaskProvider        = (*Client)(nil)
	_ core.SMSProvider         = (*Client)(nil)
	_ core.AutoLoginProvider   = (*Client)(nil)
)

// The vendor DOES publish an output budget: every chat row of GET /api/v1/models
// carries `max_output_tokens` (measured live 2026-10-01: deepseek-v4-flash-0731
// 384000, GLM-5.3-Flash 131072, Kimi-k2.6 65536).  This module used to drop it
// and asserted its absence here on the belief that the vendor published none;
// the live body disproved that.
//
// The half of the old concern that still holds is that a number must never be
// INVENTED.  So the interface is implemented, and this pins the honest half: on
// a cold cache -- and for the built-in fallback table, which carries no
// vendor-published budget -- the answer is ok=false, never a guessed number.
func TestTheModuleReportsAPublishedBudgetAndNeverInventsOne(t *testing.T) {
	c := newTestClient(t, "{}", nil)

	// Cold cache: the module must decline rather than answer from the fallback.
	if n, ok := c.ModelMaxOutputTokens(context.Background(), "deepseek-v4-flash-0731"); ok {
		t.Errorf("a cold cache answered %d, want ok=false: the built-in table carries no vendor budget", n)
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), ""); ok {
		t.Error("an empty model name was answered, want ok=false")
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "no-such-model"); ok {
		t.Error("an unknown model was answered, want ok=false")
	}
}

// The SMS login is a two-step code exchange, so the module drives it through
// core.AutoLoginProvider rather than core.LoginProvider, and no Loomy desktop
// credential location has been verified, so the import interfaces stay
// unimplemented.  None of that may be faked.
func TestTheModuleUsesAutoLoginRatherThanPanelLoginOrImport(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	if _, ok := any(c).(core.LoginProvider); ok {
		t.Error("Client implements core.LoginProvider, but the SMS flow needs a code submission the interface cannot carry")
	}
	if _, ok := any(c).(core.AutoLoginProvider); !ok {
		t.Error("Client does not implement core.AutoLoginProvider, so the panel has no way to run the SMS login")
	}
	if _, ok := any(c).(core.CredentialImporter); ok {
		t.Error("Client implements core.CredentialImporter, but no Loomy desktop credential location has been verified")
	}
	if _, ok := any(c).(core.BundleImporter); ok {
		t.Error("Client implements core.BundleImporter, but this vendor publishes no importable credential document")
	}
}

func TestTheModuleRegistersItselfUnderLoomy(t *testing.T) {
	found := false
	for _, name := range core.Registered() {
		if name == "loomy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the module is not registered; core.Registered() = %v", core.Registered())
	}

	built, err := core.Build("loomy", core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("core.Build(\"loomy\"): %v", err)
	}
	if built.Name() != "loomy" {
		t.Errorf("Name = %q, want \"loomy\"", built.Name())
	}
}

func TestNewNeverFailsOnAConfigurationItCannotRead(t *testing.T) {
	built, err := New(core.Deps{DataDir: t.TempDir(), Config: json.RawMessage("{not json at all")})
	if err != nil {
		t.Fatalf("New failed on a malformed config instead of falling back to defaults: %v", err)
	}
	if built.Name() != "loomy" {
		t.Errorf("Name = %q", built.Name())
	}
}

func TestChatWithoutACredentialIsNotConfigured(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
}

func TestChatWithoutAModelIsUnsupported(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("nil request: %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("blank model: %v, want core.ErrUnsupported", err)
	}
}

// The single most expensive protocol detail in this module: /chat/completions
// wants `Authorization: Bearer`, everything else wants a lowercase `token`, and
// sending the wrong one yields HTTP 200 with a business error.  The chat request
// therefore sends both, and this asserts the exact headers that go on the wire.
func TestChatSendsBothAuthenticationHeaders(t *testing.T) {
	var chat *http.Request
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path == "/api/v1/chat/completions" {
			chat = r
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		}
		return jsonResponse(200, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if chat == nil {
		t.Fatal("no request reached /chat/completions")
	}
	if got := chat.Method; got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}
	if got := chat.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q, want %q", got, "Bearer "+testToken)
	}
	if got := chat.Header.Get("token"); got != testToken {
		t.Errorf("token header = %q, want %q", got, testToken)
	}
	if got := chat.Header.Get("Accept"); got != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", got)
	}
	if got := chat.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if strings.Contains(rt.lastBody(), testToken) {
		t.Error("the session token leaked into the request body")
	}
}

func TestChatRecordsTheCredentialThatServedIt(t *testing.T) {
	rt := &stubTransport{handler: func(*http.Request, string) *http.Response {
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	served := ""
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if served != "loomy-1" {
		t.Errorf("ServedBy = %q, want the account that served the request", served)
	}
}

// A caller that copied the display name out of the panel must still reach the
// model: the credit multiplier lives inside the name, not in a separate field.
func TestChatResolvesADisplayNameToTheVendorModelID(t *testing.T) {
	var sent map[string]any
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path == "/api/v1/chat/completions" {
			mustJSON(t, body, &sent)
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		}
		return jsonResponse(200, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "DeepSeek V4 Flash 0731 · x3.0",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if got := sent["model"]; got != "deepseek-v4-flash-0731" {
		t.Errorf("model on the wire = %#v, want the vendor id", got)
	}
}

func TestChatLeavesAnUnknownModelNameAlone(t *testing.T) {
	var sent map[string]any
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path == "/api/v1/chat/completions" {
			mustJSON(t, body, &sent)
			return sseResponse("data: [DONE]\n\n")
		}
		return jsonResponse(200, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "some-model-that-does-not-exist-yet",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if got := sent["model"]; got != "some-model-that-does-not-exist-yet" {
		t.Errorf("model on the wire = %#v; an unknown id must be passed through untouched", got)
	}
}

// The vendor answers HTTP 200 for business failures, so a 200 alone proves
// nothing.  A 100002 is the session dying, and it has to park the account.
func TestChatTreatsABusinessErrorInA200AsAFailure(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, failureEnvelope("100002", "缺少 token")))
	seedAccount(t, c, "loomy-1", testToken, 0)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat reported success on a 100002 response")
	}
	if !strings.Contains(err.Error(), "100002") {
		t.Errorf("error = %q, want the business code", err)
	}
	acc, _ := c.store.lookup("loomy-1")
	if !acc.dead {
		t.Error("the account was not parked after the vendor rejected the session")
	}
}

func TestChatFailsOverToTheSecondAccount(t *testing.T) {
	attempts := 0
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		attempts++
		if r.Header.Get("token") == testToken {
			return jsonResponse(200, failureEnvelope("100002", "缺少 token"))
		}
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "first", testToken, 0)
	seedAccount(t, c, "second", testTokenTwo, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if attempts != 2 {
		t.Errorf("%d attempts, want 2", attempts)
	}
	first, _ := c.store.lookup("first")
	if !first.dead {
		t.Error("the rejected account was not parked")
	}
	second, _ := c.store.lookup("second")
	if second.dead {
		t.Error("the working account was parked")
	}
}

func TestChatWithEverySessionGoneExplainsWhy(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now(), time.Minute)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded with no usable session")
	}
	if !strings.Contains(err.Error(), "no refresh endpoint") {
		t.Errorf("error = %q, want it to say the session cannot be renewed", err)
	}
	if !strings.Contains(err.Error(), "import the new session token") {
		t.Errorf("error = %q, want it to tell the operator what to do", err)
	}
}

func TestChatWithEveryAccountParkedSaysToRetry(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", &apiError{Status: 500, Message: "boom"}, time.Now(), time.Hour)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded with every account parked")
	}
	if !strings.Contains(err.Error(), "parked or cooling down") {
		t.Errorf("error = %q, want the retry-soon explanation", err)
	}
}

// A transport failure is not a business failure; the account should cool down,
// not be condemned.
func TestChatCoolsDownOnATransportFailure(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: errors.New("connection reset by peer")})
	seedAccount(t, c, "loomy-1", testToken, 0)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded although the transport failed")
	}
	acc, _ := c.store.lookup("loomy-1")
	if acc.dead {
		t.Error("a transport failure parked the account as dead")
	}
	if acc.selectable(time.Now()) {
		t.Error("the account ignored its cooldown")
	}
}

// /models accepts only the lowercase token header.  Sending Authorization as
// well is harmless there, but sending it INSTEAD is the bug the reference calls
// out, so the absence is asserted.
func TestModelsSendsOnlyTheTokenHeader(t *testing.T) {
	var modelReq *http.Request
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path == "/api/v1/models" {
			modelReq = r
			return jsonResponse(200, okEnvelope(
				`[{"id":"deepseek-v4-flash-0731","name":"DeepSeek V4 Flash 0731 · x3.0","type":"chat","context_length":1048576}]`))
		}
		return jsonResponse(200, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "deepseek-v4-flash-0731" {
		t.Fatalf("models = %#v", models)
	}
	if modelReq == nil {
		t.Fatal("no request reached /models")
	}
	if got := modelReq.Header.Get("token"); got != testToken {
		t.Errorf("token header = %q, want %q", got, testToken)
	}
	if got := modelReq.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q; /models accepts only the lowercase token header", got)
	}
	if got := modelReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
}

func TestRefreshModelsKeepsTheCatalogueWhenUpstreamFails(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(500, "upstream is having a bad day"))
	seedAccount(t, c, "loomy-1", testToken, 0)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("RefreshModels reported success on an HTTP 500")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh emptied the catalogue; the model picker would go blank")
	}
}

func TestRefreshModelsWithoutACredentialIsNotConfigured(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))

	models, err := c.RefreshModels(context.Background())
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Errorf("error = %v, want core.ErrNotConfigured", err)
	}
	if len(models) == 0 {
		t.Error("the built-in catalogue was not returned alongside the error")
	}
}

func TestModelsAlwaysAnswersOffline(t *testing.T) {
	c := newTestClient(t, "{}", nil)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("Models returned nothing with no cache and no credential")
	}
	if models[0].OwnedBy != "loomy" {
		t.Errorf("OwnedBy = %q, want \"loomy\"", models[0].OwnedBy)
	}
}

func TestStatusReportsTheBareModelIDs(t *testing.T) {
	rt := alwaysJSON(200, okEnvelope("null"))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	st := c.Status(context.Background())
	if !st.Ready {
		t.Errorf("Ready = false for a healthy account: %s", st.Detail)
	}
	if rt.count() != 0 {
		t.Errorf("Status made %d upstream calls; it must stay offline", rt.count())
	}
	if len(st.Models) == 0 {
		t.Fatal("Status reported no models")
	}
	for _, id := range st.Models {
		if strings.Contains(id, " · ") {
			t.Errorf("model id %q carries the display name's multiplier", id)
		}
	}
}
