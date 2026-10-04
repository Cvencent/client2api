package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func configFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client2api.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seeding config: %v", err)
	}
	return path
}

func configPanel(path string) *panel {
	return &panel{
		opts:   Options{ConfigPath: path, Version: "test", Listen: "127.0.0.1:0"},
		chores: newTaskQueues(),
	}
}

// doConfig drives the real handler and returns the recorder plus the decoded
// body when there is one.
func doConfig(t *testing.T, p *panel, method, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/panel/api/config", nil)
	} else {
		r = httptest.NewRequest(method, "/panel/api/config", strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	p.handleConfig(w, r)

	out := map[string]any{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s returned a non-JSON body %q: %v", method, w.Body.String(), err)
		}
	}
	return w, out
}

func mustSave(t *testing.T, p *panel, patch string) map[string]any {
	t.Helper()
	w, out := doConfig(t, p, http.MethodPatch, patch)
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH failed: HTTP %d body=%s", w.Code, w.Body.String())
	}
	return out
}

func onDisk(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("saved config is not valid JSON: %v\n%s", err, raw)
	}
	return m
}

const baseConfig = `{
  "listen": "127.0.0.1:8788",
  "api_key": "sk-super-secret",
  "data_dir": "data",
  "proxy": "",
  "aliases": {
    "gpt-4o": "trae/custom_model_gpt-5",
    "claude-sonnet-4": "workbuddy/claude-sonnet-4"
  },
  "disabled": [],
  "clients": { "kimi": {}, "zcode": {} }
}`

// ---------------------------------------------------------------------------
// the sentinel that makes editing a redacted document safe
// ---------------------------------------------------------------------------

// The panel can only ever show "<redacted>" where a credential is, so a save
// that echoes the form back must not write that literal over a live secret.
// This is the single most important property of the write path.
func TestConfigPatchLeavesARedactedSecretAlone(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	_, get := doConfig(t, p, http.MethodGet, "")
	cfg, ok := get["config"].(map[string]any)
	if !ok {
		t.Fatalf("GET did not return a config object: %v", get)
	}
	if got := cfg["api_key"]; got != redactedPlaceholder {
		t.Fatalf("GET should have redacted api_key, got %v", got)
	}

	// Exactly what the editor does: show the redacted document, let the human
	// change one unrelated field, send the whole thing back.
	cfg["proxy"] = "http://127.0.0.1:8888"
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustSave(t, p, string(body))

	if got := onDisk(t, path)["api_key"]; got != "sk-super-secret" {
		t.Fatalf("the real api_key was overwritten: got %v", got)
	}
	if got := onDisk(t, path)["proxy"]; got != "http://127.0.0.1:8888" {
		t.Fatalf("proxy change was not saved: got %v", got)
	}
}

// An operator has to be able to replace a secret, and to clear one: the
// sentinel means "unchanged", not "make it impossible to edit".
func TestConfigPatchCanReplaceAndClearASecret(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"api_key":"sk-br-nd-new"}`)
	if got := onDisk(t, path)["api_key"]; got != "sk-br-nd-new" {
		t.Fatalf("api_key was not replaced: got %v", got)
	}

	mustSave(t, p, `{"api_key":""}`)
	if got := onDisk(t, path)["api_key"]; got != "" {
		t.Fatalf("api_key was not cleared: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// merge semantics
// ---------------------------------------------------------------------------

func TestConfigPatchMergesOneAliasAndKeepsTheRest(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"aliases":{"glm-5.3":"zcode/GLM-5.3"}}`)

	al, ok := onDisk(t, path)["aliases"].(map[string]any)
	if !ok {
		t.Fatal("aliases is not an object")
	}
	if len(al) != 3 {
		t.Fatalf("expected 3 aliases after adding one, got %d: %v", len(al), al)
	}
	if al["gpt-4o"] != "trae/custom_model_gpt-5" {
		t.Errorf("an unrelated alias was lost: %v", al["gpt-4o"])
	}
	if al["glm-5.3"] != "zcode/GLM-5.3" {
		t.Errorf("the new alias was not written: %v", al["glm-5.3"])
	}
}

func TestConfigPatchDeletesAKeyWithNull(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"aliases":{"gpt-4o":null}}`)

	al := onDisk(t, path)["aliases"].(map[string]any)
	if _, still := al["gpt-4o"]; still {
		t.Fatalf("null did not delete the alias: %v", al)
	}
	if len(al) != 1 {
		t.Fatalf("expected exactly one alias left, got %v", al)
	}
}

func TestConfigPatchDeepMergesAClientConfig(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"clients":{"kimi":{"login_mode":"device","log_level":"debug"}}}`)
	mustSave(t, p, `{"clients":{"kimi":{"log_level":"warn"}}}`)

	clients := onDisk(t, path)["clients"].(map[string]any)
	kimi := clients["kimi"].(map[string]any)
	if kimi["login_mode"] != "device" {
		t.Errorf("a sibling key was lost on the second save: %v", kimi)
	}
	if kimi["log_level"] != "warn" {
		t.Errorf("the override did not apply: %v", kimi)
	}
	if _, ok := clients["zcode"]; !ok {
		t.Errorf("an untouched client was removed: %v", clients)
	}
}

// A whole client entry can be dropped, which is how a module goes back to its
// own defaults.
func TestConfigPatchCanRemoveAWholeClientEntry(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"clients":{"zcode":null}}`)

	clients := onDisk(t, path)["clients"].(map[string]any)
	if _, ok := clients["zcode"]; ok {
		t.Fatalf("zcode was not removed: %v", clients)
	}
	if _, ok := clients["kimi"]; !ok {
		t.Fatalf("kimi was removed as a side effect: %v", clients)
	}
}

// The panel does not own the config schema, so a key it has never heard of
// must survive a save untouched.
func TestConfigPatchKeepsUnknownTopLevelKeys(t *testing.T) {
	path := configFile(t, `{"listen":"127.0.0.1:1","future_knob":{"a":1},"aliases":{}}`)
	p := configPanel(path)

	mustSave(t, p, `{"data_dir":"elsewhere"}`)

	got := onDisk(t, path)
	if _, ok := got["future_knob"]; !ok {
		t.Fatalf("an unknown top-level key was dropped: %v", got)
	}
	if got["data_dir"] != "elsewhere" {
		t.Fatalf("the patch did not apply: %v", got)
	}
}

// ---------------------------------------------------------------------------
// validation
// ---------------------------------------------------------------------------

func TestConfigPatchRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty patch", `{}`, "empty patch"},
		{"alias without a slash", `{"aliases":{"x":"gpt-4o"}}`, "client/model"},
		{"alias with an empty target", `{"aliases":{"x":""}}`, "empty target"},
		{"alias target that is not a string", `{"aliases":{"x":42}}`, "must map to a string"},
		{"clients that is not an object", `{"clients":[]}`, "must be an object"},
		{"client config that is not an object", `{"clients":{"kimi":"device"}}`, "must be an object"},
		{"disabled that is not an array", `{"disabled":"kimi"}`, "must be an array"},
		{"disabled holding a non-string", `{"disabled":[1]}`, "only client names"},
		{"listen that is not a string", `{"listen":8788}`, "must be a string"},
		{"malformed JSON", `{"listen":`, "invalid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := configFile(t, baseConfig)
			before := onDisk(t, path)

			w, out := doConfig(t, configPanel(path), http.MethodPatch, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			if msg, _ := out["error"].(string); !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not mention %q", msg, tc.want)
			}
			// A rejected patch must not have touched the file.
			if after := onDisk(t, path); !jsonEqual(before, after) {
				t.Errorf("the file changed despite a rejected patch:\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

// A config that is already broken on disk must produce a clear error rather
// than a half-written file.
func TestConfigPatchRefusesToMergeIntoUnparseableJSON(t *testing.T) {
	path := configFile(t, `{"listen": `)
	p := configPanel(path)

	w, _ := doConfig(t, p, http.MethodPatch, `{"data_dir":"x"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if got := strings.TrimSpace(readFileString(t, path)); got != `{"listen":` {
		t.Fatalf("the broken file was rewritten: %q", got)
	}
}

// Without -config there is nowhere to save, and the panel says so instead of
// guessing a path.
func TestConfigPatchRefusesWithoutAConfigPath(t *testing.T) {
	w, out := doConfig(t, configPanel(""), http.MethodPatch, `{"data_dir":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "without a config path") {
		t.Fatalf("unhelpful error: %v", out)
	}
}

// GET keeps its old behaviour: redacted, and other verbs are still refused.
func TestConfigMethodHandling(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	w, _ := doConfig(t, p, http.MethodDelete, "")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE should be refused, got %d", w.Code)
	}

	// POST is accepted as an alias for PATCH, because a browser form or a
	// plain curl is an easy way to drive this.
	w, _ = doConfig(t, p, http.MethodPost, `{"data_dir":"via-post"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST should save, got %d: %s", w.Code, w.Body.String())
	}
	if onDisk(t, path)["data_dir"] != "via-post" {
		t.Fatalf("POST did not save: %v", onDisk(t, path))
	}
}

// ---------------------------------------------------------------------------
// the response and the file it produces
// ---------------------------------------------------------------------------

// A save reports what it wrote and, crucially, that a restart is needed:
// nothing re-reads the config at runtime, so claiming otherwise would be a lie.
func TestConfigPatchReportsChangesAndTheRestartRequirement(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path), `{"data_dir":"other"}`)

	if out["saved"] != true {
		t.Errorf("saved flag missing: %v", out)
	}
	if out["restart_required"] != true {
		t.Errorf("restart_required should be true: %v", out)
	}
	changed, _ := out["changed"].([]any)
	if len(changed) != 1 || changed[0] != "data_dir" {
		t.Fatalf("changed = %v, want [data_dir]", out["changed"])
	}
	// The response carries the new state, redacted, so the editor can re-render
	// without a second round trip.
	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("the save response has no config object: %v", out)
	}
	if cfg["api_key"] != redactedPlaceholder {
		t.Errorf("the save response leaked or lost api_key: %v", cfg["api_key"])
	}
	if cfg["data_dir"] != "other" {
		t.Errorf("the save response is stale: %v", cfg["data_dir"])
	}
}

// A no-op save is not a change, and the flag should say so.
func TestConfigPatchWithNoEffectiveChangeReportsNothing(t *testing.T) {
	path := configFile(t, baseConfig)
	out := mustSave(t, configPanel(path), `{"data_dir":"data"}`)

	if changed, _ := out["changed"].([]any); len(changed) != 0 {
		t.Fatalf("changed = %v, want empty", changed)
	}
}

// Saving the same thing twice must be byte-identical, so a "save" on an
// untouched form does not show up as a whole-file rewrite in git.
func TestConfigPatchIsByteStableOnRepeat(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"aliases":{"glm-5.3":"zcode/GLM-5.3"}}`)
	first := readFileString(t, path)

	mustSave(t, p, `{"aliases":{"glm-5.3":"zcode/GLM-5.3"}}`)
	if second := readFileString(t, path); second != first {
		t.Fatalf("a repeated save rewrote the file:\n%s\n---\n%s", first, second)
	}
}

// The file stays readable and in the documented key order rather than being
// alphabetised by the encoder.
func TestConfigPatchWritesCanonicalKeyOrder(t *testing.T) {
	path := configFile(t, baseConfig)
	mustSave(t, configPanel(path), `{"data_dir":"data"}`)

	text := readFileString(t, path)
	if !strings.HasPrefix(text, "{\n  \"listen\":") {
		t.Fatalf("listen should be the first key:\n%s", text)
	}
	if !strings.HasSuffix(text, "}\n") {
		t.Fatalf("the file should end with a newline:\n%q", text)
	}
	order := []string{"listen", "api_key", "data_dir", "proxy", "aliases", "disabled", "clients"}
	last := -1
	for _, k := range order {
		i := strings.Index(text, "\n  \""+k+"\":")
		if i < 0 {
			t.Fatalf("key %q missing from the saved file:\n%s", k, text)
		}
		if i < last {
			t.Fatalf("key %q is out of order:\n%s", k, text)
		}
		last = i
	}
}

// The process runs fine with no config file at all, so the panel must be able
// to create one.
func TestConfigPatchCreatesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "client2api.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	p := configPanel(path)

	mustSave(t, p, `{"listen":"127.0.0.1:9000","aliases":{"x":"kimi/k3"}}`)

	got := onDisk(t, path)
	if got["listen"] != "127.0.0.1:9000" {
		t.Fatalf("the new file is wrong: %v", got)
	}
}

// Concurrent saves must not interleave a read-modify-write and lose one.
func TestConfigPatchSerialisesConcurrentSaves(t *testing.T) {
	path := configFile(t, `{"aliases":{}}`)
	p := configPanel(path)

	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			body := `{"aliases":{"a` + string(rune('0'+i)) + `":"kimi/k3"}}`
			w, _ := doConfig(t, p, http.MethodPatch, body)
			if w.Code != http.StatusOK {
				t.Errorf("concurrent save failed: %d %s", w.Code, w.Body.String())
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	al := onDisk(t, path)["aliases"].(map[string]any)
	if len(al) != 8 {
		t.Fatalf("lost an alias to a race: got %d, want 8: %v", len(al), al)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 自动排程的写入校验
//
// 保存先写文件、reload 才读回，所以一个“写得进去但读不出来”的时点会留下
// 一个坏配置和一个还在跑旧时间表的进程。下面三条钉住写前的拒绝。
// ---------------------------------------------------------------------------

func TestConfigRefusesABadScheduleHour(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	w, _ := doConfig(t, p, http.MethodPatch, `{"schedule":{"checkin_hours":[25]}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with hour 25 = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "checkin_hours") {
		t.Errorf("the rejection does not name the field: %s", body)
	}
	if _, ok := onDisk(t, path)["schedule"]; ok {
		t.Error("a rejected save still wrote the schedule block")
	}
}

func TestConfigRefusesABadPerPlatformOverride(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	w, _ := doConfig(t, p, http.MethodPatch,
		`{"schedule":{"clients":{"wb":{"checkin":{"hours":"9"}}}}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a string hours = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "schedule.clients.wb.checkin.hours") {
		t.Errorf("the rejection does not point at the platform: %s", body)
	}
}

func TestConfigAcceptsPerPlatformOverrides(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)

	mustSave(t, p, `{"schedule":{"enabled":true,"clients":{"wb":{"checkin":{"enabled":true,"hours":[7,19]}}}}}`)

	sched, ok := onDisk(t, path)["schedule"].(map[string]any)
	if !ok {
		t.Fatal("the schedule block did not survive the save")
	}
	if sched["enabled"] != true {
		t.Errorf("schedule.enabled = %v, want true", sched["enabled"])
	}
	clients, _ := sched["clients"].(map[string]any)
	wb, _ := clients["wb"].(map[string]any)
	checkin, _ := wb["checkin"].(map[string]any)
	if checkin["enabled"] != true {
		t.Errorf("the override's switch was lost: %v", wb)
	}
	if got, _ := checkin["hours"].([]any); len(got) != 2 || got[0] != float64(7) || got[1] != float64(19) {
		t.Errorf("the override's hours were lost: %v", checkin["hours"])
	}
}

// TestConfigDeletesAPerPlatformOverride pins the "跟随全局" write: null on a
// (client, batch) pair must remove it, not merge into it.
func TestConfigDeletesAPerPlatformOverride(t *testing.T) {
	path := configFile(t, `{"schedule":{"enabled":true,"clients":{"wb":{"checkin":{"enabled":true,"hours":[7]}}}}}`)
	p := configPanel(path)

	mustSave(t, p, `{"schedule":{"clients":{"wb":{"checkin":null}}}}`)

	sched, _ := onDisk(t, path)["schedule"].(map[string]any)
	clients, _ := sched["clients"].(map[string]any)
	wb, _ := clients["wb"].(map[string]any)
	if _, ok := wb["checkin"]; ok {
		t.Fatalf("the override survived a null patch: %v", wb)
	}
	// The shared timetable is untouched: only the override went away.
	if sched["enabled"] != true {
		t.Errorf("the shared master switch was disturbed: %v", sched)
	}
}
