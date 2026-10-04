package zcode

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// envelope
// ---------------------------------------------------------------------------

// sealForTest writes the desktop client's envelope around plain using an
// explicit passphrase, so a test can produce both a value this machine can open
// and one it cannot.
func sealForTest(t *testing.T, plain, passphrase string) string {
	t.Helper()
	key := sha256.Sum256([]byte(passphrase))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x2a}, credentialNonceLen)
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	tag := sealed[len(sealed)-credentialTagLen:]
	body := sealed[:len(sealed)-credentialTagLen]

	b64 := base64.RawURLEncoding.EncodeToString
	return encryptedValuePrefix + b64(nonce) + "." + b64(tag) + "." + b64(body)
}

// ambientPassphrase reports the passphrase this machine derives with no
// environment override in play.
func ambientPassphrase(t *testing.T) string {
	t.Helper()
	t.Setenv(credentialSecretEnv, "")
	pass := credentialPassphrase()
	if pass == "" {
		t.Fatal("no passphrase derived for this machine")
	}
	return pass
}

func TestCredentialEnvelopeRoundTrips(t *testing.T) {
	isolateHome(t)
	pass := ambientPassphrase(t)

	const plain = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"
	sealed := sealForTest(t, plain, pass)

	got, ok := decryptValue(sealed)
	if !ok {
		t.Fatalf("decryptValue(%q) failed", sealed)
	}
	if got != plain {
		t.Errorf("decryptValue = %q, want %q", got, plain)
	}
}

func TestCredentialEnvelopeAlsoAcceptsTheTrailingTagLayout(t *testing.T) {
	isolateHome(t)
	pass := ambientPassphrase(t)

	key := sha256.Sum256([]byte(pass))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("gcm: %v", err)
	}
	nonce := bytes.Repeat([]byte{0x11}, credentialNonceLen)
	sealed := gcm.Seal(nil, nonce, []byte("plain-value"), nil)
	b64 := base64.RawURLEncoding.EncodeToString
	// Two-part form: nonce.ciphertext+tag.
	twoPart := encryptedValuePrefix + b64(nonce) + "." + b64(sealed)

	got, ok := decryptValue(twoPart)
	if !ok {
		t.Fatalf("decryptValue(%q) failed", twoPart)
	}
	if got != "plain-value" {
		t.Errorf("decryptValue = %q", got)
	}
}

func TestCredentialEnvelopeRefusesWhatItCannotOpen(t *testing.T) {
	isolateHome(t)
	pass := ambientPassphrase(t)
	good := sealForTest(t, "secret", pass)

	tests := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"plain value", "not-an-envelope"},
		{"prefix only", "enc:v1:"},
		{"opaque body", "enc:v1:opaque"},
		{"too many parts", "enc:v1:a.b.c.d"},
		{"nonce too short", "enc:v1:" + base64.RawURLEncoding.EncodeToString([]byte{1, 2}) + ".AAAA.AAAA"},
		{"not base64", "enc:v1:!!!!.!!!!.!!!!"},
		{"ciphertext too short", "enc:v1:AAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA.AA"},
		{"wrong passphrase", sealForTest(t, "secret", "a-completely-different-machine")},
		{"truncated envelope", good[:len(good)-4]},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := decryptValue(tc.value); ok {
				t.Errorf("decryptValue(%q) = %q, want refusal", tc.value, got)
			}
		})
	}
}

func TestCredentialEnvelopeHonoursTheEnvironmentOverride(t *testing.T) {
	isolateHome(t)
	t.Setenv(credentialSecretEnv, "operator-supplied")

	sealed := sealForTest(t, "from-the-environment", "operator-supplied")
	got, ok := decryptValue(sealed)
	if !ok || got != "from-the-environment" {
		t.Fatalf("decryptValue = %q, %v", got, ok)
	}

	// A value sealed under the machine-derived passphrase must not open now.
	ambient := sealForTest(t, "machine-derived", credentialSecretTag+"win32:C:\\home:someone")
	if got, ok := decryptValue(ambient); ok {
		t.Errorf("value under another passphrase opened as %q", got)
	}
}

func TestOpenCredentialPassesPlainValuesThrough(t *testing.T) {
	isolateHome(t)

	if got, ok := openCredential("  plain-key  "); !ok || got != "plain-key" {
		t.Errorf("openCredential(plain) = %q, %v", got, ok)
	}
	if _, ok := openCredential("   "); ok {
		t.Error("openCredential(blank) reported a credential")
	}
	if _, ok := openCredential("enc:v1:opaque"); ok {
		t.Error("openCredential(unopenable) reported a credential")
	}
}

// ---------------------------------------------------------------------------
// connection entries
// ---------------------------------------------------------------------------

const realConnectionKey = "account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key"

func TestAConnectionEntryBecomesAProviderAccount(t *testing.T) {
	acct, ok := providerAccountFromKey(realConnectionKey, "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x")
	if !ok {
		t.Fatal("providerAccountFromKey refused a real connection entry")
	}
	if acct.Provider != providerBigmodel {
		t.Errorf("provider = %q, want %q", acct.Provider, providerBigmodel)
	}
	if acct.Mode != modeAPIKey {
		t.Errorf("mode = %q, want %q", acct.Mode, modeAPIKey)
	}
	if acct.BaseURL != baseBigmodelKey {
		t.Errorf("baseURL = %q, want %q", acct.BaseURL, baseBigmodelKey)
	}
	if acct.UserID != "61161790588087632" {
		t.Errorf("user id = %q", acct.UserID)
	}
	if acct.secret() != "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x" {
		t.Errorf("secret = %q", acct.secret())
	}
	if acct.Label != "BigModel Coding Plan" {
		t.Errorf("label = %q", acct.Label)
	}
	if acct.ID != "zcode-credentials:bigmodel-individual-coding-plan:61161790588087632" {
		t.Errorf("id = %q", acct.ID)
	}
}

func TestAConnectionEntryHoldingAPlanJWTUsesThePlanEndpoint(t *testing.T) {
	jwt := makeJWT(`{"user_id":"u-9","sub":"u-9"}`)
	acct, ok := providerAccountFromKey(realConnectionKey, jwt)
	if !ok {
		t.Fatal("providerAccountFromKey refused a JWT connection entry")
	}
	if acct.Mode != modeJWT || acct.BaseURL != baseZaiPlan {
		t.Errorf("mode/baseURL = %q/%q", acct.Mode, acct.BaseURL)
	}
	if acct.UserID != "u-9" {
		t.Errorf("user id = %q", acct.UserID)
	}
}

func TestConnectionEntriesThatAreNotCredentialsAreRefused(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value any
	}{
		{"not a connection entry", "oauth:bigmodel:access_token", "token"},
		{"not an api key", "account-provider:coding-plan:account:bigmodel-x:account:1:refresh-token", "x"},
		{"unknown provider family", "account-provider:coding-plan:account:someone-else-x:account:1:api-key", "k"},
		{"no provider segment", "account-provider:coding-plan:api-key", "k"},
		{"non-string value", realConnectionKey, 42},
		{"empty value", realConnectionKey, ""},
		{"unopenable value", realConnectionKey, "enc:v1:opaque"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := providerAccountFromKey(tc.key, tc.value); ok {
				t.Errorf("providerAccountFromKey(%q) accepted a non-credential", tc.key)
			}
		})
	}
}

func TestSplitProviderAccountKey(t *testing.T) {
	tests := []struct {
		key      string
		provider string
		kind     string
		userID   string
		ok       bool
	}{
		{realConnectionKey, providerBigmodel, "individual-coding-plan", "61161790588087632", true},
		{"account-provider:coding-plan:account:zai-individual-coding-plan:account:42:api-key", providerZai, "individual-coding-plan", "42", true},
		{"account-provider:coding-plan:account:bigmodel:account:7:api-key", providerBigmodel, "", "7", true},
		{"account-provider:coding-plan:account:bigmodel-x:api-key", providerBigmodel, "x", "", true},
		{"nothing-here", "", "", "", false},
	}
	for _, tc := range tests {
		provider, kind, userID, ok := splitProviderAccountKey(tc.key)
		if ok != tc.ok {
			t.Errorf("%q: ok = %v, want %v", tc.key, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if provider != tc.provider || kind != tc.kind || userID != tc.userID {
			t.Errorf("%q: got (%q,%q,%q), want (%q,%q,%q)",
				tc.key, provider, kind, userID, tc.provider, tc.kind, tc.userID)
		}
	}
}

// ---------------------------------------------------------------------------
// discovery end to end
// ---------------------------------------------------------------------------

func accountByID(t *testing.T, sources []credentialSource, id string) (Account, bool) {
	t.Helper()
	for _, s := range sources {
		if s.ID == id {
			return s.Account, true
		}
	}
	return Account{}, false
}

// TestDiscoveryCompletesATruncatedProviderKey is the case that made the module
// unusable on a machine where the desktop client had signed in: config.json
// carries only the key id, the secret half lives encrypted in credentials.json,
// and without joining them the discovered account can only ever answer 401.
func TestDiscoveryCompletesATruncatedProviderKey(t *testing.T) {
	home := isolateHome(t)
	pass := ambientPassphrase(t)
	const full = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"

	writeZcodeV2(t, home,
		`{"provider":{"builtin:bigmodel":{`+
			`"enabled":true,"kind":"anthropic","name":"Bigmodel - API Key",`+
			`"options":{"apiKey":"b20b5a93536f417f928524a399a830b6","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key":`+
			jsonString(sealForTest(t, full, pass))+`}`)

	sources := discover(nil)

	cfgAcct, ok := accountByID(t, sources, "zcode-config:builtin:bigmodel")
	if !ok {
		t.Fatal("the provider config entry was not discovered")
	}
	if cfgAcct.secret() != full {
		t.Errorf("provider config secret = %q, want the completed key", cfgAcct.secret())
	}
	if cfgAcct.Mode != modeAPIKey || cfgAcct.BaseURL != baseBigmodelKey {
		t.Errorf("provider config mode/baseURL = %q/%q", cfgAcct.Mode, cfgAcct.BaseURL)
	}
	// The panel must be able to say where the completed key came from.
	for _, s := range sources {
		if s.ID == "zcode-config:builtin:bigmodel" && !strings.Contains(s.Note, "completed") {
			t.Errorf("note does not explain the completion: %q", s.Note)
		}
	}

	connAcct, ok := accountByID(t, sources, "zcode-credentials:bigmodel-individual-coding-plan:61161790588087632")
	if !ok {
		t.Fatal("the encrypted connection entry was not discovered")
	}
	if connAcct.secret() != full {
		t.Errorf("connection secret = %q", connAcct.secret())
	}
	if connAcct.BaseURL != baseBigmodelKey {
		t.Errorf("connection baseURL = %q", connAcct.BaseURL)
	}
}

// TestDiscoveryLeavesATruncatedKeyUncompletedUnderAnotherPassphrase proves the
// module degrades honestly: a value it cannot open is reported as such instead
// of being guessed at, and no account is invented from it.
func TestDiscoveryLeavesATruncatedKeyUncompletedUnderAnotherPassphrase(t *testing.T) {
	home := isolateHome(t)
	pass := ambientPassphrase(t)
	const full = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"
	sealed := sealForTest(t, full, pass)

	t.Setenv(credentialSecretEnv, "a-different-machine")

	writeZcodeV2(t, home,
		`{"provider":{"builtin:bigmodel":{`+
			`"enabled":true,"kind":"anthropic","name":"Bigmodel - API Key",`+
			`"options":{"apiKey":"b20b5a93536f417f928524a399a830b6","baseURL":"https://open.bigmodel.cn/api/anthropic"}}}}`,
		`{"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key":`+
			jsonString(sealed)+`}`)

	sources := discover(nil)

	acct, ok := accountByID(t, sources, "zcode-config:builtin:bigmodel")
	if !ok {
		t.Fatal("the provider config entry was not discovered")
	}
	if acct.secret() != "b20b5a93536f417f928524a399a830b6" {
		t.Errorf("secret = %q, want the truncated id unchanged", acct.secret())
	}
	for _, s := range sources {
		if s.ID == "zcode-config:builtin:bigmodel" && !strings.Contains(s.Note, "not readable") {
			t.Errorf("note does not report the unreadable secret: %q", s.Note)
		}
	}
	if _, ok := accountByID(t, sources, "zcode-credentials:bigmodel-individual-coding-plan:61161790588087632"); ok {
		t.Error("an account was invented from an entry this machine cannot open")
	}
}

func jsonString(s string) string {
	var buf bytes.Buffer
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		default:
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
	return buf.String()
}

// TestDiscoveryIgnoresUnopenableConnectionEntriesWithoutLosingTheRest keeps the
// failure local: one entry this machine cannot read must not stop the others.
func TestDiscoveryIgnoresUnopenableConnectionEntriesWithoutLosingTheRest(t *testing.T) {
	home := isolateHome(t)
	pass := ambientPassphrase(t)

	writeZcodeV2(t, home, "",
		`{`+
			`"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key":`+
			jsonString(sealForTest(t, "good.key", pass))+`,`+
			`"account-provider:coding-plan:account:zai-individual-coding-plan:account:99:api-key":"enc:v1:opaque",`+
			`"zcodejwttoken":`+jsonString(makeJWT(`{"user_id":"u-3"}`))+`}`)

	sources := discover(nil)

	if _, ok := accountByID(t, sources, "zcode-credentials:bigmodel-individual-coding-plan:61161790588087632"); !ok {
		t.Error("the readable connection entry was dropped")
	}
	if _, ok := accountByID(t, sources, "zcode-credentials:zai-individual-coding-plan:99"); ok {
		t.Error("the unopenable connection entry became an account")
	}
	if _, ok := accountByID(t, sources, "zcode-credentials:zcodejwttoken"); !ok {
		t.Error("the plan JWT was dropped")
	}
}

func TestCredentialSecretsIndexesOnlyDecryptablePairs(t *testing.T) {
	dir := t.TempDir()
	pass := ambientPassphrase(t)
	path := dir + string(os.PathSeparator) + "credentials.json"

	body := `{` +
		`"account-provider:coding-plan:account:bigmodel-x:account:1:api-key":` + jsonString(sealForTest(t, "id1.sec1", pass)) + `,` +
		`"account-provider:coding-plan:account:zai-x:account:2:api-key":"enc:v1:opaque",` +
		`"oauth:bigmodel:access_token":"not-a-pair",` +
		`"zcodejwttoken":"a.b.c"` +
		`}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	secrets := credentialSecrets(path, nil)
	if got := secrets.byID["id1"]; got != "id1.sec1" {
		t.Errorf("secrets.byID[id1] = %q", got)
	}
	if len(secrets.byID) != 1 {
		t.Errorf("indexed %d secrets, want 1: %v", len(secrets.byID), secrets.byID)
	}
	// The store's key name spells out the account the key was minted for, and
	// that is the only place an API key's account can be learned from.
	if got := secrets.identity["id1"]; got != "1" {
		t.Errorf("secrets.identity[id1] = %q, want 1", got)
	}
	if len(secrets.identity) != 1 {
		t.Errorf("indexed %d identities, want 1: %v", len(secrets.identity), secrets.identity)
	}
}

func TestCredentialSecretsSurvivesAMissingFile(t *testing.T) {
	if got := credentialSecrets(t.TempDir()+string(os.PathSeparator)+"absent.json", nil); len(got.byID) != 0 {
		t.Errorf("secrets = %v, want empty", got)
	}
}

// TestATombstonedEntryDoesNotSwallowTheCredentialItShares pins the ordering
// between the deleted-account tombstones and the duplicate-secret rule.
//
// The desktop client stores the same key twice: a truncated copy under the
// provider config and the full "id.secret" pair in the credential store.  If a
// tombstoned copy is admitted first it claims the secret, the surviving name is
// then rejected as a duplicate, and the tombstone removes what is left — so the
// module ends up with no usable account at all, even though the operator only
// meant to hide one name.
func TestATombstonedEntryDoesNotSwallowTheCredentialItShares(t *testing.T) {
	home := isolateHome(t)
	const (
		keyID  = "b20b5a93536f417f928524a399a830b6"
		secret = "b20b5a93536f417f928524a399a830b6.QzW3HFQXiadIEm2x"
	)
	encrypted := sealForTest(t, secret, ambientPassphrase(t))

	writeZcodeV2(t, home,
		`{"provider":{
		  "builtin:bigmodel":{"kind":"anthropic","options":{"apiKey":"`+keyID+`","baseURL":"https://open.bigmodel.cn/api/anthropic"}}
		}}`,
		`{"account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:61161790588087632:api-key":`+
			jsonString(encrypted)+`}`)

	dataDir := t.TempDir()
	// The operator deleted the truncated provider-config entry, exactly as the
	// live machine has it.
	state := `{"version":1,"removed":["zcode-config:builtin:bigmodel"]}`
	if err := os.WriteFile(filepath.Join(dataDir, accountsFile), []byte(state), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	p := newPool(dataDir, loadConfig(nil, nil), http.DefaultClient, nil)
	statuses := p.accountsForStatus()

	var (
		gotDeleted bool
		survivor   string
	)
	for _, s := range statuses {
		if s.ID == "zcode-config:builtin:bigmodel" {
			gotDeleted = true
		}
		if strings.HasPrefix(s.ID, "zcode-credentials:bigmodel-") {
			survivor = s.ID
			if s.Extra["mode"] != modeAPIKey {
				t.Errorf("mode = %q, want %q", s.Extra["mode"], modeAPIKey)
			}
		}
	}
	if gotDeleted {
		t.Error("the tombstoned entry came back")
	}
	if survivor == "" {
		t.Fatalf("the surviving name was swallowed by the tombstoned duplicate; accounts = %+v", statuses)
	}
	if got := p.usableCount(); got != 1 {
		t.Fatalf("usableCount = %d, want 1 (accounts = %+v)", got, statuses)
	}
}
