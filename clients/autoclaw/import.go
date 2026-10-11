package autoclaw

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"client2api/internal/core"
)

const (
	autoClawUserDataDirName = "AutoClaw-official"
	credentialsFile         = "account-credentials.enc"
	profileFile             = "account-profile.json"
	deviceFile              = "device/device-id.json"
)

type profileDocument struct {
	SchemaVersion int `json:"schemaVersion"`
	Profile       struct {
		AccountID     string `json:"accountId"`
		DisplayName   any    `json:"displayName"`
		Phone         string `json:"phone"`
		Email         any    `json:"email"`
		FirstLogin    bool   `json:"firstLogin"`
		WebFirstLogin bool   `json:"webFirstLogin"`
		NumericUserID any    `json:"numericUserId"`
		LastLoginAt   int64  `json:"lastLoginAt"`
		CreatedAt     int64  `json:"createdAt"`
		UpdatedAt     any    `json:"updatedAt"`
		IsGuest       bool   `json:"isGuest"`
	} `json:"profile"`
}

type deviceDocument struct {
	SchemaVersion int    `json:"schemaVersion"`
	DeviceID      string `json:"deviceId"`
}

func accountKey(accountID string) string {
	sum := sha256.Sum256([]byte("zwork-account-v1\x00" + strings.TrimSpace(accountID)))
	return hex.EncodeToString(sum[:])
}

func desktopUserDataDirPath() string {
	if v := strings.TrimSpace(os.Getenv(envUserData)); v != "" {
		return v
	}
	appData := strings.TrimSpace(os.Getenv("APPDATA"))
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, autoClawUserDataDirName)
}

func deviceIDFromUserData(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(deviceFile)))
	if err != nil {
		return ""
	}
	var doc deviceDocument
	if err := json.Unmarshal(raw, &doc); err == nil {
		return strings.TrimSpace(doc.DeviceID)
	}
	return ""
}

func desktopCredentialPath(dir, accountID string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	if strings.TrimSpace(accountID) != "" {
		return filepath.Join(dir, "accounts", accountKey(accountID), credentialsFile)
	}
	return ""
}

func localStatePathForCredential(path string) string {
	// accounts/<key>/account-credentials.enc -> <userData>/Local State
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "Local State")
}

type localStateEnvelope struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

type desktopCredentialDocument struct {
	SchemaVersion int    `json:"schemaVersion"`
	Revision      int    `json:"revision"`
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	UpdatedAt     any    `json:"updatedAt"`
}

func parseDesktopCredential(raw []byte) (credential, error) {
	var doc desktopCredentialDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return credential{}, fmt.Errorf("autoclaw: decoding desktop credential: %w", err)
	}
	if strings.TrimSpace(doc.AccessToken) == "" {
		return credential{}, errors.New("autoclaw: desktop credential has no accessToken")
	}
	return credential{
		AccessToken:  strings.TrimSpace(doc.AccessToken),
		RefreshToken: strings.TrimSpace(doc.RefreshToken),
	}, nil
}

func deriveSafeStorageKey(state []byte, unprotect func([]byte) ([]byte, error)) ([]byte, error) {
	var doc localStateEnvelope
	if err := json.Unmarshal(state, &doc); err != nil {
		return nil, fmt.Errorf("autoclaw: reading Local State: %w", err)
	}
	encoded := strings.TrimSpace(doc.OSCrypt.EncryptedKey)
	if encoded == "" {
		return nil, errors.New("autoclaw: Local State has no os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: decoding os_crypt.encrypted_key: %w", err)
	}
	if len(blob) <= len("DPAPI") || string(blob[:len("DPAPI")]) != "DPAPI" {
		return nil, errors.New("autoclaw: safeStorage key is not a Windows DPAPI blob")
	}
	key, err := unprotect(blob[len("DPAPI"):])
	if err != nil {
		return nil, fmt.Errorf("autoclaw: unwrapping safeStorage key with DPAPI: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("autoclaw: safeStorage key is %d bytes, want 32", len(key))
	}
	return key, nil
}

func decryptSafeStorageBlob(blob, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("autoclaw: safeStorage key is %d bytes, want 32", len(key))
	}
	if len(blob) < 3+12+16 || string(blob[:3]) != "v10" {
		return nil, errors.New("autoclaw: unsupported Electron safeStorage envelope")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: safeStorage cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: safeStorage GCM: %w", err)
	}
	plain, err := gcm.Open(nil, blob[3:15], blob[15:], nil)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: opening safeStorage blob: %w", err)
	}
	return plain, nil
}

func readDesktopCredential(path string) ([]byte, error) {
	if !safeStorageSupported() {
		return nil, errors.New("autoclaw: desktop credential decryption is supported only on Windows")
	}
	state, err := os.ReadFile(localStatePathForCredential(path))
	if err != nil {
		return nil, fmt.Errorf("autoclaw: reading Local State: %w", err)
	}
	key, err := deriveSafeStorageKey(state, unprotectDPAPI)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: reading credential: %w", err)
	}
	return decryptSafeStorageBlob(blob, key)
}

func (c *Client) discoverLocalCredentials() []core.DiscoveredCredential {
	dir := desktopUserDataDirPath()
	if dir == "" {
		return nil
	}
	accountsDir := filepath.Join(dir, "accounts")
	entries, err := os.ReadDir(accountsDir)
	if err != nil {
		return nil
	}
	deviceID := deviceIDFromUserData(dir)
	var out []core.DiscoveredCredential
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		base := filepath.Join(accountsDir, e.Name())
		credPath := filepath.Join(base, credentialsFile)
		if _, err := os.Stat(credPath); err != nil {
			continue
		}
		label := e.Name()[:min(12, len(e.Name()))]
		if raw, err := os.ReadFile(filepath.Join(base, profileFile)); err == nil {
			var doc profileDocument
			if json.Unmarshal(raw, &doc) == nil {
				if s := documentString(doc.Profile.Phone); s != "" {
					label = maskPhone(s)
				} else if s := documentString(doc.Profile.AccountID); s != "" {
					label = s
				}
			}
		}
		out = append(out, core.DiscoveredCredential{
			Path:       credPath,
			Kind:       "autoclaw-desktop",
			Label:      label,
			Note:       "AutoClaw 桌面客户端（Electron safeStorage + DPAPI）",
			Importable: safeStorageSupported(),
			Imported:   importedCredential(c.pool, e.Name(), deviceID),
		})
	}
	return out
}

func importedCredential(p *pool, acctKey, deviceID string) bool {
	if p == nil {
		return false
	}
	wantDevice := strings.TrimSpace(deviceID)
	wantKey := strings.TrimSpace(acctKey)
	for _, e := range p.all() {
		if wantDevice != "" && !strings.EqualFold(strings.TrimSpace(e.acct.DeviceID), wantDevice) {
			continue
		}
		if strings.TrimSpace(e.acct.AccountID) == "" {
			continue
		}
		if accountKey(e.acct.AccountID) == wantKey {
			return true
		}
	}
	return false
}

func documentString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return fmt.Sprintf("%.0f", t)
	case json.Number:
		return t.String()
	}
	return ""
}

func (c *Client) readLocalCredential(path string) (credential, error) {
	plain, err := readDesktopCredential(path)
	if err != nil {
		return credential{}, err
	}
	cred, err := parseDesktopCredential(plain)
	if err != nil {
		return credential{}, err
	}
	base := filepath.Dir(path)
	cred.DeviceID = deviceIDFromUserData(filepath.Dir(filepath.Dir(base)))
	if raw, err := os.ReadFile(filepath.Join(base, profileFile)); err == nil {
		var doc profileDocument
		if json.Unmarshal(raw, &doc) == nil {
			cred.AccountID = strings.TrimSpace(doc.Profile.AccountID)
			cred.Nickname = documentString(doc.Profile.DisplayName)
			cred.Phone = strings.TrimSpace(doc.Profile.Phone)
			cred.NumericUserID = documentString(doc.Profile.NumericUserID)
		}
	}
	if exp := jwtExpiryMs(cred.AccessToken); exp > 0 {
		cred.ExpiresAt = flexString(strconv.FormatInt(exp, 10))
	}
	return cred, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	out := []core.DiscoveredCredential{}
	if c.cfg.autoDiscover() {
		out = append(out, c.discoverLocalCredentials()...)
	}
	for _, p := range c.cfg.credentialPaths() {
		out = append(out, core.DiscoveredCredential{Path: p, Kind: "autoclaw-credential", Importable: true})
	}
	return out, nil
}

func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	targets := paths
	if all || len(targets) == 0 {
		discovered, _ := c.Discover(ctx)
		targets = targets[:0]
		for _, d := range discovered {
			if d.Importable {
				targets = append(targets, d.Path)
			}
		}
	}
	if len(targets) == 0 {
		return nil, errors.New("autoclaw: no importable credentials found")
	}
	var out []core.AccountRecord
	var failures []string
	for _, p := range targets {
		var cred credential
		var err error
		if strings.HasSuffix(strings.ToLower(p), ".enc") {
			cred, err = c.readLocalCredential(p)
		} else {
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				err = rerr
			} else {
				cred, err = parseCredentialBundle(raw)
			}
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		e := c.pool.put(account{credential: cred, Label: cred.displayName(), Origin: originImport})
		out = append(out, c.pool.record(e.acct.id()))
	}
	if len(out) == 0 && len(failures) > 0 {
		return nil, errors.New(strings.Join(failures, "; "))
	}
	return out, nil
}

func parseCredentialBundle(raw []byte) (credential, error) {
	var single credential
	if err := json.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single.AccessToken) != "" {
		return single, nil
	}
	// Operator-exported credential documents may use the desktop client's
	// camelCase names.
	var doc desktopCredentialDocument
	if err := json.Unmarshal(raw, &doc); err == nil && strings.TrimSpace(doc.AccessToken) != "" {
		return credential{AccessToken: strings.TrimSpace(doc.AccessToken), RefreshToken: strings.TrimSpace(doc.RefreshToken)}, nil
	}
	return credential{}, errors.New("autoclaw: no access_token found")
}

// ImportBundle implements core.BundleImporter, so the panel can upload a JSON
// credential document instead of pasting one field at a time.
//
// Accepted shapes:
//
//	{"access_token":"...","refresh_token":"..."}
//	{"accessToken":"...","refreshToken":"..."}          (desktop noun names)
//	{"accounts":[ ...one of the above... ]}             (a batch)
//
// One bad entry does not discard the good ones: the per-entry failures are
// reported in the report, and a non-nil error means the document itself could
// not be read at all.
func (c *Client) ImportBundle(ctx context.Context, name string, data []byte) (core.BundleImportReport, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return core.BundleImportReport{}, errors.New("autoclaw: empty bundle")
	}
	entries, err := bundleEntries(trimmed)
	if err != nil {
		return core.BundleImportReport{}, err
	}
	if len(entries) == 0 {
		return core.BundleImportReport{}, errors.New("autoclaw: no credential entries in bundle")
	}
	deviceID := strings.TrimSpace(deviceIDFromUserData(desktopUserDataDirPath()))
	report := core.BundleImportReport{Total: len(entries)}
	for i, raw := range entries {
		cred, cerr := parseCredentialBundle(raw)
		if cerr != nil {
			report.Skipped++
			report.Errors = append(report.Errors, fmt.Sprintf("entry %d: %v", i+1, cerr))
			continue
		}
		if cred.DeviceID == "" {
			cred.DeviceID = deviceID
		}
		if ms := jwtExpiryMs(cred.AccessToken); ms > 0 && strings.TrimSpace(cred.ExpiresAt.String()) == "" {
			cred.ExpiresAt = flexString(strconv.FormatInt(ms, 10))
		}

		c.pool.put(account{credential: cred, Label: cred.displayName(), Origin: originImport})
		report.Imported++
	}
	if report.Imported == 0 && len(report.Errors) > 0 {
		return report, errors.New("autoclaw: no usable credential in bundle: " + strings.Join(report.Errors, "; "))
	}
	return report, nil
}

func bundleEntries(raw []byte) ([][]byte, error) {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("autoclaw: bundle is not JSON: %w", err)
	}
	switch v := probe.(type) {
	case []any:
		out := make([][]byte, 0, len(v))
		for _, item := range v {
			if b, err := json.Marshal(item); err == nil {
				out = append(out, b)
			}
		}
		return out, nil
	case map[string]any:
		if list, ok := v["accounts"]; ok {
			if arr, ok := list.([]any); ok {
				out := make([][]byte, 0, len(arr))
				for _, item := range arr {
					if b, err := json.Marshal(item); err == nil {
						out = append(out, b)
					}
				}
				return out, nil
			}
		}
		return [][]byte{raw}, nil
	default:
		return nil, errors.New("autoclaw: unsupported bundle shape")
	}
}
