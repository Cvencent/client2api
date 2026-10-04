package workbuddy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// restartPool builds a second pool over the same directory, the way a process
// restart does: a fresh Pool with no memory, then the same accounts.
func restartPool(t *testing.T, dir string, accounts ...*Auth) *Pool {
	t.Helper()
	p := NewPool(nil, 0)
	p.AttachState(dir, nil)
	p.Replace(accounts)
	return p
}

func writePoolFile(t *testing.T, dir string, st persistedPool) {
	t.Helper()
	if err := core.WriteJSONAtomic(filepath.Join(dir, poolStateFile), st); err != nil {
		t.Fatalf("write %s: %v", poolStateFile, err)
	}
}

// TestWorkbuddyPoolStateIsNotWrittenBesideCredentials pins the directory split.
// The credential loader globs <accounts_dir>/*.json, so a state file in that
// directory is enumerated and parsed as an account.  An earlier version wrote
// pool.json beside the credentials and only the file-listing assertions in
// accounts_test.go caught it, so the invariant is asserted on its own here.
func TestWorkbuddyPoolStateIsNotWrittenBesideCredentials(t *testing.T) {
	c, _ := newTestClient(t, nil)

	state := c.poolStateDir()
	if state == "" {
		t.Fatal("no pool state directory was chosen")
	}
	if state == c.accountsDir() {
		t.Fatalf("pool state would be written into the credential directory %s", state)
	}
	// It still has to be inside the module's own data directory, which is the
	// only directory this module is allowed to write to.
	rel, err := filepath.Rel(c.deps.DataDir, state)
	if err != nil {
		t.Fatalf("rel(%s, %s): %v", c.deps.DataDir, state, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("state directory %s is outside the data directory %s", state, c.deps.DataDir)
	}
}

func TestWorkbuddyPoolStateCooldownSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}

	p := NewPool([]*Auth{a}, 0)
	p.AttachState(dir, nil)
	// The vendor's quota day flips at 04:00 and the sign-in top-up lands later
	// that morning, so a park of some fixed length would expire inside the same
	// day and spend another hard-credit error learning nothing.  The park has to
	// reach the next day boundary instead.
	_, d := p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
	if d <= 0 || d > 24*time.Hour {
		t.Fatalf("cooldown = %v, want a park until the next 04:00", d)
	}
	if got := time.Now().Add(d).Hour(); got != hardCreditHour {
		t.Fatalf("park ends at hour %d, want %d", got, hardCreditHour)
	}
	if p.Ready() {
		t.Fatal("the account should be parked")
	}

	// Restarting our process does not shorten it.
	again := restartPool(t, dir, &Auth{AccessToken: "at-111111111111", UID: "u1"})
	if again.Ready() {
		t.Fatal("a cooldown to the next 04:00 must survive a restart")
	}
	if _, ok := again.Pick(nil); ok {
		t.Fatal("a parked account must not be picked after a restart")
	}
	snap := again.Snapshot()
	if len(snap) != 1 || snap[0].State != stateExhausted {
		t.Fatalf("snapshot = %+v, want one exhausted account", snap)
	}
}

func TestWorkbuddyPoolStateExpiredCooldownDoesNotPark(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	writePoolFile(t, dir, persistedPool{Version: poolStateVersion, Accounts: []persistedPoolAccount{{
		ID:            a.ID(),
		State:         stateExhausted,
		CooldownUntil: time.Now().Add(-time.Hour),
		Note:          "credit exhausted",
		Fails:         3,
	}}})

	p := restartPool(t, dir, a)
	if !p.Ready() {
		t.Fatal("a deadline that passed while we were down must not park the account")
	}
	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d", len(snap))
	}
	// The counter is not a deadline, so it still comes back.
	if got := snap[0].Extra["failures"]; got != 3 {
		t.Fatalf("failures = %v, want 3", got)
	}
	if snap[0].State != stateReady {
		t.Fatalf("state = %q, want %q", snap[0].State, stateReady)
	}
}

func TestWorkbuddyPoolStateNeverHoldsACredential(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-SECRET-abcdefghijklmnop", UID: "u1"}
	p := NewPool([]*Auth{a}, 0)
	p.AttachState(dir, nil)
	p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "look at at-SECRET-abcdefghijklmnop"})

	raw, err := os.ReadFile(filepath.Join(dir, poolStateFile))
	if err != nil {
		t.Fatalf("read %s: %v", poolStateFile, err)
	}
	if len(raw) == 0 {
		t.Fatal("the state file is empty; this assertion is not checking anything")
	}
	for _, secret := range []string{"at-SECRET", "SECRET-abcdefghijklmnop", "abcdefghijklmnop"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s holds %q", poolStateFile, secret)
		}
	}
}

func TestWorkbuddyPoolStateCorruptFileIsIgnored(t *testing.T) {
	for _, body := range []string{
		"",
		"{",
		"not json at all",
		`{"version":1,"accounts":"nope"}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, poolStateFile), []byte(body), 0600); err != nil {
			t.Fatalf("seed: %v", err)
		}
		a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
		p := restartPool(t, dir, a)
		if !p.Ready() {
			t.Fatalf("body %q parked a healthy account", body)
		}
	}
}

func TestWorkbuddyPoolStateNewerVersionIsNotGuessedAt(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	writePoolFile(t, dir, persistedPool{Version: poolStateVersion + 1, Accounts: []persistedPoolAccount{{
		ID:            a.ID(),
		State:         stateExhausted,
		CooldownUntil: time.Now().Add(6 * time.Hour),
	}}})

	p := restartPool(t, dir, a)
	if !p.Ready() {
		t.Fatal("a file from a newer build must be ignored, not obeyed")
	}
}

func TestWorkbuddyPoolStateWithoutADirectoryIsMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := NewPool([]*Auth{a}, 0)
	p.AttachState("", nil)
	p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
	if p.Ready() {
		t.Fatal("the in-memory cooldown must still apply")
	}
	if _, err := os.Stat(filepath.Join(dir, poolStateFile)); !os.IsNotExist(err) {
		t.Fatalf("a pool without a directory wrote a file: %v", err)
	}
}

func TestWorkbuddyPoolStateStagedRecordNeverOverrulesThisProcess(t *testing.T) {
	// White box: Replace only ever stages into an entry it just built, so this
	// calls the hand-off directly to pin the guard itself.
	p := NewPool(nil, 0)
	p.pending = map[string]persistedPoolAccount{
		"u1": {ID: "u1", State: stateExhausted, CooldownUntil: time.Now().Add(6 * time.Hour)},
	}
	decided := &poolEntry{auth: &Auth{AccessToken: "at-111111111111", UID: "u1"}}
	decided.state = stateCooling
	decided.until = time.Now().Add(time.Minute)
	p.applyStagedLocked(decided)
	if decided.state != stateCooling || decided.until.Sub(time.Now()) > 2*time.Minute {
		t.Fatalf("a staged record overruled this process: state=%q until=%v", decided.state, decided.until)
	}
}

func TestWorkbuddyPoolStateSkipsTheWriteWithoutATransition(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{AccessToken: "at-111111111111", UID: "u1"}
	p := NewPool([]*Auth{a}, 0)
	p.AttachState(dir, nil)

	path := filepath.Join(dir, poolStateFile)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("nothing has changed yet, but %s exists: %v", poolStateFile, err)
	}

	// MarkSuccess runs on every successful request.  Nothing about this account
	// changed, so nothing should be written.
	p.MarkSuccess(a)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("MarkSuccess on a healthy account wrote %s", poolStateFile)
	}

	// A real transition must write.
	p.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"})
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a cooldown did not reach %s: %v", poolStateFile, err)
	}

	// Clearing that cooldown is itself a transition, so it must be written.
	p.MarkSuccess(a)
	if !p.Ready() {
		t.Fatal("MarkSuccess must clear the cooldown")
	}

	// A second MarkSuccess has nothing left to change.  Pin the file's mtime to
	// the past first, so any rewrite becomes visible.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	p.MarkSuccess(a)
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !st.ModTime().Truncate(time.Second).Equal(past) {
		t.Fatal("MarkSuccess rewrote the file without a transition")
	}
}
