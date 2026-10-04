package kimi

// ---------------------------------------------------------------------------
// Panel account management
//
// The panel finds these capabilities by type assertion on the registered
// Client, so implementing the three optional interfaces below is all it takes
// for kimi to grow an account table.  The shape of the implementation is
// dictated by one fact:
//
//	THIS MODULE OWNS NO CREDENTIAL STORE.
//
// The "account" is the Kimi Code CLI's own login: a device-code OAuth token the
// CLI keeps in the OS keyring or in its own credentials file, and refreshes
// itself.  This module can *observe* that login and can *bind* the CLI's
// executable; it cannot create, refresh or delete it.  So:
//
//   - AccountFields is empty on purpose.  Nothing a human could type would
//     create a credential, and a form on a loopback HTTP page would only invite
//     somebody to paste a token into it.
//   - AddAccount always refuses, and says what to run instead (`kimi login`).
//   - RemoveAccount never touches the CLI's files.  It does the least that is
//     true to "delete this row": the panel login this module created is signed
//     out for real, a binding this module registered is removed, and a row that
//     merely *reports* state the CLI owns (`cli-login`, a probed credential) is
//     forgotten in accounts.json while the CLI's own file stays where it is.
//   - TestAccount probes the CLI and the login state.  It never sends a
//     conversation to the vendor.
//   - RefreshAccount reports, per account, that kimi has no renewal to trigger.
//   - LoginProvider is a *guided* flow: the operator runs `kimi login` in a
//     terminal and the module polls for the credential to appear.  The CLI's
//     interactive TUI is never driven programmatically.
//
// The only state this module writes lives in <DataDir>/accounts.json:
// operator enabled/disabled overrides plus imported executable bindings.  It
// never contains a token -- only ids, booleans, paths and timestamps.
// ---------------------------------------------------------------------------

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Compile-time proof that the module satisfies the optional capabilities.  If
// core's contract ever changes shape, this fails the build here rather than at
// the panel's type assertion.
var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.LoginProvider      = (*Client)(nil)
	_ core.Reviver            = (*Client)(nil)
)

const (
	// accountsFileName is this module's own state file inside Deps.DataDir.
	accountsFileName = "accounts.json"

	// accountStateVersion is the schema version of that file.
	//
	// 1 was {version, enabled, bindings}.  2 adds the optional `health` map
	// (see health.go).  3 adds the optional `forgotten` map.  The bump is
	// honest rather than load-bearing: a v1 file carries no health memory and
	// no deletions, which is exactly what a v1 file means, and a v3 file is
	// still read by an older build (it ignores the keys it does not know and
	// rewrites the file without them, losing only a cache and a hidden row).
	// loadState handles the old shapes explicitly instead of trusting a
	// hand-edited `health`/`forgotten` key in an older file.
	accountStateVersion = 3

	// cliLoginID is the stable panel id of the CLI's own login.  It matches the
	// id Status() has always reported for the "no credential evidence" case.
	cliLoginID = "cli-login"

	// bindingKindBinary marks a binding that pins the kimi executable.
	bindingKindBinary = "binary"

	// loginSessionPrefix namespaces the panel login sessions.
	loginSessionPrefix = "kimi-login-"

	// loginPollHint is appended to a login message that was inferred rather
	// than verified.
	loginPollHint = " Press Test on the cli-login account to confirm."
)

// installHint is the one-line recovery instruction used everywhere the CLI is
// missing.  Kept in one place so Status, Accounts, TestAccount and StartLogin
// cannot drift apart.
const installHint = "install it with `irm https://code.kimi.com/kimi-code/install.ps1 | iex` (Windows), " +
	"then run `kimi login` in a terminal"

// refreshUnsupported is the honest per-account answer from RefreshAccount.
const refreshUnsupported = "kimi has no credential renewal this module can trigger: the CLI owns its OAuth token " +
	"(RFC 8628 device-code flow against auth.kimi.com) and refreshes it itself on every prompt. " +
	"Run `kimi login` in a terminal if the CLI reports an expired session."

// ---------------------------------------------------------------------------
// Persistent state: <DataDir>/accounts.json
// ---------------------------------------------------------------------------

// bindingRecord is one executable path the operator imported through the panel.
// It is deliberately a path and nothing else: a path is not a credential.
type bindingRecord struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	Label   string `json:"label,omitempty"`
	AddedAt string `json:"added_at,omitempty"`
}

// accountState is the whole of this module's account bookkeeping.
type accountState struct {
	Version int `json:"version"`
	// Enabled holds only the *explicit* operator choices, keyed by account id.
	// An absent id keeps whatever default the account itself implies, so an
	// upgrade of this module never silently changes an existing setup.
	Enabled map[string]bool `json:"enabled,omitempty"`
	// Bindings are executable paths imported through the panel.
	Bindings []bindingRecord `json:"bindings,omitempty"`
	// Health is what this module has *learned* about each account across runs:
	// healthy/cooling/dead, a cooldown deadline, a short redacted reason and the
	// last time it worked (see health.go).  It is additive and optional on
	// purpose: a file written before this field existed loads as "no health
	// memory yet", and an older build reading this file ignores the field and
	// rewrites the file without it -- health is a cache, Enabled is the choice.
	Health map[string]healthRecord `json:"health,omitempty"`
	// Forgotten remembers rows the operator deleted from the panel whose
	// backing state this module does not own: the value is the evidence that
	// was true when the deletion happened, keyed by account id.  The row stays
	// out of the table while the evidence is unchanged and comes back on its
	// own once the CLI's login actually moves -- which is why a deletion here
	// is remembered rather than performed.
	Forgotten map[string]string `json:"forgotten,omitempty"`
}

func (s accountState) enabledOr(id string, def bool) bool {
	if v, ok := s.Enabled[id]; ok {
		return v
	}
	return def
}

// explicitlyDisabled reports whether the operator turned this account off.
// Only an explicit choice counts, so a default is never mistaken for a
// decision the operator made.
func (s accountState) explicitlyDisabled(id string) bool {
	v, ok := s.Enabled[id]
	return ok && !v
}

// hidden reports whether the operator deleted this row from the panel and
// nothing about it has changed since.  The evidence string is what the row
// looked like at the moment of the deletion: this module cannot delete the
// CLI's own login, so a deletion of such a row is remembered rather than
// performed, and any change in the evidence brings the row back by itself (see
// RemoveAccount).
func (s accountState) hidden(id, evidence string) bool {
	want, ok := s.Forgotten[id]
	return ok && want == evidence
}

func (s accountState) bindingByPath(path string) (bindingRecord, bool) {
	key := bindingKey(path)
	if key == "" {
		return bindingRecord{}, false
	}
	for _, b := range s.Bindings {
		if bindingKey(b.Path) == key {
			return b, true
		}
	}
	return bindingRecord{}, false
}

// accountStore is the in-process half of the account machinery: a mutex
// serialising the read-modify-write cycle on accounts.json, plus the transient
// panel login sessions (which are never persisted -- a login flow does not
// survive a restart, and pretending it does would be a lie).
type accountStore struct {
	mu       sync.Mutex
	sessions map[string]*loginSession
	// device holds the panel-driven device-code logins (see weblogin.go).  They
	// are kept apart from sessions because the two flows answer different
	// questions: sessions ask "did the CLI's own login appear yet?", device
	// sessions are logins this module performs itself.
	device map[string]*deviceSession
}

// statePath is the state file, or "" when the core gave us no DataDir.
func (c *Client) statePath() string {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, accountsFileName)
}

// loadState reads the state file.  A missing file is the normal empty state,
// not an error; anything else (unreadable, malformed) is reported so the panel
// can say so instead of silently ignoring the operator's settings.
func (c *Client) loadState() (accountState, error) {
	p := c.statePath()
	if p == "" {
		return accountState{Version: accountStateVersion}, nil
	}
	var st accountState
	if err := core.ReadJSON(p, &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return accountState{Version: accountStateVersion}, nil
		}
		return accountState{}, fmt.Errorf("kimi: reading %s: %w", accountsFileName, err)
	}
	if st.Version < 2 {
		// A version 1 file is {version, enabled, bindings} and nothing else.
		// There is nothing to migrate: "no health memory yet" is precisely what
		// a v1 file means.  Anything that looks like health in a v1 file was put
		// there by hand or by a future build, and is dropped rather than
		// trusted -- an unverified record must never park an account.
		st.Health = nil
	}
	if st.Version < 3 {
		// `forgotten` arrived with version 3.  A file written before it hides
		// nothing, so a key that looks like a deletion in an older file was put
		// there by hand or by a newer build and is dropped for the same reason:
		// the table must reflect the accounts that exist now, not a claim about
		// them that this build cannot verify.
		st.Forgotten = nil
	}
	return st, nil
}

func (c *Client) saveState(st accountState) error {
	p := c.statePath()
	if p == "" {
		return errors.New("kimi: no DataDir is configured, so account state cannot be stored")
	}
	st.Version = accountStateVersion
	if err := core.WriteJSONAtomic(p, st); err != nil {
		return fmt.Errorf("kimi: writing %s: %w", accountsFileName, err)
	}
	return nil
}

// mutateState runs one atomic read-modify-write on the state file.
func (c *Client) mutateState(fn func(*accountState) error) (accountState, error) {
	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()

	st, err := c.loadState()
	if err != nil {
		return accountState{}, err
	}
	if st.Enabled == nil {
		st.Enabled = map[string]bool{}
	}
	if err := fn(&st); err != nil {
		return accountState{}, err
	}
	if err := c.saveState(st); err != nil {
		return accountState{}, err
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// Cache invalidation
//
// Both discovery caches are TTL'd at 10s (they back Status, which the panel
// refreshes constantly).  A panel action that changes the answer -- importing a
// binary, toggling an account, polling a login -- must not wait out the TTL.
// ---------------------------------------------------------------------------

func (r *runner) invalidateCredCache() {
	r.credMu.Lock()
	r.credAt = time.Time{}
	r.credMu.Unlock()
}

func (r *runner) invalidateBinaryCache() {
	r.binMu.Lock()
	r.binAt = time.Time{}
	r.binMu.Unlock()
}

// boundBinding is the first binding that is enabled, healthy enough to be used
// right now, and still points at a real file.
//
// The health check is what stops a pinned executable that has been failing from
// being picked again on every request: a cooling binding falls through to the
// next one (and eventually to PATH and the well-known locations), and a dead
// one stays out until something revives it.
func (c *Client) boundBinding() (bindingRecord, bool) {
	st, err := c.loadState()
	if err != nil {
		return bindingRecord{}, false
	}
	for _, b := range st.Bindings {
		if b.Kind != bindingKindBinary {
			continue
		}
		if st.explicitlyDisabled(b.ID) {
			continue
		}
		if !c.selectable(b.ID) {
			continue
		}
		if isRegularFile(b.Path) {
			return b, true
		}
	}
	return bindingRecord{}, false
}

// boundBinary is the first usable binding's path.  It is an *explicit* operator
// decision, so it outranks PATH and the well-known install locations, but never
// the configured clients.kimi.binary.
func (c *Client) boundBinary() string {
	b, ok := c.boundBinding()
	if !ok {
		return ""
	}
	return b.Path
}

// ---------------------------------------------------------------------------
// Account entries
// ---------------------------------------------------------------------------

// Account kinds, as seen by the panel.
const (
	kindCLILogin   = "cli-login"
	kindCredential = "credential"
	kindBinding    = "binding"
)

// accountEntry is one row of the panel's table plus what the mutation verbs
// need to know about it.
type accountEntry struct {
	record  core.AccountRecord
	kind    string
	binding bindingRecord
	source  credentialSource
}

// credentialAccountID is the stable panel id for one probed credential source.
// It must stay byte-identical to the id Status() reports through
// credentialReport.accounts(), or the operator's enabled/disabled choice would
// apply to one table and not the other.
func credentialAccountID(s credentialSource) string {
	return s.Kind + ":" + filepath.Base(s.Ref)
}

// foundRefs lists the references (a path or an environment variable *name* --
// never a value) of every credential source that was found.
func (r credentialReport) foundRefs() []string {
	var out []string
	for _, s := range r.sources {
		if s.Found {
			out = append(out, s.Ref)
		}
	}
	return out
}

func (r credentialReport) foundIDs() []string {
	var out []string
	for _, s := range r.sources {
		if s.Found {
			out = append(out, credentialAccountID(s))
		}
	}
	return out
}

// cliLoginEvidence is the signature of the `cli-login` row: everything that
// decides whether that row is real.  Forgetting the row records this string, so
// the row stays hidden while nothing has changed and returns by itself once it
// has -- a CLI appearing or being uninstalled, a credential showing up, or
// assume_logged_in being switched on.
func cliLoginEvidence(bin string, creds credentialReport, assume bool) string {
	return fmt.Sprintf("bin=%s|assume=%t|refs=%s", bin, assume, strings.Join(creds.foundRefs(), ","))
}

// credentialEvidence is the signature of one probed credential source: where it
// was found, plus the change detector recorded when it was found.  A refreshed
// credential is new evidence about the account, so the row becomes visible again
// on its own.  That is why the detector is the file's size and modification time
// rather than the expiry it happens to state: a rotated token can keep the same
// expiry, and a file that states no expiry at all would otherwise never be able
// to come back.
func credentialEvidence(s credentialSource) string {
	return s.Ref + "|" + s.Evidence
}

// accountEntries builds the panel's view of this module's accounts.
//
// Unlike Status() -- whose shape is frozen by its own tests and which reports
// credential *evidence* -- this list always leads with the `cli-login` row,
// because that is the account.  The rows that follow are where the evidence for
// it was found, plus any executable binding the operator imported.
func (c *Client) accountEntries(ctx context.Context) ([]accountEntry, error) {
	_ = ctx // kept for the interface; every probe below is local and cheap

	st, err := c.loadState()
	if err != nil {
		return nil, err
	}
	bin, binErr := c.run.binaryPath()
	creds := c.credentials()

	out := make([]accountEntry, 0, len(creds.sources)+len(st.Bindings)+2)
	// The panel login comes first when it exists: it is the account this
	// module owns outright, and the one an operator can actually act on
	// (enable, disable, sign out).  `cli-login` below it belongs to the CLI.
	if sts, ok := c.tokenStatus(); ok {
		web := core.AccountRecord{
			ID:        sts.ID,
			Label:     sts.Label,
			Enabled:   st.enabledOr(webLoginID, true),
			State:     sts.State,
			ExpiresAt: sts.ExpiresAt,
			Note:      sts.Note,
			Fields:    sts.Extra,
			Identity:  sts.Identity,
		}
		web.Note, web.State = c.applyHealth(web.Fields, web.Note, web.State, webLoginID)
		out = append(out, accountEntry{record: web, kind: kindCredential})
	}
	if c.cliLoginIsLive(bin, creds) {
		// A hidden row is one the operator deleted from the panel.  The check is
		// on the evidence rather than on the id, so a genuine CLI login can
		// never be hidden forever by a deletion: the row returns as soon as the
		// CLI's own login state actually moves.
		if !st.hidden(cliLoginID, cliLoginEvidence(bin, creds, c.cfg.AssumeLoggedIn)) {
			out = append(out, c.cliLoginEntry(bin, binErr, creds, st))
		}
	}

	for _, s := range c.credentialRows(creds) {
		if !s.Found {
			continue
		}
		id := credentialAccountID(s)
		if st.hidden(id, credentialEvidence(s)) {
			continue
		}
		rec := core.AccountRecord{
			ID:        id,
			Label:     s.Ref,
			Enabled:   st.enabledOr(id, true),
			State:     "ready",
			ExpiresAt: s.ExpiresAt,
			Note:      s.Note,
			Identity:  s.Identity,
			Fields: map[string]any{
				"kind":   s.Kind,
				"source": s.Ref,
			},
		}
		if s.ExpiresAt != "" {
			if exp, perr := time.Parse(time.RFC3339, s.ExpiresAt); perr == nil && exp.Before(time.Now()) {
				rec.State = "invalid"
				if rec.Note == "" {
					rec.Note = "credential has expired; the CLI refreshes it on the next prompt"
				}
			}
		}
		out = append(out, accountEntry{record: rec, kind: kindCredential, source: s})
	}

	for _, b := range st.Bindings {
		out = append(out, c.bindingEntry(b, st))
	}
	return out, nil
}

// credentialRows is the set of evidence rows the panel lists.  With
// show_credential_rows on (the default) it is every source the login probe
// found, so the operator can see which file or variable backed the login.
// With it off the list is empty: the cli-login row above already names every
// file it read, so the per-file rows would add rows without adding anything.
func (c *Client) credentialRows(creds credentialReport) []credentialSource {
	if !c.cfg.showCredentialRows() {
		return nil
	}
	return creds.sources
}

// cliLoginIsLive reports whether the CLI's own login is something an operator
// could actually act on.
//
// `cli-login` is a view onto state this module does not own: the token lives in
// the CLI's files or its OS keyring. With no CLI installed, no opt-in and no
// credential on disk there is nothing behind it, and the row degenerates into a
// permanent "the kimi CLI is not installed" line that can be neither used (the
// CLI is absent) nor removed (this module must not delete the CLI's files). So
// it is simply not rendered until a CLI is genuinely in play — the panel login
// covers the CLI-less install on its own.
func (c *Client) cliLoginIsLive(bin string, creds credentialReport) bool {
	return bin != "" || c.cfg.AssumeLoggedIn || len(creds.foundRefs()) > 0
}

// cliLoginEntry is the CLI's own login as the panel sees it.
func (c *Client) cliLoginEntry(bin string, binErr error, creds credentialReport, st accountState) accountEntry {
	rec := core.AccountRecord{
		ID:      cliLoginID,
		Label:   "kimi CLI login",
		State:   "unknown",
		Enabled: st.enabledOr(cliLoginID, c.cfg.AssumeLoggedIn),
		// The CLI's own credential is where this row's evidence comes from, so
		// it is also where the account it belongs to is read: the panel login
		// above and this login are two credentials for one Kimi user.
		Identity: creds.identity(),
	}
	fields := map[string]any{"kind": kindCLILogin, "owner": "kimi Code CLI"}
	if bin != "" {
		fields["binary"] = bin
	}
	rec.Fields = fields

	refs := creds.foundRefs()
	switch {
	case binErr != nil:
		rec.State = "invalid"
		rec.Enabled = st.enabledOr(cliLoginID, false)
		rec.Note = "the kimi CLI is not installed, so no login can exist; " + installHint
	case len(refs) > 0:
		rec.State = "ready"
		rec.Enabled = st.enabledOr(cliLoginID, true)
		rec.Note = fmt.Sprintf("logged in: credential evidence at %s. The token itself belongs to the CLI and is never read by this module.",
			strings.Join(refs, ", "))
	case c.cfg.AssumeLoggedIn:
		rec.State = "ready"
		rec.Enabled = st.enabledOr(cliLoginID, true)
		rec.Note = "assume_logged_in is set, so the login check is skipped; the CLI may be logged in via the OS keyring"
	default:
		rec.State = "unknown"
		rec.Note = "no credential file found; the CLI may still hold a token in the OS keyring, which the standard library cannot read. Run `kimi login` in a terminal if this is wrong."
	}

	// What this module learned on previous runs is the one thing the local
	// probe above cannot see -- a run that failed an hour ago is invisible to a
	// stat -- so it is folded in last, and it explains the note rather than
	// overwriting the probe's own answer.
	rec.Note, rec.State = c.applyHealth(fields, rec.Note, rec.State, cliLoginID)

	return accountEntry{record: rec, kind: kindCLILogin}
}

// bindingEntry renders one imported executable binding.
func (c *Client) bindingEntry(b bindingRecord, st accountState) accountEntry {
	label := b.Label
	if strings.TrimSpace(label) == "" {
		label = b.Path
	}
	rec := core.AccountRecord{
		ID:      b.ID,
		Label:   label,
		Enabled: st.enabledOr(b.ID, true),
		State:   "ready",
		Note: "executable binding registered by this module in " + accountsFileName + "; it pins " +
			b.Path + " as the kimi CLI, and is used only when clients.kimi.binary is empty",
		Fields: map[string]any{
			"kind":  b.Kind,
			"path":  b.Path,
			"added": b.AddedAt,
		},
	}
	switch fi, err := os.Stat(b.Path); {
	case err != nil:
		rec.State = "invalid"
		rec.Note = "the imported path no longer exists: " + b.Path
	case fi.IsDir():
		rec.State = "invalid"
		rec.Note = "the imported path is a directory: " + b.Path
	}
	rec.Note, rec.State = c.applyHealth(rec.Fields, rec.Note, rec.State, b.ID)
	return accountEntry{record: rec, kind: kindBinding, binding: b}
}

// ---------------------------------------------------------------------------
// core.AccountManager
// ---------------------------------------------------------------------------

// AccountFields implements core.AccountManager.  It is empty on purpose: kimi's
// credential is created by the CLI's own device-code login, so there is no
// value a human could type here that would produce a working account.  An empty
// slice is the contract's documented way of saying "listable and testable, but
// nothing can be typed in" -- the panel hides the form and keeps the table.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	_ = ctx
	return []core.FieldSpec{}
}

// Accounts implements core.AccountManager.  It never returns a token: the CLI's
// credential is reported by presence and expiry only.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	entries, err := c.accountEntries(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]core.AccountRecord, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.record)
	}
	return out, nil
}

// AddAccount implements core.AccountManager.  It always refuses: this module has
// no credential store to add to, and silently succeeding would leave the panel
// showing an account that does not exist.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	_ = ctx
	_ = spec
	return core.AccountRecord{}, fmt.Errorf(
		"kimi: accounts cannot be added by hand. This module has no credential store: the account is the Kimi Code CLI's own login, "+
			"so run `kimi login` in a terminal (AccountFields is empty on purpose). "+
			"To pin the CLI executable instead, use the panel's Discover/Import action: %w", core.ErrUnsupported)
}

// RemoveAccount implements core.AccountManager.
//
// "Delete this row" is answered with the least that is true to it:
//
//   - `kimi-web` is the login this module performed itself, so it is signed
//     out: the stored token is deleted and the account is gone for real.
//   - `cli-login` and the `credential:*` rows are views onto state the CLI
//     owns.  Their files belong to the CLI and are never touched; the row is
//     *forgotten* instead -- recorded in accounts.json together with the
//     evidence that was true at that moment, so it stays out of the table while
//     nothing changes and returns on its own once the CLI's login does change.
//   - a binding this module registered is removed for real, as before.
//
// Deleting an already-forgotten row succeeds: the operator asked for the end
// state, not for a specific transition.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("kimi: RemoveAccount: empty account id")
	}
	if id == webLoginID {
		return c.signOut()
	}

	st, err := c.loadState()
	if err != nil {
		return err
	}
	if _, forgotten := st.Forgotten[id]; forgotten {
		return nil
	}

	entries, err := c.accountEntries(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.record.ID != id {
			continue
		}
		switch e.kind {
		case kindCLILogin:
			bin, _ := c.run.binaryPath()
			return c.forgetRow(id, cliLoginEvidence(bin, c.credentials(), c.cfg.AssumeLoggedIn))
		case kindCredential:
			return c.forgetRow(id, credentialEvidence(e.source))
		}
		break
	}

	removed := false
	if _, err := c.mutateState(func(st *accountState) error {
		kept := make([]bindingRecord, 0, len(st.Bindings))
		for _, b := range st.Bindings {
			if b.ID == id {
				removed = true
				continue
			}
			kept = append(kept, b)
		}
		st.Bindings = kept
		delete(st.Forgotten, id)
		return nil
	}); err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("kimi: no such account %q", id)
	}
	// The row is gone, so what was remembered about it goes too: a record for an
	// account that no longer exists could only ever be stale.
	c.revive(id)
	c.run.invalidateBinaryCache()
	return nil
}

// forgetRow hides one row whose backing state belongs to the CLI.  Nothing
// outside accounts.json is touched: this module will not delete another
// program's credential, and the row is remembered rather than performed --
// a change in the evidence brings it back (see accountState.hidden).
//
// It is named apart from health.go's forget, which drops the in-memory health
// record: that one is called *here*, through revive, before the row is hidden,
// so a later flush cannot write the health entry back into a file that says the
// row is gone.
func (c *Client) forgetRow(id, evidence string) error {
	// Everything remembered about a row that is no longer displayed is dropped
	// with it.  When the row comes back it does so as what it is now, not as
	// what it was.
	c.revive(id)
	if _, err := c.mutateState(func(st *accountState) error {
		if st.Forgotten == nil {
			st.Forgotten = map[string]string{}
		}
		st.Forgotten[id] = evidence
		delete(st.Enabled, id)
		delete(st.Health, id)
		return nil
	}); err != nil {
		return err
	}
	c.run.invalidateCredCache()
	c.run.invalidateBinaryCache()
	c.deps.Log("kimi: %s is no longer listed in the panel; its credential belongs to the CLI and was left in place", id)
	return nil
}

// unforget drops a remembered deletion, if there is one.  It is deliberately
// cheap: a row that was never hidden costs one read of a small state file.
func (c *Client) unforget(id string) error {
	st, err := c.loadState()
	if err != nil {
		return err
	}
	if _, hidden := st.Forgotten[id]; !hidden {
		return nil
	}
	if _, err := c.mutateState(func(s *accountState) error {
		delete(s.Forgotten, id)
		return nil
	}); err != nil {
		return err
	}
	c.run.invalidateCredCache()
	return nil
}

// signOut deletes the panel login this module created.  It is the one removal
// here that is a real deletion, because it is the one credential this module
// owns.
func (c *Client) signOut() error {
	if err := c.clearToken(); err != nil {
		return err
	}
	// The account is gone, so what was remembered about it goes too: in memory
	// first, then in the file.
	c.revive(webLoginID)
	if _, err := c.mutateState(func(st *accountState) error {
		delete(st.Enabled, webLoginID)
		delete(st.Health, webLoginID)
		delete(st.Forgotten, webLoginID)
		return nil
	}); err != nil {
		return err
	}
	c.run.invalidateCredCache()
	c.deps.Log("kimi: signed out of the panel login; the stored token for %s was removed", webLoginID)
	return nil
}

// SetAccountEnabled implements core.AccountManager.
//
// This is meaningful for kimi: `cli-login` is the CLI's login, and turning it
// off makes Chat refuse to run against it (see loginDisabledError), which is
// exactly what an operator wants when they are rotating or debugging a login.
// A binding switched off is no longer consulted as the executable.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("kimi: SetAccountEnabled: empty account id")
	}
	// An explicit choice about an id outranks a previous deletion: asking about
	// this account again is the operator bringing the row back, and it is the
	// only way a forgotten `cli-login` returns before the CLI's own state moves.
	if err := c.unforget(id); err != nil {
		return err
	}
	entries, err := c.accountEntries(ctx)
	if err != nil {
		return err
	}
	known := false
	for _, e := range entries {
		if e.record.ID == id {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("kimi: no such account %q", id)
	}

	if _, err := c.mutateState(func(st *accountState) error {
		st.Enabled[id] = enabled
		return nil
	}); err != nil {
		return err
	}
	if enabled {
		// Enabling an account is the operator saying "try this again", and it is
		// one of only two things that may revive a *dead* account (see
		// reviveOnEvidence for the other).  Everything remembered about it is
		// forgotten, including a cooldown the operator did not agree to wait out.
		c.revive(id)
	}
	c.run.invalidateBinaryCache()
	return nil
}

// ReviveAccount implements core.Reviver.
//
// Everything this module can hold against an account lives in one file: the
// `health` map of accounts.json remembers cooling and dead, and the `enabled`
// map remembers a credential the operator parked by hand.  So the operator's
// revive is exactly the operator's enable, and this delegates rather than
// reimplementing it -- one code path means the panel's revive button and its
// enable toggle can never drift apart about what "try this again" means.
//
// No credential is minted, refreshed or guessed at here.  A revived login
// whose token is genuinely dead must fail again on its next real run: that
// failure is the evidence the panel wants to show, and a revive that papered
// over it would be hiding the very thing the operator asked to see.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("kimi: ReviveAccount: empty account id")
	}
	return c.SetAccountEnabled(ctx, id, true)
}

// TestAccount implements core.AccountManager.
//
// The probe is deliberately local and cheap: does the CLI exist, and is there
// login evidence?  No conversation is sent to the vendor, so "ok" means "this
// account would be allowed to run", not "the model answered".
func (c *Client) TestAccount(ctx context.Context, id string) (res core.TestResult, err error) {
	start := time.Now()
	defer func() { res.ElapsedMS = time.Since(start).Milliseconds() }()

	id = strings.TrimSpace(id)
	if id == "" {
		return res, errors.New("kimi: TestAccount: empty account id")
	}
	entries, err := c.accountEntries(ctx)
	if err != nil {
		return res, err
	}
	var found *accountEntry
	for i := range entries {
		if entries[i].record.ID == id {
			found = &entries[i]
			break
		}
	}
	if found == nil {
		// `cli-login` is not listed while no CLI is in play, but it stays a real
		// id: an explicit probe should still report *why* it is unusable instead
		// of claiming it does not exist.  SetAccountEnabled deliberately does not
		// do this — there is no point toggling a row the panel never offers.
		if id != cliLoginID {
			return res, fmt.Errorf("kimi: no such account %q", id)
		}
		st, serr := c.loadState()
		if serr != nil {
			return res, serr
		}
		bin, binErr := c.run.binaryPath()
		e := c.cliLoginEntry(bin, binErr, c.credentials(), st)
		found = &e
	}

	res.AccountID = id
	res.Model = c.cfg.DefaultModel
	// Gate on the operator's *explicit* choice, not on the displayed Enabled
	// flag: that flag carries a historical default (cli-login reads as disabled
	// while no login exists), and reporting "disabled" instead of the real
	// reason would be exactly the kind of dishonesty this module avoids.
	if c.accountExplicitlyDisabled(id) {
		res.Error = fmt.Sprintf("account %q is disabled in the panel; enable it before testing", id)
		return res, nil
	}
	// A cooling or dead account is not usable, and this probe must say so: it
	// answers "would this account be allowed to run", not "is the file there".
	// Reporting a healthy-looking file for an account this module refuses to
	// select would be exactly the kind of dishonesty this module avoids.
	if note := c.healthNote(id); note != "" {
		res.Error = fmt.Sprintf("account %q is not usable right now: %s", id, note)
		return res, nil
	}

	switch found.kind {
	case kindBinding:
		path := found.binding.Path
		if !isRegularFile(path) {
			res.Error = fmt.Sprintf("the imported binding no longer points at a file: %s", path)
			return res, nil
		}
		creds := c.credentials()
		if !creds.usable() && !c.cfg.AssumeLoggedIn {
			res.Error = fmt.Sprintf("kimi CLI binary found at %s but no login credentials were found; run `kimi login`. Looked for: %s",
				path, strings.Join(creds.searched, ", "))
			return res, nil
		}
		res.OK = true
		res.Reply = fmt.Sprintf("binding is usable: %s exists and login evidence was found (%s). No conversation was sent.",
			path, strings.Join(creds.foundRefs(), ", "))
		return res, nil

	case kindCredential:
		res.OK = true
		res.Reply = fmt.Sprintf("credential source %s is present (%s). No conversation was sent.",
			found.source.Ref, found.record.State)
		return res, nil
	}

	bin, binErr := c.run.binaryPath()
	if binErr != nil {
		res.Error = "kimi CLI not found on PATH or in any known install location; " + installHint
		return res, nil
	}
	creds := c.credentials()
	if !creds.usable() && !c.cfg.AssumeLoggedIn {
		res.Error = fmt.Sprintf("kimi CLI found at %s but no login credentials were found; run `kimi login`. Looked for: %s (and the environment variables %s)",
			bin, strings.Join(creds.searched, ", "), strings.Join(credentialEnvVars, ", "))
		return res, nil
	}
	res.OK = true
	if refs := creds.foundRefs(); len(refs) > 0 {
		res.Reply = fmt.Sprintf("kimi CLI found at %s and login evidence found at %s. No conversation was sent.",
			bin, strings.Join(refs, ", "))
	} else {
		res.Reply = fmt.Sprintf("kimi CLI found at %s; assume_logged_in is set, so the login check is skipped. No conversation was sent.", bin)
	}
	return res, nil
}

// RefreshAccount implements core.AccountManager.
//
// Kimi has no renewal this module could perform, so every account gets an
// honest ok=false with the reason.  The call only fails when the request itself
// names an account that does not exist.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	id = strings.TrimSpace(id)
	entries, err := c.accountEntries(ctx)
	if err != nil {
		return nil, err
	}

	if id != "" {
		known := false
		for _, e := range entries {
			if e.record.ID == id {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("kimi: no such account %q", id)
		}
		return []core.RefreshResult{{AccountID: id, OK: false, Error: refreshUnsupported}}, nil
	}

	out := make([]core.RefreshResult, 0, len(entries))
	for _, e := range entries {
		out = append(out, core.RefreshResult{AccountID: e.record.ID, OK: false, Error: refreshUnsupported})
	}
	return out, nil
}

// loginDisabledError reports why no login route may be used, or nil when at
// least one may.  Only an explicit operator choice counts.
//
// There are two routes now, and they cover for each other: the panel's own web
// login (webLoginID, see weblogin.go) and the CLI's login (cliLoginID).  A
// disable only makes the module unusable when it removes the last route, so the
// CLI checks below are skipped while a live web token is present, and they only
// count while the CLI itself is in play — see the cliLoginIsLive guard below.
func (c *Client) loginDisabledError(creds credentialReport) error {
	st, err := c.loadState()
	if err != nil {
		// An unreadable state file must not brick Chat; Accounts() surfaces it.
		return nil
	}

	webLive := false
	if tok, ok := c.loadToken(); ok && !tok.expired() {
		if st.explicitlyDisabled(webLoginID) {
			return nil // the CLI may still cover; the checks below decide
		}
		webLive = true
	}

	if !webLive {
		// Only honour an explicit `cli-login` disable while the CLI is actually in
		// play.  The row is not listed otherwise (see cliLoginIsLive), and a stale
		// flag left over from an uninstalled CLI would otherwise disable a route
		// the operator can no longer see — a trap with no way back out.
		if bin, _ := c.run.binaryPath(); c.cliLoginIsLive(bin, creds) && st.explicitlyDisabled(cliLoginID) {
			if !creds.usable() {
				return fmt.Errorf("the panel web login (account %s) and the kimi CLI login (account %s) are both unavailable or disabled; "+
					"re-enable one in the panel, or sign in again", webLoginID, cliLoginID)
			}
			return fmt.Errorf("the kimi CLI login is disabled in the panel (account %s); re-enable it there, "+
				"or run `kimi login` if you meant to log in again", cliLoginID)
		}
	}
	if webLive {
		return nil
	}

	found := false
	for _, s := range creds.sources {
		if !s.Found {
			continue
		}
		found = true
		if !st.explicitlyDisabled(credentialAccountID(s)) {
			return nil
		}
	}
	if found {
		return errors.New("every credential source this module found is disabled in the panel")
	}
	return nil
}

// accountExplicitlyDisabled reports whether the operator turned this account
// off by hand.  It deliberately ignores the *displayed* Enabled flag on a
// record: that flag carries a historical default (cli-login reads as disabled
// while no login exists), and a panel action must never mistake a default for
// an instruction.  An unreadable state file counts as "not disabled" so that a
// corrupt file cannot silently switch the module off.
func (c *Client) accountExplicitlyDisabled(id string) bool {
	st, err := c.loadState()
	if err != nil {
		return false
	}
	return st.explicitlyDisabled(id)
}

// applyAccountOverrides folds the operator's explicit choices into the Status
// rows.  It returns a non-empty note when the state file could not be read.
func (c *Client) applyAccountOverrides(accounts []core.AccountStatus) string {
	st, err := c.loadState()
	if err != nil {
		msg := redactSecrets(err.Error())
		if len(msg) > 160 {
			msg = msg[:160] + "…"
		}
		return msg
	}
	for i := range accounts {
		if v, ok := st.Enabled[accounts[i].ID]; ok {
			accounts[i].Enabled = v
		}
		// Health is folded in here as well as in Accounts(): Status() is the
		// cheapest way for an operator to see why a login is not being used,
		// and it must not require reading the JSON file by hand.  Both the
		// row's field map and the row's note carry the explanation, because
		// the note is the sentence the panel actually shows.
		if _, ok := st.Health[accounts[i].ID]; ok {
			if accounts[i].Extra == nil {
				accounts[i].Extra = map[string]any{}
			}
			accounts[i].Note, accounts[i].State = c.applyHealth(accounts[i].Extra, accounts[i].Note, accounts[i].State, accounts[i].ID)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// core.CredentialImporter
// ---------------------------------------------------------------------------

// Discover implements core.CredentialImporter.  It is strictly read-only: it
// stats and lists, and never writes, never copies a credential, and never reads
// a token out of a file.
//
// Only the CLI executable itself is Importable.  Everything else found on this
// machine is reported with Importable=false and a note saying why, because a
// panel that offers to "import" a device id or an encrypted desktop token store
// would be lying about what it can do.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	_ = ctx

	st, err := c.loadState()
	if err != nil {
		return nil, err
	}
	bound := map[string]bool{}
	for _, b := range st.Bindings {
		bound[bindingKey(b.Path)] = true
	}

	var out []core.DiscoveredCredential
	seen := map[string]bool{}
	add := func(d core.DiscoveredCredential) {
		key := bindingKey(d.Path)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		if bound[key] {
			d.Imported = true
		}
		out = append(out, d)
	}

	// 1. The executable a request would actually run today.
	if p, lerr := exec.LookPath(kimiBinaryName()); lerr == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			p = abs
		}
		add(core.DiscoveredCredential{
			Path:       p,
			Kind:       "cli-binary",
			Label:      filepath.Base(p) + " (on PATH)",
			Note:       "the executable this gateway would run for a request today",
			Importable: true,
		})
	}

	// 2. Well-known install locations from candidateBinaryPaths().
	for _, p := range candidateBinaryPaths() {
		if isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path:       p,
				Kind:       "cli-binary",
				Label:      filepath.Base(p) + " (known location)",
				Note:       "known kimi install location",
				Importable: true,
			})
		}
	}

	home, _ := os.UserHomeDir()
	if home != "" {
		// 3. ~/.kimi-work/bin -- the desktop app's tool directory.
		workBin := filepath.Join(home, ".kimi-work", "bin")
		for _, name := range listDirNames(workBin) {
			p := filepath.Join(workBin, name)
			if !isRegularFile(p) {
				continue
			}
			if isCLIBinaryName(name) {
				add(core.DiscoveredCredential{
					Path:       p,
					Kind:       "cli-binary",
					Label:      name + " (~/.kimi-work/bin)",
					Note:       "kimi-shaped executable found in ~/.kimi-work/bin",
					Importable: true,
				})
				continue
			}
			add(core.DiscoveredCredential{
				Path: p,
				Kind: "kimi-work-tool",
				Note: "auxiliary tool from the kimi desktop toolchain, not the kimi Code CLI; " +
					"binding it would make every request fail, so it is not importable",
				Importable: false,
			})
		}

		// 4. The local webbridge daemon's artefacts.
		wb := filepath.Join(home, ".kimi-webbridge")
		if p := filepath.Join(wb, "identity.json"); isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path: p,
				Kind: "webbridge-identity",
				Note: "device identity of the local kimi-webbridge daemon (a device_id, no credential). " +
					"Importing it would register a device, not a login, so it is not importable",
				Importable: false,
			})
		}
		if p := filepath.Join(wb, "daemon.pid"); isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path:       p,
				Kind:       "webbridge-daemon",
				Note:       "pid file of the local kimi-webbridge daemon; not a credential and not importable",
				Importable: false,
			})
		}
		if p := filepath.Join(wb, "bin", "kimi-webbridge"+exeSuffix()); isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path: p,
				Kind: "webbridge-binary",
				Note: "the webbridge daemon binary, not the kimi Code CLI; it cannot serve a chat request, " +
					"so it is not importable",
				Importable: false,
			})
		}
	}

	// 5. The desktop app's own stores.
	if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
		desktop := filepath.Join(appdata, "kimi-desktop")
		if isDir(desktop) {
			add(core.DiscoveredCredential{
				Path: desktop,
				Kind: "desktop-app",
				Note: "the kimi desktop app is installed for this user. Its login is separate from the CLI's " +
					"and this module cannot use it, so it is not importable",
				Importable: false,
			})
		}
		if p := filepath.Join(desktop, "bridge-store", "token-store.json"); isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path: p,
				Kind: "desktop-token-store",
				Note: "kimi-desktop's token store. It is encrypted with Chromium safeStorage (Windows DPAPI / " +
					"macOS Keychain) and the standard library cannot decrypt it, so this module cannot import it",
				Importable: false,
			})
		}
		if p := filepath.Join(desktop, "kimi-agent", "kimi-work-models-cache.json"); isRegularFile(p) {
			add(core.DiscoveredCredential{
				Path:       p,
				Kind:       "desktop-model-cache",
				Note:       "kimi-desktop's model cache; it lists model ids but holds no credential",
				Importable: false,
			})
		}
	}

	// 6. The credential files this module already probes for the login check.
	// They are evidence, not something to import: the CLI owns them.
	files := c.cfg.CredentialFiles
	if len(files) == 0 {
		files = credentialFileCandidates()
	}
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || !isRegularFile(f) {
			continue
		}
		add(core.DiscoveredCredential{
			Path: f,
			Kind: "credentials-file",
			Note: "already probed by this module's login check; its contents are never copied into this " +
				"module's state, so there is nothing to import",
			Importable: false,
		})
	}

	return out, nil
}

// Import implements core.CredentialImporter.  It registers the selected paths
// in this module's own DataDir as explicit executable bindings.  It copies no
// credential and reads no token: the binding is a path plus a timestamp.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	found, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]core.DiscoveredCredential, len(found))
	for _, d := range found {
		byPath[bindingKey(d.Path)] = d
	}

	var wanted []core.DiscoveredCredential
	seen := map[string]bool{}
	addWanted := func(d core.DiscoveredCredential) {
		k := bindingKey(d.Path)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		wanted = append(wanted, d)
	}
	if all {
		for _, d := range found {
			if d.Importable {
				addWanted(d)
			}
		}
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		key := bindingKey(p)
		d, ok := byPath[key]
		if !ok {
			if abs, aerr := filepath.Abs(p); aerr == nil {
				d, ok = byPath[bindingKey(abs)]
			}
		}
		if !ok {
			return nil, fmt.Errorf("kimi: %q was not reported by Discover; run the discovery scan again", p)
		}
		if !d.Importable {
			return nil, fmt.Errorf("kimi: %q is not importable: %s", d.Path, d.Note)
		}
		addWanted(d)
	}
	if len(wanted) == 0 {
		// Nothing importable was found.  That is not a malformed request, so it
		// is not an error -- Discover already told the truth about this machine.
		return []core.AccountRecord{}, nil
	}

	// Validate everything before writing anything, so a bad path cannot leave a
	// half-applied import behind.
	abs := make([]string, 0, len(wanted))
	for _, d := range wanted {
		a, aerr := filepath.Abs(d.Path)
		if aerr != nil {
			a = d.Path
		}
		if !isRegularFile(a) {
			return nil, fmt.Errorf("kimi: %q is not a file, so it cannot be bound", d.Path)
		}
		if !isCLIBinaryName(filepath.Base(a)) {
			return nil, fmt.Errorf("kimi: %q is not a kimi CLI executable; only the CLI itself can be bound", d.Path)
		}
		abs = append(abs, a)
	}

	st, err := c.mutateState(func(st *accountState) error {
		for _, a := range abs {
			if _, ok := st.bindingByPath(a); ok {
				continue // idempotent: importing twice must not duplicate
			}
			st.Bindings = append(st.Bindings, bindingRecord{
				ID:      bindingID(a),
				Kind:    bindingKindBinary,
				Path:    a,
				Label:   a,
				AddedAt: time.Now().UTC().Format(time.RFC3339),
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Binding an executable -- including re-binding one this module already
	// knows -- is the operator pointing at it and saying "use this", so whatever
	// was remembered about it (a cooldown from a failed run, an old reason) is
	// dropped.  Import is idempotent on the record, and on the health too.
	ids := make([]string, 0, len(abs))
	for _, a := range abs {
		ids = append(ids, bindingID(a))
	}
	c.reviveMany(ids)
	// A binding on disk also disproves "there is no kimi CLI on this machine",
	// which is the only thing that ever puts cli-login into cooldown; leaving
	// that memory in place would keep the panel blaming a CLI that is standing
	// right here.
	if len(ids) > 0 {
		c.reviveOnEvidence(cliLoginID, causeCLINotFound, true)
	}
	c.run.invalidateBinaryCache()

	byID := make(map[string]bindingRecord, len(st.Bindings))
	for _, b := range st.Bindings {
		byID[b.ID] = b
	}
	imported := make([]core.AccountRecord, 0, len(abs))
	for _, a := range abs {
		b, ok := byID[bindingID(a)]
		if !ok {
			continue
		}
		imported = append(imported, c.bindingEntry(b, st).record)
	}
	return imported, nil
}

// ---------------------------------------------------------------------------
// core.LoginProvider: the guided CLI flow
//
// This is the original flow, kept intact but no longer the default.  It is
// guided rather than automated because the CLI's login is an interactive TUI:
// driving *that* from a background HTTP handler would be a screen-scraper with
// no stable contract.  Instead the panel hands the operator the command to run
// and polls for the CLI's own credential to appear.
//
// The automated flow lives in weblogin.go: it speaks RFC 8628 to the vendor
// directly and therefore needs no TUI to scrape.  StartLogin/PollLogin/
// CancelLogin below route between the two by config and by session-id prefix.
// ---------------------------------------------------------------------------

// loginSession is one panel login attempt.  It lives in memory only: a login
// flow that survives a restart is a flow nobody can reason about.
type loginSession struct {
	id        string
	startedAt time.Time
	state     string
	message   string
	accountID string
	// baseline is the credential evidence already present when the session
	// started.  Only *new* evidence counts as success, otherwise an already
	// logged-in machine would report success without anything happening.
	baseline []string
	// identity is the signature of ~/.kimi-webbridge/identity.json at start.
	identity string
}

func (s *accountStore) putSession(sess *loginSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions == nil {
		s.sessions = map[string]*loginSession{}
	}
	s.sessions[sess.id] = sess
}

func loginStateOf(s *loginSession) core.LoginState {
	return core.LoginState{
		SessionID: s.id,
		State:     s.state,
		Message:   s.message,
		AccountID: s.accountID,
	}
}

// startCLILogin is the guided CLI flow, selected by `login_mode: "cli"`.
func (c *Client) startCLILogin(ctx context.Context) (core.LoginState, error) {
	_ = ctx

	bin, binErr := c.run.binaryPath()
	sess := &loginSession{id: loginSessionPrefix + randHex(12), startedAt: time.Now()}

	if binErr != nil {
		sess.state = core.LoginFailed
		sess.message = "the kimi CLI was not found on PATH or in any known install location, so `kimi login` cannot run: " + installHint
		c.acct.putSession(sess)
		return loginStateOf(sess), nil
	}

	creds := c.credentials()
	if refs := creds.foundRefs(); len(refs) > 0 {
		sess.state = core.LoginSuccess
		sess.accountID = cliLoginID
		sess.message = fmt.Sprintf("already logged in: credential evidence at %s. Nothing to do; run `kimi logout` in a terminal first if you want to log in as somebody else.",
			strings.Join(refs, ", "))
		c.acct.putSession(sess)
		return loginStateOf(sess), nil
	}
	if c.cfg.AssumeLoggedIn {
		sess.state = core.LoginSuccess
		sess.accountID = cliLoginID
		sess.message = "assume_logged_in is set, so this module already treats the CLI as logged in; nothing to do."
		c.acct.putSession(sess)
		return loginStateOf(sess), nil
	}

	sess.state = core.LoginPending
	sess.baseline = creds.foundIDs()
	sess.identity = identitySignature()
	sess.message = fmt.Sprintf(
		"kimi CLI found at %s. This module does not drive the CLI's interactive login, so open a terminal and run `kimi login` yourself "+
			"(RFC 8628 device-code flow: the CLI prints a code and a URL). This session turns to success as soon as the CLI's credential "+
			"appears, or when ~/.kimi-webbridge/identity.json changes.", bin)
	c.acct.putSession(sess)
	return loginStateOf(sess), nil
}

// pollCLILogin advances a guided CLI login session.
func (c *Client) pollCLILogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	_ = ctx
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("kimi: PollLogin: empty session id")
	}

	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()

	sess, ok := c.acct.sessions[id]
	if !ok {
		return core.LoginState{}, fmt.Errorf("kimi: unknown login session %q (it may have been cancelled, or this process was restarted; start a new login)", id)
	}
	if sess.state != core.LoginPending {
		return loginStateOf(sess), nil
	}

	// Force a fresh probe: the login may have happened seconds ago and the
	// cached report would still be empty.
	c.run.invalidateCredCache()
	creds := c.credentials()
	for _, s := range creds.sources {
		if !s.Found {
			continue
		}
		sid := credentialAccountID(s)
		if !containsString(sess.baseline, sid) {
			sess.state = core.LoginSuccess
			sess.accountID = cliLoginID
			sess.message = fmt.Sprintf("login detected: %s now carries a credential. The token itself was never read by this module.",
				s.Ref)
			return loginStateOf(sess), nil
		}
	}

	// A weaker, explicitly-flagged signal: the webbridge identity file changed.
	// This cannot prove a CLI login (the CLI may keep its token only in the OS
	// keyring, which the standard library cannot read), so the message says so.
	if sig := identitySignature(); sig != "" && sig != sess.identity {
		sess.state = core.LoginSuccess
		sess.accountID = cliLoginID
		sess.message = "the kimi-webbridge identity file changed since this login started. The CLI's token may live only in the OS keyring, " +
			"which this module cannot read, so this success is inferred rather than verified." + loginPollHint
		return loginStateOf(sess), nil
	}

	return loginStateOf(sess), nil
}

// cancelCLILogin discards a guided CLI login session and nothing else: whatever
// the operator started in their terminal is theirs.
func (c *Client) cancelCLILogin(ctx context.Context, sessionID string) error {
	_ = ctx
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("kimi: CancelLogin: empty session id")
	}

	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()

	sess, ok := c.acct.sessions[id]
	if !ok {
		return fmt.Errorf("kimi: unknown login session %q", id)
	}
	sess.state = core.LoginCancelled
	sess.message = "cancelled by the operator; the CLI's own login state was not touched. " +
		"If you already started `kimi login` in a terminal, finish it there."
	return nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// bindingKey normalises a path for comparison.  Windows compares paths
// case-insensitively, and the panel may hand back a path spelled differently
// from the one Discover reported.
func bindingKey(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(filepath.ToSlash(filepath.Clean(p)))
}

// bindingID is a stable, opaque id for one bound path.  It is derived from the
// path alone, so it is idempotent across restarts and leaks nothing.
func bindingID(path string) string {
	sum := sha256.Sum256([]byte(bindingKey(path)))
	return bindingKindBinary + ":" + hex.EncodeToString(sum[:])[:12]
}

func isRegularFile(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// isCLIBinaryName reports whether a file name is the kimi Code CLI itself.
func isCLIBinaryName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "kimi", "kimi.exe", "kimi.cmd", "kimi.bat", "kimi.ps1":
		return true
	}
	return false
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// listDirNames lists the immediate file names of dir, or nothing when it is
// absent.  Read-only, and never recursive.
func listDirNames(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// identityPath is the local webbridge daemon's device-identity file.  It is a
// device id, not a credential; only its change over time is used, never its
// contents.
func identityPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kimi-webbridge", "identity.json")
}

// identitySignature is a size+mtime fingerprint, or "" when the file is absent.
func identitySignature() string {
	p := identityPath()
	if p == "" {
		return ""
	}
	fi, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
