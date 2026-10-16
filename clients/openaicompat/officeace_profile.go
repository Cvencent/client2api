package openaicompat

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OfficeAce (Office Claw) signs in with a Huawei account; the service mints the
// model-gateway application key/secret pair as part of that login and the
// desktop client keeps it in its own encrypted profile store.  The panel's
// browser sign-in mints the pair directly; this file is the Windows fallback
// that adopts an already signed-in desktop client without asking the operator
// to copy secrets out of the app.
//
// Layout, relative to the client's working directory (packages/api):
//
//	.config/secure-config-nodejs/.oauth-profile-encryption-key
//	.config/secure-config-nodejs/oauth-<urlencoded userId>.json
//
// The key file holds "OC-DPAPI-1\n<base64 DPAPI blob>" once the client has
// upgraded it; an older install may still hold the plaintext secret.  Each
// profile is AES-256-GCM JSON whose key is SHA-256 of that secret.  The
// decrypted payload carries modelInfo.model_api_url_base and
// modelInfo.model_auth_info.model_app_key / model_app_secret -- exactly the
// pair this relay sends as HTTP Basic.
const (
	officeAceSecureDirEnv = "OFFICE_CLAW_SECURE_CONFIG_DIR"
	officeAceInstallEnv   = "OFFICEACE_HOME"
	officeAceSecureDirRel = ".config/secure-config-nodejs"
	officeAceKeyFileName  = ".oauth-profile-encryption-key"
	officeAceDPAPIMagic   = "OC-DPAPI-1"
	officeAceFilePrefix   = "oauth-"
	officeAcePendingMark  = "oauth-pending-"
	officeAceFileSuffix   = ".json"
)

// officeAceCredential is one usable OfficeAce login: the application key pair
// the model gateway authenticates with, plus the endpoint and identity the
// client recorded for it.
type officeAceCredential struct {
	SourcePath string
	UserID     string
	UserName   string
	AccountID  string
	BaseURL    string
	AppKey     string
	AppSecret  string
	// Session is the cloud credential the same login minted, when the profile
	// carries one.  It is what lets an imported account sign the premium
	// routes, not just chat.
	Session *officeAceStoredSession
}

func (c officeAceCredential) usable() bool {
	return strings.TrimSpace(c.AppKey) != "" && strings.TrimSpace(c.AppSecret) != ""
}

func (c officeAceCredential) label() string {
	switch {
	case strings.TrimSpace(c.UserName) != "":
		return strings.TrimSpace(c.UserName)
	case strings.TrimSpace(c.AccountID) != "":
		return strings.TrimSpace(c.AccountID)
	case strings.TrimSpace(c.UserID) != "":
		return truncate(strings.TrimSpace(c.UserID), 40)
	}
	return "OfficeAce account"
}

// accountID is stable across re-imports of the same OfficeAce login, so a
// refreshed key updates the existing row instead of piling up duplicates.
func (c officeAceCredential) accountID() string {
	seed := firstNonEmpty(strings.TrimSpace(c.UserID), strings.TrimSpace(c.AccountID), strings.TrimSpace(c.SourcePath))
	return officeAceAccountID(seed)
}

// officeAceAccountID derives the stable local account id from whatever
// identity the login flow has. Keeping the rule in one helper makes a panel
// login and a later local-profile import update the same account row.
func officeAceAccountID(seed string) string {
	seed = strings.TrimSpace(seed)
	sum := sha256.Sum256([]byte("officeace-account-v1\x00" + seed))
	return "oa-" + hex.EncodeToString(sum[:10])
}

type officeAceProfileFile struct {
	path   string
	userID string
}

type officeAceCandidate struct {
	path string
	cred officeAceCredential
	err  error
}

// officeAceProfileFiles lists the stored logins in one secure-config
// directory.  Pending logins ("oauth-pending-*") are half-finished flows, not
// credentials, and are skipped.
func officeAceProfileFiles(dir string) []officeAceProfileFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []officeAceProfileFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, officeAceFilePrefix) || !strings.HasSuffix(lower, officeAceFileSuffix) {
			continue
		}
		if strings.HasPrefix(lower, officeAcePendingMark) {
			continue
		}
		raw := name[len(officeAceFilePrefix) : len(name)-len(officeAceFileSuffix)]
		out = append(out, officeAceProfileFile{path: filepath.Join(dir, name), userID: officeAceDecodeUserID(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

func officeAceDecodeUserID(raw string) string {
	if decoded, err := url.PathUnescape(raw); err == nil && strings.TrimSpace(decoded) != "" {
		return strings.TrimSpace(decoded)
	}
	return strings.TrimSpace(raw)
}

// parseOfficeAceInstallSecret accepts the DPAPI envelope the current client
// writes and the plaintext secret older installs left behind.  unprotect is a
// parameter so the envelope handling is testable without touching DPAPI.
func parseOfficeAceInstallSecret(raw []byte, unprotect func([]byte) ([]byte, error)) (string, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "", errors.New("officeace: install secret file is empty")
	}
	if !strings.HasPrefix(text, officeAceDPAPIMagic) {
		return text, nil
	}
	blob := strings.TrimSpace(strings.TrimPrefix(text, officeAceDPAPIMagic))
	if blob == "" {
		return "", errors.New("officeace: DPAPI envelope has no payload")
	}
	enc, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("officeace: decoding DPAPI envelope: %w", err)
	}
	plain, err := unprotect(enc)
	if err != nil {
		return "", fmt.Errorf("officeace: unwrapping install secret: %w", err)
	}
	secret := strings.TrimSpace(string(plain))
	if secret == "" {
		return "", errors.New("officeace: DPAPI payload is empty")
	}
	return secret, nil
}

// officeAceProfileKey derives the AES key the client uses: SHA-256 over the
// install secret, used as a 32-byte key rather than as text.
func officeAceProfileKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

type officeAceEnvelope struct {
	Version int    `json:"version"`
	Alg     string `json:"alg"`
	IV      string `json:"iv"`
	Tag     string `json:"tag"`
	Data    string `json:"data"`
}

func decryptOfficeAceProfile(raw, key []byte) ([]byte, error) {
	var env officeAceEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("officeace: decoding profile envelope: %w", err)
	}
	if env.Version != 1 || !strings.EqualFold(strings.TrimSpace(env.Alg), "aes-256-gcm") {
		return nil, fmt.Errorf("officeace: unsupported profile encryption (version %d, alg %q)", env.Version, env.Alg)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("officeace: profile key is %d bytes, want 32", len(key))
	}
	iv, err := decodeOfficeAceBase64(env.IV)
	if err != nil {
		return nil, fmt.Errorf("officeace: decoding profile iv: %w", err)
	}
	tag, err := decodeOfficeAceBase64(env.Tag)
	if err != nil {
		return nil, fmt.Errorf("officeace: decoding profile tag: %w", err)
	}
	data, err := decodeOfficeAceBase64(env.Data)
	if err != nil {
		return nil, fmt.Errorf("officeace: decoding profile data: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("officeace: profile cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("officeace: profile GCM: %w", err)
	}
	sealed := append(append([]byte(nil), data...), tag...)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("officeace: opening profile: %w", err)
	}
	return plain, nil
}

// decodeOfficeAceBase64 reads Node's base64url output, tolerating padding.
func decodeOfficeAceBase64(raw string) ([]byte, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "=")
	if s == "" {
		return nil, errors.New("empty value")
	}
	return base64.RawURLEncoding.DecodeString(s)
}

type officeAceProfileDocument struct {
	UserName  string `json:"userName"`
	AccountID string `json:"accountId"`
	UserID    string `json:"user_id"`
	// principalUrn is the "iam::<account>:user:<name>" identity the client
	// stores after the permission check.
	PrincipalURN string `json:"principalUrn"`
	ModelInfo struct {
		APIURLBase string `json:"model_api_url_base"`
		AuthInfo   struct {
			AppKey    string `json:"model_app_key"`
			AppSecret string `json:"model_app_secret"`
		} `json:"model_auth_info"`
	} `json:"modelInfo"`
	Credential   *officeAceProfileCredential `json:"credential"`
	CredentialV3 *officeAceProfileCredential `json:"credentialV3"`
	DPoP         *struct {
		PrivateJWK *struct {
			D string `json:"d"`
		} `json:"privateJwk"`
	} `json:"dpop"`
}

// officeAceProfileCredential is one temporary credential the client stored.
// The v5 form keeps the refresh token; the v3 form does not.
type officeAceProfileCredential struct {
	Access           string `json:"access"`
	Secret           string `json:"secret"`
	STSToken         string `json:"sts_token"`
	ProjectID        string `json:"project_id"`
	ExpiresAt        string `json:"expires_at"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresAt string `json:"refresh_expires_at"`
}

func parseOfficeAceCredential(plain []byte, path, fileUserID string) (officeAceCredential, error) {
	var doc officeAceProfileDocument
	if err := json.Unmarshal(plain, &doc); err != nil {
		return officeAceCredential{}, fmt.Errorf("officeace: decoding profile: %w", err)
	}
	identity := firstNonEmpty(strings.TrimSpace(fileUserID), strings.TrimSpace(doc.UserID), strings.TrimSpace(doc.AccountID))
	cred := officeAceCredential{
		SourcePath: path,
		UserID:     identity,
		UserName:   strings.TrimSpace(doc.UserName),
		AccountID:  strings.TrimSpace(doc.AccountID),
		BaseURL:    normalizeOfficeAceBaseURL(doc.ModelInfo.APIURLBase),
		AppKey:     strings.TrimSpace(doc.ModelInfo.AuthInfo.AppKey),
		AppSecret:  strings.TrimSpace(doc.ModelInfo.AuthInfo.AppSecret),
		Session:    officeAceSessionFromProfile(doc),
	}
	return cred, nil
}

// officeAceSessionFromProfile lifts the temporary credential out of a stored
// profile.  A profile written before the client kept them, or one whose
// credential has no usable fields, yields nil so the account still imports for
// chat.
func officeAceSessionFromProfile(doc officeAceProfileDocument) *officeAceStoredSession {
	if doc.Credential == nil {
		return nil
	}
	sess := &officeAceStoredSession{
		Region:           officeAceRegionFromBaseURL(doc.ModelInfo.APIURLBase),
		AccessKey:        strings.TrimSpace(doc.Credential.Access),
		SecretKey:        strings.TrimSpace(doc.Credential.Secret),
		SecurityToken:    strings.TrimSpace(doc.Credential.STSToken),
		ProjectID:        strings.TrimSpace(doc.Credential.ProjectID),
		ExpiresAt:        strings.TrimSpace(doc.Credential.ExpiresAt),
		RefreshToken:     strings.TrimSpace(doc.Credential.RefreshToken),
		RefreshExpiresAt: strings.TrimSpace(doc.Credential.RefreshExpiresAt),
		AccountID:        strings.TrimSpace(doc.AccountID),
		UserName:         strings.TrimSpace(doc.UserName),
	}
	if doc.CredentialV3 != nil {
		sess.V3AccessKey = strings.TrimSpace(doc.CredentialV3.Access)
		sess.V3SecretKey = strings.TrimSpace(doc.CredentialV3.Secret)
		sess.V3SecurityToken = strings.TrimSpace(doc.CredentialV3.STSToken)
		sess.V3ProjectID = strings.TrimSpace(doc.CredentialV3.ProjectID)
		sess.V3ExpiresAt = strings.TrimSpace(doc.CredentialV3.ExpiresAt)
	}
	if doc.DPoP != nil && doc.DPoP.PrivateJWK != nil {
		sess.DPoPScalar = strings.TrimSpace(doc.DPoP.PrivateJWK.D)
	}
	if !sess.usable() {
		return nil
	}
	return sess
}

// normalizeOfficeAceBaseURL mirrors the desktop client's own normalisation:
// default the scheme to https and make sure the OpenAI root ends in /v2.
func normalizeOfficeAceBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	raw = strings.TrimRight(raw, "/")
	if !strings.HasSuffix(strings.ToLower(raw), "/v2") {
		raw += "/v2"
	}
	return raw
}
