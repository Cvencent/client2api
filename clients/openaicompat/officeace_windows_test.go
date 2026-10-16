//go:build windows

package openaicompat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"client2api/internal/core"
)

func protectOfficeAceDPAPIForTest(t *testing.T, plain []byte) []byte {
	t.Helper()
	if len(plain) == 0 {
		t.Fatal("nothing to protect")
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		t.Fatalf("CryptProtectData: %v", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...)
}

// The client stores the install secret as an OC-DPAPI-1 envelope; unwrapping it
// has to return exactly the secret the client wrote.
func TestOfficeAceDPAPIEnvelopeRoundTrip(t *testing.T) {
	secret := "officeace-test-dpapi-secret"
	blob := protectOfficeAceDPAPIForTest(t, []byte(secret))
	raw := []byte(officeAceDPAPIMagic + "\n" + base64.StdEncoding.EncodeToString(blob) + "\n")
	got, err := parseOfficeAceInstallSecret(raw, unprotectOfficeAceDPAPI)
	if err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if got != secret {
		t.Fatalf("unwrapped secret = %q, want the protected value", got)
	}
}

// setupOfficeAceStore writes a fabricated profile store and points discovery at
// it through the client's own OFFICE_CLAW_SECURE_CONFIG_DIR override, so the
// test never touches a real OfficeAce installation.
func setupOfficeAceStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(officeAceSecureDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, officeAceKeyFileName), []byte(testOfficeAceSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write install secret: %v", err)
	}
	path := filepath.Join(dir, "oauth-acct-1%3Auser-1.json")
	if err := os.WriteFile(path, sealOfficeAceProfile(t, testOfficeAceSecret, officeAceTestProfile()), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

func TestOfficeAceDiscoverAndImportLocalProfile(t *testing.T) {
	profilePath := setupOfficeAceStore(t)
	cfg := core.SourceConfig{BaseURL: testOfficeAceBaseURL}
	deps := core.Deps{DataDir: t.TempDir()}
	s, err := newSource("officeace", cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	importer, ok := wrapOfficeAceSource("officeace", cfg, s).(core.CredentialImporter)
	if !ok {
		t.Fatal("the OfficeAce relay must implement CredentialImporter")
	}
	ctx := context.Background()

	found, err := importer.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("discovered %d credentials, want 1", len(found))
	}
	if !found[0].Importable || found[0].Imported {
		t.Fatalf("importable/imported = %v/%v, want true/false", found[0].Importable, found[0].Imported)
	}
	if got := found[0].Label; got != "test-user" {
		t.Fatalf("label = %q, want the OfficeAce user name", got)
	}
	if got := found[0].Kind; got != "officeace-oauth-profile" {
		t.Fatalf("kind = %q", got)
	}
	// Discovery is what the panel renders, so it must not carry the key pair.
	payload, err := json.Marshal(found)
	if err != nil {
		t.Fatalf("marshal discovery: %v", err)
	}
	if strings.Contains(string(payload), testOfficeAceAppKey) || strings.Contains(string(payload), testOfficeAceAppSecret) {
		t.Fatal("discovery output leaked a credential")
	}

	records, err := importer.Import(ctx, []string{profilePath}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("imported %d accounts, want 1", len(records))
	}
	if records[0].Label != "test-user" || !strings.HasPrefix(records[0].ID, "oa-") {
		t.Fatalf("record = %+v", records[0])
	}

	stored, err := os.ReadFile(filepath.Join(deps.DataDir, "accounts.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !strings.Contains(string(stored), testOfficeAceAppKey) || !strings.Contains(string(stored), testOfficeAceAppSecret) {
		t.Fatal("the app key pair was not persisted for the relay")
	}

	accounts, err := s.Accounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts = %d, %v", len(accounts), err)
	}
	found, err = importer.Discover(ctx)
	if err != nil {
		t.Fatalf("second discover: %v", err)
	}
	if len(found) != 1 || !found[0].Imported {
		t.Fatalf("second discovery did not report the account as imported")
	}

	again, err := importer.Import(ctx, nil, true)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if len(again) != 1 || again[0].ID != records[0].ID {
		t.Fatalf("re-import created a second row: %+v", again)
	}
	accounts, err = s.Accounts(ctx)
	if err != nil || len(accounts) != 1 {
		t.Fatalf("accounts after re-import = %d, %v", len(accounts), err)
	}
}

func TestOfficeAceImportKeepsAccountGateway(t *testing.T) {
	profilePath := setupOfficeAceStore(t)
	cfg := core.SourceConfig{BaseURL: "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2"}
	s, err := newSource("officeace", cfg, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	importer, ok := wrapOfficeAceSource("officeace", cfg, s).(core.CredentialImporter)
	if !ok {
		t.Fatal("the OfficeAce relay must implement CredentialImporter")
	}
	ctx := context.Background()

	found, err := importer.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("discovered %d credentials, want 1", len(found))
	}
	if !found[0].Importable {
		t.Fatal("a profile from another OfficeAce gateway must remain importable")
	}
	if !strings.Contains(found[0].Note, "不同") || !strings.Contains(found[0].Note, "自己的网关地址") {
		t.Fatalf("discovery note does not explain the account gateway override: %q", found[0].Note)
	}
	records, err := importer.Import(ctx, []string{profilePath}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("imported %d accounts, want 1", len(records))
	}
	s.mu.Lock()
	got := s.providerLocked(s.rows[0]).BaseURL
	s.mu.Unlock()
	if got != testOfficeAceBaseURL {
		t.Fatalf("account gateway = %q, want %q", got, testOfficeAceBaseURL)
	}
}
