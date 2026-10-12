package panel

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/modelmeta"
)

func TestModelContextOverridesRoundTrip(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore(filepath.Join(t.TempDir(), "model_context.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"zcode":{"glm-5.3-flash":{"context_length":1048576,"max_output_tokens":131072}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = get(t, h, "/panel/api/model_context")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1048576") || !strings.Contains(rec.Body.String(), "131072") {
		t.Fatalf("GET did not return saved values: %s", rec.Body.String())
	}
	got, ok := store.Get("zcode", "glm-5.3-flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("store = %+v ok=%v", got, ok)
	}
}

func TestModelContextEffortPolicyRoundTrip(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"workbuddy":{"cn:glm-5.3":{"supported_efforts":["low","high","max"],"default_effort":"max","elevate_top":true}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got, ok := store.Get("workbuddy", "cn:glm-5.3")
	if !ok {
		t.Fatal("effort policy was not stored")
	}
	if len(got.SupportedEfforts) != 3 || got.SupportedEfforts[2] != "max" || got.DefaultEffort != "max" || !got.ElevateTop {
		t.Fatalf("stored effort policy = %+v", got)
	}

	rec = get(t, h, "/panel/api/model_context")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"supported_efforts", "default_effort", "elevate_top"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET response missing %q: %s", want, rec.Body.String())
		}
	}
}
