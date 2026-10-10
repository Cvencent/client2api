package qoder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// accounts.go holds the credential store, the pool and the panel-facing
// AccountManager implementation.
//
// The credential is a device token the Qoder CN desktop client mints at login
// and keeps in an Electron safeStorage blob.  This module can also obtain one
// through the vendor browser device grant or accept a pasted token; it never
// invents a credential behind the operator's back.  When the vendor stops
// accepting a token the remedy is the panel's browser re-login, local import,
// or manual replacement.

const (
	originConfig = "config"
	originStored = "stored"
)

// storedAccount is one credential as it is written to disk.  The token is the
// only secret in this file and it is written through the atomic writer.
type storedAccount struct {
	ImportPath   string `json:"import_path,omitempty"`
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserType     string `json:"user_type,omitempty"`
	Phone        string `json:"phone,omitempty"`
	Name         string `json:"name,omitempty"`
	// ExpiresAtMS is the millisecond timestamp the token expires at, or 0 for
	// "unknown".  0 is not expired: the vendor is the authority on whether a
	// token still works, and refusing to try on a guess is worse than letting
	// the server answer 401.
	ExpiresAtMS int64 `json:"expires_at_ms,omitempty"`
	// RefreshExpiresAtMS is the same idea for the refresh token.  It is
	// recorded for display only; nothing here renews a credential.
	RefreshExpiresAtMS int64 `json:"refresh_expires_at_ms,omitempty"`
	Enabled            bool  `json:"enabled"`
}

type accountFile struct {
	Accounts []storedAccount `json:"accounts"`
}

// accountState is the runtime half, persisted separately so that a restart does
// not silently turn a rejected token back into a healthy-looking one.
type accountState struct {
	Dead         bool   `json:"dead,omitempty"`
	Failures     int    `json:"failures,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	CooldownTill string `json:"cooldown_until,omitempty"`
	LastUsed     string `json:"last_used,omitempty"`
}

type persistedState struct {
	Accounts map[string]accountState `json:"accounts"`
}

// account is one credential plus the penalties the pool accumulated for it.
type account struct {
	storedAccount
	origin string

	failures     int
	cooldownTill time.Time
	dead         bool
	lastError    string
	lastUsed     time.Time
}

// selectable reports whether the pool may hand this account a request.
func (a *account) selectable(now time.Time) bool {
	if !a.Enabled || a.dead {
		return false
	}
	if strings.TrimSpace(a.Token) == "" {
		return false
	}
	return !now.Before(a.cooldownTill)
}

func (a *account) expired(now time.Time) bool {
	return a.ExpiresAtMS > 0 && a.ExpiresAtMS <= now.UnixMilli()
}

// store is the credential pool.
type store struct {
	mu        sync.Mutex
	path      string
	statePath string
	all       []*account
}

func loadStore(path, statePath string) (*store, error) {
	s := &store{path: path, statePath: statePath}

	var file accountFile
	if err := readJSONIfPresent(path, &file); err != nil {
		// A corrupt credential file must not take the module down: it is
		// reported, the store stays empty, and the operator can re-import.
		return s, fmt.Errorf("qoder: reading %s: %w", path, err)
	}
	for _, sa := range file.Accounts {
		if strings.TrimSpace(sa.Token) == "" {
			continue
		}
		s.all = append(s.all, &account{storedAccount: sa, origin: originStored})
	}

	var states persistedState
	if err := readJSONIfPresent(statePath, &states); err != nil {
		return s, fmt.Errorf("qoder: reading %s: %w", statePath, err)
	}
	for _, a := range s.all {
		st, ok := states.Accounts[a.ID]
		if !ok {
			continue
		}
		a.dead = st.Dead
		a.failures = st.Failures
		a.lastError = st.LastError
		a.cooldownTill = parseRFC3339(st.CooldownTill)
		a.lastUsed = parseRFC3339(st.LastUsed)
	}
	return s, nil
}

func readJSONIfPresent(path string, v any) error {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return core.ReadJSON(path, v)
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// save writes the credentials.  The caller holds the lock.
func (s *store) save() error {
	if s.path == "" {
		return nil
	}
	file := accountFile{Accounts: make([]storedAccount, 0, len(s.all))}
	for _, a := range s.all {
		file.Accounts = append(file.Accounts, a.storedAccount)
	}
	return core.WriteJSONAtomic(s.path, file)
}

// saveState writes the runtime penalties.  The caller holds the lock.
func (s *store) saveState() error {
	if s.statePath == "" {
		return nil
	}
	file := persistedState{Accounts: map[string]accountState{}}
	for _, a := range s.all {
		if !a.dead && a.failures == 0 && a.lastError == "" && a.cooldownTill.IsZero() && a.lastUsed.IsZero() {
			continue
		}
		st := accountState{Dead: a.dead, Failures: a.failures, LastError: a.lastError}
		if !a.cooldownTill.IsZero() {
			st.CooldownTill = a.cooldownTill.UTC().Format(time.RFC3339)
		}
		if !a.lastUsed.IsZero() {
			st.LastUsed = a.lastUsed.UTC().Format(time.RFC3339)
		}
		file.Accounts[a.ID] = st
	}
	return core.WriteJSONAtomic(s.statePath, file)
}

// applyConfig installs the accounts that came from configuration.  A config
// entry is authoritative for its id and is re-applied on every start, which is
// what makes RemoveAccount refuse to delete one.
func (s *store) applyConfig(configured []account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range configured {
		acc := configured[i]
		acc.origin = originConfig
		if acc.Label == "" {
			acc.Label = acc.ID
		}
		replaced := false
		for j, existing := range s.all {
			if existing.ID == acc.ID {
				// Preserve runtime penalties: editing a label must not silently
				// pardon a token the vendor rejected.
				acc.failures = existing.failures
				acc.cooldownTill = existing.cooldownTill
				acc.dead = existing.dead
				acc.lastError = existing.lastError
				acc.lastUsed = existing.lastUsed
				s.all[j] = &acc
				replaced = true
				break
			}
		}
		if !replaced {
			s.all = append(s.all, &acc)
		}
	}
}

func (s *store) snapshot() []account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]account, 0, len(s.all))
	for _, a := range s.all {
		out = append(out, *a)
	}
	return out
}

func (s *store) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.all)
}

func (s *store) lookup(id string) (account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID == id {
			return *a, true
		}
	}
	return account{}, false
}

// candidates returns the accounts a request may be tried against, in the order
// the pool wants them tried: operator priority first (lower wins), then least
// recently used, with tokens past their declared expiry ranked last.
func (s *store) candidates(now time.Time) []account {
	s.mu.Lock()
	defer s.mu.Unlock()

	var fresh, stale []*account
	for _, a := range s.all {
		if !a.selectable(now) {
			continue
		}
		if a.expired(now) {
			stale = append(stale, a)
		} else {
			fresh = append(fresh, a)
		}
	}
	byPriorityThenAge := func(list []*account) {
		sort.SliceStable(list, func(i, j int) bool {
			pi := core.AccountPriority("qoder", list[i].ID)
			pj := core.AccountPriority("qoder", list[j].ID)
			if pi != pj {
				return pi < pj
			}
			return list[i].lastUsed.Before(list[j].lastUsed)
		})
	}
	byPriorityThenAge(fresh)
	byPriorityThenAge(stale)

	out := make([]account, 0, len(fresh)+len(stale))
	for _, a := range append(fresh, stale...) {
		out = append(out, *a)
	}
	return out
}

// penalise records a failure.  A rejected token is parked until the operator
// imports a new one; anything else gets a cooldown, because a rate limit or a
// network hiccup is temporary.
func (s *store) penalise(id string, err error, now time.Time, cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.failures++
		a.lastError = redactErr(err)
		if failureKind(err) == core.FailureAuth {
			a.dead = true
		} else {
			a.cooldownTill = now.Add(cooldown)
		}
		if saveErr := s.saveState(); saveErr != nil {
			_ = saveErr
		}
		return
	}
}

// reset clears the penalties after a successful call.
func (s *store) reset(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.failures = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.dead = false
		a.lastUsed = now
		if err := s.saveState(); err != nil {
			_ = err
		}
		return
	}
}

// clearPenalties clears health verdicts without touching the LRU timestamp.
// A successful balance read proves the credential still works, but it is not a
// chat request and must not change which account the pool prefers next.
func (s *store) clearPenalties(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.failures = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.dead = false
		if err := s.saveState(); err != nil {
			_ = err
		}
		return
	}
}

// revive clears every runtime penalty and re-enables the account.  It never
// touches the token.
func (s *store) revive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.dead = false
		a.failures = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.Enabled = true
		if err := s.save(); err != nil {
			return err
		}
		return s.saveState()
	}
	return fmt.Errorf("qoder: no account %q", id)
}

func (s *store) setEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.Enabled = enabled
		return s.save()
	}
	return fmt.Errorf("qoder: no account %q", id)
}

func (s *store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.all {
		if a.ID != id {
			continue
		}
		if a.origin == originConfig {
			return fmt.Errorf("qoder: account %q comes from the configuration and cannot be removed here", id)
		}
		s.all = append(s.all[:i], s.all[i+1:]...)
		if err := s.save(); err != nil {
			return err
		}
		return s.saveState()
	}
	return fmt.Errorf("qoder: no account %q", id)
}

// put adds or replaces one account and persists it.
func (s *store) put(acc account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.all {
		if a.ID == acc.ID {
			acc.origin = a.origin
			// A replacement credential is fresh evidence. Carrying the
			// previous token's cooldown or dead verdict onto it would leave
			// a working account stuck until the operator manually revived it.
			if acc.Token == a.Token {
				acc.failures = a.failures
				acc.cooldownTill = a.cooldownTill
				acc.dead = a.dead
				acc.lastError = a.lastError
				acc.lastUsed = a.lastUsed
			}
			s.all[i] = &acc
			return s.save()
		}
	}
	s.all = append(s.all, &acc)
	return s.save()
}

// ---------------------------------------------------------------------------
// Panel-facing account management
// ---------------------------------------------------------------------------

// penalise records a failure with the cooldown that suits it.
func (c *Client) penalise(id string, err error, now time.Time) {
	cooldown := c.cfg.cooldown()
	if failureKind(err) == core.FailureAuth {
		cooldown = c.cfg.authCooldown()
	}
	c.store.penalise(id, err, now, cooldown)
}

// logf is a nil-safe logger, so a Client built by hand in a test needs no Deps.
func (c *Client) logf(format string, args ...any) {
	if c.deps.Logf != nil {
		c.deps.Logf(format, args...)
	}
}

// AccountFields describes the add form.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "token",
			Label:       "设备令牌 (token)",
			Type:        "password",
			Required:    true,
			Placeholder: "dt-...",
			Help: "Qoder CN 客户端的设备令牌，以 dt- 开头。在客户端里打开一次后，" +
				"优先用上面的「浏览器登录」直接获取；也可以从客户端导入或手动粘贴。",
		},
		{
			Key:         "refresh_token",
			Label:       "刷新令牌 (refresh token)",
			Type:        "password",
			Placeholder: "drt-...",
			Help:        "可选。仅用于显示，本模块不会用它换新令牌。",
			Advanced:    true,
		},
		{
			Key:         "label",
			Label:       "备注",
			Type:        "text",
			Placeholder: "例如工作号",
			Help:        "只给你自己看，不参与路由。",
		},
		{
			Key:      "phone",
			Label:    "手机号",
			Type:     "text",
			Advanced: true,
		},
		{
			Key:      "expires_at",
			Label:    "过期时间",
			Type:     "text",
			Advanced: true,
			Help:     "可选。留空表示未知，由厂商决定令牌是否还有效。",
		},
	}
}

func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	now := time.Now().UTC()
	accounts := c.store.snapshot()
	out := make([]core.AccountRecord, 0, len(accounts))
	for i := range accounts {
		out = append(out, accountRecord(&accounts[i], now))
	}
	return out, nil
}

// AddAccount imports a device token by hand.
//
// The token is probed before it is stored, because importing a token the vendor
// has already rejected produces an account that can never work.  A transport
// failure is NOT a rejection: the account is stored with the failure in its
// note, so a flaky network cannot block an import.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	token := strings.TrimSpace(spec.Fields["token"])
	if token == "" {
		return core.AccountRecord{}, errors.New("qoder: 设备令牌是必填的")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return core.AccountRecord{}, errors.New("qoder: 令牌里含有空白字符，请原样粘贴")
	}

	acc := account{
		storedAccount: storedAccount{
			Label:              strings.TrimSpace(spec.Label),
			Token:              token,
			RefreshToken:       strings.TrimSpace(spec.Fields["refresh_token"]),
			UserID:             strings.TrimSpace(spec.Fields["user_id"]),
			Phone:              strings.TrimSpace(spec.Fields["phone"]),
			Name:               strings.TrimSpace(spec.Fields["name"]),
			ExpiresAtMS:        parseExpiryMS(spec.Fields["expires_at"]),
			RefreshExpiresAtMS: 0,
			Enabled:            spec.EnabledOr(true),
		},
		origin: originStored,
	}
	acc.ID = strings.TrimSpace(spec.ID)
	if acc.ID == "" {
		acc.ID = defaultAccountID(acc)
	}
	if acc.Label == "" {
		acc.Label = defaultAccountLabel(acc)
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	info, err := c.up.userInfo(probeCtx, acc.Token)
	cancel()
	if err != nil {
		if failureKind(err) == core.FailureAuth {
			return core.AccountRecord{}, fmt.Errorf("qoder: 厂商拒绝了这个令牌 (%s)，请重新获取", redactErr(err))
		}
		c.logf("qoder: could not verify the new account %s before storing it: %s", acc.ID, redactErr(err))
		acc.lastError = redactErr(err)
	} else {
		acc = mergeUserInfo(acc, info)
		if info != nil && strings.TrimSpace(info.ID) != "" {
			acc.ID = "qoder-" + strings.TrimSpace(info.ID)
		}
		if strings.TrimSpace(spec.Label) == "" {
			acc.Label = defaultAccountLabel(acc)
		}
	}

	if err := c.store.put(acc); err != nil {
		return core.AccountRecord{}, err
	}
	stored, _ := c.store.lookup(acc.ID)
	record := accountRecord(&stored, time.Now().UTC())
	return record, nil
}

func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	return c.store.remove(id)
}

func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return c.store.setEnabled(id, enabled)
}

// ReviveAccount clears this account's runtime penalties and re-enables it.  It
// does not touch the token.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	return c.store.revive(id)
}

// TestAccount sends one minimum-size real chat request through the same gateway
// route normal traffic uses. A model-list or userinfo read only proves the token
// opens the account; it does not prove the account can answer.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("qoder: no account %q", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()

	start := time.Now()
	reply, err := c.probeChat(probeCtx, acc)
	result := core.TestResult{AccountID: id, ElapsedMS: time.Since(start).Milliseconds()}
	if err != nil {
		// A refusal is a result, not an error: only an unknown account id is an
		// error, because that is the one thing the caller could not have known.
		result.Error = redactErr(err)
		if failureKind(err) == core.FailureAuth {
			result.Error = "令牌已被厂商拒绝，请在账号页用「链接重登」重新登录，或从 Qoder CN 客户端重新导入"
		}
		c.penalise(id, err, time.Now().UTC())
		return result, nil
	}

	c.store.reset(id, time.Now().UTC())
	result.OK = true
	result.Model = c.cfg.testModel()
	result.Reply = reply
	return result, nil
}

// RefreshAccount verifies the credentials instead of renewing them.
//
// The refresh token Qoder CN issues is deliberately NOT used: a refresh rotates
// the pair, and rotating it here would silently invalidate the copy the desktop
// client still holds.  Renewing therefore stays an explicit operator action
// (panel browser sign-in or desktop-client import), and this method reports
// honestly so the panel can say "重新登录" when the token is really gone.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	targets := c.store.snapshot()
	if id != "" {
		acc, ok := c.store.lookup(id)
		if !ok {
			return nil, fmt.Errorf("qoder: no account %q", id)
		}
		targets = []account{acc}
	}

	results := make([]core.RefreshResult, 0, len(targets))
	for i := range targets {
		acc := targets[i]
		probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
		_, err := c.up.userInfo(probeCtx, acc.Token)
		cancel()

		if err == nil {
			c.store.reset(acc.ID, time.Now().UTC())
			results = append(results, core.RefreshResult{AccountID: acc.ID, OK: true})
			continue
		}
		if !callerGone(err) {
			c.penalise(acc.ID, err, time.Now().UTC())
		}
		message := redactErr(err)
		if failureKind(err) == core.FailureAuth {
			message = "令牌已失效，请在账号页用「链接重登」重新登录，或从 Qoder CN 客户端重新导入"
		}
		results = append(results, core.RefreshResult{AccountID: acc.ID, OK: false, Error: message})
	}
	return results, nil
}

// mergeUserInfo folds a userinfo answer into an account.
func mergeUserInfo(acc account, info *userInfo) account {
	return mergeAccount(acc, info)
}

func mergeAccount(acc account, info *userInfo) account {
	if info == nil {
		return acc
	}
	if info.ID != "" {
		acc.UserID = info.ID
	}
	if info.Name != "" {
		acc.Name = info.Name
	}
	if info.SecurityMobile != "" {
		acc.Phone = info.SecurityMobile
	}
	return acc
}

// describeIdentity renders the signed-in identity for a test reply.
func describeIdentity(info *userInfo) string {
	if info == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if info.Name != "" {
		parts = append(parts, "账号 "+info.Name)
	}
	if info.SecurityMobile != "" {
		parts = append(parts, "手机号 "+info.SecurityMobile)
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "，") + "）"
}

// accountRecord renders one account for the panel.  It never carries the token.
func accountRecord(a *account, now time.Time) core.AccountRecord {
	record := core.AccountRecord{
		ID:       a.ID,
		Label:    a.Label,
		Enabled:  a.Enabled,
		State:    "ready",
		Identity: a.UserID,
		Fields: map[string]any{
			"origin":    a.origin,
			"removable": a.origin != originConfig,
		},
	}
	if a.UserID != "" {
		record.Fields["user_id"] = a.UserID
	}
	if a.Name != "" {
		record.Fields["name"] = a.Name
	}
	if a.Phone != "" {
		record.Fields["phone"] = a.Phone
	}
	if a.UserType != "" {
		record.Fields["user_type"] = a.UserType
	}
	if a.RefreshToken != "" {
		record.Fields["has_refresh_token"] = true
	}
	if a.ExpiresAtMS > 0 {
		record.ExpiresAt = time.UnixMilli(a.ExpiresAtMS).UTC().Format(time.RFC3339)
	}

	switch {
	case a.dead:
		record.State = "invalid"
		record.Note = "令牌已被厂商拒绝；请在账号页点「链接重登」重新登录"
	case !a.Enabled:
		record.State = "cooling"
		record.Note = "被操作者停用"
	case a.expired(now):
		record.State = "invalid"
		record.Note = "令牌已过期；请在账号页点「链接重登」重新登录"
	case !a.cooldownTill.IsZero() && now.Before(a.cooldownTill):
		record.State = "cooling"
		record.Note = "冷却中，还有 " + humanAge(a.cooldownTill.Sub(now))
	}

	if a.failures > 0 {
		record.Fields["failures"] = a.failures
	}
	if a.lastError != "" {
		record.Fields["last_error"] = a.lastError
	}
	if !a.cooldownTill.IsZero() && now.Before(a.cooldownTill) {
		record.Fields["cooldown_until"] = a.cooldownTill.UTC().Format(time.RFC3339)
	}
	return record
}

// accountStatus is the Status view of an account: the same information
// accountRecord carries, in the shape core.Status wants.
func accountStatus(a *account, now time.Time) core.AccountStatus {
	record := accountRecord(a, now)
	return core.AccountStatus{
		ID:        record.ID,
		Label:     record.Label,
		Enabled:   record.Enabled,
		State:     record.State,
		ExpiresAt: record.ExpiresAt,
		Note:      record.Note,
		Extra:     record.Fields,
		Identity:  record.Identity,
	}
}

// defaultAccountID derives a stable, non-secret id from the credential.  The
// vendor's own user id is preferred; otherwise the token is hashed, which keeps
// the id stable across imports without ever printing the token.
func defaultAccountID(a account) string {
	if a.UserID != "" {
		return "qoder-" + a.UserID
	}
	sum := sha256.Sum256([]byte(a.Token))
	return "qoder-" + hex.EncodeToString(sum[:6])
}

// defaultAccountLabel is what the panel shows when the operator supplied no
// label of their own.
func defaultAccountLabel(a account) string {
	if a.Name != "" {
		return a.Name
	}
	if a.Phone != "" {
		return a.Phone
	}
	return a.ID
}

// humanAge renders a duration the way the other modules do.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
