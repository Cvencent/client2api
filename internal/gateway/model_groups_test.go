package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

func modelGroupServer(t *testing.T) (*http.Server, *testClient, *testClient) {
	t.Helper()
	alpha := &testClient{
		name:      "alpha",
		catalogue: []core.Model{{ID: "alpha-model"}},
		events:    usageEvents(1, 1),
	}
	beta := &testClient{
		name:      "beta",
		catalogue: []core.Model{{ID: "beta-model"}},
		events:    usageEvents(1, 1),
	}
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {
			Members: []core.ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
			PlatformPriorities: map[string]int{"alpha": 20, "beta": 10},
		},
	})
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	}), alpha, beta
}

func TestModelGroupCatalogueEntry(t *testing.T) {
	srv, _, _ := modelGroupServer(t)

	rec := chatListModels(t, srv)
	var out modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /v1/models: %v (%s)", err, rec.Body.String())
	}
	var group *modelEntry
	for i := range out.Data {
		if out.Data[i].ID == "shared" {
			group = &out.Data[i]
			break
		}
	}
	if group == nil {
		t.Fatalf("model group is missing from the catalogue: %s", rec.Body.String())
	}
	if group.OwnedBy != "group" {
		t.Fatalf("group owned_by = %q, want group", group.OwnedBy)
	}
	members, ok := group.Extra["members"].([]any)
	if !ok || len(members) != 2 {
		t.Fatalf("group members = %#v, want the two configured members", group.Extra["members"])
	}
}

func TestModelGroupChatUsesConfiguredPriority(t *testing.T) {
	srv, alpha, beta := modelGroupServer(t)

	rec := chat(t, srv, `{"model":"shared","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("group chat = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if beta.seen == nil || beta.seen.Model != "beta-model" {
		t.Fatalf("group chat did not reach the highest-priority member: %+v", beta.seen)
	}
	if alpha.seen != nil {
		t.Fatalf("group chat reached the lower-priority member: %+v", alpha.seen)
	}
}

func chatListModels(t *testing.T, srv *http.Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/models = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	return rec
}
