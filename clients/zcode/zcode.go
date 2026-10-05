// Package zcode implements the ZCode client module.
//
// It exposes the ZCode / Z.AI coding-plan models (GLM-5.3, GLM-5.3-Flash, …)
// as an OpenAI-compatible client2api module.  The upstream speaks the Anthropic
// Messages protocol, so this package translates OpenAI -> Anthropic on the way
// out and Anthropic -> core.Events on the way back.
//
// Everything here is private to zcode; no other module may import it.
package zcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/fingerprint"
)

func init() { core.Register("zcode", New) }

// Client is the zcode module.  It is safe for concurrent use.
type Client struct {
	deps core.Deps
	cfg  *Config
	http *http.Client
	pool *pool
	// models is the TTL cache of the vendor's own model list (see models.go).
	models *modelCache
	// logins holds the panel-driven Z.AI sign-in flows that are still in
	// flight.  It is mirrored to web_login.json inside the data dir.
	logins *loginSessions
	// affinity pins a conversation to the account that has been serving it, so
	// a multi-turn conversation does not re-roll credentials — and therefore
	// re-roll its risk profile — on every turn.  See affinity.go.
	affinity *core.Affinity
	// board is the TTL cache behind the task board's claim preview (tasks.go).
	board claimBoard

	// captcha caches the Aliyun verify parameter the JWT channel needs, so a
	// burst of turns does not start a browser for every request.  See
	// captcha_browser.go.
	captcha captchaCache
	// mintBrowser is the built-in solver, nil when no browser is installed.
	// A field rather than a direct call so a test can substitute one without
	// launching Edge.
	mintBrowser func(context.Context, regionInfo) (string, error)
}

// New builds the module.  It never returns an error for a bad config: a
// degraded module that still reports why is more useful than a missing one.
func New(deps core.Deps) (core.Client, error) {
	cfg := loadConfig(deps.Config, deps.Logf)
	hc := deps.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	// A named profile replaces the transport every vendor-facing request goes
	// through, including the account pool's.  The Go hello is one of the few
	// things about this proxy a vendor can spot without reading a single
	// header, so a module that cares names a profile here.  The default
	// (empty) keeps the client above, so this is a no-op unless asked for.
	if fp := cfg.tlsProfile(); fp != fingerprint.ProfileNone {
		fhc, err := fingerprint.New(fingerprint.Options{
			Profile:  fp,
			Protocol: cfg.tlsProtocol(),
			Proxy:    deps.Proxy,
			Logf:     deps.Logf,
		})
		if err != nil {
			deps.Log("zcode: tls fingerprint %q ignored: %v", string(fp), err)
		} else {
			hc = fhc
			deps.Log("zcode: tls handshake imitates the %s client", string(fp))
		}
	}
	// The affinity table is built with the shared default window and its sweep
	// started here; a live config change retunes it through ApplyLive
	// (affinity.go) without a restart.
	//
	// The pool resolves the built-in captcha browser here, once.  captchaReady
	// has to answer without launching anything -- the panel asks it every few
	// seconds -- and a machine with no browser must report the JWT channel as
	// unusable up front instead of failing at the first request.
	p := newPool(deps.DataDir, cfg, hc, deps.Logf)
	p.browser = newBrowserSolver(cfg, deps.Logf)
	if p.browser != nil {
		deps.Log("zcode: the jwt captcha will be minted with %s", p.browser.exe)
	}
	c := &Client{
		deps:     deps,
		cfg:      cfg,
		http:     hc,
		pool:     p,
		models:   newModelCache(),
		logins:   &loginSessions{},
		affinity: core.NewAffinity(0),
	}
	if p.browser != nil {
		c.mintBrowser = p.browser.solve
	}
	c.affinity.StartGC()
	return c, nil
}

// Name returns the routing prefix ("zcode/<model>").
func (c *Client) Name() string { return "zcode" }

// Models and RefreshModels live in models.go: the catalogue is fetched from
// the vendor's own /v1/models endpoint and cached with a TTL.

// Status is rendered by the panel every few seconds, so it must stay cheap and
// must never make a network call.
func (c *Client) Status(ctx context.Context) core.Status {
	ready, detail := c.pool.summary()

	ids := c.cfg.modelIDs()
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			models = append(models, id)
		}
	}

	return core.Status{
		Name:      "zcode",
		Ready:     ready,
		Detail:    detail,
		Accounts:  c.pool.accountsForStatus(),
		Models:    models,
		UpdatedAt: time.Now(),
	}
}

// ---------------------------------------------------------------------------
// Chat
// ---------------------------------------------------------------------------

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

	// Explicit operator override: route everything at one base URL, with no
	// credentials at all.  Off unless configured; never a silent fallback.
	if c.cfg.UpstreamBase != "" {
		return c.attempt(ctx, req, stopgapAccount(c.cfg), model)
	}

	if c.pool.usableCount() == 0 {
		_, detail := c.pool.summary()
		return nil, fmt.Errorf("%w: %s", core.ErrNotConfigured, detail)
	}

	attempts := c.cfg.attempts()
	base := core.RotateBackoffBase
	exclude := make(map[string]bool, attempts)
	var lastAcct *Account
	var lastErr error
	// busy records that every account we could reach was at its per-account
	// in-flight ceiling, which the gateway maps to 429 rather than "no account".
	busy := false

	// The stickiness key is derived once, from the request itself: it does not
	// change between attempts, and deriving it per attempt would let a caller
	// that mutates its options mid-request split one conversation across two
	// accounts.  An empty key means "not conversation-scoped" and pickAccount
	// then behaves exactly like pool.next.
	key := conversationKey(req)

	for i := 0; i < attempts; i++ {
		acct := c.pickAccount(exclude, key, model)
		if acct == nil {
			break
		}
		// Take this account's in-flight slot before any upstream call.  A full
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
		// Pace the rotation.  Five attempts fired back to back from one egress
		// IP is exactly the density risk control measures, and this is the
		// largest attempt count in the project.  i is the index of the attempt
		// about to run, so the first wait is BackoffFrom(base, 0) -- the same
		// first wait the gateway itself uses.  There is deliberately no wait
		// before the first attempt, and the account is selected before the
		// wait so an exhausted pool never sleeps at all.
		//
		// BackoffFrom already applies core.JitterDur, so it must not be
		// wrapped again: doing so would widen the spread to about +/-56% and
		// make the wait drift from the gateway's own rotation.
		if i > 0 && !core.SleepCtx(ctx, core.BackoffFrom(base, i-1)) {
			// The caller went away while we were waiting for the next account.
			return nil, classifyTerminal(lastErr, lastAcct)
		}
		lastAcct = acct

		stream, err := c.attempt(ctx, req, acct, model)
		if err == nil {
			return stream, nil
		}
		lastErr = err

		if ctx.Err() != nil {
			return nil, classifyTerminal(err, acct)
		}
		var ue *upstreamError
		if errors.As(err, &ue) {
			if ue.Kind == kindModel {
				// A model rejection is a property of the request, not of the
				// credential: retrying another account cannot help.  It is
				// still classified so the gateway can attribute it, but as
				// FailureOther -- the one kind it never rotates.
				return nil, core.Fail("zcode", acct.ID, core.FailureOther, ue.Status,
					fmt.Errorf("%w: %s", core.ErrUnsupported, ue.short()))
			}
			if ue.Kind == kindCaptcha && c.pool.captchaReady() {
				// A fresh verification parameter was already fetched for this
				// attempt; give the same account one more chance on the next
				// loop iteration by un-excluding it.
				// The parameter this attempt carried was refused, so it is not
				// reusable: drop it, then give the same account another go.
				c.captcha.invalidate()
				delete(exclude, acct.ID)
			}
			// Risk control is judged from the egress IP, not the credential:
			// feed every block into the shared gate and stop the moment it
			// reports that the IP itself is being refused.  A nil Guard (unit
			// tests) is safe -- ReportWAF has a nil-receiver check.
			if ue.failureKind() == core.FailureWAF && c.deps.Guard.ReportWAF(acct.ID) {
				return nil, classifyTerminal(err, acct)
			}
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
	c.pool.setLastErr(truncate(lastErr.Error(), 200))
	return nil, classifyTerminal(lastErr, lastAcct)
}

// classifyTerminal translates the error that ended a Chat call into the shared
// error contract, attributed to the account that produced it.  Only a
// *core.Failure is rotated, WAF-gated and attributed by the gateway, so a module
// that returns a plain error silently opts out of the shared machinery -- which
// is exactly what this module used to do.
func classifyTerminal(err error, acct *Account) error {
	if err == nil {
		return nil
	}
	// Already classified: keep it and fill in the attribution the gateway
	// reads to feed the IP gate and /v1/status.
	if f, ok := core.AsFailure(err); ok {
		if f.Account == "" && acct != nil {
			f.Account = acct.ID
		}
		return err
	}
	// Local capacity problems keep their own shape: the gateway maps them by
	// errors.Is, and rotating accounts cannot conjure a missing credential or
	// a missing captcha solver.
	if errors.Is(err, core.ErrNotConfigured) || errors.Is(err, core.ErrUnsupported) {
		return err
	}

	id := ""
	if acct != nil {
		id = acct.ID
	}
	var ue *upstreamError
	if errors.As(err, &ue) {
		return core.Fail("zcode", id, ue.failureKind(), ue.Status, err)
	}
	// Transport failures, decode failures and the caller's own cancellation:
	// FailureUpstream is the retryable catch-all.
	return core.Fail("zcode", id, core.FailureUpstream, 0, err)
}

// stopgapAccount is the synthetic credential used with upstream_base.
func stopgapAccount(cfg *Config) *Account {
	return &Account{
		ID:       "stopgap",
		Label:    "upstream_base override",
		Provider: "stopgap",
		Mode:     "none",
		BaseURL:  cfg.UpstreamBase,
		Enabled:  true,
		State:    stateReady,
	}
}

// attempt performs exactly one upstream request for one account.
func (c *Client) attempt(ctx context.Context, req *core.ChatRequest, acct *Account, model string) (core.Stream, error) {
	body, err := buildRequestBody(req, c.cfg, acct, model)
	if err != nil {
		return nil, fmt.Errorf("zcode: build request: %w", err)
	}
	payload, err := marshalNoEscape(body)
	if err != nil {
		return nil, fmt.Errorf("zcode: encode request: %w", err)
	}

	region, verifyParam := "", ""
	if acct.Mode == modeJWT {
		verifyParam, region, err = c.solveCaptcha(ctx)
		if err != nil {
			return nil, err
		}
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

	url := c.upstreamURL(acct)
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("zcode: build http request: %w", err)
	}
	c.applyHeaders(httpReq, acct, region, verifyParam)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("zcode: upstream request failed: %w", err)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))

	if resp.StatusCode >= http.StatusBadRequest {
		raw := readLimited(resp.Body, 1<<20)
		_ = resp.Body.Close()
		ue := classify(resp.StatusCode, raw)
		ue.Msg = redact(ue.Msg, acct.secret())
		c.recordFailure(acct, ue)
		return nil, ue
	}

	if strings.Contains(contentType, "text/event-stream") {
		handedOff = true
		stream := newAnthropicStream(resp.Body, model)
		stream.cancel = cancel
		return stream, nil
	}

	raw := readLimited(resp.Body, 8<<20)
	_ = resp.Body.Close()

	if ue := classifyEnvelope(resp.StatusCode, raw); ue != nil {
		ue.Msg = redact(ue.Msg, acct.secret())
		c.recordFailure(acct, ue)
		return nil, ue
	}

	events, err := responseEvents(raw, model)
	if err != nil {
		return nil, fmt.Errorf("zcode: decode upstream response: %w", err)
	}
	handedOff = true
	return newSliceStream(events, nil, cancel), nil
}

// upstreamURL resolves the chat endpoint for an account.
func (c *Client) upstreamURL(acct *Account) string {
	base := strings.TrimSpace(c.cfg.UpstreamBase)
	if base == "" && acct != nil {
		base = strings.TrimSpace(acct.BaseURL)
	}
	if base == "" {
		provider, mode := providerZai, modeAPIKey
		if acct != nil {
			if acct.Provider != "" {
				provider = acct.Provider
			}
			if acct.Mode != "" {
				mode = acct.Mode
			}
		}
		base = defaultBaseURL(provider, mode)
	}
	return joinMessages(base)
}

// applyIdentityHeaders installs the companion headers that make a request look
// like it came from the official client.  It carries no credential, so both the
// chat path and the plan-billing path share it -- and sharing it is the point:
// a second header set for billing would drift from the one the chat path
// proved against the live vendor.
func (c *Client) applyIdentityHeaders(h http.Header) {
	id := c.cfg.Identity
	// Both carry the same value: the version the release manifest last
	// advertised, or the configured one when it could not be read.
	setHeader(h, "User-Agent", c.pool.userAgent())
	setHeader(h, "X-ZCode-App-Version", c.appVersion())
	setHeader(h, "X-ZCode-Agent", id.Agent)
	setHeader(h, "HTTP-Referer", id.Referer)
	setHeader(h, "X-Title", id.Title)
	setHeader(h, "X-Platform", id.Platform)
	setHeader(h, "X-Release-Channel", id.ReleaseChannel)
	setHeader(h, "X-Client-Language", id.Language)
	setHeader(h, "X-Client-Timezone", id.Timezone)
	setHeader(h, "X-Os-Category", id.OSCategory)
	setHeader(h, "X-Os-Version", id.OSVersion)
	setHeader(h, "X-Device-Mid", c.pool.deviceMid())
}

// applyHeaders installs the identity, auth and trace headers.  Values that are
// not printable ASCII are dropped rather than sent.
func (c *Client) applyHeaders(req *http.Request, acct *Account, region, verifyParam string) {
	h := req.Header
	c.applyIdentityHeaders(h)

	setHeader(h, "Content-Type", "application/json")
	setHeader(h, "Accept", "application/json")
	setHeader(h, "Anthropic-Version", "2023-06-01")

	if acct != nil {
		switch acct.Mode {
		case modeJWT:
			if acct.jwt != "" {
				setHeader(h, "Authorization", "Bearer "+acct.jwt)
			}
			// The start-plan channel accepts ONLY these three trace headers;
			// adding x-query-id or x-session-id here triggers 3012.
			trace := randomUUID()
			setHeader(h, "x-request-id", trace)
			setHeader(h, "x-zcode-session-type", "main")
			setHeader(h, "x-zcode-trace-id", trace)
		case modeAPIKey:
			if s := acct.secret(); s != "" {
				setHeader(h, "x-api-key", s)
			}
		}
	}

	if verifyParam != "" {
		setHeader(h, "X-Aliyun-Captcha-Verify-Param", verifyParam)
		// Without the matching region header the upstream answers 3007.
		setHeader(h, "X-Aliyun-Captcha-Verify-Region", region)
	}
}

func setHeader(h http.Header, key, value string) {
	if strings.TrimSpace(value) == "" || !printableASCII(value) {
		return
	}
	h.Set(key, value)
}

// solveCaptcha produces the Aliyun verify param for one call, together with the
// region that has to be echoed back beside it.
//
// The order is the whole point:
//
//  1. A param the operator's own browser just minted (the panel's claim button)
//     wins over anything this process can arrange: they watched it succeed,
//     against the real risk engine, on this exact network.
//  2. A parameter minted a moment ago is reused for captchaParamTTL.  The
//     vendor accepts one for a window, and starting a browser per turn would
//     add seconds to every request.
//  3. captcha_command, when the operator pointed at a solver.
//  4. The built-in browser mint (captcha_browser.go), which is what lets an
//     ordinary install use the JWT channel with no setup at all.
//
// A local failure to mint is reported as ErrNotConfigured, not as a vendor
// failure: rotating accounts cannot conjure a captcha, so the gateway should
// say what is actually missing instead of blaming a credential.
func (c *Client) solveCaptcha(ctx context.Context) (param, region string, err error) {
	if sol, ok := core.CaptchaSolutionFrom(ctx); ok {
		region = sol.Region
		if region == "" {
			region = c.pool.regionFor(ctx).Region
		}
		c.captcha.put(sol.Param, region, time.Now())
		return sol.Param, region, nil
	}

	if cached, ok := c.captcha.get(time.Now()); ok {
		return cached.param, cached.region, nil
	}

	if name := strings.TrimSpace(c.cfg.CaptchaCommand); name != "" {
		info := c.pool.regionFor(ctx)
		param, err := c.solveCaptchaCommand(ctx, name, info)
		if err != nil {
			return "", "", err
		}
		c.captcha.put(param, info.Region, time.Now())
		return param, info.Region, nil
	}

	if c.mintBrowser != nil {
		info := c.pool.sceneFor(ctx)
		param, err := c.mintBrowser(ctx, info)
		if err != nil {
			if ctx.Err() != nil {
				return "", "", ctx.Err()
			}
			return "", "", fmt.Errorf("%w: %v", core.ErrNotConfigured, err)
		}
		c.captcha.put(param, info.Region, time.Now())
		return param, info.Region, nil
	}

	return "", "", fmt.Errorf("%w: the jwt channel needs an Aliyun captcha solver: %s", core.ErrNotConfigured, c.captchaHint())
}

// captchaHint explains which knob is missing, so the panel's note points at the
// one an operator can actually turn.
func (c *Client) captchaHint() string {
	if c.cfg.captchaBrowser() {
		return "no Edge or Chrome was found; set captcha_command, or claim from the panel so a browser can solve it"
	}
	return "captcha_browser is off; set captcha_command, or claim from the panel so a browser can solve it"
}

// solveCaptchaCommand runs the operator-supplied solver.
func (c *Client) solveCaptchaCommand(ctx context.Context, name string, info regionInfo) (string, error) {
	solveCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	out, err := exec.CommandContext(solveCtx, name, captchaArgs(c.cfg, info)...).Output()
	if err != nil {
		return "", fmt.Errorf("zcode: captcha solver failed: %w", err)
	}
	param, ok := parseVerifyParam(string(out))
	if !ok {
		return "", errors.New("zcode: captcha solver produced no VERIFY_PARAM line")
	}
	return param, nil
}

// captchaArgs expands the configured argument template.
func captchaArgs(cfg *Config, info regionInfo) []string {
	args := make([]string, 0, len(cfg.CaptchaArgs))
	for _, a := range cfg.CaptchaArgs {
		a = strings.ReplaceAll(a, "{scene}", info.SceneID)
		a = strings.ReplaceAll(a, "{region}", info.Region)
		a = strings.ReplaceAll(a, "{prefix}", info.Prefix)
		args = append(args, a)
	}
	return args
}

// parseVerifyParam extracts the solver's VERIFY_PARAM=<value> output line.
func parseVerifyParam(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if value, ok := strings.CutPrefix(line, "VERIFY_PARAM="); ok {
			if value = strings.TrimSpace(value); value != "" {
				return value, true
			}
		}
	}
	return "", false
}

// recordFailure applies the account state machine for a classified failure.
func (c *Client) recordFailure(acct *Account, ue *upstreamError) {
	if ue == nil {
		return
	}
	c.pool.setLastErr(ue.short())
	if acct == nil || acct.ID == "stopgap" {
		return
	}

	now := time.Now()
	switch ue.Kind {
	case kindExhausted:
		c.pool.mark(acct.ID, func(a *Account) {
			a.State = stateExhausted
			a.LastError = ue.short()
		})
	case kindInvalid:
		c.pool.mark(acct.ID, func(a *Account) {
			a.State = stateInvalid
			a.LastError = ue.short()
		})
	case kindRisk:
		c.pool.mark(acct.ID, func(a *Account) {
			a.State = stateCooling
			a.CooldownUntil = now.Add(c.cfg.cooldown())
			a.Note = "risk control (3012): back off before retrying"
			a.LastError = ue.short()
		})
	case kindRateLimit:
		c.pool.mark(acct.ID, func(a *Account) {
			a.State = stateCooling
			a.CooldownUntil = now.Add(c.cfg.cooldown())
			a.LastError = ue.short()
		})
	case kindConcurrency:
		c.pool.mark(acct.ID, func(a *Account) {
			a.State = stateCooling
			a.CooldownUntil = now.Add(defaultConcurrCoolSec * time.Second)
			a.LastError = ue.short()
		})
	case kindCaptcha:
		c.pool.mark(acct.ID, func(a *Account) {
			a.LastError = ue.short()
		})
	}
}

func readLimited(r io.Reader, max int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	return b
}
