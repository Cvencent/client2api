package workbuddy

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

// cockpit_test.go covers the operator-supplied credential bundle: the JSON
// export a cockpit install produces, uploaded through the panel.  Every test
// here builds its own data directory and a fake transport, so nothing reads the
// developer's real credentials or reaches the network.

// cockpitFarFuture is a millisecond expiry far enough out that the pool never
// treats the imported account as stale.  The vendor dump uses milliseconds.
const cockpitFarFuture = 4102444800000 // 2100-01-01T00:00:00Z

// cockpitRT answers every upstream call with an empty success envelope.  The
// bundle import does best-effort post-import work (check-in, trial, balance),
// and this keeps that work harmless without pretending any of it succeeded.
func cockpitRT(t *testing.T) *fakeRT {
	t.Helper()
	return &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(200, wbEnvelope("")), nil
	}}
}

// cockpitBundle renders a bundle document from account rows.
func cockpitBundle(t *testing.T, rows ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return b
}

// cockpitRow is the common shape of a dump entry: the fields the importer needs
// plus the vendor-only columns it must tolerate.
func cockpitRow(id, uid, access, refresh, domain string) map[string]any {
	return map[string]any{
		"id": id, "uid": uid,
		"access_token": access, "refresh_token": refresh,
		"token_type": "Bearer", "domain": domain,
		"expires_at": cockpitFarFuture,
		"email":      id + "@example.test",
		"status":     "1", "payment_type": "0", "checkin_streak": 3,
		"dosage_notify_code": "", "usage_updated_at": 0,
		"last_checkin_time": 0, "created_at": 0, "last_used": 0,
	}
}

// cockpitFileNames lists the credential files in dir, sorted.
func cockpitFileNames(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, panelFilePrefix+"*.json"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	return names
}

// cockpitReadAuth loads one stored credential back off disk.
func cockpitReadAuth(t *testing.T, dir, uid string) *Auth {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, panelFileName(uid)))
	if err != nil {
		t.Fatalf("read credential for %s: %v", uid, err)
	}
	a, err := ParseAuth(raw)
	if err != nil {
		t.Fatalf("ParseAuth(%s): %v", uid, err)
	}
	return a
}

func TestWorkbuddyImportBundleStoresEveryValidAccount(t *testing.T) {
	c, dir := panelClient(t, cockpitRT(t), nil)
	doc := cockpitBundle(t,
		cockpitRow("a1", "uid-bundle-0001", "at-1", "rt-1", "copilot.tencent.com"),
		cockpitRow("a2", "uid-bundle-0002", "at-2", "rt-2", "workbuddy.ai"),
	)
	rep, err := c.ImportBundle(context.Background(), "dump.json", doc)
	if err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if rep.Total != 2 || rep.Imported != 2 || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want total=2 imported=2 skipped=0", rep)
	}
	if len(rep.Errors) != 0 {
		t.Fatalf("errors = %v, want none", rep.Errors)
	}
	if names := cockpitFileNames(t, dir); len(names) != 2 {
		t.Fatalf("stored files = %v, want 2", names)
	}
	// The realm is derived from the domain at import time, because the pool
	// decides by realm name first and by domain second: recording the answer
	// here is what keeps the two in agreement.
	cn := cockpitReadAuth(t, dir, "uid-bundle-0001")
	if cn.UID != "uid-bundle-0001" || cn.AccessTokenValue() != "at-1" || cn.RefreshTokenValue() != "rt-1" {
		t.Fatalf("cn credential = %+v", cn)
	}
	if cn.IsGlobal() {
		t.Fatalf("copilot.tencent.com was filed as international")
	}
	intl := cockpitReadAuth(t, dir, "uid-bundle-0002")
	if !intl.IsGlobal() {
		t.Fatalf("workbuddy.ai was not filed as international")
	}
	// The nickname falls back to the email when the dump has no nickname.
	if intl.NicknameValue() != "a2@example.test" {
		t.Fatalf("nickname = %q, want the email fallback", intl.NicknameValue())
	}
	if got := c.pool.Len(); got != 2 {
		t.Fatalf("pool holds %d accounts, want 2", got)
	}
}

func TestWorkbuddyImportBundleSkipsBadRowsAndKeepsTheGoodOnes(t *testing.T) {
	c, dir := panelClient(t, cockpitRT(t), nil)
	noToken := cockpitRow("b2", "uid-bundle-0012", "", "rt-2", "copilot.tencent.com")
	badUID := cockpitRow("b3", "uid/bundle/0013", "at-3", "rt-3", "copilot.tencent.com")
	doc := cockpitBundle(t,
		cockpitRow("b1", "uid-bundle-0011", "at-1", "rt-1", "copilot.tencent.com"),
		noToken,
		badUID,
		cockpitRow("b4", "uid-bundle-0014", "at-4", "rt-4", "copilot.tencent.com"),
	)
	rep, err := c.ImportBundle(context.Background(), "dump.json", doc)
	if err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if rep.Total != 4 || rep.Imported != 2 || rep.Skipped != 2 {
		t.Fatalf("report = %+v, want total=4 imported=2 skipped=2", rep)
	}
	// One unusable row must not cost the other ninety-nine: the reasons are
	// reported per row and the import still succeeds.
	if len(rep.Errors) != 2 {
		t.Fatalf("errors = %v, want 2 entries", rep.Errors)
	}
	joined := strings.Join(rep.Errors, "\n")
	if !strings.Contains(joined, "b2") {
		t.Fatalf("errors do not name the row with the missing token: %v", rep.Errors)
	}
	if !strings.Contains(joined, "b3") {
		t.Fatalf("errors do not name the row with the invalid uid: %v", rep.Errors)
	}
	if names := cockpitFileNames(t, dir); len(names) != 2 {
		t.Fatalf("stored files = %v, want 2", names)
	}
	if got := c.pool.Len(); got != 2 {
		t.Fatalf("pool holds %d accounts, want 2", got)
	}
}

func TestWorkbuddyImportBundleRejectsADocumentItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"not json", "{not json", "invalid json"},
		{"not an array", `{"uid":"uid-1"}`, "invalid json"},
		{"empty array", `[]`, "contains no accounts"},
		{"null", `null`, "contains no accounts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := panelClient(t, cockpitRT(t), nil)
			rep, err := c.ImportBundle(context.Background(), "dump.json", []byte(tc.doc))
			if err == nil {
				t.Fatalf("ImportBundle accepted %q (report %+v)", tc.doc, rep)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if names := cockpitFileNames(t, dir); len(names) != 0 {
				t.Fatalf("a rejected document still wrote %v", names)
			}
		})
	}
}

func TestWorkbuddyImportBundleReadsMillisecondExpiryAndDefaultsAMissingOne(t *testing.T) {
	c, dir := panelClient(t, cockpitRT(t), nil)
	dated := cockpitRow("c1", "uid-bundle-0021", "at-1", "rt-1", "copilot.tencent.com")
	dated["expires_at"] = 1700000000000
	undated := cockpitRow("c2", "uid-bundle-0022", "at-2", "rt-2", "copilot.tencent.com")
	undated["expires_at"] = 0
	doc := cockpitBundle(t, dated, undated)
	rep, err := c.ImportBundle(context.Background(), "dump.json", doc)
	if err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if rep.Imported != 2 {
		t.Fatalf("report = %+v, want 2 imported", rep)
	}
	// The dump counts milliseconds; a unix-seconds store would put every
	// account in the year 55000 and no refresh would ever fire.
	if got, want := cockpitReadAuth(t, dir, "uid-bundle-0021").ExpiresAt, int64(1700000000); got != want {
		t.Fatalf("expiry = %d, want %d", got, want)
	}
	// A missing expiry is unknown, not expired: the account is still usable and
	// a refresh will settle the real value.
	want := time.Now().Add(cockpitDefaultLifetime).Unix()
	got := cockpitReadAuth(t, dir, "uid-bundle-0022").ExpiresAt
	if got < want-120 || got > want+120 {
		t.Fatalf("defaulted expiry = %d, want about %d", got, want)
	}
}

func TestWorkbuddyImportBundleRefusesAUidThatCouldLeaveTheDirectory(t *testing.T) {
	// The uid becomes part of the file name, so the character set is a safety
	// property and not a tidiness one: "../evil" would otherwise choose where
	// the credential lands.
	bad := []string{"../evil", `..\evil`, "a/b", "a b", "uid\t1", strings.Repeat("u", 65)}
	for _, uid := range bad {
		t.Run(uid, func(t *testing.T) {
			c, dir := panelClient(t, cockpitRT(t), nil)
			doc := cockpitBundle(t, cockpitRow("d1", uid, "at-1", "rt-1", "copilot.tencent.com"))
			rep, err := c.ImportBundle(context.Background(), "dump.json", doc)
			if err != nil {
				t.Fatalf("ImportBundle: %v", err)
			}
			if rep.Imported != 0 || rep.Skipped != 1 {
				t.Fatalf("report = %+v, want 0 imported and 1 skipped", rep)
			}
			if names := cockpitFileNames(t, dir); len(names) != 0 {
				t.Fatalf("uid %q still wrote %v", uid, names)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "evil.json")); err == nil {
				t.Fatalf("uid %q escaped the accounts directory", uid)
			}
		})
	}
}

func TestWorkbuddyImportBundleOverwritesTheAccountItAlreadyHas(t *testing.T) {
	c, dir := panelClient(t, cockpitRT(t), nil)
	first := cockpitBundle(t, cockpitRow("e1", "uid-bundle-0031", "at-old", "rt-old", "copilot.tencent.com"))
	if _, err := c.ImportBundle(context.Background(), "first.json", first); err != nil {
		t.Fatalf("first ImportBundle: %v", err)
	}
	second := cockpitBundle(t, cockpitRow("e1", "uid-bundle-0031", "at-new", "rt-new", "copilot.tencent.com"))
	rep, err := c.ImportBundle(context.Background(), "second.json", second)
	if err != nil {
		t.Fatalf("second ImportBundle: %v", err)
	}
	if rep.Imported != 1 {
		t.Fatalf("report = %+v, want 1 imported", rep)
	}
	// Re-uploading a newer dump is how an operator refreshes one account, so
	// the same uid must land on the same file rather than accumulate copies.
	if names := cockpitFileNames(t, dir); len(names) != 1 {
		t.Fatalf("stored files = %v, want the re-import to overwrite in place", names)
	}
	if got := cockpitReadAuth(t, dir, "uid-bundle-0031").AccessTokenValue(); got != "at-new" {
		t.Fatalf("access token = %q, want the newer dump to win", got)
	}
	if got := c.pool.Len(); got != 1 {
		t.Fatalf("pool holds %d accounts, want 1", got)
	}
}

func TestWorkbuddyImportBundleKeepsTwoAccountsThatShareAFilenamePrefix(t *testing.T) {
	c, dir := panelClient(t, cockpitRT(t), nil)
	// File names are truncated to 48 characters, so two long uids can share a
	// slug.  Overwriting there would destroy one account with the other's
	// credentials, so a colliding uid has to get a disambiguating suffix.
	prefix := strings.Repeat("a", 48)
	doc := cockpitBundle(t,
		cockpitRow("f1", prefix+"1", "at-1", "rt-1", "copilot.tencent.com"),
		cockpitRow("f2", prefix+"2", "at-2", "rt-2", "copilot.tencent.com"),
	)
	rep, err := c.ImportBundle(context.Background(), "dump.json", doc)
	if err != nil {
		t.Fatalf("ImportBundle: %v", err)
	}
	if rep.Imported != 2 {
		t.Fatalf("report = %+v, want 2 imported", rep)
	}
	if names := cockpitFileNames(t, dir); len(names) != 2 {
		t.Fatalf("stored files = %v, want 2 distinct files", names)
	}
	if got := c.pool.Len(); got != 2 {
		t.Fatalf("pool holds %d accounts, want 2", got)
	}
}

func TestWorkbuddyImportBundleIsAdvertisedAsACapability(t *testing.T) {
	c, _ := panelClient(t, cockpitRT(t), nil)
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Bundle {
		t.Fatal("capabilities do not advertise the bundle import")
	}
	if _, ok := core.AsBundleImporter(c); !ok {
		t.Fatal("the client does not satisfy core.BundleImporter")
	}
}
