package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// accounts_test.go covers the credential shape and the store that persists it.
//
// The round-trip test is the important one: a credential that does not survive
// a write and a read is a credential the operator has to paste again every
// time the process restarts.

// testAccount is a complete, usable credential.
func testAccount() account {
	return account{
		AccessKeyID:     "AKIDTESTACCOUNT",
		SecretAccessKey: "SKTESTACCOUNTSECRET",
		SecurityToken:   "TOKTESTACCOUNT",
		RefreshToken:    "RTTESTACCOUNT",
		ExpiresAt:       time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC).Unix(),
		DomainID:        "DOMTEST",
		UserID:          "USERTEST",
		UserName:        "test user",
		Note:            "the test account",
	}
}

// TestAccountIdentityIsTheAccessKeyID is the module's most important
// departure from its siblings: every other module keys an account by its
// access token, and CodeArts keys it by the access key id.
func TestAccountIdentityIsTheAccessKeyID(t *testing.T) {
	a := testAccount()
	if got := a.identity(); got != "AKIDTESTACCOUNT" {
		t.Errorf("identity() = %q, want the access key id", got)
	}
	if got := a.accountID(); got != "AKIDTESTACCOUNT" {
		t.Errorf("accountID() = %q", got)
	}

	// A credential with an explicit id uses it, and identity still reports the
	// access key so the panel can group it with a config-supplied twin.
	a.ID = "explicit-id"
	if got := a.accountID(); got != "explicit-id" {
		t.Errorf("accountID() = %q, want the explicit id", got)
	}
	if got := a.identity(); got != "AKIDTESTACCOUNT" {
		t.Errorf("identity() = %q; an explicit id must not change the vendor identity", got)
	}

	// A credential with no access key falls back to the user id.
	b := account{UserID: "U123", SecretAccessKey: "s"}
	if got := b.accountID(); got != "user:U123" {
		t.Errorf("accountID() = %q, want user:U123", got)
	}
	if got := b.identity(); got != "" {
		t.Errorf("identity() = %q, want empty when there is no access key", got)
	}

	// Nothing at all is the empty id, which the pool refuses to store.
	if got := (account{}).accountID(); got != "" {
		t.Errorf("the zero account has id %q", got)
	}
}

// TestAccountLabel checks the panel label prefers a human name and never shows
// a full access key.
func TestAccountLabel(t *testing.T) {
	a := testAccount()
	a.Note = ""
	a.UserName = "someone"
	if got := a.label(); got != "someone" {
		t.Errorf("label() = %q, want the user name", got)
	}
	a.UserName = ""
	got := a.label()
	if !strings.HasPrefix(got, "AK ") {
		t.Errorf("label() = %q, want an AK-prefixed mask", got)
	}
	if strings.Contains(got, a.AccessKeyID) {
		t.Errorf("label() leaked the whole access key: %q", got)
	}
	if got := (account{}).label(); got != "codearts" {
		t.Errorf("the zero account's label = %q", got)
	}
}

// TestMaskKey checks the truncation, including the short-key case where the
// first and last four characters would overlap.
func TestMaskKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "***"},
		{"short", "***"},
		{"12345678", "***"},
		{"123456789", "1234…6789"},
		{"AKIDTESTACCOUNT", "AKID…OUNT"},
	} {
		if got := maskKey(tc.in); got != tc.want {
			t.Errorf("maskKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAccountFingerprint checks the tag is stable, differs between credentials
// and does not contain either secret.
func TestAccountFingerprint(t *testing.T) {
	a := testAccount()
	fp := a.fingerprint()
	if fp == "" {
		t.Fatal("fingerprint() is empty")
	}
	if a.fingerprint() != fp {
		t.Error("fingerprint() is not stable")
	}
	if strings.Contains(fp, a.SecretAccessKey) || strings.Contains(fp, a.AccessKeyID) {
		t.Error("fingerprint() contains a credential")
	}
	b := testAccount()
	b.SecretAccessKey = "a different secret"
	if b.fingerprint() == fp {
		t.Error("two different credentials share a fingerprint")
	}
	// Swapping the two halves must not produce the same tag, which is what the
	// \x00 separator is for.
	c := testAccount()
	c.AccessKeyID, c.SecretAccessKey = c.SecretAccessKey, c.AccessKeyID
	if c.fingerprint() == fp {
		t.Error("the fingerprint is order-insensitive")
	}
}

// TestAccountUsableAndRefreshable checks the two capability predicates.  A
// bare AK/SK pair can list models but cannot chat, which is the distinction
// `usable` draws.
func TestAccountUsableAndRefreshable(t *testing.T) {
	full := testAccount()
	// A refresh needs the key the credential was minted with, so the fixture
	// has to carry one for this predicate to be true.
	full.DpopPrivateJwk = `{"kty":"EC","crv":"P-256","x":"a","y":"b","d":"c"}`
	if !full.usable() {
		t.Error("a full credential is not usable")
	}
	if !full.refreshable() {
		t.Error("a full credential is not refreshable")
	}
	// Without the DPoP key the credential can chat but cannot renew itself.
	noDpop := testAccount()
	if !noDpop.usable() {
		t.Error("a credential without a DPoP key is not usable")
	}
	if noDpop.refreshable() {
		t.Error("a credential without a DPoP key must not be refreshable")
	}

	bare := account{AccessKeyID: "AK", SecretAccessKey: "SK"}
	if !bare.usable() {
		t.Error("a bare AK/SK pair must be usable for the catalogue")
	}
	if bare.refreshable() {
		t.Error("a bare AK/SK pair must not be refreshable")
	}

	noSecret := account{AccessKeyID: "AK"}
	if noSecret.usable() {
		t.Error("a credential with no secret key must not be usable")
	}
	noToken := full
	noToken.RefreshToken = ""
	if noToken.refreshable() {
		t.Error("a credential with no refresh token must not be refreshable")
	}
	noKey := full
	noKey.DpopPrivateJwk = ""
	if noKey.refreshable() {
		t.Error("a credential with no DPoP key must not be refreshable: the refresh would be signed with the wrong key")
	}
	// Whitespace-only values are not values.
	spaces := account{AccessKeyID: "   ", SecretAccessKey: "\t"}
	if spaces.usable() {
		t.Error("a whitespace-only credential was usable")
	}
}

// TestAccountExpiry checks that a zero expiry means "unknown" and never
// "expired", which is the difference between one wasted request and a
// credential that is refused forever.
func TestAccountExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a := testAccount()
	a.ExpiresAt = now.Add(-time.Second).Unix()
	if !a.expired(now) {
		t.Error("a past expiry was not expired")
	}
	a.ExpiresAt = now.Add(time.Second).Unix()
	if a.expired(now) {
		t.Error("a future expiry was expired")
	}
	if !a.expiringWithin(now, time.Minute) {
		t.Error("a credential expiring in a second is not expiring within a minute")
	}
	if a.expiringWithin(now, time.Millisecond) {
		t.Error("a credential expiring in a second is expiring within a millisecond")
	}
	a.ExpiresAt = 0
	if a.expired(now) || a.expiringWithin(now, time.Hour) {
		t.Error("an unknown expiry was treated as expiring")
	}
	if got := a.expiresAtString(); got != "" {
		t.Errorf("expiresAtString() = %q, want empty for an unknown expiry", got)
	}
	a.ExpiresAt = now.Unix()
	if got := a.expiresAtString(); got == "" {
		t.Error("expiresAtString() is empty for a known expiry")
	}
}

// TestMergeAccount checks the overlay rules, in particular that the park switch
// is ORed so a re-add can never quietly re-enable a parked credential.
func TestMergeAccount(t *testing.T) {
	base := account{AccessKeyID: "AK", SecretAccessKey: "SK", UserName: "kept", ExpiresAt: 111, Disabled: true}
	over := account{AccessKeyID: "AK2", SecurityToken: "TOK", UserName: "   ", ExpiresAt: 222}
	got := mergeAccount(base, over)
	if got.AccessKeyID != "AK2" {
		t.Errorf("a non-empty field did not override: %q", got.AccessKeyID)
	}
	if got.SecretAccessKey != "SK" {
		t.Errorf("an empty field erased a value: %q", got.SecretAccessKey)
	}
	if got.UserName != "kept" {
		t.Errorf("a whitespace-only field erased a value: %q", got.UserName)
	}
	if got.SecurityToken != "TOK" {
		t.Errorf("a new field was not added: %q", got.SecurityToken)
	}
	if got.ExpiresAt != 222 {
		t.Errorf("a non-zero expiry did not override: %d", got.ExpiresAt)
	}
	if !got.Disabled {
		t.Error("the park switch was cleared by a merge")
	}
	// The other direction: an empty incoming credential cannot clear anything.
	empty := mergeAccount(base, account{})
	if empty.AccessKeyID != "AK" || empty.SecretAccessKey != "SK" || empty.ExpiresAt != 111 || !empty.Disabled {
		t.Errorf("an empty overlay changed the account: %+v", empty)
	}
	// A zero incoming expiry does not clear a known one.
	if got := mergeAccount(base, account{ExpiresAt: 0}).ExpiresAt; got != 111 {
		t.Errorf("a zero expiry cleared a known one: %d", got)
	}
}

// TestValidateSecret checks the copy-paste accidents the form must reject.
func TestValidateSecret(t *testing.T) {
	if err := validateSecret("access_key_id", "AKID", maxAccessKeyBytes); err != nil {
		t.Errorf("a valid value was rejected: %v", err)
	}
	for name, in := range map[string]string{
		"empty":            "",
		"newline":          "AK\nID",
		"carriage return":  "AK\rID",
		"tab":              "AK\tID",
		"nul":              "AK\x00ID",
		"delete":           "AK\x7fID",
		"non-ascii":        "AKID\u4e2d\u6587",
		"replacement char": "AK\uFFFDID",
		"too long":         strings.Repeat("A", maxAccessKeyBytes+1),
	} {
		if err := validateSecret("access_key_id", in, maxAccessKeyBytes); err == nil {
			t.Errorf("validateSecret accepted a %s value", name)
		}
	}
	// The boundary itself is allowed.
	if err := validateSecret("access_key_id", strings.Repeat("A", maxAccessKeyBytes), maxAccessKeyBytes); err != nil {
		t.Errorf("a value of exactly the maximum length was rejected: %v", err)
	}
}

// TestValidAccountID checks what the panel may address.  The id becomes a map
// key and appears in a URL path, so anything surprising is refused.
func TestValidAccountID(t *testing.T) {
	for _, ok := range []string{"AKIDTESTACCOUNT", "a", "A-b_c.d:e@f/g", strings.Repeat("x", 160)} {
		if !validAccountID(ok) {
			t.Errorf("validAccountID(%q) = false", ok)
		}
	}
	for name, bad := range map[string]string{
		"empty":       "",
		"whitespace":  "   ",
		"space":       "a b",
		"newline":     "a\nb",
		"path escape": "a/../../b",
		"non-ascii":   "\u4e2d\u6587",
		"too long":    strings.Repeat("x", 161),
		"percent":     "a%b",
		"backslash":   `a\b`,
		"quotes":      `a"b`,
		"semicolon":   "a;b",
	} {
		if validAccountID(bad) {
			t.Errorf("validAccountID accepted a %s id", name)
		}
	}
}

// TestSanitizeLabel checks that a pasted multi-line blob cannot become a label.
func TestSanitizeLabel(t *testing.T) {
	if got := sanitizeLabel("  hello  "); got != "hello" {
		t.Errorf("sanitizeLabel = %q", got)
	}
	if got := sanitizeLabel("a\nb\tc"); got != "abc" {
		t.Errorf("control characters survived: %q", got)
	}
	if got := sanitizeLabel(""); got != "" {
		t.Errorf("sanitizeLabel(\"\") = %q", got)
	}
	long := strings.Repeat("x", maxLabelRunes+50)
	got := sanitizeLabel(long)
	if n := len([]rune(got)); n != maxLabelRunes {
		t.Errorf("sanitizeLabel capped at %d runes, want %d", n, maxLabelRunes)
	}
}

// TestAccountFromSpec checks the panel form path, including that the id is
// derived when the form does not supply one.
func TestAccountFromSpec(t *testing.T) {
	spec := core.AccountSpec{
		Label: "my account",
		Fields: map[string]string{
			"access_key_id":     " AKIDFORM ",
			"secret_access_key": " SKFORM ",
			"security_token":    "TOKFORM",
			"refresh_token":     "RTFORM",
			"expires_at":        "2030-01-02T03:04:05Z",
			"user_name":         "form user",
		},
	}
	a, err := accountFromSpec(spec)
	if err != nil {
		t.Fatalf("accountFromSpec: %v", err)
	}
	if a.AccessKeyID != "AKIDFORM" || a.SecretAccessKey != "SKFORM" {
		t.Errorf("values were not trimmed: %+v", a)
	}
	if a.ID != "AKIDFORM" {
		t.Errorf("id = %q, want the access key id", a.ID)
	}
	if a.Note != "my account" {
		t.Errorf("note = %q", a.Note)
	}
	if a.ExpiresAt != time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).Unix() {
		t.Errorf("expiry = %d", a.ExpiresAt)
	}
	if a.Disabled {
		t.Error("a form submission is enabled unless it says otherwise")
	}

	// An explicit disabled flag is honoured.
	no := false
	spec.Enabled = &no
	a, err = accountFromSpec(spec)
	if err != nil {
		t.Fatalf("accountFromSpec: %v", err)
	}
	if !a.Disabled {
		t.Error("Enabled=false did not park the credential")
	}

	// Missing halves are refused.
	for name, fields := range map[string]map[string]string{
		"no access key":  {"secret_access_key": "SK"},
		"no secret key":  {"access_key_id": "AK"},
		"bad access key": {"access_key_id": "AK\nID", "secret_access_key": "SK"},
		"bad secret key": {"access_key_id": "AK", "secret_access_key": "SK\nID"},
	} {
		if _, err := accountFromSpec(core.AccountSpec{Fields: fields}); err == nil {
			t.Errorf("accountFromSpec accepted a form with %s", name)
		}
	}

	// An id that cannot be addressed is refused.
	if _, err := accountFromSpec(core.AccountSpec{
		Fields: map[string]string{"id": "bad id", "access_key_id": "AK", "secret_access_key": "SK"},
	}); err == nil {
		t.Error("accountFromSpec accepted an unusable id")
	}
}

// TestCredentialPersistenceRoundTrip is the store's core guarantee: what the
// pool writes is what the pool reads back, secrets and all.
func TestCredentialPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := testAccount()

	first := newPool()
	first.storePath = filepath.Join(dir, accountsFile)
	first.statePath = filepath.Join(dir, stateFile)
	first.load(nil)
	first.put(want, true)
	if got := first.len(); got != 1 {
		t.Fatalf("after put the pool has %d entries, want 1", got)
	}

	// The file on disk must be the documented document.
	raw, err := os.ReadFile(first.storePath)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}
	var store accountsStore
	if err := json.Unmarshal(raw, &store); err != nil {
		t.Fatalf("the store is not the documented JSON: %v\n%s", err, raw)
	}
	if store.Version != accountsVersion {
		t.Errorf("store version = %d, want %d", store.Version, accountsVersion)
	}
	if len(store.Accounts) != 1 {
		t.Fatalf("the store holds %d accounts", len(store.Accounts))
	}

	// A second pool over the same directory must rebuild the same credential.
	second := newPool()
	second.storePath = first.storePath
	second.statePath = first.statePath
	second.load(nil)
	if got := second.len(); got != 1 {
		t.Fatalf("the reloaded pool has %d entries, want 1", got)
	}
	e := second.pick(nil)
	if e == nil {
		t.Fatal("the reloaded pool has no usable entry")
	}
	got := e.account()
	if got.AccessKeyID != want.AccessKeyID {
		t.Errorf("access key id = %q, want %q", got.AccessKeyID, want.AccessKeyID)
	}
	if got.SecretAccessKey != want.SecretAccessKey {
		t.Errorf("secret access key did not survive the round-trip")
	}
	if got.SecurityToken != want.SecurityToken {
		t.Errorf("security token = %q, want %q", got.SecurityToken, want.SecurityToken)
	}
	if got.RefreshToken != want.RefreshToken {
		t.Errorf("refresh token did not survive the round-trip")
	}
	if got.ExpiresAt != want.ExpiresAt {
		t.Errorf("expiry = %d, want %d", got.ExpiresAt, want.ExpiresAt)
	}
	if got.DomainID != want.DomainID || got.UserID != want.UserID || got.UserName != want.UserName {
		t.Errorf("the identity fields did not survive: %+v", got)
	}
	if got.Note != want.Note {
		t.Errorf("note = %q, want %q", got.Note, want.Note)
	}
	if got.identity() != want.identity() {
		t.Errorf("identity = %q, want %q", got.identity(), want.identity())
	}

	// The stored entry is removable; a config-sourced one is not.
	if !e.removable {
		t.Error("a stored credential is not removable")
	}
	if e.origin != originStored {
		t.Errorf("origin = %q, want %q", e.origin, originStored)
	}
}

// TestPersistenceRoundTripWithDpopKey checks the piece that makes a credential
// refreshable: the private JWK has to survive the store too.
func TestPersistenceRoundTripWithDpopKey(t *testing.T) {
	dir := t.TempDir()
	key, err := generateDpopKeyPair()
	if err != nil {
		t.Fatalf("generateDpopKeyPair: %v", err)
	}
	a := testAccount()
	a.DpopPrivateJwk = key.PrivateJwk
	a.CodeVerifier = "a-pkce-verifier"

	p := newPool()
	p.storePath = filepath.Join(dir, accountsFile)
	p.statePath = filepath.Join(dir, stateFile)
	p.load(nil)
	p.put(a, true)

	reloaded := newPool()
	reloaded.storePath = p.storePath
	reloaded.statePath = p.statePath
	reloaded.load(nil)
	e := reloaded.pick(nil)
	if e == nil {
		t.Fatal("the reloaded pool is empty")
	}
	got := e.account()
	if got.DpopPrivateJwk != key.PrivateJwk {
		t.Fatal("the DPoP private key did not survive the round-trip")
	}
	if got.CodeVerifier != a.CodeVerifier {
		t.Errorf("the PKCE verifier did not survive the round-trip")
	}
	if !got.refreshable() {
		t.Error("the reloaded credential is not refreshable")
	}
	// And the restored key really is the same key.
	restored, err := keyPairFromStoredJwk(got.DpopPrivateJwk)
	if err != nil {
		t.Fatalf("the stored key does not rebuild: %v", err)
	}
	if restored.thumbprint() != key.thumbprint() {
		t.Error("the restored key has a different thumbprint")
	}
}

// TestStoreDoesNotWriteConfigCredentials checks that a credential supplied by
// the config file is never copied into the store, because a second copy is
// free to drift from the first.
func TestStoreDoesNotWriteConfigCredentials(t *testing.T) {
	dir := t.TempDir()
	p := newPool()
	p.storePath = filepath.Join(dir, accountsFile)
	p.statePath = filepath.Join(dir, stateFile)

	fromConfig := testAccount()
	fromConfig.AccessKeyID = "AKFROMCONFIG"
	fromConfig.SecretAccessKey = "SKFROMCONFIG"
	p.load([]account{fromConfig})

	if got := p.len(); got != 1 {
		t.Fatalf("the configured roster produced %d entries, want 1", got)
	}
	e := p.pick(nil)
	if e == nil {
		t.Fatal("the configured credential is not usable")
	}
	if e.origin != originConfig {
		t.Errorf("origin = %q, want %q", e.origin, originConfig)
	}
	if e.removable {
		t.Error("a config credential must not be removable from the panel")
	}
	// Nothing was written, because nothing had to be.
	if _, err := os.Stat(p.storePath); err == nil {
		t.Error("the pool wrote a store file for a config-only roster")
	}
	// Removing it is refused with a message that names the config.
	if err := p.remove(e.id()); err == nil {
		t.Error("a config credential was removed")
	} else if !strings.Contains(err.Error(), "config") {
		t.Errorf("the refusal does not mention the config: %v", err)
	}
}

// TestPoolSurvivesACorruptStore checks the degradation the brief asks for: a
// corrupt accounts.json must not wedge the module, and the configured roster
// must still work.
func TestPoolSurvivesACorruptStore(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, accountsFile)
	if err := os.WriteFile(storePath, []byte("{not json at all"), 0o600); err != nil {
		t.Fatalf("writing the corrupt store: %v", err)
	}

	var reported error
	p := newPool()
	p.storePath = storePath
	p.statePath = filepath.Join(dir, stateFile)
	p.onSave = func(err error) { reported = err }
	cfg := testAccount()
	cfg.AccessKeyID = "AKFROMCONFIG"
	p.load([]account{cfg})

	if p.len() != 1 {
		t.Fatalf("a corrupt store cost the configured roster: %d entries", p.len())
	}
	if reported == nil {
		t.Error("the corrupt store was not reported")
	}
}

// TestPoolStateIsSeparateFromCredentials checks that health lives in its own
// file: a state write must never rewrite the credential store.
func TestPoolStateIsSeparateFromCredentials(t *testing.T) {
	dir := t.TempDir()
	p := newPool()
	p.storePath = filepath.Join(dir, accountsFile)
	p.statePath = filepath.Join(dir, stateFile)
	p.load(nil)
	p.put(testAccount(), true)

	storeBefore, err := os.ReadFile(p.storePath)
	if err != nil {
		t.Fatalf("reading the store: %v", err)
	}

	e := p.pick(nil)
	p.markFailure(e, kindQuota, "out of credit")
	p.persist(true)

	storeAfter, err := os.ReadFile(p.storePath)
	if err != nil {
		t.Fatalf("re-reading the store: %v", err)
	}
	if string(storeBefore) != string(storeAfter) {
		t.Error("a health update rewrote the credential store")
	}
	if _, err := os.Stat(p.statePath); err != nil {
		t.Errorf("the state file was not written: %v", err)
	}

	// The health survives a reload.  The entry is not available — a quota
	// cooldown lasts a day — so the state is read directly rather than through
	// pick, which only ever returns entries that can serve right now.
	reloaded := newPool()
	reloaded.storePath = p.storePath
	reloaded.statePath = p.statePath
	reloaded.load(nil)
	all := reloaded.all()
	if len(all) != 1 {
		t.Fatalf("the reloaded pool has %d entries, want 1", len(all))
	}
	if all[0].state != stateExhausted {
		t.Errorf("state = %q, want %q", all[0].state, stateExhausted)
	}
	if all[0].until <= time.Now().UnixMilli() {
		t.Errorf("the cooldown deadline did not survive the reload: %d", all[0].until)
	}
	if reloaded.pick(nil) != nil {
		t.Error("a credential inside its quota cooldown was offered for use")
	}
	if all[0].account().AccessKeyID != testAccount().AccessKeyID {
		t.Error("the credential did not survive alongside its health")
	}
}
