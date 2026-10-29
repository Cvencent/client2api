// Package kilocode implements the client2api module for the Kilo AI Gateway
// (api.kilo.ai/api/gateway), the multi-vendor router behind Kilo Code.
//
// The gateway is OpenRouter-shaped: `provider/model` ids, an OpenAI
// chat-completions surface, a public `GET /models` catalogue, and a bearer API
// key.  Its distinguishing feature here is that it serves FREE models to
// unauthenticated callers (rate-limited per IP), so this module ships an
// anonymous free-tier identity that works with no sign-in at all, plus
// panel-managed API keys for an operator's own higher limits.
package kilocode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const clientName = "kilocode"

// softErrorThreshold is how many consecutive unattributable failures park an
// account.  A single 500 from one upstream provider is not a verdict on a key.
const softErrorThreshold = 3

// maxRotate bounds how many credentials one chat request may try.  Rotation is
// for "this key is out of credit / throttled / rejected", not a retry loop: the
// gateway's own retry budget sits on top of this.
const maxRotate = 3

// maxErrorBytes bounds how much of an error body is read into memory.
const maxErrorBytes = 64 << 10

var defaultHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	},
}

func init() { core.Register(clientName, New) }

// Client is the module.  Every mutable knob lives behind mu or inside pool /
// modelCache, so Chat is safe to call concurrently.
type Client struct {
	deps   core.Deps
	cfg    Config
	cfgErr error

	pool   *pool
	models *modelCache
	now    func() time.Time

	// affinity pins a conversation to the credential that first served it.
	// See affinity.go.
	affinity *core.Affinity

	mu            sync.Mutex
	liveLimit     *int
	liveThreshold *int
	liveCooldown  time.Duration
	lastErr       string

	// loginMu guards logins, the in-flight device authorisations the panel
	// drives through core.LoginProvider.  See login.go.
	loginMu sync.Mutex
	logins  map[string]*loginSession
}

// New builds the module.
//
// It NEVER returns an error: a config the operator mistyped is remembered,
// logged and reported through Status, and the defaults are used instead.  A
// module that refuses to be constructed disappears from the panel, which is
// exactly when it needs to be visible.
func New(deps core.Deps) (core.Client, error) {
	c := &Client{deps: deps, now: time.Now, pool: newPool(), models: &modelCache{}}
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		c.cfgErr = err
		deps.Log("%s: %v (using built-in defaults)", clientName, core.Redact(err.Error()))
		cfg = Config{}
	}
	c.cfg = cfg.normalize()
	c.pool.reload(c.buildCredentials())
	return c, nil
}

// ensure fills the fields a hand-assembled (test) client may be missing, and
// loads credentials exactly once.
func (c *Client) ensure() {
	if c.now == nil {
		c.now = time.Now
	}
	if c.pool == nil {
		c.pool = newPool()
		c.pool.reload(c.buildCredentials())
	}
	if c.models == nil {
		c.models = &modelCache{}
	}
	if c.affinity == nil {
		c.affinity = core.NewAffinity(0)
		c.affinity.StartGC()
	}
}

func (c *Client) Name() string { return clientName }

func (c *Client) httpClient() *http.Client {
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	return defaultHTTPClient
}

// limit is the per-credential in-flight ceiling.  An explicit live value of 0
// means "no ceiling", which is different from "unset".
func (c *Client) limit() int {
	c.mu.Lock()
	v := c.liveLimit
	c.mu.Unlock()
	if v != nil {
		return *v
	}
	return c.cfg.maxInFlight()
}

func (c *Client) parkThreshold() int {
	c.mu.Lock()
	v := c.liveThreshold
	c.mu.Unlock()
	if v != nil && *v > 0 {
		return *v
	}
	return softErrorThreshold
}

// cooldown is the generic park duration, honouring a live override.
func (c *Client) cooldown() time.Duration {
	c.mu.Lock()
	d := c.liveCooldown
	c.mu.Unlock()
	if d > 0 {
		return d
	}
	return c.cfg.cooldown()
}

// noteError remembers the last error for Status and logs it only when it
// changed, so a failing endpoint does not flood the log.
func (c *Client) noteError(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	msg = truncate(core.Redact(msg), 300)
	c.mu.Lock()
	changed := msg != c.lastErr
	c.lastErr = msg
	c.mu.Unlock()
	if changed {
		c.deps.Log("%s: %s", clientName, msg)
	}
}

func (c *Client) lastError() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// --- chat -----------------------------------------------------------------

// Chat picks a credential, sends the request and returns the response stream.
//
// The body is built and validated BEFORE any account is taken, so a request
// this module cannot express costs nothing but a 400.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.ensure()
	if ctx == nil {
		ctx = context.Background()
	}
	// free_only refuses a paid id before any upstream round trip.  The
	// catalogue is narrowed too, but a caller can still name a paid id
	// explicitly, and that must not spend paid credit.
	if req != nil && !c.modelAllowed(req.Model) {
		return nil, fmt.Errorf("%w: model %q is not a free Kilo model (free_only is on)", core.ErrUnsupported, req.Model)
	}
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		return nil, err
	}
	mode := requestMode(req.Options)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var lastErr error
	busySeen := map[string]bool{}
	for attempt := 0; attempt < maxRotate; attempt++ {
		var acct accountRecord
		var err error
		if len(busySeen) > 0 {
			// Once a bound credential has lost the gateway-slot race, do not
			// let affinity hand the same credential straight back on the
			// retry. Rotate through the pool instead.
			acct, err = c.pool.acquire(c.now(), c.limit())
		} else {
			acct, err = c.acquireAccount(req)
		}
		if err != nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		// The gateway's per-account ceiling may be tighter than the pool's own;
		// when it refuses, give the pool slot back and try another credential.
		if err := req.AcquireAccountSlot(acct.ID); err != nil {
			c.pool.release(acct.ID)
			if busySeen[acct.ID] {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, fmt.Errorf("%w: no Kilo credential has a free in-flight slot", core.ErrBusy)
			}
			busySeen[acct.ID] = true
			// A full account is not an upstream attempt. Keep the request's
			// switch budget for a credential that can actually be called.
			attempt--
			continue
		}
		// The account is settled here, before the response is known: the
		// gateway's "served by" column must not be a guess.
		c.bindServedConversation(req, acct.ID)
		core.NoteServedBy(req, acct.ID)

		stream, err := c.openChat(ctx, acct, body, mode)
		if err == nil {
			return stream, nil
		}
		c.pool.release(acct.ID)
		lastErr = err
		c.noteError(err.Error())
		if !retryableChatError(err) {
			return nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("%w: no Kilo credential could serve the request", core.ErrBusy)
}

// retryableChatError decides whether another credential is worth trying.  A
// request the vendor rejected on its shape (or a model that does not exist)
// would fail on every key, so it is returned as-is.
func retryableChatError(err error) bool {
	switch failureKindOfError(err) {
	case kindCredit, kindDailyQuota, kindRateLimit, kindAuth, kindServer:
		return true
	}
	return false
}

// openChat issues one streaming completion against a credential the caller
// already holds an in-flight slot for.
func (c *Client) openChat(ctx context.Context, acct accountRecord, body []byte, mode string) (core.Stream, error) {
	return c.doChat(ctx, acct, body, true, mode)
}

// doChat is the single place a POST /chat/completions is issued.  holds says
// whether the caller took an in-flight slot from the pool (a normal chat does;
// the panel's Test button does not), because only the holder may release it.
func (c *Client) doChat(ctx context.Context, acct accountRecord, body []byte, holds bool, mode string) (core.Stream, error) {
	reqCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.chatTimeout())
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.chatURL(), bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("kilocode: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.cfg.applyHeaders(httpReq, acct.bearer(), mode)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		cancel()
		return nil, c.classifyUpstream(acct.ID, err)
	}
	if resp.StatusCode != http.StatusOK {
		raw := readLimited(resp.Body, maxErrorBytes)
		_ = resp.Body.Close()
		cancel()
		return nil, c.classifyHTTP("chat", acct.ID, resp.StatusCode, raw)
	}
	return newChatStream(c, resp, acct.ID, cancel, holds), nil
}

// --- status ---------------------------------------------------------------

// Status performs no network I/O at all: the panel polls it every ten seconds
// and a module that blocks here takes the whole panel down with it.
func (c *Client) Status(ctx context.Context) core.Status {
	c.ensure()
	now := c.now()
	st := core.Status{
		Name:      clientName,
		UpdatedAt: now,
		Accounts:  c.pool.statuses(now),
		Models:    c.modelIDs(),
	}
	st.Ready = c.pool.ready(now, c.limit()) > 0
	st.Detail = c.pool.summary(now, c.limit())
	if c.cfgErr != nil {
		st.Detail += "; config error: " + c.describeError(c.cfgErr)
	}
	if msg := c.lastError(); msg != "" {
		st.Detail += "; last error: " + msg
	}
	return st
}

// PoolStats reports the in-flight counters the panel shows.
func (c *Client) PoolStats() core.PoolStats {
	c.ensure()
	inFlight, full := c.pool.stats(c.limit())
	stats := core.PoolStats{InFlight: inFlight, InFlightFull: full}
	if c.affinity != nil {
		stats.StickySessions = c.affinity.Count()
	}
	return stats
}

// Health is the stricter view /healthz uses: a pool whose only usable
// credentials are already at their in-flight ceiling is not servable.
func (c *Client) Health() core.Health {
	c.ensure()
	now := c.now()
	h := core.Health{Total: c.pool.len()}
	for _, a := range c.pool.statuses(now) {
		switch {
		case !a.Enabled:
			h.Disabled++
		case a.State == "ready":
			h.Ready++
		default:
			h.Cooling++
		}
	}
	servable := c.pool.ready(now, c.limit())
	h.Servable = servable > 0
	h.Note = fmt.Sprintf("%d of %d credential(s) selectable", servable, h.Total)
	if h.Total == 0 {
		h.Note = "no Kilo credential configured"
	}
	return h
}

// --- shared helpers -------------------------------------------------------
//
// These are package-local on purpose: a shared helper would have to guess which
// vendor's quirks it is accommodating.

// truncate cuts a string to n RUNES (vendor messages are frequently Chinese, so
// bytes would cut mid-character) and marks the cut.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// toInt reads an int out of the several shapes JSON and config files produce.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case uint:
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		return int(n), true
	case uint64:
		return int(n), true
	case float32:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(i), true
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		var num json.Number
		if err := json.Unmarshal([]byte(s), &num); err != nil {
			return 0, false
		}
		return toInt(num)
	}
	return 0, false
}

// readLimited reads at most n bytes; a read error is not fatal for an error
// body, so the partial content is returned.
func readLimited(r io.Reader, n int64) []byte {
	if r == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil && len(b) == 0 {
		return nil
	}
	return b
}

// --- interface assertions -------------------------------------------------

var (
	_ core.Client              = (*Client)(nil)
	_ core.AccountManager      = (*Client)(nil)
	_ core.CredentialImporter  = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
	_ core.PoolStatsReporter   = (*Client)(nil)
	_ core.HealthProvider      = (*Client)(nil)
	_ core.ConversationBinder  = (*Client)(nil)
	_ core.LiveReloader        = (*Client)(nil)
)
