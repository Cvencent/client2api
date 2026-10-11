package doubao

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
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// clientimport.go reads the session out of the Doubao *desktop client* rather
// than driving a browser sign-in.  The client is a Chromium shell, so its
// signed-in session lives in the ordinary Chromium places: the AES key in
// <user-data>/Local State (DPAPI-wrapped) and the cookies in
// <user-data>/<profile>/Network/Cookies.
//
// Chromium 127+ seals cookie values with an "app-bound" key: the plaintext
// carries a 32-byte binary header before the cookie text.  The header is
// metadata the browser strips when it sends the cookie, so it must be stripped
// here too or the replayed session is rejected as "invalid user".
//
// Reading is strictly read-only: the database is opened read-only, so a running
// client is never disturbed and nothing on disk is modified.

// dpapiPrefix is the ASCII tag Chromium prepends to the Local State key.
const dpapiPrefix = "DPAPI"

// appBoundHeaderSize is the binary header Chromium 127+ prepends to a cookie
// value inside the AES-GCM plaintext.
const appBoundHeaderSize = 32

// localStateEnvelope is the slice of Chromium's Local State this module reads.
type localStateEnvelope struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

// cookieRow is one decrypted cookie.
type cookieRow struct {
	name  string
	value string
}

// clientSession is one signed-in desktop profile, reduced to the cookie header
// the web API expects.
type clientSession struct {
	profileDir string
	userData   string
	cookie     string
}

// Discover implements core.CredentialImporter.  It lists the signed-in Doubao
// desktop profiles it can read, without consuming any credential.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	dir := c.cfg.clientUserDataDir()
	if dir == "" {
		return nil, nil
	}
	if !clientImportSupported() {
		return []core.DiscoveredCredential{{
			Path:       dir,
			Kind:       "doubao-desktop",
			Label:      "豆包桌面客户端",
			Note:       "本平台无法读取豆包客户端的 Cookie（仅支持 Windows），请改用网页登录",
			Importable: false,
		}}, nil
	}

	profiles, err := c.desktopProfiles(dir)
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	out := make([]core.DiscoveredCredential, 0, len(profiles))
	for _, p := range profiles {
		imported := c.hasClientProfile(p.dir)
		note := "读取已登录的豆包桌面客户端会话（" + p.describe() + "）"
		out = append(out, core.DiscoveredCredential{
			Path:       p.dir,
			Kind:       "doubao-desktop",
			Label:      "豆包桌面客户端 · " + p.name,
			Note:       note,
			Importable: p.signedIn,
			Imported:   imported,
		})
	}
	return out, nil
}

// Import implements core.CredentialImporter.  all imports every readable
// profile; otherwise only the named profile directories are imported.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	discovered, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]core.DiscoveredCredential, len(discovered))
	for _, d := range discovered {
		allowed[strings.ToLower(filepath.Clean(d.Path))] = d
	}

	var targets []core.DiscoveredCredential
	if all {
		for _, d := range discovered {
			if d.Importable {
				targets = append(targets, d)
			}
		}
	} else {
		for _, p := range paths {
			if d, ok := allowed[strings.ToLower(filepath.Clean(p))]; ok && d.Importable {
				targets = append(targets, d)
			}
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}

	out := make([]core.AccountRecord, 0, len(targets))
	var failures []string
	for _, d := range targets {
		rec, importErr := c.importClientProfile(ctx, d.Path)
		if importErr != nil {
			failures = append(failures, importErr.Error())
			continue
		}
		out = append(out, rec)
	}
	if len(out) == 0 && len(failures) > 0 {
		return nil, errors.New(strings.Join(failures, "; "))
	}
	for _, f := range failures {
		if c.deps.Logf != nil {
			c.deps.Logf("doubao: client import: %s", f)
		}
	}
	return out, nil
}

// importClientProfile reads one desktop profile and stores its session.
func (c *Client) importClientProfile(ctx context.Context, profileDir string) (core.AccountRecord, error) {
	session, err := c.readClientSession(profileDir)
	if err != nil {
		return core.AccountRecord{}, err
	}
	acc := &account{
		Cookie:  session.cookie,
		Label:   "豆包客户端 " + filepath.Base(profileDir),
		Enabled: true,
		Source:  "client",
	}
	// Verifying here keeps the label honest: the toast says which signed-in
	// account the row is, not merely that a cookie was found.
	info, verr := c.up.accountInfo(ctx, acc)
	if verr == nil {
		acc.Label = "豆包客户端 · " + info.displayName()
		acc.Identity = info.identity()
	}
	rec, aerr := c.store.upsert(acc)
	if aerr != nil {
		return core.AccountRecord{}, aerr
	}
	record := accountRecord(&rec, time.Now().UTC())
	if verr != nil {
		record.Note = strings.TrimSpace(record.Note + " 导入后未能验证（" + redactErr(verr) + "）")
	}
	return record, nil
}

// profileInfo is one candidate profile directory.
type profileInfo struct {
	name     string
	dir      string
	signedIn bool
}

func (p profileInfo) describe() string {
	if p.signedIn {
		return "已登录"
	}
	return "未登录"
}

// desktopProfiles enumerates the Chromium profiles under the client's user-data
// directory, in the order Chromium itself names them.
func (c *Client) desktopProfiles(userData string) ([]profileInfo, error) {
	entries, err := os.ReadDir(userData)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("doubao: reading %s: %w", userData, err)
	}
	var out []profileInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !isProfileDirName(name) {
			continue
		}
		dir := filepath.Join(userData, name)
		if _, err := os.Stat(filepath.Join(dir, "Network", "Cookies")); err != nil {
			// Older Chromium keeps Cookies directly under the profile.
			if _, err := os.Stat(filepath.Join(dir, "Cookies")); err != nil {
				continue
			}
		}
		out = append(out, profileInfo{name: name, dir: dir, signedIn: c.profileSignedIn(dir)})
	}
	pinned := c.cfg.clientProfileDir()
	if pinned != "" {
		for i := range out {
			if strings.EqualFold(out[i].name, pinned) {
				out = []profileInfo{out[i]}
				return out, nil
			}
		}
		return nil, nil
	}
	sort.SliceStable(out, func(i, j int) bool { return profileOrder(out[i].name) < profileOrder(out[j].name) })
	return out, nil
}

// profileOrder sorts Default first, then numbered profiles.
func profileOrder(name string) int {
	if strings.EqualFold(name, "Default") {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(name, "Profile %d", &n); err == nil {
		return n + 1
	}
	return 1 << 20
}

// isProfileDirName matches the directory names Chromium uses for profiles.
func isProfileDirName(name string) bool {
	if strings.EqualFold(name, "Default") {
		return true
	}
	var n int
	_, err := fmt.Sscanf(name, "Profile %d", &n)
	return err == nil
}

// profileSignedIn reports whether the profile holds a Doubao session cookie.
// It reads the cookie database read-only and never decrypts anything.
func (c *Client) profileSignedIn(profileDir string) bool {
	for _, p := range cookieDBPaths(profileDir) {
		db, err := openSQLite(p)
		if err != nil {
			continue
		}
		rows, qerr := db.queryRows(
			"select count(*) from cookies where name in ('sessionid','sessionid_ss','sid_tt') and host_key like '%doubao%'")
		_ = closeSQLite(db)
		if qerr != nil && !sqliteSupported() {
			return false
		}
		if len(rows) == 1 && len(rows[0]) == 1 {
			if n := parseDecimal(rows[0][0]); n > 0 {
				return true
			}
		}
	}
	return false
}

// hasClientProfile reports whether an account already came from this profile.
// The store does not record provenance, so this matches on the source tag and
// the profile name the import baked into the label.
func (c *Client) hasClientProfile(profileDir string) bool {
	profile := filepath.Base(strings.TrimSpace(profileDir))
	if strings.TrimSpace(profile) == "" {
		return false
	}
	suffix := "客户端 " + profile
	for _, acc := range c.store.snapshot() {
		if acc.Source != "client" {
			continue
		}
		if strings.Contains(acc.Label, suffix) {
			return true
		}
	}
	return false
}

// readClientSession reads and decrypts one profile's Doubao cookies.
func (c *Client) readClientSession(profileDir string) (clientSession, error) {
	if !clientImportSupported() {
		return clientSession{}, errors.New("doubao: reading the desktop client is supported only on Windows")
	}
	userData := filepath.Dir(profileDir)
	key, err := clientCookieKey(filepath.Join(userData, "Local State"))
	if err != nil {
		return clientSession{}, err
	}
	var rows []cookieRow
	var lastErr error
	for _, p := range cookieDBPaths(profileDir) {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		rows, err = readClientCookies(p, key)
		if err == nil {
			break
		}
		lastErr = err
	}
	if len(rows) == 0 {
		if lastErr != nil {
			return clientSession{}, lastErr
		}
		return clientSession{}, errors.New("doubao: the desktop client profile holds no Doubao session")
	}
	header := cookieHeaderFromRows(rows)
	if !hasSessionCookie(header) {
		return clientSession{}, errors.New("doubao: the desktop client profile is not signed in to Doubao")
	}
	return clientSession{profileDir: profileDir, userData: userData, cookie: header}, nil
}

// cookieDBPaths are the places Chromium has kept the cookie database over
// versions.
func cookieDBPaths(profileDir string) []string {
	return []string{
		filepath.Join(profileDir, "Network", "Cookies"),
		filepath.Join(profileDir, "Cookies"),
	}
}

// clientCookieKey unwraps the AES key from Local State.  The key is base64 of
// the ASCII tag "DPAPI" followed by a DPAPI blob.
func clientCookieKey(localStatePath string) ([]byte, error) {
	raw, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, fmt.Errorf("doubao: reading %s: %w", localStatePath, err)
	}
	var state localStateEnvelope
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("doubao: parsing Local State: %w", err)
	}
	encoded := strings.TrimSpace(state.OSCrypt.EncryptedKey)
	if encoded == "" {
		return nil, errors.New("doubao: Local State has no os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("doubao: decoding os_crypt.encrypted_key: %w", err)
	}
	if len(blob) <= len(dpapiPrefix) || string(blob[:len(dpapiPrefix)]) != dpapiPrefix {
		return nil, errors.New("doubao: cookie key is not a Windows DPAPI blob")
	}
	key, err := unprotectClientDPAPI(blob[len(dpapiPrefix):])
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("doubao: cookie key is %d bytes, want 32", len(key))
	}
	return key, nil
}

// readClientCookies reads the cookie table and decrypts each Doubao value.
func readClientCookies(path string, key []byte) ([]cookieRow, error) {
	db, err := openSQLite(path)
	if err != nil {
		return nil, err
	}
	defer closeSQLite(db)
	rows, err := db.queryRows(
		"select host_key, name, value, encrypted_value from cookies where host_key like '%doubao%'")
	if err != nil {
		return nil, err
	}
	out := make([]cookieRow, 0, len(rows))
	for _, row := range rows {
		if len(row) < 4 {
			continue
		}
		name := string(row[1])
		if name == "" {
			continue
		}
		plain := string(row[2])
		if plain == "" && len(row[3]) > 0 {
			dec, derr := decryptCookieValue(row[3], key)
			if derr != nil {
				continue
			}
			plain = dec
		}
		if plain == "" {
			continue
		}
		out = append(out, cookieRow{name: name, value: plain})
	}
	return out, nil
}

// decryptCookieValue opens one Chromium cookie value and strips the app-bound
// header.  Older builds wrote a bare DPAPI blob; both are handled.
func decryptCookieValue(blob, key []byte) (string, error) {
	if len(blob) >= 3 && (string(blob[:3]) == "v10" || string(blob[:3]) == "v11") {
		plain, err := openAESGCM(blob[3:], key)
		if err != nil {
			return "", err
		}
		return stripAppBoundHeader(plain), nil
	}
	// A pre-v10 value is itself a DPAPI blob.
	plain, err := unprotectClientDPAPI(blob)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// openAESGCM opens the nonce||ciphertext||tag layout Chromium writes.
func openAESGCM(payload, key []byte) ([]byte, error) {
	const nonceSize = 12
	if len(payload) < nonceSize {
		return nil, errors.New("doubao: AES-GCM payload is too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := payload[:nonceSize]
	return gcm.Open(nil, nonce, payload[nonceSize:], nil)
}

// stripAppBoundHeader removes Chromium's 32-byte app-bound header when it is
// present, and leaves plaintext alone when it is not.
func stripAppBoundHeader(plain []byte) string {
	if len(plain) > appBoundHeaderSize {
		tail := plain[appBoundHeaderSize:]
		if looksLikeCookieValue(tail) {
			return string(tail)
		}
	}
	return string(plain)
}

// looksLikeCookieValue reports whether b is plausible cookie text: printable
// ASCII with no control bytes.  The app-bound header is binary, so this cleanly
// separates the two cases.
func looksLikeCookieValue(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// cookieHeaderFromRows renders the cookie rows as a request Cookie header,
// dropping duplicates (last writer wins) and any value that cannot be sent.
func cookieHeaderFromRows(rows []cookieRow) string {
	seen := make(map[string]int, len(rows))
	ordered := make([]cookieRow, 0, len(rows))
	for _, r := range rows {
		if !allPrintableASCII(r.value) {
			continue
		}
		if i, ok := seen[r.name]; ok {
			ordered[i] = r
			continue
		}
		seen[r.name] = len(ordered)
		ordered = append(ordered, r)
	}
	pairs := make([][2]string, 0, len(ordered))
	for _, r := range ordered {
		pairs = append(pairs, [2]string{r.name, r.value})
	}
	return cookieHeaderFromValues(pairs)
}

func allPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
