package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"client2api/internal/core"
)

const panelBuiltinSourceID = "zz-panel-builtin-test"

var registerPanelBuiltinOnce sync.Once

func registerPanelBuiltin(t *testing.T) {
	t.Helper()
	registerPanelBuiltinOnce.Do(func() {
		core.RegisterBuiltinSource(panelBuiltinSourceID, core.SourceConfig{
			Label:   "shipped panel platform",
			BaseURL: "https://builtin-panel.example.test/v1",
		})
	})
}

func TestPanelProjectsBuiltinSourceWithoutWritingConfig(t *testing.T) {
	registerPanelBuiltin(t)
	path := configFile(t, `{}`)
	p := configPanel(path)
	w, out := doConfig(t, p, http.MethodGet, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	found := false
	for _, id := range out["builtin_sources"].([]any) {
		if id == panelBuiltinSourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("builtin_sources = %v, want %q", out["builtin_sources"], panelBuiltinSourceID)
	}
	cfg := out["config"].(map[string]any)
	sources := cfg["sources"].(map[string]any)
	row := sources[panelBuiltinSourceID].(map[string]any)
	if row["label"] != "shipped panel platform" || row["base_url"] != "https://builtin-panel.example.test/v1" {
		t.Fatalf("projected row = %v", row)
	}
	if _, ok := onDisk(t, path)["sources"]; ok {
		t.Fatal("GET projected a builtin into the config file")
	}
}

func TestPanelKeepsExplicitBuiltinOverride(t *testing.T) {
	registerPanelBuiltin(t)
	path := configFile(t, `{"sources":{"`+panelBuiltinSourceID+`":{"label":"operator","base_url":"https://operator.example.test/v1","disabled":true}}}`)
	p := configPanel(path)
	_, out := doConfig(t, p, http.MethodGet, "")
	cfg := out["config"].(map[string]any)
	row := cfg["sources"].(map[string]any)[panelBuiltinSourceID].(map[string]any)
	if row["label"] != "operator" || row["base_url"] != "https://operator.example.test/v1" || row["disabled"] != true {
		t.Fatalf("explicit override was not preserved: %v", row)
	}
}

func TestPanelHidesEditAndDeleteForBuiltinSource(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		"BUILTIN_SOURCES = new Set((cfg.data && cfg.data.builtin_sources) || []);",
		"BUILTIN_SOURCES.has(n) ? '' : '<button class=\"xs sourceEdit\"",
		"' · 内置平台'",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("builtin source UI is missing %q", want)
		}
	}
}

func TestPanelRefusesToDeleteBuiltinSource(t *testing.T) {
	registerPanelBuiltin(t)
	p := configPanel(configFile(t, `{}`))
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	p.handleSource(w, httptest.NewRequest(http.MethodDelete, "/panel/api/sources/"+panelBuiltinSourceID, nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "built-in platforms cannot be deleted") {
		t.Fatalf("delete builtin: %d %s", w.Code, w.Body.String())
	}
}
