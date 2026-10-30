package loomy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// accounts.go holds the credential store, the pool, and the panel-facing
// AccountManager implementation.
//
// Loomy has no refresh endpoint, so this file's whole job is to keep the truth
// straight: a session is either working, or it is past the 14-day life the
// vendor gives it, or the vendor has already said no.  Nothing here ever mints
// a credential.

const (
	originConfig = "config"
	originStored = "stored"

	minBackgroundProbeDelay = 3 * time.Hour
	backgroundProbeJitter   = 3 * time.Hour

	// sessionDeadFailureLimit is the historical 100002 threshold.  Chat now
	// uses the shorter cooldown ladder below for every real-chat failure; the
	// constant remains for legacy state and side-channel callers.
	sessionDeadFailureLimit = 3

	// Chat failures use a short retry followed by a short, escalating
	// cooldown.  The fourth failed round is terminal: the account is marked
	// abnormal instead of being handed another cooldown.
	chatCooldownStage1 = 30 * time.Second
	chatCooldownStage2 = time.Minute
	chatCooldownStage3 = 2 * time.Minute
)

// chatRetryDelay is the pause before the one same-account retry.  It is a
// variable so tests do not have to sleep for five seconds.
var chatRetryDelay = 5 * time.Second

func chatCooldownForStage(stage int) (time.Duration, bool) {
	switch stage {
	case 1:
		return chatCooldownStage1, true
	case 2:
		return chatCooldownStage2, true
	case 3:
		return chatCooldownStage3, true
	default:
		return 0, false
	}
}

// backgroundProbeDelay spreads recovery probes over a 3..6 hour window.
// The variable is deliberately injectable from tests; production uses the
// package RNG, so the sweep does not develop a fixed rhythm.
var backgroundProbeDelay = func() time.Duration {
	return minBackgroundProbeDelay + time.Duration(rand.Int63n(int64(backgroundProbeJitter)))
}

// storedAccount is one session as it is written to disk.  The token is the only
// secret in this file and it is written with 0600 through the atomic writer.
type storedAccount struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	AccessToken string `json:"access_token"`
	UserID      string `json:"userid,omitempty"`
	Phone       string `json:"phone,omitempty"`
	Nickname    string `json:"nickname,omitempty"`
	// ExpiresAtMS is the millisecond timestamp the credential expires at, or 0
	// for "unknown".  0 is not expired: the vendor is the authority on whether a
	// session still works, and refusing to try a credential on a guess is worse
	// than letting the server answer 100002.
	ExpiresAtMS int64 `json:"expires_at_ms,omitempty"`
	Enabled     bool  `json:"enabled"`
}

type accountFile struct {
	Accounts []storedAccount `json:"accounts"`
}

// accountState is the runtime half, persisted separately so that a restart does
// not silently turn a rejected session back into a healthy-looking one.
type accountState struct {
	Dead            bool   `json:"dead,omitempty"`
	Abnormal        bool   `json:"abnormal,omitempty"`
	Failures        int    `json:"failures,omitempty"`
	SessionFailures int    `json:"session_failures,omitempty"`
	CooldownStage   int    `json:"cooldown_stage,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	CooldownTill    string `json:"cooldown_until,omitempty"` // RFC3339
	ProbeAfter      string `json:"probe_after,omitempty"`    // RFC3339
	LastUsed        string `json:"last_used,omitempty"`      // RFC3339
	LowCredit       bool   `json:"low_credit,omitempty"`
	// InviteCode/InviteActivated memoize the last definitive activation answer
	// so the panel can show what the account is bound to without asking the
	// vendor on every render.
	InviteCode      string `json:"invite_code,omitempty"`
	InviteActivated bool   `json:"invite_activated,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
}

// account is one credential plus the penalties the pool has accumulated for it.
type account struct {
	storedAccount
	origin string

	// Runtime state.  None of it is a credential, and none of it survives a
	// revive.
	failures        int
	sessionFailures int
	cooldownStage   int
	cooldownTill    time.Time
	probeAfter      time.Time
	lowCredit       bool
	dead            bool
	abnormal        bool
	lastError       string
	lastUsed        time.Time
	inviteCode      string
	inviteActivated bool
}

// selectable reports whether the pool may hand this account a request.
//
// A credential past its declared expiry is still selectable -- it is just
// ranked last -- because the only authority on whether a session works is the
// vendor.  A credential the vendor has already rejected is not: retrying it on
// every request would turn one honest failure into a stream of them.
func (a *account) selectable(now time.Time) bool {
	if !a.Enabled || a.dead || a.abnormal || a.lowCredit {
		return false
	}
	if strings.TrimSpace(a.AccessToken) == "" {
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
		return s, fmt.Errorf("loomy: reading %s: %w", path, err)
	}
	for _, sa := range file.Accounts {
		if strings.TrimSpace(sa.AccessToken) == "" {
			continue
		}
		s.all = append(s.all, &account{storedAccount: sa, origin: originStored})
	}

	var states stateFile
	if err := readJSONIfPresent(statePath, &states); err != nil {
		return s, fmt.Errorf("loomy: reading %s: %w", statePath, err)
	}
	migratedState := false
	now := time.Now().UTC()
	for _, a := range s.all {
		st, ok := states.Accounts[a.ID]
		if !ok {
			continue
		}
		a.dead = st.Dead
		a.abnormal = st.Abnormal
		a.failures = st.Failures
		a.sessionFailures = st.SessionFailures
		a.cooldownStage = st.CooldownStage
		a.lastError = st.LastError
		a.inviteCode = st.InviteCode
		a.inviteActivated = st.InviteActivated
		a.cooldownTill = parseRFC3339(st.CooldownTill)
		a.probeAfter = parseRFC3339(st.ProbeAfter)

		// Older versions parked an account permanently on the first chat-path
		// 100002.  That verdict was too eager: migrate it to one recorded
		// session failure and an ordinary cooldown so a working session can
		// recover.  States written by the new terminal threshold still carry
		// session_failures and remain parked.
		if st.Dead && st.SessionFailures == 0 {
			a.dead = false
			a.sessionFailures = 1
			a.cooldownTill = now.Add(defaultCooldown)
			migratedState = true
		}
		if a.probeAfter.IsZero() && !a.dead && !a.cooldownTill.IsZero() {
			a.probeAfter = now.Add(backgroundProbeDelay())
			migratedState = true
		}
		a.lowCredit = st.LowCredit
		a.lastUsed = parseRFC3339(st.LastUsed)
	}
	if migratedState {
		_ = s.saveState()
	}
	return s, nil
}

func readJSONIfPresent(path string, v any) error {
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
//
// With no path there is no DataDir, so nothing is persisted anywhere: the pool
// still works for the life of the process and simply does not survive a
// restart.  Returning early keeps a store built without a directory from
// failing every write.
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
	file := stateFile{Accounts: map[string]accountState{}}
	for _, a := range s.all {
		if !a.dead && !a.abnormal && a.failures == 0 && a.sessionFailures == 0 && a.cooldownStage == 0 && a.lastError == "" && a.cooldownTill.IsZero() && a.probeAfter.IsZero() && a.lastUsed.IsZero() && !a.lowCredit && a.inviteCode == "" && !a.inviteActivated {
			continue
		}
		st := accountState{
			Dead:            a.dead,
			Abnormal:        a.abnormal,
			Failures:        a.failures,
			SessionFailures: a.sessionFailures,
			CooldownStage:   a.cooldownStage,
			LastError:       a.lastError,
			LowCredit:       a.lowCredit,
			InviteCode:      a.inviteCode,
			InviteActivated: a.inviteActivated,
		}
		if !a.cooldownTill.IsZero() {
			st.CooldownTill = a.cooldownTill.UTC().Format(time.RFC3339)
		}
		if !a.probeAfter.IsZero() {
			st.ProbeAfter = a.probeAfter.UTC().Format(time.RFC3339)
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
				// Preserve the runtime penalties: editing the label in a config
				// file must not silently pardon a session the vendor rejected.
				acc.failures = existing.failures
				acc.sessionFailures = existing.sessionFailures
				acc.cooldownStage = existing.cooldownStage
				acc.cooldownTill = existing.cooldownTill
				acc.probeAfter = existing.probeAfter
				acc.lowCredit = existing.lowCredit
				acc.dead = existing.dead
				acc.abnormal = existing.abnormal
				acc.lastError = existing.lastError
				acc.lastUsed = existing.lastUsed
				acc.inviteCode = existing.inviteCode
				acc.inviteActivated = existing.inviteActivated
				// A configured expiry is only a local estimate.  If a successful vendor
				// call happened after that timestamp, the state file records it in
				// lastUsed; do not let a restart resurrect the stale, disproved date.
				if acc.ExpiresAtMS > 0 && !existing.lastUsed.IsZero() && existing.lastUsed.UnixMilli() >= acc.ExpiresAtMS {
					acc.ExpiresAtMS = 0
				}
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

// snapshot returns value copies so callers never touch live state.
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
// the pool wants them tried: least recently used first, with credentials past
// their declared expiry ranked last.
//
// Expiry only reorders.  The vendor is the only authority on whether a session
// still works, so a session past the date the operator wrote down is still
// tried -- last -- rather than being withheld on a guess.
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
	byAge := func(list []*account) {
		sort.SliceStable(list, func(i, j int) bool {
			return list[i].lastUsed.Before(list[j].lastUsed)
		})
	}
	byAge(fresh)
	byAge(stale)

	out := make([]account, 0, len(fresh)+len(stale))
	ordered := append(fresh, stale...)
	best, found := 0, false
	for _, a := range ordered {
		prio := core.AccountPriority("loomy", a.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return nil
	}
	for _, a := range ordered {
		if core.AccountPriority("loomy", a.ID) != best {
			continue
		}
		out = append(out, *a)
	}
	return out
}

// firstExpired returns the most recent expired account, used to explain why a
// request could not be served.
func (s *store) firstExpired(now time.Time) (account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.dead || a.abnormal || a.expired(now) {
			return *a, true
		}
	}
	return account{}, false
}

// backgroundProbeDue gates vendor probes started by shared background sweeps.
// A penalised account gets one probe per randomised quiet window; an explicit
// operator action does not call this method and is never delayed.
func (s *store) backgroundProbeDue(id string, now time.Time, delay func() time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		if !a.Enabled || a.dead || a.abnormal || strings.TrimSpace(a.AccessToken) == "" || a.expired(now) {
			return false
		}
		if a.lowCredit {
			return false
		}
		if now.Before(a.cooldownTill) {
			return false
		}
		if a.probeAfter.IsZero() {
			return true
		}
		if now.Before(a.probeAfter) {
			return false
		}
		next := backgroundProbeDelay()
		if delay != nil {
			next = delay()
		}
		if next < minBackgroundProbeDelay {
			next = minBackgroundProbeDelay
		}
		a.probeAfter = now.Add(next)
		_ = s.saveState()
		return true
	}
	return false
}

// penalise records an authoritative chat-path failure.  A rejected session is
// parked for good; anything else gets a cooldown, because a rate limit or a
// network hiccup is temporary.
func (s *store) penalise(id string, err error, now time.Time, cooldown time.Duration) {
	s.penaliseWith(id, err, now, cooldown, true)
}

// penaliseChat records a real chat-path failure.  A session-dead response is
// cooled down first; only a run of consecutive real-chat failures without a
// success in between is strong enough to park the credential permanently.
func (s *store) penaliseChat(id string, err error, now time.Time, cooldown time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.failures++
		a.lastError = redactErr(err)
		a.sessionFailures = 0
		nextStage := a.cooldownStage + 1
		if delay, ok := chatCooldownForStage(nextStage); ok {
			a.cooldownStage = nextStage
			a.cooldownTill = now.Add(delay)
			a.probeAfter = now.Add(backgroundProbeDelay())
			if saveErr := s.saveState(); saveErr != nil {
				_ = saveErr
			}
			return
		}
		a.abnormal = true
		a.cooldownTill = time.Time{}
		a.probeAfter = time.Time{}
		if saveErr := s.saveState(); saveErr != nil {
			_ = saveErr
		}
		return
	}
}

// penaliseChatWithStage records one failed round using the caller-supplied
// stage.  The stage is read and written under the store lock so concurrent
// requests for one account cannot overwrite each other's successful reset.
func (s *store) penaliseChatWithStage(id string, err error, now time.Time, stage int, cooldown time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		if a.cooldownStage != stage {
			return false
		}
		a.failures++
		a.lastError = redactErr(err)
		a.sessionFailures = 0
		nextStage := a.cooldownStage + 1
		if delay, ok := chatCooldownForStage(nextStage); ok {
			a.cooldownStage = nextStage
			a.cooldownTill = now.Add(delay)
			a.probeAfter = now.Add(backgroundProbeDelay())
		} else {
			a.abnormal = true
			a.cooldownTill = time.Time{}
			a.probeAfter = time.Time{}
		}
		if saveErr := s.saveState(); saveErr != nil {
			_ = saveErr
		}
		return true
	}
	return false
}

// markChatCreditExhausted records the second, confirmed credit refusal on the
// chat path. Unlike penaliseChatWithStage this is not a timed cooldown: it is
// the same durable low-credit park a zero balance read creates, so the account
// cannot re-enter rotation merely because a short transient cooldown lapsed.
func (s *store) markChatCreditExhausted(id string, err error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.lowCredit = true
		a.lastError = redactErr(err)
		// A funding verdict supersedes a transient chat-ladder park. Keep the
		// failure count for the panel's diagnostic history, but clear the
		// timed cooldown and its stage so lowCredit is the only reason this
		// account is parked.
		a.cooldownStage = 0
		a.cooldownTill = time.Time{}
		a.probeAfter = time.Time{}
		a.sessionFailures = 0
		if saveErr := s.saveState(); saveErr != nil {
			_ = saveErr
		}
		return true
	}
	return false
}

func (s *store) cooldownStage(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID == id {
			return a.cooldownStage
		}
	}
	return 0
}

// penaliseRecoverable records a failure from a side channel such as a
// catalogue refresh, task report or heartbeat.  Those calls are useful
// evidence, but they are not authoritative enough to label a credential
// permanently dead: the same session can answer a real chat request moments
// later.  A session-dead answer here therefore gets an ordinary cooldown and
// stays eligible for a real front-end probe.
func (s *store) penaliseRecoverable(id string, err error, now time.Time, cooldown time.Duration) {
	s.penaliseWith(id, err, now, cooldown, false)
}

func (s *store) penaliseWith(id string, err error, now time.Time, cooldown time.Duration, terminalSessionDead bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.failures++
		a.lastError = redactErr(err)
		if terminalSessionDead && failureKind(err) == core.FailureSessionDead {
			a.dead = true
			a.sessionFailures = sessionDeadFailureLimit
			a.probeAfter = time.Time{}
		} else {
			a.cooldownTill = now.Add(cooldown)
			a.probeAfter = now.Add(backgroundProbeDelay())
		}
		if saveErr := s.saveState(); saveErr != nil {
			// Losing the state file costs honesty across a restart, never
			// correctness now, so it is reported and the request continues.
			_ = saveErr
		}
		return
	}
}

// reset clears the penalties after a successful call.
//
// A successful vendor call is stronger evidence than the locally-computed
// expiry: the requested 14-day lifetime is not returned or renewed by Loomy,
// and the server can keep accepting a session after that local timestamp.  If
// the stored expiry has already passed, drop it so the account is treated as
// expiry-unknown from now on; keeping it would leave a working account marked
// abnormal forever.
func (s *store) reset(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		clearExpiry := a.expired(now)
		a.failures = 0
		a.sessionFailures = 0
		a.cooldownStage = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.probeAfter = time.Time{}
		a.lastUsed = now
		if clearExpiry {
			a.ExpiresAtMS = 0
			if err := s.save(); err != nil {
				_ = err
			}
		}
		if err := s.saveState(); err != nil {
			_ = err
		}
		return
	}
}

// resetAfterProbe clears the runtime penalties after a successful real model
// probe or chat stream.  Unlike reset, it also clears dead: a successful
// completion is stronger evidence than an earlier vendor rejection.  Keepalive
// deliberately keeps using reset, because accepting a session is weaker than
// answering chat.
func (s *store) resetAfterProbe(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		clearExpiry := a.expired(now)
		a.dead = false
		a.abnormal = false
		a.failures = 0
		a.sessionFailures = 0
		a.cooldownStage = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.probeAfter = time.Time{}
		a.lastUsed = now
		if clearExpiry {
			a.ExpiresAtMS = 0
			if err := s.save(); err != nil {
				_ = err
			}
		}
		if err := s.saveState(); err != nil {
			_ = err
		}
		return
	}
}

// revive clears every runtime penalty and re-enables the account.  It never
// touches the credential.
func (s *store) revive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		a.dead = false
		a.abnormal = false
		a.failures = 0
		a.sessionFailures = 0
		a.cooldownStage = 0
		a.lastError = ""
		a.cooldownTill = time.Time{}
		a.probeAfter = time.Time{}
		a.lowCredit = false
		a.Enabled = true
		if err := s.save(); err != nil {
			return err
		}
		return s.saveState()
	}
	return fmt.Errorf("loomy: no account %q", id)
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
	return fmt.Errorf("loomy: no account %q", id)
}

// applyBalance records the last balance verdict against the platform's
// low-balance guard.  It owns only the dedicated lowCredit park: a rate-limit
// or session cooldown keeps its own clock, and a successful balance read must
// not clear it by accident.
func (s *store) applyBalance(id string, credits int64, reserve int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		if reserve < 0 {
			if !a.lowCredit {
				return false
			}
			a.lowCredit = false
			_ = s.saveState()
			return true
		}
		if credits <= int64(reserve) {
			if a.lowCredit {
				return false
			}
			a.lowCredit = true
			_ = s.saveState()
			return true
		}
		if !a.lowCredit {
			return false
		}
		a.lowCredit = false
		_ = s.saveState()
		return true
	}
	return false
}

// clearLowCredit removes only the low-balance park.  It is used when the
// operator disables the guard at run time: the account may still be cooling
// from a separate vendor verdict, and that clock must stay untouched.
func (s *store) clearLowCredit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cleared := 0
	for _, a := range s.all {
		if !a.lowCredit {
			continue
		}
		a.lowCredit = false
		cleared++
	}
	if cleared > 0 {
		_ = s.saveState()
	}
	return cleared
}

// setInviteState remembers the last definitive invite binding for an account.
// It is display metadata, not a credential; the vendor remains the authority
// and the next status call replaces it.
func (s *store) setInviteState(id, code string, activated bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.all {
		if a.ID != id {
			continue
		}
		code = strings.TrimSpace(code)
		if a.inviteCode == code && a.inviteActivated == activated {
			return
		}
		a.inviteCode = code
		a.inviteActivated = activated
		_ = s.saveState()
		return
	}
}

func (s *store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.all {
		if a.ID != id {
			continue
		}
		if a.origin == originConfig {
			return fmt.Errorf("loomy: account %q comes from the configuration and cannot be removed here", id)
		}
		s.all = append(s.all[:i], s.all[i+1:]...)
		if err := s.save(); err != nil {
			return err
		}
		return s.saveState()
	}
	return fmt.Errorf("loomy: no account %q", id)
}

// put adds or replaces one account and persists it.
func (s *store) put(acc account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.all {
		if a.ID == acc.ID {
			acc.origin = a.origin
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

// penalise records a real chat-path failure.  A session-dead answer is cooled
// down first; the store only parks it after a run of consecutive failures.
func (c *Client) penalise(id string, err error, now time.Time) {
	c.store.penaliseChat(id, err, now, c.cfg.cooldown())
}

// penaliseRecoverable records evidence from a side channel without letting it
// permanently park the credential.  A genuine dead session will still be
// discovered by the next real chat request; a transient false alarm here must
// not disable an account that is answering chat successfully.
func (c *Client) penaliseRecoverable(id string, err error, now time.Time) {
	c.store.penaliseRecoverable(id, err, now, c.cfg.cooldown())
}

// applyBalanceGuard parks or revives an account after a balance read.  It only
// owns the low-credit park, so a balance refresh cannot accidentally clear a
// rate-limit or session cooldown.
func (c *Client) applyBalanceGuard(id string, credits int64) bool {
	if c == nil || c.store == nil {
		return false
	}
	return c.store.applyBalance(id, credits, c.reserveCreditLimit())
}

// logf is a nil-safe logger, so a Client built by hand in a test needs no Deps.
func (c *Client) logf(format string, args ...any) {
	if c.deps.Logf != nil {
		c.deps.Logf(format, args...)
	}
}

func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "access_token",
			Label:       "Session token",
			Type:        "password",
			Required:    true,
			Placeholder: "32 lowercase hex characters",
			Help: "The `session` value the Loomy client stores after an SMS or " +
				"WeChat login. It is the only secret this module needs.",
		},
		{
			Key:         "userid",
			Label:       "User id",
			Type:        "text",
			Placeholder: "18 digits",
			Help:        "Optional. Shown as the account identity and used to group duplicates.",
		},
		{
			Key:         "phone",
			Label:       "Phone",
			Type:        "text",
			Placeholder: "11 digits",
			Help:        "Optional, for the operator's own reference.",
		},
		{
			Key:   "nickname",
			Label: "Nickname",
			Type:  "text",
		},
		{
			Key:   "expires_at",
			Label: "Expires at",
			Type:  "text",
			Help: "Optional. A millisecond timestamp, a Unix timestamp, or a date. " +
				"Loomy sessions last 14 days from login and the server never returns the " +
				"expiry, so if you leave this blank the module treats it as unknown and " +
				"lets the server decide.",
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

// AddAccount imports a session token by hand.
//
// The token is probed before it is stored, because importing a session the
// vendor has already rejected would produce an account that can never work.
// A transport failure is NOT a rejection: the account is stored and the failure
// is reported in its note, so a flaky network cannot block an import.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	token := strings.TrimSpace(spec.Fields["access_token"])
	if token == "" {
		return core.AccountRecord{}, errors.New("loomy: a session token is required")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return core.AccountRecord{}, errors.New("loomy: the session token contains whitespace; paste it without spaces")
	}

	acc := account{
		storedAccount: storedAccount{
			Label:       strings.TrimSpace(spec.Label),
			AccessToken: token,
			UserID:      strings.TrimSpace(spec.Fields["userid"]),
			Phone:       strings.TrimSpace(spec.Fields["phone"]),
			Nickname:    strings.TrimSpace(spec.Fields["nickname"]),
			ExpiresAtMS: parseExpiryMS(spec.Fields["expires_at"]),
			Enabled:     spec.EnabledOr(true),
		},
		origin: originStored,
	}
	acc.ID = strings.TrimSpace(spec.ID)
	if acc.ID == "" {
		acc.ID = defaultAccountID(acc)
	}
	if acc.Label == "" {
		acc.Label = acc.ID
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	err := c.up.probeCredential(probeCtx, token)
	cancel()
	if err != nil {
		if failureKind(err) == core.FailureSessionDead {
			return core.AccountRecord{}, fmt.Errorf(
				"loomy: the vendor rejected this session (%s); check the token", redactErr(err))
		}
		c.logf("loomy: could not verify the new account %s before storing it: %s", acc.ID, redactErr(err))
		acc.lastError = redactErr(err)
	}

	if err := c.store.put(acc); err != nil {
		return core.AccountRecord{}, err
	}
	// A hand-pasted session is still an account the operator wants on the
	// shared invite code.  Binding is best-effort: the credential is already
	// stored, and a vendor refusal is visible/retryable in the accounts page.
	_, _ = c.bindInviteAfterLogin(ctx, acc.ID, c.logf)
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
// does not touch the token: a revived session the vendor has really killed is
// expected to fail on its next use, and that failure is the evidence the pool
// wants.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	return c.store.revive(id)
}

// BackgroundProbeDue implements core.BackgroundProbeGate.
func (c *Client) BackgroundProbeDue(_ context.Context, id string) bool {
	return c.store.backgroundProbeDue(id, time.Now().UTC(), backgroundProbeDelay)
}

// TestAccount sends one minimum-size real chat completion through the account
// under test, so only an account that can actually answer is reported as OK.
func (c *Client) TestAccountModel(ctx context.Context, id, wantModel string) (core.TestResult, error) {
	acc, ok := c.store.lookup(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("loomy: no account %q", id)
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	defer cancel()

	start := time.Now()
	model := strings.TrimSpace(wantModel)
	if model == "" {
		model = probeModel(c.cachedOrFallback())
	}
	reply, err := c.probeChat(probeCtx, acc, model)
	elapsed := time.Since(start).Milliseconds()

	result := core.TestResult{AccountID: id, Model: model, ElapsedMS: elapsed}
	if err != nil {
		// A refusal is a result, not an error: only an unknown account id is an
		// error, because that is the one thing the caller could not have known.
		result.Error = redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			result.Error = "the session is expired or rejected; log in again and import the new token"
		}
		return result, nil
	}

	result.OK = true
	result.Reply = reply
	c.store.resetAfterProbe(id, time.Now().UTC())
	return result, nil
}

// TestAccount keeps the pre-model signature: it probes with the module's own
// default model, which is what every caller before the model picker meant.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	return c.TestAccountModel(ctx, id, "")
}

// probeModel names the first catalogue model, which is the cheapest probe
// target Loomy exposes.
func probeModel(models []core.Model) string {
	if len(models) == 0 {
		return ""
	}
	return models[0].ID
}

// probeChat sends one minimum-size real completion and returns the first text
// it produced.
//
// A credential read proves the session is accepted; only a completion proves
// the account can actually answer, which is what the panel's 测试 button
// asks.
func (c *Client) probeChat(ctx context.Context, acc account, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("loomy: no model is available to probe with")
	}
	maxTokens := 16
	body, err := buildBody(&core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		return "", err
	}
	stream, err := c.up.openChatStream(ctx, acc.AccessToken, body)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var reply strings.Builder
	reasoningOnly := false
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if reply.Len() > 0 {
				return reply.String(), nil
			}
			return "", err
		}
		switch event.Type {
		case core.EventDelta:
			reply.WriteString(event.Delta)
			if event.Reasoning != "" {
				reasoningOnly = true
			}
		case core.EventError:
			if event.Err != nil {
				if reply.Len() > 0 {
					return reply.String(), nil
				}
				return "", event.Err
			}
		case core.EventDone:
			// keep draining until the stream really ends
		}
	}
	if reply.Len() == 0 {
		if reasoningOnly {
			return "reasoning-only probe response", nil
		}
		return "", fmt.Errorf("loomy: %s answered without any text", model)
	}
	return reply.String(), nil
}

// RefreshAccount checks the credentials instead of renewing them.
//
// Loomy has no refresh endpoint: the session the vendor issues at login lives
// 14 days and then it is over.  This method therefore VERIFIES and reports
// honestly, which is what the panel needs in order to say "log in again".
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	targets := c.store.snapshot()
	if id != "" {
		acc, ok := c.store.lookup(id)
		if !ok {
			return nil, fmt.Errorf("loomy: no account %q", id)
		}
		targets = []account{acc}
	}

	results := make([]core.RefreshResult, 0, len(targets))
	for i := range targets {
		acc := targets[i]
		probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
		err := c.up.probeCredential(probeCtx, acc.AccessToken)
		cancel()

		if err == nil {
			c.store.reset(acc.ID, time.Now().UTC())
			results = append(results, core.RefreshResult{AccountID: acc.ID, OK: true})
			continue
		}
		// A probe the caller cancelled says nothing about the credential: the
		// panel cancels a refresh in flight when the operator navigates away.
		// Our own probe deadline is a WithTimeout child and arrives as
		// DeadlineExceeded, so it still parks the account -- see callerGone.
		//
		// The cancel() above cannot be the cause: it runs after the probe has
		// already returned.
		if !callerGone(err) {
			c.penaliseRecoverable(acc.ID, err, time.Now().UTC())
		}
		message := redactErr(err)
		if failureKind(err) == core.FailureSessionDead {
			message = "the session has expired and Loomy has no refresh endpoint; log in again and import the new token"
		}
		results = append(results, core.RefreshResult{AccountID: acc.ID, OK: false, Error: message})
	}
	return results, nil
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
		record.Fields["userid"] = a.UserID
	}
	if a.Phone != "" {
		record.Fields["phone"] = a.Phone
	}
	if a.Nickname != "" {
		record.Fields["nickname"] = a.Nickname
	}
	if a.ExpiresAtMS > 0 {
		record.ExpiresAt = time.UnixMilli(a.ExpiresAtMS).UTC().Format(time.RFC3339)
	}

	switch {
	case a.dead:
		record.State = "invalid"
		record.Note = "the vendor rejected this session; log in again and import the new token"
	case a.abnormal:
		record.State = "invalid"
		record.Note = "the account was marked abnormal after repeated chat failures; revive it after fixing the cause, or log in again"
	case !a.Enabled:
		record.State = "cooling"
		record.Note = "parked by the operator"
	case a.expired(now):
		record.State = "invalid"
		record.Note = "the session expired (Loomy sessions last 14 days and cannot be renewed); " +
			"log in again and import the new token"
	case a.lowCredit:
		record.State = "cooling"
		record.Note = "credit balance is at or below the reserve; waiting for a funded " +
			"balance refresh or daily check-in"
		record.Fields["low_credit"] = true
	case !a.cooldownTill.IsZero() && now.Before(a.cooldownTill):
		record.State = "cooling"
		record.Note = "cooling down for another " + humanAge(a.cooldownTill.Sub(now))
	}

	if a.failures > 0 {
		record.Fields["failures"] = a.failures
	}
	if a.sessionFailures > 0 {
		record.Fields["session_failures"] = a.sessionFailures
	}
	if a.lastError != "" {
		record.Fields["last_error"] = a.lastError
	}
	if !a.cooldownTill.IsZero() && now.Before(a.cooldownTill) {
		record.Fields["cooldown_until"] = a.cooldownTill.UTC().Format(time.RFC3339)
	}
	if a.ExpiresAtMS == 0 {
		record.Fields["expiry_known"] = false
	}
	if a.inviteCode != "" {
		record.Fields["invite_code"] = a.inviteCode
		record.Fields["invite_activated"] = a.inviteActivated
	}
	return record
}

// accountStatus is the Status view of an account: exactly the information
// accountRecord carries, in the shape core.Status wants (Extra rather than
// Fields).  Keeping one builder for both means the panel and the status line can
// never disagree about whether a session is dead.
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
// user id is preferred because it is what the panel groups on; otherwise the
// token is hashed, which keeps the id stable across imports without ever
// printing the token.
func defaultAccountID(a account) string {
	if a.UserID != "" {
		return "loomy-" + a.UserID
	}
	sum := sha256.Sum256([]byte(a.AccessToken))
	return "loomy-" + hex.EncodeToString(sum[:6])
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
