// Package qwenwork implements the "qwenwork" client: an OpenAI-shaped façade
// over QwenWork / QoderWork, the CN desktop agent backend at
// gateway.qwenwork.cn.
//
// The upstream protocol is not OpenAI: the request is a signed "agent envelope"
// POSTed to an SSE endpoint, authenticated with a COSY signature over a
// per-session RSA-wrapped AES key (see cosy.go).  This module translates in
// both directions and owns the account pool, its cooldowns and its device
// authorisation flow.
//
// PROVENANCE: this is a clean-room implementation written from
// docs/upstream/qwenwork.md.  The reference implementation
// (qwenwork2api-makers) ships no licence, so none of its code, comments,
// identifiers or file layout were copied; it was read only to learn the wire
// protocol, and the cryptography here is Go's crypto/* rather than a
// transliteration of the reference's JavaScript.  See README.md for the
// provenance statement and the account-ban risk this carries.
package qwenwork

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register("qwenwork", New) }

const (
	// maxSessions bounds the COSY session cache.  A session costs one RSA
	// encrypt to build, so the cache is an optimisation only and may be
	// dropped wholesale when it grows.
	maxSessions = 128

	// modelsRetryInterval throttles background catalogue refreshes so a
	// failing upstream cannot be hammered by every /v1/models call.
	modelsRetryInterval = 30 * time.Second
)

// sessionEntry caches one COSY session alongside the access token it was built
// from, so a rotated token invalidates the entry implicitly.
type sessionEntry struct {
	sess  cosySession
	token string
}

// Client implements core.Client for qwenwork.
type Client struct {
	deps core.Deps
	cfg  config

	// Paths inside Deps.DataDir; empty when no DataDir was supplied, in which
	// case nothing is persisted anywhere.
	accountsPath string
	statePath    string
	loginPath    string

	pool *pool

	// affinity pins a conversation to the account that first served it, so a
	// multi-turn conversation keeps landing on one credential instead of being
	// walked by the LRU picker.  See affinity.go; it is nil-safe, so a Client
	// built by hand in a test may leave it nil.
	affinity *core.Affinity

	// paceBaseOverride replaces core.RotateBackoffBase as the base of the wait
	// between two attempts in the account retry loop.  No config knob exists
	// for it -- nothing in production should need one -- so it is unexported
	// and only ever set by a test, which is how the loop is exercised without
	// a 500 ms nap per retry.
	paceBaseOverride time.Duration

	sessMu   sync.Mutex
	sessions map[string]sessionEntry

	httpOnce  sync.Once
	ownClient *http.Client

	// vendorClient is non-nil only when a TLS profile was configured.  It
	// carries the imitating handshake and takes precedence over every other
	// client, so the fingerprint cannot be silently bypassed.
	vendorClient *http.Client

	modelsMu        sync.Mutex
	models          []core.Model
	modelsAt        time.Time
	modelsAttemptAt time.Time
	modelsLoading   bool

	stateMu     sync.Mutex
	upstreamOK  bool
	lastErrKind errKind
	lastErr     string
	lastErrAt   time.Time

	// inFlight is the number of chat streams currently open.  openChat feeds
	// it through core.TrackStream, so PoolStats reports real concurrency.
	inFlight atomic.Int64

	// Panel-driven device authorisations (see accounts.go).  These live in
	// memory only: a restart drops a half-finished flow, and the operator
	// simply starts another one.  The URL is also mirrored to login.json,
	// which is what Status reads.
	loginMu sync.Mutex
	panels  map[string]*panelLogin
}

// New builds the client.  It never fails for a missing credential: that is a
// runtime condition reported by Status(), not a construction failure.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("qwenwork: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	if deps.DataDir != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("qwenwork: creating data dir: %v", err)
		}
	}

	c := &Client{
		deps:     deps,
		cfg:      cfg,
		sessions: make(map[string]sessionEntry, 4),
		panels:   map[string]*panelLogin{},
	}
	c.setPaths()
	c.pool = newPool(c.accountsPath, c.statePath, defaultStoreFlush, cooldownConfig{
		def:   cfg.cooldown(),
		short: cfg.shortCooldown(),
		quota: cfg.quotaCooldown(),
	}, deps.Log)
	c.pool.load(configuredAccounts(cfg))

	// Conversation stickiness is opt-in and additive: the table starts empty
	// and is only ever consulted by pickAccount, so a client that never sees a
	// conversation_id behaves exactly as it did before.  The GC goroutine is
	// what keeps a day of traffic from growing the table without bound.
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()

	c.maybeStartLogin()

	// A named profile replaces every vendor-facing client with one whose TLS
	// handshake imitates that browser.  The Go hello is one of the few things
	// about this proxy a vendor can spot without reading a single header, so a
	// module that cares names a profile here.  The default (empty) changes
	// nothing, leaving the shared client in place.
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, err := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if err != nil {
			deps.Log("qwenwork: tls fingerprint %q ignored: %v", string(fp), err)
		} else {
			c.vendorClient = fhc
			deps.Log("qwenwork: tls handshake imitates the %s client", string(fp))
		}
	}
	return c, nil
}

// Name is both the registry key and the routing prefix ("qwenwork/pro").
func (c *Client) Name() string { return "qwenwork" }

// setPaths resolves the module's files inside Deps.DataDir.  With no DataDir it
// leaves them empty, so the module can never write outside its own directory.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
	c.loginPath = filepath.Join(c.deps.DataDir, loginFile)
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

// Models returns the catalogue.  It is deliberately non-blocking: the vendor's
// list is refreshed in the background, so this answers from the cache when it
// is fresh and from the built-in catalogue otherwise.  Registry.Resolve calls
// Models on every module for a bare model name, so it must stay cheap.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	cached, at := c.cachedModels()
	if len(cached) > 0 {
		if time.Since(at) < c.cfg.modelsTTL() {
			return cached, nil
		}
		c.refreshModelsAsync()
		return cached, nil
	}
	c.refreshModelsAsync()
	return fallbackModels(), nil
}

// RefreshModels implements core.ModelRefresher: it asks the vendor for the
// catalogue right now, bypassing the modelsTTL cache.  Unlike Models it is
// synchronous, because the panel's "re-fetch from upstream" button is an
// explicit request that has to report success or failure.
//
// A failed refresh must never empty the catalogue.  Every error path returns
// the last good list alongside the error -- or the built-in catalogue when
// nothing has ever been fetched -- so one flaky refresh cannot blank the model
// picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	previous, _ := c.cachedModels()
	fallback := previous
	if len(fallback) == 0 {
		fallback = fallbackModels()
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()

	models, err := c.fetchModels(rctx)
	if err != nil {
		c.deps.Log("qwenwork: model list refresh: %v", err)
		return fallback, fmt.Errorf("qwenwork: refresh models: %s", c.scrubModelError(err))
	}
	if len(models) == 0 {
		return fallback, errors.New("qwenwork: refresh models: upstream returned an empty catalogue")
	}
	c.modelsMu.Lock()
	c.models = models
	c.modelsAt = time.Now()
	// This call just did the work, so do not let the background refresher
	// immediately repeat it.
	c.modelsAttemptAt = time.Now()
	c.modelsMu.Unlock()
	return models, nil
}

// scrubModelError renders a model-list failure without ever echoing an
// account's credentials.  cleanErrorText is the module's existing scrubber;
// the tokens are additionally replaced verbatim, because a bare token carries
// no label for its patterns to key on.
func (c *Client) scrubModelError(err error) string {
	if err == nil {
		return ""
	}
	msg := cleanErrorText(err.Error())
	for _, e := range c.pool.all() {
		acct := c.pool.accountOf(e)
		for _, s := range []string{acct.AccessToken, acct.RefreshToken} {
			if s != "" {
				msg = strings.ReplaceAll(msg, s, core.MaskSecret(s))
			}
		}
	}
	return msg
}

// refreshModelsAsync starts at most one background refresh, and not more often
// than modelsRetryInterval.
func (c *Client) refreshModelsAsync() {
	c.modelsMu.Lock()
	if c.modelsLoading || time.Since(c.modelsAttemptAt) < modelsRetryInterval {
		c.modelsMu.Unlock()
		return
	}
	c.modelsLoading = true
	c.modelsAttemptAt = time.Now()
	c.modelsMu.Unlock()

	core.GoSafe("qwenwork model refresh", func(msg string) { c.deps.Log("qwenwork: %s", msg) }, func() {
		// This defer still runs on the panic path (GoSafe recovers after fn's
		// own defers), so a panicking refresh cannot leave modelsLoading stuck
		// true and block every later refresh for the life of the process.
		defer func() {
			c.modelsMu.Lock()
			c.modelsLoading = false
			c.modelsMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()
		models, err := c.fetchModels(ctx)
		if err != nil {
			c.deps.Log("qwenwork: model list: %v", err)
			return
		}
		if len(models) == 0 {
			return
		}
		c.modelsMu.Lock()
		c.models = models
		c.modelsAt = time.Now()
		c.modelsMu.Unlock()
	})
}

// fetchModels asks the vendor for the live catalogue, trying accounts in the
// pool's least-recently-used order.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	if !c.pool.ready() {
		return nil, errors.New("no usable account")
	}
	skip := map[string]bool{}
	var lastErr error
	for i := 0; i < c.cfg.maxAttempts(); i++ {
		e := c.pool.pick(skip)
		if e == nil {
			break
		}
		sess, err := c.sessionFor(e.acct)
		if err != nil {
			skip[e.acct.id()] = true
			lastErr = err
			continue
		}
		models, err := c.modelList(ctx, e.acct, sess)
		if err == nil {
			return models, nil
		}
		lastErr = err
		skip[e.acct.id()] = true
		if ue, ok := asUpstreamError(err); ok && !retryable(ue.Kind) {
			break
		}
	}
	if lastErr == nil {
		lastErr = core.ErrNotConfigured
	}
	return nil, lastErr
}

func (c *Client) cachedModels() ([]core.Model, time.Time) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	return c.models, c.modelsAt
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider.  When the caller
// omits max_tokens the gateway asks what the vendor advertises for this model,
// which is the fix for a model advertising more than the module's own default
// being silently truncated.
//
// It answers from the cache only.  The vendor list carries max_output_tokens
// per model (upstream.go projects it into Extra) and Models refreshes that
// cache in the background, so a cold cache means "cannot say" for this one
// request rather than a metadata round trip inside a chat turn.
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	models, _ := c.cachedModels()
	return core.OutputLimitFor(models, model)
}

// builtinModels is the catalogue the vendor's desktop app ships with.  It is
// what /v1/models answers before (or without) a live account, so a caller can
// always discover the model ids; entries refreshed from the vendor replace it.
var builtinModels = []struct{ ID, Name string }{
	{"pro", "QwenWork 高级 (Pro)"},
	{"flash", "QwenWork Qwen3.8-Flash"},
	{"qwen3.8-max-preview", "QwenWork Qwen3.8-Max"},
}

func fallbackModels() []core.Model {
	out := make([]core.Model, 0, len(builtinModels))
	for _, m := range builtinModels {
		out = append(out, core.Model{
			ID:      m.ID,
			OwnedBy: "qwenwork",
			Extra: map[string]any{
				"display_name":          m.Name,
				"context_length":        defaultContextLength,
				"max_completion_tokens": maxOutputTokens,
				"vision":                true,
				"reasoning":             false,
				"fallback":              true,
			},
		})
	}
	return out
}

// modelIDs is the bare list Status reports.
func (c *Client) modelIDs() []string {
	models, _ := c.cachedModels()
	if len(models) == 0 {
		models = fallbackModels()
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// chat
// ---------------------------------------------------------------------------

// Chat opens one streamed completion.  The model arrives with the
// "<client>/" prefix already stripped by the gateway.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, core.ErrUnsupported
	}
	if strings.TrimSpace(req.Model) == "" {
		// The reference would silently fall back to "pro"; an unqualified
		// request is a caller error, so say so instead of guessing.
		return nil, core.ErrUnsupported
	}
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	// The conversation key is derived here, at the one entry point every
	// request goes through, and handed to openChat so the account picker can
	// pin the conversation before it falls back to the LRU pick.  An empty key
	// (no conversation_id, no prompt_cache_key, no user) means "not
	// conversation-scoped" and turns the feature off for this request.
	stream, err := c.openChat(ctx, cancel, req, mapModel(req.Model), conversationKey(req))
	if err != nil {
		cancel()
		return nil, err
	}
	return stream, nil
}

// ---------------------------------------------------------------------------
// COSY sessions
// ---------------------------------------------------------------------------

// sessionFor returns the cached COSY session for an account, building one when
// it is missing or the access token has changed.
func (c *Client) sessionFor(acct account) (cosySession, error) {
	key := acct.id()

	c.sessMu.Lock()
	if e, ok := c.sessions[key]; ok && e.token == acct.AccessToken {
		c.sessMu.Unlock()
		return e.sess, nil
	}
	c.sessMu.Unlock()

	sess, err := newCosySession(acct.UID, acct.Nickname, acct.Email, acct.AccessToken)
	if err != nil {
		return cosySession{}, err
	}

	c.sessMu.Lock()
	if len(c.sessions) >= maxSessions {
		c.sessions = make(map[string]sessionEntry, maxSessions)
	}
	c.sessions[key] = sessionEntry{sess: sess, token: acct.AccessToken}
	c.sessMu.Unlock()
	return sess, nil
}

// invalidateSession forgets one account's session, so the next request rebuilds
// it against a freshly minted token.
func (c *Client) invalidateSession(acct account) {
	c.sessMu.Lock()
	delete(c.sessions, acct.id())
	c.sessMu.Unlock()
}

// ---------------------------------------------------------------------------
// credential discovery / login
// ---------------------------------------------------------------------------

// reloadAccounts re-reads the credential store and the health file, then merges
// the config over them.  It is called after a successful device authorisation so
// the new account joins the pool without a restart.
func (c *Client) reloadAccounts() {
	c.pool.reload(configuredAccounts(c.cfg))
}

// maybeStartLogin runs the device flow in the background when the config asks
// for it and nothing is usable yet.  The URL goes to stdout: there is no login
// subcommand in cmd/, so an operator watching the log is the intended UX, and
// the same flow is reachable programmatically through RunDeviceFlow.
func (c *Client) maybeStartLogin() {
	if !c.cfg.loginRequested() || c.pool.ready() {
		return
	}
	c.deps.Log("qwenwork: starting device authorisation flow")
	core.GoSafe("qwenwork device flow", func(msg string) { c.deps.Log("qwenwork: %s", msg) }, func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
		defer cancel()
		if err := RunDeviceFlow(ctx, c.deps, os.Stdout); err != nil {
			c.deps.Log("qwenwork: device flow: %v", err)
			return
		}
		c.reloadAccounts()
	})
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

// Status is deliberately cheap: no network calls, one small file read and a
// mutex-guarded snapshot.  The panel calls it every ten seconds.
func (c *Client) Status(ctx context.Context) core.Status {
	st := core.Status{
		Name:      "qwenwork",
		UpdatedAt: time.Now(),
		Models:    c.modelIDs(),
	}
	st.Accounts = c.pool.snapshot()

	kind, msg, at := c.lastUpstream()

	switch {
	case c.pool.len() == 0:
		st.Ready = false
		if rec := c.loginState(); rec != nil {
			st.Detail = "device flow not completed: open " + rec.URL
		} else {
			st.Detail = "no credential: set clients.qwenwork.access_token, " + envAccessToken +
				", or run the device flow (\"login\": true)"
		}
	case c.pool.ready():
		st.Ready = true
		st.Detail = c.pool.summary() + "; models: " + strings.Join(st.Models, ", ")
	case kind == kindNetwork && msg != "":
		st.Ready = false
		st.Detail = "upstream unreachable: " + msg
	default:
		st.Ready = false
		st.Detail = "all accounts unavailable (" + c.pool.summary() + ")"
		if msg != "" {
			st.Detail += "; last error: " + msg
		}
	}

	if !at.IsZero() && !st.Ready {
		st.Detail += " (last attempt " + humanAge(time.Since(at)) + " ago)"
	}
	return st
}

func (c *Client) lastUpstream() (errKind, string, time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.lastErrKind, c.lastErr, c.lastErrAt
}

// humanAge renders a duration the way a one-line status wants it.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	default:
		return itoa(int(d.Hours())) + "h"
	}
}
