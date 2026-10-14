package gemini

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

// accountRecord is one credential, both as persisted and as the pool holds it.
// Health fields are persisted too: a quota cooldown lasts twelve hours, far
// longer than a restart, and losing it would send the next request straight
// back into a 429.
type accountRecord struct {
	ID string `json:"id"`
	// Label is the display name (email when known).
	Label string `json:"label,omitempty"`
	// Identity is the account's stable id (OIDC sub).  It is what lets the
	// panel group credentials that belong to the same Google account.
	Identity string `json:"identity,omitempty"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Expiry       string `json:"expiry,omitempty"`
	Sub          string `json:"sub,omitempty"`
	Email        string `json:"email,omitempty"`
	Project      string `json:"cloudaicompanionProject,omitempty"`

	Enabled       bool   `json:"enabled"`
	AddedAt       string `json:"added_at,omitempty"`
	Note          string `json:"note,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	ErrCount      int    `json:"err_count,omitempty"`

	// Tier is the account's Google tier (Pro/Free), a display-only note.
	Tier string `json:"tier,omitempty"`

	// Runtime-only state, guarded by the pool's mutex.
	inFlight int
	lastUsed time.Time
}

func (a *accountRecord) accountConfig() AccountConfig {
	return AccountConfig{
		Label:              a.Label,
		AccessToken:        a.AccessToken,
		RefreshToken:       a.RefreshToken,
		TokenType:          a.TokenType,
		ExpiresIn:          a.ExpiresIn,
		Scope:              a.Scope,
		Expiry:             a.Expiry,
		Sub:                a.Sub,
		Email:              a.Email,
		CloudAICodeProject: a.Project,
		Disabled:           !a.Enabled,
	}
}

// accountFromConfig turns a configured credential into a record.
func accountFromConfig(cfg AccountConfig, now time.Time) accountRecord {
	identity := firstNonEmpty(cfg.Sub, cfg.Email, tokenFingerprint(cfg.AccessToken))
	if identity == "" {
		identity = tokenFingerprint(cfg.RefreshToken)
	}
	label := firstNonEmpty(cfg.Label, cfg.Email, cfg.Sub, identity)
	return accountRecord{
		ID:           accountID(identity),
		Label:        label,
		Identity:     identity,
		AccessToken:  cfg.AccessToken,
		RefreshToken: cfg.RefreshToken,
		TokenType:    cfg.TokenType,
		ExpiresIn:    cfg.ExpiresIn,
		Scope:        cfg.Scope,
		Expiry:       cfg.Expiry,
		Sub:          cfg.Sub,
		Email:        cfg.Email,
		Project:      cfg.CloudAICodeProject,
		Enabled:      !cfg.Disabled,
		AddedAt:      now.UTC().Format(time.RFC3339),
	}
}

func accountID(identity string) string { return clientName + ":" + identity }

func tokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:16]
}

// --- pool -------------------------------------------------------------------

type pool struct {
	mu    sync.Mutex
	accts []*accountRecord
	rr    int
}

func newPool() *pool { return &pool{} }

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

func (p *pool) selectable(a *accountRecord, now time.Time, limit int) bool {
	if !a.Enabled || a.AccessToken == "" {
		return false
	}
	if limit > 0 && a.inFlight >= limit {
		return false
	}
	return !coolingDown(a, now)
}

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

// acquire picks a selectable account, honouring operator priority, and reserves
// one in-flight slot.
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
			return nil, fmt.Errorf("%w: no Gemini account; sign in from the panel", core.ErrNotConfigured)
		}
		return nil, fmt.Errorf("%w: every Gemini account is disabled", core.ErrNotConfigured)
	}

	var chosen *accountRecord
	chosenPriority := 0
	for i := 0; i < len(p.accts); i++ {
		idx := (p.rr + i) % len(p.accts)
		a := p.accts[idx]
		if !p.selectable(a, now, limit) {
			continue
		}
		priority := core.AccountPriority(clientName, a.ID)
		if chosen == nil || priority < chosenPriority || (priority == chosenPriority && a.lastUsed.Before(chosen.lastUsed)) {
			chosen = a
			chosenPriority = priority
		}
	}
	if chosen == nil {
		p.rr++
		return nil, fmt.Errorf("%w: every Gemini account is cooling down or at its in-flight ceiling", core.ErrBusy)
	}
	chosen.inFlight++
	chosen.lastUsed = now
	p.rr = (p.rr + 1) % len(p.accts)
	return chosen, nil
}

func (p *pool) acquireByID(id string, now time.Time, limit int) (*accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(strings.TrimSpace(id))
	if a == nil || !p.selectable(a, now, limit) {
		return nil, fmt.Errorf("%w: account %s is not selectable", core.ErrBusy, id)
	}
	a.inFlight++
	a.lastUsed = now
	return a, nil
}

func (p *pool) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.find(id); a != nil && a.inFlight > 0 {
		a.inFlight--
	}
}

func (p *pool) find(id string) *accountRecord {
	for _, a := range p.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

func (p *pool) byID(id string) *accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.find(id); a != nil {
		copied := *a
		return &copied
	}
	return nil
}

// firstReady returns a selectable account without reserving it, falling back to
// any enabled one so an idempotent read (quota, tier) still tries.
func (p *pool) firstReady(now time.Time, limit int) *accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if p.selectable(a, now, limit) {
			copied := *a
			return &copied
		}
	}
	for _, a := range p.accts {
		if a.Enabled && a.AccessToken != "" {
			copied := *a
			return &copied
		}
	}
	return nil
}

func (p *pool) upsert(rec accountRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if existing := p.find(rec.ID); existing != nil {
		if rec.AddedAt == "" {
			rec.AddedAt = existing.AddedAt
		}
		rec.inFlight = existing.inFlight
		rec.lastUsed = existing.lastUsed
		*existing = rec
		return
	}
	copied := rec
	p.accts = append(p.accts, &copied)
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

func (p *pool) updateTokens(id, accessToken, refreshToken, expiresAt string, tokenType string) bool {
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
		a.Expiry = expiresAt
	}
	if tokenType != "" {
		a.TokenType = tokenType
	}
	a.CooldownUntil = ""
	a.LastError = ""
	a.ErrCount = 0
	return true
}

func (p *pool) setProject(id, project string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil || project == "" || a.Project == project {
		return false
	}
	a.Project = project
	return true
}

func (p *pool) setTier(id, tier string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil || tier == "" || a.Tier == tier {
		return false
	}
	a.Tier = tier
	return true
}

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

func (p *pool) stats(limit int) (inFlight, full int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		inFlight += a.inFlight
		if a.Enabled && a.AccessToken != "" && limit > 0 && a.inFlight >= limit {
			full++
		}
	}
	return inFlight, full
}

func (p *pool) statuses(now time.Time) []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.accts))
	for _, a := range p.accts {
		st := core.AccountStatus{
			ID:        a.ID,
			Label:     a.Label,
			Enabled:   a.Enabled,
			ExpiresAt: a.Expiry,
			State:     stateOf(a, now),
			Identity:  a.Identity,
		}
		st.Note = noteOf(a, now)
		extra := map[string]any{}
		if a.Tier != "" {
			extra["tier"] = a.Tier
		}
		if a.inFlight > 0 {
			extra["in_flight"] = a.inFlight
		}
		if len(extra) > 0 {
			st.Extra = extra
		}
		out = append(out, st)
	}
	return out
}

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

func noteOf(a *accountRecord, now time.Time) string {
	if !a.Enabled {
		return firstNonEmpty(a.LastError, "disabled")
	}
	if a.CooldownUntil != "" {
		if until, err := time.Parse(time.RFC3339, a.CooldownUntil); err == nil && until.After(now) {
			return firstNonEmpty(a.LastError, "cooling down") + " (until " + until.UTC().Format(time.RFC3339) + ")"
		}
	}
	return firstNonEmpty(a.LastError, a.Note)
}

func (p *pool) summary(now time.Time, limit int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return "no account; sign in from the panel"
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

// --- credential store -------------------------------------------------------

type credentialsJSON struct {
	Version  int             `json:"version"`
	Accounts []accountRecord `json:"accounts"`
}

const credentialsVersion = 1

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
		if rec.Identity == "" {
			rec.Identity = firstNonEmpty(rec.Sub, rec.Email)
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt < out[j].AddedAt })
	return out
}

func saveCredentials(path string, recs []accountRecord) error {
	if len(recs) == 0 {
		recs = []accountRecord{}
	}
	return core.WriteJSONAtomic(path, credentialsJSON{Version: credentialsVersion, Accounts: recs})
}

// decodeCredentialsBody parses a pasted credential blob: the store's own shape,
// a bare array, a single object, or a Google OAuth token file.
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
		// A Google OAuth token file commonly nests the tokens under "tokens"
		// or "installed"; accept both plus a flat object.
		var nested struct {
			Tokens *AccountConfig `json:"tokens"`
		}
		if err := json.Unmarshal([]byte(trimmed), &nested); err == nil && nested.Tokens != nil {
			return []AccountConfig{*nested.Tokens}, nil
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
