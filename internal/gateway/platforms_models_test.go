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

// TestModelsHidesBlacklistedEntries pins that the aggregated catalogue agrees
// with routing: a platform/model the operator disabled must not be advertised.
// Otherwise a caller that lists models and picks one gets a refusal it could
// not have predicted from the list.  An alias whose target is disabled is just
// as misleading and must be hidden too.
func TestModelsHidesBlacklistedEntries(t *testing.T) {
	reg := core.NewRegistry()
	reg.Add(&testClient{name: "cline", catalogue: []core.Model{
		{ID: "gpt-9", OwnedBy: "cline"},
		{ID: "claude-sonnet-4.5", OwnedBy: "cline"},
	}})
	reg.AddAlias("cheap", "cline/gpt-9")
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"cline": {DisabledModels: []string{"gpt-9"}},
	})
	srv := NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})

	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var out modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	ids := map[string]bool{}
	for _, e := range out.Data {
		ids[e.ID] = true
	}
	if ids["cline/gpt-9"] {
		t.Error("a blacklisted client/model entry is still advertised in /v1/models")
	}
	if !ids["cline/claude-sonnet-4.5"] {
		t.Error("an allowed model disappeared from /v1/models")
	}
	if ids["cheap"] {
		t.Error("an alias pointing at a blacklisted model is still advertised")
	}
}
