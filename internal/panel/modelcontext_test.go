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

// TestModelContextRestoreDefaultClearsManualElevateChoice pins what the row's
// "restore default" action posts for a model with an embedded default: it must
// drop the manual choice rather than write elevate_top:false, or a V4.1 Flash
// row could never be turned back on by restoring defaults.
func TestModelContextRestoreDefaultClearsManualElevateChoice(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("cline", "cline-free/deepseek-v4.1-flash", false); err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"cline":{"cline-free/deepseek-v4.1-flash":{"elevate_top_default":true}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := store.Get("cline", "cline-free/deepseek-v4.1-flash"); ok {
		t.Fatal("restore default left a manual entry behind")
	}
	if !modelmeta.ElevateTop("cline", "cline-free/deepseek-v4.1-flash", store) {
		t.Fatal("restore default did not fall back to the embedded V4.1 default")
	}
}

func TestModelContextEffortMappingsRoundTrip(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"glm-5.3-flash":{"glm-5.3-flash":{"effort_mappings":[{"from":"high","to":"max"},{"from":"xhigh","to":"max"}]}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}

	got, ok := store.Get("glm-5.3-flash", "glm-5.3-flash")
	if !ok {
		t.Fatal("effort mappings were not stored")
	}
	if len(got.EffortMappings) != 2 {
		t.Fatalf("effort mappings = %+v, want two rules", got.EffortMappings)
	}
	if got.EffortMappings[0] != (modelmeta.EffortMapping{From: "high", To: "max"}) ||
		got.EffortMappings[1] != (modelmeta.EffortMapping{From: "xhigh", To: "max"}) {
		t.Fatalf("effort mappings = %+v", got.EffortMappings)
	}

	rec = get(t, h, "/panel/api/model_context")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"effort_mappings"`) {
		t.Fatalf("GET response missing effort_mappings: %s", rec.Body.String())
	}
}
func TestModelContextRestoreDefaultAlsoClearsEffortMappings(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings("glm-5.3-flash", "glm-5.3-flash", []modelmeta.EffortMapping{{From: "high", To: "max"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("glm-5.3-flash", "glm-5.3-flash", false); err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"glm-5.3-flash":{"glm-5.3-flash":{"elevate_top_default":true}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got, ok := store.Get("glm-5.3-flash", "glm-5.3-flash"); ok {
		t.Fatalf("restore default left a manual entry behind: %+v", got)
	}
}
