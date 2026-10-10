// Package lobsterai implements the LobsterAI (有道龙虾, Youdao) upstream.
//
// LobsterAI is an OpenAI-compatible chat endpoint behind a Youdao account
// system, and it breaks the shape of every other module in this repository in
// four ways:
//
//   - Credentials cannot be minted by a password exchange.  The operator signs
//     in through a browser page that hands an authCode back to a local callback,
//     which is then exchanged for a token pair.
//   - Chat is SSE-ONLY.  Sending stream:false makes the upstream answer 500, so
//     this module always asks for a stream and re-aggregates it when the caller
//     wanted a single answer.
//   - The chat response carries no {code,msg,data} envelope, while every other
//     endpoint does.  The envelope check is therefore applied to the account
//     endpoints only.
//   - There is no image input anywhere in the product, so an image part is
//     refused with core.ErrUnsupported rather than silently dropped.
//
// The module implements core.Client, core.AccountManager,
// core.CredentialImporter, core.LoginProvider, core.CheckinProvider,
// core.BalanceProvider, core.ModelRefresher and core.PoolStatsReporter.  It
// deliberately does NOT implement core.ModelLimitsProvider: see models.go.
package lobsterai

import (
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

func init() {
	core.Register(clientName, New)
}

// defaultHTTPClient is used when the harness did not hand us one.  Chat streams
// can idle for a long time between deltas, so the transport keeps connections
// warm rather than relying on the default pool.
var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Client is the LobsterAI module.
//
// One mutex per concern, never one global lock: the model cache, the version
// cache, the login sessions and the error note are independent, and the account
// pool carries its own lock so a slow chat request never blocks a Status call.
type Client struct {
	deps   core.Deps
	cfg    Config
	cfgErr error
	now    func() time.Time

	pool *pool

	// vendorClient, when non-nil, replaces the shared HTTP client with one
	// whose TLS handshake imitates the official desktop client.  Set by New
	// when the config names a tls_profile.
	vendorClient *http.Client

	modelsMu sync.Mutex
	models   []core.Model
	modelsAt time.Time

	versionMu       sync.Mutex
	version         string
	versionAt       time.Time
	versionFetching bool

	loginMu sync.Mutex
	logins  map[string]*loginSession

	// affinity pins a conversation to the account that first served it. See
	// affinity.go.
	affinity *core.Affinity

	errMu   sync.Mutex
	lastErr string
}

// New builds the module.
//
// It never fails: a rejected config is remembered and reported through Status
// and Chat, because a module that refuses to be constructed disappears from the
// panel entirely and the operator loses the ability to see what is wrong.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{
		deps:   deps,
		now:    time.Now,
		pool:   newPool(),
		logins: map[string]*loginSession{},
	}
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		c.cfgErr = err
		deps.Log("config rejected, continuing with defaults: %v", err)
		cfg = Config{}
	}
	c.cfg = cfg.normalize()
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		if fhc, ferr := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		}); ferr == nil {
			c.vendorClient = fhc
		} else {
			deps.Log("lobsterai: tls fingerprint %q ignored: %v", string(fp), ferr)
		}
	}
	recs := c.loadAccounts()
	if len(recs) > 0 {
		deps.Log("lobsterai: loaded %d account(s)", len(recs))
	}
	return c, nil
}

// ensure fills the fields a Client built without New would be missing, so a
// hand-assembled client in a test behaves like a real one.
func (c *Client) ensure() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pool == nil {
		c.pool = newPool()
	}
	c.loginMu.Lock()
	if c.logins == nil {
		c.logins = map[string]*loginSession{}
	}
	c.loginMu.Unlock()
}

func (c *Client) Name() string { return clientName }

// loadAccounts builds the initial pool from the credential store plus any
// credential the config file supplied.
//
// The store wins on a collision: it is the copy the renewal path keeps
// up to date, and a config file is written once and then forgotten.  A
// config-supplied account is still worth honouring, because it is the only way
// to start the process from a provisioned token with no sign-in at all.
func (c *Client) loadAccounts() []accountRecord {
	recs := loadCredentials(c.credentialsPath())
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		seen[r.ID] = true
	}
	now := c.now()
	for _, acct := range c.cfg.configuredAccounts() {
		rec := accountFromConfig(acct, now)
		if rec.ID == "" || seen[rec.ID] {
			continue
		}
		seen[rec.ID] = true
		recs = append(recs, rec)
	}
	c.pool.reload(recs)
	return recs
}

func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	if c.vendorClient != nil {
		return c.vendorClient
	}
	return defaultHTTPClient
}

// Models reports the catalogue.  It never fails: routing and the panel need
// something to show even while the upstream is unreachable, so a failed fetch
// falls back to the last good list and then to the built-in catalogue.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	if models, ok := c.cachedModels(); ok {
		return models, nil
	}
	models, err := c.fetchModels(ctx)
	if err == nil && len(models) > 0 {
		return models, nil
	}
	if err != nil {
		c.noteError("listing models: " + c.describeError(err))
	}
	if last := c.lastModels(); len(last) > 0 {
		return last, nil
	}
	return fallbackModels(c.cfg), nil
}

// RefreshModels bypasses the TTL.  As core.ModelRefresher requires, a failure
// still returns the best list available alongside the error.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	models, err := c.fetchModels(ctx)
	if err == nil && len(models) > 0 {
		return models, nil
	}
	if err == nil {
		err = fmt.Errorf("models: the upstream returned an empty model list")
	}
	if last := c.lastModels(); len(last) > 0 {
		return last, err
	}
	return fallbackModels(c.cfg), err
}

// Chat opens a completion.
//
// The request body is built and validated before any account is taken, so an
// unsupported request fails the same way whether or not the upstream is up.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	stream, err := c.openChat(ctx, cancel, req, body)
	if err != nil {
		cancel()
		return nil, err
	}
	return stream, nil
}

// maxRotate bounds the account rotation, counting the first attempt.  Every
// failure kind rotates rather than returning immediately: one dead account must
// not fail a request that another account can serve.
const maxRotate = 3

// openChat picks an account, renews it if needed and opens the stream, rotating
// on failure.
func (c *Client) openChat(ctx context.Context, cancel context.CancelFunc, req *core.ChatRequest, body []byte) (core.Stream, error) {
	// Warm the version cache in the background.  A chat must not wait on a
	// third-party manifest service, so this request sends whatever is cached
	// (the configured fallback on a cold cache) and later ones send the real
	// date-style version.
	c.refreshVersionAsync()
	version := c.clientVersion(ctx)
	var lastErr error
	for attempt := 0; attempt < maxRotate; attempt++ {
		if err := ctx.Err(); err != nil {
			break
		}
		acct, err := c.acquireAccount(req, "")
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		// The gateway's per-account ceiling may be tighter than the pool's own;
		// when it refuses, give the pool slot back and try another account
		// before spending a token refresh on it.
		if err := req.AcquireAccountSlot(acct.ID); err != nil {
			c.pool.release(acct.ID)
			continue
		}
		// Bind only after the slot is genuinely held, so a busy account that was
		// skipped cannot capture the conversation.
		c.bindServedConversation(req, acct.ID)
		if err := c.ensureFresh(ctx, acct); err != nil {
			c.pool.release(acct.ID)
			lastErr = c.classifyErrFor(acct, err)
			continue
		}
		core.NoteServedBy(req, acct.ID)
		spec := requestSpec{
			method:  http.MethodPost,
			url:     c.cfg.chatURL(),
			body:    body,
			bearer:  acct.AccessToken,
			version: version,
			accept:  "text/event-stream, application/json",
			timeout: c.cfg.chatTimeout(),
		}
		resp, err := c.do(ctx, spec)
		if err != nil {
			c.pool.release(acct.ID)
			// A cancelled caller is not evidence about the credential: the
			// operator pressed Stop, or the browser went away.  Without this
			// guard classifyErrFor folds the cancellation into kindServer, and
			// softErrorThreshold of those in a row park the account.
			//
			// ctx is the derived one -- Chat wrapped it in chat_timeout -- so a
			// deadline here may be our own timeout, which *is* worth recording.
			// Only context.Canceled is unambiguous.
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			lastErr = c.classifyErrFor(acct, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			errBody := readLimited(resp.Body, maxErrorBody)
			resp.Body.Close()
			c.pool.release(acct.ID)
			lastErr = c.classifyHTTP("chat", acct.ID, resp.StatusCode, errBody)
			continue
		}
		return newChatStream(c, resp, acct, cancel, !req.Stream), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no LobsterAI account could serve the request", core.ErrBusy)
}

// Status reports the module's health.
//
// It is deliberately cheap: the panel polls it, so it performs no network I/O
// and takes only the pool lock and the two cache locks.
func (c *Client) Status(ctx context.Context) core.Status {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	now := c.now()
	st := core.Status{
		Name:      clientName,
		UpdatedAt: now,
		Accounts:  c.pool.statuses(now),
		Models:    c.modelIDs(),
	}
	version, live := c.versionState()
	switch {
	case c.cfgErr != nil:
		st.Detail = "config rejected, using defaults: " + truncate(c.cfgErr.Error(), 200)
	case len(st.Accounts) == 0:
		st.Detail = "no LobsterAI account yet; sign in from the panel"
	default:
		st.Ready = true
		detail := c.pool.summary(now, c.cfg.maxInFlight())
		if live {
			detail += "; client version " + version
		} else {
			detail += "; client version " + version + " (fallback, the version endpoint has not answered)"
		}
		st.Detail = detail
	}
	if msg := c.lastErrorNote(); msg != "" {
		st.Detail += "; last error: " + msg
	}
	return st
}

// PoolStats reports the in-flight accounting.  core.PoolStatsReporter requires
// it to be cheap, so it only reads the pool's counters.
func (c *Client) PoolStats() core.PoolStats {
	c.ensure()
	inFlight, full := c.pool.stats(c.cfg.maxInFlight())
	return core.PoolStats{InFlight: inFlight, InFlightFull: full}
}

// --- error notes ------------------------------------------------------------

// noteError remembers the last failure and logs it once per change, so a
// polling panel does not fill the log with the same line.
func (c *Client) noteError(msg string) {
	msg = truncate(core.Redact(msg), 300)
	c.errMu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.errMu.Unlock()
	if changed {
		c.deps.Log("lobsterai: %s", msg)
	}
}

func (c *Client) lastErrorNote() string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.lastErr
}

// scrubSecret removes a token from a message before it can reach a log.  The
// generic redactor runs first, then the exact secret is replaced by its mask in
// case the redactor did not recognise the shape.
func scrubSecret(msg, secret string) string {
	msg = core.Redact(msg)
	if secret == "" {
		return msg
	}
	return strings.ReplaceAll(msg, secret, core.MaskSecret(secret))
}

var (
	_ core.Client            = (*Client)(nil)
	_ core.ModelRefresher    = (*Client)(nil)
	_ core.PoolStatsReporter = (*Client)(nil)
)
