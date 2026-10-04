package qwenwork

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// accounts.go is this module's optional panel surface: core.AccountManager and
// core.LoginProvider.
//
// Nothing here is required by core.Client, and nothing here is visible to
// another module: the panel discovers these capabilities with a type assertion
// on the registered value (see internal/core/accounts.go), so qwenwork can grow
// account management without the gateway, the panel or any sibling module
// learning about it.
//
// QwenWork has no CredentialImporter.  The desktop client stores its session in
// %APPDATA%\QwenWorkCN\auth.dat, which is opaque to a stdlib-only module, and
// inventing an importer that can never import anything would be worse than not
// having one.  The supported way to mint a credential is the PKCE device flow,
// which LoginProvider exposes to the panel.

var (
	_ core.AccountManager = (*Client)(nil)
	_ core.LoginProvider  = (*Client)(nil)
	_ core.Reviver        = (*Client)(nil)
)

const (
	// originConfig marks an account the operator pinned in the config file or
	// the environment; those cannot be removed from the panel, because the
	// config is the authority and a restart would resurrect them.
	originConfig = "config"
	// originStored marks an account that lives only in this module's own
	// accounts.json (a device-flow login or a hand-added token).
	originStored = "stored"

	// probeTimeout bounds one TestAccount round trip.  It is shorter than the
	// chat timeout on purpose: a panel button must answer promptly.
	probeTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// core.AccountManager
// ---------------------------------------------------------------------------

// AccountFields describes the "add account" form.  Everything here is optional
// except the token itself: an operator pasting a credential out of the vendor's
// desktop app usually has nothing else to hand.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "access_token",
			Label:       "Access token",
			Type:        "password",
			Required:    true,
			Placeholder: "security_oauth_token",
			Help:        "The QwenWork session token. Refresh token is optional; without one the account cannot be renewed automatically.",
		},
		{
			Key:         "label",
			Label:       "Label",
			Type:        "text",
			Placeholder: "work laptop",
			Help:        "Shown in the panel instead of the account id. Never sent upstream.",
		},
		{
			Key:   "uid",
			Label: "User id",
			Type:  "text",
			Help:  "Optional. The vendor's user id; with it the account keeps a stable id across token rotations.",
		},
		{
			Key:   "nickname",
			Label: "Nickname",
			Type:  "text",
		},
		{
			Key:   "email",
			Label: "Email",
			Type:  "text",
		},
		{
			Key:   "refresh_token",
			Label: "Refresh token",
			Type:  "password",
			Help:  "Optional. With it the module renews the access token by itself.",
		},
		{
			Key:         "expires_at",
			Label:       "Expires at",
			Type:        "text",
			Placeholder: "2026-12-31T00:00:00Z",
			Help:        "Optional. RFC 3339, \"2006-01-02 15:04:05\", or a unix timestamp. Empty means unknown, not expired.",
		},
	}
}

// Accounts lists every credential the module holds.  It never fails for an
// empty pool: an empty slice is a valid answer, and the panel renders it as an
// empty table rather than an error.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	records := c.pool.panelRecords()
	configured := configuredIDs(c.cfg)
	for i := range records {
		origin := originStored
		if configured[records[i].ID] {
			origin = originConfig
		}
		records[i].Fields["origin"] = origin
		records[i].Fields["removable"] = origin != originConfig
	}
	return records, nil
}

// AddAccount stores one credential and puts it straight into the pool, so the
// next request can use it without a restart.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	token := spec.Field("access_token")
	if token == "" {
		return core.AccountRecord{}, errors.New("access_token is required")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return core.AccountRecord{}, errors.New("access_token must not contain whitespace")
	}

	a := account{
		ID:           spec.Field("id"),
		UID:          spec.Field("uid"),
		Nickname:     spec.Field("nickname"),
		Email:        spec.Field("email"),
		AccessToken:  token,
		RefreshToken: spec.Field("refresh_token"),
		ExpiresAt:    parseExpiry(spec.Field("expires_at")),
		Note:         firstNonEmpty(spec.Field("label"), spec.Label),
		Disabled:     !spec.EnabledOr(true),
	}

	e := c.pool.put(a)
	// put() merges rather than replaces, and mergeAccount ORs the Disabled
	// flags so a re-add can never quietly re-enable a credential the operator
	// parked.  Honour the form's explicit choice separately.
	if err := c.pool.setEnabled(e.acct.id(), spec.EnabledOr(true)); err != nil {
		return core.AccountRecord{}, err
	}
	// A new token means the cached COSY session was built against the old one.
	c.invalidateSession(a)

	recs := c.pool.panelRecords()
	for i := range recs {
		if recs[i].ID == e.acct.id() {
			recs[i].Fields["origin"] = originStored
			recs[i].Fields["removable"] = true
			return recs[i], nil
		}
	}
	return core.AccountRecord{}, fmt.Errorf("account %q vanished after being added", e.acct.id())
}

// RemoveAccount deletes one credential.  A credential the config or the
// environment supplies cannot be deleted here: it would come back on the next
// restart, so the module says so instead of pretending.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("account id is required")
	}
	if configuredIDs(c.cfg)[id] {
		return fmt.Errorf("account %q comes from the configuration and cannot be removed here; drop it from clients.qwenwork instead", id)
	}
	if !c.pool.remove(id) {
		return fmt.Errorf("account %q not found", id)
	}
	return nil
}

// SetAccountEnabled parks or revives one credential without deleting it.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if id == "" {
		return errors.New("account id is required")
	}
	return c.pool.setEnabled(id, enabled)
}

// ReviveAccount clears every runtime penalty for one account and re-enables it.
//
// To the operator "revive" and "enable" are one intent, so this lifts the park
// and the disable flag together.  It never touches the credential: an account
// whose token is genuinely dead has to fail on its next real request, because
// that failure is the evidence the pool needs.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("account %q not found", id)
	}
	c.pool.revive(e)
	return nil
}

// TestAccount makes one real call against the vendor with this credential.  A
// refusal is a result, not an error: the panel needs to show *why* a credential
// is no good, and only an unknown id is a programming mistake.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	e := c.pool.find(id)
	if e == nil {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	started := time.Now()
	res := core.TestResult{AccountID: e.acct.id()}

	acct := c.pool.accountOf(e)
	sess, err := c.sessionFor(acct)
	var models []core.Model
	if err == nil {
		models, err = c.modelList(ctx, acct, sess)
	}
	res.ElapsedMS = time.Since(started).Milliseconds()

	if err != nil {
		kind, msg := classifyErr(err)
		c.pool.markFailure(e, kind, msg)
		c.noteUpstream(false, kind, msg)
		res.OK = false
		res.Error = core.Redact(msg)
		return res, nil
	}

	c.pool.markUsed(e)
	c.noteUpstream(true, kindNone, "")
	res.OK = true
	if len(models) > 0 {
		res.Model = models[0].ID
	}
	res.Reply = fmt.Sprintf("%d model(s) reachable", len(models))
	return res, nil
}

// RefreshAccount renews one credential, or every credential when id is empty.
// It reports per-account outcomes: one account without a refresh token is not a
// reason to fail the whole call.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*entry
	if id == "" {
		targets = c.pool.all()
	} else {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("account %q not found", id)
		}
		targets = []*entry{e}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, e := range targets {
		r := core.RefreshResult{AccountID: e.acct.id()}
		switch {
		case c.pool.refreshTokenOf(e) == "":
			r.Error = "no refresh token stored; the device flow must mint a new credential"
		default:
			if err := c.tryRefresh(ctx, e); err != nil {
				r.Error = core.Redact(err.Error())
			} else {
				r.OK = true
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// core.LoginProvider — the PKCE device flow, driven from the panel
// ---------------------------------------------------------------------------

// panelLogin is one device authorisation started from the panel.  It is
// deliberately in-memory: a restart drops the flow, and the operator simply
// starts another one.  The URL is also mirrored to login.json, which is what
// Status reads to say "open this URL".
type panelLogin struct {
	nonce     string
	url       string
	verifier  string
	startedAt time.Time
	expiresAt time.Time
	cancel    context.CancelFunc

	mu        sync.Mutex
	state     string
	message   string
	accountID string
}

func (l *panelLogin) set(state, message, accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = state
	l.message = message
	if accountID != "" {
		l.accountID = accountID
	}
}

func (l *panelLogin) snapshot() core.LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := core.LoginState{
		SessionID: l.nonce,
		State:     l.state,
		URL:       l.url,
		Message:   l.message,
		AccountID: l.accountID,
	}
	if l.state == core.LoginPending && time.Now().After(l.expiresAt) {
		st.State = core.LoginFailed
		st.Message = "the authorisation window expired; start again"
	}
	return st
}

// StartLogin mints a PKCE challenge, hands the operator a URL, and polls in the
// background until the human authorises or the window closes.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if c.accountsPath == "" {
		return core.LoginState{}, errors.New("no data directory: a credential could not be stored")
	}

	// An already-usable account must not block an explicit sign-in.  The operator
	// pressed 登录, so run the flow: authorising the same vendor account updates
	// that record (the pool is keyed by uid), a different one adds a second.  A
	// hard stop here left the button looking broken — it reported success with no
	// URL and no way to act on the advice it printed.
	haveUsable := c.pool.ready()

	verifier, err := newPKCEVerifier()
	if err != nil {
		return core.LoginState{}, err
	}
	nonce := newUUID()
	machineID := newUUID()
	authURL := deviceAuthURL(c.cfg.baseURL(), pkceChallengeS256(verifier), nonce, machineID, c.cfg.clientID(), c.cfg.redirectURI())

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
	sess := &panelLogin{
		nonce:     nonce,
		url:       authURL,
		verifier:  verifier,
		startedAt: time.Now(),
		expiresAt: time.Now().Add(c.cfg.loginTimeout()),
		cancel:    cancel,
		state:     core.LoginPending,
		message:   "open the URL in a browser and authorise access",
	}
	if haveUsable {
		sess.message += " (an account is already usable; authorising the same vendor account updates it, a different one adds a second)"
	}
	c.putLogin(sess)

	// Mirror the pending flow where Status already looks for it, so the client
	// card and the account screen tell the operator the same thing.
	if c.loginPath != "" {
		rec := loginRecord{
			URL:       authURL,
			Nonce:     nonce,
			Verifier:  verifier,
			StartedAt: sess.startedAt.Unix(),
			ExpiresAt: sess.expiresAt.Unix(),
		}
		if err := core.WriteJSONAtomic(c.loginPath, rec); err != nil {
			c.deps.Log("qwenwork: writing %s: %v", loginPathBase(), err)
		}
	}

	core.GoSafe("qwenwork panel login", func(msg string) { c.deps.Log("qwenwork: %s", msg) },
		func() { c.runPanelLogin(runCtx, sess) })
	return sess.snapshot(), nil
}

// runPanelLogin polls the vendor until the human authorises, the window closes
// or CancelLogin stops it, then records the outcome on the session.
func (c *Client) runPanelLogin(ctx context.Context, sess *panelLogin) {
	defer sess.cancel()
	interval := c.cfg.pollInterval()
	for {
		grant, pending, err := c.pollGrant(ctx, sess.nonce, sess.verifier)
		if err != nil {
			// A single failed poll is not fatal: the human may still be
			// clicking through the consent screen.
			c.deps.Log("qwenwork: panel login poll: %v", err)
		}
		if !pending && err == nil {
			acct := account{
				UID:          grant.UserID,
				Nickname:     grant.UserName,
				AccessToken:  grant.accessToken(),
				RefreshToken: grant.RefreshToken,
				ExpiresAt:    grant.expiryUnix(time.Now()),
				CreatedAt:    time.Now().Unix(),
			}
			if err := upsertStoredAccount(c.accountsPath, acct); err != nil {
				sess.set(core.LoginFailed, "authorised, but the credential could not be stored: "+err.Error(), "")
				return
			}
			c.reloadAccounts()
			c.removeLoginFile()
			sess.set(core.LoginSuccess, "authorised; the credential is stored and live", acct.id())
			return
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				sess.set(core.LoginCancelled, "cancelled", "")
			} else {
				sess.set(core.LoginFailed, "authorisation was not completed in time", "")
			}
			return
		case <-timer.C:
		}
	}
}

// PollLogin reports the state of one panel login.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if sessionID == "" {
		return core.LoginState{}, errors.New("session id is required")
	}
	sess := c.loginByID(sessionID)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("login session %q not found", sessionID)
	}
	return sess.snapshot(), nil
}

// CancelLogin stops a pending flow.  It never logs anything out: the vendor's
// consent screen is the only thing that can grant a credential, so abandoning
// the poll simply leaves no credential behind.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	sess := c.loginByID(sessionID)
	if sess == nil {
		return fmt.Errorf("login session %q not found", sessionID)
	}
	sess.cancel()
	sess.set(core.LoginCancelled, "cancelled", "")
	c.removeLoginFile()
	c.forgetLogin(sessionID)
	return nil
}

// ---------------------------------------------------------------------------
// panel-login bookkeeping
// ---------------------------------------------------------------------------

func (c *Client) putLogin(sess *panelLogin) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.panels == nil {
		c.panels = map[string]*panelLogin{}
	}
	c.panels[sess.nonce] = sess
}

func (c *Client) loginByID(id string) *panelLogin {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.panels[id]
}

func (c *Client) forgetLogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.panels, id)
}

func (c *Client) removeLoginFile() {
	if c.loginPath == "" {
		return
	}
	if err := os.Remove(c.loginPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.deps.Log("qwenwork: removing %s: %v", loginPathBase(), err)
	}
}

// ---------------------------------------------------------------------------
// pool helpers used by the panel surface
// ---------------------------------------------------------------------------

// panelRecords renders every entry, including parked and disabled ones, so an
// operator can always see and revive what they turned off.
func (p *pool) panelRecords() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountRecord, 0, len(p.entries))
	for _, e := range p.entries {
		p.availableLocked(e, now)
		rec := core.AccountRecord{
			ID:    e.acct.id(),
			Label: panelLabel(e.acct),
			// e.note is the transient status text; acct.Note is the operator's
			// own label, so it is not repeated here.
			Enabled: !e.acct.Disabled,
			State:   e.state,
			Note:    core.Redact(e.note),
			Fields: map[string]any{
				"failures": e.fails,
			},
		}
		if e.acct.UID != "" {
			rec.Fields["uid"] = e.acct.UID
		}
		if e.acct.Nickname != "" {
			rec.Fields["nickname"] = e.acct.Nickname
		}
		if e.acct.Email != "" {
			rec.Fields["email"] = e.acct.Email
		}
		if e.acct.RefreshToken != "" {
			rec.Fields["refreshable"] = true
		}
		if e.acct.ExpiresAt > 0 {
			expiry := time.Unix(e.acct.ExpiresAt, 0).UTC()
			rec.ExpiresAt = expiry.Format(time.RFC3339)
			rec.Fields["expires_in"] = int64(time.Until(expiry).Seconds())
		}
		if e.until.After(now) {
			rec.Fields["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// find returns the live entry for an id, or nil.
func (p *pool) find(id string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.acct.id() == id {
			return e
		}
	}
	return nil
}

// all returns every entry in a stable order.
func (p *pool) all() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*entry, len(p.entries))
	copy(out, p.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].acct.id() < out[j].acct.id() })
	return out
}

// accountOf copies one entry's credential out from under the lock.
func (p *pool) accountOf(e *entry) account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.acct
}

// refreshTokenOf reads one entry's refresh token out from under the lock.
func (p *pool) refreshTokenOf(e *entry) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.acct.RefreshToken
}

// remove deletes one entry and persists both files.  It reports whether the id
// existed, so the caller can tell "deleted" from "never there".
func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.acct.id() != id {
			continue
		}
		p.entries = append(p.entries[:i], p.entries[i+1:]...)
		p.saveLocked()
		p.flushStateLocked(true)
		return true
	}
	return false
}

// setEnabled parks or revives one entry.  Disabling is a hard park: the state
// label becomes "invalid" so the pool cannot hand the account to a request,
// while the credential itself stays on disk for the operator to revive.
func (p *pool) setEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.acct.id() != id {
			continue
		}
		e.acct.Disabled = !enabled
		if enabled {
			e.free()
			e.note = ""
		} else {
			e.until = time.Time{}
			e.acct.CooldownUntil = 0
			e.state = stateInvalid
			e.note = firstNonEmpty(e.note, "disabled by the operator")
		}
		p.flushStateLocked(true)
		return nil
	}
	return fmt.Errorf("account %q not found", id)
}

// ---------------------------------------------------------------------------
// small shared helpers
// ---------------------------------------------------------------------------

// panelLabel is the human handle shown in the panel.  An operator who typed a
// label into the add-account form expects to see it, so it wins over the
// derived nickname/uid handle; label() still backs every account that never
// got one.
func panelLabel(a account) string {
	return firstNonEmpty(a.Note, a.label())
}

// configuredIDs is the set of ids the config and the environment pin.  Those
// accounts are the config's to remove, not the panel's.
func configuredIDs(cfg config) map[string]bool {
	out := map[string]bool{}
	for _, a := range configuredAccounts(cfg) {
		out[a.id()] = true
	}
	return out
}

// classifyErr maps a Go error onto the pool's failure vocabulary.  A transport
// error is "network" so Status can say the upstream is unreachable; anything
// else earns no cooldown, because the fault is not the credential's.
func classifyErr(err error) (errKind, string) {
	if err == nil {
		return kindNone, ""
	}
	if ue, ok := asUpstreamError(err); ok {
		return ue.Kind, ue.Error()
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return kindNetwork, err.Error()
	}
	return kindNone, err.Error()
}

// errText renders an error for a panel message without ever returning "".
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func loginPathBase() string { return loginFile }
