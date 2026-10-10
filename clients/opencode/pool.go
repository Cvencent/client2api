package opencode

import (
	"crypto/sha256"
	"encoding/hex"
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

// ---------------------------------------------------------------------------
// The account pool.
//
// Console OAuth tokens authenticate workspace configuration calls. Inference
// credentials and routing come from that configuration, independently of login.
// ---------------------------------------------------------------------------

// accountRecord is one Zen credential.
type accountRecord struct {
	ID               string            `json:"id"`
	Label            string            `json:"label,omitempty"`
	APIKey           string            `json:"api_key"`
	AuthMode         string            `json:"auth_mode,omitempty"` // api_key|anonymous|oauth
	AccessToken      string            `json:"access_token,omitempty"`
	RefreshToken     string            `json:"refresh_token,omitempty"`
	ExpiresAt        string            `json:"expires_at,omitempty"`
	OrgID            string            `json:"org_id,omitempty"`
	OrgName          string            `json:"org_name,omitempty"`
	Email            string            `json:"email,omitempty"`
	AllowedModels    []string          `json:"allowed_models,omitempty"`
	InferenceBaseURL string            `json:"inference_base_url,omitempty"`
	InferenceHeaders map[string]string `json:"inference_headers,omitempty"`
	InferenceError   string            `json:"inference_error,omitempty"`

	// Source records where the credential came from: config, env or import.
	// It is shown in the panel so an operator can tell a pasted key from a
	// discovered one.
	Source string `json:"source,omitempty"`

	Enabled       bool   `json:"enabled"`
	AddedAt       string `json:"added_at,omitempty"`
	Note          string `json:"note,omitempty"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
	// CooldownKind is "rate" or "quota", so an exhausted account is not shown
	// as merely cooling down.
	CooldownKind string `json:"cooldown_kind,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	ErrCount     int    `json:"err_count,omitempty"`

	// inFlight and lastUsed are runtime-only: they are guarded by the pool
	// mutex and never persisted, because a restart has no in-flight requests.
	inFlight int
	lastUsed time.Time
}

// accountID namespaces a credential inside this module.
//
// The id is a fingerprint of the key rather than the key itself: the id is
// printed in the panel, written into logs and used as a map key, and none of
// those should carry a live credential.
func accountID(uid string) string { return clientName + ":" + uid }

// keyFingerprint is the stable, non-reversible account id for a key.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

func (a *accountRecord) credentialPresent() bool {
	return a != nil && (strings.TrimSpace(a.APIKey) != "" || strings.TrimSpace(a.AccessToken) != "")
}

func (a *accountRecord) inferenceReady() bool {
	if a == nil || !a.credentialPresent() {
		return false
	}
	if a.authMode() != "oauth" {
		return true
	}
	if a.InferenceError != "" {
		return false
	}
	return strings.TrimSpace(a.APIKey) != "" ||
		strings.TrimSpace(a.InferenceHeaders["Authorization"]) != "" ||
		strings.TrimSpace(a.InferenceHeaders["X-Api-Key"]) != ""
}

func (a *accountRecord) authMode() string {
	if a == nil || strings.TrimSpace(a.AuthMode) == "" {
		return "api_key"
	}
	return strings.TrimSpace(a.AuthMode)
}

// secret is the value that must never appear in a log line or an error for
// this account: the OAuth access token when there is one, else the API key.
func (a *accountRecord) secret() string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.AccessToken) != "" {
		return a.AccessToken
	}
	return a.APIKey
}

// authSpec fills the credential headers for this account.  The credential
// Console tokens are never used as a fallback Zen API key.
func (a *accountRecord) authSpec(spec requestSpec) requestSpec {
	if a == nil {
		return spec
	}
	switch a.authMode() {
	case "anonymous":
		spec.apiKey = firstNonEmpty(a.APIKey, "public")
	case "oauth":
		spec.bearer = a.APIKey
		spec.orgID = a.OrgID
		if a.InferenceBaseURL != "" {
			spec.url = strings.TrimRight(a.InferenceBaseURL, "/") + "/chat/completions"
		}
		spec.headers = make(map[string]string, len(a.InferenceHeaders))
		for k, v := range a.InferenceHeaders {
			spec.headers[k] = strings.ReplaceAll(v, "{env:OPENCODE_CONSOLE_TOKEN}", a.AccessToken)
		}
	default:
		spec.bearer = a.APIKey
	}
	return spec
}

// sourceLabel describes where a credential came from, for the panel.
func sourceLabel(source string) string {
	switch source {
	case sourceConfig:
		return "config"
	case sourceEnv:
		return "environment"
	case sourceImport:
		return "imported"
	case sourcePanel:
		return "panel"
	default:
		return firstNonEmpty(source, "unknown")
	}
}

const (
	sourceConfig = "config"
	sourceEnv    = "env"
	sourceImport = "import"
	sourcePanel  = "panel"
)

type pool struct {
	mu    sync.Mutex
	accts []*accountRecord
	rr    int
}

func newPool() *pool { return &pool{} }

// reload replaces the account set, carrying the runtime counters across for ids
// that survive.  Dropping them would let a reload raise the effective
// concurrency ceiling above max_in_flight.
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
		cp := rec
		next = append(next, &cp)
	}
	p.accts = next
	if p.rr >= len(next) {
		p.rr = 0
	}
}

// snapshot returns a deep copy of the account set.
func (p *pool) snapshot() []accountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]accountRecord, 0, len(p.accts))
	for _, a := range p.accts {
		out = append(out, *a)
	}
	return out
}

func (p *pool) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accts)
}

// onlyAnonymous reports whether the pool is non-empty and every account is an
// anonymous free credential.  It decides whether the served catalogue must be
// narrowed to the free allowlist.
func (p *pool) onlyAnonymous() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.accts) == 0 {
		return false
	}
	found := false
	for _, a := range p.accts {
		if !a.Enabled || !a.inferenceReady() {
			continue
		}
		found = true
		if a.authMode() != "anonymous" {
			return false
		}
	}
	return found
}

// hasAnonymous reports whether any account is the anonymous free credential.
// A request routed to one of those only satisfies Zen's free tier when it
// carries the module's free-tier request signature, so Chat has to know
// whether preparing that second body is worth it.
func (p *pool) hasAnonymous() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if a.authMode() == "anonymous" {
			return true
		}
	}
	return false
}

// coolingDown reports whether an account is parked right now.
func coolingDown(a *accountRecord, now time.Time) bool {
	if a == nil || a.CooldownUntil == "" {
		return false
	}
	until, err := time.Parse(time.RFC3339, a.CooldownUntil)
	if err != nil {
		// An unparseable timestamp must not park an account forever.
		return false
	}
	return now.Before(until)
}

// selectable reports whether an account may take a request right now.
func selectable(a *accountRecord, now time.Time, limit int) bool {
	return a != nil && a.Enabled && a.inferenceReady() && a.inFlight < limit && !coolingDown(a, now)
}

func (p *pool) capacityBlocked(now time.Time, limit int, eligible func(*accountRecord) bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if a.Enabled && a.inferenceReady() && eligible(a) && !coolingDown(a, now) && a.inFlight >= limit {
			return true
		}
	}
	return false
}

// acquire reserves the least-recently-used selectable account.
//
// The three failure modes are distinguished because the gateway maps them
// differently: no credential at all is 503 (the operator must act), while a
// saturated pool is 429 (the caller should retry).
func (p *pool) acquire(now time.Time, limit int) (*accountRecord, error) {
	return p.acquireWhere(now, limit, func(*accountRecord) bool { return true })
}

func (p *pool) acquireWhere(now time.Time, limit int, eligible func(*accountRecord) bool) (*accountRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.accts) == 0 {
		return nil, fmt.Errorf(
			"%w: no OpenCode Zen account; add one in the panel or set OPENCODE_API_KEY",
			core.ErrNotConfigured)
	}
	enabled := 0
	for _, a := range p.accts {
		if a.Enabled && a.inferenceReady() {
			enabled++
		}
	}
	if enabled == 0 {
		return nil, fmt.Errorf("%w: accounts are disabled or have no Zen inference credential; refresh Console workspace configuration or add a Zen API key", core.ErrNotConfigured)
	}
	matching := false
	for _, a := range p.accts {
		if a.Enabled && a.inferenceReady() && eligible(a) {
			matching = true
			break
		}
	}
	if !matching {
		return nil, fmt.Errorf("%w: no OpenCode account supports the requested model", core.ErrUnsupported)
	}

	n := len(p.accts)
	best, found := 0, false
	for _, a := range p.accts {
		if !selectable(a, now, limit) || !eligible(a) {
			continue
		}
		prio := core.AccountPriority("opencode", a.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return nil, fmt.Errorf(
			"%w: every OpenCode Zen account is cooling down or at its in-flight ceiling", core.ErrBusy)
	}
	for i := 0; i < n; i++ {
		idx := (p.rr + i) % n
		a := p.accts[idx]
		if !selectable(a, now, limit) || !eligible(a) {
			continue
		}
		if core.AccountPriority("opencode", a.ID) != best {
			continue
		}
		a.inFlight++
		a.lastUsed = now
		p.rr = (idx + 1) % n
		cp := *a
		return &cp, nil
	}
	return nil, fmt.Errorf(
		"%w: every OpenCode Zen account is cooling down or at its in-flight ceiling", core.ErrBusy)
}

// release gives a slot back.  It matches by id rather than by pointer so a
// stream that outlives a reload still balances the books.
func (p *pool) release(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.find(id); a != nil && a.inFlight > 0 {
		a.inFlight--
	}
}

// find returns the live record.  Callers must hold the lock.
func (p *pool) find(id string) *accountRecord {
	for _, a := range p.accts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// byID returns a copy, so a caller cannot mutate pool state.
func (p *pool) byID(id string) (*accountRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// firstReady returns a copy of an account that could plausibly serve a request
// now, without reserving it.  Used by TestAccount, which must not consume a
// slot.  When nothing is selectable it falls back to any enabled account, so a
// test can still explain why an account is parked.
func (p *pool) firstReady(now time.Time, limit int) (*accountRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		if selectable(a, now, limit) {
			cp := *a
			return &cp, true
		}
	}
	for _, a := range p.accts {
		if a.Enabled && a.credentialPresent() {
			cp := *a
			return &cp, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Mutators.  All report whether anything changed.
// ---------------------------------------------------------------------------

func (p *pool) upsert(rec accountRecord) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	// A record with no key can never serve a request; storing it would only
	// add a permanently-invalid row to the panel.  AddAccount and
	// recordsFromFile already refuse one, so refuse it here too.
	if rec.ID == "" || !rec.credentialPresent() {
		return false
	}
	if a := p.find(rec.ID); a != nil {
		if rec.Label != "" {
			a.Label = rec.Label
		}
		if rec.APIKey != "" {
			a.APIKey = rec.APIKey
		}
		// A refresh supplies only the OAuth fields, so every non-empty field is
		// merged instead of the whole record being replaced.  A field the
		// caller leaves blank keeps its stored value (OrgID, Email, ...).
		if rec.AuthMode != "" {
			a.AuthMode = rec.AuthMode
		}
		if rec.AccessToken != "" {
			a.AccessToken = rec.AccessToken
			a.APIKey = rec.APIKey
			a.InferenceBaseURL = rec.InferenceBaseURL
			a.InferenceHeaders = rec.InferenceHeaders
			a.InferenceError = rec.InferenceError
			a.AllowedModels = rec.AllowedModels
		}
		if rec.RefreshToken != "" {
			a.RefreshToken = rec.RefreshToken
		}
		if rec.ExpiresAt != "" {
			a.ExpiresAt = rec.ExpiresAt
		}
		if rec.OrgID != "" {
			a.OrgID = rec.OrgID
		}
		if rec.OrgName != "" {
			a.OrgName = rec.OrgName
		}
		if rec.Email != "" {
			a.Email = rec.Email
		}
		if len(rec.AllowedModels) > 0 {
			a.AllowedModels = rec.AllowedModels
		}
		if rec.Source != "" {
			a.Source = rec.Source
		}
		if rec.Note != "" {
			a.Note = rec.Note
		}
		a.Enabled = rec.Enabled
		return true
	}
	cp := rec
	if cp.AddedAt == "" {
		cp.AddedAt = time.Now().UTC().Format(time.RFC3339)
	}
	p.accts = append(p.accts, &cp)
	return true
}

func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accts {
		if a.ID == id {
			p.accts = append(p.accts[:i], p.accts[i+1:]...)
			if p.rr > i {
				p.rr--
			}
			if p.rr >= len(p.accts) {
				p.rr = 0
			}
			return true
		}
	}
	return false
}

// setEnabled flips an account's enabled flag, clearing the failure state when
// it is turned back on so a re-enabled account does not stay parked.
func (p *pool) setEnabled(id string, enabled bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.Enabled = enabled
	if enabled {
		a.CooldownUntil = ""
		a.CooldownKind = ""
		a.LastError = ""
		a.ErrCount = 0
	}
	return true
}

func (p *pool) setNote(id, note string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.Note = note
	return true
}

// setCooldown parks an account until `until`, recording the reason.
func (p *pool) setCooldown(id string, until time.Time, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.CooldownUntil = until.UTC().Format(time.RFC3339)
	a.LastError = reason
	if strings.Contains(strings.ToLower(reason), "credit") ||
		strings.Contains(strings.ToLower(reason), "quota") {
		a.CooldownKind = "quota"
	} else {
		a.CooldownKind = "rate"
	}
	return true
}

func (p *pool) clearCooldown(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	if a.CooldownUntil == "" && a.LastError == "" && a.ErrCount == 0 {
		return false
	}
	a.CooldownUntil = ""
	a.CooldownKind = ""
	a.LastError = ""
	a.ErrCount = 0
	return true
}

// noteError records a soft failure.  A cooldown is written only once the count
// reaches the threshold, so a single 400 is never displayed as "rate limited
// for an hour".
func (p *pool) noteError(id, msg string, now time.Time, threshold int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.LastError = msg
	a.ErrCount++
	if threshold <= 0 {
		threshold = softErrorThreshold
	}
	if a.ErrCount >= threshold && a.CooldownUntil == "" {
		a.CooldownUntil = now.Add(defaultCooldown).UTC().Format(time.RFC3339)
		a.CooldownKind = "rate"
	}
	return true
}

// disable marks an account unusable.  "disabled" and "rate limited" are
// different states, so the cooldown is cleared rather than set.
func (p *pool) disable(id, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.find(id)
	if a == nil {
		return false
	}
	a.Enabled = false
	a.CooldownUntil = ""
	a.CooldownKind = ""
	a.LastError = reason
	return true
}

// ---------------------------------------------------------------------------
// Reporting.
// ---------------------------------------------------------------------------

func (p *pool) stats(limit int) (inFlight, full int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accts {
		inFlight += a.inFlight
		if a.Enabled && a.credentialPresent() && a.inFlight >= limit {
			full++
		}
	}
	return inFlight, full
}

// stateOf maps an account onto the panel's state vocabulary.
func stateOf(a *accountRecord, now time.Time) string {
	switch {
	case a == nil:
		return "unknown"
	case !a.Enabled || !a.inferenceReady():
		return "invalid"
	case coolingDown(a, now):
		if a.CooldownKind == "quota" {
			return "exhausted"
		}
		return "cooling"
	default:
		return "ready"
	}
}

// noteOf is the one-line explanation shown next to the state.
func noteOf(a *accountRecord, now time.Time) string {
	if a == nil {
		return ""
	}
	if a.authMode() == "oauth" && !a.inferenceReady() {
		note := firstNonEmpty(a.InferenceError, "Console login saved; workspace has no Zen inference credential. Refresh workspace configuration or add a Zen API key.")
		if !a.Enabled {
			note = "disabled; " + note
		}
		return note
	}
	if !a.Enabled {
		return firstNonEmpty(a.LastError, "disabled")
	}
	if !a.credentialPresent() {
		return "no credential"
	}
	if coolingDown(a, now) {
		if a.CooldownKind == "quota" {
			return firstNonEmpty(a.LastError, "out of credit") + " (until " + a.CooldownUntil + ")"
		}
		return firstNonEmpty(a.LastError, "cooling down") + " (until " + a.CooldownUntil + ")"
	}
	if a.Note != "" {
		return a.Note
	}
	return ""
}

// statuses projects the pool into the panel's account list.  No credential
// value ever leaves this function: the key is reduced to a mask.
func (p *pool) statuses(now time.Time) []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.AccountStatus, 0, len(p.accts))
	for _, a := range p.accts {
		st := core.AccountStatus{
			ID:      a.ID,
			Label:   a.Label,
			Enabled: a.Enabled,
			State:   stateOf(a, now),
			Note:    noteOf(a, now),
			// Identity stays empty on purpose: Zen's API key is not an account
			// identifier and no account id is derivable from it.  Guessing one
			// would merge unrelated logins.
			Extra: map[string]any{
				"source":    sourceLabel(a.Source),
				"key":       core.MaskSecret(a.secret()),
				"in_flight": a.inFlight,
				"auth_mode": a.authMode(),
			},
		}
		if a.ErrCount > 0 {
			st.Extra["errors"] = a.ErrCount
		}
		if a.AddedAt != "" {
			st.Extra["added_at"] = a.AddedAt
		}
		out = append(out, st)
	}
	return out
}

// summary is the one-line pool description Status carries.
func (p *pool) summary(now time.Time, limit int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	ready, cooling, exhausted, disabled := 0, 0, 0, 0
	inFlight := 0
	for _, a := range p.accts {
		inFlight += a.inFlight
		switch stateOf(a, now) {
		case "ready":
			ready++
		case "cooling":
			cooling++
		case "exhausted":
			exhausted++
		default:
			disabled++
		}
	}
	parts := []string{fmt.Sprintf("%d/%d accounts ready", ready, len(p.accts))}
	if cooling > 0 {
		parts = append(parts, fmt.Sprintf("%d cooling", cooling))
	}
	if exhausted > 0 {
		parts = append(parts, fmt.Sprintf("%d out of credit", exhausted))
	}
	if disabled > 0 {
		parts = append(parts, fmt.Sprintf("%d disabled", disabled))
	}
	if inFlight > 0 {
		parts = append(parts, fmt.Sprintf("%d in flight", inFlight))
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// Persistence.
// ---------------------------------------------------------------------------

// credentialsJSON is the on-disk shape.  It is versioned so a future format
// change can be detected instead of silently misread.
type credentialsJSON struct {
	Version  int             `json:"version"`
	Accounts []accountRecord `json:"accounts"`
}

// loadCredentials reads the module's own credential file.
//
// An absent or unreadable file is NOT an error: it means "no credentials yet",
// which is the normal state of a fresh install.
func loadCredentials(path string) []accountRecord {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	recs, err := decodeCredentialsBody(raw)
	if err != nil {
		return nil
	}
	return recs
}

// saveCredentials writes the credential file at 0600 through an atomic rename.
func saveCredentials(path string, recs []accountRecord) error {
	if path == "" {
		return nil
	}
	if recs == nil {
		recs = []accountRecord{}
	}
	if err := core.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return core.WriteJSONAtomic(path, credentialsJSON{
		Version:  credentialsVersion,
		Accounts: recs,
	})
}

// decodeCredentialsBody accepts the module's own shape and the two obvious
// variants: a bare array, and a single object.
func decodeCredentialsBody(raw []byte) ([]accountRecord, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	switch trimmed[0] {
	case '[':
		var recs []accountRecord
		if err := json.Unmarshal([]byte(trimmed), &recs); err != nil {
			return nil, err
		}
		return recs, nil
	case '{':
		var doc credentialsJSON
		if err := json.Unmarshal([]byte(trimmed), &doc); err == nil && len(doc.Accounts) > 0 {
			return doc.Accounts, nil
		}
		var one accountRecord
		if err := json.Unmarshal([]byte(trimmed), &one); err != nil {
			return nil, err
		}
		if one.ID == "" && one.APIKey == "" {
			return nil, errors.New("credential file carried no accounts")
		}
		return []accountRecord{one}, nil
	default:
		return nil, errors.New("credential file is not JSON")
	}
}

// ---------------------------------------------------------------------------
// core.PoolStatsReporter.
// ---------------------------------------------------------------------------

// PoolStats reports the in-flight accounting the panel shows.
func (c *Client) PoolStats() core.PoolStats {
	c.ensure()
	inFlight, full := c.pool.stats(c.cfg.maxInFlight())
	return core.PoolStats{InFlight: inFlight, InFlightFull: full}
}

// sortedAccountIDs is a small helper used by tests and by deterministic output.
func sortedAccountIDs(recs []accountRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}
