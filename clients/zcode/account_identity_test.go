// Tests for the account identity this module publishes to the panel.
//
// The panel has always listed credentials, and one vendor account can hold
// several of them: the ZCode desktop client keeps an API key in its provider
// config and the plan JWT in credentials.json for the same Zhipu user, and a
// panel sign-in can add a third.  Without an identity each credential rendered
// as its own account, so the operator saw "three accounts" where there is one,
// and an unattended sweep spent a request on every credential.
//
// These tests pin the two facts the grouping rests on: an API-key credential
// learns whose it is from the credential store's key name (a key itself carries
// no such name), and the record the panel reads publishes what the account
// knows.

package zcode

import (
	"os"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestAnAPIKeyCredentialNamesTheAccountItBelongsTo covers the live pairing.  The
// desktop client names its connection entries after the account they were
// issued to, so that name is the only place an API key's account id can come
// from.
func TestAnAPIKeyCredentialNamesTheAccountItBelongsTo(t *testing.T) {
	home := isolateHome(t)
	pass := ambientPassphrase(t)
	const full = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"
	const userID = "61161790588087632"

	writeZcodeV2(t, home,
		`{"provider":{"builtin:bigmodel":{`+
			`"enabled":true,"kind":"anthropic","name":"Bigmodel - API Key",`+
			`"options":{"apiKey":"b20b5a93536f417f928524a399a830b6","baseURL":"`+baseBigmodelKey+`"}}}}`,
		`{"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:`+userID+`:api-key":`+
			jsonString(sealForTest(t, full, pass))+`}`)

	acct, ok := accountByID(t, discover(nil), "zcode-config:builtin:bigmodel")
	if !ok {
		t.Fatal("the provider config entry was not discovered")
	}
	if acct.Mode != modeAPIKey {
		t.Fatalf("mode = %q, want an API key", acct.Mode)
	}
	if acct.UserID != userID {
		t.Errorf("an API-key credential must name its account, got %q", acct.UserID)
	}
}

// TestTwoCredentialsForOneAccountShareAnIdentity is the regression guard for the
// report that started this work: the panel showed the coding-plan API key and
// the plan JWT as two separate accounts.  They are two channels of one account,
// and an identity they agree on is the only thing that can say so.
func TestTwoCredentialsForOneAccountShareAnIdentity(t *testing.T) {
	home := isolateHome(t)
	pass := ambientPassphrase(t)
	const full = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"
	const userID = "61161790588087632"

	writeZcodeV2(t, home,
		`{"provider":{"builtin:bigmodel":{`+
			`"enabled":true,"kind":"anthropic","name":"Bigmodel - API Key",`+
			`"options":{"apiKey":"b20b5a93536f417f928524a399a830b6","baseURL":"`+baseBigmodelKey+`"}}}}`,
		`{"zcodejwttoken":`+jsonString(makeJWT(`{"user_id":"`+userID+`"}`))+`,`+
			`"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:`+userID+`:api-key":`+
			jsonString(sealForTest(t, full, pass))+`}`)

	byIdentity := map[string][]string{}
	for _, s := range discover(nil) {
		if s.UserID == "" {
			continue
		}
		byIdentity[s.UserID] = append(byIdentity[s.UserID], s.ID)
	}
	if len(byIdentity) != 1 {
		t.Fatalf("the discovered credentials name %d accounts, want 1: %v", len(byIdentity), byIdentity)
	}
	ids, ok := byIdentity[userID]
	if !ok {
		t.Fatalf("identity = %v, want %q", byIdentity, userID)
	}
	// The desktop client's provider config is the row the operator complained
	// about, so it must be in the group — not merely agreeing with some other
	// pair of credentials.
	if !slices.Contains(ids, "zcode-config:builtin:bigmodel") {
		t.Errorf("the provider config credential is not in the group: %v", ids)
	}
	if len(ids) < 2 {
		t.Errorf("only %v named the account, so the grouping is untested", ids)
	}
}

// TestTheJWTAccountLabelNamesTheLogin pins the readable handle the panel shows
// in the recent-calls account column.  The credential store keys the token by
// channel ("zcodejwttoken"), so a bare "ZCode plan JWT" cannot tell two Zhipu
// logins apart; the vendor user id suffix can.
func TestTheJWTAccountLabelNamesTheLogin(t *testing.T) {
	const userID = "61161790588087632"
	if got := jwtAccountLabel(userID); got != "ZCode plan JWT ...88087632" {
		t.Errorf("jwtAccountLabel(%q) = %q, want the user id suffix", userID, got)
	}
	if got := jwtAccountLabel(""); got != "ZCode plan JWT" {
		t.Errorf("jwtAccountLabel(\"\") = %q, want the plain channel label", got)
	}
	if got := jwtAccountLabel("12345678"); got != "ZCode plan JWT 12345678" {
		t.Errorf("an eight-character id must not be elided: %q", got)
	}
}

// TestAStalePersistedLabelDoesNotHideTheAccountName is the regression for the
// "still says JWT" report: a state file written by an older build can carry a
// label the current discovery would name better ("ZCode plan JWT" where the
// credential itself now tells us "ZCode plan JWT ...88087632").  Runtime state
// that the pool persists (enabled / cooldown / note) must survive the restart,
// but a *derived* label must not be able to overwrite the freshly discovered
// one, or the fix in jwtAccountLabel can never reach an operator who has
// already run the old build once.
func TestAStalePersistedLabelDoesNotHideTheAccountName(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":true}`)
	const id = "zcode-credentials:zcodejwttoken"
	jwt := makeJWT(`{"user_id":"u-1","sub":"u-1"}`)

	writeZcodeV2(t, env.home, "", `{"zcodejwttoken":"`+jwt+`"}`)
	// The state file as an older build left it: the label it knew, plus
	// runtime state that must survive this same restart.
	stale := `{"version":1,"accounts":[{"id":"` + id + `","label":"ZCode plan JWT",` +
		`"enabled":false,"state":"ready"}]}`
	if err := os.WriteFile(env.statePath(), []byte(stale), 0o600); err != nil {
		t.Fatalf("write the stale state: %v", err)
	}

	c := env.client(t, nil)
	rec, ok := recordsByID(t, c)[id]
	if !ok {
		t.Fatal("the discovered credential vanished")
	}
	if rec.Label != "ZCode plan JWT u-1" {
		t.Errorf("label = %q, want the freshly discovered account name", rec.Label)
	}
	if rec.Enabled {
		t.Errorf("the persisted enabled=false must survive the restart")
	}
}

// TestRecordForPublishesTheIdentity pins the projection: the panel can only
// group on what the record carries, so the account's user id has to survive the
// trip.  A credential that knows no account must report none rather than an
// empty-but-present value, because the panel treats "" as "keep this one on its
// own".
func TestRecordForPublishesTheIdentity(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acct Account
		want string
	}{
		{
			name: "an api key that knows its account",
			acct: Account{ID: "a", Mode: modeAPIKey, UserID: "u-1"},
			want: "u-1",
		},
		{
			name: "a jwt that knows its account",
			acct: Account{ID: "b", Mode: modeJWT, UserID: "u-2"},
			want: "u-2",
		},
		{
			name: "a credential the vendor never named",
			acct: Account{ID: "c", Mode: modeAPIKey},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acct := tc.acct
			if got := recordFor(&acct, now, true).Identity; got != tc.want {
				t.Errorf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAManagedAccountKeepsTheIdentityItWasSignedInWith covers the store.  An API
// key cannot be asked who it belongs to once the sign-in response is gone, so
// the id has to be written down at that moment and read back on every later
// start; a JWT still carries its own, so the stored copy is only a fallback.
func TestAManagedAccountKeepsTheIdentityItWasSignedInWith(t *testing.T) {
	cases := []struct {
		name string
		m    managedAccount
		want string
	}{
		{
			name: "an api key keeps the id captured at sign-in",
			m:    managedAccount{ID: "a", Kind: kindAPIKey, APIKey: "k", UserID: "u-1"},
			want: "u-1",
		},
		{
			name: "an api key stored before the field existed has none",
			m:    managedAccount{ID: "b", Kind: kindAPIKey, APIKey: "k"},
			want: "",
		},
		{
			name: "a jwt is read from the token itself",
			m:    managedAccount{ID: "c", Kind: kindJWT, JWT: makeJWT(`{"user_id":"u-3"}`)},
			want: "u-3",
		},
		{
			name: "a jwt falls back to the stored id when the token is opaque",
			m:    managedAccount{ID: "d", Kind: kindJWT, JWT: "not-a-jwt", UserID: "u-4"},
			want: "u-4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.account().UserID; got != tc.want {
				t.Errorf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAStoredAccountKeepsItsIdentityAcrossARestart is the end-to-end version of
// the case above: the id has to reach disk and come back, because the pool is
// rebuilt from the store on every start and the grouping is decided from the
// pool.  The store is written the way the sign-in flow writes it.
func TestAStoredAccountKeepsItsIdentityAcrossARestart(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	const id = "zcode-managed:8235292677a4"

	body := `{"version":` + strconv.Itoa(managedVersion) + `,"accounts":[{"id":"` + id + `",` +
		`"label":"z.ai panel login","kind":"` + kindAPIKey + `","api_key":"panel-issued-key",` +
		`"user_id":"61161790588087632","region":"zai","origin":"` + originPanelLogin + `","enabled":true}]}`
	if err := os.WriteFile(env.storePath(), []byte(body), 0o600); err != nil {
		t.Fatalf("write the store: %v", err)
	}

	rec, ok := recordsByID(t, env.client(t, nil))[id]
	if !ok {
		t.Fatal("the stored account is not in the panel view")
	}
	if rec.Identity != "61161790588087632" {
		t.Errorf("identity = %q, want the account it was signed in with", rec.Identity)
	}
}
