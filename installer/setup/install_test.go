//go:build windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// TestOperatorStatePaths pins the rule that decides what an upgrade may
// overwrite.  Getting this wrong is how an operator loses the platform routing
// config, the account pool or the usage history just by installing a new setup
// .exe over a working install.
func TestOperatorStatePaths(t *testing.T) {
	cases := []struct {
		rel  string
		keep bool
		data bool
	}{
		{"configs/client2api.json", true, false},
		{`configs\client2api.json`, true, false},
		{"configs/client2api.example.json", false, false},
		{"data", true, true},
		{"data/usage.json", true, true},
		{"data/cline/accounts.json", true, true},
		{`data\cline\accounts.json`, true, true},
		{"client2api.exe", false, false},
		{"probe.exe", false, false},
		{"README.md", false, false},
		{"configs", false, false},
		{"database/x.json", false, false},
	}
	for _, c := range cases {
		if got := isOperatorState(c.rel); got != c.keep {
			t.Errorf("isOperatorState(%q) = %v, want %v", c.rel, got, c.keep)
		}
		if got := isAccountData(c.rel); got != c.data {
			t.Errorf("isAccountData(%q) = %v, want %v", c.rel, got, c.data)
		}
	}
}

// TestWritePayloadPreservesExistingOperatorState is the end-to-end form of the
// rule above: installing over a directory that already holds a live config
// must replace the program and leave that config exactly as it was found.
func TestWritePayloadPreservesExistingOperatorState(t *testing.T) {
	dir := t.TempDir()

	const live = `{"listen":"127.0.0.1:9999","pool":{"prefer_expiring":true}}`
	configPath := filepath.Join(dir, "configs", "client2api.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}

	// A program file must still be replaced; only operator state is sticky.
	exePath := filepath.Join(dir, appExe)
	if err := os.WriteFile(exePath, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}

	written, preserved, err := writePayload(dir, true)
	if err != nil {
		t.Fatalf("writePayload: %v", err)
	}
	if preserved < 1 {
		t.Fatalf("preserved = %d, want at least the live config", preserved)
	}
	if written == 0 {
		t.Fatal("written = 0, want the program files to be copied")
	}

	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != live {
		t.Errorf("configs/client2api.json was overwritten by the payload\n got: %s\nwant: %s", got, live)
	}

	stale, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(stale) == "stale" {
		t.Error("client2api.exe was not replaced by the payload")
	}
}

// TestWritePayloadSeedsFreshInstall covers the other half: with nothing on
// disk there is no operator state to protect, so the packaged config lands and
// a program-only package still seeds the config but no data tree.
func TestWritePayloadSeedsFreshInstall(t *testing.T) {
	for _, includeData := range []bool{true, false} {
		dir := t.TempDir()
		written, preserved, err := writePayload(dir, includeData)
		if err != nil {
			t.Fatalf("writePayload(includeData=%v): %v", includeData, err)
		}
		if preserved != 0 {
			t.Errorf("includeData=%v: preserved = %d on a fresh directory, want 0", includeData, preserved)
		}
		if written == 0 {
			t.Errorf("includeData=%v: written = 0", includeData)
		}
		if _, err := os.Stat(filepath.Join(dir, "configs", "client2api.json")); err != nil {
			t.Errorf("includeData=%v: fresh install did not seed configs/client2api.json: %v", includeData, err)
		}
		if !includeData {
			requireNoDataFiles(t, dir)
		}
	}
}

// TestCopyPayloadKeepsExistingAccountPool covers the shape the packaged payload
// only has when the build staged data/: an existing account pool and usage
// history survive the copy, while program data such as the example config is
// refreshed.
func TestCopyPayloadKeepsExistingAccountPool(t *testing.T) {
	fake := fstest.MapFS{
		"client2api.exe":                  {Data: []byte("packaged exe")},
		"configs/client2api.json":         {Data: []byte(`{"packaged":true}`)},
		"configs/client2api.example.json": {Data: []byte(`{"example":true}`)},
		"data/cline/accounts.json":        {Data: []byte(`{"packaged":true}`)},
		"data/usage.json":                 {Data: []byte(`{"packaged":true}`)},
	}

	dir := t.TempDir()
	seed(t, dir, "configs/client2api.json", `{"live":true}`)
	seed(t, dir, "data/cline/accounts.json", `{"live":true}`)
	seed(t, dir, "data/usage.json", `{"live":true}`)

	written, preserved, err := copyPayload(fake, ".", dir, true)
	if err != nil {
		t.Fatalf("copyPayload: %v", err)
	}
	if preserved != 3 {
		t.Errorf("preserved = %d, want 3 (config plus two data files)", preserved)
	}
	if written != 2 {
		t.Errorf("written = %d, want 2 (the program and the example config)", written)
	}

	for _, rel := range []string{"configs/client2api.json", "data/cline/accounts.json", "data/usage.json"} {
		if got := read(t, dir, rel); got != `{"live":true}` {
			t.Errorf("%s = %s, want the live copy to survive", rel, got)
		}
	}
	if got := read(t, dir, "client2api.exe"); got != "packaged exe" {
		t.Errorf("client2api.exe = %q, want the packaged program", got)
	}
	if got := read(t, dir, "configs/client2api.example.json"); got != `{"example":true}` {
		t.Errorf("example config = %s, want the packaged one", got)
	}
}

// TestCopyPayloadNoDataSkipsAccountPool pins the -no-data shape: the config
// still lands, the account pool is never written.
func TestCopyPayloadNoDataSkipsAccountPool(t *testing.T) {
	fake := fstest.MapFS{
		"configs/client2api.json":  {Data: []byte(`{"packaged":true}`)},
		"data/cline/accounts.json": {Data: []byte(`{"packaged":true}`)},
	}

	dir := t.TempDir()
	written, preserved, err := copyPayload(fake, ".", dir, false)
	if err != nil {
		t.Fatalf("copyPayload: %v", err)
	}
	if preserved != 0 {
		t.Errorf("preserved = %d, want 0 on an empty directory", preserved)
	}
	if written != 1 {
		t.Errorf("written = %d, want 1 (the config; the account file is dropped)", written)
	}
	requireNoDataFiles(t, dir)
}

// requireNoDataFiles asserts that nothing was written under data/.  An empty
// data/ directory is not the thing worth pinning -- go:embed drops empty
// directories while an in-memory test tree synthesises them -- a file in it is.
func requireNoDataFiles(t *testing.T, dir string) {
	t.Helper()
	_ = filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			t.Errorf("-no-data wrote %s", filepath.ToSlash(rel))
		}
		return nil
	})
}

func seed(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestSnapshotPathsMatchesWhatTheInstallCanWrite pins the rollback file list:
// the payload files (minus the .gitkeep placeholder), the uninstaller and the
// panel shortcut the installer writes itself, and no data/ file when the
// package is a program-only build.
func TestSnapshotPathsMatchesWhatTheInstallCanWrite(t *testing.T) {
	fake := fstest.MapFS{
		"client2api.exe":                  {Data: []byte("x")},
		"configs/client2api.example.json": {Data: []byte("x")},
		"data/.gitkeep":                   {Data: nil},
		"data/cline/accounts.json":        {Data: []byte("x")},
	}

	full, err := snapshotPaths(fake, ".", true)
	if err != nil {
		t.Fatalf("snapshotPaths(includeData=true): %v", err)
	}
	for _, want := range []string{"client2api.exe", "configs/client2api.example.json", "data/cline/accounts.json", uninstallExe, panelLNK} {
		if !hasPath(full, want) {
			t.Errorf("full snapshot is missing %q: %v", want, full)
		}
	}
	if hasPath(full, "data/.gitkeep") {
		t.Errorf("the .gitkeep placeholder must not enter the rollback log: %v", full)
	}

	noData, err := snapshotPaths(fake, ".", false)
	if err != nil {
		t.Fatalf("snapshotPaths(includeData=false): %v", err)
	}
	if hasPath(noData, "data/cline/accounts.json") {
		t.Errorf("-no-data rollback log must not contain the account pool: %v", noData)
	}
}

// TestSnapshotRollbackRestoresThePreviousVersion is the promise the upgrade path
// makes: when a build fails after the new files are already on disk, the
// previous program files come back and anything the install created is removed.
// Operator state is deliberately outside that set -- the installer copies it to
// the side of neither direction, and this pins that.
func TestSnapshotRollbackRestoresThePreviousVersion(t *testing.T) {
	fake := fstest.MapFS{
		"client2api.exe":                  {Data: []byte("new exe")},
		"panelsmoke.exe":                  {Data: []byte("new smoke")},
		"configs/client2api.example.json": {Data: []byte("new example")},
		"configs/client2api.json":         {Data: []byte("packaged config")},
		"data/usage.json":                 {Data: []byte("packaged usage")},
	}

	dir := t.TempDir()
	seed(t, dir, "client2api.exe", "old exe")
	seed(t, dir, "configs/client2api.example.json", "old example")
	seed(t, dir, "configs/client2api.json", "live config")
	seed(t, dir, "data/usage.json", "live usage")

	snap, err := takeSnapshot(fake, ".", dir, true)
	if err != nil {
		t.Fatalf("takeSnapshot: %v", err)
	}
	defer snap.discard()

	// The install got as far as writing its files before the post-check said no.
	seed(t, dir, "client2api.exe", "broken new exe")
	seed(t, dir, "panelsmoke.exe", "broken new smoke")
	seed(t, dir, "configs/client2api.example.json", "new example")

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := read(t, dir, "client2api.exe"); got != "old exe" {
		t.Errorf("client2api.exe = %q, want the previous version back", got)
	}
	if got := read(t, dir, "configs/client2api.example.json"); got != "old example" {
		t.Errorf("example config = %q, want the previous version back", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "panelsmoke.exe")); !os.IsNotExist(err) {
		t.Errorf("panelsmoke.exe was created by the failed install and must be removed, stat err = %v", err)
	}
	// Operator state was never the installer's to overwrite, and the rollback
	// must not have copied the packaged copy of either over the live one.
	if got := read(t, dir, "configs/client2api.json"); got != "live config" {
		t.Errorf("live config = %q, want it untouched", got)
	}
	if got := read(t, dir, "data/usage.json"); got != "live usage" {
		t.Errorf("usage history = %q, want it untouched", got)
	}
}

// TestSnapshotRollbackOnFreshInstallRemovesEverything covers the other shape:
// with nothing on disk there is no previous version, so a failed first install
// has to leave the directory empty rather than half-populated.
func TestSnapshotRollbackOnFreshInstallRemovesEverything(t *testing.T) {
	fake := fstest.MapFS{
		"client2api.exe":          {Data: []byte("new exe")},
		"configs/client2api.json": {Data: []byte("packaged config")},
	}
	dir := t.TempDir()

	snap, err := takeSnapshot(fake, ".", dir, true)
	if err != nil {
		t.Fatalf("takeSnapshot: %v", err)
	}
	defer snap.discard()

	seed(t, dir, "client2api.exe", "new exe")
	seed(t, dir, "configs/client2api.json", "packaged config")
	seed(t, dir, uninstallExe, "installer")

	if err := snap.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, rel := range []string{"client2api.exe", "configs/client2api.json", uninstallExe} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s survived a fresh-install rollback (stat err = %v)", rel, err)
		}
	}
}

func hasPath(paths []string, want string) bool {
	want = filepath.FromSlash(want)
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
