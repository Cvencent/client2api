package gateway

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

func exposedTestServer(t *testing.T, reg *core.Registry) *http.Server {
	t.Helper()
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})
}

func TestModelsListFiltersByExposedPolicy(t *testing.T) {
	reg := core.NewRegistry()
	reg.Add(&testClient{name: "alpha", catalogue: []core.Model{{ID: "alpha-model"}}})
	reg.Add(&testClient{name: "beta", catalogue: []core.Model{{ID: "beta-model"}}})
	reg.Add(&fakeTranscriptionClient{name: "speech", models: []core.Model{{ID: "whisper-1"}}})
	reg.AddAlias("alias", "alpha/alpha-model")
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "alpha-model"},
			{Client: "beta", Model: "beta-model"},
		}},
	})
	srv := exposedTestServer(t, reg)

	all := modelIDs(t, srv)
	for _, id := range []string{
		"alpha/alpha-model", "beta/beta-model", "Auto/alpha-model",
		"speech/whisper-1", "shared", "alias",
	} {
		if _, ok := all[id]; !ok {
			t.Fatalf("unconfigured /v1/models is missing %q: %v", id, all)
		}
	}

	reg.SetExposedModels([]string{
		"alpha/alpha-model", "Auto/alpha-model", "shared", "alias", "speech/whisper-1",
	}, true)
	got := modelIDs(t, srv)
	if len(got) != 5 {
		t.Fatalf("filtered /v1/models has %d entries, want 5: %v", len(got), got)
	}
	if _, ok := got["beta/beta-model"]; ok {
		t.Fatalf("unselected platform model is still advertised: %v", got)
	}

	reg.SetExposedModels(nil, true)
	if got := modelIDs(t, srv); len(got) != 0 {
		t.Fatalf("explicitly empty exposed policy still advertised models: %v", got)
	}
}

func TestUnselectedModelCallIsRejectedBeforeRouting(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.SetExposedModels([]string{"other"}, true)
	srv := exposedTestServer(t, reg)

	rec := chat(t, srv, `{"model":"alpha/m1","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unselected model call = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if alpha.chatCalls != 0 {
		t.Fatalf("unselected model reached the module %d times", alpha.chatCalls)
	}
	if !strings.Contains(rec.Body.String(), "not exposed") {
		t.Fatalf("rejection does not explain the exposed-model policy: %s", rec.Body.String())
	}
}

func TestUnselectedAliasIsRejectedEvenWhenItsTargetIsExposed(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.AddAlias("alias", "alpha/m1")
	reg.SetExposedModels([]string{"alpha/m1"}, true)
	srv := exposedTestServer(t, reg)

	rec := chat(t, srv, `{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unselected alias call = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if alpha.chatCalls != 0 {
		t.Fatalf("unselected alias reached its target %d times", alpha.chatCalls)
	}
}

func TestUnselectedAutoAndGroupAreRejectedEvenWhenMembersAreExposed(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	beta := &testClient{name: "beta", catalogue: []core.Model{{ID: "m2"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "m1"},
			{Client: "beta", Model: "m2"},
		}},
	})
	reg.SetExposedModels([]string{"alpha/m1", "beta/m2"}, true)
	srv := exposedTestServer(t, reg)

	for _, model := range []string{"Auto/m1", "shared"} {
		rec := chat(t, srv, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unselected %s call = %d, want 404: %s", model, rec.Code, rec.Body.String())
		}
	}
	if alpha.chatCalls != 0 || beta.chatCalls != 0 {
		t.Fatalf("unselected virtual models reached a module: alpha=%d beta=%d", alpha.chatCalls, beta.chatCalls)
	}
}

func TestSelectedAliasAutoAndGroupRemainCallable(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}, events: usageEvents(1, 1)}
	beta := &testClient{name: "beta", catalogue: []core.Model{{ID: "m2"}}, events: usageEvents(1, 1)}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.AddAlias("alias", "alpha/m1")
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "m1"},
			{Client: "beta", Model: "m2"},
		}, PlatformPriorities: map[string]int{"alpha": 10, "beta": 20}},
	})
	reg.SetExposedModels([]string{"alias", "Auto/m1", "shared"}, true)
	srv := exposedTestServer(t, reg)

	for _, model := range []string{"alias", "Auto/m1", "shared"} {
		rec := chat(t, srv, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("selected %s call = %d, want 200: %s", model, rec.Code, rec.Body.String())
		}
	}
	if alpha.chatCalls != 3 {
		t.Fatalf("selected virtual models reached alpha %d times, want 3", alpha.chatCalls)
	}
}

func TestUnselectedTranscriptionModelIsRejectedBeforeProviderCall(t *testing.T) {
	provider := &fakeTranscriptionClient{
		name:   "speech",
		models: []core.Model{{ID: "whisper-1"}},
		result: core.TranscriptionResult{Text: "hello"},
	}
	reg := core.NewRegistry()
	reg.Add(provider)
	reg.SetExposedModels([]string{"other"}, true)
	srv := exposedTestServer(t, reg)

	rec := transcriptionRequest(t, srv, "speech/whisper-1", "json", []byte("audio"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unselected transcription call = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if len(provider.seen) != 0 {
		t.Fatalf("unselected transcription reached the provider %d times", len(provider.seen))
	}
}

func TestUnselectedCountTokensModelIsRejected(t *testing.T) {
	alpha := &testClient{name: "alpha", catalogue: []core.Model{{ID: "m1"}}}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.SetExposedModels([]string{"other"}, true)
	srv := exposedTestServer(t, reg)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"alpha/m1","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unselected count_tokens = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
