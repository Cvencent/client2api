package panel

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModuleOptionsAreWellFormed keeps the table usable as a form: every entry
// needs a key and a label to render, and a dropdown without choices would draw
// an empty select.
func TestModuleOptionsAreWellFormed(t *testing.T) {
	kinds := map[moduleOptionKind]bool{
		optText: true, optBool: true, optChoice: true, optNumber: true, optList: true,
	}
	if len(moduleOptions) == 0 {
		t.Fatal("moduleOptions is empty; the config page would fall back to raw JSON")
	}
	for module, opts := range moduleOptions {
		if len(opts) == 0 {
			t.Errorf("module %q has an empty option list", module)
		}
		seen := map[string]bool{}
		for _, o := range opts {
			if strings.TrimSpace(o.Key) == "" {
				t.Errorf("module %q has an option with no key", module)
			}
			if strings.TrimSpace(o.Label) == "" {
				t.Errorf("module %q key %q has no label", module, o.Key)
			}
			if !kinds[o.Kind] {
				t.Errorf("module %q key %q has unknown kind %q", module, o.Key, o.Kind)
			}
			if o.Kind == optChoice && len(o.Choices) == 0 {
				t.Errorf("module %q key %q is a dropdown with no choices", module, o.Key)
			}
			if seen[o.Key] {
				t.Errorf("module %q lists key %q twice", module, o.Key)
			}
			seen[o.Key] = true
		}
	}
}

// TestModuleOptionKeysExistInModuleSource is the typo guard.  The authority on
// what a module reads is the module's own struct tags, so every key the panel
// claims to describe has to appear as a json tag somewhere in that module's
// non-test sources.  Without this a rename in a client would leave the form
// writing a key nobody reads, and the setting would silently do nothing.
func TestModuleOptionKeysExistInModuleSource(t *testing.T) {
	for module, opts := range moduleOptions {
		files, err := filepath.Glob(filepath.Join("..", "..", "clients", module, "*.go"))
		if err != nil {
			t.Fatalf("globbing %s sources: %v", module, err)
		}
		if len(files) == 0 {
			t.Errorf("module %q has no source under clients/%s", module, module)
			continue
		}
		var src strings.Builder
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("reading %s: %v", f, err)
			}
			src.Write(raw)
		}
		text := src.String()
		for _, o := range opts {
			if !strings.Contains(text, `json:"`+o.Key) {
				t.Errorf("module %q: no json tag %q in clients/%s; the panel would edit a key the module never reads",
					module, o.Key, module)
			}
		}
	}
}

// TestExampleConfigClientKeysHaveControls is the coverage guard.  The example
// config is what an operator copies, so every client setting the example ships
// must be editable through a labelled control rather than only through the raw
// JSON box.  It does not require the table to cover *every* struct tag -- the
// advanced box still exists for tuning knobs.
func TestExampleConfigClientKeysHaveControls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "client2api.example.json"))
	if err != nil {
		t.Fatalf("reading the example config: %v", err)
	}
	var doc struct {
		Clients map[string]map[string]json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the example config: %v", err)
	}
	if len(doc.Clients) == 0 {
		t.Fatal("the example config has no clients section")
	}
	for module, settings := range doc.Clients {
		covered := map[string]bool{}
		for _, o := range moduleOptions[module] {
			covered[o.Key] = true
		}
		for key := range settings {
			if !covered[key] {
				t.Errorf("clients.%s.%s ships in the example config but has no labelled control", module, key)
			}
		}
	}
}

// TestConfigPayloadCarriesModuleSchema pins the wire contract the page reads:
// module_schema has to be in GET /panel/api/config, otherwise the editor has
// nothing to render forms from and silently degrades to the old JSON box.
func TestConfigPayloadCarriesModuleSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client2api.json")
	if err := os.WriteFile(path, []byte("{\"listen\":\"127.0.0.1:1\"}\n"), 0o600); err != nil {
		t.Fatalf("writing the config: %v", err)
	}
	p := &panel{opts: Options{ConfigPath: path}}
	rec := httptest.NewRecorder()
	p.configRead(rec, httptest.NewRequest("GET", "/panel/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("config read = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		ModuleSchema map[string][]moduleOption `json:"module_schema"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}
	zcode := payload.ModuleSchema["zcode"]
	if len(zcode) == 0 {
		t.Fatalf("module_schema has no zcode entry: %s", rec.Body.String())
	}
	found := false
	for _, o := range zcode {
		if o.Key == "captcha_browser" {
			found = true
			if o.Kind != optBool {
				t.Errorf("zcode.captcha_browser kind = %q, want %q", o.Kind, optBool)
			}
		}
	}
	if !found {
		t.Error("module_schema.zcode lost its captcha_browser control")
	}
}
