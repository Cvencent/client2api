package qwenwork

// revive_test.go — core.Reviver for qwenwork.
//
// QwenWork stores a cooldown TWICE: on the live entry and on the account record
// that is re-read at load.  These tests therefore assert the revival through the
// picker and through the files a restart reads, because clearing only the
// in-memory copy leaves the account parked again after a reload.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// reviveClient builds a Client over dir, so a second one can prove a revival
// survived a restart.
func reviveClient(t *testing.T, dir string) *Client {
	t.Helper()
	clearCredentialEnv(t)
	deps := core.Deps{
		DataDir:    dir,
		Logf:       func(string, ...any) {},
		HTTPClient: &http.Client{Transport: offlineTransport()},
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.(*Client)
}

// storedAccounts reads the credential store back from disk.
func storedAccounts(t *testing.T, dir string) []account {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatalf("read %s: %v", accountsFile, err)
	}
	var accts []account
	if err := json.Unmarshal(raw, &accts); err != nil {
		t.Fatalf("parse %s: %v", accountsFile, err)
	}
	return accts
}

// TestQwenworkReviveUnparksAnAccount pins the basic contract: a quota park is
// gone, and the account is handed to a request again.
func TestQwenworkReviveUnparksAnAccount(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-revive", "uid": "u-revive"})

	c.pool.markFailure(c.pool.find(rec.ID), kindQuota, "quota exhausted")
	if c.pool.ready() {
		t.Fatal("a parked account must not keep the pool ready")
	}
	if c.pool.pick(nil) != nil {
		t.Fatal("a parked account must not be picked")
	}

	if err := c.ReviveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.ready() {
		t.Fatal("a revived account must be usable")
	}
	if got := c.pool.pick(nil); got == nil || got.acct.id() != rec.ID {
		t.Fatalf("pick after revive = %v", got)
	}
	recs, err := c.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(recs) != 1 || recs[0].State != stateReady || !recs[0].Enabled {
		t.Fatalf("after revive = %+v, want one ready, enabled account", recs)
	}
	if n, _ := recs[0].Fields["failures"].(int); n != 0 {
		t.Fatalf("the failure counter survived the revive: %v", recs[0].Fields["failures"])
	}
	if recs[0].Fields["cooldown_until"] != nil {
		t.Fatalf("the cooldown survived the revive: %v", recs[0].Fields["cooldown_until"])
	}
}

// TestQwenworkReviveClearsADeadAccount pins the verdict only operator action can
// lift: markDead disables the credential outright.
func TestQwenworkReviveClearsADeadAccount(t *testing.T) {
	c := panelClient(t, "", nil)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-dead", "uid": "u-dead"})

	e := c.pool.find(rec.ID)
	c.pool.markDead(e, "refresh token rejected")
	if c.pool.ready() {
		t.Fatal("a dead account must not keep the pool ready")
	}
	if !e.acct.Disabled {
		t.Fatal("markDead should disable the credential")
	}

	if err := c.ReviveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.ready() {
		t.Fatal("a revived account must be usable")
	}
	recs, _ := c.Accounts(ctx)
	if len(recs) != 1 || !recs[0].Enabled || recs[0].State != stateReady {
		t.Fatalf("after revive = %+v, want one ready, enabled account", recs)
	}
	if recs[0].Note != "" {
		t.Errorf("note = %q, want it cleared", recs[0].Note)
	}
}

// TestQwenworkReviveRejectsUnknownIDs pins requirement 4.  The panel turns this
// error into a 404, so a silent success would lie to the operator.
func TestQwenworkReviveRejectsUnknownIDs(t *testing.T) {
	c := panelClient(t, "", nil)
	addPanelAccount(t, c, map[string]string{"access_token": "tok-known", "uid": "u-known"})
	ctx := context.Background()

	for _, id := range []string{"", "   ", "uid:nope"} {
		if err := c.ReviveAccount(ctx, id); err == nil {
			t.Fatalf("ReviveAccount(%q) succeeded, want an error", id)
		}
	}
}

// TestQwenworkReviveEnablesAParkedCredential pins requirements 2 and 4: revive
// and enable are one intent, and the credential is only switched back on --
// never re-minted.
func TestQwenworkReviveEnablesAParkedCredential(t *testing.T) {
	const token = "QWENWORK-REVIVE-TOKEN-0001"
	dir := t.TempDir()
	c := reviveClient(t, dir)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": token, "uid": "u-park"})

	if err := c.SetAccountEnabled(ctx, rec.ID, false); err != nil {
		t.Fatalf("park the credential: %v", err)
	}
	if c.pool.ready() {
		t.Fatal("a parked credential must not keep the pool ready")
	}
	if err := c.ReviveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !c.pool.ready() {
		t.Fatal("the pool should be ready again")
	}

	accts := storedAccounts(t, dir)
	if len(accts) != 1 {
		t.Fatalf("%d stored accounts, want 1", len(accts))
	}
	got := accts[0]
	if got.AccessToken != token {
		t.Fatal("the access token changed; a revive must not mint one")
	}
	if got.RefreshToken != "" {
		t.Fatalf("a refresh token was invented: %q", got.RefreshToken)
	}
	if got.Disabled {
		t.Fatal("the credential is still disabled on disk")
	}
	if got.CooldownUntil != 0 || got.LastError != "" {
		t.Fatalf("the penalty survived on disk: cooldown=%d last_error=%q", got.CooldownUntil, got.LastError)
	}
}

// TestQwenworkReviveSurvivesAReload pins requirement 5.  The cooldown is stored
// on the account record itself, so only a store write that drops it keeps the
// next load from re-parking the account.
func TestQwenworkReviveSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	c := reviveClient(t, dir)
	ctx := context.Background()
	rec := addPanelAccount(t, c, map[string]string{"access_token": "tok-reload", "uid": "u-reload"})

	c.pool.markFailure(c.pool.find(rec.ID), kindQuota, "quota exhausted")
	raw, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil || !strings.Contains(string(raw), "cooldown_until") {
		t.Fatalf("the park did not reach %s: %v (%s)", accountsFile, err, raw)
	}

	if err := c.ReviveAccount(ctx, rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	for _, a := range storedAccounts(t, dir) {
		if a.id() == rec.ID && (a.CooldownUntil != 0 || a.Disabled) {
			t.Fatalf("the stored record is still parked: %+v", a)
		}
	}

	// A restart is a second client over the same data directory.
	again := reviveClient(t, dir)
	if !again.pool.ready() {
		t.Fatal("the revive did not survive the reload")
	}
	if got := again.pool.pick(nil); got == nil || got.acct.id() != rec.ID {
		t.Fatalf("pick after reload = %v", got)
	}
}
