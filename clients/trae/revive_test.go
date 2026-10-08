package trae

// revive_test.go — core.Reviver for trae.
//
// The revival is asserted through the picker, the per-model predicate and the
// on-disk records the next start reads back, because a cleared struct field
// proves nothing about whether the account can actually serve a request again.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// reviveClient builds a Client whose pool and account store live in dir, with
// automatic discovery off so no test can touch the developer's real Trae
// credential.
func reviveClient(t *testing.T, dir string) *Client {
	t.Helper()
	cfg := loadConfig(nil, nil)
	off := false
	cfg.AutoDiscover = &off
	c := testClient(t, cfg, nil, nil)
	c.dataDir = dir
	c.store = loadAccountStore(dir)
	c.pool = NewPool(nil)
	c.pool.AttachState(dir, t.Logf)
	return c
}

// TestTraeReviveUnparksAnAccount pins the basic contract: a plan-limit park is
// gone and the account is selectable again immediately.
func TestTraeReviveUnparksAnAccount(t *testing.T) {
	a := testAuth("u1", "tok-1")
	c := testClient(t, nil, []*Auth{a}, nil)

	if _, d := c.pool.MarkFailure(a, &Error{Kind: ErrPlanLimit, Status: 200}); d != planLimitCooldown {
		t.Fatalf("park = %v, want %v", d, planLimitCooldown)
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

// TestTraeReviveClearsAPermanentInvalidPark pins the one verdict a deadline
// cannot lift: MarkInvalid has no until, so only an operator override (or fresh
// evidence) can free the account.
func TestTraeReviveClearsAPermanentInvalidPark(t *testing.T) {
	a := testAuth("u1", "tok-1")
	c := testClient(t, nil, []*Auth{a}, nil)
	c.pool.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	c.pool.MarkInvalid(a, "credential rejected")
	if c.pool.Ready() {
		t.Fatal("an invalid account must not be ready")
	}
	c.pool.now = func() time.Time { return time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC) }
	if c.pool.Ready() {
		t.Fatal("an invalid park must not expire by time")
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.Ready() {
		t.Fatal("a revived invalid account must be usable")
	}
	if _, ok := c.pool.Pick(nil); !ok {
		t.Fatal("a revived invalid account must be picked again")
	}
}

// TestTraeReviveClearsModelParks pins requirement 5: the operator override is
// total, so it lifts both a vendor refusal and a rate-limit reset window.
func TestTraeReviveClearsModelParks(t *testing.T) {
	a := testAuth("u1", "tok-1")
	c := testClient(t, nil, []*Auth{a}, nil)

	if d := c.pool.MarkModelBlocked(a, "model-a", modelBlockReason); d <= 0 {
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
		// The key is always present for a module that reports model parks;
		// what must be gone is every entry in it.
		if parks, ok := s.Extra["model_cooldowns"].([]core.ModelPark); ok && len(parks) > 0 {
			t.Fatalf("model parks survived the revive: %v", parks)
		}
	}
}

// TestTraeReviveRejectsUnknownIDs pins requirement 4.  The panel turns this
// error into a 404, so a silent success would lie to the operator.
func TestTraeReviveRejectsUnknownIDs(t *testing.T) {
	// The real shape: an account store plus a pool, as New builds them.
	c := reviveClient(t, t.TempDir())
	ctx := context.Background()

	for _, id := range []string{"", "   ", "no-such-account"} {
		if err := c.ReviveAccount(ctx, id); err == nil {
			t.Fatalf("ReviveAccount(%q) succeeded, want an error", id)
		}
	}
}

// TestTraeReviveEnablesAParkedCredential pins requirements 2 and 4: revive and
// enable are one intent, and the stored credential is only switched back on --
// never re-minted, never rewritten with a different token.
func TestTraeReviveEnablesAParkedCredential(t *testing.T) {
	const token = "TRAE-REVIVE-TOKEN-0001"
	dir := t.TempDir()
	c := reviveClient(t, dir)
	ctx := context.Background()
	rec := addTestAccount(t, c, map[string]string{"access_token": token})

	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("park the credential: %v", err)
	}
	if c.pool.Len() != 0 {
		t.Fatal("a disabled credential must leave the pool")
	}
	if err := c.ReviveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if c.pool.Len() != 1 || !c.pool.Ready() {
		t.Fatalf("pool after revive: %d account(s), ready=%v", c.pool.Len(), c.pool.Ready())
	}

	// Read the store back from disk: the revival has to be durable, and the
	// credential itself has to be byte-for-byte the one the operator pasted.
	onDisk := loadAccountStore(dir)
	if len(onDisk.Accounts) != 1 {
		t.Fatalf("%d stored accounts, want 1", len(onDisk.Accounts))
	}
	got := onDisk.Accounts[0]
	if got.ID != rec.ID || got.AccessToken != token {
		t.Fatal("the credential changed; a revive must not mint or replace a token")
	}
	if got.RefreshToken != "" {
		t.Fatalf("a refresh token was invented: %q", got.RefreshToken)
	}
	if onDisk.suppressed(rec.ID) {
		t.Fatal("the account is still suppressed after the revive")
	}
}

// TestTraeReviveSurvivesAReload pins requirement 5.  The stale unhealthy record
// has to leave the state file, or the next start re-parks the account the
// operator just freed.
func TestTraeReviveSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	a := testAuth("u1", "tok-1")
	c := reviveClient(t, dir)
	c.pool.Replace([]*Auth{a})

	if _, d := c.pool.MarkFailure(a, &Error{Kind: ErrQuota, Status: 200}); d != quotaCooldown {
		t.Fatalf("park = %v, want %v", d, quotaCooldown)
	}
	raw, err := os.ReadFile(statePath(dir))
	if err != nil || !strings.Contains(string(raw), a.ID()) {
		t.Fatalf("the park did not reach %s: %v (%s)", poolStateFile, err, raw)
	}

	if err := c.ReviveAccount(context.Background(), a.ID()); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	raw, err = os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatalf("read %s: %v", poolStateFile, err)
	}
	if strings.Contains(string(raw), a.ID()) {
		t.Fatalf("%s still records the revived account: %s", poolStateFile, raw)
	}

	// A restart is a fresh pool over the same directory, exactly as New builds
	// it.  The revived account must still be selectable.
	restarted := statePool(t, dir, testAuth("u1", "tok-1"))
	if !restarted.Ready() {
		t.Fatal("the revive did not survive the reload")
	}
	if picked, ok := restarted.Pick(nil); !ok || picked.ID() != a.ID() {
		t.Fatalf("pick after reload = %v/%v", picked, ok)
	}
}
