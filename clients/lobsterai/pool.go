package lobsterai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// accountRecord is one credential, both as it is persisted in accounts.json and
// as the pool holds it in memory.
//
// The health fields (cooldown_until, last_error, err_count) are persisted too:
// a quota cooldown lasts twelve hours, which is longer than any sane restart
// interval, and losing it would send a request straight back into a 402.
type accountRecord struct {
	ID            string `json:"id"`
	Label         string `json:"label,omitempty"`
	UID           string `json:"uid,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	UUID          string `json:"uuid,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	FirstKeyfrom  string `json:"first_keyfrom,omitempty"`
	LatestKeyfrom string `json:"latest_keyfrom,omitempty"`
	Nickname      string `json:"nickname,omitempty"`
	Enabled       bool   `json:"enabled"`
	AddedAt       string `json:"added_at,omitempty"`
	Note          string `json:"note,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	ErrCount      int    `json:"err_count,omitempty"`

	// Runtime-only state.  Unexported, so it never reaches the file, and
	// guarded by the pool's mutex rather than one of its own: an account is
	// only ever touched through the pool.
	inFlight int
	lastUsed time.Time
}

// accountConfig converts a persisted record into the config spelling the URL
// builders want.
func (a *accountRecord) accountConfig() AccountConfig {
	return AccountConfig{
		Label:         a.Label,
		UID:           a.UID,
		UserID:        a.UserID,
		UUID:          a.UUID,
		AccessToken:   a.AccessToken,
		RefreshToken:  a.RefreshToken,
		ExpiresAt:     a.ExpiresAt,
		FirstKeyfrom:  a.FirstKeyfrom,
		LatestKeyfrom: a.LatestKeyfrom,
		Nickname:      a.Nickname,
		Disabled:      !a.Enabled,
	}
}

// accountFromConfig converts a configured credential into a record.  The id is
// derived from the uid, falling back to a hash of the token so a credential
// pasted without a uid still gets a stable name.
func accountFromConfig(cfg AccountConfig, now time.Time) accountRecord {
	uid := firstNonEmpty(cfg.UID, cfg.UserID)
	if uid == "" {
		uid = tokenFingerprint(cfg.AccessToken)
	}
	label := cfg.Label
	if label == "" {
		label = firstNonEmpty(cfg.Nickname, uid)
	}
	return accountRecord{
		ID:            accountID(uid),
		Label:         label,
		UID:           uid,
		UserID:        cfg.UserID,
		UUID:          cfg.UUID,
		AccessToken:   cfg.AccessToken,
		RefreshToken:  cfg.RefreshToken,
		ExpiresAt:     cfg.ExpiresAt,
		FirstKeyfrom:  cfg.FirstKeyfrom,
		LatestKeyfrom: cfg.LatestKeyfrom,
		Nickname:      cfg.Nickname,
		Enabled:       !cfg.Disabled,
		AddedAt:       now.UTC().Format(time.RFC3339),
	}
}

// accountID namespaces a uid so the panel can tell two vendors' "12345" apart.
func accountID(uid string) string { return clientName + ":" + uid }

// tokenFingerprint is the fallback identity: the same trick the vendor's own
// client uses when the login response carries no user id at all.
func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// pool is the in-memory account set.  Every mutation that the panel or the
// health policy performs goes through it, and the Client persists the result.
type pool struct {
	mu    sync.Mutex
	accts []*accountRecord
	rr    int
}

func newPool() *pool { return &pool{} }

// reload replaces the account set, carrying runtime health across for ids that
// survive.  An account that was in flight when the file was reloaded keeps its
// in-flight count, so a reload cannot silently raise the concurrency ceiling.
func (p *pool) reload(recs []accountRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := make(map[string]*accountRecord, len(p.accts))
	for _, a := range p.accts {
		prev[a.ID] = a
	}
	next := make([]*accountRecord, 0, len(recs))
	for i := range recs {
		rec := recs[i]
		if rec.ID == "" {
			continue
		}
		if old, ok := prev[rec.ID]; ok {
			rec.inFlight = old.inFlight
			rec.lastUsed = old.lastUsed
		}
		copied := rec
		next = append(next, &copied)
	}
	p.accts = next
	if p.rr >= len(next) {
		p.rr = 0
	}
}

// snapshot returns a deep copy, safe to marshal or hand to the panel.
func (p *pool) snapshot() []accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]accountRecord, 0, len(p.accts))
	for _, a := range p.accts {
		out = append(out, *a)
	}
	return out
}

func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accts)
}

// ready counts the accounts a request could actually use right now: enabled,
// not cooling down and below the ceiling.
func (p *pool) ready(now time.Time, limit int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, a := range p.accts {
		if p.selectable(a, now, limit) {
			n++
		}
	}
	return n
}

// selectable reports whether an account may take a request.  Callers hold the
// lock.
func (p *pool) selectable(a *accountRecord, now time.Time, limit int) bool {
	if !a.Enabled || a.AccessToken == "" {
		return false
	}
	if a.inFlight >= limit {
		return false
	}
	return !coolingDown(a, now)
}

// coolingDown reports whether an account is parked.  Callers hold the lock.
func coolingDown(a *accountRecord, now time.Time) bool {
	if a.CooldownUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, a.CooldownUntil)
	if err != nil {
		return false
	}
	return until.After(now)
}

// acquire picks the least recently used selectable account and reserves one
// in-flight slot on it.
//
// The three failure modes are distinguished because the gateway maps them to
// different answers: no credential at all is ErrNotConfigured (a 503 that tells
// the operator to log in), a ceiling is ErrBusy (a 429 that tells the client to
// come back), and neither is a plain error.
func (p *pool) acquire(now time.Time, limit int) (*accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	enabled := 0
	for _, a := range p.accts {
		if a.Enabled && a.AccessToken != "" {
			enabled++
		}
	}
	if enabled == 0 {
		if len(p.accts) == 0 {
			return nil, fmt.Errorf("%w: no LobsterAI account; add one from the panel", core.ErrNotConfigured)
		}
		return nil, fmt.Errorf("%w: every LobsterAI account is disabled", core.ErrNotConfigured)
	}

	var chosen *accountRecord
	chosenPriority := 0
	for i := 0; i < len(p.accts); i++ {
		idx := (p.rr + i) % len(p.accts)
		a := p.accts[idx]
		if !p.selectable(a, now, limit) {
			continue
		}
		priority := core.AccountPriority("lobsterai", a.ID)
		if chosen == nil || priority < chosenPriority || (priority == chosenPriority && a.lastUsed.Before(chosen.lastUsed)) {
			chosen = a
			chosenPriority = priority
		}
	}
	if chosen == nil {
		p.rr++
		return nil, fmt.Errorf("%w: every LobsterAI account is cooling down or at its in-flight ceiling", core.ErrBusy)
	}
	chosen.inFlight++
	chosen.lastUsed = now
	p.rr = (p.rr + 1) % len(p.accts)
	return chosen, nil
}

// release returns an in-flight slot.  It is keyed by id rather than by pointer
// so a stream that outlives a reload still balances the books.
func (p *pool) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.find(id); a != nil && a.inFlight > 0 {
		a.inFlight--
	}
}

// find looks an account up.  Callers hold the lock.
func (p *pool) find(id string) *accountRecord {
	for _, a := range p.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// byID is the locked lookup the Client uses.
func (p *pool) byID(id string) *accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.find(id); a != nil {
		copied := *a
		return &copied
	}
	return nil
}

// firstReady returns the first selectable account without reserving it, for
// one-shot operations (models, balance, check-in) that are not chat streams.
func (p *pool) firstReady(now time.Time, limit int) *accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if p.selectable(a, now, limit) {
			copied := *a
			return &copied
		}
	}
	// Fall back to any enabled account: a cooling-down account can still serve
	// an idempotent read, and refusing to report a balance because the account
	// was rate limited an hour ago is worse than trying.
	for _, a := range p.accts {
		if a.Enabled && a.AccessToken != "" {
			copied := *a
			return &copied
		}
	}
	return nil
}

// upsert inserts or replaces a record, keeping the original added_at.
func (p *pool) upsert(rec accountRecord) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.find(rec.ID); existing != nil {
		if rec.AddedAt == "" {
			rec.AddedAt = existing.AddedAt
		}
		rec.inFlight = existing.inFlight
		rec.lastUsed = existing.lastUsed
		*existing = rec
		return true
	}
	copied := rec
	p.accts = append(p.accts, &copied)
	return true
}

func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accts {
		if a.ID == id {
			p.accts = append(p.accts[:i], p.accts[i+1:]...)
			if p.rr >= len(p.accts) {
				p.rr = 0
			}
			return true
		}
	}
	return false
}

func (p *pool) setEnabled(id string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil || a.Enabled == enabled {
		return false
	}
	a.Enabled = enabled
	if enabled {
		a.CooldownUntil = ""
		a.LastError = ""
		a.ErrCount = 0
	}
	return true
}

// updateTokens applies a successful exchange or renewal.
//
// latest_keyfrom is deliberately NOT rewritten here even though a renewal body
// carries it: the vendor's own client replays the stored value, and a field the
// server may validate is not something to invent a new value for.
func (p *pool) updateTokens(id, accessToken, refreshToken, expiresAt string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	if accessToken != "" {
		a.AccessToken = accessToken
	}
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	if expiresAt != "" {
		a.ExpiresAt = expiresAt
	}
	a.CooldownUntil = ""
	a.LastError = ""
	a.ErrCount = 0
	return true
}

// setNote records the operator-visible note for an account.
func (p *pool) setNote(id, note string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil || a.Note == note {
		return false
	}
	a.Note = note
	return true
}

// setCooldown parks an account until the given time.
func (p *pool) setCooldown(id string, until time.Time, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.CooldownUntil = until.UTC().Format(time.RFC3339)
	a.LastError = truncate(reason, 300)
	return true
}

// clearCooldown unpark an account, used when a renewal succeeds.
func (p *pool) clearCooldown(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.CooldownUntil = ""
	a.LastError = ""
	a.ErrCount = 0
	return true
}

// noteError counts a soft failure and parks the account once the count reaches
// the threshold.  It writes no cooldown before then, so a 400 cannot be
// displayed as "rate limited for an hour".
func (p *pool) noteError(id, msg string, now time.Time, threshold int, park time.Duration) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.ErrCount++
	a.LastError = truncate(msg, 300)
	if threshold > 0 && a.ErrCount >= threshold {
		a.CooldownUntil = now.Add(park).UTC().Format(time.RFC3339)
	}
	return true
}

// disable turns an account off after a terminal failure (a rejected refresh
// token).  It writes no cooldown: "disabled" and "rate limited" are different
// states and the panel renders them differently.
func (p *pool) disable(id, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.Enabled = false
	a.LastError = truncate(reason, 300)
	a.CooldownUntil = ""
	return true
}

// stats reports the pool counters the panel shows.
func (p *pool) stats(limit int) (inFlight, full int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		inFlight += a.inFlight
		if a.Enabled && a.AccessToken != "" && a.inFlight >= limit {
			full++
		}
	}
	return inFlight, full
}

// statuses renders the pool for core.Status.
func (p *pool) statuses(now time.Time) []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.accts))
	for _, a := range p.accts {
		st := core.AccountStatus{
			ID:        a.ID,
			Label:     a.Label,
			Enabled:   a.Enabled,
			ExpiresAt: a.ExpiresAt,
			State:     stateOf(a, now),
			Identity:  firstNonEmpty(a.Nickname, a.UID),
		}
		st.Note = noteOf(a, now)
		if a.inFlight > 0 {
			st.Extra = map[string]any{"in_flight": a.inFlight}
		}
		out = append(out, st)
	}
	return out
}

// stateOf maps an account onto the four states the panel understands.
func stateOf(a *accountRecord, now time.Time) string {
	switch {
	case !a.Enabled:
		return "invalid"
	case a.AccessToken == "":
		return "invalid"
	case coolingDown(a, now):
		return "cooling"
	case a.LastError != "":
		return "cooling"
	default:
		return "ready"
	}
}

// noteOf is the one-line explanation shown next to an account.
func noteOf(a *accountRecord, now time.Time) string {
	if !a.Enabled {
		return firstNonEmpty(a.LastError, "disabled")
	}
	if a.CooldownUntil != "" {
		if until, err := time.Parse(time.RFC3339, a.CooldownUntil); err == nil && until.After(now) {
			msg := firstNonEmpty(a.LastError, "cooling down")
			return msg + " (until " + until.UTC().Format(time.RFC3339) + ")"
		}
	}
	return firstNonEmpty(a.LastError, a.Note)
}

// summary is the pool's one-line description for core.Status.Detail.
func (p *pool) summary(now time.Time, limit int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return "no account; log in from the panel"
	}
	enabled, ready, cooling, inflight := 0, 0, 0, 0
	for _, a := range p.accts {
		if a.Enabled && a.AccessToken != "" {
			enabled++
		}
		if p.selectable(a, now, limit) {
			ready++
		} else if coolingDown(a, now) {
			cooling++
		}
		inflight += a.inFlight
	}
	parts := []string{fmt.Sprintf("%d/%d accounts ready", ready, enabled)}
	if cooling > 0 {
		parts = append(parts, fmt.Sprintf("%d cooling", cooling))
	}
	if inflight > 0 {
		parts = append(parts, fmt.Sprintf("%d in flight", inflight))
	}
	return strings.Join(parts, ", ")
}

// credentialsJSON is the file shape.  It carries a version so a future change
// can migrate rather than guess.
type credentialsJSON struct {
	Version  int             `json:"version"`
	Accounts []accountRecord `json:"accounts"`
}

const credentialsVersion = 1

// loadCredentials reads the store.  An absent or unreadable file is not an
// error: it means "no credentials yet", which is exactly what a fresh install
// looks like.
func loadCredentials(path string) []accountRecord {
	var file credentialsJSON
	if err := core.ReadJSON(path, &file); err != nil {
		return nil
	}
	out := make([]accountRecord, 0, len(file.Accounts))
	for _, rec := range file.Accounts {
		if rec.ID == "" {
			continue
		}
		if rec.AddedAt == "" {
			rec.AddedAt = time.Now().UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt < out[j].AddedAt })
	return out
}

// saveCredentials writes the store atomically.
func saveCredentials(path string, recs []accountRecord) error {
	if len(recs) == 0 {
		recs = []accountRecord{}
	}
	return core.WriteJSONAtomic(path, credentialsJSON{Version: credentialsVersion, Accounts: recs})
}

// decodeCredentialsBody parses a pasted credential blob: either the store's own
// {"version":..,"accounts":[...]} shape, a bare array, a single object, or the
// vendor's nested {auth:{...},account:{...}} file.  It exists so an operator can
// import what the vendor's own client wrote without hand-editing it.
func decodeCredentialsBody(raw []byte) ([]AccountConfig, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty credential payload")
	}
	switch trimmed[0] {
	case '[':
		var list []AccountConfig
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			return nil, fmt.Errorf("credential list: %w", err)
		}
		return list, nil
	case '{':
		var file credentialsJSON
		if err := json.Unmarshal([]byte(trimmed), &file); err == nil && len(file.Accounts) > 0 {
			out := make([]AccountConfig, 0, len(file.Accounts))
			for _, rec := range file.Accounts {
				out = append(out, rec.accountConfig())
			}
			return out, nil
		}
		var nested struct {
			Auth    *AccountConfig `json:"auth"`
			Account *struct {
				UID      any    `json:"uid"`
				UserID   any    `json:"userId"`
				YID      any    `json:"yid"`
				Nickname string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal([]byte(trimmed), &nested); err == nil && nested.Auth != nil {
			acct := *nested.Auth
			if nested.Account != nil {
				acct.UID = firstNonEmpty(asString(nested.Account.UID), asString(nested.Account.UserID), asString(nested.Account.YID))
				acct.Nickname = firstNonEmpty(acct.Nickname, nested.Account.Nickname)
			}
			return []AccountConfig{acct}, nil
		}
		var single AccountConfig
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return nil, fmt.Errorf("credential object: %w", err)
		}
		return []AccountConfig{single}, nil
	default:
		return nil, fmt.Errorf("credential payload must be a JSON object or array")
	}
}
