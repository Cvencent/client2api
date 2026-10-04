package opencode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// writeFile creates a file (and its parent) under a temp dir.
func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// ---------------------------------------------------------------------------
// Locating opencode's own credential store.
// ---------------------------------------------------------------------------

func TestOpencodeDataDirHonoursXDG(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	if got, want := opencodeDataDir(), filepath.Join(tmp, "opencode"); got != want {
		t.Fatalf("opencodeDataDir = %q, want %q", got, want)
	}
	if got, want := vendorAuthPath(), filepath.Join(tmp, "opencode", "auth.json"); got != want {
		t.Fatalf("vendorAuthPath = %q, want %q", got, want)
	}

	// A blank XDG_DATA_HOME must fall back to the home directory, not to "".
	t.Setenv("XDG_DATA_HOME", "   ")
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this machine")
	}
	want := filepath.Join(home, ".local", "share", "opencode")
	if got := opencodeDataDir(); got != want {
		t.Fatalf("opencodeDataDir = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// opencode's auth.json.
// ---------------------------------------------------------------------------

func TestReadVendorAuthFileAbsentIsNotAnError(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "nope", "auth.json")} {
		res := readVendorAuthFile(path)
		if res.present || len(res.recs) != 0 || res.note != "" {
			t.Fatalf("readVendorAuthFile(%q) = %+v, want the zero result", path, res)
		}
	}
}

func TestReadVendorAuthFileAPIShape(t *testing.T) {
	const key = "sk-zen-live-abcdefgh"
	path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"),
		`{"opencode":{"type":"api","key":"`+key+`","metadata":{"note":"mine"}}}`)

	res := readVendorAuthFile(path)
	if !res.present {
		t.Fatal("present = false for a readable provider map")
	}
	if len(res.recs) != 1 {
		t.Fatalf("recs = %d, want 1", len(res.recs))
	}
	rec := res.recs[0]
	if rec.APIKey != key {
		t.Fatalf("APIKey = %q, want the file's key", rec.APIKey)
	}
	if rec.ID != accountID(keyFingerprint(key)) {
		t.Fatalf("ID = %q, want the key fingerprint, not the key", rec.ID)
	}
	if strings.Contains(rec.ID, key) {
		t.Fatal("the account id embeds the raw key")
	}
	if rec.Source != sourceImport || !rec.Enabled {
		t.Fatalf("record = %+v, want an enabled imported record", rec)
	}
	// The note goes to the panel, so it must not carry the key.
	if strings.Contains(res.note, key) {
		t.Fatalf("note = %q, must not carry the key", res.note)
	}
	if !strings.Contains(res.note, core.MaskSecret(key)) {
		t.Fatalf("note = %q, want the masked key", res.note)
	}
}

func TestReadVendorAuthFileIgnoresOtherProviders(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"),
		`{"anthropic":{"type":"api","key":"sk-ant-1"},"openai":{"type":"api","key":"sk-oai-1"}}`)

	res := readVendorAuthFile(path)
	if !res.present {
		t.Fatal("present = false")
	}
	if len(res.recs) != 0 {
		t.Fatalf("recs = %+v, want none: no opencode provider is present", res.recs)
	}
	if !strings.Contains(res.note, `no "opencode" provider`) {
		t.Fatalf("note = %q, want it to explain the miss", res.note)
	}
	// The operator needs to know what WAS found, so the miss is diagnosable.
	if !strings.Contains(res.note, "anthropic") || !strings.Contains(res.note, "openai") {
		t.Fatalf("note = %q, want it to list the providers that were found", res.note)
	}
}

// Zen has no refresh flow, so an oauth entry could never be renewed here:
// importing it would create an account that fails forever.
func TestReadVendorAuthFileOAuthIsReportedNotImported(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"),
		`{"opencode":{"type":"oauth","access":"a-token","refresh":"r-token","expires":1790000000}}`)

	res := readVendorAuthFile(path)
	if len(res.recs) != 0 {
		t.Fatalf("recs = %+v, want none for an oauth entry", res.recs)
	}
	if !strings.Contains(res.note, "oauth") || !strings.Contains(res.note, "refresh") {
		t.Fatalf("note = %q, want it to explain why oauth is not imported", res.note)
	}
}

func TestReadVendorAuthFileEdgeShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantNote string
	}{
		{"blank key", `{"opencode":{"type":"api","key":"  "}}`, "carries no key"},
		{"unknown type", `{"opencode":{"type":"cookie","key":"k"}}`, "unsupported type"},
		{"no type", `{"opencode":{"key":"k"}}`, "unsupported type"},
		{"empty object", `{}`, "no providers"},
		{"broken json", `{not json`, "not a JSON provider map"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"), tc.body)
			res := readVendorAuthFile(path)
			if !res.present {
				t.Fatal("present = false for a readable file")
			}
			if len(res.recs) != 0 {
				t.Fatalf("recs = %+v, want none", res.recs)
			}
			if !strings.Contains(res.note, tc.wantNote) {
				t.Fatalf("note = %q, want it to contain %q", res.note, tc.wantNote)
			}
		})
	}
}

func TestReadVendorAuthFileProviderIDIsCaseInsensitive(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"),
		`{"OpenCode":{"type":"API","key":"sk-mixed-1"}}`)

	res := readVendorAuthFile(path)
	if len(res.recs) != 1 {
		t.Fatalf("recs = %+v, want the entry matched case-insensitively", res.recs)
	}
	if res.recs[0].APIKey != "sk-mixed-1" {
		t.Fatalf("APIKey = %q", res.recs[0].APIKey)
	}
}

// ---------------------------------------------------------------------------
// Candidate precedence.
// ---------------------------------------------------------------------------

func TestCandidatesPrecedenceAndDedupe(t *testing.T) {
	c := newTestClient(t, Config{
		APIKey: "sk-config-1",
		Accounts: []AccountConfig{
			{APIKey: "sk-config-2", Label: "second"},
			{Key: "sk-config-1"}, // same key as the top-level one
			{APIKey: "   "},      // blank: dropped
		},
	})
	// Set the environment AFTER the client is built: newTestClient blanks
	// apiKeyEnv so a test never picks up the machine's real key.
	t.Setenv(apiKeyEnv, "sk-env-1")

	got := c.candidates()
	want := []struct{ key, source string }{
		{"sk-config-1", sourceConfig},
		{"sk-config-2", sourceConfig},
		{"sk-env-1", sourceEnv},
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %+v, want %d entries", got, len(want))
	}
	for i, w := range want {
		if got[i].key != w.key || got[i].source != w.source {
			t.Fatalf("candidate %d = %+v, want key %q source %q", i, got[i], w.key, w.source)
		}
	}
	if got[1].label != "second" {
		t.Fatalf("label = %q, want the configured label", got[1].label)
	}
	if got[2].label != apiKeyEnv {
		t.Fatalf("label = %q, want the env var name", got[2].label)
	}
}

func TestCandidatesWithoutAnySource(t *testing.T) {
	t.Setenv(apiKeyEnv, "")
	c := newTestClient(t, Config{})
	if got := c.candidates(); len(got) != 0 {
		t.Fatalf("candidates = %+v, want none", got)
	}
}

// The config account label defaults to its position so the panel can tell two
// unlabelled keys apart.
func TestCandidatesLabelsUnlabelledAccounts(t *testing.T) {
	t.Setenv(apiKeyEnv, "")
	c := newTestClient(t, Config{Accounts: []AccountConfig{{APIKey: "sk-a"}, {APIKey: "sk-b"}}})
	got := c.candidates()
	if len(got) != 2 || got[0].label != "config account 1" || got[1].label != "config account 2" {
		t.Fatalf("candidates = %+v", got)
	}
}

func TestVendorAuthPathForHonoursTheDisableFlag(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	on := newTestClient(t, Config{})
	if got := on.vendorAuthPathFor(); got == "" || !strings.HasSuffix(got, "auth.json") {
		t.Fatalf("vendorAuthPathFor = %q, want the auth.json path", got)
	}
	off := newTestClient(t, Config{DisableAuthJSONDiscovery: true}.normalize())
	if got := off.vendorAuthPathFor(); got != "" {
		t.Fatalf("vendorAuthPathFor = %q, want it disabled", got)
	}
	if got := off.vendorCandidates(); len(got) != 0 {
		t.Fatalf("vendorCandidates = %+v, want none when discovery is disabled", got)
	}
}

// ---------------------------------------------------------------------------
// core.CredentialImporter.
// ---------------------------------------------------------------------------

// Discover must be read-only, and the vendor file must be reported with a note
// the operator can act on.
func TestDiscoverIsReadOnlyAndReportsTheVendorFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	authPath := writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"api","key":"sk-zen-1"}}`)

	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover = %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("Discover = %+v, want just the vendor file", found)
	}
	d := found[0]
	if d.Path != authPath || d.Kind != "opencode-auth" {
		t.Fatalf("entry = %+v", d)
	}
	if !d.Importable || d.Imported {
		t.Fatalf("entry = %+v, want importable and not yet imported", d)
	}
	if !strings.Contains(d.Note, core.MaskSecret("sk-zen-1")) {
		t.Fatalf("note = %q, want the masked key", d.Note)
	}
	// Read-only: no account was created and nothing was written.
	if c.pool.size() != 0 {
		t.Fatalf("Discover created %d accounts", c.pool.size())
	}
	if _, err := os.Stat(filepath.Join(c.deps.DataDir, credentialsFile)); !os.IsNotExist(err) {
		t.Fatalf("Discover wrote the credential store: %v", err)
	}
}

func TestDiscoverThenImportMarksItImported(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	authPath := writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"api","key":"sk-zen-2"}}`)

	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	recs, err := c.Import(context.Background(), []string{authPath}, false)
	if err != nil {
		t.Fatalf("Import = %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Import returned %d accounts, want 1", len(recs))
	}
	if recs[0].Identity != "" {
		t.Fatalf("Identity = %q, want it empty: Zen's key is not an account id", recs[0].Identity)
	}

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover = %v", err)
	}
	if len(found) != 1 || !found[0].Imported {
		t.Fatalf("Discover = %+v, want the entry marked imported", found)
	}
}

func TestDiscoverScansTheImportDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	good := writeFile(t, filepath.Join(c.deps.DataDir, importDirName, "good.json"),
		`[{"api_key":"sk-import-1"}]`)
	writeFile(t, filepath.Join(c.deps.DataDir, importDirName, "broken.json"), `{not json`)
	// A non-JSON leftover must be ignored rather than reported as broken.
	writeFile(t, filepath.Join(c.deps.DataDir, importDirName, "notes.txt"), `hello`)

	found, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover = %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("Discover = %+v, want the two .json files", found)
	}
	byPath := map[string]core.DiscoveredCredential{}
	for _, d := range found {
		byPath[d.Path] = d
	}
	if g := byPath[good]; g.Kind != "opencode-json" || !g.Importable {
		t.Fatalf("good.json = %+v", g)
	}
	broken := byPath[filepath.Join(c.deps.DataDir, importDirName, "broken.json")]
	if broken.Importable || !strings.Contains(broken.Note, "unreadable") {
		t.Fatalf("broken.json = %+v", broken)
	}
}

// One bad leftover must not hide the good ones.
func TestImportAllSkipsABrokenFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	writeFile(t, filepath.Join(c.deps.DataDir, importDirName, "good.json"), `[{"api_key":"sk-good-1"}]`)
	writeFile(t, filepath.Join(c.deps.DataDir, importDirName, "broken.json"), `{not json`)

	recs, err := c.Import(context.Background(), nil, true)
	if err != nil {
		t.Fatalf("Import(all) = %v, want the sweep to skip the broken file", err)
	}
	if len(recs) != 1 {
		t.Fatalf("Import(all) = %d accounts, want the one good key", len(recs))
	}
}

func TestImportNoPathsIsANoOp(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	recs, err := c.Import(context.Background(), nil, false)
	if err != nil || recs != nil {
		t.Fatalf("Import(nil, false) = %+v, %v; want nil, nil", recs, err)
	}
	if c.pool.size() != 0 {
		t.Fatal("Import(nil, false) changed the pool")
	}
}

// A path the operator named explicitly gets its real error.
func TestImportExplicitPathReportsItsError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})
	c.deps.DataDir = t.TempDir()

	missing := filepath.Join(t.TempDir(), "absent.json")
	if _, err := c.Import(context.Background(), []string{missing}, false); err == nil {
		t.Fatal("Import reported no error for a missing explicit path")
	}

	blank := writeFile(t, filepath.Join(t.TempDir(), "blank.json"), `[{"label":"no key here"}]`)
	_, err := c.Import(context.Background(), []string{blank}, false)
	if err == nil || !strings.Contains(err.Error(), "carries no usable credential") {
		t.Fatalf("Import(keyless file) = %v", err)
	}
}

// ---------------------------------------------------------------------------
// recordsFromFile.
// ---------------------------------------------------------------------------

func TestRecordsFromFileStampsIDAndSource(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	path := writeFile(t, filepath.Join(t.TempDir(), "creds.json"),
		`[{"api_key":"sk-plain-1","label":"mine"},{"label":"no key"}]`)
	recs, err := c.recordsFromFile(path)
	if err != nil {
		t.Fatalf("recordsFromFile = %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("recs = %+v, want only the keyed record", recs)
	}
	if recs[0].ID != accountID(keyFingerprint("sk-plain-1")) {
		t.Fatalf("ID = %q, want a stamped fingerprint", recs[0].ID)
	}
	if recs[0].Source != sourceImport {
		t.Fatalf("Source = %q, want %q", recs[0].Source, sourceImport)
	}
	if recs[0].Label != "mine" {
		t.Fatalf("Label = %q, want the file's label preserved", recs[0].Label)
	}
}

func TestRecordsFromFileRoutesTheVendorAuthFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)
	c := newTestClient(t, Config{})

	// A vendor auth.json is a provider MAP, which decodeCredentialsBody would
	// misread as a single record; routing by path is what makes it work.
	path := writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"api","key":"sk-zen-3"}}`)
	recs, err := c.recordsFromFile(path)
	if err != nil {
		t.Fatalf("recordsFromFile = %v", err)
	}
	if len(recs) != 1 || recs[0].APIKey != "sk-zen-3" || recs[0].Source != sourceImport {
		t.Fatalf("recs = %+v", recs)
	}

	// An oauth-only vendor file has nothing usable and must say so.
	oauth := writeFile(t, filepath.Join(tmp, "opencode", "auth.json"),
		`{"opencode":{"type":"oauth","access":"a","refresh":"r"}}`)
	_, err = c.recordsFromFile(oauth)
	if err == nil || !strings.Contains(err.Error(), "oauth") {
		t.Fatalf("recordsFromFile(oauth) = %v, want the oauth explanation", err)
	}
}

func TestRecordsFromFileRejectsEmptyAndKeyless(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	if _, err := c.recordsFromFile(""); err == nil {
		t.Fatal("recordsFromFile(\"\") reported no error")
	}
	empty := writeFile(t, filepath.Join(t.TempDir(), "empty.json"), `{}`)
	if _, err := c.recordsFromFile(empty); err == nil {
		t.Fatal("recordsFromFile({}) reported no error")
	}
}

func TestSamePath(t *testing.T) {
	if !samePath(`C:\a\b\c.json`, `C:\a\b\c.json`) {
		t.Fatal("samePath missed an identical path")
	}
	if samePath("/a/b.json", "/a/c.json") {
		t.Fatal("samePath matched different paths")
	}
}

// A JSON body the module wrote itself must round-trip through recordsFromFile,
// so a credential survives a restart.
func TestRecordsFromFileReadsTheModulesOwnStore(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := newTestClient(t, Config{})

	path := filepath.Join(t.TempDir(), "accounts.json")
	recs := []accountRecord{{
		ID:      "opencode:abc123",
		Label:   "panel",
		APIKey:  "sk-stored-1",
		Enabled: true,
		Source:  sourceImport,
	}}
	if err := saveCredentials(path, recs); err != nil {
		t.Fatalf("saveCredentials = %v", err)
	}
	got, err := c.recordsFromFile(path)
	if err != nil {
		t.Fatalf("recordsFromFile = %v", err)
	}
	if len(got) != 1 || got[0].APIKey != "sk-stored-1" || got[0].ID != "opencode:abc123" {
		t.Fatalf("got = %+v", got)
	}
}

// The vendor map is a JSON object keyed by provider, and the module must not
// confuse it with its own {version,accounts} store.
func TestVendorAuthFileIsNotTheModuleStore(t *testing.T) {
	const body = `{"opencode":{"type":"api","key":"sk-zen-4"}}`
	path := writeFile(t, filepath.Join(t.TempDir(), "auth.json"), body)

	// The raw decoder does not understand the provider map...
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["accounts"]; ok {
		t.Fatal("the vendor map looks like the module store")
	}
	// ...but readVendorAuthFile does.
	if res := readVendorAuthFile(path); len(res.recs) != 1 {
		t.Fatalf("readVendorAuthFile = %+v, want the vendor map understood", res)
	}
}
