package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bundle_test.go covers the whole-instance snapshot: what an export picks up,
// what it leaves behind, and what an import refuses before it writes anything.

// writeBundleFile seeds one file, creating parents, the way a module would.
func writeBundleFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// bundlePanel builds the smallest panel the two handlers need.
func bundlePanel(dataDir, configPath string) *panel {
	return &panel{
		opts:   Options{DataDir: dataDir, ConfigPath: configPath, Version: "test", Listen: "127.0.0.1:0"},
		chores: newTaskQueues(),
	}
}

// bundleExport calls GET /panel/api/bundle and decodes the document.
func bundleExport(t *testing.T, p *panel) (*httptest.ResponseRecorder, bundleDoc) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handleBundle(rec, httptest.NewRequest(http.MethodGet, "/panel/api/bundle", nil))
	var doc bundleDoc
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("export returned a non-bundle body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, doc
}

// bundleImport posts a document at the handler and decodes the reply.
func bundleImport(t *testing.T, p *panel, doc bundleDoc) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/bundle", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.handleBundle(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("import returned a non-JSON body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, out
}

// bundleSourceDir lays out the credential and non-credential files the export
// has to tell apart.
func bundleSourceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeBundleFile(t, filepath.Join(dir, "cline", "accounts.json"), `{"accounts":[{"id":"a1","access_token":"tok-cline"}]}`)
	writeBundleFile(t, filepath.Join(dir, "cline", "state.json"), `{"accounts":{"a1":{"failures":3}}}`)
	writeBundleFile(t, filepath.Join(dir, "workbuddy", "wb-1.json"), `{"uid":"wb-1","access_token":"tok-wb"}`)
	writeBundleFile(t, filepath.Join(dir, "workbuddy", "pool.json"), `{"round_robin":0}`)
	writeBundleFile(t, filepath.Join(dir, "workbuddy", "cache", "models.json"), `{"models":[]}`)
	writeBundleFile(t, filepath.Join(dir, "openrouter", "credentials.json"), `{"api_key":"sk-or-1"}`)
	writeBundleFile(t, filepath.Join(dir, "usage.json"), `{"requests":999}`)
	return dir
}

func bundleSourceConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client2api.json")
	writeBundleFile(t, path, `{
  "listen": "127.0.0.1:8788",
  "api_key": "sk-inbound-1",
  "data_dir": "data",
  "proxy": "http://proxy.example:3128",
  "platforms": {"cline": {"priority": 2, "disabled_models": ["stealth/space-bunny-alpha"]}},
  "disabled": ["loomy"],
  "schedule": {"enabled": true, "checkin_hours": [9, 21]},
  "pool": {"max_in_flight": 3, "prefer_expiring": true},
  "cooldown": {"soft_rate": "600s"},
  "prompt": {"mode": "append", "file": "prompt.md"},
  "session_sticky": {"enabled": true, "ttl": "30m"},
  "features": {"sanitize_blacklist_fingerprints": true},
  "panel": {"package_detail_limit": 3},
  "aliases": {"glm-5.3": "zcode/GLM-5.3"},
  "clients": {"openrouter": {"free_only": true}}
}`)
	return path
}

func TestBundleExportCarriesCredentialFilesAndSkipsHistory(t *testing.T) {
	src := bundleSourceDir(t)
	rec, doc := bundleExport(t, bundlePanel(src, bundleSourceConfig(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("export failed: HTTP %d body=%s", rec.Code, rec.Body.String())
	}
	if doc.Kind != bundleKind || doc.Version != bundleVersion {
		t.Fatalf("export header = %q v%d, want %q v%d", doc.Kind, doc.Version, bundleKind, bundleVersion)
	}
	got := map[string]string{}
	for _, f := range doc.Files {
		got[f.Client+"/"+f.Name] = string(f.Data)
	}
	want := map[string]string{
		"cline/accounts.json":         `{"accounts":[{"id":"a1","access_token":"tok-cline"}]}`,
		"openrouter/credentials.json": `{"api_key":"sk-or-1"}`,
		"workbuddy/wb-1.json":         `{"uid":"wb-1","access_token":"tok-wb"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("exported %d files (%v), want %d", len(got), bundleKeys(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("exported %s = %q, want %q", k, got[k], v)
		}
	}
	for _, unwanted := range []string{"cline/state.json", "workbuddy/pool.json", "workbuddy/cache/models.json", "usage.json"} {
		if _, ok := got[unwanted]; ok {
			t.Errorf("export carried %s, which is state or history, not an account", unwanted)
		}
	}
	if len(doc.Source.Clients) != 3 {
		t.Errorf("source.clients = %v, want the three modules that own credentials", doc.Source.Clients)
	}
	// The whole config travels, not a platform-policy subset: a restore that
	// brought the accounts back but left the pool tuning, prompt policy and
	// inbound key at the target's defaults is not the same instance.
	if _, ok := doc.Config["platforms"]; !ok {
		t.Errorf("export dropped the platforms block: %v", doc.Config)
	}
	for key, want := range map[string]any{
		"listen":  "127.0.0.1:8788",
		"api_key": "sk-inbound-1",
		"proxy":   "http://proxy.example:3128",
	} {
		if got := doc.Config[key]; got != want {
			t.Errorf("export %s = %v, want %v", key, got, want)
		}
	}
	for _, key := range []string{"pool", "cooldown", "prompt", "session_sticky", "features", "panel"} {
		if _, ok := doc.Config[key]; !ok {
			t.Errorf("export dropped the %s section: %v", key, doc.Config)
		}
	}
	if pool, _ := doc.Config["pool"].(map[string]any); pool["max_in_flight"] != float64(3) {
		t.Errorf("export mangled pool settings: %v", doc.Config["pool"])
	}
	if disabled, ok := doc.Config["disabled"].([]any); !ok || len(disabled) != 1 {
		t.Errorf("export dropped disabled[]: %v", doc.Config["disabled"])
	}
	// Aliases and per-module settings are what make the restored instance
	// behave like the one that exported it, so they travel with the policy.
	if aliases, ok := doc.Config["aliases"].(map[string]any); !ok || aliases["glm-5.3"] != "zcode/GLM-5.3" {
		t.Errorf("export dropped aliases: %v", doc.Config["aliases"])
	}
	if clients, ok := doc.Config["clients"].(map[string]any); !ok || clients["openrouter"] == nil {
		t.Errorf("export dropped per-client settings: %v", doc.Config["clients"])
	}
	// A download of credentials must not be cacheable anywhere.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, bundleFilePrefix) {
		t.Errorf("Content-Disposition = %q, want a file name built from %q", cd, bundleFilePrefix)
	}
}

func bundleKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBundleExportRefusesWithoutADataDir(t *testing.T) {
	rec, _ := bundleExport(t, bundlePanel("", bundleSourceConfig(t)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("export without a data dir = HTTP %d, want 400", rec.Code)
	}
}

func TestBundleImportRestoresAccountsPlatformsAndBacksUp(t *testing.T) {
	src := bundleSourceDir(t)
	_, doc := bundleExport(t, bundlePanel(src, bundleSourceConfig(t)))

	dst := t.TempDir()
	writeBundleFile(t, filepath.Join(dst, "cline", "accounts.json"), `{"accounts":[{"id":"old"}]}`)
	cfg := filepath.Join(t.TempDir(), "client2api.json")
	writeBundleFile(t, cfg, `{"listen":"127.0.0.1:9999","data_dir":"data","platforms":{"cline":{"priority":9}},"unknown_future_section":{"keep":true}}`)

	p := bundlePanel(dst, cfg)
	reloads := 0
	p.opts.Reload = func() error { reloads++; return nil }

	rec, out := bundleImport(t, p, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("import failed: HTTP %d body=%s", rec.Code, rec.Body.String())
	}
	if out["ok"] != true {
		t.Fatalf("import reported failure: %v", out)
	}
	if out["restart_required"] != true {
		t.Errorf("restart_required = %v, want true: modules read their store at startup", out["restart_required"])
	}
	if out["reloaded"] != true || reloads != 1 {
		t.Errorf("reloaded=%v reloads=%d, want the platform policy hot-applied once", out["reloaded"], reloads)
	}

	// Every credential landed byte for byte.
	for _, f := range doc.Files {
		raw, err := os.ReadFile(filepath.Join(dst, f.Client, f.Name))
		if err != nil {
			t.Fatalf("reading restored %s/%s: %v", f.Client, f.Name, err)
		}
		if string(raw) != string(f.Data) {
			t.Errorf("restored %s/%s = %q, want %q", f.Client, f.Name, raw, f.Data)
		}
		// Windows has no POSIX mode bits: os.Chmod and Mode().Perm() both
		// collapse to read-only-or-not there, so the check only means something
		// on the platforms whose credential files actually carry a mode.
		if runtime.GOOS != "windows" {
			info, err := os.Stat(filepath.Join(dst, f.Client, f.Name))
			if err != nil {
				t.Fatalf("stat restored %s/%s: %v", f.Client, f.Name, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Errorf("%s/%s mode = %v, want 0600", f.Client, f.Name, perm)
			}
		}
	}
	// State and history were not restored over the target's own.
	if _, err := os.Stat(filepath.Join(dst, "cline", "state.json")); !os.IsNotExist(err) {
		t.Errorf("import created cline/state.json; state must not travel")
	}

	// The overwritten file is recoverable.
	backup, _ := out["backup"].(string)
	if backup == "" {
		t.Fatalf("import did not report a backup directory: %v", out)
	}
	old, err := os.ReadFile(filepath.Join(backup, "cline", "accounts.json"))
	if err != nil {
		t.Fatalf("reading the backup: %v", err)
	}
	if string(old) != `{"accounts":[{"id":"old"}]}` {
		t.Errorf("backup holds %q, want the file that was replaced", old)
	}

	// The whole config file is merged: the bundle's own settings win, and a key
	// the snapshot knows nothing about is left exactly as it was on disk.
	merged := map[string]any{}
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("reading merged config: %v", err)
	}
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("merged config is not JSON: %v", err)
	}
	if merged["listen"] != "127.0.0.1:8788" {
		t.Errorf("import did not carry the bundle's listen: %v", merged["listen"])
	}
	if merged["api_key"] != "sk-inbound-1" || merged["proxy"] != "http://proxy.example:3128" {
		t.Errorf("import dropped api_key/proxy: %v", merged)
	}
	if _, ok := merged["unknown_future_section"]; !ok {
		t.Errorf("import blanked a key the bundle knows nothing about: %v", merged)
	}
	for _, key := range []string{"pool", "cooldown", "prompt", "session_sticky", "features", "panel"} {
		if merged[key] == nil {
			t.Errorf("import dropped the %s section: %v", key, merged)
		}
	}
	if pool, _ := merged["pool"].(map[string]any); pool["prefer_expiring"] != true {
		t.Errorf("import mangled pool settings: %v", merged["pool"])
	}
	plats, _ := merged["platforms"].(map[string]any)
	cline, _ := plats["cline"].(map[string]any)
	if cline == nil || cline["priority"] != float64(2) {
		t.Errorf("platforms.cline = %v, want priority 2 from the bundle", plats["cline"])
	}
	if merged["disabled"] == nil || merged["schedule"] == nil {
		t.Errorf("import dropped disabled/schedule: %v", merged)
	}
	aliases, _ := merged["aliases"].(map[string]any)
	if aliases == nil || aliases["glm-5.3"] != "zcode/GLM-5.3" {
		t.Errorf("import dropped aliases: %v", merged["aliases"])
	}
	clients, _ := merged["clients"].(map[string]any)
	or, _ := clients["openrouter"].(map[string]any)
	if or == nil || or["free_only"] != true {
		t.Errorf("import dropped per-client settings: %v", merged["clients"])
	}
}

func TestBundleImportRejectsUnsafeOrUnknownEntriesWithoutWriting(t *testing.T) {
	cases := []struct {
		name string
		doc  bundleDoc
	}{
		{
			name: "path traversal in client",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "../evil", Name: "accounts.json", Data: []byte("{}")},
			}},
		},
		{
			name: "separator in file name",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: "sub/accounts.json", Data: []byte("{}")},
			}},
		},
		{
			name: "absolute path",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: `c:\windows\evil.json`, Data: []byte("{}")},
			}},
		},
		{
			name: "not a credential file",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: "state.json", Data: []byte("{}")},
			}},
		},
		{
			name: "checksum mismatch",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: "accounts.json", SHA256: strings.Repeat("0", 64), Data: []byte("{}")},
			}},
		},
		{
			name: "size mismatch",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: "accounts.json", Size: 99, Data: []byte("{}")},
			}},
		},
		{
			name: "wrong kind",
			doc:  bundleDoc{Kind: "something.else", Version: bundleVersion},
		},
		{
			name: "newer version",
			doc:  bundleDoc{Kind: bundleKind, Version: bundleVersion + 1},
		},
		{
			name: "same file twice",
			doc: bundleDoc{Kind: bundleKind, Version: bundleVersion, Files: []bundleFile{
				{Client: "cline", Name: "accounts.json", Data: []byte("{}")},
				{Client: "cline", Name: "ACCOUNTS.JSON", Data: []byte("{}")},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := t.TempDir()
			cfg := filepath.Join(t.TempDir(), "client2api.json")
			writeBundleFile(t, cfg, `{"listen":"127.0.0.1:1"}`)
			p := bundlePanel(dst, cfg)
			reloads := 0
			p.opts.Reload = func() error { reloads++; return nil }
			rec, _ := bundleImport(t, p, tc.doc)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("import = HTTP %d (%s), want 400", rec.Code, rec.Body.String())
			}
			if reloads != 0 {
				t.Errorf("a refused import still hot-reloaded the config")
			}
			entries, err := os.ReadDir(dst)
			if err != nil {
				t.Fatalf("reading data dir: %v", err)
			}
			for _, e := range entries {
				if e.Name() == "backups" {
					continue
				}
				t.Errorf("refused import wrote %s", e.Name())
			}
			// The config file is untouched too.
			raw, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatalf("reading config: %v", err)
			}
			if string(raw) != `{"listen":"127.0.0.1:1"}` {
				t.Errorf("refused import rewrote the config: %s", raw)
			}
		})
	}
}

func TestBundleImportRestoresAccountsWithoutAConfigPath(t *testing.T) {
	src := bundleSourceDir(t)
	_, doc := bundleExport(t, bundlePanel(src, bundleSourceConfig(t)))

	dst := t.TempDir()
	p := bundlePanel(dst, "")
	rec, out := bundleImport(t, p, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("import without a config path = HTTP %d (%s), want 200", rec.Code, rec.Body.String())
	}
	cfg, _ := out["config"].(map[string]any)
	if applied, _ := cfg["applied"].(bool); applied {
		t.Errorf("config reported applied with no config path: %v", cfg)
	}
	warnings, _ := out["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if s, _ := w.(string); strings.Contains(s, "config path") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warning about the unapplied config, got %v", out["warnings"])
	}
	if _, err := os.Stat(filepath.Join(dst, "cline", "accounts.json")); err != nil {
		t.Errorf("accounts were not restored: %v", err)
	}
}

// TestBundleImportReportsARestartForColdKeysAlone pins the case where nothing
// is written to the data directory but the config still cannot be live: an
// import of listen/proxy/clients with no accounts must not claim success that
// only a restart can deliver.
func TestBundleImportReportsARestartForColdKeysAlone(t *testing.T) {
	dst := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "client2api.json")
	writeBundleFile(t, cfg, `{"listen":"127.0.0.1:9999"}`)
	p := bundlePanel(dst, cfg)
	p.opts.Reload = func() error { return nil }

	// Cold keys only: nothing for the hot reload to apply.
	rec, out := bundleImport(t, p, bundleDoc{
		Kind: bundleKind, Version: bundleVersion,
		Config: map[string]any{"listen": "127.0.0.1:8788", "clients": map[string]any{"cline": map[string]any{}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("import = HTTP %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if out["restart_required"] != true {
		t.Errorf("restart_required = %v, want true for listen/clients", out["restart_required"])
	}

	// A bundle of only hot keys is live: no restart should be asked for.
	rec2, out2 := bundleImport(t, p, bundleDoc{
		Kind: bundleKind, Version: bundleVersion,
		Config: map[string]any{"pool": map[string]any{"max_in_flight": 2}},
	})
	if rec2.Code != http.StatusOK {
		t.Fatalf("hot-only import = HTTP %d (%s), want 200", rec2.Code, rec2.Body.String())
	}
	if out2["restart_required"] != false {
		t.Errorf("restart_required = %v, want false for a hot-only bundle", out2["restart_required"])
	}
}

// TestBundleColdKeysMatchThePanel guards the one rule that is written down
// twice: which config keys only take effect after a restart.  The importer
// reports it as restart_required and the config page reports it after a save;
// a key added to one list and not the other makes one of the two lie.
func TestBundleColdKeysMatchThePanel(t *testing.T) {
	ui := poolStatsUISource(t)
	start := strings.Index(ui, "const CFG_RESTART_KEYS = {")
	if start < 0 {
		t.Fatal("index.html has no CFG_RESTART_KEYS to compare against")
	}
	rest := ui[start:]
	end := strings.Index(rest, "};")
	if end < 0 {
		t.Fatal("CFG_RESTART_KEYS looks unterminated")
	}
	body := strings.TrimPrefix(rest[:end], "const CFG_RESTART_KEYS = {")
	uiKeys := map[string]bool{}
	for _, entry := range strings.Split(body, ",") {
		entry = strings.TrimSpace(strings.Trim(entry, "{} "))
		if entry == "" {
			continue
		}
		name, _, ok := strings.Cut(entry, ":")
		if !ok {
			t.Fatalf("cannot parse CFG_RESTART_KEYS entry %q", entry)
		}
		uiKeys[strings.TrimSpace(name)] = true
	}
	if len(uiKeys) == 0 {
		t.Fatal("parsed no keys out of CFG_RESTART_KEYS")
	}
	for k := range coldConfigKeys {
		if !uiKeys[k] {
			t.Errorf("coldConfigKeys has %q but CFG_RESTART_KEYS does not: the importer would ask for a restart the panel never mentions", k)
		}
	}
	for k := range uiKeys {
		if !coldConfigKeys[k] {
			t.Errorf("CFG_RESTART_KEYS has %q but coldConfigKeys does not: a save would promise a hot reload the importer calls cold", k)
		}
	}
}
