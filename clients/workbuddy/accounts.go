package workbuddy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// accounts.go is this module's optional panel surface: core.AccountManager and
// core.CredentialImporter.
//
// Nothing here is required by core.Client and nothing here is visible to
// another module: the panel discovers these capabilities with a type assertion
// on the registered value (see internal/core/accounts.go), so workbuddy can
// grow account management without the gateway, the panel or any sibling module
// learning about it.
//
// A WorkBuddy "account" is one credential JSON file in the module's accounts
// directory.  That makes every panel operation a file operation, which is why
// this module can offer something the token-only modules cannot: disabling an
// account is a rename, so it survives a restart and the file stays on disk for
// the operator to inspect.

var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.LoginProvider      = (*Client)(nil)
	_ core.Reviver            = (*Client)(nil)
)

const (
	// disabledSuffix marks a credential the operator parked.  LoadAccounts
	// globs "*.json", so a parked file is invisible to the pool until it is
	// renamed back.
	disabledSuffix = ".disabled"
	// disabledState is the state label the panel shows for a parked file.
	disabledState = "disabled"
	// originFile marks a credential discovered on disk; originPanel marks one
	// the operator created from the panel.  Both are removable — the accounts
	// directory *is* this module's store — but the panel says which is which.
	originFile  = "file"
	originPanel = "panel"
	// panelFilePrefix keeps panel-created credentials recognisable on disk.
	panelFilePrefix = "wb-"
	// probeTimeout bounds one TestAccount round trip.
	probeTimeout = 45 * time.Second
	// discoverDepth and discoverLimit bound the vendor-store walk so the
	// panel's Import screen cannot stall on a huge directory tree.
	discoverDepth = 3
	discoverLimit = 200
)

// ---------------------------------------------------------------------------
// core.AccountManager
// ---------------------------------------------------------------------------

// AccountFields describes the "add account" form.  Only the access token is
// required: an operator pasting a credential usually has nothing else, and
// every other field has a working default.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "access_token",
			Label:       "Access token",
			Type:        "password",
			Required:    true,
			Placeholder: "eyJ…",
			Help:        "The WorkBuddy session token. Written to a new credential file in the accounts directory.",
		},
		{
			Key:   "refresh_token",
			Label: "Refresh token",
			Type:  "password",
			Help:  "Optional. Without it the module cannot renew the session and the account dies when the token expires.",
		},
		{
			Key:         "nickname",
			Label:       "Label",
			Type:        "text",
			Placeholder: "work laptop",
			Help:        "Shown in the panel. Never sent upstream.",
		},
		{
			Key:   "uid",
			Label: "User id",
			Type:  "text",
			Help:  "Optional. With it the account keeps a stable id, and its file is named after it.",
		},
		{
			Key:   "enterprise_id",
			Label: "Enterprise id",
			Type:  "text",
		},
		{
			Key:         "domain",
			Label:       "Domain",
			Type:        "text",
			Placeholder: "workbuddy.ai",
			Help:        "Optional. The tenant domain sent as an origin hint; it also decides the realm when realm is left blank.",
		},
		{
			Key:     "realm",
			Label:   "Realm",
			Type:    "select",
			Options: []string{"", realmCN, realmGlobal},
			Help:    "Which upstream to talk to. Blank infers it from the domain.",
		},
		{
			Key:         "expires_at",
			Label:       "Expires at",
			Type:        "text",
			Placeholder: "2026-12-31T00:00:00Z",
			Help:        "Optional. RFC 3339 or a unix timestamp. Blank means unknown, not expired.",
		},
		{
			Key:   "device_token",
			Label: "Device token",
			Type:  "password",
			Help:  "Optional. Only needed when the tenant binds requests to a device fingerprint.",
		},
	}
}

// Accounts lists every credential this module knows about, including the ones
// parked on disk, which the pool cannot see.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.refreshAccounts(false)

	records := c.poolRecords()
	for _, rec := range c.disabledRecords() {
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

// poolRecords renders the live pool for the panel.
func (c *Client) poolRecords() []core.AccountRecord {
	auths := c.pool.Accounts()
	states := map[string]core.AccountStatus{}
	for _, s := range c.pool.Snapshot() {
		states[s.ID] = s
	}
	out := make([]core.AccountRecord, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		id := a.ID()
		st := states[id]
		rec := core.AccountRecord{
			ID:      id,
			Label:   a.Label(),
			Enabled: true,
			State:   st.State,
			Note:    st.Note,
			Fields: map[string]any{
				"realm":  a.RealmName(),
				"file":   filepath.Base(a.FilePath),
				"origin": originFile,
			},
		}
		if exp := a.ExpiryTime(); !exp.IsZero() {
			rec.ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		if st.Extra != nil {
			if n, ok := st.Extra["failures"]; ok {
				rec.Fields["failures"] = n
			}
		}
		if rec.State == "" {
			rec.State = stateUnknown
		}
		if rec.State != stateReady {
			// The pool can park an account without removing it; the panel must
			// not offer a switch that looks like it is already off.
			rec.Enabled = true
		}
		if strings.HasPrefix(filepath.Base(a.FilePath), panelFilePrefix) {
			rec.Fields["origin"] = originPanel
		}
		out = append(out, rec)
	}
	return out
}

// disabledRecords lists the parked files the pool cannot see.
func (c *Client) disabledRecords() []core.AccountRecord {
	dir := c.accountsDir()
	if dir == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"+disabledSuffix))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	out := make([]core.AccountRecord, 0, len(matches))
	for _, p := range matches {
		base := filepath.Base(p)
		origin := originFile
		if strings.HasPrefix(base, panelFilePrefix) {
			origin = originPanel
		}
		rec := core.AccountRecord{
			ID:      disabledID(p),
			Label:   disabledLabel(p),
			Enabled: false,
			State:   disabledState,
			Note:    "parked by the operator; the credential file is still on disk",
			Fields: map[string]any{
				"file":   base,
				"origin": origin,
			},
		}
		// The file still parses, so its realm and expiry can be shown.
		if raw, err := os.ReadFile(p); err == nil {
			if a, err := ParseAuth(raw); err == nil {
				rec.Fields["realm"] = a.RealmName()
				if exp := a.ExpiryTime(); !exp.IsZero() {
					rec.ExpiresAt = exp.UTC().Format(time.RFC3339)
				}
			}
		}
		out = append(out, rec)
	}
	return out
}

// AddAccount writes one credential file and puts it straight into the pool, so
// the next request can use it without a restart.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	dir := c.accountsDir()
	if dir == "" {
		return core.AccountRecord{}, errors.New("no data directory configured: a credential could not be stored")
	}

	token := strings.TrimSpace(spec.Field("access_token"))
	if token == "" {
		return core.AccountRecord{}, errors.New("access_token is required")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return core.AccountRecord{}, errors.New("access_token must not contain whitespace")
	}
	realm := strings.ToLower(strings.TrimSpace(spec.Field("realm")))
	switch realm {
	case "", realmCN, realmGlobal:
	default:
		return core.AccountRecord{}, fmt.Errorf("realm must be %q, %q or blank", realmCN, realmGlobal)
	}

	uid := strings.TrimSpace(spec.Field("uid"))
	nickname := firstNonEmpty(spec.Field("nickname"), spec.Label)

	a := &Auth{
		AccessToken:  token,
		RefreshToken: strings.TrimSpace(spec.Field("refresh_token")),
		ExpiresAt:    parseExpiry(spec.Field("expires_at")),
		Domain:       strings.TrimSpace(spec.Field("domain")),
		Realm:        realm,
		UID:          uid,
		EnterpriseID: strings.TrimSpace(spec.Field("enterprise_id")),
		Nickname:     nickname,
		DeviceToken:  strings.TrimSpace(spec.Field("device_token")),
	}

	// Reuse the existing file when this account is already on disk, so adding
	// the same uid twice updates rather than duplicates.
	if existing := c.findAuthByUID(uid); existing != nil && uid != "" {
		a.FilePath = existing.FilePath
	} else {
		a.FilePath = filepath.Join(dir, panelFileName(uid))
		if _, err := os.Stat(a.FilePath); err == nil {
			// A file is already there but holds a different account: never
			// clobber it, pick a free name instead.
			a.FilePath = filepath.Join(dir, panelFileNameSuffixed(uid, randomSuffix()))
		}
	}

	if err := core.EnsureDir(dir); err != nil {
		return core.AccountRecord{}, fmt.Errorf("cannot create %s: %w", dir, err)
	}
	if err := a.SaveAtomic(); err != nil {
		return core.AccountRecord{}, err
	}
	c.refreshAccounts(true)

	for _, rec := range c.poolRecords() {
		if rec.ID == a.ID() {
			rec.Fields["origin"] = originPanel
			return rec, nil
		}
	}
	return core.AccountRecord{}, fmt.Errorf("account %q was written but did not load", a.ID())
}

// RemoveAccount deletes one credential file.  Every account this module knows
// about lives in its own accounts directory, so removal is always possible —
// but a parked file has to be removed by its file handle, since the pool cannot
// see it.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	if a := c.findAuth(id); a != nil {
		if a.FilePath == "" {
			return fmt.Errorf("account %q has no file to remove", id)
		}
		if err := os.Remove(a.FilePath); err != nil {
			return fmt.Errorf("cannot remove %s: %w", filepath.Base(a.FilePath), err)
		}
		c.refreshAccounts(true)
		return nil
	}
	if p, ok := c.findDisabled(id); ok {
		if err := os.Remove(p); err != nil {
			return fmt.Errorf("cannot remove %s: %w", filepath.Base(p), err)
		}
		c.refreshAccounts(true)
		return nil
	}
	return fmt.Errorf("account %q not found", id)
}

// SetAccountEnabled parks or revives one credential by renaming its file.
// Because the pool loads "*.json" and a parked file ends in ".json.disabled",
// the rename is all it takes — and it survives a restart.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	if enabled {
		p, ok := c.findDisabled(id)
		if !ok {
			if a := c.findAuth(id); a != nil {
				return nil // already live
			}
			return fmt.Errorf("account %q not found", id)
		}
		target := strings.TrimSuffix(p, disabledSuffix)
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("cannot revive %s: %s already exists",
				filepath.Base(p), filepath.Base(target))
		}
		if err := os.Rename(p, target); err != nil {
			return fmt.Errorf("cannot revive %s: %w", filepath.Base(p), err)
		}
		c.refreshAccounts(true)
		return nil
	}

	a := c.findAuth(id)
	if a == nil {
		if _, ok := c.findDisabled(id); ok {
			return nil // already parked
		}
		return fmt.Errorf("account %q not found", id)
	}
	if a.FilePath == "" {
		return fmt.Errorf("account %q has no file to park", id)
	}
	if err := os.Rename(a.FilePath, a.FilePath+disabledSuffix); err != nil {
		return fmt.Errorf("cannot park %s: %w", filepath.Base(a.FilePath), err)
	}
	c.refreshAccounts(true)
	return nil
}

// ReviveAccount implements core.Reviver: the operator's override for one
// account.  To the operator "revive" and "enable" are one intent, so a
// credential parked on disk is renamed back as well as cleared in memory.
//
// Nothing here mints or refreshes anything.  An account whose token is genuinely
// dead comes back exactly as dead as it was, and its next real request says so;
// inventing a live credential here would hide that evidence from the pool.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	c.refreshAccounts(false)
	a := c.findAuth(id)
	if a == nil {
		if _, parked := c.findDisabled(id); !parked {
			return fmt.Errorf("account %q not found", id)
		}
		// The rename puts the credential back in the "*.json" set the pool
		// loads, which is also what gives the pool an entry to clear.
		if err := c.SetAccountEnabled(ctx, id, true); err != nil {
			return err
		}
		a = c.findAuth(id)
		if a == nil {
			return fmt.Errorf("account %q was enabled but did not reload", id)
		}
	}
	c.pool.Revive(a)
	return nil
}

// TestAccount makes one real call against the vendor with this credential.  A
// refusal is a result, not an error: the panel needs to show *why* a credential
// is no good, and only an unknown id is a programming mistake.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	a := c.findAuth(id)
	if a == nil {
		if _, ok := c.findDisabled(id); ok {
			return core.TestResult{
				AccountID: id,
				OK:        false,
				Error:     "the account is parked; enable it before testing",
			}, nil
		}
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	type probe struct {
		models []core.Model
		err    error
	}
	started := time.Now()
	ch := make(chan probe, 1)
	// GoSafe, not a bare "go": the probe would otherwise take the gateway down,
	// and the report has to fill ch as well -- an empty ch would leave the
	// select below waiting out the whole probe timeout.
	core.GoSafe("workbuddy account probe", func(msg string) {
		ch <- probe{err: errors.New(msg)}
	}, func() {
		models, err := c.fetchModelsBounded(a, a.RealmName())
		ch <- probe{models, err}
	})

	res := core.TestResult{AccountID: a.ID()}
	select {
	case r := <-ch:
		res.ElapsedMS = time.Since(started).Milliseconds()
		if r.err != nil {
			kind, _ := c.pool.MarkFailure(a, r.err)
			c.up.log("workbuddy: panel test of %s failed (%s)", core.MaskSecret(a.ID()), kind)
			res.OK = false
			res.Error = core.Redact(describeFailure(r.err))
			return res, nil
		}
		c.pool.MarkSuccess(a)
		res.OK = true
		if len(r.models) > 0 {
			res.Model = r.models[0].ID
		}
		res.Reply = fmt.Sprintf("%d model(s) reachable", len(r.models))
		return res, nil
	case <-ctx.Done():
		res.ElapsedMS = time.Since(started).Milliseconds()
		res.OK = false
		res.Error = "the catalogue fetch did not finish in time"
		return res, nil
	}
}

// RefreshAccount renews one credential, or every credential when id is empty.
// It reports per-account outcomes: one account without a refresh token is not a
// reason to fail the whole call.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*Auth
	if strings.TrimSpace(id) == "" {
		targets = c.pool.Accounts()
	} else {
		a := c.findAuth(id)
		if a == nil {
			if _, ok := c.findDisabled(id); ok {
				return []core.RefreshResult{{
					AccountID: id,
					OK:        false,
					Error:     "the account is parked; enable it before refreshing",
				}}, nil
			}
			return nil, fmt.Errorf("account %q not found", id)
		}
		targets = []*Auth{a}
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, a := range targets {
		r := core.RefreshResult{AccountID: a.ID()}
		switch {
		case a.RefreshTokenValue() == "":
			r.Error = "no refresh token stored; place a fresh credential file instead"
		default:
			if err := c.refreshAccount(a); err != nil {
				r.Error = core.Redact(describeFailure(err))
			} else {
				r.OK = true
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// core.CredentialImporter
// ---------------------------------------------------------------------------

// Discover reports what could be imported.  The vendor's own stores are sealed
// in Tencent "$wbEncrypted" envelopes, so they are reported as not importable
// rather than hidden — that is the answer to "why is the Import screen empty".
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	c.refreshAccounts(false)
	existing := map[string]bool{}
	for _, a := range c.pool.Accounts() {
		if a != nil && a.FilePath != "" {
			existing[a.FilePath] = true
		}
	}

	out := make([]core.DiscoveredCredential, 0, 8)

	// Credentials already in the module's own directory are listed as imported.
	dir := c.accountsDir()
	if dir != "" {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		sort.Strings(matches)
		for _, p := range matches {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			a, err := ParseAuth(raw)
			if err != nil {
				continue
			}
			out = append(out, core.DiscoveredCredential{
				Path:       p,
				Kind:       "credential-file",
				Label:      firstNonEmpty(a.Label(), filepath.Base(p)),
				Note:       "already in the accounts directory",
				Importable: false,
				Imported:   true,
			})
		}
	}

	// Then the vendor's own locations.
	for _, store := range vendorCredentialStores() {
		found := 0
		_ = walkJSON(store.Path, 0, func(p string, raw []byte) {
			if found >= discoverLimit || existing[p] {
				return
			}
			found++
			a, err := ParseAuth(raw)
			if err != nil {
				// A file that looks like a credential but does not parse is
				// worth naming: it is usually the encrypted store.
				out = append(out, core.DiscoveredCredential{
					Path:       p,
					Kind:       "unreadable",
					Label:      filepath.Base(p),
					Note:       "not a plaintext credential (" + err.Error() + ")",
					Importable: false,
				})
				return
			}
			out = append(out, core.DiscoveredCredential{
				Path:       p,
				Kind:       "credential-file",
				Label:      firstNonEmpty(a.Label(), filepath.Base(p)),
				Note:       "plaintext credential found in " + store.Note,
				Importable: true,
			})
		})
	}
	return out, nil
}

// Import copies the named files into the accounts directory.  With no paths and
// all set, it imports everything Discover marked importable.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	dir := c.accountsDir()
	if dir == "" {
		return nil, errors.New("no data directory configured: an import could not be stored")
	}

	candidates := paths
	if len(candidates) == 0 {
		if !all {
			return nil, errors.New("no paths given")
		}
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			if d.Importable && !d.Imported {
				candidates = append(candidates, d.Path)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("nothing to import")
	}

	// The result is one record per candidate, so the panel can show which
	// copies worked and which did not.  A path that fails becomes a minimal
	// record carrying the reason; it never hides the successes.
	out := make([]core.AccountRecord, 0, len(candidates))
	failed := make([]core.AccountRecord, 0, 2)
	imported := map[string]string{} // account id -> file name it landed in

	for _, p := range candidates {
		fail := func(note string) {
			failed = append(failed, core.AccountRecord{
				ID:      p,
				Label:   filepath.Base(p),
				Enabled: false,
				State:   "error",
				Note:    note,
			})
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			fail("cannot read: " + err.Error())
			continue
		}
		a, err := ParseAuth(raw)
		if err != nil {
			fail("not a plaintext credential: " + err.Error())
			continue
		}
		if err := core.EnsureDir(dir); err != nil {
			fail("cannot create the accounts directory: " + err.Error())
			continue
		}
		target := filepath.Join(dir, panelFileName(a.UID))
		if filepath.Dir(target) == filepath.Dir(p) && filepath.Base(target) == filepath.Base(p) {
			// Already home: nothing to copy, but still worth reporting.
			a.FilePath = p
			imported[a.ID()] = filepath.Base(target)
			continue
		}
		if _, err := os.Stat(target); err == nil {
			target = filepath.Join(dir, panelFileNameSuffixed(a.UID, randomSuffix()))
		}
		a.FilePath = target
		if err := a.SaveAtomic(); err != nil {
			fail("cannot write the copy: " + err.Error())
			continue
		}
		imported[a.ID()] = filepath.Base(target)
	}

	c.refreshAccounts(true)
	live := map[string]core.AccountRecord{}
	for _, rec := range c.poolRecords() {
		live[rec.ID] = rec
	}
	for _, rec := range c.disabledRecords() {
		if _, dup := live[rec.ID]; !dup {
			live[rec.ID] = rec
		}
	}

	ids := make([]string, 0, len(imported))
	for id := range imported {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		base := imported[id]
		rec, ok := live[id]
		if !ok {
			// The copy is on disk but the pool has not picked it up: say so
			// rather than reporting a success the operator cannot see.
			rec = core.AccountRecord{
				ID:      id,
				Label:   base,
				Enabled: true,
				State:   stateUnknown,
				Note:    "imported as " + base,
				Fields:  map[string]any{"file": base, "origin": originPanel},
			}
		} else {
			if rec.Fields == nil {
				rec.Fields = map[string]any{}
			}
			rec.Fields["origin"] = originPanel
			if rec.Note == "" {
				rec.Note = "imported as " + base
			}
		}
		out = append(out, rec)
	}
	out = append(out, failed...)
	return out, nil
}

// ---------------------------------------------------------------------------
// lookup helpers
// ---------------------------------------------------------------------------

// findAuth returns the live credential for a panel id, or nil.
func (c *Client) findAuth(id string) *Auth {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	for _, a := range c.pool.Accounts() {
		if a == nil {
			continue
		}
		if a.ID() == id || (a.FilePath != "" && filepath.Base(a.FilePath) == id) {
			return a
		}
	}
	return nil
}

// findAuthByUID is findAuth restricted to the uid form, used when deciding
// whether an add should update an existing file.
func (c *Client) findAuthByUID(uid string) *Auth {
	if strings.TrimSpace(uid) == "" {
		return nil
	}
	for _, a := range c.pool.Accounts() {
		if a != nil && a.UIDValue() == uid {
			return a
		}
	}
	return nil
}

// findDisabled returns the parked file for a panel id.  It matches both the
// parsed uid and the file name, so a parked account stays addressable even if
// it never parsed.
func (c *Client) findDisabled(id string) (string, bool) {
	dir := c.accountsDir()
	if dir == "" {
		return "", false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"+disabledSuffix))
	sort.Strings(matches)
	for _, p := range matches {
		if disabledID(p) == id || filepath.Base(p) == id {
			return p, true
		}
	}
	return "", false
}

// disabledID is the panel id for a parked file: the uid when the file still
// parses, else the file name without the suffix.
func disabledID(path string) string {
	if raw, err := os.ReadFile(path); err == nil {
		if a, err := ParseAuth(raw); err == nil && a.UID != "" {
			return a.UID
		}
	}
	return strings.TrimSuffix(filepath.Base(path), disabledSuffix)
}

func disabledLabel(path string) string {
	if raw, err := os.ReadFile(path); err == nil {
		if a, err := ParseAuth(raw); err == nil {
			if l := a.Label(); l != "" {
				return l
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), disabledSuffix)
}

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

// panelFileName builds a safe credential file name for an account.  A uid makes
// the name stable across restarts; without one a random suffix keeps two
// accounts from colliding.
func panelFileName(uid string) string {
	return panelFileNameSuffixed(uid, "")
}

// panelFileNameSuffixed is panelFileName with a disambiguating suffix inserted
// *before* the extension.  Appending the suffix to the whole name instead would
// produce "wb-uid-x.json-abc123", which no longer matches the "*.json" glob the
// pool loads, so the account would be written and then silently ignored.
func panelFileNameSuffixed(uid, suffix string) string {
	slug := sanitizeFileComponent(uid)
	if slug == "" {
		slug = randomSuffix()
	}
	if s := sanitizeFileComponent(suffix); s != "" {
		slug += "-" + s
	}
	return panelFilePrefix + slug + ".json"
}

// sanitizeFileComponent keeps only characters that are safe in a file name on
// every platform this builds for.
func sanitizeFileComponent(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// randomSuffix is a short, collision-resistant file-name suffix.  It uses the
// module's own id generator so there is one source of randomness in the module.
func randomSuffix() string {
	id := newMessageID()
	if len(id) > 8 {
		id = id[:8]
	}
	return sanitizeFileComponent(id)
}

// parseExpiry accepts RFC 3339, "2006-01-02 15:04:05", or a unix timestamp in
// seconds or milliseconds.  An empty or unparseable value means "unknown", not
// "expired".
func parseExpiry(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		if n > 1e12 { // milliseconds
			return n / 1000
		}
		return n
	}
	return 0
}

// describeFailure renders an upstream error for a panel message without ever
// returning an empty string.
func describeFailure(err error) string {
	if err == nil {
		return ""
	}
	var ue *Error
	if asError(err, &ue) && ue != nil {
		if ue.Msg != "" {
			return ue.Msg
		}
		return ue.Kind.String()
	}
	return err.Error()
}

// walkJSON calls fn for every "*.json" file under root, up to discoverDepth
// levels and discoverLimit files.  Errors are ignored on purpose: a permission
// failure deep in a vendor directory must not empty the Import screen.
func walkJSON(root string, depth int, fn func(path string, raw []byte)) error {
	if depth > discoverDepth {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	n := 0
	for _, e := range entries {
		if n >= discoverLimit {
			return nil
		}
		p := filepath.Join(root, e.Name())
		if e.IsDir() {
			_ = walkJSON(p, depth+1, fn)
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() > 1<<20 {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		n++
		fn(p, raw)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
