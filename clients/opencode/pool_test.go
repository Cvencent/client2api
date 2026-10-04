package opencode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestKeyFingerprintIsStableAndNotTheKey(t *testing.T) {
	key := "sk-abcdefghijklmnop"
	fp := keyFingerprint(key)
	if fp == "" {
		t.Fatal("keyFingerprint returned an empty string")
	}
	if strings.Contains(fp, key) || strings.Contains(key, fp) {
		t.Fatalf("fingerprint %q leaks the key", fp)
	}
	if len(fp) != 16 {
		t.Fatalf("fingerprint length = %d, want 16 hex chars", len(fp))
	}
	if keyFingerprint(key) != fp {
		t.Fatal("keyFingerprint is not deterministic")
	}
	if keyFingerprint(key+"x") == fp {
		t.Fatal("keyFingerprint collided on different keys")
	}
}

func TestAccountIDUsesTheFingerprint(t *testing.T) {
	id := accountID(keyFingerprint("sk-secret"))
	if !strings.HasPrefix(id, clientName+":") {
		t.Fatalf("accountID = %q, want the %s: prefix", id, clientName)
	}
	if strings.Contains(id, "sk-secret") {
		t.Fatalf("accountID %q embeds the raw key", id)
	}
}

func TestSourceLabel(t *testing.T) {
	cases := map[string]string{
		sourceConfig: "config",
		sourceEnv:    "environment",
		sourceImport: "imported",
		sourcePanel:  "panel",
		// An empty source is a record the store did not label; the panel shows
		// "unknown" rather than an empty cell, and an unrecognised source is
		// echoed rather than hidden.
		"":       "unknown",
		"custom": "custom",
	}
	for in, want := range cases {
		if got := sourceLabel(in); got != want {
			t.Fatalf("sourceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Selection.
// ---------------------------------------------------------------------------

func TestPoolAcquireWithoutAccountsIsNotConfigured(t *testing.T) {
	p := newPool()
	_, err := p.acquire(testNow, 4)
	wantErrIs(t, err, core.ErrNotConfigured)
	if !strings.Contains(err.Error(), "no OpenCode Zen account") {
		t.Fatalf("error = %q, want it to say there is no account", err)
	}
}

func TestPoolAcquireWithEveryAccountDisabledIsNotConfigured(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: false})
	_, err := p.acquire(testNow, 4)
	wantErrIs(t, err, core.ErrNotConfigured)
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error = %q, want it to say the accounts are disabled", err)
	}
}

// A cooling account must not be handed out: that is the whole point of parking.
func TestPoolAcquireSkipsCoolingAccounts(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.upsert(accountRecord{ID: "b", APIKey: "sk-b", Enabled: true})
	p.setCooldown("a", testNow.Add(time.Hour), "rate limit")

	got, err := p.acquire(testNow, 4)
	if err != nil {
		t.Fatalf("acquire = %v", err)
	}
	if got.ID != "b" {
		t.Fatalf("acquired %q, want the account that is not cooling down", got.ID)
	}
}

func TestPoolAcquireHonoursTheInFlightCeiling(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	if _, err := p.acquire(testNow, 1); err != nil {
		t.Fatalf("first acquire = %v", err)
	}
	_, err := p.acquire(testNow, 1)
	wantErrIs(t, err, core.ErrBusy)
	if !strings.Contains(err.Error(), "in-flight ceiling") {
		t.Fatalf("error = %q, want it to mention the in-flight ceiling", err)
	}
}

func TestPoolAcquireRoundRobins(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.upsert(accountRecord{ID: "b", APIKey: "sk-b", Enabled: true})

	first, err := p.acquire(testNow, 4)
	if err != nil {
		t.Fatalf("acquire = %v", err)
	}
	second, err := p.acquire(testNow, 4)
	if err != nil {
		t.Fatalf("acquire = %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("both acquires returned %q, want the rotation to move on", first.ID)
	}
}

// release takes an id, not a pointer, so a stream that outlives a reload still
// balances the books.
func TestPoolReleaseBalancesByID(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	if _, err := p.acquire(testNow, 1); err != nil {
		t.Fatalf("acquire = %v", err)
	}
	if inFlight, _ := p.stats(1); inFlight != 1 {
		t.Fatalf("in flight = %d, want 1", inFlight)
	}
	p.release("a")
	if inFlight, _ := p.stats(1); inFlight != 0 {
		t.Fatalf("in flight = %d, want 0 after release", inFlight)
	}
	// A release for an account that no longer exists must not go negative.
	p.release("a")
	if inFlight, _ := p.stats(1); inFlight != 0 {
		t.Fatalf("in flight = %d, want 0 after a second release", inFlight)
	}
}

// A reload must carry the in-flight count across, or an operator editing the
// panel could raise the concurrency ceiling behind the module's back.
func TestPoolReloadCarriesInFlight(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	if _, err := p.acquire(testNow, 1); err != nil {
		t.Fatalf("acquire = %v", err)
	}
	p.reload([]accountRecord{{ID: "a", APIKey: "sk-a", Enabled: true}})
	if inFlight, _ := p.stats(1); inFlight != 1 {
		t.Fatalf("in flight = %d, want the reload to preserve it", inFlight)
	}
}

// A timestamp we cannot parse must not park an account forever.
func TestPoolCoolingDownIgnoresAnUnparseableTimestamp(t *testing.T) {
	a := &accountRecord{ID: "a", Enabled: true, APIKey: "sk", CooldownUntil: "not-a-time"}
	if coolingDown(a, testNow) {
		t.Fatal("an unparseable CooldownUntil parked the account")
	}
	a.CooldownUntil = testNow.Add(-time.Minute).UTC().Format(time.RFC3339)
	if coolingDown(a, testNow) {
		t.Fatal("an expired cooldown still parked the account")
	}
	a.CooldownUntil = testNow.Add(time.Minute).UTC().Format(time.RFC3339)
	if !coolingDown(a, testNow) {
		t.Fatal("a live cooldown did not park the account")
	}
}

// ---------------------------------------------------------------------------
// Mutators.
// ---------------------------------------------------------------------------

// "out of credit" and "rate limited" want different cooldowns, so the kind is
// derived from the vendor's own words.
func TestPoolSetCooldownClassifiesQuotaFromTheReason(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.setCooldown("a", testNow.Add(time.Hour), "insufficient credit")
	got, _ := p.byID("a")
	if got.CooldownKind != "quota" {
		t.Fatalf("CooldownKind = %q, want quota for a credit reason", got.CooldownKind)
	}
	p.setCooldown("a", testNow.Add(time.Minute), "rate limit exceeded")
	got, _ = p.byID("a")
	if got.CooldownKind != "rate" {
		t.Fatalf("CooldownKind = %q, want rate", got.CooldownKind)
	}
}

// A 400 must never be displayed as "rate limited for an hour": the park only
// happens once the error count crosses the threshold.
//
// noteError's bool reports "the account existed and was touched", not "the
// account was parked", so the assertions below read the record itself.
func TestPoolNoteErrorParksOnlyAfterTheThreshold(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})

	p.noteError("a", "boom", testNow, 3)
	got, _ := p.byID("a")
	if got.ErrCount != 1 {
		t.Fatalf("ErrCount = %d, want 1", got.ErrCount)
	}
	if got.CooldownUntil != "" {
		t.Fatalf("CooldownUntil = %q, want none below the threshold", got.CooldownUntil)
	}

	p.noteError("a", "boom", testNow, 3)
	if got, _ = p.byID("a"); got.CooldownUntil != "" {
		t.Fatalf("CooldownUntil = %q, want none at ErrCount %d", got.CooldownUntil, got.ErrCount)
	}

	p.noteError("a", "boom", testNow, 3)
	got, _ = p.byID("a")
	if got.ErrCount != 3 {
		t.Fatalf("ErrCount = %d, want 3", got.ErrCount)
	}
	if got.CooldownUntil == "" {
		t.Fatal("the third soft error did not park the account")
	}
	if got.CooldownKind != "rate" {
		t.Fatalf("CooldownKind = %q, want rate for a soft error", got.CooldownKind)
	}
	want := testNow.Add(defaultCooldown).UTC().Format(time.RFC3339)
	if got.CooldownUntil != want {
		t.Fatalf("CooldownUntil = %q, want %q", got.CooldownUntil, want)
	}
}

// "disabled" and "rate limited" are different states and must not be confused.
func TestPoolDisableClearsTheCooldown(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.setCooldown("a", testNow.Add(time.Hour), "rate limit")

	if !p.disable("a", "invalid api key") {
		t.Fatal("disable reported no change")
	}
	got, _ := p.byID("a")
	if got.Enabled {
		t.Fatal("the account is still enabled after disable")
	}
	if got.CooldownUntil != "" {
		t.Fatalf("CooldownUntil = %q, want it cleared: a disabled account is not rate limited", got.CooldownUntil)
	}
	if got.LastError != "invalid api key" {
		t.Fatalf("LastError = %q, want the reason recorded", got.LastError)
	}
}

func TestPoolEnableClearsTheErrorState(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: false})
	p.noteError("a", "boom", testNow, 1)
	p.disable("a", "dead")

	if !p.setEnabled("a", true) {
		t.Fatal("setEnabled(true) reported no change")
	}
	got, _ := p.byID("a")
	if !got.Enabled {
		t.Fatal("the account is still disabled")
	}
	if got.CooldownUntil != "" || got.LastError != "" || got.ErrCount != 0 {
		t.Fatalf("re-enabling left stale state: cooldown=%q err=%q count=%d",
			got.CooldownUntil, got.LastError, got.ErrCount)
	}
}

func TestPoolRemoveAndUnknownIDs(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	if p.remove("nope") {
		t.Fatal("remove of an unknown id reported a change")
	}
	if !p.remove("a") {
		t.Fatal("remove of a known id reported no change")
	}
	if p.size() != 0 {
		t.Fatalf("size = %d, want 0", p.size())
	}
	if p.setEnabled("a", true) || p.setNote("a", "x") || p.clearCooldown("a") || p.disable("a", "x") {
		t.Fatal("a mutator acted on an account that no longer exists")
	}
}

// ---------------------------------------------------------------------------
// Projection.
// ---------------------------------------------------------------------------

func TestPoolStateOf(t *testing.T) {
	cases := []struct {
		name string
		rec  *accountRecord
		want string
	}{
		{"nil", nil, "unknown"},
		{"no key", &accountRecord{ID: "a", Enabled: true}, "invalid"},
		{"disabled", &accountRecord{ID: "a", Enabled: false, APIKey: "sk"}, "invalid"},
		{"ready", &accountRecord{ID: "a", Enabled: true, APIKey: "sk"}, "ready"},
		{
			"cooling",
			&accountRecord{ID: "a", Enabled: true, APIKey: "sk",
				CooldownUntil: testNow.Add(time.Minute).UTC().Format(time.RFC3339)},
			"cooling",
		},
		{
			"exhausted",
			&accountRecord{ID: "a", Enabled: true, APIKey: "sk", CooldownKind: "quota",
				CooldownUntil: testNow.Add(time.Minute).UTC().Format(time.RFC3339)},
			"exhausted",
		},
		{"last error", &accountRecord{ID: "a", Enabled: true, APIKey: "sk", LastError: "boom"}, "cooling"},
	}
	for _, tc := range cases {
		if got := stateOf(tc.rec, testNow); got != tc.want {
			t.Fatalf("%s: stateOf = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The panel is an operator surface, so it sees a masked key and no identity
// claim: Zen's key is not an account id.
func TestPoolStatusesMaskTheKeyAndLeaveIdentityEmpty(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{
		ID: "opencode:abc", Label: "laptop", APIKey: "sk-verysecretvalue",
		Enabled: true, Source: sourcePanel, AddedAt: testNow.UTC().Format(time.RFC3339),
	})
	got := p.statuses(testNow)
	if len(got) != 1 {
		t.Fatalf("statuses = %d, want 1", len(got))
	}
	st := got[0]
	if st.Identity != "" {
		t.Fatalf("Identity = %q, want empty: Zen exposes no account id", st.Identity)
	}
	blob, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), "sk-verysecretvalue") {
		t.Fatalf("the raw key reached the panel: %s", blob)
	}
	if st.Extra["source"] != "panel" {
		t.Fatalf("Extra[source] = %v, want panel", st.Extra["source"])
	}
}

func TestPoolSummaryCounts(t *testing.T) {
	p := newPool()
	got := p.summary(testNow, 4)
	if !strings.Contains(got, "0/0 accounts ready") {
		t.Fatalf("summary = %q, want the 0/0 form", got)
	}

	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.upsert(accountRecord{ID: "b", APIKey: "sk-b", Enabled: true})
	p.upsert(accountRecord{ID: "c", APIKey: "sk-c", Enabled: false})
	p.setCooldown("b", testNow.Add(time.Hour), "rate limit")

	got = p.summary(testNow, 4)
	for _, want := range []string{"1/3 accounts ready", "1 cooling", "1 disabled"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary = %q, want it to contain %q", got, want)
		}
	}
}

func TestPoolStatsCountsInFlightAndFull(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.upsert(accountRecord{ID: "b", APIKey: "sk-b", Enabled: true})
	if _, err := p.acquire(testNow, 1); err != nil {
		t.Fatalf("acquire = %v", err)
	}
	inFlight, full := p.stats(1)
	if inFlight != 1 {
		t.Fatalf("inFlight = %d, want 1", inFlight)
	}
	if full != 1 {
		t.Fatalf("full = %d, want 1: the acquired account is at its ceiling", full)
	}
}

func TestSortedAccountIDs(t *testing.T) {
	got := sortedAccountIDs([]accountRecord{{ID: "c"}, {ID: "a"}, {ID: "b"}})
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sortedAccountIDs = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Persistence.
// ---------------------------------------------------------------------------

// An absent file means "no credentials yet", not a failure.
func TestLoadCredentialsMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	if got := loadCredentials(path); got != nil {
		t.Fatalf("loadCredentials(absent) = %v, want nil", got)
	}
}

func TestSaveAndLoadCredentialsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "accounts.json")
	in := []accountRecord{{
		ID: "opencode:abc", Label: "laptop", APIKey: "sk-secret",
		Enabled: true, Source: sourcePanel, ErrCount: 2,
		CooldownUntil: testNow.Add(time.Hour).UTC().Format(time.RFC3339),
		CooldownKind:  "quota",
	}}
	if err := saveCredentials(path, in); err != nil {
		t.Fatalf("saveCredentials = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Windows has no POSIX permission bits: os.Stat synthesises 0666 for any
	// writable file, so the 0600 that core.WriteJSONAtomic asks for cannot be
	// observed there.  Assert the owner-write bit on Windows and the exact
	// bits everywhere else, rather than skip the check entirely.
	if runtime.GOOS == "windows" {
		if info.Mode().Perm()&0o200 == 0 {
			t.Fatalf("mode = %v, want the owner-write bit set", info.Mode().Perm())
		}
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	out := loadCredentials(path)
	if len(out) != 1 {
		t.Fatalf("loadCredentials = %d records, want 1", len(out))
	}
	if out[0].APIKey != "sk-secret" || out[0].CooldownKind != "quota" || out[0].ErrCount != 2 {
		t.Fatalf("round trip lost data: %+v", out[0])
	}
}

func TestDecodeCredentialsBodyVariants(t *testing.T) {
	bare := `[{"id":"opencode:a","api_key":"sk-a","enabled":true}]`
	recs, err := decodeCredentialsBody([]byte(bare))
	if err != nil {
		t.Fatalf("bare array: %v", err)
	}
	if len(recs) != 1 || recs[0].APIKey != "sk-a" {
		t.Fatalf("bare array decoded to %+v", recs)
	}

	wrapped := `{"version":1,"accounts":[{"id":"opencode:b","api_key":"sk-b"}]}`
	recs, err = decodeCredentialsBody([]byte(wrapped))
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "opencode:b" {
		t.Fatalf("wrapped decoded to %+v", recs)
	}

	single := `{"id":"opencode:c","api_key":"sk-c"}`
	recs, err = decodeCredentialsBody([]byte(single))
	if err != nil {
		t.Fatalf("single object: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "opencode:c" {
		t.Fatalf("single object decoded to %+v", recs)
	}

	if _, err := decodeCredentialsBody([]byte(`not json`)); err == nil {
		t.Fatal("garbage decoded without an error")
	}
	if _, err := decodeCredentialsBody([]byte(`{}`)); err == nil {
		t.Fatal("an empty object decoded without an error")
	}
}

// A record with no key would be a permanently dead panel row.
func TestUpsertRejectsAKeylessRecord(t *testing.T) {
	p := newPool()
	if p.upsert(accountRecord{ID: "a"}) {
		t.Fatal("upsert accepted a record with no API key")
	}
	if p.size() != 0 {
		t.Fatalf("size = %d, want 0", p.size())
	}
}

func TestUpsertPreservesAddedAtAndReplacesTheKey(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-old", Enabled: true, AddedAt: "2026-01-01T00:00:00Z"})
	p.upsert(accountRecord{ID: "a", APIKey: "sk-new", Enabled: true})
	got, _ := p.byID("a")
	if got.APIKey != "sk-new" {
		t.Fatalf("APIKey = %q, want the new key", got.APIKey)
	}
	if got.AddedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("AddedAt = %q, want the original timestamp preserved", got.AddedAt)
	}
}

func TestLoadCredentialsIgnoresGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	if err := os.WriteFile(path, []byte("{ this is not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := loadCredentials(path); got != nil {
		t.Fatalf("loadCredentials(garbage) = %v, want nil", got)
	}
}

func TestPoolByIDReturnsACopy(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	got, ok := p.byID("a")
	if !ok {
		t.Fatal("byID did not find the account")
	}
	got.APIKey = "tampered"
	again, _ := p.byID("a")
	if again.APIKey != "sk-a" {
		t.Fatal("byID handed out a pointer into the pool")
	}
	if _, ok := p.byID("nope"); ok {
		t.Fatal("byID found an account that does not exist")
	}
}

func TestPoolFirstReadyFallsBackToAnyEnabledAccount(t *testing.T) {
	p := newPool()
	p.upsert(accountRecord{ID: "a", APIKey: "sk-a", Enabled: true})
	p.setCooldown("a", testNow.Add(time.Hour), "rate limit")
	got, ok := p.firstReady(testNow, 4)
	if !ok {
		t.Fatal("firstReady found nothing although an enabled account exists")
	}
	if got.ID != "a" {
		t.Fatalf("firstReady = %q, want the enabled account as a last resort", got.ID)
	}
	empty := newPool()
	if _, ok := empty.firstReady(testNow, 4); ok {
		t.Fatal("firstReady reported success on an empty pool")
	}
}

func TestErrorsAreTyped(t *testing.T) {
	p := newPool()
	_, err := p.acquire(testNow, 4)
	var f *core.Failure
	if errors.As(err, &f) {
		t.Fatalf("acquire error %v is a *core.Failure; it should be a plain sentinel so the gateway maps it to 503", err)
	}
}
