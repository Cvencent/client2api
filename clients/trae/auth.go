package trae

// auth.go — credential discovery and the per-account token record.
//
// The Trae desktop app keeps its auth blob in
//
//	%APPDATA%\<product>\User\globalStorage\storage.json
//
// under the key "iCubeAuthInfo://icube.cloudide", as base64 of a "tc"
// container (see tcrypt.go).  Decrypting it yields a flat JSON object:
//
//	{"token": "...", "refreshToken": "...", "expiredAt": "...",
//	 "refreshExpiredAt": "...", "host": "https://api.trae.cn",
//	 "userId": "...", "userRegion": {...}, "account": {...}}
//
// The same file carries the device fingerprint the upstream insists on:
// "telemetry.machineId", "telemetry.devDeviceId" and the key
// "iCubeAuthInfo://icube-dc:<digits>" whose numeric suffix is the SOLO device
// id.  A forged or unregistered fingerprint is silently dropped by the
// upstream (TLS connects, no response ever arrives), so discovery is the
// default and config only overrides it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// storageKeyAuth is the globalStorage key holding the encrypted auth blob.
const storageKeyAuth = "iCubeAuthInfo://icube.cloudide"

// knownProducts are the Trae desktop products whose globalStorage may hold a
// credential, in preference order.  The first two are the current names; "Trae"
// is the pre-rename directory and is usually an empty shell.
var knownProducts = []string{"Trae CN", "TRAE SOLO CN", "Trae"}

// errNoCredential marks a storage.json that carries no usable auth blob.
var errNoCredential = errors.New("no credential in storage.json")

// Auth is one account: a token pair plus the fingerprint that goes with it.
// It is safe for concurrent use; tokens are read under a read lock so a
// refresh can never be observed half-applied.
type Auth struct {
	mu sync.RWMutex

	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	RefreshExpiresAt time.Time
	Host             string
	UserID           string
	Username         string
	Email            string
	Region           string
	Scope            string
	MachineID        string
	DeviceID         string

	// Source is the storage.json the credential came from ("" when it came
	// from config).  Product is the Trae product directory name.
	Source     string
	Product    string
	Configured bool

	// IDOverride pins the account key for credentials stored by the panel.
	// Without it a hand-added credential with no user_id would be keyed by a
	// hash of its token, and refreshing the token would silently change the
	// account's identity — the panel would see it disappear and reappear.
	IDOverride string

	// Credits observed on the wire via notify_usage (best effort).
	IdeCredits  int64
	WorkCredits int64
	BillingMode string
}

// traeAuthFile is the decrypted auth blob.
type traeAuthFile struct {
	Token            string `json:"token"`
	RefreshToken     string `json:"refreshToken"`
	ExpiredAt        string `json:"expiredAt"`
	RefreshExpiredAt string `json:"refreshExpiredAt"`
	Host             string `json:"host"`
	UserID           string `json:"userId"`
	UserRegion       struct {
		Region   string `json:"region"`
		AiRegion string `json:"_aiRegion"`
	} `json:"userRegion"`
	Account struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Scope    string `json:"scope"`
	} `json:"account"`
}

// ---- token access ---------------------------------------------------------

// Token returns the current access token.
func (a *Auth) Token() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue returns the current refresh token.
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// ID is the stable identity used by the pool.  It prefers the Trae user id and
// falls back to a hash of the refresh token so a config-only credential still
// gets a stable key.
func (a *Auth) ID() string {
	a.mu.RLock()
	uid, rt, at, override := a.UserID, a.RefreshToken, a.AccessToken, a.IDOverride
	a.mu.RUnlock()
	if override != "" {
		return override
	}
	if uid != "" {
		return uid
	}
	seed := rt
	if seed == "" {
		seed = at
	}
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// Label is a human-readable account name for Status.
func (a *Auth) Label() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	switch {
	case a.Username != "":
		return a.Username
	case a.UserID != "":
		return a.UserID
	case a.Product != "":
		return a.Product
	default:
		return "configured account"
	}
}

// Masked renders the account without ever exposing a token.
func (a *Auth) Masked() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return core.MaskSecret(a.AccessToken)
}

// Expiry returns the access-token expiry (zero when unknown).
func (a *Auth) Expiry() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ExpiresAt
}

// RefreshExpiry returns the refresh-token expiry (zero when unknown).
func (a *Auth) RefreshExpiry() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshExpiresAt
}

// NeedsRefresh reports whether the access token expires within skew.
func (a *Auth) NeedsRefresh(skew time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.AccessToken == "" {
		return a.RefreshToken != ""
	}
	if a.ExpiresAt.IsZero() {
		return false
	}
	return !time.Now().Before(a.ExpiresAt.Add(-skew))
}

// Expired reports whether the access token is already past its expiry.
func (a *Auth) Expired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.ExpiresAt.IsZero() && !time.Now().Before(a.ExpiresAt)
}

// SetTokens installs a freshly minted token pair.
func (a *Auth) SetTokens(access, refresh string, expires, refreshExpires time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken = access
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if !expires.IsZero() {
		a.ExpiresAt = expires
	}
	if !refreshExpires.IsZero() {
		a.RefreshExpiresAt = refreshExpires
	}
}

// SetCredits records the credit balances seen on the wire.
func (a *Auth) SetCredits(ide, work int64, mode string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ide > 0 {
		a.IdeCredits = ide
	}
	if work > 0 {
		a.WorkCredits = work
	}
	if mode != "" {
		a.BillingMode = mode
	}
}

// snapshot returns a consistent copy of the token fields.
func (a *Auth) snapshot() (access, refresh string, expires time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken, a.RefreshToken, a.ExpiresAt
}

// ---- discovery ------------------------------------------------------------

// discoverAuths builds the account list: an explicit config credential first,
// then every readable storage.json under the known product directories.
func discoverAuths(cfg *Config, logf func(string, ...any)) []*Auth {
	log := func(format string, args ...any) {
		if logf != nil {
			logf(format, args...)
		}
	}
	var out []*Auth
	seen := map[string]bool{}
	add := func(a *Auth) {
		if a == nil {
			return
		}
		key := a.ID()
		if key == "" {
			return
		}
		if seen[key] {
			log("ignoring duplicate account %s", a.Masked())
			return
		}
		seen[key] = true
		out = append(out, a)
	}

	if cfg.AccessToken != "" {
		a := &Auth{
			AccessToken:  cfg.AccessToken,
			RefreshToken: cfg.RefreshToken,
			UserID:       cfg.UserID,
			Host:         cfg.authHost(),
			MachineID:    cfg.MachineID,
			DeviceID:     cfg.DeviceID,
			Region:       cfg.Region,
			Product:      "config",
			Configured:   true,
		}
		if t, ok := parseTime(cfg.AccessTokenExpiry); ok {
			a.ExpiresAt = t
		}
		add(a)
	}

	if !cfg.autoDiscover() {
		return out
	}

	for _, path := range storageCandidates(cfg) {
		a, err := loadAuthFromStorage(path, cfg)
		if err != nil {
			if !errors.Is(err, errNoCredential) && !errors.Is(err, os.ErrNotExist) {
				log("credential at %s unusable: %v", path, err)
			}
			continue
		}
		log("discovered account %s from %s (access token %s, expires %s)",
			a.Masked(), path, expiryState(a.ExpiresAt), formatTime(a.ExpiresAt))
		add(a)
	}
	return out
}

// storageCandidates lists the storage.json files to try.
func storageCandidates(cfg *Config) []string {
	if p := strings.TrimSpace(cfg.StoragePath); p != "" {
		return []string{expandHome(p)}
	}
	if len(cfg.StoragePaths) > 0 {
		out := make([]string, 0, len(cfg.StoragePaths))
		for _, p := range cfg.StoragePaths {
			out = append(out, expandHome(p))
		}
		return out
	}
	var out []string
	if base := os.Getenv("APPDATA"); base != "" {
		for _, product := range knownProducts {
			out = append(out, filepath.Join(base, product, "User", "globalStorage", "storage.json"))
		}
	}
	return out
}

// expandHome resolves a leading "~" against the user home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadAuthFromStorage reads one storage.json and decrypts its credential.
func loadAuthFromStorage(path string, cfg *Config) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(raw, &store); err != nil {
		return nil, fmt.Errorf("storage.json parse: %w", err)
	}
	var encoded string
	if v, ok := store[storageKeyAuth]; ok {
		_ = json.Unmarshal(v, &encoded)
	}
	if strings.TrimSpace(encoded) == "" {
		return nil, errNoCredential
	}
	plain, err := DecryptTCBase64(encoded)
	if err != nil {
		return nil, fmt.Errorf("decrypt %s: %w", storageKeyAuth, err)
	}
	var f traeAuthFile
	if err := json.Unmarshal(plain, &f); err != nil {
		return nil, fmt.Errorf("auth blob parse: %w", err)
	}
	if strings.TrimSpace(f.Token) == "" {
		return nil, fmt.Errorf("%w: empty access token", errNoCredential)
	}

	a := &Auth{
		AccessToken:  f.Token,
		RefreshToken: f.RefreshToken,
		Host:         firstNonEmpty(f.Host, cfg.authHost()),
		UserID:       f.UserID,
		Username:     f.Account.Username,
		Email:        f.Account.Email,
		Scope:        f.Account.Scope,
		Region:       firstNonEmpty(f.UserRegion.Region, cfg.Region),
		Source:       path,
		Product:      filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))),
	}
	if t, ok := parseTime(f.ExpiredAt); ok {
		a.ExpiresAt = t
	}
	if t, ok := parseTime(f.RefreshExpiredAt); ok {
		a.RefreshExpiresAt = t
	}
	a.MachineID = rawString(store["telemetry.machineId"])
	a.DeviceID = extractSoloDeviceID(store)
	if a.DeviceID == "" {
		a.DeviceID = hashDeviceID(a.MachineID)
	}
	// Explicit config always wins over discovery.
	if cfg.MachineID != "" {
		a.MachineID = cfg.MachineID
	}
	if cfg.DeviceID != "" {
		a.DeviceID = cfg.DeviceID
	}
	return a, nil
}

// extractSoloDeviceID pulls the numeric device id out of the
// "iCubeAuthInfo://icube-dc:<digits>" storage key.
func extractSoloDeviceID(store map[string]json.RawMessage) string {
	const prefix = "iCubeAuthInfo://icube-dc:"
	for key := range store {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		id := key[len(prefix):]
		if id != "" && isDigits(id) {
			return id
		}
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// hashDeviceID ports auth.js hashDeviceId(): a 32-bit rolling hash rendered as
// a zero-padded 19-digit decimal.  Used only when the storage file has no
// iCubeAuthInfo://icube-dc:<id> key.
func hashDeviceID(machineID string) string {
	if machineID == "" {
		return ""
	}
	var hash int32
	for i := 0; i < len(machineID); i++ {
		hash = (hash << 5) - hash + int32(machineID[i])
	}
	v := int64(hash)
	if v < 0 {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	for len(s) < 19 {
		s = "0" + s
	}
	return s
}

// rawString decodes a JSON string value, tolerating null/absent.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// parseTime accepts RFC3339 (with or without fractional seconds) and Unix
// seconds/milliseconds.
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(normalizeEpoch(n), 0), true
	}
	return time.Time{}, false
}

// normalizeEpoch converts a millisecond epoch to seconds.
func normalizeEpoch(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// epochTime converts a Unix epoch (seconds or milliseconds) into a time,
// returning the zero time for a non-positive value.
func epochTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(normalizeEpoch(v), 0)
}

func expiryState(t time.Time) string {
	if t.IsZero() {
		return "unknown expiry"
	}
	if time.Now().After(t) {
		return "EXPIRED"
	}
	return "valid"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
