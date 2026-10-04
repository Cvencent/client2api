// Package cline implements the "cline" client: an OpenAI-shaped façade over
// Cline (cline.bot), the VS Code / CLI coding agent backed by api.cline.bot.
//
// The chat endpoint is already OpenAI-shaped, so this module's job is not
// translation: it owns the credential (a WorkOS device-authorisation flow and
// the `workos:`-prefixed bearer token that flow produces), the three-source
// model catalogue, the account pool and its cooldowns, and the reasoning-effort
// switch that decides whether a model thinks at all.
//
// PROVENANCE: clean-room, from a read of the official client's traffic.  No
// third-party implementation was copied.  See README.md for the measured
// protocol facts and the account-ban risk this carries.
package cline

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register("cline", New) }

// Client implements core.Client for cline.
type Client struct {
	deps core.Deps
	cfg  config

	// Paths inside Deps.DataDir; empty when no DataDir was supplied, in which
	// case nothing is persisted anywhere.
	accountsPath string
	statePath    string
	loginPath    string

	pool   *pool
	models *modelCache

	// affinity pins a conversation to the account that first served it, so a
	// multi-turn conversation keeps landing on one credential instead of being
	// walked by the LRU picker.  It is nil-safe: a Client built by hand in a
	// test may leave it nil.
	affinity *core.Affinity

	httpOnce  sync.Once
	ownClient *http.Client

	// inflight counts requests currently holding a stream, so the module can
	// answer core.ErrBusy instead of opening an unbounded number of upstream
	// connections.
	inflightMu sync.Mutex
	inflight   int

	stateMu    sync.Mutex
	lastErr    string
	lastErrAt  time.Time
	upstreamOK bool

	loginMu     sync.Mutex
	panelLogins map[string]*panelLogin
}

// New builds the client.  It never fails for a missing credential: that is a
// runtime condition reported by Status(), not a construction failure.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("cline: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("cline: could not create the data directory: %v", err)
		}
	}
	c := &Client{
		deps:     deps,
		cfg:      cfg,
		models:   &modelCache{},
		affinity: core.NewAffinity(0),
	}
	c.setPaths()
	c.pool = newPool(c.accountsPath, c.statePath, defaultStoreFlush, cooldownConfig{
		def:   cfg.cooldown(),
		short: cfg.shortCooldown(),
		quota: cfg.quotaCooldown(),
	}, deps.Logf)
	c.pool.load(configuredAccounts(cfg))
	if c.affinity != nil {
		c.affinity.StartGC()
	}
	c.maybeStartLogin()
	return c, nil
}

// Name implements core.Client.
func (c *Client) Name() string { return "cline" }

// setPaths joins the module's data files onto Deps.DataDir, leaving them empty
// when there is no DataDir (in which case nothing is persisted).
func (c *Client) setPaths() {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return
	}
	c.accountsPath = filepath.Join(dir, accountsFile)
	c.statePath = filepath.Join(dir, stateFile)
	c.loginPath = filepath.Join(dir, loginFile)
}

// httpClient returns the client every request goes through: the host's when one
// was supplied, otherwise this module's own.
func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	c.httpOnce.Do(func() {
		if fp := c.cfg.tlsProfile(); fp != fingerprint.ProfileNone {
			if fhc, ferr := fingerprint.New(fingerprint.Options{
				Profile:  fp,
				Protocol: c.cfg.tlsProtocol(),
				Proxy:    c.deps.Proxy,
				Logf:     c.deps.Logf,
			}); ferr == nil {
				c.ownClient = fhc
				return
			} else {
				c.deps.Log("cline: tls fingerprint %q ignored: %v", string(fp), ferr)
			}
		}
		c.ownClient = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	})
	return c.ownClient
}

// --- chat ------------------------------------------------------------------

// Chat opens one streamed completion.  The model arrives with the "cline/"
// prefix already stripped by the gateway.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, core.ErrUnsupported
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, core.ErrUnsupported
	}
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}
	if !c.enter() {
		return nil, core.ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	stream, err := c.openChat(ctx, cancel, req, core.ConversationKeyOf(req))
	if err != nil {
		cancel()
		c.leave()
		return nil, err
	}
	return &trackedStream{Stream: stream, leave: c.leave}, nil
}

// enter claims an in-flight slot, returning false at the ceiling.
func (c *Client) enter() bool {
	c.inflightMu.Lock()
	defer c.inflightMu.Unlock()
	if c.inflight >= c.cfg.maxConcurrency() {
		return false
	}
	c.inflight++
	return true
}

// leave releases an in-flight slot.
func (c *Client) leave() {
	c.inflightMu.Lock()
	if c.inflight > 0 {
		c.inflight--
	}
	c.inflightMu.Unlock()
}

// trackedStream releases the in-flight slot when the stream is closed.
type trackedStream struct {
	core.Stream
	once  sync.Once
	leave func()
}

// Close releases the slot exactly once.
func (t *trackedStream) Close() error {
	err := t.Stream.Close()
	t.once.Do(t.leave)
	return err
}

// openChat runs the account retry loop.  It is the single point every request
// goes through, which is what makes the cooldowns and the stickiness real.
func (c *Client) openChat(ctx context.Context, release context.CancelFunc, req *core.ChatRequest, convKey string) (core.Stream, error) {
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}
	body, err := buildBody(req, c.cfg.maxTokens(), c.cfg.reasoningEffort())
	if err != nil {
		return nil, err
	}

	// Best-effort pre-refresh: an account close to expiry -- or one that has
	// already lapsed -- is renewed before the pool is asked to pick from it.
	//
	// The candidate list must NOT be the usable set.  An expired account is
	// unavailable by definition, so it can never appear in usable(), and the
	// refresh that would make it usable again would therefore never run: the
	// pool would stay empty and every request would 503 until an operator
	// re-ran Cline's own CLI.
	for _, e := range c.pool.refreshCandidates(time.Now(), c.cfg.refreshMargin()) {
		_ = c.tryRefresh(ctx, e)
	}

	skip := map[string]bool{}
	refreshed := map[string]bool{}
	attempts := c.cfg.maxAttempts()
	var lastErr error
	var lastAcct account
	var issued int
	// busy records that the pool itself is fine but every account we reached
	// was at its per-account in-flight ceiling.
	busy := false

	for i := 0; i < attempts; i++ {
		e := c.pickAccount(skip, convKey)
		if e == nil {
			break
		}
		// A full account is skipped, not blamed: the next iteration picks
		// another one before any upstream call is made.
		if err := req.AcquireAccountSlot(e.acct.id()); err != nil {
			skip[e.acct.id()] = true
			busy = true
			continue
		}
		// Name the credential for the gateway's usage ledger.
		core.NoteServedBy(req, e.acct.id())
		if issued > 0 && !c.pace(ctx, issued-1) {
			return nil, classifyTerminal(lastErr, lastAcct)
		}
		issued++

		resp, err := c.chatStream(ctx, e.acct, body)
		if err == nil {
			c.pool.markUsed(e)
			c.noteUpstream(true, "")
			return newClineStream(ctx, release, resp.Body), nil
		}

		ue, ok := asUpstreamError(err)
		if !ok {
			// A context error or a local failure: do not blame the account.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		c.noteUpstream(false, ue.Message)

		if ue.Kind == kindAuth && e.acct.refreshable() && !refreshed[e.acct.id()] {
			refreshed[e.acct.id()] = true
			if refreshErr := c.tryRefresh(ctx, e); refreshErr == nil {
				i-- // the retry does not consume an attempt slot
				continue
			} else {
				msg := cleanErrorText(refreshErr.Error())
				c.noteUpstream(false, msg)
				c.pool.markDead(e, msg)
				skip[e.acct.id()] = true
				lastErr, lastAcct = refreshErr, e.acct
				continue
			}
		}

		c.pool.markFailureWith(e, ue.Kind, ue.Message, ue.RetryAfter)
		skip[e.acct.id()] = true
		lastErr, lastAcct = ue, e.acct
		if !retryable(ue.Kind) {
			break
		}
	}

	if lastErr != nil {
		return nil, classifyTerminal(lastErr, lastAcct)
	}
	if busy {
		return nil, core.ErrBusy
	}
	return nil, core.ErrNotConfigured
}

// chatStream performs one upstream attempt.  A non-200 response becomes a
// classified error; a vendor refusal is a normal result, not a Go error.
func (c *Client) chatStream(ctx context.Context, acct account, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.endpoint(chatPath), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyClientHeaders(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		// A transport failure has no HTTP status, so it is classified as a
		// network error rather than blamed on the credential.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, &upstreamError{Kind: kindNetwork, Message: cleanErrorText(err.Error())}
	}
	if resp.StatusCode != http.StatusOK {
		raw := readLimited(resp.Body, maxAuthBodyBytes)
		_ = resp.Body.Close()
		return nil, newUpstreamError(resp.StatusCode, errorMessage(string(raw)), resp.Header)
	}
	return resp, nil
}

// pickAccount selects the account for one attempt: a conversation's bound
// account when it is still usable, otherwise the pool's LRU pick, which is then
// bound.  An empty key degenerates into the plain LRU pick.
func (c *Client) pickAccount(skip map[string]bool, convKey string) *entry {
	if convKey != "" && c.affinity != nil {
		if id, ok := c.affinity.Resolve(convKey, c.usableFor); ok {
			if e := c.pool.usableEntry(id, skip); e != nil {
				return e
			}
		}
	}
	e := c.pool.pick(skip)
	if e == nil {
		return nil
	}
	if convKey != "" && c.affinity != nil {
		c.affinity.Bind(convKey, e.acct.id())
	}
	return e
}

// usableFor reports whether an account id may serve a request now.
func (c *Client) usableFor(id string) bool {
	e := c.pool.get(id)
	if e == nil {
		return false
	}
	return c.pool.availableNow(e)
}

// pace waits out the rotation backoff before another upstream attempt.
func (c *Client) pace(ctx context.Context, failed int) bool {
	return core.SleepCtx(ctx, core.BackoffFrom(core.RotateBackoffBase, failed))
}

// tryRefresh renews one account's token in place.  A nil error means the
// account is usable again.
func (c *Client) tryRefresh(ctx context.Context, e *entry) error {
	if e == nil || !e.acct.refreshable() {
		return errors.New("cline: no refresh token")
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	grant, err := c.refresh(ctx, e.acct.RefreshToken)
	if err != nil {
		c.deps.Log("cline: refresh for %s failed: %v", e.acct.id(), scrubError(err))
		return err
	}
	next := accountFromGrant(e.acct, grant)
	ne := c.pool.put(next)
	c.pool.revive(ne)
	c.deps.Log("cline: refreshed token for %s", next.id())
	return nil
}

// classifyTerminal wraps the retry loop's final error in the shared
// core.Failure contract, attributed to the account that produced it.
func classifyTerminal(err error, acct account) error {
	if err == nil {
		return core.ErrNotConfigured
	}
	kind := kindClient
	status := 0
	if ue, ok := asUpstreamError(err); ok {
		kind = ue.Kind
		status = ue.Status
	}
	return core.Fail("cline", acct.id(), failureKind(kind), status, err)
}

// noteUpstream records the last upstream verdict so Status can distinguish
// "upstream unreachable" from "no credential" without making a network call.
func (c *Client) noteUpstream(ok bool, msg string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.upstreamOK = ok
	c.lastErrAt = time.Now()
	if ok {
		c.lastErr = ""
		return
	}
	c.lastErr = cleanErrorText(msg)
}

// --- login -----------------------------------------------------------------

// maybeStartLogin runs the device flow in the background when the config asks
// for it and nothing is usable yet.  The URL goes to stdout: there is no login
// subcommand in cmd/, so an operator watching the log is the intended UX, and
// the same flow is reachable programmatically through RunDeviceFlow.
func (c *Client) maybeStartLogin() {
	if !c.cfg.loginRequested() || c.pool.ready() {
		return
	}
	c.deps.Log("cline: starting device authorisation flow")
	// GoSafe, not a bare "go": the flow waits on a browser round-trip, so it
	// outlives whatever triggered it, and a panic here would take the gateway
	// down instead of just failing one login attempt.
	core.GoSafe("cline device flow", func(msg string) { c.deps.Log("cline: %s", msg) }, func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
		defer cancel()
		if err := RunDeviceFlow(ctx, c.deps, os.Stdout); err != nil {
			c.deps.Log("cline: device flow: %v", scrubError(err))
			return
		}
		c.reloadAccounts()
	})
}

// reloadAccounts re-reads the credential store and the health file, then merges
// the config over them.
func (c *Client) reloadAccounts() {
	c.pool.reload(configuredAccounts(c.cfg))
}

// scrubError removes anything credential-shaped from an error string.
func scrubError(err error) string {
	if err == nil {
		return ""
	}
	return cleanErrorText(err.Error())
}

// --- status ----------------------------------------------------------------

// Status is deliberately cheap: no network calls, one small file read and a
// mutex-guarded snapshot.  The panel polls it, so it must never block.
func (c *Client) Status(ctx context.Context) core.Status {
	models := c.models.snapshot()
	if len(models) == 0 {
		models = fallbackCatalog()
	}
	st := core.Status{
		Name:      "cline",
		Accounts:  c.pool.snapshot(),
		Models:    sortedIDs(models),
		UpdatedAt: time.Now(),
	}

	c.stateMu.Lock()
	lastErr := c.lastErr
	lastErrAt := c.lastErrAt
	upstreamOK := c.upstreamOK
	c.stateMu.Unlock()

	ready := c.pool.ready()
	switch {
	case c.pool.len() == 0:
		st.Ready = false
		if rec := c.loginState(); rec != nil {
			st.Detail = "device flow not completed: open " + rec.URL
			break
		}
		st.Detail = "no credential: set clients.cline.access_token, " + envAccessToken +
			", or run the device flow (\"login\": true)"
	case ready:
		st.Ready = true
		st.Detail = c.pool.summary() + "; models: " + strings.Join(st.Models, ", ")
	default:
		st.Ready = false
		st.Detail = "all accounts unavailable (" + c.pool.summary() + ")"
		if lastErr != "" {
			st.Detail += "; last error: " + lastErr
		}
	}
	if !st.Ready && lastErr != "" && !ready && c.pool.len() > 0 {
		if !upstreamOK {
			st.Detail = "upstream unreachable: " + lastErr
		}
		if !lastErrAt.IsZero() {
			st.Detail += " (last attempt " + humanAge(time.Since(lastErrAt)) + " ago)"
		}
	}
	return core.RedactStatus(st)
}

// --- small helpers ---------------------------------------------------------

// humanAge is in pool.go.
