package kimi

// Offline tests for the per-account health memory (health.go).
//
// Everything here is hermetic and fast: the credential locations point at temp
// directories, PATH is emptied, the "CLI" is a stub script, and time is injected
// through healthStore.now -- so a cooldown of ninety seconds is tested without
// sleeping for one.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// fakeClock is a controllable stand-in for time.Now, installed on the health
// store so cooldowns and the write debounce can be driven exactly.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at = f.at.Add(d)
}

// useFakeClock hands the client's health layer a clock the test controls.
func useFakeClock(c *Client, start time.Time) *fakeClock {
	f := &fakeClock{at: start.UTC()}
	c.health.now = f.now
	return f
}

// accountsFileBytes is the raw file on disk.  Assertions about what is (and is
// not) persisted are made on these bytes rather than on a struct, because the
// bytes are what an operator could read and grep.
func accountsFileBytes(t *testing.T, dataDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, accountsFileName))
	if err != nil {
		t.Fatalf("reading %s: %v", accountsFileName, err)
	}
	return string(b)
}

func writeAccountsRaw(t *testing.T, dataDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dataDir, accountsFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// failingStubBody is a stub CLI that writes line to stderr and exits non-zero.
func failingStubBody(line string) string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\n>&2 echo " + line + "\r\nexit /b 3\r\n"
	}
	return "#!/bin/sh\necho '" + line + "' >&2\nexit 3\n"
}

// quietStubBody is a stub CLI that does nothing at all, successfully.
func quietStubBody() string {
	if runtime.GOOS == "windows" {
		return "@echo off\r\nexit /b 0\r\n"
	}
	return "#!/bin/sh\nexit 0\n"
}

// newBoundClient builds a client whose CLI comes from an imported binding: a
// stub script on disk, recorded as a binding in accounts.json.  Import is the
// only way to reach such a binary, so this is the state-file shape a real panel
// creates -- and therefore the shape the pick path has to honour.
func newBoundClient(t *testing.T, stubBody string, cfg map[string]any) (*Client, string, string) {
	t.Helper()
	dataDir := t.TempDir()
	stub := writeScript(t, t.TempDir(), stubCLIName(), stubBody)
	id := bindingID(stub)
	raw, err := json.Marshal(accountState{
		Version: accountStateVersion,
		Enabled: map[string]bool{id: true},
		Bindings: []bindingRecord{{
			ID: id, Kind: bindingKindBinary, Path: stub, Label: stub, AddedAt: "2024-01-01T00:00:00Z",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeAccountsRaw(t, dataDir, string(raw))

	merged := map[string]any{"assume_logged_in": true}
	for k, v := range cfg {
		merged[k] = v
	}
	return newClientIn(t, dataDir, merged), id, dataDir
}

// healthOnDisk reads one account's record back out of the file.
func healthOnDisk(t *testing.T, c *Client, id string) healthRecord {
	t.Helper()
	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	return st.Health[id]
}

// drainChat runs one request all the way to the end of its event stream.  A
// test that wants the CLI's outcome recorded has to read the stream out:
// Close() cancels the run context first, and finishUpstream deliberately
// discards the outcome of a run nobody stayed to watch.
func drainChat(t *testing.T, c *Client, text string) {
	t.Helper()
	st, err := c.Chat(context.Background(), userReq("kimi", text))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	for {
		if _, rerr := st.Recv(); rerr != nil {
			break
		}
	}
	_ = st.Close()
}

// ---------------------------------------------------------------------------
// Backward compatibility
// ---------------------------------------------------------------------------

// TestHealthOldStateFileLoadsWithNoMemory pins the promise that a state file
// written by an older build (version 1, no health key) still loads: the
// operator's own choices survive, and nothing is remembered about health.
func TestHealthOldStateFileLoadsWithNoMemory(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)

	dataDir := t.TempDir()
	const binding = "binary:0123456789ab"
	writeAccountsRaw(t, dataDir, `{
  "version": 1,
  "enabled": {"cli-login": true, "`+binding+`": false},
  "bindings": [{"id": "`+binding+`", "kind": "binary", "path": "C:\\nope\\kimi.exe", "label": "nope", "added_at": "2024-01-01T00:00:00Z"}]
}`)
	c := newClientIn(t, dataDir, map[string]any{"assume_logged_in": true})

	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState on a version-1 file: %v", err)
	}
	if !st.Enabled[cliLoginID] {
		t.Fatalf("the enabled flag was lost: %+v", st.Enabled)
	}
	if st.Enabled[binding] {
		t.Fatalf("a disabled binding came back enabled: %+v", st.Enabled)
	}
	if len(st.Bindings) != 1 {
		t.Fatalf("the bindings were lost: %+v", st.Bindings)
	}
	if len(st.Health) != 0 {
		t.Fatalf("a version-1 file must yield no health memory, got %+v", st.Health)
	}
	if !c.selectable(cliLoginID) {
		t.Fatalf("an account with no memory must be selectable (note: %q)", c.healthNote(cliLoginID))
	}
	if rec := c.healthOf(binding); rec != (healthRecord{}) {
		t.Fatalf("healthOf on a fresh file = %+v", rec)
	}

	// A write from this build must not trample what the old build stored.
	c.markUsed(cliLoginID)
	st, err = c.loadState()
	if err != nil {
		t.Fatalf("loadState after a write: %v", err)
	}
	if st.Version != accountStateVersion {
		t.Fatalf("version = %d, want %d", st.Version, accountStateVersion)
	}
	if !st.Enabled[cliLoginID] || st.Enabled[binding] {
		t.Fatalf("the enabled flags changed across a write: %+v", st.Enabled)
	}
	if len(st.Bindings) != 1 {
		t.Fatalf("the bindings were lost across a write: %+v", st.Bindings)
	}
	if rec := st.Health[cliLoginID]; rec.State != healthHealthy || rec.LastUsed == "" {
		t.Fatalf("markUsed did not persist: %+v", st.Health)
	}
}

// ---------------------------------------------------------------------------
// The pick path
// ---------------------------------------------------------------------------

// TestHealthCoolingIsSkippedUntilItsDeadline: cooling is bounded, and the clock
// is the only thing that heals it.
func TestHealthCoolingIsSkippedUntilItsDeadline(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, dataDir := newClient(t, nil)
	clk := useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	c.markCooling("acct-a", causeProcessFailed, "kimi: CLI exited with status 3", 90*time.Second)

	if c.selectable("acct-a") {
		t.Fatal("a cooling account must not be selectable")
	}
	note := c.healthNote("acct-a")
	for _, want := range []string{"cooling", causeProcessFailed, "2030-01-01T00:01:30Z"} {
		if !strings.Contains(note, want) {
			t.Fatalf("healthNote = %q, want it to mention %q", note, want)
		}
	}

	onDisk := accountsFileBytes(t, dataDir)
	for _, want := range []string{`"health"`, `"cooling"`, "acct-a", "cooldown_until"} {
		if !strings.Contains(onDisk, want) {
			t.Fatalf("%s does not contain %q:\n%s", accountsFileName, want, onDisk)
		}
	}

	clk.advance(89 * time.Second)
	if c.selectable("acct-a") {
		t.Fatal("a cooling account became selectable before its deadline")
	}
	clk.advance(2 * time.Second)
	if !c.selectable("acct-a") {
		t.Fatalf("a cooling account did not heal after its deadline: %q", c.healthNote("acct-a"))
	}
	st, err := c.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := st.Health["acct-a"]; ok {
		t.Fatalf("a healed record was left on disk: %+v", rec)
	}
}

// TestHealthDeadIsSkippedUntilOperatorActs: time never revives a dead account;
// the operator enabling it does.
func TestHealthDeadIsSkippedUntilOperatorActs(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, map[string]any{"assume_logged_in": true})
	clk := useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	c.markDead(cliLoginID, causeNoCredential, "no login credential was found")
	if c.selectable(cliLoginID) {
		t.Fatal("a dead account must not be selectable")
	}
	note := c.healthNote(cliLoginID)
	for _, want := range []string{"dead", "kimi login"} {
		if !strings.Contains(note, want) {
			t.Fatalf("healthNote = %q, want it to mention %q", note, want)
		}
	}

	clk.advance(24 * time.Hour)
	if c.selectable(cliLoginID) {
		t.Fatal("time revived a dead account")
	}

	// Enabling an account in the panel is the operator saying "try this again",
	// so it clears the memory and the account is selectable again.
	if err := c.SetAccountEnabled(context.Background(), cliLoginID, true); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if !c.selectable(cliLoginID) {
		t.Fatalf("enabling did not revive the account: %q", c.healthNote(cliLoginID))
	}
	if rec := c.healthOf(cliLoginID); rec != (healthRecord{}) {
		t.Fatalf("enabling left health behind: %+v", rec)
	}
	st, err := c.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := st.Health[cliLoginID]; ok {
		t.Fatalf("a revived account is still on disk: %+v", rec)
	}
	if !st.Enabled[cliLoginID] {
		t.Fatalf("enabling did not persist: %+v", st.Enabled)
	}
}

// TestHealthCooledBindingIsNotPicked: a binding this module watched fail must
// not be chosen for the next request, and must come back when its cooldown
// passes -- but never because time passed while it was dead.
func TestHealthCooledBindingIsNotPicked(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, id, _ := newBoundClient(t, quietStubBody(), nil)
	clk := useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	if got := c.boundBinary(); got == "" {
		t.Fatal("the imported binding was not picked before anything failed")
	}
	bin, account, err := c.run.binaryPathFrom()
	if err != nil || bin == "" || account != id {
		t.Fatalf("binaryPathFrom = (%q, %q, %v), want the binding %q", bin, account, err, id)
	}

	c.markCooling(id, causeProcessFailed, "kimi: CLI exited with status 3", 90*time.Second)
	c.run.invalidateBinaryCache()
	if got := c.boundBinary(); got != "" {
		t.Fatalf("a cooling binding was still picked: %q", got)
	}
	if _, _, err := c.run.binaryPathFrom(); err == nil {
		t.Fatal("a cooling binding still resolved to a usable binary")
	}

	clk.advance(91 * time.Second)
	if _, ok := c.boundBinding(); !ok {
		t.Fatalf("the binding did not come back after its cooldown: %q", c.healthNote(id))
	}

	c.markDead(id, causeNoCredential, "the credential is gone")
	if got := c.boundBinary(); got != "" {
		t.Fatalf("a dead binding was still picked: %q", got)
	}
	clk.advance(365 * 24 * time.Hour)
	if _, ok := c.boundBinding(); ok {
		t.Fatal("time revived a dead binding")
	}
	c.revive(id)
	if _, ok := c.boundBinding(); !ok {
		t.Fatal("reviving did not bring the binding back")
	}
}

// ---------------------------------------------------------------------------
// What reaches the disk
// ---------------------------------------------------------------------------

// TestHealthStateFileNeverHoldsASecret drives a real failed run whose stderr
// carried a token, and asserts on the raw bytes of the file.
func TestHealthStateFileNeverHoldsASecret(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	const secret = "sk-abcdefghijklmnopqrstuvwxyz"
	c, id, dataDir := newBoundClient(t, failingStubBody("kimi: not logged in; Authorization: Bearer "+secret), nil)

	drainChat(t, c, "hello")

	onDisk := accountsFileBytes(t, dataDir)
	if strings.Contains(onDisk, secret) {
		t.Fatalf("a token reached %s:\n%s", accountsFileName, onDisk)
	}
	// The failure this test is about must actually have been recorded, or the
	// assertion above would pass on a file that was never written.
	for _, want := range []string{`"health"`, id, causeProcessFailed, "last_error"} {
		if !strings.Contains(onDisk, want) {
			t.Fatalf("%s does not contain %q:\n%s", accountsFileName, want, onDisk)
		}
	}
	// ...and the rest of the file is still the operator's to keep.
	for _, want := range []string{`"bindings"`, `"enabled"`, stubCLIName()} {
		if !strings.Contains(onDisk, want) {
			t.Fatalf("the health write dropped %q from %s:\n%s", want, accountsFileName, onDisk)
		}
	}
}

// TestHealthCorruptStateFileIsSurvived: an unreadable file must not panic, must
// not turn into health memory, and must not lose the operator's flags once the
// file is readable again.
func TestHealthCorruptStateFileIsSurvived(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	const good = `{"version":1,"enabled":{"cli-login":false},"bindings":[]}`
	writeAccountsRaw(t, dataDir, good)
	c := newClientIn(t, dataDir, map[string]any{"assume_logged_in": true})

	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts on a valid file: %v", err)
	}
	if row := findAccount(t, recs, cliLoginID); row.Enabled {
		t.Fatalf("the disabled choice was not honoured: %+v", row)
	}

	// Corrupt it.  Nothing may panic.  This client already read its memory while
	// the file was still readable, so a record taken now lives in memory only;
	// the write is what fails, and that failure must stay harmless.
	writeAccountsRaw(t, dataDir, "{not json")
	c.markDead("acct-x", causeNoCredential, "no credential")
	if st := c.Status(ctx); !strings.Contains(st.Detail, "account state unreadable") {
		t.Fatalf("Status does not say the state is unreadable: %q", st.Detail)
	}
	if _, err := c.Accounts(ctx); err == nil {
		t.Fatal("Accounts hid an unreadable state file")
	}
	// The write failed, so the bytes that were there are still there.
	if got := accountsFileBytes(t, dataDir); got != "{not json" {
		t.Fatalf("a failed health write rewrote the state file: %q", got)
	}

	// A client that starts up while the file is unreadable must invent nothing:
	// no memory on disk means no memory to report.
	fresh := newClientIn(t, dataDir, map[string]any{"assume_logged_in": true})
	if !fresh.selectable("acct-x") {
		t.Fatal("an unreadable state file turned into health memory")
	}
	if note := fresh.healthNote("acct-x"); note != "" {
		t.Fatalf("healthNote on an unreadable file = %q", note)
	}
	// Health is a cache: learning something while the file cannot be written is
	// still allowed, it just is not durable.
	fresh.markDead("acct-x", causeNoCredential, "no credential")
	if fresh.selectable("acct-x") {
		t.Fatal("a failure seen while the state file was unreadable was not remembered at all")
	}

	// The module could not write, so it must not have destroyed what was there:
	// making the file readable again must restore the operator's choice.
	writeAccountsRaw(t, dataDir, good)
	recs, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts after the file became readable again: %v", err)
	}
	if row := findAccount(t, recs, cliLoginID); row.Enabled {
		t.Fatalf("the enabled flag was lost: %+v", row)
	}
}

// TestHealthAtomicWriteLeavesNoTempFile: the write is temp-file-plus-rename, so
// nothing may be left behind next to it.
func TestHealthAtomicWriteLeavesNoTempFile(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, dataDir := newClient(t, nil)

	c.markCooling("acct-a", causeTimeout, "kimi: request timed out after 1s", time.Minute)

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing was written at all")
	}
	sawAccounts := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
		if e.Name() == accountsFileName {
			sawAccounts = true
		}
	}
	if !sawAccounts {
		t.Fatalf("%s was not written; DataDir holds %v", accountsFileName, entries)
	}
}

// TestHealthFlushIsDebounced: a level update inside the window is dropped, and
// the next one after it is written.
func TestHealthFlushIsDebounced(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)
	base := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := useFakeClock(c, base)

	c.markUsed("acct-a")
	if got := healthOnDisk(t, c, "acct-a").LastUsed; got != healthTime(base) {
		t.Fatalf("first write: last_used = %q, want %q", got, healthTime(base))
	}

	clk.advance(2 * time.Second)
	c.markUsed("acct-a")
	if got := healthOnDisk(t, c, "acct-a").LastUsed; got != healthTime(base) {
		t.Fatalf("a level update was written inside the window: last_used = %q", got)
	}

	clk.advance(4 * time.Second)
	c.markUsed("acct-a")
	if want := healthTime(base.Add(6 * time.Second)); healthOnDisk(t, c, "acct-a").LastUsed != want {
		t.Fatalf("last_used after the window = %q, want %q", healthOnDisk(t, c, "acct-a").LastUsed, want)
	}
}

// ---------------------------------------------------------------------------
// Classification
// ---------------------------------------------------------------------------

// TestHealthClassifiesOnlyWhatItCanSee: the verdicts for conditions this
// machine can actually observe, and the fact that everything else is only
// remembered -- never punished with a cooldown for a guess.
func TestHealthClassifiesOnlyWhatItCanSee(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, nil)
	clk := useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	// A caller walking away says nothing about the account.
	c.noteFailure("acct-a", context.Canceled, "", 0)
	if !c.selectable("acct-a") || c.healthOf("acct-a") != (healthRecord{}) {
		t.Fatalf("a cancellation was remembered: %+v", c.healthOf("acct-a"))
	}

	// A failure this module cannot explain is remembered, not punished.
	exotic := errors.New("kimi: something this module cannot explain")
	if v, ok := classifyLocally(exotic); ok {
		t.Fatalf("an unexplained failure was classified as %q", v.Cause)
	}
	c.noteFailure("acct-b", exotic, "", 0)
	rec := c.healthOf("acct-b")
	if rec.State != healthHealthy || rec.CooldownUntil != "" || !strings.Contains(rec.LastError, "cannot explain") {
		t.Fatalf("unclassified failure = %+v", rec)
	}
	if !c.selectable("acct-b") {
		t.Fatal("an unclassifiable failure parked the account")
	}

	// A non-zero child exit is evidence of *a* failure, not of its cause, so it
	// cools for a bounded time instead of dying.
	c.noteCLIFailure("acct-c", errors.New("kimi: CLI exited with status 3"), nil)
	rec = c.healthOf("acct-c")
	if rec.State != healthCooling || rec.Cause != causeProcessFailed || rec.CooldownUntil == "" {
		t.Fatalf("a failed CLI run = %+v", rec)
	}
	clk.advance(cooldownProcessFailed + time.Second)
	if !c.selectable("acct-c") {
		t.Fatal("a process failure never healed")
	}

	// A refused credential is the one vendor answer remembered as dead.
	c.noteFailure("acct-d", &credentialRefusedError{Status: http.StatusBadRequest}, "", 0)
	rec = c.healthOf("acct-d")
	if rec.State != healthDead || rec.Cause != causeCredentialRefused {
		t.Fatalf("a refused credential = %+v", rec)
	}
	clk.advance(48 * time.Hour)
	if c.selectable("acct-d") {
		t.Fatal("time revived a refused credential")
	}

	// An HTTP status is machine-readable, so 401/403 dies and 5xx cools.
	c.noteFailure("acct-e", &upstreamStatusError{Where: "https://example.invalid", Status: http.StatusUnauthorized}, "", 0)
	if rec := c.healthOf("acct-e"); rec.State != healthDead {
		t.Fatalf("HTTP 401 = %+v", rec)
	}
	c.noteFailure("acct-f", &upstreamStatusError{Where: "https://example.invalid", Status: http.StatusServiceUnavailable}, "", 0)
	rec = c.healthOf("acct-f")
	if rec.State != healthCooling || strings.Contains(rec.LastError, "sk-") {
		t.Fatalf("HTTP 503 = %+v", rec)
	}

	// And all of it came back readable, with nothing secret-shaped in it.
	st, err := c.loadState()
	if err != nil {
		t.Fatal(err)
	}
	if st.Health["acct-b"].LastError == "" || st.Health["acct-d"].State != healthDead {
		t.Fatalf("health was not persisted: %+v", st.Health)
	}
}

// TestHealthSurfacesOnPanelRows: an operator must be able to see *why* an
// account is not being used without opening the JSON file.
func TestHealthSurfacesOnPanelRows(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	ctx := context.Background()
	c, id, _ := newBoundClient(t, quietStubBody(), nil)

	c.markCooling(id, causeProcessFailed, "kimi: CLI exited with status 3", 90*time.Second)
	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	row := findAccount(t, recs, id)
	if got := row.Fields["health"]; got != healthCooling {
		t.Fatalf("fields[health] = %v, want %q (row %+v)", got, healthCooling, row)
	}
	if got, _ := row.Fields["health_cause"].(string); got != causeProcessFailed {
		t.Fatalf("fields[health_cause] = %v, want %q", row.Fields["health_cause"], causeProcessFailed)
	}
	if got, _ := row.Fields["cooldown_until"].(string); got == "" {
		t.Fatalf("fields[cooldown_until] is missing: %+v", row.Fields)
	}
	if got, _ := row.Fields["last_error"].(string); got == "" {
		t.Fatalf("fields[last_error] is missing: %+v", row.Fields)
	}
	if !strings.Contains(row.Note, "cooling") {
		t.Fatalf("the row does not explain itself: %q", row.Note)
	}

	// The Status() rows carry the same memory in Extra.
	if err := c.saveToken(storedToken{AccessToken: "not-a-real-token", ExpiresAt: time.Now().Add(time.Hour).Unix()}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	c.markCooling(webLoginID, causeHTTPStatus, "https://example.invalid answered HTTP 503", time.Minute)
	st := c.Status(ctx)
	var web *core.AccountStatus
	for i := range st.Accounts {
		if st.Accounts[i].ID == webLoginID {
			web = &st.Accounts[i]
		}
	}
	if web == nil {
		t.Fatalf("the web-login row is missing: %+v", st.Accounts)
	}
	if got := web.Extra["health"]; got != healthCooling {
		t.Fatalf("extra[health] = %v, want %q", got, healthCooling)
	}
	if !strings.Contains(web.Note, "cooling") {
		t.Fatalf("the Status row does not explain itself: %q", web.Note)
	}
}

// TestADeadAccountIsNotReportedAsReady: the panel colours a row by its state,
// so a row that still says "ready" after this module has given up on the
// account is a lie the operator acts on -- green badge, 503 on the next
// request.  The stored health is the authority on usability, not the probe that
// produced the row.
func TestADeadAccountIsNotReportedAsReady(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	ctx := context.Background()
	c, _ := newClient(t, map[string]any{})
	storedPanelToken(t, c)

	// Before anything has failed, the panel login really is ready.
	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if row := findAccount(t, recs, webLoginID); row.State != "ready" {
		t.Fatalf("a fresh panel login = %q, want ready", row.State)
	}
	if st := c.Status(ctx); !st.Ready {
		t.Fatalf("a fresh panel login is not reported as ready: %s", st.Detail)
	}

	// A vendor 403 is terminal for this account (health.go remembers HTTP
	// statuses as machine-readable), so the row must stop advertising it.
	c.noteFailure(webLoginID,
		&upstreamStatusError{Where: "https://api.kimi.com/coding/v1/chat/completions", Status: http.StatusForbidden},
		"", 0)
	recs, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	row := findAccount(t, recs, webLoginID)
	if row.State == "ready" {
		t.Fatalf("a dead account is still reported as ready: %+v", row)
	}
	if row.State != "invalid" {
		t.Fatalf("row state = %q, want invalid", row.State)
	}
	if got := row.Fields["health"]; got != healthDead {
		t.Fatalf("fields[health] = %v, want %q", got, healthDead)
	}
	if !strings.Contains(row.Note, "403") {
		t.Fatalf("the row does not explain itself: %q", row.Note)
	}

	// The same lowering has to reach Status(): the panel's overview reads the
	// state from there as well.
	st := c.Status(ctx)
	var web *core.AccountStatus
	for i := range st.Accounts {
		if st.Accounts[i].ID == webLoginID {
			web = &st.Accounts[i]
		}
	}
	if web == nil {
		t.Fatalf("the web-login row is missing: %+v", st.Accounts)
	}
	if web.State != "invalid" {
		t.Fatalf("Status() reports a dead account as %q, want invalid", web.State)
	}
	// The badge above the table is the thing an operator reads first, so it has
	// to fall with the rows.  A client that still counts as "ready" while every
	// request answers 503 is the failure mode this whole test is about.
	if st.Ready {
		t.Fatalf("Status() reports the client as ready while its only account is dead: %s", st.Detail)
	}
	if !strings.Contains(st.Detail, "no account can serve a request right now") {
		t.Fatalf("Status() does not explain why it is not ready: %q", st.Detail)
	}
	if !strings.Contains(st.Detail, "403") {
		t.Fatalf("Status() does not quote the cause: %q", st.Detail)
	}

	// A cooling account is not ready either, and gets the panel's own word for
	// it rather than a state the frontend cannot tally.
	c.markCooling(webLoginID, causeHTTPStatus, "https://example.invalid answered HTTP 503", time.Minute)
	recs, err = c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if row := findAccount(t, recs, webLoginID); row.State != "cooling" {
		t.Fatalf("a cooling account = %q, want cooling", row.State)
	}
}

// The panel renders the note verbatim under the account id, so the health
// explanation must not run into whatever sentence came before it.
func TestHealthNoteSuffixStartsANewSentence(t *testing.T) {
	cases := []struct {
		name string
		note string
		want string
	}{
		{"no note at all", "", "dead"},
		{"no punctuation", "signed in from the panel", "signed in from the panel. Health: dead."},
		{"already a full stop", "signed in from the panel.", "signed in from the panel. Health: dead."},
		{"a question mark counts", "signed in?", "signed in? Health: dead."},
		{"trailing whitespace", "signed in from the panel   ", "signed in from the panel. Health: dead."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthNoteSuffix(tc.note, "dead"); got != tc.want {
				t.Fatalf("healthNoteSuffix(%q) = %q, want %q", tc.note, got, tc.want)
			}
		})
	}
	if got := healthNoteSuffix("signed in", ""); got != "signed in" {
		t.Fatalf("an empty reason must leave the note alone, got %q", got)
	}
}
