package qoder

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
)

// credential.go implements core.CredentialImporter: it reads the device token
// the Qoder CN desktop client already has, so the operator does not have to copy
// it by hand.
//
// The blob is an Electron safeStorage secret (AES-256-GCM) whose key is itself
// protected with Windows DPAPI.  credential_windows.go unwraps it; on any other
// platform the importer reports that it cannot, and the pasted-token path keeps
// working.

// desktopCredential is the decrypted auth.v1.dat payload.
type desktopCredential struct {
	SchemaVersion         int    `json:"schemaVersion"`
	Token                 string `json:"token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresAt             string `json:"expiresAt"`
	RefreshTokenExpiresAt string `json:"refreshTokenExpiresAt"`
	User                  struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"user"`
}

// desktopUserDataDirPath resolves the Electron user-data directory.  The env
// override exists for a non-default install layout; the default matches the
// Qoder CN desktop build this module was written against.
//
// realm selects which build's directory to read: the two builds keep separate
// credential stores under separate names, and both can be installed at once
// (which is the point of the realm split).
func desktopUserDataDirPathFor(realm string) string {
	if normalizeRealm(realm) == realmGlobal {
		if v := strings.TrimSpace(os.Getenv("CLIENT2API_QODER_GLOBAL_USER_DATA_DIR")); v != "" {
			return v
		}
		appData := strings.TrimSpace(os.Getenv("APPDATA"))
		if appData == "" {
			return ""
		}
		return filepath.Join(appData, desktopUserDataDirGlobal)
	}
	if v := strings.TrimSpace(os.Getenv("CLIENT2API_QODER_USER_DATA_DIR")); v != "" {
		return v
	}
	appData := strings.TrimSpace(os.Getenv("APPDATA"))
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, desktopUserDataDir)
}

// desktopUserDataDirPath resolves the domestic build's directory.
func desktopUserDataDirPath() string {
	return desktopUserDataDirPathFor(realmCN)
}

// localCredentialPath is the encrypted blob inside the domestic directory.
func localCredentialPath() string {
	return localCredentialPathFor(realmCN)
}

// localCredentialPathFor is the encrypted blob inside one realm's directory.
func localCredentialPathFor(realm string) string {
	dir := desktopUserDataDirPathFor(realm)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, desktopAuthFile)
}

type localStateEnvelope struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

// Discover lists the local Qoder credentials when there are any: the mainland
// build's blob and the international build's blob are two separate files in two
// separate directories, and an operator may have both installed.  It is
// read-only: the blob is not decrypted here.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	var out []core.DiscoveredCredential
	for _, realm := range []string{realmCN, realmGlobal} {
		path := localCredentialPathFor(realm)
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("qoder: reading %s: %w", path, err)
		}

		name := "Qoder CN"
		if realm == realmGlobal {
			name = "Qoder（国际版）"
		}
		note := "读取 " + name + " 客户端保存的设备令牌（Electron safeStorage + DPAPI）"
		if !safeStorageSupported() {
			note = "本平台无法解密 " + name + " 的凭据（只支持 Windows），请改用手动粘贴令牌"
		}

		imported := false
		if c.store != nil {
			imported = c.hasImportedPath(path)
		}
		out = append(out, core.DiscoveredCredential{
			Path:       path,
			Kind:       desktopAuthFile,
			Label:      name + " 客户端（" + time.Unix(info.ModTime().Unix(), 0).Local().Format("2006-01-02 15:04") + " 更新）",
			Note:       note,
			Importable: safeStorageSupported(),
			Imported:   imported,
		})
	}
	return out, nil
}

// realmForCredentialPath reports which realm a discovered blob belongs to, by
// matching it against the two builds' directories.  It is how Import knows which
// host to probe the token against without re-deriving the path layout.
func realmForCredentialPath(path string) string {
	target := filepath.Clean(strings.TrimSpace(path))
	if target == "" || target == "." {
		return realmCN
	}
	for _, realm := range []string{realmCN, realmGlobal} {
		dir := desktopUserDataDirPathFor(realm)
		if dir == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(dir), filepath.Dir(target)) {
			return realm
		}
	}
	return realmCN
}

// hasImportedPath reports whether an account came from this file.  The store
// does not record provenance, so the honest answer is "no" once an account with
// the same vendor identity exists; that is what stops the panel from offering a
// second import of the same login.
func (c *Client) hasImportedPath(path string) bool {
	target := filepath.Clean(strings.TrimSpace(path))
	if strings.TrimSpace(path) == "" || target == "." {
		return false
	}
	for _, acc := range c.store.snapshot() {
		imported := filepath.Clean(strings.TrimSpace(acc.ImportPath))
		if imported != "." && strings.EqualFold(imported, target) {
			return true
		}
	}
	return false
}

// Import reads one or every discovered blob and stores what it finds.
//
// Unlike AddAccount this does NOT refuse a token the vendor rejects: the blob is
// the desktop client's own credential, so a rejection is reported on the account
// row (and in the returned error) instead of blocking the import.  A rejected
// snapshot is still the operator's evidence of what is in the file.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	discovered, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]core.DiscoveredCredential, len(discovered))
	for _, d := range discovered {
		allowed[d.Path] = d
	}

	var targets []string
	if all {
		for _, d := range discovered {
			if d.Importable {
				targets = append(targets, d.Path)
			}
		}
	} else {
		for _, p := range paths {
			if _, ok := allowed[p]; ok {
				targets = append(targets, p)
			}
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}

	out := make([]core.AccountRecord, 0, len(targets))
	var failures []string
	for _, path := range targets {
		record, importErr := c.importOne(ctx, path)
		if importErr != nil {
			failures = append(failures, importErr.Error())
			continue
		}
		out = append(out, record)
	}
	if len(out) == 0 && len(failures) > 0 {
		return nil, errors.New(strings.Join(failures, "; "))
	}
	for _, f := range failures {
		c.logf("qoder: import: %s", f)
	}
	return out, nil
}

// importOne decrypts one blob and stores the credential in it.
func (c *Client) importOne(ctx context.Context, path string) (core.AccountRecord, error) {
	realm := realmForCredentialPath(path)
	raw, err := readDesktopCredential(path)
	if err != nil {
		return core.AccountRecord{}, fmt.Errorf("qoder: %s: %w", path, err)
	}
	var payload desktopCredential
	if err := json.Unmarshal(raw, &payload); err != nil {
		return core.AccountRecord{}, fmt.Errorf("qoder: %s is not a Qoder credential: %w", path, err)
	}
	token := strings.TrimSpace(payload.Token)
	if token == "" {
		return core.AccountRecord{}, fmt.Errorf("qoder: %s holds no device token", path)
	}

	buildName := "Qoder CN"
	if realm == realmGlobal {
		buildName = "Qoder（国际版）"
	}
	acc := account{
		storedAccount: storedAccount{
			ImportPath:         path,
			Token:              token,
			RefreshToken:       strings.TrimSpace(payload.RefreshToken),
			UserID:             strings.TrimSpace(payload.User.ID),
			Phone:              strings.TrimSpace(firstNonEmpty(payload.User.Phone, "")),
			Name:               strings.TrimSpace(payload.User.Name),
			ExpiresAtMS:        parseExpiryMS(payload.ExpiresAt),
			RefreshExpiresAtMS: parseExpiryMS(payload.RefreshTokenExpiresAt),
			Enabled:            true,
			Realm:              realm,
		},
		origin: originStored,
	}
	if acc.ID == "" {
		acc.ID = defaultAccountID(acc)
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	info, probeErr := c.up.userInfoFor(probeCtx, realm, acc.Token)
	cancel()
	switch {
	case probeErr == nil:
		acc = mergeAccount(acc, info)
		acc.Realm = realm
	case failureKind(probeErr) == core.FailureAuth:
		acc.lastError = "厂商拒绝了这个令牌（" + redactErr(probeErr) + "），可能需要在 " + buildName + " 客户端重新登录后再导入"
	default:
		acc.lastError = redactErr(probeErr)
	}
	if acc.ID == "" {
		acc.ID = defaultAccountID(acc)
	}
	if acc.Realm == "" {
		acc.Realm = realm
	}
	if acc.UserID != "" {
		// A vendor identity is the stable key, so re-importing after a re-login
		// replaces the same account instead of adding a second row.
		acc.ID = "qoder-" + acc.UserID
	}
	if acc.Label == "" {
		acc.Label = defaultAccountLabel(acc)
	}

	if err := c.store.put(acc); err != nil {
		return core.AccountRecord{}, err
	}
	stored, _ := c.store.lookup(acc.ID)
	record := accountRecord(&stored, time.Now().UTC())
	if probeErr != nil {
		c.logf("qoder: imported %s but could not verify it: %s", path, redactErr(probeErr))
		record.Note = strings.TrimSpace(record.Note + " " + stored.lastError)
	}
	return record, nil
}

func safeStorageKeyFromLocalState(payload []byte, unprotect func([]byte) ([]byte, error)) ([]byte, error) {
	var state localStateEnvelope
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, fmt.Errorf("qoder: reading Chromium Local State: %w", err)
	}
	encoded := strings.TrimSpace(state.OSCrypt.EncryptedKey)
	if encoded == "" {
		return nil, errors.New("qoder: Local State has no os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("qoder: decoding os_crypt.encrypted_key: %w", err)
	}
	if len(blob) <= len("DPAPI") || string(blob[:len("DPAPI")]) != "DPAPI" {
		return nil, errors.New("qoder: safeStorage key is not a Windows DPAPI blob")
	}
	key, err := unprotect(blob[len("DPAPI"):])
	if err != nil {
		return nil, fmt.Errorf("qoder: unwrapping safeStorage key with DPAPI: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("qoder: safeStorage key is %d bytes, want 32", len(key))
	}
	return key, nil
}

func decryptSafeStorageBlob(blob, key []byte) ([]byte, error) {
	const (
		prefix    = "v10"
		nonceSize = 12
		tagSize   = 16
	)
	if len(key) != 32 {
		return nil, fmt.Errorf("qoder: safeStorage key is %d bytes, want 32", len(key))
	}
	if len(blob) < len(prefix)+nonceSize+tagSize || string(blob[:len(prefix)]) != prefix {
		return nil, errors.New("qoder: unsupported Electron safeStorage envelope")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("qoder: safeStorage cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("qoder: safeStorage GCM: %w", err)
	}
	nonce := blob[len(prefix) : len(prefix)+nonceSize]
	ciphertext := blob[len(prefix)+nonceSize:]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("qoder: opening safeStorage blob: %w", err)
	}
	return plain, nil
}

func readDesktopCredential(path string) ([]byte, error) {
	if !safeStorageSupported() {
		return nil, errors.New("qoder: desktop credential decryption is supported only on Windows")
	}
	statePath := filepath.Join(filepath.Dir(path), "Local State")
	state, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("qoder: reading %s: %w", statePath, err)
	}
	key, err := safeStorageKeyFromLocalState(state, unprotectDPAPI)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("qoder: reading %s: %w", path, err)
	}
	return decryptSafeStorageBlob(blob, key)
}
