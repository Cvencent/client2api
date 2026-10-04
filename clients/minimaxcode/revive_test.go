package minimaxcode

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

// freshAuthFixture is the same store after the operator signed in again in
// MiniMax Code: a new access token, a new refresh token, a live expiry.
const freshAuthFixture = `{"schemaVersion":1,"records":{` +
	`"user\u0000mcode-public":{"schemaVersion":1,"accessToken":"mmoat_after_reauth_9001",` +
	`"refreshToken":"mmort_after_reauth_9001","tokenType":"Bearer","clientId":"mcode-public",` +
	`"scopes":["openid"],"audience":"agent","expiresAtMs":4102444800000,` +
	`"generation":9,"loginEpoch":"epoch-2"}}}`

const freshAuthStateFixture = `{"schemaVersion":1,"status":"authenticated","storeKind":"file",` +
	`"clientId":"mcode-public","buildEnv":"cn","region":"prod",` +
	`"generation":9,"expiresAtMs":4102444800000}`

// emptyAuthFixture is what MiniMax Code leaves behind when it signs out.
const emptyAuthFixture = `{"schemaVersion":1,"records":{}}`

// discoveredClient builds a client whose whole store is a temp directory, and
// returns the path of the auth.json plus the id discovery gave the row.
func discoveredClient(t *testing.T, fixture string) (*Client, *fakeTransport, string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "auth")
	path := filepath.Join(root, "cn", "prod", "mcode-public", "auth.json")
	writeFile(t, path, fixture)
	writeFile(t, filepath.Join(filepath.Dir(path), "auth-state.json"), expiredAuthStateFixture)

	c, ft := newTestClient(t, map[string]any{"auto_discover": true, "auth_dir": root},
		func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, reply), nil
		})
	recs := c.pool.recordsForPanel()
	if len(recs) != 1 {
		t.Fatalf("discovery produced %d accounts, want 1", len(recs))
	}
	return c, ft, path, recs[0].ID
}

// newClientAt is newTestClient with a caller-chosen data directory, so a test
// can plant a persisted accounts.json and watch what a fresh process does with
// it.  Everything else is identical.
func newClientAt(t *testing.T, dataDir string, cfg map[string]any, handler func(*http.Request) (*http.Response, error)) *Client {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	if _, ok := cfg["auto_discover"]; !ok {
		cfg["auto_discover"] = false
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal test config: %v", err)
	}
	built, err := New(core.Deps{
		DataDir:    dataDir,
		Config:     json.RawMessage(raw),
		HTTPClient: &http.Client{Transport: &fakeTransport{handler: handler}},
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := built.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", built)
	}
	return c
}

// Revive is the operator's "I signed in again on the vendor side" button.  It
// must pick up the credential the client just wrote, clear the park, and leave
// the row usable -- without this module minting anything itself.
func TestReviveAdoptsTheCredentialAFreshSignInLeft(t *testing.T) {
	c, _, path, id := discoveredClient(t, expiredAuthFixture)
	c.pool.noteFailure(id, core.FailureAuth, "HTTP 401: invalid access token")

	if row, _ := c.pool.current(id); row.State != stateInvalid {
		t.Fatalf("the fixture did not park the row: state = %q", row.State)
	}

	// The operator signs in again in MiniMax Code; the vendor client rewrites
	// the store this module reads.
	writeFile(t, path, freshAuthFixture)
	writeFile(t, filepath.Join(filepath.Dir(path), "auth-state.json"), freshAuthStateFixture)

	if err := c.ReviveAccount(context.Background(), id); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}

	row, ok := c.pool.current(id)
	if !ok {
		t.Fatal("the revived account vanished from the pool")
	}
	if row.Token != "mmoat_after_reauth_9001" || row.RefreshToken != "mmort_after_reauth_9001" {
		t.Fatalf("revive left the old credential in place: %q / %q", row.Token, row.RefreshToken)
	}
	if row.State != stateReady || row.Failures != 0 || row.LastError != "" {
		t.Fatalf("revive left the penalty in place: %+v", row)
	}
	if !core.CapabilitiesOf(context.Background(), c).Revive {
		t.Fatal("the panel is not told this module can revive an account")
	}
}

// A revive with nothing new to adopt must fail loudly instead of clearing the
// park and pretending the dead credential now works.
func TestReviveWithoutANewSignInExplainsWhatToDo(t *testing.T) {
	c, _, path, id := discoveredClient(t, expiredAuthFixture)
	c.pool.noteFailure(id, core.FailureAuth, "HTTP 401: invalid access token")

	err := c.ReviveAccount(context.Background(), id)
	if err == nil {
		t.Fatal("ReviveAccount succeeded without a fresh credential")
	}
	if !strings.Contains(err.Error(), "sign in again") {
		t.Fatalf("the error does not name the remedy: %v", err)
	}
	if row, _ := c.pool.current(id); row.State != stateInvalid {
		t.Fatalf("a failed revive cleared the park anyway: state = %q", row.State)
	}

	// A store the client emptied is the same answer, not a success.
	writeFile(t, path, emptyAuthFixture)
	if err := c.ReviveAccount(context.Background(), id); err == nil || !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("revive against an empty store = %v, want a clear failure", err)
	}

	// An unknown id is an error, never a silent success.
	if err := c.ReviveAccount(context.Background(), "minimaxcode:nope"); err == nil {
		t.Fatal("revive accepted an id this module never held")
	}
}

// A typed credential has no store behind it, so revive is the plain Reviver
// contract: drop the penalty and let the vendor judge it again.
func TestReviveClearsThePenaltyOnATypedCredential(t *testing.T) {
	c, _ := newTestClient(t, nil, nil)
	rec, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": testToken},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	c.pool.noteFailure(rec.ID, core.FailureAuth, "HTTP 401: invalid access token")

	if err := c.ReviveAccount(context.Background(), rec.ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	row, _ := c.pool.current(rec.ID)
	if row.State != stateReady || row.Failures != 0 || row.Token != testToken {
		t.Fatalf("revive did not restore the typed credential: %+v", row)
	}
}

// When the client signs out it empties the store, and the access token in this
// process is dead with it.  The row must say why in the panel's own vocabulary
// instead of spending a request to collect another HTTP 401.
func TestASignOutParksTheRowAsNeedingALogin(t *testing.T) {
	c, ft, path, id := discoveredClient(t, expiredAuthFixture)
	writeFile(t, path, emptyAuthFixture)

	got := c.ensureFresh(context.Background(), id)
	if got.Token != "mmoat_stale_9001" {
		t.Fatalf("ensureFresh invented a token: %q", got.Token)
	}
	if ft.count() != 0 {
		t.Fatalf("a signed-out credential still sent %d upstream request(s)", ft.count())
	}
	row, _ := c.pool.current(id)
	if row.State != stateInvalid || row.LastError != noteLoginRequired {
		t.Fatalf("the row does not explain the sign-out: state=%q note=%q", row.State, row.LastError)
	}
}

// MiniMax retires the refresh token the moment it is exchanged, and the desktop
// client shares the store.  If the client refreshed while this process was not
// looking, adopting its record must win over spending the copy in memory:
// otherwise the loser of that race writes a retired token over the live one.
func TestEnsureFreshAdoptsTheClientsTokenInsteadOfSpendingItsOwn(t *testing.T) {
	c, ft, path, id := discoveredClient(t, expiredAuthFixture)

	// The desktop client refreshed first and wrote a token this process has
	// never seen.  (A live expiry, so it is not itself due for renewal.)
	writeFile(t, path, `{"schemaVersion":1,"records":{`+
		`"user\u0000mcode-public":{"schemaVersion":1,"accessToken":"mmoat_client_won_9001",`+
		`"refreshToken":"mmort_client_won_9001","tokenType":"Bearer","clientId":"mcode-public",`+
		`"expiresAtMs":4102444800000,"generation":8}}}`)

	got := c.ensureFresh(context.Background(), id)
	if got.Token != "mmoat_client_won_9001" || got.RefreshToken != "mmort_client_won_9001" {
		t.Fatalf("ensureFresh kept its own copy: %q / %q", got.Token, got.RefreshToken)
	}
	if ft.count() != 0 {
		t.Fatalf("ensureFresh exchanged a token the client had already rotated: %d request(s)", ft.count())
	}

	// The adopted token must survive as the pool's row, and the store must
	// still be the client's own file.
	row, _ := c.pool.current(id)
	if row.Token != "mmoat_client_won_9001" {
		t.Fatalf("the pool row was not updated: %q", row.Token)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the store back: %v", err)
	}
	var doc struct {
		Records map[string]struct {
			AccessToken string `json:"accessToken"`
		} `json:"records"`
	}
	if err := json.Unmarshal(blob, &doc); err != nil {
		t.Fatalf("the store is no longer readable JSON: %v", err)
	}
	for _, rec := range doc.Records {
		if rec.AccessToken != "mmoat_client_won_9001" {
			t.Fatalf("the store was rewritten with %q", rec.AccessToken)
		}
	}
}

// A sign-out used to make the row vanish entirely, leaving the operator an
// empty pool and no explanation.  The persisted state must bring it back as an
// unselectable "needs a login" row whose 恢复 button adopts whatever the next
// sign-in writes to the store.
func TestASignedOutRowSurvivesARestartAndRevivesAfterALogin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "auth")
	dir := filepath.Join(root, "prod", "cn", "mcode-public")
	writeFile(t, filepath.Join(dir, "auth.json"), emptyAuthFixture)
	writeFile(t, filepath.Join(dir, "auth-state.json"),
		`{"schemaVersion":2,"status":"anonymous","clientId":"mcode-public","buildEnv":"prod","region":"cn"}`)

	dataDir := t.TempDir()
	writeFile(t, filepath.Join(dataDir, "accounts.json"),
		`{"version":1,"accounts":[{"id":"minimaxcode:prod/cn/mcode-public","state":"invalid",`+
			`"failures":3,"last_error":"HTTP 401: invalid access token"}]}`)

	c := newClientAt(t, dataDir, map[string]any{"auto_discover": true, "auth_dir": root},
		func(*http.Request) (*http.Response, error) { return jsonResponse(200, reply), nil })

	recs := c.pool.recordsForPanel()
	if len(recs) != 1 {
		t.Fatalf("a signed-out row vanished from the panel: %d accounts", len(recs))
	}
	if recs[0].State != stateInvalid || recs[0].Note != noteLoginRequired {
		t.Fatalf("the row does not explain the sign-out: state=%q note=%q", recs[0].State, recs[0].Note)
	}
	if !recs[0].Enabled {
		t.Fatal("the placeholder row is disabled, so the panel hides its buttons")
	}

	// The operator signs back in; revive adopts the fresh credential in place.
	writeFile(t, filepath.Join(dir, "auth.json"), freshAuthFixture)
	if err := c.ReviveAccount(context.Background(), recs[0].ID); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	row, ok := c.pool.current(recs[0].ID)
	if !ok || row.State != stateReady || row.Token != "mmoat_after_reauth_9001" {
		t.Fatalf("the sign-in was not adopted: %+v", row)
	}
}
