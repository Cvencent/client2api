package openaicompat

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// Every value below is invented for the test.  Real OfficeAce credentials never
// enter a fixture, a log line or a test assertion string.
const (
	testOfficeAceSecret    = "officeace-test-install-secret"
	testOfficeAceAppKey    = "officeace-test-app-key"
	testOfficeAceAppSecret = "officeace-test-app-secret"
	testOfficeAceBaseURL   = "https://modelgw-0042.officeace.cn-north-4.huaweicloud-agentarts.com/v2"
)

// sealOfficeAceProfile builds the AES-256-GCM envelope the OfficeAce client
// writes, with a fabricated payload.
func sealOfficeAceProfile(t *testing.T, secret string, payload []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(officeAceProfileKey(secret))
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("cipher.NewGCM: %v", err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	sealed := gcm.Seal(nil, iv, payload, nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	data := sealed[:len(sealed)-gcm.Overhead()]
	raw, err := json.Marshal(officeAceEnvelope{
		Version: 1,
		Alg:     "aes-256-gcm",
		IV:      base64.RawURLEncoding.EncodeToString(iv),
		Tag:     base64.RawURLEncoding.EncodeToString(tag),
		Data:    base64.RawURLEncoding.EncodeToString(data),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func officeAceTestProfile() []byte {
	return []byte(`{
  "userName": "test-user",
  "accountId": "acct-1",
  "user_id": "user-1",
  "modelInfo": {
    "model_api_url_base": "https://modelgw-0042.officeace.cn-north-4.huaweicloud-agentarts.com",
    "model_auth_info": {
      "model_app_key": "` + testOfficeAceAppKey + `",
      "model_app_secret": "` + testOfficeAceAppSecret + `",
      "maas_api_key": "not-used-by-the-relay"
    }
  }
}`)
}

func TestOfficeAceInstallSecretShapes(t *testing.T) {
	got, err := parseOfficeAceInstallSecret([]byte("  plain-secret\n"), func([]byte) ([]byte, error) {
		t.Fatal("a plaintext secret must not be sent to DPAPI")
		return nil, nil
	})
	if err != nil || got != "plain-secret" {
		t.Fatalf("plaintext secret = %q, %v", got, err)
	}

	blob := []byte{9, 8, 7, 6}
	envelope := officeAceDPAPIMagic + "\n" + base64.StdEncoding.EncodeToString(blob) + "\n"
	got, err = parseOfficeAceInstallSecret([]byte(envelope), func(in []byte) ([]byte, error) {
		if !bytes.Equal(in, blob) {
			t.Fatalf("DPAPI input = %v, want %v", in, blob)
		}
		return []byte("unwrapped-secret"), nil
	})
	if err != nil || got != "unwrapped-secret" {
		t.Fatalf("DPAPI secret = %q, %v", got, err)
	}

	for name, raw := range map[string][]byte{
		"empty":        []byte("   \n"),
		"bad base64":   []byte(officeAceDPAPIMagic + "\nnot base64 !!"),
		"empty DPAPI":  []byte(officeAceDPAPIMagic + "\n"),
		"empty unwrap": []byte(officeAceDPAPIMagic + "\n" + base64.StdEncoding.EncodeToString(blob)),
	} {
		unprotect := func([]byte) ([]byte, error) { return []byte("  "), nil }
		if name != "empty unwrap" {
			unprotect = func([]byte) ([]byte, error) { return []byte("ok"), nil }
		}
		if _, err := parseOfficeAceInstallSecret(raw, unprotect); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
}

func TestOfficeAceDecryptProfile(t *testing.T) {
	secret := testOfficeAceSecret
	raw := sealOfficeAceProfile(t, secret, officeAceTestProfile())
	plain, err := decryptOfficeAceProfile(raw, officeAceProfileKey(secret))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	cred, err := parseOfficeAceCredential(plain, filepath.Join("store", "oauth-acct-1%3Auser-1.json"), "acct-1:user-1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cred.AppKey != testOfficeAceAppKey || cred.AppSecret != testOfficeAceAppSecret {
		t.Fatalf("credential pair did not round-trip")
	}
	if cred.BaseURL != testOfficeAceBaseURL {
		t.Fatalf("base url = %q, want %q", cred.BaseURL, testOfficeAceBaseURL)
	}
	if cred.UserName != "test-user" || cred.UserID != "acct-1:user-1" {
		t.Fatalf("identity = %q / %q", cred.UserName, cred.UserID)
	}
	if cred.label() != "test-user" {
		t.Fatalf("label = %q", cred.label())
	}
	if !cred.usable() {
		t.Fatal("credential should be usable")
	}
	id := cred.accountID()
	if !strings.HasPrefix(id, "oa-") || id != cred.accountID() {
		t.Fatalf("account id %q is not stable", id)
	}

	if _, err := decryptOfficeAceProfile(raw, officeAceProfileKey("another-secret")); err == nil {
		t.Fatal("a wrong install secret must not decrypt the profile")
	}

	var env officeAceEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	tag, err := base64.RawURLEncoding.DecodeString(env.Tag)
	if err != nil {
		t.Fatalf("decode tag: %v", err)
	}
	tag[len(tag)-1] ^= 0xff
	env.Tag = base64.RawURLEncoding.EncodeToString(tag)
	tampered, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal tampered envelope: %v", err)
	}
	if _, err := decryptOfficeAceProfile(tampered, officeAceProfileKey(secret)); err == nil {
		t.Fatal("a tampered profile must fail authentication")
	}
}

func TestOfficeAceEndpointAndBaseURL(t *testing.T) {
	if !officeAceEndpoint("officeace", "https://anything.test/v1") {
		t.Fatal("the officeace id must be treated as OfficeAce")
	}
	if !officeAceEndpoint("oa-gateway", "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2") {
		t.Fatal("an OfficeAce host must be detected by hostname")
	}
	if officeAceEndpoint("relay", "https://api.example.test/v1") {
		t.Fatal("an unrelated relay must not be treated as OfficeAce")
	}
	for raw, want := range map[string]string{
		"modelgw.example":                "https://modelgw.example/v2",
		"https://modelgw.example":        "https://modelgw.example/v2",
		"https://modelgw.example/v2/":    "https://modelgw.example/v2",
		"https://modelgw.example/prefix": "https://modelgw.example/prefix/v2",
		"":                               "",
	} {
		if got := normalizeOfficeAceBaseURL(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestOfficeAceProfileFilesSkipPendingAndForeign(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"oauth-acct-1%3Auser-1.json",
		"oauth-pending-deadbeef.json",
		"w3-someone.json",
		"notes.txt",
		"oauth-broken.json.bak",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	files := officeAceProfileFiles(dir)
	if len(files) != 1 {
		t.Fatalf("profile files = %d, want 1", len(files))
	}
	if files[0].userID != "acct-1:user-1" {
		t.Fatalf("decoded user id = %q", files[0].userID)
	}
	if got := officeAceLabelFromPath(files[0].path); got != "acct-1:user-1" {
		t.Fatalf("label from path = %q", got)
	}
}

func TestWrapOfficeAceSourceOnlyWrapsOfficeAce(t *testing.T) {
	cfg := core.SourceConfig{BaseURL: "https://api.example.test/v1"}
	s, err := newSource("relay", cfg, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := wrapOfficeAceSource("relay", cfg, s).(core.CredentialImporter); ok {
		t.Fatal("a plain custom relay must not offer credential import")
	}

	if !officeAceLocalSupported() {
		return
	}
	oaCfg := core.SourceConfig{BaseURL: "https://modelgw-0009.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2"}
	oa, err := newSource("officeace", oaCfg, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := wrapOfficeAceSource("officeace", oaCfg, oa)
	if _, ok := wrapped.(core.CredentialImporter); !ok {
		t.Fatal("an OfficeAce relay must offer credential import")
	}
	if _, ok := wrapped.(core.AccountManager); !ok {
		t.Fatal("the wrapped relay must keep its account manager surface")
	}
	if _, ok := wrapped.(core.SourceClient); !ok {
		t.Fatal("the wrapped relay must keep its source surface")
	}
}

func TestOfficeAceImportEmptySelectionIsNoop(t *testing.T) {
	s, err := newSource("officeace", core.SourceConfig{BaseURL: "https://relay.test/v2"}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	local := &officeAceLocalSource{officeAceSource: &officeAceSource{source: s}}
	recs, err := local.Import(context.Background(), nil, false)
	if err != nil {
		t.Fatalf("empty selection returned an error: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("empty selection imported %d accounts", len(recs))
	}
}
