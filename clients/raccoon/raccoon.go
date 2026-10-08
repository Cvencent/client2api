// Package raccoon implements a client for 商汤小浣熊 (SenseTime Raccoon Work,
// https://xiaohuanxiong.com).
//
// It was written as a clean-room Go port of the reference TypeScript provider
// in the Gitee project `iJetLi/deepseek-harness-codearts` (src/raccoon*.ts).
// Every protocol constant, header, envelope rule and display-name rule below
// is traceable to that reference; the Go structure follows the local
// `clients/qwenwork` module.
//
// Deliberate scope decisions (see README.md):
//   - interactive sign-in IS implemented: a loopback page carries a locally
//     rendered QR code and an SMS form.  The QR encoder is stdlib-only (see
//     qr.go), so no dependency was added.
//   - the daily 300 login credits are claimed through the desktop login grant
//     (POST /api/web/desktop/v1/login/points/grant), exposed as the
//     `login-points` CheckinProvider action; the server grants them once per
//     day and answers `granted:false` on a repeat request.
package raccoon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register("raccoon", New) }

const (
	// modelsRetryInterval rate-limits the background catalogue refresh so a
	// failing upstream cannot be hammered by every Models() call.
	modelsRetryInterval = 30 * time.Second
)

// Client is the raccoon module.
type Client struct {
	deps core.Deps
	cfg  config

	accountsPath string
	statePath    string

	pool *pool

	httpOnce sync.Once
	httpc    *http.Client

	modelsMu        sync.Mutex
	models          []core.Model
	modelsAt        time.Time
	modelsAttemptAt time.Time
	modelsLoading   bool

	stateMu    sync.Mutex
	lastErr    string
	lastErrAt  time.Time
	upstreamOK bool

	// loginMu guards logins, the in-flight interactive login sessions.  Each
	// session owns its own mutex and its own loopback server.
	loginMu sync.Mutex
	logins  map[string]*loginSession

	// now is a test seam; production always uses time.Now.
	now func() time.Time
}

// New builds the module. A malformed config is logged and replaced with
// defaults: it must never be a construction failure.
func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("raccoon: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	c := &Client{deps: deps, cfg: cfg, now: time.Now}
	c.setPaths()
	if c.accountsPath != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("raccoon: cannot create data dir %s: %v", deps.DataDir, err)
		}
	}
	c.pool = newPool(
		c.accountsPath,
		c.statePath,
		cfg.storeFlush(),
		cooldownConfig{def: cfg.cooldown(), short: cfg.shortCooldown(), quota: cfg.quotaCooldown()},
		deps.Logf,
	)
	c.pool.load(configuredAccounts(cfg))
	c.httpc = c.httpClient()
	return c, nil
}

// setPaths resolves the two state files inside Deps.DataDir. When DataDir is
// empty the paths stay empty, which makes the pool skip every read and write
// rather than scattering state over the working directory.
func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
}

func (c *Client) httpClient() *http.Client {
	c.httpOnce.Do(func() {
		if c.deps.HTTPClient != nil {
			c.httpc = c.deps.HTTPClient
			return
		}
		if fp := c.cfg.tlsProfile(); fp != fingerprint.ProfileNone {
			if fhc, ferr := fingerprint.New(fingerprint.Options{
				Profile:  fp,
				Protocol: c.cfg.tlsProtocol(),
				Proxy:    c.deps.Proxy,
				Logf:     c.deps.Logf,
			}); ferr == nil {
				c.httpc = fhc
				return
			} else {
				c.deps.Log("raccoon: tls fingerprint %q ignored: %v", string(fp), ferr)
			}
		}
		tr := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		}
		if p := strings.TrimSpace(c.deps.Proxy); p != "" {
			if u, err := url.Parse(p); err == nil {
				tr.Proxy = http.ProxyURL(u)
			}
		}
		c.httpc = &http.Client{Transport: tr}
	})
	return c.httpc
}

// Name is both the registry key and the routing prefix (`raccoon/<model>`).
func (c *Client) Name() string { return "raccoon" }

// ---- model catalogue ---------------------------------------------------

// Models is on the hot path (Registry.Resolve walks every module), so it must
// stay cheap: it never blocks on the network. A cold or stale cache kicks a
// background refresh and immediately answers with whatever is available.
func (c *Client) Models(ctx context.Context) ([]core.Model, error) {
	if list := c.cachedModels(); len(list) > 0 && c.modelsFresh() {
		return list, nil
	}
	c.refreshModelsAsync()
	if list := c.cachedModels(); len(list) > 0 {
		return list, nil
	}
	return fallbackModels(), nil
}

// RefreshModels implements core.ModelRefresher. On any failure it returns the
// last good list ALONGSIDE the error, so a flaky refresh can never blank the
// model picker.
func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	list, err := c.fetchModels(ctx)
	if err != nil {
		if last := c.cachedModels(); len(last) > 0 {
			return last, err
		}
		return fallbackModels(), err
	}
	if len(list) == 0 {
		err := errors.New("raccoon: model catalogue came back empty")
		if last := c.cachedModels(); len(last) > 0 {
			return last, err
		}
		return fallbackModels(), err
	}
	c.setModels(list)
	return list, nil
}

// fetchModels walks the pool until one account yields a catalogue.
func (c *Client) fetchModels(ctx context.Context) ([]core.Model, error) {
	ctx = orBackground(ctx)
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}
	skip := map[string]bool{}
	var lastErr error
	for i := 0; i < c.cfg.maxAttempts(); i++ {
		e := c.pool.pick(skip)
		if e == nil {
			break
		}
		skip[e.acct.id()] = true
		c.ensureFresh(ctx, e)
		list, err := c.fetchCatalog(ctx, e.acct.cred())
		if err == nil && len(list) > 0 {
			return list, nil
		}
		if err == nil {
			err = errors.New("raccoon: empty model catalogue")
		}
		lastErr = err
		if !retryable(classifyErr(err)) {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil {
		lastErr = core.ErrNotConfigured
	}
	return nil, lastErr
}

func (c *Client) refreshModelsAsync() {
	c.modelsMu.Lock()
	if c.modelsLoading {
		c.modelsMu.Unlock()
		return
	}
	now := c.now()
	if !c.modelsAttemptAt.IsZero() && now.Sub(c.modelsAttemptAt) < modelsRetryInterval {
		c.modelsMu.Unlock()
		return
	}
	c.modelsLoading = true
	c.modelsAttemptAt = now
	c.modelsMu.Unlock()

	core.GoSafe("raccoon models", nil, func() {
		defer func() {
			c.modelsMu.Lock()
			c.modelsLoading = false
			c.modelsMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.modelsTimeout())
		defer cancel()
		if list, err := c.fetchModels(ctx); err == nil && len(list) > 0 {
			c.setModels(list)
		}
	})
}

func (c *Client) setModels(list []core.Model) {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	c.models = list
	c.modelsAt = c.now()
}

// cachedModels returns a copy of the live catalogue. It never falls back to
// the built-in table: callers that want a usable list use Models().
func (c *Client) cachedModels() []core.Model {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if len(c.models) == 0 {
		return nil
	}
	out := make([]core.Model, len(c.models))
	copy(out, c.models)
	return out
}

func (c *Client) modelsFresh() bool {
	c.modelsMu.Lock()
	defer c.modelsMu.Unlock()
	if c.modelsAt.IsZero() {
		return false
	}
	return c.now().Sub(c.modelsAt) < c.cfg.modelsTTL()
}

func (c *Client) modelIDs() []string {
	list := c.cachedModels()
	if len(list) == 0 {
		list = fallbackModels()
	}
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

// ModelMaxOutputTokens implements core.ModelLimitsProvider. The interface
// forbids fetching, so this answers from the live catalogue cache only and
// declines on a cold cache rather than triggering a network call. (The
// built-in table is deliberately not used here: a cold cache must decline.)
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	id := strings.TrimSpace(model)
	if id == "" {
		return 0, false
	}
	list := c.cachedModels()
	if len(list) == 0 {
		return 0, false
	}
	if n, ok := core.OutputLimitFor(list, id); ok {
		return n, true
	}
	if bare := stripMultiplier(id); bare != id {
		if n, ok := core.OutputLimitFor(list, bare); ok {
			return n, true
		}
	}
	if n, ok := outputLimitByDisplayName(list, id); ok {
		return n, true
	}
	return 0, false
}

// outputLimitByDisplayName resolves a model by the display name the panel
// shows, with or without its price suffix, so a caller that round-trips a
// display name still gets the right budget.
func outputLimitByDisplayName(list []core.Model, want string) (int, bool) {
	if want == "" {
		return 0, false
	}
	bare := stripMultiplier(want)
	for _, m := range list {
		dn, _ := m.Extra["display_name"].(string)
		if dn == "" {
			continue
		}
		if dn != want && stripMultiplier(dn) != bare {
			continue
		}
		if n, ok := core.ModelOutputLimit(m); ok {
			return n, true
		}
	}
	return 0, false
}

// ---- chat --------------------------------------------------------------

// Chat opens one streaming completion.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || strings.TrimSpace(req.Model) == "" {
		return nil, core.ErrUnsupported
	}
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}
	model := mapModel(req.Model)
	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	stream, err := c.openChat(ctx, cancel, req, model)
	if err != nil {
		cancel()
		return nil, err
	}
	return stream, nil
}

// openChat walks the pool (least-recently-used first) until one account
// accepts the request, parking the ones that failed.
func (c *Client) openChat(ctx context.Context, cancel context.CancelFunc, req *core.ChatRequest, model string) (core.Stream, error) {
	skip := map[string]bool{}
	var lastErr error
	// busy records that every account we reached was at its per-account
	// in-flight ceiling.
	busy := false
	for i := 0; i < c.cfg.maxAttempts(); i++ {
		e := c.pool.pick(skip)
		if e == nil {
			break
		}
		// Take the account's slot before the network call.  A full account is
		// skipped so the next candidate is tried.
		if err := req.AcquireAccountSlot(e.acct.id()); err != nil {
			skip[e.acct.id()] = true
			busy = true
			continue
		}
		skip[e.acct.id()] = true
		c.ensureFresh(ctx, e)
		stream, err := c.chatWith(ctx, cancel, e, req, model)
		if err == nil {
			c.pool.markUsed(e)
			c.noteUpstreamOK()
			return stream, nil
		}
		lastErr = err
		kind := classifyErr(err)
		msg := truncate(c.scrub(err.Error()), 240)
		if kind == kindAuth {
			c.pool.markDead(e, msg)
		} else {
			c.pool.markFailure(e, kind, msg)
		}
		if !retryable(kind) || ctx.Err() != nil {
			break
		}
	}
	if lastErr == nil && busy {
		return nil, core.ErrBusy
	}
	if lastErr == nil {
		lastErr = core.ErrNotConfigured
	}
	c.noteFailure(lastErr)
	return nil, lastErr
}

// chatWith performs one attempt, refreshing once and retrying exactly once
// when the server rejects the access token.
func (c *Client) chatWith(ctx context.Context, cancel context.CancelFunc, e *entry, req *core.ChatRequest, model string) (core.Stream, error) {
	resp, err := c.postChat(ctx, e.acct.cred(), req, model)
	if err != nil {
		if !isAuthError(err) || !e.acct.cred().refreshable() {
			return nil, err
		}
		if rerr := c.refreshAccount(ctx, e); rerr != nil {
			return nil, err
		}
		resp, err = c.postChat(ctx, e.acct.cred(), req, model)
		if err != nil {
			return nil, err
		}
	}
	core.NoteServedBy(req, e.acct.id())
	return newRaccoonStream(ctx, cancel, resp.Body, c.cfg.firstTokenTimeout(), c.cfg.idleTimeout()), nil
}

// postChat sends one streaming chat completion. A non-2xx response, or a 2xx
// response that carries a JSON document instead of an SSE stream, is turned
// into an error (or re-framed as a single-frame stream) so the caller never
// tries to parse a JSON error body as SSE.
func (c *Client) postChat(ctx context.Context, cred credential, req *core.ChatRequest, model string) (*http.Response, error) {
	body := buildChatBody(req, model, true)
	hdr := raccoonHeaders(cred, c.cfg.platform(), c.cfg.version())
	hdr.Set("Accept", "text/event-stream")
	resp, err := c.send(ctx, http.MethodPost, pathChat, hdr, body)
	if err != nil {
		return nil, err
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	jsonish := strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream")
	if resp.StatusCode < 400 && !jsonish {
		return resp, nil
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if rerr != nil {
		return nil, fmt.Errorf("raccoon: reading response: %w", rerr)
	}
	data, derr := decodeEnvelope(resp.StatusCode, raw)
	if derr != nil {
		return nil, derr
	}
	if resp.StatusCode >= 400 {
		return nil, &apiError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	// A whole completion delivered as one JSON document — either bare or
	// wrapped in the vendor envelope: re-frame the unwrapped payload as SSE so
	// a single parser handles both shapes.
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 || string(payload) == "null" {
		payload = bytes.TrimSpace(raw)
	}
	framed := "data: " + string(payload) + "\n\ndata: [DONE]\n\n"
	resp.Body = io.NopCloser(strings.NewReader(framed))
	return resp, nil
}

// ensureFresh renews a credential that is inside the renewal window. It is
// best effort: a failed proactive refresh is logged and the request is still
// attempted, because the server's 401 is the authority.
func (c *Client) ensureFresh(ctx context.Context, e *entry) {
	cr := e.acct.cred()
	if !cr.refreshable() {
		return
	}
	now := c.now()
	if !cr.expired(now) && !cr.needsRefresh(now, c.cfg.refreshMargin()) {
		return
	}
	if err := c.refreshAccount(ctx, e); err != nil {
		c.deps.Log("raccoon: proactive refresh failed for %s: %v", e.acct.id(), c.scrub(err.Error()))
	}
}

// ---- status ------------------------------------------------------------

// Status is cheap: no network, one small file read at most.
func (c *Client) Status(ctx context.Context) core.Status {
	st := core.Status{Name: "raccoon", UpdatedAt: c.now().UTC()}
	st.Accounts = c.pool.snapshot()
	st.Models = c.modelIDs()

	if c.pool.len() == 0 {
		st.Detail = "no credential: set clients.raccoon.access_token (or " + envAccessToken +
			"), or paste one in the panel"
		return st
	}

	ready := 0
	for _, a := range st.Accounts {
		if a.Enabled && a.State == stateReady {
			ready++
		}
	}
	st.Ready = ready > 0

	detail := fmt.Sprintf("%d/%d accounts ready (%s)", ready, len(st.Accounts), c.pool.summary())
	if !st.Ready {
		detail = "no account available: " + c.pool.summary()
	}
	if c.modelsFresh() {
		detail += fmt.Sprintf("; catalogue %d models", len(c.cachedModels()))
	} else {
		detail += "; catalogue not loaded"
	}
	if msg, at := c.lastError(); msg != "" {
		detail += fmt.Sprintf("; last error %s ago: %s", humanAge(c.now().Sub(at)), msg)
	}
	st.Detail = detail
	return st
}

func (c *Client) noteUpstreamOK() {
	c.stateMu.Lock()
	c.upstreamOK = true
	c.lastErr = ""
	c.lastErrAt = time.Time{}
	c.stateMu.Unlock()
}

func (c *Client) noteFailure(err error) {
	c.stateMu.Lock()
	c.upstreamOK = false
	c.lastErr = truncate(c.scrub(err.Error()), 240)
	c.lastErrAt = c.now()
	c.stateMu.Unlock()
}

func (c *Client) lastError() (string, time.Time) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.lastErr, c.lastErrAt
}

// scrub removes credential material before anything reaches a log or a status
// string.
func (c *Client) scrub(s string) string {
	for _, e := range c.pool.all() {
		if t := strings.TrimSpace(string(e.acct.AccessToken)); len(t) > 12 {
			s = strings.ReplaceAll(s, t, core.MaskSecret(t))
		}
		if t := strings.TrimSpace(string(e.acct.RefreshToken)); len(t) > 12 {
			s = strings.ReplaceAll(s, t, core.MaskSecret(t))
		}
	}
	return core.Redact(s)
}

// mapModel normalises a routed model id: it drops any `client/` prefix and
// the ` · x0.75` billing suffix the picker shows.
func mapModel(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return stripMultiplier(m)
}

func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}
