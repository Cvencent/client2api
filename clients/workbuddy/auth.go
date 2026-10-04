package workbuddy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Realm names.  WorkBuddy has two independent realms; an account belongs to
// exactly one of them and the realm decides which host the request goes to.
const (
	realmCN     = "cn"
	realmGlobal = "global"
)

// Auth is one WorkBuddy account credential.  It mirrors the on-disk shape of
// the reference implementation (client2api-lab/_upstream/workbuddy2api-panel,
// internal/auth/auth.go) but is otherwise independent of it: no other package
// in this program imports this type.
//
// Every field is guarded by mu because RefreshToken mutates the record while
// concurrent chats read it.
type Auth struct {
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix seconds; 0 means "unknown"
	Domain       string
	Realm        string // "cn", "global"/"intl", or "" (inferred from Domain)

	UID          string
	EnterpriseID string
	Nickname     string
	DeviceToken  string

	// Daily-reward bookkeeping.  Both are local wall-clock stamps written by
	// the check-in path ("2006-01-02 15:04:05"), never by the vendor; the
	// CN realm records a check-in and the intl realm a daily-activity chat.
	// They live in the credential file so "did I already do today?" survives
	// a restart, exactly like the reference implementation's lastCheckin.
	LastCheckin   string
	LastDailyChat string

	FilePath string
}

// On-disk document.  The reference file nests token fields under "auth" and
// identity under "account"; a flat document with the same keys is also accepted
// so hand-written credentials are easy to produce.
type authDoc struct {
	Auth        authCore    `json:"auth"`
	Account     authAccount `json:"account"`
	DeviceToken string      `json:"device_token,omitempty"`
	// Daily-reward stamps sit at the top level, matching the key names the
	// reference implementation writes (lastCheckin / lastDailyChat).
	LastCheckin   string `json:"lastCheckin,omitempty"`
	LastDailyChat string `json:"lastDailyChat,omitempty"`
}

type authCore struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
}

type authAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

type flatDoc struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	ExpiresAt     int64  `json:"expiresAt"`
	Domain        string `json:"domain"`
	Realm         string `json:"realm"`
	UID           string `json:"uid"`
	EnterpriseID  string `json:"enterpriseId"`
	Nickname      string `json:"nickname"`
	DeviceToken   string `json:"device_token"`
	LastCheckin   string `json:"lastCheckin"`
	LastDailyChat string `json:"lastDailyChat"`
}

// ParseAuth accepts either document shape.  An accessToken is mandatory: a file
// without one is not a credential and is reported as an error so callers can
// skip it silently.
func ParseAuth(raw []byte) (*Auth, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	a := &Auth{}
	if _, nested := probe["auth"]; nested {
		var doc authDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a.AccessToken = strings.TrimSpace(doc.Auth.AccessToken)
		a.RefreshToken = strings.TrimSpace(doc.Auth.RefreshToken)
		a.ExpiresAt = doc.Auth.ExpiresAt
		a.Domain = strings.TrimSpace(doc.Auth.Domain)
		a.Realm = strings.TrimSpace(doc.Auth.Realm)
		a.UID = strings.TrimSpace(doc.Account.UID)
		a.EnterpriseID = strings.TrimSpace(doc.Account.EnterpriseID)
		a.Nickname = doc.Account.Nickname
		a.DeviceToken = strings.TrimSpace(doc.DeviceToken)
		a.LastCheckin = strings.TrimSpace(doc.LastCheckin)
		a.LastDailyChat = strings.TrimSpace(doc.LastDailyChat)
	} else {
		var doc flatDoc
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a.AccessToken = strings.TrimSpace(doc.AccessToken)
		a.RefreshToken = strings.TrimSpace(doc.RefreshToken)
		a.ExpiresAt = doc.ExpiresAt
		a.Domain = strings.TrimSpace(doc.Domain)
		a.Realm = strings.TrimSpace(doc.Realm)
		a.UID = strings.TrimSpace(doc.UID)
		a.EnterpriseID = strings.TrimSpace(doc.EnterpriseID)
		a.Nickname = doc.Nickname
		a.DeviceToken = strings.TrimSpace(doc.DeviceToken)
		a.LastCheckin = strings.TrimSpace(doc.LastCheckin)
		a.LastDailyChat = strings.TrimSpace(doc.LastDailyChat)
	}
	if a.AccessToken == "" {
		return nil, errors.New("parse_error: missing accessToken")
	}
	return a, nil
}

// SaveAtomic rewrites the credential file.  It refuses to persist a record with
// no access token, so a half-built account can never clobber a good file.
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.AccessToken == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", core.MaskSecret(a.UID))
	}
	if a.FilePath == "" {
		return errors.New("save refused: empty FilePath")
	}
	doc := authDoc{
		Auth: authCore{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt,
			Domain:       a.Domain,
			Realm:        a.Realm,
		},
		Account: authAccount{
			UID:          a.UID,
			EnterpriseID: a.EnterpriseID,
			Nickname:     a.Nickname,
		},
	}
	if a.DeviceToken != "" {
		doc.DeviceToken = a.DeviceToken
	}
	doc.LastCheckin = a.LastCheckin
	doc.LastDailyChat = a.LastDailyChat
	return core.WriteJSONAtomic(a.FilePath, doc)
}

// LoadAccounts reads every "*.json" file in dir and keeps the ones that parse
// as credentials.  Unparseable or unrelated files are skipped, never fatal.
func LoadAccounts(dir string) ([]*Auth, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	out := make([]*Auth, 0, len(matches))
	seen := make(map[string]bool)
	for _, p := range matches {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		a, err := ParseAuth(raw)
		if err != nil {
			continue
		}
		a.FilePath = p
		if a.UID != "" {
			if seen[a.UID] {
				continue
			}
			seen[a.UID] = true
		}
		out = append(out, a)
	}
	return out, nil
}

// --- accessors -------------------------------------------------------------
//
// All of them are nil-safe and take the lock, so a concurrent refresh cannot
// tear a read.

func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

func (a *Auth) DeviceTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.DeviceToken
}

func (a *Auth) UIDValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.UID
}

func (a *Auth) EnterpriseIDValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.EnterpriseID
}

func (a *Auth) NicknameValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Nickname
}

// LastCheckinValue returns the local stamp of the last CN check-in, or "".
func (a *Auth) LastCheckinValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LastCheckin
}

// LastDailyChatValue returns the local stamp of the last intl daily-activity
// chat, or "".
func (a *Auth) LastDailyChatValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.LastDailyChat
}

// MarkCheckin stamps the CN check-in as done now, in the same local format the
// reference implementation uses.
func (a *Auth) MarkCheckin() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastCheckin = time.Now().Format(checkinStampLayout)
}

// MarkDailyChat stamps the intl daily activity as done now.
func (a *Auth) MarkDailyChat() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LastDailyChat = time.Now().Format(checkinStampLayout)
}

// checkinStampLayout matches the reference implementation's
// time.strftime("%Y-%m-%d %H:%M:%S").
const checkinStampLayout = "2006-01-02 15:04:05"

// didToday reports whether a stored stamp belongs to today.  A blank stamp is
// never "today".  Both sides are compared on the date prefix only, so a stamp
// written by an older build in another format still works as long as it starts
// with the date.
func didToday(stamp string, now time.Time) bool {
	stamp = strings.TrimSpace(stamp)
	if stamp == "" {
		return false
	}
	return strings.HasPrefix(stamp, now.Format("2006-01-02"))
}

// RealmName normalises the realm.  The reference implementation spells the
// international realm "global" while the product documentation calls it
// "intl"; both are accepted, as is inferring it from the domain.
func (a *Auth) RealmName() string {
	if a == nil {
		return realmCN
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

func (a *Auth) realmLocked() string {
	switch strings.ToLower(strings.TrimSpace(a.Realm)) {
	case "global", "intl", "international":
		return realmGlobal
	case "cn", "china", "mainland":
		return realmCN
	}
	if isGlobalDomain(a.Domain) {
		return realmGlobal
	}
	return realmCN
}

// IsGlobal reports whether the account lives in the international realm.
func (a *Auth) IsGlobal() bool { return a.RealmName() == realmGlobal }

// isGlobalDomain matches the international product domain and its subdomains.
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	if d == "" {
		return false
	}
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh reports whether the access token is expired, or will be within
// the given window.  An unknown expiry is NOT treated as needing a refresh:
// there is nothing to compare against, and the 401 path already refreshes on
// demand, so a credential file that simply omits expiresAt stays usable
// instead of being invalidated by a refresh that can never succeed.
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	if a == nil {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// ExpiryTime returns the token expiry, or the zero time when unknown.
func (a *Auth) ExpiryTime() time.Time {
	if a == nil {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return time.Time{}
	}
	return time.Unix(a.ExpiresAt, 0)
}

// SetTokens installs a refreshed token set.  Empty values leave the existing
// field untouched so a partial refresh response cannot erase data.
func (a *Auth) SetTokens(access, refresh string, expiresAt int64, domain string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if access != "" {
		a.AccessToken = access
	}
	if refresh != "" {
		a.RefreshToken = refresh
	}
	if domain != "" {
		a.Domain = domain
	}
	if expiresAt > 0 {
		a.ExpiresAt = expiresAt
	}
}

// Snapshot returns a shallow copy safe to hand to the header builders.  It
// deliberately does not copy the mutex.
func (a *Auth) Snapshot() *Auth {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return &Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		Realm:        a.Realm,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
		FilePath:     a.FilePath,
	}
}

// ID is a stable, non-secret identifier for status output.
func (a *Auth) ID() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.UID != "" {
		return a.UID
	}
	if a.FilePath != "" {
		return filepath.Base(a.FilePath)
	}
	return "unknown"
}

// Label is a human-readable, non-secret name for status output.
func (a *Auth) Label() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	nick := strings.TrimSpace(a.Nickname)
	uid := a.UID
	domain := a.Domain
	a.mu.Unlock()
	if nick != "" {
		return nick
	}
	if uid != "" {
		return core.MaskSecret(uid)
	}
	if domain != "" {
		return domain
	}
	return "account"
}

// --- vendor credential discovery ------------------------------------------

// vendorCredentialStores lists the places the WorkBuddy / CodeBuddy desktop and
// CLI products keep their own credentials.  We only ever *read* them, and only
// to tell the operator where a usable token might live: the real tokens on this
// platform are sealed in Tencent "$wbEncrypted" envelopes that cannot be
// decrypted without the vendor's key hierarchy, so they are reported, not
// imported.
type vendorStore struct {
	Path string
	Note string
}

func vendorCredentialStores() []vendorStore {
	home, _ := os.UserHomeDir()
	local := os.Getenv("LOCALAPPDATA")
	roaming := os.Getenv("APPDATA")
	var out []vendorStore
	add := func(p, note string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		if _, err := os.Stat(p); err == nil {
			out = append(out, vendorStore{Path: p, Note: note})
		}
	}
	if local != "" {
		add(filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth", "workbuddy-desktop.info"),
			"encrypted account/token store (Tencent $wbEncrypted envelope)")
		add(filepath.Join(local, "WorkBuddy"), "desktop app data")
	}
	if roaming != "" {
		add(filepath.Join(roaming, "WorkBuddy"), "desktop app data")
	}
	if home != "" {
		add(filepath.Join(home, ".workbuddy"), "CLI/desktop state")
		add(filepath.Join(home, ".codebuddy"), "CLI state")
		add(filepath.Join(home, ".workbuddy", "keyblob"), "wrapped key material for the encrypted stores")
	}
	return out
}

// VendorStoreNote is a one-line, secret-free summary of the vendor credential
// stores found on this machine.  It never reads the file contents.
func VendorStoreNote() string {
	stores := vendorCredentialStores()
	if len(stores) == 0 {
		return ""
	}
	names := make([]string, 0, len(stores))
	for _, s := range stores {
		names = append(names, s.Path)
	}
	return strings.Join(names, "; ")
}
