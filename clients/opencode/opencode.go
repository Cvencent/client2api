package opencode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Registration and construction.
//
// OpenCode Zen is a paid model gateway that speaks the OpenAI chat-completions
// protocol and translates server-side, so this module is a thin OpenAI client:
// POST <base>/chat/completions, one SSE stream back.
//
// Routing: the gateway splits `<client>/<model>` on the FIRST slash
// (internal/core/registry.go:169), so `opencode/gpt-5.1` arrives here with
// req.Model already reduced to `gpt-5.1`.  This module never re-adds or
// re-strips the prefix.
// ---------------------------------------------------------------------------

func init() { core.Register(clientName, New) }

// userAgent is a compatibility string, not a claim about the binary: Zen gates
// the anonymous free tier on the User-Agent's product name and version floor.
// Anything that is not `opencode/<major.minor>` at >= 1.18 is refused with a
// FreeTierError, so this must stay CLI-shaped even though we are not the CLI.
const userAgent = "opencode/1.18.34"

// maxRotate is how many accounts one Chat call may try before giving up.
const maxRotate = 3

// Client implements core.Client plus the optional interfaces listed at the
// bottom of this file.
type Client struct {
	deps core.Deps
	cfg  Config
	// cfgErr records a rejected config block.  It never stops construction:
	// the contract requires that a bad config cannot take the module down.
	cfgErr error

	// ensureOnce guards the lazy initialisation of now/pool and the first
	// account load.  Tests arm it directly to build a hermetic client.
	ensureOnce sync.Once
	now        func() time.Time
	pool       *pool

	httpOnce sync.Once
	http     *http.Client

	modelsMu sync.Mutex
	models   []core.Model
	modelsAt time.Time

	// loginMu guards the interactive-login sessions (free/OAuth).
	loginMu sync.Mutex
	logins  map[string]*loginSession

	errMu   sync.Mutex
	lastErr string
	// affinity pins a conversation to the account that first served it. See
	// affinity.go.
	affinity *core.Affinity
	// sessionOnce guards the lazy minting of the free-tier session id that
	// the anonymous handshake must carry.  See freeTierSession.
	sessionOnce    sync.Once
	session        string
	sessionCounter uint32
}

// New builds the module.  It never returns an error: a malformed config block
// is logged and replaced with defaults, so `clients.opencode` can never be the
// reason the process fails to start.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{deps: deps}
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		c.cfgErr = err
		deps.Log("clients.%s: config rejected, continuing with defaults: %v", clientName, err)
		cfg = Config{}
	}
	c.cfg = cfg.normalize()
	return c, nil
}

// ensure lazily fills the clock, the pool and the accounts.
func (c *Client) ensure() {
	c.ensureOnce.Do(func() {
		if c.now == nil {
			c.now = time.Now
		}
		if c.pool == nil {
			c.pool = newPool()
		}
		if c.affinity == nil {
			c.affinity = core.NewAffinity(0)
			c.affinity.StartGC()
		}
		c.loadAccounts()
	})
}

// Name is the registered name and the routing prefix.
func (c *Client) Name() string { return clientName }

// httpClient is the deps-supplied client when there is one, else a private
// client with a connection pool sized for a gateway.
func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	c.httpOnce.Do(func() {
		tr := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
		if p := strings.TrimSpace(c.deps.Proxy); p != "" {
			if u, err := url.Parse(p); err == nil {
				tr.Proxy = http.ProxyURL(u)
			}
		}
		c.http = &http.Client{Transport: tr}
	})
	return c.http
}

// requestSpec is one outbound request.
//
// There is deliberately no timeout field: the deadline lives on the context, so
// that a stream's cancel func can outlive this call.  A timeout applied here
// would be cancelled by a deferred cancel the moment the headers arrived,
// killing the body mid-stream.
type requestSpec struct {
	method string
	url    string
	body   []byte
	bearer string
	apiKey string
	orgID  string
	// session is the `x-opencode-session` header.  It is set only for the
	// anonymous free-tier handshake; see freeTierSession in freesession.go.
	session string
	accept  string
}

// do performs one request.
func (c *Client) do(ctx context.Context, spec requestSpec) (*http.Response, error) {
	var body io.Reader
	if spec.body != nil {
		body = bytes.NewReader(spec.body)
	}
	req, err := http.NewRequestWithContext(ctx, spec.method, spec.url, body)
	if err != nil {
		return nil, fmt.Errorf("%s: building request: %w", clientName, err)
	}
	if spec.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if spec.accept != "" {
		req.Header.Set("Accept", spec.accept)
	}
	// The credential kinds are mutually exclusive: an anonymous account sends
	// x-api-key, everything else sends a bearer token.  Preferring apiKey keeps
	// a caller that set both from sending an Authorization header the vendor
	// would reject.
	if spec.apiKey != "" {
		req.Header.Set("x-api-key", spec.apiKey)
	} else if spec.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+spec.bearer)
	}
	if spec.orgID != "" {
		req.Header.Set("x-opencode-org-id", spec.orgID)
	}
	if spec.session != "" {
		req.Header.Set("x-opencode-session", spec.session)
		req.Header.Set("x-opencode-session-id", spec.session)
	}
	for k, v := range c.cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", userAgent)
	return c.httpClient().Do(req)
}

// ---------------------------------------------------------------------------
// core.Client: models.
// ---------------------------------------------------------------------------

// Models returns the catalogue.  It never returns an error: the panel's model
// picker must not go empty because the vendor is having a bad day.
//
// GET /models is public, so an unconfigured module still consults it; the
// built-in catalogue is only the final outage fallback.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	if cached, ok := c.cachedModels(); ok {
		return c.servedModels(cached), nil
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	models, err := c.fetchModels(ctx)
	if err != nil {
		c.noteError("listing models: " + describeError(err))
		if last, ok := c.lastModels(); ok {
			return c.servedModels(last), nil
		}
		return c.servedModels(fallbackModels(c.cfg)), nil
	}
	c.storeModels(models)
	return c.servedModels(models), nil
}

// RefreshModels bypasses the TTL cache.
//
// On failure it returns the last known-good list ALONGSIDE the error, so a
// failed refresh never empties the picker.  The public list is fetched even
// without a credential; the built-in catalogue is only the outage fallback.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	c.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	models, err := c.fetchModels(ctx)
	if err != nil {
		if last, ok := c.lastModels(); ok {
			return c.servedModels(last), err
		}
		return c.servedModels(fallbackModels(c.cfg)), err
	}
	c.storeModels(models)
	return c.servedModels(models), nil
}

// ---------------------------------------------------------------------------
// core.Client: chat.
// ---------------------------------------------------------------------------

// Chat opens one upstream stream.
//
// The request body is built before any account is taken, so a request this
// module cannot express never consumes a pool slot.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}
	// An anonymous-only pool can only serve the free allowlist, so a paid model
	// is refused here instead of spending an upstream round trip on a certain
	// FreeTierError.
	if c.pool.onlyAnonymous() && !anonymousModelAllowed(req.Model, c.cfg) {
		return nil, fmt.Errorf("%w: model %q is not available to the anonymous free account", core.ErrUnsupported, req.Model)
	}
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		return nil, err
	}
	// A request that lands on the anonymous credential needs the free-tier
	// handshake, which is a different body from a signed-in account's.  It is
	// built here, once, and chosen per account inside openChat.
	var freeBody []byte
	if c.pool.hasAnonymous() {
		freeBody, err = buildFreeChatBody(c.cfg, req)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	stream, err := c.openChat(ctx, cancel, req, body, freeBody)
	if err != nil {
		cancel()
		return nil, err
	}
	return stream, nil
}

// openChat takes an account and opens the stream, rotating on a retryable
// verdict.
//
// The last real verdict is what the caller sees: "every account is cooling
// down" is a worse answer than the 429 that caused it.
func (c *Client) openChat(ctx context.Context, cancel context.CancelFunc, req *core.ChatRequest, body, freeBody []byte) (core.Stream, error) {
	var lastErr error
	for attempt := 0; attempt < maxRotate; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		acct, err := c.acquireAccount(req)
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		// The gateway's per-account ceiling sits above the pool's own: when the
		// operator's limit is the tighter one, hand the pool slot back and try
		// another account rather than holding a slot we cannot use.
		if err := req.AcquireAccountSlot(acct.ID); err != nil {
			c.pool.release(acct.ID)
			continue
		}
		// Bind only after the slot is genuinely held, so a busy account that was
		// skipped cannot capture the conversation.
		c.bindServedConversation(req, acct.ID)
		// NoteServedBy is called right after the pick: this is the account the
		// panel must show, and nothing later in the request may change it.
		core.NoteServedBy(req, acct.ID)

		// An OAuth credential is refreshed in place when it is about to expire;
		// a refresh failure is not fatal, so the request still goes out with the
		// token we have and the vendor decides.
		if err := c.ensureFreshOAuth(ctx, acct); err != nil {
			c.noteError("refreshing OAuth token: " + describeError(err))
		}

		// The anonymous credential only answers with the free-tier handshake
		// applied; every other account sends the plain body.
		// The handshake is two independent things — the body and the session
		// header — and the gate refuses a request that carries only one.
		spec := requestSpec{
			method: "POST",
			url:    c.cfg.chatURL(),
			body:   body,
			accept: "text/event-stream",
		}
		if freeBody != nil && acct.authMode() == "anonymous" {
			spec.body = freeBody
			spec.session = c.freeTierSession()
		}
		resp, err := c.do(ctx, acct.authSpec(spec))
		if err != nil {
			c.pool.release(acct.ID)
			lastErr = c.classifyErrFor(acct, "chat", err)
			if !core.Retryable(core.FailureKindOf(lastErr)) {
				return nil, lastErr
			}
			continue
		}
		if resp.StatusCode != 200 {
			errBody := readLimited(resp.Body, maxErrorBody)
			resp.Body.Close()
			c.pool.release(acct.ID)
			// classifyHTTP records the verdict against the account itself.
			lastErr = c.classifyHTTP("chat", acct, resp.StatusCode, errBody)
			if !core.Retryable(core.FailureKindOf(lastErr)) {
				return nil, lastErr
			}
			continue
		}
		return newChatStream(c, resp.Body, acct, ctx, cancel, !req.Stream), nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no OpenCode Zen account could serve the request", core.ErrBusy)
}

// ---------------------------------------------------------------------------
// core.Client: status.
// ---------------------------------------------------------------------------

// Status is cheap and makes no network call: the panel polls it every 10s.
func (c *Client) Status(ctx context.Context) core.Status {
	c.ensure()
	now := c.now()
	st := core.Status{
		Name:      clientName,
		UpdatedAt: now.UTC(),
		Accounts:  c.pool.statuses(now),
		Models:    c.modelIDs(),
	}
	switch {
	case c.cfgErr != nil:
		st.Detail = "config rejected, using defaults: " + truncate(core.Redact(c.cfgErr.Error()), 200)
	case c.pool.size() == 0:
		st.Detail = "no OpenCode Zen account yet; set " + apiKeyEnv +
			", add one in the panel, or import opencode's auth.json"
	default:
		st.Ready = true
		st.Detail = c.pool.summary(now, c.cfg.maxInFlight())
	}
	if msg := c.lastErrorNote(); msg != "" {
		if st.Detail == "" {
			st.Detail = "last error: " + msg
		} else {
			st.Detail += "; last error: " + msg
		}
	}
	return core.RedactStatus(st)
}

// noteError records the module's most recent failure for Status, logging it
// once per distinct message.  Everything credential-shaped is redacted first.
func (c *Client) noteError(msg string) {
	msg = truncate(core.Redact(msg), 300)
	c.errMu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.errMu.Unlock()
	if changed {
		c.deps.Log("clients.%s: %s", clientName, msg)
	}
}

// lastErrorNote reads the recorded failure.
func (c *Client) lastErrorNote() string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.lastErr
}

// Compile-time proof of the interfaces this module implements.
//
// Deliberately absent, and why:
//   - core.BalanceProvider: Zen exposes no balance, credit or quota endpoint.
//     /zen/v1/key, /zen/v1/credits and /zen/v1/me all answer 404 with an HTML
//     body.  A fabricated zero would be worse than saying so.
//   - Reviver, CaptchaProvider: there is no session to revive, and the login
//     flows need no browser captcha.
//
// Implemented here: LoginProvider/RealmLoginProvider (the free anonymous realm
// and the OpenCode Console device-code flow).
//   - CheckinProvider, TaskProvider, BatchPlanner: Zen has no check-in and no
//     task/claim API.
//   - ConversationBinder, HintProvider, Degrader, PackageProvider,
//     VoucherProvider, BundleImporter: not needed by this module's shape.
//
// Implemented in health.go: HealthProvider, the pool census the status page
// reads as its servability verdict.
var (
	_ core.Client              = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.PoolStatsReporter   = (*Client)(nil)
)
