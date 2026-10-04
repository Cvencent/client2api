package kimi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reviveClient builds a client over its own empty DataDir with the `cli-login`
// row live, which is the shape the panel shows when an operator presses revive.
func reviveClient(t *testing.T, dataDir string) *Client {
	t.Helper()
	isolateCredentials(t)
	noCLIOnPath(t)
	return newClientIn(t, dataDir, map[string]any{"assume_logged_in": true})
}

// stateOnDisk is the parsed state file: what the next process would read.
func stateOnDisk(t *testing.T, c *Client) accountState {
	t.Helper()
	st, err := c.loadState()
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	return st
}

// TestKimiReviveClearsADeadVerdict: a dead account is not selectable, time
// never revives it, and ReviveAccount does -- in memory and on disk, so a
// restart cannot undo the operator's decision.
func TestKimiReviveClearsADeadVerdict(t *testing.T) {
	dir := t.TempDir()
	c := reviveClient(t, dir)
	useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	c.markDead(cliLoginID, causeNoCredential, "no login credential was found")
	if c.selectable(cliLoginID) {
		t.Fatal("a dead account must not be selectable")
	}

	if err := c.ReviveAccount(context.Background(), cliLoginID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	// Asserted through the gate the request path consults, not through the
	// record alone: a revive that only edits a struct is not a revive.
	if !c.selectable(cliLoginID) {
		t.Fatalf("revive left the account unusable: %q", c.healthNote(cliLoginID))
	}
	if rec := c.healthOf(cliLoginID); rec != (healthRecord{}) {
		t.Fatalf("revive left health in memory: %+v", rec)
	}
	if rec, ok := stateOnDisk(t, c).Health[cliLoginID]; ok {
		t.Fatalf("revive left health on disk: %+v", rec)
	}

	c2 := reviveClient(t, dir)
	if !c2.selectable(cliLoginID) {
		t.Fatalf("the revival did not survive a reload: %q", c2.healthNote(cliLoginID))
	}
}

// TestKimiReviveBringsBackACooledBinding: the pick path really is what comes
// back, and the imported path -- the only credential this module owns -- is
// left exactly as the operator imported it.
func TestKimiReviveBringsBackACooledBinding(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, id, dir := newBoundClient(t, quietStubBody(), nil)
	useFakeClock(c, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))

	before := stateOnDisk(t, c).Bindings[0]
	c.markCooling(id, causeProcessFailed, "the CLI exited non-zero", time.Hour)
	if _, ok := c.boundBinding(); ok {
		t.Fatal("a cooling binding must not be picked")
	}

	if err := c.ReviveAccount(context.Background(), id); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	got, ok := c.boundBinding()
	if !ok || got.ID != id {
		t.Fatalf("boundBinding after revive = %+v, %v; want the binding back", got, ok)
	}
	if after := stateOnDisk(t, c).Bindings[0]; after != before {
		t.Fatalf("revive changed the imported binding:\n before %+v\n after  %+v", before, after)
	}

	c2 := newClientIn(t, dir, map[string]any{"assume_logged_in": true})
	if got2, ok2 := c2.boundBinding(); !ok2 || got2.ID != id {
		t.Fatalf("the revival did not survive a reload: %+v, %v", got2, ok2)
	}
}

// TestKimiReviveRejectsUnknownIDs: the panel turns this error into a 404, so
// an id that is not in the table must fail and must not invent a state file.
func TestKimiReviveRejectsUnknownIDs(t *testing.T) {
	dir := t.TempDir()
	c := reviveClient(t, dir)

	for _, bad := range []string{"", "   ", "no-such-account"} {
		if err := c.ReviveAccount(context.Background(), bad); err == nil {
			t.Errorf("ReviveAccount(%q) = nil, want an error", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, accountsFileName)); !os.IsNotExist(err) {
		t.Fatalf("a rejected revive wrote %s (err=%v): a state file must record a choice, not a lookup", accountsFileName, err)
	}
}

// TestKimiReviveClearsAnOperatorPark: the park the operator made and the
// verdict this module learned are both cleared, and nothing else in the DataDir
// is created -- reviving is not an excuse to mint a credential.
func TestKimiReviveClearsAnOperatorPark(t *testing.T) {
	dir := t.TempDir()
	c := reviveClient(t, dir)

	if err := c.SetAccountEnabled(context.Background(), cliLoginID, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}
	c.markDead(cliLoginID, causeCLINotFound, "the CLI was not found")
	if !c.accountExplicitlyDisabled(cliLoginID) {
		t.Fatal("the operator's park was not recorded")
	}

	refsBefore := strings.Join(c.credentials().foundRefs(), ",")
	if err := c.ReviveAccount(context.Background(), cliLoginID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if c.accountExplicitlyDisabled(cliLoginID) {
		t.Fatal("revive left the account parked")
	}
	if !c.selectable(cliLoginID) {
		t.Fatalf("revive left the account unusable: %q", c.healthNote(cliLoginID))
	}
	st := stateOnDisk(t, c)
	if !st.Enabled[cliLoginID] {
		t.Fatalf("the cleared park was not persisted: %+v", st.Enabled)
	}
	if rec, ok := st.Health[cliLoginID]; ok {
		t.Fatalf("revive left health on disk: %+v", rec)
	}
	if after := strings.Join(c.credentials().foundRefs(), ","); after != refsBefore {
		t.Fatalf("revive changed the credential evidence: %q -> %q", refsBefore, after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != accountsFileName {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("revive wrote %v into the DataDir, want only %s", names, accountsFileName)
	}
}

// TestKimiReviveLeavesOtherChoicesAlone: reviving one account is not a licence
// to forget what the operator decided about the others.
func TestKimiReviveLeavesOtherChoicesAlone(t *testing.T) {
	dir := t.TempDir()
	writeAccountsRaw(t, dir, `{"version":2,"enabled":{"`+cliLoginID+`":false,"`+webLoginID+`":false}}`)
	c := reviveClient(t, dir)

	if err := c.ReviveAccount(context.Background(), cliLoginID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	st := stateOnDisk(t, c)
	if !st.Enabled[cliLoginID] {
		t.Fatalf("the revived account is not enabled: %+v", st.Enabled)
	}
	if st.Enabled[webLoginID] {
		t.Fatalf("reviving one account re-enabled another: %+v", st.Enabled)
	}
	if raw := accountsFileBytes(t, dir); strings.Contains(raw, healthDead) || strings.Contains(raw, healthCooling) {
		t.Fatalf("a health verdict survived the revive: %s", raw)
	}
}
