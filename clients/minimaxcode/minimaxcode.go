// Package minimaxcode relays MiniMax Code (the MiniMax desktop coding client)
// through the OpenAI-compatible gateway.
//
// The upstream was recovered by capturing the desktop client's own traffic:
//
//	POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages
//	Authorization: Bearer <accessToken>
//	anthropic-version: 2023-06-01
//	{"model":"MiniMax-M3","max_tokens":32,"messages":[{"role":"user","content":"…"}]}
//
// so this module is a second Anthropic-Messages translator, independent of the
// others: nothing here is shared with clients/zcode or clients/trae, and it may
// be rewritten without any other module noticing.
//
// Credentials come from the desktop client's own store, which is plain JSON and
// needs no secretbox:
//
//	<home>/.minimax/auth/<buildEnv>/<region>/<clientId>/auth.json
//
// The token inside is a 60-character "mmoat_…" string, not a JWT.
package minimaxcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register(name, New) }

// anthropicVersion is the only API version the captured request carried.
const anthropicVersion = "2023-06-01"

// defaultUserAgent is what the module sends when the operator has not chosen a
// value.  It is INFERRED, not captured: the capture recorded only Authorization,
// Content-Type and anthropic-version, so nothing about the real UA is known.
// MiniMax Code is an Electron application whose provider is '@ai-sdk/anthropic'
// (see ~/.minimax/config.yaml), so its fetch runs on Chromium and sends a
// Chromium UA.  Go's own "Go-http-client/1.1" would be a far louder tell than a
// plausible Chromium string, which is why a default exists at all.
const defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Client is the minimaxcode module.  It is safe for concurrent use.
type Client struct {
	deps core.Deps
	cfg  *Config
	http *http.Client
	pool *pool
	logf func(string, ...any)

	// affinity pins a conversation to the account that has already served it, so
	// a multi-turn exchange keeps one credential and one prompt cache instead of
	// rotating under the caller.  It is built by New; every method that touches
	// it is nil-safe, so a Client assembled by hand still works.  See
	// affinity.go.
	affinity *core.Affinity

	// models is the catalogue, re-read from the desktop client's config.yaml by
	// RefreshModels.  There is no vendor model-list endpoint to call (see
	// README), so "refresh" means re-reading the local file.
	mu     sync.RWMutex
	models []core.Model

	// logins holds the in-flight device-code sign-ins StartLogin opened.  They
	// are keyed by the session id the panel polls with, kept just long enough
	// for the panel's last poll to read the outcome, and swept by the next
	// StartLogin; see login.go and putLogin.
	loginsMu sync.Mutex
	logins   map[string]*loginSession
}

// The sign-in table is guarded for the whole operation rather than handed out:
// the panel can poll a session while the operator starts or cancels another,
// and a map read that raced a map write would be a data race under -race.
func (c *Client) putLogin(s *loginSession) {
	c.loginsMu.Lock()
	defer c.loginsMu.Unlock()
	if c.logins == nil {
		c.logins = map[string]*loginSession{}
	}
	// Starting a sign-in is the natural moment to sweep the table: the panel
	// stops polling the instant it sees a terminal state, so a finished or
	// expired session would otherwise sit here for the life of the process.
	now := time.Now()
	for id, other := range c.logins {
		if other == nil {
			delete(c.logins, id)
			continue
		}
		other.mu.Lock()
		done := other.status != core.LoginPending
		expired := !other.expiresAt.IsZero() && now.After(other.expiresAt)
		other.mu.Unlock()
		if done || expired {
			delete(c.logins, id)
		}
	}
	c.logins[s.id] = s
}

func (c *Client) getLogin(id string) (*loginSession, bool) {
	c.loginsMu.Lock()
	defer c.loginsMu.Unlock()
	s, ok := c.logins[id]
	return s, ok
}

func (c *Client) dropLogin(id string) {
	c.loginsMu.Lock()
	defer c.loginsMu.Unlock()
	delete(c.logins, id)
}

// New builds the module.  It never returns an error for a bad config: a
// degraded module that still reports why is more useful than a missing one.
func New(deps core.Deps) (core.Client, error) {
	cfg := loadConfig(deps.Config, deps.Logf)

	logf := deps.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	hc := deps.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	// A named profile replaces the transport every vendor-facing request goes
	// through, including the account pool's.  MiniMax Code is Electron, so the
	// configured profile is normally "chrome".  The default (empty) keeps the
	// client above, so this is a no-op unless asked for.
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, err := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if err != nil {
			logf("minimaxcode: tls fingerprint %q ignored: %v", string(fp), err)
		} else {
			hc = fhc
			logf("minimaxcode: tls handshake imitates the %s client", string(fp))
		}
	}

	c := &Client{deps: deps, cfg: cfg, http: hc, logf: logf}
	c.pool = newPool(deps.DataDir, cfg, hc, logf)

	// A zero TTL selects core's documented default window (30m, swept every
	// 5m).  ApplyLive can move both afterwards without a restart.
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()

	c.setModels(coreModels(cfg.catalogueRows(logf)))
	return c, nil
}

// Name returns the routing prefix ("minimaxcode/<model>").
func (c *Client) Name() string { return name }

func (c *Client) setModels(models []core.Model) {
	c.mu.Lock()
	c.models = models
	c.mu.Unlock()
}

// Models answers from the cached catalogue.  It is called on every panel
// refresh, so it must stay cheap: reading the vendor's config.yaml on each call
// would be wasteful and would make the panel's latency depend on the disk.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]core.Model, len(c.models))
	copy(out, c.models)
	return out, nil
}

// RefreshModels re-reads the model catalogue from the desktop client's own
// config.yaml.
//
// This is NOT a network refresh: MiniMax publishes no model-list endpoint for
// this product (the obvious one answers HTTP 503 with
// errorReason "direct_route_not_configured").  What can change without a restart
// is the local whitelist, so that is what this re-reads.
//
// An unreadable or malformed config.yaml is NOT an error: catalogueRows
// degrades to the compiled-in table, and that table is what the caller gets.
// The error return is for the case where nothing at all survives -- an empty
// file plus a models filter that matches none of it -- because reporting
// success while handing back no models would silently blank the picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	models := coreModels(c.cfg.catalogueRows(c.logf))
	if len(models) == 0 {
		last, _ := c.Models(ctx)
		return last, errors.New("minimaxcode: no model survived the local catalogue and the configured models filter; keeping whatever was already loaded")
	}
	c.setModels(models)
	return models, nil
}

func (c *Client) modelIDs() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.models))
	for _, m := range c.models {
		out = append(out, m.ID)
	}
	return out
}

// Status is rendered by the panel every few seconds, so it must stay cheap and
// must never make a network call.
func (c *Client) Status(ctx context.Context) core.Status {
	ready, detail := c.pool.summary()
	return core.Status{
		Name:      name,
		Ready:     ready,
		Detail:    detail,
		Accounts:  c.pool.accountsForStatus(),
		Models:    c.modelIDs(),
		UpdatedAt: time.Now(),
	}
}

// Chat translates the request, picks a credential and returns the upstream
// answer as a core.Stream.  It returns core.ErrNotConfigured when there is no
// usable credential and core.ErrUnsupported when the request cannot be
// expressed on the Anthropic wire.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: empty request", core.ErrUnsupported)
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}

	if c.pool.usableCount() == 0 {
		_, detail := c.pool.summary()
		return nil, fmt.Errorf("%w: %s", core.ErrNotConfigured, detail)
	}

	attempts := c.cfg.maxAccountAttempts()
	base := core.RotateBackoffBase
	exclude := make(map[string]bool, attempts)
	// The stickiness key for this request.  "" means the request is not
	// conversation-scoped, and the affinity table is then not touched at all.
	key := conversationKey(req)
	var lastAcctID string
	var lastErr error
	// busy records that every account we reached was at its per-account
	// in-flight ceiling.
	busy := false

	for i := 0; i < attempts; i++ {
		// pickAccount resolves the conversation's binding first and only falls
		// back to the normal rotation.  It hands back a SNAPSHOT: the pool's
		// next() returns a live pointer, and this loop reads credential fields
		// through ensureFresh, so holding the pointer would be a data race
		// against a concurrent refresh.
		acct, ok := c.pickAccount(exclude, key, model)
		if !ok {
			break
		}
		// Take the account's in-flight slot before any upstream call.  A full
		// account is skipped, not blamed, so the next iteration uses another.
		if err := req.AcquireAccountSlot(acct.ID); err != nil {
			exclude[acct.ID] = true
			busy = true
			continue
		}
		// Name the credential for the gateway's usage ledger and console row.
		// A retry overwrites this, so the value read once Chat returns is the
		// account of the attempt that actually got through.
		core.NoteServedBy(req, acct.ID)
		// Pace the rotation.  i is the index of the attempt about to run, so
		// the first wait is BackoffFrom(base, 0) -- the same first wait the
		// gateway itself uses.  The account is selected before the wait, so an
		// exhausted pool never sleeps at all.
		if i > 0 && !core.SleepCtx(ctx, core.BackoffFrom(base, i-1)) {
			return nil, classifyTerminal(lastErr, lastAcctID)
		}
		lastAcctID = acct.ID

		// An expired credential is renewed before it is used, never after it
		// has already been rejected: the vendor rotates the refresh token on
		// every exchange, so spending one on a request that was going to fail
		// anyway would be a real loss.  The credential comes back as a
		// snapshot, so this request never reads a field that a concurrent
		// request is rewriting.
		cred := c.ensureFresh(ctx, acct.ID)

		stream, err := c.attempt(ctx, &cred, model, req)
		if err == nil {
			c.pool.noteSuccess(acct.ID)
			return stream, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, classifyTerminal(err, acct.ID)
		}
		var ue *upstreamError
		if errors.As(err, &ue) {
			c.pool.noteFailure(acct.ID, ue.failureKind(), ue.short())
			if ue.Kind == kindModel {
				// A model rejection is a property of the request, not of the
				// credential: retrying another account cannot help.  It is
				// still classified so the gateway can attribute it, but as
				// FailureOther -- the one kind it never rotates.
				return nil, core.Fail(name, acct.ID, core.FailureOther, ue.Status,
					fmt.Errorf("%w: %s", core.ErrUnsupported, ue.short()))
			}
			// Risk control is judged from the egress IP, not the credential:
			// feed every block into the shared gate and stop the moment it
			// reports that the IP itself is being refused.  A nil Guard (unit
			// tests) is safe -- ReportWAF has a nil-receiver check.
			if ue.failureKind() == core.FailureWAF && c.deps.Guard.ReportWAF(acct.ID) {
				return nil, classifyTerminal(err, acct.ID)
			}
		} else {
			c.pool.noteFailure(acct.ID, core.FailureUpstream, truncate(err.Error(), 200))
		}
		if errors.Is(err, core.ErrNotConfigured) {
			return nil, err
		}
	}

	if lastErr == nil && busy {
		return nil, core.ErrBusy
	}
	if lastErr == nil {
		_, detail := c.pool.summary()
		lastErr = fmt.Errorf("%w: %s", core.ErrNotConfigured, detail)
	}
	return nil, classifyTerminal(lastErr, lastAcctID)
}

// classifyTerminal translates the error that ended a Chat call into the shared
// error contract, attributed to the account that produced it.  Only a
// *core.Failure is rotated, WAF-gated and attributed by the gateway, so a module
// that returns a plain error silently opts out of the shared machinery.
//
// It takes an account id rather than an *Account because Chat now holds
// snapshots: an empty id simply means "no account to blame".
func classifyTerminal(err error, acctID string) error {
	if err == nil {
		return nil
	}
	if f, ok := core.AsFailure(err); ok {
		if f.Account == "" {
			f.Account = acctID
		}
		return err
	}
	// Local capacity problems keep their own shape: the gateway maps them by
	// errors.Is, and rotating accounts cannot conjure a missing credential.
	if errors.Is(err, core.ErrNotConfigured) || errors.Is(err, core.ErrUnsupported) {
		return err
	}

	var ue *upstreamError
	if errors.As(err, &ue) {
		return core.Fail(name, acctID, ue.failureKind(), ue.Status, err)
	}
	// Transport failures, decode failures and the caller's own cancellation:
	// FailureUpstream is the retryable catch-all.
	return core.Fail(name, acctID, core.FailureUpstream, 0, err)
}

// ensureFresh renews an account's access token when it is about to lapse and
// returns the credential the caller should send.
//
// The work happens on a snapshot read under the pool lock, never on a pointer
// the caller happens to hold: two chats can share one account, and the pool's
// own mutex is the only thing that makes those reads and writes agree.
//
// The renewal is serialised per account, because the vendor retires a refresh
// token the moment it is exchanged.  Two concurrent renewals would leave the
// loser's already-retired token in the store and break the desktop client too.
//
// Every failure here is logged and swallowed: an unrenewable credential is
// still worth trying, because the vendor is the only authority on whether it
// works, and refusing to send it would turn a stale expiry field into an outage.
func (c *Client) ensureFresh(ctx context.Context, id string) Account {
	acct, ok := c.pool.current(id)
	if !ok || !c.cfg.refreshEnabled() || !renewable(acct) {
		return acct
	}

	lock := c.pool.refreshLock(id)
	lock.Lock()
	defer lock.Unlock()

	// Somebody may have renewed this account while we waited for the lock.  If
	// so, hand back what they produced rather than spending the token again.
	if cur, ok := c.pool.current(id); ok {
		acct = cur
	}
	if !renewable(acct) {
		return acct
	}

	// The desktop client shares this store, and MiniMax retires a refresh token
	// the moment it is exchanged.  Re-read the file before spending our copy:
	// if the client signed in again, or rotated the token after this process
	// last looked, adopting its record is strictly better than racing it.
	// A rotation race is how the loser ends up writing a retired token over
	// the live one and signing the operator out of MiniMax Code entirely.
	if cred, ok := c.readStoreCredential(acct); ok && cred.Access != acct.Token {
		c.logf("minimaxcode: adopting the credential the desktop client wrote for %s", acct.ID)
		if c.pool.adoptCredential(acct.ID, cred) {
			acct.Token = cred.Access
			acct.RefreshToken = cred.Refresh
			acct.ExpiresAt = cred.ExpiresAt
			acct.Generation = cred.Generation
			acct.RecordKey = cred.RecordKey
			return acct
		}
	}
	if storeCredentialGone(acct) {
		// The client signed out, which empties the store.  There is nothing
		// left to exchange, so park the row with the real reason instead of
		// spending a request on a credential the vendor already killed.
		c.logf("minimaxcode: the MiniMax Code client signed out (%s has no records); %s needs a fresh sign-in", acct.AuthPath, acct.ID)
		c.pool.markNeedsLogin(acct.ID)
		return acct
	}

	clientIDs := c.refreshClientIDs(acct)
	access, refresh, expiresAt, err := refreshCredential(ctx, c.http, c.cfg.oauthTokenURL(), clientIDs, acct.RefreshToken)
	if err != nil {
		c.logf("minimaxcode: cannot refresh %s: %v", acct.ID, err)
		if refreshRejected(err) {
			// invalid_grant means the vendor retired the authorization, not
			// that this attempt was unlucky.  Record the remedy in the row so
			// the panel says 需要重新登录 instead of a later HTTP 401.
			c.pool.markNeedsLogin(acct.ID)
		}
		return acct
	}
	// A discovered row writes the rotated pair back into the desktop client's
	// store.  A row the panel signed in has no such file; its durable home is
	// managed_accounts.json, rewritten below.
	if acct.AuthPath != "" {
		if werr := writeBackCredential(acct.AuthPath, acct.RecordKey, access, refresh, expiresAt, acct.Generation+1); werr != nil {
			// The token is still usable in memory for this process; only the
			// desktop client misses out.  Say so rather than pretending it worked.
			c.logf("minimaxcode: refreshed %s but could not write it back: %v", acct.ID, werr)
		}
	}
	c.pool.mark(acct.ID, func(a *Account) {
		a.Token = access
		if strings.TrimSpace(refresh) != "" {
			a.RefreshToken = refresh
		}
		if !expiresAt.IsZero() {
			a.ExpiresAt = expiresAt
		}
		a.Generation++
		// A successful exchange proves the credential works again; a penalty
		// left over from the lapsed token must not keep the row unselectable.
		a.State = stateReady
		a.CooldownUntil = time.Time{}
		a.Failures = 0
		a.LastError = ""
	})
	if acct.Managed {
		// MiniMax retires the old refresh token the moment it is exchanged, so
		// a rotated pair that is not on disk before the process ends is a
		// credential the vendor has already spent.
		if err := c.pool.persistManaged(); err != nil {
			c.logf("minimaxcode: refreshed %s but could not store the rotated token: %v", acct.ID, err)
		}
	}
	c.logf("minimaxcode: refreshed the access token for %s", acct.ID)

	// Return the credential as it now stands rather than re-reading the pool:
	// the caller only needs something safe to send.
	acct.Token = access
	if strings.TrimSpace(refresh) != "" {
		acct.RefreshToken = refresh
	}
	if !expiresAt.IsZero() {
		acct.ExpiresAt = expiresAt
	}
	acct.Generation++
	return acct
}

// renewable reports whether a snapshot has a lapsed token and somewhere to
// renew it from.  A credential that recorded no expiry counts as lapsed: the
// store's own field is advisory, and trying is cheaper than failing.
func renewable(a Account) bool {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return false
	}
	// The rotated pair has to land somewhere durable: a credential discovered
	// in the desktop client's store writes back into that store, while a row
	// the panel signed in itself owns its refresh token in this module's
	// managed_accounts.json.  A bare typed token has neither home, so it cannot
	// be renewed and the panel's re-login is the only honest remedy.
	if strings.TrimSpace(a.AuthPath) == "" && !a.Managed {
		return false
	}
	return a.ExpiresAt.IsZero() || !a.ExpiresAt.After(time.Now().Add(refreshLeeway))
}

// refreshClientIDs lists the OAuth client ids to try, in order.  The store's own
// id comes first because it is the one the credential was issued to; the CLI's
// id is the documented alternative and is only tried after that fails.
func (c *Client) refreshClientIDs(acct Account) []string {
	var out []string
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	if parts := strings.Split(acct.ID, "/"); len(parts) > 0 {
		add(parts[len(parts)-1])
	}
	// A row the panel signed in records the exact client the credential was
	// minted for, so that one is tried before the guesses below.  Presenting
	// the wrong client id to the token endpoint fails, and MiniMax counts the
	// attempt either way.
	add(acct.ClientID)
	add(c.cfg.OAuthClientID)
	add(oauthClientIDCLI)
	add(oauthClientIDDesktop)
	return out
}

// attempt performs exactly one upstream request for one account.
func (c *Client) attempt(ctx context.Context, acct *Account, model string, req *core.ChatRequest) (core.Stream, error) {
	body, err := buildRequestBody(req, c.cfg, acct, model)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: build request: %w", err)
	}
	payload, err := marshalNoEscape(body)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: encode request: %w", err)
	}

	reqCtx := ctx
	var cancel context.CancelFunc
	if d := c.cfg.timeout(); d > 0 {
		reqCtx, cancel = context.WithTimeout(ctx, d)
	}
	handedOff := false
	defer func() {
		if !handedOff && cancel != nil {
			cancel()
		}
	}()

	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.upstreamURL(acct), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: build http request: %w", err)
	}
	c.applyHeaders(httpReq.Header, acct)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: upstream request failed: %w", err)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))

	if resp.StatusCode >= http.StatusBadRequest {
		raw := readLimited(resp.Body, 1<<20)
		_ = resp.Body.Close()
		ue := classify(resp.StatusCode, raw)
		ue.Msg = redactSecret(ue.Msg, acct.secret())
		return nil, ue
	}

	if strings.Contains(contentType, "text/event-stream") {
		handedOff = true
		return newAnthropicStream(resp.Body, cancel), nil
	}

	raw := readLimited(resp.Body, 8<<20)
	_ = resp.Body.Close()

	if ue := classifyEnvelope(resp.StatusCode, raw); ue != nil {
		ue.Msg = redactSecret(ue.Msg, acct.secret())
		return nil, ue
	}

	events, err := responseEvents(raw, model)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: decode upstream response: %w", err)
	}
	handedOff = true
	return newSliceStream(events, nil, cancel), nil
}

// baseURL resolves the endpoint prefix for an account.  The config-level
// override wins, then the account's own, then the vendor default.  The
// /minimax-cloud routes are addressed relative to this same prefix, so a base
// URL pointing at a proxy routes both namespaces to the same place.
func (c *Client) baseURL(acct *Account) string {
	base := strings.TrimSpace(c.cfg.BaseURL)
	if base == "" && acct != nil {
		base = strings.TrimSpace(acct.BaseURL)
	}
	if base == "" {
		base = defaultBaseURL
	}
	return base
}

// upstreamURL resolves the chat endpoint for an account.
func (c *Client) upstreamURL(acct *Account) string {
	return messagesURL(c.baseURL(acct))
}

// applyHeaders installs exactly the headers the captured request carried, plus
// a User-Agent.  Adding anything the capture did not show would be a guess, and
// a wrong guess is a fingerprint.
func (c *Client) applyHeaders(h http.Header, acct *Account) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("anthropic-version", anthropicVersion)
	h.Set("User-Agent", defaultUserAgent)
	if acct != nil {
		if tok := strings.TrimSpace(acct.Token); tok != "" {
			h.Set("Authorization", "Bearer "+tok)
		}
	}
}

// redactSecret removes a credential from a vendor message before it is logged,
// attributed, or handed to the panel.  The exact token is stripped first,
// because core.Redact only knows the shapes it was taught and a "mmoat_…" string
// is not one of them.
func redactSecret(msg, secret string) string {
	if s := strings.TrimSpace(secret); s != "" {
		msg = strings.ReplaceAll(msg, s, "***")
	}
	return core.Redact(msg)
}
