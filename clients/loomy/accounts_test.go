package loomy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// accounts_test.go covers the part of this module that has no upstream to lean
// on: the credential store.  Loomy cannot renew a session, so every test here is
// really about whether the module keeps the truth straight across a restart --
// a working session stays working, and a session the vendor rejected stays
// rejected instead of quietly looking healthy again.

func TestStoredCredentialSurvivesARestart(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	dir := filepath.Dir(c.store.path)
	expires := time.Now().Add(48 * time.Hour).UnixMilli()

	if err := c.store.put(account{
		storedAccount: storedAccount{
			ID:          "loomy-1",
			Label:       "loomy-1",
			AccessToken: testToken,
			UserID:      testUserID,
			Phone:       "13800000000",
			Nickname:    "tester",
			ExpiresAtMS: expires,
			Enabled:     true,
		},
		origin: originStored,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, accountsFile)); err != nil {
		t.Fatalf("the credential file was not written to the data dir: %v", err)
	}

	reloaded := newTestClientInDir(t, dir, "{}", nil)
	got, ok := reloaded.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the credential did not survive a restart")
	}
	if got.AccessToken != testToken {
		t.Errorf("access token = %q, want the one that was stored", got.AccessToken)
	}
	if got.UserID != testUserID || got.Phone != "13800000000" || got.Nickname != "tester" {
		t.Errorf("identity fields were lost: %#v", got.storedAccount)
	}
	if got.ExpiresAtMS != expires {
		t.Errorf("expiry = %d, want %d", got.ExpiresAtMS, expires)
	}
	if !got.Enabled {
		t.Error("the credential came back disabled")
	}
	if got.origin != originStored {
		t.Errorf("origin = %q, want %q", got.origin, originStored)
	}
}

// This is the module's single most important promise: a session the vendor has
// already refused must not come back looking healthy after a restart.
func TestARejectedSessionStaysParkedAcrossARestart(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	dir := filepath.Dir(c.store.path)
	seedAccount(t, c, "loomy-1", testToken, 0)

	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now(), time.Minute)
	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the account vanished")
	}
	if !acc.dead {
		t.Error("a session-dead failure did not park the account")
	}
	if acc.selectable(time.Now()) {
		t.Error("a rejected session is still selectable")
	}

	reloaded := newTestClientInDir(t, dir, "{}", nil)
	again, ok := reloaded.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the credential was lost across the restart")
	}
	if !again.dead {
		t.Error("the park did not survive a restart; the dead session looks healthy again")
	}
	if again.selectable(time.Now()) {
		t.Error("the reloaded account is selectable despite having been rejected")
	}
}

// A 500 is not evidence that the session is gone, so it must cool down rather
// than be parked as dead -- and the cooldown has to actually expire.
func TestATransientFailureCoolsDownRatherThanKillingTheAccount(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)
	now := time.Now()

	c.store.penalise("loomy-1", &apiError{Status: 500, Message: "boom"}, now, time.Hour)
	acc, _ := c.store.lookup("loomy-1")
	if acc.dead {
		t.Error("an upstream 500 parked the account as dead")
	}
	if acc.selectable(now) {
		t.Error("the account ignored its cooldown")
	}
	if !acc.selectable(now.Add(2 * time.Hour)) {
		t.Error("the account is still cooling after the cooldown should have expired")
	}
}

// The only authority on whether a session works is the vendor, so a credential
// past its locally-computed expiry is still tried -- just last.
func TestAnExpiredSessionIsStillTriedButRankedLast(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	now := time.Now()
	seedAccount(t, c, "stale", testTokenTwo, now.Add(-time.Hour).UnixMilli())
	seedAccount(t, c, "fresh", testToken, now.Add(24*time.Hour).UnixMilli())

	stale, _ := c.store.lookup("stale")
	if !stale.expired(now) {
		t.Error("an account past its declared expiry is not reported as expired")
	}
	if !stale.selectable(now) {
		t.Error("an expired session was withheld; the vendor is the authority, not a local stamp")
	}

	got := c.store.candidates(now)
	if len(got) != 2 {
		t.Fatalf("candidates returned %d accounts, want 2", len(got))
	}
	if got[0].ID != "fresh" || got[1].ID != "stale" {
		t.Errorf("candidate order = %s, %s; want fresh before stale", got[0].ID, got[1].ID)
	}
}

func TestCandidatesSkipParkedDisabledAndTokenlessAccounts(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	now := time.Now()
	seedAccount(t, c, "on", testToken, 0)
	seedAccount(t, c, "off", testTokenTwo, 0)
	seedAccount(t, c, "dead", testToken, 0)
	seedAccount(t, c, "blank", "   ", 0)

	if err := c.store.setEnabled("off", false); err != nil {
		t.Fatalf("setEnabled: %v", err)
	}
	c.store.penalise("dead", errSessionExpired("dead"), now, time.Minute)

	got := c.store.candidates(now)
	if len(got) != 1 {
		ids := make([]string, 0, len(got))
		for _, a := range got {
			ids = append(ids, a.ID)
		}
		t.Fatalf("candidates = %v, want only the usable account", ids)
	}
	if got[0].ID != "on" {
		t.Errorf("candidates[0] = %q, want \"on\"", got[0].ID)
	}
}

func TestStatusOnAnExpiredSessionMarksTheAccountAndSaysHowToFixIt(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, time.Now().Add(-time.Minute).UnixMilli())

	st := c.Status(context.Background())
	if st.Name != "loomy" {
		t.Errorf("Name = %q, want \"loomy\"", st.Name)
	}
	// An expiry stamp is a local guess, so it does not by itself make the module
	// unavailable: the session is still handed to the vendor, which answers
	// 100002 if it really is dead.  What must be honest is the account row.
	if !st.Ready {
		t.Errorf("Ready = false for a session that is still worth trying: %s", st.Detail)
	}
	if len(st.Accounts) != 1 {
		t.Fatalf("Status reported %d accounts, want 1", len(st.Accounts))
	}
	acc := st.Accounts[0]
	if acc.State != "invalid" {
		t.Errorf("account state = %q, want \"invalid\"", acc.State)
	}
	if !strings.Contains(acc.Note, "14 days") {
		t.Errorf("account note = %q, want it to explain the 14-day session", acc.Note)
	}
	if !strings.Contains(acc.Note, "import the new token") {
		t.Errorf("account note = %q, want it to tell the operator to re-import", acc.Note)
	}
	if acc.Identity != "loomy-1" {
		t.Errorf("Identity = %q, want the account id", acc.Identity)
	}
	if !strings.Contains(st.Detail, "1 of 1 accounts usable") {
		t.Errorf("Detail = %q", st.Detail)
	}
}

func TestStatusOnARejectedSessionTellsTheOperatorToLogInAgain(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now(), time.Minute)

	st := c.Status(context.Background())
	if st.Ready {
		t.Error("Ready = true with only a rejected session")
	}
	if !strings.Contains(st.Detail, "no refresh endpoint") {
		t.Errorf("Detail = %q, want it to say the session cannot be renewed", st.Detail)
	}
	if !strings.Contains(st.Detail, "import the new token") {
		t.Errorf("Detail = %q, want it to tell the operator what to do", st.Detail)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "invalid" {
		t.Errorf("accounts = %#v, want one invalid row", st.Accounts)
	}
}

func TestStatusWithNoCredentialSaysSoWithoutCallingUpstream(t *testing.T) {
	rt := alwaysJSON(200, okEnvelope("null"))
	c := newTestClient(t, "{}", rt)

	st := c.Status(context.Background())
	if st.Ready {
		t.Error("Ready = true with no credential at all")
	}
	if !strings.Contains(st.Detail, "no credential") {
		t.Errorf("Detail = %q", st.Detail)
	}
	if rt.count() != 0 {
		t.Errorf("Status made %d upstream calls; the panel calls it every ten seconds", rt.count())
	}
}

// The token is the one secret this module holds, and an account record is
// rendered into the panel and into logs, so it must never carry it.
func TestAnAccountRecordNeverEchoesTheSessionToken(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)

	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Accounts returned %d records, want 1", len(records))
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), testToken) {
		t.Errorf("the account record leaked the session token: %s", raw)
	}
	if records[0].Identity != "loomy-1" {
		t.Errorf("Identity = %q, want the account id rather than the token", records[0].Identity)
	}
}

func TestAccountFieldsDeclareTheTokenAsASecret(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	fields := c.AccountFields(context.Background())

	var token *core.FieldSpec
	for i := range fields {
		if fields[i].Key == "access_token" {
			token = &fields[i]
		}
	}
	if token == nil {
		t.Fatal("the add form does not ask for access_token")
	}
	if token.Type != "password" {
		t.Errorf("access_token type = %q, want \"password\" so the panel masks it", token.Type)
	}
	if !token.Required {
		t.Error("access_token is not marked required")
	}
}

// A credential that came from the configuration file cannot be deleted through
// the panel: deleting it would only make it reappear on the next reload.
func TestAConfigOwnedAccountCannotBeRemovedFromThePanel(t *testing.T) {
	cfg := `{"access_token":"` + testToken + `","userid":"` + testUserID + `"}`
	c := newTestClient(t, cfg, nil)
	id := "loomy-" + testUserID

	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 1 || records[0].ID != id {
		t.Fatalf("accounts = %#v, want the configured account %q", records, id)
	}

	err = c.RemoveAccount(context.Background(), id)
	if err == nil {
		t.Fatal("a configuration-owned account was removed through the panel")
	}
	if !strings.Contains(err.Error(), "configuration") {
		t.Errorf("error = %q, want it to explain that the account comes from the configuration", err)
	}
	if c.store.count() != 1 {
		t.Error("the account was removed anyway")
	}
}

func TestAStoredAccountCanBeRemoved(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if err := c.RemoveAccount(context.Background(), "loomy-1"); err != nil {
		t.Fatalf("RemoveAccount: %v", err)
	}
	if c.store.count() != 0 {
		t.Error("the account is still in the store")
	}
	if _, ok := c.store.lookup("loomy-1"); ok {
		t.Error("the account is still reachable by id")
	}
}

func TestSetAccountEnabledParksTheAccountWithoutDeletingIt(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if err := c.SetAccountEnabled(context.Background(), "loomy-1", false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	acc, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("parking deleted the account")
	}
	if acc.selectable(time.Now()) {
		t.Error("a parked account is still selectable")
	}
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("Status reported %d accounts, want the parked one", len(st.Accounts))
	}
	if st.Accounts[0].State != "cooling" {
		t.Errorf("state = %q, want \"cooling\"", st.Accounts[0].State)
	}
	if !strings.Contains(st.Accounts[0].Note, "operator") {
		t.Errorf("note = %q, want it to say the operator parked it", st.Accounts[0].Note)
	}
}

func TestReviveAccountClearsEveryPenaltyAndPersistsIt(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	dir := filepath.Dir(c.store.path)
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now(), time.Minute)
	if err := c.store.setEnabled("loomy-1", false); err != nil {
		t.Fatalf("setEnabled: %v", err)
	}

	if err := c.ReviveAccount(context.Background(), "loomy-1"); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	acc, _ := c.store.lookup("loomy-1")
	if acc.dead {
		t.Error("revive left the account parked as dead")
	}
	if !acc.Enabled {
		t.Error("revive did not re-enable the account")
	}
	if !acc.selectable(time.Now()) {
		t.Error("the revived account is still not selectable")
	}

	reloaded := newTestClientInDir(t, dir, "{}", nil)
	again, _ := reloaded.store.lookup("loomy-1")
	if again.dead || !again.Enabled {
		t.Error("the revive was undone by the next load from disk")
	}
}

func TestAddAccountRejectsAnUnusableTokenBeforeItIsStored(t *testing.T) {
	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"empty", "", "required"},
		{"only whitespace", "   ", "required"},
		{"an embedded space", "0123456789abcdef 0123456789abcdef", "whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))
			_, err := c.AddAccount(context.Background(), core.AccountSpec{
				Fields: map[string]string{"access_token": tc.token},
			})
			if err == nil {
				t.Fatal("AddAccount accepted an unusable token")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if c.store.count() != 0 {
				t.Error("the rejected credential was stored anyway")
			}
		})
	}
}

func TestAddAccountRefusesASessionTheVendorRejects(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, failureEnvelope("100002", "缺少 token")))

	_, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": testToken, "userid": testUserID},
	})
	if err == nil {
		t.Fatal("AddAccount stored a session the vendor had already rejected")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error = %q, want it to say the vendor rejected the session", err)
	}
	if c.store.count() != 0 {
		t.Error("the rejected credential was stored anyway")
	}
}

// A probe that fails for any other reason is not evidence about the credential,
// so the operator's paste is kept rather than thrown away.
func TestAddAccountKeepsTheCredentialWhenTheProbeMerelyFails(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(500, "upstream is having a bad day"))

	record, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"access_token": testToken, "userid": testUserID},
	})
	if err != nil {
		t.Fatalf("AddAccount refused a credential over an unrelated upstream failure: %v", err)
	}
	if c.store.count() != 1 {
		t.Fatal("the credential was not stored")
	}
	if record.ID != "loomy-"+testUserID {
		t.Errorf("record id = %q, want %q", record.ID, "loomy-"+testUserID)
	}
	acc, _ := c.store.lookup(record.ID)
	if acc.dead {
		t.Error("an unrelated 500 parked the credential as dead")
	}
	if acc.lastError == "" {
		t.Error("the failed probe was not recorded for the operator to see")
	}
}

func TestAddAccountStoresAndReportsTheIdentity(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))

	record, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{
			"access_token": testToken,
			"userid":       testUserID,
			"phone":        "13800000000",
			"nickname":     "tester",
		},
	})
	if err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if record.ID != "loomy-"+testUserID {
		t.Errorf("id = %q, want %q", record.ID, "loomy-"+testUserID)
	}
	if record.Identity != testUserID {
		t.Errorf("Identity = %q, want the userid", record.Identity)
	}
	if !record.Enabled {
		t.Error("a freshly added credential came back disabled")
	}
	if record.State != "ready" {
		t.Errorf("state = %q, want \"ready\"", record.State)
	}
	if record.Fields["origin"] != originStored {
		t.Errorf("origin = %#v, want %q", record.Fields["origin"], originStored)
	}
	if record.Fields["removable"] != true {
		t.Errorf("removable = %#v, want true for a credential the operator pasted", record.Fields["removable"])
	}
}

func TestTestAccountExplainsARejectedSession(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, failureEnvelope("100002", "缺少 token")))
	seedAccount(t, c, "loomy-1", testToken, 0)

	result, err := c.TestAccount(context.Background(), "loomy-1")
	if err != nil {
		t.Fatalf("TestAccount returned a hard error for an upstream refusal: %v", err)
	}
	if result.OK {
		t.Error("OK = true for a session the vendor rejected")
	}
	if !strings.Contains(result.Error, "expired or rejected") {
		t.Errorf("error = %q, want the operator-facing explanation", result.Error)
	}
	if result.ElapsedMS < 0 {
		t.Errorf("ElapsedMS = %d", result.ElapsedMS)
	}
}

func TestTestAccountRejectsAnUnknownID(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))
	if _, err := c.TestAccount(context.Background(), "nope"); err == nil {
		t.Fatal("TestAccount accepted an unknown account id")
	}
}

// The panel's "refresh" button is wired to something Loomy genuinely cannot do:
// there is no refresh endpoint.  The honest answer is to verify the session and
// say plainly that re-importing is the only remedy.
func TestRefreshAccountVerifiesRatherThanRenews(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, failureEnvelope("100002", "缺少 token")))
	seedAccount(t, c, "loomy-1", testToken, 0)

	results, err := c.RefreshAccount(context.Background(), "loomy-1")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("RefreshAccount returned %d results, want 1", len(results))
	}
	if results[0].OK {
		t.Error("OK = true for a session the vendor rejected")
	}
	if !strings.Contains(results[0].Error, "no refresh endpoint") {
		t.Errorf("error = %q, want it to say the session cannot be renewed", results[0].Error)
	}
	acc, _ := c.store.lookup("loomy-1")
	if !acc.dead {
		t.Error("the rejected session was not parked")
	}
}

func TestRefreshAccountAcceptsAnEmptyIDAsEveryAccount(t *testing.T) {
	c := newTestClient(t, "{}", alwaysJSON(200, okEnvelope("null")))
	seedAccount(t, c, "loomy-1", testToken, 0)
	seedAccount(t, c, "loomy-2", testTokenTwo, 0)

	results, err := c.RefreshAccount(context.Background(), "")
	if err != nil {
		t.Fatalf("RefreshAccount: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("RefreshAccount returned %d results, want one per account", len(results))
	}
	for _, r := range results {
		if !r.OK {
			t.Errorf("account %s was not verified: %s", r.AccountID, r.Error)
		}
	}
}

func TestDefaultAccountIDPrefersTheUserIDAndNeverUsesTheToken(t *testing.T) {
	withUserID := defaultAccountID(account{storedAccount: storedAccount{UserID: testUserID, AccessToken: testToken}})
	if withUserID != "loomy-"+testUserID {
		t.Errorf("id = %q, want %q", withUserID, "loomy-"+testUserID)
	}

	without := defaultAccountID(account{storedAccount: storedAccount{AccessToken: testToken}})
	if !strings.HasPrefix(without, "loomy-") {
		t.Fatalf("id = %q, want a \"loomy-\" prefix", without)
	}
	if without == testToken || strings.Contains(without, testToken) {
		t.Errorf("id = %q, want a hash rather than the session token", without)
	}
	if suffix := strings.TrimPrefix(without, "loomy-"); len(suffix) != 12 {
		t.Errorf("id suffix = %q, want 12 hex characters", suffix)
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{-time.Minute, "0s"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		if got := humanAge(tc.age); got != tc.want {
			t.Errorf("humanAge(%s) = %q, want %q", tc.age, got, tc.want)
		}
	}
}

func TestAccountsOnAnEmptyStoreIsAnEmptyListNotAnError(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	records, err := c.Accounts(context.Background())
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("Accounts returned %#v, want an empty slice", records)
	}
}
