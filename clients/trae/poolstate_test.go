package trae

// poolstate_test.go — the health that has to survive a restart.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// statePool builds a pool over the given accounts with persistence attached to
// dir, exactly as New does (attach, then replace).
func statePool(t *testing.T, dir string, accounts ...*Auth) *Pool {
	t.Helper()
	p := NewPool(nil)
	p.AttachState(dir, t.Logf)
	p.Replace(accounts)
	return p
}

func statePath(dir string) string { return filepath.Join(dir, poolStateFile) }

// pinned reports whether path still carries the mtime we last pinned on it.
func pinned(t *testing.T, dir string, want time.Time) bool {
	t.Helper()
	st, err := os.Stat(statePath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return st.ModTime().Truncate(time.Second).Equal(want)
}

// TestPoolStateSkipsTheWriteWithoutATransition guards against write
// amplification: MarkSuccess runs on every successful request, so it must not
// rewrite the file to announce that nothing changed.  It also pins the other
// half — a pool whose accounts are all healthy has nothing to remember, so a
// start must not create the file (and therefore the directory) at all.
func TestPoolStateSkipsTheWriteWithoutATransition(t *testing.T) {
	dir := t.TempDir()
	a := testAuth("u1", "tok-1")
	p := statePool(t, dir, a)

	if _, err := os.Stat(statePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("a pool with nothing but healthy accounts wrote %s: %v", poolStateFile, err)
	}
	p.MarkSuccess(a)
	if _, err := os.Stat(statePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("MarkSuccess on an already-ready account wrote %s", poolStateFile)
	}

	// A real transition must reach the disk.
	p.MarkFailure(a, &Error{Kind: ErrQuota})
	if _, err := os.Stat(statePath(dir)); err != nil {
		t.Fatalf("a cooldown did not reach %s: %v", poolStateFile, err)
	}

	// Clearing it is a transition too, so the file is rewritten here.
	p.MarkSuccess(a)

	// A second MarkSuccess has nothing left to change.  Pin the mtime first, so
	// any rewrite becomes visible.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(statePath(dir), past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	p.MarkSuccess(a)
	if !pinned(t, dir, past) {
		t.Fatal("MarkSuccess rewrote the file without a transition")
	}
}

// TestPoolStateCooldownSurvivesARestart is the whole point of the file: a plan
// limit parks an account for 12h, and a process that forgets that replays the
// request the vendor just refused.
func TestPoolStateCooldownSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	a := testAuth("u1", "tok-1")

	first := statePool(t, dir, a)
	kind, cd := first.MarkFailure(a, &Error{Kind: ErrPlanLimit})
	if kind != ErrPlanLimit || cd != planLimitCooldown {
		t.Fatalf("MarkFailure = %v/%v, want %v/%v", kind, cd, ErrPlanLimit, planLimitCooldown)
	}
	if _, ok := first.Pick(nil); ok {
		t.Fatal("the just-parked account was still picked")
	}

	// A new process, reading nothing but the directory.
	restarted := statePool(t, dir, testAuth("u1", "tok-1"))
	if _, ok := restarted.Pick(nil); ok {
		t.Fatal("the cooldown did not survive the restart: the account was picked again")
	}
	snap := restarted.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot has %d accounts, want 1", len(snap))
	}
	if snap[0].State != stateExhausted {
		t.Errorf("State = %q after restart, want %q", snap[0].State, stateExhausted)
	}
	// The note is what tells an operator why the account is parked.
	if !strings.Contains(snap[0].Note, ErrPlanLimit.String()) {
		t.Errorf("Note = %q, want it to record %q", snap[0].Note, ErrPlanLimit.String())
	}
}

// TestPoolStateExpiredCooldownDoesNotPark is the other half: the file records a
// deadline, and once it has passed the account must be usable again without any
// operator action.
func TestPoolStateExpiredCooldownDoesNotPark(t *testing.T) {
	dir := t.TempDir()
	// Written by hand so the deadline is unambiguously in the past.
	writePoolState(t, dir, persistedPool{
		Version: poolStateVersion,
		Accounts: []persistedPoolAccount{{
			ID:            "u1",
			State:         stateExhausted,
			CooldownUntil: time.Now().Add(-time.Minute),
			Note:          ErrQuota.String(),
		}},
	})
	p := statePool(t, dir, testAuth("u1", "tok-1"))
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("an account whose cooldown already expired is still parked")
	}
	if got := p.Snapshot()[0].State; got != stateReady {
		t.Errorf("State = %q, want %q", got, stateReady)
	}
}

// TestPoolStateNeverHoldsACredential is the constraint that makes this file
// safe to write at all: accounts.json already holds live tokens, and this one
// must add nothing to that exposure.
func TestPoolStateNeverHoldsACredential(t *testing.T) {
	dir := t.TempDir()
	const secret = "tok-SECRET-abcdefghijklmnop"
	a := testAuth("u1", secret)
	p := statePool(t, dir, a)
	p.MarkFailure(a, &Error{Kind: ErrQuota})
	p.MarkSuccess(a)
	p.MarkInvalid(a, "session dead")

	raw, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("the state file was not written: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("the state file contains the account's credential:\n%s", raw)
	}
	if strings.Contains(string(raw), "tok-") {
		t.Fatalf("the state file contains something token-shaped:\n%s", raw)
	}
}

// TestPoolStateDoesNotPersistInvalid pins the deliberate omission.  "invalid"
// means "the credential this process last saw cannot be refreshed", and every
// start re-reads the credential from its source, so the old verdict says
// nothing about the new one.
func TestPoolStateDoesNotPersistInvalid(t *testing.T) {
	dir := t.TempDir()
	a := testAuth("u1", "tok-1")
	p := statePool(t, dir, a)
	p.MarkFailure(a, &Error{Kind: ErrQuota})
	p.MarkInvalid(a, "session dead")

	raw, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var st persistedPool
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state file is not valid JSON: %v", err)
	}
	if len(st.Accounts) != 0 {
		t.Fatalf("an invalid account was persisted: %+v", st.Accounts)
	}

	restarted := statePool(t, dir, testAuth("u1", "tok-1"))
	if _, ok := restarted.Pick(nil); !ok {
		t.Fatal("the account is still parked: an invalid verdict must not come back from disk")
	}
}

// TestPoolStateCorruptFileIsIgnored: the file is an optimisation, so a damaged
// one must cost a cooldown and never a request or a startup.
func TestPoolStateCorruptFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "{", "not json at all", `{"version":1,"accounts":"nope"}`} {
		if err := os.WriteFile(statePath(dir), []byte(bad), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		p := statePool(t, dir, testAuth("u1", "tok-1"))
		if _, ok := p.Pick(nil); !ok {
			t.Fatalf("a corrupt state file (%q) parked the only account", bad)
		}
		if _, err := os.Stat(statePath(dir)); err != nil {
			t.Fatalf("the pool stopped writing state after a corrupt read: %v", err)
		}
	}
}

// TestPoolStateNewerVersionIsNotGuessedAt: a file from a build that knows more
// than this one must be left alone rather than half-understood.
func TestPoolStateNewerVersionIsNotGuessedAt(t *testing.T) {
	dir := t.TempDir()
	writePoolState(t, dir, persistedPool{
		Version: poolStateVersion + 1,
		Accounts: []persistedPoolAccount{{
			ID:            "u1",
			State:         stateExhausted,
			CooldownUntil: time.Now().Add(12 * time.Hour),
		}},
	})
	p := statePool(t, dir, testAuth("u1", "tok-1"))
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("a state file from a newer build was applied instead of ignored")
	}
}

// TestPoolStateWithoutADirectoryIsMemoryOnly: NewPool is used by tests and by
// callers that never wanted a disk, and neither may start writing files.
func TestPoolStateWithoutADirectoryIsMemoryOnly(t *testing.T) {
	a := testAuth("u1", "tok-1")
	p := NewPool([]*Auth{a}) // no AttachState at all
	if _, cd := p.MarkFailure(a, &Error{Kind: ErrQuota}); cd != quotaCooldown {
		t.Fatalf("cooldown = %v, want %v", cd, quotaCooldown)
	}
	if _, ok := p.Pick(nil); ok {
		t.Fatal("a memory-only pool stopped honouring its own cooldown")
	}
	// An explicitly empty directory must behave the same way.
	p.AttachState("   ", t.Logf)
	p.MarkSuccess(a)
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("MarkSuccess did not clear the cooldown")
	}
}

// TestPoolStateStagedRecordNeverOverrulesThisProcess: Replace runs on every
// credential change, and it must not resurrect a cooldown that this process has
// already cleared.
func TestPoolStateStagedRecordNeverOverrulesThisProcess(t *testing.T) {
	dir := t.TempDir()
	a := testAuth("u1", "tok-1")
	p := statePool(t, dir, a)
	p.MarkFailure(a, &Error{Kind: ErrQuota})

	// The panel re-reads the credentials while the cooldown is live.
	p.Replace([]*Auth{a})
	if _, ok := p.Pick(nil); ok {
		t.Fatal("Replace dropped a live cooldown")
	}
	// Now the cooldown is cleared, and a Replace must not bring it back from
	// the staged snapshot taken at AttachState.
	p.MarkSuccess(a)
	p.Replace([]*Auth{a})
	if _, ok := p.Pick(nil); !ok {
		t.Fatal("Replace restored a cooldown that had been cleared")
	}
}

// writePoolState is the only place a test writes the file by hand.
func writePoolState(t *testing.T, dir string, st persistedPool) {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if err := os.WriteFile(statePath(dir), raw, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
}
