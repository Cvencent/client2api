package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// Google's OpenAI-compatible layer names every catalogue id "models/<name>".
// The scan must store the bare name so routing ids stay vendor-neutral; the
// chat endpoint accepts the bare name, so nothing needs to re-add a prefix.
func TestSourceScanStripsGoogleModelsPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"models/gemini-3.8-flash"},{"id":"models/gemini-3.5-flash"},{"id":"bare-name"}]}`)
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	deps := core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()}
	c, err := newSource("google-relay", core.SourceConfig{Label: "Google", BaseURL: srv.URL + "/v1"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key"}}); err != nil {
		t.Fatal(err)
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
		if len(m.ID) > 0 && m.ID[:7] == "models/" {
			t.Errorf("scanned id kept the Google prefix: %q", m.ID)
		}
	}
	want := []string{"bare-name", "gemini-3.5-flash", "gemini-3.8-flash"}
	if len(ids) != len(want) {
		t.Fatalf("scanned ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("scanned ids = %v, want %v", ids, want)
		}
	}
}
