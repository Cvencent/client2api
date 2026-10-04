package workbuddy

// revive_test.go — core.Reviver for workbuddy.
//
// The point of these tests is that a revive is visible through the paths the
// module actually uses: the picker, the per-model predicate and the file the
// next start reads back.  Asserting a cleared struct field would pass even if
// selection still refused the account.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reviveTestAccount returns the one credential newTestClient installed.
func reviveTestAccount(t *testing.T, c *Client) *Auth {
	t.Helper()
	accounts := c.pool.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("test setup: %d accounts, want 1", len(accounts))
	}
	return accounts[0]
}

// TestWorkbuddyReviveUnparksAnAccount pins the basic contract: a parked account
// is selectable again immediately, through the picker.
func TestWorkbuddyReviveUnparksAnAccount(t *testing.T) {
	c, _ := newTestClient(t, nil)
	a := reviveTestAccount(t, c)

	// Any positive park will do here: this test is about Revive, and the exact
	// shape of a hard-credit park (until the next 04:00) is pinned next door in
	// TestWorkbuddyPoolStateCooldownSurvivesARestart.
	if _, d := c.pool.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"}); d <= 0 {
		t.Fatalf("park = %v, want a positive cooldown", d)
	}
	if c.pool.Ready() {
		t.Fatal("the account should be parked")
	}
	if _, ok := c.pool.Pick(nil); ok {
		t.Fatal("a parked account must not be picked")
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.Ready() {
		t.Fatal("a revived account must be usable")
	}
	picked, ok := c.pool.Pick(nil)
	if !ok || picked.ID() != a.ID() {
		t.Fatalf("pick after revive = %v/%v", picked, ok)
	}
	snap := c.pool.Snapshot()
	if len(snap) != 1 || snap[0].State != stateReady {
		t.Fatalf("snapshot = %+v, want one ready account", snap)
	}
	if n, has := snap[0].Extra["failures"]; has {
		t.Fatalf("the failure counter survived the revive: %v", n)
	}
}

// TestWorkbuddyReviveClearsASessionDeadPark pins requirement 1 for the verdict
// that MarkSuccess alone cannot lift before its deadline: the repeated-12153
// escalation into stateInvalid.
func TestWorkbuddyReviveClearsASessionDeadPark(t *testing.T) {
	c, _ := newTestClient(t, nil)
	a := reviveTestAccount(t, c)

	for i := 0; i < sessionDeadThreshold; i++ {
		c.pool.MarkFailure(a, &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"})
	}
	snap := c.pool.Snapshot()
	if len(snap) != 1 || snap[0].State != stateInvalid {
		t.Fatalf("snapshot = %+v, want one invalid account after %d dead sessions", snap, sessionDeadThreshold)
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if _, ok := c.pool.Pick(nil); !ok {
		t.Fatal("a revived session-dead account must be picked again")
	}
	// The escalation counter has to be back at zero, or the next dead session
	// would re-park the account on the first strike rather than the third.
	c.pool.MarkFailure(a, &Error{Kind: ErrSessionDead, Status: 401, Msg: "Offline user session not found"})
	snap = c.pool.Snapshot()
	if len(snap) != 1 || snap[0].State != stateCooling {
		t.Fatalf("state after one dead session = %+v, want cooling", snap)
	}
}

// TestWorkbuddyReviveClearsModelParks pins requirement 5: the operator override
// is total, so it lifts a model park whether it came from a 11102 refusal or
// from the vendor's own rate-limit reset.
func TestWorkbuddyReviveClearsModelParks(t *testing.T) {
	c, _ := newTestClient(t, nil)
	a := reviveTestAccount(t, c)

	if d := c.pool.MarkModelBlocked(a, "model-a", ModelBlockReason); d <= 0 {
		t.Fatalf("MarkModelBlocked = %v, want a park", d)
	}
	if d := c.pool.MarkModelRateLimited(a, "model-b", time.Time{}, time.Hour, modelRateLimitReason); d <= 0 {
		t.Fatalf("MarkModelRateLimited = %v, want a park", d)
	}
	if c.pool.UsableForModel(a.ID(), "model-a") {
		t.Fatal("model-a should be parked")
	}
	if c.pool.UsableForModel(a.ID(), "model-b") {
		t.Fatal("model-b should be parked")
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.UsableForModel(a.ID(), "model-a") || !c.pool.UsableForModel(a.ID(), "model-b") {
		t.Fatal("a reviving operator expects every model park to be lifted")
	}
	for _, s := range c.pool.Snapshot() {
		if parks, has := s.Extra["model_cooldowns"]; has {
			t.Fatalf("model parks survived the revive: %v", parks)
		}
	}
}

// TestWorkbuddyReviveRejectsUnknownIDs pins requirement 4.  The panel turns this
// error into a 404, so a silent success would lie to the operator.
func TestWorkbuddyReviveRejectsUnknownIDs(t *testing.T) {
	c, _ := newTestClient(t, nil)
	ctx := context.Background()

	for _, id := range []string{"", "   ", "no-such-account"} {
		if err := c.ReviveAccount(ctx, id); err == nil {
			t.Fatalf("ReviveAccount(%q) succeeded, want an error", id)
		}
	}
}

// TestWorkbuddyReviveEnablesAParkedCredential pins requirements 2 and 4: revive
// and enable are one intent, and the credential file itself is only renamed --
// never rewritten, never re-minted.
func TestWorkbuddyReviveEnablesAParkedCredential(t *testing.T) {
	c, _ := newTestClient(t, nil)
	a := reviveTestAccount(t, c)
	path := a.FilePath
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential: %v", err)
	}

	if err := c.SetAccountEnabled(context.Background(), a.ID(), false); err != nil {
		t.Fatalf("park the credential: %v", err)
	}
	if _, err := os.Stat(path + disabledSuffix); err != nil {
		t.Fatalf("the parked file is not on disk as %s: %v", filepath.Base(path)+disabledSuffix, err)
	}
	if c.pool.Len() != 0 {
		t.Fatal("a parked credential must leave the pool")
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if c.pool.Len() != 1 || !c.pool.Ready() {
		t.Fatalf("pool after revive: %d account(s), ready=%v", c.pool.Len(), c.pool.Ready())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential after revive: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the credential file changed; a revive must not mint or rewrite a credential")
	}
}

// TestWorkbuddyReviveSurvivesAReload pins requirement 5.  The stale unhealthy
// record has to leave the state file, or the next start re-parks the account the
// operator just freed.
func TestWorkbuddyReviveSurvivesAReload(t *testing.T) {
	c, _ := newTestClient(t, nil)
	a := reviveTestAccount(t, c)
	statePath := filepath.Join(c.poolStateDir(), poolStateFile)

	if _, d := c.pool.MarkFailure(a, &Error{Kind: ErrHardCredit, Status: 402, Msg: "no credit"}); d <= 0 {
		t.Fatalf("park = %v, want a positive cooldown", d)
	}
	if raw, err := os.ReadFile(statePath); err != nil || !strings.Contains(string(raw), a.ID()) {
		t.Fatalf("the park did not reach %s: %v (%s)", poolStateFile, err, raw)
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read %s: %v", poolStateFile, err)
	}
	if strings.Contains(string(raw), a.ID()) {
		t.Fatalf("%s still records the revived account: %s", poolStateFile, raw)
	}

	// A restart is a fresh pool over the same directory, exactly as New builds
	// it.  The revived account must still be selectable.
	again := restartPool(t, c.poolStateDir(), &Auth{AccessToken: "at-reload-only", UID: a.ID()})
	if !again.Ready() {
		t.Fatal("the revive did not survive the reload")
	}
	if picked, ok := again.Pick(nil); !ok || picked.ID() != a.ID() {
		t.Fatalf("pick after reload = %v/%v", picked, ok)
	}
}
