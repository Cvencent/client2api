package codearts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// panel.go is the surface the management panel drives: the account list, the
// add/remove/enable/test/refresh actions, and the interactive sign-in flow.
//
// The compile-time assertions that this module satisfies each of these
// interfaces live in codearts.go, next to the interfaces the module
// deliberately does NOT implement.

// probeTimeout bounds one TestAccount call.
const probeTimeout = 30 * time.Second

// AccountFields describes the credential form.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:      "access_key_id",
			Label:    "Access key ID",
			Type:     "text",
			Required: true,
			Help:     "The AK from a CodeArts credential. A plain AK/SK pair can list models but cannot run a completion; sign in to get a security token.",
		},
		{
			Key:      "secret_access_key",
			Label:    "Secret access key",
			Type:     "password",
			Required: true,
			Help:     "The SK that pairs with the access key ID.",
		},
		{
			Key:   "security_token",
			Label: "Security token",
			Type:  "password",
			Help:  "Required to sign a chat request. Empty for a plain AK/SK pair, which cannot complete a conversation.",
		},
		{
			Key:   "refresh_token",
			Label: "Refresh token",
			Type:  "password",
			Help:  "Lets the module renew the credential on its own. Signing in fills this in automatically.",
		},
		{
			Key:   "expires_at",
			Label: "Expires at",
			Type:  "text",
			Help:  "When the security token stops working: RFC 3339, \"2006-01-02 15:04:05\", or a unix timestamp. Empty means unknown, which is never treated as expired.",
		},
		{
			Key:   "label",
			Label: "Label",
			Type:  "text",
			Help:  "Optional name for this account in the panel.",
		},
	}
}

// Accounts lists the pooled credentials.  It never fails for an empty pool:
// an empty list is the honest answer for "nothing is configured".
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	entries := c.pool.all()
	snaps := c.pool.snapshot()
	states := make(map[string]string, len(snaps))
	for _, s := range snaps {
		states[s.ID] = s.State
	}
	out := make([]core.AccountRecord, 0, len(entries))
	now := time.Now()
	for _, e := range entries {
		rec := core.AccountRecord{
			ID:        e.id(),
			Label:     e.label(),
			Enabled:   !e.acct.Disabled,
			State:     states[e.id()],
			ExpiresAt: e.acct.expiresAtString(),
			Note:      e.note,
			Identity:  e.acct.identity(),
			Fields: map[string]any{
				"origin":      e.origin,
				"removable":   e.removable,
				"refreshable": e.acct.refreshable(),
				// How early the account must be renewed.  The scheduler reads
				// this instead of guessing, so a credential idles no longer than
				// the module itself would wait before a chat request.
				"refresh_margin_seconds": int64(c.cfg.refreshMargin().Seconds()),
				"has_token":              strings.TrimSpace(e.acct.SecurityToken) != "",
				"fingerprint":            e.acct.fingerprint(),
				"access_key_id":          maskKey(e.acct.AccessKeyID),
			},
		}
		if e.acct.UserName != "" {
			rec.Fields["user_name"] = e.acct.UserName
		}
		if e.acct.UserID != "" {
			rec.Fields["user_id"] = e.acct.UserID
		}
		if e.acct.expired(now) {
			rec.Fields["expired"] = true
		}
		out = append(out, rec)
	}
	return out, nil
}

// AddAccount stores a credential submitted through the panel.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	a, err := accountFromSpec(spec)
	if err != nil {
		return core.AccountRecord{}, err
	}
	if c.accountsPath == "" {
		return core.AccountRecord{}, errNoDataDir
	}
	now := time.Now().Unix()
	a.CreatedAt, a.UpdatedAt = now, now
	e := c.pool.put(a, spec.EnabledOr(true))
	// A new credential invalidates whatever the account was cooled down for.
	c.pool.revive(e.id())
	return core.AccountRecord{
		ID:        e.id(),
		Label:     e.label(),
		Enabled:   !e.acct.Disabled,
		State:     stateUnknown,
		ExpiresAt: e.acct.expiresAtString(),
		Identity:  e.acct.identity(),
		Fields: map[string]any{
			"origin":                 originStored,
			"removable":              true,
			"refreshable":            e.acct.refreshable(),
			"refresh_margin_seconds": int64(c.cfg.refreshMargin().Seconds()),
			"has_token":              strings.TrimSpace(e.acct.SecurityToken) != "",
			"access_key_id":          maskKey(e.acct.AccessKeyID),
		},
	}, nil
}

// RemoveAccount drops a credential.  A config-sourced account cannot be
// removed here, and the message says where it lives instead.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if !validAccountID(id) {
		return errors.New("codearts: a valid account id is required")
	}
	if err := c.pool.remove(id); err != nil {
		return err
	}
	return nil
}

// SetAccountEnabled parks or unparks a credential.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if !validAccountID(id) {
		return errors.New("codearts: a valid account id is required")
	}
	return c.pool.setEnabled(id, enabled)
}

// TestAccount sends one minimal completion, so the operator learns whether the
// credential actually works.
//
// An upstream refusal is a RESULT, not an error: TestResult.OK is false and
// TestResult.Error carries the reason.  Only an unknown account id is an error.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	started := time.Now()
	res := core.TestResult{AccountID: id}
	e := c.findEntry(id)
	if e == nil {
		return res, fmt.Errorf("codearts: no such account %q", id)
	}
	res.AccountID = e.id()
	model := c.firstModel()
	if model == "" {
		res.Error = "no model is known yet, so the credential could not be exercised"
		res.ElapsedMS = time.Since(started).Milliseconds()
		return res, nil
	}
	res.Model = model

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	maxTok := 16
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &maxTok,
	}
	core.NoteServedBy(req, e.id())
	resp, err := c.sendChat(ctx, e.account(), req)
	if err != nil {
		res.Error = core.Redact(cleanErrorText(err.Error()))
		res.ElapsedMS = time.Since(started).Milliseconds()
		return res, nil
	}
	defer resp.Body.Close()
	st := newStream(ctx, resp.Body, c.cfg.firstTokenWait(), c.cfg.chunkWait(), nil)
	defer st.Close()

	var reply strings.Builder
	for {
		ev, err := st.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, errStreamDone) {
				break
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				break
			}
			if reply.Len() > 0 {
				break
			}
			res.Error = core.Redact(cleanErrorText(err.Error()))
			res.ElapsedMS = time.Since(started).Milliseconds()
			return res, nil
		}
		switch ev.Type {
		case core.EventDelta:
			reply.WriteString(ev.Delta)
		case core.EventDone:
			res.ElapsedMS = time.Since(started).Milliseconds()
			res.OK = true
			res.Reply = truncateReply(reply.String())
			return res, nil
		}
		if reply.Len() > 200 {
			// Enough to prove the credential works; the rest of the answer is
			// not worth waiting for.
			res.OK = true
			res.Reply = truncateReply(reply.String())
			res.ElapsedMS = time.Since(started).Milliseconds()
			return res, nil
		}
	}
	res.OK = reply.Len() > 0
	res.Reply = truncateReply(reply.String())
	res.ElapsedMS = time.Since(started).Milliseconds()
	if !res.OK && res.Error == "" {
		res.Error = "the model accepted the credential but produced no output"
	}
	return res, nil
}

// truncateReply caps what the panel shows.
func truncateReply(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return s
}

// firstModel is the first model in the catalogue, used to exercise a
// credential.  It prefers a free-quota model, because exercising a paid model
// costs the operator money.
func (c *Client) firstModel() string {
	models := c.cachedModels()
	for _, m := range models {
		if c.isBenefitModel(m.ID) {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return ""
}

// RefreshAccount renews credentials (core.AccountManager).
//
// An empty id means every account that can be renewed.  Each account's outcome
// is reported separately, so one dead refresh token does not hide the others'
// results.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*entry
	if strings.TrimSpace(id) == "" {
		targets = c.pool.all()
	} else {
		e := c.findEntry(id)
		if e == nil {
			return nil, fmt.Errorf("codearts: no such account %q", id)
		}
		targets = []*entry{e}
	}
	if len(targets) == 0 {
		return nil, core.ErrNotConfigured
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, e := range targets {
		res := core.RefreshResult{AccountID: e.id()}
		switch {
		case !e.acct.refreshable():
			res.Error = "this credential has no refresh token, so it cannot be renewed; sign in again"
		default:
			if err := c.refreshCredential(ctx, e); err != nil {
				res.Error = core.Redact(cleanErrorText(err.Error()))
				if errors.Is(err, errRefreshTerminal) {
					c.pool.markDead(e, err.Error())
				}
			} else {
				res.OK = true
			}
		}
		out = append(out, res)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// interactive sign-in
// ---------------------------------------------------------------------------

// panelLogin is one in-progress sign-in.
type panelLogin struct {
	mu sync.Mutex

	nonce     string
	url       string
	verifier  string
	startedAt time.Time
	expiresAt time.Time
	cancel    context.CancelFunc

	state     string
	message   string
	accountID string
}

// set records the outcome of the flow.
func (l *panelLogin) set(state, message, accountID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = state
	l.message = message
	if accountID != "" {
		l.accountID = accountID
	}
}

// snapshot renders the session for the panel.
func (l *panelLogin) snapshot() core.LoginState {
	if l == nil {
		return core.LoginState{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := core.LoginState{
		SessionID: l.nonce,
		State:     l.state,
		URL:       l.url,
		Message:   l.message,
		AccountID: l.accountID,
	}
	if st.State == core.LoginPending && time.Now().After(l.expiresAt) {
		st.State = core.LoginFailed
		st.Message = "the authorisation window expired; start again"
	}
	return st
}

// putLogin stores a session in the in-memory map.
func (c *Client) putLogin(l *panelLogin) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*panelLogin)
	}
	c.logins[l.nonce] = l
}

// loginByID looks a session up.
func (c *Client) loginByID(id string) *panelLogin {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// dropLogin forgets a session.
func (c *Client) dropLogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.logins, id)
}

// removeLoginFile clears the mirrored in-progress flow.
func (c *Client) removeLoginFile() {
	if c.loginPath == "" {
		return
	}
	if err := os.Remove(c.loginPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.deps.Log("codearts: removing %s: %v", c.loginPath, err)
	}
}

// StartLogin begins a sign-in (core.LoginProvider).
//
// The flow is the portal's OAuth code flow: a PKCE challenge, a local callback
// server on a port the portal will accept, and an exchange of the code it
// redirects back with.  The URL is returned immediately — the operator has to
// open it in a browser, so waiting for anything here would leave the button
// looking dead.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	verifier, challenge, err := newPKCEChallenge()
	if err != nil {
		return core.LoginState{}, err
	}
	key, err := generateDpopKeyPair()
	if err != nil {
		return core.LoginState{}, err
	}
	ln, err := newLocalCallback()
	if err != nil {
		return core.LoginState{}, err
	}
	authURL := buildAuthorizeURL(ln.port, challenge, "")

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
	sess := &panelLogin{
		nonce:     newUUID(),
		url:       authURL,
		verifier:  verifier,
		startedAt: time.Now(),
		expiresAt: time.Now().Add(c.cfg.loginTimeout()),
		cancel:    cancel,
		state:     core.LoginPending,
		message:   "open the URL in a browser and sign in; this window closes when you are done",
	}
	c.putLogin(sess)
	c.writeLoginFile(sess)

	core.GoSafe("codearts login", nil, func() {
		defer cancel()
		c.runBrowserLogin(runCtx, sess, ln, key)
	})
	return sess.snapshot(), nil
}

// PollLogin reports the state of one sign-in.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if strings.TrimSpace(sessionID) == "" {
		return core.LoginState{}, errors.New("codearts: a session id is required")
	}
	sess := c.loginByID(sessionID)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("codearts: no such login session %q", sessionID)
	}
	st := sess.snapshot()
	if st.State != core.LoginPending {
		c.dropLogin(sessionID)
	}
	return st, nil
}

// CancelLogin stops an in-progress sign-in.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	sess := c.loginByID(sessionID)
	if sess == nil {
		return fmt.Errorf("codearts: no such login session %q", sessionID)
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.set(core.LoginCancelled, "cancelled", "")
	c.dropLogin(sessionID)
	c.removeLoginFile()
	return nil
}

// writeLoginFile mirrors the pending flow, so a restart does not lose the
// operator's place.
func (c *Client) writeLoginFile(sess *panelLogin) {
	if c.loginPath == "" || sess == nil {
		return
	}
	rec := loginRecord{
		URL:       sess.url,
		SessionID: sess.nonce,
		StartedAt: sess.startedAt.Unix(),
		ExpiresAt: sess.expiresAt.Unix(),
	}
	if err := core.WriteJSONAtomic(c.loginPath, rec); err != nil {
		c.deps.Log("codearts: writing %s: %v", c.loginPath, err)
	}
}

// loginRecord is the on-disk shape of an in-progress sign-in.  It carries no
// secret: the PKCE verifier and the DPoP key live only in memory.
type loginRecord struct {
	URL       string `json:"url"`
	SessionID string `json:"session_id"`
	StartedAt int64  `json:"started_at"`
	ExpiresAt int64  `json:"expires_at"`
}
