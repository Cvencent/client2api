package zcode

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// reviveEnv builds a client with one config-provided account ("k1") over a data
// directory the caller can reopen, so a revival can be observed across a
// restart.  The transport is the caller's, which is how a test counts attempts.
func reviveEnv(t *testing.T, transport *fakeTransport) (*Client, string) {
	t.Helper()
	isolateHome(t)
	dir := t.TempDir()
	return reviveClientAt(t, dir, transport), dir
}

func reviveClientAt(t *testing.T, dir string, transport *fakeTransport) *Client {
	t.Helper()
	c, err := New(core.Deps{
		DataDir:    dir,
		Config:     json.RawMessage(accountsConfigJSON(1)),
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}
	return client
}

// selectableAt asks the pool the same question next() asks, at an arbitrary
// instant, so a "permanent" park can be proven permanent.
func selectableAt(t *testing.T, p *pool, id string, now time.Time) bool {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID == id {
			return p.selectableLocked(a, now)
		}
	}
	t.Fatalf("no account with id %q", id)
	return false
}

// TestZcodeReviveUnparksAnAccount drives the real Chat path into an exhausted
// park and asserts the operator override makes the account selectable again
// without touching the network.
func TestZcodeReviveUnparksAnAccount(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusPaymentRequired, `{"error":{"message":"insufficient balance"}}`), nil
	}}
	c, _ := reviveEnv(t, transport)

	if _, err := chatOnce(t.Context(), c); err == nil {
		t.Fatal("Chat returned no error")
	}
	if got := c.pool.usableCount(); got != 0 {
		t.Fatalf("an exhausted account must not be selectable: usableCount = %d", got)
	}
	attempts := transport.count()

	if err := c.ReviveAccount(context.Background(), "k1"); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if got := transport.count(); got != attempts {
		t.Errorf("a revive must not call the vendor: attempts = %d, want %d", got, attempts)
	}
	if got := c.pool.usableCount(); got != 1 {
		t.Errorf("usableCount after revive = %d, want 1", got)
	}
	if acct := c.pool.next(map[string]bool{}); acct == nil || acct.ID != "k1" {
		t.Errorf("next after revive = %v, want k1", acct)
	}
	rec := recordsByID(t, c)["k1"]
	if rec.State != stateReady {
		t.Errorf("state after revive = %q, want %q", rec.State, stateReady)
	}
	if rec.Note != "" {
		t.Errorf("revive left a verdict behind: note = %q", rec.Note)
	}
}

// TestZcodeReviveClearsAPermanentInvalidPark pins the strongest case: the
// invalid state has no deadline, so only an override (or new evidence) can
// bring the account back.
func TestZcodeReviveClearsAPermanentInvalidPark(t *testing.T) {
	c, dir := reviveEnv(t, &fakeTransport{})
	c.pool.mark("k1", func(a *Account) {
		a.State = stateInvalid
		a.Note = "credential rejected"
		a.LastError = "401 invalid api key"
	})

	far := time.Now().Add(365 * 24 * time.Hour)
	if selectableAt(t, c.pool, "k1", far) {
		t.Fatal("an invalid account must never become selectable by waiting")
	}
	if err := c.ReviveAccount(context.Background(), "k1"); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if !selectableAt(t, c.pool, "k1", time.Now()) {
		t.Error("the revived account is still not selectable")
	}

	// The revival has to be on disk too, or the next load re-parks the account.
	c2 := reviveClientAt(t, dir, &fakeTransport{})
	if got := c2.pool.usableCount(); got != 1 {
		t.Errorf("after a restart usableCount = %d, want 1", got)
	}
	if rec := recordsByID(t, c2)["k1"]; rec.State != stateReady {
		t.Errorf("after a restart state = %q, want %q", rec.State, stateReady)
	}
	raw, err := os.ReadFile(filepath.Join(dir, accountsFile))
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(raw), stateInvalid) || strings.Contains(string(raw), "credential rejected") {
		t.Errorf("the state file still holds the penalty: %s", raw)
	}
}

// TestZcodeReviveRejectsUnknownIDs keeps the panel's 404 contract honest.
func TestZcodeReviveRejectsUnknownIDs(t *testing.T) {
	c, _ := reviveEnv(t, &fakeTransport{})
	for _, id := range []string{"", "   ", "no-such-account"} {
		if err := c.ReviveAccount(context.Background(), id); err == nil {
			t.Errorf("ReviveAccount(%q) = nil, want an error", id)
		}
	}
}

// TestZcodeReviveEnablesAParkedCredential covers the operator's other half of
// the intent: a credential switched off must come back, and the credential
// itself must be untouched.
func TestZcodeReviveEnablesAParkedCredential(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	c := env.client(t, &fakeTransport{})

	rec, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		fieldAPIKey: testAPIKey,
	}})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	c.pool.mark(rec.ID, func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
		a.Note = "rate limited"
		a.LastError = "429 slow down"
	})
	if err := c.SetAccountEnabled(context.Background(), rec.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got := c.pool.usableCount(); got != 0 {
		t.Fatalf("a parked credential must not be selectable: usableCount = %d", got)
	}
	before := recordsByID(t, c)[rec.ID]

	if err := c.ReviveAccount(context.Background(), rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if got := c.pool.usableCount(); got != 1 {
		t.Errorf("usableCount after revive = %d, want 1", got)
	}
	got := recordsByID(t, c)[rec.ID]
	if !got.Enabled {
		t.Error("the revived account is still disabled")
	}
	if got.State != stateReady || got.Note != "" {
		t.Errorf("revive left the penalty behind: state = %q note = %q", got.State, got.Note)
	}
	if got.Fields["fingerprint"] != before.Fields["fingerprint"] {
		t.Errorf("the credential changed: fingerprint %v -> %v",
			before.Fields["fingerprint"], got.Fields["fingerprint"])
	}

	// The store must have kept the credential byte-for-byte: a revive clears
	// verdicts, it never mints or rewrites a secret.
	store, err := os.ReadFile(env.storePath())
	if err != nil {
		t.Fatalf("read managed store: %v", err)
	}
	var ms managedStore
	if err := json.Unmarshal(store, &ms); err != nil {
		t.Fatalf("parse managed store: %v", err)
	}
	if len(ms.Accounts) != 1 {
		t.Fatalf("managed store holds %d accounts, want 1", len(ms.Accounts))
	}
	if ms.Accounts[0].ID != rec.ID || ms.Accounts[0].APIKey != testAPIKey {
		t.Errorf("the credential changed: id = %q key = %q", ms.Accounts[0].ID, ms.Accounts[0].APIKey)
	}
	if ms.Accounts[0].Enabled == nil || !*ms.Accounts[0].Enabled {
		t.Error("the store does not remember the revived (enabled) state")
	}

	// And it survives a restart through both the store and the state file.
	c2 := env.client(t, &fakeTransport{})
	if got := c2.pool.usableCount(); got != 1 {
		t.Errorf("after a restart usableCount = %d, want 1", got)
	}
	if rec2 := recordsByID(t, c2)[rec.ID]; !rec2.Enabled || rec2.State != stateReady {
		t.Errorf("after a restart the account is not usable: enabled = %v state = %q", rec2.Enabled, rec2.State)
	}
}
