package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The probe file's contract is the tool's, not ours: the gateway reads the
// envelope (version/probes) and relays each entry verbatim.  These tests pin
// both halves -- the envelope the frontend keys off, and the fact that an entry
// survives byte-for-byte so a field the tool adds later needs no gateway change.

const probeBodyOne = `{"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,` +
	`"measured":48000,"verdict":"clamped","note":"钳制","tested_at":"2026-09-15 18:30:00",` +
	`"source":"probe_max_tokens.py"}}}`

func writeProbeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	f := filepath.Join(dir, name)
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return f
}

// probeProbes digs the probes object out of a response without assuming the
// concrete entry type, so a failure prints what actually arrived.
func probeProbes(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	got := decodeMap(t, rec)
	raw, ok := got["probes"]
	if !ok {
		t.Fatalf("response has no probes field: %v", got)
	}
	probes, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("probes = %v (%T), want an object", raw, raw)
	}
	return probes
}

// TestModelProbesRelaysTheToolOutput covers the configured-and-present case: the
// envelope is 200 + exists:true + updated_at, and the entry reaches the browser
// with every field the tool wrote.
func TestModelProbesRelaysTheToolOutput(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, DefaultProbeFileName, probeBodyOne)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := get(t, h, "/panel/api/model_probes")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	if got["exists"] != true {
		t.Errorf("exists = %v, want true", got["exists"])
	}
	if got["updated_at"] == "" || got["updated_at"] == nil {
		t.Errorf("updated_at = %v, want the file's modtime", got["updated_at"])
	} else if _, err := time.Parse(time.RFC3339, got["updated_at"].(string)); err != nil {
		t.Errorf("updated_at = %v, want RFC3339: %v", got["updated_at"], err)
	}

	probes := probeProbes(t, rec)
	if len(probes) != 1 {
		t.Fatalf("probes = %v, want exactly the one entry", probes)
	}
	entry, ok := probes["cn:glm-5.2"].(map[string]any)
	if !ok {
		t.Fatalf("entry = %v (%T), want an object", probes["cn:glm-5.2"], probes["cn:glm-5.2"])
	}
	// Fields the gateway does not understand must still come through.
	if entry["verdict"] != "clamped" || entry["measured"] != float64(48000) ||
		entry["claimed"] != float64(131072) || entry["note"] != "钳制" ||
		entry["tested_at"] != "2026-09-15 18:30:00" || entry["source"] != "probe_max_tokens.py" {
		t.Errorf("entry lost or altered fields: %v", entry)
	}
}

// TestModelProbesWithoutADataIsAnEmptySet pins the deliberate non-error: both
// "not configured" and "configured but the tool has not run yet" must render an
// un-annotated table, not a failure banner over an optional extra.
func TestModelProbesWithoutADataIsAnEmptySet(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "never-written.json")

	for _, tc := range []struct {
		name string
		file string
	}{
		{"unconfigured", ""},
		{"file missing", missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(Options{Version: "test", Started: time.Now(), ProbeFile: tc.file})
			rec := get(t, h, "/panel/api/model_probes")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			got := decodeMap(t, rec)
			if got["exists"] != false {
				t.Errorf("exists = %v, want false", got["exists"])
			}
			// An empty object, not null: the frontend does pr.probes || {} and
			// the reference normalises it too.  Keeping the key present is what
			// lets a caller tell "no data" from "field renamed".
			if probes, ok := got["probes"].(map[string]any); !ok || len(probes) != 0 {
				t.Errorf("probes = %v (%T), want an empty object", got["probes"], got["probes"])
			}
			// A configured-but-absent file must stay absent: reading is not a
			// reason to create the file the probe tool owns.
			if tc.file != "" {
				if _, err := os.Stat(tc.file); !os.IsNotExist(err) {
					t.Errorf("the read-only endpoint touched %s (stat err = %v)", tc.file, err)
				}
			}
		})
	}
}

// TestModelProbesCorruptFileIsBadGateway: a file that exists but cannot be
// parsed is reported, so a broken probe run is visible instead of silently
// looking like "no probes yet".  The message is the parser's own complaint, as
// in the reference; the read path is where the filename shows up.
func TestModelProbesCorruptFileIsBadGateway(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, "broken.json", `{"version":1,"probes":{`)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := get(t, h, "/panel/api/model_probes")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)
	msg, _ := got["error"].(string)
	if !strings.Contains(msg, "parse probes") {
		t.Errorf("error = %q, want it to say the parse failed", msg)
	}
	if got["probes"] != nil || got["exists"] != nil {
		t.Errorf("a failed read must not also report a result set: %v", got)
	}
}

// TestModelProbesReflectsARerunWithoutARestart: the whole point of an
// out-of-band file is that re-running the probe tool is enough.  A cache here
// would quietly show a stale verdict until the gateway restarted.
func TestModelProbesReflectsARerunWithoutARestart(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, DefaultProbeFileName, `{"version":1,"probes":{"m1":{"measured":1000,"verdict":"at_least"}}}`)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := get(t, h, "/panel/api/model_probes")
	if probeProbes(t, rec)["m1"] == nil {
		t.Fatalf("first read lost the entry: %v", rec.Body.String())
	}

	writeProbeFile(t, dir, DefaultProbeFileName, `{"version":1,"probes":{"m2":{"measured":2000,"verdict":"clamped"}}}`)
	rec = get(t, h, "/panel/api/model_probes")
	probes := probeProbes(t, rec)
	if probes["m1"] != nil || probes["m2"] == nil {
		t.Errorf("second read = %v, want the rerun's contents", probes)
	}
}

// TestModelProbesIsGetOnly: nothing in this gateway ever writes the file the
// tool owns, so a write method must not reach the reader.  The method check
// lives in the handler, like every other route in this package: a "GET " mux
// pattern would let the shell's any-method catch-all answer a POST with a bare
// "404 page not found", which is neither the 405 the route means nor the JSON
// error shape the panel's own client parses.
func TestModelProbesIsGetOnly(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, DefaultProbeFileName, probeBodyOne)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := post(t, h, "/panel/api/model_probes")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "glm-5.2") {
		t.Errorf("POST returned probe data: %s", rec.Body.String())
	}
}

// TestModelProbesEntryCanBeAnyJSON guards the passthrough property directly: a
// probe entry is json.RawMessage on our side, so a tool that starts writing an
// array or a bare number must not turn into a 502.
func TestModelProbesEntryCanBeAnyJSON(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, DefaultProbeFileName,
		`{"version":2,"probes":{"weird":[1,2,3],"scalar":7,"nested":{"a":{"b":true}}}}`)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := get(t, h, "/panel/api/model_probes")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	probes := probeProbes(t, rec)
	if len(probes) != 3 {
		t.Fatalf("probes = %v, want all three entries", probes)
	}
	if arr, ok := probes["weird"].([]any); !ok || len(arr) != 3 {
		t.Errorf("array entry = %v (%T), want it relayed as an array", probes["weird"], probes["weird"])
	}
	if probes["scalar"] != float64(7) {
		t.Errorf("scalar entry = %v, want 7", probes["scalar"])
	}
}

// TestModelProbesBodyIsValidJSON re-decodes through json.RawMessage, i.e. it
// checks that we relayed the tool's bytes rather than re-serialising a struct
// that happens to drop what it does not know about.
func TestModelProbesBodyIsValidJSON(t *testing.T) {
	dir := t.TempDir()
	f := writeProbeFile(t, dir, DefaultProbeFileName,
		`{"version":1,"probes":{"cn:m":{"unknown_future_field":{"deep":[1,null,"x"]}}}}`)
	h := New(Options{Version: "test", Started: time.Now(), ProbeFile: f})

	rec := get(t, h, "/panel/api/model_probes")
	got := decodeMap(t, rec)
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var again struct {
		Probes map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again.Probes["cn:m"]), "unknown_future_field") {
		t.Errorf("unknown field dropped: %s", again.Probes["cn:m"])
	}
}
