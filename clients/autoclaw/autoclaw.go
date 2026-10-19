// Package autoclaw implements the independent AutoClaw desktop platform.
//
// AutoClaw is NOT Zhipu, BigModel, ZCode or WorkBuddy.  It has its own host,
// product identity, token store and model catalogue; this package keeps those
// boundaries explicit.
package autoclaw

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
	"sync/atomic"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
	"client2api/internal/smscap"
)

func init() { core.Register("autoclaw", New) }

const modelsRetryInterval = 30 * time.Second

type Client struct {
	deps core.Deps
	cfg  config

	accountsPath string
	statePath    string
	pool         *pool

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

	loginMu sync.Mutex
	logins  map[string]*loginSession

	// smsRot hands out the next province in the one-time-SMS rotation
	// (sms.go).  autos is the in-flight auto-login job table (autologin.go);
	// it is created lazily by putAutoJob.
	smsRot smscap.Rotator
	autoMu sync.Mutex
	autos  map[string]*autoJob

	deviceMu sync.Mutex
	deviceID string

	now      func() time.Time
	affinity *core.Affinity
	inFlight atomic.Int64
}

func New(deps core.Deps) (core.Client, error) {
	cfg, err := parseConfig(deps.Config)
	if err != nil {
		deps.Log("autoclaw: invalid config, using defaults: %v", err)
		cfg = config{}
	}
	applyEnv(&cfg)
	c := &Client{deps: deps, cfg: cfg, now: time.Now}
	c.affinity = core.NewAffinity(0)
	c.affinity.StartGC()
	c.setPaths()
	if c.accountsPath != "" {
		if err := core.EnsureDir(deps.DataDir); err != nil {
			deps.Log("autoclaw: cannot create data dir %s: %v", deps.DataDir, err)
		}
	}
	c.pool = newPool(c.accountsPath, c.statePath, cfg.storeFlush(), cooldownConfig{def: cfg.cooldown(), short: cfg.shortCooldown(), quota: cfg.quotaCooldown()}, deps.Logf)
	c.pool.load(configuredAccounts(cfg))
	c.httpc = c.httpClient()
	return c, nil
}

func (c *Client) setPaths() {
	if c.deps.DataDir == "" {
		return
	}
	c.accountsPath = filepath.Join(c.deps.DataDir, accountsFile)
	c.statePath = filepath.Join(c.deps.DataDir, stateFile)
}

func (c *Client) httpClient() *http.Client {
	c.httpOnce.Do(func() {
		if fp := c.cfg.tlsProfile(); fp != fingerprint.ProfileNone {
			if fhc, ferr := fingerprint.New(fingerprint.Options{Profile: fp, Protocol: c.cfg.tlsProtocol(), Proxy: c.deps.Proxy, Logf: c.deps.Logf}); ferr == nil {
				c.httpc = fhc
				return
			} else {
				c.deps.Log("autoclaw: tls fingerprint %q ignored: %v", string(fp), ferr)
			}
		}
		if c.deps.HTTPClient != nil {
			c.httpc = c.deps.HTTPClient
			return
		}
		tr := &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConns: 32, MaxIdleConnsPerHost: 8,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second, ForceAttemptHTTP2: true,
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

func (c *Client) Name() string { return "autoclaw" }

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

func (c *Client) RefreshModels(ctx context.Context) ([]core.Model, error) {
	list, err := c.fetchModels(ctx)
	if err != nil {
		if last := c.cachedModels(); len(last) > 0 {
			return last, err
		}
		return fallbackModels(), err
	}
	if len(list) == 0 {
		err := errors.New("autoclaw: model catalogue came back empty")
		if last := c.cachedModels(); len(last) > 0 {
			return last, err
		}
		return fallbackModels(), err
	}
	c.setModels(list)
	return list, nil
}

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
			err = errors.New("autoclaw: empty model catalogue")
		}
		lastErr = err
		if !retryable(classifyErr(err)) || ctx.Err() != nil {
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
	if c.modelsLoading || (!c.modelsAttemptAt.IsZero() && c.now().Sub(c.modelsAttemptAt) < modelsRetryInterval) {
		c.modelsMu.Unlock()
		return
	}
	c.modelsLoading = true
	c.modelsAttemptAt = c.now()
	c.modelsMu.Unlock()
	core.GoSafe("autoclaw models", nil, func() {
		defer func() { c.modelsMu.Lock(); c.modelsLoading = false; c.modelsMu.Unlock() }()
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
	return !c.modelsAt.IsZero() && c.now().Sub(c.modelsAt) < c.cfg.modelsTTL()
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
func (c *Client) ModelMaxOutputTokens(ctx context.Context, model string) (int, bool) {
	id := strings.TrimSpace(model)
	if id == "" {
		return 0, false
	}
	list := c.cachedModels()
	if len(list) == 0 {
		return 0, false
	}
	return core.OutputLimitFor(list, id)
}

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

func (c *Client) openChat(ctx context.Context, cancel context.CancelFunc, req *core.ChatRequest, model string) (core.Stream, error) {
	skip := map[string]bool{}
	var lastErr error
	busy := false
	for i := 0; i < c.cfg.maxAttempts(); i++ {
		e := c.pickAccount(skip, conversationKey(req))
		if e == nil {
			break
		}
		if err := req.AcquireAccountSlot(e.acct.id()); err != nil {
			skip[e.acct.id()] = true
			busy = true
			// A full account is not an upstream attempt. Keep the request's
			// switch budget for an account that can actually be called.
			i--
			continue
		}
		skip[e.acct.id()] = true
		c.bindServedConversation(req, e.acct.id())
		c.ensureFresh(ctx, e)
		stream, err := c.chatWith(ctx, cancel, e, req, model)
		if err == nil {
			c.pool.markUsed(e)
			c.noteUpstreamOK()
			c.inFlight.Add(1)
			return core.TrackStream(stream, func() { c.inFlight.Add(-1) }), nil
		}
		kind := classifyErr(err)
		msg := truncate(c.scrub(err.Error()), 240)
		if kind == kindAuth {
			c.pool.markDead(e, msg)
		} else {
			c.pool.markFailure(e, kind, msg)
		}
		// Hand a retryable refusal to the gateway with its account attached.
		// A bare apiError is opaque to core.AsFailure, so the pool rotates
		// inside AutoClaw but the gateway never offers the model-group sibling
		// that could serve the same request.
		if retryable(kind) {
			lastErr = core.Fail(c.Name(), e.acct.id(), coreFailureKind(kind), statusOf(err), err)
		} else {
			lastErr = err
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
	return newAutoclawStream(ctx, cancel, resp.Body, c.cfg.firstTokenTimeout(), c.cfg.idleTimeout()), nil
}

func (c *Client) postChat(ctx context.Context, cred credential, req *core.ChatRequest, model string) (*http.Response, error) {
	body := buildChatBody(req, model, true)
	session := conversationKey(req)
	if session == "" {
		session = newRequestID()
	}
	hdr := chatHeaders(cred, model, session, newRequestID(), c.cfg.version())
	resp, err := c.send(ctx, http.MethodPost, pathChatCompletions, hdr, body)
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
		return nil, fmt.Errorf("autoclaw: reading response: %w", rerr)
	}
	data, derr := decodeEnvelope(resp.StatusCode, raw)
	if derr != nil {
		return nil, derr
	}
	if resp.StatusCode >= 400 {
		return nil, &apiError{Status: resp.StatusCode, Message: truncate(string(raw), 300)}
	}
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 || string(payload) == "null" {
		payload = bytes.TrimSpace(raw)
	}
	resp.Body = io.NopCloser(strings.NewReader("data: " + string(payload) + "\n\ndata: [DONE]\n\n"))
	return resp, nil
}

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
		c.deps.Log("autoclaw: proactive refresh failed for %s: %v", e.acct.id(), c.scrub(err.Error()))
	}
}

func (c *Client) Status(ctx context.Context) core.Status {
	st := core.Status{Name: "autoclaw", UpdatedAt: c.now().UTC()}
	st.Accounts = c.pool.snapshot()
	st.Models = c.modelIDs()
	if c.pool.len() == 0 {
		st.Detail = "no credential: set clients.autoclaw.access_token (or " + envAccessToken + "), or import the AutoClaw desktop account"
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
func (c *Client) scrub(s string) string {
	for _, e := range c.pool.all() {
		for _, tok := range []string{e.acct.AccessToken, e.acct.RefreshToken} {
			if t := strings.TrimSpace(tok); len(t) > 12 {
				s = strings.ReplaceAll(s, t, core.MaskSecret(t))
			}
		}
	}
	return core.Redact(s)
}
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

// coreFailureKind maps the module's account-pool taxonomy onto the gateway's
// shared vocabulary.  The pool's kinds decide whether to rotate an account;
// the gateway's kinds decide whether a different platform or caller-visible
// retry is allowed.
func coreFailureKind(kind errKind) core.FailureKind {
	switch kind {
	case kindAuth:
		return core.FailureAuth
	case kindQuota:
		return core.FailureQuota
	case kindTransient:
		return core.FailureRateLimited
	case kindNetwork:
		return core.FailureUpstream
	default:
		return core.FailureOther
	}
}
