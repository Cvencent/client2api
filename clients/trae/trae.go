// Package trae implements the Trae client module for client2api.
//
// It speaks the Trae SOLO chat protocol (POST /api/agent/v3/llm_utils_chat),
// auto-discovers the credential the Trae desktop app keeps in
// %APPDATA%\Trae CN\User\globalStorage\storage.json, and exposes it as an
// OpenAI-compatible model behind the "trae/" route prefix.
//
// Ported from the MIT-licensed references in client2api-lab/_upstream:
// trae2api-web (Go, the port target) and trae2api (Node, the written spec).
// See README.md.
package trae

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

// clientName is both the registered name and the routing prefix.
const clientName = "trae"

func init() { core.Register(clientName, New) }

// Client implements core.Client for Trae.  It is safe for concurrent use.
type Client struct {
	name string
	cfg  *Config
	log  func(string, ...any)
	now  func() time.Time

	// httpClient serves the short JSON calls; streamClient serves the SSE
	// chat body and must not carry a total timeout.
	httpClient   *http.Client
	streamClient *http.Client

	pool *Pool

	// affinity pins a conversation to the account that already served it.  See
	// affinity.go.  It is created by New, and every method on it is nil-safe, so
	// a Client assembled by a test literal keeps working with stickiness simply
	// switched off.
	affinity *core.Affinity

	// backoffBase is the un-jittered base for the pause between two
	// in-module attempts.  Zero — what New leaves it at — means
	// core.RotateBackoffBase; tests inject a tiny value so the pacing can be
	// observed without waiting out the shared 500ms base.
	backoffBase time.Duration

	// dataDir is this module's private directory; the panel-added account
	// store lives there and nowhere else.  storeMu guards store, which is
	// separate from pool's own lock.
	dataDir string
	storeMu sync.Mutex
	store   *accountStore

	mu       sync.Mutex
	models   []core.Model
	modelsAt time.Time
	lastErr  string

	// logins holds the in-flight panel login sessions (weblogin.go).  The map is
	// created lazily so a Client built by a test literal works unchanged.
	loginMu sync.Mutex
	logins  map[string]*webLoginSession

	requests atomic.Int64
	failures atomic.Int64
}

// New builds the trae client.  A missing credential is NOT an error: Status()
// reports why nothing can be served and Chat returns core.ErrNotConfigured.
func New(deps core.Deps) (core.Client, error) {
	cfg := loadConfig(deps.Config, deps.Logf)
	c := &Client{
		name:    clientName,
		cfg:     cfg,
		now:     time.Now,
		dataDir: deps.DataDir,
		store:   loadAccountStore(deps.DataDir),
		log:     func(format string, args ...any) { deps.Log(format, args...) },
	}
	hc := deps.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	c.httpClient = hc
	c.streamClient = hc

	// A named profile replaces the shared client with one whose TLS handshake
	// imitates that browser.  The Go hello is one of the few things about this
	// proxy a vendor can spot without reading a single header, so a module that
	// cares names a profile here.  The default (empty) returns the stock client,
	// leaving the shared one in place, so this is a no-op unless asked for.
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, err := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if err != nil {
			c.log("tls fingerprint %q ignored: %v", string(fp), err)
		} else {
			c.httpClient, c.streamClient = fhc, fhc
			c.log("tls handshake imitates the %s client", string(fp))
		}
	}

	c.pool = NewPool(nil)
	// A cooldown is a vendor verdict with a deadline, so it must outlive this
	// process: without this, a restart replays the request that was just
	// refused.  Attaching before the first rebuild is what lets the restored
	// records be handed to the accounts that rebuild creates.
	c.pool.AttachState(c.dataDir, c.log)
	// Conversation→account stickiness.  core's defaults are the 30-minute idle
	// TTL and the 5-minute sweep, the reference's numbers.  The GC goroutine
	// lives as long as the process, which is as long as the client: core.Client
	// has no Close to hang a stop from.
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	c.rebuildPool()
	if n := c.pool.Len(); n == 0 {
		c.log("no credential found; set clients.trae.storage_path, storage_paths or access_token")
	} else {
		c.log("ready with %d account(s): %s", n, c.pool.Summary())
	}
	return c, nil
}

// Name is the routing prefix.
func (c *Client) Name() string { return c.name }

// Models returns the model catalogue.  The live catalogue is cached; when the
// upstream is unreachable (or there is no account) the static SOLO list is
// served so /v1/models never goes empty.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	c.mu.Lock()
	cached, at := c.models, c.modelsAt
	c.mu.Unlock()
	if len(cached) > 0 && c.now().Sub(at) < defaultModelsTTL {
		return cached, nil
	}

	a, ok := c.pool.Pick(nil)
	if !ok {
		return c.staticModels(), nil
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()

	models, err := c.fetchModels(rctx, a)
	if err != nil {
		c.noteErr(err)
		c.log("model catalogue fetch failed, using static list: %v", err)
		if len(cached) > 0 {
			return cached, nil
		}
		return c.staticModels(), nil
	}
	c.pool.MarkSuccess(a)
	c.mu.Lock()
	c.models, c.modelsAt = models, c.now()
	c.mu.Unlock()
	return models, nil
}

// RefreshModels implements core.ModelRefresher: it re-reads the catalogue from
// the vendor, deliberately bypassing the defaultModelsTTL that Models() honours.
// The panel calls it behind its "re-fetch from upstream" button, so it must do
// the network round trip even when the cache is still warm.
//
// A failed refresh must never empty the catalogue.  On every error path the
// previous good list is returned alongside the error -- or the static fallback
// when nothing has ever been fetched -- so one flaky refresh cannot blank the
// model picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	previous := append([]core.Model(nil), c.models...)
	c.mu.Unlock()

	fallback := previous
	if len(fallback) == 0 {
		fallback = c.staticModels()
	}

	a, ok := c.pool.Pick(nil)
	if !ok {
		return fallback, fmt.Errorf("%s: refresh models: %w", c.name, core.ErrNotConfigured)
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()

	models, err := c.fetchModels(rctx, a)
	if err != nil {
		c.noteErr(err)
		return fallback, fmt.Errorf("%s: refresh models: %s", c.name, scrubSecret(err.Error(), a.Token()))
	}
	c.pool.MarkSuccess(a)
	c.mu.Lock()
	c.models, c.modelsAt = models, c.now()
	c.mu.Unlock()
	return models, nil
}

// scrubSecret renders an upstream failure without ever echoing the credential
// we sent.  core.Redact catches credential-shaped text (bearer headers, JWTs,
// "token=..." labels); the account's own token is replaced verbatim as well,
// because a bare token carries no label for core.Redact to key on.
func scrubSecret(msg, secret string) string {
	msg = core.Redact(msg)
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, core.MaskSecret(secret))
	}
	return msg
}

// staticModels is the offline fallback catalogue.
func (c *Client) staticModels() []core.Model {
	ids := c.cfg.modelIDs()
	out := make([]core.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, core.Model{ID: id, OwnedBy: c.name})
	}
	return out
}

// Chat opens a streaming completion.  It fails over across accounts according
// to the classified error kind.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if req == nil {
		return nil, core.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.pool.Len() == 0 {
		return nil, core.ErrNotConfigured
	}
	c.requests.Add(1)

	body, err := BuildBody(req, c.cfg.payloadOptions())
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	attempts := c.cfg.attempts()
	if attempts < 1 {
		attempts = 1
	}
	// The stickiness key and the model are derived once, before the rotation
	// loop.  The key must not change between attempts: a retry on another
	// account has to re-bind the SAME conversation rather than start choosing a
	// new one.  The model is what the scored picker ranks candidates for.
	convKey := conversationKey(req)
	model := strings.TrimSpace(req.Model)

	skip := map[string]bool{}
	var lastErr error
	// lastAccount names the account behind lastErr.  The loop can move on
	// before it runs out of attempts, so the failure must be attributed to
	// whoever produced the terminal answer, not to whoever went first.
	var lastAccount string
	// busy records that every account we reached was at its per-account
	// in-flight ceiling.
	busy := false

	for attempt := 0; attempt < attempts; attempt++ {
		a, ok := c.pickAccount(skip, convKey, model)
		if !ok {
			break
		}
		// Take the account's in-flight slot before any upstream call.  A full
		// account is skipped, not blamed, so the next iteration uses another.
		if err := req.AcquireAccountSlot(a.ID()); err != nil {
			skip[a.ID()] = true
			busy = true
			continue
		}
		// Name the credential for the gateway's usage ledger and console row.
		// A retry overwrites this, so the value read once Chat returns is the
		// account of the attempt that actually got through.
		core.NoteServedBy(req, a.ID())

		// Pace the rotation.  Walking accounts back to back is what makes a
		// vendor read this client as a scraper, so wait before the second and
		// every later attempt — never after the last, and never before the
		// first.  A dead caller context stops the loop here and whatever
		// attempt failed owns the error.
		//
		// The index is attempt-1 so the first wait is BackoffFrom(base, 0) --
		// the same first wait the gateway's own rotation uses; taking the loop
		// index would start at base<<1 and make this module sleep twice as
		// long as the gateway for the same failure.  BackoffFrom already
		// applies core.JitterDur, so it must not be wrapped again.
		if attempt > 0 {
			if !core.SleepCtx(ctx, core.BackoffFrom(c.rotateBase(), attempt-1)) {
				if lastErr != nil {
					return nil, classifyTerminal(lastAccount, lastErr)
				}
				return nil, ctx.Err()
			}
		}

		if err := c.ensureToken(ctx, a); err != nil {
			c.noteErr(err)
			c.log("attempt %d/%d: account %s unusable: %v", attempt+1, attempts, a.Label(), err)
			lastErr, lastAccount = err, a.ID()
			skip[a.ID()] = true
			continue
		}

		rc, apiErr, transportErr := c.chatStream(ctx, a, body)
		switch {
		case transportErr != nil:
			c.failures.Add(1)
			c.noteErr(transportErr)
			kind, _ := c.pool.MarkFailureForModel(a, model, transportErr)
			c.log("attempt %d/%d on %s: %v", attempt+1, attempts, a.Label(), transportErr)
			lastErr, lastAccount = transportErr, a.ID()
			skip[a.ID()] = true
			if !retryableKind(kind) {
				return nil, classifyTerminal(a.ID(), transportErr)
			}
		case apiErr != nil:
			c.failures.Add(1)
			c.noteErr(apiErr)
			kind, _ := c.pool.MarkFailureForModel(a, model, apiErr)
			c.log("attempt %d/%d on %s rejected: %v", attempt+1, attempts, a.Label(), apiErr)
			lastErr, lastAccount = apiErr, a.ID()
			if !retryableKind(kind) || attempt == attempts-1 {
				return nil, classifyTerminal(a.ID(), apiErr)
			}
			skip[a.ID()] = true
		default:
			c.pool.MarkSuccessForModel(a, model)
			return newStream(c, a, rc), nil
		}
	}

	if lastErr != nil {
		return nil, classifyTerminal(lastAccount, lastErr)
	}
	if busy {
		return nil, core.ErrBusy
	}
	return nil, core.ErrNotConfigured
}

// rotateBase is the un-jittered pause before the second and later attempts.
// trae has no config knob for rotation pacing — cooldown_sec is the pool's own
// per-account cooldown, not an inter-attempt wait — so the shared contract's
// base is the default.
func (c *Client) rotateBase() time.Duration {
	if c.backoffBase > 0 {
		return c.backoffBase
	}
	return core.RotateBackoffBase
}

// ensureToken makes sure the account has a usable access token.  With
// self_renew off (the default) an expired token cannot be repaired here — the
// desktop app must be opened — so the account is skipped with an ErrAuth
// explaining why; the caller can then report an auth failure if every account
// is in that state instead of a misleading "nothing configured".
func (c *Client) ensureToken(ctx context.Context, a *Auth) error {
	if !a.NeedsRefresh(c.cfg.refreshSkew()) {
		return nil
	}
	if !c.cfg.selfRenew() {
		if a.Token() == "" || a.Expired() {
			c.log("account %s needs a fresh token but self_renew is off; open Trae to refresh it", a.Label())
			return &Error{Kind: ErrAuth, Msg: "access token expired and self_renew is off; open Trae to refresh it"}
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()
	if err := c.refreshToken(rctx, a); err != nil {
		c.noteErr(err)
		c.pool.MarkFailure(a, err)
		c.log("token refresh for %s failed: %v", a.Label(), err)
		return err
	}
	c.log("refreshed access token for %s (expires %s)", a.Label(), formatTime(a.Expiry()))
	return nil
}

// Status renders the panel view.  It never blocks and never touches the
// network.
func (c *Client) Status(ctx context.Context) core.Status {
	accounts := c.pool.Snapshot()
	return core.Status{
		Name:      c.name,
		Ready:     c.pool.Ready() && c.usableNow(),
		Detail:    c.detailLine(),
		Accounts:  accounts,
		Models:    c.cfg.modelIDs(),
		UpdatedAt: time.Now(),
	}
}

// usableNow reports whether some account could actually serve a request: a
// live access token, or a refresh token when self-renewal is enabled.
func (c *Client) usableNow() bool {
	for _, a := range c.pool.Accounts() {
		if a.Token() != "" && !a.Expired() {
			return true
		}
		if c.cfg.selfRenew() && a.RefreshTokenValue() != "" {
			return true
		}
	}
	return false
}

// detailLine is the one-line human summary for Status().
func (c *Client) detailLine() string {
	parts := []string{c.pool.Summary()}
	parts = append(parts, fmt.Sprintf("channel %s", c.cfg.functionName()))
	parts = append(parts, "self_renew "+onOff(c.cfg.selfRenew()))
	if n := c.requests.Load(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d requests / %d failures", n, c.failures.Load()))
	}
	if err := c.lastErrText(); err != "" {
		parts = append(parts, "last error: "+err)
	}
	return strings.Join(parts, "; ")
}

// recordNotifyUsage folds a notify_usage frame's credit balances into the
// account so Status() can surface them.
func (c *Client) recordNotifyUsage(a *Auth, m map[string]any) {
	if a == nil || m == nil {
		return
	}
	mode, _ := m["billing_mode"].(string)
	var ide, work int64
	if info, ok := m["cn_credits_remain_info"].(map[string]any); ok {
		ide = numFromAny(info["ide_credits"])
		work = numFromAny(info["work_credits"])
	}
	a.SetCredits(ide, work, mode)
}

func (c *Client) noteErr(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastErr = err.Error()
	c.mu.Unlock()
}

func (c *Client) lastErrText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// newTraceID renders 16 random bytes as hex, used for the X-Request-ID pair and
// for the trace id inside x-flow-traceparent.
func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// newSpanID renders 8 random bytes as hex: the 16-character span half of
// x-flow-traceparent, matching the reference's uuid-slice of the same width.
func newSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
